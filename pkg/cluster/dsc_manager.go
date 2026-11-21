package cluster

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The Components page's DataScienceCluster manager: a component tree built
// from the installed CRD, compared with the release default (the installed
// CSV's alm-examples), and changes that patch only the fields they are
// about: a JSON Patch (RFC 6902) whose "test" operations make it fail as a
// whole when the DSC changed since the user looked, after a server-side
// dry run, with fieldValidation=Strict and the field manager below.

// dscComponentsManager is the field manager of every change this page
// makes to the DataScienceCluster. It is not the Environment page's
// (rhoai-nightly-updater-dsc-<key>) or MaaS's (rhoai-nightly-updater-maas-dsc)
// manager: a change made here takes the field away from them, so Clean up
// and MaaS Remove never turn back what a user set here (restoreCheck and
// dscMaaSOwned check ownership).
const dscComponentsManager = "rhoai-nightly-updater-components"

// selfManagedPlatform is the DataScienceCluster's status.release.name on
// self-managed RHOAI (rhods-operator pkg/cluster/const.go SelfManagedRhoai,
// "OpenShift AI Self-Managed"). Managed RHOAI ("OpenShift AI Cloud
// Service") creates default-dsc itself (pkg/bootstrap
// leader_election_init.go: only when cfg.Platform == ManagedRhoai).
const selfManagedPlatform = "OpenShift AI Self-Managed"

// dscView is one read of everything the DSC manager needs.
type dscView struct {
	state      string
	count      int
	obj        map[string]any
	meta       kube.ObjectMeta
	name, uid  string
	version    string // the version the DSC was read at, "v3"
	apiVersion string // "datasciencecluster.opendatahub.io/v3"
	crd        *dscCRD
	crdErr     error
	spec       map[string]any // the spec schema at version
	tree       []dscToggle
	byKey      map[string]dscToggle
	protected  map[string]bool
	// defaults is the release default's spec; nil with defaultsErr set
	// when there is none to use.
	defaults      map[string]any
	defaultsLabel map[string]any
	defaultsErr   string
	op            *installedOperator
	// argoApp is the Argo CD application managing the DSC (argo.managedBy).
	argoApp string
	argo    argoTracking
	acm     bool
	// helm names the Helm release that manages the DSC ("" when none).
	helm string
}

// helmManagerOf names the Helm release that manages an object: Helm 3 sets
// the annotations meta.helm.sh/release-name and release-namespace on every
// object it installs, and charts set the label
// app.kubernetes.io/managed-by: Helm (RHOAI-Build-Config ships such a chart,
// rhai-on-openshift-chart, with a DataScienceCluster). "" when neither is
// set.
func helmManagerOf(meta kube.ObjectMeta) string {
	if name := meta.Annotations["meta.helm.sh/release-name"]; name != "" {
		if ns := meta.Annotations["meta.helm.sh/release-namespace"]; ns != "" {
			return ns + "/" + name
		}
		return name
	}
	if strings.EqualFold(meta.Labels["app.kubernetes.io/managed-by"], "Helm") {
		return "(release not named)"
	}
	return ""
}

// readDSCView reads the DataScienceCluster (the first, as the Components
// page shows it), the CRD and the release default.
func readDSCView(c *Client) (*dscView, error) {
	r, err := listServed(c, dscGroup, dscListFmt, nil, dscFallbackVersions)
	if err != nil {
		return nil, fmt.Errorf("fetching DSC list: %w", err)
	}
	v := &dscView{state: DSCStateNoCRD, byKey: map[string]dscToggle{}}
	if r.Version == "" {
		return v, nil
	}
	var list struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(r.Body, &list); err != nil {
		return nil, fmt.Errorf("parsing DSC list: %w", err)
	}
	v.version, v.count, v.state = r.Version, len(list.Items), DSCStateNoDSC
	if v.count == 0 {
		return v, nil
	}
	v.state, v.obj = DSCStatePresent, list.Items[0]
	if raw, err := json.Marshal(v.obj["metadata"]); err == nil {
		_ = json.Unmarshal(raw, &v.meta)
	}
	v.name, v.uid = v.meta.Name, v.meta.UID
	v.apiVersion, _ = v.obj["apiVersion"].(string)
	if v.apiVersion == "" {
		v.apiVersion = dscGroup + "/" + v.version
	}
	v.argo = argoTrackingOf(c, v.obj, dscGroup, "DataScienceCluster")
	v.argoApp = v.argo.managedBy()
	v.acm = v.argoApp == "" && acmEnforcesSpec(v.meta)
	v.helm = helmManagerOf(v.meta)
	v.crd, v.crdErr = readDSCCRD(c)
	if v.crdErr == nil {
		if ver := v.crd.version(v.version); ver != nil && ver.Spec != nil {
			v.spec = ver.Spec
		} else {
			v.crdErr = fmt.Errorf("the CRD has no schema for served version %s", v.version)
		}
	}
	v.tree = dscToggleTree(v.spec)
	for _, t := range v.tree {
		v.byKey[t.key()] = t
	}
	v.protected = dscProtectedFields(v.spec)
	v.readDefaults(c)
	return v, nil
}

// readDefaults loads the release default: the DataScienceCluster example of
// the installed CSV's alm-examples in the version the DSC is read at. The
// GitHub sample is not used: only what the installed bundle ships is the
// release default.
func (v *dscView) readDefaults(c *Client) {
	op, err := getInstalledOperator(c)
	if err != nil {
		v.defaultsErr = "Cannot read the installed operator: " + err.Error()
		return
	}
	v.op = op
	ex, ok := dscExamplesFromALM(op.ALMExamples)[v.version]
	if !ok {
		v.defaultsErr = fmt.Sprintf("The installed operator %s ships no DataScienceCluster %s example in its CSV's alm-examples, so there is no release default to compare with.", kube.NonEmpty(op.Version, op.Name), v.version)
		return
	}
	spec, _ := kube.NormalizeJSON(ex.Spec["spec"]).(map[string]any)
	if spec == nil {
		v.defaultsErr = "The release default has no spec."
		return
	}
	if v.spec != nil {
		var unknown []string
		pruneUnknownDSCFields(spec, v.spec, "spec", &unknown)
		if len(unknown) > 0 {
			sort.Strings(unknown)
			v.defaultsErr = "The release default sets fields the installed CRD does not define (" + strings.Join(unknown, ", ") + "), so it is not used."
			return
		}
	}
	v.defaults = spec
	meta, _ := ex.Spec["metadata"].(map[string]any)
	v.defaultsLabel, _ = kube.NormalizeJSON(meta["labels"]).(map[string]any)
}

func (v *dscView) components() map[string]any {
	spec, _ := v.obj["spec"].(map[string]any)
	comps, _ := spec["components"].(map[string]any)
	return comps
}

func (v *dscView) defaultComponents() map[string]any {
	comps, _ := v.defaults["components"].(map[string]any)
	return comps
}

func (v *dscView) status() map[string]any {
	s, _ := v.obj["status"].(map[string]any)
	return s
}

