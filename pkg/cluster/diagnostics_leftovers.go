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
	"time"
)

// "Left over from an earlier release": objects that an older release left
// in a shape the running operator cannot apply, found from the operator's
// own failure reports (the conditions of the DataScienceCluster, the
// Platform and every module CR, in components.platform.opendatahub.io and
// services.platform.opendatahub.io) and confirmed with live reads, never
// from a list of known objects.
//
// Live on RHOAI 3.6.0 (2026-11-19, a cluster upgraded over several
// releases), the DataScienceCluster was Not Ready while every component
// said Ready:
//
//  1. Stored field types. The DSC reported
//     "failure deploying resource /default-trustyai: apply failed
//     components.platform.opendatahub.io/v1alpha1, Kind=TrustyAI: unable to
//     patch ...: failed to create manager for existing fields: failed to
//     convert new object (...) to smd typed: errors:
//     .spec.eval.lmeval.permitOnline: expected string, got
//     &value.valueUnstructured{Value:false}". The stored module CR had
//     booleans that an earlier release wrote, and the CRD now says
//     "type: string, enum [allow, deny], default deny". Kubernetes keeps a
//     stored object as written, so the upgrade never changed it. This
//     server-side apply error is raised only for a live object without
//     managedFields: the apply then first records the live object's fields
//     under the manager "before-first-apply", converting it to the schema's
//     typed form, and a stored value of another type fails that conversion
//     ("failed to create manager for existing fields", kubernetes v1.34
//     staging/src/k8s.io/apimachinery/pkg/util/managedfields/internal/
//     skipnonapplied.go:75-91). An object loses its managedFields when a
//     non-apply update cannot track them, which is reported nowhere: the
//     field manager removes them instead (fieldmanager.go:145-164,
//     UpdateNoErrors). The module operator, whose typed informer cannot
//     decode the object, crash-looped ("failed waiting for
//     *v1alpha1.TrustyAI Informer to sync"). Fixed by setting both fields to
//     the CRD default, after a person checked that false meant "not
//     permitted", which is "deny".
//  2. Changed selector. Then the module CR reported "failure deploying
//     resource redhat-ods-applications/trustyai-service-operator-controller
//     -manager: apply failed apps/v1, Kind=Deployment: ... spec.selector:
//     Invalid value: ...: field is immutable". An older release created the
//     Deployment (owned by the module CR) with another selector. Fixed by
//     deleting it; the module operator created it again and the DSC became
//     Ready.
//
// Both fixes run with the user's token after the checks below, which fail
// closed. Whether the operator retries after the field fix was read from
// the operator source (rhods-operator rhoai-3.6 datasciencecluster
// _controller.go): the DSC controller watches every module CR it
// configures with the dependent predicate (odh-platform-utilities
// framework v0.3.1 predicates/dependent), which passes any change of an
// object other than its resourceVersion and managedFields, so the patch
// itself makes the DSC reconcile; a watch event bypasses the controller's
// back-off. The fix therefore does not nudge it. If a scan still finds the
// error after the fields are valid, a separate "Retry now" adds and removes
// an annotation on the DataScienceCluster: its own watch passes annotation
// changes (the framework's default For predicates include
// AnnotationChangedPredicate), which the live recovery used.

const (
	leftoversCheckName = "Left over from an earlier release"
	leftoverIDPrefix   = "leftover-"

	fixLeftoverFields   = "fix-leftover-fields:" // <Kind>/<name>
	fixLeftoverRecreate = "recreate-leftover:"   // <Kind>/<namespace>/<name>
	fixLeftoverRetry    = "retry-dsc-reconcile:" // <DataScienceCluster name>
	leftoverRetryAnno   = "rhoai-nightly-updater.opendatahub.io/retry"
	leftoverManager     = "rhoai-nightly-updater-diagnostics"
)

var (
	// leftoverRecreateWait bounds how long a recreate fix waits for the
	// operator to create the workload again and for its pods to be ready.
	leftoverRecreateWait = 2 * time.Minute
	leftoverRecreatePoll = 5 * time.Second
)

func leftoverFieldsID(kind, name string) string {
	return leftoverIDPrefix + "fields-" + strings.ToLower(kind+"-"+name)
}

func leftoverRecreateID(kind, namespace, name string) string {
	return leftoverIDPrefix + "recreate-" + strings.ToLower(kind+"-"+namespace+"-"+name)
}

// leftoverObject returns the object's reported failures when the
// leftovers check handles it (rf.leftover) with the class want, else nil.
// Every failure of the object is collected first, so an object with other
// failures too is never taken.
func leftoverObject(s reporterScan, group, kind, namespace, name string, want applyFailureClass) *reportedApplyFailure {
	rf := s.found[strings.Join([]string{group, kind, namespace, name}, "|")]
	if rf == nil || !rf.leftover() || !rf.has(want) {
		return nil
	}
	return rf
}

// usersReader lists what still uses a component (componentUsers); nil when
// the caller cannot read that.
type usersReader func(c *Client, component string) ([]string, error)

// checkLeftovers is the "Left over from an earlier release" check.
func checkLeftovers(c *Client) checkOutput { return leftoversCheck(c, componentUsers) }

// checkLeftoversGitOps is checkLeftovers in the gitops profile, whose
// ServiceAccount does not read the resources that tell what uses a
// component (deploy/rbac-gitops.yaml); fixes are off there anyway.
func checkLeftoversGitOps(c *Client) checkOutput { return leftoversCheck(c, nil) }

