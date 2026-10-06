package cluster

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

const componentAPIGroup = "components.platform.opendatahub.io"

// componentKind is one kind served by components.platform.opendatahub.io.
type componentKind struct {
	Resource string // plural, e.g. "kserves"
	Kind     string // e.g. "Kserve"
	Verbs    []string
}

// componentAPI is the served version of components.platform.opendatahub.io
// and its kinds, discovered so the list follows whatever operator version
// installed the CRDs (3.6 added aihubs, aipipelines and
// mcplifecycleoperators). Version is "" when the group is not served
// (fresh cluster, or RHOAI 2.x).
type componentAPI struct {
	Version string
	Kinds   []componentKind
}

// componentListPath is the cluster-wide collection of one component
// resource at the discovered version.
func componentListPath(version, resource string) string {
	return "/apis/" + componentAPIGroup + "/" + version + "/" + resource
}

// cachedComponentAPI returns a function that discovers the component API on
// first use and then returns the same result.
func cachedComponentAPI(c *Client) func() (componentAPI, error) {
	var once sync.Once
	var api componentAPI
	var err error
	return func() (componentAPI, error) {
		once.Do(func() { api, err = discoverComponentAPI(c) })
		return api, err
	}
}

// errModuleKindUnknown: no served components.platform.opendatahub.io kind
// matches the module name (an unmapped part-of value, or the component API
// is not served at all, e.g. while its CRDs are reinstalled).
var errModuleKindUnknown = errors.New("no served " + componentAPIGroup + " kind matches it")

// moduleCRPresent reports whether any CR of the module (lower-case kind)
// exists. A module whose kind is not served returns errModuleKindUnknown:
// "no CR" cannot be told apart from "unknown producer" then, so callers
// must not treat it as removed. Discovery and list errors are returned.
func moduleCRPresent(c *Client, discover func() (componentAPI, error), module string) (bool, error) {
	api, err := discover()
	if err != nil {
		return false, err
	}
	if _, ok := api.resourceForModule(module); !ok {
		return false, fmt.Errorf("module %q: %w", module, errModuleKindUnknown)
	}
	items, err := listModuleCRs(c, discover, module)
	return len(items) > 0, err
}

// moduleCRItem is the metadata of a module CR.
type moduleCRItem struct {
	Metadata struct {
		Name              string   `json:"name"`
		Finalizers        []string `json:"finalizers"`
		DeletionTimestamp string   `json:"deletionTimestamp"`
	} `json:"metadata"`
}

// listModuleCRs lists the CRs of one module (lower-case kind).
func listModuleCRs(c *Client, discover func() (componentAPI, error), module string) ([]moduleCRItem, error) {
	api, err := discover()
	if err != nil {
		return nil, err
	}
	resource, ok := api.resourceForModule(module)
	if !ok {
		return nil, nil
	}
	body, _, err := c.get(componentListPath(api.Version, resource))
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", resource, err)
	}
	var list struct {
		Items []moduleCRItem `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("parse %s: %w", resource, err)
	}
	return list.Items, nil
}

// resourceForModule maps a module name (the lower-case kind, e.g. kserve,
// aihub, ogx, as used by platform.opendatahub.io/part-of) to its resource.
func (a componentAPI) resourceForModule(module string) (string, bool) {
	for _, k := range a.Kinds {
		if strings.ToLower(k.Kind) == module {
			return k.Resource, true
		}
	}
	return "", false
}

// discoverComponentAPI reads the API discovery documents of the component
// group (readable by every authenticated user through system:discovery). A
// group that is not served yields an empty componentAPI and no error.
func discoverComponentAPI(c *Client) (componentAPI, error) {
	body, _, err := c.get("/apis/" + componentAPIGroup)
	if IsK8sError(err, 404) {
		return componentAPI{}, nil
	}
	if err != nil {
		return componentAPI{}, fmt.Errorf("discover %s: %w", componentAPIGroup, err)
	}
	var group struct {
		PreferredVersion struct {
			GroupVersion string `json:"groupVersion"`
			Version      string `json:"version"`
		} `json:"preferredVersion"`
	}
	if err := json.Unmarshal(body, &group); err != nil {
		return componentAPI{}, fmt.Errorf("parse %s discovery: %w", componentAPIGroup, err)
	}
	version := group.PreferredVersion.Version
	if version == "" {
		_, version, _ = strings.Cut(group.PreferredVersion.GroupVersion, "/")
	}
	if version == "" {
		return componentAPI{}, fmt.Errorf("%s discovery lists no preferred version", componentAPIGroup)
	}
	body, _, err = c.get("/apis/" + componentAPIGroup + "/" + version)
	if err != nil {
		return componentAPI{}, fmt.Errorf("discover %s/%s resources: %w", componentAPIGroup, version, err)
	}
	var list struct {
		Resources []struct {
			Name  string   `json:"name"`
			Kind  string   `json:"kind"`
			Verbs []string `json:"verbs"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return componentAPI{}, fmt.Errorf("parse %s/%s resources: %w", componentAPIGroup, version, err)
	}
	api := componentAPI{Version: version}
	for _, r := range list.Resources {
		if strings.Contains(r.Name, "/") {
			continue // subresource such as kserves/status
		}
		api.Kinds = append(api.Kinds, componentKind{Resource: r.Name, Kind: r.Kind, Verbs: r.Verbs})
	}
	return api, nil
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
	api, err := discoverComponentAPI(c)
	if err != nil {
		return 0, []string{err.Error()}
	}
	unstuck := 0
	var warnings []string
	for _, kind := range api.Kinds {
		verbs := strings.Join(kind.Verbs, ",") + ","
		if !strings.Contains(verbs, "list,") || !strings.Contains(verbs, "patch,") {
			continue
		}
		resource := kind.Resource
		listPath := componentListPath(api.Version, resource)
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
