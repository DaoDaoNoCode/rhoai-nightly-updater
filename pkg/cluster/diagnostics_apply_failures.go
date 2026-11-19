package cluster

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// "Objects the operator cannot update": leftovers of an upgrade, downgrade
// or round trip that the operator reports on its CRs but cannot fix itself.
// The DataScienceCluster, the Platform and the module CRs report every
// object the operator failed to apply as
//
//	failure deploying resource <namespace>/<name>: apply failed <group/version>, Kind=<Kind>: <API error>
//
// (cluster-scoped objects have an empty namespace). Three API errors need a
// person (live on RHOAI 3.6 after upgrades and a 3.6 -> 3.5.1 -> 3.6 round
// trip):
//   - a field is immutable (a Deployment created by an older version has
//     another spec.selector): the object must be deleted and recreated;
//   - "Only one reference can have Controller set to true": ownership moved
//     between versions, so the new owner cannot take the object over;
//   - "failed to create typed live|patch object ... expected <type>, got":
//     the live CRD and the stored object (or the operator's desired object)
//     disagree on a field's type, typically because OLM applied an older
//     bundle's version of a same-named CRD.
//
// The first two are fixed by deleting the object; its operator recreates
// it. Module operators back off a failing item exponentially
// (controller-runtime, up to about 16m40s), so the operator that reports it
// is restarted to reconcile at once. The schema case needs judgement. All
// three are guidance only: the tool deletes nothing.

const applyFailuresCheckName = "Objects the operator cannot update"

var applyFailurePattern = regexp.MustCompile(`failure deploying resource ([^\s/:]*)/([^\s/:]+): apply failed ([^\s,]+), Kind=([A-Za-z0-9]+):`)