func leftoversCheck(c *Client, users usersReader) checkOutput {
	out := checkOutput{check: CheckResult{Name: leftoversCheckName, Status: "pass"}}
	s := scanApplyReporters(c, allApplyFailures)
	appNS := applicationsNamespace(c)
	keys := make([]string, 0, len(s.found))
	for k, rf := range s.found {
		if rf.leftover() {
			keys = append(keys, k)
		}
	}
	// Stored field types first: the module operator cannot run until they
	// are fixed, and it is what recreates a workload.
	sort.Slice(keys, func(i, j int) bool {
		si, sj := s.found[keys[i]].has(applyStoredType), s.found[keys[j]].has(applyStoredType)
		if si != sj {
			return si
		}
		return keys[i] < keys[j]
	})
	for _, k := range keys {
		rf := s.found[k]
		switch {
		case rf.has(applyStoredType):
			st := analyzeStored(c, s, rf)
			st.Crash = moduleOperatorCrash(c, st, time.Now())
			out.problems = append(out.problems, st.problem())
		case rf.has(applyImmutable):
			r := analyzeRecreate(c, rf, appNS, readDeploymentForScan, users)
			out.problems = append(out.problems, r.problem())
		}
	}
	switch {
	case len(out.problems) > 0:
		out.check.Status = "fail"
		out.check.Detail = fmt.Sprintf("%s left over from an earlier release", countNoun(len(out.problems), "object", "objects"))
	case len(s.errs) > 0:
		out.check.Status = "warn"
		out.check.Detail = "Nothing left over in the CRs that could be read"
	default:
		out.check.Detail = fmt.Sprintf("No stored field of an older type and no changed selector reported (%s checked)", countNoun(s.scanned, "CR", "CRs"))
	}
	if len(s.errs) > 0 {
		out.check.Detail += "; could not read: " + strings.Join(s.errs, "; ")
	}
	return out
}

// --- Stored field types ------------------------------------------------------

// fieldSchema is what a CRD's structural schema says about one field.
type fieldSchema struct {
	Found, HasDefault, Required, IntOrString, Nullable bool
	Type                                               string
	Enum                                               []any
	Default                                            any
}

func (s fieldSchema) text() string {
	parts := []string{nonEmpty(s.Type, "any type")}
	if s.IntOrString {
		parts[0] = "integer or string"
	}
	if len(s.Enum) > 0 {
		var vals []string
		for _, e := range s.Enum {
			vals = append(vals, fieldText(e))
		}
		parts = append(parts, "one of "+strings.Join(vals, ", "))
	}
	if s.HasDefault {
		parts = append(parts, "default "+fieldText(s.Default))
	}
	if s.Required {
		parts = append(parts, "required")
	} else {
		parts = append(parts, "optional")
	}
	return strings.Join(parts, ", ")
}

// leftoverField is one field the operator's error names.
type leftoverField struct {
	Path     string   // as reported: ".spec.eval.lmeval.permitOnline"
	Keys     []string // the path as keys; nil when it is not a plain field path
	Expected string   // the type the error names ("string", "boolean", "numeric", "map", "list")
	Value    any
	Present  bool
	Schema   fieldSchema
	Change   string // "set-default", "remove", "none" or "valid"
	After    any
	Why      string
	Review   bool
}

var (
	reportedField = regexp.MustCompile(`^(\.\S+): expected ([A-Za-z]+), got (.*)$`)
	fieldKey      = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
)

// fieldKeys splits ".spec.a.b" into keys; nil for list elements, quoted
// keys or anything outside spec (status is the operator's, metadata is not
// a schema field).
func fieldKeys(path string) []string {
	keys := strings.Split(strings.TrimPrefix(path, "."), ".")
	if len(keys) < 2 || keys[0] != "spec" {
		return nil
	}
	for _, k := range keys {
		if !fieldKey.MatchString(k) {
			return nil
		}
	}
	return keys
}

// schemaAt walks a structural schema (openAPIV3Schema) along keys.
func schemaAt(root map[string]any, keys []string) fieldSchema {
	node, parent := root, map[string]any(nil)
	for _, k := range keys {
		props, _ := node["properties"].(map[string]any)
		child, ok := props[k].(map[string]any)
		if !ok {
			return fieldSchema{}
		}
		parent, node = node, child
	}
	s := fieldSchema{Found: true}
	s.Type, _ = node["type"].(string)
	s.Enum, _ = node["enum"].([]any)
	s.Default, s.HasDefault = node["default"]
	s.IntOrString, _ = node["x-kubernetes-int-or-string"].(bool)
	s.Nullable, _ = node["nullable"].(bool)
	if parent != nil {
		req, _ := parent["required"].([]any)
		for _, r := range req {
			if r == keys[len(keys)-1] {
				s.Required = true
			}
		}
	}
	return s
}

// valueMatches reports whether a stored JSON value has the schema's type
// (and is one of its enum values).
func valueMatches(v any, s fieldSchema) bool {
	if v == nil {
		return s.Nullable
	}
	whole := func() bool { f, ok := v.(float64); return ok && f == float64(int64(f)) }
	ok := true
	switch {
	case s.IntOrString:
		_, isString := v.(string)
		ok = isString || whole()
	case s.Type == "string":
		_, ok = v.(string)
	case s.Type == "boolean":
		_, ok = v.(bool)
	case s.Type == "integer":
		ok = whole()
	case s.Type == "number":
		_, ok = v.(float64)
	case s.Type == "object":
		_, ok = v.(map[string]any)
	case s.Type == "array":
		_, ok = v.([]any)
	}
	if ok && len(s.Enum) > 0 {
		found := false
		for _, e := range s.Enum {
			found = found || jsonEqual(e, v)
		}
		ok = found
	}
	return ok
}

// smdTypeName is the word structured-merge-diff uses in "expected <type>"
// for a schema type.
func smdTypeName(s fieldSchema) string {
	if s.IntOrString {
		return ""
	}
	return map[string]string{"string": "string", "boolean": "boolean", "integer": "numeric", "number": "numeric", "object": "map", "array": "list"}[s.Type]
}

// afterText words the field's value after the fix.
func (lf *leftoverField) afterText() string {
	if lf.Change == "set-default" {
		return fieldJSON(lf.After)
	}
	return "no value (removed)"
}

