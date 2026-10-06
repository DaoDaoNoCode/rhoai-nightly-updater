package cluster

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
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

type dscCondition struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	Reason             string `json:"reason"`
	Message            string `json:"message"`
	Severity           string `json:"severity"`
	LastTransitionTime string `json:"lastTransitionTime"`
}

type dscObject struct {
	APIVersion string `json:"apiVersion"`
	Metadata   struct {
		Name       string `json:"name"`
		Generation int64  `json:"generation"`
	} `json:"metadata"`
	Spec struct {
		ApplicationsNamespace string `json:"applicationsNamespace"` // DSCInitialization
	} `json:"spec"`
	Status struct {
		Phase              string         `json:"phase"`
		ObservedGeneration int64          `json:"observedGeneration"`
		Conditions         []dscCondition `json:"conditions"`
	} `json:"status"`
}

// listFirstServed lists a collection with the newest served version and
// returns the items. pathFmt is the collection path with %s for the
// version. apiFound is false when no version is served (no CRD).
func listFirstServed(c *Client, pathFmt, resource string) (items []dscObject, apiFound bool, err error) {
	for _, version := range []string{"v2", "v1"} {
		body, _, err := c.get(fmt.Sprintf(pathFmt, version))
		if IsK8sError(err, http.StatusNotFound) {
			continue
		}
		if err != nil {
			return nil, true, err
		}
		var list struct {
			Items []dscObject `json:"items"`
		}
		if err := json.Unmarshal(body, &list); err != nil {
			return nil, true, fmt.Errorf("parse %s: %w", resource, err)
		}
		return list.Items, true, nil
	}
	return nil, false, nil
}

// conditionFailing: a non-Info condition that is not True (Degraded, the
// one negative-polarity condition, when True). The roll-ups Ready and
// Progressing are not counted.
func conditionFailing(cond dscCondition) bool {
	if cond.Type == "Ready" || cond.Type == "Progressing" || strings.EqualFold(cond.Severity, "Info") {
		return false
	}
	if cond.Type == "Degraded" {
		return cond.Status == "True"
	}
	return cond.Status != "True"
}

func conditionLine(cond dscCondition) string {
	line := fmt.Sprintf("%s=%s", cond.Type, cond.Status)
	if cond.Reason != "" {
		line += fmt.Sprintf(" (%s)", cond.Reason)
	}
	if msg := truncate(cond.Message, 300); msg != "" {
		line += ": " + msg
	}
	return line
}

// failingConditions returns the failing conditions as lines. Duplicate
// messages are reported once.
func failingConditions(conds []dscCondition) []string {
	seen := map[string]bool{}
	var out []string
	for _, cond := range conds {
		if !conditionFailing(cond) {
			continue
		}
		msg := truncate(cond.Message, 300)
		if seen[msg] && msg != "" {
			continue
		}
		seen[msg] = true
		out = append(out, conditionLine(cond))
	}
	sort.Strings(out)
	return out
}

// classifiedCondition is one condition with what it says about the cause.
type classifiedCondition struct {
	Source     string // "DataScienceCluster default-dsc", "Trainer default-trainer"
	Module     string // lower-case module kind, "" when not about one module
	Cond       dscCondition
	Blocking   bool // counts for Ready (severity not Info)
	Deps       []*dependency
	Gate       bool
	Stale      bool
	ApplyIDs   []string
	Conversion bool
}

func (cc *classifiedCondition) label() string { return cc.Source + ": " + conditionLine(cc.Cond) }

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
	Obj          dscObject
}

// staleSince reports a module CR whose operator has not observed its
// current generation, with no condition change for moduleStaleAfter.
func (m moduleObject) staleFor(now time.Time) (time.Duration, bool) {
	st := m.Obj.Status
	if st.ObservedGeneration <= 0 || st.ObservedGeneration >= m.Obj.Metadata.Generation {
		return 0, false
	}
	var newest time.Time
	for _, cond := range st.Conditions {
		if t, ok := parseK8sTime(cond.LastTransitionTime); ok && t.After(newest) {
			newest = t
		}
	}
	if newest.IsZero() || now.Sub(newest) < moduleStaleAfter {
		return 0, false
	}
	return now.Sub(newest), true
}

