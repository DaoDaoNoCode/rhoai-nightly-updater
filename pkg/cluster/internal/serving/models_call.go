package serving

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/mlflow"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// "Can it be called?": after a model is Ready, this app sends it one
// minimal request through the way it is reached, as a user would, and
// reports what answered. A model can be Ready and still not answer a
// caller: a teammate's chat got 404 (live, RHOAI 3.6, 2026-11-18), and the
// two 404s mean different things:
//   - 404 with vLLM's JSON error, {"error":{"message":"The model `X` does
//     not exist","type":"NotFoundError","code":404}}: the request reached the
//     model, but its "model" was not the served name (spec.model.name; the
//     display name or the Hugging Face id are not it);
//   - 404 with an empty body: nothing answers on that path; the request did
//     not reach the model (the gateway's route is /<project>/<model>/...).
//
// The request carries the model's own token: the Secret this tool made as
// the dashboard's "Require token authentication" does (models_token.go),
// whose ServiceAccount may only get this one model. Never the user's token
// nor this app's: the model's pod is in a project its editors control, and
// whoever controls what answers at its address sees the token (SECURITY.md,
// "Which token is used where"). The token is read for one check, sent only
// in the Authorization header to the https address KServe reports, never
// logged or kept. An internal-only model is called without a token (its
// in-cluster address is not authenticated). A MaaS model is called with a
// MaaS API key, which this tool does not create: MaaS's own readiness of
// the model is reported instead.

var (
	// ModelCallRetryWindow bounds how long a check keeps retrying answers
	// that can be transient right after a model became Ready: the gateway
	// has not loaded the route yet (404 without a body, 503), a new
	// hostname is not in DNS yet, a connection fails.
	ModelCallRetryWindow = 90 * time.Second
	ModelCallPoll        = 5 * time.Second
)

// serviceCAFile is the service CA OpenShift mounts in every pod: it signs
// the serving certificates service-ca issues (a gateway's in-cluster
// listener).
const serviceCAFile = "/var/run/secrets/kubernetes.io/serviceaccount/service-ca.crt"

// modelCallTLSConfig trusts what mlflow.SamplesTLSConfig trusts (system roots, the
// pod's ca.crt with the default ingress CA) and the service CA.
func modelCallTLSConfig(getenv func(string) string, readFile func(string) ([]byte, error)) *tls.Config {
	cfg := mlflow.SamplesTLSConfig(getenv, readFile)
	if pem, err := readFile(serviceCAFile); err == nil {
		if cfg.RootCAs == nil {
			cfg.RootCAs = x509.NewCertPool()
		}
		cfg.RootCAs.AppendCertsFromPEM(pem)
	}
	return cfg
}

// modelCallHTTPClient sends the check's request. vLLM on CPU answers a
// one-token request within seconds; the first one after start can take
// longer. It never follows a redirect (the token goes only to the address
// called). Tests replace it.
var modelCallHTTPClient = &http.Client{
	Timeout:       45 * time.Second,
	CheckRedirect: mlflow.RefuseRedirect,
	Transport: &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		TLSClientConfig:     modelCallTLSConfig(os.Getenv, os.ReadFile),
		TLSHandshakeTimeout: 10 * time.Second,
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	},
}

// modelCallRequest is the minimal request for a model of that type: one
// token of chat, or one tiny embedding.
func modelCallRequest(base, served, typ string) (string, []byte) {
	base = strings.TrimRight(base, "/")
	if typ == ModelTypeEmbedding {
		body, _ := json.Marshal(map[string]any{"model": served, "input": []string{"hi"}})
		return base + "/v1/embeddings", body
	}
	body, _ := json.Marshal(map[string]any{"model": served, "messages": []any{map[string]any{"role": "user", "content": "hi"}}, "max_tokens": 1})
	return base + "/v1/chat/completions", body
}

// callModelOnce sends the request; the answer's body is read up to 4 KiB.
func callModelOnce(ctx context.Context, target string, body []byte, token string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := modelCallHTTPClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, data, nil
}

// vllmNotFound: the body is vLLM's (OpenAI-shaped) error for a model name
// it does not serve.
// Newer vLLM nests it ({"error":{"type":"NotFoundError",...}}); older vLLM
// answers it at the top level ({"object":"error","type":"NotFoundError",
// "message":...}).
func vllmNotFound(body []byte) bool {
	var e struct {
		Object  string `json:"object"`
		Type    string `json:"type"`
		Message string `json:"message"`
		Error   struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) != nil {
		return false
	}
	if e.Error.Type == "NotFoundError" || strings.Contains(e.Error.Message, "does not exist") {
		return true
	}
	return e.Object == "error" && (e.Type == "NotFoundError" || strings.Contains(e.Message, "does not exist"))
}