// decide fills in the field's plan from the live object and the schema.
// Every change needs review (an acknowledgement): the tool cannot know
// what a value of the older type meant, so it cannot tell that the
// default, or no value, means the same. Nothing in the CRD or the
// operator relates the old value to the new one; a value that looks like
// "off" (false, 0, "") is no exception.
func (lf *leftoverField) decide(obj map[string]any, root map[string]any) {
	lf.Keys = fieldKeys(lf.Path)
	if lf.Keys == nil {
		lf.Change, lf.Why = "none", "the path is not a plain field under spec, so the tool does not change it"
		return
	}
	lf.Value, lf.Present = lookupPath(obj, lf.Keys)
	lf.Schema = schemaAt(root, lf.Keys)
	switch {
	case !lf.Present:
		lf.Change, lf.Why = "valid", "the object no longer has this field"
		return
	case !lf.Schema.Found:
		lf.Change, lf.Why = "none", "the installed CRD does not describe this field"
		return
	case valueMatches(lf.Value, lf.Schema):
		lf.Change, lf.Why = "valid", "the stored value already matches the installed CRD"
		return
	case lf.Expected != "" && smdTypeName(lf.Schema) != lf.Expected:
		lf.Change, lf.Why = "none", fmt.Sprintf("the operator's error expects %s, but the installed CRD says %s, so the error is about another version of the schema", lf.Expected, nonEmpty(lf.Schema.Type, "no type"))
		return
	case lf.Schema.HasDefault && valueMatches(lf.Schema.Default, lf.Schema):
		lf.Change, lf.After, lf.Review = "set-default", lf.Schema.Default, true
		lf.Why = fmt.Sprintf("the installed CRD's default; the stored %s may not mean the same as %s, so check what the field means before you confirm", fieldJSON(lf.Value), fieldJSON(lf.Schema.Default))
	case !lf.Schema.Required:
		lf.Change, lf.Review = "remove", true
		lf.Why = fmt.Sprintf("the field is optional and the installed CRD gives no default, so it is removed; the stored %s may have meant something that no value does not, so check what the field means before you confirm", fieldJSON(lf.Value))
	default:
		lf.Change, lf.Why = "none", "the field is required and the CRD gives no default, so the right value needs judgement"
	}
}

// storedLeftover is a module CR whose stored fields have older types.
type storedLeftover struct {
	rf                                        *reportedApplyFailure
	Kind, Name, Group, Version, Resource, CRD string
	UID, ResourceVersion                      string
	Fields                                    []*leftoverField
	Blocker                                   string // why there is no automatic fix
	CRDNote                                   string
	Crash                                     *leftoverCrash
	object                                    map[string]any
	notReady                                  bool
}

func (st *storedLeftover) ref() string { return st.Kind + " " + st.Name }

// changes are the fields the fix changes.
func (st *storedLeftover) changes() []*leftoverField {
	var out []*leftoverField
	for _, lf := range st.Fields {
		if lf.Change == "set-default" || lf.Change == "remove" {
			out = append(out, lf)
		}
	}
	return out
}

func (st *storedLeftover) needsReview() bool {
	for _, lf := range st.changes() {
		if lf.Review {
			return true
		}
	}
	return false
}

// analyzeStored reads the object and its CRD for a stored-type failure.
// Every read error leaves a Blocker (fail closed).
func analyzeStored(c *Client, s reporterScan, rf *reportedApplyFailure) *storedLeftover {
	f := rf.applyFailure
	st := &storedLeftover{rf: rf, Kind: f.Kind, Name: f.Name}
	_, st.Version, _ = strings.Cut(f.GroupVersion, "/")
	for _, r := range rf.reporters {
		st.notReady = st.notReady || r.NotReady
	}
	for _, raw := range rf.fields[applyStoredType] {
		if m := reportedField.FindStringSubmatch(raw); m != nil {
			st.Fields = append(st.Fields, &leftoverField{Path: m[1], Expected: m[2]})
		}
	}
	st.Group = f.group()
	if !isModuleAPIGroup(st.Group) {
		st.Blocker = fmt.Sprintf("the tool changes only module CRs (%s), and this is a %s", strings.Join(moduleAPIGroups, ", "), f.GroupVersion)
		return st
	}
	k, ok := s.api.kindOf(st.Group, f.Kind)
	if !ok {
		st.Blocker = fmt.Sprintf("%s does not serve the kind %s, so the tool cannot read the object", st.Group, f.Kind)
		return st
	}
	st.Resource = k.Resource
	st.CRD = k.crd()

	obj := s.modules[moduleScanKey(st.Group, f.Kind)][f.Name]
	if st.Version != k.Version || obj == nil {
		var err error
		obj, err = listModuleObject(c, st.Group, st.Version, st.Resource, f.Name)
		if err != nil {
			st.Blocker = fmt.Sprintf("could not read %s: %v", st.ref(), err)
			return st
		}
	}
	if obj == nil {
		st.Blocker = fmt.Sprintf("%s does not exist (any more)", st.ref())
		return st
	}
	st.object = obj
	meta, _ := obj["metadata"].(map[string]any)
	st.UID, _ = meta["uid"].(string)
	st.ResourceVersion, _ = meta["resourceVersion"].(string)
	if del, _ := meta["deletionTimestamp"].(string); del != "" {
		st.Blocker = fmt.Sprintf("%s is being deleted", st.ref())
		return st
	}

	crd, err := readCRDByName(c, st.CRD)
	if err != nil {
		st.Blocker = fmt.Sprintf("could not read CRD %s: %v", st.CRD, err)
		return st
	}
	root := crdSchema(crd, st.Version)
	if root == nil {
		st.Blocker = fmt.Sprintf("CRD %s has no schema for version %s", st.CRD, st.Version)
		return st
	}
	current, note := crdCurrent(c, crd, st.CRD)
	st.CRDNote = note
	for _, lf := range st.Fields {
		lf.decide(obj, root)
	}
	if !current {
		st.Blocker = note
		return st
	}
	for _, lf := range st.Fields {
		if lf.Change == "none" {
			st.Blocker = fmt.Sprintf("%s has no automatic fix: %s", lf.Path, lf.Why)
			return st
		}
	}
	return st
}

