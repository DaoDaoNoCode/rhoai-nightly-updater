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
	// An entry that recorded its project's namespace UID is gone also when
	// a namespace of that name now has another UID (the project was made
	// again); one without (from before the UID was kept) goes by its name.
	projectUIDGone := func(project, uid string) bool {
		st := namespace(project)
		return st.gone || uid != "" && st.uid != "" && st.uid != uid
	}
	projectOf := func(key string) string {
		project, _, _ := strings.Cut(key, "/")
		return project
	}
	var forgotten []string

	if models, err := serving.ReadModelsRecord(c); err != nil {
		slog.Warn("background check: the models record cannot be read", "error", err)
	} else if maas, err := serving.ReadMaaSRecord(c); err != nil {
		slog.Warn("background check: the MaaS record cannot be read", "error", err)
	} else {
		changed := false
		for key := range models {
			project, model, _ := strings.Cut(key, "/")
			if maas.IsPublished(project, model) || !projectGone(project) {
				continue
			}
			delete(models, key)
			changed = true
			forgotten = append(forgotten, "model "+key)
		}
		if changed {
			if err := serving.WriteModelsRecord(c, models); err != nil {
				slog.Warn("background check: the models record cannot be written", "error", err)
			}
		}
	}

	if pipes, err := storage.ReadPipelineRecord(c); err != nil {
		slog.Warn("background check: the pipeline server record cannot be read", "error", err)
	} else {
		changed := false
		for key := range pipes {
			if projectGone(projectOf(key)) {
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

	if dropped, err := workbench.ForgetGoneProjects(c, projectUIDGone); err != nil {
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
