package cluster

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Stale admission webhooks and CRD conversion webhooks.
//
// A webhook configuration is stale only when every Service it calls is
// NotFound AND its producer is not coming back (RHOAI operator notes §3.4):
//   - OLM-owned configs (label olm.owner=<csv>): OLM recreates missing
//     webhooks while the CSV exists (the CSV goes Failed/ComponentUnhealthy,
//     then Pending/NeedsReinstall and is reinstalled; operator-lifecycle-
//     manager olm/operator.go updateInstallStatus) and deletes them with the
//     CSV (olm/operator.go, CSV deletion cleanup). So they are stale only
//     when the CSV is gone.
//   - Configs applied by the platform for a module (label
//     platform.opendatahub.io/part-of=<module>): rhods-operator re-applies
//     them while the module CR exists, so they are stale only when no CR of
//     that module exists.
//   - Anything else: the producer is unknown, so no automatic deletion.
// While the operator CSV is Pending, InstallReady, Installing or Replacing
// an upgrade is in flight and nothing is deleted.
// A Service with no ready endpoints is not checked: the app has no RBAC for
// EndpointSlices, and a restarting pod must not count as stale.

type admissionConfig struct {
	Kind     string `json:"-"`
	Resource string `json:"-"`
	Metadata struct {
		Name            string            `json:"name"`
		UID             string            `json:"uid"`
		ResourceVersion string            `json:"resourceVersion"`
		Labels          map[string]string `json:"labels"`
		OwnerReferences []struct {
			APIVersion string `json:"apiVersion"`
			Kind       string `json:"kind"`
			Name       string `json:"name"`
		} `json:"ownerReferences"`
	} `json:"metadata"`
	Webhooks []struct {
		Name          string  `json:"name"`
		FailurePolicy *string `json:"failurePolicy"`
		ClientConfig  struct {
			Service *struct {
				Namespace string `json:"namespace"`
				Name      string `json:"name"`
			} `json:"service"`
		} `json:"clientConfig"`
	} `json:"webhooks"`
}

func (a admissionConfig) ref() string { return a.Kind + " " + a.Metadata.Name }

func (a admissionConfig) isRHOAI() bool {
	labels := map[string]interface{}{}
	for k, v := range a.Metadata.Labels {
		labels[k] = v
	}
	if isRHOAIWebhook(a.Metadata.Name, labels) || a.Metadata.Labels["platform.opendatahub.io/part-of"] != "" {
		return true
	}
	for _, o := range a.Metadata.OwnerReferences {
		if strings.Contains(o.APIVersion, "opendatahub.io") {
			return true
		}
	}
	return false
}

func (a admissionConfig) services() []string {
	var out []string
	for _, wh := range a.Webhooks {
		if svc := wh.ClientConfig.Service; svc != nil && svc.Name != "" && svc.Namespace != "" {
			ref := svc.Namespace + "/" + svc.Name
			if !containsString(out, ref) {
				out = append(out, ref)
			}
		}
	}
	sort.Strings(out)
	return out
}

func (a admissionConfig) blocking() bool {
	for _, wh := range a.Webhooks {
		// admissionregistration/v1 defaults failurePolicy to Fail.
		if wh.FailurePolicy == nil || *wh.FailurePolicy == "Fail" {
			return true
		}
	}
	return false
}

type webhookVerdict struct {
	Config    admissionConfig
	Missing   []string // all referenced Services, all NotFound
	Deletable bool
	Reason    string // why it is (not) safe to delete
}

// webhookEnv caches lookups shared by all configs of one evaluation.
type webhookEnv struct {
	c           *Client
	services    map[string]error // nil = exists
	csvs        map[string]error
	modules     map[string]moduleState
	moduleKinds map[string]string // lowercase kind -> resource
	upgrading   string            // non-empty: CSV name and phase of an in-flight upgrade
}

type moduleState struct {
	exists bool
	err    error
}

func newWebhookEnv(c *Client) *webhookEnv {
	env := &webhookEnv{c: c, services: map[string]error{}, csvs: map[string]error{}, modules: map[string]moduleState{}}
	if csvs, err := listOperatorCSVs(c); err == nil {
		for _, csv := range csvs {
			switch csv.Phase {
			case "Pending", "InstallReady", "Installing", "Replacing":
				env.upgrading = fmt.Sprintf("%s is %s", csv.Name, csv.Phase)
			}
		}
	}
	return env
}