// listModuleObject finds a module CR by listing its resource (the
// ServiceAccount may list, not get, module CRs). nil when it does not exist.
func listModuleObject(c *Client, group, version, resource, name string) (map[string]any, error) {
	body, _, err := c.Do(http.MethodGet, componentListPath(group, version, resource), "", nil,
		url.Values{"fieldSelector": {"metadata.name=" + name}})
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("parse %s: %w", resource, err)
	}
	for _, it := range list.Items {
		if meta, _ := it["metadata"].(map[string]any); meta["name"] == name {
			return it, nil
		}
	}
	return nil, nil
}

// readCRDByName lists the CRDs with a name field selector: the
// ServiceAccount may list CRDs but get only one by name. An error when the
// CRD does not exist.
func readCRDByName(c *Client, name string) (map[string]any, error) {
	body, _, err := c.Do(http.MethodGet, "/apis/apiextensions.k8s.io/v1/customresourcedefinitions", "", nil,
		url.Values{"fieldSelector": {"metadata.name=" + name}})
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("parse CRDs: %w", err)
	}
	for _, it := range list.Items {
		if meta, _ := it["metadata"].(map[string]any); meta["name"] == name {
			return it, nil
		}
	}
	return nil, fmt.Errorf("CRD %s does not exist", name)
}

// crdSchema is the openAPIV3Schema of one version of a CRD.
func crdSchema(crd map[string]any, version string) map[string]any {
	spec, _ := crd["spec"].(map[string]any)
	versions, _ := spec["versions"].([]any)
	for _, v := range versions {
		m, _ := v.(map[string]any)
		if m["name"] != version {
			continue
		}
		schema, _ := m["schema"].(map[string]any)
		root, _ := schema["openAPIV3Schema"].(map[string]any)
		return root
	}
	return nil
}

// platformCRDManager is the field manager the RHOAI operator writes the
// module CRDs with: its deploy action applies every CRD it renders with
// client.FieldOwner(resources.PlatformFieldOwner) (odh-platform-utilities
// framework v0.3.1 controller/actions/deploy/action_deploy.go:389-396,
// deployCRD), and PlatformFieldOwner is "platform.opendatahub.io"
// (framework/resources/resources.go:30). rhods-operator rhoai-3.6 uses that
// deploy action (pkg/controller/actions/deploy/action_deploy.go aliases it;
// pkg/resources/resources.go:15 re-exports PlatformFieldOwner) in the
// Platform controller that applies the module charts and manifests.
const platformCRDManager = "platform.opendatahub.io"

// crdCurrent decides whether the installed CRD, not the object, is the
// current shape. A downgrade or a round trip can leave a CRD at an older
// bundle's schema (diagnostics_apply_failures.go), and then changing the
// object to match it would be wrong. The CRD counts as current only when
//   - (a) the installed CSV owns it (spec.customresourcedefinitions.owned):
//     it is part of the installed operator's bundle, which OLM installs, or
//   - (b) its spec.versions was last written (managedFields, the newest
//     entry that owns f:spec.f:versions) by platformCRDManager, the RHOAI
//     operator's own field manager.
//
// Anything else (another or no manager, two managers with the same newest
// time, anything that cannot be read) means the tool cannot tell that the
// CRD is the newer side, and it refuses (fail closed).
func crdCurrent(c *Client, crd map[string]any, name string) (bool, string) {
	csv, owned, csvErr := installedCSVOwnsCRD(c, name)
	if csvErr == nil && owned {
		return true, fmt.Sprintf("CRD %s is part of the installed operator's bundle (the installed CSV %s owns it)", name, csv)
	}
	csvNote := fmt.Sprintf("the installed CSV %s does not own it", nonEmpty(csv, "(unknown)"))
	if csvErr != nil {
		csvNote = fmt.Sprintf("the installed CSV could not be read (%v)", csvErr)
	}
	meta, _ := crd["metadata"].(map[string]any)
	entries, _ := meta["managedFields"].([]any)
	when := ""
	managers := map[string]bool{}
	for _, e := range entries {
		mf, _ := e.(map[string]any)
		fields, _ := mf["fieldsV1"].(map[string]any)
		spec, _ := fields["f:spec"].(map[string]any)
		t, _ := mf["time"].(string)
		if _, ok := spec["f:versions"]; !ok {
			continue
		}
		m, _ := mf["manager"].(string)
		switch {
		case t > when:
			when, managers = t, map[string]bool{m: true}
		case t == when:
			managers[m] = true
		}
	}
	names := make([]string, 0, len(managers))
	for m := range managers {
		names = append(names, m)
	}
	sort.Strings(names)
	switch {
	case len(names) == 0:
		return false, fmt.Sprintf("the tool can't tell that CRD %s is the newer side: %s, and its managedFields do not say who wrote its schema", name, csvNote)
	case len(names) > 1:
		return false, fmt.Sprintf("the tool can't tell that CRD %s is the newer side: %s, and its schema was last written by %s at the same time (%s)", name, csvNote, strings.Join(names, " and "), nonEmpty(when, "an unknown time"))
	case names[0] != platformCRDManager:
		return false, fmt.Sprintf("the tool can't tell that CRD %s is the newer side: %s, and its schema was last written by %s at %s, not by the RHOAI operator (field manager %s)", name, csvNote, nonEmpty(names[0], "(no name)"), nonEmpty(when, "an unknown time"), platformCRDManager)
	}
	return true, fmt.Sprintf("CRD %s's schema was last written by the RHOAI operator (field manager %s) at %s", name, platformCRDManager, nonEmpty(when, "an unknown time"))
}

