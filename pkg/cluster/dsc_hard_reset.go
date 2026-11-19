package cluster

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Hard reset: delete the DataScienceCluster and create the release default
// in its place, with the same name and the namespaces of the old one. The
// operator's DSC finalizer (platform.opendatahub.io/finalizer, rhods-operator
// 3.6 datasciencecluster_controller.go WithFinalizer(disableDSCModulesOnDelete))
// sets every module to Removed and the modules are cleaned up in two phases;
// only then is the DSC gone and the new one can be created (the validating
// webhook allows one DataScienceCluster). User resources (Notebooks, DSPAs,
// InferenceServices, model registries) are not owned by the DSC and survive;
// their controllers are gone until the components come back.

// The hard reset's waits. Tests shorten them; every wait runs on clk.
var (
	HardResetDeleteTimeout = 10 * time.Minute
	HardResetReadyTimeout  = 3 * time.Minute
	HardResetPollInterval  = 5 * time.Second
)

// platformName returns the platform the operator reports: the DSC's
// status.release.name, else the DSCInitialization's.
func (v *dscView) platformName(c *Client) string {
	release, _ := v.status()["release"].(map[string]interface{})
	if name, _ := release["name"].(string); name != "" {
		return name
	}
	r, err := listServed(c, dsciGroup, "/apis/dscinitialization.opendatahub.io/%s/dscinitializations", nil, dsciFallbackVersions)
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

// newDefaultObject is the DataScienceCluster a hard reset creates: the
// release default (alm-examples) with the old DSC's name and the values of
// its protected fields (the workbench and AI Hub namespaces), where the
// default has the object that holds them.
func (v *dscView) newDefaultObject() map[string]interface{} {
	spec, _ := normalizeJSON(v.defaults).(map[string]interface{})
	comps, _ := spec["components"].(map[string]interface{})
	fields := make([]string, 0, len(v.protected))
	for f := range v.protected {
		fields = append(fields, f)
	}
	sort.Strings(fields)
	for _, f := range fields {
		keys := strings.Split(f, ".")
		old, ok := lookupPath(v.components(), keys)
		if !ok {
			continue
		}
		parent, ok := lookupPath(comps, keys[:len(keys)-1])
		if pm, isMap := parent.(map[string]interface{}); ok && isMap {
			pm[keys[len(keys)-1]] = old
		}
	}
	meta := map[string]interface{}{"name": v.name}
	if len(v.defaultsLabel) > 0 {
		meta["labels"] = v.defaultsLabel
	}
	return map[string]interface{}{"apiVersion": v.apiVersion, "kind": "DataScienceCluster", "metadata": meta, "spec": spec}
}

func (v *dscView) planHardReset(c *Client) *dscPlanned {
	p := v.newPlan("hard-reset")
	if msg, code := v.readOnlyReason(); msg != "" {
		return p.refuse(msg, code)
	}
	if v.defaults == nil {
		return p.refuse(v.defaultsErr, "prerequisites")
	}
	if platform := v.platformName(c); platform != selfManagedPlatform {
		return p.refuse(fmt.Sprintf("The operator reports the platform as %q, not %q. Managed RHOAI recreates default-dsc itself, and on any platform this tool cannot identify a hard reset is not offered. Use the soft reset.",
			nonEmpty(platform, "unknown"), selfManagedPlatform), "managed_platform")
	}
	if ok, why := deploymentReady(c, SubNS, "rhods-operator"); !ok {
		return p.refuse("rhods-operator removes the DataScienceCluster's finalizer and must be running, or the deletion hangs: "+why+". Nothing was changed.", "prerequisites")
	}
	obj := v.newDefaultObject()
	text, err := yaml.Marshal(obj)
	if err != nil {
		return p.refuse("Cannot build the new DataScienceCluster: "+err.Error(), "internal")
	}
	p.plan.NewObject = string(text)
	newSpec, _ := obj["spec"].(map[string]interface{})
	newComps, _ := newSpec["components"].(map[string]interface{})
	p.plan.Changes = v.changesBetween(v.components(), newComps)
	for _, t := range v.tree {
		if len(t.Path) == 1 && v.componentEnabled(v.components(), t) {
			p.plan.TornDown = append(p.plan.TornDown, t.key())
		}
		if t.Toggle && v.deployed(v.components(), t.Path) && !v.deployed(newComps, t.Path) {
			p.plan.StayRemoved = append(p.plan.StayRemoved, t.key())
		}
	}
	p.plan.Blocked, p.plan.Warnings = v.removalBlocks(c, v.components(), map[string]interface{}{}, true)
	p.plan.Token = planToken("hard-reset", v.uid, v.apiVersion, obj)
	return p
}

// hardResetStep emits one step of the hard reset.
type hardResetRun struct {
	emit func(UpdateStepEvent)
	logs []string
}

func (r *hardResetRun) step(step, status, msg string) {
	r.logs = append(r.logs, msg)
	r.emit(UpdateStepEvent{Step: step, Status: status, Message: msg})
}

func (r *hardResetRun) fail(step, msg, code string) *types.OperationResponse {
	r.step(step, "failed", msg)
	return &types.OperationResponse{Success: false, Message: msg, Logs: r.logs, ErrorCode: code}
}

// dscDeleteBody is the DeleteOptions of the hard reset: the DSC is deleted
// only if it is still the one the user reviewed (its UID).
func dscDeleteBody(uid string) []byte {
	body, _ := json.Marshal(map[string]interface{}{"apiVersion": "v1", "kind": "DeleteOptions", "preconditions": map[string]interface{}{"uid": uid}})
	return body
}

// HardResetDSC deletes the DataScienceCluster the user reviewed and creates
// the release default in its place. Steps: check, delete, wait_deleted,
// create, wait_ready. It stops without creating anything when the old DSC
// is not gone by HardResetDeleteTimeout; it never removes a finalizer.
func HardResetDSC(c *Client, req types.DSCPlanRequest, emit func(UpdateStepEvent)) (*types.OperationResponse, error) {
	r := &hardResetRun{emit: emit}
	r.step("check", "running", "Checking the DataScienceCluster, the operator and every component")
	v, err := readDSCView(c)
	if err != nil {
		return r.fail("check", "Cannot read the DataScienceCluster: "+err.Error()+". Nothing was changed.", errorCodeFromK8sErr(err)), nil
	}
	p := v.planHardReset(c)
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
	if _, _, err := c.do(http.MethodDelete, path, "application/json", dscDeleteBody(v.uid), url.Values{"dryRun": {"All"}}); err != nil {
		return r.fail("check", "The API server refused deleting the DataScienceCluster in its dry run: "+conversionMessage(err)+". Nothing was changed.", nonEmpty(errorCodeFromK8sErr(err), "validation")), nil
	}
	r.step("check", "success", fmt.Sprintf("DataScienceCluster %s can be reset: %d components are torn down and the release default of operator %s is created", v.name, len(p.plan.TornDown), v.op.Version))

	obj := v.newDefaultObject()
	createYAML := p.plan.NewObject
	recreateHelp := fmt.Sprintf("Recreate it with Create DataScienceCluster on the Components page (it creates the release default named default-dsc), or with: oc apply -f - with this object:\n%s", createYAML)

	r.step("delete", "running", "Deleting DataScienceCluster "+v.name)
	if _, _, err := c.do(http.MethodDelete, path, "application/json", dscDeleteBody(v.uid), nil); err != nil {
		if IsK8sError(err, http.StatusNotFound) || IsK8sError(err, http.StatusConflict) {
			return r.fail("delete", "DataScienceCluster "+v.name+" was deleted or replaced by someone else since the check, so nothing was changed by this reset. Reload the page.", "conflict"), nil
		}
		return r.fail("delete", "Deleting the DataScienceCluster failed: "+conversionMessage(err)+". Nothing was changed.", errorCodeFromK8sErr(err)), nil
	}
	recordDSCActivity(c, "hard-reset-dsc", v.name+": deleted (uid "+v.uid+") for a hard reset", true)
	r.step("delete", "success", "The operator is removing every component of DataScienceCluster "+v.name)

	r.step("wait_deleted", "running", "Waiting for the operator to clean up and remove the DataScienceCluster")
	if res := waitDSCGone(c, path, v, r); res != nil {
		recordDSCActivity(c, "hard-reset-dsc", v.name+": "+res.Message, false)
		return res, nil
	}

	r.step("create", "running", "Creating the release default DataScienceCluster "+v.name)
	body, _ := json.Marshal(obj)
	createPath := fmt.Sprintf(dscListFmt, v.version)
	q := url.Values{"fieldManager": {dscComponentsManager}, "fieldValidation": {"Strict"}}
	dry := url.Values{"fieldManager": {dscComponentsManager}, "fieldValidation": {"Strict"}, "dryRun": {"All"}}
	if _, _, err := c.do(http.MethodPost, createPath, "application/json", body, dry); err != nil {
		res := r.fail("create", "The old DataScienceCluster was deleted, but the API server refused the release default in its dry run: "+conversionMessage(err)+". "+recreateHelp, nonEmpty(errorCodeFromK8sErr(err), "validation"))
		recordDSCActivity(c, "hard-reset-dsc", v.name+": deleted, creating the default refused", false)
		return res, nil
	}
	if _, _, err := c.do(http.MethodPost, createPath, "application/json", body, q); err != nil {
		res := r.fail("create", "The old DataScienceCluster was deleted, but creating the release default failed: "+conversionMessage(err)+". "+recreateHelp, errorCodeFromK8sErr(err))
		recordDSCActivity(c, "hard-reset-dsc", v.name+": deleted, creating the default failed", false)
		return res, nil
	}
	recordDSCActivity(c, "hard-reset-dsc", v.name+": the release default of operator "+v.op.Version+" created", true)
	r.step("create", "success", "Created the release default DataScienceCluster "+v.name)

	r.step("wait_ready", "running", "Waiting for the components to be ready")
	msg := waitDSCReady(c, path, r)
	slog.Info("dsc hard reset", "dsc", v.name, "user", getUser(c))
	return &types.OperationResponse{Success: true, Message: msg, Logs: r.logs}, nil
}

// dscWaitState is what one poll of a DSC being deleted or provisioned sees.
type dscWaitState struct {
	found      bool
	uid        string
	finalizers []string
	conditions []types.DSCConditionView
}

func readDSCWaitState(c *Client, path string) (dscWaitState, error) {
	body, _, err := c.get(path)
	if IsK8sError(err, http.StatusNotFound) {
		return dscWaitState{}, nil
	}
	if err != nil {
		return dscWaitState{}, err
	}
	var obj map[string]interface{}
	if err := json.Unmarshal(body, &obj); err != nil {
		return dscWaitState{}, err
	}
	v := &dscView{obj: obj}
	var meta objectMeta
	if raw, err := json.Marshal(obj["metadata"]); err == nil {
		_ = json.Unmarshal(raw, &meta)
	}
	return dscWaitState{found: true, uid: meta.UID, finalizers: meta.Finalizers, conditions: v.conditions()}, nil
}

// waitDSCGone polls until the old DSC is gone. It returns the failed
// result when it is not gone in time, a DSC with another UID appears, or
// the read fails; nil once it is gone.
func waitDSCGone(c *Client, path string, v *dscView, r *hardResetRun) *types.OperationResponse {
	deadline := clk.Now().Add(HardResetDeleteTimeout)
	last := ""
	var st dscWaitState
	for {
		var err error
		st, err = readDSCWaitState(c, path)
		if err != nil {
			return r.fail("wait_deleted", "Cannot read the DataScienceCluster while it is deleted: "+err.Error()+". Nothing was created. Check it with oc get datasciencecluster "+v.name+" -o yaml, and create the release default with Create DataScienceCluster on the Components page once it is gone.", errorCodeFromK8sErr(err))
		}
		if !st.found {
			r.step("wait_deleted", "success", "DataScienceCluster "+v.name+" is gone")
			return nil
		}
		if st.uid != v.uid {
			return r.fail("wait_deleted", fmt.Sprintf("A new DataScienceCluster %s appeared (UID %s) before this reset created one: someone else, or the operator, created it. This reset creates nothing. Review it on the Components page.", v.name, st.uid), "conflict")
		}
		if msg := deletionProgress(st); msg != last {
			last = msg
			r.step("wait_deleted", "running", msg)
		}
		if !clk.Now().Before(deadline) {
			break
		}
		if err := clk.Sleep(c.ctx, HardResetPollInterval); err != nil {
			return r.fail("wait_deleted", "The reset was stopped while DataScienceCluster "+v.name+" was being deleted ("+err.Error()+"). Nothing was created; create the release default with Create DataScienceCluster on the Components page once it is gone.", "interrupted")
		}
	}
	return r.fail("wait_deleted", fmt.Sprintf("DataScienceCluster %s is still being deleted after %s: finalizer %s remains. The operator removes it once every module is cleaned up (%s). Nothing was created. "+
		"Do not remove the finalizer by hand: the operator would leave modules behind. Diagnostics (Platform modules) shows which module is stuck; once the DataScienceCluster is gone, create the release default with Create DataScienceCluster on the Components page.",
		v.name, HardResetDeleteTimeout, nonEmpty(strings.Join(st.finalizers, ", "), "none"), nonEmpty(modulesNote(st), "no module status reported")), "timeout")
}

// deletionProgress words a poll of the DSC being deleted.
func deletionProgress(st dscWaitState) string {
	msg := "Still deleting: finalizer " + nonEmpty(strings.Join(st.finalizers, ", "), "none")
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
			return "ModulesReady: " + truncate(cd.Message, 300)
		}
	}
	return ""
}

// waitDSCReady polls the new DSC until its Ready condition is True, and
// words what is still provisioning when HardResetReadyTimeout passes.
func waitDSCReady(c *Client, path string, r *hardResetRun) string {
	deadline := clk.Now().Add(HardResetReadyTimeout)
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
		if !clk.Now().Before(deadline) {
			break
		}
		if clk.Sleep(c.ctx, HardResetPollInterval) != nil {
			break
		}
	}
	msg := fmt.Sprintf("The DataScienceCluster was reset to the release default. It is not Ready yet after %s: %s. The operator keeps provisioning; this page shows each component as it comes up.", HardResetReadyTimeout, nonEmpty(last, "no status reported yet"))
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
			waiting = append(waiting, cd.Type+" ("+nonEmpty(cd.Reason, cd.Status)+")")
		}
	}
	return false, strings.Join(waiting, ", ")
}
