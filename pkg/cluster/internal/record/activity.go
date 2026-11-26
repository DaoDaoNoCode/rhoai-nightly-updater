package record

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// maxConflictRetries bounds the read-modify-write attempts of one entry.
// Writers that collide each lose at most once per other writer that wins,
// so this many attempts let that many concurrent writers all land. This
// process's own entries are written one at a time (activityWriter), so
// conflicts come from another updater pod's writes only; the bound is
// kept as it was.
const maxConflictRetries = 8

// activityBackoff is the wait before attempt (1 or more) after a conflict:
// a random part of 25 ms doubled per attempt, at most 800 ms, so writers
// that collided do not collide again in step. Tests replace it.
var activityBackoff = jitteredBackoff

// jitteredBackoff is the wait before attempt (1 or more) of a write retried
// after a conflict: a random part of 25 ms doubled per attempt, at most
// 800 ms.
func jitteredBackoff(attempt int) time.Duration {
	ceiling := min(25*time.Millisecond<<(attempt-1), 800*time.Millisecond)
	return time.Duration(rand.Int64N(int64(ceiling))) + time.Millisecond
}

const (
	ActivityConfigMapName  = "rhoai-nightly-updater-activity"
	OperationConfigMapName = "rhoai-nightly-updater-operation"
	// MaxActivityEntries is the retention of a category without its own.
	MaxActivityEntries = 20
	// MaxActivityBytes keeps the stored log far below the 1 MiB ConfigMap
	// limit (https://kubernetes.io/docs/concepts/configuration/configmap/).
	MaxActivityBytes  = 256 << 10
	MaxActivityDetail = 1024
)

// The users' bucket (the non-admin design, decision D6): the entries of
// actions run by signed-in users who are not cluster-admins (actorRole
// "user", project actions) are kept apart from the cluster-admins'. At most
// MaxUserActivityEntries of them are kept, and at most
// MaxUserActivityEntriesPerUser of one user's (the newest), and they are
// trimmed first when the log is too large. So users cannot push the
// admins' history out, which keeps its retention per category.
const (
	MaxUserActivityEntries        = 40
	MaxUserActivityEntriesPerUser = 5
)

// Retention per category of the cluster-admins' entries, so frequent
// Dashboard Dev changes do not evict the operator history. At most
// 50+30+20+20+20+20 = 160 entries are kept, plus the users' bucket.
var ActivityRetention = map[string]int{
	"operator":       50,
	"dashboard-dev":  30,
	"test-resources": 20,
	"setup":          20,
	"diagnostics":    20,
}