func (e *webhookEnv) service(ref string) error {
	if err, ok := e.services[ref]; ok {
		return err
	}
	ns, name, _ := strings.Cut(ref, "/")
	_, _, err := e.c.get(namespacedPath("v1", "services", ns, name))
	e.services[ref] = err
	return err
}

func (e *webhookEnv) csvExists(ns, name string) error {
	key := ns + "/" + name
	if err, ok := e.csvs[key]; ok {
		return err
	}
	_, _, err := e.c.get(namespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", ns, name))
	e.csvs[key] = err
	return err
}

// moduleCRExists reports whether any CR of the platform module exists, using
// discovery of components.platform.opendatahub.io to map the module name
// (the lower-case kind, e.g. kserve, aihub, ogx) to its resource.
func (e *webhookEnv) moduleCRExists(module string) moduleState {
	if st, ok := e.modules[module]; ok {
		return st
	}
	if e.moduleKinds == nil {
		e.moduleKinds = map[string]string{}
		body, _, err := e.c.get("/apis/components.platform.opendatahub.io/v1alpha1")
		if err != nil {
			st := moduleState{err: fmt.Errorf("discover module kinds: %w", err)}
			e.modules[module] = st
			e.moduleKinds = nil
			return st
		}
		var disc struct {
			Resources []struct {
				Name string `json:"name"`
				Kind string `json:"kind"`
			} `json:"resources"`
		}
		_ = json.Unmarshal(body, &disc)
		for _, r := range disc.Resources {
			if !strings.Contains(r.Name, "/") {
				e.moduleKinds[strings.ToLower(r.Kind)] = r.Name
			}
		}
	}
	resource, ok := e.moduleKinds[module]
	if !ok {
		st := moduleState{err: fmt.Errorf("no module kind %q is served", module)}
		e.modules[module] = st
		return st
	}
	body, _, err := e.c.get("/apis/components.platform.opendatahub.io/v1alpha1/" + resource)
	if err != nil {
		st := moduleState{err: err}
		e.modules[module] = st
		return st
	}
	var list struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		st := moduleState{err: err}
		e.modules[module] = st
		return st
	}
	st := moduleState{exists: len(list.Items) > 0}
	e.modules[module] = st
	return st
}

// evaluate returns nil when the config is not stale.
func (e *webhookEnv) evaluate(cfg admissionConfig) *webhookVerdict {
	svcs := cfg.services()
	if len(svcs) == 0 {
		return nil
	}
	for _, s := range svcs {
		// Any Service that exists, or that cannot be checked, means the
		// config is not (provably) stale. Deleting a config removes all of
		// its webhooks, so every Service must be gone.
		if err := e.service(s); !IsK8sError(err, http.StatusNotFound) {
			return nil
		}
	}
	v := &webhookVerdict{Config: cfg, Missing: svcs}
	labels := cfg.Metadata.Labels
	switch {
	case e.upgrading != "":
		v.Reason = "an operator upgrade is in progress (" + e.upgrading + "); OLM and the operator recreate webhooks when it finishes"
	case labels["olm.owner"] != "":
		ns := nonEmpty(labels["olm.owner.namespace"], SubNS)
		err := e.csvExists(ns, labels["olm.owner"])
		switch {
		case err == nil:
			v.Reason = fmt.Sprintf("its CSV %s still exists, so OLM recreates it; the operator pods are probably down (see Operator pods)", labels["olm.owner"])
		case IsK8sError(err, http.StatusNotFound):
			v.Deletable = true
			v.Reason = fmt.Sprintf("its CSV %s no longer exists", labels["olm.owner"])
		default:
			v.Reason = fmt.Sprintf("could not check its CSV %s: %v", labels["olm.owner"], err)
		}
	case labels["platform.opendatahub.io/part-of"] != "" && labels["platform.opendatahub.io/part-of"] != "platform":
		module := labels["platform.opendatahub.io/part-of"]
		st := e.moduleCRExists(module)
		switch {
		case st.err != nil:
			v.Reason = fmt.Sprintf("could not check whether module %s is installed: %v", module, st.err)
		case st.exists:
			v.Reason = fmt.Sprintf("module %s is installed, so the operator re-applies its Service and webhooks; check the module's pods", module)
		default:
			v.Deletable = true
			v.Reason = fmt.Sprintf("module %s has no CR any more (it was removed)", module)
		}
	default:
		v.Reason = "the tool cannot tell which operator owns it"
	}
	return v
}

