// Package kube is the Kubernetes API client of the cluster packages: the
// Client and its transport, the API errors and how a handler reports them,
// the REST helpers, the clock every wait runs on and the cache registry.
// It imports no other package of pkg/cluster.
package kube

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
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"
)

// MaxResponseBody is the upper limit on K8s API response body reads (50 MB).
const MaxResponseBody = 50 << 20

// InClusterCAFile is the service account CA bundle mounted into every pod.
const InClusterCAFile = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"

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
	if pool, err := loadCAPool(readFile, InClusterCAFile); err == nil {
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
	// DetailsKind and DetailsName are the Status's details: the object a
	// 404 is about ("useroauthaccesstokens", "sha256~…"), empty when the
	// resource type itself is not served.
	DetailsKind string
	DetailsName string
	// User: the request carried the signed-in user's own token
	// (NewUserClient), so a 401 or 403 is about that user's sign-in or
	// permissions, not the app's RBAC.
	User bool
}

// Messages of a 401 and a 403 answered to the signed-in user's own token in
// the middle of an operation (K8sError.Error).
const (
	UserTokenRejectedMessage = "your OpenShift sign-in is no longer accepted (it expired or was revoked), so the operation stopped here. Log out of the updater and log in again, then check what the operation changed before you retry"
	UserForbiddenMessage     = "your OpenShift account is not allowed to do this: the updater makes every change with your own permissions"
)

func (e *K8sError) Error() string {
	msg := fmt.Sprintf("k8s API error %d (%s)", e.Status, e.Reason)
	if e.K8sMessage != "" {
		msg += ": " + e.K8sMessage
	}
	switch {
	case e.User && e.Status == http.StatusUnauthorized:
		msg += "; " + UserTokenRejectedMessage
	case e.User && e.Status == http.StatusForbidden:
		msg += "; " + UserForbiddenMessage
	}
	return msg
}