// ActivityActions maps each recorded action to its category and label.
var ActivityActions = map[string][2]string{
	"update":                        {"operator", "Updated to nightly"},
	"refresh":                       {"operator", "Operator refreshed"},
	"reinstall":                     {"operator", "Different build installed"},
	"rollback":                      {"operator", "Rolled back to stable"},
	"deploy-pr":                     {"dashboard-dev", "Dashboard PR deployed"},
	"deploy-dashboard-pr":           {"dashboard-dev", "Dashboard PR deployed"},
	"deploy-dashboard-main":         {"dashboard-dev", "Dashboard main deployed"},
	"revert-dashboard":              {"dashboard-dev", "Dashboard reverted"},
	"setup-s3":                      {"test-resources", "S3 storage set up"},
	"teardown-s3":                   {"test-resources", "S3 storage torn down"},
	"setup-pipeline-server":         {"test-resources", "Pipeline server set up"},
	"teardown-pipeline-server":      {"test-resources", "Pipeline server torn down"},
	"setup-mlflow":                  {"test-resources", "MLflow set up"},
	"teardown-mlflow":               {"test-resources", "MLflow torn down"},
	"deploy-mlflow-pr":              {"test-resources", "MLflow PR deployed"},
	"revert-mlflow":                 {"test-resources", "MLflow reverted"},
	"create-pull-secret":            {"setup", "Pull secret configured"},
	"create-image-mirror":           {"setup", "Image mirror created"},
	"remove-image-mirror":           {"setup", "Image mirror removed"},
	"add-node-pull-secret":          {"setup", "Added to the nodes' pull secret"},
	"remove-node-pull-secret":       {"setup", "Removed from the nodes' pull secret"},
	"copy-registry-pull-secret":     {"setup", "Pull secret copied for the internal registry"},
	"remove-registry-pull-secret":   {"setup", "Internal registry's copy of the pull secret removed"},
	"create-dsc":                    {"setup", "DataScienceCluster created"},
	"repair-dsc":                    {"setup", "DataScienceCluster repaired"},
	"set-component-state":           {"setup", "DataScienceCluster component changed"},
	"fix-dsc":                       {"setup", "DataScienceCluster fixed"},
	"reset-dsc":                     {"setup", "DataScienceCluster reset in place"},
	"hard-reset-dsc":                {"setup", "DataScienceCluster hard reset"},
	"recipe-start":                  {"setup", "Environment recipe started"},
	"recipe":                        {"setup", "Environment recipe finished"},
	"create-recipe-project":         {"setup", "Recipe project created"},
	"delete-recipe-project":         {"setup", "Recipe project deleted"},
	"assist-rollout":                {"diagnostics", "Stuck rollout assisted"},
	"fix-delete-stale-webhooks":     {"diagnostics", "Stale webhooks deleted"},
	"fix-recreate-subscription":     {"diagnostics", "Subscription recreated"},
	"fix-delete-stale-installplans": {"diagnostics", "Failed InstallPlans deleted"},
	"fix-maas-gateway":              {"diagnostics", "MaaS gateway fixed"},
	"disable-component":             {"diagnostics", "Component disabled"},
	"restart-operator":              {"diagnostics", "Operator restarted"},
}

// ActivityFailedLabels names a failed or refused action; actions without an
// entry get "<label> (failed)" in the UI.
var ActivityFailedLabels = map[string]string{
	"update":                      "Update to nightly failed",
	"refresh":                     "Operator refresh failed",
	"reinstall":                   "Installing a different build failed",
	"deploy-dashboard-pr":         "Dashboard PR deploy failed",
	"deploy-dashboard-main":       "Dashboard main deploy failed",
	"revert-dashboard":            "Dashboard revert failed",
	"setup-s3":                    "S3 storage setup failed",
	"teardown-s3":                 "S3 storage teardown failed",
	"setup-pipeline-server":       "Pipeline server setup failed",
	"teardown-pipeline-server":    "Pipeline server teardown failed",
	"setup-mlflow":                "MLflow setup failed",
	"teardown-mlflow":             "MLflow teardown failed",
	"deploy-mlflow-pr":            "MLflow PR deploy failed",
	"revert-mlflow":               "MLflow revert failed",
	"create-pull-secret":          "Pull secret update failed",
	"create-image-mirror":         "Creating the image mirror failed",
	"remove-image-mirror":         "Removing the image mirror failed",
	"add-node-pull-secret":        "Adding to the nodes' pull secret failed",
	"remove-node-pull-secret":     "Removing from the nodes' pull secret failed",
	"copy-registry-pull-secret":   "Copying the pull secret for the internal registry failed",
	"remove-registry-pull-secret": "Removing the internal registry's copy failed",
	"create-dsc":                  "DataScienceCluster creation failed",
	"repair-dsc":                  "DataScienceCluster repair failed",
	"hard-reset-dsc":              "DataScienceCluster hard reset failed",
	"recipe":                      "Environment recipe stopped or failed",
	"create-recipe-project":       "Creating the recipe project failed",
	"delete-recipe-project":       "Deleting the recipe project failed",
}

// legacyActivityActions are the names earlier versions recorded for an
// action, read as the current one: entries already in the log keep their
// category and label.
var legacyActivityActions = map[string]string{
	"setup-minio":    "setup-s3",
	"teardown-minio": "teardown-s3",
}

// currentActivityAction is the current name of a recorded action.
func currentActivityAction(action string) string {
	if a, ok := legacyActivityActions[action]; ok {
		return a
	}
	return action
}

func ActivityCategory(action string) string {
	if a, ok := ActivityActions[currentActivityAction(action)]; ok {
		return a[0]
	}
	return "other"
}