// stateOf returns the managementState the operator acts on for the toggle
// at path in components, and whether components sets it. An unset state is
// the CRD's schema default for it where it has one (rhods-operator 3.6
// modules/aipipelines handler.go starts argoWorkflowsControllers Managed,
// the schema's default), else Removed (components.NormalizeManagementState
// of "").
func (v *dscView) stateOf(components map[string]any, path []string) (string, bool) {
	if s, ok := lookupState(components, path); ok && s != "" {
		return s, true
	}
	if t, ok := v.byKey[strings.Join(path, ".")]; ok && t.SchemaDefault != "" {
		return t.SchemaDefault, false
	}
	return "Removed", false
}

// deployed reports whether the operator deploys the toggle at path in
// components: its own state (stateOf) is Managed (Unmanaged for Kueue keeps
// it enabled) and every toggle above it is Managed (a sub-component is
// deployed through its parent's module; rhods-operator 3.6
// modules/aigateway/handler.go).
func (v *dscView) deployed(components map[string]any, path []string) bool {
	s, _ := v.stateOf(components, path)
	if s != "Managed" && s != "Unmanaged" {
		return false
	}
	for i := 1; i < len(path); i++ {
		if t, ok := v.byKey[strings.Join(path[:i], ".")]; ok && t.Toggle {
			if as, _ := v.stateOf(components, path[:i]); as != "Managed" {
				return false
			}
		}
	}
	return true
}

// componentEnabled reports whether a top-level component's module runs: its
// own state, or for a group without one (dashboard, data) any part.
func (v *dscView) componentEnabled(components map[string]any, top dscToggle) bool {
	if top.Toggle {
		return v.deployed(components, top.Path)
	}
	for _, t := range v.descendants(top) {
		if t.Toggle && v.deployed(components, t.Path) {
			return true
		}
	}
	return false
}

// descendants returns the toggles below t.
func (v *dscView) descendants(t dscToggle) []dscToggle {
	prefix := t.key() + "."
	var out []dscToggle
	for _, d := range v.tree {
		if strings.HasPrefix(d.key(), prefix) {
			out = append(out, d)
		}
	}
	return out
}

// conditionFor returns the status condition about a component or
// sub-component: <Name>Ready or <Name>Available (MaaSPortalAvailable),
// matched case-insensitively (AIHubReady for aiHub).
func conditionFor(conds []types.DSCConditionView, name string) *types.DSCConditionView {
	for _, suffix := range []string{"ready", "available"} {
		for i := range conds {
			if strings.EqualFold(conds[i].Type, name+suffix) {
				return &conds[i]
			}
		}
	}
	return nil
}

func (v *dscView) conditions() []types.DSCConditionView {
	raw, _ := v.status()["conditions"].([]any)
	var out []types.DSCConditionView
	for _, c := range raw {
		m, _ := c.(map[string]any)
		cv := types.DSCConditionView{}
		cv.Type, _ = m["type"].(string)
		cv.Status, _ = m["status"].(string)
		cv.Reason, _ = m["reason"].(string)
		cv.Message, _ = m["message"].(string)
		if cv.Type != "" {
			out = append(out, cv)
		}
	}
	return out
}

// readOnlyReason says why the page must not change this DSC now, with an
// error code; "" when it may.
func (v *dscView) readOnlyReason() (string, string) {
	switch {
	case v.state != DSCStatePresent:
		return "There is no DataScienceCluster.", "prerequisites"
	case v.count > 1:
		return fmt.Sprintf("There are %d DataScienceClusters; the operator allows one. Delete the extra ones in the OpenShift console first.", v.count), "prerequisites"
	case v.argoApp != "":
		return gitOpsRefusal("DataScienceCluster "+v.name, v.argoApp, "a change made here") + v.argo.unconfirmedNote(), kube.ErrorCodeGitOpsManaged
	case v.acm:
		return fmt.Sprintf("DataScienceCluster %s is enforced by %s, which puts back what differs from the policy. Change the policy instead. Nothing was changed.", v.name, acmCatalogManager), kube.ErrorCodeGitOpsManaged
	case v.helm != "":
		return fmt.Sprintf("DataScienceCluster %s is managed by Helm (release %s): the next helm upgrade would undo a change made here, and a hard reset would replace the object Helm tracks. Change the chart's values instead. Nothing was changed.", v.name, v.helm), kube.ErrorCodeGitOpsManaged
	case v.meta.DeletionTimestamp != "":
		return fmt.Sprintf("DataScienceCluster %s is being deleted (finalizers: %s). Nothing was changed.", v.name, kube.NonEmpty(strings.Join(v.meta.Finalizers, ", "), "none")), "conflict"
	case v.crdErr != nil:
		return "Cannot read the DataScienceCluster CRD, so changes cannot be checked against it: " + v.crdErr.Error(), "prerequisites"
	case v.crd.storage() != v.version:
		return fmt.Sprintf("The DataScienceCluster is read as %s, but the CRD stores %s: the operator's conversion does not work now, so this page changes nothing. Diagnostics explains the conversion failure.", v.version, kube.NonEmpty(v.crd.storage(), "no version")), "prerequisites"
	}
	return "", ""
}

// GetDSCManager returns the Components page's view of the DataScienceCluster.
func GetDSCManager(c *Client) (*types.DSCManager, error) {
	v, err := readDSCView(c)
	if err != nil {
		return nil, err
	}
	m := v.model()
	// A hard reset that deleted the DSC and did not create its
	// replacement is offered to finish while no DSC exists.
	if v.state == DSCStateNoDSC {
		pending, err := pendingHardReset(c)
		if err != nil {
			m.PendingHardResetError = "Cannot read the recorded hard reset: " + err.Error()
		}
		m.PendingHardReset = pending
	}
	return m, nil
}

func (v *dscView) model() *types.DSCManager {
	out := &types.DSCManager{State: v.state, Exists: v.state == DSCStatePresent, DSCCount: v.count, Nodes: []types.DSCNode{}, Mismatches: []types.DSCMismatch{}, Conditions: []types.DSCConditionView{}}
	if !out.Exists {
		return out
	}
	out.Name, out.UID, out.APIVersion = v.name, v.uid, v.apiVersion
	if v.crd != nil {
		out.StorageVersion = v.crd.storage()
	}
	if v.crdErr != nil {
		out.SchemaError = v.crdErr.Error()
	}
	out.DefaultsError = v.defaultsErr
	if v.defaults != nil {
		out.DefaultsSource = "alm-examples of the installed CSV " + v.op.Name
	}
	if v.op != nil {
		out.OperatorVersion = v.op.Version
	}
	out.ReadOnlyReason, _ = v.readOnlyReason()
	out.ManagedBy, out.ACMEnforced = v.argoApp, v.acm
	out.Deleting, out.Finalizers = v.meta.DeletionTimestamp != "", v.meta.Finalizers
	st := v.status()
	out.Phase, _ = st["phase"].(string)
	release, _ := st["release"].(map[string]any)
	out.ReleaseName, _ = release["name"].(string)
	out.ReleaseVersion, _ = release["version"].(string)
	conds := v.conditions()
	for _, want := range []string{"Ready", "ProvisioningSucceeded", "ComponentsReady", "ModulesReady"} {
		for _, cd := range conds {
			if cd.Type == want {
				out.Conditions = append(out.Conditions, cd)
			}
		}
	}
	for _, cd := range conds {
		if isUpgradeGateCondition(dscCondition{Type: cd.Type, Status: cd.Status, Reason: cd.Reason, Message: cd.Message}) && cd.Status != "True" {
			out.UpgradeGates = append(out.UpgradeGates, fmt.Sprintf("%s (%s): %s", cd.Type, cd.Reason, kube.TruncateText(cd.Message, 300)))
		}
	}
	statusComps, _ := st["components"].(map[string]any)
	comps, defComps := v.components(), v.defaultComponents()
	for _, t := range v.tree {
		name := t.Path[len(t.Path)-1]
		n := types.DSCNode{Path: t.key(), Name: name, Depth: len(t.Path) - 1, Toggle: t.Toggle, Allowed: t.Allowed, SchemaDefault: t.SchemaDefault}
		if len(t.Path) > 1 {
			n.Parent = strings.Join(t.Path[:len(t.Path)-1], ".")
		}
		if sc, ok := statusComps[name].(map[string]any); ok {
			n.Actual, _ = sc["managementState"].(string)
		}
		n.Condition = conditionFor(conds, name)
		if t.Toggle {
			n.Spec, n.SpecSet = lookupState(comps, t.Path)
			if v.defaults != nil {
				n.Default, n.DefaultSet = lookupState(defComps, t.Path)
				if n.SpecSet {
					want := "Removed" // unset is Removed
					if n.DefaultSet {
						want = n.Default
					} else if t.SchemaDefault != "" {
						want = t.SchemaDefault
					}
					n.Changed = n.Spec != want
				}
			}
		}
		out.Nodes = append(out.Nodes, n)
	}
	out.Mismatches = v.mismatches()
	out.Nodes = withUnknownNodes(out.Nodes, out.Mismatches)
	out.Comparison = v.comparison()
	return out
}

