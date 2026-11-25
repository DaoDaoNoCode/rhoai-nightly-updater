package observability

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/argo"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/dashdev"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/serving"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The MaaS tab's usage logs, user IDs and portal.
//
// User IDs: MaasTenantConfig default-tenant's spec.telemetry.logs.captureUser
// (models-as-a-service rhoai-3.7-ea.1 docs/content/observability/setup.md
// L106-119; odh-observability docs/usage-logs.md L76-92); off by default, it
// makes the usage logs record each caller's user ID, which "My Usage" needs.
// The portal: DataScienceCluster spec.components.dashboard.maasPortal
// (rhods-operator rhoai-3.7-ea.1 api/components/v1alpha1/dashboard_types.go
// L32-33; odh-dashboard docs/maas-portal.md), at
// https://<the dashboard's gateway domain>/maas-portal/ (L36). This tool
// records what each was before on the object it changes (an annotation),
// changes it with a field manager of its own, and puts it back on Turn off
// and on MaaS's Clean up only while it is still what this tool set.
const (
	captureUserAnnotation = "rhoai-nightly-updater/capture-user-previous"
	captureUserManager    = "rhoai-nightly-updater-capture-user"
	portalAnnotation      = "rhoai-nightly-updater/maas-portal-previous"
	portalManager         = "rhoai-nightly-updater-maas-portal"
	unsetValue            = "unset"
	componentsGroup       = "components.platform.opendatahub.io"
)

// Step IDs of the MaaS tab's own actions.
const (
	StepCaptureUser = "capture-user"
	StepPortal      = "portal"
)

// --- reads --------------------------------------------------------------------------

type tenant struct {
	Found       bool
	Meta        kube.ObjectMeta
	CaptureUser *bool
	Conditions  []kube.Condition
}

func readTenant(c *kube.Client) (tenant, error) {
	var obj struct {
		Metadata kube.ObjectMeta `json:"metadata"`
		Spec     struct {
			Telemetry struct {
				Logs struct {
					CaptureUser *bool `json:"captureUser"`
				} `json:"logs"`
			} `json:"telemetry"`
		} `json:"spec"`
		Status struct {
			Conditions []kube.Condition `json:"conditions"`
		} `json:"status"`
	}
	found, err := c.GetIntoOptional(rhoai.NamespacedPath(serving.MaaSAPI(c), "maastenantconfigs", "models-as-a-service", "default-tenant"), &obj)
	if err != nil || !found {
		return tenant{}, err
	}
	return tenant{Found: true, Meta: obj.Metadata, CaptureUser: obj.Spec.Telemetry.Logs.CaptureUser, Conditions: obj.Status.Conditions}, nil
}

// portalKey is the DataScienceCluster's key of the portal under
// spec.components.dashboard: maasPortal, or maasConsumerPortal (an earlier
// API's spelling, dashboard_types.go L38-40); found false when this RHOAI
// has neither, and reports no MaaSPortalAvailable.
func portalKey(read rhoai.DSCRead) (key, state string, found bool) {
	spec, _ := read.Object["spec"].(map[string]any)
	comps, _ := spec["components"].(map[string]any)
	dash, _ := comps["dashboard"].(map[string]any)
	for _, k := range []string{"maasPortal", "maasConsumerPortal"} {
		if p, ok := dash[k].(map[string]any); ok {
			state, _ = p["managementState"].(string)
			return k, state, true
		}
	}
	if s, _ := serving.DSCConditionStatus(read.Object, "MaaSPortalAvailable"); s != "" {
		// Not written yet: the key of the version read (v2 calls it
		// maasConsumerPortal, dashboard_types.go L38-40).
		if read.Version == "v2" {
			return "maasConsumerPortal", "", true
		}
		return "maasPortal", "", true
	}
	return "", "", false
}

// portalURL is the portal's address from the Dashboard CR: its
// status.maasPortalUrl when the dashboard operator published one, else
// https://<spec.gateway.domain>/maas-portal/ (odh-dashboard
// docs/maas-portal.md L36), with published false; "" when neither is there.
func portalURL(c *kube.Client) (url string, published bool) {
	var items []struct {
		Spec struct {
			Gateway struct {
				Domain string `json:"domain"`
			} `json:"gateway"`
		} `json:"spec"`
		Status struct {
			MaaSPortalURL string `json:"maasPortalUrl"`
		} `json:"status"`
	}
	api := componentsGroup + "/" + rhoai.ServedVersions(c, componentsGroup, []string{"v1alpha1"}).Versions[0]
	if err := c.ListInto(rhoai.ClusterPath(api, "dashboards", ""), &items); err != nil || len(items) == 0 {
		return "", false
	}
	if u := items[0].Status.MaaSPortalURL; u != "" {
		return u, true
	}
	if d := items[0].Spec.Gateway.Domain; d != "" {
		return "https://" + d + "/maas-portal/", false
	}
	return "", false
}