// imageRefPattern finds an image reference with a tag and/or digest.
var imageRefPattern = regexp.MustCompile(`[a-z0-9.-]+(?::[0-9]+)?/[a-zA-Z0-9._/-]+?(?::([a-zA-Z0-9._-]+))?(?:@sha256:([a-f0-9]{64}))?(?:\s|$)`)

// activityBuild returns "<tag> · <short digest>" for the image in detail.
func activityBuild(detail string) string {
	m := imageRefPattern.FindStringSubmatch(detail)
	if m == nil || (m[1] == "" && m[2] == "") {
		return ""
	}
	tag, digest := m[1], m[2]
	if len(digest) > 12 {
		digest = digest[:12]
	}
	switch {
	case tag != "" && digest != "":
		return tag + " · " + digest
	case tag != "":
		return tag
	default:
		return "sha256:" + digest
	}
}

// DescribeActivity fills the read-time fields of an entry. An entry
// written before entries had a scope and an actorRole reads as a
// cluster-admin's cluster action: before project actions, only
// cluster-admins changed anything through the updater.
func DescribeActivity(e *types.ActivityEntry) {
	if e.Scope == nil {
		e.Scope = &types.ActivityScope{Kind: types.ActivityScopeCluster}
	}
	if e.ActorRole == "" {
		e.ActorRole = ActorRoleAdmin
	}
	e.Action = currentActivityAction(e.Action)
	e.Category = ActivityCategory(e.Action)
	if l, ok := ActivityFailedLabels[e.Action]; ok && !e.Success {
		e.Label = l
	} else if a, ok := ActivityActions[e.Action]; ok {
		e.Label = a[1]
	} else {
		e.Label = strings.ReplaceAll(e.Action, "-", " ")
	}
	if e.Category == "operator" {
		e.Build = activityBuild(e.Detail)
	}
}

// userActivity reports whether e is in the users' bucket: an entry of a
// user who is not a cluster-admin. An entry without an actorRole is a
// cluster-admin's (DescribeActivity).
func userActivity(e types.ActivityEntry) bool {
	return e.ActorRole == ActorRoleUser
}

// TrimActivity keeps, preserving order, the newest of the users' entries
// within their bucket (MaxUserActivityEntries in total, at most
// MaxUserActivityEntriesPerUser each), the newest of the cluster-admins'
// entries of each category within its retention, and the whole log within
// MaxActivityBytes: the users' entries are dropped first, oldest first,
// then the oldest of the rest.
func TrimActivity(entries []types.ActivityEntry) []types.ActivityEntry {
	counts := map[string]int{}
	perUser := map[string]int{}
	users := 0
	keep := make([]bool, len(entries))
	for i := len(entries) - 1; i >= 0; i-- {
		if e := entries[i]; userActivity(e) {
			if users < MaxUserActivityEntries && perUser[e.User] < MaxUserActivityEntriesPerUser {
				users++
				perUser[e.User]++
				keep[i] = true
			}
			continue
		}
		cat := ActivityCategory(entries[i].Action)
		limit, ok := ActivityRetention[cat]
		if !ok {
			limit = MaxActivityEntries
		}
		if counts[cat] < limit {
			counts[cat]++
			keep[i] = true
		}
	}
	out := make([]types.ActivityEntry, 0, len(entries))
	for i, e := range entries {
		if keep[i] {
			out = append(out, e)
		}
	}
	for len(out) > 1 {
		data, err := json.Marshal(out)
		if err != nil || len(data) <= MaxActivityBytes {
			break
		}
		drop := 0
		for i, e := range out {
			if userActivity(e) {
				drop = i
				break
			}
		}
		out = append(out[:drop:drop], out[drop+1:]...)
	}
	return out
}

func GetActivityNamespace() string {
	if ns := os.Getenv("NAMESPACE"); ns != "" {
		return ns
	}
	if data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		return strings.TrimSpace(string(data))
	}
	return "rhoai-nightly-updater"
}

