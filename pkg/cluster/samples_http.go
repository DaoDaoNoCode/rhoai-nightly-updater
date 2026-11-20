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
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// The MLflow samples call a server that is not the Kubernetes API: MLflow,
// at the address its CR reports (the data-science gateway,
// https://rh-ai.<apps>/mlflow). It authorizes the bearer token itself: it
// runs the kubernetes-auth app of mlflow-kubernetes-plugins v2.1.1 (the
// version the RHOAI 3.6/3.7 image pins,
// requirements/konflux-pypi-requirements.txt:4) in SelfSubjectAccessReview
// mode (mlflow-operator rhoai-3.6 charts/mlflow/templates/
// deployment.yaml:201-202). Each request becomes an SSAR with the caller's
// token on group mlflow.kubeflow.org in the namespace the
// X-MLflow-Workspace header names (auth/authorizer.py:276-343). A request is
// checked without a name first; only when that is denied are the names
// resolved and checked one by one (auth/core.py:812-851), so RBAC
// resourceNames work on these pseudo-resources.
//
// The app sends its own token: the ServiceAccount's in the cluster, the
// developer's (DEV_TOKEN) in DEV_MODE. MLflow is reached through the
// cluster's gateway, never through a Service or Route in a user's project:
// a token sent there could be captured by whoever can run a pod behind it.
// The sample pipeline is therefore run by the user with their own token
// (samples_pipeline.go). Checked live on RHOAI 3.6: a ServiceAccount token
// reaches MLflow through the gateway, which answers 403 without the rights.

// samplesTLSConfig trusts the system roots (publicly signed gateway
// certificates, as on ROSA), the pod's ca.crt, and
// KUBE_CA_FILE when set. On OpenShift ca.crt also holds the default ingress
// CA: kube-controller-manager's root-ca-file is the serviceaccount-ca bundle
// (cluster-kube-controller-manager-operator bindata/assets/config/
// defaultconfig.yaml:38-39), which combines kube-apiserver-server-ca with
// default-ingress-cert (pkg/operator/targetconfigcontroller/
// targetconfigcontroller.go:802-811). So the gateway's default certificate
// verifies in the pod. Verification is skipped only with DEV_MODE and
// DEV_INSECURE_TLS, as for the API server (client.go apiServerTLSConfig).
func samplesTLSConfig(getenv func(string) string, readFile func(string) ([]byte, error)) *tls.Config {
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	for _, f := range []string{getenv("KUBE_CA_FILE"), inClusterCAFile} {
		if f == "" {
			continue
		}
		if pem, err := readFile(f); err == nil {
			pool.AppendCertsFromPEM(pem)
		}
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}
	if getenv("DEV_MODE") == "true" && getenv("DEV_INSECURE_TLS") == "true" {
		cfg.InsecureSkipVerify = true
	}
	return cfg
}

// samplesHTTPClient sends the MLflow requests. The demo generator
// took 27 s live, so a request may take up to two minutes; the operation's
// own deadline still applies through the request context. Tests replace it.
// It never follows a redirect: the request carries the app's token, which
// Go's client would resend on a same-host redirect (also to plain http), so
// a 3xx answer is returned as it is and reported as an error
// (redirectError).
var samplesHTTPClient = &http.Client{
	Timeout:       2 * time.Minute,
	CheckRedirect: refuseRedirect,
	Transport: &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		TLSClientConfig:     samplesTLSConfig(os.Getenv, os.ReadFile),
		ForceAttemptHTTP2:   true,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	},
}

// refuseRedirect makes the client return a 3xx answer instead of following
// it (as fetchConfigLabels' client in image_labels.go, which strips the
// token instead).
func refuseRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// isRedirect: a 3xx answer, which the clients above do not follow.
func isRedirect(status int) bool { return status >= 300 && status < 400 }

// redirectMessage says, in words, that a 3xx was not followed, and where it
// pointed (shortened; the Location holds no token).
func redirectMessage(resp *http.Response) string {
	loc := resp.Header.Get("Location")
	if len(loc) > 200 {
		loc = loc[:200] + "..."
	}
	if loc == "" {
		loc = "another address"
	}
	return fmt.Sprintf("a redirect to %s, which this tool does not follow (its token is sent only to the address it called). Check the address the cluster reports for this server", loc)
}

// sampleAPI is one server the samples call: its base URL, the bearer token
// and fixed headers (the MLflow workspace).
type sampleAPI struct {
	ctx     context.Context
	base    string
	token   string
	headers map[string]string
	server  string // "MLflow", for messages
}

// sampleAPIError is a non-2xx answer. Code is MLflow's error_code
// (RESOURCE_DOES_NOT_EXIST, PERMISSION_DENIED, ENDPOINT_NOT_FOUND, ...) or
// empty; Message is the server's message, shortened. Neither carries the
// token, which is only ever sent in the Authorization header.
type sampleAPIError struct {
	Status  int
	Code    string
	Message string
	server  string
	// fastAPINotFound: the body was exactly FastAPI's {"detail":"Not Found"}.
	fastAPINotFound bool
}

func (e *sampleAPIError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	if e.Code != "" {
		return fmt.Sprintf("%s answered %d %s: %s", e.server, e.Status, e.Code, msg)
	}
	return fmt.Sprintf("%s answered %d: %s", e.server, e.Status, msg)
}

func asSampleAPIError(err error) (*sampleAPIError, bool) {
	var e *sampleAPIError
	ok := errors.As(err, &e)
	return e, ok
}