func listAdmissionConfigs(c *Client) ([]admissionConfig, []string) {
	var out []admissionConfig
	var errs []string
	for _, wk := range webhookKinds {
		body, _, err := c.get(clusterPath("admissionregistration.k8s.io/v1", wk.resource, ""))
		if err != nil {
			errs = append(errs, fmt.Sprintf("list %s: %v", wk.resource, err))
			continue
		}
		var list struct {
			Items []admissionConfig `json:"items"`
		}
		if err := json.Unmarshal(body, &list); err != nil {
			errs = append(errs, fmt.Sprintf("parse %s: %v", wk.resource, err))
			continue
		}
		for _, item := range list.Items {
			item.Kind, item.Resource = wk.kind, wk.resource
			if item.isRHOAI() {
				out = append(out, item)
			}
		}
	}
	return out, errs
}

var webhookKinds = []struct{ kind, resource string }{
	{"ValidatingWebhookConfiguration", "validatingwebhookconfigurations"},
	{"MutatingWebhookConfiguration", "mutatingwebhookconfigurations"},
}

func findStaleWebhooks(c *Client) ([]webhookVerdict, []string) {
	configs, errs := listAdmissionConfigs(c)
	env := newWebhookEnv(c)
	// Look up all referenced Services concurrently; evaluate() then reads
	// the cache.
	var refs []string
	for _, cfg := range configs {
		for _, ref := range cfg.services() {
			if !containsString(refs, ref) {
				refs = append(refs, ref)
			}
		}
	}
	results := make([]error, len(refs))
	parallelFor(len(refs), 8, func(i int) {
		ns, name, _ := strings.Cut(refs[i], "/")
		_, _, results[i] = c.get(namespacedPath("v1", "services", ns, name))
	})
	for i, ref := range refs {
		env.services[ref] = results[i]
	}
	var out []webhookVerdict
	for _, cfg := range configs {
		if v := env.evaluate(cfg); v != nil {
			out = append(out, *v)
		}
	}
	return out, errs
}

type staleConversion struct {
	CRD            string
	Service        string
	StoredVersions []string
	Module         string // DSC component the Service name points to, if any
	ModuleState    string
}

// rhoaiCRDSelectors select the CRDs RHOAI installs: the operator's own
// (OLM package label, e.g. DSC and DSCI) and those the platform applies for
// modules. Listing all CRDs would download every schema (13 MB live).
var rhoaiCRDSelectors = []string{
	"operators.coreos.com/" + SubName + "." + SubNS,
	"platform.opendatahub.io/part-of",
}

func findStaleConversions(c *Client) ([]staleConversion, error) {
	type crdItem struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Spec struct {
			Conversion *struct {
				Strategy string `json:"strategy"`
				Webhook  *struct {
					ClientConfig struct {
						Service *struct {
							Namespace string `json:"namespace"`
							Name      string `json:"name"`
						} `json:"service"`
					} `json:"clientConfig"`
				} `json:"webhook"`
			} `json:"conversion"`
		} `json:"spec"`
		Status struct {
			StoredVersions []string `json:"storedVersions"`
		} `json:"status"`
	}
	var items []crdItem
	seen := map[string]bool{}
	for _, sel := range rhoaiCRDSelectors {
		body, _, err := c.do(http.MethodGet, "/apis/apiextensions.k8s.io/v1/customresourcedefinitions", "", nil, url.Values{"labelSelector": {sel}})
		if IsK8sError(err, http.StatusNotFound) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		var list struct {
			Items []crdItem `json:"items"`
		}
		if err := json.Unmarshal(body, &list); err != nil {
			return nil, fmt.Errorf("parse CRDs: %w", err)
		}
		for _, it := range list.Items {
			if !seen[it.Metadata.Name] {
				seen[it.Metadata.Name] = true
				items = append(items, it)
			}
		}
	}
	components := readDSCComponents(c)
	var out []staleConversion
	for _, crd := range items {
		conv := crd.Spec.Conversion
		if conv == nil || conv.Strategy != "Webhook" || conv.Webhook == nil || conv.Webhook.ClientConfig.Service == nil {
			continue
		}
		svc := conv.Webhook.ClientConfig.Service
		if !strings.HasPrefix(svc.Namespace, "redhat-ods") && !strings.Contains(crd.Metadata.Name, "opendatahub") {
			continue
		}
		_, _, err := c.get(namespacedPath("v1", "services", svc.Namespace, svc.Name))
		if !IsK8sError(err, http.StatusNotFound) {
			continue
		}
		sc := staleConversion{CRD: crd.Metadata.Name, Service: svc.Namespace + "/" + svc.Name, StoredVersions: crd.Status.StoredVersions}
		normalized := strings.ReplaceAll(svc.Name, "-", "")
		for name, state := range components {
			if len(name) > len(sc.Module) && strings.Contains(normalized, name) {
				sc.Module, sc.ModuleState = name, state
			}
		}
		out = append(out, sc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CRD < out[j].CRD })
	return out, nil
}