// Activity appends an activity entry to the ConfigMap-backed log.
// It uses optimistic concurrency (resourceVersion) to prevent lost updates
// when multiple replicas attempt concurrent writes. On 409 Conflict the
// read-modify-write cycle is retried up to maxConflictRetries times, after
// a jittered backoff (activityBackoff, waited on the cluster packages'
// clock).
// It never returns an error to callers — failures are logged as warnings.
//
// The entry's scope and actorRole come from c when the caller did not set
// them (stampActivity).
func Activity(c *kube.Client, entry types.ActivityEntry) {
	entry = stampActivity(c, entry)
	// Not cancelled with the request (for example an SSE connection drop),
	// but bounded: activityWriteTimeout for the whole call, retries
	// included, and never past the operation's shared post-deadline end
	// (see postOperationContext), so a stalled API server cannot hold a
	// shutting-down pod past its drain.
	// This process's entries are written one at a time (activityWriter),
	// so they never conflict with each other. The wait for the others is
	// bounded on its own (activityWaitTimeout, within the same hard end);
	// the write's own budget starts once it may write.
	waitCtx, cancelWait := BookkeepingContext(c, activityWaitTimeout())
	defer cancelWait()
	if beforeActivityWait != nil {
		beforeActivityWait()
	}
	if err := activityWriter.do(waitCtx, func() error {
		ctx, cancel := BookkeepingContext(c, ActivityWriteTimeout)
		defer cancel()
		recordActivityWithClient(c.WithContext(ctx), entry)
		return nil
	}); err != nil {
		slog.Warn("failed to save activity configmap", "error", err)
	}
}

// Action records an action of the signed-in user, stamped now.
func Action(c *kube.Client, action, detail string, success bool) {
	Activity(c, types.ActivityEntry{
		Timestamp: kube.NowRFC3339(),
		User:      kube.GetUser(c),
		Action:    action,
		Detail:    detail,
		Success:   success,
	})
}

// stampActivity fills what an entry says about the operation of c that
// wrote it, unless the caller set it: its actorRole (ActorRole: the
// project actions' gate marks a user's client) and its scope, the project
// of a project action's context (WithActionScope) or the cluster.
func stampActivity(c *kube.Client, e types.ActivityEntry) types.ActivityEntry {
	if c == nil {
		return e
	}
	if e.ActorRole == "" {
		e.ActorRole = ActorRole(c)
	}
	if e.Scope == nil {
		e.Scope = &types.ActivityScope{Kind: types.ActivityScopeCluster}
		if project, uid := ActionScope(c); project != "" {
			e.Scope = &types.ActivityScope{Kind: types.ActivityScopeProject, Project: project, ProjectUID: uid}
		}
	}
	return e
}

// Cancelled records the action of c's user as failed because it was
// cancelled at a safe point: reason "Cancelled by you" or "Cancelled by a
// cluster-admin" (ActionCancelledError.Reason).
func Cancelled(c *kube.Client, action, detail string, cancel *ActionCancelledError) {
	Activity(c, types.ActivityEntry{
		Timestamp: kube.NowRFC3339(),
		User:      kube.GetUser(c),
		Action:    action,
		Detail:    detail,
		Reason:    cancel.Reason(),
	})
}

// activityWaitTimeout bounds the wait of an activity entry for this
// process's other entries: a few writes' budgets.
func activityWaitTimeout() time.Duration { return 4 * ActivityWriteTimeout }

// beforeActivityWait, when set, runs before an entry waits for the others
// (tests).
var beforeActivityWait func()

