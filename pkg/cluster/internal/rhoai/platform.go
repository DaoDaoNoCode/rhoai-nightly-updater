package rhoai

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// K8s resource names and namespaces used throughout RHOAI operations.
const (
	CatalogName = "rhoai-catalog-dev"
	CatalogNS   = "openshift-marketplace"
	SubName     = "rhods-operator"
	SubNS       = "redhat-ods-operator"
	IDMSSource  = "registry.redhat.io/rhoai"
)

// DSCState values reported by GetComponents.
const (
	DSCStatePresent = "present"
	DSCStateNoDSC   = "no-dsc" // CRD installed, no DataScienceCluster yet
	DSCStateNoCRD   = "no-crd" // operator not installed, or its CRDs not created yet
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
	ComponentAPIGroup = "components.platform.opendatahub.io"
	ServicesAPIGroup  = "services.platform.opendatahub.io"
)

func ReadSubscription(c *kube.Client) (SubscriptionDetails, error) {
	path := NamespacedPath("operators.coreos.com/v1alpha1", "subscriptions", SubNS, SubName)
	var sub struct {
		Metadata kube.ObjectMeta `json:"metadata"`
		Spec     struct {
			Source  string `json:"source"`
			Channel string `json:"channel"`
		} `json:"spec"`
		Status *struct {
			State        *string `json:"state"`
			InstalledCSV string  `json:"installedCSV"`
		} `json:"status"`
	}
	exists, err := c.GetIntoOptional(path, &sub)
	if err != nil {
		if kube.IsDecodeError(err) {
			return SubscriptionDetails{}, fmt.Errorf("unmarshal: %w", err)
		}
		return SubscriptionDetails{}, fmt.Errorf("request failed: %w", err)
	}
	if !exists {
		return SubscriptionDetails{Info: types.SubscriptionInfo{State: "Not Installed", Namespace: SubNS}}, nil
	}
	state := "Unknown"
	installed := ""
	if sub.Status != nil {
		if sub.Status.State != nil {
			state = *sub.Status.State
		}
		installed = sub.Status.InstalledCSV
	}
	return SubscriptionDetails{
		Info: types.SubscriptionInfo{
			Namespace: SubNS,
			Name:      SubName,
			Source:    sub.Spec.Source,
			Channel:   sub.Spec.Channel,
			State:     state,
		},
		InstalledCSV: installed,
		Found:        true,
		Meta:         sub.Metadata,
	}, nil
}

// SubscriptionDetails is the RHOAI Subscription as read by status checks.
// info.ManagedBy is not set: gitOpsManagers sets it from meta.
type SubscriptionDetails struct {
	Info         types.SubscriptionInfo
	InstalledCSV string
	Found        bool
	Meta         kube.ObjectMeta
}

func GetSubscription(c *kube.Client) (types.SubscriptionInfo, error) {
	sub, err := ReadSubscription(c)
	return sub.Info, err
}

// csvSummary is the part of a ClusterServiceVersion the status checks read.
type csvSummary struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Version     string `json:"version"`
		DisplayName string `json:"displayName"`
	} `json:"spec"`
	Status struct {
		Phase *string `json:"phase"`
	} `json:"status"`
}

func GetCSV(c *kube.Client) (types.CSVInfo, error) {
	// Prefer the Subscription's installed CSV when multiple versions coexist
	// during OLM reconciliation. List order is not installation identity.
	sub, err := ReadSubscription(c)
	if err != nil {
		return types.CSVInfo{}, err
	}
	return CSVForInstalledName(c, sub.InstalledCSV)
}

