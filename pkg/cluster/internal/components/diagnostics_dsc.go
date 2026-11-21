package components

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/report"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
)

// DataScienceCluster / DSCInitialization readiness.
//
// The operator rolls component and module health up into the DSC "Ready"
// condition. Conditions with severity "Info" are informational (for example
// a component set to Removed, or an optional dependency that is not
// installed) and do not participate in readiness: "Only dependents with
// ConditionSeverityError (or empty severity, which defaults to Error)
// participate in happiness computation. Conditions with
// ConditionSeverityInfo are ignored." (odh-platform-utilities
// pkg/controller/conditions/conditions.go). The CRD schema says severity
// "defaults to Error" when unset. DSCInitialization reports status.phase
// Ready, Progressing or Error (rhods-operator rhoai-3.6
// internal/controller/status/status.go).
//
// Each failing condition is classified, and the DSC problem points to the
// problem that fixes its cause: a missing prerequisite operator
// (prerequisites.go), a module operator in back-off (module_backoff.go), an
// upgrade gate, an object the operator cannot update
// (diagnostics_apply_failures.go) or a dead CRD conversion webhook
// (diagnostics_webhooks.go). The severity decides whether a missing
// prerequisite blocks Ready or only gates an optional feature: live, after
// cert-manager and JobSet were installed, the DSC was Ready while
// KserveLLMInferenceServiceWideEPDependencies (severity Info) still said
// "LeaderWorkerSet not installed".

// conditionFailing: a non-Info condition that is not True (Degraded, the
// one negative-polarity condition, when True). The roll-ups Ready and
// Progressing are not counted.
func conditionFailing(cond rhoai.DSCCondition) bool {
	if cond.Type == "Ready" || cond.Type == "Progressing" || strings.EqualFold(cond.Severity, "Info") {
		return false
	}
	if cond.Type == "Degraded" {
		return cond.Status == "True"
	}
	return cond.Status != "True"
}

func ConditionLine(cond rhoai.DSCCondition) string {
	line := fmt.Sprintf("%s=%s", cond.Type, cond.Status)
	if cond.Reason != "" {
		line += fmt.Sprintf(" (%s)", cond.Reason)
	}
	if msg := kube.TruncateText(cond.Message, 300); msg != "" {
		line += ": " + msg
	}
	return line
}

// FailingConditions returns the failing conditions as lines. Duplicate
// messages are reported once.
func FailingConditions(conds []rhoai.DSCCondition) []string {
	seen := map[string]bool{}
	var out []string
	for _, cond := range conds {
		if !conditionFailing(cond) {
			continue
		}
		msg := kube.TruncateText(cond.Message, 300)
		if seen[msg] && msg != "" {
			continue
		}
		seen[msg] = true
		out = append(out, ConditionLine(cond))
	}
	sort.Strings(out)
	return out
}

// ClassifiedCondition is one condition with what it says about the cause.
type ClassifiedCondition struct {
	Source     string // "DataScienceCluster default-dsc", "Trainer default-trainer"
	Module     string // lower-case module kind, "" when not about one module
	Cond       rhoai.DSCCondition
	Blocking   bool // counts for Ready (severity not Info)
	Deps       []*Dependency
	Gate       bool
	Stale      bool
	ApplyIDs   []string
	Conversion bool
}

func (cc *ClassifiedCondition) label() string { return cc.Source + ": " + ConditionLine(cc.Cond) }

// conditionModule maps a condition type to the module it is about: the
// longest served module kind that prefixes it (KserveLLMInferenceService-
// Dependencies is about kserve).
func conditionModule(condType string, kinds []string) string {
	lower := strings.ToLower(condType)
	best := ""
	for _, k := range kinds {
		if strings.HasPrefix(lower, k) && len(k) > len(best) {
			best = k
		}
	}
	return best
}

// moduleObject is a module CR with its lower-case kind.
type moduleObject struct {
	Kind, Module string
	Obj          rhoai.DSCObject
}

