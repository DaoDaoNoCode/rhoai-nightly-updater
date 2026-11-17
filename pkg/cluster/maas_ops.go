package cluster

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// maasRun is one streamed MaaS operation.
type maasRun struct {
	c        *Client
	emit     func(UpdateStepEvent)
	logs     []string
	activity string
	target   string
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
	switch req.Action {
	case "setup", "teardown":
		if req.Project != "" {
			return "the platform takes no project"
		}
	case "publish", "unpublish":
		if !playgroundProjectRegex.MatchString(req.Project) {
			return "a valid project is required"
		}
	default:
		return "action must be setup, teardown, publish or unpublish"
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
	if req.Project != "" {
		m.target = "model in " + req.Project
	}
	if problem := ValidateMaaSRequest(req); problem != "" {
		return m.fail("check", problem, "validation", "invalid request")
	}
	switch req.Action {
	case "setup":
		return m.setup()
	case "teardown":
		return m.teardown()
	case "publish":
		return m.publish(req.Project)
	}
	return m.unpublish(req.Project, true)
}

// TeardownMaaSPlatform removes the MaaS platform this tool set up (Clean up
// everything).
func TeardownMaaSPlatform(c *Client) (*types.OperationResponse, error) {
	return RunMaaS(c, types.MaaSRequest{Action: "teardown"}, nil)
}

// UnpublishMaaSModel unpublishes this tool's model in project (Clean up
// everything).
func UnpublishMaaSModel(c *Client, project string) (*types.OperationResponse, error) {
	return RunMaaS(c, types.MaaSRequest{Action: "unpublish", Project: project}, nil)
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
	if ok && meta.terminating() {
		return nil, fmt.Sprintf("%s is still being deleted. Wait until it is gone, then try again.", maasDesc("Gateway", "")), "terminating"
	}
	if ok && !gatewayOwned(meta) {
		return nil, "", "foreign"
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
	for _, kind := range []string{"Gateway"} {
		k := kind
		read := func() (objectMeta, bool, error) { return readObjectMeta(m.c, maasObjectPath(k, "")) }
		action, meta, found, err := ensureToolObject(m.c, maasDesc(kind, ""), maasObjectPath(kind, ""), maasCreatePath(kind, ""), plan.objects[kind], plan.current[kind], plan.found[kind], read, gatewayOwned, dryRun)
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

// --- setup ----------------------------------------------------------------------

func (m *maasRun) setup() (*types.OperationResponse, error) {
	c := m.c
	m.step("check", "running", "Checking what the MaaS platform needs...")
	rec, err := readMaaSRecord(c)
	if err != nil {
		return m.fail("check", err.Error()+" Nothing was changed.", errorCodeFromK8sErr(err), "record read failed")
	}
	dsc, err := readMaaSDSC(c)
	if err != nil {
		return m.fail("check", fmt.Sprintf("Cannot read the DataScienceCluster; nothing was changed: %v.", err), errorCodeFromK8sErr(err), "dsc read failed")
	}
	switch {
	case dsc.read.Object == nil:
		return m.fail("check", "This cluster has no DataScienceCluster. A cluster-admin creates one on the Components page. Nothing was changed.", "prerequisites", "no dsc")
	case dsc.aigateway != "Managed":
		return m.fail("check", fmt.Sprintf("MaaS runs in RHOAI's AI gateway, which is %s in DataScienceCluster %s. A cluster-admin sets aigateway to Managed first (platform-setup.md). Nothing was changed.", firstNonEmpty(dsc.aigateway, "not set"), dsc.name), "prerequisites", "aigateway off")
	case dsc.maas != "Managed" && dsc.argoApp != "":
		return m.fail("check", gitOpsRefusal("The DataScienceCluster", dsc.argoApp, "turning on Models as a Service")+" Set aigateway.modelsAsAService to Managed in Git. Nothing was changed.", ErrorCodeGitOpsManaged, "dsc gitops")
	}
	kq, err := readKuadrant(c)
	if err != nil {
		return m.fail("check", fmt.Sprintf("Cannot read the Kuadrant CR; nothing was changed: %v.", err), errorCodeFromK8sErr(err), "kuadrant read failed")
	}
	if !kq.found {
		return m.fail("check", "MaaS needs Red Hat Connectivity Link with a Kuadrant CR, and there is none. A cluster-admin installs it as platform-setup.md shows; this tool does not. Nothing was changed.", "prerequisites", "no kuadrant")
	}
	if !kq.ready {
		m.step("check", "running", "The Kuadrant CR is not Ready yet ("+firstNonEmpty(kq.message, "no message")+"); continuing.")
	}
	if w := rhclWarning(rhclVersion(c)); w != "" {
		m.step("check", "running", w)
	}
	infra := maasInfraNamespace(c)
	if _, found, err := readObjectMeta(c, "/api/v1/namespaces/"+infra); err != nil || !found {
		return m.fail("check", fmt.Sprintf("The AI gateway's infrastructure namespace %s does not exist yet (the AI gateway operator creates it). Wait for the operator, then try again. Nothing was changed.", infra), "prerequisites", "no infra namespace")
	}

	// The database: a connection Secret someone else made is used as it is.
	secMeta, secFound, err := readObjectMeta(c, maasObjectPath("Secret", infra))
	if err != nil {
		return m.fail("check", fmt.Sprintf("Cannot read Secret %s/%s; nothing was changed: %v.", infra, maasDBConfigSecret, err), errorCodeFromK8sErr(err), "secret read failed")
	}
	foreignDB := secFound && !toolOwned(secMeta)
	var clMeta objectMeta
	var clFound bool
	if !foreignDB {
		if v := servedVersions(c, cnpgGroup, nil); v.Discovered && !containsString(v.Versions, "v1") {
			return m.fail("check", "The database needs the CloudNativePG operator (postgresql.cnpg.io/v1), which is not installed. A cluster-admin installs it from OperatorHub, or creates Secret maas-db-config for a PostgreSQL of their own (maas-setup.md). Nothing was changed.", "prerequisites", "no cnpg")
		}
		clMeta, clFound, _, _, err = readMaaSDBCluster(c, infra)
		if err != nil && !IsK8sError(err, 404) {
			return m.fail("check", fmt.Sprintf("Cannot read %s; nothing was changed: %v.", maasDesc("Cluster", infra), err), errorCodeFromK8sErr(err), "db read failed")
		}
		if clFound && !toolOwned(clMeta) {
			return m.fail("check", fmt.Sprintf("%s exists and was not created by this tool, so it is left alone. Create Secret %s/%s for it yourself, or remove it. Nothing was changed.", maasDesc("Cluster", infra), infra, maasDBConfigSecret), "not_managed", "db foreign")
		}
	}

	// The gateway: one someone else made is used as it is.
	gwPlan, msg, code := planMaaSGateway(c, infra, rec.Published)
	foreignGW := code == "foreign"
	if msg != "" {
		return m.fail("check", msg, code, "gateway check failed")
	}

	// Every write as a server-side dry run first.
	if !foreignDB {
		if _, _, _, err := ensureToolObject(c, maasDesc("Cluster", infra), maasObjectPath("Cluster", infra), maasCreatePath("Cluster", infra), maasDBClusterObject(infra), clMeta, clFound, func() (objectMeta, bool, error) { return readObjectMeta(c, maasObjectPath("Cluster", infra)) }, toolOwned, true); err != nil {
			return m.fail("check", fmt.Sprintf("The API server rejects %s: %v. Nothing was changed.", maasDesc("Cluster", infra), err), maasWriteCode(err), "db dry run failed")
		}
		if _, _, _, err := ensureToolObject(c, maasDesc("Secret", infra), maasObjectPath("Secret", infra), maasCreatePath("Secret", infra), maasDBConfigObject(infra, "postgresql://dry-run@localhost/maas?sslmode=require"), secMeta, secFound, func() (objectMeta, bool, error) { return readObjectMeta(c, maasObjectPath("Secret", infra)) }, toolOwned, true); err != nil {
			return m.fail("check", fmt.Sprintf("The API server rejects %s: %v. Nothing was changed.", maasDesc("Secret", infra), err), maasWriteCode(err), "secret dry run failed")
		}
	}
	if !foreignGW {
		if msg, code := m.applyGateway(gwPlan, true); msg != "" {
			return m.fail("check", msg, code, "gateway dry run failed")
		}
	}
	m.step("check", "success", "Everything the platform needs is there.")

	// 1. Database Setup.
	if foreignDB {
		m.step("database", "success", fmt.Sprintf("Secret %s/%s already exists and was not created by this tool: maas-api uses the database it names, and this tool does not change it.", infra, maasDBConfigSecret))
	} else {
		m.step("database", "running", fmt.Sprintf("Creating a one-instance CloudNativePG database %s/%s (for development and test)...", infra, maasDBCluster))
		read := func() (objectMeta, bool, error) { return readObjectMeta(c, maasObjectPath("Cluster", infra)) }
		if _, _, _, err := ensureToolObject(c, maasDesc("Cluster", infra), maasObjectPath("Cluster", infra), maasCreatePath("Cluster", infra), maasDBClusterObject(infra), clMeta, clFound, read, toolOwned, false); err != nil {
			return m.fail("database", fmt.Sprintf("Failed to apply %s: %v.", maasDesc("Cluster", infra), err), maasWriteCode(err), "db apply failed")
		}
		m.step("database", "running", "Waiting for the database to start (usually 1 to 2 minutes)...")
		ready, err := waitForGone(c, MaaSDatabaseTimeout, MaaSPoll, func(cc *Client) (bool, error) {
			_, _, ok, _, err := readMaaSDBCluster(cc, infra)
			return ok, err
		})
		if err != nil {
			return m.fail("database", fmt.Sprintf("Stopped waiting for the database: %v. It was created; set up again to continue.", err), firstNonEmpty(errorCodeFromK8sErr(err), "cancelled"), "db wait failed")
		}
		if !ready {
			return m.fail("database", fmt.Sprintf("The database is not running after %s, and maas-api needs it before MaaS is turned on. Set up again to continue; the MaaS tab shows its state.", clockText(MaaSDatabaseTimeout)), "not_ready", "db not ready")
		}
		connURL, err := maasConnectionURL(c, infra)
		if err != nil {
			return m.fail("database", err.Error()+" Set up again to retry.", errorCodeFromK8sErr(err), "db secret read failed")
		}
		cur, found, err := readObjectMeta(c, maasObjectPath("Secret", infra))
		if err != nil {
			return m.fail("database", fmt.Sprintf("Cannot read Secret %s/%s: %v.", infra, maasDBConfigSecret, err), errorCodeFromK8sErr(err), "secret read failed")
		}
		read = func() (objectMeta, bool, error) { return readObjectMeta(c, maasObjectPath("Secret", infra)) }
		if _, _, _, err := ensureToolObject(c, maasDesc("Secret", infra), maasObjectPath("Secret", infra), maasCreatePath("Secret", infra), maasDBConfigObject(infra, connURL), cur, found, read, toolOwned, false); err != nil {
			return m.fail("database", fmt.Sprintf("Failed to write Secret %s/%s: %v.", infra, maasDBConfigSecret, maasWriteCode(err)), maasWriteCode(err), "secret apply failed")
		}
		m.step("database", "success", fmt.Sprintf("The database runs, and Secret %s/%s holds its connection URL (sslmode=require).", infra, maasDBConfigSecret))
	}

	// 2. Create Gateway.
	if foreignGW {
		m.step("gateway", "success", fmt.Sprintf("Gateway %s/%s already exists and was not created by this tool: MaaS uses it as it is.", inferenceGatewayNamespace, maasGatewayName))
	} else {
		m.step("gateway", "running", "Creating Gateway "+maasGatewayName+" with an external load balancer...")
		if msg, code := m.applyGateway(gwPlan, false); msg != "" {
			return m.fail("gateway", msg, code, "gateway apply failed")
		}
		m.step("gateway", "running", "Waiting for the gateway to be programmed (up to "+clockText(MaaSGatewayTimeout)+")...")
		programmed, err := waitForGone(c, MaaSGatewayTimeout, MaaSPoll, func(cc *Client) (bool, error) {
			gw, err := readMaaSGateway(cc)
			return gw.programmed && gw.address != "", err
		})
		switch {
		case err != nil:
			return m.fail("gateway", fmt.Sprintf("Stopped waiting for the gateway: %v. It was created.", err), firstNonEmpty(errorCodeFromK8sErr(err), "cancelled"), "gateway wait failed")
		case !programmed:
			m.step("gateway", "success", fmt.Sprintf("Created; not programmed after %s. It may still come up; the MaaS tab shows its state.", clockText(MaaSGatewayTimeout)))
		default:
			published, _ := waitForGone(c, InferenceGatewayDNSTimeout, MaaSPoll, func(cc *Client) (bool, error) { return dnsPublished(cc, gwPlan.host) == "published", nil })
			if published {
				m.step("gateway", "success", fmt.Sprintf("Programmed; https://%s is published in DNS.", gwPlan.host))
			} else {
				m.step("gateway", "success", fmt.Sprintf("Programmed; https://%s is not in DNS yet (a new load balancer's name can take a few minutes).", gwPlan.host))
			}
		}
	}

	// 3. Authorino TLS: checked; a cluster-admin runs the docs' script.
	tlsNote := ""
	if missing, _ := authorinoTLS(c, kq.namespace); missing != "" {
		tlsNote = " Authorino TLS is not configured (" + missing + "): a cluster-admin runs the commands the MaaS tab shows, or API keys are refused."
		m.step("authorino-tls", "success", "Not configured: "+missing+". This tool does not change Connectivity Link's Authorino; the MaaS tab shows the docs' commands for a cluster-admin.")
	} else {
		m.step("authorino-tls", "success", "Authorino serves TLS and trusts the service CA.")
	}

	// 4. Configure DataScienceCluster.
	if dsc.maas == "Managed" {
		who := "by this tool"
		if !rec.DSCChanged {
			who = "already, not by this tool"
		}
		m.step("dsc", "success", "modelsAsAService is Managed ("+who+").")
	} else {
		m.step("dsc", "running", fmt.Sprintf("Setting aigateway.modelsAsAService to Managed in DataScienceCluster %s (it was %s; removal restores that)...", dsc.name, firstNonEmpty(dsc.maas, "not set")))
		rec.DSCChanged, rec.PreviousModelsAsAService = true, dsc.maas
		if err := writeMaaSRecord(c, rec); err != nil {
			return m.fail("dsc", err.Error()+" The DataScienceCluster was not changed.", errorCodeFromK8sErr(err), "record write failed")
		}
		if err := patchModelsAsAService(c, "Managed"); err != nil {
			return m.fail("dsc", fmt.Sprintf("Failed to turn on MaaS in the DataScienceCluster: %v.", err), errorCodeFromK8sErr(err), "dsc patch failed")
		}
		m.step("dsc", "success", "modelsAsAService is Managed.")
	}

	// 5. Validation.
	m.step("ready", "running", "Waiting for the operator: maas-api, the default tenant, and the gateway's authentication (usually 2 to 3 minutes)...")
	last := ""
	ready, err := waitForGone(c, MaaSPlatformTimeout, MaaSPoll, func(cc *Client) (bool, error) {
		api := deploymentAvailable(cc, infra, "maas-api")
		ai, _ := readyCondition(cc, namespacedPath(maasAPIGroup, "aitenants", "ai-tenants", "models-as-a-service"))
		auth := maasGatewayAuth(cc)
		msg := fmt.Sprintf("maas-api: %s; tenant: %s; gateway authentication: %s", map[string]string{"True": "available", "False": "starting", "missing": "not created yet"}[api], firstNonEmpty(map[string]string{"True": "ready", "False": "not ready", "missing": "not created yet"}[ai], "unknown"), firstNonEmpty(auth, "unknown"))
		if msg != last {
			last = msg
			m.step("ready", "running", msg)
		}
		return api == "True" && ai == "True" && auth == "enforced", nil
	})
	if err != nil {
		return m.fail("ready", fmt.Sprintf("Stopped waiting: %v. Setup itself is done; the MaaS tab shows the platform's state.", err), firstNonEmpty(errorCodeFromK8sErr(err), "cancelled"), "ready wait failed")
	}
	if !ready {
		m.step("ready", "success", fmt.Sprintf("Still starting after %s (%s). The MaaS tab shows each part.", clockText(MaaSPlatformTimeout), last))
		return m.done("The MaaS platform is set up and still starting." + tlsNote)
	}
	m.step("ready", "success", "maas-api is available, the tenant is ready, and the gateway checks every request.")
	return m.done("The MaaS platform is ready." + tlsNote)
}

// patchModelsAsAService sets aigateway.modelsAsAService.managementState.
func patchModelsAsAService(c *Client, state string) error {
	path, err := dataScienceClusterPath(c)
	if err != nil {
		return err
	}
	patch := fmt.Sprintf(`{"spec":{"components":{"aigateway":{"modelsAsAService":{"managementState":%q}}}}}`, state)
	_, _, err = c.patch(path, []byte(patch))
	return err
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
	infra := maasInfraNamespace(c)
	type obj struct {
		kind  string
		meta  objectMeta
		found bool
	}
	var objs []obj
	owned := 0
	for _, kind := range []string{"Gateway", "Secret", "Cluster"} {
		meta, found, err := readObjectMeta(c, maasObjectPath(kind, infra))
		if err != nil && (kind != "Cluster" || !IsK8sError(err, 404)) {
			return m.fail("check", fmt.Sprintf("Cannot read %s; nothing was deleted: %v.", maasDesc(kind, infra), err), errorCodeFromK8sErr(err), "read failed")
		}
		objs = append(objs, obj{kind, meta, found})
		if found && toolOwned(meta) {
			owned++
		}
	}
	if owned == 0 && !rec.DSCChanged && len(rec.Published) == 0 {
		m.step("check", "success", "Nothing of the MaaS platform was set up by this tool.")
		return m.done("The MaaS platform was not set up by this tool; nothing to remove.")
	}
	if rec.DSCChanged && dsc.maas == "Managed" && dsc.argoApp != "" {
		return m.fail("check", gitOpsRefusal("The DataScienceCluster", dsc.argoApp, "turning off Models as a Service")+" Nothing was deleted.", ErrorCodeGitOpsManaged, "dsc gitops")
	}
	if !rec.DSCChanged && dsc.maas == "Managed" && owned > 0 {
		return m.fail("check", "MaaS was turned on in the DataScienceCluster by someone else, and it uses the gateway and database this tool created: removing them would break it. Turn MaaS off first (aigateway.modelsAsAService Removed), then remove. Nothing was deleted.", "in_use", "maas on, not by tool")
	}
	m.step("check", "success", "Removing, in reverse order of setup: published models, MaaS in the DataScienceCluster, the gateway, the database.")

	// Published models first.
	m.step("unpublish", "running", "Unpublishing this tool's models...")
	for _, project := range append([]string(nil), rec.Published...) {
		res, _ := (&maasRun{c: c, activity: "unpublish-maas", target: "model in " + project}).unpublish(project, false)
		m.logs = append(m.logs, res.Logs...)
		if !res.Success {
			return m.fail("unpublish", res.Message, res.ErrorCode, "unpublish failed")
		}
	}
	m.step("unpublish", "success", fmt.Sprintf("%d %s unpublished.", len(rec.Published), verb(len(rec.Published), "model", "models")))
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
		m.step("dsc", "success", "modelsAsAService is "+prev+" again, as before setup.")
	} else {
		m.step("dsc", "success", "Not changed: this tool did not turn MaaS on.")
	}

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
	if _, res := del("gateway", objs[0]); res != nil {
		return res, nil
	}
	m.step("gateway", "success", "The gateway this tool created is deleted; OpenShift removes its load balancer. GatewayClass "+maasGatewayClass+" stays: it is OpenShift's shared class for gateways and costs nothing on its own.")
	for _, o := range objs[1:] {
		if _, res := del("database", o); res != nil {
			return res, nil
		}
	}
	m.step("database", "success", "The database and its connection Secret are deleted (with the database's volume).")
	for _, o := range objs {
		if o.found && !toolOwned(o.meta) {
			kept = append(kept, maasDesc(o.kind, infra)+" (not created by this tool)")
		}
	}
	msg := "The MaaS platform this tool set up is removed. Kept: GatewayClass " + maasGatewayClass + " (OpenShift's shared class for gateways; it costs nothing on its own), Connectivity Link (Kuadrant) and Authorino, the CloudNativePG operator, and the namespaces and objects RHOAI's operator made"
	if len(kept) > 0 {
		msg += "; " + strings.Join(kept, "; ")
	}
	return m.done(msg + ".")
}

// --- publish ----------------------------------------------------------------------

func maasModelRefObjectFor(project, displayName string) map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": maasAPIGroup,
		"kind":       "MaaSModelRef",
		"metadata": map[string]interface{}{
			"name": "qwen25-05b-instruct", "namespace": project, "labels": toolLabels(),
			"annotations": map[string]interface{}{"openshift.io/display-name": displayName, "openshift.io/description": "Published by rhoai-nightly-updater"},
		},
		"spec": map[string]interface{}{"modelRef": map[string]interface{}{"kind": "LLMInferenceService", "name": maasModelRefName}},
	}
}

// maasAccessObjects are the tool's MaaSSubscription and MaaSAuthPolicy for
// the published projects (the docs' free sample: system:authenticated, 100
// tokens a minute, priority 10).
func maasAccessObjects(projects []string) (map[string]interface{}, map[string]interface{}) {
	var subRefs, authRefs []interface{}
	for _, p := range projects {
		subRefs = append(subRefs, map[string]interface{}{"name": maasModelRefName, "namespace": p,
			"tokenRateLimits": []interface{}{map[string]interface{}{"limit": 100, "window": "1m"}}})
		authRefs = append(authRefs, map[string]interface{}{"name": maasModelRefName, "namespace": p})
	}
	group := map[string]interface{}{"groups": []interface{}{map[string]interface{}{"name": maasSubjectGroup}}, "users": []interface{}{}}
	sub := map[string]interface{}{
		"apiVersion": maasAPIGroup,
		"kind":       "MaaSSubscription",
		"metadata": map[string]interface{}{"name": "rhoai-nightly-updater-subscription", "namespace": maasTenantNamespace, "labels": toolLabels(),
			"annotations": map[string]interface{}{"openshift.io/display-name": "rhoai-nightly-updater models", "openshift.io/description": "100 tokens a minute for every signed-in user"}},
		"spec": map[string]interface{}{"owner": group, "modelRefs": subRefs, "priority": 10},
	}
	auth := map[string]interface{}{
		"apiVersion": maasAPIGroup,
		"kind":       "MaaSAuthPolicy",
		"metadata": map[string]interface{}{"name": "rhoai-nightly-updater-access", "namespace": maasTenantNamespace, "labels": toolLabels(),
			"annotations": map[string]interface{}{"openshift.io/display-name": "rhoai-nightly-updater models", "openshift.io/description": "Every signed-in user may use this tool's published models"}},
		"spec": map[string]interface{}{"modelRefs": authRefs, "subjects": group},
	}
	return sub, auth
}

func maasAccessPath(kind string) string {
	if kind == "MaaSSubscription" {
		return namespacedPath(maasAPIGroup, "maassubscriptions", "models-as-a-service", "rhoai-nightly-updater-subscription")
	}
	return namespacedPath(maasAPIGroup, "maasauthpolicies", "models-as-a-service", "rhoai-nightly-updater-access")
}

func maasAccessCreatePath(kind string) string {
	if kind == "MaaSSubscription" {
		return withToolFieldManager(namespacedPath(maasAPIGroup, "maassubscriptions", "models-as-a-service", ""))
	}
	return withToolFieldManager(namespacedPath(maasAPIGroup, "maasauthpolicies", "models-as-a-service", ""))
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

// setModelGatewayRef adds (on) or removes the MaaS gateway ref on the
// tool's model, as field manager rhoai-nightly-updater-maas: a server-side
// apply of only the refs, so removing them reverts exactly this change.
//
// The model keeps its inference gateway: the docs append the MaaS gateway
// to the model's refs (model-setup.md, "/spec/gateway/refs/-") and the
// dashboard's MaaS transformer keeps the other refs. The tool's model
// names no ref (spec.router.gateway {}: the default, the inference
// gateway), so a lone MaaS ref would replace that; live, the model's
// inference address then answered 404 and the playground 500. The refs
// are written as the inference gateway and the MaaS gateway, as a
// dashboard-deployed model ends up.
func setModelGatewayRef(c *Client, project, resourceVersion string, on bool) error {
	path, _ := toolModelPath(project, maasModelRefName)
	obj := map[string]interface{}{
		"apiVersion": llmisvcAPIVersion,
		"kind":       "LLMInferenceService",
		"metadata":   map[string]interface{}{"name": "qwen25-05b-instruct", "namespace": project, "resourceVersion": resourceVersion},
	}
	if on {
		obj["spec"] = map[string]interface{}{"router": map[string]interface{}{"gateway": map[string]interface{}{
			"refs": []interface{}{
				map[string]interface{}{"name": inferenceGatewayName, "namespace": inferenceGatewayNamespace},
				map[string]interface{}{"name": maasGatewayName, "namespace": inferenceGatewayNamespace},
			},
		}}}
	}
	data, _ := json.Marshal(obj)
	query := url.Values{"fieldManager": {maasModelManager}, "force": {"true"}}
	_, _, err := c.do(http.MethodPatch, path, "application/apply-patch+yaml", data, query)
	return err
}

func (m *maasRun) publish(project string) (*types.OperationResponse, error) {
	c := m.c
	m.step("check", "running", "Checking the platform and the model...")
	st, err := GetMaaSStatus(c)
	if err != nil {
		return m.fail("check", err.Error()+" Nothing was changed.", errorCodeFromK8sErr(err), "status failed")
	}
	if !st.Platform.Ready {
		return m.fail("check", "The MaaS platform is not ready ("+st.Platform.Message+"). Set it up first. Nothing was changed.", "prerequisites", "platform not ready")
	}
	model, found, err := readToolModel(c, project, maasModelRefName)
	switch {
	case err != nil:
		return m.fail("check", fmt.Sprintf("Cannot read the model in '%s': %v. Nothing was changed.", project, err), errorCodeFromK8sErr(err), "model read failed")
	case !found:
		return m.fail("check", fmt.Sprintf("Project '%s' has no model of this tool. Set one up on the Models tab first. Nothing was changed.", project), "prerequisites", "no model")
	case !modelOwned(model.Metadata):
		return m.fail("check", fmt.Sprintf("The model in '%s' was not created by this tool, so it is not published. Nothing was changed.", project), "not_managed", "model foreign")
	}
	if cond, ok := findCondition(model.Status.Conditions, "Ready"); !ok || cond.Status != "True" {
		return m.fail("check", fmt.Sprintf("The model in '%s' is not ready yet. Publish it once it is. Nothing was changed.", project), "prerequisites", "model not ready")
	}
	ref, refFound, err := readMaaSModelRef(c, project)
	if err != nil {
		return m.fail("check", fmt.Sprintf("Cannot read MaaSModelRef %s/%s: %v. Nothing was changed.", project, maasModelRefName, err), errorCodeFromK8sErr(err), "ref read failed")
	}
	if refFound && !toolOwned(ref.Metadata) {
		return m.fail("check", fmt.Sprintf("Project '%s' already has a MaaSModelRef %s that this tool did not create; it is left alone. Nothing was changed.", project, maasModelRefName), "not_managed", "ref foreign")
	}
	rec, err := readMaaSRecord(c)
	if err != nil {
		return m.fail("check", err.Error()+" Nothing was changed.", errorCodeFromK8sErr(err), "record read failed")
	}
	rec.setPublished(project, true)
	infra := maasInfraNamespace(c)
	gwPlan, msg, code := planMaaSGateway(c, infra, rec.Published)
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
		if gw.gw.admits(project, labels) == "no" {
			return m.fail("check", fmt.Sprintf("Gateway %s/%s, which this tool did not create, does not admit routes from '%s'. Its owner adds the project (maas-setup.md: label it maas.opendatahub.io/gateway-access=true when the gateway selects that label). Nothing was changed.", inferenceGatewayNamespace, maasGatewayName, project), "prerequisites", "gateway does not admit")
		}
		gwPlan = nil
	}
	sub, auth := maasAccessObjects(rec.Published)
	refObj := maasModelRefObjectFor(project, firstNonEmpty(model.Metadata.Annotations["openshift.io/display-name"], maasModelRefName))
	if gwPlan != nil {
		if msg, code := m.applyGateway(gwPlan, true); msg != "" {
			return m.fail("check", msg, code, "gateway dry run failed")
		}
	}
	for _, w := range []struct {
		desc, path, create string
		obj                map[string]interface{}
	}{
		{"MaaSModelRef " + project + "/" + maasModelRefName, namespacedPath(maasAPIGroup, "maasmodelrefs", project, "qwen25-05b-instruct"), withToolFieldManager(namespacedPath(maasAPIGroup, "maasmodelrefs", project, "")), refObj},
		{"MaaSSubscription " + maasSubscriptionName, maasAccessPath("MaaSSubscription"), maasAccessCreatePath("MaaSSubscription"), sub},
		{"MaaSAuthPolicy " + maasAuthPolicyName, maasAccessPath("MaaSAuthPolicy"), maasAccessCreatePath("MaaSAuthPolicy"), auth},
	} {
		if err := applyMaaSObject(c, w.desc, w.path, w.create, w.obj, true); err != nil {
			return m.fail("check", fmt.Sprintf("The API server rejects %s: %v. Nothing was changed.", w.desc, err), maasWriteCode(err), "dry run failed")
		}
	}
	m.step("check", "success", "The platform is ready and the model can be published.")

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
	m.step("model", "running", "Routing the model through the MaaS gateway...")
	if err := setModelGatewayRef(c, project, model.Metadata.ResourceVersion, true); err != nil {
		return m.fail("model", fmt.Sprintf("Failed to route the model through the MaaS gateway: %v. Unpublish to undo.", err), maasWriteCode(err), "model patch failed")
	}
	m.step("model", "success", "The model routes through "+maasGatewayName+" as well as the inference gateway, so its address and the playground keep working.")
	m.step("maas", "running", "Creating the model reference, the subscription and the access policy...")
	for _, w := range []struct {
		desc, path, create string
		obj                map[string]interface{}
	}{
		{"MaaSModelRef " + project + "/" + maasModelRefName, namespacedPath(maasAPIGroup, "maasmodelrefs", project, "qwen25-05b-instruct"), withToolFieldManager(namespacedPath(maasAPIGroup, "maasmodelrefs", project, "")), refObj},
		{"MaaSSubscription " + maasSubscriptionName, maasAccessPath("MaaSSubscription"), maasAccessCreatePath("MaaSSubscription"), sub},
		{"MaaSAuthPolicy " + maasAuthPolicyName, maasAccessPath("MaaSAuthPolicy"), maasAccessCreatePath("MaaSAuthPolicy"), auth},
	} {
		if err := applyMaaSObject(c, w.desc, w.path, w.create, w.obj, false); err != nil {
			return m.fail("maas", fmt.Sprintf("Failed to apply %s: %v. Unpublish to undo.", w.desc, err), maasWriteCode(err), "apply failed")
		}
		m.logs = append(m.logs, "OK: "+w.desc)
	}
	m.step("maas", "success", fmt.Sprintf("MaaSModelRef %s/%s, MaaSSubscription %s and MaaSAuthPolicy %s are applied.", project, maasModelRefName, maasSubscriptionName, maasAuthPolicyName))
	m.step("ready", "running", "Waiting for MaaS to accept the model...")
	last := ""
	ready, err := waitForGone(c, MaaSPublishTimeout, MaaSPoll, func(cc *Client) (bool, error) {
		r, _, err := readMaaSModelRef(cc, project)
		if err != nil {
			return false, err
		}
		a, _ := maasPhase(cc, maasAccessPath("MaaSAuthPolicy"))
		s, _ := maasPhase(cc, maasAccessPath("MaaSSubscription"))
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
		return m.done(fmt.Sprintf("The model in '%s' is published and still being accepted.", project))
	}
	m.step("ready", "success", "MaaS lists the model.")
	return m.done(fmt.Sprintf("The model in '%s' is published to MaaS under subscription %s.", project, maasSubscriptionName))
}

// --- unpublish ---------------------------------------------------------------------

// unpublish removes the publication of the tool's model in project, in
// reverse order: the access policy and subscription entries (the objects
// when no other project is left), the model reference, the model's gateway
// ref, and the project from the gateway. stepped is false when it runs as
// part of a removal.
func (m *maasRun) unpublish(project string, stepped bool) (*types.OperationResponse, error) {
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
	ref, refFound, err := readMaaSModelRef(c, project)
	if err != nil {
		return m.fail("check", fmt.Sprintf("Cannot read MaaSModelRef %s/%s: %v. Nothing was changed.", project, maasModelRefName, err), errorCodeFromK8sErr(err), "ref read failed")
	}
	if refFound && !toolOwned(ref.Metadata) {
		return m.fail("check", fmt.Sprintf("MaaSModelRef %s/%s was not created by this tool; it is left alone.", project, maasModelRefName), "not_managed", "ref foreign")
	}
	if !refFound && !rec.published(project) {
		say("check", "success", "Not published.")
		return m.done(fmt.Sprintf("The model in '%s' is not published; nothing to undo.", project))
	}
	say("check", "success", "Unpublishing, in reverse order of publishing.")
	rec.setPublished(project, false)

	// The access policy and the subscription.
	say("maas", "running", "Removing the model from the access policy and the subscription...")
	sub, auth := maasAccessObjects(rec.Published)
	for _, w := range []struct {
		kind, desc string
		obj        map[string]interface{}
	}{{"MaaSAuthPolicy", "MaaSAuthPolicy " + maasAuthPolicyName, auth}, {"MaaSSubscription", "MaaSSubscription " + maasSubscriptionName, sub}} {
		path := maasAccessPath(w.kind)
		meta, found, err := readObjectMeta(c, path)
		if err != nil {
			return m.fail("maas", fmt.Sprintf("Cannot read %s: %v.", w.desc, err), errorCodeFromK8sErr(err), "read failed")
		}
		if !found || !toolOwned(meta) {
			continue
		}
		if len(rec.Published) == 0 {
			if _, err := deleteWithUID(c, path, meta.UID); err != nil && !IsK8sError(err, 404) {
				return m.fail("maas", fmt.Sprintf("Failed to delete %s: %v.", w.desc, err), maasWriteCode(err), "delete failed")
			}
			_, _ = waitForDeletion(c, path, MaaSDeleteTimeout, MaaSPoll)
			m.logs = append(m.logs, "OK: "+w.desc+" deleted")
			continue
		}
		if err := applyMaaSObject(c, w.desc, path, maasAccessCreatePath(w.kind), w.obj, false); err != nil {
			return m.fail("maas", fmt.Sprintf("Failed to update %s: %v.", w.desc, err), maasWriteCode(err), "apply failed")
		}
	}
	if refFound {
		path := namespacedPath(maasAPIGroup, "maasmodelrefs", project, "qwen25-05b-instruct")
		if _, err := deleteWithUID(c, path, ref.Metadata.UID); err != nil && !IsK8sError(err, 404) {
			return m.fail("maas", fmt.Sprintf("Failed to delete MaaSModelRef %s/%s: %v.", project, maasModelRefName, err), maasWriteCode(err), "delete failed")
		}
		if gone, err := waitForDeletion(c, path, MaaSDeleteTimeout, MaaSPoll); err != nil || !gone {
			return m.fail("maas", fmt.Sprintf("MaaSModelRef %s/%s is still being deleted (maas-controller removes its finalizer; it must be running). Unpublish again to check.", project, maasModelRefName), "in_progress", "ref still deleting")
		}
	}
	say("maas", "success", "The model reference is deleted, and the access policy and subscription no longer list the model.")

	// The model's gateway ref.
	say("model", "running", "Routing the model back through its own gateway...")
	model, found, err := readToolModel(c, project, maasModelRefName)
	if err != nil {
		return m.fail("model", fmt.Sprintf("Cannot read the model in '%s': %v.", project, err), errorCodeFromK8sErr(err), "model read failed")
	}
	if found && modelOwned(model.Metadata) {
		if err := setModelGatewayRef(c, project, model.Metadata.ResourceVersion, false); err != nil {
			return m.fail("model", fmt.Sprintf("Failed to remove the MaaS gateway from the model: %v. Unpublish again to retry.", err), maasWriteCode(err), "model patch failed")
		}
	}
	say("model", "success", "The model routes as before publishing.")

	// The project on the gateway.
	infra := maasInfraNamespace(c)
	if err := writeMaaSRecord(c, rec); err != nil {
		return m.fail("gateway", err.Error(), errorCodeFromK8sErr(err), "record write failed")
	}
	if gwPlan, msg, code := planMaaSGateway(c, infra, rec.Published); msg == "" && code != "foreign" && gwPlan.found["Gateway"] {
		gwPlan.classMissing = false // only the gateway changes
		if msg, code := m.applyGateway(gwPlan, false); msg != "" {
			return m.fail("gateway", msg, code, "gateway apply failed")
		}
	}
	say("gateway", "success", fmt.Sprintf("The MaaS gateway no longer admits routes from '%s'.", project))
	return m.done(fmt.Sprintf("The model in '%s' is unpublished from MaaS.", project))
}
