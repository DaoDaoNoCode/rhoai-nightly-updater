package cluster

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
)

// Operators this tool installs from OperatorHub for an Environment feature
// (MaaS, Models), described as data (operatorSpec) and installed the way the
// product docs install them: a Namespace, an OperatorGroup when the
// namespace has none, and a Subscription with installPlanApproval
// Automatic. What is installed already is found by its package, wherever it
// is (operatorState), and used as it is.

var (
	// MaaSOperatorTimeout bounds the wait for OLM to install or upgrade an
	// operator (the docs wait 300 s for AtLatestKnown, then for the
	// operator's Deployment).
	MaaSOperatorTimeout = 10 * time.Minute
	// MaaSOperatorResolveGrace is how long a Subscription may stay at an
	// older CSV after its channel changed before OLM is taken to have no
	// path there (AtLatestKnown: seen live 2026-11-18, RHCL 1.4.3 switched
	// straight to stable-1.5).
	MaaSOperatorResolveGrace = 2 * time.Minute
)

// operatorSpec is an operator this tool installs and how.
type operatorSpec struct {
	// id names the operator in step and part IDs ("cert-manager").
	id    string
	title string
	pkg   string
	// subName is the Subscription this tool creates; subNames the names an
	// existing install's Subscription is found by in its CSV's namespace
	// (and the only ones this tool changes).
	subName   string
	subNames  []string
	namespace string
	source    string
	// group is the OperatorGroup created when the namespace has none; ""
	// when the namespace must have one already (openshift-operators).
	group string
	// ownNamespace: the group created targets its own namespace only
	// (install mode OwnNamespace); else it has no spec (AllNamespaces).
	ownNamespace bool
	// api is the API group the operator serves, and controller the name of
	// its controller Deployment: the API served without a CSV of the
	// package counts as installed only while that Deployment exists
	// (controllerDeployed: CRDs stay after an uninstall).
	api, controller string
	// channel picks the channel to install from the catalog's package (a
	// refusal when none fits); nil: the package's default channel.
	channel func(operatorPackage) (string, string)
	// env is the Subscription's spec.config.env ("" none).
	env []map[string]any
}

// operatorSubscriptionPath is the path of an operator Subscription this
// tool reads by name or changes; only these names, written out so the RBAC
// rules (resourceNames) can be checked against them (rbac_coverage_test.go).
// Any other Subscription is read from the namespace's list.
func operatorSubscriptionPath(ns, name string) (string, bool) {
	switch name {
	case "kuadrant-operator":
		return namespacedPath("operators.coreos.com/v1alpha1", "subscriptions", ns, "kuadrant-operator"), true
	case "rhcl-operator":
		return namespacedPath("operators.coreos.com/v1alpha1", "subscriptions", ns, "rhcl-operator"), true
	case "cloudnative-pg":
		return namespacedPath("operators.coreos.com/v1alpha1", "subscriptions", ns, "cloudnative-pg"), true
	}
	return "", false
}

// olmSubscription is a Subscription as read.
type olmSubscription struct {
	Metadata objectMeta `json:"metadata"`
	Spec     struct {
		Channel             string         `json:"channel"`
		Name                string         `json:"name"`
		Source              string         `json:"source"`
		SourceNamespace     string         `json:"sourceNamespace"`
		InstallPlanApproval string         `json:"installPlanApproval"`
		Config              map[string]any `json:"config"`
	} `json:"spec"`
	Status struct {
		State        string         `json:"state"`
		InstalledCSV string         `json:"installedCSV"`
		CurrentCSV   string         `json:"currentCSV"`
		LastUpdated  string         `json:"lastUpdated"`
		Conditions   []k8sCondition `json:"conditions"`
	} `json:"status"`
	// argoApp is the Argo CD application managing it (argoTracking.managedBy),
	// set by readOperatorSubscription.
	argoApp string
}

func parseSubscription(raw []byte) (*olmSubscription, error) {
	var sub olmSubscription
	if err := json.Unmarshal(raw, &sub); err != nil {
		return nil, err
	}
	return &sub, nil
}

// withArgoManager sets sub.argoApp: the Argo CD application that manages
// it, as Argo CD answers (argocd.go).
func withArgoManager(c *Client, sub *olmSubscription) *olmSubscription {
	if sub != nil {
		sub.argoApp = newArgoCheck(c).object(sub.Metadata.Annotations, sub.Metadata.Labels,
			argoObject{group: argoSubscriptionObject.group, kind: "Subscription", namespace: sub.Metadata.Namespace, name: sub.Metadata.Name}).managedBy()
	}
	return sub
}

