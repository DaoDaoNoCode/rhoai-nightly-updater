package cluster

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"

	"strings"
	"time"
)

// The operators MaaS needs, installed from OperatorHub the way the MaaS
// install docs do it (opendatahub-io/models-as-a-service at 68b70af1,
// docs/content/install/platform-setup.md, "Red Hat OpenShift AI" tab):
//
//   - Red Hat Connectivity Link: Namespace kuadrant-system, OperatorGroup
//     kuadrant-operator-group (no spec: all namespaces), and Subscription
//     kuadrant-operator for package rhcl-operator from redhat-operators
//     (installPlanApproval Automatic), with the env the same tab gives under
//     "Rate limiting when Limitador is unavailable" (rate limits "may not be
//     enforced unless the Kuadrant operator is configured to deny traffic
//     on failure"): RATELIMIT_CHECK_SERVICE_FAILURE_MODE and
//     RATELIMIT_REPORT_SERVICE_FAILURE_MODE deny, so rate limits fail
//     closed (rhclDenyOnFailureEnv). AUTH_SERVICE_TIMEOUT, a load tuning
//     note, is not set. The channel is not the docs' "stable" (1.4.3, which
//     prerequisites.md warns about) but the one whose head meets the docs'
//     minimum for RHOAI 3.6 (rhclDocsMinimum), read from the PackageManifest.
//     The same page says RHOAI-managed clusters commonly name it
//     rhcl-operator (in rh-connectivity-link), so an existing install is
//     found by either name, in the namespace of its CSV.
//   - CloudNativePG, for this tool's database (the MaaS docs leave
//     PostgreSQL to the customer, maas-setup.md "Database Setup"): package
//     cloudnative-pg from certified-operators, its default channel, laid
//     out as Connectivity Link is: its own Namespace cnpg-system,
//     OperatorGroup cloudnative-pg for all namespaces, Subscription
//     cloudnative-pg (the package's name, as CloudNativePG's own OpenShift
//     tests name it). Not openshift-operators: platform-setup.md (Install
//     Gateway API Controller) warns that "any operator installed into the
//     openshift-operators namespace can pull an OSSM upgrade into its
//     InstallPlan (OLM v0 limitation), re-breaking the gateway". An
//     existing install is found by its CSV, in any namespace.
//
// What this tool installs it records (maasRecord.Operators), and never
// removes: both are shared platform pieces (RHOAI's inference gateway
// authentication, AuthPolicy openshift-ai-inference-authn, is Kuadrant's).

// rhclDocsMinimum is the Connectivity Link version the MaaS docs ask for
// with RHOAI 3.6 (prerequisites.md, "RHOAI 3.6 — use RHCL v1.5": "use RHCL
// v1.5.0+ (OLM channel stable-1.5)").
const rhclDocsMinimum = "1.5"

// rhclAuthBypassVersion is the release prerequisites.md warns about: "RHCL
// v1.4.0 contains a Wasm shim bug that silently bypasses gateway
// authentication. Management endpoints return AUTH_FAILURE; inference
// endpoints work but are unauthenticated. Upgrade to RHCL v1.4.1+."
const rhclAuthBypassVersion = "1.4.0"

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

// maasOperator is an operator MaaS needs and how this tool installs it.
type maasOperator struct {
	title string
	pkg   string
	// subName is the Subscription this tool creates; subNames the names an
	// existing install is found by (and the only ones this tool changes).
	subName   string
	subNames  []string
	namespace string
	source    string
	// group is the OperatorGroup created when the namespace has none; ""
	// when the namespace must have one already (openshift-operators).
	group string
}

var (
	rhclOperator = maasOperator{title: "Red Hat Connectivity Link", pkg: "rhcl-operator", subName: "kuadrant-operator", subNames: []string{"kuadrant-operator", "rhcl-operator"},
		namespace: "kuadrant-system", source: "redhat-operators", group: "kuadrant-operator-group"}
	cnpgOperator = maasOperator{title: "CloudNativePG", pkg: "cloudnative-pg", subName: "cloudnative-pg", subNames: []string{"cloudnative-pg"},
		namespace: "cnpg-system", source: "certified-operators", group: "cloudnative-pg"}
)

