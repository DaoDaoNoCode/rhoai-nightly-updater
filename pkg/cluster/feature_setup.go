package cluster

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// One-click prerequisites: what an Environment feature needs on the cluster
// before its own setup runs, set up from its tab after one confirmation
// that lists every change (GET /api/resources/prerequisites, POST
// /api/resources/prerequisites/stream). MaaS sets its own up (maas_setup.go)
// with the same pieces: the operator model (olm_operators.go) and the
// DataScienceCluster component change below.
//
// Per feature (RHOAI 3.6, red-hat-data-services/rhods-operator rhoai-3.6
// api/datasciencecluster/v2 and v3: aipipelines, mlflowoperator, kserve,
// ogx and dashboard under spec.components; v1 calls the pipelines component
// datasciencepipelines):
//   - Pipelines: aipipelines Managed. No operator.
//   - MLflow: mlflowoperator Managed. No operator.
//   - Models: kserve Managed, and the cert-manager Operator for Red Hat
//     OpenShift, which KServe checks for (red-hat-data-services/kserve
//     rhoai-3.6 kserve-module/pkg/kservemodule/dependencies.go, the
//     subscription dependencies behind KserveLLMInferenceServiceDependencies).
//     KServe also checks for Red Hat Connectivity Link; whether a model needs
//     it, and a gateway, depends on how the model is routed, so it is not
//     part of this plan (the MaaS tab installs it). A routing choice adds
//     its operators to featureSetup.operators.
//   - Playground: ogx and dashboard Managed (its models come from the
//     Models and MaaS tabs).
//
// Optional operators (LeaderWorkerSet for Wide EP, JobSet, Kueue, ...) are
// never in a plan. A component this tool turned on is recorded with what it
// was before; Clean up turns it back only while this tool still owns the
// field (the API server gives a field to whoever last set it) and nothing
// on the cluster uses the component. Operators are never removed: other
// features share them.

// certManagerOperator is the cert-manager Operator for Red Hat OpenShift as
// odh-gitops installs it (opendatahub-io/odh-gitops at 72ffa08,
// components/operators/cert-manager: Namespace cert-manager-operator,
// OperatorGroup cert-manager-operator without a spec, Subscription
// openshift-cert-manager-operator from redhat-operators with automatic
// approval). The channel is the package's default (stable-v1 when this was
// written). The operator makes its operand, CertManager/cluster, itself.
// Its CSV declares AWS token auth (a role ARN the console asks for), which
// is optional: RHOAI uses self-signed and CA issuers only, so no role is
// set. An install found elsewhere (the console puts it in
// cert-manager-operator with an own-namespace OperatorGroup named after the
// package) is used as it is. cert-manager.io served with a cert-manager
// Deployment but no CSV is a cert-manager not installed by OLM: KServe does
// not count it, and a second one would fight it, so setup refuses.
var certManagerOperator = operatorSpec{id: "cert-manager", title: "cert-manager Operator for Red Hat OpenShift", pkg: "openshift-cert-manager-operator",
	subName: "openshift-cert-manager-operator", subNames: []string{"openshift-cert-manager-operator"}, namespace: "cert-manager-operator",
	source: "redhat-operators", group: "cert-manager-operator", api: certManagerGroup, controller: "cert-manager"}

const certManagerGroup = "cert-manager.io"

// dashboardComponent is the RHOAI dashboard, whose gen-ai API runs the
// playground.
var dashboardComponent = featureComponent{feature: "The RHOAI dashboard", keys: []string{"dashboard"}}

// featureSetup is what a feature needs on the cluster.
type featureSetup struct {
	id, title  string
	components []featureComponent
	// operators are installed in this order after the components, each
	// when no Subscription of its package exists (KServe counts only
	// those).
	operators []operatorSpec
	// dependency is the DataScienceCluster condition that names missing
	// operators (KserveLLMInferenceServiceDependencies), waited on while it
	// names one of operators ("" none).
	dependency string
	// kuadrant: a Kuadrant CR is made after the operators (Models routed
	// through a gateway: featureSetupFor).
	kuadrant bool
}

// featureSetupFor is the feature's setup for how its model is reached
// (models only): through a gateway ("default-gateway", "gateway"), calls are
// authenticated by Red Hat Connectivity Link, which KServe also requires
// before it starts a model with token authentication
// (router_preconditions_odh.go), so its operator and its Kuadrant CR are
// added, as the MaaS docs install them (the MaaS tab's own steps). "maas"
// needs the MaaS platform (its tab sets it up, Connectivity Link included);
// "internal" and "" need neither.
func featureSetupFor(id, exposure string) featureSetup {
	fs, _ := featureSetupOf(id)
	if id != "models" {
		return fs
	}
	if exposure == exposureDefault || exposure == exposureGateway {
		fs.operators = append(append([]operatorSpec(nil), fs.operators...), rhclOperator)
		fs.kuadrant = true
	}
	return fs
}

var featureSetups = []featureSetup{
	{id: "pipelines", title: "Pipelines", components: []featureComponent{pipelinesComponent}},
	{id: "mlflow", title: "MLflow", components: []featureComponent{mlflowComponent}},
	{id: "models", title: "Models", components: []featureComponent{kserveComponent}, operators: []operatorSpec{certManagerOperator}, dependency: llmDependenciesCondition},
	{id: "playground", title: "Playground", components: []featureComponent{playgroundComponent, dashboardComponent}},
}

func featureSetupOf(id string) (featureSetup, bool) {
	for _, fs := range featureSetups {
		if fs.id == id {
			return fs, true
		}
	}
	return featureSetup{}, false
}

var (
	// PrerequisiteReadyTimeout bounds the wait for a component turned on
	// to report Ready, and for KServe to see the operators installed.
	PrerequisiteReadyTimeout = 10 * time.Minute
)

// --- record (snapshot ConfigMap key "prerequisites") ---------------------------------

const (
	prereqRecordKey     = "prerequisites"
	prereqRecordManager = "rhoai-nightly-updater-prerequisites-record"
)

// prereqRecord is what setup changed that the cluster cannot tell: the
// components it turned on, with their value before, and the operators it
// installed (which stay: other features share them).
type prereqRecord struct {
	Components map[string]prereqComponent `json:"components,omitempty"`
	Shared     map[string]string          `json:"shared,omitempty"`
	At         string                     `json:"at,omitempty"`
	By         string                     `json:"by,omitempty"`
}

type prereqComponent struct {
	Previous string `json:"previous,omitempty"`
	Feature  string `json:"feature"`
}

func readPrereqRecord(c *Client) (prereqRecord, error) {
	var rec prereqRecord
	raw, err := readSnapshotKey(c, prereqRecordKey)
	if err != nil {
		return rec, fmt.Errorf("read this tool's record of the prerequisites it set up: %w", err)
	}
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &rec); err != nil {
			return rec, fmt.Errorf("parse this tool's record of the prerequisites it set up: %w", err)
		}
	}
	return rec, nil
}