// GetMaaSStatus is serving.GetMaaSStatus with the usage logs and the
// portal.
func GetMaaSStatus(c *kube.Client) (*types.MaaSStatus, error) {
	st, err := serving.GetMaaSStatus(c)
	if err != nil || st == nil {
		return st, err
	}
	st.UsageLogs = usageLogsState(c)
	st.Portal = portalState(c)
	return st, nil
}

func usageLogsState(c *kube.Client) *types.MaaSUsageLogs {
	out := &types.MaaSUsageLogs{}
	d, found, err := readDSCI(c)
	switch {
	case err != nil:
		out.State, out.Message = "unknown", err.Error()
		return out
	case !found:
		out.State, out.Message = "unknown", "This cluster has no DSCInitialization."
		return out
	}
	// What the Observability tab's Set up would refuse, all of it (the
	// operators, monitoring Managed, the S3 storage, the default
	// StorageClass, usage logs in this RHOAI).
	if v := read(c); v.refusal != "" {
		out.SetUpBlockedReason = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(v.refusal), "Nothing was changed."))
	}
	if t, err := readTenant(c); err == nil && t.Found {
		out.TenantFound, out.CaptureUser = true, t.CaptureUser
		if prev, ok := t.Meta.Annotations[captureUserAnnotation]; ok {
			out.CaptureUserByTool, out.CaptureUserPrevious = true, prev
		}
		if cond, ok := kube.FindCondition(t.Conditions, "Degraded"); ok && cond.Status == "True" && strings.Contains(cond.Message, "UsageLogs") {
			out.Warning = "The default tenant reports: " + kube.TruncateText(cond.Message, 300)
		}
	}
	if d.field(fieldUsageLogs) == nil {
		out.State, out.Message = "missing", "Not set up: MaaS calls are not logged, and the usage dashboards (RHOAI dashboard and MaaS portal) have no data. The Observability tab sets them up, as does MaaS's Set up with Also set up usage logs."
		return out
	}
	mon, err := readMonitoring(c)
	if err != nil {
		out.State, out.Message = "unknown", err.Error()
		return out
	}
	if waiting := notAvailable(mon, []string{"UsageLogsLokiStackAvailable", "UsageLogsCollectorAvailable", "PersesAvailable"}); len(waiting) > 0 {
		out.State, out.Message = "waiting", "Set up; RHOAI reports: "+strings.Join(waiting, "; ")+". Dashboards MaaS made before Perses ran show up once the Perses operator retries them, by itself."
		return out
	}
	out.State, out.Message = "ready", "Each call through the MaaS gateway is logged; the Usage dashboard (RHOAI dashboard, Observe & monitor) shows them to cluster-admins, and My Usage shows each user their own calls when user IDs are recorded."
	return out
}

// dashDevPausedOperator is the dashboard operator (namespace/name) while a
// recorded Dashboard Dev session keeps it at 0 replicas (dashdev
// OverrideSummary: SessionRecorded and OperatorPaused), else "". An
// operator paused outside Dashboard Dev, a session whose operator runs, or
// a read error is not that evidence. Tests replace it
// (SetDashDevPausedForTest).
var dashDevPausedOperator = func(c *kube.Client) string {
	o, err := dashdev.OverrideSummary(c)
	if err != nil || o == nil || !o.SessionRecorded || !o.OperatorPaused {
		return ""
	}
	return o.OperatorNamespace + "/" + o.OperatorName
}

