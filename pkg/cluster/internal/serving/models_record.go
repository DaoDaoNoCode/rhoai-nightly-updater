package serving

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
)

// Which catalog-named models are this tool's.
//
// A model's name is one of the catalog's, which anyone may give an
// LLMInferenceService in a project they can edit, and so are the tool's
// label (app.kubernetes.io/managed-by) and a write under the tool's field
// manager (a client names its own fieldManager). Neither proves who made
// the object: a project editor could make a look-alike qwen3-1-7b and then
// get a cluster-admin to publish it to MaaS for every signed-in user. So
// the tool records the UID the API server gave each model it created, in
// its own namespace, which project users cannot write: the snapshot
// ConfigMap's data key "models", by "<project>/<model>", server-side
// applied under a field manager of its own, as the playground and MaaS
// records are (the RBAC is that ConfigMap's existing patch grant, and
// create when it does not exist yet). A model is the tool's when its UID is
// the recorded one: no record, or another UID (the model was deleted and
// made again elsewhere), is someone else's.
//
// No release ever set up a model of this catalog (Models is new in 3.0.0),
// so nothing earlier needs adopting. A model that a pre-release build of
// 3.0.0 set up has no record and is shown as not this tool's; delete it
// with oc and set it up again (RUNBOOK.md).
const (
	ModelsRecordKey     = "models"
	modelsRecordManager = "rhoai-nightly-updater-models"
)

// RecordedModel is one model the tool created.
type RecordedModel struct {
	// UID is the LLMInferenceService's, as the create returned it.
	UID string `json:"uid"`
	// TokenSecretUID is the UID of the model's token Secret the tool made
	// (models_token.go); the tool reads it only for "Can it be called?"
	// (models_call.go).
	TokenSecretUID string `json:"tokenSecretUID,omitempty"`
	At             string `json:"at,omitempty"`
	By             string `json:"by,omitempty"`
}

// ModelsRecord is the record, by ModelRecordKey.
type ModelsRecord map[string]RecordedModel

func ModelRecordKey(project, name string) string { return project + "/" + name }

// owns: meta is the model the tool created (its UID is the recorded one).
func (r ModelsRecord) Owns(meta kube.ObjectMeta) bool {
	e, ok := r[ModelRecordKey(meta.Namespace, meta.Name)]
	return ok && e.UID != "" && e.UID == meta.UID
}

// ownsModel reads the record and says whether meta is the tool's model.
func ownsModel(c *kube.Client, meta kube.ObjectMeta) (bool, error) {
	rec, err := ReadModelsRecord(c)
	if err != nil {
		return false, err
	}
	return rec.Owns(meta), nil
}

// ReadModelsRecord reads the record (empty when nothing is recorded).
func ReadModelsRecord(c *kube.Client) (ModelsRecord, error) {
	var cm struct {
		Data map[string]string `json:"data"`
	}
	found, err := c.Bookkeeping().GetIntoOptional(rhoai.NamespacedPath("v1", "configmaps", record.GetActivityNamespace(), record.SnapshotConfigMapName), &cm)
	if err != nil {
		if kube.IsDecodeError(err) {
			return nil, fmt.Errorf("parse the snapshot ConfigMap: %w", err)
		}
		return nil, fmt.Errorf("read this tool's record of its models: %w", err)
	}
	if !found {
		return ModelsRecord{}, nil
	}
	rec := ModelsRecord{}
	if raw := cm.Data[ModelsRecordKey]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &rec); err != nil {
			return nil, fmt.Errorf("parse this tool's record of its models: %w", err)
		}
	}
	return rec, nil
}

// WriteModelsRecord applies the whole record (operations hold the cluster
// lock, so read-change-write does not race another writer).
func WriteModelsRecord(c *kube.Client, rec ModelsRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	ns := record.GetActivityNamespace()
	cm, _ := json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": record.SnapshotConfigMapName, "namespace": ns},
		"data":       map[string]any{ModelsRecordKey: string(data)},
	})
	query := url.Values{"fieldManager": {modelsRecordManager}, "force": {"true"}}
	if _, _, err := c.Bookkeeping().Do(http.MethodPatch, rhoai.NamespacedPath("v1", "configmaps", ns, record.SnapshotConfigMapName), "application/apply-patch+yaml", cm, query); err != nil {
		return fmt.Errorf("record this tool's models: %w", err)
	}
	return nil
}

// recordModel records the model the tool just created (meta as the create
// returned it).
func recordModel(c *kube.Client, meta kube.ObjectMeta) error {
	rec, err := ReadModelsRecord(c)
	if err != nil {
		return err
	}
	rec[ModelRecordKey(meta.Namespace, meta.Name)] = RecordedModel{UID: meta.UID, At: record.NowRFC3339(), By: kube.GetUser(c)}
	return WriteModelsRecord(c, rec)
}

// recordTokenSecret records the token Secret the tool made for its model.
func recordTokenSecret(c *kube.Client, project, name, uid string) error {
	rec, err := ReadModelsRecord(c)
	if err != nil {
		return err
	}
	e, ok := rec[ModelRecordKey(project, name)]
	if !ok {
		return fmt.Errorf("record this tool's models: %s/%s is not recorded", project, name)
	}
	e.TokenSecretUID = uid
	rec[ModelRecordKey(project, name)] = e
	return WriteModelsRecord(c, rec)
}

// forgetModels drops the entries of keys from the record (nothing is
// written when none is recorded).
func forgetModels(c *kube.Client, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	rec, err := ReadModelsRecord(c)
	if err != nil {
		return err
	}
	changed := false
	for _, k := range keys {
		if _, ok := rec[k]; ok {
			delete(rec, k)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return WriteModelsRecord(c, rec)
}