// ParseK8sError tries to parse a Kubernetes Status object from the response body.
// If parsing fails, it returns a generic error with the status code.
func ParseK8sError(body []byte, statusCode int) error {
	var status struct {
		Kind    string `json:"kind"`
		Status  string `json:"status"`
		Message string `json:"message"`
		Reason  string `json:"reason"`
		Code    int    `json:"code"`
		Details struct {
			Name string `json:"name"`
			Kind string `json:"kind"`
		} `json:"details"`
	}
	if err := json.Unmarshal(body, &status); err == nil && status.Kind == "Status" && status.Status == "Failure" {
		return &K8sError{
			Status:      statusCode,
			Reason:      status.Reason,
			K8sMessage:  status.Message,
			DetailsKind: status.Details.Kind,
			DetailsName: status.Details.Name,
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
	// bookkeeper, when set, is the client of the updater's own records
	// (see bookkeeping).
	bookkeeper *Client
	// asUser: the token is the signed-in user's own (NewUserClient), not
	// the app's ServiceAccount's. Errors then say that the user's sign-in
	// or permissions, not the app's RBAC, refused a request.
	asUser bool
	// signedInUser, when set, is the client of the signed-in user's own
	// token for a ServiceAccount client (see SetSignedInUser).
	signedInUser *Client
	// statusRead: the client serves a read request (pkg/api withAuth),
	// whose pages poll every few seconds, so an expensive read may reuse a
	// recent answer (argoApplicationsCache). Clients of changes never set
	// it: every check that refuses or allows a change reads fresh.
	statusRead bool
	// noRetry, when set, keeps an answer the API server asks to repeat from
	// being retried when it reports true for its error (NoRetryWhen).
	noRetry func(error) bool
}

// NewUserClient is NewClientWithContext for the signed-in user's own token:
// the client every cluster change is made with (pkg/api withMutationAuth).
func NewUserClient(ctx context.Context, token string) *Client {
	c := NewClientWithContext(ctx, token)
	c.asUser = true
	return c
}

// ActsAsUser reports whether the client carries the signed-in user's own
// token (NewUserClient) rather than the app's ServiceAccount token.
func (c *Client) ActsAsUser() bool { return c.asUser }

// SetSignedInUser gives a ServiceAccount client (a read request's, pkg/api
// withAdminRead) the client of the signed-in user's own token (NewUserClient),
// for the reads that must not use the ServiceAccount's RBAC: the hard reset's
// namespace guard reads owners of any kind (namespaceOwnerBlocker).
func (c *Client) SetSignedInUser(u *Client) { c.signedInUser = u }

// SignedInUser returns the client that acts with the signed-in user's
// own token: c itself when it carries it (a change's client), else the one
// SetSignedInUser gave, with c's context and user; nil when there is none
// (the gitops profile's Update, a background job).
func (c *Client) SignedInUser() *Client {
	if c.asUser {
		return c
	}
	if c.signedInUser == nil {
		return nil
	}
	u := *c.signedInUser
	u.ctx, u.signedInUser, u.bookkeeper = c.ctx, nil, c.bookkeeper
	if u.username == "" {
		u.username = c.username
	}
	return &u
}

// SetStatusRead marks the client as one of a read request (pkg/api
// withAuth): it may reuse a recent answer of the reads that allow it. Never
// for a change's client.
func (c *Client) SetStatusRead() { c.statusRead = true }

// StatusRead reports whether SetStatusRead marked the client.
func (c *Client) StatusRead() bool { return c.statusRead }

// SetUsername sets the logged-in user identity for audit logging.
func (c *Client) SetUsername(username string) {
	c.username = username
}

// SetBookkeeping makes b (the ServiceAccount's client) the client of the
// updater's own records: the activity, snapshot and operation ConfigMaps in
// its namespace (see bookkeeping).
func (c *Client) SetBookkeeping(b *Client) {
	c.bookkeeper = b
}

// bookkeeping returns the client for the updater's own records in its
// namespace (the activity log, the snapshot and the operation record): the
// ServiceAccount's client set with SetBookkeeping, with this client's
// context and user, or this client itself when none is set (a read, a
// background job and dev mode run with that token anyway). The records are
// the app's, not the user's: they must be written whatever the user's
// token may do, and also when it stops working mid-operation. Every
// request to those ConfigMaps goes through it (TestBookkeepingUsesTheServiceAccount).
func (c *Client) Bookkeeping() *Client {
	if c.bookkeeper == nil {
		return c
	}
	b := *c.bookkeeper
	// The records are the updater's, not a change the user made: they do
	// not count in the operation's Changes.
	b.ctx, b.bookkeeper, b.asUser = withoutChanges(c.ctx), nil, false
	if b.username == "" {
		b.username = c.username
	}
	return &b
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
		// An IPv6 KUBERNETES_SERVICE_HOST needs brackets, as in client-go's
		// rest.InClusterConfig.
		baseURL: "https://" + net.JoinHostPort(host, port),
		token:   token,
		ctx:     ctx,
		httpClient: &http.Client{
			Timeout:   30 * time.Second,
			Transport: sharedTransport,
		},
	}
}

// Do sends one authenticated request to the Kubernetes API. query values are
// added to the path's own query. Error responses are returned as *K8sError
// together with their body; transport failures as *NetworkError. A response
// the API server asks to repeat later is sent again (see retryWait).
func (c *Client) Do(method, path, contentType string, body []byte, query url.Values) ([]byte, int, error) {
	respBody, _, code, err := c.doWithHeader(method, path, contentType, body, query)
	return respBody, code, err
}

// Retries of a request the API server asks to repeat (retryWait), as
// client-go's rest client makes them (k8s.io/client-go v0.33.4).
const (
	// maxRetries is client-go's default (rest/request.go:170): at most 10
	// retries, 11 attempts (rest/with_retry.go:161-165).
	maxRetries = 10
	// retryWaitBudget bounds the summed Retry-After waits of one request.
	// client-go has none (rest/urlbackoff.go:86 sleeps the full value each
	// time). 32 s is the largest Retry-After the kube-apiserver sends
	// (k8s.io/apiserver v0.33.4): an unready watch cache asks for up to
	// 30 s (30 once the cache has been down about a minute;
	// pkg/storage/cacher/util.go:51-56), priority and fairness for up to
	// 32 s (pkg/util/flowcontrol/dropped_requests_tracker.go:29). A wait
	// over the budget is not shortened: the answer is returned instead.
	retryWaitBudget = 32 * time.Second
)

// NoRetryWhen returns a copy of c whose requests are not retried when skip
// reports true for the error of an answer the API server asks to repeat
// (retryWait); skip replaces any predicate set on c before. For a read
// whose caller handles that answer at once: a versioned RHOAI read gets
// "429 storage is (re)initializing: ... conversion webhook ... failed" with
// Retry-After 30 while the watch cache cannot convert the stored objects,
// which never clears by itself, and reads the next served version instead
// (rhoai.readServed).
func (c *Client) NoRetryWhen(skip func(error) bool) *Client {
	cp := *c
	cp.noRetry = skip
	return &cp
}

// doWithHeader is Do that also returns the response's header (nil when no
// response came). An answer the API server asks to repeat (retryWait) is
// sent again after the wait it asks for, as client-go does
// (rest/request.go:1242-1286); the body is a []byte, so every attempt sends
// it in full. When the client's context ends during a wait, the context's
// error is returned with the last answer's in its text (client-go's
// WrapPreviousError, rest/with_retry.go:268-305).
func (c *Client) doWithHeader(method, path, contentType string, body []byte, query url.Values) ([]byte, http.Header, int, error) {
	var waited time.Duration
	for retries := 0; ; retries++ {
		respBody, header, code, err := c.doOnce(method, path, contentType, body, query)
		wait, retry := c.retryWait(method, header, code, err, retries, waited)
		if !retry {
			return respBody, header, code, err
		}
		// No path: an object name can be secret (ReadSessionToken's path
		// names the user's OAuth access token by its digest).
		slog.Debug("the API server asked to retry the request", "method", method, "status", code, "retryAfter", wait, "retry", retries+1)
		if sleepErr := clk.Sleep(c.ctx, wait); sleepErr != nil {
			return nil, nil, 0, &retryStoppedError{ctxErr: sleepErr, last: err}
		}
		waited += wait
	}
}

// retryStoppedError is the context's error that ended a retry wait, with
// the last answer in its text only, as client-go's wrapPreviousError
// (rest/with_retry.go:298-305): errors.Is and As see the context's error,
// so the request reads as cancelled or timed out, not as that answer.
type retryStoppedError struct{ ctxErr, last error }

func (e *retryStoppedError) Error() string {
	return e.ctxErr.Error() + " - error from a previous attempt: " + e.last.Error()
}

func (e *retryStoppedError) Unwrap() error { return e.ctxErr }

// retryWait reports whether an answer is sent again and after how long.
// As in client-go (rest/with_retry.go:148-193, checkWait :309-318,
// retryAfterSeconds rest/request.go:1542-1549): an error status 429, or 500
// and above, with Retry-After in whole seconds, at most maxRetries times. A
// 429 or 5xx without that header is returned at once, and so is any other
// 4xx. Unlike client-go, a 5xx is retried only for a read (GET, HEAD): a
// write the server answered 5xx may have been applied, while a 429 is
// refused before the request is processed. The waits of one request are
// bounded by retryWaitBudget, a wait longer than what is left of the
// client's context deadline is not begun (the answer is returned rather
// than the deadline's error), and NoRetryWhen's skip applies.
func (c *Client) retryWait(method string, header http.Header, code int, err error, retries int, waited time.Duration) (time.Duration, bool) {
	if _, ok := errors.AsType[*K8sError](err); !ok {
		return 0, false
	}
	read := method == http.MethodGet || method == http.MethodHead
	if code != http.StatusTooManyRequests && (code < http.StatusInternalServerError || !read) {
		return 0, false
	}
	secs, convErr := strconv.Atoi(header.Get("Retry-After"))
	if convErr != nil || retries >= maxRetries {
		return 0, false
	}
	if secs > int(retryWaitBudget/time.Second) {
		return 0, false // also keeps a huge value from overflowing
	}
	wait := time.Duration(max(secs, 0)) * time.Second
	if waited+wait > retryWaitBudget || (c.noRetry != nil && c.noRetry(err)) {
		return 0, false
	}
	if deadline, ok := c.ctx.Deadline(); ok && clk.Until(deadline) < wait {
		return 0, false
	}
	return wait, true
}

// doOnce sends the request once.
func (c *Client) doOnce(method, path, contentType string, body []byte, query url.Values) ([]byte, http.Header, int, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(c.ctx, method, c.baseURL+path, reader)
	if err != nil {
		return nil, nil, 0, err
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

	// The API server ignores the query's options of a DELETE with a body
	// (IsDryRun): a dry run asked for in the query would delete.
	if method == http.MethodDelete && len(body) > 0 && req.URL.Query().Has("dryRun") {
		return nil, nil, 0, errors.New("refused to send a DELETE with DeleteOptions and dryRun in its query: the API server reads only the body's options, so it would not be a dry run; put dryRun in the DeleteOptions")
	}
	answered := startAPIRequest(c.ctx, method, req.URL, body)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		answered(0)
		return nil, nil, 0, wrapNetworkError(err)
	}
	answered(resp.StatusCode)
	defer resp.Body.Close()

	// One byte more than the limit tells a response over it from one that
	// fits, which would otherwise come back cut and fail later as JSON.
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBody+1))
	if err != nil {
		return nil, resp.Header, resp.StatusCode, err
	}
	if len(respBody) > MaxResponseBody {
		return nil, resp.Header, resp.StatusCode, fmt.Errorf("%s %s: the API server's response is larger than %d MiB", method, path, MaxResponseBody>>20)
	}
	if resp.StatusCode >= 400 {
		err := ParseK8sError(respBody, resp.StatusCode)
		if k, ok := errors.AsType[*K8sError](err); ok {
			k.User = c.asUser
		}
		return respBody, resp.Header, resp.StatusCode, err
	}
	return respBody, resp.Header, resp.StatusCode, nil
}