func writePrereqRecord(c *Client, rec prereqRecord) error {
	rec.At, rec.By = nowRFC3339(), getUser(c)
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if err := writeSnapshotKey(c, prereqRecordKey, prereqRecordManager, string(data)); err != nil {
		return fmt.Errorf("record the prerequisites this tool set up: %w", err)
	}
	return nil
}

// readSnapshotKey reads one key of this tool's snapshot ConfigMap ("" when
// it or the ConfigMap is missing).
func readSnapshotKey(c *Client, key string) (string, error) {
	body, _, err := c.bookkeeping().get(namespacedPath("v1", "configmaps", getActivityNamespace(), snapshotConfigMapName))
	if IsK8sError(err, 404) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var cm struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(body, &cm); err != nil {
		return "", fmt.Errorf("parse the snapshot ConfigMap: %w", err)
	}
	return cm.Data[key], nil
}

// writeSnapshotKey applies one key of the snapshot ConfigMap as manager:
// server-side apply keeps the keys other managers own.
func writeSnapshotKey(c *Client, key, manager, value string) error {
	ns := getActivityNamespace()
	cm, _ := json.Marshal(map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]interface{}{"name": snapshotConfigMapName, "namespace": ns},
		"data":       map[string]interface{}{key: value},
	})
	query := url.Values{"fieldManager": {manager}, "force": {"true"}}
	_, _, err := c.bookkeeping().do(http.MethodPatch, namespacedPath("v1", "configmaps", ns, snapshotConfigMapName), "application/apply-patch+yaml", cm, query)
	return err
}

// --- DataScienceCluster components ---------------------------------------------------

// dscComponentManager is the field manager of a component's managementState
// this tool sets: the API server then tells whether the value is still the
// tool's (dscComponentOwned). One per component, so turning one back never
// touches the ownership of another.
func dscComponentManager(key string) string {
	return "rhoai-nightly-updater-dsc-" + key
}

// patchDSCComponent sets spec.components.<key>.managementState of the
// DataScienceCluster (a merge patch as the component's field manager; a
// server-side dry run with dryRun). Nothing else of the component changes.
// With a resourceVersion, the API server refuses the patch (409) when the
// DataScienceCluster changed since it was read. It returns the object the
// API server answered with.
func patchDSCComponent(c *Client, key, state, resourceVersion string, dryRun bool) (map[string]interface{}, error) {
	path, err := dataScienceClusterPath(c)
	if err != nil {
		return nil, err
	}
	body := map[string]interface{}{"spec": map[string]interface{}{"components": map[string]interface{}{key: map[string]interface{}{"managementState": state}}}}
	if resourceVersion != "" {
		body["metadata"] = map[string]interface{}{"resourceVersion": resourceVersion}
	}
	patch, _ := json.Marshal(body)
	query := url.Values{"fieldManager": {dscComponentManager(key)}}
	if dryRun {
		query.Set("dryRun", "All")
	}
	answer, _, err := c.do(http.MethodPatch, path, "application/merge-patch+json", patch, query)
	if err != nil {
		return nil, err
	}
	var obj map[string]interface{}
	_ = json.Unmarshal(answer, &obj)
	return obj, nil
}

// dscComponentOwned: this tool's field manager of the component still owns
// its managementState. The API server moves a field to another manager only
// when that manager changes its value (an update to the same value, or an
// apply of it, leaves it owned, or shared): so this tells whether someone
// changed the component since this tool's setup, not whether someone set
// it to Managed again.
func dscComponentOwned(dsc map[string]interface{}, key string) bool {
	raw, _ := json.Marshal(dsc["metadata"])
	var meta objectMeta
	_ = json.Unmarshal(raw, &meta)
	return meta.ownsFieldPath([]string{dscComponentManager(key)}, "f:spec", "f:components", "f:"+key, "f:managementState")
}

// dscComponentCommand is the change from a terminal.
func dscComponentCommand(dsc, key, state string) string {
	return fmt.Sprintf(`oc patch datasciencecluster %s --type=merge -p '{"spec":{"components":{"%s":{"managementState":"%s"}}}}'`, shellQuote(firstNonEmpty(dsc, "default-dsc")), key, state)
}

// componentTitle names a component for people.
func componentTitle(fc featureComponent, key string) string {
	name := strings.TrimPrefix(fc.feature, "The ")
	if strings.Contains(name, "(") {
		return name // "Model serving (KServe)"
	}
	return fmt.Sprintf("%s (%s)", name, key)
}

// --- the plan --------------------------------------------------------------------------

// componentPlan is a component as read, and whether setup turns it on.
type componentPlan struct {
	fc            featureComponent
	key, state    string
	turnOn        bool
	ready, msg    string // its <key>Ready condition
	owned         bool   // this tool's manager owns its managementState
	recorded      bool   // the record says this tool turned it on
	recordedPrior string
}

// operatorPlan is an operator as read, and what setup does about it.
type operatorPlan struct {
	op      operatorSpec
	st      operatorState
	install *operatorInstallPlan
	byTool  bool
}

// featurePlan is everything a feature's prerequisites setup does, decided
// on reads before any write. refusal is set when it cannot run; part
// problems that do not block are in the parts.
type featurePlan struct {
	fs                featureSetup
	rec               prereqRecord
	dsc               dscRead
	dscName, argoApp  string
	components        []componentPlan
	operators         []operatorPlan
	depStatus, depMsg string
	refusal, code     string
	parts             []types.MaaSPart
	plan              []string
}

func (p *featurePlan) refuse(msg, code string) {
	if p.refusal == "" {
		p.refusal, p.code = msg, code
	}
}

// clusterReads are the reads every feature's plan shares.
type clusterReads struct {
	rec    prereqRecord
	recErr error
	dsc    dscRead
	dscErr error
	csvs   []clusterCSV
	subs   []*olmSubscription
	olmErr error
}

func readClusterForPrerequisites(c *Client) clusterReads {
	var r clusterReads
	r.rec, r.recErr = readPrereqRecord(c)
	r.dsc, r.dscErr = readFirstDSC(c)
	r.csvs, r.olmErr = listClusterCSVs(c)
	if r.olmErr == nil {
		r.subs, r.olmErr = listSubscriptions(c, "")
	}
	return r
}

