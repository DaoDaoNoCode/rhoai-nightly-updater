package features

import (
	"fmt"
	"log/slog"
	"maps"
	"strings"
	"sync"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/serving"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// A set of prerequisites: what several features need, set up as one
// operation (an environment recipe's "prerequisites" step). The Environment
// tabs keep RunPrerequisites, one feature at a time.

// SetItem is one feature of a set of prerequisites.
type SetItem struct {
	Feature string
	// Exposure is how the model is reached (models only), as in
	// types.PrerequisitesRequest.
	Exposure string
	// OperatorsOnly plans only the feature's operators (and its Kuadrant
	// CR), not its components, dependency condition or ready hook. A
	// feature whose operators come first (kueue) is in a set only so: its
	// component is turned on by its own setup afterwards.
	OperatorsOnly bool
}

// setupOf is the feature setup an item plans.
func setupOf(it SetItem) featureSetup {
	fs := featureSetupFor(it.Feature, it.Exposure)
	if it.OperatorsOnly {
		fs.components, fs.dependency, fs.ready = nil, "", nil
	}
	return fs
}

// FeatureTitle names a feature for people ("Pipelines"), or returns id.
func FeatureTitle(id string) string {
	if fs, ok := featureSetupOf(id); ok {
		return fs.title
	}
	return id
}

// ValidatePrerequisitesSet returns why items cannot run as one set, or "".
func ValidatePrerequisitesSet(items []SetItem) string {
	if len(items) == 0 {
		return "a set of prerequisites names at least one feature"
	}
	seen := map[string]bool{}
	for _, it := range items {
		if problem := ValidatePrerequisitesRequest(types.PrerequisitesRequest{Feature: it.Feature, Exposure: it.Exposure}); problem != "" {
			return problem
		}
		if seen[it.Feature] {
			return fmt.Sprintf("feature %s is named twice", it.Feature)
		}
		seen[it.Feature] = true
		if fs, _ := featureSetupOf(it.Feature); fs.operatorsFirst && !it.OperatorsOnly {
			return fmt.Sprintf("feature %s turns its component on after its operators are installed, so a set takes only its operators", it.Feature)
		}
	}
	return ""
}

// GetPrerequisitesSetStatus reads what each feature of items needs on the
// cluster, planned as RunPrerequisitesSet plans it.
func GetPrerequisitesSetStatus(c *kube.Client, items []SetItem) (*types.PrerequisitesStatus, error) {
	r := readClusterForPrerequisites(c)
	out := &types.PrerequisitesStatus{Features: map[string]types.FeaturePrerequisites{}}
	for _, it := range items {
		if it.Feature == "models" {
			out.ModelsExposure = it.Exposure
		}
		out.Features[it.Feature] = planFeature(c, setupOf(it), r).status()
	}
	return out, nil
}

// setComponent is a component of a set, with the feature it is recorded
// for: the first in the set that needs it.
type setComponent struct {
	cp      componentPlan
	feature string
}

// setUnion is what a set of plans changes, each part once.
type setUnion struct {
	components []setComponent
	operators  []operatorPlan
	// kuadrant: a plan makes the Kuadrant CR; missing: it does not exist
	// yet, in kuadrantNS.
	kuadrant, kuadrantMissing bool
	kuadrantNS                string
}

func unionOf(plans []*featurePlan) setUnion {
	var u setUnion
	seenComp, seenOp := map[string]bool{}, map[string]bool{}
	for _, p := range plans {
		for _, cp := range p.components {
			if !seenComp[cp.key] {
				seenComp[cp.key] = true
				u.components = append(u.components, setComponent{cp: cp, feature: p.fs.id})
			}
		}
		for _, o := range p.operators {
			if !seenOp[o.op.ID] {
				seenOp[o.op.ID] = true
				u.operators = append(u.operators, o)
			}
		}
		if p.fs.kuadrant {
			u.kuadrant = true
			for _, pt := range p.parts {
				if pt.ID == "kuadrant" && pt.State == "missing" {
					u.kuadrantMissing = true
				}
			}
		}
	}
	u.kuadrantNS = serving.RHCLOperator.Namespace
	for _, o := range u.operators {
		if o.op.ID == serving.RHCLOperator.ID {
			u.kuadrantNS = kube.FirstNonEmpty(o.st.CSVNS, serving.RHCLOperator.Namespace)
		}
	}
	return u
}

func (u setUnion) componentPlans() []componentPlan {
	out := make([]componentPlan, 0, len(u.components))
	for _, sc := range u.components {
		out = append(out, sc.cp)
	}
	return out
}

// record is base with everything the set changes, and whether that is
// anything: the components it turns on (with their value before), the
// operators it installs and the Kuadrant CR it makes.
func (u setUnion) record(base prereqRecord) (prereqRecord, bool) {
	rec := prereqRecord{Components: maps.Clone(base.Components), Shared: maps.Clone(base.Shared)}
	if rec.Components == nil {
		rec.Components = map[string]prereqComponent{}
	}
	if rec.Shared == nil {
		rec.Shared = map[string]string{}
	}
	changed := false
	for _, sc := range u.components {
		if sc.cp.turnOn {
			rec.Components[sc.cp.key] = prereqComponent{Previous: sc.cp.state, Feature: sc.feature}
			changed = true
		}
	}
	for _, o := range u.operators {
		if o.install != nil {
			rec.Shared[o.op.ID] = sharedOperatorEntry(o)
			changed = true
		}
	}
	if u.kuadrantMissing {
		rec.Shared["kuadrant"] = "Kuadrant " + u.kuadrantNS + "/kuadrant"
		changed = true
	}
	return rec, changed
}

// RunPrerequisitesSet sets up what every feature of items needs on the
// cluster as one operation, with RunPrerequisites' steps: "prerequisites"
// (every read for every feature, and a server-side dry run of every write
// for all of them, before any change), "dsc", one step per operator,
// "kuadrant" and "ready".
//
// Prerequisite runs must never overlap (each writes the whole record it
// read), so a set is one run: it plans the union of the features' plans
// and writes the record once, before any change; a component's entry
// names the first feature in items that needs it, as that feature's own
// setup would have. Then it turns the components on (RHOAI's operator
// deploys them while the operators install), creates every missing
// Subscription, and waits for all operators at once: every wait starts at
// the same moment with envkit.OperatorTimeout, so they share one deadline
// and take as long as the slowest, not their sum. Then it makes the
// Kuadrant CR once, and waits once until every feature's components,
// dependency condition and ready hook report ready. A feature that
// refuses refuses the whole set, before any change. Only what is missing
// is done; running it again continues where a failed run stopped.
func RunPrerequisitesSet(c *kube.Client, items []SetItem, onStep func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
	m := &serving.Run{Client: c, Emit: onStep, Activity: "setup-prerequisites", Target: "prerequisites"}
	if problem := ValidatePrerequisitesSet(items); problem != "" {
		return m.Fail("prerequisites", problem, "validation", "invalid request")
	}
	titles, ids := make([]string, 0, len(items)), make([]string, 0, len(items))
	for _, it := range items {
		titles, ids = append(titles, FeatureTitle(it.Feature)), append(ids, it.Feature)
	}
	what, need := kube.JoinTitles(titles), kube.Verb(len(items), "needs", "need")
	m.Target = what + " prerequisites"
	m.Step("prerequisites", "running", fmt.Sprintf("Checking what %s %s on the cluster...", what, need))
	r := readClusterForPrerequisites(c)
	plans := make([]*featurePlan, 0, len(items))
	for _, it := range items {
		p := planFeature(c, setupOf(it), r)
		if p.refusal != "" {
			return m.Fail("prerequisites", p.fs.title+": "+p.refusal, kube.FirstNonEmpty(p.code, "prerequisites"), "check failed")
		}
		plans = append(plans, p)
	}
	u := unionOf(plans)
	// Every write of every feature as a server-side dry run first.
	for _, sc := range u.components {
		cp := sc.cp
		if !cp.turnOn {
			continue
		}
		if _, err := patchDSCComponent(c, cp.key, cp.fc.OnState(), "", true); err != nil {
			return m.Fail("prerequisites", fmt.Sprintf("The API server rejects setting %s to %s in the DataScienceCluster: %v. Nothing was changed.%s", cp.key, cp.fc.OnState(), err, kube.TemplateHint(err)), serving.MaaSWriteCode(err), "dry run failed")
		}
	}
	for _, o := range u.operators {
		if o.install == nil {
			continue
		}
		if _, err := envkit.CreateOperatorObjects(c, o.install, true); err != nil {
			return m.Fail("prerequisites", fmt.Sprintf("The API server rejects %s's install: %v. Nothing was changed.%s", o.op.Title, err, kube.TemplateHint(err)), serving.MaaSWriteCode(err), "dry run failed")
		}
	}
	// The record, once, before any change.
	rec, changed := u.record(r.rec)
	if changed {
		if err := writePrereqRecord(c, rec); err != nil {
			return m.Fail("prerequisites", err.Error()+" Nothing was changed.", kube.ErrorCodeFromK8sErr(err), "record write failed")
		}
	}
	m.Step("prerequisites", "success", "Everything Set up changes is checked.")

	if len(u.components) > 0 {
		if r, err := turnOnComponents(m, u.componentPlans(), plans[0].dscName, func() (*types.OperationResponse, error) { return nil, nil }); r != nil || err != nil {
			return r, err
		}
	}
	var waits []operatorWait
	for _, o := range u.operators {
		w, r, err := startOperator(m, o, nil)
		if r != nil || err != nil {
			return r, err
		}
		if w != nil {
			waits = append(waits, *w)
		}
	}
	if r, err := waitOperators(m, waits); r != nil || err != nil {
		return r, err
	}
	if u.kuadrant {
		if r, err := ensureKuadrant(m, &rec, rec.Shared["kuadrant"] != ""); r != nil || err != nil {
			return r, err
		}
	}
	note := ""
	if needsReadyWait(plans) {
		n, r, err := waitReady(m, plans)
		if r != nil || err != nil {
			return r, err
		}
		note = n
	}
	slog.Info("prerequisites set up", "features", strings.Join(ids, ","), "user", kube.GetUser(c))
	return m.Done(fmt.Sprintf("What %s %s on the cluster is set up.%s", what, need, note))
}

// needsReadyWait: a plan has a component, a dependency condition or a
// ready hook to wait for.
func needsReadyWait(plans []*featurePlan) bool {
	for _, p := range plans {
		if len(p.components) > 0 || p.fs.dependency != "" || p.fs.ready != nil {
			return true
		}
	}
	return false
}

// waitOperators waits for every Subscription of waits at once, each on
// its own step; it returns the first failure in waits' order (the others
// are reported failed on their steps), or nil when all are installed.
func waitOperators(m *serving.Run, waits []operatorWait) (*types.OperationResponse, error) {
	if len(waits) == 0 {
		return nil, nil
	}
	var mu sync.Mutex
	say := func(id, status, msg string) {
		mu.Lock()
		defer mu.Unlock()
		m.Step(id, status, msg)
	}
	fails := make([]*serving.OperatorWaitFailure, len(waits))
	var wg sync.WaitGroup
	for i, w := range waits {
		wg.Go(func() {
			defer func() {
				if p := recover(); p != nil {
					slog.Error("operator wait panicked", "operator", w.title, "panic", p)
					fails[i] = &serving.OperatorWaitFailure{Message: fmt.Sprintf("The wait for %s stopped on an internal error. Its Subscription stays; Set up again to continue.", w.title), Code: "internal", Activity: "operator wait failed"}
				}
			}()
			fails[i] = serving.AwaitOperator(m.Client, w.step, w.ns, w.name, w.title, say)
		})
	}
	wg.Wait()
	first := -1
	for i, f := range fails {
		switch {
		case f == nil:
		case first < 0:
			first = i
		default:
			m.Step(waits[i].step, "failed", f.Message)
		}
	}
	if first < 0 {
		return nil, nil
	}
	f := fails[first]
	return m.Fail(waits[first].step, f.Message, f.Code, f.Activity)
}