// portalPausedNote says that the portal waits for Dashboard Dev, or "".
// The dashboard operator deploys the portal's operand when the Dashboard's
// spec.maasPortal is Managed (odh-dashboard rhoai-3.6
// docs/dashboard-operator.md L161-165), and a Dashboard Dev session keeps
// that operator at 0 replicas (dashdev; operator.OperationOptions), so the
// portal comes up only once the session is reverted. Turning the portal on
// is still allowed: nothing is left stuck. The legacy flow (no module
// operator) has no such operator to pause. Callers add it only while the
// portal is not available.
func portalPausedNote(c *kube.Client) string {
	operator := dashDevPausedOperator(c)
	if operator == "" {
		return ""
	}
	return fmt.Sprintf("Dashboard Dev keeps the dashboard operator %s at 0 replicas, and that operator deploys the portal: it comes up once Dashboard Dev is reverted (Dashboard Dev page).", operator)
}

func portalState(c *kube.Client) *types.MaaSPortal {
	p, _ := portalRead(c)
	return p
}

// portalRead is portalState that tells a DataScienceCluster that cannot be
// read (an error) from one without a portal (nil, nil).
func portalRead(c *kube.Client) (*types.MaaSPortal, error) {
	read, err := rhoai.ReadFirstDSC(c)
	if err != nil {
		return nil, err
	}
	if read.Object == nil {
		return nil, nil
	}
	_, state, found := portalKey(read)
	if !found {
		return nil, nil
	}
	p := &types.MaaSPortal{ManagementState: state, On: state == "Managed"}
	p.Available, p.Message = serving.DSCConditionStatus(read.Object, "MaaSPortalAvailable")
	meta, _ := read.Object["metadata"].(map[string]any)
	annotations, _ := meta["annotations"].(map[string]any)
	if prev, ok := annotations[portalAnnotation].(string); ok {
		p.SetByTool, p.Previous = true, prev
	}
	// The portal is independent of the core dashboard's managementState
	// (odh-dashboard docs/maas-portal.md L11, L18); only Argo CD tracking
	// the DataScienceCluster keeps this tool from changing it.
	if tr := argo.TrackingOf(c, read.Object, rhoai.DSCGroup, "DataScienceCluster"); tr.ManagedBy() != "" {
		p.BlockedReason = argo.GitOpsRefusal("The DataScienceCluster", tr.ManagedBy(), "changing the MaaS portal") + tr.UnconfirmedNote()
	}
	if p.On {
		var published bool
		p.URL, published = portalURL(c)
		if p.URL != "" && !published {
			p.Message = strings.TrimSpace(p.Message + " The dashboard operator has not published the portal's address yet; this is where it will be.")
		}
		if note := portalPausedNote(c); note != "" && p.Available != "True" {
			p.Message = strings.TrimSpace(p.Message + " " + note)
		}
	}
	return p, nil
}

// fieldSetBySomeone: a manager wrote the field at path (its managedFields
// name it); a value only the API server's defaulting filled in has none.
func fieldSetBySomeone(m kube.ObjectMeta, path ...string) bool {
	var managers []string
	for _, e := range m.ManagedFields {
		managers = append(managers, e.Manager)
	}
	return m.OwnsFieldPath(managers, path...)
}

// dscMeta is a DataScienceCluster's metadata as read.
func dscMeta(obj map[string]any) kube.ObjectMeta {
	raw, _ := json.Marshal(obj["metadata"])
	var m kube.ObjectMeta
	_ = json.Unmarshal(raw, &m)
	return m
}

// --- actions ------------------------------------------------------------------------

// ValidateMaaSRequest is serving.ValidateMaaSRequest with the usage logs'
// and the portal's actions and fields.
func ValidateMaaSRequest(req types.MaaSRequest) string {
	switch req.Action {
	case "capture-user-on", "capture-user-off", "portal-on", "portal-off":
		if req.Project != "" || req.Model != "" || req.Database != nil || req.Access != nil || req.Limit != nil || req.ConfirmKeysLost || req.UsageLogs || req.CaptureUser {
			return req.Action + " takes no field"
		}
		return ""
	}
	if (req.UsageLogs || req.CaptureUser) && req.Action != "setup" {
		return req.Action + " takes no usage logs choice"
	}
	return serving.ValidateMaaSRequest(req)
}

