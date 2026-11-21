package cluster

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
)

// The module CRs are in two API groups. RHOAI 3.6 serves most modules (and
// Kueue, which the RHOAI operator still reconciles in-tree) in
// components.platform.opendatahub.io, but the databaseservice module's CR in
// services.platform.opendatahub.io: rhods-operator rhoai-3.6
// pkg/cluster/gvk/gvk.go gives DatabaseService (and Monitoring) the group of
// api/services/v1alpha1/groupversion_info.go, and
// prefetched-charts/databaseservice/templates/rbac.authorization.k8s.io_v1_clusterrole.yaml
// grants its operator databaseservices and databaseservices/finalizers in
// that group. The Platform's module list (config.opendatahub.io Platform
// spec.modules, api/config/v1alpha1/platform_types.go) has one field per
// module whatever group its CR is in, so the kinds are discovered in both
// groups and no caller assumes one. The services group also serves kinds
// that are not modules (Auth, GatewayConfig), which the RHOAI operator
// reconciles itself; callers treat them like any kind whose operator they
// cannot identify (no ServiceAccount of the applications namespace may
// update them), which refuses (fail closed).
const (
	componentAPIGroup = "components.platform.opendatahub.io"
	servicesAPIGroup  = "services.platform.opendatahub.io"
)

// moduleAPIGroups are the groups whose kinds are module CRs (above).
var moduleAPIGroups = []string{componentAPIGroup, servicesAPIGroup}

// isModuleAPIGroup reports whether group is one of moduleAPIGroups.
func isModuleAPIGroup(group string) bool { return slices.Contains(moduleAPIGroups, group) }

// isModuleAPIVersion reports whether an apiVersion ("<group>/<version>") is
// in a module API group.
func isModuleAPIVersion(apiVersion string) bool {
	group, _, _ := strings.Cut(apiVersion, "/")
	return isModuleAPIGroup(group)
}

// componentKind is one kind served by a module API group.
type componentKind struct {
	Group    string // e.g. "components.platform.opendatahub.io"
	Version  string // the group's preferred version
	Resource string // plural, e.g. "kserves"
	Kind     string // e.g. "Kserve"
	Verbs    []string
	// Namespaced: the kind is namespaced (module kinds are cluster-scoped).
	Namespaced bool
}

// listPath is the cluster-wide collection of the kind.
func (k componentKind) listPath() string { return componentListPath(k.Group, k.Version, k.Resource) }

// crd is the name of the kind's CustomResourceDefinition.
func (k componentKind) crd() string { return k.Resource + "." + k.Group }

// module is the module name of the kind: its lower-case kind, as used by
// platform.opendatahub.io/part-of and by the tool's module names.
func (k componentKind) module() string { return strings.ToLower(k.Kind) }

// componentAPI is the served module API groups and their kinds, discovered
// so the list follows whatever operator version installed the CRDs (3.6
// added aihubs, aipipelines and mcplifecycleoperators, and databaseservices
// in the services group). Groups maps each served group to its preferred
// version; a group that is not served (fresh cluster, RHOAI 2.x) is absent.
type componentAPI struct {
	Groups map[string]string
	Kinds  []componentKind
}

// served reports whether any module API group is served.
func (a componentAPI) served() bool { return len(a.Groups) > 0 }

