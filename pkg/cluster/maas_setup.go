package cluster

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// setup sets the MaaS platform up in the docs' order, doing only what is
// missing, after checking every step (and dry-running every write it can)
// first: the docs' platform setup (platform-setup.md: Connectivity Link,
// its Kuadrant CR), then their MaaS setup (maas-setup.md: the database db
// asks for, when maas-db-config does not exist yet (maas_database.go);
// gateway; Authorino TLS; DataScienceCluster; validation). Steps: "check",
// "rhcl", "kuadrant", "cnpg", "database", "gateway", "authorino-tls",
// "dsc", "ready". Running it again continues where a failed run stopped.
func (m *maasRun) setup(db *types.MaaSDatabaseRequest) (*types.OperationResponse, error) {
	c := m.c
	m.step("check", "running", "Checking what the MaaS platform needs...")
	rec, err := readMaaSRecord(c)
	if err != nil {
		return m.fail("check", err.Error()+" Nothing was changed.", kube.ErrorCodeFromK8sErr(err), "record read failed")
	}
	dsc, err := readMaaSDSC(c)
	if err != nil {
		return m.fail("check", fmt.Sprintf("Cannot read the DataScienceCluster; nothing was changed: %v.", err), kube.ErrorCodeFromK8sErr(err), "dsc read failed")
	}
	needDSC := dsc.aigateway != "Managed" || dsc.maas != "Managed"
	switch {
	case dsc.read.Object == nil:
		return m.fail("check", "This cluster has no DataScienceCluster. A cluster-admin creates one on the Components page. Nothing was changed.", "prerequisites", "no dsc")
	case needDSC && dsc.argoApp != "":
		return m.fail("check", gitOpsRefusal("The DataScienceCluster", dsc.argoApp, "turning on Models as a Service")+" Set aigateway and aigateway.modelsAsAService to Managed in Git. Nothing was changed."+dsc.argo.unconfirmedNote(), kube.ErrorCodeGitOpsManaged, "dsc gitops")
	}

	// Connectivity Link and its Kuadrant CR.
	rhcl, err := readOperator(c, rhclOperator)
	if err != nil {
		return m.fail("check", fmt.Sprintf("Cannot read the installed operators, so this tool cannot tell whether Connectivity Link is installed; nothing was changed: %v.%s", err, templateHint(err)), kube.ErrorCodeFromK8sErr(err), "csv read failed")
	}
	kVersion, err := kuadrantServedVersion(c)
	if err != nil {
		return m.fail("check", fmt.Sprintf("Cannot read the Kuadrant API; nothing was changed: %v.", err), kube.ErrorCodeFromK8sErr(err), "kuadrant read failed")
	}
	// Kuadrant's API served without an rhcl-operator CSV counts as installed
	// only while its controller runs (CRDs stay after an uninstall).
	kRuns := kVersion != ""
	if kRuns && !rhcl.installed() {
		if kRuns, err = controllerDeployed(c, kuadrantController); err != nil {
			return m.fail("check", fmt.Sprintf("Cannot tell whether Kuadrant's operator runs; nothing was changed: %v.%s", err, templateHint(err)), kube.ErrorCodeFromK8sErr(err), "controller read failed")
		}
	}
	var rhclPlan *operatorInstallPlan
	if !rhcl.installed() && !kRuns && rhcl.pending() == nil {
		plan, msg, code := planOperatorInstall(c, rhclOperator)
		if msg != "" {
			return m.fail("check", msg, code, "rhcl plan failed")
		}
		rhclPlan = plan
	}
	if w := rhclWarning(rhcl.version); w != "" {
		m.step("check", "running", w)
	}
	kq, err := readKuadrant(c)
	if err != nil {
		return m.fail("check", fmt.Sprintf("Cannot read the Kuadrant CR; nothing was changed: %v.", err), kube.ErrorCodeFromK8sErr(err), "kuadrant read failed")
	}
	kuadrantNS := kube.FirstNonEmpty(kq.namespace, rhcl.csvNS, rhclOperator.namespace)

	// The infrastructure namespace and the database.
	infra, err := maasInfraNamespace(c)
	if err != nil {
		return m.fail("check", sentenceStart(err.Error())+". Nothing was changed.", "prerequisites", "infra namespace unknown")
	}
	_, infraFound, err := kube.ReadObjectMeta(c, "/api/v1/namespaces/"+infra)
	if err != nil {
		return m.fail("check", fmt.Sprintf("Cannot read namespace %s; nothing was changed: %v.%s", infra, err, templateHint(err)), kube.ErrorCodeFromK8sErr(err), "namespace read failed")
	}
	if _, ours := maasInfraNamespacePath(infra); !infraFound && !ours {
		return m.fail("check", fmt.Sprintf("Namespace %s, where maas-api and its database go, does not exist, and this tool creates only the AI gateway's own infrastructure namespaces. A cluster-admin creates it. Nothing was changed.", infra), "prerequisites", "no infra namespace")
	}
	if msg, code := maasNamespaceRefusal(c, infra); msg != "" {
		return m.fail("check", msg, code, "namespace not rhoai's")
	}
	// The database: the one already chosen (or someone else's
	// maas-db-config, used as it is), else the one db asks for.
	dbPlan, msg, code := planMaaSDatabase(c, rec, infra, infraFound, db, false)
	if msg != "" {
		return m.fail("check", msg, code, "database check failed")
	}

	// The gateway: one someone else made is used as it is, unless it
	// admits routes from every namespace.
	gwPlan, msg, code := planMaaSGateway(c, infra, rec.projects())
	foreignGW := code == "foreign"
	if msg != "" {
		return m.fail("check", msg, code, "gateway check failed")
	}

	// Every write as a server-side dry run first, where its namespace and
	// API exist already.
	if r, err := m.setupDryRuns(maasSetupChecks{rhclPlan: rhclPlan, kq: kq, kVersion: kVersion, kuadrantNS: kuadrantNS,
		infra: infra, infraFound: infraFound, dbPlan: dbPlan, gwPlan: gwPlan, foreignGW: foreignGW, needDSC: needDSC}); r != nil || err != nil {
		return r, err
	}
	m.step("check", "success", "Everything Set up changes is checked.")

	// 1. Connectivity Link (platform-setup.md, RHOAI tab).
	if r, err := m.setupRHCL(&rec, rhcl, rhclPlan, kVersion); r != nil || err != nil {
		return r, err
	}

	// 2. The Kuadrant CR.
	if r, err := m.setupKuadrant(&rec, &kq, kVersion, kuadrantNS); r != nil || err != nil {
		return r, err
	}

	// 3. CloudNativePG, when the database is a CloudNativePG one.
	if r, err := m.ensureCNPG(&rec, dbPlan); r != nil || err != nil {
		return r, err
	}

	// 4. The infrastructure namespace and the database (maas-setup.md
	// "Database Setup").
	if !infraFound {
		if r, err := m.createMaaSInfraNamespace(&rec, infra); r != nil || err != nil {
			return r, err
		}
	}
	if r, err := m.makeDatabase(&rec, dbPlan, dsc.maas == "Managed", false); r != nil || err != nil {
		return r, err
	}

	// 5. Create Gateway.
	if r, err := m.setupGateway(gwPlan, foreignGW); r != nil || err != nil {
		return r, err
	}

	// 6. Authorino TLS (setup-authorino-tls.sh), once Kuadrant has made
	// Authorino.
	if r, err := m.applyAuthorinoTLS(&rec, kq.namespace, "authorino-tls"); r != nil || err != nil {
		return r, err
	}

	// 7. Configure DataScienceCluster.
	if r, err := m.setupDSC(&rec, dsc, needDSC); r != nil || err != nil {
		return r, err
	}

	// 8. Validation.
	return m.waitPlatformReady(infra)
}