// installedCSVOwnsCRD reports whether the installed RHOAI CSV lists the CRD
// among its owned CRDs.
func installedCSVOwnsCRD(c *Client, crd string) (string, bool, error) {
	sub, err := readSubscriptionState(c)
	if err != nil {
		return "", false, err
	}
	if sub.InstalledCSV == "" {
		return "", false, fmt.Errorf("the Subscription names no installed CSV")
	}
	var csv struct {
		Spec struct {
			CRDs struct {
				Owned []struct {
					Name string `json:"name"`
				} `json:"owned"`
			} `json:"customresourcedefinitions"`
		} `json:"spec"`
	}
	if err := c.GetInto(namespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", SubNS, sub.InstalledCSV), &csv); err != nil {
		if isDecodeError(err) {
			return sub.InstalledCSV, false, fmt.Errorf("parse CSV %s: %w", sub.InstalledCSV, err)
		}
		return sub.InstalledCSV, false, err
	}
	for _, o := range csv.Spec.CRDs.Owned {
		if o.Name == crd {
			return sub.InstalledCSV, true, nil
		}
	}
	return sub.InstalledCSV, false, nil
}

// patchOps is the JSON Patch of the fix: the object's UID and
// resourceVersion (the object the user reviewed) and each field's stored
// value are tested first, so nothing changes if anything moved.
func (st *storedLeftover) patchOps() []jsonPatchOp {
	ops := []jsonPatchOp{patchOp("test", []string{"metadata", "uid"}, st.UID), patchOp("test", []string{"metadata", "resourceVersion"}, st.ResourceVersion)}
	for _, lf := range st.changes() {
		ops = append(ops, patchOp("test", lf.Keys, lf.Value))
		if lf.Change == "set-default" {
			ops = append(ops, patchOp("replace", lf.Keys, lf.After))
		} else {
			ops = append(ops, patchOp("remove", lf.Keys, nil))
		}
	}
	return ops
}

// leftoverCrash is a module operator that keeps crashing.
type leftoverCrash struct {
	Deployment string
	Pods       []string // "Pod <ns>/<name>"
	Restarts   int
	Message    string
	Confirmed  bool // its last exit names the informer of the stored kind
}

// moduleOperatorCrash finds the module operator of a stored leftover's
// module and reports it when its pods crash-loop or keep restarting. It is
// Confirmed when a pod's last exit message says it could not sync the
// informer of the leftover's kind (only there when the container writes
// its log as the termination message; the pod log itself is not read).
func moduleOperatorCrash(c *Client, st *storedLeftover, now time.Time) *leftoverCrash {
	if st.Resource == "" {
		return nil
	}
	d, _ := findModuleOperator(c, newModuleOperatorFinder(c, nil), strings.ToLower(st.Kind))
	if d == nil || len(d.TemplateLabels) == 0 {
		return nil
	}
	var sel []string
	for k, v := range d.TemplateLabels {
		sel = append(sel, k+"="+v)
	}
	sort.Strings(sel)
	pods, err := listDiagPods(c, d.Namespace, strings.Join(sel, ","))
	if err != nil {
		return nil
	}
	informer := regexp.MustCompile(`\*[a-z0-9]+\.` + regexp.QuoteMeta(st.Kind) + ` Informer to sync`)
	var crash *leftoverCrash
	for _, p := range pods {
		if p.Metadata.Namespace == "" {
			p.Metadata.Namespace = d.Namespace
		}
		is := classifyPod(p, now)
		if is == nil || (is.Kind != issueCrashLoop && is.Kind != issueRestarting) {
			continue
		}
		if crash == nil {
			crash = &leftoverCrash{Deployment: d.ref()}
		}
		crash.Pods = append(crash.Pods, "Pod "+is.Namespace+"/"+is.Pod)
		if is.Restarts > crash.Restarts {
			crash.Restarts = is.Restarts
		}
		if informer.MatchString(is.Message) {
			crash.Confirmed, crash.Message = true, is.Message
		} else if crash.Message == "" {
			crash.Message = is.Message
		}
	}
	return crash
}

func (st *storedLeftover) fieldChanges() []FieldChange {
	var out []FieldChange
	for _, lf := range st.Fields {
		fc := FieldChange{Object: st.ref(), Field: lf.Path, Change: nonEmpty(lf.Change, "none"), Note: lf.Why, Review: lf.Review}
		if lf.Present {
			fc.Now = fieldJSON(lf.Value)
		}
		if lf.Change == "set-default" {
			fc.After = fieldJSON(lf.After)
		}
		if lf.Schema.Found {
			fc.Schema = lf.Schema.text()
		} else {
			fc.Schema = "not described by the installed CRD"
		}
		if lf.Change == "" {
			fc.Note = st.Blocker
		}
		out = append(out, fc)
	}
	return out
}

func fieldJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// reportersText names the CRs that report the failure, the most specific
// first.
func reportersText(rf *reportedApplyFailure) string {
	sort.SliceStable(rf.reporters, func(i, j int) bool { return rf.reporters[i].rank > rf.reporters[j].rank })
	var names []string
	for _, r := range rf.reporters {
		names = append(names, r.String())
	}
	return strings.Join(names, ", ")
}