// RunMaaS is serving.RunMaaS, with: the usage logs set up first when a
// setup asks for them, user IDs recorded after it when it asks for them,
// the actions of the usage logs' and the portal's rows, and, on teardown,
// the user IDs setting and the portal put back first.
func RunMaaS(c *kube.Client, req types.MaaSRequest, onStep func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
	m := &serving.Run{Client: c, Emit: onStep, Activity: req.Action + "-maas", Target: "MaaS platform"}
	if problem := ValidateMaaSRequest(req); problem != "" {
		return m.Fail("check", problem, "validation", "invalid request")
	}
	switch req.Action {
	case "capture-user-on", "capture-user-off":
		m.Target = "MaaS user IDs in the usage logs"
		if r, err := setCaptureUser(m, req.Action == "capture-user-on"); r != nil || err != nil {
			return r, err
		}
		return m.Done(map[bool]string{true: "MaaS records user IDs in the usage logs from now on.", false: "The user IDs setting is as before this tool turned it on."}[req.Action == "capture-user-on"])
	case "portal-on", "portal-off":
		m.Target = "the MaaS portal"
		if r, err := setPortal(m, req.Action == "portal-on"); r != nil || err != nil {
			return r, err
		}
		return m.Done(map[bool]string{true: "The MaaS portal is turned on; RHOAI's dashboard operator deploys it.", false: "The MaaS portal is as before this tool turned it on."}[req.Action == "portal-on"])
	case "teardown":
		res, err := serving.RunMaaS(c, req, onStep)
		return afterMaaSTeardown(c, res, err)
	case "setup":
		return runMaaSSetup(m, req, onStep)
	}
	return serving.RunMaaS(c, req, onStep)
}

// runMaaSSetup sets up the usage logs first (the Observability tab's Set
// up, its own steps) when req asks for them, then MaaS, then records user
// IDs when req asks for it.
func runMaaSSetup(m *serving.Run, req types.MaaSRequest, onStep func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
	c := m.Client
	usage, capture := req.UsageLogs, req.CaptureUser
	req.UsageLogs, req.CaptureUser = false, false
	if usage {
		obs := &serving.Run{Client: c, Emit: onStep, Activity: "setup-observability", Target: "usage logs and dashboards"}
		if r, err := SetUp(obs); r != nil || err != nil {
			if r != nil {
				r.Message += " MaaS was not set up."
			}
			return r, err
		}
		m.Logs = append(m.Logs, obs.Logs...)
	}
	res, err := serving.RunMaaS(c, req, onStep)
	if err != nil || res == nil || !res.Success || !capture {
		return res, err
	}
	logs := append(append([]string{}, m.Logs...), res.Logs...)
	m.Logs = logs
	if r, err := setCaptureUser(m, true); r != nil || err != nil {
		if r != nil {
			r.Message = "MaaS is set up, but user IDs are not recorded: " + r.Message
		}
		return r, err
	}
	res.Message += " MaaS records user IDs in the usage logs."
	res.Logs = m.Logs
	return res, nil
}

// patchMerge merge-patches path as manager, strictly validated.
func patchMerge(c *kube.Client, path, manager string, body map[string]any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	_, _, err = c.Do(http.MethodPatch, path, "application/merge-patch+json", data, url.Values{"fieldManager": {manager}, "fieldValidation": {"Strict"}})
	return err
}