// componentListPath is the cluster-wide collection of one module resource.
func componentListPath(group, version, resource string) string {
	return "/apis/" + group + "/" + version + "/" + resource
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

// errModuleKindUnknown: no served module kind matches the module name (an
// unmapped part-of value, or the module API is not served at all, e.g. while
// its CRDs are reinstalled).
var errModuleKindUnknown = errors.New("no served " + strings.Join(moduleAPIGroups, " or ") + " kind matches it")

// moduleCRPresent reports whether any CR of the module (lower-case kind)
// exists. A module whose kind is not served returns errModuleKindUnknown:
// "no CR" cannot be told apart from "unknown producer" then, so callers
// must not treat it as removed. Discovery and list errors are returned.
func moduleCRPresent(c *Client, discover func() (componentAPI, error), module string) (bool, error) {
	api, err := discover()
	if err != nil {
		return false, err
	}
	if _, ok := api.kindForModule(module); !ok {
		return false, fmt.Errorf("module %q: %w", module, errModuleKindUnknown)
	}
	items, err := listModuleCRs(c, discover, module)
	return len(items) > 0, err
}

// moduleCRItem is the metadata of a module CR.
type moduleCRItem struct {
	Metadata struct {
		Name              string     `json:"name"`
		UID               string     `json:"uid"`
		Finalizers        []string   `json:"finalizers"`
		DeletionTimestamp string     `json:"deletionTimestamp"`
		OwnerReferences   []ownerRef `json:"ownerReferences"`
	} `json:"metadata"`
}

// listModuleCRs lists the CRs of one module (lower-case kind).
func listModuleCRs(c *Client, discover func() (componentAPI, error), module string) ([]moduleCRItem, error) {
	api, err := discover()
	if err != nil {
		return nil, err
	}
	return listModuleCRsIn(c, api, module)
}

// listModuleCRsIn is listModuleCRs with the API already discovered.
func listModuleCRsIn(c *Client, api componentAPI, module string) ([]moduleCRItem, error) {
	k, ok := api.kindForModule(module)
	if !ok {
		return nil, nil
	}
	return listKindCRs(c, k)
}

// listKindCRs lists the CRs of one module kind.
func listKindCRs(c *Client, k componentKind) ([]moduleCRItem, error) {
	var items []moduleCRItem
	if err := c.listInto(k.listPath(), &items); err != nil {
		if isDecodeError(err) {
			return nil, fmt.Errorf("parse %s: %w", k.crd(), err)
		}
		return nil, fmt.Errorf("list %s: %w", k.crd(), err)
	}
	return items, nil
}

// kindForModule maps a module name (the lower-case kind, e.g. kserve, aihub,
// databaseservice, as used by platform.opendatahub.io/part-of) to its kind.
// A name that matches a kind in both groups is ambiguous and matches none:
// callers then refuse as for an unknown kind.
func (a componentAPI) kindForModule(module string) (componentKind, bool) {
	var found []componentKind
	for _, k := range a.Kinds {
		if k.Kind != "" && k.module() == module {
			found = append(found, k)
		}
	}
	if len(found) != 1 {
		return componentKind{}, false
	}
	return found[0], true
}

// kindOf returns the served kind named kind in group.
func (a componentAPI) kindOf(group, kind string) (componentKind, bool) {
	for _, k := range a.Kinds {
		if k.Group == group && k.Kind == kind {
			return k, true
		}
	}
	return componentKind{}, false
}

// discoverComponentAPI reads the API discovery documents of the module
// groups (readable by every authenticated user through system:discovery). A
// group that is not served is left out, with no error; any other read error
// is returned.
func discoverComponentAPI(c *Client) (componentAPI, error) {
	api := componentAPI{Groups: map[string]string{}}
	for _, group := range moduleAPIGroups {
		body, found, err := c.getOptional("/apis/" + group)
		if err != nil {
			return componentAPI{}, fmt.Errorf("discover %s: %w", group, err)
		}
		if !found {
			continue
		}
		var doc struct {
			PreferredVersion struct {
				GroupVersion string `json:"groupVersion"`
				Version      string `json:"version"`
			} `json:"preferredVersion"`
		}
		if err := json.Unmarshal(body, &doc); err != nil {
			return componentAPI{}, fmt.Errorf("parse %s discovery: %w", group, err)
		}
		version := doc.PreferredVersion.Version
		if version == "" {
			_, version, _ = strings.Cut(doc.PreferredVersion.GroupVersion, "/")
		}
		if version == "" {
			return componentAPI{}, fmt.Errorf("%s discovery lists no preferred version", group)
		}
		body, _, err = c.get("/apis/" + group + "/" + version)
		if err != nil {
			return componentAPI{}, fmt.Errorf("discover %s/%s resources: %w", group, version, err)
		}
		var list struct {
			Resources []struct {
				Name       string   `json:"name"`
				Kind       string   `json:"kind"`
				Verbs      []string `json:"verbs"`
				Namespaced bool     `json:"namespaced"`
			} `json:"resources"`
		}
		if err := json.Unmarshal(body, &list); err != nil {
			return componentAPI{}, fmt.Errorf("parse %s/%s resources: %w", group, version, err)
		}
		api.Groups[group] = version
		for _, r := range list.Resources {
			if strings.Contains(r.Name, "/") {
				continue // subresource such as kserves/status
			}
			api.Kinds = append(api.Kinds, componentKind{Group: group, Version: version, Resource: r.Name, Kind: r.Kind, Verbs: r.Verbs, Namespaced: r.Namespaced})
		}
	}
	return api, nil
}

// StuckComponentCRAge is how long a component CR must have been deleting
// before its finalizers may be removed.
var StuckComponentCRAge = 10 * time.Minute

// cleanupStuckComponentCRs removes the finalizers of a module CR (in either
// module API group) only when nothing else can: the CR has been deleting for
// StuckComponentCRAge and the module operator that owns its finalizer is
// gone (finalizerOwner: its ServiceAccount is known from RBAC, and no
// Deployment and no running Pod runs as it; module operators are owned by
// the Platform, not by the CSV, so they keep running while rhods-operator is
// reinstalled). Stripping a finalizer whose operator still exists would skip
// that operator's cleanup and orphan its operands, so every other case (an
// unknown operator, a read error) is left alone and logged. Even with the
// operator gone, the finalizer stood for cleanup that only its operator
// does: what the CR owns is garbage-collected once it is gone, but anything
// that operator would have removed that the CR does not own (objects in
// other namespaces, cluster-scoped objects without an owner reference to
// the CR) stays behind. This is the documented pattern for unblocking a
// stuck deletion:
// https://kubernetes.io/docs/concepts/overview/working-with-objects/finalizers/
// It returns the number of CRs unblocked and the problems or skips to log.
func cleanupStuckComponentCRs(c *Client) (int, []string) {
	api, err := discoverComponentAPI(c)
	if err != nil {
		return 0, []string{err.Error()}
	}
	operators := newModuleOperatorFinder(c, func() (componentAPI, error) { return api, nil })
	unstuck := 0
	var warnings []string
	for _, kind := range api.Kinds {
		verbs := strings.Join(kind.Verbs, ",") + ","
		if !strings.Contains(verbs, "list,") || !strings.Contains(verbs, "patch,") {
			continue
		}
		resource := kind.Resource
		listPath := kind.listPath()
		var items []struct {
			Metadata struct {
				Name              string   `json:"name"`
				UID               string   `json:"uid"`
				ResourceVersion   string   `json:"resourceVersion"`
				Finalizers        []string `json:"finalizers"`
				DeletionTimestamp *string  `json:"deletionTimestamp"`
			} `json:"metadata"`
		}
		if err := c.listInto(listPath, &items); err != nil {
			if isDecodeError(err) {
				warnings = append(warnings, fmt.Sprintf("parse %s: %v", resource, err))
				continue
			}
			warnings = append(warnings, fmt.Sprintf("list %s: %v", resource, err))
			continue
		}
		for _, item := range items {
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
			owner, err := operators.finalizerOwner(kind.module(), m.Name)
			switch {
			case err != nil:
				warnings = append(warnings, fmt.Sprintf("cannot identify the operator that owns the finalizers of %s: %v; left untouched", id, err))
				continue
			case owner.Unknown != "":
				warnings = append(warnings, fmt.Sprintf("%s has been deleting since %s, but the operator that owns its finalizers %v is unknown (%s); left untouched", id, *m.DeletionTimestamp, m.Finalizers, owner.Unknown))
				continue
			case !owner.gone():
				warnings = append(warnings, fmt.Sprintf("%s has been deleting since %s, but its operator %s still exists and will finish the deletion; left untouched", id, *m.DeletionTimestamp, owner.stillThere()))
				continue
			}
			if m.UID == "" || m.ResourceVersion == "" {
				warnings = append(warnings, fmt.Sprintf("%s has no uid or resourceVersion to guard the change; left untouched", id))
				continue
			}
			// Guarded by the inspected object: the API server rejects a merge
			// patch whose metadata.resourceVersion differs from the stored
			// one with 409 Conflict, so a CR that was deleted and recreated
			// under the same name, or whose finalizers changed since they
			// were inspected, is left alone. A recreated CR always has a new
			// resourceVersion (and uid, which is immutable).
			if err := c.patchJSON(listPath+"/"+m.Name, map[string]any{"metadata": map[string]any{
				"uid": m.UID, "resourceVersion": m.ResourceVersion, "finalizers": []string{},
			}}); err != nil {
				switch {
				case isK8sConflict(err), isK8sNotFound(err), IsK8sError(err, 422):
					warnings = append(warnings, fmt.Sprintf("%s changed or was replaced after it was checked; its finalizers were left untouched", id))
				default:
					warnings = append(warnings, fmt.Sprintf("remove finalizers from %s: %v", id, err))
				}
				continue
			}
			slog.Info("removed stuck finalizer from module CR whose operator is gone",
				"resource", resource, "name", m.Name, "finalizers", m.Finalizers,
				"operatorServiceAccounts", owner.ServiceAccounts, "namespace", owner.Namespace, "grant", owner.Grant)
			unstuck++
		}
	}
	return unstuck, warnings
}