// staleSeen remembers, per module CR, the generation its operator lagged
// behind and when a scan first saw that. There is no other trustworthy
// clock: a condition's lastTransitionTime says nothing about when the spec
// changed, so a restart could be offered right after a spec change. A CR
// counts as stale only when two scans at least ModuleStaleAfter apart saw
// the same generation unobserved. The memory is the process's own: after a
// restart of this app, the wait starts again (never a false positive).
var staleSeen = struct {
	sync.Mutex
	m map[string]staleObservation
}{m: map[string]staleObservation{}}

var _ = kube.RegisterCachePurge(func() {
	staleSeen.Lock()
	staleSeen.m = map[string]staleObservation{}
	staleSeen.Unlock()
})

type staleObservation struct {
	generation int64
	first      time.Time
}

// staleFor reports how long the CR's operator has been seen lagging behind
// its current generation, once that is at least ModuleStaleAfter.
func (m moduleObject) staleFor(c *kube.Client, now time.Time) (time.Duration, bool) {
	key := strings.Join([]string{c.BaseURL(), m.Module, m.Obj.Metadata.Name, m.Obj.Metadata.UID}, "|")
	st, gen := m.Obj.Status, m.Obj.Metadata.Generation
	staleSeen.Lock()
	defer staleSeen.Unlock()
	if st.ObservedGeneration <= 0 || st.ObservedGeneration >= gen {
		delete(staleSeen.m, key)
		return 0, false
	}
	seen, ok := staleSeen.m[key]
	if !ok || seen.generation != gen {
		staleSeen.m[key] = staleObservation{generation: gen, first: now}
		return 0, false
	}
	if age := now.Sub(seen.first); age >= ModuleStaleAfter {
		return age, true
	}
	return 0, false
}