// isNotFound: the object does not exist, as MLflow says it: 404
// RESOURCE_DOES_NOT_EXIST (MlflowException's code for a missing experiment,
// model, version, MCP server or skill). Any other 404 is not this: a route
// the server does not know (isEndpointMissing), or a 404 without MLflow's
// error body, such as the gateway's while MLflow restarts, which says
// nothing about the object.
func isNotFound(err error) bool {
	e, ok := asSampleAPIError(err)
	return ok && e.Status == http.StatusNotFound && e.Code == "RESOURCE_DOES_NOT_EXIST"
}

// isEndpointMissing: the server has no such API. Only two answers say so:
// the kubernetes-auth plugin's 404 ENDPOINT_NOT_FOUND for a route without
// an authorization rule, sent before any access review (auth/core.py:
// 700-714), and FastAPI's own {"detail":"Not Found"} for a route it does
// not have. A 404 with any other body (an empty one from the gateway
// during a redeploy) is neither, so it stays an error.
func isEndpointMissing(err error) bool {
	e, ok := asSampleAPIError(err)
	return ok && e.Status == http.StatusNotFound && (e.Code == "ENDPOINT_NOT_FOUND" || e.fastAPINotFound)
}

// isAlreadyExists: MLflow's RESOURCE_ALREADY_EXISTS, or HTTP 409.
func isAlreadyExists(err error) bool {
	e, ok := asSampleAPIError(err)
	return ok && (e.Code == "RESOURCE_ALREADY_EXISTS" || e.Status == http.StatusConflict)
}

// isDenied: the server refused the token (401) or the access review (403).
func isDenied(err error) bool {
	e, ok := asSampleAPIError(err)
	return ok && (e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden)
}

// sampleErrorCode maps a samples call error to an operation errorCode.
func sampleErrorCode(err error) string {
	switch {
	case isDenied(err):
		return "forbidden"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	}
	if _, ok := asSampleAPIError(err); ok {
		return "sample_api"
	}
	return "network"
}

// deniedHint explains a 401/403 from MLflow, which decides with access
// reviews of the caller's token: the signed-in user's own for a change
// (asUser), the ServiceAccount's for the status.
func deniedHint(err error, asUser bool) string {
	if !isDenied(err) {
		return ""
	}
	if asUser {
		return " MLflow refused your own OpenShift permissions for this call (the updater makes changes as you), or your sign-in expired: log out and log in again, or ask for the rights in this project."
	}
	return " The updater's ServiceAccount lacks the rights for this call: re-apply the template (deploy/template.yaml) so its ClusterRole has the samples rules. MLflow caches access decisions for up to 5 minutes, so a change can take that long to apply."
}

// deniedReadHint explains a refused read of the samples status, which is
// made with the signed-in user's own token (GetMLflowSamplesStatus): the
// user may not see it, nothing for an admin to re-apply.
func deniedReadHint(err error) string {
	if !isDenied(err) {
		return ""
	}
	return " MLflow refused your own OpenShift permissions for this read: you cannot see this in the project's MLflow workspace, or your sign-in expired (log out and log in again)."
}

const maxSampleErrorMessage = 300

// parseSampleAPIError reads an error body: MLflow's {"error_code","message"},
// grpc-gateway's {"code","message"} (KFP), FastAPI's {"detail"}, or text
// (kube-rbac-proxy answers "Forbidden (user=..., verb=..., ...)").
func parseSampleAPIError(server string, status int, body []byte) *sampleAPIError {
	e := &sampleAPIError{Status: status, server: server}
	var parsed struct {
		ErrorCode string          `json:"error_code"`
		Message   string          `json:"message"`
		Error     string          `json:"error"`
		Detail    json.RawMessage `json:"detail"`
	}
	if json.Unmarshal(body, &parsed) == nil {
		e.Code = parsed.ErrorCode
		e.Message = firstNonEmpty(parsed.Message, parsed.Error)
		if e.Message == "" && len(parsed.Detail) > 0 {
			var s string
			if json.Unmarshal(parsed.Detail, &s) == nil {
				e.Message = s
				e.fastAPINotFound = e.Code == "" && s == "Not Found"
			} else {
				e.Message = string(parsed.Detail)
			}
		}
	} else {
		e.Message = strings.TrimSpace(string(body))
	}
	if len(e.Message) > maxSampleErrorMessage {
		e.Message = e.Message[:maxSampleErrorMessage] + "..."
	}
	return e
}

// send makes one request. body is sent as is with contentType; out, when
// not nil, receives the JSON answer.
func (a *sampleAPI) send(method, path string, query url.Values, contentType string, body []byte, out any) error {
	u := a.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(a.ctx, method, u, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, v := range a.headers {
		req.Header.Set(k, v)
	}
	resp, err := samplesHTTPClient.Do(req)
	if err != nil {
		// url.Error repeats the URL, which holds no secret.
		return fmt.Errorf("cannot reach %s: %w", a.server, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return fmt.Errorf("read the answer of %s: %w", a.server, err)
	}
	if isRedirect(resp.StatusCode) {
		return &sampleAPIError{Status: resp.StatusCode, server: a.server, Message: redirectMessage(resp)}
	}
	if resp.StatusCode >= 300 {
		return parseSampleAPIError(a.server, resp.StatusCode, respBody)
	}
	if out != nil && len(bytes.TrimSpace(respBody)) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("parse the answer of %s to %s %s: %w", a.server, method, path, err)
		}
	}
	return nil
}

// sendJSON sends in as JSON (nil sends no body).
func (a *sampleAPI) sendJSON(method, path string, query url.Values, in, out any) error {
	if in == nil {
		return a.send(method, path, query, "", nil, out)
	}
	data, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return a.send(method, path, query, "application/json", data, out)
}
