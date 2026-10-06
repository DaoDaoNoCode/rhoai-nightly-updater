package cluster

import (
	"context"
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
// the operator history. At most 50+30+20+20+20 = 140 entries are kept.
var activityRetention = map[string]int{
	"operator":      50,
	"dashboard-dev": 30,
	"setup":         20,
	"diagnostics":   20,
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
	"setup-minio":                   {"dashboard-dev", "MinIO set up"},
	"teardown-minio":                {"dashboard-dev", "MinIO torn down"},
	"setup-pipeline-server":         {"dashboard-dev", "Pipeline server set up"},
	"teardown-pipeline-server":      {"dashboard-dev", "Pipeline server torn down"},
	"setup-mlflow":                  {"dashboard-dev", "MLflow set up"},
	"teardown-mlflow":               {"dashboard-dev", "MLflow torn down"},
	"deploy-mlflow-pr":              {"dashboard-dev", "MLflow PR deployed"},
	"revert-mlflow":                 {"dashboard-dev", "MLflow reverted"},
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
	if a, ok := activityActions[e.Action]; ok {
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
	// Use a background context so activity recording succeeds even after
	// the request context is canceled (e.g., SSE connection drop).
	bgClient := c.WithContext(context.Background())
	recordActivityWithClient(bgClient, entry)
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

// SaveOperationMarker records the running operation in a ConfigMap, or
// clears the record when marker is nil.
func SaveOperationMarker(c *Client, marker *types.OperationMarker) error {
	value := ""
	if marker != nil {
		data, err := json.Marshal(marker)
		if err != nil {
			return err
		}
		value = string(data)
	}
	ns := getActivityNamespace()
	cm := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]interface{}{"name": operationConfigMapName, "namespace": ns},
		"data":       map[string]interface{}{"operation": value},
	}
	if _, _, err := c.apply(namespacedPath("v1", "configmaps", ns, operationConfigMapName), cm); err != nil {
		return fmt.Errorf("save operation marker: %w", err)
	}
	return nil
}

// lastCompletedKey holds the most recent finished operation in the
// operation ConfigMap, next to the running-operation marker.
const lastCompletedKey = "lastCompleted"

// GetOperationState returns the running-operation marker and the last
// completed operation; either is nil when none is recorded. An unreadable
// lastCompleted value is ignored rather than hiding the marker.
func GetOperationState(c *Client) (*types.OperationMarker, *types.CompletedOperation, error) {
	ns := getActivityNamespace()
	body, _, err := c.get(namespacedPath("v1", "configmaps", ns, operationConfigMapName))
	if IsK8sError(err, 404) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("get operation marker: %w", err)
	}
	var cm struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(body, &cm); err != nil {
		return nil, nil, fmt.Errorf("parse operation marker: %w", err)
	}
	var last *types.CompletedOperation
	if raw := cm.Data[lastCompletedKey]; raw != "" {
		var done types.CompletedOperation
		if err := json.Unmarshal([]byte(raw), &done); err != nil {
			slog.Warn("ignoring an unreadable last completed operation", "error", err)
		} else {
			last = &done
		}
	}
	raw := cm.Data["operation"]
	if raw == "" {
		return nil, last, nil
	}
	var marker types.OperationMarker
	if err := json.Unmarshal([]byte(raw), &marker); err != nil {
		return nil, last, fmt.Errorf("parse operation marker: %w", err)
	}
	return &marker, last, nil
}

// SaveCompletedOperation clears the running-operation marker and records
// done as the last completed operation in a single write, so the two never
// disagree. It is a JSON merge patch of these two keys only. The marker is
// written by server-side apply, whose field manager never owns
// lastCompleted, so the next marker write keeps it (an applier only removes
// fields it owned: https://kubernetes.io/docs/reference/using-api/server-side-apply/#field-management).
func SaveCompletedOperation(c *Client, done *types.CompletedOperation) error {
	value, err := json.Marshal(done)
	if err != nil {
		return err
	}
	ns := getActivityNamespace()
	data := map[string]string{"operation": "", lastCompletedKey: string(value)}
	patch, err := json.Marshal(map[string]interface{}{"data": data})
	if err != nil {
		return err
	}
	_, _, err = c.patch(namespacedPath("v1", "configmaps", ns, operationConfigMapName), patch)
	if IsK8sError(err, 404) {
		cm, mErr := json.Marshal(map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata":   map[string]interface{}{"name": operationConfigMapName, "namespace": ns},
			"data":       data,
		})
		if mErr != nil {
			return mErr
		}
		_, _, err = c.post(namespacedPath("v1", "configmaps", ns, ""), cm)
	}
	if err != nil {
		return fmt.Errorf("save completed operation: %w", err)
	}
	return nil
}
