package modelregistry

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/mlflow"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/serving"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Registration mirrors the dashboard Register model form (odh-dashboard rhoai-3.6
// packages/model-registry/upstream/frontend/src/app/pages/modelRegistry/screens/RegisterModel/utils.ts:
// 39-141): calls go through the registry REST API at the endpoint the Service exposes
// via the cluster gateway, authenticated with the signed-in user's token—the same path
// the dashboard BFF takes (federated mode, user_token). kube-rbac-proxy on the registry
// validates that token for get services/<name>. Unlike MLflow samples (mlflow/samples_http.go),
// nothing is directed at a Service or Route in an end-user project.
//
// The registry exposes no DELETE (api/openapi/model-registry.yaml omits it; models and
// versions may only be archived), and names remain reserved: a repeat setup resolves the
// model by name and transitions it back to LIVE. The sample is tied to the registry alone
// (removing the registry removes its database as well).

// apiPath matches the API version the dashboard BFF invokes.
const apiPath = "/api/model_registry/v1alpha3"

// sampleVersion names the sample version (artifact naming follows the dashboard, keyed off
// this value).
const sampleVersion = "v1"

// sampleModel is the Models tab default entry: the sample references the weights that tab
// serves.
func sampleModel() serving.CatalogModel {
	for _, m := range serving.ModelCatalog {
		if m.Default {
			return m
		}
	}
	return serving.ModelCatalog[0]
}

// SampleName returns the name under which the model is registered.
func SampleName() string { return sampleModel().Name }

// SampleURI is the weight location—the URI from which the Models tab deploys the model
// (KServe's hf://<org>/<repo>:<revision>).
func SampleURI() string { return sampleModel().URI }

// sampleFormat is the "Source model format" field on the form: weight format (catalog
// models use safetensors per serving/models.go).
const sampleFormat = "safetensors"

// modelTypeProperty satisfies the 3.6 form requirement for model type on the registered
// model (registerModelTypeUtils.ts:34-49; key model_type, values generative, predictive or
// unknown): chat models count as generative.
var modelTypeProperty = map[string]any{
	"model_type": map[string]any{"metadataType": "MetadataStringValue", "string_value": "generative"},
}

// registeredModelBody builds the form's initial POST registered_models payload.
func registeredModelBody(owner string) map[string]any {
	m := sampleModel()
	body := map[string]any{
		"name":             m.Name,
		"description":      m.Description,
		"customProperties": modelTypeProperty,
		"state":            "LIVE",
	}
	if owner != "" {
		body["owner"] = owner
	}
	return body
}

// versionBody builds the follow-on POST registered_models/{id}/versions payload.
func versionBody(owner, modelID string) map[string]any {
	body := map[string]any{
		"name":              sampleVersion,
		"description":       "The weights the Models tab serves, at the commit it pins.",
		"customProperties":  map[string]any{},
		"state":             "LIVE",
		"registeredModelId": modelID,
	}
	if owner != "" {
		body["author"] = owner
	}
	return body
}

// artifactBody builds the third step: POST model_versions/{id}/artifacts. The form may
// include author, but the dashboard BFF strips it before calling the registry (server
// ModelArtifact has no such field; bff internal/api/model_versions_handler.go), so this
// payload omits it too.
func artifactBody() map[string]any {
	return map[string]any{
		"name":               sampleVersion,
		"description":        "The weights the Models tab serves, at the commit it pins.",
		"customProperties":   map[string]any{},
		"state":              "LIVE",
		"modelFormatName":    sampleFormat,
		"modelFormatVersion": "",
		"uri":                SampleURI(),
		"artifactType":       "model-artifact",
	}
}

// entity holds a model, version, or artifact as returned by the registry.
type entity struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	State string `json:"state"`
}

type entityList struct {
	Items []entity `json:"items"`
	Size  int      `json:"size"`
}

// api constructs the registry REST client for the given gateway address.
func api(c *kube.Client, address string) *mlflow.SampleAPI {
	return &mlflow.SampleAPI{Ctx: c.Context(), Base: address + apiPath, Token: c.BearerToken(), Server: "The model registry"}
}

// send marshals in as JSON when present (nil means no body) and decodes the response into out.
func send(a *mlflow.SampleAPI, method, path string, query url.Values, in, out any) error {
	if in == nil {
		return a.Send(method, path, query, "", nil, out)
	}
	data, err := jsonBody(in)
	if err != nil {
		return err
	}
	return a.Send(method, path, query, "application/json", data, out)
}

// isNotFound reports whether the registry returned 404 for a name-based lookup.
func isNotFound(err error) bool {
	var e *mlflow.SampleAPIError
	return errors.As(err, &e) && e.Status == http.StatusNotFound
}

// findModel fetches the sample by name (GET registered_model?name=); found is false when
// no matching model exists.
func findModel(a *mlflow.SampleAPI) (entity, bool, error) {
	var m entity
	err := send(a, http.MethodGet, "/registered_model", url.Values{"name": {SampleName()}}, nil, &m)
	if isNotFound(err) {
		return m, false, nil
	}
	return m, err == nil, err
}