// maasSetupChecks is what setup's check step found, for the dry runs.
type maasSetupChecks struct {
	rhclPlan   *operatorInstallPlan
	kq         kuadrantState
	kVersion   string
	kuadrantNS string
	infra      string
	infraFound bool
	dbPlan     *maasDBPlan
	gwPlan     *maasGatewayPlan
	foreignGW  bool
	needDSC    bool
}

// setupDryRuns runs every write of Set up as a server-side dry run, where
// its namespace and API exist already. Nil, nil: all are accepted.
func (m *maasRun) setupDryRuns(k maasSetupChecks) (*types.OperationResponse, error) {
	c := m.c
	rhclPlan, kq, kVersion, kuadrantNS, infra, infraFound := k.rhclPlan, k.kq, k.kVersion, k.kuadrantNS, k.infra, k.infraFound
	dbPlan, gwPlan, foreignGW, needDSC := k.dbPlan, k.gwPlan, k.foreignGW, k.needDSC
	dry := func(what string, err error) (*types.OperationResponse, error) {
		return m.fail("check", fmt.Sprintf("The API server rejects %s: %v. Nothing was changed.%s", what, err, templateHint(err)), maasWriteCode(err), "dry run failed")
	}
	if rhclPlan != nil {
		if _, err := createOperatorObjects(c, rhclPlan, true); err != nil {
			return dry("Connectivity Link's install", err)
		}
	}
	if !kq.found && kVersion != "" {
		// Only where its namespace exists: Kuadrant's API can be served
		// (CRDs left by an uninstall) before the install makes the
		// namespace, and the API server refuses the dry run there (seen live).
		_, kNSFound, err := kube.ReadObjectMeta(c, "/api/v1/namespaces/"+kuadrantNS)
		if err != nil {
			return m.fail("check", fmt.Sprintf("Cannot read namespace %s; nothing was changed: %v.%s", kuadrantNS, err, templateHint(err)), kube.ErrorCodeFromK8sErr(err), "namespace read failed")
		}
		if kNSFound {
			if err := createKuadrant(c, kVersion, kuadrantNS, true); err != nil {
				return dry("the Kuadrant CR", err)
			}
		}
	}
	if !infraFound {
		if err := createInfraNamespace(c, infra, true); err != nil {
			return dry("namespace "+infra, err)
		}
	}
	if what, err := dbPlan.dryRun(c); err != nil {
		return m.fail("check", dbPlan.redact(fmt.Sprintf("The API server rejects %s: %v. Nothing was changed.%s", what, err, templateHint(err))), maasWriteCode(err), "dry run failed")
	}
	if !foreignGW {
		if msg, code := m.applyGateway(gwPlan, true); msg != "" {
			return m.fail("check", msg, code, "gateway dry run failed")
		}
	}
	if kq.found {
		if _, err := configureAuthorinoTLS(c, kq.namespace, true); err != nil && !errors.Is(err, errAuthorinoNotYet) {
			return dry("Authorino's TLS settings", err)
		}
	}
	if needDSC {
		if err := patchMaaSDSC(c, "Managed", "Managed", true); err != nil {
			return dry("the DataScienceCluster change", err)
		}
	}
	return nil, nil
}

