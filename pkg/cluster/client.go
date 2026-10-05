package cluster

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"time"
)

// maxResponseBody is the upper limit on K8s API response body reads (50 MB).
const maxResponseBody = 50 << 20

// inClusterCAFile is the service account CA bundle mounted into every pod.
const inClusterCAFile = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"

// sharedTransport is a package-level http.Transport reused by all Client instances.
// The CA is loaded once at init time; only the bearer token varies per Client.
var sharedTransport *http.Transport

func init() {
	tlsConfig, warning := apiServerTLSConfig(os.Getenv, os.ReadFile)
	if warning != "" {
		slog.Warn(warning)
	}
	sharedTransport = newAPIServerTransport(tlsConfig)
}

// newAPIServerTransport returns the transport used for every API server call.
// A custom TLSClientConfig disables HTTP/2 unless ForceAttemptHTTP2 is set
// (net/http.Transport docs), and the default of 2 idle connections per host
// made each parallel status poll open 6-8 new TLS connections. With HTTP/2
// the parallel requests share one connection. No Proxy is set: the in-cluster
// API server must be reached directly.
func newAPIServerTransport(tlsConfig *tls.Config) *http.Transport {
	return &http.Transport{
		TLSClientConfig:       tlsConfig,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}

// apiServerTLSConfig chooses how the API server certificate is verified:
//   - in a pod, the service account CA;
//   - KUBE_CA_FILE, when set (for clusters whose API certificate is signed by a
//     private CA, e.g. the certificate-authority-data of the kubeconfig);
//   - otherwise the system trust store (ROSA/OSD API certificates are publicly
//     trusted);
//   - verification is skipped only in DEV_MODE with DEV_INSECURE_TLS=true.
//
// The returned warning, if any, must be logged.
func apiServerTLSConfig(getenv func(string) string, readFile func(string) ([]byte, error)) (*tls.Config, string) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile := getenv("KUBE_CA_FILE"); caFile != "" {
		pool, err := loadCAPool(readFile, caFile)
		if err != nil {
			// Fail closed: an unusable CA must not silently fall back to another trust source.
			tlsConfig.RootCAs = x509.NewCertPool()
			return tlsConfig, fmt.Sprintf("KUBE_CA_FILE %s is unusable (%v); API server requests will fail TLS verification", caFile, err)
		}
		tlsConfig.RootCAs = pool
		return tlsConfig, ""
	}
	if pool, err := loadCAPool(readFile, inClusterCAFile); err == nil {
		tlsConfig.RootCAs = pool
		return tlsConfig, ""
	}
	if getenv("DEV_MODE") == "true" && getenv("DEV_INSECURE_TLS") == "true" {
		tlsConfig.InsecureSkipVerify = true
		return tlsConfig, "DEV_INSECURE_TLS=true: API server TLS verification is DISABLED. Your token can be intercepted on this network; unset DEV_INSECURE_TLS or set KUBE_CA_FILE instead"
	}
	// System roots (RootCAs nil).
	return tlsConfig, ""
}

func loadCAPool(readFile func(string) ([]byte, error), path string) (*x509.CertPool, error) {
	pem, err := readFile(path)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("no PEM certificates found")
	}
	return pool, nil
}

// K8sError represents a parsed Kubernetes API error response.
type K8sError struct {
	Status     int
	Reason     string
	K8sMessage string
}

func (e *K8sError) Error() string {
	if e.K8sMessage != "" {
		return fmt.Sprintf("k8s API error %d (%s): %s", e.Status, e.Reason, e.K8sMessage)
	}
	return fmt.Sprintf("k8s API error %d (%s)", e.Status, e.Reason)
}

// parseK8sError tries to parse a Kubernetes Status object from the response body.
// If parsing fails, it returns a generic error with the status code.
func parseK8sError(body []byte, statusCode int) error {
	var status struct {
		Kind    string `json:"kind"`
		Status  string `json:"status"`
		Message string `json:"message"`
		Reason  string `json:"reason"`
		Code    int    `json:"code"`
	}
	if err := json.Unmarshal(body, &status); err == nil && status.Kind == "Status" && status.Status == "Failure" {
		return &K8sError{
			Status:     statusCode,
			Reason:     status.Reason,
			K8sMessage: status.Message,
		}
	}
	return &K8sError{
		Status: statusCode,
		Reason: http.StatusText(statusCode),
	}
}

