package serving

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/argo"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
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
func (m *Run) setup(db *types.MaaSDatabaseRequest) (*types.OperationResponse, error) {
	c := m.Client
	m.Step("check", "running", "Checking what the MaaS platform needs...")
	rec, err := ReadMaaSRecord(c)
	if err != nil {
		return m.Fail("check", err.Error()+" Nothing was changed.", kube.ErrorCodeFromK8sErr(err), "record read failed")
	}
	dsc, err := readMaaSDSC(c)
	if err != nil {
		return m.Fail("check", fmt.Sprintf("Cannot read the DataScienceCluster; nothing was changed: %v.", err), kube.ErrorCodeFromK8sErr(err), "dsc read failed")
	}
	needDSC := dsc.aigateway != "Managed" || dsc.maas != "Managed"
	switch {
	case dsc.read.Object == nil:
		return m.Fail("check", "This cluster has no DataScienceCluster. A cluster-admin creates one on the Components page. Nothing was changed.", "prerequisites", "no dsc")
	case needDSC && dsc.argoApp != "":
		return m.Fail("check", argo.GitOpsRefusal("The DataScienceCluster", dsc.argoApp, "turning on Models as a Service")+" Set aigateway and aigateway.modelsAsAService to Managed in Git. Nothing was changed."+dsc.argo.UnconfirmedNote(), kube.ErrorCodeGitOpsManaged, "dsc gitops")
	}

	// Connectivity Link and its Kuadrant CR.
	rhcl, err := envkit.ReadOperator(c, RHCLOperator)
	if err != nil {
		return m.Fail("check", fmt.Sprintf("Cannot read the installed operators, so this tool cannot tell whether Connectivity Link is installed; nothing was changed: %v.%s", err, kube.TemplateHint(err)), kube.ErrorCodeFromK8sErr(err), "csv read failed")
	}
	kVersion, err := KuadrantServedVersion(c)
	if err != nil {
		return m.Fail("check", fmt.Sprintf("Cannot read the Kuadrant API; nothing was changed: %v.", err), kube.ErrorCodeFromK8sErr(err), "kuadrant read failed")
	}
	// Kuadrant's API served without an rhcl-operator CSV counts as installed
	// only while its controller runs (CRDs stay after an uninstall).
	kRuns := kVersion != ""
	if kRuns && !rhcl.Installed() {
		if kRuns, err = envkit.ControllerDeployed(c, KuadrantController); err != nil {
			return m.Fail("check", fmt.Sprintf("Cannot tell whether Kuadrant's operator runs; nothing was changed: %v.%s", err, kube.TemplateHint(err)), kube.ErrorCodeFromK8sErr(err), "controller read failed")
		}
	}
	var rhclPlan *envkit.OperatorInstallPlan
	if !rhcl.Installed() && !kRuns && rhcl.Pending() == nil {
		plan, msg, code := envkit.PlanOperatorInstall(c, RHCLOperator)
		if msg != "" {
			return m.Fail("check", msg, code, "rhcl plan failed")
		}
		rhclPlan = plan
	}
	if w := RHCLWarning(rhcl.Version); w != "" {
		m.Step("check", "running", w)
	}
	kq, err := ReadKuadrant(c)
	if err != nil {
		return m.Fail("check", fmt.Sprintf("Cannot read the Kuadrant CR; nothing was changed: %v.", err), kube.ErrorCodeFromK8sErr(err), "kuadrant read failed")
	}
	kuadrantNS := kube.FirstNonEmpty(kq.Namespace, rhcl.CSVNS, RHCLOperator.Namespace)

	// The infrastructure namespace and the database.
	infra, err := MaaSInfraNamespace(c)
	if err != nil {
		return m.Fail("check", kube.SentenceStart(err.Error())+". Nothing was changed.", "prerequisites", "infra namespace unknown")
	}
	_, infraFound, err := kube.ReadObjectMeta(c, "/api/v1/namespaces/"+infra)
	if err != nil {
		return m.Fail("check", fmt.Sprintf("Cannot read namespace %s; nothing was changed: %v.%s", infra, err, kube.TemplateHint(err)), kube.ErrorCodeFromK8sErr(err), "namespace read failed")
	}
	if _, ours := maasInfraNamespacePath(infra); !infraFound && !ours {
		return m.Fail("check", fmt.Sprintf("Namespace %s, where maas-api and its database go, does not exist, and this tool creates only the AI gateway's own infrastructure namespaces. A cluster-admin creates it. Nothing was changed.", infra), "prerequisites", "no infra namespace")
	}
	if msg, code := MaaSNamespaceRefusal(c, infra); msg != "" {
		return m.Fail("check", msg, code, "namespace not rhoai's")
	}
	// The database: the one already chosen (or someone else's
	// maas-db-config, used as it is), else the one db asks for.
	dbPlan, msg, code := planMaaSDatabase(c, rec, infra, infraFound, db, false)
	if msg != "" {
		return m.Fail("check", msg, code, "database check failed")
	}

	// The gateway: one someone else made is used as it is, unless it
	// admits routes from every namespace.
	projects, err := maasLiveProjects(c, rec, MaaSPub{}, MaaSPub{})
	if err != nil {
		return m.Fail("check", fmt.Sprintf("Cannot check this tool's publications: %v. Nothing was changed.", err), kube.ErrorCodeFromK8sErr(err), "ref read failed")
	}
	gwPlan, msg, code := planMaaSGateway(c, infra, projects)
	foreignGW := code == "foreign"
	if msg != "" {
		return m.Fail("check", msg, code, "gateway check failed")
	}

	// Every write as a server-side dry run first, where its namespace and
	// API exist already.
	if r, err := m.setupDryRuns(maasSetupChecks{rhclPlan: rhclPlan, kq: kq, kVersion: kVersion, kuadrantNS: kuadrantNS,
		infra: infra, infraFound: infraFound, dbPlan: dbPlan, gwPlan: gwPlan, foreignGW: foreignGW, needDSC: needDSC}); r != nil || err != nil {
		return r, err
	}
	m.Step("check", "success", "Everything Set up changes is checked.")

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
		if r, err := m.CreateMaaSInfraNamespace(&rec, infra); r != nil || err != nil {
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
	if r, err := m.applyAuthorinoTLS(&rec, kq.Namespace, "authorino-tls", "Set up again to continue"); r != nil || err != nil {
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
	rhclPlan   *envkit.OperatorInstallPlan
	kq         kuadrantState
	kVersion   string
	kuadrantNS string
	infra      string
	infraFound bool
	dbPlan     *MaaSDBPlan
	gwPlan     *maasGatewayPlan
	foreignGW  bool
	needDSC    bool
}

// setupDryRuns runs every write of Set up as a server-side dry run, where
// its namespace and API exist already. Nil, nil: all are accepted.
func (m *Run) setupDryRuns(k maasSetupChecks) (*types.OperationResponse, error) {
	c := m.Client
	rhclPlan, kq, kVersion, kuadrantNS, infra, infraFound := k.rhclPlan, k.kq, k.kVersion, k.kuadrantNS, k.infra, k.infraFound
	dbPlan, gwPlan, foreignGW, needDSC := k.dbPlan, k.gwPlan, k.foreignGW, k.needDSC
	dry := func(what string, err error) (*types.OperationResponse, error) {
		return m.Fail("check", fmt.Sprintf("The API server rejects %s: %v. Nothing was changed.%s", what, err, kube.TemplateHint(err)), MaaSWriteCode(err), "dry run failed")
	}
	if rhclPlan != nil {
		if _, err := envkit.CreateOperatorObjects(c, rhclPlan, true); err != nil {
			return dry("Connectivity Link's install", err)
		}
	}
	if !kq.Found && kVersion != "" {
		// Only where its namespace exists: Kuadrant's API can be served
		// (CRDs left by an uninstall) before the install makes the
		// namespace, and the API server refuses the dry run there (seen live).
		_, kNSFound, err := kube.ReadObjectMeta(c, "/api/v1/namespaces/"+kuadrantNS)
		if err != nil {
			return m.Fail("check", fmt.Sprintf("Cannot read namespace %s; nothing was changed: %v.%s", kuadrantNS, err, kube.TemplateHint(err)), kube.ErrorCodeFromK8sErr(err), "namespace read failed")
		}
		if kNSFound {
			if err := CreateKuadrant(c, kVersion, kuadrantNS, true); err != nil {
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
		return m.Fail("check", dbPlan.Redact(fmt.Sprintf("The API server rejects %s: %v. Nothing was changed.%s", what, err, kube.TemplateHint(err))), MaaSWriteCode(err), "dry run failed")
	}
	if !foreignGW {
		if msg, code := m.applyGateway(gwPlan, true); msg != "" {
			return m.Fail("check", msg, code, "gateway dry run failed")
		}
	}
	if kq.Found {
		if _, err := configureAuthorinoTLS(c, kq.Namespace, true); err != nil && !errors.Is(err, errAuthorinoNotYet) {
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
func (m *Run) setupRHCL(rec *MaaSRecord, rhcl envkit.OperatorState, rhclPlan *envkit.OperatorInstallPlan, kVersion string) (*types.OperationResponse, error) {
	c := m.Client
	switch {
	case rhclPlan != nil:
		m.Step("rhcl", "running", fmt.Sprintf("Installing Red Hat Connectivity Link %s (channel %s) from redhat-operators...", rhclPlan.Head, rhclPlan.Channel))
		rec.setShared("rhcl", fmt.Sprintf("Subscription %s/%s, channel %s", RHCLOperator.Namespace, RHCLOperator.SubName, rhclPlan.Channel))
		if err := WriteMaaSRecord(c, *rec); err != nil {
			return m.Fail("rhcl", err.Error()+" Nothing was changed.", kube.ErrorCodeFromK8sErr(err), "record write failed")
		}
		made, err := envkit.CreateOperatorObjects(c, rhclPlan, false)
		for _, d := range made {
			m.Logs = append(m.Logs, "OK: "+d+" created")
		}
		if err != nil {
			return m.Fail("rhcl", fmt.Sprintf("Failed to %v. Set up again to continue.", err), MaaSWriteCode(err), "rhcl install failed")
		}
		fallthrough
	case !rhcl.Installed() && kVersion == "" && rhcl.Pending() != nil:
		ns, name := RHCLOperator.Namespace, RHCLOperator.SubName
		if p := rhcl.Pending(); p != nil {
			ns, name = p.Metadata.Namespace, p.Metadata.Name
		}
		if r, msg := m.WaitOperator("rhcl", ns, name, RHCLOperator.Title); r != nil {
			return r, msg
		}
	case rhcl.Installed():
		m.Step("rhcl", "success", fmt.Sprintf("Red Hat Connectivity Link %s is installed%s.", rhcl.Version, ByWhom(rec.Shared["rhcl"] != "" && rhcl.Sub != nil && ToolOwned(rhcl.Sub.Metadata))))
	default:
		m.Step("rhcl", "success", "Kuadrant's API is served (installed without an rhcl-operator CSV); MaaS uses it as it is.")
	}
	return nil, nil
}

// setupKuadrant is Set up's step 2: the Kuadrant CR, and the wait until it
// is Ready (kq is updated with what the wait read). Nil, nil: go on.
func (m *Run) setupKuadrant(rec *MaaSRecord, kq *kuadrantState, kVersion, kuadrantNS string) (*types.OperationResponse, error) {
	c := m.Client
	if kq.Found {
		m.Step("kuadrant", "success", fmt.Sprintf("Kuadrant %s/kuadrant exists%s.", kq.Namespace, ByWhom(rec.Shared["kuadrant"] != "" && ToolOwned(kq.Meta))))
	} else {
		m.Step("kuadrant", "running", "Waiting for Connectivity Link's Kuadrant API...")
		version := kVersion
		if _, err := kube.WaitForGone(c, envkit.OperatorTimeout, envkit.PollInterval, func(cc *kube.Client) (bool, error) {
			v, err := KuadrantServedVersion(cc)
			version = v
			return v != "", err
		}); err != nil || version == "" {
			return m.Fail("kuadrant", fmt.Sprintf("Connectivity Link's Kuadrant API is not served after %s%s. Set up again to continue; the MaaS tab shows Connectivity Link's state.", ClockText(envkit.OperatorTimeout), ErrSuffix(err)), kube.FirstNonEmpty(kube.ErrorCodeFromK8sErr(err), "not_ready"), "kuadrant api missing")
		}
		rec.setShared("kuadrant", "Kuadrant "+kuadrantNS+"/kuadrant")
		if err := WriteMaaSRecord(c, *rec); err != nil {
			return m.Fail("kuadrant", err.Error(), kube.ErrorCodeFromK8sErr(err), "record write failed")
		}
		if r, err := m.WaitKuadrantKindAdmissible("kuadrant"); r != nil || err != nil {
			return r, err
		}
		m.Step("kuadrant", "running", fmt.Sprintf("Creating the Kuadrant CR %s/kuadrant...", kuadrantNS))
		if err := CreateKuadrant(c, version, kuadrantNS, false); err != nil {
			return m.Fail("kuadrant", fmt.Sprintf("Failed to create the Kuadrant CR %s/kuadrant: %v. Set up again to retry.", kuadrantNS, err), MaaSWriteCode(err), "kuadrant create failed")
		}
		m.Logs = append(m.Logs, "OK: Kuadrant "+kuadrantNS+"/kuadrant created")
	}
	m.Step("kuadrant", "running", "Waiting for Kuadrant to be Ready (it starts Authorino and Limitador)...")
	ready, err := kube.WaitForGone(c, maasKuadrantTimeout, envkit.PollInterval, func(cc *kube.Client) (bool, error) {
		k, err := ReadKuadrant(cc)
		if err == nil && k.Found {
			*kq = k
		}
		return k.Ready, err
	})
	switch {
	case err != nil:
		return m.Fail("kuadrant", fmt.Sprintf("Stopped waiting for Kuadrant: %v.", err), kube.FirstNonEmpty(kube.ErrorCodeFromK8sErr(err), "cancelled"), "kuadrant wait failed")
	case !ready:
		// Without even the Authorino CR, the Authorino TLS step would only
		// wait as long again: say why now, before the steps after this.
		if kq.Found {
			dc, cancel := m.diagnosisClient()
			made, aerr := authorinoCRExists(dc, kq.Namespace)
			if aerr == nil && !made {
				msg := fmt.Sprintf("Stopped after waiting %s for Kuadrant to be Ready.", ClockText(maasKuadrantTimeout)) + kuadrantRecovery(dc, *kq, true, "Set up again to continue")
				cancel()
				return m.Fail("kuadrant", msg, "not_ready", "kuadrant not ready")
			}
			cancel()
		}
		m.Step("kuadrant", "success", fmt.Sprintf("Kuadrant is not Ready after %s (%s); continuing.", ClockText(maasKuadrantTimeout), kube.FirstNonEmpty(kq.readyText(), "no message")))
	default:
		m.Step("kuadrant", "success", "Kuadrant "+kq.Namespace+"/kuadrant is Ready.")
	}
	return nil, nil
}

// CreateMaaSInfraNamespace creates the AI gateway's infrastructure
// namespace (Set up's step 4), recording it as this tool's unless someone
// made it meanwhile. Nil, nil: go on.
func (m *Run) CreateMaaSInfraNamespace(rec *MaaSRecord, infra string) (*types.OperationResponse, error) {
	c := m.Client
	m.Step("database", "running", fmt.Sprintf("Creating namespace %s, the AI gateway's infrastructure namespace, as the docs' setup-database.sh does...", infra))
	rec.InfraNamespace = infra
	if err := WriteMaaSRecord(c, *rec); err != nil {
		return m.Fail("database", err.Error(), kube.ErrorCodeFromK8sErr(err), "record write failed")
	}
	if err := createInfraNamespace(c, infra, false); err != nil && !kube.IsK8sConflict(err) {
		return m.Fail("database", fmt.Sprintf("Failed to create namespace %s: %v. Set up again to retry.", infra, err), MaaSWriteCode(err), "namespace create failed")
	} else if kube.IsK8sConflict(err) {
		// Made meanwhile (by RHOAI, or someone): it is not this tool's.
		rec.InfraNamespace = ""
		if err := WriteMaaSRecord(c, *rec); err != nil {
			return m.Fail("database", err.Error(), kube.ErrorCodeFromK8sErr(err), "record write failed")
		}
		if msg, code := MaaSNamespaceRefusal(c, infra); msg != "" {
			return m.Fail("database", msg, code, "namespace not rhoai's")
		}
	} else {
		m.Logs = append(m.Logs, "OK: Namespace "+infra+" created")
	}
	return nil, nil
}

// setupDSC is Set up's step 7: aigateway and modelsAsAService to Managed,
// recording the values before. Nil, nil: done.
func (m *Run) setupDSC(rec *MaaSRecord, dsc maasDSC, needDSC bool) (*types.OperationResponse, error) {
	c := m.Client
	if !needDSC {
		m.Step("dsc", "success", "aigateway and modelsAsAService are Managed"+ByWhom(rec.DSCChanged)+".")
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
		m.Step("dsc", "running", fmt.Sprintf("Setting aigateway and its modelsAsAService to Managed in DataScienceCluster %s (it was %s; removal restores that)...", dsc.name, strings.Join(fields, ", ")))
		if err := WriteMaaSRecord(c, *rec); err != nil {
			return m.Fail("dsc", err.Error()+" The DataScienceCluster was not changed.", kube.ErrorCodeFromK8sErr(err), "record write failed")
		}
		ag, maas := "", ""
		if dsc.aigateway != "Managed" {
			ag = "Managed"
		}
		if dsc.maas != "Managed" {
			maas = "Managed"
		}
		if err := patchMaaSDSC(c, ag, maas, false); err != nil {
			return m.Fail("dsc", fmt.Sprintf("Failed to turn on MaaS in the DataScienceCluster: %v.", err), kube.ErrorCodeFromK8sErr(err), "dsc patch failed")
		}
		m.Step("dsc", "success", "aigateway and modelsAsAService are Managed.")
	}
	return nil, nil
}

func ByWhom(tool bool) string {
	if tool {
		return " (by this tool)"
	}
	return " (not by this tool)"
}

func ErrSuffix(err error) string {
	if err != nil {
		return ": " + err.Error()
	}
	return ""
}

// WaitOperator waits for OLM to install a Subscription's CSV, reporting
// progress on step; it returns a failed response, or nil when installed.
func (m *Run) WaitOperator(step, ns, name, title string) (*types.OperationResponse, error) {
	m.Step(step, "running", fmt.Sprintf("Waiting for OLM to install %s (usually 1 to 3 minutes)...", title))
	p, err := envkit.WaitForSubscription(m.Client, ns, name, func(s string) { m.Step(step, "running", s) })
	switch {
	case err != nil:
		return m.Fail(step, fmt.Sprintf("Stopped waiting for %s: %v. Its Subscription stays; Set up again to continue.", title, err), kube.FirstNonEmpty(kube.ErrorCodeFromK8sErr(err), "cancelled"), "operator wait failed")
	case p.Failed != "":
		return m.Fail(step, fmt.Sprintf("%s does not install: %s. Its Subscription %s/%s stays; a cluster-admin checks it (oc describe subscription %s -n %s).", title, p.Failed, ns, name, name, ns), "not_ready", "operator install failed")
	case !p.Done:
		return m.Fail(step, fmt.Sprintf("%s is not installed after %s (%s). Set up again to continue once it is.", title, ClockText(envkit.OperatorTimeout), kube.FirstNonEmpty(p.Text, "no progress")), "not_ready", "operator not ready")
	}
	m.Step(step, "success", fmt.Sprintf("%s is installed (%s).", title, p.InstalledCSV))
	return nil, nil
}

// setupGateway is setup's gateway step (setup-gateway.sh loadbalancer mode).
func (m *Run) setupGateway(gwPlan *maasGatewayPlan, foreign bool) (*types.OperationResponse, error) {
	c := m.Client
	if foreign {
		m.Step("gateway", "success", fmt.Sprintf("Gateway %s/%s already exists and was not created by this tool: MaaS uses it as it is.", inferenceGatewayNamespace, MaaSGatewayName))
		return nil, nil
	}
	m.Step("gateway", "running", "Creating Gateway "+MaaSGatewayName+" with an external load balancer...")
	if msg, code := m.applyGateway(gwPlan, false); msg != "" {
		return m.Fail("gateway", msg, code, "gateway apply failed")
	}
	m.Step("gateway", "running", "Waiting for the gateway to be programmed (up to "+ClockText(maasGatewayTimeout)+")...")
	programmed, err := kube.WaitForGone(c, maasGatewayTimeout, envkit.PollInterval, func(cc *kube.Client) (bool, error) {
		gw, err := readMaaSGateway(cc)
		return gw.programmed && gw.address != "", err
	})
	switch {
	case err != nil:
		return m.Fail("gateway", fmt.Sprintf("Stopped waiting for the gateway: %v. It was created.", err), kube.FirstNonEmpty(kube.ErrorCodeFromK8sErr(err), "cancelled"), "gateway wait failed")
	case !programmed:
		m.Step("gateway", "success", fmt.Sprintf("Created; not programmed after %s. It may still come up; the MaaS tab shows its state.", ClockText(maasGatewayTimeout)))
	default:
		published, _ := kube.WaitForGone(c, inferenceGatewayDNSTimeout, envkit.PollInterval, func(cc *kube.Client) (bool, error) { return dnsPublished(cc, gwPlan.host) == "published", nil })
		if published {
			m.Step("gateway", "success", fmt.Sprintf("Programmed; https://%s is published in DNS.", gwPlan.host))
		} else {
			m.Step("gateway", "success", fmt.Sprintf("Programmed; https://%s is not in DNS yet (a new load balancer's name can take a few minutes).", gwPlan.host))
		}
	}
	return nil, nil
}

// errAuthorinoNotYet: Kuadrant has not made Authorino's objects yet;
// errNoAuthorinoCR (with it): not even the Authorino CR.
var (
	errAuthorinoNotYet = errors.New("kuadrant has not made Authorino yet")
	errNoAuthorinoCR   = errors.New("no Authorino authorino")
)

// applyAuthorinoTLS is the Authorino TLS step: it waits for Kuadrant's
// Authorino in ns, then configures what is missing and records that this
// tool did. again is how the caller's user runs it again, after "Once
// Kuadrant is Ready, " ("Set up again to continue").
func (m *Run) applyAuthorinoTLS(rec *MaaSRecord, ns, step, again string) (*types.OperationResponse, error) {
	c := m.Client
	if ns == "" {
		return m.Fail(step, "There is no Kuadrant CR, so there is no Authorino to configure. Set up creates it.", "prerequisites", "no kuadrant")
	}
	if missing, known, _ := AuthorinoTLS(c, ns); missing == "" && known {
		m.Step(step, "success", "Authorino in "+ns+" serves TLS and trusts the service CA"+ByWhom(rec.Shared["authorino-tls"] != "")+".")
		return nil, nil
	}
	m.Step(step, "running", "Configuring Authorino TLS in "+ns+" as the docs' setup-authorino-tls.sh does...")
	var changes []string
	var lastErr error
	done, err := kube.WaitForGone(c, maasKuadrantTimeout, envkit.PollInterval, func(cc *kube.Client) (bool, error) {
		ch, err := configureAuthorinoTLS(cc, ns, false)
		changes = append(changes, ch...)
		if err != nil && cc.Context().Err() != nil {
			// The wait's end cut this poll off: what the polls before
			// found is the reason.
			return false, nil
		}
		lastErr = err
		if errors.Is(err, errAuthorinoNotYet) {
			return false, nil
		}
		return err == nil, err
	})
	if len(changes) > 0 {
		rec.setShared("authorino-tls", "Authorino in "+ns)
		if werr := WriteMaaSRecord(c, *rec); werr != nil {
			m.Logs = append(m.Logs, "Authorino TLS was configured, but this tool could not record it: "+werr.Error())
		}
	}
	for _, ch := range changes {
		m.Logs = append(m.Logs, "OK: "+ch)
	}
	switch {
	case err != nil && c.Context().Err() != nil:
		// The operation ended (its deadline, or the user) while Kuadrant
		// had not made Authorino.
		return m.Fail(step, m.authorinoMissing(fmt.Sprintf("Stopped waiting for Authorino in %s: %v; it is not there yet", ns, err), lastErr, again), kube.FirstNonEmpty(kube.ErrorCodeFromK8sErr(err), "cancelled"), "authorino wait failed")
	case err != nil:
		return m.Fail(step, fmt.Sprintf("Failed to configure Authorino TLS in %s: %v. Run it again to retry; the MaaS tab shows the docs' commands too.%s", ns, err, kube.TemplateHint(err)), MaaSWriteCode(err), "authorino tls failed")
	case !done:
		return m.Fail(step, m.authorinoMissing(fmt.Sprintf("Authorino in %s is not there after %s", ns, ClockText(maasKuadrantTimeout)), lastErr, again), "not_ready", "authorino missing")
	}
	if len(changes) == 0 {
		m.Step(step, "success", "Authorino in "+ns+" already serves TLS and trusts the service CA.")
	} else {
		m.Step(step, "success", "Authorino TLS is configured: "+strings.Join(changes, "; ")+".")
	}
	if len(changes) > 0 {
		// The script's closing note: maas-api picks the TLS setup up on
		// restart. None runs before MaaS is turned on.
		if infra, err := MaaSInfraNamespace(c); err != nil {
			m.Logs = append(m.Logs, "Warning: maas-api was not restarted to use Authorino's TLS: "+err.Error())
		} else if restarted, err := restartMaaSAPI(c, infra); err == nil && restarted {
			m.Logs = append(m.Logs, "OK: maas-api restarted to use Authorino's TLS")
		}
	}
	return nil, nil
}

// kuadrantDiagnosisWait bounds the reads that word why Authorino is not
// there, which run on their own context: the operation's may have ended.
const kuadrantDiagnosisWait = 30 * time.Second

// diagnosisClient is m's client on its own context, bounded by
// kuadrantDiagnosisWait, with the operation's values.
func (m *Run) diagnosisClient() (*kube.Client, context.CancelFunc) {
	ctx, cancel := kube.Clock().WithTimeout(context.WithoutCancel(m.Client.Context()), kuadrantDiagnosisWait)
	return m.Client.WithContext(ctx), cancel
}

// authorinoMissing words why Authorino is not there: lead says what
// the wait saw, last is what its last poll found, and the Kuadrant CR's
// Ready condition (kuadrantRecovery) says what to do.
func (m *Run) authorinoMissing(lead string, last error, again string) string {
	c, cancel := m.diagnosisClient()
	defer cancel()
	msg := fmt.Sprintf("%s (%s).", lead, kube.FirstNonEmpty(errText(last), "no answer"))
	k, err := ReadKuadrant(c)
	switch {
	case err != nil:
		return msg + fmt.Sprintf(" The Kuadrant CR cannot be read: %v. Once Kuadrant is Ready, %s.", err, again)
	case !k.Found:
		return msg + " There is no Kuadrant CR any more; Set up creates it."
	}
	return msg + kuadrantRecovery(c, k, errors.Is(last, errNoAuthorinoCR), again)
}

// kuadrantRecovery is what Kuadrant CR k says about an Authorino that is
// not there (noAuthorinoCR: not even its CR), and what to do; it starts with
// a space. It names an operator restart only for the reasons Kuadrant
// reports when Authorino or Limitador is missing from its topology, as when
// the API server refused them and the operator dropped its retry
// (WaitKuadrantKindAdmissible); not for MissingDependency, which Kuadrant
// reports first (no Gateway API provider yet, say), where a restart does
// not help. This tool restarts nothing itself.
func kuadrantRecovery(c *kube.Client, k kuadrantState, noAuthorinoCR bool, again string) string {
	made := ""
	if noAuthorinoCR {
		made = " and has not made Authorino"
	}
	why := kube.FirstNonEmpty(k.readyText(), "no reason given")
	switch {
	case k.Ready:
		return fmt.Sprintf(" Kuadrant %s/kuadrant is Ready; %s.", k.Namespace, again)
	case k.ReadyStatus != "False":
		return fmt.Sprintf(" Kuadrant %s/kuadrant is not Ready yet (%s)%s. Once Kuadrant is Ready, %s.", k.Namespace, kube.FirstNonEmpty(k.readyText(), "it reports no Ready condition"), made, again)
	case !noAuthorinoCR || (k.Reason != "LimitadorNotReady" && k.Reason != "AuthorinoNotReady"):
		return fmt.Sprintf(" Kuadrant %s/kuadrant is not Ready (%s)%s. Once Kuadrant is Ready, %s.", k.Namespace, why, made, again)
	}
	opNS, _, err := envkit.ControllerNamespace(c, KuadrantController)
	if err != nil || opNS == "" {
		opNS = RHCLOperator.Namespace
	}
	return fmt.Sprintf(" Kuadrant %s/kuadrant is not Ready (%s)%s; its operator can drop its own retry (Kuadrant/kuadrant-operator#2102), so a cluster-admin can restart it (oc rollout restart deployment/%s -n %s). Once Kuadrant is Ready, %s.", k.Namespace, why, made, KuadrantController, opNS, again)
}

// errText is err's text, "" for nil.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// waitPlatformReady is setup's validation: maas-api, the tenant and the
// gateway's authentication.
func (m *Run) waitPlatformReady(infra string) (*types.OperationResponse, error) {
	c := m.Client
	m.Step("ready", "running", "Waiting for the operator: maas-api, the default tenant, and the gateway's authentication (usually 2 to 3 minutes)...")
	last := ""
	ready, err := kube.WaitForGone(c, maasPlatformTimeout, envkit.PollInterval, func(cc *kube.Client) (bool, error) {
		api := deploymentAvailable(cc, infra, "maas-api")
		ai, _ := readyCondition(cc, rhoai.NamespacedPath(MaaSAPI(c), "aitenants", "ai-tenants", "models-as-a-service"))
		auth := MaaSGatewayAuth(cc)
		msg := fmt.Sprintf("maas-api: %s; tenant: %s; gateway authentication: %s", map[string]string{"True": "available", "False": "starting", "missing": "not created yet"}[api], kube.FirstNonEmpty(map[string]string{"True": "ready", "False": "not ready", "missing": "not created yet"}[ai], "unknown"), kube.FirstNonEmpty(auth, "unknown"))
		if msg != last {
			last = msg
			m.Step("ready", "running", msg)
		}
		return api == "True" && ai == "True" && auth == "enforced", nil
	})
	if err != nil {
		return m.Fail("ready", fmt.Sprintf("Stopped waiting: %v. Setup itself is done; the MaaS tab shows the platform's state.", err), kube.FirstNonEmpty(kube.ErrorCodeFromK8sErr(err), "cancelled"), "ready wait failed")
	}
	if !ready {
		m.Step("ready", "success", fmt.Sprintf("Still starting after %s (%s). The MaaS tab shows each part.", ClockText(maasPlatformTimeout), last))
		return m.Done("The MaaS platform is set up and still starting.")
	}
	m.Step("ready", "success", "maas-api is available, the tenant is ready, and the gateway checks every request.")
	return m.Done("The MaaS platform is ready.")
}

// patchMaaSDSC sets aigateway.managementState and
// aigateway.modelsAsAService.managementState ("" leaves one as it is) as
// field manager rhoai-nightly-updater-maas-dsc: the API server then tells
// whether each value is still the tool's (dscFieldOwned). batchGateway and
// aiGrid, the AI gateway's other sub-components (rhods-operator rhoai-3.6
// api/components/v1alpha1/aigateway_types.go), are not MaaS's and stay as
// they are.
func patchMaaSDSC(c *kube.Client, aigateway, maas string, dryRun bool) error {
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