// The parsed object is pasted into copy-paste commands, so it must look
// like a Kubernetes object: anything else in a condition message is
// ignored (Kubernetes object-name rules:
// https://kubernetes.io/docs/concepts/overview/working-with-objects/names/).
var (
	dns1123Subdomain = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
	dns1123Label     = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	kindPattern      = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*$`)
	groupVersion     = regexp.MustCompile(`^[a-z0-9.-]+/[a-z0-9]+$|^v[0-9a-z]+$`)
)

// validObject reports whether the parsed object follows the Kubernetes
// naming rules (an empty namespace is a cluster-scoped object).
func (f applyFailure) validObject() bool {
	return len(f.Name) <= 253 && dns1123Subdomain.MatchString(f.Name) &&
		(f.Namespace == "" || (len(f.Namespace) <= 63 && dns1123Label.MatchString(f.Namespace))) &&
		kindPattern.MatchString(f.Kind) && groupVersion.MatchString(f.GroupVersion)
}

// shellQuote quotes s for a POSIX shell unless it only has characters that
// need no quoting.
func shellQuote(s string) string {
	if s != "" && safeShellWord.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

var safeShellWord = regexp.MustCompile(`^[A-Za-z0-9._/:=@%+,-]+$`)

// shellCommand joins words into a command line, quoting each.
func shellCommand(words ...string) string {
	quoted := make([]string, len(words))
	for i, w := range words {
		quoted[i] = shellQuote(w)
	}
	return strings.Join(quoted, " ")
}

var (
	invalidFieldPattern  = regexp.MustCompile(`([A-Za-z][\w.\[\]-]*): Invalid value: `)
	controllerOwnerFound = regexp.MustCompile(`Found "true" in references for ([^\n;]+)`)
	typedFieldPattern    = regexp.MustCompile(`(\.[\w.\[\]"=-]+): expected ([A-Za-z]+), got (&\{[^}]*\}|[^\s,;)]+)`)
)

type applyFailureClass string

const (
	applyImmutable       applyFailureClass = "immutable"
	applyControllerOwner applyFailureClass = "controller-owner"
	applySchemaMismatch  applyFailureClass = "schema-mismatch"
	// applyStoredType: the API server could not convert the stored object
	// to the CRD's schema for server-side apply ("failed to create manager
	// for existing fields: failed to convert new object (...) to smd typed:
	// errors: <path>: expected <type>, got ..."), live on RHOAI 3.6 after
	// upgrades over several releases. The "Left over from an earlier
	// release" check handles it (diagnostics_leftovers.go).
	applyStoredType applyFailureClass = "stored-type"
)

// leftoverWorkloads are the kinds whose spec.selector is immutable, by API
// group: the "Left over from an earlier release" check recreates them.
var leftoverWorkloads = map[string]string{"Deployment": "apps", "StatefulSet": "apps", "DaemonSet": "apps", "Job": "batch"}

// leftover reports whether the "Left over from an earlier release" check
// handles the failure: a stored object of an older schema, or a workload
// whose spec.selector changed. checkApplyFailures leaves those out.
func (f applyFailure) leftover() bool {
	switch f.Class {
	case applyStoredType:
		return true
	case applyImmutable:
		return f.selectorChanged()
	}
	return false
}

// selectorChanged: an immutable spec.selector of a workload kind.
func (f applyFailure) selectorChanged() bool {
	group, ok := leftoverWorkloads[f.Kind]
	return ok && f.group() == group && slices.Contains(f.Fields, "spec.selector")
}

// leftover decides per object: the "Left over from an earlier release"
// check takes an object only when every failure reported for it is one it
// fixes (stored field types alone, or an immutable spec.selector alone).
// An object with other failures stays one problem here (one problem per
// object), with guidance.
func (rf *reportedApplyFailure) leftover() bool {
	if len(rf.classes) != 1 {
		return false
	}
	switch rf.classes[0] {
	case applyStoredType:
		return true
	case applyImmutable:
		fields := rf.fields[applyImmutable]
		return rf.selectorChanged() && len(fields) == 1 && fields[0] == "spec.selector"
	}
	return false
}

// allApplyFailures keeps every failure.
func allApplyFailures(applyFailure) bool { return true }

// applyFailure is one object an operator could not apply.
type applyFailure struct {
	Namespace, Name, GroupVersion, Kind string
	Class                               applyFailureClass
	Detail                              string   // the API error
	Fields                              []string // immutable fields, or "<path>: expected <type>, got <value>"
	Owners                              string   // the controller owners, for applyControllerOwner
}

func (f applyFailure) group() string {
	if g, _, ok := strings.Cut(f.GroupVersion, "/"); ok {
		return g
	}
	return "" // core API ("v1")
}

func (f applyFailure) object() string {
	if f.Namespace == "" {
		return f.Kind + " " + f.Name
	}
	return f.Kind + " " + f.Namespace + "/" + f.Name
}

// resourceArg is the kind as oc accepts it, qualified by its group.
func (f applyFailure) resourceArg() string {
	if g := f.group(); g != "" {
		return strings.ToLower(f.Kind) + "." + g
	}
	return strings.ToLower(f.Kind)
}

// ocCommand is "oc <verb> <kind.group> <name> [-n <ns>] <extra...>",
// every argument shell-quoted.
func (f applyFailure) ocCommand(verb string, extra ...string) string {
	words := []string{"oc", verb, f.resourceArg(), f.Name}
	if f.Namespace != "" {
		words = append(words, "-n", f.Namespace)
	}
	return shellCommand(append(words, extra...)...)
}

// problemID is the ID of the problem reported for the object.
func (f applyFailure) problemID() string {
	switch {
	case f.Class == applyStoredType:
		return leftoverFieldsID(f.Kind, f.Name)
	case f.leftover():
		return leftoverRecreateID(f.Kind, f.Namespace, f.Name)
	}
	return f.genericProblemID()
}

// genericProblemID is the ID of this check's problem for the object.
func (f applyFailure) genericProblemID() string {
	return "operator-apply-failed-" + strings.ToLower(strings.Join([]string{f.Kind, f.Namespace, f.Name}, "-"))
}

// key identifies the object (not the failure): one problem per object.
func (f applyFailure) key() string {
	return strings.Join([]string{f.group(), f.Kind, f.Namespace, f.Name}, "|")
}

// parseApplyFailures finds the classified apply failures in a condition
// message. Failures of another kind are left out.
func parseApplyFailures(msg string) []applyFailure {
	locs := applyFailurePattern.FindAllStringSubmatchIndex(msg, -1)
	var out []applyFailure
	for i, loc := range locs {
		end := len(msg)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		f := applyFailure{
			Namespace: msg[loc[2]:loc[3]], Name: msg[loc[4]:loc[5]], GroupVersion: msg[loc[6]:loc[7]], Kind: msg[loc[8]:loc[9]],
			Detail: strings.TrimSpace(msg[loc[1]:end]),
		}
		if !f.validObject() {
			continue
		}
		switch {
		case strings.Contains(f.Detail, "Only one reference can have Controller set to true"):
			f.Class = applyControllerOwner
			if m := controllerOwnerFound.FindStringSubmatch(f.Detail); m != nil {
				f.Owners = strings.TrimRight(strings.TrimSpace(m[1]), ".")
			}
		case strings.Contains(f.Detail, "field is immutable"):
			f.Class = applyImmutable
			for _, m := range invalidFieldPattern.FindAllStringSubmatch(f.Detail, -1) {
				if !slices.Contains(f.Fields, m[1]) {
					f.Fields = append(f.Fields, m[1])
				}
			}
		case strings.Contains(f.Detail, "to smd typed") && strings.Contains(f.Detail, "expected "):
			f.Class = applyStoredType
			for _, m := range typedFieldPattern.FindAllStringSubmatch(f.Detail, -1) {
				field := fmt.Sprintf("%s: expected %s, got %s", m[1], m[2], m[3])
				if !slices.Contains(f.Fields, field) {
					f.Fields = append(f.Fields, field)
				}
			}
			if len(f.Fields) == 0 {
				continue
			}
		case strings.Contains(f.Detail, "failed to create typed") && strings.Contains(f.Detail, "expected "):
			f.Class = applySchemaMismatch
			for _, m := range typedFieldPattern.FindAllStringSubmatch(f.Detail, -1) {
				field := fmt.Sprintf("%s: expected %s, got %s", m[1], m[2], m[3])
				if !slices.Contains(f.Fields, field) {
					f.Fields = append(f.Fields, field)
				}
			}
		default:
			continue
		}
		out = append(out, f)
	}
	return out
}

// applyReporter is a CR whose conditions report apply failures.
type applyReporter struct {
	Kind, Name, UID string
	// Module is the lower-case kind of a components.platform.opendatahub.io
	// CR, "" for the DataScienceCluster and the Platform.
	Module   string
	NotReady bool // its Ready condition is False
	rank     int  // module CR 2, Platform 1, DSC 0: the most specific reports first
}

func (r applyReporter) String() string { return r.Kind + " " + r.Name }

// reportedApplyFailure is one object with every failure reported for it
// and every CR that reports one.
type reportedApplyFailure struct {
	applyFailure // the object (the first failure parsed)
	classes      []applyFailureClass
	fields       map[applyFailureClass][]string
	owners       string
	details      []string
	reporters    []applyReporter
}

func (rf *reportedApplyFailure) add(f applyFailure) {
	if !rf.has(f.Class) {
		rf.classes = append(rf.classes, f.Class)
	}
	for _, field := range f.Fields {
		if !slices.Contains(rf.fields[f.Class], field) {
			rf.fields[f.Class] = append(rf.fields[f.Class], field)
		}
	}
	if rf.owners == "" {
		rf.owners = f.Owners
	}
	if !slices.Contains(rf.details, f.Detail) {
		rf.details = append(rf.details, f.Detail)
	}
}

func (rf *reportedApplyFailure) has(class applyFailureClass) bool {
	return slices.Contains(rf.classes, class)
}

// collectApplyFailures scans the conditions of one CR for the failures
// keep accepts.
func collectApplyFailures(found map[string]*reportedApplyFailure, r applyReporter, conds []dscCondition, keep func(applyFailure) bool) {
	for _, cond := range conds {
		if cond.Type == "Ready" && cond.Status == "False" {
			r.NotReady = true
		}
	}
	for _, cond := range conds {
		for _, f := range parseApplyFailures(cond.Message) {
			if !keep(f) {
				continue
			}
			k := f.key()
			if found[k] == nil {
				found[k] = &reportedApplyFailure{applyFailure: f, fields: map[applyFailureClass][]string{}}
			}
			rf := found[k]
			rf.add(f)
			dup := false
			for _, existing := range rf.reporters {
				dup = dup || existing.String() == r.String()
			}
			if !dup {
				rf.reporters = append(rf.reporters, r)
			}
		}
	}
}

// reporterScan is what scanApplyReporters read: the failures it kept, the
// component API, the DataScienceClusters and the module CRs (each also as
// the raw object, by kind and name).
type reporterScan struct {
	api     componentAPI
	found   map[string]*reportedApplyFailure
	dscs    []dscObject
	modules map[string]map[string]map[string]any
	scanned int
	errs    []string
}

// scanApplyReporters reads the conditions of the DataScienceCluster, the
// Platform and every module CR (whatever kinds this operator version
// serves) and collects the apply failures keep accepts.
func scanApplyReporters(c *Client, keep func(applyFailure) bool) reporterScan {
	s := reporterScan{found: map[string]*reportedApplyFailure{}, modules: map[string]map[string]map[string]any{}}

	dscs, _, err := listFirstServed(c, dscGroup, dscListFmt, "datascienceclusters", dscFallbackVersions)
	if err != nil {
		s.errs = append(s.errs, fmt.Sprintf("DataScienceCluster: %v", err))
	}
	s.dscs = dscs
	for _, d := range dscs {
		s.scanned++
		collectApplyFailures(s.found, applyReporter{Kind: "DataScienceCluster", Name: d.Metadata.Name, UID: d.Metadata.UID}, d.Status.Conditions, keep)
	}

	platforms, _, err := listFirstServed(c, platformGroup, platformListFmt, "platforms", platformFallbackVersions)
	if err != nil {
		s.errs = append(s.errs, fmt.Sprintf("Platform: %v", err))
	}
	for _, p := range platforms {
		s.scanned++
		collectApplyFailures(s.found, applyReporter{Kind: "Platform", Name: p.Metadata.Name, UID: p.Metadata.UID, rank: 1}, p.Status.Conditions, keep)
	}

	api, err := discoverComponentAPI(c)
	if err != nil {
		s.errs = append(s.errs, err.Error())
	}
	s.api = api
	type moduleList struct {
		items []json.RawMessage
		err   error
	}
	lists := make([]moduleList, len(api.Kinds))
	parallelFor(len(api.Kinds), 6, func(i int) {
		body, _, err := c.get(componentListPath(api.Version, api.Kinds[i].Resource))
		if err != nil {
			lists[i].err = fmt.Errorf("list %s: %w", api.Kinds[i].Resource, err)
			return
		}
		var list struct {
			Items []json.RawMessage `json:"items"`
		}
		lists[i].err = json.Unmarshal(body, &list)
		lists[i].items = list.Items
	})
	for i, k := range api.Kinds {
		if lists[i].err != nil {
			s.errs = append(s.errs, lists[i].err.Error())
			continue
		}
		for _, raw := range lists[i].items {
			var it dscObject
			var obj map[string]any
			if json.Unmarshal(raw, &it) != nil || json.Unmarshal(raw, &obj) != nil {
				continue
			}
			s.scanned++
			if s.modules[k.Kind] == nil {
				s.modules[k.Kind] = map[string]map[string]any{}
			}
			s.modules[k.Kind][it.Metadata.Name] = obj
			collectApplyFailures(s.found, applyReporter{Kind: k.Kind, Name: it.Metadata.Name, UID: it.Metadata.UID, Module: strings.ToLower(k.Kind), rank: 2}, it.Status.Conditions, keep)
		}
	}
	return s
}

func checkApplyFailures(c *Client) checkOutput {
	out := checkOutput{check: CheckResult{Name: applyFailuresCheckName, Status: "pass"}}
	s := scanApplyReporters(c, allApplyFailures)
	found, api, errs, scanned := s.found, s.api, s.errs, s.scanned

	keys := make([]string, 0, len(found))
	for k, rf := range found {
		if !rf.leftover() {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	operators := newModuleOperatorFinder(c, nil)
	for _, k := range keys {
		out.problems = append(out.problems, applyFailureProblem(c, api, operators, found[k]))
	}

	switch {
	case len(out.problems) > 0:
		out.check.Status = "fail"
		out.check.Detail = fmt.Sprintf("%s the operator cannot update", countNoun(len(out.problems), "object", "objects"))
	case len(errs) > 0:
		out.check.Status = "warn"
		out.check.Detail = "No apply failures in the CRs that could be read"
	default:
		out.check.Detail = fmt.Sprintf("No apply failures reported (%s checked)", countNoun(scanned, "CR", "CRs"))
	}
	if len(errs) > 0 {
		out.check.Detail += "; could not read: " + strings.Join(errs, "; ")
	}
	return out
}

// restartGuidance names the operator that reconciles the reporting CR and
// how to restart it, from the module's operator (moduleOperatorFinder); ""
// command when the operator is not known.
func restartGuidance(operators *moduleOperatorFinder, r applyReporter) (who, cmd string) {
	ns, name := SubNS, SubName // the DataScienceCluster and the Platform
	rhoai := true
	if r.Module != "" {
		op, _, err := operators.find(r.Module, r.Name)
		if err != nil || op == nil {
			return fmt.Sprintf("the operator that reconciles %s", r), ""
		}
		ns, name, rhoai = op.Namespace, op.Name, op.RHOAI
	}
	if rhoai {
		// The RHOAI operator's Deployment belongs to its CSV (OLM reverts
		// edits to it), so its pod is deleted instead of a rollout restart.
		return fmt.Sprintf("the RHOAI operator (%s/%s)", ns, name),
			shellCommand("oc", "delete", "pod", "-n", ns, "-l", "name="+name)
	}
	return fmt.Sprintf("%s/%s", ns, name), shellCommand("oc", "rollout", "restart", "deployment/"+name, "-n", ns)
}

func applyFailureProblem(c *Client, api componentAPI, operators *moduleOperatorFinder, rf *reportedApplyFailure) Problem {
	sort.SliceStable(rf.reporters, func(i, j int) bool { return rf.reporters[i].rank > rf.reporters[j].rank })
	reporter := rf.reporters[0]
	severity := "warning"
	var names []string
	for _, r := range rf.reporters {
		names = append(names, r.String())
		if r.NotReady {
			severity = "critical"
		}
	}
	f := rf.applyFailure
	evidence := []string{fmt.Sprintf("Reported by %s", strings.Join(names, ", "))}
	for _, d := range rf.details {
		evidence = append(evidence, fmt.Sprintf("Failure deploying %s (%s): %s", f.object(), f.GroupVersion, truncateText(d, 500)))
	}
	p := Problem{
		ID:              f.genericProblemID(),
		Severity:        severity,
		AffectedObjects: []string{f.object()},
		AutoFixable:     false,
	}
	who, restart := restartGuidance(operators, reporter)
	var titles, descriptions, fixes, cmds []string

	// A schema mismatch first: re-applying the CRD comes before anything
	// that makes the operator write the object again.
	if rf.has(applySchemaMismatch) {
		crd := crdNameFor(api, f)
		titles = append(titles, "does not match its CRD's schema")
		descriptions = append(descriptions, "The live CRD and the object disagree on the type of a field, so the operator cannot update the object. "+
			"This usually follows a downgrade or a round trip to an older version: OLM applies every CRD of the bundle it installs, so an older bundle replaces a same-named CRD, "+
			"and a CRD the newer version no longer ships keeps the older schema after going back.")
		for _, field := range rf.fields[applySchemaMismatch] {
			evidence = append(evidence, "Field "+field)
		}
		if crd != "" {
			evidence = append(evidence, crdWriterEvidence(c, crd)...)
			p.AffectedObjects = append(p.AffectedObjects, "CustomResourceDefinition "+crd)
		}
		crdRef := nonEmpty(crd, fmt.Sprintf("the CRD of %s in group %s", f.Kind, f.group()))
		fixes = append(fixes, fmt.Sprintf("Re-apply %s from the installed operator version's bundle, then correct the listed fields of %s to the type that CRD expects. "+
			"The right value needs judgement, so there is no automatic fix.", crdRef, f.object()))
		cmds = append(cmds, f.ocCommand("get", "-o", "yaml"))
		if crd != "" {
			cmds = append(cmds, shellCommand("oc", "get", "crd", crd, "-o", "jsonpath={.metadata.managedFields}"))
		} else if g := f.group(); g != "" {
			cmds = append(cmds, shellCommand("oc", "api-resources", "--api-group="+g))
		}
	}
	recreate := rf.has(applyImmutable) || rf.has(applyControllerOwner)
	if rf.has(applyImmutable) {
		fields := nonEmpty(strings.Join(rf.fields[applyImmutable], ", "), "a field")
		titles = append(titles, fmt.Sprintf("must be recreated: %s cannot be changed", fields))
		descriptions = append(descriptions, fmt.Sprintf("The operator cannot apply its version of %s because %s is immutable; an older version created the object with a different value. "+
			"The operator keeps failing until the object is deleted.", f.object(), fields))
	}
	if rf.has(applyControllerOwner) {
		titles = append(titles, "has two controller owners")
		descriptions = append(descriptions, fmt.Sprintf("Ownership of %s moved between operator versions, and the object still carries the old controller reference, so the operator that owns it now cannot update it.", f.object()))
		if rf.owners != "" {
			evidence = append(evidence, "Controller references: "+rf.owners)
		}
	}
	restartStep := fmt.Sprintf("restart %s so it reconciles at once instead of after its back-off", who)
	if restart == "" {
		restartStep += " (the tool does not know which Deployment that is)"
	}
	if recreate {
		impact := "The operator recreates it on its next reconcile."
		if f.Kind == "Deployment" || f.Kind == "StatefulSet" || f.Kind == "DaemonSet" {
			impact = fmt.Sprintf("Its pods stop until the operator recreates it; for a controller %s that is safe, because the operator recreates it with the current spec.", f.Kind)
		}
		fixes = append(fixes, fmt.Sprintf("Delete %s, then %s; the operator recreates it with the current spec and owner. %s", f.object(), restartStep, impact))
		cmds = append(cmds, f.ocCommand("delete"))
	} else {
		fixes = append(fixes, "Afterwards "+restartStep+".")
	}
	if restart != "" {
		cmds = append(cmds, restart)
	}
	p.Title = fmt.Sprintf("%s %s", f.object(), strings.Join(titles, "; it also "))
	p.Description = strings.Join(descriptions, " ")
	p.Fix = strings.Join(fixes, " ")
	p.Evidence = evidence
	p.TechnicalCmd = strings.Join(cmds, "; ")
	return p
}

// crdNameFor returns the CRD of the failing object's kind when the tool
// knows its plural: the discovered module kinds and the DSC, DSCI and
// Platform. "" otherwise.
func crdNameFor(api componentAPI, f applyFailure) string {
	group := f.group()
	if group == componentAPIGroup {
		for _, k := range api.Kinds {
			if k.Kind == f.Kind {
				return k.Resource + "." + group
			}
		}
	}
	known := map[string]string{
		"datasciencecluster.opendatahub.io/DataScienceCluster": "datascienceclusters.datasciencecluster.opendatahub.io",
		"dscinitialization.opendatahub.io/DSCInitialization":   "dscinitializations.dscinitialization.opendatahub.io",
		"config.opendatahub.io/Platform":                       "platforms.config.opendatahub.io",
	}
	return known[group+"/"+f.Kind]
}

// crdWriterEvidence says who last wrote the CRD's spec.versions, from its
// managedFields. The ServiceAccount may list CRDs but get only two by name,
// so it lists with a name field selector.
func crdWriterEvidence(c *Client, crd string) []string {
	body, _, err := c.do(http.MethodGet, "/apis/apiextensions.k8s.io/v1/customresourcedefinitions", "", nil,
		url.Values{"fieldSelector": {"metadata.name=" + crd}})
	if err != nil {
		return []string{fmt.Sprintf("CRD %s: could not read its managedFields: %v", crd, err)}
	}
	var list struct {
		Items []struct {
			Metadata struct {
				ManagedFields []struct {
					Manager   string         `json:"manager"`
					Operation string         `json:"operation"`
					Time      string         `json:"time"`
					FieldsV1  map[string]any `json:"fieldsV1"`
				} `json:"managedFields"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if json.Unmarshal(body, &list) != nil || len(list.Items) == 0 {
		return []string{fmt.Sprintf("CRD %s was not found", crd)}
	}
	manager, operation, when := "", "", ""
	for _, mf := range list.Items[0].Metadata.ManagedFields {
		spec, _ := mf.FieldsV1["f:spec"].(map[string]any)
		if _, ok := spec["f:versions"]; ok && mf.Time >= when {
			manager, operation, when = mf.Manager, mf.Operation, mf.Time
		}
	}
	if manager == "" {
		return []string{fmt.Sprintf("CRD %s: its managedFields do not say who wrote spec.versions", crd)}
	}
	line := fmt.Sprintf("CRD %s: spec.versions last written by %s (%s) at %s", crd, manager, operation, nonEmpty(when, "an unknown time"))
	if strings.Contains(strings.ToLower(manager), "catalog") || strings.Contains(strings.ToLower(manager), "olm") {
		line += "; OLM's catalog operator writes the CRDs of a bundle when it installs it"
	}
	return []string{line}
}
