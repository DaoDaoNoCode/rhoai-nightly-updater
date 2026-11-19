package cluster

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The Gen AI playground (Environment page, Playground tab) is the RHOAI
// dashboard's own: this tool asks the installed dashboard's gen-ai API to
// set it up, report it and delete it, exactly as a click in the dashboard's
// Gen AI studio does. The dashboard then writes whatever this RHOAI
// release's playground is made of (an OGXServer, its configuration
// ConfigMaps, a pgvector database), so no playground configuration is
// copied here and nothing depends on the release.
//
// The calls (odh-dashboard packages/gen-ai, the frontend's own contract),
// all on the dashboard's address with a bearer token the API layer chooses
// (pkg/api genAIToken) and passes in as a plain string:
//
//	GET    /gen-ai/api/v1/aaa/models?namespace=<ns>   the project's models (no "sources": namespace and custom endpoints only)
//	GET    /gen-ai/api/v1/maas/models?namespace=<ns>  Models as a Service (the user's token only)
//	GET    /gen-ai/api/v1/lsd/status?namespace=<ns>   {"data":{"name","phase",...}} or {"data":null}
//	POST   /gen-ai/api/v1/lsd/install?namespace=<ns>  {"models":[...],...}
//	DELETE /gen-ai/api/v1/lsd/delete?namespace=<ns>   {"name":"<server>"}
//
// The dashboard's API acts with the caller's token. Set up, delete and
// Clean up are changes: they call it with the signed-in user's own token
// (the client's, NewUserClient), so the dashboard acts as that user, as
// when they use the playground themselves, and may add a Models as a
// Service model (genAIUserCallAllowed). The status, which any signed-in
// user sees, calls it with the app's ServiceAccount token, for which
// deploy/template.yaml grants the reads (SECURITY.md audits them). The tool
// records the projects it requested a playground in, in its own namespace;
// a playground it did not request is shown but never deleted.
//
// No call with the ServiceAccount's token may reach Models as a Service:
// the gen-ai API forwards the caller's token to the MaaS API through the
// MaaS gateway (odh-dashboard rhoai-3.6 packages/gen-ai/bff
// maas_models_handler.go, aaa_models_handler.go fetchMaaSModels for
// sources=maas, token_k8s_client.go generateLlamaStackConfig for an
// install with a MaaS model; packages/maas/bff maas_client.go
// addIdentityHeaderToRequest), and that gateway admits routes from every
// published project, whose editors could attach one for the MaaS API's
// path and receive the token. So with the ServiceAccount's token the tool
// never calls maas/models or aaa/models with sources and never installs a
// MaaS model (genAICallAllowed), and the status reads the MaaS models it
// shows from their MaaSModelRefs with the Kubernetes API
// (listMaaSPlaygroundModels). The user's own token goes there only when the
// user sets up a playground with a MaaS model, the same exposure as doing
// it in the dashboard.

const genAIAPIPrefix = "/gen-ai/api/v1"

// Waits of a playground operation, bounded and on the clock (tests shorten
// nothing: the clock is fake).
var (
	// PlaygroundInstallRetryTimeout bounds retrying an install the
	// dashboard answers 409: the previous playground's database is still
	// being removed.
	PlaygroundInstallRetryTimeout = 3 * time.Minute
	PlaygroundInstallRetryPoll    = 10 * time.Second
	// PlaygroundReadyTimeout bounds the wait for the playground to report
	// Ready (60-100 s live, more on the first pull of its image).
	PlaygroundReadyTimeout = 6 * time.Minute
	PlaygroundPoll         = 5 * time.Second
	// PlaygroundDeleteTimeout bounds the wait until the dashboard no
	// longer reports the playground.
	PlaygroundDeleteTimeout = 3 * time.Minute
)

// playgroundStatusMaxProjects bounds the projects a status reads (one
// gen-ai call each, a few at a time).
const (
	playgroundStatusMaxProjects = 100
	playgroundStatusParallel    = 6
	playgroundStatusTTL         = 10 * time.Second
	playgroundMaxModels         = 50
)

// genAIBaseURL is the dashboard's address ("" when unknown). Tests replace it.
var genAIBaseURL = dashboardBaseURL

// genAIHTTPClient sends the gen-ai calls; it trusts the same certificates
// as the samples' calls to the same gateway, and does not follow redirects
// either. Tests replace it.
var genAIHTTPClient = samplesHTTPClient

// genAIAPI is the dashboard's gen-ai API for one request.
type genAIAPI struct {
	ctx   context.Context
	base  string
	token string
	// asUser: token is the signed-in user's own (a change's client), not
	// the ServiceAccount's; it allows genAIUserCallAllowed.
	asUser bool
}

func (a genAIAPI) with(ctx context.Context) genAIAPI {
	a.ctx = ctx
	return a
}

// genAIError is a non-2xx answer of the gen-ai API: the HTTP status, the
// call, and the API's own message. It never holds the token, which is
// only ever sent in the Authorization header.
type genAIError struct {
	Status  int
	Method  string
	Path    string
	Code    string
	Message string
}

func (e *genAIError) Error() string {
	msg := firstNonEmpty(e.Message, http.StatusText(e.Status))
	return fmt.Sprintf("the dashboard's gen-ai API answered %d to %s %s: %s", e.Status, e.Method, e.Path, msg)
}

func asGenAIError(err error) (*genAIError, bool) {
	var e *genAIError
	ok := errors.As(err, &e)
	return e, ok
}

func genAIStatus(err error) int {
	if e, ok := asGenAIError(err); ok {
		return e.Status
	}
	return 0
}

const maxGenAIErrorMessage = 400