// CSVForInstalledName returns the RHOAI CSV. When the Subscription names its
// installed CSV, that CSV is read by name (~225 KB instead of a ~475 KB list on
// a cluster with copied CSVs). The list is only read when there is no name or
// the named CSV is missing, so a stale Subscription reference is still
// detected below.
func CSVForInstalledName(c *kube.Client, installedName string) (types.CSVInfo, error) {
	if installedName != "" {
		body, _, err := c.Get(NamespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", SubNS, installedName))
		if err == nil {
			var csv csvSummary
			if err := json.Unmarshal(body, &csv); err != nil {
				return types.CSVInfo{}, fmt.Errorf("unmarshal: %w", err)
			}
			// Lifecycle cleanup acts on this identity, so only trust an
			// object that really is the named CSV; otherwise use the list.
			if csv.Metadata.Name == installedName {
				return csv.info(), nil
			}
		} else if !kube.IsK8sNotFound(err) {
			return types.CSVInfo{}, fmt.Errorf("request failed: %w", err)
		}
	}
	// OLM labels the CSVs it copies into this namespace from operators
	// installed elsewhere with olm.copiedFrom; they are never the RHOAI CSV.
	path := NamespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", SubNS, "") +
		"?labelSelector=" + url.QueryEscape("!olm.copiedFrom")
	var items []csvSummary
	if err := c.ListInto(path, &items); err != nil {
		if kube.IsDecodeError(err) {
			return types.CSVInfo{}, fmt.Errorf("unmarshal: %w", err)
		}
		return types.CSVInfo{}, fmt.Errorf("request failed: %w", err)
	}

	// Primary: match by displayName (most specific)
	// Fallback: match by name prefix "rhods-operator." (resilient to branding changes)
	var fallback *types.CSVInfo
	for _, item := range items {
		info := item.info()
		name, displayName := item.Metadata.Name, item.Spec.DisplayName
		if installedName != "" {
			if name == installedName {
				return info, nil
			}
			if fallback == nil && (displayName == "Red Hat OpenShift AI" || strings.HasPrefix(name, "rhods-operator.")) {
				fallback = &info
			}
			continue
		}
		if displayName == "Red Hat OpenShift AI" {
			return info, nil
		}
		if fallback == nil && strings.HasPrefix(name, "rhods-operator.") {
			fallback = &info
		}
	}
	if fallback != nil {
		if installedName != "" {
			// A stale Subscription reference is not proof that the operator is
			// absent. Stop lifecycle cleanup rather than guessing which CSV to delete.
			return types.CSVInfo{}, fmt.Errorf("subscription references missing CSV %q, but RHOAI CSV %q still exists; wait for OLM reconciliation or resolve the stale Subscription reference before retrying", installedName, fallback.Name)
		}
		return *fallback, nil
	}
	return types.CSVInfo{Phase: "Not Found"}, nil
}

// GetCatalogSource reads the nightly CatalogSource. Who manages it
// (ManagedBy, ManagedByACM) is not set: gitOpsManagers sets it from the
// metadata readCatalogSource returns.
func GetCatalogSource(c *kube.Client) (types.CatalogSourceInfo, error) {
	cs, _, err := ReadCatalogSource(c)
	return cs, err
}

func ReadCatalogSource(c *kube.Client) (types.CatalogSourceInfo, *kube.ObjectMeta, error) {
	path := NamespacedPath("operators.coreos.com/v1alpha1", "catalogsources", CatalogNS, CatalogName)
	var result struct {
		Metadata kube.ObjectMeta `json:"metadata"`
		Spec     struct {
			Image string `json:"image"`
		} `json:"spec"`
		Status struct {
			ConnectionState struct {
				LastObservedState *string `json:"lastObservedState"`
			} `json:"connectionState"`
		} `json:"status"`
	}
	found, err := c.GetIntoOptional(path, &result)
	if err != nil {
		if kube.IsDecodeError(err) {
			return types.CatalogSourceInfo{Exists: false, Name: CatalogName}, nil, fmt.Errorf("unmarshal: %w", err)
		}
		return types.CatalogSourceInfo{Exists: false, Name: CatalogName}, nil, fmt.Errorf("request failed: %w", err)
	}
	if !found {
		// expected when the CatalogSource does not exist yet
		return types.CatalogSourceInfo{Exists: false, Name: CatalogName}, nil, nil
	}

	state := "Unknown"
	if s := result.Status.ConnectionState.LastObservedState; s != nil {
		state = *s
	}

	return types.CatalogSourceInfo{
		Exists: true,
		Name:   CatalogName,
		Image:  result.Spec.Image,
		State:  state,
	}, &result.Metadata, nil
}

// DSCRead is the first DataScienceCluster as read.
type DSCRead struct {
	Object map[string]any // nil without a DSC
	State  string         // DSCStatePresent, DSCStateNoDSC or DSCStateNoCRD
	// Version is the API version it was read at ("v3"); Skipped are the
	// newer served versions that failed with a conversion webhook error.
	Version string
	Skipped []versionFailure
}

// ReadFirstDSC returns the first DataScienceCluster, read at the newest
// served version that works (servedVersions; v2 then v1 when discovery is
// unavailable), and the DSC state.
func ReadFirstDSC(c *kube.Client) (DSCRead, error) {
	r, err := ListServed(c, DSCGroup, DSCListFmt, nil, DSCFallbackVersions)
	if err != nil {
		return DSCRead{}, fmt.Errorf("fetching DSC list: %w", err)
	}
	if r.Version == "" {
		return DSCRead{State: DSCStateNoCRD}, nil
	}
	var list struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(r.Body, &list); err != nil {
		return DSCRead{}, fmt.Errorf("parsing DSC list: %w", err)
	}
	out := DSCRead{State: DSCStateNoDSC, Version: r.Version, Skipped: r.Skipped}
	if len(list.Items) > 0 {
		out.Object, out.State = list.Items[0], DSCStatePresent
	}
	return out, nil
}

// DataScienceClusterPath returns the API path of the cluster's
// DataScienceCluster (the same one the Components page shows), at the
// version it is read at there (readFirstDSC).
func DataScienceClusterPath(c *kube.Client) (string, error) {
	read, err := ReadFirstDSC(c)
	if err != nil {
		return "", err
	}
	if read.State == DSCStateNoCRD {
		return "", fmt.Errorf("the DataScienceCluster API is not installed")
	}
	meta, _ := read.Object["metadata"].(map[string]any)
	name, _ := meta["name"].(string)
	if name == "" {
		return "", fmt.Errorf("no DataScienceCluster exists")
	}
	return fmt.Sprintf(DSCListFmt, read.Version) + "/" + name, nil
}

// ComponentKind is one kind served by a module API group.
type ComponentKind struct {
	Group    string // e.g. "components.platform.opendatahub.io"
	Version  string // the group's preferred version
	Resource string // plural, e.g. "kserves"
	Kind     string // e.g. "Kserve"
	Verbs    []string
	// Namespaced: the kind is namespaced (module kinds are cluster-scoped).
	Namespaced bool
}

// ComponentAPI is the served module API groups and their kinds, discovered
// so the list follows whatever operator version installed the CRDs (3.6
// added aihubs, aipipelines and mcplifecycleoperators, and databaseservices
// in the services group). Groups maps each served group to its preferred
// version; a group that is not served (fresh cluster, RHOAI 2.x) is absent.
type ComponentAPI struct {
	Groups map[string]string
	Kinds  []ComponentKind
}

// ComponentListPath is the cluster-wide collection of one module resource.
func ComponentListPath(group, version, resource string) string {
	return "/apis/" + group + "/" + version + "/" + resource
}

// ModuleAPIGroups are the groups whose kinds are module CRs (above).
var ModuleAPIGroups = []string{ComponentAPIGroup, ServicesAPIGroup}

// DiscoverComponentAPI reads the API discovery documents of the module
// groups (readable by every authenticated user through system:discovery). A
// group that is not served is left out, with no error; any other read error
// is returned.
func DiscoverComponentAPI(c *kube.Client) (ComponentAPI, error) {
	api := ComponentAPI{Groups: map[string]string{}}
	for _, group := range ModuleAPIGroups {
		body, found, err := c.GetOptional("/apis/" + group)
		if err != nil {
			return ComponentAPI{}, fmt.Errorf("discover %s: %w", group, err)
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
			return ComponentAPI{}, fmt.Errorf("parse %s discovery: %w", group, err)
		}
		version := doc.PreferredVersion.Version
		if version == "" {
			_, version, _ = strings.Cut(doc.PreferredVersion.GroupVersion, "/")
		}
		if version == "" {
			return ComponentAPI{}, fmt.Errorf("%s discovery lists no preferred version", group)
		}
		body, _, err = c.Get("/apis/" + group + "/" + version)
		if err != nil {
			return ComponentAPI{}, fmt.Errorf("discover %s/%s resources: %w", group, version, err)
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
			return ComponentAPI{}, fmt.Errorf("parse %s/%s resources: %w", group, version, err)
		}
		api.Groups[group] = version
		for _, r := range list.Resources {
			if strings.Contains(r.Name, "/") {
				continue // subresource such as kserves/status
			}
			api.Kinds = append(api.Kinds, ComponentKind{Group: group, Version: version, Resource: r.Name, Kind: r.Kind, Verbs: r.Verbs, Namespaced: r.Namespaced})
		}
	}
	return api, nil
}

// ListPath is the cluster-wide collection of the kind.
func (k ComponentKind) ListPath() string { return ComponentListPath(k.Group, k.Version, k.Resource) }

// CRD is the name of the kind's CustomResourceDefinition.
func (k ComponentKind) CRD() string { return k.Resource + "." + k.Group }

// Module is the module name of the kind: its lower-case kind, as used by
// platform.opendatahub.io/part-of and by the tool's module names.
func (k ComponentKind) Module() string { return strings.ToLower(k.Kind) }

// Served reports whether any module API group is served.
func (a ComponentAPI) Served() bool { return len(a.Groups) > 0 }

// KindForModule maps a module name (the lower-case kind, e.g. kserve, aihub,
// databaseservice, as used by platform.opendatahub.io/part-of) to its kind.
// A name that matches a kind in both groups is ambiguous and matches none:
// callers then refuse as for an unknown kind.
func (a ComponentAPI) KindForModule(module string) (ComponentKind, bool) {
	var found []ComponentKind
	for _, k := range a.Kinds {
		if k.Kind != "" && k.Module() == module {
			found = append(found, k)
		}
	}
	if len(found) != 1 {
		return ComponentKind{}, false
	}
	return found[0], true
}

// KindOf returns the served kind named kind in group.
func (a ComponentAPI) KindOf(group, kind string) (ComponentKind, bool) {
	for _, k := range a.Kinds {
		if k.Group == group && k.Kind == kind {
			return k, true
		}
	}
	return ComponentKind{}, false
}

// VersionFallback reports a DSC read at an older version than the
// preferred one, or nil.
func (r DSCRead) VersionFallback() *types.DSCVersionFallback {
	if len(r.Skipped) == 0 || r.Version == "" {
		return nil
	}
	return &types.DSCVersionFallback{Version: r.Skipped[0].Version, Used: r.Version, Message: r.Skipped[0].Message}
}

func (s csvSummary) info() types.CSVInfo {
	phase := "Unknown"
	if s.Status.Phase != nil {
		phase = *s.Status.Phase
	}
	return types.CSVInfo{Name: s.Metadata.Name, Version: s.Spec.Version, Phase: phase}
}