func (c *Client) Get(path string) ([]byte, int, error) {
	return c.Do(http.MethodGet, path, "", nil, nil)
}

func (c *Client) Post(path string, data []byte) ([]byte, int, error) {
	return c.Do(http.MethodPost, path, "application/json", data, nil)
}

func (c *Client) Put(path string, data []byte) ([]byte, int, error) {
	return c.Do(http.MethodPut, path, "application/json", data, nil)
}

// Patch sends a JSON merge patch.
func (c *Client) Patch(path string, patchData []byte) ([]byte, int, error) {
	return c.Do(http.MethodPatch, path, "application/merge-patch+json", patchData, nil)
}

func (c *Client) StrategicPatch(path string, patchData []byte) ([]byte, int, error) {
	return c.Do(http.MethodPatch, path, "application/strategic-merge-patch+json", patchData, nil)
}

// Apply server-side applies resource, taking ownership of its fields.
func (c *Client) Apply(path string, resource any) ([]byte, int, error) {
	return c.applyWithOptions(path, resource, false)
}

// DryRunApply validates a server-side apply without persisting it.
func (c *Client) DryRunApply(path string, resource any) ([]byte, int, error) {
	return c.applyWithOptions(path, resource, true)
}

func (c *Client) applyWithOptions(path string, resource any, dryRun bool) ([]byte, int, error) {
	data, err := json.Marshal(resource)
	if err != nil {
		return nil, 0, err
	}
	query := url.Values{"fieldManager": {"rhoai-nightly-updater"}, "force": {"true"}}
	if dryRun {
		query.Set("dryRun", "All")
	}
	return c.Do(http.MethodPatch, path, "application/apply-patch+yaml", data, query)
}