// planFeature reads what fs needs and decides what setup does: only what
// is missing. A read that fails refuses the setup (fail closed).
func planFeature(c *Client, fs featureSetup, r clusterReads) *featurePlan {
	p := &featurePlan{fs: fs, rec: r.rec, dsc: r.dsc}
	part := func(pt types.MaaSPart) { p.parts = append(p.parts, pt) }
	if r.recErr != nil {
		p.refuse(r.recErr.Error()+" Nothing was changed.", errorCodeFromK8sErr(r.recErr))
	}

	// The components.
	switch {
	case r.dscErr != nil:
		msg := fmt.Sprintf("Cannot read the DataScienceCluster; nothing was changed: %v.%s", r.dscErr, templateHint(r.dscErr))
		p.refuse(msg, errorCodeFromK8sErr(r.dscErr))
		for _, fc := range fs.components {
			part(types.MaaSPart{ID: "dsc-" + fc.keys[0], Title: componentTitle(fc, fc.keys[0]), State: "unknown", Message: msg})
		}
	case r.dsc.State == DSCStateNoCRD:
		p.refuse("RHOAI is not installed on this cluster (it has no DataScienceCluster API). A cluster-admin installs it on the Status page. Nothing was changed.", "prerequisites")
	case r.dsc.Object == nil:
		p.refuse("This cluster has no DataScienceCluster. A cluster-admin creates one on the Components page. Nothing was changed.", "prerequisites")
	}
	if r.dscErr == nil && r.dsc.Object != nil {
		meta, _ := r.dsc.Object["metadata"].(map[string]interface{})
		p.dscName, _ = meta["name"].(string)
		p.argoApp = argoApplicationOf(r.dsc.Object, "DataScienceCluster")
		for _, fc := range fs.components {
			key := fc.keyIn(r.dsc)
			state, _ := componentManagement(r.dsc.Object, key)
			cp := componentPlan{fc: fc, key: key, state: state, turnOn: state != "Managed", owned: dscComponentOwned(r.dsc.Object, key)}
			cp.ready, cp.msg = componentReadyCondition(r.dsc.Object, []string{key})
			if rc, ok := r.rec.Components[key]; ok {
				cp.recorded, cp.recordedPrior = true, rc.Previous
			}
			p.components = append(p.components, cp)
			pt := types.MaaSPart{ID: "dsc-" + key, Title: componentTitle(fc, key), ManagedByTool: cp.recorded && cp.owned && !cp.turnOn}
			switch {
			case cp.turnOn && p.argoApp != "":
				pt.State, pt.Message = "manual", fmt.Sprintf("%s in DataScienceCluster %s, which Argo CD application %q manages: set it to Managed in Git.", firstNonEmpty(state, "Not set"), p.dscName, p.argoApp)
				pt.Commands = dscComponentCommand(p.dscName, key, "Managed")
			case cp.turnOn:
				pt.State, pt.Message = "missing", fmt.Sprintf("%s in DataScienceCluster %s. Set up sets it to Managed; Clean up can turn it back.", firstNonEmpty(state, "Not set"), p.dscName)
				pt.Commands = dscComponentCommand(p.dscName, key, "Managed")
				p.plan = append(p.plan, fmt.Sprintf("DataScienceCluster %s: set %s to Managed (it is %s; Clean up can turn it back while nothing else uses it)", p.dscName, key, firstNonEmpty(state, "not set")))
			case cp.ready == "False":
				pt.State, pt.Message = "waiting", "Managed; RHOAI's operator reports it not ready: "+truncateText(firstNonEmpty(cp.msg, "no message"), 300)
			default:
				pt.State, pt.Message = "ready", "Managed in DataScienceCluster "+p.dscName
				if pt.ManagedByTool {
					pt.Message += " (turned on by this tool)"
				}
			}
			pt.NotManaged = !pt.ManagedByTool && !cp.turnOn
			part(pt)
		}
		var turnOn []string
		for _, cp := range p.components {
			if cp.turnOn {
				turnOn = append(turnOn, cp.key)
			}
		}
		if len(turnOn) > 0 && p.argoApp != "" {
			p.refuse(gitOpsRefusal("The DataScienceCluster", p.argoApp, "turning on "+strings.Join(turnOn, " and ")), ErrorCodeGitOpsManaged)
		}
		if fs.dependency != "" {
			p.depStatus, p.depMsg = dscConditionStatus(r.dsc.Object, fs.dependency)
		}
	}

	// The operators, in order.
	for _, op := range fs.operators {
		p.operators = append(p.operators, planFeatureOperator(c, p, op, r))
	}

	// The Kuadrant CR, once Connectivity Link runs.
	if fs.kuadrant {
		part(planKuadrantPart(c, p))
	}

	// What KServe still reports missing that this plan does not install
	// (Connectivity Link, which depends on how models are routed).
	if fs.dependency != "" {
		part(dependencyPart(p))
	}
	return p
}

// planKuadrantPart reads the Kuadrant CR; a missing one is created by Set
// up in Connectivity Link's namespace.
func planKuadrantPart(c *Client, p *featurePlan) types.MaaSPart {
	pt := types.MaaSPart{ID: "kuadrant", Title: "Connectivity Link instance (Kuadrant)"}
	k, err := readKuadrant(c)
	switch {
	case err != nil:
		msg := fmt.Sprintf("Cannot read the Kuadrant CR; nothing was changed: %v.%s", err, templateHint(err))
		p.refuse(msg, errorCodeFromK8sErr(err))
		pt.State, pt.Message = "unknown", msg
	case k.found && k.ready:
		pt.State, pt.Message, pt.NotManaged = "ready", k.namespace+"/kuadrant is ready", !toolOwned(k.meta)
		pt.ManagedByTool = toolOwned(k.meta)
	case k.found:
		pt.State, pt.Message = "waiting", "Kuadrant "+k.namespace+"/kuadrant is not Ready yet: "+truncateText(firstNonEmpty(k.message, "no message"), 300)
	default:
		pt.State, pt.Message = "missing", "Not created yet. Set up creates it once Connectivity Link serves its API; it starts Authorino, which checks each call's token."
		pt.Commands = kuadrantCommands(rhclOperator.namespace)
		p.plan = append(p.plan, fmt.Sprintf("Create the Kuadrant CR %s/kuadrant (as the MaaS docs do); it stays after Clean up, as other features share it", rhclOperator.namespace))
	}
	return pt
}