// setupRHCL is Set up's step 1: install Connectivity Link, or wait for an
// install in progress. Nil, nil: done.
func (m *maasRun) setupRHCL(rec *maasRecord, rhcl operatorState, rhclPlan *operatorInstallPlan, kVersion string) (*types.OperationResponse, error) {
	c := m.c
	switch {
	case rhclPlan != nil:
		m.step("rhcl", "running", fmt.Sprintf("Installing Red Hat Connectivity Link %s (channel %s) from redhat-operators...", rhclPlan.head, rhclPlan.channel))
		rec.setShared("rhcl", fmt.Sprintf("Subscription %s/%s, channel %s", rhclOperator.namespace, rhclOperator.subName, rhclPlan.channel))
		if err := writeMaaSRecord(c, *rec); err != nil {
			return m.fail("rhcl", err.Error()+" Nothing was changed.", kube.ErrorCodeFromK8sErr(err), "record write failed")
		}
		made, err := createOperatorObjects(c, rhclPlan, false)
		for _, d := range made {
			m.logs = append(m.logs, "OK: "+d+" created")
		}
		if err != nil {
			return m.fail("rhcl", fmt.Sprintf("Failed to %v. Set up again to continue.", err), maasWriteCode(err), "rhcl install failed")
		}
		fallthrough
	case !rhcl.installed() && kVersion == "" && rhcl.pending() != nil:
		ns, name := rhclOperator.namespace, rhclOperator.subName
		if p := rhcl.pending(); p != nil {
			ns, name = p.Metadata.Namespace, p.Metadata.Name
		}
		if r, msg := m.waitOperator("rhcl", ns, name, rhclOperator.title); r != nil {
			return r, msg
		}
	case rhcl.installed():
		m.step("rhcl", "success", fmt.Sprintf("Red Hat Connectivity Link %s is installed%s.", rhcl.version, byWhom(rec.Shared["rhcl"] != "" && rhcl.sub != nil && toolOwned(rhcl.sub.Metadata))))
	default:
		m.step("rhcl", "success", "Kuadrant's API is served (installed without an rhcl-operator CSV); MaaS uses it as it is.")
	}
	return nil, nil
}

