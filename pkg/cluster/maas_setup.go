package cluster

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// setup sets the MaaS platform up in the docs' order, doing only what is
// missing, after checking every step (and dry-running every write it can)
// first: the docs' platform setup (platform-setup.md: Connectivity Link,
// its Kuadrant CR), then their MaaS setup (maas-setup.md: database, with
// CloudNativePG for this tool's; gateway; Authorino TLS; DataScienceCluster;
// validation). Steps: "check", "rhcl", "kuadrant", "cnpg", "database",
// "gateway", "authorino-tls", "dsc", "ready". Running it again continues
// where a failed run stopped.
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
	needDSC := dsc.aigateway != "Managed" || dsc.maas != "Managed"
	switch {
	case dsc.read.Object == nil:
		return m.fail("check", "This cluster has no DataScienceCluster. A cluster-admin creates one on the Components page. Nothing was changed.", "prerequisites", "no dsc")
	case needDSC && dsc.argoApp != "":
		return m.fail("check", gitOpsRefusal("The DataScienceCluster", dsc.argoApp, "turning on Models as a Service")+" Set aigateway and aigateway.modelsAsAService to Managed in Git. Nothing was changed.", ErrorCodeGitOpsManaged, "dsc gitops")
	}

	// Connectivity Link and its Kuadrant CR.
	rhcl, err := readOperator(c, rhclOperator)
	if err != nil {
		return m.fail("check", fmt.Sprintf("Cannot read the installed operators, so this tool cannot tell whether Connectivity Link is installed; nothing was changed: %v.%s", err, templateHint(err)), errorCodeFromK8sErr(err), "csv read failed")
	}
	kVersion, err := kuadrantServedVersion(c)
	if err != nil {
		return m.fail("check", fmt.Sprintf("Cannot read the Kuadrant API; nothing was changed: %v.", err), errorCodeFromK8sErr(err), "kuadrant read failed")
	}
	var rhclPlan *operatorInstallPlan
	if !rhcl.installed() && kVersion == "" && rhcl.sub == nil {
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
		return m.fail("check", fmt.Sprintf("Cannot read the Kuadrant CR; nothing was changed: %v.", err), errorCodeFromK8sErr(err), "kuadrant read failed")
	}
	kuadrantNS := firstNonEmpty(kq.namespace, rhcl.csvNS, rhclOperator.namespace)

	// The infrastructure namespace and the database.
	infra := maasInfraNamespace(c)
	_, infraFound, err := readObjectMeta(c, "/api/v1/namespaces/"+infra)
	if err != nil {
		return m.fail("check", fmt.Sprintf("Cannot read namespace %s; nothing was changed: %v.%s", infra, err, templateHint(err)), errorCodeFromK8sErr(err), "namespace read failed")
	}
	if _, ours := maasInfraNamespacePath(infra); !infraFound && !ours {
		return m.fail("check", fmt.Sprintf("Namespace %s, where maas-api and its database go, does not exist, and this tool creates only the AI gateway's own infrastructure namespaces. A cluster-admin creates it. Nothing was changed.", infra), "prerequisites", "no infra namespace")
	}
	if msg, code := maasNamespaceRefusal(c, infra); msg != "" {
		return m.fail("check", msg, code, "namespace not rhoai's")
	}
	secMeta, secFound, err := readObjectMeta(c, maasObjectPath("Secret", infra))
	if err != nil {
		return m.fail("check", fmt.Sprintf("Cannot read Secret %s/%s; nothing was changed: %v.", infra, maasDBConfigSecret, err), errorCodeFromK8sErr(err), "secret read failed")
	}
	foreignDB := secFound && !toolOwned(secMeta)
	var cnpgPlan *operatorInstallPlan
	var cnpg operatorState
	cnpgServed := false
	var clMeta objectMeta
	var clFound bool
	if !foreignDB {
		if cnpgServed, err = groupServed(c, cnpgGroup); err != nil {
			return m.fail("check", fmt.Sprintf("Cannot tell whether CloudNativePG is installed; nothing was changed: %v.", err), errorCodeFromK8sErr(err), "cnpg read failed")
		}
		if !cnpgServed {
			if cnpg, err = readOperator(c, cnpgOperator); err != nil {
				return m.fail("check", fmt.Sprintf("Cannot read the installed operators; nothing was changed: %v.", err), errorCodeFromK8sErr(err), "csv read failed")
			}
			if !cnpg.installed() && cnpg.sub == nil {
				plan, msg, code := planOperatorInstall(c, cnpgOperator)
				if msg != "" {
					return m.fail("check", msg+" Or a cluster-admin creates Secret maas-db-config for a PostgreSQL of their own (maas-setup.md).", code, "cnpg plan failed")
				}
				cnpgPlan = plan
			}
		} else if infraFound {
			clMeta, clFound, _, _, err = readMaaSDBCluster(c, infra)
			if err != nil {
				return m.fail("check", fmt.Sprintf("Cannot read %s; nothing was changed: %v.", maasDesc("Cluster", infra), err), errorCodeFromK8sErr(err), "db read failed")
			}
			if clFound && !toolOwned(clMeta) {
				return m.fail("check", fmt.Sprintf("%s exists and was not created by this tool, so it is left alone. Create Secret %s/%s for it yourself, or remove it. Nothing was changed.", maasDesc("Cluster", infra), infra, maasDBConfigSecret), "not_managed", "db foreign")
			}
		}
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
	dry := func(what string, err error) (*types.OperationResponse, error) {
		return m.fail("check", fmt.Sprintf("The API server rejects %s: %v. Nothing was changed.%s", what, err, templateHint(err)), maasWriteCode(err), "dry run failed")
	}
	if rhclPlan != nil {
		if _, err := createOperatorObjects(c, rhclPlan, true); err != nil {
			return dry("Connectivity Link's install", err)
		}
	}
	if cnpgPlan != nil {
		if _, err := createOperatorObjects(c, cnpgPlan, true); err != nil {
			return dry("CloudNativePG's install", err)
		}
	}
	if !kq.found && kVersion != "" {
		if err := createKuadrant(c, kVersion, kuadrantNS, true); err != nil {
			return dry("the Kuadrant CR", err)
		}
	}
	if !infraFound {
		if err := createInfraNamespace(c, infra, true); err != nil {
			return dry("namespace "+infra, err)
		}
	}
	if !foreignDB && cnpgServed && infraFound {
		if _, _, _, err := ensureToolObject(c, maasDesc("Cluster", infra), maasObjectPath("Cluster", infra), maasCreatePath("Cluster", infra), maasDBClusterObject(infra), clMeta, clFound, func() (objectMeta, bool, error) { return readObjectMeta(c, maasObjectPath("Cluster", infra)) }, toolOwned, true); err != nil {
			return dry(maasDesc("Cluster", infra), err)
		}
		if _, _, _, err := ensureToolObject(c, maasDesc("Secret", infra), maasObjectPath("Secret", infra), maasCreatePath("Secret", infra), maasDBConfigObject(infra, "postgresql://dry-run@localhost/maas?sslmode=require"), secMeta, secFound, func() (objectMeta, bool, error) { return readObjectMeta(c, maasObjectPath("Secret", infra)) }, toolOwned, true); err != nil {
			return dry(maasDesc("Secret", infra), err)
		}
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
	m.step("check", "success", "Everything Set up changes is checked.")

	// 1. Connectivity Link (platform-setup.md, RHOAI tab).
	switch {
	case rhclPlan != nil:
		m.step("rhcl", "running", fmt.Sprintf("Installing Red Hat Connectivity Link %s (channel %s) from redhat-operators...", rhclPlan.head, rhclPlan.channel))
		rec.setShared("rhcl", fmt.Sprintf("Subscription %s/%s, channel %s", rhclOperator.namespace, rhclOperator.subName, rhclPlan.channel))
		if err := writeMaaSRecord(c, rec); err != nil {
			return m.fail("rhcl", err.Error()+" Nothing was changed.", errorCodeFromK8sErr(err), "record write failed")
		}
		made, err := createOperatorObjects(c, rhclPlan, false)
		for _, d := range made {
			m.logs = append(m.logs, "OK: "+d+" created")
		}
		if err != nil {
			return m.fail("rhcl", fmt.Sprintf("Failed to %v. Set up again to continue.", err), maasWriteCode(err), "rhcl install failed")
		}
		fallthrough
	case !rhcl.installed() && kVersion == "" && rhcl.sub != nil:
		ns, name := rhclOperator.namespace, rhclOperator.subName
		if rhcl.sub != nil {
			ns, name = rhcl.sub.Metadata.Namespace, rhcl.sub.Metadata.Name
		}
		if r, msg := m.waitOperator("rhcl", ns, name, rhclOperator.title); r != nil {
			return r, msg
		}
	case rhcl.installed():
		m.step("rhcl", "success", fmt.Sprintf("Red Hat Connectivity Link %s is installed%s.", rhcl.version, byWhom(rec.Shared["rhcl"] != "" && rhcl.sub != nil && toolOwned(rhcl.sub.Metadata))))
	default:
		m.step("rhcl", "success", "Kuadrant's API is served (installed without an rhcl-operator CSV); MaaS uses it as it is.")
	}

	// 2. The Kuadrant CR.
	if kq.found {
		m.step("kuadrant", "success", fmt.Sprintf("Kuadrant %s/kuadrant exists%s.", kq.namespace, byWhom(rec.Shared["kuadrant"] != "" && toolOwned(kq.meta))))
	} else {
		m.step("kuadrant", "running", "Waiting for Connectivity Link's Kuadrant API...")
		version := kVersion
		if _, err := waitForGone(c, MaaSOperatorTimeout, MaaSPoll, func(cc *Client) (bool, error) {
			v, err := kuadrantServedVersion(cc)
			version = v
			return v != "", err
		}); err != nil || version == "" {
			return m.fail("kuadrant", fmt.Sprintf("Connectivity Link's Kuadrant API is not served after %s%s. Set up again to continue; the MaaS tab shows Connectivity Link's state.", clockText(MaaSOperatorTimeout), errSuffix(err)), firstNonEmpty(errorCodeFromK8sErr(err), "not_ready"), "kuadrant api missing")
		}
		rec.setShared("kuadrant", "Kuadrant "+kuadrantNS+"/kuadrant")
		if err := writeMaaSRecord(c, rec); err != nil {
			return m.fail("kuadrant", err.Error(), errorCodeFromK8sErr(err), "record write failed")
		}
		m.step("kuadrant", "running", fmt.Sprintf("Creating the Kuadrant CR %s/kuadrant...", kuadrantNS))
		if err := createKuadrant(c, version, kuadrantNS, false); err != nil {
			return m.fail("kuadrant", fmt.Sprintf("Failed to create the Kuadrant CR %s/kuadrant: %v. Set up again to retry.", kuadrantNS, err), maasWriteCode(err), "kuadrant create failed")
		}
		m.logs = append(m.logs, "OK: Kuadrant "+kuadrantNS+"/kuadrant created")
	}
	m.step("kuadrant", "running", "Waiting for Kuadrant to be Ready (it starts Authorino and Limitador)...")
	ready, err := waitForGone(c, MaaSKuadrantTimeout, MaaSPoll, func(cc *Client) (bool, error) {
		k, err := readKuadrant(cc)
		if err == nil && k.found {
			kq = k
		}
		return k.ready, err
	})
	switch {
	case err != nil:
		return m.fail("kuadrant", fmt.Sprintf("Stopped waiting for Kuadrant: %v.", err), firstNonEmpty(errorCodeFromK8sErr(err), "cancelled"), "kuadrant wait failed")
	case !ready:
		m.step("kuadrant", "success", fmt.Sprintf("Kuadrant is not Ready after %s (%s); continuing.", clockText(MaaSKuadrantTimeout), firstNonEmpty(kq.message, "no message")))
	default:
		m.step("kuadrant", "success", "Kuadrant "+kq.namespace+"/kuadrant is Ready.")
	}

	// 3. CloudNativePG, for this tool's database.
	switch {
	case foreignDB:
		m.step("cnpg", "success", "Not needed: maas-db-config names a database of its own.")
	case cnpgServed:
		m.step("cnpg", "success", "CloudNativePG is installed"+byWhom(rec.Shared["cnpg"] != "")+".")
	default:
		ns, name := cnpgOperator.namespace, cnpgOperator.subName
		if cnpgPlan != nil {
			m.step("cnpg", "running", fmt.Sprintf("Installing CloudNativePG %s (channel %s) from certified-operators...", cnpgPlan.head, cnpgPlan.channel))
			rec.setShared("cnpg", fmt.Sprintf("Subscription %s/%s, channel %s", cnpgOperator.namespace, cnpgOperator.subName, cnpgPlan.channel))
			if err := writeMaaSRecord(c, rec); err != nil {
				return m.fail("cnpg", err.Error(), errorCodeFromK8sErr(err), "record write failed")
			}
			made, err := createOperatorObjects(c, cnpgPlan, false)
			for _, d := range made {
				m.logs = append(m.logs, "OK: "+d+" created")
			}
			if err != nil {
				return m.fail("cnpg", fmt.Sprintf("Failed to %v. Set up again to continue.", err), maasWriteCode(err), "cnpg install failed")
			}
		} else if cnpg.sub != nil {
			ns, name = cnpg.sub.Metadata.Namespace, cnpg.sub.Metadata.Name
		}
		if cnpgPlan != nil || cnpg.sub != nil {
			if r, msg := m.waitOperator("cnpg", ns, name, cnpgOperator.title); r != nil {
				return r, msg
			}
		}
		served, err := waitForGone(c, MaaSOperatorTimeout, MaaSPoll, func(cc *Client) (bool, error) { return groupServed(cc, cnpgGroup) })
		if err != nil || !served {
			return m.fail("cnpg", fmt.Sprintf("CloudNativePG's API (postgresql.cnpg.io) is not served after %s%s. Set up again to continue.", clockText(MaaSOperatorTimeout), errSuffix(err)), firstNonEmpty(errorCodeFromK8sErr(err), "not_ready"), "cnpg api missing")
		}
		m.step("cnpg", "success", "CloudNativePG runs.")
	}

	// 4. The infrastructure namespace and the database (maas-setup.md
	// "Database Setup").
	if !infraFound {
		m.step("database", "running", fmt.Sprintf("Creating namespace %s, the AI gateway's infrastructure namespace, as the docs' setup-database.sh does...", infra))
		rec.InfraNamespace = infra
		if err := writeMaaSRecord(c, rec); err != nil {
			return m.fail("database", err.Error(), errorCodeFromK8sErr(err), "record write failed")
		}
		if err := createInfraNamespace(c, infra, false); err != nil && !IsK8sError(err, 409) {
			return m.fail("database", fmt.Sprintf("Failed to create namespace %s: %v. Set up again to retry.", infra, err), maasWriteCode(err), "namespace create failed")
		} else if IsK8sError(err, 409) {
			// Made meanwhile (by RHOAI, or someone): it is not this tool's.
			rec.InfraNamespace = ""
			if err := writeMaaSRecord(c, rec); err != nil {
				return m.fail("database", err.Error(), errorCodeFromK8sErr(err), "record write failed")
			}
			if msg, code := maasNamespaceRefusal(c, infra); msg != "" {
				return m.fail("database", msg, code, "namespace not rhoai's")
			}
		} else {
			m.logs = append(m.logs, "OK: Namespace "+infra+" created")
		}
	}
	if foreignDB {
		m.step("database", "success", fmt.Sprintf("Secret %s/%s already exists and was not created by this tool: maas-api uses the database it names, and this tool does not change it.", infra, maasDBConfigSecret))
	} else {
		if !infraFound || !cnpgServed {
			// Read now that the namespace and CloudNativePG's API exist.
			clMeta, clFound, _, _, err = readMaaSDBCluster(c, infra)
			if err != nil {
				return m.fail("database", fmt.Sprintf("Cannot read %s: %v.", maasDesc("Cluster", infra), err), errorCodeFromK8sErr(err), "db read failed")
			}
			if clFound && !toolOwned(clMeta) {
				return m.fail("database", fmt.Sprintf("%s exists and was not created by this tool, so it is left alone. Create Secret %s/%s for it yourself, or remove it.", maasDesc("Cluster", infra), infra, maasDBConfigSecret), "not_managed", "db foreign")
			}
		}
		if r, err := m.setupDatabase(&rec, infra, clMeta, clFound, dsc.maas == "Managed"); r != nil || err != nil {
			return r, err
		}
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
	if !needDSC {
		m.step("dsc", "success", "aigateway and modelsAsAService are Managed"+byWhom(rec.DSCChanged)+".")
	} else {
		var fields []string
		if dsc.aigateway != "Managed" {
			fields = append(fields, "aigateway "+firstNonEmpty(dsc.aigateway, "not set"))
			rec.AIGatewayChanged, rec.PreviousAIGateway = true, dsc.aigateway
		}
		if dsc.maas != "Managed" {
			fields = append(fields, "modelsAsAService "+firstNonEmpty(dsc.maas, "not set"))
			rec.DSCChanged, rec.PreviousModelsAsAService = true, dsc.maas
		}
		m.step("dsc", "running", fmt.Sprintf("Setting aigateway and its modelsAsAService to Managed in DataScienceCluster %s (it was %s; removal restores that)...", dsc.name, strings.Join(fields, ", ")))
		if err := writeMaaSRecord(c, rec); err != nil {
			return m.fail("dsc", err.Error()+" The DataScienceCluster was not changed.", errorCodeFromK8sErr(err), "record write failed")
		}
		ag, maas := "", ""
		if dsc.aigateway != "Managed" {
			ag = "Managed"
		}
		if dsc.maas != "Managed" {
			maas = "Managed"
		}
		if err := patchMaaSDSC(c, ag, maas, false); err != nil {
			return m.fail("dsc", fmt.Sprintf("Failed to turn on MaaS in the DataScienceCluster: %v.", err), errorCodeFromK8sErr(err), "dsc patch failed")
		}
		m.step("dsc", "success", "aigateway and modelsAsAService are Managed.")
	}

	// 8. Validation.
	return m.waitPlatformReady(infra)
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
		return m.fail(step, fmt.Sprintf("Stopped waiting for %s: %v. Its Subscription stays; Set up again to continue.", title, err), firstNonEmpty(errorCodeFromK8sErr(err), "cancelled"), "operator wait failed")
	case p.failed != "":
		return m.fail(step, fmt.Sprintf("%s does not install: %s. Its Subscription %s/%s stays; a cluster-admin checks it (oc describe subscription %s -n %s).", title, p.failed, ns, name, name, ns), "not_ready", "operator install failed")
	case !p.done:
		return m.fail(step, fmt.Sprintf("%s is not installed after %s (%s). Set up again to continue once it is.", title, clockText(MaaSOperatorTimeout), firstNonEmpty(p.text, "no progress")), "not_ready", "operator not ready")
	}
	m.step(step, "success", fmt.Sprintf("%s is installed (%s).", title, p.installedCSV))
	return nil, nil
}

// setupDatabase is setup's database step: the CloudNativePG cluster, then
// maas-db-config with its address.
func (m *maasRun) setupDatabase(rec *maasRecord, infra string, clMeta objectMeta, clFound, maasOn bool) (*types.OperationResponse, error) {
	c := m.c
	m.step("database", "running", fmt.Sprintf("Creating a one-instance CloudNativePG database %s/%s (for development and test)...", infra, maasDBCluster))
	if rec.DBNamespace != infra {
		rec.DBNamespace = infra
		if err := writeMaaSRecord(c, *rec); err != nil {
			return m.fail("database", err.Error(), errorCodeFromK8sErr(err), "record write failed")
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
	// maas-api reads the Secret when it starts: one already running keeps
	// the old connection until it restarts (maas-setup.md "Restarting
	// maas-api"). The API server keeps the resourceVersion when an apply
	// changes nothing.
	if changed := !found || written.ResourceVersion != cur.ResourceVersion; changed && maasOn {
		restarted, err := restartMaaSAPI(c, infra)
		if err != nil {
			return m.fail("database", fmt.Sprintf("Secret %s/%s was written, but maas-api could not be restarted to read it: %v. Restart it (oc rollout restart deployment/maas-api -n %s) or set up again.", infra, maasDBConfigSecret, err, infra), errorCodeFromK8sErr(err), "maas-api restart failed")
		}
		if restarted {
			m.step("database", "running", "maas-api restarts to read the new connection Secret.")
		}
	}
	m.step("database", "success", fmt.Sprintf("The database runs, and Secret %s/%s holds its connection URL (sslmode=require).", infra, maasDBConfigSecret))
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
	done, err := waitForGone(c, MaaSKuadrantTimeout, MaaSPoll, func(cc *Client) (bool, error) {
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
	if infra := maasInfraNamespace(c); len(changes) > 0 {
		// The script's closing note: maas-api picks the TLS setup up on
		// restart. None runs before MaaS is turned on.
		if restarted, err := restartMaaSAPI(c, infra); err == nil && restarted {
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
	path, err := dataScienceClusterPath(c)
	if err != nil {
		return err
	}
	ag := map[string]interface{}{}
	if aigateway != "" {
		ag["managementState"] = aigateway
	}
	if maas != "" {
		ag["modelsAsAService"] = map[string]interface{}{"managementState": maas}
	}
	patch, _ := json.Marshal(map[string]interface{}{"spec": map[string]interface{}{"components": map[string]interface{}{"aigateway": ag}}})
	query := url.Values{"fieldManager": {maasDSCManager}}
	if dryRun {
		query.Set("dryRun", "All")
	}
	_, _, err = c.do(http.MethodPatch, path, "application/merge-patch+json", patch, query)
	return err
}

// dscAIGatewayOwned: the tool's field manager still owns
// aigateway.managementState (nobody has set it since setup).
func dscAIGatewayOwned(d maasDSC) bool {
	raw, _ := json.Marshal(d.read.Object["metadata"])
	var meta objectMeta
	_ = json.Unmarshal(raw, &meta)
	return meta.ownsFieldPath([]string{maasDSCManager}, "f:spec", "f:components", "f:aigateway", "f:managementState")
}