// planFeatureOperator reads op and decides what setup does about it.
func planFeatureOperator(c *Client, p *featurePlan, op operatorSpec, r clusterReads) operatorPlan {
	pt := types.MaaSPart{ID: op.id, Title: op.title}
	defer func() { p.parts = append(p.parts, pt) }()
	if r.olmErr != nil {
		msg := fmt.Sprintf("Cannot read the installed operators, so this tool cannot tell whether %s is installed; nothing was changed: %v.%s", op.title, r.olmErr, templateHint(r.olmErr))
		p.refuse(msg, errorCodeFromK8sErr(r.olmErr))
		pt.State, pt.Message = "unknown", msg
		return operatorPlan{op: op}
	}
	st, err := readOperatorIn(c, op, r.csvs, r.subs)
	if err != nil {
		msg := fmt.Sprintf("Cannot read %s's Subscription; nothing was changed: %v.%s", op.title, err, templateHint(err))
		p.refuse(msg, errorCodeFromK8sErr(err))
		pt.State, pt.Message = "unknown", msg
		return operatorPlan{op: op}
	}
	opp := operatorPlan{op: op, st: st}
	_, shared := p.rec.Shared[op.id]
	sub := st.requested
	opp.byTool = shared && sub != nil && toolOwned(sub.Metadata)
	pt.ManagedByTool, pt.NotManaged = opp.byTool, !opp.byTool
	where := ""
	if sub != nil {
		where = fmt.Sprintf(" (Subscription %s/%s, channel %s)", sub.Metadata.Namespace, sub.Metadata.Name, firstNonEmpty(sub.Spec.Channel, "not set"))
	}
	switch {
	case st.installed() && st.phase == "Succeeded":
		pt.State, pt.Message = "ready", fmt.Sprintf("Version %s runs in %s%s", st.version, st.csvNS, where)
		if sub == nil {
			// KServe counts a Subscription of the package, not a CSV.
			pt.State, pt.Message = "problem", fmt.Sprintf("Version %s runs in %s, but no Subscription of package %s exists, which is what RHOAI's KServe checks for. A cluster-admin recreates its Subscription.", st.version, st.csvNS, op.pkg)
		}
	case st.installed():
		pt.State, pt.Message = "waiting", fmt.Sprintf("Version %s is being installed in %s%s (CSV %s)", st.version, st.csvNS, where, firstNonEmpty(st.phase, "pending"))
	case st.pending() != nil:
		pt.State, pt.Message = "waiting", fmt.Sprintf("Subscription %s/%s exists; OLM is installing it (%s). Set up waits for it.", sub.Metadata.Namespace, sub.Metadata.Name, firstNonEmpty(sub.Status.State, "no state yet"))
	default:
		// Its API served by a running controller without a CSV: someone
		// else's install, which KServe does not count and a second one
		// would fight (a served API alone is CRDs left by an uninstall).
		if op.api != "" {
			served, err := operatorAPIServed(c, op)
			if err == nil && served && op.controller != "" {
				served, err = controllerDeployed(c, op.controller)
			}
			if err != nil {
				msg := fmt.Sprintf("Cannot tell whether %s runs; nothing was changed: %v.%s", op.title, err, templateHint(err))
				p.refuse(msg, errorCodeFromK8sErr(err))
				pt.State, pt.Message = "unknown", msg
				return opp
			}
			if served {
				msg := fmt.Sprintf("%s runs on this cluster (its API %s and Deployment %s exist) without being installed from OperatorHub, so RHOAI's KServe does not count it, and installing %s next to it would run two. A cluster-admin replaces it with %s from OperatorHub. Nothing was changed.", strings.TrimSuffix(op.api, ".io"), op.api, op.controller, op.title, op.title)
				p.refuse(msg, "prerequisites")
				pt.State, pt.Message, pt.NotManaged = "manual", msg, true
				return opp
			}
		}
		plan, msg, code := planOperatorInstall(c, op)
		if msg != "" {
			p.refuse(msg, code)
			pt.State, pt.Message = "problem", "Not installed, and Set up cannot install it: "+msg
			return opp
		}
		opp.install = plan
		pt.ManagedByTool, pt.NotManaged = false, false
		pt.State, pt.Message = "missing", fmt.Sprintf("Not installed. Set up installs version %s from %s (channel %s).", plan.head, op.source, plan.channel)
		pt.Commands = operatorInstallCommands(plan)
		reuse := ""
		if plan.reusedGroup != "" {
			reuse = fmt.Sprintf(", using OperatorGroup %s/%s as it is", op.namespace, plan.reusedGroup)
		}
		p.plan = append(p.plan, fmt.Sprintf("Install %s %s: %sSubscription %s/%s (package %s, channel %s, from %s, automatic updates%s); it stays after Clean up, as other features share it", op.title, plan.head, operatorPlanPrefix(plan), op.namespace, op.subName, op.pkg, plan.channel, op.source, reuse))
	}
	return opp
}

// operatorAPIServed says whether op's API group is served (each group
// named, so the RBAC coverage test resolves the discovery path).
func operatorAPIServed(c *Client, op operatorSpec) (bool, error) {
	switch op.api {
	case certManagerGroup:
		return groupServed(c, certManagerGroup)
	case kuadrantGroup:
		return groupServed(c, kuadrantGroup)
	case cnpgGroup:
		return groupServed(c, cnpgGroup)
	}
	return false, fmt.Errorf("this tool does not know the API of %s", op.title)
}

// dependencyPart is KServe's own view of its operators.
func dependencyPart(p *featurePlan) types.MaaSPart {
	pt := types.MaaSPart{ID: "dependencies", Title: "KServe's dependency check"}
	missing := parseMissingDependencies(p.depMsg)
	var others []string
	for _, m := range missing {
		if !p.installs(m) {
			others = append(others, m)
		}
	}
	switch {
	case p.depStatus == "True":
		pt.State, pt.Message = "ready", "KServe finds every operator it needs"
	case p.depStatus == "" && p.dscName != "":
		pt.State, pt.Message = "ready", "Not reported by this RHOAI version; nothing to wait for"
	case p.depStatus == "":
		pt.State, pt.Message = "unknown", "Not read"
	case len(others) > 0:
		pt.State = "manual"
		pt.Message = fmt.Sprintf("KServe also reports missing: %s. Whether a model needs it depends on how it is routed, so Set up here does not install it: the MaaS tab's Set up installs Red Hat Connectivity Link. Until then, LLMInferenceServices are refused.", strings.Join(others, ", "))
	case len(missing) > 0:
		names := strings.Join(missing, " and ")
		pt.State, pt.Message = "waiting", fmt.Sprintf("Waits for %s: KServe reports %s. Set up installs %s, then waits until KServe sees %s.", names, verb(len(missing), "it isn't installed", "they aren't installed"), verb(len(missing), "it", "them"), verb(len(missing), "it", "them"))
	default:
		pt.State, pt.Message = "waiting", "KServe reports: "+truncateText(firstNonEmpty(p.depMsg, "not met"), 300)+". Set up installs what it names, then waits until KServe sees it."
	}
	return pt
}

// installs: the plan installs (or has) the operator a condition names.
func (p *featurePlan) installs(mention string) bool {
	key := normalizeOperatorName(mention)
	for _, o := range p.operators {
		for _, n := range []string{o.op.pkg, o.op.title, o.op.id} {
			if normalizeOperatorName(n) == key {
				return true
			}
		}
	}
	return false
}

