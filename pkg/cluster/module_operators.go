package cluster

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
)

// Which Deployment removes a module CR's finalizers, found at run time.
//
// The module operators change between releases (3.6 added aigateway and
// mcplifecycleoperator), and nothing on their Deployments names the module:
// the platform labels them only platform.opendatahub.io/part-of=platform.
// What does name it is their RBAC. Each module operator's ClusterRole
// (rhods-operator rhoai-3.6 prefetched-manifests/<module>/rbac/role.yaml and
// the cluster roles of prefetched-charts) grants its ServiceAccount update
// on its module CR and on <plural>/finalizers, the permission a controller
// needs to own objects through the CR (blockOwnerDeletion), which
// kubebuilder scaffolds for the controller of a kind; the admin, editor and
// viewer roles the modules ship grant neither. So the operator of a module
// is the Deployment that:
//
//   - runs in the applications namespace (the DSCInitialization's), as a
//     ServiceAccount a ClusterRoleBinding lets update or patch the CR and
//     <plural>/finalizers in components.platform.opendatahub.io
//     (<plural>/status when no ServiceAccount there holds that), and
//   - is owned by the Platform or the DataScienceCluster: the platform
//     applies the module operators with that owner (rhods-operator
//     internal/controller/modules README, "Module CR ownership and
//     cleanup"); operands are owned by their module CR, and the RHOAI
//     operator's own Deployment by its CSV.
//
// Module CRs are cluster-scoped, so only ClusterRoleBindings can grant these
// permissions (a RoleBinding grants within its namespace only). The rule
// must name the kind's resource: a resource wildcard ("*", "*/finalizers",
// as in cluster-admin and other broad roles) or a binding to a group of
// ServiceAccounts would make any Deployment look like the operator, so it
// identifies nothing. A "*" verb or API group counts, but a more specific
// grant wins: the operator is the one Deployment whose grant needs the
// fewest wildcards. The RHOAI operator holds these permissions for every
// module too (internal/controller/modules kubebuilder_rbac.go), but its
// Deployment is neither in the applications namespace nor owned by the
// Platform.
//
// A kind no ServiceAccount of the applications namespace may update is the
// RHOAI operator's own when the installed CSV owns its CRD (3.6: Kueue, the
// one component the operator still reconciles in-tree; its CRD ships in the
// operator bundle, the module CRDs ship with their modules). Anything else
// (no such Deployment, several equally specific ones, a ServiceAccount
// with the permissions but no Deployment running as it) is unknown, and
// callers refuse (fail closed).

// moduleOperator is the Deployment that removes a module CR's finalizers.
type moduleOperator struct {
	Namespace, Name string
	// RHOAI: the RHOAI operator itself (the installed CSV owns the CRD).
	RHOAI bool
	// Via says how it was identified.
	Via string
}

func (o moduleOperator) ref() string { return o.Namespace + "/" + o.Name }

// clusterRoleRule is one PolicyRule of a ClusterRole.
type clusterRoleRule struct {
	APIGroups     []string `json:"apiGroups"`
	Resources     []string `json:"resources"`
	ResourceNames []string `json:"resourceNames"`
	Verbs         []string `json:"verbs"`
}