func listModuleObjects(c *Client, api componentAPI) ([]moduleObject, error) {
	type result struct {
		items []dscObject
		err   error
	}
	results := make([]result, len(api.Kinds))
	parallelFor(len(api.Kinds), 6, func(i int) {
		body, _, err := c.get(componentListPath(api.Version, api.Kinds[i].Resource))
		if err != nil {
			results[i].err = fmt.Errorf("list %s: %w", api.Kinds[i].Resource, err)
			return
		}
		var list struct {
			Items []dscObject `json:"items"`
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
	Ready      *dscCondition
	DSCConds   []*classifiedCondition // the DSC's failing conditions, and Info ones naming a dependency
	ModConds   []*classifiedCondition // module CR conditions with a classified cause
	Deps       []*dependency
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

func (a *dscAnalysis) gates() []*classifiedCondition {
	var out []*classifiedCondition
	for _, cc := range a.DSCConds {
		if cc.Gate {
			out = append(out, cc)
		}
	}
	return out
}

// classify fills in what the condition's message says.
func (cc *classifiedCondition) classify(deps map[string]*dependency, order *[]*dependency) {
	msg := cc.Cond.Message
	lower := strings.ToLower(msg)
	cc.Gate = isUpgradeGateCondition(cc.Cond)
	cc.Stale = strings.Contains(lower, "observedgeneration < generation") || strings.Contains(lower, "status is stale")
	cc.Conversion = strings.Contains(lower, "conversion webhook")
	for _, f := range parseApplyFailures(msg) {
		if !containsString(cc.ApplyIDs, f.problemID()) {
			cc.ApplyIDs = append(cc.ApplyIDs, f.problemID())
		}
	}
	if cc.Cond.Status == "True" && cc.Cond.Type != "Degraded" {
		return
	}
	for _, om := range parseMissingOperands(msg) {
		key := "operand-" + strings.ToLower(om.Kind)
		d := deps[key]
		if d == nil {
			mention := om.Kind + " CR"
			if om.Name != "" {
				mention = om.Kind + "/" + om.Name
			}
			d = &dependency{Mention: mention, Key: key, OperandKind: om.Kind, OperandName: om.Name}
			deps[key] = d
			*order = append(*order, d)
		}
		d.Reporters = append(d.Reporters, cc)
		cc.Deps = append(cc.Deps, d)
	}
	for _, name := range parseMissingDependencies(msg) {
		key := normalizeOperatorName(name)
		if key == "" {
			continue
		}
		d := deps[key]
		if d == nil {
			d = &dependency{Mention: name, Key: key}
			deps[key] = d
			*order = append(*order, d)
		}
		d.Reporters = append(d.Reporters, cc)
		cc.Deps = append(cc.Deps, d)
	}
}

// analyzeDSC classifies the DSC's conditions and those of the module CRs,
// resolves the prerequisites they name, and finds module operators in
// back-off. appNS is the DSCInitialization's applications namespace.
func analyzeDSC(c *Client, dsc dscObject, appNS string, now time.Time) *dscAnalysis {
	a := &dscAnalysis{Name: dsc.Metadata.Name}
	deps := map[string]*dependency{}
	api, apiErr := discoverComponentAPI(c)
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
		cc := &classifiedCondition{Source: source, Module: conditionModule(cond.Type, kinds), Cond: cond, Blocking: failing}
		if cond.Type == "Ready" {
			if !isUpgradeGateCondition(cond) || cond.Status == "True" {
				continue
			}
			cc.Blocking = true
		} else if !failing && (cond.Status == "True" || len(parseMissingDependencies(cond.Message)) == 0) {
			continue
		}
		cc.classify(deps, &a.Deps)
		a.DSCConds = append(a.DSCConds, cc)
		if cc.Blocking && cc.Module != "" {
			blockingModule[cc.Module] = true
		}
	}
	if len(a.DSCConds) == 0 {
		return a
	}

	var modules []moduleObject
	if api.Version != "" {
		modules, a.ModulesErr = listModuleObjects(c, api)
	}
	staleModules := map[string]string{}
	for _, m := range modules {
		src := m.Kind + " " + m.Obj.Metadata.Name
		for _, cond := range m.Obj.Status.Conditions {
			if cond.Status == "True" && cond.Type != "Degraded" {
				continue
			}
			cc := &classifiedCondition{Source: src, Module: m.Module, Cond: cond, Blocking: blockingModule[m.Module]}
			cc.classify(deps, &a.Deps)
			if len(cc.Deps) > 0 || cc.Stale {
				a.ModConds = append(a.ModConds, cc)
			}
		}
		if age, ok := m.staleFor(now); ok {
			staleModules[m.Module] = fmt.Sprintf("%s has generation %d but its operator last reported observedGeneration %d; no condition changed for %s",
				src, m.Obj.Metadata.Generation, m.Obj.Status.ObservedGeneration, formatDuration(age))
		}
	}
	resolveDependencies(c, a.Deps)

	// Modules whose operator has not retried.
	reasons := map[string][]string{}
	addReason := func(module, reason string) {
		if module != "" && !containsString(reasons[module], reason) {
			reasons[module] = append(reasons[module], reason)
		}
	}
	for _, cc := range append(append([]*classifiedCondition{}, a.DSCConds...), a.ModConds...) {
		if cc.Module == "" || len(cc.Deps) == 0 {
			continue
		}
		var installed []string
		ready := true
		for _, d := range cc.Deps {
			if !d.satisfied() || (!d.CSV.Changed.IsZero() && now.Sub(d.CSV.Changed) < prerequisiteGrace) {
				ready = false
				break
			}
			line := fmt.Sprintf("%s (CSV %s/%s Succeeded", d.displayName(), d.CSV.Namespace, d.CSV.Name)
			if !d.CSV.Changed.IsZero() {
				line += " " + formatDuration(now.Sub(d.CSV.Changed)) + " ago"
			}
			switch d.OperandSt {
			case operandPresent:
				line += fmt.Sprintf("; %s exists", d.Operand.ref())
			}
			installed = append(installed, line+")")
		}
		if ready {
			addReason(cc.Module, fmt.Sprintf("%s still reports %s, but it is installed: %s", cc.label(), joinMentions(cc.Deps), strings.Join(installed, "; ")))
		}
	}
	for module, why := range staleModules {
		addReason(module, why)
	}
	if a.ModulesErr != nil || api.Version == "" {
		// Without the module CRs, trust the DSC's own stale report.
		for _, cc := range a.DSCConds {
			if t, ok := parseK8sTime(cc.Cond.LastTransitionTime); cc.Stale && ok && now.Sub(t) >= moduleStaleAfter {
				addReason(cc.Module, fmt.Sprintf("%s for %s", cc.label(), formatDuration(now.Sub(t))))
			}
		}
	}
	moduleNames := make([]string, 0, len(reasons))
	for m := range reasons {
		moduleNames = append(moduleNames, m)
	}
	sort.Strings(moduleNames)
	for _, m := range moduleNames {
		b := &moduleBackoff{Module: m, Reasons: reasons[m], Blocking: blockingModule[m]}
		b.Deployment, b.FindErr = findModuleOperator(c, m, appNS)
		if b.Deployment != nil {
			b.Restart = assessRestart(c, *b.Deployment)
		}
		a.Backoffs = append(a.Backoffs, b)
	}
	return a
}

func joinMentions(deps []*dependency) string {
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
func analyzeCurrentDSC(c *Client, now time.Time) (*dscAnalysis, error) {
	dscs, apiFound, err := listFirstServed(c, "/apis/datasciencecluster.opendatahub.io/%s/datascienceclusters", "datascienceclusters")
	if err != nil {
		return nil, err
	}
	if !apiFound || len(dscs) == 0 {
		return nil, fmt.Errorf("no DataScienceCluster exists")
	}
	return analyzeDSC(c, dscs[0], applicationsNamespace(c), now), nil
}

// applicationsNamespace is the DSCInitialization's applications namespace
// ("" when it cannot be read).
func applicationsNamespace(c *Client) string {
	dscis, _, err := listFirstServed(c, "/apis/dscinitialization.opendatahub.io/%s/dscinitializations", "dscinitializations")
	if err != nil || len(dscis) == 0 {
		return ""
	}
	return dscis[0].Spec.ApplicationsNamespace
}

// conditionCauses explains one DSC condition and names the problems that
// fix it. titles maps problem IDs to titles for those this check reports.
func (a *dscAnalysis) conditionCauses(cc *classifiedCondition, titles map[string]string) (causes, related []string) {
	add := func(cause, id string) {
		if t := titles[id]; t != "" {
			cause += fmt.Sprintf(" (see %q)", t)
		}
		if !containsString(causes, cause) {
			causes = append(causes, cause)
		}
		if id != "" && !containsString(related, id) {
			related = append(related, id)
		}
	}
	b := a.backoff(cc.Module)
	explain := func(cc *classifiedCondition) {
		if cc.Gate {
			add("an upgrade gate holds provisioning", dscGateProblemID)
		}
		for _, d := range cc.Deps {
			switch {
			case d.installed() && d.OperandSt == operandMissing:
				add(fmt.Sprintf("%s is installed, but %s does not exist", d.displayName(), d.Operand.ref()), d.problemID())
			case d.operandProblem():
				add(fmt.Sprintf("%s is installed, but whether %s exists could not be checked", d.displayName(), d.Operand.ref()), d.problemID())
			case !d.satisfied():
				kind := "missing prerequisite operator"
				if !cc.Blocking {
					kind = "optional prerequisite operator"
				}
				state := "not installed"
				if d.CSV != nil {
					state = "installed but not ready (CSV " + nonEmpty(d.CSV.Phase, "phase unknown") + ")"
				}
				add(fmt.Sprintf("%s %s is %s", kind, d.displayName(), state), d.problemID())
			case b != nil:
				add(fmt.Sprintf("%s is installed now, but the %s module operator has not retried", d.displayName(), cc.Module), b.problemID())
			default:
				add(fmt.Sprintf("%s is installed now; the module picks it up on its next retry", d.displayName()), "")
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
			add(fmt.Sprintf("an object the operator cannot update (see the %q check)", applyFailuresCheckName), id)
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
	return causes, related
}

const dscGateProblemID = "dsc-upgrade-gate"

// gateProblem reports the DSC's upgrade gate conditions with the exact
// acknowledgement steps (rhods-operator docs/upgrade-ordering.md "Admin
// ack gates"). A gate whose target release the operator cannot resolve is
// not acknowledged away: it means the operator is older than what it found.
func gateProblem(c *Client, gates []*classifiedCondition) Problem {
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
	p := Problem{
		ID:              dscGateProblemID,
		Severity:        "critical",
		Title:           "The DataScienceCluster waits on an upgrade gate",
		AffectedObjects: []string{"ConfigMap " + SubNS + "/odh-upgrade-acks"},
	}
	var desc, fixes []string
	if ack {
		keys, values, err := pendingUpgradeAckKeys(c)
		desc = append(desc, "Before provisioning a new version, the operator checks upgrade gates. An unacknowledged gate blocks provisioning until an administrator sets its key to \"true\" in ConfigMap odh-upgrade-acks. This is a deliberate manual step, not a bug.")
		switch {
		case err != nil:
			evidence = append(evidence, fmt.Sprintf("ConfigMap %s/odh-upgrade-acks: %v%s", SubNS, err, templateHint(err)))
			fixes = append(fixes, "Read what the gate is about, then set its key to \"true\" in ConfigMap odh-upgrade-acks (replace <key> in the command).")
			p.TechnicalCmd = upgradeAckCommand("<key>")
		case len(keys) == 0:
			evidence = append(evidence, "ConfigMap odh-upgrade-acks has no key that is not \"true\"")
			fixes = append(fixes, "No key is waiting in odh-upgrade-acks; read the ConfigMap and the operator logs for the gate's name.")
			p.TechnicalCmd = shellCommand("oc", "get", "configmap", "odh-upgrade-acks", "-n", SubNS, "-o", "yaml")
		default:
			var cmds []string
			for _, k := range keys {
				evidence = append(evidence, fmt.Sprintf("Not acknowledged: %s=%q", k, values[k]))
				cmds = append(cmds, upgradeAckCommand(k))
			}
			fixes = append(fixes, fmt.Sprintf("Read what each gate is about (the operator's release notes name it), then acknowledge %s with the commands below.", verb(len(keys), "it", "them")))
			p.TechnicalCmd = strings.Join(cmds, "\n")
		}
	}
	if unresolved {
		desc = append(desc, "The operator cannot work out which release the gates are for. That happens when an older operator runs over resources a newer version created (a downgrade, or an install of an older build); acknowledging does not help.")
		fixes = append(fixes, "Install the operator version that created these resources, or a newer one (Update on the Status page).")
		if p.TechnicalCmd == "" {
			p.TechnicalCmd = shellCommand("oc", "get", "datasciencecluster", "-o", "jsonpath={.items[0].status.release.version}")
		}
	}
	p.Description = strings.Join(desc, " ")
	p.Evidence = evidence
	p.Fix = strings.Join(fixes, " ")
	return p
}

func checkDataScienceCluster(c *Client) checkOutput {
	const name = "DataScienceCluster"
	out := checkOutput{check: CheckResult{Name: name, Status: "pass"}}
	details := []string{}
	// warn lowers a pass to warn; fail is kept.
	warn := func(detail string) {
		if out.check.Status == "pass" {
			out.check.Status = "warn"
		}
		details = append(details, detail)
	}

	// The DSCI is read first: a DSCI in Error often explains why no DSC
	// components deploy, so it is reported whatever the DSC state is, and
	// its applications namespace locates the module operators.
	dscis, dsciAPI, dsciErr := listFirstServed(c, "/apis/dscinitialization.opendatahub.io/%s/dscinitializations", "dscinitializations")
	var dsciProblem *Problem
	appNS := ""
	if dsciErr == nil && dsciAPI && len(dscis) > 0 {
		dsci := dscis[0]
		appNS = dsci.Spec.ApplicationsNamespace
		if dsci.Status.Phase == "Error" {
			dsciProblem = &Problem{
				ID:              "dsci-error",
				Severity:        "warning",
				Title:           fmt.Sprintf("DSCInitialization %s is in phase Error", dsci.Metadata.Name),
				Description:     "The operator could not finish platform initialization (namespaces, monitoring, trusted CA bundle). Components may not deploy until this is fixed.",
				Evidence:        append([]string{fmt.Sprintf("DSCInitialization %s: phase=Error", dsci.Metadata.Name)}, failingConditions(dsci.Status.Conditions)...),
				AffectedObjects: []string{"DSCInitialization " + dsci.Metadata.Name},
				Fix:             "Fix the cause named in the conditions; the operator retries automatically.",
				TechnicalCmd:    fmt.Sprintf("oc get dscinitialization %s -o jsonpath='{.status.conditions}'", dsci.Metadata.Name),
			}
		}
	}

	dscs, apiFound, err := listFirstServed(c, "/apis/datasciencecluster.opendatahub.io/%s/datascienceclusters", "datascienceclusters")
	switch {
	case err != nil:
		warn(fmt.Sprintf("could not verify the DataScienceCluster: %v", err))
	case !apiFound:
		details = append(details, "No DataScienceCluster API (operator not installed yet)")
	case len(dscs) == 0:
		warn("No DataScienceCluster exists")
		out.problems = append(out.problems, Problem{
			ID:          "dsc-missing",
			Severity:    "info",
			Title:       "No DataScienceCluster exists",
			Description: "The operator is installed, but no DataScienceCluster exists, so no RHOAI components are deployed.",
			Fix:         "Create the DataScienceCluster from the Status page.",
		})
	default:
		dsc := dscs[0]
		a := analyzeDSC(c, dsc, appNS, time.Now())
		titles := map[string]string{}
		var causeProblems []Problem
		for _, d := range a.Deps {
			if d.problemID() == "" {
				continue
			}
			p := prerequisiteProblem(c, d)
			causeProblems = append(causeProblems, p)
		}
		for _, b := range a.Backoffs {
			causeProblems = append(causeProblems, b.problem())
		}
		if gates := a.gates(); len(gates) > 0 {
			causeProblems = append(causeProblems, gateProblem(c, gates))
		}
		for _, p := range causeProblems {
			titles[p.ID] = p.Title
		}
		if dsciProblem != nil {
			titles[dsciProblem.ID] = dsciProblem.Title
		}

		ready := a.Ready
		switch {
		case ready == nil:
			warn(fmt.Sprintf("%s has no Ready condition yet (the operator has not reconciled it)", dsc.Metadata.Name))
		case ready.Status == "True":
			details = append(details, fmt.Sprintf("%s is Ready", dsc.Metadata.Name))
			var optional []string
			for _, d := range a.Deps {
				if !d.satisfied() && !d.blocking() {
					optional = append(optional, d.displayName())
				}
			}
			if len(optional) > 0 {
				details = append(details, "optional prerequisites not installed: "+strings.Join(optional, ", "))
			}
		default:
			out.check.Status = "fail"
			p, failing := a.notReadyProblem(dsc, titles, dsciProblem)
			details = append(details, fmt.Sprintf("%s is not ready (%s)", dsc.Metadata.Name, countNoun(failing, "failing condition", "failing conditions")))
			out.problems = append(out.problems, p)
		}
		for _, p := range causeProblems {
			if p.Severity != "info" && out.check.Status == "pass" {
				out.check.Status = "warn"
			}
		}
		out.problems = append(out.problems, causeProblems...)
		if a.ModulesErr != nil {
			warn(fmt.Sprintf("module CRs not fully checked: %v", a.ModulesErr))
		}
	}

	switch {
	case dsciErr != nil:
		warn(fmt.Sprintf("could not verify the DSCInitialization: %v", dsciErr))
	case !dsciAPI || len(dscis) == 0:
		details = append(details, "no DSCInitialization")
	default:
		if dsciProblem != nil {
			out.check.Status = "fail"
			out.problems = append(out.problems, *dsciProblem)
		}
		details = append(details, fmt.Sprintf("DSCInitialization %s phase %s", dscis[0].Metadata.Name, nonEmpty(dscis[0].Status.Phase, "unknown")))
	}

	out.check.Detail = strings.Join(details, "; ")
	return out
}

// notReadyProblem lists each failing DSC condition with its classified
// cause and links the problems that fix them. It returns the number of
// failing conditions.
func (a *dscAnalysis) notReadyProblem(dsc dscObject, titles map[string]string, dsciProblem *Problem) (Problem, int) {
	ready := a.Ready
	evidence := []string{fmt.Sprintf("DataScienceCluster %s: Ready=%s (%s): %s", dsc.Metadata.Name, ready.Status, ready.Reason, truncate(ready.Message, 400))}
	var related, unclassified, fixes []string
	failing := 0
	seen := map[string]bool{}
	conds := append([]dscCondition{}, dsc.Status.Conditions...)
	sort.SliceStable(conds, func(i, j int) bool { return conds[i].Type < conds[j].Type })
	for _, cond := range conds {
		var cc *classifiedCondition
		for _, x := range a.DSCConds {
			if x.Cond.Type == cond.Type {
				cc = x
			}
		}
		if !conditionFailing(cond) && (cc == nil || !cc.Gate) {
			continue
		}
		line := conditionLine(cond)
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
			if !containsString(related, id) {
				related = append(related, id)
			}
			if t := titles[id]; t != "" {
				fix := fmt.Sprintf("%q", t)
				if !containsString(fixes, fix) {
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
	return Problem{
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
