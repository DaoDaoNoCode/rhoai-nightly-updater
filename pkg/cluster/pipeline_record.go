package cluster

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
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
	pipelineRecordKey     = "pipelineServers"
	pipelineRecordManager = "rhoai-nightly-updater-pipelines"
)

type recordedPipelineServer struct {
	UID string `json:"uid"`
	At  string `json:"at,omitempty"`
	By  string `json:"by,omitempty"`
}

// pipelineRecord is the record, by "<project>/<name>".
type pipelineRecord map[string]recordedPipelineServer

// owns: meta is a pipeline server whose UID the tool recorded.
func (r pipelineRecord) owns(meta objectMeta) bool {
	e, ok := r[meta.Namespace+"/"+meta.Name]
	return ok && e.UID != "" && e.UID == meta.UID
}

// readPipelineRecord reads the record (empty when nothing is recorded).
func readPipelineRecord(c *Client) (pipelineRecord, error) {
	var cm struct {
		Data map[string]string `json:"data"`
	}
	found, err := c.bookkeeping().getIntoOptional(namespacedPath("v1", "configmaps", getActivityNamespace(), snapshotConfigMapName), &cm)
	if err != nil {
		if isDecodeError(err) {
			return nil, fmt.Errorf("parse the snapshot ConfigMap: %w", err)
		}
		return nil, fmt.Errorf("read this tool's record of its pipeline servers: %w", err)
	}
	if !found {
		return pipelineRecord{}, nil
	}
	rec := pipelineRecord{}
	if raw := cm.Data[pipelineRecordKey]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &rec); err != nil {
			return nil, fmt.Errorf("parse this tool's record of its pipeline servers: %w", err)
		}
	}
	return rec, nil
}

func writePipelineRecord(c *Client, rec pipelineRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	ns := getActivityNamespace()
	cm, _ := json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": snapshotConfigMapName, "namespace": ns},
		"data":       map[string]any{pipelineRecordKey: string(data)},
	})
	query := url.Values{"fieldManager": {pipelineRecordManager}, "force": {"true"}}
	if _, _, err := c.bookkeeping().do(http.MethodPatch, namespacedPath("v1", "configmaps", ns, snapshotConfigMapName), "application/apply-patch+yaml", cm, query); err != nil {
		return fmt.Errorf("record this tool's pipeline servers: %w", err)
	}
	return nil
}

// recordPipelineServer records meta (a DSPA as read or written) as the tool's.
func recordPipelineServer(c *Client, meta objectMeta) error {
	if meta.UID == "" {
		return fmt.Errorf("record this tool's pipeline servers: %s/%s has no UID", meta.Namespace, meta.Name)
	}
	rec, err := readPipelineRecord(c)
	if err != nil {
		return err
	}
	rec[meta.Namespace+"/"+meta.Name] = recordedPipelineServer{UID: meta.UID, At: nowRFC3339(), By: getUser(c)}
	return writePipelineRecord(c, rec)
}

// forgetPipelineServer drops project/name from the record (nothing is
// written when it is not recorded).
func forgetPipelineServer(c *Client, project, name string) error {
	rec, err := readPipelineRecord(c)
	if err != nil {
		return err
	}
	if _, ok := rec[project+"/"+name]; !ok {
		return nil
	}
	delete(rec, project+"/"+name)
	return writePipelineRecord(c, rec)
}