// SampleStatus fetches the sample model from the registry at address.
func SampleStatus(u *kube.Client, address string) *types.ModelRegistrySample {
	s := &types.ModelRegistrySample{Model: SampleName(), Version: sampleVersion, URI: SampleURI()}
	a := api(u, address)
	m, found, err := findModel(a)
	switch {
	case err != nil:
		s.Problem = fmt.Sprintf("Cannot read the sample model: %v.", err)
		return s
	case !found:
		return s
	}
	s.Registered, s.State, s.ID = true, m.State, m.ID
	v, art, err := sampleParts(a, m.ID)
	if err != nil {
		s.Problem = fmt.Sprintf("Cannot read the sample model's version: %v.", err)
		return s
	}
	s.Complete = m.State == "LIVE" && v.State == "LIVE" && art.ID != "" && (art.State == "" || art.State == "LIVE")
	return s
}

// sampleParts reads the sample's version and its artifact (empty when
// missing).
func sampleParts(a *mlflow.SampleAPI, modelID string) (entity, entity, error) {
	var v, art entity
	var versions entityList
	if err := send(a, http.MethodGet, "/registered_models/"+url.PathEscape(modelID)+"/versions", url.Values{"name": {sampleVersion}}, nil, &versions); err != nil && !isNotFound(err) {
		return v, art, err
	}
	for _, it := range versions.Items {
		if it.Name == sampleVersion {
			v = it
		}
	}
	if v.ID == "" {
		return v, art, nil
	}
	var artifacts entityList
	if err := send(a, http.MethodGet, "/model_versions/"+url.PathEscape(v.ID)+"/artifacts", nil, nil, &artifacts); err != nil {
		return v, art, err
	}
	for _, it := range artifacts.Items {
		if it.Name == sampleVersion {
			art = it
		}
	}
	return v, art, nil
}

// registerSample ensures the sample model, version, and artifact exist (creating each only
// if absent) and reactivates archived models or versions. say receives a message after each
// registry call.
func registerSample(c *kube.Client, address string, say func(string)) error {
	a := api(c, address)
	owner := kube.GetUser(c)
	if owner == "unknown" {
		owner = ""
	}
	m, found, err := findModel(a)
	if err != nil {
		return fmt.Errorf("look up model %s: %w", SampleName(), err)
	}
	if !found {
		if err := send(a, http.MethodPost, "/registered_models", nil, registeredModelBody(owner), &m); err != nil {
			return fmt.Errorf("register model %s: %w", SampleName(), err)
		}
		say(fmt.Sprintf("Registered model %s.", SampleName()))
	} else if m.State != "LIVE" {
		if err := send(a, http.MethodPatch, "/registered_models/"+url.PathEscape(m.ID), nil, map[string]any{"state": "LIVE"}, &m); err != nil {
			return fmt.Errorf("restore model %s: %w", SampleName(), err)
		}
		say(fmt.Sprintf("Model %s was archived; it is live again.", SampleName()))
	} else {
		say(fmt.Sprintf("Model %s is registered already.", SampleName()))
	}
	if m.ID == "" {
		return fmt.Errorf("the registry answered without the id of model %s", SampleName())
	}

	var versions entityList
	if err := send(a, http.MethodGet, "/registered_models/"+url.PathEscape(m.ID)+"/versions", url.Values{"name": {sampleVersion}}, nil, &versions); err != nil && !isNotFound(err) {
		return fmt.Errorf("look up version %s of %s: %w", sampleVersion, SampleName(), err)
	}
	var v entity
	for _, it := range versions.Items {
		if it.Name == sampleVersion {
			v = it
		}
	}
	switch {
	case v.ID == "":
		if err := send(a, http.MethodPost, "/registered_models/"+url.PathEscape(m.ID)+"/versions", nil, versionBody(owner, m.ID), &v); err != nil {
			return fmt.Errorf("register version %s of %s: %w", sampleVersion, SampleName(), err)
		}
		say(fmt.Sprintf("Registered version %s.", sampleVersion))
	case v.State != "LIVE":
		if err := send(a, http.MethodPatch, "/model_versions/"+url.PathEscape(v.ID), nil, map[string]any{"state": "LIVE"}, &v); err != nil {
			return fmt.Errorf("restore version %s of %s: %w", sampleVersion, SampleName(), err)
		}
		say(fmt.Sprintf("Version %s was archived; it is live again.", sampleVersion))
	}
	if v.ID == "" {
		return fmt.Errorf("the registry answered without the id of version %s", sampleVersion)
	}

	var artifacts entityList
	if err := send(a, http.MethodGet, "/model_versions/"+url.PathEscape(v.ID)+"/artifacts", nil, nil, &artifacts); err != nil {
		return fmt.Errorf("read the artifacts of version %s: %w", sampleVersion, err)
	}
	for _, it := range artifacts.Items {
		if it.Name != sampleVersion {
			continue
		}
		if it.State == "" || it.State == "LIVE" {
			return nil
		}
		// PATCH model_artifacts/{id} (ModelArtifactUpdate); the type names
		// which kind of artifact the update is for.
		if err := send(a, http.MethodPatch, "/model_artifacts/"+url.PathEscape(it.ID), nil, map[string]any{"artifactType": "model-artifact", "state": "LIVE"}, nil); err != nil {
			return fmt.Errorf("restore the model artifact of version %s: %w", sampleVersion, err)
		}
		say("Its model artifact was not live; it is live again.")
		return nil
	}
	if err := send(a, http.MethodPost, "/model_versions/"+url.PathEscape(v.ID)+"/artifacts", nil, artifactBody(), nil); err != nil {
		return fmt.Errorf("register the model artifact of version %s: %w", sampleVersion, err)
	}
	say(fmt.Sprintf("Registered its model artifact (%s).", SampleURI()))
	return nil
}
