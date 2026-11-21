package dsc

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/components"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
	"gopkg.in/yaml.v3"
)

// Hard reset: delete the DataScienceCluster and create the release default
// in its place, with the same name and the namespaces of the old one. The
// operator's DSC finalizer (platform.opendatahub.io/finalizer) only marks
// the DSC's modules Removed on the Platform and is removed in the same
// reconcile; the module CRs the DSC owned are then garbage-collected and
// the Platform controller cleans the modules up afterwards
// (dsc_hard_reset_teardown.go, with the sources). The new DSC is created
// only once that is done (the validating webhook allows one
// DataScienceCluster). User resources (Notebooks, DSPAs, InferenceServices,
// model registries) are not owned by the DSC and survive; their controllers
// are gone until the components come back. A namespace the DSC (or what it
// owns) owns, and no other owner keeps, would not survive, so a hard reset
// is refused while one exists (namespaceOwnerBlocker, read with the
// signed-in user's own token).
//
// Before the delete, the new spec is validated against the live DSC (a
// server-side dry run), and the object to create is recorded in the
// operation ConfigMap: whatever stops the reset after the delete (a
// failure, the timeout, a lost lock, a restart of the tool), "Finish the
// hard reset" creates exactly that object once no DataScienceCluster
// exists (FinishHardReset).

// The hard reset's waits. Tests shorten them; every wait runs on clk.
var (
	HardResetDeleteTimeout = 10 * time.Minute
	HardResetReadyTimeout  = 3 * time.Minute
	HardResetPollInterval  = 5 * time.Second
)

// platformName returns the platform the operator reports: the DSC's
// status.release.name, else the DSCInitialization's.
func (v *View) platformName(c *kube.Client) string {
	release, _ := v.status()["release"].(map[string]any)
	if name, _ := release["name"].(string); name != "" {
		return name
	}
	r, err := rhoai.ListServed(c, rhoai.DSCIGroup, "/apis/dscinitialization.opendatahub.io/%s/dscinitializations", nil, rhoai.DSCIFallbackVersions)
	if err != nil || r.Body == nil {
		return ""
	}
	var list struct {
		Items []struct {
			Status struct {
				Release struct {
					Name string `json:"name"`
				} `json:"release"`
			} `json:"status"`
		} `json:"items"`
	}
	if json.Unmarshal(r.Body, &list) != nil || len(list.Items) == 0 {
		return ""
	}
	return list.Items[0].Status.Release.Name
}

// managedPlatformRefusal is the refusal for a platform the hard reset is
// not offered on ("" on self-managed RHOAI).
func managedPlatformRefusal(platform string) string {
	if platform == SelfManagedPlatform {
		return ""
	}
	return fmt.Sprintf("The operator reports the platform as %q, not %q. Managed RHOAI recreates default-dsc itself, and on any platform this tool cannot identify a hard reset is not offered. Use the soft reset.",
		kube.NonEmpty(platform, "unknown"), SelfManagedPlatform)
}

// NewDefaultObject is the DataScienceCluster a hard reset creates: the
// release default (alm-examples) with components (ResetTarget: the release
// default's, with the toggles kept), the old DSC's name and the values of
// its protected fields (the workbench and AI Hub namespaces), where the
// default has the object that holds them.
func (v *View) NewDefaultObject(comps map[string]any) map[string]any {
	spec, _ := kube.NormalizeJSON(v.defaults).(map[string]any)
	if comps, _ = kube.NormalizeJSON(comps).(map[string]any); len(comps) > 0 {
		spec["components"] = comps
	}
	fields := make([]string, 0, len(v.protected))
	for f := range v.protected {
		fields = append(fields, f)
	}
	sort.Strings(fields)
	for _, f := range fields {
		keys := strings.Split(f, ".")
		old, ok := components.LookupPath(v.components(), keys)
		if !ok {
			continue
		}
		parent, ok := components.LookupPath(comps, keys[:len(keys)-1])
		if pm, isMap := parent.(map[string]any); ok && isMap {
			pm[keys[len(keys)-1]] = old
		}
	}
	meta := map[string]any{"name": v.name}
	if len(v.defaultsLabel) > 0 {
		meta["labels"] = v.defaultsLabel
	}
	return map[string]any{"apiVersion": v.apiVersion, "kind": "DataScienceCluster", "metadata": meta, "spec": spec}
}

