package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const maxConflictRetries = 3

const (
	activityConfigMapName = "rhoai-nightly-updater-activity"
	maxActivityEntries    = 20
)

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

		// Step 2: Modify the entries
		entries = append(entries, entry)

		// Trim to max entries (keep newest)
		if len(entries) > maxActivityEntries {
			entries = entries[len(entries)-maxActivityEntries:]
		}

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

	return entries, nil
}
