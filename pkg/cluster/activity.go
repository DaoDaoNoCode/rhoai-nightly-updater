package cluster

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const maxConflictRetries = 3

const (
	activityConfigMapName  = "rhoai-nightly-updater-activity"
	operationConfigMapName = "rhoai-nightly-updater-operation"
	// maxActivityEntries is the retention of a category without its own.
	maxActivityEntries = 20
	// maxActivityBytes keeps the stored log far below the 1 MiB ConfigMap
	// limit (https://kubernetes.io/docs/concepts/configuration/configmap/).
	maxActivityBytes  = 256 << 10
	maxActivityDetail = 1024
)

// Retention per category, so frequent Dashboard Dev changes do not evict
// the operator history. At most 50+30+20+20+20+20 = 160 entries are kept.
var activityRetention = map[string]int{
	"operator":       50,
	"dashboard-dev":  30,
	"test-resources": 20,
	"setup":          20,
	"diagnostics":    20,
}

// activityActions maps each recorded action to its category and label.
var activityActions = map[string][2]string{
	"update":                        {"operator", "Updated to nightly"},
	"refresh":                       {"operator", "Operator refreshed"},
	"reinstall":                     {"operator", "Operator reinstalled"},
	"rollback":                      {"operator", "Rolled back to stable"},
	"deploy-pr":                     {"dashboard-dev", "Dashboard PR deployed"},
	"deploy-dashboard-pr":           {"dashboard-dev", "Dashboard PR deployed"},
	"deploy-dashboard-main":         {"dashboard-dev", "Dashboard main deployed"},
	"revert-dashboard":              {"dashboard-dev", "Dashboard reverted"},
	"setup-minio":                   {"test-resources", "MinIO set up"},
	"teardown-minio":                {"test-resources", "MinIO torn down"},
	"setup-pipeline-server":         {"test-resources", "Pipeline server set up"},
	"teardown-pipeline-server":      {"test-resources", "Pipeline server torn down"},
	"setup-mlflow":                  {"test-resources", "MLflow set up"},
	"teardown-mlflow":               {"test-resources", "MLflow torn down"},
	"deploy-mlflow-pr":              {"test-resources", "MLflow PR deployed"},
	"revert-mlflow":                 {"test-resources", "MLflow reverted"},
	"create-pull-secret":            {"setup", "Pull secret configured"},
	"create-dsc":                    {"setup", "DataScienceCluster created"},
	"repair-dsc":                    {"setup", "DataScienceCluster repaired"},
	"assist-rollout":                {"diagnostics", "Stuck rollout assisted"},
	"fix-delete-stale-webhooks":     {"diagnostics", "Stale webhooks deleted"},
	"fix-recreate-subscription":     {"diagnostics", "Subscription recreated"},
	"fix-delete-stale-installplans": {"diagnostics", "Failed InstallPlans deleted"},
	"fix-maas-gateway":              {"diagnostics", "MaaS gateway fixed"},
	"disable-component":             {"diagnostics", "Component disabled"},
	"restart-operator":              {"diagnostics", "Operator restarted"},
}

// activityFailedLabels names a failed or refused action; actions without an
// entry get "<label> (failed)" in the UI.
var activityFailedLabels = map[string]string{
	"update":                   "Update to nightly failed",
	"refresh":                  "Operator refresh failed",
	"reinstall":                "Operator reinstall failed",
	"deploy-dashboard-pr":      "Dashboard PR deploy failed",
	"deploy-dashboard-main":    "Dashboard main deploy failed",
	"revert-dashboard":         "Dashboard revert failed",
	"setup-minio":              "MinIO setup failed",
	"teardown-minio":           "MinIO teardown failed",
	"setup-pipeline-server":    "Pipeline server setup failed",
	"teardown-pipeline-server": "Pipeline server teardown failed",
	"setup-mlflow":             "MLflow setup failed",
	"teardown-mlflow":          "MLflow teardown failed",
	"deploy-mlflow-pr":         "MLflow PR deploy failed",
	"revert-mlflow":            "MLflow revert failed",
	"create-pull-secret":       "Pull secret update failed",
	"create-dsc":               "DataScienceCluster creation failed",
	"repair-dsc":               "DataScienceCluster repair failed",
}

