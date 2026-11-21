package envkit

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/argo"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/components"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
)

// Operators this tool installs from OperatorHub for an Environment feature
// (MaaS, Models), described as data (OperatorSpec) and installed the way the
// product docs install them: a Namespace, an OperatorGroup when the
// namespace has none, and a Subscription with installPlanApproval
// Automatic. What is installed already is found by its package, wherever it
// is (OperatorState), and used as it is.

var (
	// OperatorTimeout bounds the wait for OLM to install or upgrade an
	// operator (the docs wait 300 s for AtLatestKnown, then for the
	// operator's Deployment).
	OperatorTimeout = 10 * time.Minute
	// OperatorResolveGrace is how long a Subscription may stay at an
	// older CSV after its channel changed before OLM is taken to have no
	// path there (AtLatestKnown: seen live 2026-11-18, RHCL 1.4.3 switched
	// straight to stable-1.5).
	OperatorResolveGrace = 2 * time.Minute
	PollInterval         = 5 * time.Second
)

// OperatorSpec is an operator this tool installs and how.
type OperatorSpec struct {
	// ID names the operator in step and part IDs ("cert-manager").
	ID    string
	Title string
	Pkg   string
	// SubName is the Subscription this tool creates; SubNames the names an
	// existing install's Subscription is found by in its CSV's namespace
	// (and the only ones this tool changes).
	SubName   string
	SubNames  []string
	Namespace string
	Source    string
	// Group is the OperatorGroup created when the namespace has none; ""
	// when the namespace must have one already (openshift-operators).
	Group string
	// OwnNamespace: the group created targets its own namespace only
	// (install mode OwnNamespace); else it has no spec (AllNamespaces).
	OwnNamespace bool
	// API is the API group the operator serves, and controller the name of
	// its controller Deployment: the API served without a CSV of the
	// package counts as installed only while that Deployment exists
	// (ControllerDeployed: CRDs stay after an uninstall).
	API, Controller string
	// Channel picks the channel to install from the catalog's package (a
	// refusal when none fits); nil: the package's default channel.
	Channel func(OperatorPackage) (string, string)
	// Env is the Subscription's spec.config.env ("" none).
	Env []map[string]any
}

// operatorSubscriptionPath is the path of an operator Subscription this
// tool reads by name or changes; only these names, written out so the RBAC
// rules (resourceNames) can be checked against them (rbac_coverage_test.go).
// Any other Subscription is read from the namespace's list.
func operatorSubscriptionPath(ns, name string) (string, bool) {
	switch name {
	case "kuadrant-operator":
		return rhoai.NamespacedPath("operators.coreos.com/v1alpha1", "subscriptions", ns, "kuadrant-operator"), true
	case "rhcl-operator":
		return rhoai.NamespacedPath("operators.coreos.com/v1alpha1", "subscriptions", ns, "rhcl-operator"), true
	case "cloudnative-pg":
		return rhoai.NamespacedPath("operators.coreos.com/v1alpha1", "subscriptions", ns, "cloudnative-pg"), true
	}
	return "", false
}

// OLMSubscription is a Subscription as read.
type OLMSubscription struct {
	Metadata kube.ObjectMeta `json:"metadata"`
	Spec     struct {
		Channel             string         `json:"channel"`
		Name                string         `json:"name"`
		Source              string         `json:"source"`
		SourceNamespace     string         `json:"sourceNamespace"`
		InstallPlanApproval string         `json:"installPlanApproval"`
		Config              map[string]any `json:"config"`
	} `json:"spec"`
	Status struct {
		State        string           `json:"state"`
		InstalledCSV string           `json:"installedCSV"`
		CurrentCSV   string           `json:"currentCSV"`
		LastUpdated  string           `json:"lastUpdated"`
		Conditions   []kube.Condition `json:"conditions"`
	} `json:"status"`
	// ArgoApp is the Argo CD application managing it (argoTracking.managedBy),
	// set by ReadOperatorSubscription.
	ArgoApp string `json:"-"`
}

func parseSubscription(raw []byte) (*OLMSubscription, error) {
	var sub OLMSubscription
	if err := json.Unmarshal(raw, &sub); err != nil {
		return nil, err
	}
	return &sub, nil
}