// planHardReset plans the hard reset; keep keeps the components the user
// turned on (ResetTarget).
func (v *View) planHardReset(c *kube.Client, keep bool) *dscPlanned {
	p := v.newPlan("hard-reset")
	p.plan.KeepEnabled = keep
	if msg, code := v.readOnlyReason(); msg != "" {
		return p.refuse(msg, code)
	}
	if v.defaults == nil {
		return p.refuse(v.defaultsErr+" A hard reset needs the release default. Nothing was changed.", "prerequisites")
	}
	if msg := managedPlatformRefusal(v.platformName(c)); msg != "" {
		return p.refuse(msg, "managed_platform")
	}
	if why := OperatorDeploymentsBlocker(c); why != "" {
		return p.refuse("The RHOAI operator removes the DataScienceCluster's finalizer, so the deletion would hang: "+why+". Nothing was changed.", "prerequisites")
	}
	if why := namespaceOwnerBlocker(c); why != "" {
		return p.refuse("A hard reset is not possible. "+why+" Nothing was changed.", "prerequisites")
	}
	obj := v.NewDefaultObject(v.ResetTarget(p.plan, keep))
	created := v.asCreated(obj)
	text, err := yaml.Marshal(created)
	if err != nil {
		return p.refuse("Cannot build the new DataScienceCluster: "+err.Error(), "internal")
	}
	p.plan.NewObject = string(text)
	p.plan.DeleteTimeoutSeconds = int(HardResetDeleteTimeout / time.Second)
	p.plan.ProtectedFields = v.protectedFields()
	newSpec, _ := obj["spec"].(map[string]any)
	newComps, _ := newSpec["components"].(map[string]any)
	createdSpec, _ := created["spec"].(map[string]any)
	createdComps, _ := createdSpec["components"].(map[string]any)
	p.plan.Changes = v.changesBetween(v.defaultedComponents(v.components()), createdComps)
	for _, t := range v.tree {
		if len(t.Path) == 1 && v.componentEnabled(v.components(), t) {
			p.plan.TornDown = append(p.plan.TornDown, t.Key())
		}
		if t.Toggle && v.deployed(v.components(), t.Path) && !v.deployed(newComps, t.Path) {
			p.plan.StayRemoved = append(p.plan.StayRemoved, t.Key())
		}
	}
	v.removalBlocks(c, v.components(), newComps, true).applyTo(p.plan)
	p.plan.Token = planToken("hard-reset", v.uid, v.apiVersion, created, keep)
	return p
}

// asCreated is obj as the API server stores it: its spec with what the
// installed CRD's defaults fill in (components.ApplySchemaDefaults). The plan shows
// and compares this object; the create sends obj and the API server fills
// in the same values.
func (v *View) asCreated(obj map[string]any) map[string]any {
	out, _ := kube.NormalizeJSON(obj).(map[string]any)
	if spec, ok := out["spec"].(map[string]any); ok {
		components.ApplySchemaDefaults(spec, v.spec)
	}
	return out
}

// hardResetRun emits the steps of the hard reset and keeps them as logs.
type hardResetRun struct {
	emit func(types.UpdateStepEvent)
	logs []string
}

func (r *hardResetRun) step(step, status, msg string) {
	r.logs = append(r.logs, msg)
	r.emit(types.UpdateStepEvent{Step: step, Status: status, Message: msg})
}

func (r *hardResetRun) fail(step, msg, code string) *types.OperationResponse {
	r.step(step, "failed", msg)
	return &types.OperationResponse{Success: false, Message: msg, Logs: r.logs, ErrorCode: code}
}

// dscDeleteBody is the DeleteOptions of the hard reset: the DSC is deleted
// only if it is still the one the user reviewed (its UID), in the
// background: the garbage collector then deletes what it owns as owner-less
// objects, each module CR keeping its finalizer until its operator cleaned
// up. Foreground would also delete, in the foreground, the workloads a
// module CR owns while its operator may still need them (kubernetes v1.34
// garbagecollector.go:591-623; dsc_hard_reset_teardown.go). The reset waits
// for that cleanup itself (waitTeardown).
func dscDeleteBody(uid string) []byte {
	body, _ := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "DeleteOptions", "propagationPolicy": "Background", "preconditions": map[string]any{"uid": uid}})
	return body
}