// setCaptureUser turns the default tenant's captureUser on (recording its
// value before), or back to that value (only when this tool turned it on).
func setCaptureUser(m *serving.Run, on bool) (*types.OperationResponse, error) {
	c := m.Client
	m.Step(StepCaptureUser, "running", map[bool]string{true: "Turning on user IDs in the usage logs (MaasTenantConfig default-tenant)", false: "Putting the user IDs setting back"}[on])
	t, err := readTenant(c)
	if err != nil {
		return m.Fail(StepCaptureUser, fmt.Sprintf("Cannot read MaasTenantConfig models-as-a-service/default-tenant: %v.", err), kube.ErrorCodeFromK8sErr(err), "tenant read failed")
	}
	if !t.Found {
		return m.Fail(StepCaptureUser, "MaaS's default tenant (MaasTenantConfig models-as-a-service/default-tenant) does not exist: set up MaaS first. Nothing was changed.", "prerequisites", "no tenant")
	}
	path := rhoai.NamespacedPath(serving.MaaSAPI(c), "maastenantconfigs", "models-as-a-service", "default-tenant")
	prev, byTool := t.Meta.Annotations[captureUserAnnotation]
	current := t.CaptureUser != nil && *t.CaptureUser
	meta := map[string]any{"resourceVersion": t.Meta.ResourceVersion}
	var body map[string]any
	switch {
	case on && current:
		m.Step(StepCaptureUser, "success", "MaaS records user IDs already.")
		return nil, nil
	case on:
		// "unset" unless someone wrote the field: a false the API server
		// filled in from the CRD's default is not a value anyone set, and
		// putting it back means removing the field.
		was := unsetValue
		if t.CaptureUser != nil && fieldSetBySomeone(t.Meta, "f:spec", "f:telemetry", "f:logs", "f:captureUser") {
			was = "false"
		}
		meta["annotations"] = map[string]any{captureUserAnnotation: was}
		body = map[string]any{"metadata": meta, "spec": map[string]any{"telemetry": map[string]any{"logs": map[string]any{"captureUser": true}}}}
	case !byTool:
		return m.Fail(StepCaptureUser, "This tool did not turn user IDs on, so it does not turn them off: whoever set captureUser changes it. Nothing was changed.", "not_managed", "not set by this tool")
	case !current:
		// Changed by someone since: their value stays; only the record goes.
		meta["annotations"] = map[string]any{captureUserAnnotation: nil}
		body = map[string]any{"metadata": meta}
	default:
		var value any
		if prev == "false" {
			value = false
		}
		meta["annotations"] = map[string]any{captureUserAnnotation: nil}
		body = map[string]any{"metadata": meta, "spec": map[string]any{"telemetry": map[string]any{"logs": map[string]any{"captureUser": value}}}}
	}
	if err := patchMerge(c, path, captureUserManager, body); err != nil {
		return m.Fail(StepCaptureUser, fmt.Sprintf("Failed to change MaasTenantConfig models-as-a-service/default-tenant: %v. Nothing was changed.%s", err, kube.TemplateHint(err)), serving.MaaSWriteCode(err), "tenant patch failed")
	}
	m.Logs = append(m.Logs, "OK: MaasTenantConfig models-as-a-service/default-tenant spec.telemetry.logs.captureUser changed")
	m.Step(StepCaptureUser, "success", map[bool]string{true: "MaaS records user IDs in the usage logs.", false: "The user IDs setting is as before."}[on])
	return nil, nil
}

// setPortal turns the MaaS portal on (recording the state before), or back
// to that state (only when this tool turned it on).
func setPortal(m *serving.Run, on bool) (*types.OperationResponse, error) {
	c := m.Client
	m.Step(StepPortal, "running", map[bool]string{true: "Turning the MaaS portal on in the DataScienceCluster", false: "Putting the MaaS portal back"}[on])
	read, err := rhoai.ReadFirstDSC(c)
	if err != nil {
		return m.Fail(StepPortal, fmt.Sprintf("Cannot read the DataScienceCluster: %v.", err), kube.ErrorCodeFromK8sErr(err), "dsc read failed")
	}
	if read.Object == nil {
		return m.Fail(StepPortal, "This cluster has no DataScienceCluster. Nothing was changed.", "prerequisites", "no dsc")
	}
	key, state, found := portalKey(read)
	if !found {
		return m.Fail(StepPortal, "This RHOAI's DataScienceCluster has no MaaS portal (spec.components.dashboard.maasPortal). Nothing was changed.", "unsupported", "no portal")
	}
	if tr := argo.TrackingOf(c, read.Object, rhoai.DSCGroup, "DataScienceCluster"); tr.ManagedBy() != "" {
		return m.Fail(StepPortal, argo.GitOpsRefusal("The DataScienceCluster", tr.ManagedBy(), "changing the MaaS portal")+tr.UnconfirmedNote()+" Nothing was changed.", kube.ErrorCodeGitOpsManaged, "gitops")
	}
	md, _ := read.Object["metadata"].(map[string]any)
	annotations, _ := md["annotations"].(map[string]any)
	prev, byTool := annotations[portalAnnotation].(string)
	rv, _ := md["resourceVersion"].(string)
	meta := map[string]any{"resourceVersion": rv}
	var body map[string]any
	switch {
	case on && state == "Managed":
		note := ""
		if available, _ := serving.DSCConditionStatus(read.Object, "MaaSPortalAvailable"); available != "True" {
			note = portalPausedNote(c)
		}
		m.Step(StepPortal, "success", strings.TrimSpace("The MaaS portal is on already. "+note))
		return nil, nil
	case on:
		was := unsetValue
		if state != "" && fieldSetBySomeone(dscMeta(read.Object), "f:spec", "f:components", "f:dashboard", "f:"+key, "f:managementState") {
			was = state
		}
		meta["annotations"] = map[string]any{portalAnnotation: was}
		body = map[string]any{"metadata": meta, "spec": map[string]any{"components": map[string]any{"dashboard": map[string]any{key: map[string]any{"managementState": "Managed"}}}}}
	case !byTool:
		return m.Fail(StepPortal, "This tool did not turn the MaaS portal on, so it does not turn it off: whoever turned it on changes it in the DataScienceCluster. Nothing was changed.", "not_managed", "not set by this tool")
	case state != "Managed":
		meta["annotations"] = map[string]any{portalAnnotation: nil}
		body = map[string]any{"metadata": meta}
	default:
		var value any = prev
		if prev == unsetValue {
			value = nil
		}
		meta["annotations"] = map[string]any{portalAnnotation: nil}
		body = map[string]any{"metadata": meta, "spec": map[string]any{"components": map[string]any{"dashboard": map[string]any{key: map[string]any{"managementState": value}}}}}
	}
	path, err := rhoai.DataScienceClusterPath(c)
	if err == nil {
		err = patchMerge(c, path, portalManager, body)
	}
	if err != nil {
		msg := fmt.Sprintf("Failed to change the DataScienceCluster: %v. Nothing was changed.%s", err, kube.TemplateHint(err))
		if kube.IsK8sConflict(err) {
			msg = "The DataScienceCluster changed while this was read; nothing was changed. Try again."
		}
		return m.Fail(StepPortal, msg, serving.MaaSWriteCode(err), "dsc patch failed")
	}
	m.Logs = append(m.Logs, "OK: DataScienceCluster dashboard."+key+" changed")
	done := map[bool]string{true: "The MaaS portal is turned on.", false: "The MaaS portal is as before."}[on]
	if on {
		done = strings.TrimSpace(done + " " + portalPausedNote(c))
	}
	m.Step(StepPortal, "success", done)
	return nil, nil
}