// withArgoManager sets sub.ArgoApp: the Argo CD application that manages
// it, as Argo CD answers (argocd.go).
func withArgoManager(c *kube.Client, sub *OLMSubscription) *OLMSubscription {
	if sub != nil {
		sub.ArgoApp = argo.NewCheck(c).Object(sub.Metadata.Annotations, sub.Metadata.Labels,
			argo.Object{Group: argo.SubscriptionObject.Group, Kind: "Subscription", Namespace: sub.Metadata.Namespace, Name: sub.Metadata.Name}).ManagedBy()
	}
	return sub
}

// ReadOperatorSubscription reads Subscription ns/name: by name when it is
// one this tool may change, else from the namespace's list (a missing one
// is nil).
func ReadOperatorSubscription(c *kube.Client, ns, name string) (*OLMSubscription, error) {
	path, ok := operatorSubscriptionPath(ns, name)
	if !ok {
		subs, err := ListSubscriptions(c, ns)
		if err != nil {
			return nil, fmt.Errorf("read Subscription %s/%s: %w", ns, name, err)
		}
		for _, s := range subs {
			if s.Metadata.Name == name {
				return withArgoManager(c, s), nil
			}
		}
		return nil, nil
	}
	body, found, err := c.GetOptional(path)
	if err != nil {
		return nil, fmt.Errorf("read Subscription %s/%s: %w", ns, name, err)
	}
	if !found {
		return nil, nil
	}
	sub, err := parseSubscription(body)
	if err != nil {
		return nil, fmt.Errorf("parse Subscription %s/%s: %w", ns, name, err)
	}
	return withArgoManager(c, sub), nil
}

// ListSubscriptions lists the Subscriptions of namespace ns, or of every
// namespace when ns is "".
func ListSubscriptions(c *kube.Client, ns string) ([]*OLMSubscription, error) {
	path := rhoai.ClusterPath("operators.coreos.com/v1alpha1", "subscriptions", "")
	if ns != "" {
		path = rhoai.NamespacedPath("operators.coreos.com/v1alpha1", "subscriptions", ns, "")
	}
	var list struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := c.GetInto(path, &list); err != nil {
		if kube.IsDecodeError(err) {
			return nil, fmt.Errorf("parse the Subscription list: %w", err)
		}
		return nil, fmt.Errorf("list Subscriptions: %w", err)
	}
	out := make([]*OLMSubscription, 0, len(list.Items))
	for _, raw := range list.Items {
		sub, err := parseSubscription(raw)
		if err != nil {
			return nil, fmt.Errorf("parse the Subscription list: %w", err)
		}
		out = append(out, sub)
	}
	return out, nil
}

// OperatorState is an operator as installed: its CSV (the newest when OLM
// is replacing one), the Subscription found by the documented names (sub,
// the one this tool may change), and any Subscription of its package in any
// namespace (requested): RHOAI's KServe counts an operator as there when a
// Subscription of its package exists anywhere (odh-platform-utilities
// pkg/cluster/olm OperatorPackageRequested), and a second one elsewhere
// would install the operator twice.
type OperatorState struct {
	CSV, CSVNS, Version, Phase string
	Deployments                []string
	Sub                        *OLMSubscription
	Requested                  *OLMSubscription
}

func (s OperatorState) Installed() bool { return s.CSV != "" }

// Pending is the Subscription OLM is installing from, when there is no CSV
// yet (nil otherwise).
func (s OperatorState) Pending() *OLMSubscription {
	if s.Installed() {
		return nil
	}
	if s.Sub != nil {
		return s.Sub
	}
	return s.Requested
}

// CSVVersion is the version in a CSV name ("rhcl-operator.v1.5.0" ->
// "1.5.0"), or "".
func CSVVersion(name string) string {
	if _, after, ok := strings.Cut(name, ".v"); ok {
		return after
	}
	return ""
}

// VersionCore is a version without its pre-release or build suffix.
func VersionCore(v string) string {
	return strings.SplitN(strings.SplitN(v, "-", 2)[0], "+", 2)[0]
}