// readDSCComponents returns spec.components managementState by component
// name, or nil when there is no DataScienceCluster.
func readDSCComponents(c *Client) map[string]string {
	path, err := dataScienceClusterPath(c)
	if err != nil {
		return nil
	}
	body, _, err := c.get(path)
	if err != nil {
		return nil
	}
	var dsc struct {
		Spec struct {
			Components map[string]struct {
				ManagementState string `json:"managementState"`
			} `json:"components"`
		} `json:"spec"`
	}
	if json.Unmarshal(body, &dsc) != nil {
		return nil
	}
	out := map[string]string{}
	for k, v := range dsc.Spec.Components {
		out[k] = v.ManagementState
	}
	return out
}

func checkStaleWebhooks(c *Client) checkOutput {
	const name = "Stale webhooks"
	out := checkOutput{check: CheckResult{Name: name, Status: "pass"}}
	var conversions []staleConversion
	var convErr error
	convDone := make(chan struct{})
	go func() {
		defer close(convDone)
		conversions, convErr = findStaleConversions(c)
	}()
	stale, errs := findStaleWebhooks(c)
	<-convDone

	var deletable, guidance []webhookVerdict
	for _, v := range stale {
		if v.Deletable {
			deletable = append(deletable, v)
		} else {
			guidance = append(guidance, v)
		}
	}

	if len(deletable) > 0 {
		severity := "warning"
		var evidence, affected, lines []string
		for _, v := range deletable {
			if v.Config.blocking() {
				severity = "critical"
			}
			evidence = append(evidence, fmt.Sprintf("%s: Service %s not found; %s", v.Config.ref(), strings.Join(v.Missing, ", "), v.Reason))
			affected = append(affected, v.Config.ref())
			lines = append(lines, fmt.Sprintf("- %s (Service %s; %s)", v.Config.ref(), strings.Join(v.Missing, ", "), v.Reason))
		}
		out.problems = append(out.problems, Problem{
			ID:       "stale-webhooks",
			Severity: severity,
			Title:    fmt.Sprintf("%d leftover RHOAI webhook configuration(s) point to a Service that no longer exists", len(deletable)),
			Description: "The API server calls these webhooks, but nothing serves them and their owner is gone, so nothing will recreate them. " +
				"With failurePolicy Fail (the default), every matching request is rejected until the configuration is deleted.",
			Evidence: evidence,
			Fix:      "Delete these leftover webhook configurations. Only configurations whose Services are all missing and whose owner (CSV or module) no longer exists are deleted.",
			LearnMore: "OLM deletes the webhooks of a CSV when the CSV is deleted, and recreates them while the CSV exists. " +
				"rhods-operator re-applies a module's webhooks while the module exists. A configuration is a leftover only when both are gone.",
			AutoFixable:     true,
			AutoFixAction:   "delete-stale-webhooks",
			AffectedObjects: affected,
			ConfirmMessage: "This deletes these webhook configurations:\n" + strings.Join(lines, "\n") +
				"\n\nRight before deleting, each one is checked again (Services, owner, no operator upgrade in progress) and deleted only if it is unchanged since that check.",
			TechnicalCmd: "oc get validatingwebhookconfigurations,mutatingwebhookconfigurations -o custom-columns=NAME:.metadata.name,SERVICE:.webhooks[*].clientConfig.service.name",
		})
	}

	if len(guidance) > 0 {
		severity := "warning"
		var evidence, affected []string
		for _, v := range guidance {
			if v.Config.blocking() {
				severity = "critical"
			}
			evidence = append(evidence, fmt.Sprintf("%s: Service %s not found; not deleted automatically because %s", v.Config.ref(), strings.Join(v.Missing, ", "), v.Reason))
			affected = append(affected, v.Config.ref())
		}
		out.problems = append(out.problems, Problem{
			ID:       "webhook-service-missing",
			Severity: severity,
			Title:    fmt.Sprintf("%d RHOAI webhook configuration(s) call a missing Service", len(guidance)),
			Description: "Requests that these webhooks intercept are rejected while their Service is missing. Their owner still exists (or is unknown), " +
				"so deleting them would not help: the owner recreates them, or they may still be needed.",
			Evidence:        evidence,
			AffectedObjects: affected,
			Fix:             "Get the owner running again: check the Operator pods and RHOAI pods results and the operator logs. During an operator upgrade, wait for it to finish.",
			TechnicalCmd:    "oc get pods -n redhat-ods-operator; oc get pods -n redhat-ods-applications",
		})
	}

	if len(conversions) > 0 {
		var evidence, affected []string
		var hints []string
		for _, sc := range conversions {
			line := fmt.Sprintf("CRD %s: conversion Service %s not found (storedVersions %s)", sc.CRD, sc.Service, strings.Join(sc.StoredVersions, ", "))
			if sc.Module != "" {
				line += fmt.Sprintf("; the Service belongs to DSC component %s, which is %s", sc.Module, nonEmpty(sc.ModuleState, "not set"))
				if sc.ModuleState == "Removed" {
					hints = append(hints, sc.Module)
				}
			}
			evidence = append(evidence, line)
			affected = append(affected, "CustomResourceDefinition "+sc.CRD)
		}
		fix := "Bring back the Service that serves the conversion, usually by setting the component that owns it to Managed again"
		if len(hints) > 0 {
			fix += fmt.Sprintf(" (here: %s)", strings.Join(hints, ", "))
		}
		fix += ". Then, if you do not need the objects, delete them and set the component back to Removed. Do not delete namespaces that contain these objects before this is fixed."
		out.problems = append(out.problems, Problem{
			ID:       "stale-crd-conversion",
			Severity: "critical",
			Title:    fmt.Sprintf("%d CRD(s) use a conversion webhook whose Service is missing", len(conversions)),
			Description: "Reading or writing these objects at a version other than the one they are stored in fails, which also breaks garbage collection " +
				"and makes namespace deletion hang in Terminating. The tool does not change this automatically: switching the CRD to conversion strategy None changes how stored objects are read.",
			Evidence:        evidence,
			AffectedObjects: affected,
			Fix:             fix,
			LearnMore:       "OLM's own source notes that a conversion webhook without its Service makes all requests for the CRD's objects fail and \"ultimately breaks kubernetes garbage collection\" (operator-lifecycle-manager olm/operator.go).",
			TechnicalCmd:    "oc get crd -o custom-columns=NAME:.metadata.name,STRATEGY:.spec.conversion.strategy,SERVICE:.spec.conversion.webhook.clientConfig.service.name",
		})
	}

	var details []string
	switch {
	case len(stale) == 0 && len(conversions) == 0:
		details = append(details, "No RHOAI webhook or CRD conversion points to a missing Service")
	default:
		out.check.Status = "fail"
		details = append(details, fmt.Sprintf("%d webhook configuration(s) and %d CRD conversion(s) point to a missing Service", len(stale), len(conversions)))
	}
	if len(errs) > 0 {
		details = append(details, "could not check: "+strings.Join(errs, "; "))
		if out.check.Status == "pass" {
			out.check.Status = "warn"
		}
	}
	if convErr != nil {
		msg := fmt.Sprintf("CRD conversions not checked: %v", convErr)
		if IsK8sError(convErr, http.StatusForbidden) {
			msg = "CRD conversions not checked: this app cannot list CRDs (RBAC)"
		}
		details = append(details, msg)
		if out.check.Status == "pass" {
			out.check.Status = "warn"
		}
	}
	out.check.Detail = strings.Join(details, "; ")
	return out
}