// restoreMaaSExtras puts the user IDs setting and the portal back when this
// tool turned them on. It returns what it did, and what it could not do
// (a reason, "" when everything is as before).
func restoreMaaSExtras(c *kube.Client) (logs []string, problem string) {
	var problems []string
	if t, err := readTenant(c); err != nil {
		problems = append(problems, "Cannot read MaasTenantConfig default-tenant to put the user IDs setting back: "+err.Error()+".")
	} else if t.Found {
		if _, ok := t.Meta.Annotations[captureUserAnnotation]; ok {
			q := &serving.Run{Client: c, Activity: "capture-user-off-maas", Target: "MaaS user IDs in the usage logs"}
			if r, _ := setCaptureUser(q, false); r != nil {
				problems = append(problems, "The user IDs setting was not put back: "+r.Message)
			} else {
				logs = append(logs, "OK: user IDs setting as before")
			}
		}
	}
	if p, err := portalRead(c); err != nil {
		problems = append(problems, "Cannot read the DataScienceCluster to put the MaaS portal back: "+err.Error()+".")
	} else if p != nil && p.SetByTool {
		q := &serving.Run{Client: c, Activity: "portal-off-maas", Target: "the MaaS portal"}
		if r, _ := setPortal(q, false); r != nil {
			problems = append(problems, "The MaaS portal was not put back: "+r.Message)
		} else {
			logs = append(logs, "OK: MaaS portal as before")
		}
	}
	return logs, strings.Join(problems, " ")
}

// afterMaaSTeardown puts the user IDs setting and the portal back once the
// MaaS teardown succeeded (a teardown that refuses changes neither), and
// makes the result fail, retryably, when that could not be done.
func afterMaaSTeardown(c *kube.Client, res *types.OperationResponse, err error) (*types.OperationResponse, error) {
	if err != nil || res == nil || !res.Success {
		return res, err
	}
	logs, problem := restoreMaaSExtras(c)
	res.Logs = append(res.Logs, logs...)
	if problem != "" {
		res.Success, res.ErrorCode = false, "partial_failure"
		res.Message += " " + problem + " Clean up MaaS again to finish."
	}
	return res, nil
}

// TeardownMaaSPlatform is serving.TeardownMaaSPlatform, then the user IDs
// setting and the portal put back (Clean up everything).
func TeardownMaaSPlatform(c *kube.Client) (*types.OperationResponse, error) {
	res, err := serving.TeardownMaaSPlatform(c)
	return afterMaaSTeardown(c, res, err)
}