// setupKuadrant is Set up's step 2: the Kuadrant CR, and the wait until it
// is Ready (kq is updated with what the wait read). Nil, nil: go on.
func (m *maasRun) setupKuadrant(rec *maasRecord, kq *kuadrantState, kVersion, kuadrantNS string) (*types.OperationResponse, error) {
	c := m.c
	if kq.found {
		m.step("kuadrant", "success", fmt.Sprintf("Kuadrant %s/kuadrant exists%s.", kq.namespace, byWhom(rec.Shared["kuadrant"] != "" && toolOwned(kq.meta))))
	} else {
		m.step("kuadrant", "running", "Waiting for Connectivity Link's Kuadrant API...")
		version := kVersion
		if _, err := kube.WaitForGone(c, MaaSOperatorTimeout, MaaSPoll, func(cc *Client) (bool, error) {
			v, err := kuadrantServedVersion(cc)
			version = v
			return v != "", err
		}); err != nil || version == "" {
			return m.fail("kuadrant", fmt.Sprintf("Connectivity Link's Kuadrant API is not served after %s%s. Set up again to continue; the MaaS tab shows Connectivity Link's state.", clockText(MaaSOperatorTimeout), errSuffix(err)), kube.FirstNonEmpty(kube.ErrorCodeFromK8sErr(err), "not_ready"), "kuadrant api missing")
		}
		rec.setShared("kuadrant", "Kuadrant "+kuadrantNS+"/kuadrant")
		if err := writeMaaSRecord(c, *rec); err != nil {
			return m.fail("kuadrant", err.Error(), kube.ErrorCodeFromK8sErr(err), "record write failed")
		}
		m.step("kuadrant", "running", fmt.Sprintf("Creating the Kuadrant CR %s/kuadrant...", kuadrantNS))
		if err := createKuadrant(c, version, kuadrantNS, false); err != nil {
			return m.fail("kuadrant", fmt.Sprintf("Failed to create the Kuadrant CR %s/kuadrant: %v. Set up again to retry.", kuadrantNS, err), maasWriteCode(err), "kuadrant create failed")
		}
		m.logs = append(m.logs, "OK: Kuadrant "+kuadrantNS+"/kuadrant created")
	}
	m.step("kuadrant", "running", "Waiting for Kuadrant to be Ready (it starts Authorino and Limitador)...")
	ready, err := kube.WaitForGone(c, MaaSKuadrantTimeout, MaaSPoll, func(cc *Client) (bool, error) {
		k, err := readKuadrant(cc)
		if err == nil && k.found {
			*kq = k
		}
		return k.ready, err
	})
	switch {
	case err != nil:
		return m.fail("kuadrant", fmt.Sprintf("Stopped waiting for Kuadrant: %v.", err), kube.FirstNonEmpty(kube.ErrorCodeFromK8sErr(err), "cancelled"), "kuadrant wait failed")
	case !ready:
		m.step("kuadrant", "success", fmt.Sprintf("Kuadrant is not Ready after %s (%s); continuing.", clockText(MaaSKuadrantTimeout), kube.FirstNonEmpty(kq.message, "no message")))
	default:
		m.step("kuadrant", "success", "Kuadrant "+kq.namespace+"/kuadrant is Ready.")
	}
	return nil, nil
}

// createMaaSInfraNamespace creates the AI gateway's infrastructure
// namespace (Set up's step 4), recording it as this tool's unless someone
// made it meanwhile. Nil, nil: go on.
func (m *maasRun) createMaaSInfraNamespace(rec *maasRecord, infra string) (*types.OperationResponse, error) {
	c := m.c
	m.step("database", "running", fmt.Sprintf("Creating namespace %s, the AI gateway's infrastructure namespace, as the docs' setup-database.sh does...", infra))
	rec.InfraNamespace = infra
	if err := writeMaaSRecord(c, *rec); err != nil {
		return m.fail("database", err.Error(), kube.ErrorCodeFromK8sErr(err), "record write failed")
	}
	if err := createInfraNamespace(c, infra, false); err != nil && !kube.IsK8sConflict(err) {
		return m.fail("database", fmt.Sprintf("Failed to create namespace %s: %v. Set up again to retry.", infra, err), maasWriteCode(err), "namespace create failed")
	} else if kube.IsK8sConflict(err) {
		// Made meanwhile (by RHOAI, or someone): it is not this tool's.
		rec.InfraNamespace = ""
		if err := writeMaaSRecord(c, *rec); err != nil {
			return m.fail("database", err.Error(), kube.ErrorCodeFromK8sErr(err), "record write failed")
		}
		if msg, code := maasNamespaceRefusal(c, infra); msg != "" {
			return m.fail("database", msg, code, "namespace not rhoai's")
		}
	} else {
		m.logs = append(m.logs, "OK: Namespace "+infra+" created")
	}
	return nil, nil
}