// deleteExact deletes an object only if it still has the UID and
// resourceVersion that were inspected (DeleteOptions preconditions).
func deleteExact(c *Client, path, uid, resourceVersion string) error {
	opts := map[string]interface{}{"apiVersion": "v1", "kind": "DeleteOptions"}
	pre := map[string]string{}
	if uid != "" {
		pre["uid"] = uid
	}
	if resourceVersion != "" {
		pre["resourceVersion"] = resourceVersion
	}
	if len(pre) > 0 {
		opts["preconditions"] = pre
	}
	body, _ := json.Marshal(opts)
	_, _, err := c.do(http.MethodDelete, path, "application/json", body, nil)
	return err
}

// applyFixDeleteStaleWebhooks re-evaluates every RHOAI webhook configuration
// and deletes only the deletable stale ones, each guarded by its UID and
// resourceVersion, so a configuration that changed since the check is kept.
func applyFixDeleteStaleWebhooks(c *Client) (*types.OperationResponse, error) {
	stale, listErrs := findStaleWebhooks(c)
	var logs, deleted, failed, changed []string
	for _, v := range stale {
		if !v.Deletable {
			logs = append(logs, fmt.Sprintf("Kept %s: %s", v.Config.ref(), v.Reason))
			continue
		}
		path := clusterPath("admissionregistration.k8s.io/v1", v.Config.Resource, v.Config.Metadata.Name)
		err := deleteExact(c, path, v.Config.Metadata.UID, v.Config.Metadata.ResourceVersion)
		switch {
		case err == nil:
			deleted = append(deleted, v.Config.ref())
			logs = append(logs, fmt.Sprintf("Deleted %s (Service %s not found; %s)", v.Config.ref(), strings.Join(v.Missing, ", "), v.Reason))
		case IsK8sError(err, http.StatusNotFound):
			logs = append(logs, fmt.Sprintf("%s was already gone", v.Config.ref()))
		case IsK8sError(err, http.StatusConflict):
			changed = append(changed, v.Config.ref())
			logs = append(logs, fmt.Sprintf("Kept %s: it changed after it was checked", v.Config.ref()))
		default:
			failed = append(failed, v.Config.ref())
			logs = append(logs, fmt.Sprintf("Error: could not delete %s: %v", v.Config.ref(), err))
		}
	}
	for _, e := range listErrs {
		logs = append(logs, "Warning: "+e)
	}

	if len(deleted) > 0 {
		RecordActivity(c, types.ActivityEntry{
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			User:      getUser(c),
			Action:    "fix-delete-stale-webhooks",
			Detail:    "deleted leftover webhook configurations: " + strings.Join(deleted, ", "),
			Success:   len(failed) == 0,
		})
	}

	switch {
	case len(deleted) == 0 && len(failed) == 0 && len(changed) > 0:
		return &types.OperationResponse{Success: false, Message: fmt.Sprintf("%s changed after the check, so nothing was deleted. Run diagnostics again.", strings.Join(changed, ", ")), Logs: logs, ErrorCode: "conflict"}, nil
	case len(deleted) == 0 && len(failed) == 0:
		if len(listErrs) > 0 {
			return &types.OperationResponse{Success: false, Message: "Could not check webhooks: " + strings.Join(listErrs, "; "), Logs: logs}, nil
		}
		return nothingToDo("no leftover webhook configuration was found (a Service is back, its owner still exists, or an upgrade is in progress). Nothing was deleted.", logs), nil
	case len(deleted) == 0:
		return &types.OperationResponse{Success: false, Message: "Could not delete " + strings.Join(failed, ", "), Logs: logs}, nil
	case len(failed) > 0:
		return &types.OperationResponse{
			Success:   false,
			Message:   fmt.Sprintf("Deleted %s; could not delete %s.", strings.Join(deleted, ", "), strings.Join(failed, ", ")),
			Logs:      logs,
			ErrorCode: "partial_failure",
		}, nil
	}
	return &types.OperationResponse{
		Success: true,
		Message: fmt.Sprintf("Deleted %d leftover webhook configuration(s): %s.", len(deleted), strings.Join(deleted, ", ")),
		Logs:    logs,
	}, nil
}

// operatorCSV is a CSV of the rhods-operator package in SubNS.
type operatorCSV struct {
	Name    string
	Version string
	Phase   string
	Reason  string
	Message string
}

func listOperatorCSVs(c *Client) ([]operatorCSV, error) {
	body, _, err := c.get(namespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", SubNS, ""))
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				Version string `json:"version"`
			} `json:"spec"`
			Status struct {
				Phase   string `json:"phase"`
				Reason  string `json:"reason"`
				Message string `json:"message"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, err
	}
	var out []operatorCSV
	for _, it := range list.Items {
		if !strings.HasPrefix(it.Metadata.Name, SubName+".") {
			continue
		}
		out = append(out, operatorCSV{Name: it.Metadata.Name, Version: it.Spec.Version, Phase: it.Status.Phase, Reason: it.Status.Reason, Message: it.Status.Message})
	}
	return out, nil
}

// parallelFor runs fn(0..n-1) with at most limit calls at a time.
func parallelFor(n, limit int, fn func(i int)) {
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			fn(i)
		}(i)
	}
	wg.Wait()
}
