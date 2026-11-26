package serving

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
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
	// ActorRole is record.ActorRoleUser when a user who is not a
	// cluster-admin set it up (record.UserRecordCapRefusal counts it),
	// record.ActorRoleAdmin for a cluster-admin; "" before 5.0.0.
	ActorRole string `json:"actorRole,omitempty"`
	// ProjectUID is the project's namespace UID, for a user's entry: the
	// caps count a project deleted and made again as another one, and the
	// background check forgets the entry once the namespace has another.
	ProjectUID string `json:"projectUID,omitempty"`
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
	cmData, found, err := record.ReadSnapshotData(c)
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
	if raw := cmData[ModelsRecordKey]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &rec); err != nil {
			return nil, fmt.Errorf("parse this tool's record of its models: %w", err)
		}
	}
	return rec, nil
}

// WriteModelsRecord applies the whole record. A cluster-admin's Models
// operation holds the cluster lock, a project's model action its
// project's lock; the read-change-write cycles below run through
// record.UpdateSnapshot, so project actions in other projects of this
// process do not race them.
func WriteModelsRecord(c *kube.Client, rec ModelsRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if err := record.ApplySnapshotKeys(c, modelsRecordManager, map[string]any{ModelsRecordKey: string(data)}); err != nil {
		return fmt.Errorf("record this tool's models: %w", err)
	}
	return nil
}

// recordModel records the model the tool just created (meta as the create
// returned it). A user's entry is admitted by the caps in the same step as
// its write (record.AdmitUserEntry): a refusal is a
// *record.RecordsFullError, and nothing is written.
func recordModel(c *kube.Client, meta kube.ObjectMeta) error {
	return record.UpdateSnapshot(c, func(c *kube.Client) error {
		rec, err := ReadModelsRecord(c)
		if err != nil {
			return err
		}
		key := ModelRecordKey(meta.Namespace, meta.Name)
		e := RecordedModel{UID: meta.UID, At: kube.NowRFC3339(), By: kube.GetUser(c), ActorRole: record.ActorRole(c), ProjectUID: c.ProjectUID()}
		if e.ActorRole == record.ActorRoleUser {
			var prior *record.RecordOwner
			if old, had := rec[key]; had {
				prior = &record.RecordOwner{Project: meta.Namespace, ProjectUID: old.ProjectUID, By: old.By, ActorRole: old.ActorRole}
			}
			data, err := json.Marshal(e)
			if err != nil {
				return err
			}
			if err := record.AdmitUserEntry(c, meta.Namespace, prior, data); err != nil {
				return err
			}
		}
		rec[key] = e
		return WriteModelsRecord(c, rec)
	})
}

// recordTokenSecret records the token Secret the tool made for its model.
func recordTokenSecret(c *kube.Client, project, name, uid string) error {
	return record.UpdateSnapshot(c, func(c *kube.Client) error {
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
	})
}

// forgetModels drops the entries of keys from the record (nothing is
// written when none is recorded).
func forgetModels(c *kube.Client, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	return record.UpdateSnapshot(c, func(c *kube.Client) error {
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
	})
}

// ForgetGoneModels drops the entries gone answers true for (by project,
// model name and entry), in one read-change-write through
// record.UpdateSnapshot, and returns the keys it dropped. An entry gone
// cannot tell about stays (fail closed: gone says false).
func ForgetGoneModels(c *kube.Client, gone func(project, name string, e RecordedModel) bool) ([]string, error) {
	var dropped []string
	err := record.UpdateSnapshot(c, func(c *kube.Client) error {
		rec, err := ReadModelsRecord(c)
		if err != nil {
			return err
		}
		for key, e := range rec {
			project, name, _ := strings.Cut(key, "/")
			if gone(project, name, e) {
				delete(rec, key)
				dropped = append(dropped, key)
			}
		}
		if len(dropped) == 0 {
			return nil
		}
		return WriteModelsRecord(c, rec)
	})
	sort.Strings(dropped)
	return dropped, err
}

// ModelLive reports whether project still has the catalog model name with
// this UID, read with c (the ServiceAccount's get on LLMInferenceServices,
// which deploy/template.yaml grants cluster-wide): for the background
// check only, never shown.
func ModelLive(c *kube.Client, project, name, uid string) (bool, error) {
	path, ok := ToolModelPath(project, name)
	if !ok || uid == "" {
		return false, nil
	}
	meta, found, err := kube.ReadObjectMeta(c, path)
	if err != nil {
		return false, err
	}
	return found && meta.UID == uid, nil
}

// recordCapRefusal is why a model setup by a user who is not a
// cluster-admin must not add or take over the model's entry in the
// records (record.AdmitUserEntry), with its errorCode; "" for a
// cluster-admin. The write itself is admitted again (recordModel), so
// this early check only refuses before anything is made.
func recordCapRefusal(c *kube.Client, rec ModelsRecord, project, name string) (string, string) {
	var prior *record.RecordOwner
	if e, has := rec[ModelRecordKey(project, name)]; has {
		prior = &record.RecordOwner{Project: project, ProjectUID: e.ProjectUID, By: e.By, ActorRole: e.ActorRole}
	}
	if err := record.AdmitUserEntry(c, project, prior, nil); err != nil {
		return record.ErrorText(c, err), record.ErrorCode(err)
	}
	return "", ""
}