// Client wraps an HTTP client configured for Kubernetes API access.
type Client struct {
	baseURL    string
	token      string
	httpClient *http.Client
	ctx        context.Context
	username   string // logged-in user identity (from OAuth headers)
}

// SetUsername sets the logged-in user identity for audit logging.
func (c *Client) SetUsername(username string) {
	c.username = username
}

// WithContext returns a shallow copy of the client with a different context.
func (c *Client) WithContext(ctx context.Context) *Client {
	cp := *c
	cp.ctx = ctx
	return &cp
}

// NewClientWithContext creates a K8s API client using the given bearer token and context.
// The underlying http.Transport (with TLS/CA config) is shared across all clients;
// only the bearer token varies per instance.
func NewClientWithContext(ctx context.Context, token string) *Client {
	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	port := os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		host = "kubernetes.default.svc"
		port = "443"
	}

	return &Client{
		baseURL: fmt.Sprintf("https://%s:%s", host, port),
		token:   token,
		ctx:     ctx,
		httpClient: &http.Client{
			Timeout:   30 * time.Second,
			Transport: sharedTransport,
		},
	}
}

// do sends one authenticated request to the Kubernetes API. query values are
// added to the path's own query. Error responses are returned as *K8sError
// together with their body; transport failures as *NetworkError.
func (c *Client) do(method, path, contentType string, body []byte, query url.Values) ([]byte, int, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(c.ctx, method, c.baseURL+path, reader)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if len(query) > 0 {
		q := req.URL.Query()
		for key, values := range query {
			for _, v := range values {
				q.Set(key, v)
			}
		}
		req.URL.RawQuery = q.Encode()
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, wrapNetworkError(err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if resp.StatusCode >= 400 {
		return respBody, resp.StatusCode, parseK8sError(respBody, resp.StatusCode)
	}
	return respBody, resp.StatusCode, nil
}

func (c *Client) get(path string) ([]byte, int, error) {
	return c.do(http.MethodGet, path, "", nil, nil)
}

func (c *Client) post(path string, data []byte) ([]byte, int, error) {
	return c.do(http.MethodPost, path, "application/json", data, nil)
}

func (c *Client) put(path string, data []byte) ([]byte, int, error) {
	return c.do(http.MethodPut, path, "application/json", data, nil)
}

// patch sends a JSON merge patch.
func (c *Client) patch(path string, patchData []byte) ([]byte, int, error) {
	return c.do(http.MethodPatch, path, "application/merge-patch+json", patchData, nil)
}

func (c *Client) strategicPatch(path string, patchData []byte) ([]byte, int, error) {
	return c.do(http.MethodPatch, path, "application/strategic-merge-patch+json", patchData, nil)
}

// apply server-side applies resource, taking ownership of its fields.
func (c *Client) apply(path string, resource interface{}) ([]byte, int, error) {
	return c.applyWithOptions(path, resource, false)
}

// dryRunApply validates a server-side apply without persisting it.
func (c *Client) dryRunApply(path string, resource interface{}) ([]byte, int, error) {
	return c.applyWithOptions(path, resource, true)
}

func (c *Client) applyWithOptions(path string, resource interface{}, dryRun bool) ([]byte, int, error) {
	data, err := json.Marshal(resource)
	if err != nil {
		return nil, 0, err
	}
	query := url.Values{"fieldManager": {"rhoai-nightly-updater"}, "force": {"true"}}
	if dryRun {
		query.Set("dryRun", "All")
	}
	return c.do(http.MethodPatch, path, "application/apply-patch+yaml", data, query)
}

func (c *Client) delete(path string) (int, error) {
	_, status, err := c.do(http.MethodDelete, path, "", nil, nil)
	return status, err
}

// namespacedPath builds a K8s API path for a namespaced resource.
// Example: namespacedPath("operators.coreos.com/v1alpha1", "subscriptions", "ns", "name")
// returns "/apis/operators.coreos.com/v1alpha1/namespaces/ns/subscriptions/name".
func namespacedPath(apiGroup, resource, namespace, name string) string {
	prefix := "/apis"
	if apiGroup == "v1" {
		prefix = "/api"
	}
	if name == "" {
		return fmt.Sprintf("%s/%s/namespaces/%s/%s", prefix, apiGroup, namespace, resource)
	}
	return fmt.Sprintf("%s/%s/namespaces/%s/%s/%s", prefix, apiGroup, namespace, resource, name)
}

// clusterPath builds a K8s API path for a cluster-scoped resource.
// Example: clusterPath("config.openshift.io/v1", "clusterversions", "version")
// returns "/apis/config.openshift.io/v1/clusterversions/version".
func clusterPath(apiGroup, resource, name string) string {
	prefix := "/apis"
	if apiGroup == "v1" {
		prefix = "/api"
	}
	if name == "" {
		return fmt.Sprintf("%s/%s/%s", prefix, apiGroup, resource)
	}
	return fmt.Sprintf("%s/%s/%s/%s", prefix, apiGroup, resource, name)
}

// NetworkError wraps a transport-level error (DNS, TLS, connection refused)
// so callers can identify network failures via errors.As.
type NetworkError struct {
	Err error
}

func (e *NetworkError) Error() string {
	return fmt.Sprintf("network error: %s", e.Err)
}

func (e *NetworkError) Unwrap() error {
	return e.Err
}

// wrapNetworkError wraps transport-level errors (DNS, TLS, connection refused)
// so callers can identify network failures.
func wrapNetworkError(err error) error {
	return &NetworkError{Err: err}
}

// IsNetworkError reports whether the error originated from a network-level failure.
func IsNetworkError(err error) bool {
	var netErr *NetworkError
	return errors.As(err, &netErr)
}

// getUser returns the authenticated username set on the client.
func getUser(c *Client) string {
	if c.username != "" {
		return c.username
	}
	return "unknown"
}

// IsK8sError checks if the error is a K8sError and optionally matches a status code.
func IsK8sError(err error, statusCode int) bool {
	var k8sErr *K8sError
	if errors.As(err, &k8sErr) {
		return statusCode == 0 || k8sErr.Status == statusCode
	}
	return false
}

// GetVersion calls the /version endpoint to verify API server reachability.
// This is a lightweight, unauthenticated-friendly call suitable for readiness probes.
func (c *Client) GetVersion() (string, error) {
	body, _, err := c.get("/version")
	if err != nil {
		return "", fmt.Errorf("kubernetes API unreachable: %w", err)
	}
	var info struct {
		GitVersion string `json:"gitVersion"`
	}
	if err := json.Unmarshal(body, &info); err != nil {
		return "", fmt.Errorf("failed to parse version response: %w", err)
	}
	return info.GitVersion, nil
}

// HTTPStatusForError maps a cluster read error to the HTTP status and
// errorCode a handler returns, so the UI can tell missing permissions,
// throttling and outages apart instead of seeing a generic 500.
func HTTPStatusForError(err error) (int, string) {
	var k8sErr *K8sError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, "timeout"
	case errors.Is(err, context.Canceled):
		// The caller went away; nobody reads this status.
		return http.StatusServiceUnavailable, "canceled"
	case errors.As(err, &k8sErr):
		switch {
		case k8sErr.Status == http.StatusUnauthorized:
			return http.StatusUnauthorized, "unauthorized"
		case k8sErr.Status == http.StatusForbidden:
			return http.StatusForbidden, "forbidden"
		case k8sErr.Status == http.StatusNotFound:
			return http.StatusNotFound, "not_found"
		case k8sErr.Status == http.StatusTooManyRequests:
			return http.StatusServiceUnavailable, "rate_limited"
		case k8sErr.Status >= 500:
			return http.StatusBadGateway, "cluster_unavailable"
		}
		return http.StatusBadGateway, "cluster_error"
	case IsNetworkError(err):
		return http.StatusBadGateway, "network"
	}
	return http.StatusInternalServerError, "internal"
}