type rbacBinding struct {
	RoleRef struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
	} `json:"roleRef"`
	Subjects []struct {
		Kind      string `json:"kind"`
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"subjects"`
}

// moduleOperatorDeployment is the part of a Deployment the finder reads.
type moduleOperatorDeployment struct {
	Metadata struct {
		Name            string `json:"name"`
		OwnerReferences []struct {
			APIVersion string `json:"apiVersion"`
			Kind       string `json:"kind"`
			Name       string `json:"name"`
		} `json:"ownerReferences"`
	} `json:"metadata"`
	Spec struct {
		Template struct {
			Spec struct {
				ServiceAccountName string `json:"serviceAccountName"`
				ServiceAccount     string `json:"serviceAccount"` // deprecated alias
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
}

func (d moduleOperatorDeployment) serviceAccount() string {
	return firstNonEmpty(d.Spec.Template.Spec.ServiceAccountName, d.Spec.Template.Spec.ServiceAccount, "default")
}

// platformOwner names the Platform or DataScienceCluster that owns the
// Deployment ("" when neither does).
func (d moduleOperatorDeployment) platformOwner() string {
	for _, o := range d.Metadata.OwnerReferences {
		group, _, _ := strings.Cut(o.APIVersion, "/")
		if (o.Kind == "Platform" && group == "config.opendatahub.io") || (o.Kind == "DataScienceCluster" && group == "datasciencecluster.opendatahub.io") {
			return o.Kind + " " + o.Name
		}
	}
	return ""
}

// moduleOperatorFinder finds module operators. It reads the
// DSCInitialization, the ClusterRoles and ClusterRoleBindings, the
// applications namespace's Deployments and (only when needed) the installed
// CSV once, so a scan or a change reads them once however many modules it
// checks.
type moduleOperatorFinder struct {
	c   *Client
	api func() (componentAPI, error)

	mu          sync.Mutex
	loaded      bool
	loadErr     error
	appNS       string
	roles       map[string][]clusterRoleRule
	bindings    []rbacBinding
	deployments []moduleOperatorDeployment
	csvLoaded   bool
	csv         *installedOperator
	csvErr      error
}

func newModuleOperatorFinder(c *Client, api func() (componentAPI, error)) *moduleOperatorFinder {
	if api == nil {
		api = cachedComponentAPI(c)
	}
	return &moduleOperatorFinder{c: c, api: api}
}

func (f *moduleOperatorFinder) load() error {
	if !f.loaded {
		f.loaded = true
		f.loadErr = f.read()
	}
	return f.loadErr
}

func (f *moduleOperatorFinder) read() error {
	dscis, _, err := listFirstServed(f.c, dsciGroup, "/apis/dscinitialization.opendatahub.io/%s/dscinitializations", "dscinitializations", dsciFallbackVersions)
	switch {
	case err != nil:
		return fmt.Errorf("read the DSCInitialization for the applications namespace: %w", err)
	case len(dscis) == 0 || dscis[0].Spec.ApplicationsNamespace == "":
		return fmt.Errorf("no DSCInitialization names the applications namespace")
	}
	f.appNS = dscis[0].Spec.ApplicationsNamespace

	body, _, err := f.c.get(clusterPath("rbac.authorization.k8s.io/v1", "clusterroles", ""))
	if err != nil {
		return fmt.Errorf("list ClusterRoles: %w", err)
	}
	var roles struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Rules []clusterRoleRule `json:"rules"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &roles); err != nil {
		return fmt.Errorf("parse ClusterRoles: %w", err)
	}
	f.roles = map[string][]clusterRoleRule{}
	for _, r := range roles.Items {
		f.roles[r.Metadata.Name] = r.Rules
	}

	body, _, err = f.c.get(clusterPath("rbac.authorization.k8s.io/v1", "clusterrolebindings", ""))
	if err != nil {
		return fmt.Errorf("list ClusterRoleBindings: %w", err)
	}
	var bindings struct {
		Items []rbacBinding `json:"items"`
	}
	if err := json.Unmarshal(body, &bindings); err != nil {
		return fmt.Errorf("parse ClusterRoleBindings: %w", err)
	}
	f.bindings = bindings.Items

	body, _, err = f.c.get(namespacedPath("apps/v1", "deployments", f.appNS, ""))
	if err != nil {
		return fmt.Errorf("list Deployments in %s: %w", f.appNS, err)
	}
	var deployments struct {
		Items []moduleOperatorDeployment `json:"items"`
	}
	if err := json.Unmarshal(body, &deployments); err != nil {
		return fmt.Errorf("parse Deployments in %s: %w", f.appNS, err)
	}
	f.deployments = deployments.Items
	return nil
}

func (f *moduleOperatorFinder) installedCSV() (*installedOperator, error) {
	if !f.csvLoaded {
		f.csvLoaded = true
		f.csv, f.csvErr = getInstalledOperator(f.c)
	}
	return f.csv, f.csvErr
}