// comparison says what the comparison with the release default covered,
// and which components the CRD, the release default and the DSC list.
func (v *dscView) comparison() types.DSCComparison {
	out := types.DSCComparison{
		DefaultsRead:      v.defaults != nil,
		SchemaRead:        v.spec != nil,
		CRDComponents:     dscObjectKeys(schemaProps(schemaChild(v.spec, "components"))),
		DefaultComponents: dscObjectKeys(v.defaultComponents()),
		DSCComponents:     dscObjectKeys(v.components()),
	}
	if v.defaults != nil && v.op != nil {
		out.DefaultsCSV = v.op.Name
	}
	return out
}

// dscObjectKeys returns the keys of m, sorted; an empty list (not nil) for a
// nil map, so the API answers [].
func dscObjectKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// withUnknownNodes adds a row for each field the installed CRD does not
// define (the "unknown-field" mismatches): under the nearest node above it
// (named by its path below that node), after that node's other rows; a
// field no node is above goes last.
func withUnknownNodes(nodes []types.DSCNode, mismatches []types.DSCMismatch) []types.DSCNode {
	out := append([]types.DSCNode{}, nodes...)
	index := map[string]int{}
	for i, n := range out {
		index[n.Path] = i
	}
	for _, m := range mismatches {
		if m.Kind != "unknown-field" {
			continue
		}
		u := types.DSCNode{Path: m.Path, Name: m.Path, Unknown: true}
		parts := strings.Split(m.Path, ".")
		for i := len(parts) - 1; i >= 1; i-- {
			p := strings.Join(parts[:i], ".")
			if pi, ok := index[p]; ok {
				u.Parent, u.Name, u.Depth = p, strings.Join(parts[i:], "."), out[pi].Depth+1
				break
			}
		}
		at := len(out)
		if u.Parent != "" {
			// After the parent's subtree: the first row past it that is
			// not below it.
			at = index[u.Parent] + 1
			for at < len(out) && strings.HasPrefix(out[at].Path, u.Parent+".") {
				at++
			}
		}
		out = append(out[:at], append([]types.DSCNode{u}, out[at:]...)...)
		for i := at; i < len(out); i++ {
			index[out[i].Path] = i
		}
	}
	return out
}

// mismatches returns the differences from the release default that are
// real problems. Values the API server's defaulting fills in and values a
// user chose are not among them.
func (v *dscView) mismatches() []types.DSCMismatch {
	out := []types.DSCMismatch{}
	comps, defComps := v.components(), v.defaultComponents()
	for _, t := range v.tree {
		if !t.Toggle {
			continue
		}
		spec, specSet := lookupState(comps, t.Path)
		if v.defaults != nil && !specSet {
			if def, ok := lookupState(defComps, t.Path); ok {
				m := types.DSCMismatch{Kind: "unset", Path: t.key(), Default: def, Fixable: true}
				eff, _ := v.stateOf(comps, t.Path)
				how := "the operator treats it as Removed"
				if t.SchemaDefault != "" {
					how = fmt.Sprintf("the operator uses the CRD default, %s", t.SchemaDefault)
				}
				if def == eff {
					m.Message = fmt.Sprintf("Not set in the DataScienceCluster, so %s, which is also the release default. Fix sets it explicitly.", how)
				} else {
					m.Message = fmt.Sprintf("Not set in the DataScienceCluster, so %s, but the release default is %s. Fix sets it to %s.", how, def, def)
				}
				out = append(out, m)
			}
		}
		if spec == "Managed" {
			for i := len(t.Path) - 1; i >= 1; i-- {
				anc, ok := v.byKey[strings.Join(t.Path[:i], ".")]
				if !ok || !anc.Toggle {
					continue
				}
				if as, set := v.stateOf(comps, anc.Path); as != "Managed" {
					state := as
					if !set {
						state = "not set (" + as + ")"
					}
					out = append(out, types.DSCMismatch{Kind: "parent-removed", Path: t.key(), Parent: anc.key(),
						Message: fmt.Sprintf("Managed, but %s is %s, so the operator deploys nothing for it. Set %s to Managed, or set this to Removed.", anc.key(), state, anc.key())})
					break
				}
			}
		}
	}
	if v.spec != nil {
		var unknown []string
		pruneUnknownDSCFields(v.obj["spec"], v.spec, "spec", &unknown)
		sort.Strings(unknown)
		for _, u := range unknown {
			out = append(out, types.DSCMismatch{Kind: "unknown-field", Path: strings.TrimPrefix(u, "spec.components."), Fixable: true,
				Message: "The installed CRD does not define this field, so the operator never sees it. Fix removes it."})
		}
	}
	return out
}

// --- plans -------------------------------------------------------------------

// dscPlanned is a plan and the JSON Patch that carries it out.
type dscPlanned struct {
	plan *types.DSCPlan
	ops  []kube.JSONPatchOp
	// sets are checked in the API server's answer: a value it pruned
	// silently would otherwise pass as done.
	sets []dscAssign
}

func (v *dscView) newPlan(action string) *dscPlanned {
	return &dscPlanned{plan: &types.DSCPlan{Action: action, Name: v.name, UID: v.uid, APIVersion: v.apiVersion, Changes: []types.DSCChange{}}}
}

func (p *dscPlanned) refuse(msg, code string) *dscPlanned {
	p.plan.Refusal, p.plan.Code = msg, code
	return p
}

// componentsPath is a path under spec.components from the DSC's root.
func componentsPath(path ...string) []string {
	return append([]string{"spec", "components"}, path...)
}