func (c *Client) Delete(path string) (int, error) {
	_, status, err := c.Do(http.MethodDelete, path, "", nil, nil)
	return status, err
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

// GetUser returns the authenticated username set on the client.
func GetUser(c *Client) string {
	if c.username != "" {
		return c.username
	}
	return "unknown"
}

// IsK8sError checks if the error is a K8sError and optionally matches a status code.
func IsK8sError(err error, statusCode int) bool {
	if k8sErr, ok := errors.AsType[*K8sError](err); ok {
		return statusCode == 0 || k8sErr.Status == statusCode
	}
	return false
}

// GetVersion calls the /version endpoint to verify API server reachability.
// This is a lightweight, unauthenticated-friendly call suitable for readiness probes.
func (c *Client) GetVersion() (string, error) {
	body, _, err := c.Get("/version")
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

// ErrorCodeForbiddenForUser is the errorCode of a read the signed-in
// user's own token was refused (pkg/api withUserRead): the user may not
// see this. The UI shows "not yours to see", not the ServiceAccount's
// "template out of date" hint (TemplateHint).
const ErrorCodeForbiddenForUser = "forbidden_for_user"

// registryTokenError is an error of a container registry's token endpoint
// (the Quay token request's quayAuthError): CredentialsRejected tells
// rejected credentials from an outage.
type registryTokenError interface {
	error
	CredentialsRejected() bool
}

// HTTPStatusForError maps a cluster read error to the HTTP status and
// errorCode a handler returns, so the UI can tell missing permissions,
// throttling and outages apart instead of seeing a generic 500.
func HTTPStatusForError(err error) (int, string) {
	var k8sErr *K8sError
	var regErr registryTokenError
	switch {
	case errors.As(err, &regErr):
		if regErr.CredentialsRejected() {
			return http.StatusBadGateway, "registry_auth"
		}
		return http.StatusBadGateway, "registry_unavailable"
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, "timeout"
	case errors.Is(err, context.Canceled):
		// The caller went away; nobody reads this status.
		return http.StatusServiceUnavailable, "canceled"
	case errors.As(err, &k8sErr):
		switch {
		case k8sErr.Status == http.StatusUnauthorized && k8sErr.User:
			// The user's own sign-in, not the app's credentials.
			return http.StatusUnauthorized, "session_expired"
		case k8sErr.Status == http.StatusUnauthorized:
			return http.StatusUnauthorized, "unauthorized"
		case k8sErr.Status == http.StatusForbidden && k8sErr.User:
			// The user's own RBAC: they may not see this. Not the app's
			// RBAC ("forbidden"), which an admin fixes with make upgrade.
			return http.StatusForbidden, ErrorCodeForbiddenForUser
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
