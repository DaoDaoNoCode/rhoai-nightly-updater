package features

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/argo"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/components"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/dsc"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kueue"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/modelregistry"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/serving"
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
//   - Model registry: aiHub Managed (DataScienceCluster v3, RHOAI 3.6;
//     it also runs the Model Catalog), modelregistry before. No operator.
//     Its namespace is left to the component's default (it cannot change
//     while Managed).
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
var certManagerOperator = envkit.OperatorSpec{ID: "cert-manager", Title: "cert-manager Operator for Red Hat OpenShift", Pkg: "openshift-cert-manager-operator",
	SubName: "openshift-cert-manager-operator", SubNames: []string{"openshift-cert-manager-operator"}, Namespace: "cert-manager-operator",
	Source: "redhat-operators", Group: "cert-manager-operator", API: certManagerGroup, Controller: "cert-manager"}

const certManagerGroup = "cert-manager.io"

// dashboardComponent is the RHOAI dashboard, whose gen-ai API runs the
// playground.
var dashboardComponent = envkit.FeatureComponent{Feature: "The RHOAI dashboard", Keys: []string{"dashboard"}}

// featureSetup is what a feature needs on the cluster.
type featureSetup struct {
	id, title  string
	components []envkit.FeatureComponent
	// operators are installed in this order after the components, each
	// when no Subscription of its package exists (KServe counts only
	// those).
	operators []envkit.OperatorSpec
	// dependency is the DataScienceCluster condition that names missing
	// operators (KserveLLMInferenceServiceDependencies), waited on while it
	// names one of operators ("" none).
	dependency string
	// kuadrant: a Kuadrant CR is made after the operators (Models routed
	// through a gateway: featureSetupFor).
	kuadrant bool
	// operatorsFirst: the operators are installed before the components
	// are turned on (Kueue: RHOAI's kueue component needs its operator).
	operatorsFirst bool
	// ready is what else ready waits for once the components report
	// ready, with the user's token: what is still awaited (""), or a
	// refusal that stops the wait.
	ready func(c *kube.Client) (waiting, refusal string, err error)
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
	if exposure == serving.ExposureDefault || exposure == serving.ExposureGateway {
		fs.operators = append(append([]envkit.OperatorSpec(nil), fs.operators...), serving.RHCLOperator)
		fs.kuadrant = true
	}
	return fs
}

