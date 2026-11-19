package cluster

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// maasRun is one streamed MaaS operation.
type maasRun struct {
	c        *Client
	emit     func(UpdateStepEvent)
	logs     []string
	activity string
	target   string
	// newModel (Models routing): publish a model the Models tab just set
	// up to be reached through MaaS only. Its server runs, but it is not
	// Ready until published: its route needs the MaaS gateway to admit the
	// project, which publish does. Publish waits for Ready on its route.
	newModel bool
}

func (m *maasRun) step(id, status, msg string) {
	m.logs = append(m.logs, msg)
	if m.emit != nil {
		m.emit(UpdateStepEvent{Step: id, Status: status, Message: msg})
	}
}

func (m *maasRun) fail(id, msg, code, activity string) (*types.OperationResponse, error) {
	if m.emit != nil {
		m.emit(UpdateStepEvent{Step: id, Status: "failed", Message: msg, ErrorCode: code})
	}
	m.logs = append(m.logs, msg)
	slog.Warn("maas operation stopped", "step", id, "reason", activity, "user", getUser(m.c))
	recordModelsActivity(m.c, m.activity, fmt.Sprintf("%s (%s)", m.target, activity), false)
	return &types.OperationResponse{Success: false, Message: msg, Logs: m.logs, ErrorCode: code}, nil
}

func (m *maasRun) done(msg string) (*types.OperationResponse, error) {
	slog.Info("maas operation", "action", m.activity, "target", m.target, "user", getUser(m.c))
	recordModelsActivity(m.c, m.activity, m.target, true)
	return &types.OperationResponse{Success: true, Message: msg, Logs: m.logs}, nil
}

// maasWriteCode turns a write error into a refusal code.
func maasWriteCode(err error) string {
	var foreign *foreignObjectError
	switch {
	case errors.As(err, &foreign):
		return "not_managed"
	case errors.Is(err, errObjectChanged) || IsK8sError(err, 409):
		return "conflict"
	}
	return errorCodeFromK8sErr(err)
}

// ValidateMaaSRequest returns why req cannot run, or "".
func ValidateMaaSRequest(req types.MaaSRequest) string {
	// Which of the optional fields each action takes.
	var takesDB, takesAccess, takesLimit bool
	switch req.Action {
	case "setup", "teardown", "upgrade-rhcl", "rhcl-memory", "rhcl-deny-on-failure", "authorino-tls", "remove-tier-policies", "change-access", "change-database":
		if req.Project != "" {
			return "the platform takes no project"
		}
		if req.Model != "" {
			return "the platform takes no model"
		}
		takesDB = req.Action == "setup" || req.Action == "change-database"
		takesAccess = req.Action == "change-access"
	case "publish", "unpublish", "change-limit":
		if !playgroundProjectRegex.MatchString(req.Project) {
			return "a valid project is required"
		}
		entry, ok := catalogEntry(req.Model)
		if !ok {
			return "unknown model"
		}
		if entry.Type != modelTypeChat {
			return "only chat models are published to MaaS"
		}
		takesAccess = req.Action == "publish"
		takesLimit = req.Action == "publish" || req.Action == "change-limit"
	default:
		return "action must be setup, teardown, publish, unpublish, change-limit, change-access, change-database, upgrade-rhcl, rhcl-memory, rhcl-deny-on-failure, authorino-tls or remove-tier-policies"
	}
	switch {
	case req.Database != nil && !takesDB:
		return req.Action + " takes no database"
	case req.Access != nil && !takesAccess:
		return req.Action + " takes no audience"
	case req.Limit != nil && !takesLimit:
		return req.Action + " takes no limit"
	case req.ConfirmKeysLost && req.Action != "change-database":
		return req.Action + " takes no confirmation"
	}
	switch req.Action {
	case "change-database":
		if req.Database == nil {
			return "choose the database to change to"
		}
		if !req.ConfirmKeysLost {
			return "changing the database loses every API key: confirm it"
		}
	case "change-access":
		if req.Access == nil {
			return "say who can use the published models"
		}
	case "change-limit":
		if req.Limit == nil {
			return "give the model's new limit"
		}
	}
	if req.Database != nil {
		if problem := validateMaaSDatabaseRequest(*req.Database); problem != "" {
			return problem
		}
	}
	if _, problem := parseMaaSAudience(req.Access); req.Access != nil && problem != "" {
		return problem
	}
	if _, problem := parseMaaSLimit(req.Limit); problem != "" {
		return problem
	}
	return ""
}

// RunMaaS sets up or removes the MaaS platform, or publishes or unpublishes
// this tool's model in a project, as one operation with streamed steps:
// setup "check", "database", "gateway", "authorino-tls", "dsc", "ready";
// teardown "check", "unpublish", "dsc", "gateway", "database"; publish
// "check", "gateway", "model", "maas", "ready"; unpublish "check", "maas",
// "model", "gateway".
func RunMaaS(c *Client, req types.MaaSRequest, onStep func(UpdateStepEvent)) (*types.OperationResponse, error) {
	m := &maasRun{c: c, emit: onStep, activity: req.Action + "-maas", target: "MaaS platform"}
	entry, _ := catalogEntry(req.Model)
	if req.Project != "" {
		m.target = fmt.Sprintf("model %s in %s", firstNonEmpty(entry.Name, req.Model), req.Project)
	}
	if problem := ValidateMaaSRequest(req); problem != "" {
		return m.fail("check", problem, "validation", "invalid request")
	}
	switch req.Action {
	case "setup":
		return m.setup(req.Database)
	case "teardown":
		return m.teardown()
	case "upgrade-rhcl":
		m.target = "Connectivity Link"
		return m.upgradeRHCL()
	case "rhcl-memory":
		m.target = "Connectivity Link's memory limit"
		return m.rhclMemory()
	case "rhcl-deny-on-failure":
		m.target = "Connectivity Link's rate-limit failure mode"
		return m.rhclDenyOnFailure()
	case "authorino-tls":
		m.target = "Authorino TLS"
		return m.authorinoTLSOnly()
	case "remove-tier-policies":
		m.target = "the old tier policies"
		return m.removeTierPolicies()
	case "publish":
		return m.publish(req.Project, entry.Name, req.Access, req.Limit)
	case "change-limit":
		return m.changeLimit(req.Project, entry.Name, req.Limit)
	case "change-access":
		m.target = "who can use the published MaaS models"
		return m.changeAccess(req.Access)
	case "change-database":
		m.target = "the MaaS database"
		return m.changeDatabase(req.Database)
	}
	return m.unpublish(req.Project, entry.Name, true)
}

// TeardownMaaSPlatform removes the MaaS platform this tool set up (Clean up
// everything).
func TeardownMaaSPlatform(c *Client) (*types.OperationResponse, error) {
	return RunMaaS(c, types.MaaSRequest{Action: "teardown"}, nil)
}

// UnpublishMaaSModel unpublishes every model this tool published in
// project (Clean up everything), one after the other; it stops at the
// first that fails.
func UnpublishMaaSModel(c *Client, project string) (*types.OperationResponse, error) {
	var logs, done []string
	for _, entry := range modelCatalog {
		if entry.Type != modelTypeChat {
			continue
		}
		published, err := MaaSPublishedIn(c, project, entry.Name)
		if err != nil {
			return &types.OperationResponse{Success: false, Message: fmt.Sprintf("Cannot check whether %s in '%s' is published: %v.", entry.Name, project, err), Logs: logs, ErrorCode: errorCodeFromK8sErr(err)}, nil
		}
		if !published {
			continue
		}
		res, _ := RunMaaS(c, types.MaaSRequest{Action: "unpublish", Project: project, Model: entry.Name}, nil)
		logs = append(logs, res.Logs...)
		if !res.Success {
			res.Logs = logs
			return res, nil
		}
		done = append(done, res.Message)
	}
	if len(done) == 0 {
		return &types.OperationResponse{Success: true, Message: fmt.Sprintf("No model of this tool is published in '%s'; nothing to undo.", project), Logs: logs}, nil
	}
	return &types.OperationResponse{Success: true, Message: strings.Join(done, " "), Logs: logs}, nil
}