// readOperatorSubscription reads Subscription ns/name: by name when it is
// one this tool may change, else from the namespace's list (a missing one
// is nil).
func readOperatorSubscription(c *Client, ns, name string) (*olmSubscription, error) {
	path, ok := operatorSubscriptionPath(ns, name)
	if !ok {
		subs, err := listSubscriptions(c, ns)
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

// listSubscriptions lists the Subscriptions of namespace ns, or of every
// namespace when ns is "".
func listSubscriptions(c *Client, ns string) ([]*olmSubscription, error) {
	path := clusterPath("operators.coreos.com/v1alpha1", "subscriptions", "")
	if ns != "" {
		path = namespacedPath("operators.coreos.com/v1alpha1", "subscriptions", ns, "")
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
	out := make([]*olmSubscription, 0, len(list.Items))
	for _, raw := range list.Items {
		sub, err := parseSubscription(raw)
		if err != nil {
			return nil, fmt.Errorf("parse the Subscription list: %w", err)
		}
		out = append(out, sub)
	}
	return out, nil
}

// operatorState is an operator as installed: its CSV (the newest when OLM
// is replacing one), the Subscription found by the documented names (sub,
// the one this tool may change), and any Subscription of its package in any
// namespace (requested): RHOAI's KServe counts an operator as there when a
// Subscription of its package exists anywhere (odh-platform-utilities
// pkg/cluster/olm OperatorPackageRequested), and a second one elsewhere
// would install the operator twice.
type operatorState struct {
	csv, csvNS, version, phase string
	deployments                []string
	sub                        *olmSubscription
	requested                  *olmSubscription
}

func (s operatorState) installed() bool { return s.csv != "" }

// pending is the Subscription OLM is installing from, when there is no CSV
// yet (nil otherwise).
func (s operatorState) pending() *olmSubscription {
	if s.installed() {
		return nil
	}
	if s.sub != nil {
		return s.sub
	}
	return s.requested
}

// csvVersion is the version in a CSV name ("rhcl-operator.v1.5.0" ->
// "1.5.0"), or "".
func csvVersion(name string) string {
	if _, after, ok := strings.Cut(name, ".v"); ok {
		return after
	}
	return ""
}

// versionCore is a version without its pre-release or build suffix.
func versionCore(v string) string {
	return strings.SplitN(strings.SplitN(v, "-", 2)[0], "+", 2)[0]
}

// readOperator finds op's CSV anywhere on the cluster (OLM's copies
// excluded) and its Subscriptions. A read that fails is an error: callers
// refuse rather than guess.
func readOperator(c *Client, op operatorSpec) (operatorState, error) {
	csvs, err := listClusterCSVs(c)
	if err != nil {
		return operatorState{}, err
	}
	subs, err := listSubscriptions(c, "")
	if err != nil {
		return operatorState{}, err
	}
	return readOperatorIn(c, op, csvs, subs)
}

// readOperatorIn is readOperator with the cluster's CSVs and Subscriptions
// already listed.
func readOperatorIn(c *Client, op operatorSpec, csvs []clusterCSV, subs []*olmSubscription) (operatorState, error) {
	var st operatorState
	for _, csv := range csvs {
		if !slices.Contains(csv.packages(), op.pkg) && csvBaseName(csv.Name) != op.pkg {
			continue
		}
		v := csvVersion(csv.Name)
		// During an upgrade OLM keeps the old CSV (Replacing) next to the
		// new one: the newest is the one to report.
		if st.csv == "" || compareVersions(versionCore(v), versionCore(st.version)) > 0 {
			st.csv, st.csvNS, st.version, st.phase, st.deployments = csv.Name, csv.Namespace, v, csv.Phase, csv.Deployments
		}
	}
	ns := firstNonEmpty(st.csvNS, op.namespace)
	for _, name := range op.subNames {
		sub, err := readOperatorSubscription(c, ns, name)
		if err != nil {
			return st, err
		}
		if sub != nil && sub.Spec.Name == op.pkg {
			st.sub = sub
			break
		}
	}
	// Any Subscription of the package: the CSV's namespace's first.
	for _, s := range subs {
		if s.Spec.Name != op.pkg {
			continue
		}
		if st.requested == nil || (s.Metadata.Namespace == ns && st.requested.Metadata.Namespace != ns) {
			st.requested = s
		}
	}
	if st.requested == nil {
		st.requested = st.sub
	}
	return st, nil
}

// --- the catalog -------------------------------------------------------------------

// packageChannel is one channel of a PackageManifest.
type packageChannel struct {
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

// head is the version of the channel's newest CSV.
func (ch packageChannel) head() string {
	return versionCore(firstNonEmpty(ch.CurrentCSVDesc.Version, csvVersion(ch.CurrentCSV)))
}

func (ch packageChannel) has(csv string) bool {
	for _, e := range ch.Entries {
		if e.Name == csv {
			return true
		}
	}
	return false
}

// supports reports whether the channel's head supports an install mode;
// known is false when the catalog does not say.
func (ch packageChannel) supports(mode string) (supported, known bool) {
	for _, m := range ch.CurrentCSVDesc.InstallModes {
		if m.Type == mode {
			return m.Supported, true
		}
	}
	return false, len(ch.CurrentCSVDesc.InstallModes) > 0
}

// operatorPackage is a package as a catalog in openshift-marketplace offers it.
type operatorPackage struct {
	defaultChannel string
	channels       []packageChannel
}

func (p operatorPackage) channel(name string) (packageChannel, bool) {
	for _, ch := range p.channels {
		if ch.Name == name {
			return ch, true
		}
	}
	return packageChannel{}, false
}

// readOperatorPackage reads pkg from catalog source (openshift-marketplace),
// scoped to that catalog: several catalogs may publish a package of the
// same name. found is false when the catalog does not offer it.
func readOperatorPackage(c *Client, pkg, source string) (operatorPackage, bool, error) {
	path := namespacedPath("packages.operators.coreos.com/v1", "packagemanifests", CatalogNS, "") +
		"?labelSelector=" + url.QueryEscape("catalog="+source) + "&fieldSelector=" + url.QueryEscape("metadata.name="+pkg)
	var items []struct {
		Metadata objectMeta `json:"metadata"`
		Status   struct {
			PackageName            string           `json:"packageName"`
			CatalogSource          string           `json:"catalogSource"`
			CatalogSourceNamespace string           `json:"catalogSourceNamespace"`
			DefaultChannel         string           `json:"defaultChannel"`
			Channels               []packageChannel `json:"channels"`
		} `json:"status"`
	}
	if err := c.ListInto(path, &items); err != nil {
		if kube.IsDecodeError(err) {
			return operatorPackage{}, false, fmt.Errorf("parse package %s: %w", pkg, err)
		}
		return operatorPackage{}, false, fmt.Errorf("read package %s from catalog %s: %w", pkg, source, err)
	}
	for _, it := range items {
		if it.Metadata.Name == pkg && firstNonEmpty(it.Status.PackageName, pkg) == pkg && it.Status.CatalogSource == source && firstNonEmpty(it.Status.CatalogSourceNamespace, CatalogNS) == CatalogNS {
			return operatorPackage{defaultChannel: it.Status.DefaultChannel, channels: it.Status.Channels}, true, nil
		}
	}
	return operatorPackage{}, false, nil
}

// --- installing ----------------------------------------------------------------------

// operatorInstallPlan is what installing op takes, as checked before any
// write: the channel, and whether its namespace and OperatorGroup must be
// made. reusedGroup names the namespace's OperatorGroup when it is used as
// it is.
type operatorInstallPlan struct {
	op           operatorSpec
	channel      string
	head         string
	nsMissing    bool
	groupMissing bool
	reusedGroup  string
}

// planOperatorInstall checks that op can be installed as the docs install
// it. A refusal is a message and an error code.
func planOperatorInstall(c *Client, op operatorSpec) (*operatorInstallPlan, string, string) {
	plan := &operatorInstallPlan{op: op}
	pkg, found, err := readOperatorPackage(c, op.pkg, op.source)
	switch {
	case err != nil:
		return nil, fmt.Sprintf("Cannot read the %s package from catalog %s, so this tool cannot tell which channel to install; nothing was changed: %v.%s", op.title, op.source, err, templateHint(err)), firstNonEmpty(errorCodeFromK8sErr(err), "internal")
	case !found:
		return nil, fmt.Sprintf("Catalog %s in %s does not offer package %s, so %s cannot be installed from it. A cluster-admin checks the catalog (oc get packagemanifests -n %s -l catalog=%s). Nothing was changed.", op.source, CatalogNS, op.pkg, op.title, CatalogNS, op.source), "prerequisites"
	}
	if op.channel != nil {
		var refusal string
		if plan.channel, refusal = op.channel(pkg); plan.channel == "" {
			return nil, refusal, "prerequisites"
		}
	} else {
		plan.channel = pkg.defaultChannel
		if _, ok := pkg.channel(plan.channel); !ok {
			return nil, fmt.Sprintf("Package %s in %s names no usable default channel. Nothing was changed.", op.pkg, op.source), "prerequisites"
		}
	}
	ch, _ := pkg.channel(plan.channel)
	plan.head = ch.head()
	// The install mode of an OperatorGroup this tool would create: refused
	// before anything is written when the channel's head says it does not
	// support it (OLM would fail the CSV with UnsupportedOperatorGroup).
	newGroupMode := "AllNamespaces"
	if op.ownNamespace {
		newGroupMode = "OwnNamespace"
	}
	newGroupRefusal := func() string {
		if supported, known := ch.supports(newGroupMode); known && !supported {
			return fmt.Sprintf("%s %s does not support being installed for %s, which is how this tool installs it (its catalog lists the install modes it supports). A cluster-admin installs it. Nothing was changed.", op.title, plan.head, map[bool]string{true: "its own namespace", false: "all namespaces"}[op.ownNamespace])
		}
		return ""
	}

	_, nsFound, err := readObjectMeta(c, "/api/v1/namespaces/"+op.namespace)
	if err != nil {
		return nil, fmt.Sprintf("Cannot read namespace %s; nothing was changed: %v.%s", op.namespace, err, templateHint(err)), firstNonEmpty(errorCodeFromK8sErr(err), "internal")
	}
	plan.nsMissing = !nsFound
	if !nsFound {
		if op.group == "" {
			return nil, fmt.Sprintf("Namespace %s does not exist; OpenShift makes it for operators of all namespaces. Nothing was changed.", op.namespace), "prerequisites"
		}
		if why := newGroupRefusal(); why != "" {
			return nil, why, "prerequisites"
		}
		plan.groupMissing = true
		return plan, "", ""
	}
	groups, err := readOperatorGroups(c, op.namespace)
	if err != nil {
		return nil, fmt.Sprintf("Cannot read the OperatorGroups in %s; nothing was changed: %v.%s", op.namespace, err, templateHint(err)), firstNonEmpty(errorCodeFromK8sErr(err), "internal")
	}
	switch {
	case len(groups) > 1:
		return nil, fmt.Sprintf("Namespace %s has %d OperatorGroups (%s), and OLM installs nothing in a namespace with more than one. A cluster-admin removes the extra ones. Nothing was changed.", op.namespace, len(groups), strings.Join(groups.names(), ", ")), "prerequisites"
	case len(groups) == 0 && op.group == "":
		return nil, fmt.Sprintf("Namespace %s has no OperatorGroup, which OpenShift makes for it (global-operators). A cluster-admin restores it. Nothing was changed.", op.namespace), "prerequisites"
	case len(groups) == 0:
		if why := newGroupRefusal(); why != "" {
			return nil, why, "prerequisites"
		}
		plan.groupMissing = true
		return plan, "", ""
	}
	// One OperatorGroup: a namespace may have only one, so it is used as
	// it is when it gives an install mode the operator supports (OLM fails
	// the CSV with UnsupportedOperatorGroup otherwise). Without install
	// modes in the catalog, only the mode this tool would create is
	// accepted.
	g := groups[0]
	mode := g.installMode(op.namespace)
	want := "AllNamespaces"
	if op.ownNamespace {
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
		return nil, fmt.Sprintf("OperatorGroup %s/%s targets %s, and %s is not installed that way (it is installed for %s, as its docs install it). A cluster-admin installs it elsewhere or changes the OperatorGroup. Nothing was changed.", op.namespace, g.Metadata.Name, where, op.title, map[bool]string{true: "its own namespace", false: "all namespaces"}[op.ownNamespace]), "prerequisites"
	}
	plan.reusedGroup = g.Metadata.Name
	return plan, "", ""
}

type operatorGroups []operatorGroup

func (gs operatorGroups) names() []string {
	var out []string
	for _, g := range gs {
		out = append(out, g.Metadata.Name)
	}
	return out
}

func readOperatorGroups(c *Client, ns string) (operatorGroups, error) {
	var list struct {
		Items operatorGroups `json:"items"`
	}
	if err := c.GetInto(namespacedPath("operators.coreos.com/v1", "operatorgroups", ns, ""), &list); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// operatorObjects are the docs' objects for installing op on channel.
func operatorObjects(op operatorSpec, channel string) (ns, group, sub map[string]any) {
	ns = map[string]any{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": op.namespace, "labels": toolLabels()}}
	group = map[string]any{"apiVersion": "operators.coreos.com/v1", "kind": "OperatorGroup",
		"metadata": map[string]any{"name": op.group, "namespace": op.namespace, "labels": toolLabels()}}
	if op.ownNamespace {
		group["spec"] = map[string]any{"targetNamespaces": []any{op.namespace}}
	}
	sub = map[string]any{
		"apiVersion": "operators.coreos.com/v1alpha1",
		"kind":       "Subscription",
		"metadata":   map[string]any{"name": op.subName, "namespace": op.namespace, "labels": toolLabels()},
		"spec": map[string]any{
			"channel": channel, "installPlanApproval": "Automatic", "name": op.pkg,
			"source": op.source, "sourceNamespace": CatalogNS,
		},
	}
	if len(op.env) > 0 {
		env := make([]any, len(op.env))
		for i, e := range op.env {
			env[i] = e
		}
		sub["spec"].(map[string]any)["config"] = map[string]any{"env": env}
	}
	return ns, group, sub
}

// createOperatorObjects creates what the plan says is missing (a server-side
// dry run with dryRun): the namespace, the OperatorGroup, the Subscription.
// The Subscription's dry run needs its namespace, so it is skipped when the
// namespace does not exist yet. It returns what it created.
func createOperatorObjects(c *Client, plan *operatorInstallPlan, dryRun bool) ([]string, error) {
	ns, group, sub := operatorObjects(plan.op, plan.channel)
	var query url.Values
	if dryRun {
		query = url.Values{"dryRun": {"All"}}
	}
	var made []string
	if plan.nsMissing {
		data, _ := json.Marshal(ns)
		if _, _, err := c.Do(http.MethodPost, withToolFieldManager("/api/v1/namespaces"), "application/json", data, query); err != nil {
			return made, fmt.Errorf("create Namespace %s: %w", plan.op.namespace, err)
		}
		made = append(made, "Namespace "+plan.op.namespace)
		if dryRun {
			return made, nil
		}
	}
	if plan.groupMissing {
		data, _ := json.Marshal(group)
		if _, _, err := c.Do(http.MethodPost, withToolFieldManager(namespacedPath("operators.coreos.com/v1", "operatorgroups", plan.op.namespace, "")), "application/json", data, query); err != nil {
			return made, fmt.Errorf("create OperatorGroup %s/%s: %w", plan.op.namespace, plan.op.group, err)
		}
		made = append(made, "OperatorGroup "+plan.op.namespace+"/"+plan.op.group)
	}
	data, _ := json.Marshal(sub)
	if _, _, err := c.Do(http.MethodPost, withToolFieldManager(namespacedPath("operators.coreos.com/v1alpha1", "subscriptions", plan.op.namespace, "")), "application/json", data, query); err != nil {
		return made, fmt.Errorf("create Subscription %s/%s: %w", plan.op.namespace, plan.op.subName, err)
	}
	made = append(made, "Subscription "+plan.op.namespace+"/"+plan.op.subName)
	return made, nil
}

// csvPhase reads a CSV's phase and message ("" when it does not exist yet).
func csvPhase(c *Client, ns, name string) (string, string, error) {
	var csv struct {
		Status struct {
			Phase   string `json:"phase"`
			Message string `json:"message"`
		} `json:"status"`
	}
	found, err := c.GetIntoOptional(namespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", ns, name), &csv)
	if err != nil {
		return "", "", err
	}
	if !found {
		return "", "", nil
	}
	return csv.Status.Phase, csv.Status.Message, nil
}

// subscriptionProgress is one read of a Subscription while OLM works on it.
type subscriptionProgress struct {
	installedCSV, currentCSV, state, phase string
	// done: the installed CSV is the current one and Succeeded.
	done bool
	// failed is why OLM cannot go on ("" while it can).
	failed string
	text   string
}

func readSubscriptionProgress(c *Client, ns, name string) (subscriptionProgress, error) {
	sub, err := readOperatorSubscription(c, ns, name)
	if err != nil {
		return subscriptionProgress{}, err
	}
	if sub == nil {
		return subscriptionProgress{}, fmt.Errorf("subscription %s/%s is gone", ns, name)
	}
	p := subscriptionProgress{installedCSV: sub.Status.InstalledCSV, currentCSV: sub.Status.CurrentCSV, state: sub.Status.State}
	if cond, ok := findCondition(sub.Status.Conditions, "ResolutionFailed"); ok && cond.Status == "True" {
		p.failed = "OLM cannot resolve it: " + truncateText(firstNonEmpty(cond.Message, cond.Reason), 300)
	}
	if p.installedCSV == "" {
		p.text = "OLM is resolving the Subscription (" + firstNonEmpty(p.state, "no state yet") + ")"
		return p, nil
	}
	phase, msg, err := csvPhase(c, ns, p.installedCSV)
	if err != nil {
		return p, err
	}
	p.phase = phase
	switch {
	case phase == "Failed":
		p.failed = fmt.Sprintf("CSV %s failed: %s", p.installedCSV, truncateText(firstNonEmpty(msg, "no message"), 300))
	case phase == "Succeeded" && (p.currentCSV == "" || p.currentCSV == p.installedCSV):
		p.done = true
	}
	p.text = fmt.Sprintf("%s: %s", p.installedCSV, firstNonEmpty(phase, "being created"))
	if p.currentCSV != "" && p.currentCSV != p.installedCSV {
		p.text += "; upgrading to " + p.currentCSV
	}
	return p, nil
}

// waitForSubscription waits until OLM has installed the Subscription's
// current CSV (or reports why it cannot), reporting progress.
func waitForSubscription(c *Client, ns, name string, progress func(string)) (subscriptionProgress, error) {
	var last subscriptionProgress
	var failedSince time.Time
	_, err := waitForGone(c, MaaSOperatorTimeout, MaaSPoll, func(cc *Client) (bool, error) {
		p, err := readSubscriptionProgress(cc, ns, name)
		if err != nil {
			return false, err
		}
		if p.text != last.text && progress != nil {
			progress(p.text)
		}
		last = p
		if p.done {
			return true, nil
		}
		// OLM reports a resolution failure while a catalog refreshes, too:
		// only one that lasts stops the wait.
		if p.failed != "" {
			if failedSince.IsZero() {
				failedSince = kube.Clock().Now()
			}
			return kube.Clock().Since(failedSince) >= MaaSOperatorResolveGrace, nil
		}
		failedSince = time.Time{}
		return false, nil
	})
	return last, err
}

// patchSubscription sets fields of a Subscription this tool may change (a
// merge patch under the tool's field manager; a server-side dry run with
// dryRun).
func patchSubscription(c *Client, sub *olmSubscription, spec map[string]any, dryRun bool) error {
	path, ok := operatorSubscriptionPath(sub.Metadata.Namespace, sub.Metadata.Name)
	if !ok {
		return fmt.Errorf("subscription %s/%s is not one this tool changes", sub.Metadata.Namespace, sub.Metadata.Name)
	}
	data, _ := json.Marshal(map[string]any{"metadata": map[string]any{"resourceVersion": sub.Metadata.ResourceVersion}, "spec": spec})
	query := url.Values{"fieldManager": {toolFieldManager}}
	if dryRun {
		query.Set("dryRun", "All")
	}
	body, _, err := c.Do(http.MethodPatch, path, "application/merge-patch+json", data, query)
	if err != nil {
		return err
	}
	if !dryRun {
		if meta, perr := parseObjectMeta(body); perr == nil {
			sub.Metadata.ResourceVersion = meta.ResourceVersion
		}
	}
	return nil
}

// controllerDeployed reports whether a Deployment of that name exists in any
// namespace. A served API alone does not mean an operator runs: OLM leaves an
// operator's CRDs behind when it is uninstalled (seen live 2026-11-18 after
// removing Connectivity Link 1.5.0 and CloudNativePG), and objects made then
// are never reconciled. The field selector narrows the list on a real API
// server; the name is checked here too.
func controllerDeployed(c *Client, name string) (bool, error) {
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