// finishHelp is said after every failure once the DSC was deleted: how to
// create exactly the planned object.
func finishHelp(yamlText string) string {
	return "Once no DataScienceCluster exists, use Finish the hard reset on the Components page: it creates exactly the object this reset planned (the tool recorded it). Or create it yourself with oc create -f - and this object:\n" + yamlText
}

// HardReset deletes the DataScienceCluster the user reviewed and creates
// the release default in its place. Steps: check, delete, wait_deleted,
// create, wait_ready. Every check runs before the delete: the plan's
// (removal blockers, the operator), the delete's and the new spec's server
// dry runs. It stops without creating anything when the old DSC is not
// gone by HardResetDeleteTimeout; it never removes a finalizer.
func HardReset(c *kube.Client, req types.DSCPlanRequest, emit func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
	r := &hardResetRun{emit: emit}
	r.step("check", "running", "Checking the DataScienceCluster, the operator and every component")
	if req.KeepEnabled == nil {
		res := keepEnabledMissing()
		return r.fail("check", res.Message, res.ErrorCode), nil
	}
	v, err := ReadView(c)
	if err != nil {
		return r.fail("check", "Cannot read the DataScienceCluster: "+err.Error()+". Nothing was changed.", kube.ErrorCodeFromK8sErr(err)), nil
	}
	p := v.planHardReset(c, *req.KeepEnabled)
	if p.plan.Refusal != "" || len(p.plan.Blocked) > 0 {
		res := refusedResult(p.plan)
		r.step("check", "failed", res.Message)
		res.Logs = append(r.logs, res.Logs...)
		return res, nil
	}
	if req.Confirm != v.name {
		return r.fail("check", fmt.Sprintf("Type the DataScienceCluster's name, %s, to confirm. Nothing was changed.", v.name), "validation"), nil
	}
	if req.Name != v.name || req.UID != v.uid || req.Token == "" || req.Token != p.plan.Token {
		return r.fail("check", "The DataScienceCluster or the release default changed since you reviewed the hard reset, so nothing was changed. Review it again.", "conflict"), nil
	}
	path := dscObjectPath(v.version, v.name)
	if _, _, err := c.Do(http.MethodDelete, path, "application/json", dscDeleteBody(v.uid), url.Values{"dryRun": {"All"}}); err != nil {
		return r.fail("check", "The API server refused deleting the DataScienceCluster in its dry run: "+rhoai.ConversionMessage(err)+". Nothing was changed.", kube.NonEmpty(kube.ErrorCodeFromK8sErr(err), "validation")), nil
	}
	obj := v.NewDefaultObject(v.ResetTarget(&types.DSCPlan{}, *req.KeepEnabled))
	// The new spec in a dry run against the live DSC: the schema
	// (fieldValidation=Strict), the CRD's validation rules and the
	// operator's validating webhook see it before anything is deleted. The
	// test of the UID makes it about this DSC only.
	validate := []kube.JSONPatchOp{components.PatchOp("test", []string{"metadata", "uid"}, v.uid), components.PatchOp("replace", []string{"spec"}, obj["spec"])}
	if _, err := patchDSCFields(c, path, validate, true); err != nil {
		// A failed test of the UID gets the same bare 422 as a refused
		// spec (see dsc_manager.go): the DSC read again tells them apart.
		if kube.IsK8sError(err, http.StatusUnprocessableEntity) {
			if meta, found, rerr := kube.ReadObjectMeta(c, path); rerr == nil && (!found || meta.UID != v.uid) {
				return r.fail("check", fmt.Sprintf("DataScienceCluster %s was deleted or created again after it was checked. Nothing was changed. Review the hard reset again.", v.name), "conflict"), nil
			}
		}
		return r.fail("check", "The API server refused the release default in a dry run against DataScienceCluster "+v.name+": "+rhoai.ConversionMessage(err)+". Nothing was changed.", kube.NonEmpty(kube.ErrorCodeFromK8sErr(err), "validation")), nil
	}
	createYAML := p.plan.NewObject
	objJSON, err := json.Marshal(obj)
	if err != nil {
		return r.fail("check", "Cannot build the new DataScienceCluster: "+err.Error()+". Nothing was changed.", "internal"), nil
	}
	// The module operators whose removal (the platform's phase 2) the reset
	// waits for after their module CRs are gone.
	ops, refusal, err := HardResetOperators(c, v.uid)
	if err != nil {
		return r.fail("check", "Cannot read the module CRs the DataScienceCluster owns and their operators: "+err.Error()+". Nothing was changed.", kube.NonEmpty(kube.ErrorCodeFromK8sErr(err), "prerequisites")), nil
	}
	if refusal != "" {
		return r.fail("check", "A hard reset is not possible: "+refusal+". Nothing was changed.", "prerequisites"), nil
	}
	rec := &types.DSCHardResetRecord{Name: v.name, OldUID: v.uid, APIVersion: v.apiVersion, Object: string(objJSON), YAML: createYAML,
		OperatorCSV: v.op.Name, OperatorVersion: v.op.Version, User: kube.GetUser(c), StartedAt: kube.Clock().Now().UTC().Format(time.RFC3339)}
	for _, o := range ops {
		rec.ModuleOperators = append(rec.ModuleOperators, o.Record())
	}
	if err := record.SaveDSCHardResetRecord(c, rec); err != nil {
		return r.fail("check", "Cannot record the DataScienceCluster this reset creates, so the reset could not be finished after a failure: "+err.Error()+". Nothing was changed.", kube.NonEmpty(kube.ErrorCodeFromK8sErr(err), "internal")), nil
	}
	kept := ""
	if len(p.plan.Kept) > 0 {
		kept = ", keeping " + strings.Join(p.plan.Kept, ", ")
	}
	r.step("check", "success", fmt.Sprintf("DataScienceCluster %s can be reset: %d components are torn down and the release default of operator %s is created%s", v.name, len(p.plan.TornDown), v.op.Version, kept))

	r.step("delete", "running", "Deleting DataScienceCluster "+v.name)
	if _, _, err := c.Do(http.MethodDelete, path, "application/json", dscDeleteBody(v.uid), nil); err != nil {
		record.ClearDSCHardResetRecord(c)
		if kube.IsK8sNotFound(err) || kube.IsK8sConflict(err) {
			return r.fail("delete", "DataScienceCluster "+v.name+" was deleted or replaced by someone else since the check, so nothing was changed by this reset. Reload the page.", "conflict"), nil
		}
		return r.fail("delete", "Deleting the DataScienceCluster failed: "+rhoai.ConversionMessage(err)+". Nothing was changed.", kube.ErrorCodeFromK8sErr(err)), nil
	}
	record.Action(c, "hard-reset-dsc", v.name+": deleted (uid "+v.uid+") for a hard reset", true)
	r.step("delete", "success", "Deleted DataScienceCluster "+v.name+" (propagation Background); the operator marks its modules Removed")

	r.step("wait_deleted", "running", "Waiting until the DataScienceCluster is gone and the platform has cleaned up its modules")
	if res := waitDSCGone(c, path, v, ops, r, createYAML); res != nil {
		record.Action(c, "hard-reset-dsc", v.name+": "+firstLine(res.Message), false)
		return res, nil
	}

	r.step("create", "running", "Creating the release default DataScienceCluster "+v.name)
	if msg, code := createPlannedDSC(c, v.version, obj); msg != "" {
		res := r.fail("create", "The old DataScienceCluster was deleted, but "+msg+" "+finishHelp(createYAML), code)
		record.Action(c, "hard-reset-dsc", v.name+": deleted, creating the release default failed", false)
		return res, nil
	}
	record.ClearDSCHardResetRecord(c)
	record.Action(c, "hard-reset-dsc", v.name+": the release default of operator "+v.op.Version+" created", true)
	r.step("create", "success", "Created the release default DataScienceCluster "+v.name)

	r.step("wait_ready", "running", "Waiting for the components to be ready")
	msg := waitDSCReady(c, path, r)
	slog.Info("dsc hard reset", "dsc", v.name, "user", kube.GetUser(c))
	return &types.OperationResponse{Success: true, Message: msg, Logs: r.logs}, nil
}