// rhclDenyOnFailureEnv is the Subscription env platform-setup.md gives so
// that rate limits are enforced (traffic denied) when Limitador cannot be
// reached: "Set RATELIMIT_CHECK_SERVICE_FAILURE_MODE and
// RATELIMIT_REPORT_SERVICE_FAILURE_MODE to deny on the operator
// Subscription (spec.config.env)".
var rhclDenyOnFailureEnv = []map[string]interface{}{
	{"name": "RATELIMIT_CHECK_SERVICE_FAILURE_MODE", "value": "deny"},
	{"name": "RATELIMIT_REPORT_SERVICE_FAILURE_MODE", "value": "deny"},
}

// rhclMissingDenyEnv names the deny-on-failure variables a Subscription
// does not set to deny.
func rhclMissingDenyEnv(sub *olmSubscription) []string {
	have := map[string]string{}
	env, _ := sub.Spec.Config["env"].([]interface{})
	for _, e := range env {
		m, _ := e.(map[string]interface{})
		name, _ := m["name"].(string)
		value, _ := m["value"].(string)
		have[name] = value
	}
	var out []string
	for _, want := range rhclDenyOnFailureEnv {
		if have[want["name"].(string)] != "deny" {
			out = append(out, want["name"].(string))
		}
	}
	return out
}

// rhclDenyEnvPatch is the Subscription's spec.config.env with the
// deny-on-failure variables set and every other entry kept (a merge patch
// replaces a list whole).
func rhclDenyEnvPatch(sub *olmSubscription) []interface{} {
	var out []interface{}
	set := map[string]bool{}
	env, _ := sub.Spec.Config["env"].([]interface{})
	for _, e := range env {
		m, _ := e.(map[string]interface{})
		name, _ := m["name"].(string)
		for _, want := range rhclDenyOnFailureEnv {
			if want["name"] == name {
				m = want
				set[name] = true
			}
		}
		out = append(out, m)
	}
	for _, want := range rhclDenyOnFailureEnv {
		if !set[want["name"].(string)] {
			out = append(out, want)
		}
	}
	return out
}

// maasSubscriptionPath is the path of an operator Subscription this tool
// reads or changes; only these names, written out so the RBAC rules
// (resourceNames) can be checked against them (rbac_coverage_test.go).
func maasSubscriptionPath(ns, name string) (string, bool) {
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
		Channel             string                 `json:"channel"`
		Name                string                 `json:"name"`
		Source              string                 `json:"source"`
		SourceNamespace     string                 `json:"sourceNamespace"`
		InstallPlanApproval string                 `json:"installPlanApproval"`
		Config              map[string]interface{} `json:"config"`
	} `json:"spec"`
	Status struct {
		State        string         `json:"state"`
		InstalledCSV string         `json:"installedCSV"`
		CurrentCSV   string         `json:"currentCSV"`
		LastUpdated  string         `json:"lastUpdated"`
		Conditions   []k8sCondition `json:"conditions"`
	} `json:"status"`
	argoApp string
}

func readOperatorSubscription(c *Client, ns, name string) (*olmSubscription, error) {
	path, ok := maasSubscriptionPath(ns, name)
	if !ok {
		return nil, nil
	}
	body, _, err := c.get(path)
	if IsK8sError(err, 404) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read Subscription %s/%s: %w", ns, name, err)
	}
	var sub olmSubscription
	if err := json.Unmarshal(body, &sub); err != nil {
		return nil, fmt.Errorf("parse Subscription %s/%s: %w", ns, name, err)
	}
	var obj map[string]interface{}
	_ = json.Unmarshal(body, &obj)
	sub.argoApp = argoApplicationOf(obj, "Subscription")
	return &sub, nil
}