// setupDSC is Set up's step 7: aigateway and modelsAsAService to Managed,
// recording the values before. Nil, nil: done.
func (m *maasRun) setupDSC(rec *maasRecord, dsc maasDSC, needDSC bool) (*types.OperationResponse, error) {
	c := m.c
	if !needDSC {
		m.step("dsc", "success", "aigateway and modelsAsAService are Managed"+byWhom(rec.DSCChanged)+".")
	} else {
		var fields []string
		if dsc.aigateway != "Managed" {
			fields = append(fields, "aigateway "+kube.FirstNonEmpty(dsc.aigateway, "not set"))
			rec.AIGatewayChanged, rec.PreviousAIGateway = true, dsc.aigateway
		}
		if dsc.maas != "Managed" {
			fields = append(fields, "modelsAsAService "+kube.FirstNonEmpty(dsc.maas, "not set"))
			rec.DSCChanged, rec.PreviousModelsAsAService = true, dsc.maas
		}
		m.step("dsc", "running", fmt.Sprintf("Setting aigateway and its modelsAsAService to Managed in DataScienceCluster %s (it was %s; removal restores that)...", dsc.name, strings.Join(fields, ", ")))
		if err := writeMaaSRecord(c, *rec); err != nil {
			return m.fail("dsc", err.Error()+" The DataScienceCluster was not changed.", kube.ErrorCodeFromK8sErr(err), "record write failed")
		}
		ag, maas := "", ""
		if dsc.aigateway != "Managed" {
			ag = "Managed"
		}
		if dsc.maas != "Managed" {
			maas = "Managed"
		}
		if err := patchMaaSDSC(c, ag, maas, false); err != nil {
			return m.fail("dsc", fmt.Sprintf("Failed to turn on MaaS in the DataScienceCluster: %v.", err), kube.ErrorCodeFromK8sErr(err), "dsc patch failed")
		}
		m.step("dsc", "success", "aigateway and modelsAsAService are Managed.")
	}
	return nil, nil
}

func byWhom(tool bool) string {
	if tool {
		return " (by this tool)"
	}
	return " (not by this tool)"
}

func errSuffix(err error) string {
	if err != nil {
		return ": " + err.Error()
	}
	return ""
}

// waitOperator waits for OLM to install a Subscription's CSV, reporting
// progress on step; it returns a failed response, or nil when installed.
func (m *maasRun) waitOperator(step, ns, name, title string) (*types.OperationResponse, error) {
	m.step(step, "running", fmt.Sprintf("Waiting for OLM to install %s (usually 1 to 3 minutes)...", title))
	p, err := waitForSubscription(m.c, ns, name, func(s string) { m.step(step, "running", s) })
	switch {
	case err != nil:
		return m.fail(step, fmt.Sprintf("Stopped waiting for %s: %v. Its Subscription stays; Set up again to continue.", title, err), kube.FirstNonEmpty(kube.ErrorCodeFromK8sErr(err), "cancelled"), "operator wait failed")
	case p.failed != "":
		return m.fail(step, fmt.Sprintf("%s does not install: %s. Its Subscription %s/%s stays; a cluster-admin checks it (oc describe subscription %s -n %s).", title, p.failed, ns, name, name, ns), "not_ready", "operator install failed")
	case !p.done:
		return m.fail(step, fmt.Sprintf("%s is not installed after %s (%s). Set up again to continue once it is.", title, clockText(MaaSOperatorTimeout), kube.FirstNonEmpty(p.text, "no progress")), "not_ready", "operator not ready")
	}
	m.step(step, "success", fmt.Sprintf("%s is installed (%s).", title, p.installedCSV))
	return nil, nil
}