// ReadOperator finds op's CSV anywhere on the cluster (OLM's copies
// excluded) and its Subscriptions. A read that fails is an error: callers
// refuse rather than guess.
func ReadOperator(c *kube.Client, op OperatorSpec) (OperatorState, error) {
	csvs, err := components.ListClusterCSVs(c)
	if err != nil {
		return OperatorState{}, err
	}
	subs, err := ListSubscriptions(c, "")
	if err != nil {
		return OperatorState{}, err
	}
	return ReadOperatorIn(c, op, csvs, subs)
}

// ReadOperatorIn is ReadOperator with the cluster's CSVs and Subscriptions
// already listed.
func ReadOperatorIn(c *kube.Client, op OperatorSpec, csvs []components.ClusterCSV, subs []*OLMSubscription) (OperatorState, error) {
	var st OperatorState
	for _, csv := range csvs {
		if !slices.Contains(csv.Packages(), op.Pkg) && components.CSVBaseName(csv.Name) != op.Pkg {
			continue
		}
		v := CSVVersion(csv.Name)
		// During an upgrade OLM keeps the old CSV (Replacing) next to the
		// new one: the newest is the one to report.
		if st.CSV == "" || kube.CompareVersions(VersionCore(v), VersionCore(st.Version)) > 0 {
			st.CSV, st.CSVNS, st.Version, st.Phase, st.Deployments = csv.Name, csv.Namespace, v, csv.Phase, csv.Deployments
		}
	}
	ns := kube.FirstNonEmpty(st.CSVNS, op.Namespace)
	for _, name := range op.SubNames {
		sub, err := ReadOperatorSubscription(c, ns, name)
		if err != nil {
			return st, err
		}
		if sub != nil && sub.Spec.Name == op.Pkg {
			st.Sub = sub
			break
		}
	}
	// Any Subscription of the package: the CSV's namespace's first.
	for _, s := range subs {
		if s.Spec.Name != op.Pkg {
			continue
		}
		if st.Requested == nil || (s.Metadata.Namespace == ns && st.Requested.Metadata.Namespace != ns) {
			st.Requested = s
		}
	}
	if st.Requested == nil {
		st.Requested = st.Sub
	}
	return st, nil
}

// --- the catalog -------------------------------------------------------------------

