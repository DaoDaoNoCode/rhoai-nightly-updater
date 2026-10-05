package cluster

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

const componentAPIGroup = "components.platform.opendatahub.io"

// componentResources discovers the component CR kinds the cluster serves,
// so the list follows whatever operator version installed the CRDs (3.6
// added aihubs, aipipelines and mcplifecycleoperators). It returns the
// group/version path and the plural resource names. A missing API group
// (fresh cluster, or RHOAI 2.x) yields no resources and no error.
func componentResources(c *Client) (string, []string, error) {
	body, _, err := c.get("/apis/" + componentAPIGroup)
	if IsK8sError(err, 404) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, fmt.Errorf("discover %s: %w", componentAPIGroup, err)
	}
	var group struct {
		PreferredVersion struct {
			GroupVersion string `json:"groupVersion"`
		} `json:"preferredVersion"`
	}
	if err := json.Unmarshal(body, &group); err != nil {
		return "", nil, fmt.Errorf("parse %s discovery: %w", componentAPIGroup, err)
	}
	gv := group.PreferredVersion.GroupVersion
	if gv == "" {
		return "", nil, fmt.Errorf("%s discovery lists no preferred version", componentAPIGroup)
	}
	body, _, err = c.get("/apis/" + gv)
	if err != nil {
		return "", nil, fmt.Errorf("discover %s resources: %w", gv, err)
	}
	var list struct {
		Resources []struct {
			Name  string   `json:"name"`
			Verbs []string `json:"verbs"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return "", nil, fmt.Errorf("parse %s resources: %w", gv, err)
	}
	var names []string
	for _, r := range list.Resources {
		if strings.Contains(r.Name, "/") {
			continue // subresource such as kserves/status
		}
		verbs := strings.Join(r.Verbs, ",") + ","
		if strings.Contains(verbs, "list,") && strings.Contains(verbs, "patch,") {
			names = append(names, r.Name)
		}
	}
	return "/apis/" + gv, names, nil
}

// StuckComponentCRAge is how long a component CR must have been deleting
// before its finalizers may be removed.
var StuckComponentCRAge = 10 * time.Minute

// componentFinalizerOwners maps each component CR kind to the module operator
// Deployment in redhat-ods-applications that removes its finalizer (RHOAI
// 3.6: these Deployments are owned by the Platform CR, not by the CSV, so
// they keep running while rhods-operator is reinstalled). Kinds that are not
// listed (kueues is handled inside rhods-operator itself; mcplifecycleoperators
// and datasciencepipelines have no verified owner) are never unblocked.
var componentFinalizerOwners = map[string]string{
	"dashboards":      "dashboard-operator",
	"workbenches":     "workbenches-operator",
	"aipipelines":     "data-science-pipelines-operator-controller-manager",
	"kserves":         "kserve-module-controller-manager",
	"rays":            "ray-module-operator-controller-manager",
	"trainers":        "trainer-operator-controller-manager",
	"trustyais":       "trustyai-operator-module-controller-manager",
	"feastoperators":  "opendatahub-feast-operator",
	"ogxs":            "opendatahub-ogx-operator",
	"aihubs":          "aihub-controller-manager",
	"mlflowoperators": "mlflow-operator-controller-manager",
}

// cleanupStuckComponentCRs removes the finalizers of a component CR only when
// nothing else can: the CR has been deleting for StuckComponentCRAge and the
// module operator that owns its finalizer does not exist. Stripping a
// finalizer whose operator is still running would skip that operator's
// cleanup and orphan its operands, so every other case is left alone and
// logged. This is the documented pattern for unblocking a stuck deletion:
// https://kubernetes.io/docs/concepts/overview/working-with-objects/finalizers/
// It returns the number of CRs unblocked and the problems or skips to log.
func cleanupStuckComponentCRs(c *Client) (int, []string) {
	base, resources, err := componentResources(c)
	if err != nil {
		return 0, []string{err.Error()}
	}
	unstuck := 0
	var warnings []string
	for _, resource := range resources {
		listPath := base + "/" + resource
		body, _, err := c.get(listPath)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("list %s: %v", resource, err))
			continue
		}
		var list struct {
			Items []struct {
				Metadata struct {
					Name              string   `json:"name"`
					Finalizers        []string `json:"finalizers"`
					DeletionTimestamp *string  `json:"deletionTimestamp"`
				} `json:"metadata"`
			} `json:"items"`
		}
		if err := json.Unmarshal(body, &list); err != nil {
			warnings = append(warnings, fmt.Sprintf("parse %s: %v", resource, err))
			continue
		}
		for _, item := range list.Items {
			m := item.Metadata
			if m.DeletionTimestamp == nil || len(m.Finalizers) == 0 {
				continue
			}
			id := resource + "/" + m.Name
			since, err := time.Parse(time.RFC3339, *m.DeletionTimestamp)
			if err != nil || time.Since(since) < StuckComponentCRAge {
				warnings = append(warnings, fmt.Sprintf("%s is being deleted; its finalizers were left for its operator", id))
				continue
			}
			owner, known := componentFinalizerOwners[resource]
			if !known {
				warnings = append(warnings, fmt.Sprintf("%s has been deleting since %s, but the operator that owns its finalizers %v is unknown; left untouched", id, *m.DeletionTimestamp, m.Finalizers))
				continue
			}
			_, _, depErr := c.get(namespacedPath("apps/v1", "deployments", "redhat-ods-applications", owner))
			if depErr == nil {
				warnings = append(warnings, fmt.Sprintf("%s has been deleting since %s, but its operator %s still exists and will finish the deletion; left untouched", id, *m.DeletionTimestamp, owner))
				continue
			}
			if !IsK8sError(depErr, 404) {
				warnings = append(warnings, fmt.Sprintf("cannot check operator %s for %s: %v; left untouched", owner, id, depErr))
				continue
			}
			if _, _, err := c.patch(listPath+"/"+m.Name, []byte(`{"metadata":{"finalizers":[]}}`)); err != nil {
				warnings = append(warnings, fmt.Sprintf("remove finalizers from %s: %v", id, err))
				continue
			}
			slog.Info("removed stuck finalizer from component CR whose operator is gone",
				"resource", resource, "name", m.Name, "finalizers", m.Finalizers, "operator", owner)
			unstuck++
		}
	}
	return unstuck, warnings
}