// operatorInstallCommands are the plan's objects from a terminal.
func operatorInstallCommands(p *operatorInstallPlan) string {
	ns, group, sub := operatorObjects(p.op, p.channel)
	var objs []map[string]interface{}
	if p.nsMissing {
		objs = append(objs, ns)
	}
	if p.groupMissing {
		objs = append(objs, group)
	}
	objs = append(objs, sub)
	var docs []string
	for _, o := range objs {
		meta := o["metadata"].(map[string]interface{})
		delete(meta, "labels")
		docs = append(docs, yamlish(o, ""))
	}
	return "oc apply -f - <<'EOF'\n" + strings.Join(docs, "---\n") + "EOF\n" +
		fmt.Sprintf("oc wait --for=jsonpath='{.status.state}'=AtLatestKnown subscription/%s -n %s --timeout=300s", p.op.subName, p.op.namespace)
}

// yamlish writes the plain maps, lists and strings of an object as YAML
// (keys in a fixed order: apiVersion, kind, metadata, spec, then the rest).
func yamlish(v interface{}, indent string) string {
	var b strings.Builder
	switch t := v.(type) {
	case map[string]interface{}:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		order := map[string]int{"apiVersion": 0, "kind": 1, "metadata": 2, "spec": 3}
		sort.Slice(keys, func(i, j int) bool {
			oi, iok := order[keys[i]]
			oj, jok := order[keys[j]]
			switch {
			case iok && jok:
				return oi < oj
			case iok != jok:
				return iok
			}
			return keys[i] < keys[j]
		})
		for _, k := range keys {
			switch child := t[k].(type) {
			case map[string]interface{}, []interface{}:
				b.WriteString(indent + k + ":\n" + yamlish(child, indent+"  "))
			default:
				fmt.Fprintf(&b, "%s%s: %v\n", indent, k, child)
			}
		}
	case []interface{}:
		for _, e := range t {
			inner := yamlish(e, indent+"  ")
			b.WriteString(indent + "- " + strings.TrimPrefix(inner, indent+"  "))
		}
	default:
		fmt.Fprintf(&b, "%s%v\n", indent, t)
	}
	return b.String()
}

// status is the plan as the tab shows it.
func (p *featurePlan) status() types.FeaturePrerequisites {
	out := types.FeaturePrerequisites{Parts: p.parts, SetupPlan: p.plan, Ready: true}
	for _, pt := range p.parts {
		// What KServe still lacks that this plan does not set up belongs
		// to a routing choice (dependencyPart): it does not make the
		// feature's own prerequisites unready.
		if pt.State != "ready" && (pt.ID != "dependencies" || pt.State != "manual") {
			out.Ready = false
		}
	}
	if p.refusal != "" {
		out.SetupBlockedReason = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(p.refusal), "Nothing was changed."))
	}
	for _, cp := range p.components {
		if cp.recorded && cp.owned && cp.state == "Managed" {
			out.Restorable = append(out.Restorable, types.RestorableComponent{Component: cp.key, Previous: cp.recordedPrior, Feature: p.fs.id})
		}
	}
	return out
}

// GetPrerequisitesStatusFor reads what every feature needs on the cluster,
// with Models planned for how its model is reached (featureSetupFor; "" is
// the default).
func GetPrerequisitesStatusFor(c *Client, modelsExposure string) (*types.PrerequisitesStatus, error) {
	r := readClusterForPrerequisites(c)
	out := &types.PrerequisitesStatus{Features: map[string]types.FeaturePrerequisites{}, ModelsExposure: modelsExposure}
	for _, base := range featureSetups {
		fs := featureSetupFor(base.id, modelsExposure)
		out.Features[fs.id] = planFeature(c, fs, r).status()
	}
	return out, nil
}

// --- the setup ---------------------------------------------------------------------------

// ValidatePrerequisitesRequest returns why req cannot run, or "".
func ValidatePrerequisitesRequest(req types.PrerequisitesRequest) string {
	if _, ok := featureSetupOf(req.Feature); !ok {
		return "feature must be pipelines, mlflow, models or playground"
	}
	if req.Exposure != "" && (req.Feature != "models" || !ValidExposure(req.Exposure)) {
		return "exposure is how a model is reached (models only): default-gateway, maas, gateway or internal"
	}
	return ""
}

// RunPrerequisites sets up what a feature needs on the cluster, as one
// operation with streamed steps: "prerequisites" (every read and a
// server-side dry run of every write, before any change), "dsc" (the
// components), one step per operator ("cert-manager"), and "ready" (the
// components report Ready, and KServe sees the operators). Only what is
// missing is done; running it again continues where a failed run stopped.
func RunPrerequisites(c *Client, req types.PrerequisitesRequest, onStep func(UpdateStepEvent)) (*types.OperationResponse, error) {
	m := &maasRun{c: c, emit: onStep, activity: "setup-prerequisites", target: req.Feature + " prerequisites"}
	if problem := ValidatePrerequisitesRequest(req); problem != "" {
		return m.fail("prerequisites", problem, "validation", "invalid request")
	}
	fs := featureSetupFor(req.Feature, req.Exposure)
	m.target = fs.title + " prerequisites"
	m.step("prerequisites", "running", fmt.Sprintf("Checking what %s needs on the cluster...", fs.title))
	p := planFeature(c, fs, readClusterForPrerequisites(c))
	if p.refusal != "" {
		return m.fail("prerequisites", p.refusal, firstNonEmpty(p.code, "prerequisites"), "check failed")
	}
	// Every write as a server-side dry run first.
	for _, cp := range p.components {
		if !cp.turnOn {
			continue
		}
		if _, err := patchDSCComponent(c, cp.key, "Managed", "", true); err != nil {
			return m.fail("prerequisites", fmt.Sprintf("The API server rejects setting %s to Managed in the DataScienceCluster: %v. Nothing was changed.%s", cp.key, err, templateHint(err)), maasWriteCode(err), "dry run failed")
		}
	}
	for _, o := range p.operators {
		if o.install == nil {
			continue
		}
		if _, err := createOperatorObjects(c, o.install, true); err != nil {
			return m.fail("prerequisites", fmt.Sprintf("The API server rejects %s's install: %v. Nothing was changed.%s", o.op.title, err, templateHint(err)), maasWriteCode(err), "dry run failed")
		}
	}
	m.step("prerequisites", "success", "Everything Set up changes is checked.")

	// 1. The components.
	var on []string
	for _, cp := range p.components {
		if cp.turnOn {
			on = append(on, cp.key)
		}
	}
	if len(on) == 0 {
		m.step("dsc", "success", fmt.Sprintf("%s %s Managed in DataScienceCluster %s.", joinTitles(componentKeys(p.components)), verb(len(p.components), "is", "are"), p.dscName))
	} else {
		rec := p.rec
		if rec.Components == nil {
			rec.Components = map[string]prereqComponent{}
		}
		for _, cp := range p.components {
			if cp.turnOn {
				rec.Components[cp.key] = prereqComponent{Previous: cp.state, Feature: fs.id}
			}
		}
		m.step("dsc", "running", fmt.Sprintf("Setting %s to Managed in DataScienceCluster %s (Clean up can turn %s back)...", joinTitles(on), p.dscName, verb(len(on), "it", "them")))
		if err := writePrereqRecord(c, rec); err != nil {
			return m.fail("dsc", err.Error()+" The DataScienceCluster was not changed.", errorCodeFromK8sErr(err), "record write failed")
		}
		p.rec = rec
		for _, cp := range p.components {
			if !cp.turnOn {
				continue
			}
			if _, err := patchDSCComponent(c, cp.key, "Managed", "", false); err != nil {
				return m.fail("dsc", fmt.Sprintf("Failed to set %s to Managed: %v.%s", cp.key, err, templateHint(err)), maasWriteCode(err), "dsc patch failed")
			}
			m.logs = append(m.logs, fmt.Sprintf("OK: %s Managed (was %s)", cp.key, firstNonEmpty(cp.state, "not set")))
		}
		m.step("dsc", "success", fmt.Sprintf("%s %s Managed; RHOAI's operator deploys %s.", joinTitles(on), verb(len(on), "is", "are"), verb(len(on), "it", "them")))
	}

	// 2. The operators.
	for _, o := range p.operators {
		if r, err := m.ensureOperator(&p.rec, o); r != nil || err != nil {
			return r, err
		}
	}
	if fs.kuadrant {
		if r, err := m.ensureKuadrant(&p.rec); r != nil || err != nil {
			return r, err
		}
	}

	// 3. Ready.
	return m.waitPrerequisitesReady(p)
}

