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
		if req.Model != "" {
			return "the platform takes no model"
		}
	case "publish", "unpublish":
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
	entry, _ := catalogEntry(req.Model)
	if req.Project != "" {
		m.target = fmt.Sprintf("model %s in %s", firstNonEmpty(entry.Name, req.Model), req.Project)
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
		return m.publish(req.Project, entry.Name)
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
	if cmOK && !toolOwned(cm) {
		return nil, fmt.Sprintf("%s exists and was not created by this tool. Nothing was changed.", maasDesc("ConfigMap", "")), "not_managed"
	}
	plan.current["ConfigMap"], plan.found["ConfigMap"] = cm, cmOK
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
	gwPlan, msg, code := planMaaSGateway(c, infra, rec.projects())
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
		if rec.DBNamespace != infra {
			rec.DBNamespace = infra
			if err := writeMaaSRecord(c, rec); err != nil {
				return m.fail("database", err.Error()+" Nothing was changed.", errorCodeFromK8sErr(err), "record write failed")
			}
		}
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
		_, written, _, err := ensureToolObject(c, maasDesc("Secret", infra), maasObjectPath("Secret", infra), maasCreatePath("Secret", infra), maasDBConfigObject(infra, connURL), cur, found, read, toolOwned, false)
		if err != nil {
			return m.fail("database", fmt.Sprintf("Failed to write Secret %s/%s: %v.", infra, maasDBConfigSecret, maasWriteCode(err)), maasWriteCode(err), "secret apply failed")
		}
		// maas-api reads the Secret when it starts: one already running
		// keeps the old connection until it restarts (maas-setup.md
		// "Restarting maas-api"). The API server keeps the resourceVersion
		// when an apply changes nothing.
		if changed := !found || written.ResourceVersion != cur.ResourceVersion; changed && dsc.maas == "Managed" {
			restarted, err := restartMaaSAPI(c, infra)
			if err != nil {
				return m.fail("database", fmt.Sprintf("Secret %s/%s was written, but maas-api could not be restarted to read it: %v. Restart it (oc rollout restart deployment/maas-api -n %s) or set up again.", infra, maasDBConfigSecret, err, infra), errorCodeFromK8sErr(err), "maas-api restart failed")
			}
			if restarted {
				m.step("database", "running", "maas-api restarts to read the new connection Secret.")
			}
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
	if missing, _, _ := authorinoTLS(c, kq.namespace); missing != "" {
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
		ai, _ := readyCondition(cc, namespacedPath(maasAPI(c), "aitenants", "ai-tenants", "models-as-a-service"))
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
	for _, kind := range []string{"Gateway", "ConfigMap", "Secret", "Cluster"} {
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
	for _, o := range objs[2:] {
		if _, res := del("database", o); res != nil {
			return res, nil
		}
	}
	if rec.DBNamespace != "" {
		rec.DBNamespace = ""
		if err := writeMaaSRecord(c, rec); err != nil {
			return m.fail("database", err.Error(), errorCodeFromK8sErr(err), "record write failed")
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

func maasModelRefObjectFor(c *Client, project, name, displayName string) map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": maasAPI(c),
		"kind":       "MaaSModelRef",
		"metadata": map[string]interface{}{
			"name": name, "namespace": project, "labels": toolLabels(),
			"annotations": map[string]interface{}{"openshift.io/display-name": displayName, "openshift.io/description": "Published by rhoai-nightly-updater"},
		},
		"spec": map[string]interface{}{"modelRef": map[string]interface{}{"kind": "LLMInferenceService", "name": name}},
	}
}

// maasAccessObjects are the tool's MaaSSubscription and MaaSAuthPolicy for
// the published models (the docs' free sample: system:authenticated, 100
// tokens a minute, priority 10).
func maasAccessObjects(c *Client, pubs []maasPub) (map[string]interface{}, map[string]interface{}) {
	var subRefs, authRefs []interface{}
	for _, p := range pubs {
		subRefs = append(subRefs, map[string]interface{}{"name": p.model, "namespace": p.project,
			"tokenRateLimits": []interface{}{map[string]interface{}{"limit": 100, "window": "1m"}}})
		authRefs = append(authRefs, map[string]interface{}{"name": p.model, "namespace": p.project})
	}
	group := map[string]interface{}{"groups": []interface{}{map[string]interface{}{"name": maasSubjectGroup}}, "users": []interface{}{}}
	sub := map[string]interface{}{
		"apiVersion": maasAPI(c),
		"kind":       "MaaSSubscription",
		"metadata": map[string]interface{}{"name": "rhoai-nightly-updater-subscription", "namespace": maasTenantNamespace, "labels": toolLabels(),
			"annotations": map[string]interface{}{"openshift.io/display-name": "rhoai-nightly-updater models", "openshift.io/description": "100 tokens a minute for every signed-in user"}},
		"spec": map[string]interface{}{"owner": group, "modelRefs": subRefs, "priority": 10},
	}
	auth := map[string]interface{}{
		"apiVersion": maasAPI(c),
		"kind":       "MaaSAuthPolicy",
		"metadata": map[string]interface{}{"name": "rhoai-nightly-updater-access", "namespace": maasTenantNamespace, "labels": toolLabels(),
			"annotations": map[string]interface{}{"openshift.io/display-name": "rhoai-nightly-updater models", "openshift.io/description": "Every signed-in user may use this tool's published models"}},
		"spec": map[string]interface{}{"modelRefs": authRefs, "subjects": group},
	}
	return sub, auth
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

func (m *maasRun) publish(project, name string) (*types.OperationResponse, error) {
	c := m.c
	m.step("check", "running", "Checking the platform and the model...")
	st, err := GetMaaSStatus(c)
	if err != nil {
		return m.fail("check", err.Error()+" Nothing was changed.", errorCodeFromK8sErr(err), "status failed")
	}
	if !st.Platform.Ready {
		return m.fail("check", "The MaaS platform is not ready ("+st.Platform.Message+"). Set it up first. Nothing was changed.", "prerequisites", "platform not ready")
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
	if cond, ok := findCondition(model.Status.Conditions, "Ready"); !ok || cond.Status != "True" {
		return m.fail("check", fmt.Sprintf("%s in '%s' is not ready yet. Publish it once it is. Nothing was changed.", name, project), "prerequisites", "model not ready")
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
	// gateway, else what this build recorded when it published, else (a
	// publication by an earlier build) what that one replaced.
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
	sub, auth := maasAccessObjects(c, live)
	// The catalog's display name, never the object's: MaaS shows it to
	// every signed-in user, and a project editor can change the annotation.
	entry, _ := catalogEntry(name)
	refObj := maasModelRefObjectFor(c, project, name, firstNonEmpty(entry.DisplayName, name))
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
	if msg, code := m.routeModelThroughMaaS(project, name, original); msg != "" {
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
	sub, auth := maasAccessObjects(c, live)
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
	return m.done(fmt.Sprintf("%s in '%s' is unpublished from MaaS.", name, project))
}