// setupGateway is setup's gateway step (setup-gateway.sh loadbalancer mode).
func (m *maasRun) setupGateway(gwPlan *maasGatewayPlan, foreign bool) (*types.OperationResponse, error) {
	c := m.c
	if foreign {
		m.step("gateway", "success", fmt.Sprintf("Gateway %s/%s already exists and was not created by this tool: MaaS uses it as it is.", inferenceGatewayNamespace, maasGatewayName))
		return nil, nil
	}
	m.step("gateway", "running", "Creating Gateway "+maasGatewayName+" with an external load balancer...")
	if msg, code := m.applyGateway(gwPlan, false); msg != "" {
		return m.fail("gateway", msg, code, "gateway apply failed")
	}
	m.step("gateway", "running", "Waiting for the gateway to be programmed (up to "+clockText(MaaSGatewayTimeout)+")...")
	programmed, err := kube.WaitForGone(c, MaaSGatewayTimeout, MaaSPoll, func(cc *Client) (bool, error) {
		gw, err := readMaaSGateway(cc)
		return gw.programmed && gw.address != "", err
	})
	switch {
	case err != nil:
		return m.fail("gateway", fmt.Sprintf("Stopped waiting for the gateway: %v. It was created.", err), kube.FirstNonEmpty(kube.ErrorCodeFromK8sErr(err), "cancelled"), "gateway wait failed")
	case !programmed:
		m.step("gateway", "success", fmt.Sprintf("Created; not programmed after %s. It may still come up; the MaaS tab shows its state.", clockText(MaaSGatewayTimeout)))
	default:
		published, _ := kube.WaitForGone(c, InferenceGatewayDNSTimeout, MaaSPoll, func(cc *Client) (bool, error) { return dnsPublished(cc, gwPlan.host) == "published", nil })
		if published {
			m.step("gateway", "success", fmt.Sprintf("Programmed; https://%s is published in DNS.", gwPlan.host))
		} else {
			m.step("gateway", "success", fmt.Sprintf("Programmed; https://%s is not in DNS yet (a new load balancer's name can take a few minutes).", gwPlan.host))
		}
	}
	return nil, nil
}

// errAuthorinoNotYet: Kuadrant has not made Authorino's objects yet.
var errAuthorinoNotYet = errors.New("kuadrant has not made Authorino yet")

// applyAuthorinoTLS is the Authorino TLS step: it waits for Kuadrant's
// Authorino in ns, then configures what is missing and records that this
// tool did.
func (m *maasRun) applyAuthorinoTLS(rec *maasRecord, ns, step string) (*types.OperationResponse, error) {
	c := m.c
	if ns == "" {
		return m.fail(step, "There is no Kuadrant CR, so there is no Authorino to configure. Set up creates it.", "prerequisites", "no kuadrant")
	}
	if missing, known, _ := authorinoTLS(c, ns); missing == "" && known {
		m.step(step, "success", "Authorino in "+ns+" serves TLS and trusts the service CA"+byWhom(rec.Shared["authorino-tls"] != "")+".")
		return nil, nil
	}
	m.step(step, "running", "Configuring Authorino TLS in "+ns+" as the docs' setup-authorino-tls.sh does...")
	var changes []string
	var lastErr error
	done, err := kube.WaitForGone(c, MaaSKuadrantTimeout, MaaSPoll, func(cc *Client) (bool, error) {
		ch, err := configureAuthorinoTLS(cc, ns, false)
		changes = append(changes, ch...)
		lastErr = err
		if errors.Is(err, errAuthorinoNotYet) {
			return false, nil
		}
		return err == nil, err
	})
	if len(changes) > 0 {
		rec.setShared("authorino-tls", "Authorino in "+ns)
		if werr := writeMaaSRecord(c, *rec); werr != nil {
			m.logs = append(m.logs, "Authorino TLS was configured, but this tool could not record it: "+werr.Error())
		}
	}
	for _, ch := range changes {
		m.logs = append(m.logs, "OK: "+ch)
	}
	switch {
	case err != nil:
		return m.fail(step, fmt.Sprintf("Failed to configure Authorino TLS in %s: %v. Run it again to retry; the MaaS tab shows the docs' commands too.%s", ns, err, templateHint(err)), maasWriteCode(err), "authorino tls failed")
	case !done:
		return m.fail(step, fmt.Sprintf("Authorino in %s is not there after %s (%v). Run it again once Kuadrant is Ready.", ns, clockText(MaaSKuadrantTimeout), lastErr), "not_ready", "authorino missing")
	}
	if len(changes) == 0 {
		m.step(step, "success", "Authorino in "+ns+" already serves TLS and trusts the service CA.")
	} else {
		m.step(step, "success", "Authorino TLS is configured: "+strings.Join(changes, "; ")+".")
	}
	if len(changes) > 0 {
		// The script's closing note: maas-api picks the TLS setup up on
		// restart. None runs before MaaS is turned on.
		if infra, err := maasInfraNamespace(c); err != nil {
			m.logs = append(m.logs, "Warning: maas-api was not restarted to use Authorino's TLS: "+err.Error())
		} else if restarted, err := restartMaaSAPI(c, infra); err == nil && restarted {
			m.logs = append(m.logs, "OK: maas-api restarted to use Authorino's TLS")
		}
	}
	return nil, nil
}