func componentKeys(cps []componentPlan) []string {
	var out []string
	for _, cp := range cps {
		out = append(out, cp.key)
	}
	return out
}

// ensureOperator installs o when it is missing, or waits for the install
// OLM is doing, on the operator's own step.
func (m *maasRun) ensureOperator(rec *prereqRecord, o operatorPlan) (*types.OperationResponse, error) {
	op := o.op
	switch {
	case o.install != nil:
		m.step(op.id, "running", fmt.Sprintf("Installing %s %s (channel %s) from %s...", op.title, o.install.head, o.install.channel, op.source))
		if rec.Shared == nil {
			rec.Shared = map[string]string{}
		}
		rec.Shared[op.id] = fmt.Sprintf("Subscription %s/%s, channel %s", op.namespace, op.subName, o.install.channel)
		if err := writePrereqRecord(m.c, *rec); err != nil {
			return m.fail(op.id, err.Error()+" Nothing was installed.", errorCodeFromK8sErr(err), "record write failed")
		}
		made, err := createOperatorObjects(m.c, o.install, false)
		for _, d := range made {
			m.logs = append(m.logs, "OK: "+d+" created")
		}
		if err != nil {
			return m.fail(op.id, fmt.Sprintf("Failed to %v. Set up again to continue.", err), maasWriteCode(err), "operator install failed")
		}
		return m.waitOperator(op.id, op.namespace, op.subName, op.title)
	case o.st.installed() && o.st.phase == "Succeeded":
		m.step(op.id, "success", fmt.Sprintf("%s %s is installed%s.", op.title, o.st.version, byWhom(o.byTool)))
	case o.st.pending() != nil || o.st.installed():
		sub := o.st.requested
		if sub == nil {
			m.step(op.id, "success", fmt.Sprintf("%s %s is being installed in %s.", op.title, o.st.version, o.st.csvNS))
			return nil, nil
		}
		return m.waitOperator(op.id, sub.Metadata.Namespace, sub.Metadata.Name, op.title)
	}
	return nil, nil
}

// ensureKuadrant makes the Kuadrant CR when there is none, once
// Connectivity Link serves its API (step "kuadrant"), in the namespace
// Connectivity Link runs in, as the MaaS setup does. It stays after Clean
// up: Connectivity Link's policies are shared.
func (m *maasRun) ensureKuadrant(rec *prereqRecord) (*types.OperationResponse, error) {
	c := m.c
	k, err := readKuadrant(c)
	if err != nil {
		return m.fail("kuadrant", fmt.Sprintf("Cannot read the Kuadrant CR: %v.%s", err, templateHint(err)), errorCodeFromK8sErr(err), "kuadrant read failed")
	}
	if k.found {
		m.step("kuadrant", "success", fmt.Sprintf("Kuadrant %s/kuadrant exists%s.", k.namespace, byWhom(toolOwned(k.meta))))
		return nil, nil
	}
	rhcl, err := readOperator(c, rhclOperator)
	if err != nil {
		return m.fail("kuadrant", fmt.Sprintf("Cannot read the installed operators: %v.", err), errorCodeFromK8sErr(err), "csv read failed")
	}
	ns := firstNonEmpty(rhcl.csvNS, rhclOperator.namespace)
	m.step("kuadrant", "running", "Waiting for Connectivity Link's Kuadrant API...")
	version := ""
	if _, err := waitForGone(c, MaaSOperatorTimeout, MaaSPoll, func(cc *Client) (bool, error) {
		v, err := kuadrantServedVersion(cc)
		version = v
		return v != "", err
	}); err != nil || version == "" {
		return m.fail("kuadrant", fmt.Sprintf("Connectivity Link's Kuadrant API is not served after %s%s. Set up again to continue.", clockText(MaaSOperatorTimeout), errSuffix(err)), firstNonEmpty(errorCodeFromK8sErr(err), "not_ready"), "kuadrant api missing")
	}
	if rec.Shared == nil {
		rec.Shared = map[string]string{}
	}
	rec.Shared["kuadrant"] = "Kuadrant " + ns + "/kuadrant"
	if err := writePrereqRecord(c, *rec); err != nil {
		return m.fail("kuadrant", err.Error()+" Nothing was created.", errorCodeFromK8sErr(err), "record write failed")
	}
	if err := createKuadrant(c, version, ns, true); err != nil {
		return m.fail("kuadrant", fmt.Sprintf("The API server rejects the Kuadrant CR %s/kuadrant: %v. Nothing was created.", ns, err), maasWriteCode(err), "kuadrant dry run failed")
	}
	m.step("kuadrant", "running", fmt.Sprintf("Creating the Kuadrant CR %s/kuadrant...", ns))
	if err := createKuadrant(c, version, ns, false); err != nil {
		return m.fail("kuadrant", fmt.Sprintf("Failed to create the Kuadrant CR %s/kuadrant: %v. Set up again to retry.", ns, err), maasWriteCode(err), "kuadrant create failed")
	}
	m.logs = append(m.logs, "OK: Kuadrant "+ns+"/kuadrant created")
	m.step("kuadrant", "success", fmt.Sprintf("Kuadrant %s/kuadrant created; it starts Authorino, which checks each call's token.", ns))
	return nil, nil
}