// requestWords names the check's request by the path it calls.
func requestWords(target string) string {
	if strings.HasSuffix(target, "/v1/embeddings") {
		return "An embedding request"
	}
	return "A chat request"
}

// bodyExcerpt is an answer's body for a message: one line, shortened.
func bodyExcerpt(body []byte) string {
	s := strings.Join(strings.Fields(string(body)), " ")
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

// ClassifyModelCall turns one answer (or the error of a request without
// one) into a check. transient: it may change on its own soon.
func ClassifyModelCall(status int, body []byte, err error, target, served string, internal bool) (types.ModelCallCheck, bool) {
	out := types.ModelCallCheck{Status: status, URL: target}
	switch {
	case err != nil:
		var unknownCA x509.UnknownAuthorityError
		var dnsErr *net.DNSError
		var hostErr x509.HostnameError
		switch {
		case internal && (errors.As(err, &unknownCA) || errors.As(err, &hostErr)):
			out.Result = "not-checked"
			out.Message = fmt.Sprintf("Not checked: the model's in-cluster address uses a certificate this app does not trust (KServe's own), so this app cannot call it. From a pod on the cluster: curl -k %s.", target)
			return out, false
		case internal:
			out.Result = "not-checked"
			out.Message = fmt.Sprintf("Not checked: the model's in-cluster address cannot be reached from this app's namespace (%v). A NetworkPolicy in the project may admit only some namespaces; check it from a pod in the project.", err)
			return out, false
		case errors.As(err, &dnsErr):
			out.Result = "unreachable"
			out.Message = fmt.Sprintf("The address's hostname does not resolve (%s). A new gateway's DNS record can take a few minutes to be published; Check again in a few minutes.", dnsErr.Name)
			return out, true
		case errors.As(err, &unknownCA) || errors.As(err, &hostErr):
			out.Result = "unreachable"
			out.Message = fmt.Sprintf("The address's certificate is not trusted by this app (%v), so the model was not called. Callers outside the cluster may trust it; this app trusts the system's and the cluster's certificate authorities.", err)
			return out, false
		}
		out.Result = "unreachable"
		out.Message = fmt.Sprintf("No answer from %s: %v.", target, err)
		return out, true
	case status >= 200 && status < 300:
		out.Result, out.OK = "ok", true
		out.Message = fmt.Sprintf("%s for %q was answered (HTTP %d).", requestWords(target), served, status)
		return out, false
	case status == http.StatusNotFound && vllmNotFound(body):
		out.Result = "model-name"
		out.Message = fmt.Sprintf("The model's server answered 404: it does not serve a model named %q. A request must send \"model\": %q, the name the model was set up with (not its display name or its Hugging Face id). Its answer: %s", served, served, bodyExcerpt(body))
		return out, false
	case status == http.StatusNotFound:
		out.Result = "route"
		out.Message = fmt.Sprintf("No answer at %s (404, empty body): the gateway didn't route the request to the model. Right after setup the gateway can take a minute to load the route; check again shortly.", target)
		return out, true
	case status == http.StatusUnauthorized:
		out.Result = "auth"
		out.Message = "The gateway refused the model's own token (401). RHOAI's AuthPolicy on the gateway checks each caller's token: check that odh-model-controller and Red Hat Connectivity Link run, and that the model's token Secret is a current token of its ServiceAccount."
		return out, false
	case status == http.StatusForbidden:
		out.Result = "forbidden"
		out.Message = "The gateway accepted the model's token, but its ServiceAccount may not call the model (403). It needs get on this LLMInferenceService (Role <model>-view-role); setting the model up again restores it."
		return out, false
	case status >= 500:
		out.Result = "server-error"
		out.Message = fmt.Sprintf("The call failed on the server side (%d): %s", status, kube.FirstNonEmpty(bodyExcerpt(body), http.StatusText(status)))
		return out, status == http.StatusServiceUnavailable || status == http.StatusBadGateway || status == http.StatusGatewayTimeout
	}
	out.Result = "unexpected"
	out.Message = fmt.Sprintf("Unexpected answer %d: %s", status, kube.FirstNonEmpty(bodyExcerpt(body), http.StatusText(status)))
	return out, false
}

// readModelToken reads the model's own token from the Secret this tool
// made, only when it is the one the tool recorded (UID) and is a
// ServiceAccount token of the model's ServiceAccount.
func readModelToken(c *kube.Client, project, model, recordedUID string) (string, error) {
	if recordedUID == "" {
		return "", errors.New("the model has no token of its own yet (set it up again to make it)")
	}
	var sec struct {
		Metadata kube.ObjectMeta   `json:"metadata"`
		Type     string            `json:"type"`
		Data     map[string]string `json:"data"`
	}
	if err := c.GetInto(tokenSecretPathOf(project, model), &sec); err != nil {
		if kube.IsDecodeError(err) {
			return "", fmt.Errorf("parse the model's token Secret: %w", err)
		}
		return "", fmt.Errorf("read the model's token Secret %s/%s: %w", project, tokenSecretName(model), err)
	}
	if sec.Metadata.UID != recordedUID || sec.Type != TokenSecretType || sec.Metadata.Annotations[saNameAnnotation] != tokenSAName(model) {
		return "", fmt.Errorf("the Secret %s/%s is not the token this tool made for the model", project, tokenSecretName(model))
	}
	token, err := base64.StdEncoding.DecodeString(sec.Data["token"])
	if err != nil || len(token) == 0 {
		return "", fmt.Errorf("the model's token Secret %s/%s holds no token yet", project, tokenSecretName(model))
	}
	return string(token), nil
}

// callChecks keeps the last check of each model this app ran, by cluster,
// project, name and the model's UID (a model made again is not checked).
var callChecks = struct {
	sync.Mutex
	m map[string]struct {
		uid   string
		check types.ModelCallCheck
	}
}{m: map[string]struct {
	uid   string
	check types.ModelCallCheck
}{}}

var _ = kube.RegisterCachePurge(func() {
	callChecks.Lock()
	for k := range callChecks.m {
		delete(callChecks.m, k)
	}
	callChecks.Unlock()
})

func callCheckKey(c *kube.Client, project, name string) string {
	return c.BaseURL() + "|" + project + "/" + name
}

func rememberCallCheck(c *kube.Client, project, name, uid string, check types.ModelCallCheck) {
	callChecks.Lock()
	defer callChecks.Unlock()
	callChecks.m[callCheckKey(c, project, name)] = struct {
		uid   string
		check types.ModelCallCheck
	}{uid, check}
}

func lastCallCheck(c *kube.Client, project, name, uid string) *types.ModelCallCheck {
	callChecks.Lock()
	defer callChecks.Unlock()
	r, ok := callChecks.m[callCheckKey(c, project, name)]
	if !ok || r.uid != uid {
		return nil
	}
	check := r.check
	return &check
}

// checkModelCall runs the check on the tool's model (read fresh) and
// remembers it. progress gets each retry, in words.
func checkModelCall(c *kube.Client, project, name string, rec ModelsRecord, progress func(string)) types.ModelCallCheck {
	now := func() string { return record.NowRFC3339() }
	model, found, err := ReadToolModel(c, project, name)
	if err != nil || !found {
		why := "it does not exist"
		if err != nil {
			why = err.Error()
		}
		return types.ModelCallCheck{Result: "not-checked", Message: "Not checked: the model cannot be read (" + why + ").", At: now()}
	}
	entry, _ := CatalogEntry(name)
	served := kube.FirstNonEmpty(model.Spec.Model.Name, name)
	def, _ := readKServeGateway(c)
	exposure, gw := modelExposureOf(model, def)
	var check types.ModelCallCheck
	switch {
	case exposure == ExposureMaaS:
		check = maasCallCheck(c, project, name)
	case !SafeServedName(served):
		check = types.ModelCallCheck{Result: "not-checked", Message: "Not checked: " + unsafeServedNameWarning}
	default:
		check = callModelDirectly(c, model, project, name, served, entry.Type, exposure, gw, rec, progress)
	}
	check.At = now()
	rememberCallCheck(c, project, name, model.Metadata.UID, check)
	return check
}

// callModelDirectly is the check of a model reached at its own address
// (inside the cluster, or through a gateway that is not MaaS's).
func callModelDirectly(c *kube.Client, model LLMISvcObject, project, name, served, typ, exposure, gw string, rec ModelsRecord, progress func(string)) types.ModelCallCheck {
	var check types.ModelCallCheck
	switch exposure {
	case ExposureInternal:
		if target := WithServicePort(c, kube.FirstNonEmpty(ModelInClusterURL(model), internalAddress(project, name))); target == "" {
			check = types.ModelCallCheck{Result: "not-checked", Message: "Not checked: the model's Service " + name + "-kserve-workload-svc in " + project + " could not be read for its port."}
		} else {
			check = runModelCall(c, target, served, typ, "", true, progress)
		}
	default:
		switch {
		case model.Status.URL == "":
			check = types.ModelCallCheck{Result: "not-checked", Message: "Not checked: KServe reports no address for the model yet."}
		case !kube.SnippetURL(model.Status.URL):
			check = types.ModelCallCheck{Result: "not-checked", Message: "Not checked: " + kube.UnsafeAddressWarning("model address")}
		default:
			token, err := readModelToken(c, project, name, rec[ModelRecordKey(project, name)].TokenSecretUID)
			if err != nil {
				check = types.ModelCallCheck{Result: "not-checked", Message: "Not checked: " + err.Error() + ". This app calls a model only with the model's own token."}
				break
			}
			check = runModelCall(c, model.Status.URL, served, typ, token, false, progress)
			if check.Result == "server-error" && gw != "" && exposure == ExposureGateway {
				check.Message += " (through gateway " + gw + ")"
			}
			if check.OK {
				check = probeWithoutToken(c, check, model.Status.URL, served, typ, gw)
			}
		}
	}
	return check
}

// probeWithoutToken sends the same request through the gateway once more,
// without a token, after the model answered one with its token. The
// gateway must refuse it (401 or 403): an answer means the gateway does not
// check who calls (no AuthPolicy, one not enforced, or a gateway labelled
// opendatahub.io/managed=false), and the check fails as "unauthenticated".
// Any other outcome is said, not counted as a failure.
func probeWithoutToken(c *kube.Client, ok types.ModelCallCheck, base, served, typ, gw string) types.ModelCallCheck {
	target, body := modelCallRequest(base, served, typ)
	status, _, err := callModelOnce(c.Context(), target, body, "")
	switch {
	case err == nil && status >= 200 && status < 300:
		return types.ModelCallCheck{Result: "unauthenticated", Status: status, URL: target,
			Message: fmt.Sprintf("The model also answered a request without a token (HTTP %d): gateway %s does not check who calls, so anyone who can reach %s can use the model. Check that RHOAI's AuthPolicy on the gateway is enforced (odh-model-controller and Red Hat Connectivity Link run), and that the gateway is not labelled %s=false.", status, kube.FirstNonEmpty(gw, "the gateway"), base, ODHManagedLabel)}
	case err == nil && (status == http.StatusUnauthorized || status == http.StatusForbidden):
		ok.Message += fmt.Sprintf(" A request without a token was refused (HTTP %d).", status)
	case err == nil:
		ok.Message += fmt.Sprintf(" A request without a token got HTTP %d, so whether the gateway refuses callers without a token is not confirmed.", status)
	default:
		ok.Message += " A request without a token got no answer, so whether the gateway refuses callers without a token is not confirmed."
	}
	return ok
}

// callFailureWords is a failed check as a sentence about the model.
func callFailureWords(what string, check types.ModelCallCheck) string {
	if check.Result == "unauthenticated" {
		return what + ", but it answers without a token: " + check.Message
	}
	return what + " but cannot be called: " + check.Message
}

// runModelCall calls the model, retrying transient answers for up to
// ModelCallRetryWindow.
func runModelCall(c *kube.Client, base, served, typ, token string, internal bool, progress func(string)) types.ModelCallCheck {
	target, body := modelCallRequest(base, served, typ)
	var last types.ModelCallCheck
	attempt := 0
	_, err := kube.WaitForGone(c, ModelCallRetryWindow, ModelCallPoll, func(cc *kube.Client) (bool, error) {
		attempt++
		status, data, err := callModelOnce(cc.Context(), target, body, token)
		if err != nil && cc.Context().Err() != nil && c.Context().Err() == nil {
			// The retry window ended during this request.
			return false, nil
		}
		check, transient := ClassifyModelCall(status, data, err, target, served, internal)
		last = check
		if transient && progress != nil {
			progress(fmt.Sprintf("Not answering yet (attempt %d): %s Retrying...", attempt, check.Message))
		}
		return !transient, nil
	})
	if err != nil && last.Result == "" {
		last = types.ModelCallCheck{Result: "not-checked", Message: fmt.Sprintf("Not checked: %v.", err), URL: target}
	}
	return last
}

// maasCallCheck reports MaaS's own readiness of a model reached through
// MaaS: a call needs a MaaS API key, which this tool does not create.
func maasCallCheck(c *kube.Client, project, name string) types.ModelCallCheck {
	ref, found, err := readMaaSModelRef(c, project, name)
	switch {
	case err != nil:
		return types.ModelCallCheck{Result: "not-checked", Message: "Not checked: MaaS's model reference cannot be read: " + err.Error() + "."}
	case !found:
		return types.ModelCallCheck{Result: "not-checked", Message: "Not checked: the model routes through MaaS's gateway but is not published (no MaaSModelRef). Publish it on the MaaS tab."}
	case ref.Status.Phase != "Ready":
		return types.ModelCallCheck{Result: "not-checked", Message: fmt.Sprintf("MaaS does not list the model yet (its reference is %s).", kube.FirstNonEmpty(ref.Status.Phase, "pending"))}
	}
	return types.ModelCallCheck{Result: "maas", OK: true, URL: ref.Status.Endpoint,
		Message: "MaaS lists the model (its reference is Ready). Calls go through MaaS with an API key, which this tool does not create: the MaaS tab's commands get one and call the model."}
}