func listModuleObjects(c *kube.Client, api rhoai.ComponentAPI) ([]moduleObject, error) {
	type result struct {
		items []rhoai.DSCObject
		err   error
	}
	results := make([]result, len(api.Kinds))
	kube.ParallelFor(len(api.Kinds), 6, func(i int) {
		body, _, err := c.Get(api.Kinds[i].ListPath())
		if err != nil {
			results[i].err = fmt.Errorf("list %s: %w", api.Kinds[i].Resource, err)
			return
		}
		var list struct {
			Items []rhoai.DSCObject `json:"items"`
		}
		if err := json.Unmarshal(body, &list); err != nil {
			results[i].err = fmt.Errorf("parse %s: %w", api.Kinds[i].Resource, err)
		}
		results[i].items = list.Items
	})
	var out []moduleObject
	var errs []string
	for i, k := range api.Kinds {
		if results[i].err != nil {
			errs = append(errs, results[i].err.Error())
			continue
		}
		for _, it := range results[i].items {
			out = append(out, moduleObject{Kind: k.Kind, Module: strings.ToLower(k.Kind), Obj: it})
		}
	}
	if len(errs) > 0 {
		return out, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return out, nil
}

// dscAnalysis is the classified state of the DataScienceCluster.
type dscAnalysis struct {
	Name       string
	Ready      *rhoai.DSCCondition
	DSCConds   []*ClassifiedCondition // the DSC's failing conditions, and Info ones naming a dependency
	ModConds   []*ClassifiedCondition // module CR conditions with a classified cause
	Deps       []*Dependency
	Backoffs   []*moduleBackoff
	ModulesErr error
	APIErr     error
}

func (a *dscAnalysis) backoff(module string) *moduleBackoff {
	for _, b := range a.Backoffs {
		if b.Module == module {
			return b
		}
	}
	return nil
}

func (a *dscAnalysis) Gates() []*ClassifiedCondition {
	var out []*ClassifiedCondition
	for _, cc := range a.DSCConds {
		if cc.Gate {
			out = append(out, cc)
		}
	}
	return out
}

// Classify fills in what the condition's message says.
func (cc *ClassifiedCondition) Classify(deps map[string]*Dependency, order *[]*Dependency) {
	msg := cc.Cond.Message
	lower := strings.ToLower(msg)
	cc.Gate = IsUpgradeGateCondition(cc.Cond)
	cc.Stale = strings.Contains(lower, "observedgeneration < generation") || strings.Contains(lower, "status is stale")
	cc.Conversion = strings.Contains(lower, "conversion webhook")
	for _, f := range ParseApplyFailures(msg) {
		if !slices.Contains(cc.ApplyIDs, f.ProblemID()) {
			cc.ApplyIDs = append(cc.ApplyIDs, f.ProblemID())
		}
	}
	if cc.Cond.Status == "True" && cc.Cond.Type != "Degraded" {
		return
	}
	for _, om := range ParseMissingOperands(msg) {
		key := "operand-" + strings.ToLower(om.Kind)
		d := deps[key]
		if d == nil {
			mention := om.Kind + " CR"
			if om.Name != "" {
				mention = om.Kind + "/" + om.Name
			}
			d = &Dependency{Mention: mention, Key: key, OperandKind: om.Kind, OperandName: om.Name}
			deps[key] = d
			*order = append(*order, d)
		}
		d.Reporters = append(d.Reporters, cc)
		cc.Deps = append(cc.Deps, d)
	}
	for _, name := range ParseMissingDependencies(msg) {
		key := NormalizeOperatorName(name)
		if key == "" {
			continue
		}
		d := deps[key]
		if d == nil {
			d = &Dependency{Mention: name, Key: key}
			deps[key] = d
			*order = append(*order, d)
		}
		d.Reporters = append(d.Reporters, cc)
		cc.Deps = append(cc.Deps, d)
	}
}

// AnalyzeDSC classifies the DSC's conditions and those of the module CRs,
// resolves the prerequisites they name, and finds module operators in
// back-off. appNS is the DSCInitialization's applications namespace.
func AnalyzeDSC(c *kube.Client, dsc rhoai.DSCObject, now time.Time) *dscAnalysis {
	a := &dscAnalysis{Name: dsc.Metadata.Name}
	deps := map[string]*Dependency{}
	api, apiErr := rhoai.DiscoverComponentAPI(c)
	a.APIErr = apiErr
	var kinds []string
	for _, k := range api.Kinds {
		kinds = append(kinds, strings.ToLower(k.Kind))
	}
	source := "DataScienceCluster " + dsc.Metadata.Name
	blockingModule := map[string]bool{}
	for i := range dsc.Status.Conditions {
		cond := dsc.Status.Conditions[i]
		if cond.Type == "Ready" {
			a.Ready = &dsc.Status.Conditions[i]
		}
		failing := conditionFailing(cond)
		cc := &ClassifiedCondition{Source: source, Module: conditionModule(cond.Type, kinds), Cond: cond, Blocking: failing}
		if cond.Type == "Ready" {
			if !IsUpgradeGateCondition(cond) || cond.Status == "True" {
				continue
			}
			cc.Blocking = true
		} else if !failing && (cond.Status == "True" || len(ParseMissingDependencies(cond.Message)) == 0) {
			continue
		}
		cc.Classify(deps, &a.Deps)
		a.DSCConds = append(a.DSCConds, cc)
		if cc.Blocking && cc.Module != "" {
			blockingModule[cc.Module] = true
		}
	}
	if len(a.DSCConds) == 0 {
		return a
	}

	var modules []moduleObject
	if api.Served() {
		modules, a.ModulesErr = listModuleObjects(c, api)
	}
	staleModules := map[string]string{}
	for _, m := range modules {
		src := m.Kind + " " + m.Obj.Metadata.Name
		for _, cond := range m.Obj.Status.Conditions {
			if cond.Status == "True" && cond.Type != "Degraded" {
				continue
			}
			cc := &ClassifiedCondition{Source: src, Module: m.Module, Cond: cond, Blocking: blockingModule[m.Module]}
			cc.Classify(deps, &a.Deps)
			if len(cc.Deps) > 0 || cc.Stale {
				a.ModConds = append(a.ModConds, cc)
			}
		}
		if age, ok := m.staleFor(c, now); ok {
			staleModules[m.Module] = fmt.Sprintf("%s has generation %d but its operator last reported observedGeneration %d, unchanged across scans for %s",
				src, m.Obj.Metadata.Generation, m.Obj.Status.ObservedGeneration, FormatDuration(age))
		}
	}
	ResolveDependencies(c, a.Deps)

	// Modules whose operator has not retried.
	reasons := map[string][]string{}
	addReason := func(module, reason string) {
		if module != "" && !slices.Contains(reasons[module], reason) {
			reasons[module] = append(reasons[module], reason)
		}
	}
	for _, cc := range append(append([]*ClassifiedCondition{}, a.DSCConds...), a.ModConds...) {
		if cc.Module == "" || len(cc.Deps) == 0 {
			continue
		}
		var installed []string
		ready := true
		for _, d := range cc.Deps {
			if !d.Satisfied() || (!d.CSV.Changed.IsZero() && now.Sub(d.CSV.Changed) < prerequisiteGrace) {
				ready = false
				break
			}
			line := fmt.Sprintf("%s (CSV %s/%s Succeeded", d.DisplayName(), d.CSV.Namespace, d.CSV.Name)
			if !d.CSV.Changed.IsZero() {
				line += " " + FormatDuration(now.Sub(d.CSV.Changed)) + " ago"
			}
			switch d.OperandSt {
			case OperandPresent:
				line += fmt.Sprintf("; %s exists", d.Operand.Ref())
			}
			installed = append(installed, line+")")
		}
		if ready {
			// The condition line ends with the operator's own message, usually a
			// full sentence: the tool's note starts a new one.
			addReason(cc.Module, fmt.Sprintf("%s. It still reports %s, but it is installed: %s",
				strings.TrimRight(cc.label(), ". "), joinMentions(cc.Deps), strings.Join(installed, "; ")))
		}
	}
	for module, why := range staleModules {
		addReason(module, why)
	}
	// Without the module CRs, a stale DSC condition stays a cause without
	// a restart: its age does not show when the spec changed.
	moduleNames := make([]string, 0, len(reasons))
	for m := range reasons {
		moduleNames = append(moduleNames, m)
	}
	sort.Strings(moduleNames)
	operators := NewModuleOperatorFinder(c, nil)
	for _, m := range moduleNames {
		b := &moduleBackoff{Module: m, Reasons: reasons[m], Blocking: blockingModule[m]}
		b.Deployment, b.FindErr = FindModuleOperator(c, operators, m)
		if b.Deployment != nil {
			b.Restart = AssessRestart(c, *b.Deployment)
		}
		a.Backoffs = append(a.Backoffs, b)
	}
	return a
}

func joinMentions(deps []*Dependency) string {
	var names []string
	for _, d := range deps {
		if d.OperandKind != "" {
			names = append(names, d.Mention+" as missing")
		} else {
			names = append(names, d.Mention+" as not installed")
		}
	}
	return strings.Join(names, " and ")
}

// analyzeCurrentDSC reads the DataScienceCluster and DSCInitialization and
// analyses them (the restart fix checks again with it).
func analyzeCurrentDSC(c *kube.Client, now time.Time) (*dscAnalysis, error) {
	dscs, apiFound, err := rhoai.ListFirstServed(c, rhoai.DSCGroup, rhoai.DSCListFmt, "datascienceclusters", rhoai.DSCFallbackVersions)
	if err != nil {
		return nil, err
	}
	if !apiFound || len(dscs) == 0 {
		return nil, fmt.Errorf("no DataScienceCluster exists")
	}
	return AnalyzeDSC(c, dscs[0], now), nil
}

// conditionCauses explains one DSC condition and names the problems that
// fix it. titles maps problem IDs to titles for those this check reports.
func (a *dscAnalysis) conditionCauses(cc *ClassifiedCondition, titles map[string]string) (causes, related []string) {
	add := func(cause, id string) {
		if t := titles[id]; t != "" {
			cause += fmt.Sprintf(" (see %q)", t)
		}
		if !slices.Contains(causes, cause) {
			causes = append(causes, cause)
		}
		if id != "" && !slices.Contains(related, id) {
			related = append(related, id)
		}
	}
	b := a.backoff(cc.Module)
	explain := func(cc *ClassifiedCondition) {
		if cc.Gate {
			add("an upgrade gate holds provisioning", DSCGateProblemID)
		}
		for _, d := range cc.Deps {
			switch {
			case d.installed() && d.OperandSt == operandMissing:
				add(fmt.Sprintf("%s is installed, but %s does not exist", d.DisplayName(), d.Operand.Ref()), d.ProblemID())
			case d.operandProblem():
				add(fmt.Sprintf("%s is installed, but whether %s exists could not be checked", d.DisplayName(), d.Operand.Ref()), d.ProblemID())
			case !d.Satisfied():
				kind := "missing prerequisite operator"
				if !cc.Blocking {
					kind = "optional prerequisite operator"
				}
				state := "not installed"
				if d.CSV != nil {
					state = "installed but not ready (CSV " + kube.NonEmpty(d.CSV.Phase, "phase unknown") + ")"
				}
				add(fmt.Sprintf("%s %s is %s", kind, d.DisplayName(), state), d.ProblemID())
			case b != nil:
				add(fmt.Sprintf("%s is installed now, but the %s module operator has not retried", d.DisplayName(), cc.Module), b.problemID())
			default:
				add(fmt.Sprintf("%s is installed now; the module picks it up on its next retry", d.DisplayName()), "")
			}
		}
		if cc.Stale {
			if b != nil {
				add(fmt.Sprintf("the %s module status is stale and its operator has not reconciled", cc.Module), b.problemID())
			} else {
				add("the module status is stale (its operator is still reconciling)", "")
			}
		}
		for _, id := range cc.ApplyIDs {
			if strings.HasPrefix(id, LeftoverIDPrefix) {
				add(fmt.Sprintf("an object left over from an earlier release (see the %q check)", LeftoversCheckName), id)
				continue
			}
			add(fmt.Sprintf("an object the operator cannot update (see the %q check)", ApplyFailuresCheckName), id)
		}
		if cc.Conversion {
			add("a CRD conversion webhook cannot be called (see the \"Stale webhooks\" check)", "stale-crd-conversion")
		}
	}
	explain(cc)
	if len(causes) == 0 && cc.Module != "" {
		// The DSC only relays the module's state: use the module CR's.
		for _, mc := range a.ModConds {
			if mc.Module == cc.Module {
				explain(mc)
			}
		}
		if len(causes) == 0 && b != nil {
			add(fmt.Sprintf("the %s module operator has not retried", cc.Module), b.problemID())
		}
	}
	if len(causes) == 0 && cc.Module == "" {
		// A roll-up ("Some modules are not ready: trainer"): the causes of
		// the modules it names.
		for _, m := range ParseNotReadyModules(cc.Cond.Message) {
			var mc []string
			for _, x := range append(append([]*ClassifiedCondition{}, a.DSCConds...), a.ModConds...) {
				if x == cc || x.Module != m {
					continue
				}
				c2, r2 := a.conditionCauses(x, titles)
				for _, c := range c2 {
					if !slices.Contains(mc, c) {
						mc = append(mc, c)
					}
				}
				for _, id := range r2 {
					if !slices.Contains(related, id) {
						related = append(related, id)
					}
				}
			}
			if len(mc) == 0 {
				causes = append(causes, fmt.Sprintf("module %s is not ready; its own condition has no classified cause", m))
				continue
			}
			causes = append(causes, fmt.Sprintf("module %s: %s", m, strings.Join(mc, "; ")))
		}
	}
	return causes, related
}

// notReadyList matches the operator's roll-up messages, "Some modules are
// not ready: trainer" and "Some components are not ready: kserve, ray".
var notReadyList = regexp.MustCompile(`(?i)\b(?:modules?|components?)\s+(?:are|is)\s+not\s+ready:\s*([a-z0-9][a-z0-9 ,_-]*)`)

// ParseNotReadyModules returns the lower-case module names of a roll-up.
func ParseNotReadyModules(msg string) []string {
	m := notReadyList.FindStringSubmatch(msg)
	if m == nil {
		return nil
	}
	var out []string
	for _, f := range strings.FieldsFunc(m[1], func(r rune) bool { return r == ',' || r == ' ' }) {
		if f = strings.ToLower(strings.TrimSpace(f)); f != "" && f != "and" && !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	return out
}

const DSCGateProblemID = "dsc-upgrade-gate"

// GateProblem reports the DSC's upgrade gate conditions with the exact
// acknowledgement steps (rhods-operator docs/upgrade-ordering.md "Admin
// ack gates"). A gate whose target release the operator cannot resolve is
// not acknowledged away: it means the operator is older than what it found.
func GateProblem(c *kube.Client, gates []*ClassifiedCondition) report.Problem {
	var evidence []string
	ack, unresolved := false, false
	for _, g := range gates {
		evidence = append(evidence, g.label())
		lower := strings.ToLower(g.Cond.Message)
		if strings.Contains(lower, "failed to resolve upgrade gate") || strings.Contains(lower, "unable to determine target release") {
			unresolved = true
		} else {
			ack = true
		}
	}
	p := report.Problem{
		ID:              DSCGateProblemID,
		Severity:        "critical",
		Title:           "The DataScienceCluster waits on an upgrade gate",
		AffectedObjects: []string{"ConfigMap " + rhoai.SubNS + "/odh-upgrade-acks"},
	}
	var desc, fixes []string
	if ack {
		keys, values, err := PendingUpgradeAckKeys(c)
		desc = append(desc, "Before provisioning a new version, the operator checks upgrade gates. An unacknowledged gate blocks provisioning until an administrator sets its key to \"true\" in ConfigMap odh-upgrade-acks. This is a deliberate manual step, not a bug.")
		switch {
		case err != nil:
			evidence = append(evidence, fmt.Sprintf("ConfigMap %s/odh-upgrade-acks: %v%s", rhoai.SubNS, err, kube.TemplateHint(err)))
			fixes = append(fixes, "Read what the gate is about, then set its key to \"true\" in ConfigMap odh-upgrade-acks (replace <key> in the command).")
			p.TechnicalCmd = UpgradeAckCommand("<key>")
		case len(keys) == 0:
			evidence = append(evidence, "ConfigMap odh-upgrade-acks has no key that is not \"true\"")
			fixes = append(fixes, "No key is waiting in odh-upgrade-acks; read the ConfigMap and the operator logs for the gate's name.")
			p.TechnicalCmd = kube.ShellCommand("oc", "get", "configmap", "odh-upgrade-acks", "-n", rhoai.SubNS, "-o", "yaml")
		default:
			var cmds []string
			for _, k := range keys {
				evidence = append(evidence, fmt.Sprintf("Not acknowledged: %s=%q", k, values[k]))
				cmds = append(cmds, UpgradeAckCommand(k))
			}
			fixes = append(fixes, fmt.Sprintf("Read what each gate is about (the operator's release notes name it), then acknowledge %s with the commands below.", kube.Verb(len(keys), "it", "them")))
			p.TechnicalCmd = strings.Join(cmds, "\n")
		}
	}
	if unresolved {
		desc = append(desc, "The operator cannot work out which release the gates are for. That happens when an older operator runs over resources a newer version created (a downgrade, or an install of an older build); acknowledging does not help.")
		fixes = append(fixes, "Install the operator version that created these resources, or a newer one (Update on the Status page).")
		if p.TechnicalCmd == "" {
			p.TechnicalCmd = kube.ShellCommand("oc", "get", "datasciencecluster", "-o", "jsonpath={.items[0].status.release.version}")
		}
	}
	p.Description = strings.Join(desc, " ")
	p.Evidence = evidence
	p.Fix = strings.Join(fixes, " ")
	return p
}

// NotReadyProblem lists each failing DSC condition with its classified
// cause and links the problems that fix them. It returns the number of
// failing conditions.
func (a *dscAnalysis) NotReadyProblem(dsc rhoai.DSCObject, titles map[string]string, dsciProblem *report.Problem) (report.Problem, int) {
	ready := a.Ready
	evidence := []string{fmt.Sprintf("DataScienceCluster %s: Ready=%s (%s): %s", dsc.Metadata.Name, ready.Status, ready.Reason, kube.TruncateText(ready.Message, 400))}
	var related, unclassified, fixes []string
	failing := 0
	seen := map[string]bool{}
	conds := append([]rhoai.DSCCondition{}, dsc.Status.Conditions...)
	sort.SliceStable(conds, func(i, j int) bool { return conds[i].Type < conds[j].Type })
	for _, cond := range conds {
		var cc *ClassifiedCondition
		for _, x := range a.DSCConds {
			if x.Cond.Type == cond.Type {
				cc = x
			}
		}
		if !conditionFailing(cond) && (cc == nil || !cc.Gate) {
			continue
		}
		line := ConditionLine(cond)
		if seen[line] {
			continue
		}
		seen[line] = true
		failing++
		var causes, rel []string
		if cc != nil {
			causes, rel = a.conditionCauses(cc, titles)
		}
		if len(causes) == 0 {
			unclassified = append(unclassified, cond.Type)
			evidence = append(evidence, line+" → cause not classified; see the operator's message")
			continue
		}
		evidence = append(evidence, line+" → cause: "+strings.Join(causes, "; "))
		for _, id := range rel {
			if !slices.Contains(related, id) {
				related = append(related, id)
			}
			if t := titles[id]; t != "" {
				fix := fmt.Sprintf("%q", t)
				if !slices.Contains(fixes, fix) {
					fixes = append(fixes, fix)
				}
			}
		}
	}
	fix := ""
	if len(fixes) > 0 {
		fix = "Fix the causes with the problems listed under Related: " + strings.Join(fixes, ", ") + "."
	}
	if len(related) > len(fixes) {
		fix += " Other causes are reported by the checks named next to the conditions."
	}
	if len(unclassified) > 0 {
		fix += fmt.Sprintf(" For %s, which the tool does not classify, fix the cause the operator names (for example set an unused component to Removed); the Components page shows the per-component status.", strings.Join(unclassified, ", "))
		if dsciProblem != nil {
			related = append(related, dsciProblem.ID)
			fix += fmt.Sprintf(" The DSCInitialization is in Error too (%q), which can keep components from deploying.", dsciProblem.Title)
		}
	}
	return report.Problem{
		ID:       "dsc-not-ready",
		Severity: "warning",
		Title:    fmt.Sprintf("DataScienceCluster %s is not ready", dsc.Metadata.Name),
		Description: "The operator reports that some components or modules failed to deploy or are unhealthy. Each failing condition below is the operator's own message, " +
			"followed by its cause as far as the tool can tell. Conditions with severity Info (for example components set to Removed) are not listed.",
		Evidence:        evidence,
		AffectedObjects: []string{"DataScienceCluster " + dsc.Metadata.Name},
		Fix:             strings.TrimSpace(fix),
		RelatedProblems: related,
		TechnicalCmd:    fmt.Sprintf("oc get datasciencecluster %s -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}: {.message}{\"\\n\"}{end}'", dsc.Metadata.Name),
	}, failing
}

// IsUpgradeGateCondition: reason AdminAckRequired, or a message about
// upgrade gates (rhods-operator docs/upgrade-ordering.md "Admin ack gates";
// a 3.5 operator over newer resources reports "failed to resolve upgrade
// gate version" on the DSC Ready condition).
func IsUpgradeGateCondition(cond rhoai.DSCCondition) bool {
	return cond.Reason == "AdminAckRequired" || strings.Contains(strings.ToLower(cond.Message), "upgrade gate")
}

// PendingUpgradeAckKeys returns the keys of odh-upgrade-acks whose value is
// not "true", sorted. The Platform and DataScienceCluster gate problems
// turn each into its own acknowledgement command.
func PendingUpgradeAckKeys(c *kube.Client) ([]string, map[string]string, error) {
	var cm struct {
		Data map[string]string `json:"data"`
	}
	if err := c.GetInto(rhoai.NamespacedPath("v1", "configmaps", rhoai.SubNS, "odh-upgrade-acks"), &cm); err != nil {
		if kube.IsDecodeError(err) {
			return nil, nil, fmt.Errorf("parse ConfigMap odh-upgrade-acks: %w", err)
		}
		return nil, nil, err
	}
	var keys []string
	for k, v := range cm.Data {
		if v != "true" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys, cm.Data, nil
}

// UpgradeAckCommand acknowledges one upgrade gate.
func UpgradeAckCommand(key string) string {
	patch, _ := json.Marshal(map[string]any{"data": map[string]string{key: "true"}})
	return kube.ShellCommand("oc", "patch", "configmap", "odh-upgrade-acks", "-n", rhoai.SubNS, "--type", "merge", "-p", string(patch))
}