// operatorState is an operator as installed: its CSV (the newest when OLM
// is replacing one) and the Subscription found by the documented names.
type operatorState struct {
	csv, csvNS, version, phase string
	deployments                []string
	sub                        *olmSubscription
}

func (s operatorState) installed() bool { return s.csv != "" }

// csvVersion is the version in a CSV name ("rhcl-operator.v1.5.0" ->
// "1.5.0"), or "".
func csvVersion(name string) string {
	if i := strings.Index(name, ".v"); i >= 0 {
		return name[i+2:]
	}
	return ""
}

// versionCore is a version without its pre-release or build suffix.
func versionCore(v string) string {
	return strings.SplitN(strings.SplitN(v, "-", 2)[0], "+", 2)[0]
}

// readOperator finds op's CSV anywhere on the cluster (OLM's copies
// excluded) and its Subscription: in the CSV's namespace by the documented
// names, or, with no CSV yet, the one this tool creates. A read that fails
// is an error: callers refuse rather than guess.
func readOperator(c *Client, op maasOperator) (operatorState, error) {
	csvs, err := listClusterCSVs(c)
	if err != nil {
		return operatorState{}, err
	}
	return readOperatorIn(c, op, csvs)
}

// readOperatorIn is readOperator with the cluster's CSVs already listed.
func readOperatorIn(c *Client, op maasOperator, csvs []clusterCSV) (operatorState, error) {
	var st operatorState
	for _, csv := range csvs {
		if !containsString(csv.packages(), op.pkg) && csvBaseName(csv.Name) != op.pkg {
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
	return st, nil
}

// --- the catalog -------------------------------------------------------------------

// packageChannel is one channel of a PackageManifest.
type packageChannel struct {
	Name           string `json:"name"`
	CurrentCSV     string `json:"currentCSV"`
	CurrentCSVDesc struct {
		Version string `json:"version"`
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
	body, _, err := c.get(path)
	if err != nil {
		return operatorPackage{}, false, fmt.Errorf("read package %s from catalog %s: %w", pkg, source, err)
	}
	var list struct {
		Items []struct {
			Metadata objectMeta `json:"metadata"`
			Status   struct {
				PackageName            string           `json:"packageName"`
				CatalogSource          string           `json:"catalogSource"`
				CatalogSourceNamespace string           `json:"catalogSourceNamespace"`
				DefaultChannel         string           `json:"defaultChannel"`
				Channels               []packageChannel `json:"channels"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return operatorPackage{}, false, fmt.Errorf("parse package %s: %w", pkg, err)
	}
	for _, it := range list.Items {
		if it.Metadata.Name == pkg && firstNonEmpty(it.Status.PackageName, pkg) == pkg && it.Status.CatalogSource == source && firstNonEmpty(it.Status.CatalogSourceNamespace, CatalogNS) == CatalogNS {
			return operatorPackage{defaultChannel: it.Status.DefaultChannel, channels: it.Status.Channels}, true, nil
		}
	}
	return operatorPackage{}, false, nil
}

// rhclTargetChannel is the channel to install or upgrade Connectivity Link
// on: of the channels whose head meets the docs' minimum, the one with the
// lowest head (the docs name stable-1.5 for RHOAI 3.6, not the newest
// release), the package's default channel first on a tie. "" when no
// channel does.
func rhclTargetChannel(p operatorPackage) string {
	var best *packageChannel
	for i := range p.channels {
		ch := p.channels[i]
		if ch.head() == "" || compareVersions(ch.head(), rhclDocsMinimum) < 0 {
			continue
		}
		if best == nil {
			best = &p.channels[i]
			continue
		}
		switch cmp := compareVersions(ch.head(), best.head()); {
		case cmp < 0, cmp == 0 && ch.Name == p.defaultChannel, cmp == 0 && best.Name != p.defaultChannel && ch.Name < best.Name:
			best = &p.channels[i]
		}
	}
	if best == nil {
		return ""
	}
	return best.Name
}

// hopChannel is the channel OLM can upgrade installed on when a switch to
// target leaves it there: one that lists the installed CSV and whose head
// is newer, the newest such head below target's (seen live 2026-11-18: the
// RHCL catalog's stable-1.5 1.5.0 skips ">=1.4.4 <1.5.0", and 1.4.4 is only
// in stable-1.4, so 1.4.3 goes through stable-1.4 first). "" when none.
func hopChannel(p operatorPackage, installedCSV, target string) string {
	installed := versionCore(csvVersion(installedCSV))
	targetCh, _ := p.channel(target)
	var best *packageChannel
	for i := range p.channels {
		ch := p.channels[i]
		if ch.Name == target || !ch.has(installedCSV) || compareVersions(ch.head(), installed) <= 0 {
			continue
		}
		if targetCh.head() != "" && compareVersions(ch.head(), targetCh.head()) >= 0 {
			continue
		}
		if best == nil || compareVersions(ch.head(), best.head()) > 0 || (compareVersions(ch.head(), best.head()) == 0 && ch.Name < best.Name) {
			best = &p.channels[i]
		}
	}
	if best == nil {
		return ""
	}
	return best.Name
}

// --- installing ----------------------------------------------------------------------

// operatorInstallPlan is what installing op takes, as checked before any
// write: the channel, and whether its namespace and OperatorGroup must be
// made.
type operatorInstallPlan struct {
	op           maasOperator
	channel      string
	head         string
	nsMissing    bool
	groupMissing bool
}

// planOperatorInstall checks that op can be installed as the docs install
// it. A refusal is a message and an error code.
func planOperatorInstall(c *Client, op maasOperator) (*operatorInstallPlan, string, string) {
	plan := &operatorInstallPlan{op: op}
	pkg, found, err := readOperatorPackage(c, op.pkg, op.source)
	switch {
	case err != nil:
		return nil, fmt.Sprintf("Cannot read the %s package from catalog %s, so this tool cannot tell which channel to install; nothing was changed: %v.%s", op.title, op.source, err, templateHint(err)), firstNonEmpty(errorCodeFromK8sErr(err), "internal")
	case !found:
		return nil, fmt.Sprintf("Catalog %s in %s does not offer package %s, so %s cannot be installed from it. A cluster-admin checks the catalog (oc get packagemanifests -n %s -l catalog=%s). Nothing was changed.", op.source, CatalogNS, op.pkg, op.title, CatalogNS, op.source), "prerequisites"
	}
	if op.pkg == rhclOperator.pkg {
		plan.channel = rhclTargetChannel(pkg)
		if plan.channel == "" {
			return nil, fmt.Sprintf("No channel of %s in %s has %s or later, which the MaaS docs ask for with RHOAI 3.6. Nothing was changed.", op.pkg, op.source, rhclDocsMinimum), "prerequisites"
		}
	} else {
		plan.channel = pkg.defaultChannel
		if _, ok := pkg.channel(plan.channel); !ok {
			return nil, fmt.Sprintf("Package %s in %s names no usable default channel. Nothing was changed.", op.pkg, op.source), "prerequisites"
		}
	}
	ch, _ := pkg.channel(plan.channel)
	plan.head = ch.head()

	_, nsFound, err := readObjectMeta(c, "/api/v1/namespaces/"+op.namespace)
	if err != nil {
		return nil, fmt.Sprintf("Cannot read namespace %s; nothing was changed: %v.%s", op.namespace, err, templateHint(err)), firstNonEmpty(errorCodeFromK8sErr(err), "internal")
	}
	plan.nsMissing = !nsFound
	if nsFound {
		groups, err := readOperatorGroups(c, op.namespace)
		if err != nil {
			return nil, fmt.Sprintf("Cannot read the OperatorGroups in %s; nothing was changed: %v.%s", op.namespace, err, templateHint(err)), firstNonEmpty(errorCodeFromK8sErr(err), "internal")
		}
		switch {
		case len(groups) > 1:
			return nil, fmt.Sprintf("Namespace %s has %d OperatorGroups (%s), and OLM installs nothing in a namespace with more than one. A cluster-admin removes the extra ones. Nothing was changed.", op.namespace, len(groups), strings.Join(groups.names(), ", ")), "prerequisites"
		case len(groups) == 1 && !groups[0].allNamespaces():
			return nil, fmt.Sprintf("OperatorGroup %s/%s targets only some namespaces, and %s is installed for all namespaces (as the MaaS docs install it). A cluster-admin installs it elsewhere or changes the OperatorGroup. Nothing was changed.", op.namespace, groups[0].Metadata.Name, op.title), "prerequisites"
		case len(groups) == 0 && op.group == "":
			return nil, fmt.Sprintf("Namespace %s has no OperatorGroup, which OpenShift makes for it (global-operators). A cluster-admin restores it. Nothing was changed.", op.namespace), "prerequisites"
		}
		plan.groupMissing = len(groups) == 0
	} else {
		if op.group == "" {
			return nil, fmt.Sprintf("Namespace %s does not exist; OpenShift makes it for operators of all namespaces. Nothing was changed.", op.namespace), "prerequisites"
		}
		plan.groupMissing = true
	}
	return plan, "", ""
}

// allNamespaces: the group targets every namespace (no target list, no
// selector), as OpenShift's global-operators and the docs' group do.
func (og operatorGroup) allNamespaces() bool {
	return len(og.Spec.TargetNamespaces) == 0 && len(og.Spec.Selector) == 0
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
	body, _, err := c.get(namespacedPath("operators.coreos.com/v1", "operatorgroups", ns, ""))
	if err != nil {
		return nil, err
	}
	var list struct {
		Items operatorGroups `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// operatorObjects are the docs' objects for installing op on channel.
func operatorObjects(op maasOperator, channel string) (ns, group, sub map[string]interface{}) {
	ns = map[string]interface{}{"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]interface{}{"name": op.namespace, "labels": toolLabels()}}
	group = map[string]interface{}{"apiVersion": "operators.coreos.com/v1", "kind": "OperatorGroup",
		"metadata": map[string]interface{}{"name": op.group, "namespace": op.namespace, "labels": toolLabels()}}
	sub = map[string]interface{}{
		"apiVersion": "operators.coreos.com/v1alpha1",
		"kind":       "Subscription",
		"metadata":   map[string]interface{}{"name": op.subName, "namespace": op.namespace, "labels": toolLabels()},
		"spec": map[string]interface{}{
			"channel": channel, "installPlanApproval": "Automatic", "name": op.pkg,
			"source": op.source, "sourceNamespace": CatalogNS,
		},
	}
	if op.pkg == rhclOperator.pkg {
		env := make([]interface{}, len(rhclDenyOnFailureEnv))
		for i, e := range rhclDenyOnFailureEnv {
			env[i] = e
		}
		sub["spec"].(map[string]interface{})["config"] = map[string]interface{}{"env": env}
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
		if _, _, err := c.do(http.MethodPost, withToolFieldManager("/api/v1/namespaces"), "application/json", data, query); err != nil {
			return made, fmt.Errorf("create Namespace %s: %w", plan.op.namespace, err)
		}
		made = append(made, "Namespace "+plan.op.namespace)
		if dryRun {
			return made, nil
		}
	}
	if plan.groupMissing {
		data, _ := json.Marshal(group)
		if _, _, err := c.do(http.MethodPost, withToolFieldManager(namespacedPath("operators.coreos.com/v1", "operatorgroups", plan.op.namespace, "")), "application/json", data, query); err != nil {
			return made, fmt.Errorf("create OperatorGroup %s/%s: %w", plan.op.namespace, plan.op.group, err)
		}
		made = append(made, "OperatorGroup "+plan.op.namespace+"/"+plan.op.group)
	}
	data, _ := json.Marshal(sub)
	if _, _, err := c.do(http.MethodPost, withToolFieldManager(namespacedPath("operators.coreos.com/v1alpha1", "subscriptions", plan.op.namespace, "")), "application/json", data, query); err != nil {
		return made, fmt.Errorf("create Subscription %s/%s: %w", plan.op.namespace, plan.op.subName, err)
	}
	made = append(made, "Subscription "+plan.op.namespace+"/"+plan.op.subName)
	return made, nil
}

// csvPhase reads a CSV's phase and message ("" when it does not exist yet).
func csvPhase(c *Client, ns, name string) (string, string, error) {
	body, _, err := c.get(namespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", ns, name))
	if IsK8sError(err, 404) {
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	var csv struct {
		Status struct {
			Phase   string `json:"phase"`
			Message string `json:"message"`
		} `json:"status"`
	}
	if err := json.Unmarshal(body, &csv); err != nil {
		return "", "", err
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
		p.failed = "OLM cannot resolve it: " + truncateMessage(firstNonEmpty(cond.Message, cond.Reason))
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
		p.failed = fmt.Sprintf("CSV %s failed: %s", p.installedCSV, truncateMessage(firstNonEmpty(msg, "no message")))
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
				failedSince = clk.Now()
			}
			return clk.Since(failedSince) >= MaaSOperatorResolveGrace, nil
		}
		failedSince = time.Time{}
		return false, nil
	})
	return last, err
}

// patchSubscription sets fields of a Subscription this tool may change (a
// merge patch under the tool's field manager; a server-side dry run with
// dryRun).
func patchSubscription(c *Client, sub *olmSubscription, spec map[string]interface{}, dryRun bool) error {
	path, ok := maasSubscriptionPath(sub.Metadata.Namespace, sub.Metadata.Name)
	if !ok {
		return fmt.Errorf("subscription %s/%s is not one this tool changes", sub.Metadata.Namespace, sub.Metadata.Name)
	}
	data, _ := json.Marshal(map[string]interface{}{"metadata": map[string]interface{}{"resourceVersion": sub.Metadata.ResourceVersion}, "spec": spec})
	query := url.Values{"fieldManager": {toolFieldManager}}
	if dryRun {
		query.Set("dryRun", "All")
	}
	body, _, err := c.do(http.MethodPatch, path, "application/merge-patch+json", data, query)
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

// --- memory of the Kuadrant operator -------------------------------------------------

// rhclMemoryLimit is the memory limit that stopped the Kuadrant operator's
// OOM kills on RHCL 1.5.0 (seen live 2026-11-18: its CSV's 500Mi limit was
// OOMKilled three times, 1Gi held; Red Hat known issue CONNLINK-1878). It is
// set only after a container is seen OOMKilled, and only on request.
const rhclMemoryLimit = "1Gi"

// operatorContainer is a container of a CSV's Deployments.
type operatorContainer struct {
	deployment, name string
	resources        map[string]map[string]string
}

// csvContainers reads the containers of a CSV's Deployments (its install
// strategy), which OLM gives a Subscription's spec.config.resources to.
func csvContainers(c *Client, ns, name string) ([]operatorContainer, error) {
	body, _, err := c.get(namespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", ns, name))
	if err != nil {
		return nil, err
	}
	var csv struct {
		Spec struct {
			Install struct {
				Spec struct {
					Deployments []struct {
						Name string `json:"name"`
						Spec struct {
							Template struct {
								Spec struct {
									Containers []struct {
										Name      string                       `json:"name"`
										Resources map[string]map[string]string `json:"resources"`
									} `json:"containers"`
								} `json:"spec"`
							} `json:"template"`
						} `json:"spec"`
					} `json:"deployments"`
				} `json:"spec"`
			} `json:"install"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(body, &csv); err != nil {
		return nil, err
	}
	var out []operatorContainer
	for _, d := range csv.Spec.Install.Spec.Deployments {
		for _, ct := range d.Spec.Template.Spec.Containers {
			out = append(out, operatorContainer{deployment: d.Name, name: ct.Name, resources: ct.Resources})
		}
	}
	return out, nil
}

// oomKilledContainers names the containers of the operator's pods (pods of
// its CSV's Deployments in its namespace) whose last or current
// termination was OOMKilled.
func oomKilledContainers(c *Client, st operatorState) ([]string, error) {
	if !st.installed() || len(st.deployments) == 0 {
		return nil, nil
	}
	body, _, err := c.get(namespacedPath("v1", "pods", st.csvNS, ""))
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []struct {
			Metadata objectMeta `json:"metadata"`
			Status   struct {
				ContainerStatuses []struct {
					Name         string `json:"name"`
					RestartCount int    `json:"restartCount"`
					State        struct {
						Terminated *struct {
							Reason string `json:"reason"`
						} `json:"terminated"`
					} `json:"state"`
					LastState struct {
						Terminated *struct {
							Reason string `json:"reason"`
						} `json:"terminated"`
					} `json:"lastState"`
				} `json:"containerStatuses"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, err
	}
	var out []string
	for _, p := range list.Items {
		ofOperator := false
		for _, d := range st.deployments {
			ofOperator = ofOperator || strings.HasPrefix(p.Metadata.Name, d+"-")
		}
		if !ofOperator {
			continue
		}
		for _, cs := range p.Status.ContainerStatuses {
			oom := (cs.LastState.Terminated != nil && cs.LastState.Terminated.Reason == "OOMKilled") ||
				(cs.State.Terminated != nil && cs.State.Terminated.Reason == "OOMKilled")
			if oom {
				out = append(out, fmt.Sprintf("%s/%s (%d %s)", p.Metadata.Name, cs.Name, cs.RestartCount, verb(cs.RestartCount, "restart", "restarts")))
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// rhclMemoryResources is the spec.config.resources that raises the
// operator's memory limit to rhclMemoryLimit and keeps every other value
// its CSV sets (OLM replaces a container's whole resources with
// spec.config.resources, for every container of the CSV). It returns why
// it is not offered instead: more than one container (the value would
// apply to all), or a limit already at least rhclMemoryLimit.
func rhclMemoryResources(containers []operatorContainer) (map[string]interface{}, string) {
	if len(containers) != 1 {
		return nil, fmt.Sprintf("its CSV runs %d containers, and a Subscription's resources apply to every one of them", len(containers))
	}
	res := containers[0].resources
	limit, _ := parseQuantity(res["limits"]["memory"])
	want, _ := parseQuantity(rhclMemoryLimit)
	if limit >= want {
		return nil, fmt.Sprintf("its memory limit is already %s", res["limits"]["memory"])
	}
	out := map[string]interface{}{}
	for _, kind := range []string{"limits", "requests"} {
		values := map[string]interface{}{}
		for k, v := range res[kind] {
			values[k] = v
		}
		if kind == "limits" {
			values["memory"] = rhclMemoryLimit
		}
		if len(values) > 0 {
			out[kind] = values
		}
	}
	return out, ""
}

// The controller Deployments of Connectivity Link (Kuadrant) and
// CloudNativePG, by the names upstream gives them in OLM and non-OLM installs
// alike (seen live: openshift-operators/kuadrant-operator-controller-manager,
// cnpg-system/cnpg-controller-manager).
const (
	kuadrantController = "kuadrant-operator-controller-manager"
	cnpgController     = "cnpg-controller-manager"
)

// controllerDeployed reports whether a Deployment of that name exists in any
// namespace. A served API alone does not mean an operator runs: OLM leaves an
// operator's CRDs behind when it is uninstalled (seen live 2026-11-18 after
// removing Connectivity Link 1.5.0 and CloudNativePG), and objects made then
// are never reconciled. The field selector narrows the list on a real API
// server; the name is checked here too.
func controllerDeployed(c *Client, name string) (bool, error) {
	body, _, err := c.get("/apis/apps/v1/deployments?fieldSelector=" + url.QueryEscape("metadata.name="+name))
	if err != nil {
		return false, fmt.Errorf("list Deployments named %s: %w", name, err)
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return false, fmt.Errorf("list Deployments named %s: %w", name, err)
	}
	for _, d := range list.Items {
		if d.Metadata.Name == name {
			return true, nil
		}
	}
	return false, nil
}
