package cluster

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
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

// cleanupStuckComponentCRs removes the finalizers of component CRs that are
// already being deleted (deletionTimestamp set). It runs during Reinstall
// after the old CSV is gone, when no controller is left to run those
// finalizers; CRs that are not being deleted are never touched. This is the
// documented pattern for unblocking a stuck deletion:
// https://kubernetes.io/docs/concepts/overview/working-with-objects/finalizers/
// It returns the number of CRs unblocked and any problems found.
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
			if item.Metadata.DeletionTimestamp == nil || len(item.Metadata.Finalizers) == 0 {
				continue
			}
			if _, _, err := c.patch(listPath+"/"+item.Metadata.Name, []byte(`{"metadata":{"finalizers":[]}}`)); err != nil {
				warnings = append(warnings, fmt.Sprintf("remove finalizers from %s/%s: %v", resource, item.Metadata.Name, err))
				continue
			}
			slog.Info("removed stuck finalizer from component CR",
				"resource", resource, "name", item.Metadata.Name, "finalizers", item.Metadata.Finalizers)
			unstuck++
		}
	}
	return unstuck, warnings
}
