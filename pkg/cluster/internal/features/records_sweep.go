package features

import (
	"log/slog"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/serving"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/storage"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/workbench"
)

// Background reconciliation (see pkg/api maas_prune_job.go): resources that
// disappear eventually drop out of this application's tracking records without
// manual intervention.
//
// Models, pipeline-server, playground and workbench records store UIDs for objects this
// tool created, keyed by project. Deleting a project removes its contents; a
// later namespace with the same name gets fresh UIDs, so stale rows neither
// validate anything nor serve a purpose—they only clutter storage. The sweep
// removes them. Rows stay when the project still exists, deletion is in progress
// (namespace present), or lookup fails (fail-closed). A model row also remains
// while this tool's MaaS record still references a publication for that model:
// the MaaS pass matches publication UIDs against the model and clears
// publications before project-level cleanup. Persistence touches only this
// tool's ConfigMap, using the ServiceAccount (existing patch permission).

// SweepGoneInBackground runs that background pass: prune orphaned MaaS
// publications first, then strip record entries for removed projects. pkg/api
// acquires the cluster lock and lease for the operation.
func SweepGoneInBackground(c *kube.Client) {
	serving.PruneGoneMaaSPublicationsInBackground(c)
	SweepGoneProjectRecords(c)
}

// SweepGoneProjectRecords drops record entries whose projects no longer exist;
// unreadable or unwritable records are skipped until a later run.
func SweepGoneProjectRecords(c *kube.Client) {
	type nsState struct {
		gone bool
		uid  string
	}
	seen := map[string]nsState{}
	namespace := func(project string) nsState {
		if st, ok := seen[project]; ok {
			return st
		}
		meta, found, err := kube.ReadObjectMeta(c, rhoai.ClusterPath("v1", "namespaces", project))
		st := nsState{gone: err == nil && !found, uid: meta.UID}
		seen[project] = st
		return st
	}
	projectGone := func(project string) bool { return namespace(project).gone }
	// A workbench entry that recorded its project's namespace UID is gone
	// also when a namespace of that name now has another UID (the project
	// was made again), unless what it records still lives there: the
	// namespace may have been replaced between a setup's gate (which read
	// the UID) and its creates. Its storage is checked with the
	// ServiceAccount's list of PersistentVolumeClaims; its Notebook cannot
	// be (the ServiceAccount reads no Notebooks), so an entry that records
	// one stays, as does one whose check fails (fail closed). An entry
	// without a namespace UID (from before it was kept) goes by its name.
	workbenchGone := func(project string, e workbench.Recorded) bool {
		st := namespace(project)
		switch {
		case st.gone:
			return true
		case e.ProjectUID == "" || st.uid == "" || st.uid == e.ProjectUID:
			return false
		case e.UID != "":
			return false
		case e.StorageUID == "":
			return true
		}
		live, err := workbench.StorageLive(c, project, e.StorageUID)
		return err == nil && !live
	}
	projectOf := func(key string) string {
		project, _, _ := strings.Cut(key, "/")
		return project
	}
	var forgotten []string

	// A model entry stays while its publication is recorded. One that
	// recorded its project's namespace UID (a user's) is gone also when a
	// namespace of that name now has another UID, unless the model it
	// records still lives there (the namespace may have been replaced
	// between a setup's gate and its create), checked with the
	// ServiceAccount's get on LLMInferenceServices; a check that fails
	// keeps it (fail closed).
	if maas, err := serving.ReadMaaSRecord(c); err != nil {
		slog.Warn("background check: the MaaS record cannot be read", "error", err)
	} else if dropped, err := serving.ForgetGoneModels(c, func(project, name string, e serving.RecordedModel) bool {
		if maas.IsPublished(project, name) {
			return false
		}
		st := namespace(project)
		switch {
		case st.gone:
			return true
		case e.ProjectUID == "" || st.uid == "" || st.uid == e.ProjectUID:
			return false
		}
		live, err := serving.ModelLive(c, project, name, e.UID)
		return err == nil && !live
	}); err != nil {
		slog.Warn("background check: the models record cannot be read or written", "error", err)
	} else {
		for _, key := range dropped {
			forgotten = append(forgotten, "model "+key)
		}
	}

	if pipes, err := storage.ReadPipelineRecord(c); err != nil {
		slog.Warn("background check: the pipeline server record cannot be read", "error", err)
	} else {
		changed := false
		for key, e := range pipes {
			// One set up on project storage also goes when its project
			// was made again (another namespace UID), as that storage's
			// entry does (it is bounded only through that entry), unless
			// the pipeline server it records still lives there (the
			// namespace may have been replaced around it), or that cannot
			// be told (fail closed), as for a workbench.
			st := namespace(projectOf(key))
			remade := e.Storage == storage.RecordedOnProjectStorage && e.ProjectUID != "" && st.uid != "" && st.uid != e.ProjectUID
			if remade {
				live, err := storage.PipelineServerLive(c, projectOf(key), e.UID)
				remade = err == nil && !live
			}
			if st.gone || remade {
				delete(pipes, key)
				changed = true
				forgotten = append(forgotten, "pipeline server "+key)
			}
		}
		if changed {
			if err := storage.WritePipelineRecord(c, pipes); err != nil {
				slog.Warn("background check: the pipeline server record cannot be written", "error", err)
			}
		}
	}

	if pgs, err := ReadPlaygroundRecords(c); err != nil {
		slog.Warn("background check: the playground record cannot be read", "error", err)
	} else {
		changed := false
		for project := range pgs {
			if projectGone(project) {
				delete(pgs, project)
				changed = true
				forgotten = append(forgotten, "playground in "+project)
			}
		}
		if changed {
			if err := WritePlaygroundRecords(c, pgs); err != nil {
				slog.Warn("background check: the playground record cannot be written", "error", err)
			}
		}
	}

	// A project storage's objects live in its namespace: an entry whose
	// namespace is gone, or was made again (another UID), records nothing
	// that exists.
	storageGone := func(project string, e storage.ProjectStorageRecorded) bool {
		st := namespace(project)
		return st.gone || e.ProjectUID != "" && st.uid != "" && st.uid != e.ProjectUID
	}
	if dropped, err := storage.ForgetGoneProjectStorages(c, storageGone); err != nil {
		slog.Warn("background check: the project storage record cannot be read or written", "error", err)
	} else {
		for _, p := range dropped {
			forgotten = append(forgotten, "project storage in "+p)
		}
	}

	if dropped, err := workbench.ForgetGoneProjects(c, workbenchGone); err != nil {
		slog.Warn("background check: the workbench record cannot be read or written", "error", err)
	} else {
		for _, p := range dropped {
			forgotten = append(forgotten, "workbench in "+p)
		}
	}
	if len(forgotten) > 0 {
		slog.Info("background check: forgot the records of projects that are gone", "entries", strings.Join(forgotten, ", "))
	}
}