// firstLine is s up to its first line break.
func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// createPlannedDSC creates obj at version after checking that no
// DataScienceCluster exists (the operator allows one, and one that
// appeared meanwhile is someone else's) and a server-side dry run, with
// fieldValidation=Strict. It returns why it did not ("" when it did) and
// an error code.
func createPlannedDSC(c *kube.Client, version string, obj map[string]any) (string, string) {
	listPath := fmt.Sprintf(rhoai.DSCListFmt, version)
	body, _, err := c.Get(listPath)
	if err != nil {
		return "the DataScienceClusters could not be listed before creating one: " + rhoai.ConversionMessage(err) + ". Nothing was created.", kube.NonEmpty(kube.ErrorCodeFromK8sErr(err), "internal")
	}
	var list struct {
		Items []struct {
			Metadata kube.ObjectMeta `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return "the list of DataScienceClusters could not be read: " + err.Error() + ". Nothing was created.", "internal"
	}
	if len(list.Items) > 0 {
		return fmt.Sprintf("DataScienceCluster %s now exists (someone else, or the operator, created it), so nothing was created. Review it on the Components page.", list.Items[0].Metadata.Name), "conflict"
	}
	raw, err := json.Marshal(obj)
	if err != nil {
		return "the new DataScienceCluster could not be encoded: " + err.Error() + ". Nothing was created.", "internal"
	}
	q := url.Values{"fieldManager": {ComponentsManager}, "fieldValidation": {"Strict"}}
	dry := url.Values{"fieldManager": {ComponentsManager}, "fieldValidation": {"Strict"}, "dryRun": {"All"}}
	if _, _, err := c.Do(http.MethodPost, listPath, "application/json", raw, dry); err != nil {
		return "the API server refused the release default in its dry run: " + rhoai.ConversionMessage(err) + ". Nothing was created.", kube.NonEmpty(kube.ErrorCodeFromK8sErr(err), "validation")
	}
	if _, _, err := c.Do(http.MethodPost, listPath, "application/json", raw, q); err != nil {
		return "creating the release default failed: " + rhoai.ConversionMessage(err) + ".", kube.NonEmpty(kube.ErrorCodeFromK8sErr(err), "internal")
	}
	return "", ""
}

// dscWaitState is what one poll of a DSC being deleted or provisioned sees.
type dscWaitState struct {
	found      bool
	uid        string
	finalizers []string
	conditions []types.DSCConditionView
}

func readDSCWaitState(c *kube.Client, path string) (dscWaitState, error) {
	body, exists, err := c.GetOptional(path)
	if err != nil {
		return dscWaitState{}, err
	}
	if !exists {
		return dscWaitState{}, nil
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return dscWaitState{}, err
	}
	v := &View{obj: obj}
	var meta kube.ObjectMeta
	if raw, err := json.Marshal(obj["metadata"]); err == nil {
		_ = json.Unmarshal(raw, &meta)
	}
	return dscWaitState{found: true, uid: meta.UID, finalizers: meta.Finalizers, conditions: v.conditions()}, nil
}

// stopCause words why the operation's context ended (a lost lock names
// itself).
func stopCause(ctx context.Context) string {
	if cause := context.Cause(ctx); cause != nil {
		return cause.Error()
	}
	return "stopped"
}

// waitDSCGone polls until the old DSC is gone and its teardown is done
// (dsc_hard_reset_teardown.go): no module CR it owned is left, the module
// operators of those CRs (ops) are removed, and the Platform reports no
// module pending deletion. A read that fails is tried again until the
// deadline (the API server or the conversion webhook can be briefly
// unavailable while the operator cleans up). It returns the failed result
// when that is not done in time, a DSC with another UID appears, or the
// operation is stopped; nil once it is done. Every failure carries the
// planned object (yamlText) and the way to finish the reset.
func waitDSCGone(c *kube.Client, path string, v *View, ops []HardResetOperator, r *hardResetRun, yamlText string) *types.OperationResponse {
	deadline := kube.Clock().Now().Add(HardResetDeleteTimeout)
	last := ""
	var st dscWaitState
	var td TeardownState
	var readErr error
	gone := false
	interrupted := func() *types.OperationResponse {
		return r.fail("wait_deleted", fmt.Sprintf("The reset was stopped (%s) while DataScienceCluster %s and its modules were being removed. Nothing was created. %s", stopCause(c.Context()), v.name, finishHelp(yamlText)), "interrupted")
	}
	for {
		if c.Context().Err() != nil {
			return interrupted()
		}
		var err error
		st, err = readDSCWaitState(c, path)
		if err == nil && !st.found {
			if !gone {
				gone = true
				r.step("wait_deleted", "running", "DataScienceCluster "+v.name+" is gone")
			}
			td, err = readTeardown(c, v.uid, ops)
		}
		switch {
		case err != nil && c.Context().Err() != nil:
			return interrupted()
		case err != nil:
			readErr = err
			if msg := "Cannot read the cluster now (" + err.Error() + "); trying again"; msg != last {
				last = msg
				r.step("wait_deleted", "running", msg)
			}
		case !st.found && td.done():
			r.step("wait_deleted", "success", "DataScienceCluster "+v.name+" is gone and the platform has cleaned up its modules")
			return nil
		case !st.found:
			readErr = nil
			if msg := td.Progress(); msg != last {
				last = msg
				r.step("wait_deleted", "running", msg)
			}
		case st.uid != v.uid:
			return r.fail("wait_deleted", fmt.Sprintf("A new DataScienceCluster %s appeared (UID %s) before this reset created one: someone else, or the operator, created it. This reset creates nothing. Review it on the Components page. The object this reset planned was:\n%s", v.name, st.uid, yamlText), "conflict")
		default:
			readErr = nil
			if msg := deletionProgress(st); msg != last {
				last = msg
				r.step("wait_deleted", "running", msg)
			}
		}
		if !kube.Clock().Now().Before(deadline) {
			break
		}
		if err := kube.Clock().Sleep(c.Context(), HardResetPollInterval); err != nil {
			return interrupted()
		}
	}
	if readErr != nil {
		return r.fail("wait_deleted", fmt.Sprintf("The cluster could not be read for the last poll of %s: %v. Nothing was created. Check the DataScienceCluster with oc get datasciencecluster %s -o yaml and the modules in Diagnostics (Platform modules). %s",
			HardResetDeleteTimeout, readErr, v.name, finishHelp(yamlText)), kube.NonEmpty(kube.ErrorCodeFromK8sErr(readErr), "timeout"))
	}
	if !st.found {
		return r.fail("wait_deleted", fmt.Sprintf("DataScienceCluster %s is gone, but after %s %s. Nothing was created. %s",
			v.name, HardResetDeleteTimeout, td.Stuck(), finishHelp(yamlText)), "timeout")
	}
	return r.fail("wait_deleted", fmt.Sprintf("DataScienceCluster %s is still being deleted after %s: finalizer %s remains (%s). The RHOAI operator's DSC finalizer only marks the DSC's modules Removed on the Platform and is removed in the same reconcile, so a finalizer that stays means the operator has not processed the deletion: check that it runs and read its logs (its Deployment, named in the installed CSV, is in namespace %s). Nothing was created. "+
		"Do not remove the finalizer by hand: the modules would not be marked Removed. %s",
		v.name, HardResetDeleteTimeout, kube.NonEmpty(strings.Join(st.finalizers, ", "), "none"), kube.NonEmpty(modulesNote(st), "no module status reported"), rhoai.SubNS, finishHelp(yamlText)), "timeout")
}

// deletionProgress words a poll of the DSC being deleted.
func deletionProgress(st dscWaitState) string {
	msg := "Still deleting: finalizer " + kube.NonEmpty(strings.Join(st.finalizers, ", "), "none")
	if note := modulesNote(st); note != "" {
		msg += "; " + note
	}
	return msg
}

// modulesNote is the ModulesReady condition's message, which lists the
// modules pending deletion.
func modulesNote(st dscWaitState) string {
	for _, cd := range st.conditions {
		if cd.Type == "ModulesReady" && cd.Message != "" {
			return "ModulesReady: " + kube.TruncateText(cd.Message, 300)
		}
	}
	return ""
}

// waitDSCReady polls the new DSC until its Ready condition is True, and
// words what is still provisioning when HardResetReadyTimeout passes.
func waitDSCReady(c *kube.Client, path string, r *hardResetRun) string {
	deadline := kube.Clock().Now().Add(HardResetReadyTimeout)
	last := ""
	var st dscWaitState
	for {
		var err error
		st, err = readDSCWaitState(c, path)
		if err == nil && st.found {
			if ready, waiting := readyState(st); ready {
				r.step("wait_ready", "success", "The DataScienceCluster is Ready")
				return "The DataScienceCluster was reset to the release default and is Ready."
			} else if waiting != last {
				last = waiting
				r.step("wait_ready", "running", "Still provisioning: "+waiting)
			}
		}
		if !kube.Clock().Now().Before(deadline) {
			break
		}
		if kube.Clock().Sleep(c.Context(), HardResetPollInterval) != nil {
			break
		}
	}
	msg := fmt.Sprintf("The DataScienceCluster was reset to the release default. It is not Ready yet after %s: %s. The operator keeps provisioning; this page shows each component as it comes up.", HardResetReadyTimeout, kube.NonEmpty(last, "no status reported yet"))
	r.step("wait_ready", "success", msg)
	return msg
}

// readyState reports Ready=True, or the conditions that are not True yet
// (Removed components are not waited for).
func readyState(st dscWaitState) (bool, string) {
	var waiting []string
	for _, cd := range st.conditions {
		if cd.Type == "Ready" && cd.Status == "True" {
			return true, ""
		}
		if cd.Status != "True" && cd.Reason != "Removed" && !strings.HasSuffix(cd.Type, "Dependencies") {
			waiting = append(waiting, cd.Type+" ("+kube.NonEmpty(cd.Reason, cd.Status)+")")
		}
	}
	return false, strings.Join(waiting, ", ")
}

// --- finishing a hard reset ---------------------------------------------------

// PendingHardResetToken binds "Finish the hard reset" to the recorded
// object.
func PendingHardResetToken(rec *types.DSCHardResetRecord) string {
	return planToken("finish-hard-reset", rec.Name, rec.APIVersion, rec.Object)
}

// PendingHardReset is the recorded hard reset the Components page offers to
// finish while no DataScienceCluster exists.
func PendingHardReset(c *kube.Client) (*types.DSCPendingHardReset, error) {
	rec, err := record.ReadDSCHardResetRecord(c)
	if err != nil || rec == nil {
		return nil, err
	}
	return &types.DSCPendingHardReset{Name: rec.Name, APIVersion: rec.APIVersion, NewObject: rec.YAML, OperatorVersion: rec.OperatorVersion,
		User: rec.User, StartedAt: rec.StartedAt, Token: PendingHardResetToken(rec)}, nil
}

// FinishHardReset creates the DataScienceCluster a hard reset recorded
// and did not create, exactly as recorded, behind the hard reset's guards:
// it is the object the user reviewed (its token), no DataScienceCluster
// exists, the CRD still stores the recorded version, the installed operator
// is still the one whose release default it is, the platform is
// self-managed, the operator runs, and the API server accepts it in a dry
// run.
func FinishHardReset(c *kube.Client, req types.DSCFinishRequest) (*types.OperationResponse, error) {
	refuse := func(msg, code string) (*types.OperationResponse, error) {
		return &types.OperationResponse{Success: false, Message: msg, ErrorCode: code}, nil
	}
	rec, err := record.ReadDSCHardResetRecord(c)
	if err != nil {
		return refuse("Cannot read the recorded hard reset: "+err.Error()+". Nothing was changed.", kube.NonEmpty(kube.ErrorCodeFromK8sErr(err), "internal"))
	}
	if rec == nil {
		return refuse("No hard reset is waiting to be finished. Nothing was changed.", "conflict")
	}
	if req.Name != rec.Name || req.Token != PendingHardResetToken(rec) {
		return refuse("The recorded hard reset changed since you reviewed it, so nothing was changed. Reload the page and review it again.", "conflict")
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(rec.Object), &obj); err != nil {
		return refuse("The recorded DataScienceCluster cannot be read: "+err.Error()+". Nothing was changed.", "internal")
	}
	group, version, _ := strings.Cut(rec.APIVersion, "/")
	if group != rhoai.DSCGroup || version == "" || obj["apiVersion"] != rec.APIVersion {
		return refuse(fmt.Sprintf("The recorded DataScienceCluster has apiVersion %q, which is not a DataScienceCluster version. Nothing was changed.", rec.APIVersion), "internal")
	}
	v, err := ReadView(c)
	if err != nil {
		return nil, err
	}
	switch v.state {
	case rhoai.DSCStateNoCRD:
		return refuse("The DataScienceCluster API is not installed now. Nothing was changed.", "prerequisites")
	case rhoai.DSCStatePresent:
		return refuse(fmt.Sprintf("DataScienceCluster %s now exists, so there is nothing to finish. Nothing was changed.", v.name), "conflict")
	}
	// The same teardown the reset waits for (dsc_hard_reset_teardown.go).
	var ops []HardResetOperator
	for _, s := range rec.ModuleOperators {
		o, ok := ParseHardResetOperator(s)
		if !ok {
			return refuse(fmt.Sprintf("The recorded hard reset names a module operator the tool cannot read (%q). Nothing was changed.", s), "internal")
		}
		ops = append(ops, o)
	}
	td, err := readTeardown(c, rec.OldUID, ops)
	if err != nil {
		return refuse("Cannot check that the old DataScienceCluster's modules are cleaned up: "+err.Error()+". Nothing was changed.", kube.NonEmpty(kube.ErrorCodeFromK8sErr(err), "prerequisites"))
	}
	if !td.done() {
		return refuse("The old DataScienceCluster's modules are not cleaned up yet: "+td.Stuck()+". Nothing was changed; finish the hard reset once they are.", "prerequisites")
	}
	crd, err := components.ReadDSCCRD(c)
	if err != nil {
		return refuse("Cannot read the DataScienceCluster CRD: "+err.Error()+". Nothing was changed.", kube.NonEmpty(kube.ErrorCodeFromK8sErr(err), "prerequisites"))
	}
	if crd.Storage() != version || crd.Version(version) == nil {
		return refuse(fmt.Sprintf("The DataScienceCluster CRD now stores %s, not %s, the version the reset planned, so the recorded object is not used. Create the DataScienceCluster again with Create DataScienceCluster. Nothing was changed.", kube.NonEmpty(crd.Storage(), "no version"), version), "prerequisites")
	}
	op, err := components.ReadInstalledOperator(c)
	if err != nil {
		return refuse("Cannot read the installed operator: "+err.Error()+". Nothing was changed.", kube.NonEmpty(kube.ErrorCodeFromK8sErr(err), "prerequisites"))
	}
	if op.Name != rec.OperatorCSV {
		return refuse(fmt.Sprintf("The installed operator is %s now, not %s, whose release default the reset planned, so the recorded object is not used. Create the DataScienceCluster again with Create DataScienceCluster. Nothing was changed.", op.Name, rec.OperatorCSV), "prerequisites")
	}
	if msg := managedPlatformRefusal(v.platformName(c)); msg != "" {
		return refuse(msg, "managed_platform")
	}
	if why := OperatorDeploymentsBlocker(c); why != "" {
		return refuse("The DataScienceCluster is not created now: "+why+". Nothing was changed.", "prerequisites")
	}
	if msg, code := createPlannedDSC(c, version, obj); msg != "" {
		return refuse("The hard reset is not finished: "+msg, code)
	}
	record.ClearDSCHardResetRecord(c)
	record.Action(c, "finish-hard-reset-dsc", rec.Name+": the release default of operator "+rec.OperatorVersion+" created (hard reset finished)", true)
	slog.Info("dsc hard reset finished", "dsc", rec.Name, "user", kube.GetUser(c))
	return &types.OperationResponse{Success: true, Message: fmt.Sprintf("Created DataScienceCluster %s: the hard reset is finished. The operator brings the components up; this page shows each as it comes up.", rec.Name)}, nil
}