// finish applies the patch to a copy to list the changes, checks the
// removals it causes and binds the plan to its patch.
func (v *dscView) finish(c *Client, p *dscPlanned, teardown bool) *dscPlanned {
	if len(p.ops) == 0 {
		return p.refuse("The DataScienceCluster already matches. Nothing to change.", "nothing_to_do")
	}
	after, err := kube.ApplyJSONPatch(v.obj, p.ops)
	if err != nil {
		return p.refuse("Cannot work out the change: "+err.Error(), "internal")
	}
	afterSpec, _ := after["spec"].(map[string]any)
	afterComps, _ := afterSpec["components"].(map[string]any)
	p.plan.Changes = v.changesBetween(v.defaultedComponents(v.components()), v.defaultedComponents(afterComps))
	v.removalBlocks(c, v.components(), afterComps, teardown).applyTo(p.plan)
	p.plan.Warnings = append(p.plan.Warnings, v.orphanWarnings(v.components(), afterComps)...)
	p.plan.Token = planToken(p.plan.Action, v.uid, v.apiVersion, p.ops, p.plan.KeepEnabled)
	return p
}

// planToken binds a plan to what it does.
func planToken(parts ...any) string {
	raw, _ := json.Marshal(parts)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:12])
}

// defaultedComponents returns a copy of spec.components with what the API
// server's defaulting fills in from the installed CRD: what the
// DataScienceCluster holds once stored. Changes are listed between such
// copies, so a field the API server defaults is no change when a value
// leaves it out.
func (v *dscView) defaultedComponents(comps map[string]any) map[string]any {
	return withSchemaDefaults(comps, schemaChild(v.spec, "components"))
}

