package cluster

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
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
)

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

func (f applyFailure) nsFlag() string {
	if f.Namespace == "" {
		return ""
	}
	return " -n " + f.Namespace
}

func (f applyFailure) key() string {
	return strings.Join([]string{string(f.Class), f.GroupVersion, f.Kind, f.Namespace, f.Name}, "|")
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
		switch {
		case strings.Contains(f.Detail, "Only one reference can have Controller set to true"):
			f.Class = applyControllerOwner
			if m := controllerOwnerFound.FindStringSubmatch(f.Detail); m != nil {
				f.Owners = strings.TrimRight(strings.TrimSpace(m[1]), ".")
			}
		case strings.Contains(f.Detail, "field is immutable"):
			f.Class = applyImmutable
			for _, m := range invalidFieldPattern.FindAllStringSubmatch(f.Detail, -1) {
				if !containsString(f.Fields, m[1]) {
					f.Fields = append(f.Fields, m[1])
				}
			}
		case strings.Contains(f.Detail, "failed to create typed") && strings.Contains(f.Detail, "expected "):
			f.Class = applySchemaMismatch
			for _, m := range typedFieldPattern.FindAllStringSubmatch(f.Detail, -1) {
				field := fmt.Sprintf("%s: expected %s, got %s", m[1], m[2], m[3])
				if !containsString(f.Fields, field) {
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
	Kind, Name string
	// Module is the lower-case kind of a components.platform.opendatahub.io
	// CR, "" for the DataScienceCluster and the Platform.
	Module   string
	NotReady bool // its Ready condition is False
	rank     int  // module CR 2, Platform 1, DSC 0: the most specific reports first
}

func (r applyReporter) String() string { return r.Kind + " " + r.Name }

// reportedApplyFailure is an apply failure with every CR that reports it.
type reportedApplyFailure struct {
	applyFailure
	reporters []applyReporter
}

// collectApplyFailures scans the conditions of one CR.
func collectApplyFailures(found map[string]*reportedApplyFailure, r applyReporter, conds []dscCondition) {
	for _, cond := range conds {
		if cond.Type == "Ready" && cond.Status == "False" {
			r.NotReady = true
		}
	}
	for _, cond := range conds {
		for _, f := range parseApplyFailures(cond.Message) {
			k := f.key()
			if found[k] == nil {
				found[k] = &reportedApplyFailure{applyFailure: f}
			}
			rf := found[k]
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

func checkApplyFailures(c *Client) checkOutput {
	out := checkOutput{check: CheckResult{Name: applyFailuresCheckName, Status: "pass"}}
	found := map[string]*reportedApplyFailure{}
	var errs []string
	scanned := 0

	dscs, _, err := listFirstServed(c, "/apis/datasciencecluster.opendatahub.io/%s/datascienceclusters", "datascienceclusters")
	if err != nil {
		errs = append(errs, fmt.Sprintf("DataScienceCluster: %v", err))
	}
	for _, d := range dscs {
		scanned++
		collectApplyFailures(found, applyReporter{Kind: "DataScienceCluster", Name: d.Metadata.Name}, d.Status.Conditions)
	}

	body, _, err := c.get("/apis/config.opendatahub.io/v1alpha1/platforms")
	switch {
	case IsK8sError(err, http.StatusNotFound):
	case err != nil:
		errs = append(errs, fmt.Sprintf("Platform: %v", err))
	default:
		var list struct {
			Items []dscObject `json:"items"`
		}
		if json.Unmarshal(body, &list) == nil {
			for _, p := range list.Items {
				scanned++
				collectApplyFailures(found, applyReporter{Kind: "Platform", Name: p.Metadata.Name, rank: 1}, p.Status.Conditions)
			}
		}
	}

	// Module CRs, whatever kinds this operator version serves.
	api, err := discoverComponentAPI(c)
	if err != nil {
		errs = append(errs, err.Error())
	}
	type moduleList struct {
		items []dscObject
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
			Items []dscObject `json:"items"`
		}
		lists[i].err = json.Unmarshal(body, &list)
		lists[i].items = list.Items
	})
	for i, k := range api.Kinds {
		if lists[i].err != nil {
			errs = append(errs, lists[i].err.Error())
			continue
		}
		for _, it := range lists[i].items {
			scanned++
			collectApplyFailures(found, applyReporter{Kind: k.Kind, Name: it.Metadata.Name, Module: strings.ToLower(k.Kind), rank: 2}, it.Status.Conditions)
		}
	}

	keys := make([]string, 0, len(found))
	for k := range found {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out.problems = append(out.problems, applyFailureProblem(c, api, found[k]))
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
// how to restart it, from the module-operator mapping; "" command when the
// operator is not known.
func restartGuidance(r applyReporter) (who, cmd string) {
	ns, name := SubNS, SubName // the DataScienceCluster and the Platform
	if r.Module != "" {
		op, ok := moduleOperators[r.Module]
		if !ok {
			return fmt.Sprintf("the operator that reconciles %s", r), ""
		}
		ns, name = op.namespace, op.name
	}
	if ns == SubNS && name == SubName {
		// The RHOAI operator's Deployment belongs to its CSV (OLM reverts
		// edits to it), so its pod is deleted instead of a rollout restart.
		return fmt.Sprintf("the RHOAI operator (%s/%s)", ns, name),
			fmt.Sprintf("oc delete pod -n %s -l name=%s", ns, name)
	}
	return fmt.Sprintf("%s/%s", ns, name), fmt.Sprintf("oc rollout restart deployment/%s -n %s", name, ns)
}

func applyFailureProblem(c *Client, api componentAPI, rf *reportedApplyFailure) Problem {
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
	evidence := []string{
		fmt.Sprintf("Reported by %s: failure deploying %s (%s): %s", strings.Join(names, ", "), f.object(), f.GroupVersion, truncate(f.Detail, 500)),
	}
	id := "operator-apply-failed-" + string(f.Class) + "-" + strings.ToLower(strings.Join([]string{f.Kind, f.Namespace, f.Name}, "-"))
	p := Problem{
		ID:              id,
		Severity:        severity,
		AffectedObjects: []string{f.object()},
		AutoFixable:     false,
	}
	who, restart := restartGuidance(reporter)
	deleteCmd := fmt.Sprintf("oc delete %s %s%s", f.resourceArg(), f.Name, f.nsFlag())
	impact := "The operator recreates it on its next reconcile."
	if f.Kind == "Deployment" || f.Kind == "StatefulSet" || f.Kind == "DaemonSet" {
		impact = fmt.Sprintf("Its pods stop until the operator recreates it; for a controller %s that is safe, because the operator recreates it with the current spec.", f.Kind)
	}
	restartStep := fmt.Sprintf("then restart %s so it reconciles at once instead of after its back-off", who)
	if restart == "" {
		restartStep = fmt.Sprintf("then restart %s so it reconciles at once instead of after its back-off (the tool does not know which Deployment that is)", who)
	}
	switch f.Class {
	case applyImmutable:
		fields := nonEmpty(strings.Join(f.Fields, ", "), "a field")
		p.Title = fmt.Sprintf("%s must be recreated: %s cannot be changed", f.object(), fields)
		p.Description = fmt.Sprintf("The operator cannot apply its version of %s because %s is immutable; an older version created the object with a different value. "+
			"The operator keeps failing until the object is deleted.", f.object(), fields)
		p.Fix = fmt.Sprintf("Delete %s, %s. %s", f.object(), restartStep, impact)
	case applyControllerOwner:
		p.Title = fmt.Sprintf("%s has two controller owners", f.object())
		p.Description = fmt.Sprintf("Ownership of %s moved between operator versions, and the object still carries the old controller reference, so the operator that owns it now cannot update it.", f.object())
		if f.Owners != "" {
			evidence = append(evidence, "Controller references: "+f.Owners)
		}
		p.Fix = fmt.Sprintf("Delete %s, %s; the operator recreates it with the right owner. %s", f.object(), restartStep, impact)
	case applySchemaMismatch:
		crd := crdNameFor(api, f)
		p.Title = fmt.Sprintf("%s does not match its CRD's schema", f.object())
		p.Description = "The live CRD and the object disagree on the type of a field, so the operator cannot update the object. " +
			"This usually follows a downgrade or a round trip to an older version: OLM applies every CRD of the bundle it installs, so an older bundle replaces a same-named CRD, " +
			"and a CRD the newer version no longer ships keeps the older schema after going back."
		for _, field := range f.Fields {
			evidence = append(evidence, "Field "+field)
		}
		if crd != "" {
			evidence = append(evidence, crdWriterEvidence(c, crd)...)
			p.AffectedObjects = append(p.AffectedObjects, "CustomResourceDefinition "+crd)
		}
		crdRef := nonEmpty(crd, fmt.Sprintf("the CRD of %s in group %s", f.Kind, f.group()))
		p.Fix = fmt.Sprintf("Re-apply %s from the installed operator version's bundle, then correct the listed fields of %s to the type that CRD expects. "+
			"The right value needs judgement, so there is no automatic fix. Afterwards restart %s.", crdRef, f.object(), who)
		cmds := []string{fmt.Sprintf("oc get %s %s%s -o yaml", f.resourceArg(), f.Name, f.nsFlag())}
		if crd != "" {
			cmds = append(cmds, fmt.Sprintf("oc get crd %s -o jsonpath='{.metadata.managedFields}'", crd))
		} else if g := f.group(); g != "" {
			cmds = append(cmds, "oc api-resources --api-group="+g)
		}
		p.Evidence = evidence
		p.TechnicalCmd = strings.Join(cmds, "; ")
		return p
	}
	p.Evidence = evidence
	cmds := []string{deleteCmd}
	if restart != "" {
		cmds = append(cmds, restart)
	}
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
					Manager   string                 `json:"manager"`
					Operation string                 `json:"operation"`
					Time      string                 `json:"time"`
					FieldsV1  map[string]interface{} `json:"fieldsV1"`
				} `json:"managedFields"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if json.Unmarshal(body, &list) != nil || len(list.Items) == 0 {
		return []string{fmt.Sprintf("CRD %s was not found", crd)}
	}
	manager, operation, when := "", "", ""
	for _, mf := range list.Items[0].Metadata.ManagedFields {
		spec, _ := mf.FieldsV1["f:spec"].(map[string]interface{})
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