// parseGenAIError reads the BFF's {"error":{"code","message"}}, else a
// {"message"} or {"error":"..."}, else the text.
func parseGenAIError(method, path string, status int, body []byte) *genAIError {
	e := &genAIError{Status: status, Method: method, Path: path}
	var nested struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	var flat struct {
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	switch {
	case json.Unmarshal(body, &nested) == nil && nested.Error.Message != "":
		e.Code, e.Message = nested.Error.Code, nested.Error.Message
	case json.Unmarshal(body, &flat) == nil && (flat.Message != "" || flat.Error != ""):
		e.Message = firstNonEmpty(flat.Message, flat.Error)
	default:
		e.Message = strings.TrimSpace(string(body))
	}
	if len(e.Message) > maxGenAIErrorMessage {
		e.Message = e.Message[:maxGenAIErrorMessage] + "..."
	}
	return e
}

// errGenAINotAllowed: a gen-ai call the tool refuses to make with the
// ServiceAccount's token.
var errGenAINotAllowed = errors.New("this tool does not make this call to the dashboard's gen-ai API: it could send the updater's ServiceAccount token on to the Models as a Service gateway")

// genAICallAllowed is the only gen-ai calls the tool makes (see the
// comment at the top), none of which reaches Models as a Service:
// aaa/models without "sources" (call sets only namespace, so the dashboard
// lists the project's own models and custom endpoints), lsd/status,
// lsd/delete, and lsd/install when every model is the project's own
// (model_source_type "namespace"). Anything else (maas/models, maas/tokens,
// lsd/responses, an install with a MaaS model, a path added later) is
// refused before a request is made.
func genAICallAllowed(method, path string, in interface{}) bool {
	switch method + " " + path {
	case "GET /aaa/models", "GET /lsd/status", "DELETE /lsd/delete":
		return true
	case "POST /lsd/install":
		return genAIInstallAllowed(in)
	}
	return false
}

// genAIInstallAllowed is the only install body the tool sends: the
// project's own models, no tracing, no guardrails, no vector stores (each
// of those names further endpoints or providers the dashboard would wire
// with the caller's token), and no key the tool does not know.
func genAIInstallAllowed(in interface{}) bool {
	return genAIInstallAllowedWith(in, "namespace")
}

// genAIInstallAllowedWith is genAIInstallAllowed for models of the given
// sources.
func genAIInstallAllowedWith(in interface{}, sources ...string) bool {
	body, ok := in.(map[string]interface{})
	if !ok {
		return false
	}
	for k := range body {
		switch k {
		case "models", "enable_tracing", "enable_guardrails", "vector_stores":
		default:
			return false
		}
	}
	if body["enable_tracing"] != false || body["enable_guardrails"] != false {
		return false
	}
	if vs, ok := body["vector_stores"].([]interface{}); !ok || len(vs) != 0 {
		return false
	}
	models, ok := body["models"].([]map[string]interface{})
	if !ok || len(models) == 0 {
		return false
	}
	for _, m := range models {
		src, _ := m["model_source_type"].(string)
		if !containsString(sources, src) {
			return false
		}
		for k := range m {
			switch k {
			case "model_name", "model_source_type", "model_type", "embedding_dimension":
			default:
				return false
			}
		}
	}
	return true
}

// genAIUserCallAllowed is the gen-ai calls a change makes with the
// signed-in user's own token: those of genAICallAllowed, the MaaS models
// the user may use (maas/models), and an install that may add them
// (model_source_type "maas"), with the same narrow body otherwise. The
// dashboard then sends the user's token to the MaaS gateway, as when the
// user adds the model there.
func genAIUserCallAllowed(method, path string, in interface{}) bool {
	switch method + " " + path {
	case "GET /maas/models":
		return true
	case "POST /lsd/install":
		return genAIInstallAllowedWith(in, "namespace", "maas")
	}
	return genAICallAllowed(method, path, in)
}

// call makes one request to path (below /gen-ai/api/v1) in namespace ns.
func (a genAIAPI) call(method, path, ns string, in, out interface{}) error {
	allowed := genAICallAllowed
	if a.asUser {
		allowed = genAIUserCallAllowed
	}
	if !allowed(method, path, in) {
		return errGenAINotAllowed
	}
	u := a.base + genAIAPIPrefix + path + "?" + url.Values{"namespace": {ns}}.Encode()
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(a.ctx, method, u, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := genAIHTTPClient.Do(req)
	if err != nil {
		// url.Error names the URL, which holds no secret.
		return fmt.Errorf("cannot reach the dashboard's gen-ai API: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return fmt.Errorf("read the answer of the dashboard's gen-ai API: %w", err)
	}
	if isRedirect(resp.StatusCode) {
		return &genAIError{Status: resp.StatusCode, Method: method, Path: genAIAPIPrefix + path, Message: redirectMessage(resp)}
	}
	if resp.StatusCode >= 300 {
		return parseGenAIError(method, genAIAPIPrefix+path, resp.StatusCode, data)
	}
	if out != nil && len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("parse the answer of the dashboard's gen-ai API to %s %s: %w", method, genAIAPIPrefix+path, err)
		}
	}
	return nil
}

// genAIServer is the playground as lsd/status reports it.
type genAIServer struct {
	Name  string `json:"name"`
	Phase string `json:"phase"`
}

// playgroundStatus returns the project's playground, or nil when it has
// none. The dashboard reports none as 200 {"data":null} (rhoai-3.6
// GetOGXServerStatus); a 404 is a route or API that is not there
// (LlamaStackDistributionStatusHandler answers it for NotFound), so it is
// an error, never absence.
func (a genAIAPI) playgroundStatus(ns string) (*genAIServer, error) {
	var out struct {
		Data *genAIServer `json:"data"`
	}
	if err := a.call(http.MethodGet, "/lsd/status", ns, nil, &out); err != nil {
		return nil, err
	}
	return out.Data, nil
}

// namespaceModel is one entry of aaa/models (unknown fields ignored).
type namespaceModel struct {
	ModelName    string   `json:"model_name"`
	ModelID      string   `json:"model_id"`
	DisplayName  string   `json:"display_name"`
	Status       string   `json:"status"`
	Usecase      string   `json:"usecase"`
	ModelType    string   `json:"model_type"`
	Capabilities []string `json:"capabilities"`
}

// chatModel: not an embedding model. The dashboard's models carry
// capabilities (text-generation by default), a use case and sometimes a
// model type; any of them naming embedding makes it one.
func chatModel(capabilities []string, usecase, modelType string) bool {
	for _, s := range append([]string{usecase, modelType}, capabilities...) {
		if strings.Contains(strings.ToLower(s), "embedding") {
			return false
		}
	}
	return true
}

// markToolEmbeddingModels sets EmbeddingDimension on the project's
// embedding models that are this tool's catalog models (its label and a
// write by it): the playground can then use them as embedding models of
// that dimension. Any other embedding model is left as it is; its
// dimension is not known.
func markToolEmbeddingModels(c *Client, project string, models []types.PlaygroundModel) {
	rec, err := readModelsRecord(c)
	if err != nil {
		return
	}
	for i, m := range models {
		entry, ok := catalogEntry(m.Name)
		if m.Name == "" || !ok || m.Source != "namespace" || m.Chat || entry.Type != modelTypeEmbedding {
			continue
		}
		if obj, found, err := readToolModel(c, project, m.Name); err == nil && found && rec.owns(obj.Metadata) {
			models[i].EmbeddingDimension = entry.EmbeddingDimension
		}
	}
}

// maasModel is one entry of maas/models (unknown fields ignored).
type maasModel struct {
	ID            string `json:"id"`
	Subscriptions []struct {
		Name string `json:"name"`
	} `json:"subscriptions"`
	DisplayName  string   `json:"display_name"`
	Ready        bool     `json:"ready"`
	Capabilities []string `json:"capabilities"`
	ModelType    string   `json:"model_type"`
	Usecase      string   `json:"usecase"`
}

// listProjectModels lists the project's own models through the gen-ai API
// (an error means the API cannot be used) and, with the user's own token
// only, the Models as a Service that user may use (any error is none: MaaS
// is optional and may be off). With the ServiceAccount's token it never
// asks for MaaS models (genAICallAllowed).
// listProjectModels lists the project's own models and, only when withMaaS
// (a Set up that chose a MaaS model, on the user's own token), its Models as
// a Service models. Listing MaaS models makes the gen-ai backend send the
// caller's token through maas-default-gateway, where an editor of a
// published project can attach a route; a Set up with project models only
// must not send it there.
func (a genAIAPI) listProjectModels(ns string, withMaaS bool) ([]types.PlaygroundModel, error) {
	var own struct {
		Data []namespaceModel `json:"data"`
	}
	if err := a.call(http.MethodGet, "/aaa/models", ns, nil, &own); err != nil {
		return nil, err
	}
	var out []types.PlaygroundModel
	for _, m := range own.Data {
		name := firstNonEmpty(m.ModelName, m.ModelID)
		if name == "" {
			continue
		}
		out = append(out, types.PlaygroundModel{
			Name: name, DisplayName: m.DisplayName, Source: "namespace", Status: firstNonEmpty(m.Status, "unknown"),
			Ready: m.Status == "Running", Chat: chatModel(m.Capabilities, m.Usecase, m.ModelType),
		})
	}
	if a.asUser && withMaaS {
		var maas struct {
			Data []maasModel `json:"data"`
		}
		if err := a.call(http.MethodGet, "/maas/models", ns, nil, &maas); err != nil {
			slog.Debug("no Models as a Service for the playground", "project", ns, "error", err)
		}
		for _, m := range maas.Data {
			if m.ID == "" {
				continue
			}
			st := "Not ready"
			if m.Ready {
				st = "Ready"
			}
			var subs []string
			for _, s := range m.Subscriptions {
				if s.Name != "" {
					subs = append(subs, s.Name)
				}
			}
			out = append(out, types.PlaygroundModel{
				Name: m.ID, DisplayName: m.DisplayName, Source: "maas", Status: st, Ready: m.Ready,
				Chat: chatModel(m.Capabilities, m.Usecase, m.ModelType), Subscriptions: subs,
			})
		}
	}
	if len(out) > playgroundMaxModels {
		out = out[:playgroundMaxModels]
	}
	return out, nil
}

// Annotations maas-api reads a MaaSModelRef's details from
// (models-as-a-service maas-api internal/models/maasmodelref.go
// maasModelRefToModel, internal/constant/const.go).
const (
	maasDisplayNameAnnotation  = "openshift.io/display-name"
	maasCapabilitiesAnnotation = "opendatahub.io/model-capabilities"
)

// listMaaSPlaygroundModels lists the Models as a Service the cluster
// offers, from their MaaSModelRefs (cluster-wide list) and the
// MaaSSubscriptions that offer them (in models-as-a-service), read with
// the Kubernetes API: the ServiceAccount's existing list grants, and no
// call to the gen-ai API, which would send its token on to the MaaS
// gateway. A model's name is what maas-api lists it as
// (maasModelRefToModel): status.resolvedModelAlias, else the referenced
// ExternalModel's name for an ExternalModel, else the MaaSModelRef's name;
// it is ready when its phase is Ready. They are only shown: whether the
// user may call one depends on the subscriptions they are in, which the
// MaaS API decides for them (in the dashboard, with their own login). Any
// error, an API that is not served included, is no models: MaaS is
// optional.
func listMaaSPlaygroundModels(c *Client) []types.PlaygroundModel {
	body, _, err := c.get("/apis/" + maasAPI(c) + "/maasmodelrefs")
	if err != nil {
		if !IsK8sError(err, http.StatusNotFound) {
			slog.Debug("cannot list the MaaS models for the playground", "error", err)
		}
		return nil
	}
	var refs struct {
		Items []struct {
			Metadata objectMeta `json:"metadata"`
			Spec     struct {
				ModelRef struct {
					Kind string `json:"kind"`
					Name string `json:"name"`
				} `json:"modelRef"`
			} `json:"spec"`
			Status struct {
				Phase              string `json:"phase"`
				ResolvedModelAlias string `json:"resolvedModelAlias"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &refs); err != nil {
		slog.Debug("cannot parse the MaaS model references", "error", err)
		return nil
	}
	subs := maasSubscriptionsByModel(c)
	var out []types.PlaygroundModel
	for _, r := range refs.Items {
		name := r.Metadata.Name
		if r.Spec.ModelRef.Kind == "ExternalModel" && r.Spec.ModelRef.Name != "" {
			name = r.Spec.ModelRef.Name
		}
		name = firstNonEmpty(r.Status.ResolvedModelAlias, name)
		offered := subs[r.Metadata.Namespace+"/"+r.Metadata.Name]
		// A reference no MaaSSubscription lists is offered to nobody: not
		// shown as published.
		if name == "" || r.Metadata.terminating() || len(offered) == 0 {
			continue
		}
		var caps []string
		if raw := r.Metadata.Annotations[maasCapabilitiesAnnotation]; raw != "" {
			_ = json.Unmarshal([]byte(raw), &caps)
		}
		out = append(out, types.PlaygroundModel{
			Name: name, DisplayName: r.Metadata.Annotations[maasDisplayNameAnnotation], Source: "maas",
			Status: firstNonEmpty(r.Status.Phase, "Not ready"), Ready: r.Status.Phase == "Ready",
			Chat: chatModel(caps, r.Metadata.Annotations[genAIUseCaseAnnotation], ""), Subscriptions: offered,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// maasSubscriptionsByModel names, by MaaSModelRef ("<namespace>/<name>"),
// the MaaSSubscriptions that offer it (none when they cannot be read).
func maasSubscriptionsByModel(c *Client) map[string][]string {
	out := map[string][]string{}
	body, _, err := c.get(namespacedPath(maasAPI(c), "maassubscriptions", maasTenantNamespace, ""))
	if err != nil {
		return out
	}
	var list struct {
		Items []struct {
			Metadata objectMeta `json:"metadata"`
			Spec     struct {
				ModelRefs []struct {
					Name      string `json:"name"`
					Namespace string `json:"namespace"`
				} `json:"modelRefs"`
			} `json:"spec"`
		} `json:"items"`
	}
	if json.Unmarshal(body, &list) != nil {
		return out
	}
	for _, s := range list.Items {
		for _, ref := range s.Spec.ModelRefs {
			k := ref.Namespace + "/" + ref.Name
			if !containsString(out[k], s.Metadata.Name) {
				out[k] = append(out[k], s.Metadata.Name)
			}
		}
	}
	for k := range out {
		sort.Strings(out[k])
	}
	return out
}

// --- what this tool requested --------------------------------------------------------

// The projects this tool requested a playground in are recorded in its own
// namespace, in the snapshot ConfigMap's data key "playgrounds", written by
// server-side apply under a field manager of its own (as the recorded
// operator Subscription is, snapshot.go): the RBAC is that ConfigMap's
// existing patch grant, and create when it does not exist yet.
const (
	playgroundRecordKey     = "playgrounds"
	playgroundRecordManager = "rhoai-nightly-updater-playgrounds"
)

// playgroundRecord is one project's entry. UID is the OGXServer the
// dashboard made for the request; until it is known (an install in
// flight), a server created at or after RequestedAt is taken as the one
// requested.
type playgroundRecord struct {
	RequestedAt string `json:"requestedAt"`
	By          string `json:"by,omitempty"`
	UID         string `json:"uid,omitempty"`
}

// playgroundServerName is the OGXServer the dashboard's install makes
// (the BFF's lsdName); the ServiceAccount may read only that name, so a
// server named otherwise is never taken as this tool's.
const playgroundServerName = "lsd-genai-playground"

// readPlaygroundServer reads the playground's OGXServer (the
// ServiceAccount's own read, at a served version of ogx.io).
func readPlaygroundServer(c *Client, project, name string) (objectMeta, bool, error) {
	if name != playgroundServerName {
		return objectMeta{}, false, nil
	}
	r, err := getServed(c, "ogx.io", "/apis/ogx.io/%s/namespaces/"+project+"/ogxservers/"+playgroundServerName, []string{"v1beta1"})
	if IsK8sError(err, http.StatusNotFound) {
		return objectMeta{}, false, nil
	}
	if err != nil {
		return objectMeta{}, false, fmt.Errorf("read OGXServer %s/%s: %w", project, name, err)
	}
	var obj struct {
		Metadata objectMeta `json:"metadata"`
	}
	if err := json.Unmarshal(r.Body, &obj); err != nil {
		return objectMeta{}, false, fmt.Errorf("parse OGXServer %s/%s: %w", project, name, err)
	}
	return obj.Metadata, true, nil
}

// playgroundOwnership says whether the live playground srv in project is
// the one this tool requested (rec, when has): its OGXServer's UID is the
// recorded one, or, with none recorded yet, it was created at or after the
// request. uid is the live server's UID when owned. A record of something
// else (the playground was deleted and made again in Gen AI studio, the
// project was recreated, or the install found someone else's server) is
// stale: owned is false.
func playgroundOwnership(c *Client, project string, rec playgroundRecord, has bool, srv *genAIServer) (owned bool, uid string, err error) {
	if !has || srv == nil {
		return false, "", nil
	}
	meta, found, err := readPlaygroundServer(c, project, srv.Name)
	if err != nil || !found || meta.UID == "" {
		return false, "", err
	}
	if rec.UID != "" {
		return rec.UID == meta.UID, meta.UID, nil
	}
	requested, err1 := time.Parse(time.RFC3339, rec.RequestedAt)
	created, err2 := time.Parse(time.RFC3339, meta.CreationTimestamp)
	if err1 != nil || err2 != nil || created.Before(requested) {
		return false, "", nil
	}
	return true, meta.UID, nil
}

// readPlaygroundRecords returns the recorded projects (none when nothing is
// recorded).
func readPlaygroundRecords(c *Client) (map[string]playgroundRecord, error) {
	body, _, err := c.bookkeeping().get(namespacedPath("v1", "configmaps", getActivityNamespace(), snapshotConfigMapName))
	if IsK8sError(err, 404) {
		return map[string]playgroundRecord{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read this tool's playground records: %w", err)
	}
	var cm struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(body, &cm); err != nil {
		return nil, fmt.Errorf("parse the snapshot ConfigMap: %w", err)
	}
	out := map[string]playgroundRecord{}
	if raw := cm.Data[playgroundRecordKey]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			return nil, fmt.Errorf("parse this tool's playground records: %w", err)
		}
	}
	return out, nil
}

// writePlaygroundRecords applies the whole record (operations hold the
// cluster lock, so read-change-write does not race another writer).
func writePlaygroundRecords(c *Client, records map[string]playgroundRecord) error {
	data, err := json.Marshal(records)
	if err != nil {
		return err
	}
	ns := getActivityNamespace()
	cm, _ := json.Marshal(map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]interface{}{"name": snapshotConfigMapName, "namespace": ns},
		"data":       map[string]interface{}{playgroundRecordKey: string(data)},
	})
	query := url.Values{"fieldManager": {playgroundRecordManager}, "force": {"true"}}
	if _, _, err := c.bookkeeping().do(http.MethodPatch, namespacedPath("v1", "configmaps", ns, snapshotConfigMapName), "application/apply-patch+yaml", cm, query); err != nil {
		return fmt.Errorf("record this tool's playgrounds: %w", err)
	}
	return nil
}

// setPlaygroundRecord records project's entry (nil drops it).
func setPlaygroundRecord(c *Client, project string, rec *playgroundRecord) error {
	records, err := readPlaygroundRecords(c)
	if err != nil {
		return err
	}
	old, has := records[project]
	switch {
	case rec == nil && !has, rec != nil && has && old == *rec:
		return nil
	case rec == nil:
		delete(records, project)
	default:
		records[project] = *rec
	}
	return writePlaygroundRecords(c, records)
}

// newPlaygroundRecord is the entry of a request made now.
func newPlaygroundRecord(c *Client) *playgroundRecord {
	// Seconds, as creationTimestamp: a server made in the same second as
	// the request counts as made after it.
	return &playgroundRecord{RequestedAt: clk.Now().UTC().Format(time.RFC3339), By: getUser(c)}
}

// --- cluster parts (ServiceAccount reads) -------------------------------------------

// playgroundComponent is the DataScienceCluster component that runs the
// playground's server operator: ogx, called llamastackoperator before the
// rename. Whichever the DataScienceCluster names is used.
var playgroundComponent = featureComponent{feature: "The Gen AI playground", keys: []string{"ogx", "llamastackoperator"}}

// componentReadyCondition reads the DataScienceCluster's "<component>Ready"
// condition (OGXReady, LlamaStackOperatorReady) of the first component key
// it reports: "True", "False" or "" (not reported), with its message.
func componentReadyCondition(dsc map[string]interface{}, keys []string) (string, string) {
	status, _ := dsc["status"].(map[string]interface{})
	conds, _ := status["conditions"].([]interface{})
	for _, key := range keys {
		for _, raw := range conds {
			cond, _ := raw.(map[string]interface{})
			if t, _ := cond["type"].(string); !strings.EqualFold(t, key+"Ready") {
				continue
			}
			s, _ := cond["status"].(string)
			msg, _ := cond["message"].(string)
			if s != "True" && s != "False" {
				s = ""
			}
			return s, msg
		}
	}
	return "", ""
}

// genAIUIDeployment is the dashboard's Gen AI studio backend, which serves
// the gen-ai API.
const genAIUIDeployment = "gen-ai-ui"

// genAIUIState reads Deployment gen-ai-ui: "available", "unavailable",
// "missing", or "" when it cannot be read.
func genAIUIState(c *Client) string {
	body, _, err := c.get(fmt.Sprintf("/apis/apps/v1/namespaces/%s/deployments/%s", appsNamespace(c), genAIUIDeployment))
	if IsK8sError(err, 404) {
		return "missing"
	}
	if err != nil {
		slog.Debug("cannot read the gen-ai-ui Deployment", "error", err)
		return ""
	}
	var d struct {
		Status struct {
			Conditions []k8sCondition `json:"conditions"`
		} `json:"status"`
	}
	if json.Unmarshal(body, &d) != nil {
		return ""
	}
	if cond, ok := findCondition(d.Status.Conditions, "Available"); ok {
		if cond.Status == "True" {
			return "available"
		}
		return "unavailable"
	}
	return ""
}

// playgroundClusterParts fills what the ServiceAccount reads.
func playgroundClusterParts(c *Client, st *types.PlaygroundStatus) {
	if dsc, err := readFirstDSC(c); err == nil {
		st.Blocked = componentBlock(dsc, playgroundComponent)
		st.OperatorReady, st.OperatorMessage = componentReadyCondition(dsc.Object, playgroundComponent.keys)
	} else {
		slog.Debug("cannot read the DataScienceCluster for the playground", "error", err)
	}
	st.GenAIUI = genAIUIState(c)
}

// --- status --------------------------------------------------------------------------

var playgroundStatusCache = newLRU[string, *types.PlaygroundStatus](64)

// playgroundStatusGen is part of every cache key: a change bumps it, so
// the next status reads afresh.
var playgroundStatusGen atomic.Int64

// PurgePlaygroundStatus drops cached playground status (after a change).
func PurgePlaygroundStatus() { playgroundStatusGen.Add(1) }

func tokenKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:8])
}

// apiProblem explains an error of the gen-ai API for people, with its kind.
func apiProblem(err error) (string, string) {
	if errors.Is(err, errGenAINotAllowed) {
		return "Nothing was sent: " + err.Error() + ".", "validation"
	}
	if e, ok := asGenAIError(err); ok {
		switch e.Status {
		case http.StatusUnauthorized, http.StatusForbidden:
			return fmt.Sprintf("The dashboard's gen-ai API refused the request (HTTP %d: %s).", e.Status, firstNonEmpty(e.Message, http.StatusText(e.Status))), "forbidden"
		case http.StatusNotFound:
			return fmt.Sprintf("This RHOAI release's dashboard gen-ai API differs from the one this tool knows: %s %s answered 404 (%s). Use the playground from the dashboard's Gen AI studio instead.", e.Method, e.Path, firstNonEmpty(e.Message, "not found")), "api_missing"
		}
		return fmt.Sprintf("The dashboard's gen-ai API answered an error: %v.", err), "api_error"
	}
	return fmt.Sprintf("The dashboard's gen-ai API cannot be reached: %v.", err), "unreachable"
}

// playgroundAPI is the API for a request, or why it cannot be used.
func playgroundAPI(c *Client, token string) (genAIAPI, string, string) {
	if token == "" {
		return genAIAPI{}, "No token is available to call the dashboard's gen-ai API with (the updater's ServiceAccount token could not be read).", "no_token"
	}
	base := genAIBaseURL(c)
	if base == "" {
		return genAIAPI{}, "The RHOAI dashboard's address is unknown (no Dashboard component reports one), so its gen-ai API cannot be called.", "no_dashboard"
	}
	// The user's own token only when it is the client's (a change's client).
	return genAIAPI{ctx: c.ctx, base: base, token: token, asUser: c.asUser && token == c.token}, "", ""
}

// playgroundItem is a playground (srv, nil for none) in a project as the
// page shows it.
func playgroundItem(project string, srv *genAIServer, requested bool) types.PlaygroundState {
	s := types.PlaygroundState{Requested: requested}
	s.Namespace = project
	s.ManagedByTool = requested
	if srv == nil {
		return s
	}
	s.Name = srv.Name
	s.Phase = srv.Phase
	s.Deployed = true
	switch srv.Phase {
	case "Ready":
		s.Ready = true
		s.Message = "Ready"
	case "Failed":
		s.TerminalError = true
		s.Message = "The playground's server failed to start (phase Failed). The events of project " + project + " and its playground pods say why; delete it and set it up again."
	case "Terminating":
		s.Terminating = true
		s.Message = "Terminating"
	case "Pending", "Initializing":
		s.Message = "Starting (phase " + srv.Phase + ")"
	case "":
		s.HealthUnknown = true
		s.Message = "The dashboard reports no phase for it"
	default:
		s.HealthUnknown = true
		s.Message = "The dashboard reports phase " + srv.Phase
	}
	return s
}

// GetPlaygroundStatus reports the playground feature: the cluster parts,
// the playgrounds in the data science projects, and, for project, the
// models it can use. token is what the gen-ai API is called with.
func GetPlaygroundStatus(c *Client, token, project string) (*types.PlaygroundStatus, error) {
	key := fmt.Sprintf("%d|%s|%s", playgroundStatusGen.Load(), tokenKey(token), project)
	if st, ok := playgroundStatusCache.Get(key); ok {
		return st, nil
	}
	st := &types.PlaygroundStatus{Project: project}
	projects, err := GetDSProjects(c)
	if err != nil {
		return nil, err
	}
	if project != "" && !containsString(projects, project) {
		return nil, fmt.Errorf("project '%s' is not a data science project", project)
	}
	records, err := readPlaygroundRecords(c)
	if err != nil {
		return nil, err
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); playgroundClusterParts(c, st) }()
	api, problem, code := playgroundAPI(c, token)
	if problem != "" {
		st.APIProblem, st.APICode = problem, code
		wg.Wait()
		for p := range records {
			st.Items = append(st.Items, playgroundItem(p, nil, true))
		}
		sortPlaygrounds(st.Items)
		return st, nil
	}
	var mu sync.Mutex
	var pwg sync.WaitGroup
	if project != "" {
		pwg.Add(1)
		go func() {
			defer pwg.Done()
			models, err := api.listProjectModels(project, false)
			if err == nil {
				markToolEmbeddingModels(c, project, models)
				models = append(models, listMaaSPlaygroundModels(c)...)
				if len(models) > playgroundMaxModels {
					models = models[:playgroundMaxModels]
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				st.ModelsProblem, _ = apiProblem(err)
				if genAIStatus(err) == http.StatusNotFound {
					st.APIProblem, st.APICode = apiProblem(err)
				}
				return
			}
			st.Models = models
		}()
	}
	fo := readPlaygroundFanOut(c, api, token, projects, records)
	if project != "" && !containsString(fo.projects, project) {
		// Past the bound: the selected project is read on its own.
		fo = fo.with(c, api, project, records)
	}
	pwg.Wait()
	wg.Wait()
	st.Truncated = fo.truncated
	for _, p := range fo.projects {
		err := fo.errs[p]
		// A project the caller may not see answers 403: no playground to
		// show there.
		if s := genAIStatus(err); err != nil && st.APIProblem == "" && ((s != http.StatusForbidden && s != http.StatusUnauthorized) || p == project) {
			st.APIProblem, st.APICode = apiProblem(err)
		}
	}
	seen := map[string]bool{}
	for p, srv := range fo.servers {
		_, recorded := records[p]
		if srv != nil {
			st.Items = append(st.Items, playgroundItem(p, srv, fo.owned[p]))
			seen[p] = true
		} else if recorded {
			st.Items = append(st.Items, playgroundItem(p, nil, true))
			seen[p] = true
		}
	}
	for p := range records {
		if !seen[p] && containsString(fo.projects, p) {
			if _, read := fo.servers[p]; !read {
				// Its state could not be read: unknown, not absent.
				item := playgroundItem(p, &genAIServer{}, true)
				item.Message = "Its state could not be read from the dashboard"
				st.Items = append(st.Items, item)
			}
		}
	}
	sortPlaygrounds(st.Items)
	playgroundStatusCache.Add(key, st, playgroundStatusTTL)
	return st, nil
}

// playgroundFanOut is the playgrounds of the data science projects as the
// gen-ai API reports them (one call per project), and whether each is this
// tool's. It is read once for every selected project: changing the
// selection reads only that project's models.
type playgroundFanOut struct {
	projects  []string                // the projects read
	servers   map[string]*genAIServer // read without error (nil: none)
	owned     map[string]bool
	errs      map[string]error
	truncated bool
}

var playgroundFanOutCache = newLRU[string, *playgroundFanOut](16)

func readPlaygroundFanOut(c *Client, api genAIAPI, token string, projects []string, records map[string]playgroundRecord) *playgroundFanOut {
	key := fmt.Sprintf("%d|%s", playgroundStatusGen.Load(), tokenKey(token))
	if fo, ok := playgroundFanOutCache.Get(key); ok {
		return fo
	}
	fo := &playgroundFanOut{servers: map[string]*genAIServer{}, owned: map[string]bool{}, errs: map[string]error{}}
	fo.projects = projects
	if len(projects) > playgroundStatusMaxProjects {
		fo.projects, fo.truncated = projects[:playgroundStatusMaxProjects], true
	}
	var mu sync.Mutex
	sem := make(chan struct{}, playgroundStatusParallel)
	var wg sync.WaitGroup
	for _, p := range fo.projects {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			srv, owned, err := readProjectPlayground(c, api, p, records)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				fo.errs[p] = err
				return
			}
			fo.servers[p], fo.owned[p] = srv, owned
		}()
	}
	wg.Wait()
	playgroundFanOutCache.Add(key, fo, playgroundStatusTTL)
	return fo
}

// with is a copy of fo that also has project.
func (fo *playgroundFanOut) with(c *Client, api genAIAPI, project string, records map[string]playgroundRecord) *playgroundFanOut {
	cp := &playgroundFanOut{projects: append(append([]string{}, fo.projects...), project), servers: map[string]*genAIServer{}, owned: map[string]bool{}, errs: map[string]error{}, truncated: fo.truncated}
	for p, s := range fo.servers {
		cp.servers[p] = s
	}
	for p, o := range fo.owned {
		cp.owned[p] = o
	}
	for p, e := range fo.errs {
		cp.errs[p] = e
	}
	srv, owned, err := readProjectPlayground(c, api, project, records)
	if err != nil {
		cp.errs[project] = err
	} else {
		cp.servers[project], cp.owned[project] = srv, owned
	}
	return cp
}

// readProjectPlayground reads a project's playground and whether it is
// this tool's (read only: the record is brought up to date by the next
// operation, which holds the lock).
func readProjectPlayground(c *Client, api genAIAPI, project string, records map[string]playgroundRecord) (*genAIServer, bool, error) {
	srv, err := api.playgroundStatus(project)
	if err != nil {
		return nil, false, err
	}
	rec, has := records[project]
	owned, _, err := playgroundOwnership(c, project, rec, has, srv)
	if err != nil {
		slog.Debug("cannot tell whether the playground is this tool's", "project", project, "error", err)
	}
	return srv, owned, nil
}

func sortPlaygrounds(items []types.PlaygroundState) {
	sort.Slice(items, func(i, j int) bool { return items[i].Namespace < items[j].Namespace })
}

// --- the operation -------------------------------------------------------------------

var playgroundProjectRegex = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// ValidatePlaygroundRequest returns why req cannot run, or "".
func ValidatePlaygroundRequest(req types.PlaygroundRequest) string {
	switch req.Action {
	case "setup", "teardown":
	default:
		return "action must be setup or teardown"
	}
	if !playgroundProjectRegex.MatchString(req.Project) {
		return "invalid project name"
	}
	if req.Action == "teardown" && len(req.Models) > 0 {
		return "a teardown takes no models"
	}
	if len(req.Models) > playgroundMaxModels {
		return fmt.Sprintf("at most %d models", playgroundMaxModels)
	}
	seen := map[string]bool{}
	for _, m := range req.Models {
		if m.Source != "namespace" && m.Source != "maas" {
			return "a model's source must be namespace or maas"
		}
		if m.Name == "" || len(m.Name) > 253 || strings.ContainsAny(m.Name, " \t\n\"'\\") {
			return "invalid model name"
		}
		if seen[m.Source+"/"+m.Name] {
			return "a model is listed twice"
		}
		seen[m.Source+"/"+m.Name] = true
	}
	return ""
}

// playgroundRun is one streamed playground operation.
type playgroundRun struct {
	c       *Client
	api     genAIAPI
	emit    func(UpdateStepEvent)
	logs    []string
	project string
	action  string
}

func (p *playgroundRun) step(id, status, msg string) {
	p.logs = append(p.logs, msg)
	if p.emit != nil {
		p.emit(UpdateStepEvent{Step: id, Status: status, Message: msg})
	}
}

func (p *playgroundRun) fail(id, msg, code, activity string) (*types.OperationResponse, error) {
	if p.emit != nil {
		p.emit(UpdateStepEvent{Step: id, Status: "failed", Message: msg, ErrorCode: code})
	}
	p.logs = append(p.logs, msg)
	slog.Warn("playground operation stopped", "step", id, "reason", activity, "project", p.project, "user", getUser(p.c))
	p.record(fmt.Sprintf("project=%s (%s)", p.project, activity), false)
	return &types.OperationResponse{Success: false, Message: msg, Logs: p.logs, ErrorCode: code}, nil
}

func (p *playgroundRun) record(detail string, success bool) {
	RecordActivity(p.c, types.ActivityEntry{
		Timestamp: nowRFC3339(),
		User:      getUser(p.c),
		Action:    p.action + "-playground",
		Detail:    detail,
		Success:   success,
	})
}

// RunPlayground sets up or deletes the Gen AI playground in a project
// through the dashboard's gen-ai API, as one operation with streamed
// steps: setup "check", "install", "ready"; teardown "delete", "gone".
// token is what the gen-ai API is called with.
func RunPlayground(c *Client, req types.PlaygroundRequest, token string, onStep func(UpdateStepEvent)) (*types.OperationResponse, error) {
	defer PurgePlaygroundStatus()
	p := &playgroundRun{c: c, emit: onStep, project: req.Project, action: req.Action}
	first := "check"
	if req.Action == "teardown" {
		first = "delete"
	}
	if problem := ValidatePlaygroundRequest(req); problem != "" {
		return p.fail(first, problem, "validation", "invalid request")
	}
	api, problem, code := playgroundAPI(c, token)
	if problem != "" {
		return p.fail(first, problem, code, code)
	}
	p.api = api
	if req.Action == "teardown" {
		return p.teardown()
	}
	return p.setup(req.Models)
}

// TeardownPlayground deletes the playground this tool requested in project
// (Clean up).
func TeardownPlayground(c *Client, project, token string) (*types.OperationResponse, error) {
	return RunPlayground(c, types.PlaygroundRequest{Action: "teardown", Project: project}, token, nil)
}

// failAPI stops on a gen-ai API error with its explanation.
func (p *playgroundRun) failAPI(id string, err error, activity string) (*types.OperationResponse, error) {
	msg, code := apiProblem(err)
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		code = "cancelled"
	}
	return p.fail(id, msg, code, activity)
}

// checkPlayground is the setup's first step: everything the dashboard's
// install needs, checked before anything is written. It returns the
// models to install.
func (p *playgroundRun) checkPlayground(wanted []types.PlaygroundModelRef) ([]types.PlaygroundModel, *genAIServer, *types.OperationResponse) {
	stop := func(msg, code, activity string) ([]types.PlaygroundModel, *genAIServer, *types.OperationResponse) {
		res, _ := p.fail("check", msg, code, activity)
		return nil, nil, res
	}
	if _, msg, code := projectNamespace(p.c, p.project); msg != "" {
		return stop(strings.Replace(msg, "would not show a model there", "would not offer a playground there", 1), code, "project")
	}
	var st types.PlaygroundStatus
	playgroundClusterParts(p.c, &st)
	if st.Blocked != nil {
		return stop(st.Blocked.Reason, "component_off", "component off")
	}
	if st.OperatorReady == "False" {
		return stop(fmt.Sprintf("The playground's operator is not ready: the DataScienceCluster reports it not ready (%s). The dashboard cannot create a playground until it is; the Components page shows the details.", firstNonEmpty(st.OperatorMessage, "no message")), "component_not_ready", "operator not ready")
	}
	switch st.GenAIUI {
	case "missing":
		return stop("The dashboard's Gen AI studio is not deployed on this cluster (no Deployment gen-ai-ui), so it has no playground to set up. A cluster-admin turns Gen AI studio on in the dashboard's settings (OdhDashboardConfig genAiStudio).", "component_off", "gen-ai-ui missing")
	case "unavailable":
		return stop("The dashboard's Gen AI studio backend (Deployment gen-ai-ui) is not available. Wait until it is running; the Components page shows its pods.", "component_not_ready", "gen-ai-ui unavailable")
	}
	var notes []string
	if st.OperatorReady == "" {
		notes = append(notes, "the playground operator's readiness is not reported (unknown)")
	}
	if st.GenAIUI == "" {
		notes = append(notes, "the Gen AI studio backend's state could not be read")
	}
	srv, err := p.api.playgroundStatus(p.project)
	if err != nil {
		msg, code := apiProblem(err)
		return stop(msg, code, "status")
	}
	if srv != nil {
		owned, err := p.owned(srv)
		if err != nil {
			return stop(fmt.Sprintf("Cannot tell whether the playground in %s is this tool's: %v.", p.project, err), errorCodeFromK8sErr(err), "ownership")
		}
		if !owned {
			return stop(fmt.Sprintf("Project %s already has a playground (%s), which this tool did not set up. Use it from the dashboard's Gen AI studio; this tool does not change it.", p.project, firstNonEmpty(srv.Name, "unnamed")), "not_managed", "exists")
		}
		if srv.Phase == "Terminating" {
			return stop(fmt.Sprintf("The playground in %s is being deleted. Wait until it is gone, then set it up again.", p.project), "terminating", "terminating")
		}
	}
	withMaaS := false
	for _, w := range wanted {
		withMaaS = withMaaS || w.Source == "maas"
	}
	models, err := p.api.listProjectModels(p.project, withMaaS)
	if err != nil {
		msg, code := apiProblem(err)
		return stop(msg, code, "models")
	}
	markToolEmbeddingModels(p.c, p.project, models)
	// The dashboard's install checks only the models it is asked to use
	// (rhoai-3.6 generateLlamaStackConfig), so only those must be ready.
	var chosen []types.PlaygroundModel
	if len(wanted) == 0 {
		var left []string
		for _, m := range models {
			switch {
			case m.Source == "namespace" && m.Ready && (m.Chat || m.EmbeddingDimension > 0):
				chosen = append(chosen, m)
			case m.Source == "namespace" && m.Chat:
				left = append(left, fmt.Sprintf("%s (%s)", m.Name, firstNonEmpty(m.Status, "not ready")))
			}
		}
		if len(left) > 0 {
			notes = append(notes, "not ready, so left out: "+strings.Join(left, ", "))
		}
	} else {
		for _, w := range wanted {
			var found *types.PlaygroundModel
			for i := range models {
				if models[i].Name == w.Name && models[i].Source == w.Source {
					found = &models[i]
				}
			}
			switch {
			case found == nil:
				return stop(fmt.Sprintf("The dashboard does not offer model %s in %s. Reload the page and choose again.", w.Name, p.project), "validation", "unknown model")
			case !found.Ready:
				return stop(fmt.Sprintf("Model %s is not ready (%s). Choose a ready model.", w.Name, found.Status), "model_not_ready", "model not ready")
			case !found.Chat && found.EmbeddingDimension == 0:
				return stop(fmt.Sprintf("Model %s is an embedding model this tool did not set up, so its dimension is not known. Add it in the dashboard's Gen AI studio, where you choose Embedding and its dimension.", w.Name), "validation", "embedding model")
			}
			chosen = append(chosen, *found)
		}
	}
	chat := 0
	for _, m := range chosen {
		if m.Chat {
			chat++
		}
	}
	if chat == 0 {
		return stop(fmt.Sprintf("Project %s has no ready chat model for the playground to use. Set up a model on the Models tab first (or choose a Models as a Service model, if you have a subscription).", p.project), "no_model", "no model")
	}
	names := make([]string, len(chosen))
	for i, m := range chosen {
		names[i] = m.Name
		if !m.Chat {
			names[i] += fmt.Sprintf(" (embedding, %d dimensions)", m.EmbeddingDimension)
		}
	}
	msg := fmt.Sprintf("Ready to set up: %s with %s.", p.project, strings.Join(names, ", "))
	if len(notes) > 0 {
		msg += " Note: " + strings.Join(notes, "; ") + "."
	}
	p.step("check", "success", msg)
	return chosen, srv, nil
}

// owned says whether the live playground srv is the one this tool
// requested in the project. Operations hold the cluster lock, so it also
// brings the record up to date: a stale one is dropped, and the server's
// UID is filled in when it was not known yet.
func (p *playgroundRun) owned(srv *genAIServer) (bool, error) {
	records, err := readPlaygroundRecords(p.c)
	if err != nil {
		return false, err
	}
	rec, has := records[p.project]
	owned, uid, err := playgroundOwnership(p.c, p.project, rec, has, srv)
	if err != nil {
		return false, err
	}
	switch {
	case has && !owned:
		p.logs = append(p.logs, fmt.Sprintf("This tool's record of a playground in %s is of one that no longer exists; dropped.", p.project))
		err = setPlaygroundRecord(p.c, p.project, nil)
	case owned && rec.UID == "":
		rec.UID = uid
		err = setPlaygroundRecord(p.c, p.project, &rec)
	}
	if err != nil {
		slog.Warn("cannot update the playground record", "project", p.project, "error", err)
	}
	return owned, nil
}

// alreadyExists finds `"<name>" already exists` in a dashboard message.
var alreadyExistsName = regexp.MustCompile(`"([a-z0-9][-a-z0-9.]*)" already exists`)

func (p *playgroundRun) installError(err error) (*types.OperationResponse, error) {
	e, ok := asGenAIError(err)
	if !ok || e.Status != http.StatusBadRequest || !strings.Contains(strings.ToLower(e.Message), "already exists") {
		return p.failAPI("install", err, "install failed")
	}
	if strings.Contains(e.Message, "OGXServer already exists") || strings.Contains(strings.ToLower(e.Message), "server already exists") {
		return p.fail("install", fmt.Sprintf("Project %s has a playground server the dashboard cannot reuse (made by hand, or by an earlier release): %s. Delete it in the dashboard's Gen AI studio, then set up again.", p.project, e.Message), "conflict", "server exists")
	}
	cmd := fmt.Sprintf("oc get configmap -n %s -l opendatahub.io/dashboard=true", p.project)
	if m := alreadyExistsName.FindStringSubmatch(e.Message); m != nil {
		cmd = fmt.Sprintf("oc delete configmap -n %s %s", p.project, m[1])
	}
	return p.fail("install", fmt.Sprintf("An earlier playground in %s left objects behind without its server, and the dashboard cannot install over them (it answered: %s). This tool did not create them, so it does not delete them. Someone with edit rights in %s removes them, for example: %s (repeat for each one the dashboard names), then set up again.", p.project, e.Message, p.project, cmd), "leftovers", "leftovers")
}

func (p *playgroundRun) setup(wanted []types.PlaygroundModelRef) (*types.OperationResponse, error) {
	models, existing, stopped := p.checkPlayground(wanted)
	if stopped != nil {
		return stopped, nil
	}
	if existing == nil {
		// Recorded before the install, so a playground whose install
		// outlives this process is still known as this tool's.
		if err := setPlaygroundRecord(p.c, p.project, newPlaygroundRecord(p.c)); err != nil {
			return p.fail("install", fmt.Sprintf("Cannot record the request in this tool's namespace; nothing was set up: %v.%s", err, templateHint(err)), errorCodeFromK8sErr(err), "record")
		}
		body := map[string]interface{}{"enable_guardrails": false, "enable_tracing": false, "vector_stores": []interface{}{}}
		// model_type and embedding_dimension per model: odh-dashboard
		// rhoai-3.6 packages/gen-ai/bff internal/models/llamastack_distribution.go
		// InstallModel ("llm" or "embedding"; embedding_dimension 128 to
		// 3072000, checked in api/lsd_install_handler.go), and
		// generateLlamaStackConfig (integrations/kubernetes/token_k8s_client.go)
		// registers a namespace embedding model with
		// AddVLLMProviderAndModel, which falls back to 128 dimensions without
		// one (llamastack_config.go); the dashboard's own dialog asks for it
		// (ChatbotConfigurationTableRow.tsx: "Embedding", "Embedding dimension").
		list := make([]map[string]interface{}, len(models))
		for i, m := range models {
			list[i] = map[string]interface{}{"model_name": m.Name, "model_source_type": m.Source, "model_type": "llm"}
			if !m.Chat {
				list[i]["model_type"] = "embedding"
				list[i]["embedding_dimension"] = m.EmbeddingDimension
			}
		}
		body["models"] = list
		p.step("install", "running", fmt.Sprintf("Asking the dashboard to set up the playground in %s...", p.project))
		// Each attempt runs to its end; the retries stop at the bound.
		var installErr error
		deadline := clk.Now().Add(PlaygroundInstallRetryTimeout)
		said := false
		for {
			installErr = p.api.call(http.MethodPost, "/lsd/install", p.project, body, nil)
			if genAIStatus(installErr) != http.StatusConflict || !clk.Now().Before(deadline) {
				break
			}
			if !said {
				said = true
				p.step("install", "running", fmt.Sprintf("The previous playground's database in %s is still being removed; trying again every %s (up to %s)...", p.project, PlaygroundInstallRetryPoll, clockText(PlaygroundInstallRetryTimeout)))
			}
			if err := clk.Sleep(p.c.ctx, PlaygroundInstallRetryPoll); err != nil {
				installErr = err
				break
			}
		}
		if installErr != nil {
			if genAIStatus(installErr) == http.StatusConflict {
				p.forgetIfAbsent()
				return p.fail("install", fmt.Sprintf("The previous playground's database in %s was still being removed after %s, so the dashboard did not set up a new one. Set up again once it is gone.", p.project, clockText(PlaygroundInstallRetryTimeout)), "in_progress", "still removing")
			}
			p.forgetIfAbsent()
			return p.installError(installErr)
		}
		// The dashboard answers 200 with a server that already exists too
		// (its zero-restart path): one made by someone else since the check
		// is not this tool's.
		srv, err := p.api.playgroundStatus(p.project)
		if err != nil {
			return p.failAPI("install", err, "status")
		}
		if srv != nil {
			owned, err := p.owned(srv)
			if err != nil {
				return p.fail("install", fmt.Sprintf("The dashboard set up a playground in %s, but this tool cannot tell whether it is the one it requested: %v.", p.project, err), errorCodeFromK8sErr(err), "ownership")
			}
			if !owned {
				return p.fail("install", fmt.Sprintf("Someone else set up a playground in %s at the same time (%s), and the dashboard kept theirs. It is not this tool's, so this tool does not change it; use it from the dashboard's Gen AI studio.", p.project, firstNonEmpty(srv.Name, "unnamed")), "not_managed", "exists")
			}
		}
		p.step("install", "success", fmt.Sprintf("The dashboard is setting up the playground in %s.", p.project))
	} else {
		p.step("install", "success", fmt.Sprintf("The playground in %s exists (phase %s); waiting for it.", p.project, firstNonEmpty(existing.Phase, "unknown")))
	}
	p.step("ready", "running", "Waiting until the playground is ready (typically 1 to 2 minutes)...")
	var last, problem string
	var srv *genAIServer
	ready, err := waitForGone(p.c, PlaygroundReadyTimeout, PlaygroundPoll, func(cc *Client) (bool, error) {
		s, err := p.api.with(cc.ctx).playgroundStatus(p.project)
		if err != nil {
			return false, err
		}
		srv = s
		switch {
		case s == nil:
			problem = "the playground disappeared while it was starting (someone deleted it)."
			return true, nil
		case s.Phase == "Ready":
			return true, nil
		case s.Phase == "Failed":
			problem = "the playground's server reports phase Failed. The events of project " + p.project + " and its playground pods say why; delete it and set it up again."
			return true, nil
		}
		if msg := fmt.Sprintf("The playground is starting (phase %s)...", firstNonEmpty(s.Phase, "not reported")); msg != last {
			last = msg
			p.step("ready", "running", msg)
		}
		return false, nil
	})
	switch {
	case err != nil:
		return p.failAPI("ready", err, "wait failed")
	case problem != "":
		return p.fail("ready", "The dashboard set up the playground, but "+problem, "not_ready", "not ready")
	case !ready:
		return p.fail("ready", fmt.Sprintf("The playground in %s is not ready after %s (phase %s). It may still start: the tab shows its state.", p.project, clockText(PlaygroundReadyTimeout), firstNonEmpty(srvPhase(srv), "unknown")), "timeout", "timeout")
	}
	p.step("ready", "success", fmt.Sprintf("The playground in %s is ready.", p.project))
	p.record("project="+p.project, true)
	return &types.OperationResponse{Success: true, Message: fmt.Sprintf("The playground in %s is ready. Open it in the dashboard's Gen AI studio.", p.project), Logs: p.logs}, nil
}

func srvPhase(s *genAIServer) string {
	if s == nil {
		return ""
	}
	return s.Phase
}

// forgetIfAbsent clears the record after a failed install that left no
// playground.
func (p *playgroundRun) forgetIfAbsent() {
	if srv, err := p.api.playgroundStatus(p.project); err == nil && srv == nil {
		if err := setPlaygroundRecord(p.c, p.project, nil); err != nil {
			slog.Warn("cannot clear the playground record", "project", p.project, "error", err)
		}
	}
}

func (p *playgroundRun) teardown() (*types.OperationResponse, error) {
	records, err := readPlaygroundRecords(p.c)
	if err != nil {
		return p.fail("delete", fmt.Sprintf("Cannot read this tool's records; nothing was deleted: %v.", err), errorCodeFromK8sErr(err), "records")
	}
	_, recorded := records[p.project]
	srv, err := p.api.playgroundStatus(p.project)
	if err != nil {
		return p.failAPI("delete", err, "status")
	}
	if srv == nil {
		if recorded {
			if err := setPlaygroundRecord(p.c, p.project, nil); err != nil {
				return p.fail("delete", fmt.Sprintf("No playground is left in %s, but this tool's record of it cannot be cleared: %v.", p.project, err), errorCodeFromK8sErr(err), "record")
			}
		}
		p.record("project="+p.project+" (none)", true)
		return &types.OperationResponse{Success: true, Message: fmt.Sprintf("No playground in %s; nothing to delete.", p.project), Logs: p.logs}, nil
	}
	ours, err := p.owned(srv)
	if err != nil {
		return p.fail("delete", fmt.Sprintf("Cannot tell whether the playground in %s is this tool's, so nothing was deleted: %v.", p.project, err), errorCodeFromK8sErr(err), "ownership")
	}
	if !ours {
		return p.fail("delete", fmt.Sprintf("The playground in %s was not set up by this tool, so it was not deleted. Delete it in the dashboard's Gen AI studio if you no longer need it.", p.project), "not_managed", "not managed")
	}
	if srv.Name == "" {
		return p.fail("delete", fmt.Sprintf("The dashboard reports a playground in %s without a name, so it cannot be deleted through its API. Delete it in the dashboard's Gen AI studio.", p.project), "unknown", "no name")
	}
	if srv.Phase != "Terminating" {
		p.step("delete", "running", fmt.Sprintf("Asking the dashboard to delete the playground %s in %s...", srv.Name, p.project))
		if err := p.api.call(http.MethodDelete, "/lsd/delete", p.project, map[string]string{"name": srv.Name}, nil); err != nil {
			return p.failAPI("delete", err, "delete failed")
		}
	}
	p.step("delete", "success", fmt.Sprintf("The dashboard is deleting the playground in %s.", p.project))
	p.step("gone", "running", "Waiting until the playground is gone...")
	gone, err := waitForGone(p.c, PlaygroundDeleteTimeout, PlaygroundPoll, func(cc *Client) (bool, error) {
		s, err := p.api.with(cc.ctx).playgroundStatus(p.project)
		return err == nil && s == nil, err
	})
	if err != nil {
		return p.failAPI("gone", err, "wait failed")
	}
	if !gone {
		return p.fail("gone", fmt.Sprintf("The playground in %s is still being deleted after %s. Delete again to check.", p.project, clockText(PlaygroundDeleteTimeout)), "in_progress", "still deleting")
	}
	if err := setPlaygroundRecord(p.c, p.project, nil); err != nil {
		return p.fail("gone", fmt.Sprintf("The playground in %s is deleted, but this tool's record of it cannot be cleared: %v.%s", p.project, err, templateHint(err)), errorCodeFromK8sErr(err), "record")
	}
	p.step("gone", "success", fmt.Sprintf("The playground in %s is gone.", p.project))
	p.record("project="+p.project, true)
	return &types.OperationResponse{Success: true, Message: fmt.Sprintf("Playground deleted from %s: the dashboard removed its server, configuration and database. The project and its models stay.", p.project), Logs: p.logs}, nil
}
