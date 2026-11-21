package components

import (
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
)

// IsModuleAPIGroup reports whether group is one of moduleAPIGroups.
func IsModuleAPIGroup(group string) bool { return slices.Contains(rhoai.ModuleAPIGroups, group) }

// IsModuleAPIVersion reports whether an apiVersion ("<group>/<version>") is
// in a module API group.
func IsModuleAPIVersion(apiVersion string) bool {
	group, _, _ := strings.Cut(apiVersion, "/")
	return IsModuleAPIGroup(group)
}

// CachedComponentAPI returns a function that discovers the component API on
// first use and then returns the same result.
func CachedComponentAPI(c *kube.Client) func() (rhoai.ComponentAPI, error) {
	var once sync.Once
	var api rhoai.ComponentAPI
	var err error
	return func() (rhoai.ComponentAPI, error) {
		once.Do(func() { api, err = rhoai.DiscoverComponentAPI(c) })
		return api, err
	}
}

// errModuleKindUnknown: no served module kind matches the module name (an
// unmapped part-of value, or the module API is not served at all, e.g. while
// its CRDs are reinstalled).
var errModuleKindUnknown = errors.New("no served " + strings.Join(rhoai.ModuleAPIGroups, " or ") + " kind matches it")

// moduleCRPresent reports whether any CR of the module (lower-case kind)
// exists. A module whose kind is not served returns errModuleKindUnknown:
// "no CR" cannot be told apart from "unknown producer" then, so callers
// must not treat it as removed. Discovery and list errors are returned.
func moduleCRPresent(c *kube.Client, discover func() (rhoai.ComponentAPI, error), module string) (bool, error) {
	api, err := discover()
	if err != nil {
		return false, err
	}
	if _, ok := api.KindForModule(module); !ok {
		return false, fmt.Errorf("module %q: %w", module, errModuleKindUnknown)
	}
	items, err := ListModuleCRs(c, discover, module)
	return len(items) > 0, err
}

// ModuleCRItem is the metadata of a module CR.
type ModuleCRItem struct {
	Metadata struct {
		Name              string     `json:"name"`
		UID               string     `json:"uid"`
		Finalizers        []string   `json:"finalizers"`
		DeletionTimestamp string     `json:"deletionTimestamp"`
		OwnerReferences   []OwnerRef `json:"ownerReferences"`
	} `json:"metadata"`
}

// ListModuleCRs lists the CRs of one module (lower-case kind).
func ListModuleCRs(c *kube.Client, discover func() (rhoai.ComponentAPI, error), module string) ([]ModuleCRItem, error) {
	api, err := discover()
	if err != nil {
		return nil, err
	}
	return listModuleCRsIn(c, api, module)
}

// listModuleCRsIn is ListModuleCRs with the API already discovered.
func listModuleCRsIn(c *kube.Client, api rhoai.ComponentAPI, module string) ([]ModuleCRItem, error) {
	k, ok := api.KindForModule(module)
	if !ok {
		return nil, nil
	}
	return ListKindCRs(c, k)
}

// ListKindCRs lists the CRs of one module kind.
func ListKindCRs(c *kube.Client, k rhoai.ComponentKind) ([]ModuleCRItem, error) {
	var items []ModuleCRItem
	if err := c.ListInto(k.ListPath(), &items); err != nil {
		if kube.IsDecodeError(err) {
			return nil, fmt.Errorf("parse %s: %w", k.CRD(), err)
		}
		return nil, fmt.Errorf("list %s: %w", k.CRD(), err)
	}
	return items, nil
}

// StuckCRAge is how long a component CR must have been deleting
// before its finalizers may be removed.
var StuckCRAge = 10 * time.Minute

// CleanupStuckCRs removes the finalizers of a module CR (in either
// module API group) only when nothing else can: the CR has been deleting for
// StuckCRAge and the module operator that owns its finalizer is
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
func CleanupStuckCRs(c *kube.Client) (int, []string) {
	api, err := rhoai.DiscoverComponentAPI(c)
	if err != nil {
		return 0, []string{err.Error()}
	}
	operators := NewModuleOperatorFinder(c, func() (rhoai.ComponentAPI, error) { return api, nil })
	unstuck := 0
	var warnings []string
	for _, kind := range api.Kinds {
		verbs := strings.Join(kind.Verbs, ",") + ","
		if !strings.Contains(verbs, "list,") || !strings.Contains(verbs, "patch,") {
			continue
		}
		resource := kind.Resource
		listPath := kind.ListPath()
		var items []struct {
			Metadata struct {
				Name              string   `json:"name"`
				UID               string   `json:"uid"`
				ResourceVersion   string   `json:"resourceVersion"`
				Finalizers        []string `json:"finalizers"`
				DeletionTimestamp *string  `json:"deletionTimestamp"`
			} `json:"metadata"`
		}
		if err := c.ListInto(listPath, &items); err != nil {
			if kube.IsDecodeError(err) {
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
			if err != nil || time.Since(since) < StuckCRAge {
				warnings = append(warnings, fmt.Sprintf("%s is being deleted; its finalizers were left for its operator", id))
				continue
			}
			owner, err := operators.FinalizerOwner(kind.Module(), m.Name)
			switch {
			case err != nil:
				warnings = append(warnings, fmt.Sprintf("cannot identify the operator that owns the finalizers of %s: %v; left untouched", id, err))
				continue
			case owner.Unknown != "":
				warnings = append(warnings, fmt.Sprintf("%s has been deleting since %s, but the operator that owns its finalizers %v is unknown (%s); left untouched", id, *m.DeletionTimestamp, m.Finalizers, owner.Unknown))
				continue
			case !owner.Gone():
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
			if err := c.PatchJSON(listPath+"/"+m.Name, map[string]any{"metadata": map[string]any{
				"uid": m.UID, "resourceVersion": m.ResourceVersion, "finalizers": []string{},
			}}); err != nil {
				switch {
				case kube.IsK8sConflict(err), kube.IsK8sNotFound(err), kube.IsK8sError(err, 422):
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
