package kueue

import (
	"encoding/json"
	"fmt"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
)

// This struct mirrors what the tool provisioned, stored under key "kueue" in
// the namespace snapshot ConfigMap (server-side apply under a dedicated field
// manager, matching workbench and models records). Because users with project
// edit or cluster create rights can duplicate names and labels, ownership
// requires both the tool label and a UID that appears in this record.
const (
	RecordKey     = "kueue"
	recordManager = "rhoai-nightly-updater-kueue"
)

// Record holds Kueue configuration the tool applied—at most one per cluster, keyed by Project.
type Record struct {
	Project    string `json:"project,omitempty"`
	ProjectUID string `json:"projectUID,omitempty"`
	// LabelAdded means the tool placed kueue.openshift.io/managed=true on
	// the project; cleanup strips solely that label.
	LabelAdded bool `json:"labelAdded,omitempty"`
	// LabelPrior is the label as it was before this tool's setup: "absent"
	// (the only case the tool sets it), or its value.
	LabelPrior      string `json:"labelPrior,omitempty"`
	FlavorUID       string `json:"flavorUID,omitempty"`
	ClusterQueueUID string `json:"clusterQueueUID,omitempty"`
	LocalQueueUID   string `json:"localQueueUID,omitempty"`
	RayClusterUID   string `json:"rayClusterUID,omitempty"`
	RayJobUID       string `json:"rayJobUID,omitempty"`
	OptIn           *OptIn `json:"optIn,omitempty"`
	At              string `json:"at,omitempty"`
	By              string `json:"by,omitempty"`
}

// OptIn captures edits made during dashboard opt-in.
type OptIn struct {
	// DisableKueueSet indicates the tool flipped disableKueue to false;
	// Previous stores the prior state ("unset" or "true").
	DisableKueueSet bool   `json:"disableKueueSet,omitempty"`
	Previous        string `json:"previous,omitempty"`
	// ConfigNamespace names the namespace containing odh-dashboard-config.
	ConfigNamespace string `json:"configNamespace,omitempty"`
	// Queue hardware profile UID plus creation resourceVersion; deletion
	// happens only if the profile was never modified elsewhere.
	ProfileUID             string `json:"profileUID,omitempty"`
	ProfileNamespace       string `json:"profileNamespace,omitempty"`
	ProfileResourceVersion string `json:"profileResourceVersion,omitempty"`
}

func (o *OptIn) empty() bool {
	return o == nil || (!o.DisableKueueSet && o.ProfileUID == "")
}

// Empty reports whether the record contains no tool-owned state.
func (r Record) Empty() bool {
	return !r.LabelAdded && r.FlavorUID == "" && r.ClusterQueueUID == "" && r.LocalQueueUID == "" &&
		r.RayClusterUID == "" && r.RayJobUID == "" && r.OptIn.empty()
}

// SampleUID returns the stored UID for the given sample kind.
func (r Record) SampleUID(kind string) string {
	if kind == SampleRayJob {
		return r.RayJobUID
	}
	return r.RayClusterUID
}

func (r *Record) setSampleUID(kind, uid string) {
	if kind == SampleRayJob {
		r.RayJobUID = uid
	} else {
		r.RayClusterUID = uid
	}
}

// owns is true when m has the tool label and matches the stored UID.
func owns(m kube.ObjectMeta, uid string) bool {
	return uid != "" && m.UID == uid && hasToolLabel(m)
}

// ReadRecord loads the tool record from the snapshot, or zero when absent.
func ReadRecord(c *kube.Client) (Record, error) {
	var rec Record
	raw, err := record.ReadSnapshotKey(c, RecordKey)
	if err != nil {
		return rec, fmt.Errorf("read this tool's record of its Kueue setup: %w", err)
	}
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &rec); err != nil {
			return rec, fmt.Errorf("parse this tool's record of its Kueue setup: %w", err)
		}
	}
	return rec, nil
}

// writeRecord persists rec; clearing everything drops project fields. The
// cluster lock serializes callers, so read-modify-write cannot conflict.
func writeRecord(c *kube.Client, rec Record) error {
	if rec.Empty() && rec.Project == "" {
		rec = Record{}
	} else {
		rec.At, rec.By = kube.NowRFC3339(), kube.GetUser(c)
	}
	if rec.OptIn.empty() {
		rec.OptIn = nil
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if err := record.WriteSnapshotKey(c, RecordKey, recordManager, string(data)); err != nil {
		return fmt.Errorf("record this tool's Kueue setup: %w", err)
	}
	return nil
}

// updateRecord loads the record, runs change, and writes back only if
// marshaled output differs.
func updateRecord(c *kube.Client, change func(r *Record)) (Record, error) {
	rec, err := ReadRecord(c)
	if err != nil {
		return rec, err
	}
	before, _ := json.Marshal(rec)
	change(&rec)
	after, _ := json.Marshal(rec)
	if string(before) == string(after) {
		return rec, nil
	}
	return rec, writeRecord(c, rec)
}