// waitPlatformReady is setup's validation: maas-api, the tenant and the
// gateway's authentication.
func (m *maasRun) waitPlatformReady(infra string) (*types.OperationResponse, error) {
	c := m.c
	m.step("ready", "running", "Waiting for the operator: maas-api, the default tenant, and the gateway's authentication (usually 2 to 3 minutes)...")
	last := ""
	ready, err := kube.WaitForGone(c, MaaSPlatformTimeout, MaaSPoll, func(cc *Client) (bool, error) {
		api := deploymentAvailable(cc, infra, "maas-api")
		ai, _ := readyCondition(cc, rhoai.NamespacedPath(maasAPI(c), "aitenants", "ai-tenants", "models-as-a-service"))
		auth := maasGatewayAuth(cc)
		msg := fmt.Sprintf("maas-api: %s; tenant: %s; gateway authentication: %s", map[string]string{"True": "available", "False": "starting", "missing": "not created yet"}[api], kube.FirstNonEmpty(map[string]string{"True": "ready", "False": "not ready", "missing": "not created yet"}[ai], "unknown"), kube.FirstNonEmpty(auth, "unknown"))
		if msg != last {
			last = msg
			m.step("ready", "running", msg)
		}
		return api == "True" && ai == "True" && auth == "enforced", nil
	})
	if err != nil {
		return m.fail("ready", fmt.Sprintf("Stopped waiting: %v. Setup itself is done; the MaaS tab shows the platform's state.", err), kube.FirstNonEmpty(kube.ErrorCodeFromK8sErr(err), "cancelled"), "ready wait failed")
	}
	if !ready {
		m.step("ready", "success", fmt.Sprintf("Still starting after %s (%s). The MaaS tab shows each part.", clockText(MaaSPlatformTimeout), last))
		return m.done("The MaaS platform is set up and still starting.")
	}
	m.step("ready", "success", "maas-api is available, the tenant is ready, and the gateway checks every request.")
	return m.done("The MaaS platform is ready.")
}

// patchMaaSDSC sets aigateway.managementState and
// aigateway.modelsAsAService.managementState ("" leaves one as it is) as
// field manager rhoai-nightly-updater-maas-dsc: the API server then tells
// whether each value is still the tool's (dscFieldOwned). batchGateway and
// aiGrid, the AI gateway's other sub-components (rhods-operator rhoai-3.6
// api/components/v1alpha1/aigateway_types.go), are not MaaS's and stay as
// they are.
func patchMaaSDSC(c *Client, aigateway, maas string, dryRun bool) error {
	path, err := rhoai.DataScienceClusterPath(c)
	if err != nil {
		return err
	}
	ag := map[string]any{}
	if aigateway != "" {
		ag["managementState"] = aigateway
	}
	if maas != "" {
		ag["modelsAsAService"] = map[string]any{"managementState": maas}
	}
	patch, _ := json.Marshal(map[string]any{"spec": map[string]any{"components": map[string]any{"aigateway": ag}}})
	query := url.Values{"fieldManager": {maasDSCManager}}
	if dryRun {
		query.Set("dryRun", "All")
	}
	_, _, err = c.Do(http.MethodPatch, path, "application/merge-patch+json", patch, query)
	return err
}

// dscAIGatewayOwned: the tool's field manager still owns
// aigateway.managementState (nobody has set it since setup).
func dscAIGatewayOwned(d maasDSC) bool {
	raw, _ := json.Marshal(d.read.Object["metadata"])
	var meta kube.ObjectMeta
	_ = json.Unmarshal(raw, &meta)
	return meta.OwnsFieldPath([]string{maasDSCManager}, "f:spec", "f:components", "f:aigateway", "f:managementState")
}