// --- the gateway ----------------------------------------------------------------

// maasGatewayPlan is the gateway's objects as setup or a publication writes
// them, with what was read of each.
type maasGatewayPlan struct {
	objects map[string]map[string]interface{}
	current map[string]objectMeta
	found   map[string]bool
	// classMissing: GatewayClass openshift-default does not exist; setup
	// creates it (and nothing ever changes or deletes it).
	classMissing bool
	host         string
}

// planMaaSGateway reads the class and the gateway this tool writes, for the
// namespaces the gateway admits. A refusal is a message and a code.
func planMaaSGateway(c *Client, infraNS string, published []string) (*maasGatewayPlan, string, string) {
	plan := &maasGatewayPlan{current: map[string]objectMeta{}, found: map[string]bool{}}
	meta, ok, err := readObjectMeta(c, maasObjectPath("Gateway", ""))
	if err != nil {
		return nil, fmt.Sprintf("Cannot read %s; nothing was changed: %v.%s", maasDesc("Gateway", ""), err, templateHint(err)), errorCodeFromK8sErr(err)
	}
	plan.current["Gateway"], plan.found["Gateway"] = meta, ok
	cm, cmOK, err := readObjectMeta(c, maasObjectPath("ConfigMap", ""))
	if err != nil {
		return nil, fmt.Sprintf("Cannot read %s; nothing was changed: %v.%s", maasDesc("ConfigMap", ""), err, templateHint(err)), errorCodeFromK8sErr(err)
	}
	plan.current["ConfigMap"], plan.found["ConfigMap"] = cm, cmOK
	if ok && meta.terminating() {
		return nil, fmt.Sprintf("%s is still being deleted. Wait until it is gone, then try again.", maasDesc("Gateway", "")), "terminating"
	}
	// A gateway someone else made is used as it is, with whatever options
	// it names: the options ConfigMap matters only to the tool's own.
	if ok && !gatewayOwned(meta) {
		return nil, "", "foreign"
	}
	if cmOK && !toolOwned(cm) {
		return nil, fmt.Sprintf("%s exists and was not created by this tool. Nothing was changed.", maasDesc("ConfigMap", "")), "not_managed"
	}
	// setup-gateway.sh uses an existing openshift-default as it is; only
	// OpenShift's own controller's.
	body, _, err := c.get(maasGatewayClassPath())
	switch {
	case IsK8sError(err, 404):
		plan.classMissing = true
	case err != nil:
		return nil, fmt.Sprintf("Cannot read GatewayClass %s; nothing was changed: %v.", maasGatewayClass, err), errorCodeFromK8sErr(err)
	default:
		var gc struct {
			Spec struct {
				ControllerName string `json:"controllerName"`
			} `json:"spec"`
		}
		_ = json.Unmarshal(body, &gc)
		if gc.Spec.ControllerName != gatewayControllerName {
			return nil, fmt.Sprintf("GatewayClass %s exists and belongs to controller %q instead of OpenShift's (%s). Nothing was changed.", maasGatewayClass, gc.Spec.ControllerName, gatewayControllerName), "not_managed"
		}
	}
	domain, err := clusterAppsDomain(c)
	if err != nil {
		return nil, fmt.Sprintf("Cannot find the cluster's apps domain for maas.<domain>; nothing was changed: %v.", err), firstNonEmpty(errorCodeFromK8sErr(err), "prerequisites")
	}
	cert, err := defaultIngressCertificate(c)
	if err != nil {
		return nil, fmt.Sprintf("Cannot find the cluster's default ingress certificate; nothing was changed: %v.", err), errorCodeFromK8sErr(err)
	}
	plan.objects = maasGatewayObjects(domain, cert, maasGatewayNamespaces(infraNS, published))
	plan.host = maasHostPrefix + domain
	return plan, "", ""
}

func (m *maasRun) applyGateway(plan *maasGatewayPlan, dryRun bool) (string, string) {
	if plan.classMissing {
		// Create only (setup_gatewayclass); one created meanwhile is used.
		data, _ := json.Marshal(plan.objects["GatewayClass"])
		var query url.Values
		if dryRun {
			query = url.Values{"dryRun": {"All"}}
		}
		if _, _, err := m.c.do(http.MethodPost, withToolFieldManager(clusterPath("gateway.networking.k8s.io/v1", "gatewayclasses", "")), "application/json", data, query); err != nil && !IsK8sError(err, 409) {
			return fmt.Sprintf("Failed to create GatewayClass %s: %v.%s", maasGatewayClass, err, templateHint(err)), maasWriteCode(err)
		}
		if !dryRun {
			plan.classMissing = false
			m.logs = append(m.logs, "OK: GatewayClass "+maasGatewayClass+" created")
		}
	}
	for _, kind := range []string{"ConfigMap", "Gateway"} {
		k := kind
		read := func() (objectMeta, bool, error) { return readObjectMeta(m.c, maasObjectPath(k, "")) }
		owned := gatewayOwned
		if kind == "ConfigMap" {
			owned = toolOwned
		}
		action, meta, found, err := ensureToolObject(m.c, maasDesc(kind, ""), maasObjectPath(kind, ""), maasCreatePath(kind, ""), plan.objects[kind], plan.current[kind], plan.found[kind], read, owned, dryRun)
		plan.current[kind], plan.found[kind] = meta, found
		if err != nil {
			if dryRun {
				return fmt.Sprintf("The API server rejects %s: %v. Nothing was changed.%s", maasDesc(kind, ""), err, templateHint(err)), maasWriteCode(err)
			}
			return fmt.Sprintf("Failed to apply %s: %v.%s", maasDesc(kind, ""), err, templateHint(err)), maasWriteCode(err)
		}
		if !dryRun {
			m.logs = append(m.logs, fmt.Sprintf("OK: %s %s", maasDesc(kind, ""), action))
		}
	}
	return "", ""
}

// patchModelsAsAService sets aigateway.modelsAsAService.managementState as
// field manager rhoai-nightly-updater-maas-dsc: the API server then tells
// whether the value is still the tool's (dscMaaSOwned).
func patchModelsAsAService(c *Client, state string) error {
	path, err := dataScienceClusterPath(c)
	if err != nil {
		return err
	}
	patch := fmt.Sprintf(`{"spec":{"components":{"aigateway":{"modelsAsAService":{"managementState":%q}}}}}`, state)
	_, _, err = c.do(http.MethodPatch, path, "application/merge-patch+json", []byte(patch), url.Values{"fieldManager": {maasDSCManager}})
	return err
}

// dscMaaSOwned: the tool's field manager still owns
// aigateway.modelsAsAService.managementState, so nobody has set it since
// setup (the API server gives a field to whoever last set it).
func dscMaaSOwned(d maasDSC) bool {
	raw, _ := json.Marshal(d.read.Object["metadata"])
	var meta objectMeta
	_ = json.Unmarshal(raw, &meta)
	return meta.ownsFieldPath([]string{maasDSCManager}, "f:spec", "f:components", "f:aigateway", "f:modelsAsAService", "f:managementState")
}