func (st *storedLeftover) problem() Problem {
	rf := st.rf
	severity := "warning"
	if st.notReady {
		severity = "critical"
	}
	reporters := reportersText(rf)
	evidence := []string{"Reported by " + reporters}
	for _, d := range rf.details {
		evidence = append(evidence, "Operator error: "+truncateText(d, 500))
	}
	for _, lf := range st.Fields {
		line := fmt.Sprintf("Field %s: ", lf.Path)
		switch {
		case !lf.Present && lf.Keys != nil && st.object != nil:
			line += "not stored any more"
		case lf.Present:
			line += "stored " + fieldJSON(lf.Value)
		default:
			line += "not read"
		}
		if lf.Schema.Found {
			line += "; the installed CRD says " + lf.Schema.text()
		}
		evidence = append(evidence, line)
	}
	if st.CRDNote != "" {
		evidence = append(evidence, st.CRDNote)
	}
	p := Problem{
		ID:              leftoverFieldsID(st.Kind, st.Name),
		Severity:        severity,
		AffectedObjects: []string{st.ref()},
		FieldChanges:    st.fieldChanges(),
		TechnicalCmd:    shellCommand("oc", "get", strings.ToLower(st.Kind)+"."+st.Group, st.Name, "-o", "yaml"),
	}
	description := fmt.Sprintf("The operator cannot apply %s: the stored object has field values of a type an earlier release used, and the installed CRD now expects another type. "+
		"Kubernetes keeps a stored object as it was written, so the upgrade did not change it, and server-side apply cannot convert it. Until it matches, %s stays not ready.", st.ref(), reporters)
	if st.Crash != nil {
		line := fmt.Sprintf("Module operator %s is crash-looping (%d restarts)", st.Crash.Deployment, st.Crash.Restarts)
		if st.Crash.Confirmed {
			line += ": its last exit says it cannot sync its informer for " + st.Kind + ": " + truncateText(st.Crash.Message, 300)
			description += " Its module operator crash-loops because it cannot read the object either."
		} else {
			line += fmt.Sprintf("; its last exit message is not available here. If `oc logs --previous` shows \"failed waiting for *<version>.%s Informer to sync\", this object is the cause", st.Kind)
		}
		evidence = append(evidence, line)
		p.Explains = st.Crash.Pods
	}
	p.Description = description
	p.Evidence = evidence
	changes := st.changes()
	switch {
	case st.Blocker != "":
		p.Title = fmt.Sprintf("%s has field values from an earlier release", st.ref())
		p.Fix = fmt.Sprintf("No automatic fix: %s. Correct the listed fields of %s yourself to the type the installed CRD expects, keeping their meaning.", st.Blocker, st.ref())
	case len(changes) == 0:
		// Every field is valid now, but the report stands: the operator
		// has not applied the object again since.
		p.Title = fmt.Sprintf("%s still reports an error about %s that is already fixed", reporters, st.ref())
		p.Severity = "warning"
		p.Description = fmt.Sprintf("Every field the error names now matches the installed CRD, but %s still shows the error from its last attempt. The operator retries when an object it watches changes, usually within seconds.", reporters)
		p.FieldChanges = st.fieldChanges()
		p.Fix = "Re-scan in a minute. If the error stays, retry the reconcile."
		for _, r := range rf.reporters {
			if r.Kind == "DataScienceCluster" {
				p.Fix = fmt.Sprintf("Re-scan in a minute. If the error stays, Retry now: it adds the annotation %s to DataScienceCluster %s and removes it again, which makes the operator reconcile it at once. It changes nothing else.", leftoverRetryAnno, r.Name)
				p.AutoFixable = true
				p.AutoFixAction = fixLeftoverRetry + r.Name
				p.AffectedObjects = []string{"DataScienceCluster " + r.Name}
				p.ConfirmMessage = fmt.Sprintf("This adds the annotation %s to DataScienceCluster %s and removes it again. The operator reconciles the DataScienceCluster when its annotations change; nothing else changes.", leftoverRetryAnno, r.Name)
				p.TechnicalCmd = shellCommand("oc", "annotate", "datasciencecluster", r.Name, leftoverRetryAnno+"=now") + "; " + shellCommand("oc", "annotate", "datasciencecluster", r.Name, leftoverRetryAnno+"-")
				break
			}
		}
	default:
		p.Title = fmt.Sprintf("%s has field values from an earlier release", st.ref())
		var plan []string
		for _, lf := range changes {
			if lf.Change == "set-default" {
				plan = append(plan, fmt.Sprintf("%s: %s -> %s", lf.Path, fieldJSON(lf.Value), fieldJSON(lf.After)))
			} else {
				plan = append(plan, fmt.Sprintf("%s: %s -> removed", lf.Path, fieldJSON(lf.Value)))
			}
		}
		p.Fix = fmt.Sprintf("Set each listed field to the installed CRD's default, or remove it where the CRD has none and it is optional (the table shows each change). The tool patches only these fields of %s, with your token, after a server-side dry run, and only if each still holds the value shown. "+
			"The DataScienceCluster controller watches module CRs, so it applies the object again by itself.", st.ref())
		p.AutoFixable = true
		p.AutoFixAction = fixLeftoverFields + st.Kind + "/" + st.Name
		p.ConfirmMessage = fmt.Sprintf("This patches %s:\n- %s\n\nThe tool cannot know what a stored value of the older type meant, so a new value may not mean the same. The object is changed only if it is still the one you reviewed and each field still holds the value shown (JSON Patch tests), after the API server's dry run accepts the change. Nothing else changes.", st.ref(), strings.Join(plan, "\n- "))
		if st.needsReview() {
			var review []string
			for _, lf := range changes {
				if lf.Review {
					after := "removed"
					if lf.Change == "set-default" {
						after = fieldJSON(lf.After)
					}
					review = append(review, fmt.Sprintf("%s (%s -> %s)", lf.Path, fieldJSON(lf.Value), after))
				}
			}
			p.Acknowledge = fmt.Sprintf("I checked what these fields mean, and the new values (which may not mean the same as the old ones) are right for this cluster: %s.", strings.Join(review, ", "))
			p.AcknowledgeBinding = newAckBinding(st.UID, st.ResourceVersion, p.Acknowledge)
		}
		raw, _ := json.Marshal(st.patchOps())
		p.TechnicalCmd = shellCommand("oc", "patch", strings.ToLower(st.Kind)+"."+st.Group, st.Name, "--type=json", "-p", string(raw))
	}
	return p
}

// --- Changed selectors -------------------------------------------------------