// waitPrerequisitesReady waits until every component turned on reports
// Ready and, for KServe, until its dependency condition no longer names an
// operator this setup installs. A condition that is never reported is not a
// failure (an older RHOAI does not report it).
func (m *maasRun) waitPrerequisitesReady(p *featurePlan) (*types.OperationResponse, error) {
	c := m.c
	m.step("ready", "running", fmt.Sprintf("Waiting for RHOAI's operator to report %s ready (usually 1 to 3 minutes)...", joinTitles(componentKeys(p.components))))
	last := ""
	var dsc dscRead
	done, err := waitForGone(c, PrerequisiteReadyTimeout, MaaSPoll, func(cc *Client) (bool, error) {
		r, err := readFirstDSC(cc)
		if err != nil {
			return false, err
		}
		dsc = r
		var waiting []string
		for _, cp := range p.components {
			if s, msg := componentReadyCondition(r.Object, []string{cp.key}); s != "True" && (s == "False" || cp.turnOn) {
				waiting = append(waiting, fmt.Sprintf("%s: %s", cp.key, firstNonEmpty(truncateText(msg, 300), map[string]string{"False": "not ready", "": "not reported yet"}[s])))
			}
		}
		if p.fs.dependency != "" {
			s, msg := dscConditionStatus(r.Object, p.fs.dependency)
			if s == "False" {
				for _, mention := range parseMissingDependencies(msg) {
					if p.installs(mention) {
						waiting = append(waiting, fmt.Sprintf("KServe does not see %s yet", mention))
					}
				}
			}
		}
		msg := strings.Join(waiting, "; ")
		if msg != last && msg != "" {
			last = msg
			m.step("ready", "running", "Waiting: "+msg)
		}
		return len(waiting) == 0, nil
	})
	if err != nil {
		return m.fail("ready", fmt.Sprintf("Stopped waiting: %v. What was set up stays; Set up again to continue.", err), firstNonEmpty(errorCodeFromK8sErr(err), "cancelled"), "ready wait failed")
	}
	if !done {
		// A component never reported at all is not a failure.
		failed := false
		for _, cp := range p.components {
			if s, _ := componentReadyCondition(dsc.Object, []string{cp.key}); s == "False" {
				failed = true
			}
		}
		if failed || strings.Contains(last, "KServe does not see") {
			hint := ""
			if strings.Contains(last, "KServe does not see") {
				hint = " KServe checks its operators again only from time to time: the Diagnostics page offers to restart its operator when it still reports an installed operator missing."
			}
			return m.fail("ready", fmt.Sprintf("Not ready after %s (%s). What was set up stays; the tab shows each part.%s", clockText(PrerequisiteReadyTimeout), last, hint), "not_ready", "not ready")
		}
		m.step("ready", "success", fmt.Sprintf("RHOAI's operator does not report %s's readiness; continuing.", joinTitles(componentKeys(p.components))))
	} else {
		m.step("ready", "success", fmt.Sprintf("%s %s ready.", joinTitles(componentKeys(p.components)), verb(len(p.components), "is", "are")))
	}
	note := ""
	if p.fs.dependency != "" {
		if s, msg := dscConditionStatus(dsc.Object, p.fs.dependency); s == "False" {
			note = " KServe still reports missing: " + truncateText(msg, 300) + "."
		}
	}
	slog.Info("prerequisites set up", "feature", p.fs.id, "user", getUser(c))
	return m.done(fmt.Sprintf("What %s needs on the cluster is set up.%s", p.fs.title, note))
}

// --- turning components back (Clean up) ----------------------------------------------

// RestorableComponentKey: key is a component this tool turns on for a
// feature (Clean up may turn it back).
func RestorableComponentKey(key string) bool {
	for _, fs := range featureSetups {
		for _, fc := range fs.components {
			if slices.Contains(fc.keys, key) || (fc.v1Key != "" && fc.v1Key == key) {
				return true
			}
		}
	}
	return false
}

// componentUsers names what on the cluster still uses a component (objects
// being deleted do not count). A read that fails is returned: the caller
// keeps the component on (fail closed).
func componentUsers(c *Client, key string) ([]string, error) {
	var out []string
	live := func(meta objectMeta, kind string) {
		if meta.DeletionTimestamp == "" {
			out = append(out, fmt.Sprintf("%s %s/%s", kind, meta.Namespace, meta.Name))
		}
	}
	switch key {
	case "aipipelines", "datasciencepipelines":
		dspas, err := listDSPAs(c, "")
		if errors.Is(err, errDSPACRDMissing) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		for _, d := range dspas {
			live(d.Meta, "pipeline server")
		}
	case "mlflowoperator":
		cr, err := getMLflowCR(c)
		if IsK8sError(err, 404) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read the MLflow CR: %w", err)
		}
		if cr.Metadata.DeletionTimestamp == "" {
			out = append(out, "MLflow "+cr.Metadata.Name)
		}
	case "kserve":
		models, err := listLLMInferenceServices(c)
		if err != nil && !errors.Is(err, errLLMISvcAPIMissing) {
			return nil, err
		}
		for _, m := range models {
			live(m.Metadata, "LLMInferenceService")
		}
		body, _, err := c.get(clusterPath("serving.kserve.io/v1beta1", "inferenceservices", ""))
		if err != nil && !IsK8sError(err, 404) {
			return nil, fmt.Errorf("list InferenceServices: %w", err)
		}
		if err == nil {
			var list struct {
				Items []struct {
					Metadata objectMeta `json:"metadata"`
				} `json:"items"`
			}
			if err := json.Unmarshal(body, &list); err != nil {
				return nil, fmt.Errorf("parse InferenceServices: %w", err)
			}
			for _, it := range list.Items {
				live(it.Metadata, "InferenceService")
			}
		}
	case "ogx", "llamastackoperator", "dashboard":
		// The playground's servers (the dashboard serves their gen-ai API).
		read, err := listServed(c, "ogx.io", "/apis/ogx.io/%s/ogxservers", nil, []string{"v1beta1"})
		if err != nil {
			return nil, fmt.Errorf("list OGXServers: %w", err)
		}
		if read.Body != nil {
			var list struct {
				Items []struct {
					Metadata objectMeta `json:"metadata"`
				} `json:"items"`
			}
			if err := json.Unmarshal(read.Body, &list); err != nil {
				return nil, fmt.Errorf("parse OGXServers: %w", err)
			}
			for _, it := range list.Items {
				live(it.Metadata, "playground server")
			}
		}
	default:
		return nil, fmt.Errorf("component %s is not one this tool turns on", key)
	}
	sort.Strings(out)
	return out, nil
}

// restoreVerdict is what Clean up does with one component, decided on one
// read of the DataScienceCluster.
type restoreVerdict struct {
	restore bool   // turn it back to prev
	prev    string // its value before this tool's setup
	drop    bool   // forget it in the record (it is not this tool's to turn back any more)
	kept    string // why it stays Managed, for the result ("" none)
	log     string
	// refusal stops the whole restore before any further write.
	refusal, code string
}