// find returns the operator that removes the finalizers of the module's CR
// named crName ("" when the name is not known; rules limited to names then
// count). op is nil when it is unknown, and why says why. err is a read
// error: callers refuse as for an unknown operator.
func (f *moduleOperatorFinder) find(module, crName string) (op *moduleOperator, why string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	api, err := f.api()
	if err != nil {
		return nil, "", err
	}
	var kind *componentKind
	for i := range api.Kinds {
		if strings.ToLower(api.Kinds[i].Kind) == module {
			kind = &api.Kinds[i]
		}
	}
	switch {
	case kind == nil:
		return nil, fmt.Sprintf("no %s kind is named after %s", componentAPIGroup, module), nil
	case kind.Namespaced:
		// RoleBindings could grant a namespaced kind; they are not read.
		return nil, fmt.Sprintf("%s is namespaced; the tool identifies the operators of cluster-scoped module kinds only", kind.Kind), nil
	}
	if err := f.load(); err != nil {
		return nil, "", err
	}
	plural := kind.Resource
	for _, sub := range []string{"finalizers", "status"} {
		scores := f.serviceAccountScores(plural, sub, crName)
		if len(scores) == 0 {
			continue
		}
		var best []moduleOperatorDeployment
		bestScore := -1
		for _, d := range f.deployments {
			if d.platformOwner() == "" {
				continue
			}
			score, ok := scores[d.serviceAccount()]
			switch {
			case !ok:
			case bestScore < 0 || score < bestScore:
				best, bestScore = []moduleOperatorDeployment{d}, score
			case score == bestScore:
				best = append(best, d)
			}
		}
		switch len(best) {
		case 0:
			holders := slices.Sorted(maps.Keys(scores))
			return nil, fmt.Sprintf("in %s, %s may update %s and %s/%s, but no Deployment owned by the Platform runs as %s",
				f.appNS, strings.Join(holders, ", "), plural, plural, sub, verb(len(holders), "it", "them")), nil
		case 1:
			d := best[0]
			return &moduleOperator{Namespace: f.appNS, Name: d.Metadata.Name,
				Via: fmt.Sprintf("owned by %s; its ServiceAccount %s may update %s and %s/%s", d.platformOwner(), d.serviceAccount(), plural, plural, sub)}, "", nil
		default:
			var names []string
			for _, d := range best {
				names = append(names, d.Metadata.Name)
			}
			sort.Strings(names)
			return nil, fmt.Sprintf("several Deployments in %s may update %s/%s: %s", f.appNS, plural, sub, strings.Join(names, ", ")), nil
		}
	}
	csv, err := f.installedCSV()
	if err != nil {
		return nil, "", fmt.Errorf("read the installed RHOAI CSV: %w", err)
	}
	crd := plural + "." + componentAPIGroup
	if !slices.Contains(csv.OwnedCRDs, crd) {
		return nil, fmt.Sprintf("no ServiceAccount in %s may update %s and %s/finalizers, and the installed CSV %s does not own CRD %s", f.appNS, plural, plural, csv.Name, crd), nil
	}
	if len(csv.Deployments) != 1 {
		return nil, fmt.Sprintf("the installed CSV %s owns CRD %s but installs %d Deployments", csv.Name, crd, len(csv.Deployments)), nil
	}
	return &moduleOperator{Namespace: SubNS, Name: csv.Deployments[0], RHOAI: true,
		Via: fmt.Sprintf("the RHOAI operator: the installed CSV %s owns CRD %s", csv.Name, crd)}, "", nil
}

// serviceAccountScores returns, for each ServiceAccount of the applications
// namespace that a ClusterRoleBinding names and lets update or patch both
// the module CR and its subresource sub, the fewest wildcards its grant
// needs (bestRuleScore). Group subjects are not followed: a binding to
// every ServiceAccount (system:serviceaccounts, system:authenticated)
// would make any Deployment look like the operator.
func (f *moduleOperatorFinder) serviceAccountScores(plural, sub, crName string) map[string]int {
	out := map[string]int{}
	for _, b := range f.bindings {
		if b.RoleRef.Kind != "ClusterRole" {
			continue
		}
		rules := f.roles[b.RoleRef.Name]
		mainScore, okMain := bestRuleScore(rules, plural, "", crName)
		subScore, okSub := bestRuleScore(rules, plural, sub, crName)
		if !okMain || !okSub {
			continue
		}
		for _, s := range b.Subjects {
			if s.Kind != "ServiceAccount" || s.Namespace != f.appNS {
				continue
			}
			if cur, seen := out[s.Name]; !seen || mainScore+subScore < cur {
				out[s.Name] = mainScore + subScore
			}
		}
	}
	return out
}

// bestRuleScore is the fewest wildcards among the rules that allow update
// or patch on plural (or its subresource sub) in the module API group, as
// the API server matches rules (k8s pkg/apis/rbac/v1 evaluation_helpers.go;
// resourceNames limit the names). ok is false when no rule allows it. A
// "*" verb or API group counts as one wildcard each. The rule must name the
// resource: "*" and "*/<sub>" (cluster-admin and other broad roles) say
// nothing about which kind a ServiceAccount reconciles, so they identify
// no operator.
func bestRuleScore(rules []clusterRoleRule, plural, sub, crName string) (best int, ok bool) {
	resource := plural
	if sub != "" {
		resource += "/" + sub
	}
	for _, r := range rules {
		score := 0
		if !slices.Contains(r.Resources, resource) {
			continue
		}
		switch {
		case slices.Contains(r.APIGroups, componentAPIGroup):
		case slices.Contains(r.APIGroups, "*"):
			score++
		default:
			continue
		}
		switch {
		case slices.Contains(r.Verbs, "update"), slices.Contains(r.Verbs, "patch"):
		case slices.Contains(r.Verbs, "*"):
			score++
		default:
			continue
		}
		if len(r.ResourceNames) > 0 && crName != "" && !slices.Contains(r.ResourceNames, crName) {
			continue
		}
		if !ok || score < best {
			best, ok = score, true
		}
	}
	return best, ok
}
