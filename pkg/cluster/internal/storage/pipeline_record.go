package storage

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
)

// Which pipeline servers are the tool's, for the writes that hand them the
// storage-wide S3 keys.
//
// A DSPA is the tool's for status and teardown when it carries the tool's
// label, or, for the earliest versions, when the tool's field managers own
// its object storage (dspaInfo.managedByTool). A project editor can set
// both on a DSPA of their own: the label is theirs to write, and a client
// names its own fieldManager. That was enough for Migrate storage to write
// the new storage's keys, which open every project's pipeline artifacts,
// into the editor's project. So the tool records the UID of each pipeline
// server it creates or moves, in its own namespace, which project users
// cannot write (the snapshot ConfigMap's data key "pipelineServers", by
// "<project>/<name>", server-side applied under a field manager of its
// own, as the models record is; the RBAC is that ConfigMap's existing
// patch grant). Migrate storage moves a pipeline server only when its UID
// is the recorded one, or when the user confirmed its project in the
// dialog (unticked by default); setup records the one it creates or
// adopts in the project the user chose. Pipeline servers set up before
// 3.0.1 have no record: Migrate storage lists them for that confirmation.
const (
	PipelineRecordKey     = "pipelineServers"
	pipelineRecordManager = "rhoai-nightly-updater-pipelines"
)

type RecordedPipelineServer struct {
	UID string `json:"uid"`
	At  string `json:"at,omitempty"`
	By  string `json:"by,omitempty"`
	// Storage is RecordedOnProjectStorage for a pipeline server on its
	// project's own S3 storage (set up by a project's admin or editor);
	// "" for one on the shared storage.
	Storage string `json:"storage,omitempty"`
	// ActorRole and ProjectUID, for a pipeline server on project storage:
	// record.ActorRoleUser for a user who is not a cluster-admin, and the
	// project's namespace UID.
	ActorRole  string `json:"actorRole,omitempty"`
	ProjectUID string `json:"projectUID,omitempty"`
}

// RecordedOnProjectStorage is RecordedPipelineServer.Storage of a pipeline
// server on project storage. Such an entry needs no cap of its own: a
// pipeline server on project storage needs the project's storage, whose
// entry the caps of users' records count, and deleting the storage drops
// it (forgetProjectPipelineServer).
const RecordedOnProjectStorage = "project"

// PipelineRecord is the record, by "<project>/<name>".
type PipelineRecord map[string]RecordedPipelineServer

// owns: meta is a pipeline server whose UID the tool recorded.
func (r PipelineRecord) Owns(meta kube.ObjectMeta) bool {
	e, ok := r[meta.Namespace+"/"+meta.Name]
	return ok && e.UID != "" && e.UID == meta.UID
}

// ReadPipelineRecord reads the record (empty when nothing is recorded).
func ReadPipelineRecord(c *kube.Client) (PipelineRecord, error) {
	var cm struct {
		Data map[string]string `json:"data"`
	}
	found, err := c.Bookkeeping().GetIntoOptional(rhoai.NamespacedPath("v1", "configmaps", record.GetActivityNamespace(), record.SnapshotConfigMapName), &cm)
	if err != nil {
		if kube.IsDecodeError(err) {
			return nil, fmt.Errorf("parse the snapshot ConfigMap: %w", err)
		}
		return nil, fmt.Errorf("read this tool's record of its pipeline servers: %w", err)
	}
	if !found {
		return PipelineRecord{}, nil
	}
	rec := PipelineRecord{}
	if raw := cm.Data[PipelineRecordKey]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &rec); err != nil {
			return nil, fmt.Errorf("parse this tool's record of its pipeline servers: %w", err)
		}
	}
	return rec, nil
}

func WritePipelineRecord(c *kube.Client, rec PipelineRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	ns := record.GetActivityNamespace()
	cm, _ := json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": record.SnapshotConfigMapName, "namespace": ns},
		"data":       map[string]any{PipelineRecordKey: string(data)},
	})
	query := url.Values{"fieldManager": {pipelineRecordManager}, "force": {"true"}}
	if _, _, err := c.Bookkeeping().Do(http.MethodPatch, rhoai.NamespacedPath("v1", "configmaps", ns, record.SnapshotConfigMapName), "application/apply-patch+yaml", cm, query); err != nil {
		return fmt.Errorf("record this tool's pipeline servers: %w", err)
	}
	return nil
}

// recordPipelineServer records meta (a DSPA as read or written) as the tool's.
func recordPipelineServer(c *kube.Client, meta kube.ObjectMeta) error {
	if meta.UID == "" {
		return fmt.Errorf("record this tool's pipeline servers: %s/%s has no UID", meta.Namespace, meta.Name)
	}
	rec, err := ReadPipelineRecord(c)
	if err != nil {
		return err
	}
	rec[meta.Namespace+"/"+meta.Name] = RecordedPipelineServer{UID: meta.UID, At: kube.NowRFC3339(), By: kube.GetUser(c)}
	return WritePipelineRecord(c, rec)
}

// forgetPipelineServer drops project/name from the record (nothing is
// written when it is not recorded).
func forgetPipelineServer(c *kube.Client, project, name string) error {
	rec, err := ReadPipelineRecord(c)
	if err != nil {
		return err
	}
	if _, ok := rec[project+"/"+name]; !ok {
		return nil
	}
	delete(rec, project+"/"+name)
	return WritePipelineRecord(c, rec)
}