// restoreCheck runs every check of turning key back, on dsc: the record
// says this tool turned it on, it is still Managed, this tool's field
// manager still owns it (nobody changed it since: dscComponentOwned),
// Argo CD does not manage the DataScienceCluster, and nothing on the
// cluster uses it (a list that fails refuses).
func restoreCheck(c *Client, rec prereqRecord, dsc dscRead, key string) restoreVerdict {
	rc, ok := rec.Components[key]
	if !ok {
		return restoreVerdict{kept: key + " (this tool did not turn it on)"}
	}
	prev := firstNonEmpty(rc.Previous, "Removed")
	state, _ := componentManagement(dsc.Object, key)
	switch {
	case dsc.Object == nil || state != "Managed":
		return restoreVerdict{drop: true, log: key + " is " + firstNonEmpty(state, "not set") + " already"}
	case !dscComponentOwned(dsc.Object, key):
		return restoreVerdict{drop: true, kept: key + " (changed by someone since this tool's setup)"}
	}
	if app := argoApplicationOf(dsc.Object, "DataScienceCluster"); app != "" {
		return restoreVerdict{refusal: gitOpsRefusal("The DataScienceCluster", app, "turning "+key+" back to "+prev), code: ErrorCodeGitOpsManaged}
	}
	users, err := componentUsers(c, key)
	if err != nil {
		return restoreVerdict{refusal: fmt.Sprintf("Cannot tell whether anything still uses %s, so it stays Managed: %v.%s", key, err, templateHint(err)), code: errorCodeFromK8sErr(err)}
	}
	if len(users) > 0 {
		shown := users
		if len(shown) > 3 {
			shown = append(append([]string{}, users[:3]...), fmt.Sprintf("%d more", len(users)-3))
		}
		return restoreVerdict{kept: fmt.Sprintf("%s (in use: %s)", key, joinTitles(shown))}
	}
	return restoreVerdict{restore: true, prev: prev}
}

// RestoreComponents turns the given components back to what they were
// before this tool turned them on (Clean up, last): each only when
// restoreCheck allows it. A component in use, or changed by someone since,
// stays as it is (the result says why). Every check is made on one read
// before any write; a read that fails changes nothing. Each write carries
// the resourceVersion of the read its checks were made on: when the
// DataScienceCluster changed meanwhile (409), it is read again and every
// check runs again before the next attempt. Someone who set a component to
// Managed again without changing it does not take the field from this
// tool's manager (the API server moves a field only when its value
// changes), so then the in-use check is what keeps a component in use on.
func RestoreComponents(c *Client, keys []string) (*types.OperationResponse, error) {
	rec, err := readPrereqRecord(c)
	if err != nil {
		return &types.OperationResponse{Success: false, Message: err.Error() + " Nothing was changed.", ErrorCode: errorCodeFromK8sErr(err)}, nil
	}
	dsc, err := readFirstDSC(c)
	if err != nil {
		return &types.OperationResponse{Success: false, Message: fmt.Sprintf("Cannot read the DataScienceCluster; nothing was changed: %v.%s", err, templateHint(err)), ErrorCode: errorCodeFromK8sErr(err)}, nil
	}
	var todo []string
	var kept, logs, done []string
	dropped := false
	apply := func(key string, v restoreVerdict) {
		if v.drop {
			delete(rec.Components, key)
			dropped = true
		}
		if v.kept != "" {
			kept = append(kept, v.kept)
		}
		if v.log != "" {
			logs = append(logs, v.log)
		}
	}
	for _, key := range keys {
		v := restoreCheck(c, rec, dsc, key)
		if v.refusal != "" {
			return &types.OperationResponse{Success: false, Message: v.refusal + " Nothing was changed.", ErrorCode: v.code}, nil
		}
		if v.restore {
			todo = append(todo, key)
			continue
		}
		apply(key, v)
	}
	stop := func(msg, code string) (*types.OperationResponse, error) {
		if dropped {
			_ = writePrereqRecord(c, rec)
		}
		if len(done) > 0 {
			msg += " Already turned back: " + joinTitles(done) + "."
		} else {
			msg += " Nothing was changed."
		}
		return &types.OperationResponse{Success: false, Message: msg, Logs: logs, ErrorCode: code}, nil
	}
	// reads counts the reads of the DataScienceCluster; a component's checks
	// hold for the read they were made on, and for the answers to this
	// restore's own writes after it (each guarded by its resourceVersion).
	reads := 0
	checkedOn := map[string]int{}
	for _, key := range todo {
		for attempt := 1; ; attempt++ {
			v := restoreVerdict{restore: true, prev: firstNonEmpty(rec.Components[key].Previous, "Removed")}
			if checkedOn[key] != reads {
				if v = restoreCheck(c, rec, dsc, key); v.refusal != "" {
					return stop(v.refusal, v.code)
				}
				checkedOn[key] = reads
			}
			if !v.restore {
				apply(key, v)
				break
			}
			meta, _ := dsc.Object["metadata"].(map[string]interface{})
			rv, _ := meta["resourceVersion"].(string)
			answer, err := patchDSCComponent(c, key, v.prev, rv, false)
			if err == nil {
				if answer != nil {
					dsc.Object = answer
				}
				delete(rec.Components, key)
				dropped = true
				done = append(done, fmt.Sprintf("%s is %s again", key, v.prev))
				logs = append(logs, "OK: "+key+" "+v.prev)
				break
			}
			if !IsK8sError(err, http.StatusConflict) {
				return stop(fmt.Sprintf("Failed to turn %s back to %s: %v.%s", key, v.prev, err, templateHint(err)), errorCodeFromK8sErr(err))
			}
			if attempt == 3 {
				return stop(fmt.Sprintf("The DataScienceCluster kept changing while %s was turned back (someone else is editing it), so it stays Managed. Clean up again later.", key), "conflict")
			}
			logs = append(logs, "The DataScienceCluster changed since it was read; reading it again and checking again.")
			if dsc, err = readFirstDSC(c); err != nil {
				return stop(fmt.Sprintf("The DataScienceCluster changed since it was read, and it cannot be read again, so %s stays Managed: %v.%s", key, err, templateHint(err)), errorCodeFromK8sErr(err))
			}
			reads++
		}
	}
	if dropped {
		if err := writePrereqRecord(c, rec); err != nil {
			logs = append(logs, "Could not update this tool's record: "+err.Error())
		}
	}
	var msg []string
	if len(done) > 0 {
		msg = append(msg, joinTitles(done)+", as before setup.")
	}
	if len(kept) > 0 {
		msg = append(msg, "Kept Managed: "+strings.Join(kept, "; ")+".")
	}
	if len(msg) == 0 {
		msg = append(msg, "No component to turn back.")
	}
	slog.Info("components restored", "done", len(done), "kept", len(kept), "user", getUser(c))
	return &types.OperationResponse{Success: true, Message: strings.Join(msg, " "), Logs: logs}, nil
}