// foreignMaaSObjects names the MaaSModelRefs (cluster-wide), and the
// MaaSSubscriptions and MaaSAuthPolicies (in models-as-a-service), that
// this tool did not create. An API that is not served lists nothing.
func foreignMaaSObjects(c *Client) ([]string, error) {
	var out []string
	for _, l := range []struct{ kind, path string }{
		{"MaaSModelRef", "/apis/" + maasAPI(c) + "/maasmodelrefs"},
		{"MaaSSubscription", namespacedPath(maasAPI(c), "maassubscriptions", maasTenantNamespace, "")},
		{"MaaSAuthPolicy", namespacedPath(maasAPI(c), "maasauthpolicies", maasTenantNamespace, "")},
	} {
		body, _, err := c.get(l.path)
		if IsK8sError(err, 404) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("list %ss: %w", l.kind, err)
		}
		var list struct {
			Items []struct {
				Metadata objectMeta `json:"metadata"`
			} `json:"items"`
		}
		if err := json.Unmarshal(body, &list); err != nil {
			return nil, fmt.Errorf("parse the %s list: %w", l.kind, err)
		}
		for _, it := range list.Items {
			if !toolOwned(it.Metadata) {
				out = append(out, fmt.Sprintf("%s %s/%s", l.kind, it.Metadata.Namespace, it.Metadata.Name))
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// maasControllerGone says why maas-controller cannot remove a MaaSModelRef's
// finalizer, or "" when it may: MaaS is not Managed in the
// DataScienceCluster (the operator then removes maas-controller), or its
// Deployment is gone from the applications namespace.
func maasControllerGone(c *Client) string {
	if dsc, err := readMaaSDSC(c); err == nil && dsc.read.Object != nil && dsc.maas != "Managed" {
		return "MaaS is " + firstNonEmpty(dsc.maas, "not set") + " in the DataScienceCluster"
	}
	if ns := appsNamespace(c); ns != "" && deploymentAvailable(c, ns, "maas-controller") == "missing" {
		return "Deployment " + ns + "/maas-controller does not exist"
	}
	return ""
}

// restartMaaSAPI rolls maas-api as `oc rollout restart` does, so it reads
// a changed maas-db-config (maas-setup.md "Restarting maas-api"). A
// missing Deployment needs nothing: the operator starts it with the Secret.
func restartMaaSAPI(c *Client, ns string) (bool, error) {
	patch, _ := json.Marshal(map[string]interface{}{"spec": map[string]interface{}{"template": map[string]interface{}{"metadata": map[string]interface{}{
		"annotations": map[string]string{restartAnnotation: clk.Now().UTC().Format(time.RFC3339)},
	}}}})
	_, _, err := c.patch(namespacedPath("apps/v1", "deployments", ns, "maas-api"), patch)
	if IsK8sError(err, 404) {
		return false, nil
	}
	return err == nil, err
}

// --- removal --------------------------------------------------------------------

func (m *maasRun) teardown() (*types.OperationResponse, error) {
	c := m.c
	m.step("check", "running", "Checking what this tool set up...")
	rec, err := readMaaSRecord(c)
	if err != nil {
		return m.fail("check", err.Error()+" Nothing was deleted.", errorCodeFromK8sErr(err), "record read failed")
	}
	dsc, err := readMaaSDSC(c)
	if err != nil {
		return m.fail("check", fmt.Sprintf("Cannot read the DataScienceCluster; nothing was deleted: %v.", err), errorCodeFromK8sErr(err), "dsc read failed")
	}
	infra := firstNonEmpty(rec.DBNamespace, maasInfraNamespace(c))
	type obj struct {
		kind  string
		meta  objectMeta
		found bool
	}
	var objs []obj
	owned := 0
	// The gateway and its options, then the database: maas-db-config and
	// every database object of this tool, whichever kind it set up (a
	// Change database that stopped half-way leaves two). The user's own
	// database has no object here; one this tool did not create is kept.
	for _, kind := range []string{"Gateway", "ConfigMap", "Secret", "Cluster", "PostgresDeployment", "PostgresService"} {
		meta, found, err := readObjectMeta(c, maasObjectPath(kind, infra))
		if err != nil && (kind != "Cluster" || !IsK8sError(err, 404)) {
			return m.fail("check", fmt.Sprintf("Cannot read %s; nothing was deleted: %v.", maasDesc(kind, infra), err), errorCodeFromK8sErr(err), "read failed")
		}
		objs = append(objs, obj{kind, meta, found})
		if found && toolOwned(meta) {
			owned++
		}
	}
	// postgres-creds is never read: the record says whether this tool made
	// one (deletePOCCreds deletes it by its UID).
	if rec.POCCredsUID != "" {
		owned++
	}
	if owned == 0 && !rec.DSCChanged && !rec.AIGatewayChanged && rec.InfraNamespace == "" && len(rec.Published) == 0 {
		m.step("check", "success", "Nothing of the MaaS platform was set up by this tool.")
		return m.done("The MaaS platform was not set up by this tool; nothing to remove.")
	}
	if rec.DSCChanged && dsc.maas == "Managed" && dsc.argoApp != "" {
		return m.fail("check", gitOpsRefusal("The DataScienceCluster", dsc.argoApp, "turning off Models as a Service")+" Nothing was deleted.", ErrorCodeGitOpsManaged, "dsc gitops")
	}
	if !rec.DSCChanged && dsc.maas == "Managed" && owned > 0 {
		return m.fail("check", "MaaS was turned on in the DataScienceCluster by someone else, and it uses the gateway and database this tool created: removing them would break it. Turn MaaS off first (aigateway.modelsAsAService Removed), then remove. Nothing was deleted.", "in_use", "maas on, not by tool")
	}
	if rec.DSCChanged && dsc.maas == "Managed" && !dscMaaSOwned(dsc) {
		return m.fail("check", fmt.Sprintf("aigateway.modelsAsAService in DataScienceCluster %[1]s was set after this tool's setup, by someone else or by an earlier version of this tool that did not mark its change, so this tool does not change it back, and the gateway and database MaaS uses stay. To remove the platform: if nobody else uses MaaS, set it to Removed yourself (oc patch datasciencecluster %[1]s --type=merge -p '{\"spec\":{\"components\":{\"aigateway\":{\"modelsAsAService\":{\"managementState\":\"Removed\"}}}}}'), wait until maas-api is gone, then Remove again: it deletes the gateway and database this tool created. Nothing was deleted.", dsc.name), "in_use", "dsc changed since setup")
	}
	// Others' use of MaaS: their model references, subscriptions and access
	// policies stop working without the platform (and turning MaaS off
	// takes maas-api and their keys away).
	if others, err := foreignMaaSObjects(c); err != nil {
		return m.fail("check", fmt.Sprintf("Cannot check whether others use MaaS, so nothing was deleted: %v.%s", err, templateHint(err)), errorCodeFromK8sErr(err), "maas list failed")
	} else if len(others) > 0 {
		return m.fail("check", fmt.Sprintf("Others use this MaaS platform: %s. Removing it would break them. They unpublish and remove theirs first. Nothing was deleted.", strings.Join(others, ", ")), "in_use", "maas in use")
	}
	m.step("check", "success", "Removing, in reverse order of setup: published models, MaaS in the DataScienceCluster, the gateway, the database.")

	// Published models first.
	m.step("unpublish", "running", "Unpublishing this tool's models...")
	pubs := rec.pubs()
	for _, p := range pubs {
		res, _ := (&maasRun{c: c, activity: "unpublish-maas", target: fmt.Sprintf("model %s in %s", p.model, p.project)}).unpublish(p.project, p.model, false)
		m.logs = append(m.logs, res.Logs...)
		if !res.Success {
			return m.fail("unpublish", res.Message, res.ErrorCode, "unpublish failed")
		}
	}
	m.step("unpublish", "success", fmt.Sprintf("%d %s unpublished.", len(pubs), verb(len(pubs), "model", "models")))
	if rec, err = readMaaSRecord(c); err != nil {
		return m.fail("dsc", err.Error(), errorCodeFromK8sErr(err), "record read failed")
	}

	// MaaS in the DataScienceCluster, back to what it was.
	if rec.DSCChanged {
		prev := firstNonEmpty(rec.PreviousModelsAsAService, "Removed")
		if dsc.maas == "Managed" {
			m.step("dsc", "running", fmt.Sprintf("Setting aigateway.modelsAsAService back to %s...", prev))
			if err := patchModelsAsAService(c, prev); err != nil {
				return m.fail("dsc", fmt.Sprintf("Failed to restore modelsAsAService: %v. Nothing else was deleted.", err), errorCodeFromK8sErr(err), "dsc patch failed")
			}
			gone, _ := waitForGone(c, MaaSDeleteTimeout, MaaSPoll, func(cc *Client) (bool, error) {
				return deploymentAvailable(cc, infra, "maas-api") == "missing", nil
			})
			if !gone {
				m.step("dsc", "running", "maas-api is still being removed by the operator; continuing.")
			}
		}
		rec.DSCChanged, rec.PreviousModelsAsAService = false, ""
		if err := writeMaaSRecord(c, rec); err != nil {
			return m.fail("dsc", err.Error(), errorCodeFromK8sErr(err), "record write failed")
		}
		m.step("dsc", "running", "modelsAsAService is "+prev+" again, as before setup.")
	} else {
		m.step("dsc", "running", "modelsAsAService not changed: this tool did not turn MaaS on.")
	}
	// The AI gateway itself, back to what it was, only while nobody has set
	// it since setup (the API server gives the field to whoever last set it).
	if rec.AIGatewayChanged {
		prev := firstNonEmpty(rec.PreviousAIGateway, "Removed")
		switch {
		case dsc.aigateway != "Managed":
			m.logs = append(m.logs, "aigateway is "+firstNonEmpty(dsc.aigateway, "not set")+" already")
		case !dscAIGatewayOwned(dsc):
			m.logs = append(m.logs, "Kept aigateway Managed: it was set after this tool's setup, by someone else")
			m.step("dsc", "running", "aigateway stays Managed: someone set it after this tool's setup.")
		default:
			if err := patchMaaSDSC(c, prev, "", false); err != nil {
				return m.fail("dsc", fmt.Sprintf("Failed to restore aigateway: %v. Nothing else was deleted.", err), errorCodeFromK8sErr(err), "dsc patch failed")
			}
			m.logs = append(m.logs, "OK: aigateway is "+prev+" again")
		}
		rec.AIGatewayChanged, rec.PreviousAIGateway = false, ""
		if err := writeMaaSRecord(c, rec); err != nil {
			return m.fail("dsc", err.Error(), errorCodeFromK8sErr(err), "record write failed")
		}
	}
	m.step("dsc", "success", "The DataScienceCluster is back to what it was before setup, where this tool changed it.")

	del := func(step string, o obj) (bool, *types.OperationResponse) {
		if !o.found {
			return false, nil
		}
		if !toolOwned(o.meta) {
			m.logs = append(m.logs, "Kept "+maasDesc(o.kind, infra)+": not created by this tool")
			return false, nil
		}
		path := maasObjectPath(o.kind, infra)
		m.step(step, "running", "Deleting "+maasDesc(o.kind, infra)+"...")
		if _, err := deleteWithUID(c, path, o.meta.UID); err != nil && !IsK8sError(err, 404) {
			res, _ := m.fail(step, fmt.Sprintf("Failed to delete %s: %v. Remove again to retry.", maasDesc(o.kind, infra), err), maasWriteCode(err), "delete failed")
			return false, res
		}
		if gone, _ := waitForDeletion(c, path, MaaSDeleteTimeout, MaaSPoll); !gone {
			m.logs = append(m.logs, maasDesc(o.kind, infra)+" is still being deleted")
		}
		return true, nil
	}
	var kept []string
	// The gateway goes only when no route is attached to it, as for the
	// inference gateway: the gateway controller's count also sees routes of
	// models published by others and routes made by hand.
	if o := objs[0]; o.found && toolOwned(o.meta) {
		gw, err := readMaaSGateway(c)
		if err != nil {
			return m.fail("gateway", fmt.Sprintf("Cannot read the gateway's attached routes, so the gateway and the database were not deleted: %v.", err), errorCodeFromK8sErr(err), "gateway read failed")
		}
		if n := gw.gw.attachedRoutes(); gw.found && n > 0 {
			return m.fail("gateway", fmt.Sprintf("%d %s attached to Gateway %s/%s (models published by others, or routes made by hand; `oc get httproute -A` lists them), so the gateway and the database were not deleted. Remove again once they are gone.", n, verb(n, "route is", "routes are"), inferenceGatewayNamespace, maasGatewayName), "in_use", "routes attached")
		}
	}
	// The Gateway, then its options ConfigMap.
	for _, o := range objs[:2] {
		if _, res := del("gateway", o); res != nil {
			return res, nil
		}
	}
	m.step("gateway", "success", "The gateway this tool created is deleted; OpenShift removes its load balancer. GatewayClass "+maasGatewayClass+" stays: it is OpenShift's shared class for gateways and costs nothing on its own.")
	var deleted []string
	devOurs := false
	for _, o := range objs[2:] {
		devOurs = devOurs || (o.kind == "PostgresDeployment" && o.found && toolOwned(o.meta))
		gone, res := del("database", o)
		if res != nil {
			return res, nil
		}
		if gone {
			deleted = append(deleted, maasDesc(o.kind, infra))
		}
	}
	// The development database's postgres-creds: by its recorded UID only;
	// with no record it is kept, and the log says why.
	if rec.POCCredsUID != "" || devOurs {
		gone, res := m.deletePOCCreds("database", infra, &rec)
		if res != nil {
			return res, nil
		}
		if gone {
			deleted = append(deleted, maasDesc("PostgresSecret", infra))
		}
	}
	if rec.DBNamespace != "" || rec.Database != "" {
		rec.DBNamespace, rec.Database = "", ""
		if err := writeMaaSRecord(c, rec); err != nil {
			return m.fail("database", err.Error(), errorCodeFromK8sErr(err), "record write failed")
		}
	}
	dbMsg := "Nothing of the database was created by this tool."
	if len(deleted) > 0 {
		dbMsg = "Deleted: " + strings.Join(deleted, ", ") + " (a database's data goes with it). A PostgreSQL of your own is never touched."
	}
	if ns := rec.InfraNamespace; ns != "" {
		kept, err := m.deleteInfraNamespace(&rec, ns)
		if err != nil {
			return m.fail("database", err.Error(), errorCodeFromK8sErr(err), "record write failed")
		}
		if kept != "" {
			dbMsg += " " + kept
		} else {
			dbMsg += " Namespace " + ns + ", which this tool created, is being deleted."
		}
	}
	m.step("database", "success", dbMsg)
	for _, o := range objs {
		if o.found && !toolOwned(o.meta) {
			kept = append(kept, maasDesc(o.kind, infra)+" (not created by this tool)")
		}
	}
	msg := "The MaaS platform this tool set up is removed. Kept, as shared platform pieces other RHOAI features use (RHOAI's inference gateway authentication is Kuadrant's), also where this tool installed them: Connectivity Link with its Kuadrant CR and Authorino TLS, and the CloudNativePG operator. Also kept: GatewayClass " + maasGatewayClass + " (OpenShift's shared class for gateways; it costs nothing on its own), and the namespaces and objects RHOAI's operator made"
	if len(kept) > 0 {
		msg += "; " + strings.Join(kept, "; ")
	}
	return m.done(msg + ".")
}

// --- publish ----------------------------------------------------------------------

// maasModelRefObjectFor is the MaaSModelRef of the tool's model as the
// dashboard's MaaS option makes it (odh-dashboard rhoai-3.6 maas
// maas-model-ref.ts postDeployMaaSModelRef L161-231, which passes the
// model's UID and description to the BFF's buildModelRefUnstructured in
// packages/maas/bff/internal/repositories/maasmodelrefs.go): owned by the
// model with blockOwnerDeletion false, so garbage collection removes it
// with the model (the MaaS record then shows the publication as partly
// done, and Unpublish finishes it), and a description. The display name
// and description are the catalog's (the caller's), not the object's,
// which a project editor can change.
func maasModelRefObjectFor(c *Client, project, name, displayName, description, modelUID string) map[string]interface{} {
	annotations := map[string]interface{}{"openshift.io/display-name": displayName}
	if description != "" {
		annotations["openshift.io/description"] = description
	}
	return map[string]interface{}{
		"apiVersion": maasAPI(c),
		"kind":       "MaaSModelRef",
		"metadata": map[string]interface{}{
			"name": name, "namespace": project, "labels": toolLabels(),
			"annotations": annotations,
			"ownerReferences": []interface{}{map[string]interface{}{
				"apiVersion": llmisvcAPIVersion, "kind": "LLMInferenceService", "name": name, "uid": modelUID, "blockOwnerDeletion": false,
			}},
		},
		"spec": map[string]interface{}{"modelRef": map[string]interface{}{"kind": "LLMInferenceService", "name": name}},
	}
}

func maasAccessPath(c *Client, kind string) string {
	if kind == "MaaSSubscription" {
		return namespacedPath(maasAPI(c), "maassubscriptions", "models-as-a-service", "rhoai-nightly-updater-subscription")
	}
	return namespacedPath(maasAPI(c), "maasauthpolicies", "models-as-a-service", "rhoai-nightly-updater-access")
}

func maasAccessCreatePath(c *Client, kind string) string {
	if kind == "MaaSSubscription" {
		return withToolFieldManager(namespacedPath(maasAPI(c), "maassubscriptions", "models-as-a-service", ""))
	}
	return withToolFieldManager(namespacedPath(maasAPI(c), "maasauthpolicies", "models-as-a-service", ""))
}

// applyMaaSObject creates or updates one of the tool's MaaS objects.
func applyMaaSObject(c *Client, desc, path, createPath string, obj map[string]interface{}, dryRun bool) error {
	cur, found, err := readObjectMeta(c, path)
	if err != nil {
		return err
	}
	read := func() (objectMeta, bool, error) { return readObjectMeta(c, path) }
	_, _, _, err = ensureToolObject(c, desc, path, createPath, obj, cur, found, read, toolOwned, dryRun)
	return err
}

// isMaaSGatewayRef: a model gateway ref names the MaaS gateway.
func isMaaSGatewayRef(name, namespace string) bool {
	return name == maasGatewayName && namespace == inferenceGatewayNamespace
}

// gatewayRefNames are the model's gateway refs as "namespace/name".
func (m llmisvcObject) gatewayRefNames() []string {
	if m.Spec.Router == nil || m.Spec.Router.Gateway == nil {
		return nil
	}
	var names []string
	for _, ref := range m.Spec.Router.Gateway.Refs {
		names = append(names, firstNonEmpty(ref.Namespace, m.Metadata.Namespace)+"/"+ref.Name)
	}
	return names
}

// modelHasMaaSRef: the model still routes through the MaaS gateway.
func modelHasMaaSRef(m llmisvcObject) bool {
	if m.Spec.Router == nil || m.Spec.Router.Gateway == nil {
		return false
	}
	for _, ref := range m.Spec.Router.Gateway.Refs {
		if isMaaSGatewayRef(ref.Name, ref.Namespace) {
			return true
		}
	}
	return false
}

// maasLivePubs are the publications the shared subscription and access
// policy list: the recorded ones that still have this tool's MaaSModelRef
// (not being deleted), plus with, minus without (zero for none). A
// recorded one whose reference is gone is left out: a modelRef to nothing
// would make the subscription, and so every publication, not ready.
func maasLivePubs(c *Client, rec maasRecord, with, without maasPub) ([]maasPub, error) {
	var out []maasPub
	for _, p := range rec.pubs() {
		if p == with || p == without {
			continue
		}
		ref, found, err := readMaaSModelRef(c, p.project, p.model)
		if err != nil {
			return nil, fmt.Errorf("read MaaSModelRef %s: %w", p.key(), err)
		}
		if found && toolOwned(ref.Metadata) && !ref.Metadata.terminating() {
			out = append(out, p)
		}
	}
	if with.project != "" {
		out = append(out, with)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key() < out[j].key() })
	return out, nil
}

func (m *maasRun) publish(project, name string, access *types.MaaSAudience, limitReq *types.MaaSLimit) (*types.OperationResponse, error) {
	c := m.c
	m.step("check", "running", "Checking the platform and the model...")
	limit, problem := parseMaaSLimit(limitReq)
	if problem != "" {
		return m.fail("check", "Invalid limit: "+problem+". Nothing was changed.", "validation", "invalid limit")
	}
	st, err := GetMaaSStatus(c)
	if err != nil {
		return m.fail("check", err.Error()+" Nothing was changed.", errorCodeFromK8sErr(err), "status failed")
	}
	if !st.Platform.Ready {
		return m.fail("check", maasNotReadyReason(st)+" Nothing was changed.", "prerequisites", "platform not ready")
	}
	// Connectivity Link's version (fail closed when it cannot be read).
	rhcl, err := readOperator(c, rhclOperator)
	if err != nil {
		return m.fail("check", fmt.Sprintf("Cannot read the installed operators, so this tool cannot check Connectivity Link's version; nothing was changed: %v.", err), errorCodeFromK8sErr(err), "csv read failed")
	}
	if versionCore(rhcl.version) == rhclAuthBypassVersion {
		return m.fail("check", rhclWarning(rhcl.version)+" Nothing was changed.", "prerequisites", "rhcl auth bypass")
	}
	priority, others, err := maasSubscriptionPriority(c)
	if err != nil {
		return m.fail("check", fmt.Sprintf("Cannot list the MaaSSubscriptions, so this tool cannot pick a priority below the others'; nothing was changed: %v.", err), errorCodeFromK8sErr(err), "subscription list failed")
	}
	if len(others) > 0 && rhclBelowMinimum(rhcl.version) {
		return m.fail("check", fmt.Sprintf("MaaS already has other subscriptions (%s), and with this tool's a signed-in user would have two or more. On Connectivity Link %s such a caller can hang (the MaaS docs ask for %s or later with RHOAI 3.6), so this tool does not publish until it is upgraded: Upgrade Connectivity Link on the MaaS tab does it. Nothing was changed.", strings.Join(others, ", "), rhcl.version, rhclDocsMinimum), "prerequisites", "rhcl too old for several subscriptions")
	}
	if msg, code := maasNamespaceRefusal(c, maasInfraNamespace(c)); msg != "" {
		return m.fail("check", msg, code, "namespace not rhoai's")
	}
	refPath, _ := maasModelRefPath(c, project, name)
	model, found, err := readToolModel(c, project, name)
	owned := false
	if err == nil && found {
		owned, err = ownsModel(c, model.Metadata)
	}
	switch {
	case err != nil:
		return m.fail("check", fmt.Sprintf("Cannot read %s in '%s': %v. Nothing was changed.", name, project, err), errorCodeFromK8sErr(err), "model read failed")
	case !found:
		return m.fail("check", fmt.Sprintf("Project '%s' has no %s of this tool. Set it up on the Models tab first. Nothing was changed.", project, name), "prerequisites", "no model")
	case !owned:
		return m.fail("check", fmt.Sprintf("%s in '%s' was not created by this tool, so it is not published. Nothing was changed.", name, project), "not_managed", "model foreign")
	}
	if cond, ok := findCondition(model.Status.Conditions, "Ready"); (!ok || cond.Status != "True") && !m.newModel {
		return m.fail("check", fmt.Sprintf("%s in '%s' is not ready yet. Publish it once it is. Nothing was changed.", name, project), "prerequisites", "model not ready")
	}
	expect, msg := planModelSpecExpectation(c, name)
	if msg != "" {
		return m.fail("check", msg+" Nothing was changed.", "prerequisites", "model spec unknown")
	}
	if _, _, spec, err := readModelGatewayRefsSpec(c, project, name); err != nil {
		return m.fail("check", fmt.Sprintf("Cannot read %s in '%s': %v. Nothing was changed.", name, project, err), errorCodeFromK8sErr(err), "model read failed")
	} else if d := expect.differences(spec); len(d) > 0 {
		return m.fail("check", specChangedRefusal(name, project, d, "Nothing was changed."), "conflict", "model spec changed")
	}
	ref, refFound, err := readMaaSModelRef(c, project, name)
	if err != nil {
		return m.fail("check", fmt.Sprintf("Cannot read MaaSModelRef %s/%s: %v. Nothing was changed.", project, name, err), errorCodeFromK8sErr(err), "ref read failed")
	}
	if refFound && !toolOwned(ref.Metadata) {
		return m.fail("check", fmt.Sprintf("Project '%s' already has a MaaSModelRef %s that this tool did not create; it is left alone. Nothing was changed.", project, name), "not_managed", "ref foreign")
	}
	rec, err := readMaaSRecord(c)
	if err != nil {
		return m.fail("check", err.Error()+" Nothing was changed.", errorCodeFromK8sErr(err), "record read failed")
	}
	// The refs Unpublish writes back: the model's own while it has no MaaS
	// gateway, else what this build recorded when it published, else no
	// route for a model the Models tab just set up through MaaS only (it
	// was created on the MaaS gateway alone), else (a publication by an
	// earlier build) what that one replaced.
	refs, _, err := readModelGatewayRefs(c, project, name)
	if err != nil {
		return m.fail("check", fmt.Sprintf("Cannot read %s in '%s': %v. Nothing was changed.", name, project, err), errorCodeFromK8sErr(err), "model read failed")
	}
	original, recorded := rec.modelRefs(project, name)
	switch {
	case !refs.hasMaaS():
		original = maasModelRefs{Refs: refs}
	case recorded && !refs.maasOnly():
		return m.fail("check", refsChangedRefusal(project, name, refs, original, "Then Publish again: it records them and routes the model through "+maasGatewayName+" alone."), "conflict", "model refs changed")
	case !recorded && m.newModel && refs.maasOnly():
		original = maasModelRefs{NoRoute: true}
	case !recorded:
		original = maasModelRefs{Refs: refs.earlierBuildOriginal()}
	}
	rec.setModelRefs(project, name, original)
	// What the model had before a first publish; a publish again (after a
	// failed one) keeps what the first recorded.
	if _, recordedGenAI := rec.GenAI[maasPub{project, name}.key()]; !recordedGenAI {
		rec.setGenAI(project, name, genAIOf(model.Metadata))
	}
	rec.setPublished(project, name, true)
	infra := maasInfraNamespace(c)
	gwPlan, msg, code := planMaaSGateway(c, infra, rec.projects())
	if msg != "" {
		return m.fail("check", msg, code, "gateway check failed")
	}
	if code == "foreign" {
		gw, err := readMaaSGateway(c)
		if err != nil {
			return m.fail("check", "Cannot read the gateway: "+err.Error(), errorCodeFromK8sErr(err), "gateway read failed")
		}
		labels, msg, code := projectNamespace(c, project)
		if msg != "" {
			return m.fail("check", msg+" Nothing was changed.", code, "project check failed")
		}
		if admits := gw.gw.admits(project, labels); admits == "unknown" {
			return m.fail("check", fmt.Sprintf("Gateway %s/%s, which this tool did not create, selects the namespaces it admits in a way this tool cannot evaluate, so it cannot tell whether '%s' is admitted. Its owner checks the listeners' allowedRoutes. Nothing was changed.", inferenceGatewayNamespace, maasGatewayName, project), "prerequisites", "gateway admission unknown")
		} else if admits == "no" {
			return m.fail("check", fmt.Sprintf("Gateway %s/%s, which this tool did not create, does not admit routes from '%s'. Its owner adds the project (maas-setup.md: label it maas.opendatahub.io/gateway-access=true when the gateway selects that label). Nothing was changed.", inferenceGatewayNamespace, maasGatewayName, project), "prerequisites", "gateway does not admit")
		}
		gwPlan = nil
	}
	live, err := maasLivePubs(c, rec, maasPub{project, name}, maasPub{})
	if err != nil {
		return m.fail("check", fmt.Sprintf("Cannot check the other publications: %v. Nothing was changed.", err), errorCodeFromK8sErr(err), "ref read failed")
	}
	// The other models keep their limits, and the audience is the
	// subscription's: Publish sets it only for the first model.
	cur, err := readMaaSToolSubscription(c)
	if err != nil {
		return m.fail("check", err.Error()+" Nothing was changed.", errorCodeFromK8sErr(err), "subscription read failed")
	}
	aud := maasAudience{everyone: true}
	if cur.found {
		aud = cur.audience
	}
	if access != nil {
		want, problem := parseMaaSAudience(access)
		if problem != "" {
			return m.fail("check", "Invalid audience: "+problem+". Nothing was changed.", "validation", "invalid audience")
		}
		if cur.found && len(live) > 1 && !want.equal(cur.audience) {
			return m.fail("check", fmt.Sprintf("Who can use the published models is set for all of them (now %s), and other models are published. Change it with Change who can use them on the MaaS tab, then publish. Nothing was changed.", cur.audience.text()), "conflict", "audience differs")
		}
		aud = want
	}
	limits := map[string]maasLimit{}
	for _, p := range live {
		limits[p.key()] = cur.limitOf(p)
	}
	limits[maasPub{project, name}.key()] = limit
	sub, auth := maasAccessObjects(c, live, limits, aud, priority)
	if !aud.everyone {
		for _, p := range maasGroupProblems(c, aud.groups) {
			m.step("check", "running", p.Message)
		}
	}
	// The catalog's display name and description (the text setup writes on
	// the model), never the object's: MaaS shows them to every signed-in
	// user, and a project editor can change the annotations.
	entry, _ := catalogEntry(name)
	refObj := maasModelRefObjectFor(c, project, name, firstNonEmpty(entry.DisplayName, name), entry.Description, model.Metadata.UID)
	if gwPlan != nil {
		if msg, code := m.applyGateway(gwPlan, true); msg != "" {
			return m.fail("check", msg, code, "gateway dry run failed")
		}
	}
	for _, w := range []struct {
		desc, path, create string
		obj                map[string]interface{}
	}{
		{"MaaSModelRef " + project + "/" + name, refPath, withToolFieldManager(namespacedPath(maasAPI(c), "maasmodelrefs", project, "")), refObj},
		{"MaaSSubscription " + maasSubscriptionName, maasAccessPath(c, "MaaSSubscription"), maasAccessCreatePath(c, "MaaSSubscription"), sub},
		{"MaaSAuthPolicy " + maasAuthPolicyName, maasAccessPath(c, "MaaSAuthPolicy"), maasAccessCreatePath(c, "MaaSAuthPolicy"), auth},
	} {
		if err := applyMaaSObject(c, w.desc, w.path, w.create, w.obj, true); err != nil {
			if why := unlimitedRefusal(err, limit); why != "" && strings.HasPrefix(w.desc, "MaaSSubscription") {
				return m.fail("check", why+" Nothing was changed.", "prerequisites", "unlimited not served")
			}
			return m.fail("check", fmt.Sprintf("The API server rejects %s: %v. Nothing was changed.", w.desc, err), maasWriteCode(err), "dry run failed")
		}
	}
	m.step("check", "success", fmt.Sprintf("The platform is ready and the model can be published: %s, for %s.", limit.text(), aud.text()))

	if err := writeMaaSRecord(c, rec); err != nil {
		return m.fail("gateway", err.Error()+" Nothing was changed.", errorCodeFromK8sErr(err), "record write failed")
	}
	if gwPlan != nil {
		m.step("gateway", "running", fmt.Sprintf("Letting the MaaS gateway admit routes from '%s'...", project))
		if msg, code := m.applyGateway(gwPlan, false); msg != "" {
			return m.fail("gateway", msg+" Unpublish to undo.", code, "gateway apply failed")
		}
		m.step("gateway", "success", fmt.Sprintf("The MaaS gateway admits routes from '%s'.", project))
	} else {
		m.step("gateway", "success", "The gateway someone else made admits the project.")
	}
	if msg, code := m.routeModelThroughMaaS(project, name, original, expect); msg != "" {
		return m.fail("model", msg, code, "model routing failed")
	}
	if err := stripModelGenAI(c, project, name); err != nil {
		return m.fail("model", err.Error()+". Publish again to retry, or Unpublish to undo.", maasWriteCode(err), "genai metadata failed")
	}
	m.logs = append(m.logs, "OK: the model left Gen AI studio's AI assets (label "+genAIAssetLabel+" and annotation "+genAIUseCaseAnnotation+" removed, as the dashboard's MaaS option does); MaaS lists it")
	m.step("maas", "running", "Creating the model reference, the subscription and the access policy...")
	for _, w := range []struct {
		desc, path, create string
		obj                map[string]interface{}
	}{
		{"MaaSModelRef " + project + "/" + name, refPath, withToolFieldManager(namespacedPath(maasAPI(c), "maasmodelrefs", project, "")), refObj},
		{"MaaSSubscription " + maasSubscriptionName, maasAccessPath(c, "MaaSSubscription"), maasAccessCreatePath(c, "MaaSSubscription"), sub},
		{"MaaSAuthPolicy " + maasAuthPolicyName, maasAccessPath(c, "MaaSAuthPolicy"), maasAccessCreatePath(c, "MaaSAuthPolicy"), auth},
	} {
		if err := applyMaaSObject(c, w.desc, w.path, w.create, w.obj, false); err != nil {
			return m.fail("maas", fmt.Sprintf("Failed to apply %s: %v. Unpublish to undo.", w.desc, err), maasWriteCode(err), "apply failed")
		}
		m.logs = append(m.logs, "OK: "+w.desc)
	}
	m.step("maas", "success", fmt.Sprintf("MaaSModelRef %s/%s, MaaSSubscription %s and MaaSAuthPolicy %s are applied.", project, name, maasSubscriptionName, maasAuthPolicyName))
	m.step("ready", "running", "Waiting for MaaS to accept the model...")
	last := ""
	ready, err := waitForGone(c, MaaSPublishTimeout, MaaSPoll, func(cc *Client) (bool, error) {
		r, _, err := readMaaSModelRef(cc, project, name)
		if err != nil {
			return false, err
		}
		a, _ := maasPhase(cc, maasAccessPath(c, "MaaSAuthPolicy"))
		s, _ := maasPhase(cc, maasAccessPath(c, "MaaSSubscription"))
		msg := fmt.Sprintf("Model reference: %s; access: %s; subscription: %s", firstNonEmpty(r.Status.Phase, "pending"), firstNonEmpty(a, "pending"), firstNonEmpty(s, "pending"))
		if msg != last {
			last = msg
			m.step("ready", "running", msg)
		}
		return r.Status.Phase == "Ready" && a == "Active" && s == "Active", nil
	})
	if err != nil {
		return m.fail("ready", fmt.Sprintf("Stopped waiting: %v. The model is published; the MaaS tab shows its state.", err), firstNonEmpty(errorCodeFromK8sErr(err), "cancelled"), "wait failed")
	}
	if !ready {
		m.step("ready", "success", fmt.Sprintf("Not ready after %s (%s). The MaaS tab shows its state.", clockText(MaaSPublishTimeout), last))
		return m.done(fmt.Sprintf("%s in '%s' is published and still being accepted.", name, project))
	}
	m.step("ready", "success", "MaaS lists the model.")
	return m.done(fmt.Sprintf("%s in '%s' is published to MaaS under subscription %s.", name, project, maasSubscriptionName))
}

// --- unpublish ---------------------------------------------------------------------

// unpublish removes the publication of the tool's model name in project, in
// reverse order: the access policy and subscription entries (the objects
// when no other project is left), the model reference, the model's gateway
// ref, and the project from the gateway. stepped is false when it runs as
// part of a removal.
func (m *maasRun) unpublish(project, name string, stepped bool) (*types.OperationResponse, error) {
	c := m.c
	say := func(id, status, msg string) {
		if stepped {
			m.step(id, status, msg)
		} else {
			m.logs = append(m.logs, msg)
		}
	}
	say("check", "running", "Checking the publication...")
	rec, err := readMaaSRecord(c)
	if err != nil {
		return m.fail("check", err.Error()+" Nothing was changed.", errorCodeFromK8sErr(err), "record read failed")
	}
	refPath, _ := maasModelRefPath(c, project, name)
	ref, refFound, err := readMaaSModelRef(c, project, name)
	if err != nil {
		return m.fail("check", fmt.Sprintf("Cannot read MaaSModelRef %s/%s: %v. Nothing was changed.", project, name, err), errorCodeFromK8sErr(err), "ref read failed")
	}
	if refFound && !toolOwned(ref.Metadata) {
		return m.fail("check", fmt.Sprintf("MaaSModelRef %s/%s was not created by this tool; it is left alone.", project, name), "not_managed", "ref foreign")
	}
	model, modelFound, err := readToolModel(c, project, name)
	if err != nil {
		return m.fail("check", fmt.Sprintf("Cannot read %s in '%s': %v. Nothing was changed.", name, project, err), errorCodeFromK8sErr(err), "model read failed")
	}
	modelOwned := false
	if modelFound {
		if modelOwned, err = ownsModel(c, model.Metadata); err != nil {
			return m.fail("check", err.Error()+" Nothing was changed.", errorCodeFromK8sErr(err), "record read failed")
		}
	}
	routed := modelOwned && modelHasMaaSRef(model)
	if !refFound && !rec.published(project, name) && !routed {
		say("check", "success", "Not published.")
		return m.done(fmt.Sprintf("%s in '%s' is not published; nothing to undo.", name, project))
	}
	// The refs to write back, captured before the record forgets them; a
	// gateway someone added since publishing stops Unpublish here, before
	// anything changes.
	recordedRefs, hasRecordedRefs := rec.modelRefs(project, name)
	recordedGenAI, hasRecordedGenAI := rec.GenAI[maasPub{project, name}.key()]
	if routed && hasRecordedRefs {
		refs, _, err := readModelGatewayRefs(c, project, name)
		if err != nil {
			return m.fail("check", fmt.Sprintf("Cannot read %s in '%s': %v. Nothing was changed.", name, project, err), errorCodeFromK8sErr(err), "model read failed")
		}
		if !refs.maasOnly() {
			return m.fail("check", refsChangedRefusal(project, name, refs, recordedRefs, "Then Unpublish again: it leaves the model's gateways as you set them."), "conflict", "model refs changed")
		}
	}
	say("check", "success", "Unpublishing, in reverse order of publishing.")
	live, err := maasLivePubs(c, rec, maasPub{}, maasPub{project, name})
	if err != nil {
		return m.fail("check", fmt.Sprintf("Cannot check the other publications: %v. Nothing was changed.", err), errorCodeFromK8sErr(err), "ref read failed")
	}
	rec.setPublished(project, name, false)

	// The access policy and the subscription.
	say("maas", "running", "Removing the model from the access policy and the subscription...")
	// The models left keep their limits and the audience (fail closed: a
	// subscription that cannot be read is not rewritten with defaults).
	cur, err := readMaaSToolSubscription(c)
	if err != nil {
		return m.fail("maas", err.Error()+" Nothing was changed.", errorCodeFromK8sErr(err), "subscription read failed")
	}
	limits := map[string]maasLimit{}
	for _, p := range live {
		limits[p.key()] = cur.limitOf(p)
	}
	sub, auth := maasAccessObjects(c, live, limits, cur.audience, maasOwnSubscriptionPriority(c))
	for _, w := range []struct {
		kind, desc string
		obj        map[string]interface{}
	}{{"MaaSAuthPolicy", "MaaSAuthPolicy " + maasAuthPolicyName, auth}, {"MaaSSubscription", "MaaSSubscription " + maasSubscriptionName, sub}} {
		path := maasAccessPath(c, w.kind)
		meta, found, err := readObjectMeta(c, path)
		if err != nil {
			return m.fail("maas", fmt.Sprintf("Cannot read %s: %v.", w.desc, err), errorCodeFromK8sErr(err), "read failed")
		}
		if !found || !toolOwned(meta) {
			continue
		}
		if len(live) == 0 {
			if _, err := deleteWithUID(c, path, meta.UID); err != nil && !IsK8sError(err, 404) {
				return m.fail("maas", fmt.Sprintf("Failed to delete %s: %v.", w.desc, err), maasWriteCode(err), "delete failed")
			}
			_, _ = waitForDeletion(c, path, MaaSDeleteTimeout, MaaSPoll)
			m.logs = append(m.logs, "OK: "+w.desc+" deleted")
			continue
		}
		if err := applyMaaSObject(c, w.desc, path, maasAccessCreatePath(c, w.kind), w.obj, false); err != nil {
			return m.fail("maas", fmt.Sprintf("Failed to update %s: %v.", w.desc, err), maasWriteCode(err), "apply failed")
		}
	}
	if refFound {
		path := refPath
		if _, err := deleteWithUID(c, path, ref.Metadata.UID); err != nil && !IsK8sError(err, 404) {
			return m.fail("maas", fmt.Sprintf("Failed to delete MaaSModelRef %s/%s: %v.", project, name, err), maasWriteCode(err), "delete failed")
		}
		// maas-controller removes its finalizer after deleting the policies it
		// generated for the model. Without maas-controller nothing would:
		// the tool takes it off its own reference and says what may be left.
		if meta, found, err := readObjectMeta(c, path); err == nil && found && meta.terminating() && containsString(meta.Finalizers, maasModelFinalizer) {
			if why := maasControllerGone(c); why != "" {
				var rest []string
				for _, f := range meta.Finalizers {
					if f != maasModelFinalizer {
						rest = append(rest, f)
					}
				}
				patch, _ := json.Marshal(map[string]interface{}{"metadata": map[string]interface{}{"resourceVersion": meta.ResourceVersion, "finalizers": rest}})
				if _, _, err := c.patch(path, patch); err != nil && !IsK8sError(err, 404) {
					return m.fail("maas", fmt.Sprintf("MaaSModelRef %s/%s is being deleted and maas-controller cannot finish it (%s); removing its finalizer failed: %v. Unpublish again to retry.", project, name, why, err), maasWriteCode(err), "finalizer patch failed")
				}
				m.logs = append(m.logs, fmt.Sprintf("maas-controller cannot finish deleting MaaSModelRef %s/%s (%s), so this tool removed the finalizer %s from its own reference. Policies maas-controller generated for the model may be left in %s: oc get authpolicy,tokenratelimitpolicy -n %s -l maas.opendatahub.io/model=%s,app.kubernetes.io/managed-by=maas-controller", project, name, why, maasModelFinalizer, project, project, name))
			}
		}
		if gone, err := waitForDeletion(c, path, MaaSDeleteTimeout, MaaSPoll); err != nil || !gone {
			return m.fail("maas", fmt.Sprintf("MaaSModelRef %s/%s is still being deleted (maas-controller removes its finalizer; it must be running). Unpublish again to check.", project, name), "in_progress", "ref still deleting")
		}
	}
	say("maas", "success", "The model reference is deleted, and the access policy and subscription no longer list the model.")

	// The model's gateway refs. The record keeps the publication until the
	// route has left the MaaS gateway, so the tab shows a route that did not
	// as partly published.
	say("model", "running", "Routing the model as before publishing...")
	switch {
	case !modelFound:
		say("model", "success", "The model is gone; nothing to change on it.")
	case !modelOwned:
		say("model", "success", "The model was not created by this tool; it is left alone.")
	default:
		if msg, code := m.restoreModelRoute(project, name, recordedRefs, hasRecordedRefs, say); msg != "" {
			return m.fail("model", msg, code, "model routing failed")
		}
		if hasRecordedGenAI {
			if err := restoreModelGenAI(c, project, name, recordedGenAI); err != nil {
				return m.fail("model", err.Error()+". Unpublish again to retry.", maasWriteCode(err), "genai metadata failed")
			}
			m.logs = append(m.logs, "OK: the model is back in Gen AI studio's AI assets (its "+genAIAssetLabel+" label and "+genAIUseCaseAnnotation+" annotation)")
		}
	}

	// The project on the gateway.
	infra := maasInfraNamespace(c)
	if err := writeMaaSRecord(c, rec); err != nil {
		return m.fail("gateway", err.Error(), errorCodeFromK8sErr(err), "record write failed")
	}
	gwPlan, msg, code := planMaaSGateway(c, infra, rec.projects())
	switch {
	case msg != "":
		// The publication is gone; only the gateway's list of projects is
		// stale, and the next setup or publication rewrites it.
		say("gateway", "success", fmt.Sprintf("The MaaS gateway was not updated (%s) and may still admit routes from '%s'. Set up MaaS again to update it.", strings.TrimSuffix(msg, "."), project))
		return m.done(fmt.Sprintf("%s in '%s' is unpublished from MaaS; the MaaS gateway was not updated.", name, project))
	case code == "foreign":
		say("gateway", "success", "The gateway someone else made is not changed.")
	case !gwPlan.found["Gateway"]:
		say("gateway", "success", "There is no MaaS gateway to update.")
	case containsString(rec.projects(), project):
		say("gateway", "success", fmt.Sprintf("The MaaS gateway still admits routes from '%s': another model of this tool is published there.", project))
	default:
		gwPlan.classMissing = false // only the gateway changes
		if msg, code := m.applyGateway(gwPlan, false); msg != "" {
			return m.fail("gateway", msg, code, "gateway apply failed")
		}
		say("gateway", "success", fmt.Sprintf("The MaaS gateway no longer admits routes from '%s'.", project))
	}
	note := ""
	// A model set up through MaaS only has no direct address to note.
	if modelFound && modelOwned && !recordedRefs.NoRoute {
		if note = directRouteNote(c, project, name); note != "" {
			m.logs = append(m.logs, strings.TrimSpace(note))
		}
	}
	return m.done(fmt.Sprintf("%s in '%s' is unpublished from MaaS.%s", name, project, note))
}