// PackageChannel is one channel of a PackageManifest.
type PackageChannel struct {
	Name           string `json:"name"`
	CurrentCSV     string `json:"currentCSV"`
	CurrentCSVDesc struct {
		Version      string `json:"version"`
		InstallModes []struct {
			Type      string `json:"type"`
			Supported bool   `json:"supported"`
		} `json:"installModes"`
	} `json:"currentCSVDesc"`
	Entries []struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"entries"`
}

// Head is the version of the channel's newest CSV.
func (ch PackageChannel) Head() string {
	return VersionCore(kube.FirstNonEmpty(ch.CurrentCSVDesc.Version, CSVVersion(ch.CurrentCSV)))
}

func (ch PackageChannel) Has(csv string) bool {
	for _, e := range ch.Entries {
		if e.Name == csv {
			return true
		}
	}
	return false
}

// supports reports whether the channel's head supports an install mode;
// known is false when the catalog does not say.
func (ch PackageChannel) supports(mode string) (supported, known bool) {
	for _, m := range ch.CurrentCSVDesc.InstallModes {
		if m.Type == mode {
			return m.Supported, true
		}
	}
	return false, len(ch.CurrentCSVDesc.InstallModes) > 0
}

// OperatorPackage is a package as a catalog in openshift-marketplace offers it.
type OperatorPackage struct {
	DefaultChannel string
	Channels       []PackageChannel
}

func (p OperatorPackage) Channel(name string) (PackageChannel, bool) {
	for _, ch := range p.Channels {
		if ch.Name == name {
			return ch, true
		}
	}
	return PackageChannel{}, false
}

// ReadOperatorPackage reads pkg from catalog source (openshift-marketplace),
// scoped to that catalog: several catalogs may publish a package of the
// same name. found is false when the catalog does not offer it.
func ReadOperatorPackage(c *kube.Client, pkg, source string) (OperatorPackage, bool, error) {
	path := rhoai.NamespacedPath("packages.operators.coreos.com/v1", "packagemanifests", rhoai.CatalogNS, "") +
		"?labelSelector=" + url.QueryEscape("catalog="+source) + "&fieldSelector=" + url.QueryEscape("metadata.name="+pkg)
	var items []struct {
		Metadata kube.ObjectMeta `json:"metadata"`
		Status   struct {
			PackageName            string           `json:"packageName"`
			CatalogSource          string           `json:"catalogSource"`
			CatalogSourceNamespace string           `json:"catalogSourceNamespace"`
			DefaultChannel         string           `json:"defaultChannel"`
			Channels               []PackageChannel `json:"channels"`
		} `json:"status"`
	}
	if err := c.ListInto(path, &items); err != nil {
		if kube.IsDecodeError(err) {
			return OperatorPackage{}, false, fmt.Errorf("parse package %s: %w", pkg, err)
		}
		return OperatorPackage{}, false, fmt.Errorf("read package %s from catalog %s: %w", pkg, source, err)
	}
	for _, it := range items {
		if it.Metadata.Name == pkg && kube.FirstNonEmpty(it.Status.PackageName, pkg) == pkg && it.Status.CatalogSource == source && kube.FirstNonEmpty(it.Status.CatalogSourceNamespace, rhoai.CatalogNS) == rhoai.CatalogNS {
			return OperatorPackage{DefaultChannel: it.Status.DefaultChannel, Channels: it.Status.Channels}, true, nil
		}
	}
	return OperatorPackage{}, false, nil
}

// --- installing ----------------------------------------------------------------------

// OperatorInstallPlan is what installing op takes, as checked before any
// write: the channel, and whether its namespace and OperatorGroup must be
// made. ReusedGroup names the namespace's OperatorGroup when it is used as
// it is.
type OperatorInstallPlan struct {
	Op           OperatorSpec
	Channel      string
	Head         string
	NSMissing    bool
	GroupMissing bool
	ReusedGroup  string
}

// PlanOperatorInstall checks that op can be installed as the docs install
// it. A refusal is a message and an error code.
func PlanOperatorInstall(c *kube.Client, op OperatorSpec) (*OperatorInstallPlan, string, string) {
	plan := &OperatorInstallPlan{Op: op}
	pkg, found, err := ReadOperatorPackage(c, op.Pkg, op.Source)
	switch {
	case err != nil:
		return nil, fmt.Sprintf("Cannot read the %s package from catalog %s, so this tool cannot tell which channel to install; nothing was changed: %v.%s", op.Title, op.Source, err, kube.TemplateHint(err)), kube.FirstNonEmpty(kube.ErrorCodeFromK8sErr(err), "internal")
	case !found:
		return nil, fmt.Sprintf("Catalog %s in %s does not offer package %s, so %s cannot be installed from it. A cluster-admin checks the catalog (oc get packagemanifests -n %s -l catalog=%s). Nothing was changed.", op.Source, rhoai.CatalogNS, op.Pkg, op.Title, rhoai.CatalogNS, op.Source), "prerequisites"
	}
	if op.Channel != nil {
		var refusal string
		if plan.Channel, refusal = op.Channel(pkg); plan.Channel == "" {
			return nil, refusal, "prerequisites"
		}
	} else {
		plan.Channel = pkg.DefaultChannel
		if _, ok := pkg.Channel(plan.Channel); !ok {
			return nil, fmt.Sprintf("Package %s in %s names no usable default channel. Nothing was changed.", op.Pkg, op.Source), "prerequisites"
		}
	}
	ch, _ := pkg.Channel(plan.Channel)
	plan.Head = ch.Head()
	// The install mode of an OperatorGroup this tool would create: refused
	// before anything is written when the channel's head says it does not
	// support it (OLM would fail the CSV with UnsupportedOperatorGroup).
	newGroupMode := "AllNamespaces"
	if op.OwnNamespace {
		newGroupMode = "OwnNamespace"
	}
	newGroupRefusal := func() string {
		if supported, known := ch.supports(newGroupMode); known && !supported {
			return fmt.Sprintf("%s %s does not support being installed for %s, which is how this tool installs it (its catalog lists the install modes it supports). A cluster-admin installs it. Nothing was changed.", op.Title, plan.Head, map[bool]string{true: "its own namespace", false: "all namespaces"}[op.OwnNamespace])
		}
		return ""
	}

	_, nsFound, err := kube.ReadObjectMeta(c, "/api/v1/namespaces/"+op.Namespace)
	if err != nil {
		return nil, fmt.Sprintf("Cannot read namespace %s; nothing was changed: %v.%s", op.Namespace, err, kube.TemplateHint(err)), kube.FirstNonEmpty(kube.ErrorCodeFromK8sErr(err), "internal")
	}
	plan.NSMissing = !nsFound
	if !nsFound {
		if op.Group == "" {
			return nil, fmt.Sprintf("Namespace %s does not exist; OpenShift makes it for operators of all namespaces. Nothing was changed.", op.Namespace), "prerequisites"
		}
		if why := newGroupRefusal(); why != "" {
			return nil, why, "prerequisites"
		}
		plan.GroupMissing = true
		return plan, "", ""
	}
	groups, err := readOperatorGroups(c, op.Namespace)
	if err != nil {
		return nil, fmt.Sprintf("Cannot read the OperatorGroups in %s; nothing was changed: %v.%s", op.Namespace, err, kube.TemplateHint(err)), kube.FirstNonEmpty(kube.ErrorCodeFromK8sErr(err), "internal")
	}
	switch {
	case len(groups) > 1:
		return nil, fmt.Sprintf("Namespace %s has %d OperatorGroups (%s), and OLM installs nothing in a namespace with more than one. A cluster-admin removes the extra ones. Nothing was changed.", op.Namespace, len(groups), strings.Join(groups.names(), ", ")), "prerequisites"
	case len(groups) == 0 && op.Group == "":
		return nil, fmt.Sprintf("Namespace %s has no OperatorGroup, which OpenShift makes for it (global-operators). A cluster-admin restores it. Nothing was changed.", op.Namespace), "prerequisites"
	case len(groups) == 0:
		if why := newGroupRefusal(); why != "" {
			return nil, why, "prerequisites"
		}
		plan.GroupMissing = true
		return plan, "", ""
	}
	// One OperatorGroup: a namespace may have only one, so it is used as
	// it is when it gives an install mode the operator supports (OLM fails
	// the CSV with UnsupportedOperatorGroup otherwise). Without install
	// modes in the catalog, only the mode this tool would create is
	// accepted.
	g := groups[0]
	mode := g.InstallMode(op.Namespace)
	want := "AllNamespaces"
	if op.OwnNamespace {
		want = "OwnNamespace"
	}
	supported, known := ch.supports(mode)
	if mode == "" || (known && !supported) || (!known && mode != want) {
		where := "only some namespaces"
		switch mode {
		case "AllNamespaces":
			where = "all namespaces"
		case "OwnNamespace":
			where = "only its own namespace"
		}
		return nil, fmt.Sprintf("OperatorGroup %s/%s targets %s, and %s is not installed that way (it is installed for %s, as its docs install it). A cluster-admin installs it elsewhere or changes the OperatorGroup. Nothing was changed.", op.Namespace, g.Metadata.Name, where, op.Title, map[bool]string{true: "its own namespace", false: "all namespaces"}[op.OwnNamespace]), "prerequisites"
	}
	plan.ReusedGroup = g.Metadata.Name
	return plan, "", ""
}

type operatorGroups []components.OperatorGroup

func (gs operatorGroups) names() []string {
	var out []string
	for _, g := range gs {
		out = append(out, g.Metadata.Name)
	}
	return out
}

func readOperatorGroups(c *kube.Client, ns string) (operatorGroups, error) {
	var list struct {
		Items operatorGroups `json:"items"`
	}
	if err := c.GetInto(rhoai.NamespacedPath("operators.coreos.com/v1", "operatorgroups", ns, ""), &list); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// OperatorObjects are the docs' objects for installing op on channel.
func OperatorObjects(op OperatorSpec, channel string) (ns, group, sub map[string]any) {
	ns = map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": op.Namespace, "labels": ToolLabels()}}
	group = map[string]any{"apiVersion": "operators.coreos.com/v1", "kind": "OperatorGroup",
		"metadata": map[string]any{"name": op.Group, "namespace": op.Namespace, "labels": ToolLabels()}}
	if op.OwnNamespace {
		group["spec"] = map[string]any{"targetNamespaces": []any{op.Namespace}}
	}
	sub = map[string]any{
		"apiVersion": "operators.coreos.com/v1alpha1",
		"kind":       "Subscription",
		"metadata":   map[string]any{"name": op.SubName, "namespace": op.Namespace, "labels": ToolLabels()},
		"spec": map[string]any{
			"channel": channel, "installPlanApproval": "Automatic", "name": op.Pkg,
			"source": op.Source, "sourceNamespace": rhoai.CatalogNS,
		},
	}
	if len(op.Env) > 0 {
		env := make([]any, len(op.Env))
		for i, e := range op.Env {
			env[i] = e
		}
		sub["spec"].(map[string]any)["config"] = map[string]any{"env": env}
	}
	return ns, group, sub
}

// CreateOperatorObjects creates what the plan says is missing (a server-side
// dry run with dryRun): the namespace, the OperatorGroup, the Subscription.
// The Subscription's dry run needs its namespace, so it is skipped when the
// namespace does not exist yet. It returns what it created.
func CreateOperatorObjects(c *kube.Client, plan *OperatorInstallPlan, dryRun bool) ([]string, error) {
	ns, group, sub := OperatorObjects(plan.Op, plan.Channel)
	var query url.Values
	if dryRun {
		query = url.Values{"dryRun": {"All"}}
	}
	var made []string
	if plan.NSMissing {
		data, _ := json.Marshal(ns)
		if _, _, err := c.Do(http.MethodPost, WithToolFieldManager("/api/v1/namespaces"), "application/json", data, query); err != nil {
			return made, fmt.Errorf("create Namespace %s: %w", plan.Op.Namespace, err)
		}
		made = append(made, "Namespace "+plan.Op.Namespace)
		if dryRun {
			return made, nil
		}
	}
	if plan.GroupMissing {
		data, _ := json.Marshal(group)
		if _, _, err := c.Do(http.MethodPost, WithToolFieldManager(rhoai.NamespacedPath("operators.coreos.com/v1", "operatorgroups", plan.Op.Namespace, "")), "application/json", data, query); err != nil {
			return made, fmt.Errorf("create OperatorGroup %s/%s: %w", plan.Op.Namespace, plan.Op.Group, err)
		}
		made = append(made, "OperatorGroup "+plan.Op.Namespace+"/"+plan.Op.Group)
	}
	data, _ := json.Marshal(sub)
	if _, _, err := c.Do(http.MethodPost, WithToolFieldManager(rhoai.NamespacedPath("operators.coreos.com/v1alpha1", "subscriptions", plan.Op.Namespace, "")), "application/json", data, query); err != nil {
		return made, fmt.Errorf("create Subscription %s/%s: %w", plan.Op.Namespace, plan.Op.SubName, err)
	}
	made = append(made, "Subscription "+plan.Op.Namespace+"/"+plan.Op.SubName)
	return made, nil
}

// csvPhase reads a CSV's phase and message ("" when it does not exist yet).
func csvPhase(c *kube.Client, ns, name string) (string, string, error) {
	var csv struct {
		Status struct {
			Phase   string `json:"phase"`
			Message string `json:"message"`
		} `json:"status"`
	}
	found, err := c.GetIntoOptional(rhoai.NamespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", ns, name), &csv)
	if err != nil {
		return "", "", err
	}
	if !found {
		return "", "", nil
	}
	return csv.Status.Phase, csv.Status.Message, nil
}

// SubscriptionProgress is one read of a Subscription while OLM works on it.
type SubscriptionProgress struct {
	InstalledCSV, currentCSV, state, phase string
	// done: the installed CSV is the current one and Succeeded.
	Done bool
	// Failed is why OLM cannot go on ("" while it can).
	Failed string
	Text   string
}

func ReadSubscriptionProgress(c *kube.Client, ns, name string) (SubscriptionProgress, error) {
	sub, err := ReadOperatorSubscription(c, ns, name)
	if err != nil {
		return SubscriptionProgress{}, err
	}
	if sub == nil {
		return SubscriptionProgress{}, fmt.Errorf("subscription %s/%s is gone", ns, name)
	}
	p := SubscriptionProgress{InstalledCSV: sub.Status.InstalledCSV, currentCSV: sub.Status.CurrentCSV, state: sub.Status.State}
	if cond, ok := kube.FindCondition(sub.Status.Conditions, "ResolutionFailed"); ok && cond.Status == "True" {
		p.Failed = "OLM cannot resolve it: " + kube.TruncateText(kube.FirstNonEmpty(cond.Message, cond.Reason), 300)
	}
	if p.InstalledCSV == "" {
		p.Text = "OLM is resolving the Subscription (" + kube.FirstNonEmpty(p.state, "no state yet") + ")"
		return p, nil
	}
	phase, msg, err := csvPhase(c, ns, p.InstalledCSV)
	if err != nil {
		return p, err
	}
	p.phase = phase
	switch {
	case phase == "Failed":
		p.Failed = fmt.Sprintf("CSV %s failed: %s", p.InstalledCSV, kube.TruncateText(kube.FirstNonEmpty(msg, "no message"), 300))
	case phase == "Succeeded" && (p.currentCSV == "" || p.currentCSV == p.InstalledCSV):
		p.Done = true
	}
	p.Text = fmt.Sprintf("%s: %s", p.InstalledCSV, kube.FirstNonEmpty(phase, "being created"))
	if p.currentCSV != "" && p.currentCSV != p.InstalledCSV {
		p.Text += "; upgrading to " + p.currentCSV
	}
	return p, nil
}

// WaitForSubscription waits until OLM has installed the Subscription's
// current CSV (or reports why it cannot), reporting progress.
func WaitForSubscription(c *kube.Client, ns, name string, progress func(string)) (SubscriptionProgress, error) {
	var last SubscriptionProgress
	var failedSince time.Time
	_, err := kube.WaitForGone(c, OperatorTimeout, PollInterval, func(cc *kube.Client) (bool, error) {
		p, err := ReadSubscriptionProgress(cc, ns, name)
		if err != nil {
			return false, err
		}
		if p.Text != last.Text && progress != nil {
			progress(p.Text)
		}
		last = p
		if p.Done {
			return true, nil
		}
		// OLM reports a resolution failure while a catalog refreshes, too:
		// only one that lasts stops the wait.
		if p.Failed != "" {
			if failedSince.IsZero() {
				failedSince = kube.Clock().Now()
			}
			return kube.Clock().Since(failedSince) >= OperatorResolveGrace, nil
		}
		failedSince = time.Time{}
		return false, nil
	})
	return last, err
}

// PatchSubscription sets fields of a Subscription this tool may change (a
// merge patch under the tool's field manager; a server-side dry run with
// dryRun).
func PatchSubscription(c *kube.Client, sub *OLMSubscription, spec map[string]any, dryRun bool) error {
	path, ok := operatorSubscriptionPath(sub.Metadata.Namespace, sub.Metadata.Name)
	if !ok {
		return fmt.Errorf("subscription %s/%s is not one this tool changes", sub.Metadata.Namespace, sub.Metadata.Name)
	}
	data, _ := json.Marshal(map[string]any{"metadata": map[string]any{"resourceVersion": sub.Metadata.ResourceVersion}, "spec": spec})
	query := url.Values{"fieldManager": {ToolFieldManager}}
	if dryRun {
		query.Set("dryRun", "All")
	}
	body, _, err := c.Do(http.MethodPatch, path, "application/merge-patch+json", data, query)
	if err != nil {
		return err
	}
	if !dryRun {
		if meta, perr := kube.ParseObjectMeta(body); perr == nil {
			sub.Metadata.ResourceVersion = meta.ResourceVersion
		}
	}
	return nil
}

// ControllerDeployed reports whether a Deployment of that name exists in any
// namespace. A served API alone does not mean an operator runs: OLM leaves an
// operator's CRDs behind when it is uninstalled (seen live 2026-11-18 after
// removing Connectivity Link 1.5.0 and CloudNativePG), and objects made then
// are never reconciled. The field selector narrows the list on a real API
// server; the name is checked here too.
func ControllerDeployed(c *kube.Client, name string) (bool, error) {
	var items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	if err := c.ListInto("/apis/apps/v1/deployments?fieldSelector="+url.QueryEscape("metadata.name="+name), &items); err != nil {
		return false, fmt.Errorf("list Deployments named %s: %w", name, err)
	}
	for _, d := range items {
		if d.Metadata.Name == name {
			return true, nil
		}
	}
	return false, nil
}