// protectedFields returns the protected fields (dscProtectedFields) the
// DataScienceCluster sets, sorted: a reset keeps their value.
func (v *dscView) protectedFields() []string {
	var out []string
	for f := range v.protected {
		if _, ok := lookupPath(v.components(), strings.Split(f, ".")); ok {
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out
}

// changesBetween lists every managementState that changes, or whose
// deployment changes through a parent, and every other field that changes.
func (v *dscView) changesBetween(before, after map[string]any) []types.DSCChange {
	out := []types.DSCChange{}
	for _, t := range v.tree {
		if !t.Toggle {
			continue
		}
		b, bSet := lookupState(before, t.Path)
		a, aSet := lookupState(after, t.Path)
		db, da := v.deployed(before, t.Path), v.deployed(after, t.Path)
		if b == a && bSet == aSet && db == da {
			continue
		}
		ch := types.DSCChange{Path: t.key(), From: b, FromSet: bSet, To: a, ToSet: aSet, Toggle: true}
		switch {
		case db && !da:
			ch.Effect = "removes"
		case !db && da:
			ch.Effect = "deploys"
		}
		out = append(out, ch)
	}
	diffFields(before, after, nil, &out)
	return out
}

// diffFields lists the fields other than managementState that differ.
func diffFields(before, after map[string]any, path []string, out *[]types.DSCChange) {
	keys := map[string]bool{}
	for k := range before {
		keys[k] = true
	}
	for k := range after {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	for _, k := range sorted {
		if k == "managementState" {
			continue
		}
		p := append(append([]string{}, path...), k)
		b, bOK := before[k]
		a, aOK := after[k]
		bm, bIsMap := b.(map[string]any)
		am, aIsMap := a.(map[string]any)
		switch {
		case bIsMap && aIsMap:
			diffFields(bm, am, p, out)
		case bIsMap && !aOK:
			diffFields(bm, nil, p, out)
		case aIsMap && !bOK:
			diffFields(nil, am, p, out)
		case !kube.JSONEqual(b, a) || bOK != aOK:
			*out = append(*out, types.DSCChange{Path: strings.Join(p, "."), From: fieldText(b), FromSet: bOK, To: fieldText(a), ToSet: aOK})
		}
	}
}

func fieldText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if v == nil {
		return ""
	}
	raw, _ := json.Marshal(v)
	return string(raw)
}

// orphanWarnings names sub-components that stay Managed but deploy nothing
// after the change, because a parent stops being Managed.
func (v *dscView) orphanWarnings(before, after map[string]any) []string {
	var out []string
	for _, t := range v.tree {
		if !t.Toggle || len(t.Path) < 2 {
			continue
		}
		if s, _ := v.stateOf(after, t.Path); s != "Managed" || v.deployed(after, t.Path) || !v.deployed(before, t.Path) {
			continue
		}
		out = append(out, fmt.Sprintf("%s stays Managed but deploys nothing while %s is not Managed.", t.key(), strings.Join(t.Path[:len(t.Path)-1], ".")))
	}
	return out
}

// usersCheckedComponents are the components componentUsers can check.
var usersCheckedComponents = map[string]bool{"aipipelines": true, "datasciencepipelines": true, "mlflowoperator": true, "kserve": true, "ogx": true, "llamastackoperator": true, "dashboard": true}

// modulesOf returns the modules (guardModule) a top-level component runs in
// components, in tree order: its own, and those of its deployed parts.
func (v *dscView) modulesOf(components map[string]any, top dscToggle) []string {
	var out []string
	seen := map[string]bool{}
	add := func(m string) {
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	if top.Toggle && v.deployed(components, top.Path) {
		add(guardModule(top.key()))
	}
	for _, d := range v.descendants(top) {
		if d.Toggle && v.deployed(components, d.Path) {
			add(guardModule(d.key()))
		}
	}
	return out
}

// removalBlocks checks what stops being deployed between before and after.
// A component whose module goes away gets the removal preconditions of
// disable-component and, where the tool can tell, what still uses it; a
// sub-component whose module stays gets partBlockers. Read errors block
// (fail closed). teardown is the hard reset, which deletes every module
// and creates after: a module after deploys again gets the teardown rules
// (the standing refusals of refusedComponents do not apply, and what only
// stops working while the components are down is a warning); a module
// after leaves Removed gets the rules of removing it, and what still uses
// it blocks.
func (v *dscView) removalBlocks(c *Client, before, after map[string]any, teardown bool) removalCheck {
	rc := newRemovalChecker(c)
	var blocks []types.DSCRemovalBlock
	var out removalCheck
	for _, top := range v.tree {
		if len(top.Path) != 1 {
			continue
		}
		wasOn, isOn := v.componentEnabled(before, top), v.componentEnabled(after, top)
		if wasOn && (!isOn || teardown) {
			var reasons []string
			comesBack := map[string]bool{}
			if teardown {
				for _, m := range v.modulesOf(after, top) {
					comesBack[m] = true
				}
			}
			for _, m := range v.modulesOf(before, top) {
				if comesBack[m] {
					b, crds := rc.teardownBlockers(m)
					reasons = append(reasons, b...)
					for _, crd := range crds {
						out.conversionCRDs = append(out.conversionCRDs, types.DSCConversionCRD{CRD: crd, Component: m})
					}
				} else {
					reasons = append(reasons, rc.blockers(m)...)
				}
			}
			name := top.key()
			if usersCheckedComponents[name] {
				users, err := componentUsers(c, name)
				switch {
				case err != nil:
					reasons = append(reasons, fmt.Sprintf("could not check what still uses %s: %v", name, err))
				case len(users) > 0 && (!teardown || !isOn || name == "mlflowoperator"):
					what := "; remove them first"
					if name == "mlflowoperator" {
						// The MLflowOperator CR's finalizer waits for every MLflow CR.
						what = ", so its removal would hang; delete MLflow first (Environment page)"
					}
					reasons = append(reasons, fmt.Sprintf("in use by %s%s", strings.Join(users, ", "), what))
				case len(users) > 0:
					out.addStopsWorking(name, users...)
				}
			}
			if len(reasons) > 0 {
				blocks = append(blocks, types.DSCRemovalBlock{Component: name, Reasons: uniqueInOrder(reasons)})
			}
			continue
		}
		if !wasOn {
			continue
		}
		for _, d := range v.descendants(top) {
			if d.Toggle && v.deployed(before, d.Path) && !v.deployed(after, d.Path) {
				if reasons := rc.partBlockers(guardModule(d.key())); len(reasons) > 0 {
					blocks = append(blocks, types.DSCRemovalBlock{Component: d.key(), Reasons: reasons})
				}
			}
		}
	}
	// What uses MaaS, beyond the module checks (dsc_maas_guard.go).
	if b, down, stops := v.maasBlocks(rc, before, after, teardown); b != nil {
		blocks = addBlock(blocks, *b)
	} else {
		out.addStopsWorking(down, stops...)
	}
	out.blocked = blocks
	return out
}

// removalCheck is what removalBlocks found: the removals that must not
// happen now, and, in a hard reset, what only stops working while the
// components are down.
type removalCheck struct {
	blocked        []types.DSCRemovalBlock
	stopsWorking   []types.DSCStopsWorking
	conversionCRDs []types.DSCConversionCRD
}

// addStopsWorking records resources that stop working while component is
// down: each resource once, with every component it needs.
func (r *removalCheck) addStopsWorking(component string, resources ...string) {
	for _, res := range resources {
		i := slices.IndexFunc(r.stopsWorking, func(s types.DSCStopsWorking) bool { return s.Resource == res })
		switch {
		case i < 0:
			r.stopsWorking = append(r.stopsWorking, types.DSCStopsWorking{Resource: res, Components: []string{component}})
		case !slices.Contains(r.stopsWorking[i].Components, component):
			r.stopsWorking[i].Components = append(r.stopsWorking[i].Components, component)
		}
	}
}

// applyTo puts the result in the plan.
func (r removalCheck) applyTo(p *types.DSCPlan) {
	p.Blocked, p.StopsWorking, p.ConversionCRDs = r.blocked, r.stopsWorking, r.conversionCRDs
}

func uniqueInOrder(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// PlanDSCChange computes, from a fresh read, what a change would do.
// action is "toggle" (path, to, withParent), "fix", "soft-reset" or
// "hard-reset" (keepEnabled: keep the components the user turned on).
func PlanDSCChange(c *Client, action, path, to string, withParent, keepEnabled bool) (*types.DSCPlan, error) {
	v, err := readDSCView(c)
	if err != nil {
		return nil, err
	}
	switch action {
	case "toggle":
		return v.planToggle(c, path, to, withParent).plan, nil
	case "fix":
		return v.planFix(c).plan, nil
	case "soft-reset":
		return v.planSoftReset(c, keepEnabled).plan, nil
	case "hard-reset":
		return v.planHardReset(c, keepEnabled).plan, nil
	}
	return nil, fmt.Errorf("unknown action %q", action)
}

func (v *dscView) planToggle(c *Client, path, to string, withParent bool) *dscPlanned {
	p := v.newPlan("toggle")
	if msg, code := v.readOnlyReason(); msg != "" {
		return p.refuse(msg, code)
	}
	t, ok := v.byKey[path]
	if !ok || !t.Toggle {
		return p.refuse(fmt.Sprintf("The installed DataScienceCluster CRD defines no managementState at spec.components.%s.", path), "validation")
	}
	if len(t.Allowed) == 0 {
		return p.refuse(fmt.Sprintf("The CRD lists no values for spec.components.%s.managementState, so this page does not change it.", path), "validation")
	}
	if !slices.Contains(t.Allowed, to) {
		return p.refuse(fmt.Sprintf("spec.components.%s.managementState accepts %s, not %q.", path, strings.Join(t.Allowed, ", "), to), "validation")
	}
	comps := v.components()
	if cur, set := lookupState(comps, t.Path); set && cur == to {
		return p.refuse(fmt.Sprintf("%s is already %s. Nothing to change.", path, to), "nothing_to_do")
	}
	p.sets = []dscAssign{{Path: componentsPath(append(append([]string{}, t.Path...), "managementState")...), Value: to}}
	if to == "Managed" {
		var parents []dscToggle
		for i := 1; i < len(t.Path); i++ {
			anc, ok := v.byKey[strings.Join(t.Path[:i], ".")]
			if ok && anc.Toggle {
				if as, _ := v.stateOf(comps, anc.Path); as != "Managed" {
					parents = append(parents, anc)
				}
			}
		}
		if len(parents) > 0 {
			parent := parents[0]
			p.plan.ParentPath = parent.key()
			state, set := lookupState(comps, parent.Path)
			if !set {
				state = "not set"
			}
			if !withParent {
				return p.refuse(fmt.Sprintf("%s is %s, so setting %s to Managed would deploy nothing. Set %s to Managed in the same change, or set it first.", parent.key(), state, path, parent.key()), "parent_removed")
			}
			for _, anc := range parents {
				if !slices.Contains(anc.Allowed, "Managed") {
					return p.refuse(fmt.Sprintf("%s does not accept Managed, so %s cannot be deployed.", anc.key(), path), "validation")
				}
				p.sets = append(p.sets, dscAssign{Path: componentsPath(append(append([]string{}, anc.Path...), "managementState")...), Value: "Managed"})
			}
		}
	}
	ops, err := buildJSONPatch(v.obj, p.sets, nil)
	if err != nil {
		return p.refuse("Cannot build the change: "+err.Error(), "internal")
	}
	p.ops = ops
	return v.finish(c, p, false)
}

func (v *dscView) planFix(c *Client) *dscPlanned {
	p := v.newPlan("fix")
	if msg, code := v.readOnlyReason(); msg != "" {
		return p.refuse(msg, code)
	}
	var removes [][]string
	for _, m := range v.mismatches() {
		switch m.Kind {
		case "unset":
			p.sets = append(p.sets, dscAssign{Path: componentsPath(append(strings.Split(m.Path, "."), "managementState")...), Value: m.Default})
		case "unknown-field":
			removes = append(removes, fieldPathKeys("spec.components."+m.Path))
		}
	}
	if len(p.sets) == 0 && len(removes) == 0 {
		return p.refuse("Nothing to fix: no component is missing and no unknown field is in the DataScienceCluster.", "nothing_to_do")
	}
	ops, err := buildJSONPatch(v.obj, p.sets, removes)
	if err != nil {
		return p.refuse("Cannot build the change: "+err.Error(), "internal")
	}
	p.ops = ops
	return v.finish(c, p, false)
}

// fieldPathKeys turns "spec.components.a[0].b" into its keys.
func fieldPathKeys(path string) []string {
	var out []string
	for part := range strings.SplitSeq(path, ".") {
		for {
			i := strings.Index(part, "[")
			if i < 0 {
				out = append(out, part)
				break
			}
			if i > 0 {
				out = append(out, part[:i])
			}
			j := strings.Index(part, "]")
			if j < i {
				out = append(out, part[i:])
				break
			}
			out = append(out, part[i+1:j])
			part = part[j+1:]
			if part == "" {
				break
			}
		}
	}
	return out
}

// planSoftReset makes spec.components the release default in place: only
// what differs, keeping protected fields (dscProtectedFields) and values
// the API server's defaulting would put back anyway, and with keep the
// components the user turned on (resetTarget).
func (v *dscView) planSoftReset(c *Client, keep bool) *dscPlanned {
	p := v.newPlan("soft-reset")
	p.plan.KeepEnabled = keep
	if msg, code := v.readOnlyReason(); msg != "" {
		return p.refuse(msg, code)
	}
	if v.defaults == nil {
		return p.refuse(v.defaultsErr, "prerequisites")
	}
	target := v.resetTarget(p.plan, keep)
	p.plan.ProtectedFields = v.protectedFields()
	var removes [][]string
	compSchema := schemaChild(v.spec, "components")
	v.resetDiff(compSchema, v.components(), target, nil, &p.sets, &removes)
	ops, err := buildJSONPatch(v.obj, p.sets, removes)
	if err != nil {
		return p.refuse("Cannot build the change: "+err.Error(), "internal")
	}
	if len(ops) == 0 && len(p.plan.Kept) > 0 {
		return p.refuse(fmt.Sprintf("The DataScienceCluster already matches the release default, apart from the components you turned on, which the reset keeps (%s). Nothing to change.", strings.Join(p.plan.Kept, ", ")), "nothing_to_do")
	}
	p.ops = ops
	return v.finish(c, p, false)
}

// turnedOn returns the toggles the user turned on: set to Managed (or
// Unmanaged, where the CRD allows it) in the DataScienceCluster and
// deployed, while the release default leaves them off (Removed, or unset
// without a CRD default that enables them).
func (v *dscView) turnedOn() []dscToggle {
	if v.defaults == nil {
		return nil
	}
	live, def := v.components(), v.defaultComponents()
	var out []dscToggle
	for _, t := range v.tree {
		if !t.Toggle {
			continue
		}
		s, set := lookupState(live, t.Path)
		if !set || (s != "Managed" && s != "Unmanaged") || !slices.Contains(t.Allowed, s) || !v.deployed(live, t.Path) {
			continue
		}
		if ds, _ := v.stateOf(def, t.Path); ds == "Managed" || ds == "Unmanaged" {
			continue
		}
		out = append(out, t)
	}
	return out
}

// resetTarget is spec.components a reset makes: the release default's,
// and with keep every toggle the user turned on at its live value, with the
// toggles above it Managed (it is deployed only through them). It fills
// in the plan's TurnedOn (always, so the dialog can offer the option) and
// Kept (with keep).
func (v *dscView) resetTarget(plan *types.DSCPlan, keep bool) map[string]any {
	target, _ := kube.NormalizeJSON(v.defaultComponents()).(map[string]any)
	if target == nil {
		target = map[string]any{}
	}
	live, def := v.components(), v.defaultComponents()
	assign := map[string]string{}
	add := func(t dscToggle, state string) {
		if _, done := assign[t.key()]; !done {
			assign[t.key()] = state
		}
	}
	for _, t := range v.turnedOn() {
		for i := 1; i < len(t.Path); i++ {
			anc, ok := v.byKey[strings.Join(t.Path[:i], ".")]
			if !ok || !anc.Toggle {
				continue
			}
			if ds, _ := v.stateOf(def, anc.Path); ds != "Managed" {
				add(anc, "Managed")
			}
		}
		s, _ := lookupState(live, t.Path)
		add(t, s)
	}
	// Tree order: parents before their children.
	var ordered []string
	for _, t := range v.tree {
		if _, ok := assign[t.key()]; ok {
			ordered = append(ordered, t.key())
		}
	}
	plan.TurnedOn = ordered
	if !keep {
		return target
	}
	plan.Kept = ordered
	for _, key := range ordered {
		setStateIn(target, v.byKey[key].Path, assign[key])
	}
	return target
}

// setStateIn sets the managementState at path in components, making the
// objects that hold it.
func setStateIn(components map[string]any, path []string, state string) {
	m := components
	for _, k := range path {
		next, ok := m[k].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[k] = next
		}
		m = next
	}
	m["managementState"] = state
}

// resetDiff collects the sets and removals that turn live into target.
func (v *dscView) resetDiff(schema, live, target map[string]any, path []string, sets *[]dscAssign, removes *[][]string) {
	keys := map[string]bool{}
	for k := range live {
		keys[k] = true
	}
	for k := range target {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	for _, k := range sorted {
		p := append(append([]string{}, path...), k)
		child := schemaChild(schema, k)
		lv, lok := live[k]
		tv, tok := target[k]
		if lok && v.protected[strings.Join(p, ".")] {
			continue // keep its value
		}
		switch {
		case lok && tok:
			lm, lIsMap := lv.(map[string]any)
			tm, tIsMap := tv.(map[string]any)
			if lIsMap && tIsMap {
				v.resetDiff(child, lm, tm, p, sets, removes)
			} else if !kube.JSONEqual(lv, tv) {
				*sets = append(*sets, dscAssign{Path: componentsPath(p...), Value: tv})
			}
		case tok:
			*sets = append(*sets, dscAssign{Path: componentsPath(p...), Value: tv})
		case lok:
			if child != nil && defaultedBack(child, lv) {
				continue // the API server puts it back
			}
			if lm, isMap := lv.(map[string]any); isMap && child != nil && len(lm) > 0 && v.keepsProtectedBelow(p, lm) {
				v.resetDiff(child, lm, map[string]any{}, p, sets, removes)
				continue
			}
			*removes = append(*removes, componentsPath(p...))
		}
	}
}

// keepsProtectedBelow reports whether a live object holds a protected field.
func (v *dscView) keepsProtectedBelow(path []string, obj map[string]any) bool {
	prefix := strings.Join(path, ".") + "."
	for f := range v.protected {
		if after, ok := strings.CutPrefix(f, prefix); ok {
			if _, ok := lookupPath(obj, strings.Split(after, ".")); ok {
				return true
			}
		}
	}
	return false
}

// --- writes ------------------------------------------------------------------

// A failed JSON Patch "test" cannot be told from the API server's answer:
// k8s.io/apiserver pkg/endpoints/handlers/patch.go applyJSPatch returns
// errors.NewGenericServerResponse(422, "", schema.GroupResource{}, "",
// err.Error(), 0, false), and with isUnexpectedResponse false the
// json-patch message is dropped. The whole answer (live on OpenShift's
// kube-apiserver, 2026-11-20) is a Status of reason Invalid with message
// "the server rejected our request due to an error in our request", no
// cause and no path, the same as other 422 refusals give in part. A patch
// whose tests bind it to an object's uid and resourceVersion therefore
// reads the object again after a 422: when either moved, a test failed
// (the object is not what was checked; nothing was changed); when neither
// did, no test can have failed (they test values of that very version),
// so the 422 is the API server's own refusal (a validation rule, a
// webhook).

// dscWriteError turns the API server's refusal of a patch into a result,
// verbatim (validation rules, webhooks).
func dscWriteError(err error, dryRun bool) *types.OperationResponse {
	var k *K8sError
	what := "The API server refused the change"
	if dryRun {
		what = "The API server refused the change in its dry run"
	}
	msg := err.Error()
	if errors.As(err, &k) && k.K8sMessage != "" {
		msg = k.K8sMessage
	}
	return &types.OperationResponse{Success: false, ErrorCode: kube.NonEmpty(kube.ErrorCodeFromK8sErr(err), "validation"), Logs: []string{msg},
		Message: what + ": " + msg + ". Nothing was changed."}
}

// dscWriteAttempts bounds the patches of one change. The operator's
// status writes move the DataScienceCluster's resourceVersion too, so a
// patch bound to the version that was checked may meet one; the DSC is
// then read again and every check runs again before the next patch.
const dscWriteAttempts = 3

// dscObjectPath is the API path of the DSC name at version ("v3"). A
// package function, so the RBAC coverage test resolves it.
func dscObjectPath(version, name string) string {
	return fmt.Sprintf(dscListFmt, version) + "/" + name
}

// patchDSCFields sends a JSON Patch to the DataScienceCluster with the
// components field manager and fieldValidation=Strict, as a server-side dry
// run when dryRun is set.
func patchDSCFields(c *Client, path string, ops []kube.JSONPatchOp, dryRun bool) ([]byte, error) {
	body, err := json.Marshal(ops)
	if err != nil {
		return nil, err
	}
	q := url.Values{"fieldManager": {dscComponentsManager}, "fieldValidation": {"Strict"}}
	if dryRun {
		q.Set("dryRun", "All")
	}
	resp, _, err := c.Do(http.MethodPatch, path, "application/json-patch+json", body, q)
	return resp, err
}

// write runs the plan's patch on the DataScienceCluster the plan was
// checked on: the patch tests its uid and resourceVersion first (the
// version of the read every check was made on), then the plan's own tests
// and changes. A dry run, then the patch, then a check that the API server
// kept every value set. rejected: the API server answered 422, which a
// failed test also gets (see above), so writeChecked reads the DSC again
// to tell; nothing was changed either way (a JSON Patch applies whole or
// not at all). A read without a uid or resourceVersion (an API server
// always sets both) is refused before anything is sent: the patch could
// not be bound to it.
func (v *dscView) write(c *Client, p *dscPlanned) (res *types.OperationResponse, rejected bool) {
	if v.uid == "" || v.meta.ResourceVersion == "" {
		return &types.OperationResponse{Success: false, ErrorCode: "internal",
			Message: fmt.Sprintf("DataScienceCluster %s was read without a uid or resourceVersion, so the change cannot be limited to the object that was checked. Nothing was changed.", v.name)}, false
	}
	ops := append([]kube.JSONPatchOp{
		patchOp("test", []string{"metadata", "uid"}, v.uid),
		patchOp("test", []string{"metadata", "resourceVersion"}, v.meta.ResourceVersion),
	}, p.ops...)
	path := dscObjectPath(v.version, v.name)
	if _, err := patchDSCFields(c, path, ops, true); err != nil {
		return dscWriteError(err, true), kube.IsK8sError(err, http.StatusUnprocessableEntity)
	}
	body, err := patchDSCFields(c, path, ops, false)
	if err != nil {
		return dscWriteError(err, false), kube.IsK8sError(err, http.StatusUnprocessableEntity)
	}
	var after map[string]any
	if err := json.Unmarshal(body, &after); err != nil {
		return &types.OperationResponse{Success: false, ErrorCode: "internal", Message: "The change was sent, but the API server's answer could not be read: " + err.Error() + ". Reload the page to see the DataScienceCluster."}, false
	}
	for _, s := range p.sets {
		if got, ok := lookupPath(after, s.Path); !ok || !kube.JSONEqual(got, s.Value) {
			return &types.OperationResponse{Success: false, ErrorCode: "internal",
				Message: fmt.Sprintf("The API server accepted the change, but %s is %s instead of %s in the DataScienceCluster it returned. Reload the page and check it.", strings.Join(s.Path[2:], "."), kube.NonEmpty(fieldText(got), "unset"), fieldText(s.Value))}, false
		}
	}
	return nil, false
}

// writeChecked plans a change on the read v and writes it (write). When
// the API server refuses the patch with 422, the DSC is read again: a
// different uid (or none) ends the change (deleted or created again); the
// same resourceVersion means the refusal is the API server's own, shown as
// it is; a newer resourceVersion means a test failed on a DSC that moved
// since the read (the operator's status writes move it too), so plan runs
// again on the new read, with every check of the change (the removal
// preconditions, which are about other objects than the DSC; the reviewed
// plan's token), before the next patch. At most dscWriteAttempts patches.
// A plan that refuses, or a read that fails, ends the change with nothing
// changed. It returns the read and the plan that were written, or the
// result that ended the change.
func writeChecked(c *Client, v *dscView, plan func(v *dscView) (*dscPlanned, *types.OperationResponse)) (*dscView, *dscPlanned, *types.OperationResponse) {
	uid, name := v.uid, v.name
	for attempt := 1; ; attempt++ {
		p, res := plan(v)
		if res != nil {
			return nil, nil, res
		}
		res, rejected := v.write(c, p)
		switch {
		case res == nil:
			return v, p, nil
		case !rejected:
			return nil, nil, res
		}
		next, err := readDSCView(c)
		switch {
		case err != nil:
			return nil, nil, &types.OperationResponse{Success: false, ErrorCode: kube.ErrorCodeFromK8sErr(err), Logs: res.Logs,
				Message: fmt.Sprintf("The API server refused the change to DataScienceCluster %s, and it could not be read again to tell whether it changed since it was checked: %v. Nothing was changed.", name, err)}
		case next.state != DSCStatePresent || next.uid != uid:
			return nil, nil, &types.OperationResponse{Success: false, ErrorCode: "conflict",
				Message: fmt.Sprintf("DataScienceCluster %s was deleted or created again after the change was checked. Nothing was changed. Reload the page and review the change again.", name)}
		case next.meta.ResourceVersion == v.meta.ResourceVersion:
			return nil, nil, res
		case attempt == dscWriteAttempts:
			return nil, nil, &types.OperationResponse{Success: false, ErrorCode: "conflict", Logs: res.Logs,
				Message: fmt.Sprintf("DataScienceCluster %s changed each time the change was sent (%d attempts; the operator writes its status, or someone else is editing it), so the API server's refusal may be that change or a refusal of its own. Its last answer: %s Nothing was changed. Try again.", name, attempt, strings.TrimSuffix(res.Message, " Nothing was changed."))}
		}
		slog.Info("dsc changed since it was checked; checking again", "dsc", name, "attempt", attempt, "from", v.meta.ResourceVersion, "to", next.meta.ResourceVersion)
		v = next
	}
}

// changeSummary words the toggles a plan changes.
func changeSummary(changes []types.DSCChange) string {
	var parts []string
	for _, ch := range changes {
		if !ch.Toggle || (ch.From == ch.To && ch.FromSet == ch.ToSet) {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s %s → %s", ch.Path, kube.NonEmpty(ch.From, "not set"), kube.NonEmpty(ch.To, "not set")))
	}
	if len(parts) == 0 {
		return fmt.Sprintf("%d %s", len(changes), kube.Verb(len(changes), "field", "fields"))
	}
	return strings.Join(parts, "; ")
}

// refusedResult is the result for a plan that must not run.
func refusedResult(p *types.DSCPlan) *types.OperationResponse {
	if p.Refusal != "" {
		return &types.OperationResponse{Success: false, Message: p.Refusal, ErrorCode: p.Code}
	}
	var reasons []string
	var names []string
	for _, b := range p.Blocked {
		names = append(names, b.Component)
		reasons = append(reasons, fmt.Sprintf("%s: %s", b.Component, strings.Join(b.Reasons, "; ")))
	}
	return &types.OperationResponse{Success: false, ErrorCode: "prerequisites", Logs: reasons,
		Message: fmt.Sprintf("Not changing the DataScienceCluster: %s must not be removed now. Nothing was changed. %s", strings.Join(names, ", "), strings.Join(reasons, " "))}
}

// SetDSCManagementState sets one managementState (and, with WithParent,
// the parents that are not Managed) in the DataScienceCluster, only when
// it still holds From and the change is the plan the user reviewed (its
// UID and token, from GET /api/components/dsc/plan). It runs planToggle,
// the function the plan comes from, so the checks the dialog showed are
// the checks that run; to Removed runs the removal preconditions first.
func SetDSCManagementState(c *Client, req types.DSCToggleRequest) (*types.OperationResponse, error) {
	v, err := readDSCView(c)
	if err != nil {
		return nil, err
	}
	return v.setManagementState(c, req, "set-component-state", true), nil
}

// setManagementState runs a toggle. reviewed: the request carries the
// token of the plan the user reviewed, which must match the plan computed
// now (Diagnostics' disable-component fix has its own confirmation and no
// plan).
func (v *dscView) setManagementState(c *Client, req types.DSCToggleRequest, activity string, reviewed bool) *types.OperationResponse {
	v, p, res := writeChecked(c, v, func(v *dscView) (*dscPlanned, *types.OperationResponse) {
		return v.checkToggle(c, req, reviewed)
	})
	if res != nil {
		return res
	}
	detail := v.name + ": " + changeSummary(p.plan.Changes)
	recordActivity(c, activity, detail, true)
	slog.Info("dsc management state set", "dsc", v.name, "path", req.Path, "to", req.To, "user", kube.GetUser(c))
	msg := "Set " + changeSummary(p.plan.Changes) + "."
	if len(p.plan.Warnings) > 0 {
		msg += " " + strings.Join(p.plan.Warnings, " ")
	}
	return &types.OperationResponse{Success: true, Message: msg, Logs: []string{detail}}
}

// checkToggle checks a toggle request against the read v and plans it:
// the DSC and the field are still what the request saw, the plan's checks
// pass and, when reviewed, it is the plan the user reviewed.
func (v *dscView) checkToggle(c *Client, req types.DSCToggleRequest, reviewed bool) (*dscPlanned, *types.OperationResponse) {
	if v.state == DSCStatePresent && req.Name != "" && req.Name != v.name {
		return nil, &types.OperationResponse{Success: false, ErrorCode: "conflict", Message: fmt.Sprintf("The DataScienceCluster is %s now, not %s. Reload the page. Nothing was changed.", v.name, req.Name)}
	}
	if t, ok := v.byKey[req.Path]; ok && t.Toggle && v.state == DSCStatePresent {
		if cur, _ := lookupState(v.components(), t.Path); cur != req.From {
			return nil, &types.OperationResponse{Success: false, ErrorCode: "conflict",
				Message: fmt.Sprintf("%s changed since you looked: it is %s now, not %s. Reload the page and try again. Nothing was changed.", req.Path, kube.NonEmpty(cur, "not set"), kube.NonEmpty(req.From, "not set"))}
		}
	}
	p := v.planToggle(c, req.Path, req.To, req.WithParent)
	if p.plan.Refusal != "" || len(p.plan.Blocked) > 0 {
		return nil, refusedResult(p.plan)
	}
	if reviewed && (req.UID != v.uid || req.Token == "" || req.Token != p.plan.Token) {
		return nil, &types.OperationResponse{Success: false, ErrorCode: "conflict",
			Message: "The DataScienceCluster changed since you reviewed this change, so nothing was changed. Review it again."}
	}
	return p, nil
}

// runPlanned runs a fix or a soft reset after checking it is the plan the
// user reviewed.
func runPlanned(c *Client, req types.DSCPlanRequest, action, activity string) (*types.OperationResponse, error) {
	v, err := readDSCView(c)
	if err != nil {
		return nil, err
	}
	var planned func(v *dscView) *dscPlanned
	switch action {
	case "fix":
		planned = func(v *dscView) *dscPlanned { return v.planFix(c) }
	case "soft-reset":
		if req.KeepEnabled == nil {
			return keepEnabledMissing(), nil
		}
		planned = func(v *dscView) *dscPlanned { return v.planSoftReset(c, *req.KeepEnabled) }
	default:
		return nil, fmt.Errorf("unknown action %q", action)
	}
	v, p, res := writeChecked(c, v, func(v *dscView) (*dscPlanned, *types.OperationResponse) {
		p := planned(v)
		if p.plan.Refusal != "" || len(p.plan.Blocked) > 0 {
			return nil, refusedResult(p.plan)
		}
		if req.Name != v.name || req.UID != v.uid || req.Token == "" || req.Token != p.plan.Token {
			return nil, &types.OperationResponse{Success: false, ErrorCode: "conflict",
				Message: "The DataScienceCluster or the release default changed since you reviewed this change, so nothing was changed. Review it again."}
		}
		return p, nil
	})
	if res != nil {
		return res, nil
	}
	detail := v.name + ": " + changeSummary(p.plan.Changes)
	if len(p.plan.Kept) > 0 {
		detail += "; kept " + strings.Join(p.plan.Kept, ", ")
	}
	recordActivity(c, activity, detail, true)
	slog.Info("dsc changed", "action", action, "dsc", v.name, "user", kube.GetUser(c))
	return &types.OperationResponse{Success: true, Message: fmt.Sprintf("Changed %s.%s", changeSummary(p.plan.Changes), keptText(p.plan.Kept)), Logs: []string{detail}}, nil
}

// keptText words the components a reset kept ("" when none).
func keptText(kept []string) string {
	if len(kept) == 0 {
		return ""
	}
	return " Kept: " + strings.Join(kept, ", ") + " (you turned these on)."
}

// keepEnabledMissing refuses a reset that does not say whether to keep
// the components the user turned on: the plan differs either way.
func keepEnabledMissing() *types.OperationResponse {
	return &types.OperationResponse{Success: false, ErrorCode: "validation",
		Message: "Say whether the reset keeps the components you turned on (keepEnabled true or false), as in the plan you reviewed. Nothing was changed."}
}

// FixDSC adds the toggles the release default sets and the DSC leaves
// unset (with the default's values), and removes fields the CRD does not
// define: exactly the plan the user reviewed.
func FixDSC(c *Client, req types.DSCPlanRequest) (*types.OperationResponse, error) {
	return runPlanned(c, req, "fix", "fix-dsc")
}

// SoftResetDSC makes spec.components the release default in place.
func SoftResetDSC(c *Client, req types.DSCPlanRequest) (*types.OperationResponse, error) {
	return runPlanned(c, req, "soft-reset", "reset-dsc")
}