var featureSetups = []featureSetup{
	{id: "pipelines", title: "Pipelines", components: []envkit.FeatureComponent{envkit.PipelinesComponent}},
	{id: "mlflow", title: "MLflow", components: []envkit.FeatureComponent{envkit.MLflowComponent}},
	{id: "model-registry", title: "Model registry", components: []envkit.FeatureComponent{envkit.ModelRegistryComponent}},
	{id: "models", title: "Models", components: []envkit.FeatureComponent{serving.KServeComponent}, operators: []envkit.OperatorSpec{certManagerOperator}, dependency: serving.LLMDependenciesCondition},
	{id: "playground", title: "Playground", components: []envkit.FeatureComponent{playgroundComponent, dashboardComponent}},
	// Distributed workloads, as the RHOAI docs set Kueue up ("Manage
	// workloads with Kueue"): cert-manager and the Red Hat build of Kueue
	// first, then the DataScienceCluster's kueue Unmanaged; RHOAI then
	// creates the Kueue CR cluster, which ready waits for (kueueReady).
	{id: "kueue", title: "Distributed workloads", components: []envkit.FeatureComponent{envkit.KueueComponent}, operators: []envkit.OperatorSpec{certManagerOperator, kueue.Operator}, operatorsFirst: true, ready: kueue.InstanceWaiting},
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
	// prerequisiteReadyTimeout bounds the wait for a component turned on
	// to report Ready, and for KServe to see the operators installed.
	prerequisiteReadyTimeout = 10 * time.Minute
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

// ReadPrereqRecord reads what the feature setups recorded (empty when nothing).
func ReadPrereqRecord(c *kube.Client) (prereqRecord, error) {
	var rec prereqRecord
	raw, err := record.ReadSnapshotKey(c, prereqRecordKey)
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

func writePrereqRecord(c *kube.Client, rec prereqRecord) error {
	rec.At, rec.By = kube.NowRFC3339(), kube.GetUser(c)
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if err := record.WriteSnapshotKey(c, prereqRecordKey, prereqRecordManager, string(data)); err != nil {
		return fmt.Errorf("record the prerequisites this tool set up: %w", err)
	}
	return nil
}

// --- DataScienceCluster components ---------------------------------------------------

// dscComponentManager is the field manager of a component's managementState
// this tool sets: the API server then tells whether the value is still the
// tool's (DSCComponentOwned). One per component, so turning one back never
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
func patchDSCComponent(c *kube.Client, key, state, resourceVersion string, dryRun bool) (map[string]any, error) {
	path, err := rhoai.DataScienceClusterPath(c)
	if err != nil {
		return nil, err
	}
	body := map[string]any{"spec": map[string]any{"components": map[string]any{key: map[string]any{"managementState": state}}}}
	if resourceVersion != "" {
		body["metadata"] = map[string]any{"resourceVersion": resourceVersion}
	}
	patch, _ := json.Marshal(body)
	query := url.Values{"fieldManager": {dscComponentManager(key)}}
	if dryRun {
		query.Set("dryRun", "All")
	}
	answer, _, err := c.Do(http.MethodPatch, path, "application/merge-patch+json", patch, query)
	if err != nil {
		return nil, err
	}
	var obj map[string]any
	_ = json.Unmarshal(answer, &obj)
	return obj, nil
}

// DSCComponentOwned: this tool's field manager of the component still owns
// its managementState. The API server moves a field to another manager only
// when that manager changes its value (an update to the same value, or an
// apply of it, leaves it owned, or shared): so this tells whether someone
// changed the component since this tool's setup, not whether someone set
// it to Managed again.
func DSCComponentOwned(dsc map[string]any, key string) bool {
	raw, _ := json.Marshal(dsc["metadata"])
	var meta kube.ObjectMeta
	_ = json.Unmarshal(raw, &meta)
	return meta.OwnsFieldPath([]string{dscComponentManager(key)}, "f:spec", "f:components", "f:"+key, "f:managementState")
}

// dscComponentCommand is the change from a terminal.
func dscComponentCommand(dsc, key, state string) string {
	return fmt.Sprintf(`oc patch datasciencecluster %s --type=merge -p '{"spec":{"components":{"%s":{"managementState":"%s"}}}}'`, kube.ShellQuote(kube.FirstNonEmpty(dsc, "default-dsc")), key, state)
}

// componentTitle names a component for people.
func componentTitle(fc envkit.FeatureComponent, key string) string {
	name := strings.TrimPrefix(fc.Feature, "The ")
	if strings.Contains(name, "(") || strings.HasSuffix(name, " component") {
		return name // "Model serving (KServe)", "Kueue component"
	}
	return fmt.Sprintf("%s (%s)", name, key)
}

// --- the plan --------------------------------------------------------------------------

// componentPlan is a component as read, and whether setup turns it on.
type componentPlan struct {
	fc            envkit.FeatureComponent
	key, state    string
	turnOn        bool
	ready, msg    string // its <key>Ready condition
	owned         bool   // this tool's manager owns its managementState
	recorded      bool   // the record says this tool turned it on
	recordedPrior string
}

// operatorPlan is an operator as read, and what setup does about it.
type operatorPlan struct {
	op      envkit.OperatorSpec
	st      envkit.OperatorState
	install *envkit.OperatorInstallPlan
	byTool  bool
}

// featurePlan is everything a feature's prerequisites setup does, decided
// on reads before any write. refusal is set when it cannot run; part
// problems that do not block are in the parts.
type featurePlan struct {
	fs                featureSetup
	rec               prereqRecord
	dsc               rhoai.DSCRead
	dscName, argoApp  string // argoApp: argo.managedBy
	argo              argo.Tracking
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
	dsc    rhoai.DSCRead
	dscErr error
	csvs   []components.ClusterCSV
	subs   []*envkit.OLMSubscription
	olmErr error
}

func readClusterForPrerequisites(c *kube.Client) clusterReads {
	var r clusterReads
	r.rec, r.recErr = ReadPrereqRecord(c)
	r.dsc, r.dscErr = rhoai.ReadFirstDSC(c)
	r.csvs, r.olmErr = components.ListClusterCSVs(c)
	if r.olmErr == nil {
		r.subs, r.olmErr = envkit.ListSubscriptions(c, "")
	}
	return r
}

// planFeature reads what fs needs and decides what setup does: only what
// is missing. A read that fails refuses the setup (fail closed).
func planFeature(c *kube.Client, fs featureSetup, r clusterReads) *featurePlan {
	p := &featurePlan{fs: fs, rec: r.rec, dsc: r.dsc}
	part := func(pt types.MaaSPart) { p.parts = append(p.parts, pt) }
	if r.recErr != nil {
		p.refuse(r.recErr.Error()+" Nothing was changed.", kube.ErrorCodeFromK8sErr(r.recErr))
	}

	// The components.
	switch {
	case r.dscErr != nil:
		msg := fmt.Sprintf("Cannot read the DataScienceCluster; nothing was changed: %v.%s", r.dscErr, kube.TemplateHint(r.dscErr))
		p.refuse(msg, kube.ErrorCodeFromK8sErr(r.dscErr))
		for _, fc := range fs.components {
			part(types.MaaSPart{ID: "dsc-" + fc.Keys[0], Title: componentTitle(fc, fc.Keys[0]), State: "unknown", Message: msg})
		}
	case r.dsc.State == rhoai.DSCStateNoCRD:
		p.refuse("RHOAI is not installed on this cluster (it has no DataScienceCluster API). A cluster-admin installs it on the Status page. Nothing was changed.", "prerequisites")
	case r.dsc.Object == nil:
		p.refuse("This cluster has no DataScienceCluster. A cluster-admin creates one on the Components page. Nothing was changed.", "prerequisites")
	}
	if r.dscErr == nil && r.dsc.Object != nil {
		meta, _ := r.dsc.Object["metadata"].(map[string]any)
		p.dscName, _ = meta["name"].(string)
		p.argo = argo.TrackingOf(c, r.dsc.Object, rhoai.DSCGroup, "DataScienceCluster")
		p.argoApp = p.argo.ManagedBy()
		for _, fc := range fs.components {
			key := fc.KeyIn(r.dsc)
			state, _ := envkit.ComponentManagement(r.dsc.Object, key)
			on := fc.OnState()
			cp := componentPlan{fc: fc, key: key, state: state, turnOn: state != on, owned: DSCComponentOwned(r.dsc.Object, key)}
			cp.ready, cp.msg = componentReadyCondition(r.dsc.Object, []string{key})
			if rc, ok := r.rec.Components[key]; ok {
				cp.recorded, cp.recordedPrior = true, rc.Previous
			}
			p.components = append(p.components, cp)
			pt := types.MaaSPart{ID: "dsc-" + key, Title: componentTitle(fc, key), ManagedByTool: cp.recorded && cp.owned && !cp.turnOn}
			switch {
			case cp.turnOn && p.argoApp != "":
				pt.State, pt.Message = "manual", fmt.Sprintf("%s in DataScienceCluster %s, which Argo CD application %q manages: set it to %s in Git.", kube.FirstNonEmpty(state, "Not set"), p.dscName, p.argoApp, on)
				pt.Commands = dscComponentCommand(p.dscName, key, on)
			case cp.turnOn:
				pt.State, pt.Message = "missing", fmt.Sprintf("%s in DataScienceCluster %s. Set up sets it to %s%s; Clean up can turn it back.", kube.FirstNonEmpty(state, "Not set"), p.dscName, on, onMeaning(on))
				pt.Commands = dscComponentCommand(p.dscName, key, on)
				p.plan = append(p.plan, fmt.Sprintf("DataScienceCluster %s: set %s to %s (it is %s; Clean up can turn it back while nothing else uses it)", p.dscName, key, on, kube.FirstNonEmpty(state, "not set")))
			case cp.ready == "False":
				pt.State, pt.Message = "waiting", on+"; RHOAI's operator reports it not ready: "+kube.TruncateText(kube.FirstNonEmpty(cp.msg, "no message"), 300)
			default:
				pt.State, pt.Message = "ready", on+" in DataScienceCluster "+p.dscName
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
			p.refuse(argo.GitOpsRefusal("The DataScienceCluster", p.argoApp, "turning on "+strings.Join(turnOn, " and "))+p.argo.UnconfirmedNote(), kube.ErrorCodeGitOpsManaged)
		}
		if fs.dependency != "" {
			p.depStatus, p.depMsg = serving.DSCConditionStatus(r.dsc.Object, fs.dependency)
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
func planKuadrantPart(c *kube.Client, p *featurePlan) types.MaaSPart {
	pt := types.MaaSPart{ID: "kuadrant", Title: "Connectivity Link instance (Kuadrant)"}
	k, err := serving.ReadKuadrant(c)
	switch {
	case err != nil:
		msg := fmt.Sprintf("Cannot read the Kuadrant CR; nothing was changed: %v.%s", err, kube.TemplateHint(err))
		p.refuse(msg, kube.ErrorCodeFromK8sErr(err))
		pt.State, pt.Message = "unknown", msg
	case k.Found && k.Ready:
		pt.State, pt.Message, pt.NotManaged = "ready", k.Namespace+"/kuadrant is ready", !serving.ToolOwned(k.Meta)
		pt.ManagedByTool = serving.ToolOwned(k.Meta)
	case k.Found:
		pt.State, pt.Message = "waiting", "Kuadrant "+k.Namespace+"/kuadrant is not Ready yet: "+kube.TruncateText(kube.FirstNonEmpty(k.Message, "no message"), 300)
	default:
		pt.State, pt.Message = "missing", "Not created yet. Set up creates it once Connectivity Link serves its API; it starts Authorino, which checks each call's token."
		pt.Commands = serving.KuadrantCommands(serving.RHCLOperator.Namespace)
		p.plan = append(p.plan, fmt.Sprintf("Create the Kuadrant CR %s/kuadrant (as the MaaS docs do); it stays after Clean up, as other features share it", serving.RHCLOperator.Namespace))
	}
	return pt
}

// planFeatureOperator reads op and decides what setup does about it.
func planFeatureOperator(c *kube.Client, p *featurePlan, op envkit.OperatorSpec, r clusterReads) operatorPlan {
	pt := types.MaaSPart{ID: op.ID, Title: op.Title}
	defer func() { p.parts = append(p.parts, pt) }()
	if r.olmErr != nil {
		msg := fmt.Sprintf("Cannot read the installed operators, so this tool cannot tell whether %s is installed; nothing was changed: %v.%s", op.Title, r.olmErr, kube.TemplateHint(r.olmErr))
		p.refuse(msg, kube.ErrorCodeFromK8sErr(r.olmErr))
		pt.State, pt.Message = "unknown", msg
		return operatorPlan{op: op}
	}
	st, err := envkit.ReadOperatorIn(c, op, r.csvs, r.subs)
	if err != nil {
		msg := fmt.Sprintf("Cannot read %s's Subscription; nothing was changed: %v.%s", op.Title, err, kube.TemplateHint(err))
		p.refuse(msg, kube.ErrorCodeFromK8sErr(err))
		pt.State, pt.Message = "unknown", msg
		return operatorPlan{op: op}
	}
	opp := operatorPlan{op: op, st: st}
	_, shared := p.rec.Shared[op.ID]
	sub := st.Requested
	opp.byTool = shared && sub != nil && serving.ToolOwned(sub.Metadata)
	pt.ManagedByTool, pt.NotManaged = opp.byTool, !opp.byTool
	where := ""
	if sub != nil {
		where = fmt.Sprintf(" (Subscription %s/%s, channel %s)", sub.Metadata.Namespace, sub.Metadata.Name, kube.FirstNonEmpty(sub.Spec.Channel, "not set"))
	}
	switch {
	case st.Installed() && st.Phase == "Succeeded":
		pt.State, pt.Message = "ready", fmt.Sprintf("Version %s runs in %s%s", st.Version, st.CSVNS, where)
		if sub == nil {
			// KServe counts a Subscription of the package, not a CSV.
			pt.State, pt.Message = "problem", fmt.Sprintf("Version %s runs in %s, but no Subscription of package %s exists, which is what RHOAI's KServe checks for. A cluster-admin recreates its Subscription.", st.Version, st.CSVNS, op.Pkg)
		}
	case st.Installed():
		pt.State, pt.Message = "waiting", fmt.Sprintf("Version %s is being installed in %s%s (CSV %s)", st.Version, st.CSVNS, where, kube.FirstNonEmpty(st.Phase, "pending"))
	case st.Pending() != nil:
		pt.State, pt.Message = "waiting", fmt.Sprintf("Subscription %s/%s exists; OLM is installing it (%s). Set up waits for it.", sub.Metadata.Namespace, sub.Metadata.Name, kube.FirstNonEmpty(sub.Status.State, "no state yet"))
	default:
		// Kueue: who installed it is told by its CRD's OLM labels, whatever
		// its controller is named (kueue.OutsideOLM).
		if op.API == kueue.APIGroup {
			outside, detail, err := kueue.OutsideOLM(c)
			if err != nil {
				msg := fmt.Sprintf("Cannot tell whether Kueue runs outside OperatorHub; nothing was changed: %v.%s", err, kube.TemplateHint(err))
				p.refuse(msg, kube.ErrorCodeFromK8sErr(err))
				pt.State, pt.Message = "unknown", msg
				return opp
			}
			if outside {
				msg := fmt.Sprintf("Kueue's API (%s) is served on this cluster by a Kueue installed outside OperatorHub (%s): upstream manifests, or the Kueue RHOAI 2.x embedded. RHOAI does not count it, and installing %s next to it would run two. A cluster-admin migrates to %s from OperatorHub. Nothing was changed.", op.API, detail, op.Title, op.Title)
				p.refuse(msg, "prerequisites")
				pt.State, pt.Message, pt.NotManaged = "manual", msg, true
				return opp
			}
		}
		// Its API served by a running controller without a CSV: someone
		// else's install, which KServe does not count and a second one
		// would fight (a served API alone is CRDs left by an uninstall).
		if op.API != "" && op.API != kueue.APIGroup {
			served, err := operatorAPIServed(c, op)
			if err == nil && served && op.Controller != "" {
				served, err = envkit.ControllerDeployed(c, op.Controller)
			}
			if err != nil {
				msg := fmt.Sprintf("Cannot tell whether %s runs; nothing was changed: %v.%s", op.Title, err, kube.TemplateHint(err))
				p.refuse(msg, kube.ErrorCodeFromK8sErr(err))
				pt.State, pt.Message = "unknown", msg
				return opp
			}
			if served {
				msg := fmt.Sprintf("%s runs on this cluster (its API %s and Deployment %s exist) without being installed from OperatorHub, so RHOAI's KServe does not count it, and installing %s next to it would run two. A cluster-admin replaces it with %s from OperatorHub. Nothing was changed.", strings.TrimSuffix(op.API, ".io"), op.API, op.Controller, op.Title, op.Title)
				p.refuse(msg, "prerequisites")
				pt.State, pt.Message, pt.NotManaged = "manual", msg, true
				return opp
			}
		}
		plan, msg, code := envkit.PlanOperatorInstall(c, op)
		if msg != "" {
			p.refuse(msg, code)
			pt.State, pt.Message = "problem", "Not installed, and Set up cannot install it: "+msg
			return opp
		}
		opp.install = plan
		pt.ManagedByTool, pt.NotManaged = false, false
		pt.State, pt.Message = "missing", fmt.Sprintf("Not installed. Set up installs version %s from %s (channel %s).", plan.Head, op.Source, plan.Channel)
		pt.Commands = operatorInstallCommands(plan)
		reuse := ""
		if plan.ReusedGroup != "" {
			reuse = fmt.Sprintf(", using OperatorGroup %s/%s as it is", op.Namespace, plan.ReusedGroup)
		}
		p.plan = append(p.plan, fmt.Sprintf("Install %s %s: %sSubscription %s/%s (package %s, channel %s, from %s, automatic updates%s); it stays after Clean up, as other features share it", op.Title, plan.Head, serving.OperatorPlanPrefix(plan), op.Namespace, op.SubName, op.Pkg, plan.Channel, op.Source, reuse))
	}
	return opp
}

// operatorAPIServed says whether op's API group is served (each group
// named, so the RBAC coverage test resolves the discovery path).
func operatorAPIServed(c *kube.Client, op envkit.OperatorSpec) (bool, error) {
	switch op.API {
	case certManagerGroup:
		return serving.GroupServed(c, certManagerGroup)
	case serving.KuadrantGroup:
		return serving.GroupServed(c, serving.KuadrantGroup)
	case serving.CNPGGroup:
		return serving.GroupServed(c, serving.CNPGGroup)
	}
	return false, fmt.Errorf("this tool does not know the API of %s", op.Title)
}

// dependencyPart is KServe's own view of its operators.
func dependencyPart(p *featurePlan) types.MaaSPart {
	pt := types.MaaSPart{ID: "dependencies", Title: "KServe's dependency check"}
	missing := components.ParseMissingDependencies(p.depMsg)
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
		pt.State, pt.Message = "waiting", fmt.Sprintf("Waits for %s: KServe reports %s. Set up installs %s, then waits until KServe sees %s.", names, kube.Verb(len(missing), "it isn't installed", "they aren't installed"), kube.Verb(len(missing), "it", "them"), kube.Verb(len(missing), "it", "them"))
	default:
		pt.State, pt.Message = "waiting", "KServe reports: "+kube.TruncateText(kube.FirstNonEmpty(p.depMsg, "not met"), 300)+". Set up installs what it names, then waits until KServe sees it."
	}
	return pt
}

// installs: the plan installs (or has) the operator a condition names.
func (p *featurePlan) installs(mention string) bool {
	key := components.NormalizeOperatorName(mention)
	for _, o := range p.operators {
		for _, n := range []string{o.op.Pkg, o.op.Title, o.op.ID} {
			if components.NormalizeOperatorName(n) == key {
				return true
			}
		}
	}
	return false
}

// operatorInstallCommands are the plan's objects from a terminal.
func operatorInstallCommands(p *envkit.OperatorInstallPlan) string {
	ns, group, sub := envkit.OperatorObjects(p.Op, p.Channel)
	var objs []map[string]any
	if p.NSMissing {
		objs = append(objs, ns)
	}
	if p.GroupMissing {
		objs = append(objs, group)
	}
	objs = append(objs, sub)
	var docs []string
	for _, o := range objs {
		meta := o["metadata"].(map[string]any)
		delete(meta, "labels")
		docs = append(docs, yamlish(o, ""))
	}
	return "oc apply -f - <<'EOF'\n" + strings.Join(docs, "---\n") + "EOF\n" +
		fmt.Sprintf("oc wait --for=jsonpath='{.status.state}'=AtLatestKnown subscription/%s -n %s --timeout=300s", p.Op.SubName, p.Op.Namespace)
}

// yamlish writes the plain maps, lists and strings of an object as YAML
// (keys in a fixed order: apiVersion, kind, metadata, spec, then the rest).
func yamlish(v any, indent string) string {
	var b strings.Builder
	switch t := v.(type) {
	case map[string]any:
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
			case map[string]any, []any:
				b.WriteString(indent + k + ":\n" + yamlish(child, indent+"  "))
			default:
				fmt.Fprintf(&b, "%s%s: %v\n", indent, k, child)
			}
		}
	case []any:
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
		if cp.recorded && cp.owned && cp.state == cp.fc.OnState() {
			out.Restorable = append(out.Restorable, types.RestorableComponent{Component: cp.key, Previous: cp.recordedPrior, Feature: p.fs.id})
		}
	}
	return out
}

// GetPrerequisitesStatusFor reads what every feature needs on the cluster,
// with Models planned for how its model is reached (featureSetupFor; "" is
// the default).
func GetPrerequisitesStatusFor(c *kube.Client, modelsExposure string) (*types.PrerequisitesStatus, error) {
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
		return "feature must be pipelines, mlflow, model-registry, models, playground or kueue"
	}
	if req.Exposure != "" && (req.Feature != "models" || !serving.ValidExposure(req.Exposure)) {
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
func RunPrerequisites(c *kube.Client, req types.PrerequisitesRequest, onStep func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
	m := &serving.Run{Client: c, Emit: onStep, Activity: "setup-prerequisites", Target: req.Feature + " prerequisites"}
	if problem := ValidatePrerequisitesRequest(req); problem != "" {
		return m.Fail("prerequisites", problem, "validation", "invalid request")
	}
	fs := featureSetupFor(req.Feature, req.Exposure)
	m.Target = fs.title + " prerequisites"
	m.Step("prerequisites", "running", fmt.Sprintf("Checking what %s needs on the cluster...", fs.title))
	p := planFeature(c, fs, readClusterForPrerequisites(c))
	if p.refusal != "" {
		return m.Fail("prerequisites", p.refusal, kube.FirstNonEmpty(p.code, "prerequisites"), "check failed")
	}
	// Every write as a server-side dry run first.
	for _, cp := range p.components {
		if !cp.turnOn {
			continue
		}
		if _, err := patchDSCComponent(c, cp.key, cp.fc.OnState(), "", true); err != nil {
			return m.Fail("prerequisites", fmt.Sprintf("The API server rejects setting %s to %s in the DataScienceCluster: %v. Nothing was changed.%s", cp.key, cp.fc.OnState(), err, kube.TemplateHint(err)), serving.MaaSWriteCode(err), "dry run failed")
		}
	}
	for _, o := range p.operators {
		if o.install == nil {
			continue
		}
		if _, err := envkit.CreateOperatorObjects(c, o.install, true); err != nil {
			return m.Fail("prerequisites", fmt.Sprintf("The API server rejects %s's install: %v. Nothing was changed.%s", o.op.Title, err, kube.TemplateHint(err)), serving.MaaSWriteCode(err), "dry run failed")
		}
	}
	m.Step("prerequisites", "success", "Everything Set up changes is checked.")

	// The components, and the operators: in that order, or the operators
	// first (fs.operatorsFirst: RHOAI's kueue needs its operator to be
	// there, as the docs install it first).
	components := func() (*types.OperationResponse, error) {
		var on []string
		for _, cp := range p.components {
			if cp.turnOn {
				on = append(on, cp.key)
			}
		}
		if len(on) == 0 {
			m.Step("dsc", "success", fmt.Sprintf("%s %s %s in DataScienceCluster %s.", kube.JoinTitles(componentKeys(p.components)), kube.Verb(len(p.components), "is", "are"), onStateOf(p.components), p.dscName))
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
			m.Step("dsc", "running", fmt.Sprintf("Setting %s to %s in DataScienceCluster %s (Clean up can turn %s back)...", kube.JoinTitles(on), onStateOf(p.components), p.dscName, kube.Verb(len(on), "it", "them")))
			if err := writePrereqRecord(c, rec); err != nil {
				return m.Fail("dsc", err.Error()+" The DataScienceCluster was not changed.", kube.ErrorCodeFromK8sErr(err), "record write failed")
			}
			p.rec = rec
			for _, cp := range p.components {
				if !cp.turnOn {
					continue
				}
				if _, err := patchDSCComponent(c, cp.key, cp.fc.OnState(), "", false); err != nil {
					return m.Fail("dsc", fmt.Sprintf("Failed to set %s to %s: %v.%s", cp.key, cp.fc.OnState(), err, kube.TemplateHint(err)), serving.MaaSWriteCode(err), "dsc patch failed")
				}
				m.Logs = append(m.Logs, fmt.Sprintf("OK: %s %s (was %s)", cp.key, cp.fc.OnState(), kube.FirstNonEmpty(cp.state, "not set")))
			}
			m.Step("dsc", "success", fmt.Sprintf("%s %s %s; RHOAI's operator deploys %s.", kube.JoinTitles(on), kube.Verb(len(on), "is", "are"), onStateOf(p.components), kube.Verb(len(on), "it", "them")))
		}
		return nil, nil
	}
	operators := func() (*types.OperationResponse, error) {
		for _, o := range p.operators {
			if r, err := ensureOperator(m, &p.rec, o); r != nil || err != nil {
				return r, err
			}
		}
		if fs.kuadrant {
			if r, err := ensureKuadrant(m, &p.rec); r != nil || err != nil {
				return r, err
			}
		}
		return nil, nil
	}
	steps := []func() (*types.OperationResponse, error){components, operators}
	if fs.operatorsFirst {
		steps = []func() (*types.OperationResponse, error){operators, components}
	}
	for _, step := range steps {
		if r, err := step(); r != nil || err != nil {
			return r, err
		}
	}

	// 3. Ready.
	return waitPrerequisitesReady(m, p)
}

// onMeaning says what a non-Managed on state means, in plain words.
func onMeaning(on string) string {
	if on == "Unmanaged" {
		return ", which means RHOAI uses the Kueue operator you install instead of its own"
	}
	return ""
}

// onStateOf is the state the plan's components are turned on with
// (Unmanaged for kueue, the only component of its feature; else Managed).
func onStateOf(cps []componentPlan) string {
	for _, cp := range cps {
		if cp.fc.OnState() != "Managed" {
			return cp.fc.OnState()
		}
	}
	return "Managed"
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
func ensureOperator(m *serving.Run, rec *prereqRecord, o operatorPlan) (*types.OperationResponse, error) {
	op := o.op
	switch {
	case o.install != nil:
		m.Step(op.ID, "running", fmt.Sprintf("Installing %s %s (channel %s) from %s...", op.Title, o.install.Head, o.install.Channel, op.Source))
		if rec.Shared == nil {
			rec.Shared = map[string]string{}
		}
		rec.Shared[op.ID] = fmt.Sprintf("Subscription %s/%s, channel %s", op.Namespace, op.SubName, o.install.Channel)
		if err := writePrereqRecord(m.Client, *rec); err != nil {
			return m.Fail(op.ID, err.Error()+" Nothing was installed.", kube.ErrorCodeFromK8sErr(err), "record write failed")
		}
		made, err := envkit.CreateOperatorObjects(m.Client, o.install, false)
		for _, d := range made {
			m.Logs = append(m.Logs, "OK: "+d+" created")
		}
		if err != nil {
			return m.Fail(op.ID, fmt.Sprintf("Failed to %v. Set up again to continue.", err), serving.MaaSWriteCode(err), "operator install failed")
		}
		return m.WaitOperator(op.ID, op.Namespace, op.SubName, op.Title)
	case o.st.Installed() && o.st.Phase == "Succeeded":
		m.Step(op.ID, "success", fmt.Sprintf("%s %s is installed%s.", op.Title, o.st.Version, serving.ByWhom(o.byTool)))
	case o.st.Pending() != nil || o.st.Installed():
		sub := o.st.Requested
		if sub == nil {
			m.Step(op.ID, "success", fmt.Sprintf("%s %s is being installed in %s.", op.Title, o.st.Version, o.st.CSVNS))
			return nil, nil
		}
		return m.WaitOperator(op.ID, sub.Metadata.Namespace, sub.Metadata.Name, op.Title)
	}
	return nil, nil
}

// ensureKuadrant makes the Kuadrant CR when there is none, once
// Connectivity Link serves its API (step "kuadrant"), in the namespace
// Connectivity Link runs in, as the MaaS setup does. It stays after Clean
// up: Connectivity Link's policies are shared.
func ensureKuadrant(m *serving.Run, rec *prereqRecord) (*types.OperationResponse, error) {
	c := m.Client
	k, err := serving.ReadKuadrant(c)
	if err != nil {
		return m.Fail("kuadrant", fmt.Sprintf("Cannot read the Kuadrant CR: %v.%s", err, kube.TemplateHint(err)), kube.ErrorCodeFromK8sErr(err), "kuadrant read failed")
	}
	if k.Found {
		m.Step("kuadrant", "success", fmt.Sprintf("Kuadrant %s/kuadrant exists%s.", k.Namespace, serving.ByWhom(serving.ToolOwned(k.Meta))))
		return nil, nil
	}
	rhcl, err := envkit.ReadOperator(c, serving.RHCLOperator)
	if err != nil {
		return m.Fail("kuadrant", fmt.Sprintf("Cannot read the installed operators: %v.", err), kube.ErrorCodeFromK8sErr(err), "csv read failed")
	}
	ns := kube.FirstNonEmpty(rhcl.CSVNS, serving.RHCLOperator.Namespace)
	m.Step("kuadrant", "running", "Waiting for Connectivity Link's Kuadrant API...")
	version := ""
	if _, err := kube.WaitForGone(c, envkit.OperatorTimeout, envkit.PollInterval, func(cc *kube.Client) (bool, error) {
		v, err := serving.KuadrantServedVersion(cc)
		version = v
		return v != "", err
	}); err != nil || version == "" {
		return m.Fail("kuadrant", fmt.Sprintf("Connectivity Link's Kuadrant API is not served after %s%s. Set up again to continue.", serving.ClockText(envkit.OperatorTimeout), serving.ErrSuffix(err)), kube.FirstNonEmpty(kube.ErrorCodeFromK8sErr(err), "not_ready"), "kuadrant api missing")
	}
	if rec.Shared == nil {
		rec.Shared = map[string]string{}
	}
	rec.Shared["kuadrant"] = "Kuadrant " + ns + "/kuadrant"
	if err := writePrereqRecord(c, *rec); err != nil {
		return m.Fail("kuadrant", err.Error()+" Nothing was created.", kube.ErrorCodeFromK8sErr(err), "record write failed")
	}
	if err := serving.CreateKuadrant(c, version, ns, true); err != nil {
		return m.Fail("kuadrant", fmt.Sprintf("The API server rejects the Kuadrant CR %s/kuadrant: %v. Nothing was created.", ns, err), serving.MaaSWriteCode(err), "kuadrant dry run failed")
	}
	m.Step("kuadrant", "running", fmt.Sprintf("Creating the Kuadrant CR %s/kuadrant...", ns))
	if err := serving.CreateKuadrant(c, version, ns, false); err != nil {
		return m.Fail("kuadrant", fmt.Sprintf("Failed to create the Kuadrant CR %s/kuadrant: %v. Set up again to retry.", ns, err), serving.MaaSWriteCode(err), "kuadrant create failed")
	}
	m.Logs = append(m.Logs, "OK: Kuadrant "+ns+"/kuadrant created")
	m.Step("kuadrant", "success", fmt.Sprintf("Kuadrant %s/kuadrant created; it starts Authorino, which checks each call's token.", ns))
	return nil, nil
}

// waitPrerequisitesReady waits until every component turned on reports
// Ready and, for KServe, until its dependency condition no longer names an
// operator this setup installs. A condition that is never reported is not a
// failure (an older RHOAI does not report it).
func waitPrerequisitesReady(m *serving.Run, p *featurePlan) (*types.OperationResponse, error) {
	c := m.Client
	m.Step("ready", "running", fmt.Sprintf("Waiting for RHOAI's operator to report %s ready (usually 1 to 3 minutes)...", kube.JoinTitles(componentKeys(p.components))))
	last := ""
	// refused stops the wait; extra is what fs.ready still waits for.
	refused, extra := "", ""
	var dsc rhoai.DSCRead
	done, err := kube.WaitForGone(c, prerequisiteReadyTimeout, envkit.PollInterval, func(cc *kube.Client) (bool, error) {
		r, err := rhoai.ReadFirstDSC(cc)
		if err != nil {
			return false, err
		}
		dsc = r
		var waiting []string
		for _, cp := range p.components {
			if s, msg := componentReadyCondition(r.Object, []string{cp.key}); s != "True" && (s == "False" || cp.turnOn) {
				waiting = append(waiting, fmt.Sprintf("%s: %s", cp.key, kube.FirstNonEmpty(kube.TruncateText(msg, 300), map[string]string{"False": "not ready", "": "not reported yet"}[s])))
			}
		}
		if p.fs.dependency != "" {
			s, msg := serving.DSCConditionStatus(r.Object, p.fs.dependency)
			if s == "False" {
				for _, mention := range components.ParseMissingDependencies(msg) {
					if p.installs(mention) {
						waiting = append(waiting, fmt.Sprintf("KServe does not see %s yet", mention))
					}
				}
			}
		}
		if p.fs.ready != nil {
			w, refusal, err := p.fs.ready(cc)
			if err != nil {
				return false, err
			}
			if refusal != "" {
				refused = refusal
				return true, nil
			}
			extra = w
			if w != "" {
				waiting = append(waiting, w)
			}
		}
		msg := strings.Join(waiting, "; ")
		if msg != last && msg != "" {
			last = msg
			m.Step("ready", "running", "Waiting: "+msg)
		}
		return len(waiting) == 0, nil
	})
	if err != nil {
		return m.Fail("ready", fmt.Sprintf("Stopped waiting: %v. What was set up stays; Set up again to continue.", err), kube.FirstNonEmpty(kube.ErrorCodeFromK8sErr(err), "cancelled"), "ready wait failed")
	}
	if refused != "" {
		return m.Fail("ready", refused+" What was set up stays.", "unsupported", "refused")
	}
	if !done {
		// A component never reported at all is not a failure.
		failed := extra != ""
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
			return m.Fail("ready", fmt.Sprintf("Not ready after %s (%s). What was set up stays; the tab shows each part.%s", serving.ClockText(prerequisiteReadyTimeout), last, hint), "not_ready", "not ready")
		}
		m.Step("ready", "success", fmt.Sprintf("RHOAI's operator does not report %s's readiness; continuing.", kube.JoinTitles(componentKeys(p.components))))
	} else {
		m.Step("ready", "success", fmt.Sprintf("%s %s ready.", kube.JoinTitles(componentKeys(p.components)), kube.Verb(len(p.components), "is", "are")))
	}
	note := ""
	if p.fs.dependency != "" {
		if s, msg := serving.DSCConditionStatus(dsc.Object, p.fs.dependency); s == "False" {
			note = " KServe still reports missing: " + kube.TruncateText(msg, 300) + "."
		}
	}
	slog.Info("prerequisites set up", "feature", p.fs.id, "user", kube.GetUser(c))
	return m.Done(fmt.Sprintf("What %s needs on the cluster is set up.%s", p.fs.title, note))
}

// --- turning components back (Clean up) ----------------------------------------------

// componentOnState is the state the component key is turned on with.
func componentOnState(key string) string {
	for _, fs := range featureSetups {
		for _, fc := range fs.components {
			if slices.Contains(fc.Keys, key) {
				return fc.OnState()
			}
		}
	}
	return "Managed"
}

// RestorableComponentKey: key is a component this tool turns on for a
// feature (Clean up may turn it back).
func RestorableComponentKey(key string) bool {
	for _, fs := range featureSetups {
		for _, fc := range fs.components {
			if slices.Contains(fc.Keys, key) || (fc.V1Key != "" && fc.V1Key == key) {
				return true
			}
		}
	}
	return false
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

// restoreCheck runs every check of turning key back, on dscRead: the record
// says this tool turned it on, it is still Managed, this tool's field
// manager still owns it (nobody changed it since: DSCComponentOwned),
// Argo CD does not manage the DataScienceCluster, and nothing on the
// cluster uses it (a list that fails refuses).
func restoreCheck(c *kube.Client, rec prereqRecord, dscRead rhoai.DSCRead, key string) restoreVerdict {
	rc, ok := rec.Components[key]
	if !ok {
		return restoreVerdict{kept: key + " (this tool did not turn it on)"}
	}
	prev := kube.FirstNonEmpty(rc.Previous, "Removed")
	state, _ := envkit.ComponentManagement(dscRead.Object, key)
	switch {
	case dscRead.Object == nil || state != componentOnState(key):
		return restoreVerdict{drop: true, log: key + " is " + kube.FirstNonEmpty(state, "not set") + " already"}
	case !DSCComponentOwned(dscRead.Object, key):
		return restoreVerdict{drop: true, kept: key + " (changed by someone since this tool's setup)"}
	}
	if argoCD := argo.TrackingOf(c, dscRead.Object, rhoai.DSCGroup, "DataScienceCluster"); argoCD.ManagedBy() != "" {
		return restoreVerdict{refusal: argo.GitOpsRefusal("The DataScienceCluster", argoCD.ManagedBy(), "turning "+key+" back to "+prev) + argoCD.UnconfirmedNote(), code: kube.ErrorCodeGitOpsManaged}
	}
	var users []string
	var err error
	if slices.Contains(envkit.ModelRegistryComponent.Keys, key) {
		// The model registries in the namespace RHOAI keeps them in. Only
		// this check lists them (Clean up, with the user's token), not
		// dsc.ComponentUsers, which the ServiceAccount's reads reach. On
		// RHOAI 3.6 aiHub also runs the Model Catalog, which has no
		// objects to count: the Clean up dialog says it goes too.
		ns := modelregistry.Namespace(dscRead.Object)
		if ns == "" {
			return restoreVerdict{kept: key + " (the DataScienceCluster names no namespace for model registries, so this tool cannot tell whether one is left)"}
		}
		users, err = modelregistry.Users(c, ns)
	} else if key == envkit.KueueComponent.Keys[0] {
		// The queues left on the cluster, of any owner (the tool's own are
		// deleted first, by Clean up's Kueue part), read with the user's
		// token.
		users, err = kueue.Users(c)
	} else {
		users, err = dsc.ComponentUsers(c, key)
	}
	if err != nil {
		return restoreVerdict{refusal: fmt.Sprintf("Cannot tell whether anything still uses %s, so it stays Managed: %v.%s", key, err, kube.TemplateHint(err)), code: kube.ErrorCodeFromK8sErr(err)}
	}
	if len(users) > 0 {
		shown := users
		if len(shown) > 3 {
			shown = append(append([]string{}, users[:3]...), fmt.Sprintf("%d more", len(users)-3))
		}
		return restoreVerdict{kept: fmt.Sprintf("%s (in use: %s)", key, kube.JoinTitles(shown))}
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
func RestoreComponents(c *kube.Client, keys []string) (*types.OperationResponse, error) {
	rec, err := ReadPrereqRecord(c)
	if err != nil {
		return &types.OperationResponse{Success: false, Message: err.Error() + " Nothing was changed.", ErrorCode: kube.ErrorCodeFromK8sErr(err)}, nil
	}
	dsc, err := rhoai.ReadFirstDSC(c)
	if err != nil {
		return &types.OperationResponse{Success: false, Message: fmt.Sprintf("Cannot read the DataScienceCluster; nothing was changed: %v.%s", err, kube.TemplateHint(err)), ErrorCode: kube.ErrorCodeFromK8sErr(err)}, nil
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
			msg += " Already turned back: " + kube.JoinTitles(done) + "."
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
			v := restoreVerdict{restore: true, prev: kube.FirstNonEmpty(rec.Components[key].Previous, "Removed")}
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
			meta, _ := dsc.Object["metadata"].(map[string]any)
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
			if !kube.IsK8sConflict(err) {
				return stop(fmt.Sprintf("Failed to turn %s back to %s: %v.%s", key, v.prev, err, kube.TemplateHint(err)), kube.ErrorCodeFromK8sErr(err))
			}
			if attempt == 3 {
				return stop(fmt.Sprintf("The DataScienceCluster kept changing while %s was turned back (someone else is editing it), so it stays Managed. Clean up again later.", key), "conflict")
			}
			logs = append(logs, "The DataScienceCluster changed since it was read; reading it again and checking again.")
			if dsc, err = rhoai.ReadFirstDSC(c); err != nil {
				return stop(fmt.Sprintf("The DataScienceCluster changed since it was read, and it cannot be read again, so %s stays Managed: %v.%s", key, err, kube.TemplateHint(err)), kube.ErrorCodeFromK8sErr(err))
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
		msg = append(msg, kube.JoinTitles(done)+", as before setup.")
	}
	if len(kept) > 0 {
		msg = append(msg, "Kept Managed: "+strings.Join(kept, "; ")+".")
	}
	if len(msg) == 0 {
		msg = append(msg, "No component to turn back.")
	}
	slog.Info("components restored", "done", len(done), "kept", len(kept), "user", kube.GetUser(c))
	return &types.OperationResponse{Success: true, Message: strings.Join(msg, " "), Logs: logs}, nil
}