func activityCategory(action string) string {
	if a, ok := activityActions[action]; ok {
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

// describeActivity fills the read-time fields of an entry.
func describeActivity(e *types.ActivityEntry) {
	e.Category = activityCategory(e.Action)
	if l, ok := activityFailedLabels[e.Action]; ok && !e.Success {
		e.Label = l
	} else if a, ok := activityActions[e.Action]; ok {
		e.Label = a[1]
	} else {
		e.Label = strings.ReplaceAll(e.Action, "-", " ")
	}
	if e.Category == "operator" {
		e.Build = activityBuild(e.Detail)
	}
}

// trimActivity keeps the newest entries of each category within its
// retention and the whole log within maxActivityBytes, preserving order.
func trimActivity(entries []types.ActivityEntry) []types.ActivityEntry {
	counts := map[string]int{}
	keep := make([]bool, len(entries))
	for i := len(entries) - 1; i >= 0; i-- {
		cat := activityCategory(entries[i].Action)
		limit, ok := activityRetention[cat]
		if !ok {
			limit = maxActivityEntries
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
		if err != nil || len(data) <= maxActivityBytes {
			break
		}
		out = out[1:]
	}
	return out
}

func getActivityNamespace() string {
	if ns := os.Getenv("NAMESPACE"); ns != "" {
		return ns
	}
	if data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		return strings.TrimSpace(string(data))
	}
	return "rhoai-nightly-updater"
}

// RecordActivity appends an activity entry to the ConfigMap-backed log.
// It uses optimistic concurrency (resourceVersion) to prevent lost updates
// when multiple replicas attempt concurrent writes. On 409 Conflict the
// read-modify-write cycle is retried up to maxConflictRetries times.
// It never returns an error to callers — failures are logged as warnings.
func RecordActivity(c *Client, entry types.ActivityEntry) {
	// Not cancelled with the request (for example an SSE connection drop),
	// but bounded: activityWriteTimeout for the whole call, retries
	// included, and never past the operation's shared post-deadline end
	// (see postOperationContext), so a stalled API server cannot hold a
	// shutting-down pod past its drain.
	ctx, cancel := postOperationContext(c, activityWriteTimeout)
	defer cancel()
	recordActivityWithClient(c.WithContext(ctx), entry)
}

func recordActivityWithClient(c *Client, entry types.ActivityEntry) {
	ns := getActivityNamespace()
	// Read-time fields are derived, not stored.
	entry.Label, entry.Category, entry.Build = "", "", ""
	if len(entry.Detail) > maxActivityDetail {
		entry.Detail = entry.Detail[:maxActivityDetail] + "…"
	}
	cmPath := namespacedPath("v1", "configmaps", ns, activityConfigMapName)

	for attempt := 0; attempt < maxConflictRetries; attempt++ {
		if attempt > 0 {
			slog.Info("retrying activity record after conflict", "attempt", attempt+1)
		}

		var entries []types.ActivityEntry
		var resourceVersion string
		isNew := false

		// Step 1: GET the ConfigMap with its resourceVersion
		body, _, err := c.get(cmPath)
		if err != nil {
			if !IsK8sError(err, 404) {
				slog.Warn("failed to get activity configmap", "error", err)
				return
			}
			// ConfigMap does not exist yet — we will create it
			isNew = true
			entries = []types.ActivityEntry{}
		} else {
			var cm map[string]interface{}
			if err := json.Unmarshal(body, &cm); err != nil {
				slog.Warn("failed to parse activity configmap", "error", err)
				return
			}
			// Extract resourceVersion for optimistic concurrency
			if metadata, ok := cm["metadata"].(map[string]interface{}); ok {
				if rv, ok := metadata["resourceVersion"].(string); ok {
					resourceVersion = rv
				}
			}
			data, _ := cm["data"].(map[string]interface{})
			if raw, ok := data["entries"].(string); ok && raw != "" {
				if err := json.Unmarshal([]byte(raw), &entries); err != nil {
					slog.Warn("failed to parse activity entries", "error", err)
					entries = []types.ActivityEntry{}
				}
			}
		}

		// Step 2: Modify the entries; keep the newest per category
		entries = trimActivity(append(entries, entry))

		entriesJSON, err := json.Marshal(entries)
		if err != nil {
			slog.Warn("failed to marshal activity entries", "error", err)
			return
		}

		// Step 3: PUT/Create with resourceVersion for optimistic locking
		metadata := map[string]interface{}{
			"name":      activityConfigMapName,
			"namespace": ns,
		}
		if resourceVersion != "" {
			metadata["resourceVersion"] = resourceVersion
		}

		cm := map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   metadata,
			"data": map[string]interface{}{
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
			cmListPath := namespacedPath("v1", "configmaps", ns, "")
			_, _, err = c.post(cmListPath, cmData)
		} else {
			// Update with PUT for existing ConfigMaps (carries resourceVersion)
			_, _, err = c.put(cmPath, cmData)
		}

		if err != nil {
			// Step 4: If 409 Conflict, retry from step 1
			if IsK8sError(err, 409) {
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
func GetActivity(c *Client) ([]types.ActivityEntry, error) {
	ns := getActivityNamespace()
	cmPath := namespacedPath("v1", "configmaps", ns, activityConfigMapName)

	body, _, err := c.get(cmPath)
	if err != nil {
		if IsK8sError(err, 404) {
			return []types.ActivityEntry{}, nil
		}
		return nil, fmt.Errorf("get activity configmap: %w", err)
	}

	var cm map[string]interface{}
	if err := json.Unmarshal(body, &cm); err != nil {
		return nil, fmt.Errorf("parse activity configmap: %w", err)
	}

	data, _ := cm["data"].(map[string]interface{})
	raw, ok := data["entries"].(string)
	if !ok || raw == "" {
		return []types.ActivityEntry{}, nil
	}

	var entries []types.ActivityEntry
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return nil, fmt.Errorf("parse activity entries: %w", err)
	}
	for i := range entries {
		describeActivity(&entries[i])
	}

	return entries, nil
}