// workloadObject is the part of a Deployment, StatefulSet, DaemonSet or Job
// the recreate fix reads.
type workloadObject struct {
	Metadata struct {
		Name              string `json:"name"`
		Namespace         string `json:"namespace"`
		UID               string `json:"uid"`
		ResourceVersion   string `json:"resourceVersion"`
		DeletionTimestamp string `json:"deletionTimestamp"`
		OwnerReferences   []struct {
			APIVersion string `json:"apiVersion"`
			Kind       string `json:"kind"`
			Name       string `json:"name"`
			UID        string `json:"uid"`
			Controller *bool  `json:"controller"`
		} `json:"ownerReferences"`
	} `json:"metadata"`
	Spec struct {
		Replicas *int `json:"replicas"`
		Template struct {
			Metadata struct {
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
		} `json:"template"`
	} `json:"spec"`
	Status struct {
		ReadyReplicas          int `json:"readyReplicas"`
		NumberReady            int `json:"numberReady"`
		DesiredNumberScheduled int `json:"desiredNumberScheduled"`
		Active                 int `json:"active"`
		Succeeded              int `json:"succeeded"`
	} `json:"status"`
}

// ready: the workload's pods are ready (a Job has started or finished).
func (w workloadObject) ready(kind string) bool {
	want := 1
	if w.Spec.Replicas != nil {
		want = *w.Spec.Replicas
	}
	switch kind {
	case "DaemonSet":
		return w.Status.NumberReady >= w.Status.DesiredNumberScheduled && w.Status.DesiredNumberScheduled > 0
	case "Job":
		return w.Status.Active > 0 || w.Status.Succeeded > 0
	}
	return w.Status.ReadyReplicas >= want
}

// workloadReader reads a workload; found=false when it does not exist.
// read=false when this reader cannot read the kind.
type workloadReader func(c *Client, kind, namespace, name string) (obj workloadObject, found, read bool, err error)

// readDeploymentForScan reads Deployments only: the ServiceAccount reads no
// other workload kind, so the scan leaves those to the fix, which reads
// them with the user's token.
func readDeploymentForScan(c *Client, kind, namespace, name string) (workloadObject, bool, bool, error) {
	if kind != "Deployment" {
		return workloadObject{}, false, false, nil
	}
	body, _, err := c.Get(namespacedPath("apps/v1", "deployments", namespace, name))
	return decodeWorkload(body, err)
}

func decodeWorkload(body []byte, err error) (workloadObject, bool, bool, error) {
	var w workloadObject
	if isK8sNotFound(err) {
		return w, false, true, nil
	}
	if err != nil {
		return w, false, true, err
	}
	if err := json.Unmarshal(body, &w); err != nil {
		return w, false, true, fmt.Errorf("parse: %w", err)
	}
	return w, true, true, nil
}

// recreateLeftover is a workload whose selector changed.
type recreateLeftover struct {
	rf                 *reportedApplyFailure
	AppNS              string
	Read, Found        bool
	Obj                workloadObject
	Owner              string // "<Kind> <name>" of the controller owner
	Module             string
	Recreator          string
	Blockers, Warnings []string
	AckReasons         []string
	notReady           bool
}

func (r *recreateLeftover) ref() string { return r.rf.object() }

// analyzeRecreate checks a changed-selector workload: it must exist, be
// in the RHOAI applications namespace, have as its controller owner a CR
// that reports the failure (so that CR's operator applies it again), and
// that operator must be running. What goes down meanwhile, and what the
// tool cannot check, needs an acknowledgement.
func analyzeRecreate(c *Client, rf *reportedApplyFailure, appNS string, read workloadReader, users usersReader) *recreateLeftover {
	f := rf.applyFailure
	r := &recreateLeftover{rf: rf, AppNS: appNS}
	for _, rep := range rf.reporters {
		r.notReady = r.notReady || rep.NotReady
	}
	switch {
	case appNS == "":
		r.Blockers = append(r.Blockers, "the RHOAI applications namespace could not be read from the DSCInitialization")
	case f.Namespace != appNS:
		r.Blockers = append(r.Blockers, fmt.Sprintf("it is not in the RHOAI applications namespace %s; the tool recreates workloads only there", appNS))
	}
	obj, found, readOK, err := read(c, f.Kind, f.Namespace, f.Name)
	switch {
	case err != nil:
		r.Blockers = append(r.Blockers, fmt.Sprintf("could not read %s: %v", r.ref(), err))
		return r
	case !readOK:
		return r
	}
	r.Read, r.Found, r.Obj = true, found, obj
	if !found {
		return r
	}
	if obj.Metadata.DeletionTimestamp != "" {
		r.Blockers = append(r.Blockers, "it is already being deleted")
		return r
	}

	// The controller owner must be a CR that reports the failure: that
	// CR's operator applies the object, so it creates it again.
	var owner *applyReporter
	ownerText := "no controller owner"
	for _, o := range obj.Metadata.OwnerReferences {
		if o.Controller == nil || !*o.Controller {
			continue
		}
		ownerText = fmt.Sprintf("controller owner %s %s (%s)", o.Kind, o.Name, o.APIVersion)
		group, _, _ := strings.Cut(o.APIVersion, "/")
		if !isModuleAPIGroup(group) && (group != dscGroup || o.Kind != "DataScienceCluster") {
			continue
		}
		for i := range rf.reporters {
			rep := rf.reporters[i]
			if rep.Kind == o.Kind && rep.Name == o.Name && rep.UID != "" && rep.UID == o.UID {
				owner = &rf.reporters[i]
			}
		}
	}
	if owner == nil {
		r.Blockers = append(r.Blockers, fmt.Sprintf("it has %s, not a module CR or the DataScienceCluster that reports the failure; the tool deletes only objects whose owner's operator creates them again", ownerText))
		return r
	}
	r.Owner = owner.String()
	r.Module = owner.Module

	// The operator that recreates it must be running.
	if r.Module == "" {
		if why := operatorDeploymentsBlocker(c); why != "" {
			r.Blockers = append(r.Blockers, why)
		} else {
			r.Recreator = "the RHOAI operator"
		}
	} else {
		d, why := findModuleOperator(c, newModuleOperatorFinder(c, nil), r.Module)
		switch {
		case d == nil:
			r.Blockers = append(r.Blockers, fmt.Sprintf("the tool cannot tell which Deployment runs the %s module operator that would create it again (%s)", r.Module, why))
		case d.Namespace == f.Namespace && d.Name == f.Name && f.Kind == "Deployment":
			r.Blockers = append(r.Blockers, "it is the module operator that would create it again")
		case d.Available < 1:
			r.Blockers = append(r.Blockers, fmt.Sprintf("the %s module operator %s, which creates it again, has no available pod; fix that first", r.Module, d.ref()))
		default:
			r.Recreator = fmt.Sprintf("the %s module operator %s", r.Module, d.ref())
		}
	}

	// What stops while it is gone.
	r.Warnings = append(r.Warnings, fmt.Sprintf("%s and its pods stop now; %s creates it again with the current selector on its next reconcile, usually within seconds; the old ReplicaSets and pods are removed in the background.", r.ref(), nonEmpty(r.Recreator, "its operator")))
	switch {
	case r.Module != "" && usersCheckedComponents[r.Module] && users == nil:
		r.AckReasons = append(r.AckReasons, fmt.Sprintf("this scan does not check what uses %s", r.Module))
	case r.Module != "" && usersCheckedComponents[r.Module]:
		inUse, err := users(c, r.Module)
		switch {
		case err != nil:
			r.AckReasons = append(r.AckReasons, fmt.Sprintf("the tool could not check what uses %s (%v)", r.Module, err))
		case len(inUse) > 0:
			r.Warnings = append(r.Warnings, fmt.Sprintf("%s is in use: %s. Until the new pods are ready, changes to these are not acted on.", r.Module, strings.Join(inUse, ", ")))
			r.AckReasons = append(r.AckReasons, fmt.Sprintf("%s is in use (%s)", r.Module, strings.Join(inUse, ", ")))
		}
	default:
		r.AckReasons = append(r.AckReasons, fmt.Sprintf("the tool cannot tell whether %s is in use: it has no reader for that component's resources", nonEmpty(r.Module, r.Owner)))
	}
	if labels := obj.Spec.Template.Metadata.Labels; len(labels) > 0 {
		hooks, err := webhooksServedBy(c, operatorDeployment{Namespace: f.Namespace, Name: f.Name, TemplateLabels: labels})
		switch {
		case err != nil:
			r.AckReasons = append(r.AckReasons, fmt.Sprintf("the tool could not check which webhooks its pods serve (%v)", err))
		case len(hooks) > 0:
			r.Warnings = append(r.Warnings, fmt.Sprintf("Its pods serve the failurePolicy Fail webhooks %s: requests they check are rejected until the new pods are ready.", strings.Join(hooks, ", ")))
			r.AckReasons = append(r.AckReasons, "its pods serve failurePolicy Fail webhooks")
		}
	}
	return r
}

func (r *recreateLeftover) problem() Problem {
	rf, f := r.rf, r.rf.applyFailure
	severity := "warning"
	if r.notReady {
		severity = "critical"
	}
	reporters := reportersText(rf)
	evidence := []string{"Reported by " + reporters}
	for _, d := range rf.details {
		evidence = append(evidence, "Operator error: "+truncateText(d, 500))
	}
	p := Problem{
		ID:              leftoverRecreateID(f.Kind, f.Namespace, f.Name),
		Severity:        severity,
		Title:           fmt.Sprintf("%s must be recreated: its selector changed", r.ref()),
		AffectedObjects: []string{r.ref()},
		Description: fmt.Sprintf("An earlier release created %s with another spec.selector. Kubernetes does not allow changing a selector, so the operator cannot apply the current version and %s stays not ready. "+
			"Deleting it lets the operator create it again with the current spec.", r.ref(), reporters),
		TechnicalCmd: f.ocCommand("delete"),
	}
	switch {
	case !r.Read && len(r.Blockers) == 0:
		evidence = append(evidence, fmt.Sprintf("Not read by this scan: the tool's ServiceAccount does not read %ss. Fix reads it with your token first and refuses unless the checks below pass.", f.Kind))
	case r.Read && !r.Found:
		evidence = append(evidence, fmt.Sprintf("%s does not exist now; the operator creates it on its next reconcile", r.ref()))
		p.Title = fmt.Sprintf("%s was deleted and is waiting for the operator", r.ref())
		p.Fix = fmt.Sprintf("Re-scan in a minute. If %s stays missing, restart the operator that reconciles %s.", r.ref(), reporters)
		p.Evidence = evidence
		return p
	case r.Owner != "":
		evidence = append(evidence, fmt.Sprintf("%s is owned by %s; %s", r.ref(), r.Owner, nonEmpty(r.Recreator, "its operator")+" creates it again"))
	}
	for _, b := range r.Blockers {
		evidence = append(evidence, "Not deleted automatically: "+b)
	}
	evidence = append(evidence, r.Warnings...)
	p.Evidence = evidence
	if len(r.Blockers) > 0 {
		p.Fix = fmt.Sprintf("No automatic fix: %s. Check it yourself, then delete %s with the command below; the operator that owns it creates it again.", strings.Join(r.Blockers, "; "), r.ref())
		return p
	}
	p.Fix = fmt.Sprintf("Delete %s so that the operator creates it again with the current selector. The tool deletes it with your token, only if it is unchanged since it was checked, and then waits up to %s for it to come back ready.", r.ref(), formatDuration(leftoverRecreateWait))
	p.AutoFixable = true
	p.AutoFixAction = fixLeftoverRecreate + f.Kind + "/" + f.Namespace + "/" + f.Name
	p.ConfirmMessage = fmt.Sprintf("This deletes %s (propagation Background), after a server-side dry run, and only if its UID and resourceVersion are unchanged. %s", r.ref(), strings.Join(r.Warnings, " "))
	if reasons := r.ackReasons(); len(reasons) > 0 {
		p.Acknowledge = fmt.Sprintf("I accept that %s is down until the operator creates it again: %s.", r.ref(), strings.Join(reasons, "; "))
		// The object as this scan read it; an object it could not read
		// binds empty (the statement says so).
		p.AcknowledgeBinding = newAckBinding(r.Obj.Metadata.UID, r.Obj.Metadata.ResourceVersion, p.Acknowledge)
	}
	return p
}

// ackReasons are the reasons the delete needs an acknowledgement:
// AckReasons, and that the object was not read when it was not.
func (r *recreateLeftover) ackReasons() []string {
	out := slices.Clone(r.AckReasons)
	if !r.Read {
		out = append(out, fmt.Sprintf("this scan could not read the %s (the fix checks its owner and namespace first)", r.rf.Kind))
	}
	return out
}
