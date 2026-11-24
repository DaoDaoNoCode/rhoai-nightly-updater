package workbench

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
)

// Workbench ownership for this tool. Because any project editor can attach the
// nightly-workbench name and the tool label to a Notebook, ownership is not
// inferred from labels alone: the tool persists the Notebook UID and the
// PersistentVolumeClaim UID assigned by the API server, scoped to its namespace,
// under snapshot ConfigMap key "workbenches" as "<project>/nightly-workbench",
// using server-side apply with a dedicated field manager (same pattern as the
// models record). A workbench counts as owned when it has the tool label and
// its UID matches the stored value; the same rule applies to its storage.
const (
	RecordKey     = "workbenches"
	recordManager = "rhoai-nightly-updater-workbenches"
)

// Recorded describes a single workbench provisioned by the tool.
type Recorded struct {
	UID        string `json:"uid,omitempty"`
	StorageUID string `json:"storageUID,omitempty"`
	At         string `json:"at,omitempty"`
	By         string `json:"by,omitempty"`
}

// Record maps each project to its stored workbench metadata.
type Record map[string]Recorded

func recordKey(project string) string { return project + "/" + Name }

// Get returns the stored entry for project, if any.
func (r Record) Get(project string) (Recorded, bool) {
	e, ok := r[recordKey(project)]
	return e, ok
}

// Projects lists every project that has a record entry, in sorted order.
func (r Record) Projects() []string {
	var out []string
	for k := range r {
		if p, ok := strings.CutSuffix(k, "/"+Name); ok {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// OwnsNotebook reports whether meta is a tool-created workbench in its project.
func (r Record) OwnsNotebook(meta kube.ObjectMeta) bool {
	e, ok := r.Get(meta.Namespace)
	return ok && meta.HasToolLabel() && e.UID != "" && e.UID == meta.UID
}

// OwnsStorage reports whether meta is tool-created workbench storage.
func (r Record) OwnsStorage(meta kube.ObjectMeta) bool {
	e, ok := r.Get(meta.Namespace)
	return ok && meta.HasToolLabel() && e.StorageUID != "" && e.StorageUID == meta.UID
}

// ReadRecord loads the workbench record; an empty map means nothing was stored.
func ReadRecord(c *kube.Client) (Record, error) {
	raw, err := record.ReadSnapshotKey(c, RecordKey)
	if err != nil {
		return nil, fmt.Errorf("read this tool's record of its workbenches: %w", err)
	}
	rec := Record{}
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &rec); err != nil {
			return nil, fmt.Errorf("parse this tool's record of its workbenches: %w", err)
		}
	}
	return rec, nil
}

// writeRecord server-side-applies the full record. Callers already hold the
// cluster lock, so read-modify-write cannot race another writer.
func writeRecord(c *kube.Client, rec Record) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if err := record.WriteSnapshotKey(c, RecordKey, recordManager, string(data)); err != nil {
		return fmt.Errorf("record this tool's workbenches: %w", err)
	}
	return nil
}

// updateRecord loads the record, mutates the project entry via change, removes
// the entry when both UIDs are cleared, then persists the result.
func updateRecord(c *kube.Client, project string, change func(e *Recorded)) error {
	rec, err := ReadRecord(c)
	if err != nil {
		return err
	}
	e := rec[recordKey(project)]
	before := e
	change(&e)
	if e == before {
		return nil
	}
	if e.UID == "" && e.StorageUID == "" {
		delete(rec, recordKey(project))
	} else {
		rec[recordKey(project)] = e
	}
	return writeRecord(c, rec)
}
