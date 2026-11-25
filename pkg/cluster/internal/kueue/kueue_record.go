package kueue

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
)

// The shape matches what this tool has provisioned and serializes under snapshot
// key "kueue" (server-side apply with its own field manager, same pattern as
// workbench and models records). Project editors and anyone with cluster create
// can clone names and labels, so ownership checks need the tool label and a UID
// recorded here.
const (
	RecordKey     = "kueue"
	recordManager = "rhoai-nightly-updater-kueue"
)

// Record is the Kueue state applied by the tool—one cluster-wide entry per Project at most.
type Record struct {
	Project    string `json:"project,omitempty"`
	ProjectUID string `json:"projectUID,omitempty"`
	// LabelAdded: the tool applied kueue.openshift.io/managed=true on the
	// project; teardown removes only that label.
	LabelAdded bool `json:"labelAdded,omitempty"`
	// LabelPrior is how the label looked before setup: "absent" (when the
	// tool introduces it) or the previous value string.
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

// OptIn holds dashboard opt-in changes the tool performed.
type OptIn struct {
	// DisableKueueSet means disableKueue was set to false; Previous keeps
	// what it was before ("unset" or "true").
	DisableKueueSet bool   `json:"disableKueueSet,omitempty"`
	Previous        string `json:"previous,omitempty"`
	// ConfigNamespace is where odh-dashboard-config lives.
	ConfigNamespace string `json:"configNamespace,omitempty"`
	// Profile UID and creation resourceVersion; the profile is deleted
	// only when nothing else has changed it since.
	ProfileUID             string `json:"profileUID,omitempty"`
	ProfileNamespace       string `json:"profileNamespace,omitempty"`
	ProfileResourceVersion string `json:"profileResourceVersion,omitempty"`
}

func (o *OptIn) empty() bool {
	return o == nil || (!o.DisableKueueSet && o.ProfileUID == "")
}

// Empty is true when nothing in the record reflects tool-owned resources.
func (r Record) Empty() bool {
	return !r.LabelAdded && r.FlavorUID == "" && r.ClusterQueueUID == "" && r.LocalQueueUID == "" &&
		r.RayClusterUID == "" && r.RayJobUID == "" && r.OptIn.empty()
}

// SampleUID looks up the UID persisted for a sample kind.
func (r Record) SampleUID(kind string) string {
	if kind == SampleRayJob {
		return r.RayJobUID
	}
	return r.RayClusterUID
}

// reconcileReplacedProject settles rec's project entries for a project that
// is gone or was deleted and made again (its namespace's UID is not
// rec.ProjectUID). Deleting a namespace deletes what is in it, but a
// namespace an admin force-finalizes can leave its objects in etcd, and
// they come back in a new namespace of that name with their old UIDs and
// the tool's label: those are still the tool's. So the recorded LocalQueue
// and both samples (whichever a setup asks for) are read by name in
// project, and each recorded UID stays only while an object with exactly
// that UID is there (setup then reuses it, Clean up deletes it by UID);
// otherwise it is cleared. ProjectUID and the label entries are always
// cleared: the label was on the old Namespace object. The ResourceFlavor,
// ClusterQueue and dashboard opt-in are cluster-scoped and stay. A read
// that fails is returned, and rec is not changed (fail closed).
func reconcileReplacedProject(c *kube.Client, rec *Record, project string) error {
	next := *rec
	next.ProjectUID = ""
	next.LabelAdded, next.LabelPrior = false, ""
	if rec.LocalQueueUID != "" {
		q, found, err := readLocalQueue(c, project)
		if err != nil {
			return fmt.Errorf("read LocalQueue %s: %w", LocalQueueName, err)
		}
		if !found || q.Metadata.UID != rec.LocalQueueUID {
			next.LocalQueueUID = ""
		}
	}
	for _, kind := range []string{SampleRayCluster, SampleRayJob} {
		if rec.SampleUID(kind) == "" {
			continue
		}
		s, found, err := readSample(c, kind, project)
		if err != nil {
			return fmt.Errorf("read %s: %w", sampleText(kind), err)
		}
		if !found || s.Metadata.UID != rec.SampleUID(kind) {
			next.setSampleUID(kind, "")
		}
	}
	*rec = next
	return nil
}

// setProjectEntries copies from's project-scoped entries (project UID,
// label, LocalQueue and samples) into r.
func (r *Record) setProjectEntries(from Record) {
	r.ProjectUID = from.ProjectUID
	r.LabelAdded, r.LabelPrior = from.LabelAdded, from.LabelPrior
	r.LocalQueueUID, r.RayClusterUID, r.RayJobUID = from.LocalQueueUID, from.RayClusterUID, from.RayJobUID
}

// errRecordChanged: the stored record is no longer the one a run checked.
var errRecordChanged = errors.New("this tool's record of its Kueue setup changed meanwhile")

// writeReconciled stores the project entries reconcileReplacedProject
// settled in rec, only while the stored record still names rec.Project
// with the project UID oldUID; otherwise it returns errRecordChanged and
// writes nothing.
func writeReconciled(c *kube.Client, oldUID string, rec Record) (Record, error) {
	return updateRecordIf(c, func(e *Record) error {
		if e.Project != rec.Project || e.ProjectUID != oldUID {
			return errRecordChanged
		}
		e.setProjectEntries(rec)
		return nil
	})
}

func (r *Record) setSampleUID(kind, uid string) {
	if kind == SampleRayJob {
		r.RayJobUID = uid
	} else {
		r.RayClusterUID = uid
	}
}

// owns reports whether m carries the tool label and its UID equals the stored one.
func owns(m kube.ObjectMeta, uid string) bool {
	return uid != "" && m.UID == uid && hasToolLabel(m)
}

// ReadRecord fetches the tool record from the snapshot; missing data yields zero value.
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

// writeRecord saves rec; an empty wipe also clears project fields. Callers hold
// the cluster lock, so concurrent read-modify-write cannot collide.
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

// updateRecord reads, applies change, and writes only when serialization changes.
func updateRecord(c *kube.Client, change func(r *Record)) (Record, error) {
	return updateRecordIf(c, func(r *Record) error {
		change(r)
		return nil
	})
}

// updateRecordIf is updateRecord with a change that may refuse: its error
// is returned and nothing is written.
func updateRecordIf(c *kube.Client, change func(r *Record) error) (Record, error) {
	rec, err := ReadRecord(c)
	if err != nil {
		return rec, err
	}
	before, _ := json.Marshal(rec)
	if err := change(&rec); err != nil {
		return rec, err
	}
	after, _ := json.Marshal(rec)
	if string(before) == string(after) {
		return rec, nil
	}
	return rec, writeRecord(c, rec)
}