func recordActivityWithClient(c *kube.Client, entry types.ActivityEntry) {
	ns := GetActivityNamespace()
	// Read-time fields are derived, not stored.
	entry.Label, entry.Category, entry.Build = "", "", ""
	if len(entry.Detail) > MaxActivityDetail {
		entry.Detail = entry.Detail[:MaxActivityDetail] + "…"
	}
	cmPath := rhoai.NamespacedPath("v1", "configmaps", ns, ActivityConfigMapName)

	for attempt := range maxConflictRetries {
		if attempt > 0 {
			slog.Info("retrying activity record after conflict", "attempt", attempt+1)
			if err := kube.Clock().Sleep(c.Context(), activityBackoff(attempt)); err != nil {
				slog.Warn("failed to save activity configmap: no time left to retry after a conflict", "attempts", attempt, "error", err)
				return
			}
		}

		var entries []types.ActivityEntry
		var resourceVersion string
		isNew := false

		// Step 1: GET the ConfigMap with its resourceVersion
		body, found, err := c.Bookkeeping().GetOptional(cmPath)
		if err != nil {
			slog.Warn("failed to get activity configmap", "error", err)
			return
		}
		if !found {
			// ConfigMap does not exist yet — we will create it
			isNew = true
			entries = []types.ActivityEntry{}
		} else {
			var cm map[string]any
			if err := json.Unmarshal(body, &cm); err != nil {
				slog.Warn("failed to parse activity configmap", "error", err)
				return
			}
			// Extract resourceVersion for optimistic concurrency
			if metadata, ok := cm["metadata"].(map[string]any); ok {
				if rv, ok := metadata["resourceVersion"].(string); ok {
					resourceVersion = rv
				}
			}
			data, _ := cm["data"].(map[string]any)
			if raw, ok := data["entries"].(string); ok && raw != "" {
				if err := json.Unmarshal([]byte(raw), &entries); err != nil {
					slog.Warn("failed to parse activity entries", "error", err)
					entries = []types.ActivityEntry{}
				}
			}
		}

		// Step 2: Modify the entries; keep the newest per category
		entries = TrimActivity(append(entries, entry))

		entriesJSON, err := json.Marshal(entries)
		if err != nil {
			slog.Warn("failed to marshal activity entries", "error", err)
			return
		}

		// Step 3: PUT/Create with resourceVersion for optimistic locking
		metadata := map[string]any{
			"name":      ActivityConfigMapName,
			"namespace": ns,
		}
		if resourceVersion != "" {
			metadata["resourceVersion"] = resourceVersion
		}

		cm := map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   metadata,
			"data": map[string]any{
				"entries": string(entriesJSON),
			},
		}

		cmData, err := json.Marshal(cm)
		if err != nil {
			slog.Warn("failed to marshal activity configmap", "error", err)
			return
		}

		if isNew {
			// Create with POST for new ConfigMaps
			cmListPath := rhoai.NamespacedPath("v1", "configmaps", ns, "")
			_, _, err = c.Bookkeeping().Post(cmListPath, cmData)
		} else {
			// Update with PUT for existing ConfigMaps (carries resourceVersion)
			_, _, err = c.Bookkeeping().Put(cmPath, cmData)
		}

		if err != nil {
			// Step 4: If 409 Conflict, retry from step 1
			if kube.IsK8sConflict(err) {
				slog.Warn("activity configmap conflict, will retry", "attempt", attempt+1)
				continue
			}
			slog.Warn("failed to save activity configmap", "error", err)
			return
		}

		// Success
		return
	}

	slog.Warn("failed to save activity configmap after retries", "retries", maxConflictRetries)
}

// GetActivity retrieves the activity log from the ConfigMap.
func GetActivity(c *kube.Client) ([]types.ActivityEntry, error) {
	ns := GetActivityNamespace()
	cmPath := rhoai.NamespacedPath("v1", "configmaps", ns, ActivityConfigMapName)

	var cm map[string]any
	found, err := c.Bookkeeping().GetIntoOptional(cmPath, &cm)
	if err != nil {
		if kube.IsDecodeError(err) {
			return nil, fmt.Errorf("parse activity configmap: %w", err)
		}
		return nil, fmt.Errorf("get activity configmap: %w", err)
	}
	if !found {
		return []types.ActivityEntry{}, nil
	}

	data, _ := cm["data"].(map[string]any)
	raw, ok := data["entries"].(string)
	if !ok || raw == "" {
		return []types.ActivityEntry{}, nil
	}

	var entries []types.ActivityEntry
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return nil, fmt.Errorf("parse activity entries: %w", err)
	}
	for i := range entries {
		DescribeActivity(&entries[i])
	}

	return entries, nil
}
