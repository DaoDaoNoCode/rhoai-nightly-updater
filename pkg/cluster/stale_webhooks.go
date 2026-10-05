package cluster

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Stale admission webhooks and CRD conversion webhooks: the one
// implementation used by diagnostics (report and the delete-stale-webhooks
// fix), Reinstall (cleanup after the CSV is deleted) and the MinIO teardown
// guard (namespace deletion). It follows RHOAI_OPERATOR_NOTES §3.4.
//
// A webhook configuration is stale only when ALL of these hold:
//  1. Not one of its webhooks can be served: every Service it calls is
//     NotFound, or exists but has had no ready endpoint for longer than
//     staleServiceGrace (a restarting pod is not staleness). Deleting a
//     configuration removes all of its webhooks, so one serving Service is
//     enough to keep it.
//  2. Its producer is not coming back:
//     - olm.owner=<rhods-operator CSV>: that CSV is NotFound or being
//       deleted. While the CSV exists OLM heals its configs itself: a
//       missing one sends the CSV Failed (ComponentUnhealthy), then
//       Pending (NeedsReinstall), and the reinstall recreates it; deleting
//       the CSV deletes them (operator-lifecycle-manager olm/operator.go
//       updateInstallStatus and the csv-cleanup finalizer; notes §3.2).
//     - platform.opendatahub.io/part-of=<module>, or an ownerReference to a
//       components.platform.opendatahub.io module CR: no CR of that module
//       exists. rhods-operator re-applies a module's webhooks and Services
//       while its CR exists (notes §2.3, §3.2).
//     - part-of=platform, or owned by the Platform CR (live:
//       odh-observability-webhook, workbenches-operator-...): rhods-operator
//       applies these for the whole platform. They are reported with that
//       reason and never deleted automatically.
//     - anything else: the producer is unknown, so the configuration is
//       reported but never deleted automatically.
//  3. No rhods-operator CSV is Pending, InstallReady, Installing or
//     Replacing, i.e. no install or upgrade is in flight. During one,
//     nothing is evaluated (and so nothing is reported or deleted): Services
//     and configs come and go as part of the normal upgrade. If the CSV list
//     cannot be read, nothing is deletable.
//  4. The configuration itself is older than staleServiceGrace, so a
//     producer that is creating it together with its Service is not raced.
//
// Name matching (opendatahub/rhods) only selects which configurations are
// looked at; it is never the staleness test (notes §3.4: 12 live configs
// match by name and all are healthy).
//
// Callers and their policies:
//   - Diagnostics reports every non-serving configuration; the fix deletes
//     the deletable ones, re-evaluated right before deleting, each with a
//     uid/resourceVersion precondition.
//   - Reinstall runs the same evaluation and deletion after it has removed
//     the Subscription and the CSV. OLM's csv-cleanup finalizer normally
//     deletes the old CSV's configs itself; the step only removes leftovers
//     (configs of CSVs that are gone, or of removed modules). Because a CSV
//     with a deletionTimestamp can never be reinstalled, rule 2 treats it as
//     gone, which is what lets Reinstall clean up while its own CSV
//     deletion is still finishing. No reinstall-only exception is needed.
//   - The MinIO namespace guard only reads CRD conversion webhooks (below),
//     with no grace period: a namespace delete must not start while any
//     conversion webhook is down, even briefly.
//
// CRD conversion webhooks (spec.conversion.strategy Webhook) are judged by
// rule 1 only. They are never changed automatically: switching a CRD to
// strategy None changes how stored objects are read (notes §6.2). A dead
// one breaks reads at non-storage versions, garbage collection and
// namespace deletion (operator-lifecycle-manager olm/operator.go, quoted in
// notes §3.3, D3).

// staleServiceGrace is how long a Service must have had no ready endpoint,
// and how old a webhook configuration must be, before it counts as stale.
var staleServiceGrace = 5 * time.Minute

// webhookNow is the clock for grace periods (tests replace it).
var webhookNow = time.Now

type serviceState int

const (
	// serviceServing: a ready endpoint exists, or the Service cannot be
	// proven down (no selector, ExternalName, endpoints changed within the
	// grace period).
	serviceServing serviceState = iota
	// serviceMissing: the Service is NotFound.
	serviceMissing
	// serviceNoEndpoints: the Service exists but has had no ready endpoint
	// for at least the grace period.
	serviceNoEndpoints
	// serviceUnknown: a lookup failed; callers must not treat it as down.
	serviceUnknown
)

type serviceHealth struct {
	state serviceState
	// exists is true when the Service was read; with serviceUnknown it
	// means only its endpoints could not be checked.
	exists bool
	err    error
}

func (h serviceHealth) down() bool {
	return h.state == serviceMissing || h.state == serviceNoEndpoints
}

// describe renders a down Service for evidence and messages.
func (h serviceHealth) describe(ref string) string {
	switch h.state {
	case serviceMissing:
		return ref + " not found"
	case serviceNoEndpoints:
		return ref + " has no ready endpoints"
	case serviceUnknown:
		return fmt.Sprintf("%s could not be checked (%v)", ref, h.err)
	}
	return ref + " is serving"
}

// checkServiceHealth reads one Service and, when it exists, its
// EndpointSlices. grace 0 counts a Service without ready endpoints as down
// at once (the namespace guard).
func checkServiceHealth(c *Client, ref string, grace time.Duration) serviceHealth {
	ns, name, _ := strings.Cut(ref, "/")
	body, _, err := c.get(namespacedPath("v1", "services", ns, name))
	if IsK8sError(err, http.StatusNotFound) {
		return serviceHealth{state: serviceMissing}
	}
	if err != nil {
		return serviceHealth{state: serviceUnknown, err: err}
	}
	var svc struct {
		Metadata struct {
			CreationTimestamp string `json:"creationTimestamp"`
		} `json:"metadata"`
		Spec struct {
			Type string `json:"type"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(body, &svc); err != nil {
		return serviceHealth{state: serviceUnknown, err: fmt.Errorf("parse Service %s: %w", ref, err)}
	}
	// An ExternalName Service is resolved through DNS; the tool cannot
	// prove it does not serve.
	if svc.Spec.Type == "ExternalName" {
		return serviceHealth{state: serviceServing, exists: true}
	}
	body, _, err = c.get(namespacedPath("discovery.k8s.io/v1", "endpointslices", ns, "") + "?labelSelector=" + url.QueryEscape("kubernetes.io/service-name="+name))
	if err != nil {
		return serviceHealth{state: serviceUnknown, exists: true, err: fmt.Errorf("list endpoints of Service %s: %w", ref, err)}
	}
	var slices struct {
		Items []struct {
			Metadata struct {
				CreationTimestamp string            `json:"creationTimestamp"`
				Annotations       map[string]string `json:"annotations"`
				ManagedFields     []struct {
					Time string `json:"time"`
				} `json:"managedFields"`
			} `json:"metadata"`
			Endpoints []struct {
				Conditions struct {
					// A nil ready condition means ready (EndpointConditions API).
					Ready *bool `json:"ready"`
				} `json:"conditions"`
			} `json:"endpoints"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &slices); err != nil {
		return serviceHealth{state: serviceUnknown, exists: true, err: fmt.Errorf("parse endpoints of Service %s: %w", ref, err)}
	}
	// The last time the endpoints changed: the newest of the Service and
	// slice creation times, the EndpointSlice controller's
	// endpoints.kubernetes.io/last-change-trigger-time annotation and the
	// slices' managedFields update times (live on OCP 4.22).
	last, _ := parseK8sTime(svc.Metadata.CreationTimestamp)
	for _, s := range slices.Items {
		for _, ep := range s.Endpoints {
			if ep.Conditions.Ready == nil || *ep.Conditions.Ready {
				return serviceHealth{state: serviceServing, exists: true}
			}
		}
		times := []string{s.Metadata.CreationTimestamp, s.Metadata.Annotations["endpoints.kubernetes.io/last-change-trigger-time"]}
		for _, mf := range s.Metadata.ManagedFields {
			times = append(times, mf.Time)
		}
		for _, ts := range times {
			if t, ok := parseK8sTime(ts); ok && t.After(last) {
				last = t
			}
		}
	}
	if grace > 0 && (last.IsZero() || webhookNow().Sub(last) < grace) {
		// Too recent (or of unknown age) to tell a restart from an outage.
		return serviceHealth{state: serviceServing, exists: true}
	}
	return serviceHealth{state: serviceNoEndpoints, exists: true}
}

// serviceHealthCache checks each Service once per evaluation, concurrently.
type serviceHealthCache struct {
	c       *Client
	grace   time.Duration
	results map[string]serviceHealth
}

func newServiceHealthCache(c *Client, grace time.Duration) *serviceHealthCache {
	return &serviceHealthCache{c: c, grace: grace, results: map[string]serviceHealth{}}
}

// prefetch checks the given Services concurrently (at most 8 at a time).
func (s *serviceHealthCache) prefetch(refs []string) {
	var todo []string
	for _, ref := range refs {
		if _, ok := s.results[ref]; !ok && !containsString(todo, ref) {
			todo = append(todo, ref)
		}
	}
	out := make([]serviceHealth, len(todo))
	parallelFor(len(todo), 8, func(i int) { out[i] = checkServiceHealth(s.c, todo[i], s.grace) })
	for i, ref := range todo {
		s.results[ref] = out[i]
	}
}

func (s *serviceHealthCache) lookup(ref string) serviceHealth {
	if h, ok := s.results[ref]; ok {
		return h
	}
	h := checkServiceHealth(s.c, ref, s.grace)
	s.results[ref] = h
	return h
}

// --- Admission webhook configurations ---

type admissionConfig struct {
	Kind     string `json:"-"`
	Resource string `json:"-"`
	Metadata struct {
		Name              string            `json:"name"`
		UID               string            `json:"uid"`
		ResourceVersion   string            `json:"resourceVersion"`
		CreationTimestamp string            `json:"creationTimestamp"`
		Labels            map[string]string `json:"labels"`
		OwnerReferences   []struct {
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

// isRHOAI selects the configurations to evaluate: OLM-installed for the
// RHOAI CSV, applied by the platform for a module, owned by an RHOAI object,
// or carrying an RHOAI name without any OLM owner (older installs).
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

// blocking reports whether any webhook rejects requests when it cannot be
// called.
func (a admissionConfig) blocking() bool {
	for _, wh := range a.Webhooks {
		// admissionregistration/v1 defaults failurePolicy to Fail.
		if wh.FailurePolicy == nil || *wh.FailurePolicy == "Fail" {
			return true
		}
	}
	return false
}

// isRHOAIWebhook reports whether a webhook configuration was installed by OLM
// for the RHOAI operator CSV, or carries an RHOAI name without any OLM owner
// label (a leftover from older installs). It only selects candidates; the
// staleness rules above decide.
func isRHOAIWebhook(name string, labels map[string]interface{}) bool {
	if rhoaiOLMOwner(labels) != "" {
		return true
	}
	if owner, _ := labels["olm.owner"].(string); owner != "" {
		// Owned by a different operator's CSV.
		return false
	}
	return strings.Contains(name, "opendatahub") || strings.Contains(name, "rhods")
}

// rhoaiOLMOwner returns the RHOAI CSV named in the olm.owner label, or "".
func rhoaiOLMOwner(labels map[string]interface{}) string {
	owner, _ := labels["olm.owner"].(string)
	if owner != SubName && !strings.HasPrefix(owner, SubName+".") {
		return ""
	}
	if ns, _ := labels["olm.owner.namespace"].(string); ns != "" && ns != SubNS {
		return ""
	}
	return owner
}

// webhookVerdict describes a configuration none of whose webhooks can be
// served.
type webhookVerdict struct {
	Config    admissionConfig
	Down      []string // every referenced Service, each described as down
	Deletable bool
	Reason    string // why it is (not) safe to delete
}

func (v webhookVerdict) downText() string { return strings.Join(v.Down, ", ") }

// webhookScan is one evaluation of the RHOAI webhook configurations.
type webhookScan struct {
	Verdicts []webhookVerdict
	// Upgrading names the CSV install in flight; nothing was evaluated.
	Upgrading string
	Errors    []string
}

// webhookEnv caches the lookups shared by all configurations of one scan.
type webhookEnv struct {
	c            *Client
	health       *serviceHealthCache
	csvs         map[string]csvPresence
	modules      map[string]moduleState
	componentAPI func() (componentAPI, error)
	// installCheckErr is set when the CSV list could not be read, so it is
	// unknown whether an install is in flight.
	installCheckErr error
}

type csvPresence struct {
	present bool
	err     error
}

type moduleState struct {
	exists bool
	err    error
}

// rhoaiInstallInFlight reads the RHOAI CSVs; it returns the CSV and phase of
// an install or upgrade in flight, or "".
func rhoaiInstallInFlight(c *Client) (string, error) {
	csvs, err := listOperatorCSVs(c)
	if IsK8sError(err, http.StatusNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	for _, csv := range csvs {
		switch csv.Phase {
		case "Pending", "InstallReady", "Installing", "Replacing":
			return fmt.Sprintf("%s is %s", csv.Name, csv.Phase), nil
		}
	}
	return "", nil
}

// csvPresent reports whether the CSV exists and is not being deleted. A
// deleting CSV is never reinstalled, so it will not recreate its webhooks.
func (e *webhookEnv) csvPresent(ns, name string) csvPresence {
	key := ns + "/" + name
	if p, ok := e.csvs[key]; ok {
		return p
	}
	var p csvPresence
	body, _, err := e.c.get(namespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", ns, name))
	switch {
	case IsK8sError(err, http.StatusNotFound):
	case err != nil:
		p.err = err
	default:
		var csv struct {
			Metadata struct {
				DeletionTimestamp *string `json:"deletionTimestamp"`
			} `json:"metadata"`
		}
		if jerr := json.Unmarshal(body, &csv); jerr != nil {
			p.err = jerr
		} else {
			p.present = csv.Metadata.DeletionTimestamp == nil
		}
	}
	e.csvs[key] = p
	return p
}

// moduleCRExists reports whether any CR of the platform module exists.
func (e *webhookEnv) moduleCRExists(module string) moduleState {
	if st, ok := e.modules[module]; ok {
		return st
	}
	exists, err := moduleCRPresent(e.c, e.componentAPI, module)
	st := moduleState{exists: exists, err: err}
	e.modules[module] = st
	return st
}

// configModule returns the platform module that produces the configuration:
// its platform.opendatahub.io/part-of label, or the kind of its owning
// components.platform.opendatahub.io CR. "" when there is none.
func configModule(cfg admissionConfig) string {
	if m := cfg.Metadata.Labels["platform.opendatahub.io/part-of"]; m != "" && m != "platform" {
		return m
	}
	for _, o := range cfg.Metadata.OwnerReferences {
		if strings.HasPrefix(o.APIVersion, "components.platform.opendatahub.io/") {
			return strings.ToLower(o.Kind)
		}
	}
	return ""
}

// platformConfig reports whether the platform itself (rhods-operator's
// Platform controller) applies the configuration.
func platformConfig(cfg admissionConfig) bool {
	if cfg.Metadata.Labels["platform.opendatahub.io/part-of"] == "platform" {
		return true
	}
	for _, o := range cfg.Metadata.OwnerReferences {
		if strings.HasPrefix(o.APIVersion, "config.opendatahub.io/") && o.Kind == "Platform" {
			return true
		}
	}
	return false
}

// evaluate returns nil when the configuration can still be served.
func (e *webhookEnv) evaluate(cfg admissionConfig) *webhookVerdict {
	svcs := cfg.services()
	if len(svcs) == 0 {
		return nil
	}
	var down []string
	for _, s := range svcs {
		h := e.health.lookup(s)
		if !h.down() {
			return nil
		}
		down = append(down, h.describe(s))
	}
	v := &webhookVerdict{Config: cfg, Down: down}
	labels := cfg.Metadata.Labels
	created, _ := parseK8sTime(cfg.Metadata.CreationTimestamp)
	switch {
	case e.installCheckErr != nil:
		v.Reason = fmt.Sprintf("the tool could not check whether an operator install is in progress: %v", e.installCheckErr)
	case !created.IsZero() && webhookNow().Sub(created) < staleServiceGrace:
		v.Reason = "it was created less than " + formatDuration(staleServiceGrace) + " ago, so its owner may still be setting it up"
	case labels["olm.owner"] != "":
		owner := labels["olm.owner"]
		if rhoaiOLMOwner(map[string]interface{}{"olm.owner": owner, "olm.owner.namespace": labels["olm.owner.namespace"]}) == "" {
			v.Reason = fmt.Sprintf("it belongs to another operator's CSV (%s)", owner)
			break
		}
		p := e.csvPresent(nonEmpty(labels["olm.owner.namespace"], SubNS), owner)
		switch {
		case p.err != nil:
			v.Reason = fmt.Sprintf("could not check its CSV %s: %v", owner, p.err)
		case p.present:
			v.Reason = fmt.Sprintf("its CSV %s still exists, so OLM recreates it; the operator pods are probably down (see Operator pods)", owner)
		default:
			v.Deletable = true
			v.Reason = fmt.Sprintf("its CSV %s no longer exists", owner)
		}
	case platformConfig(cfg):
		v.Reason = "rhods-operator applies it for the whole platform and re-applies it while RHOAI is installed; check the pods behind its Service"
	case configModule(cfg) != "":
		module := configModule(cfg)
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

var webhookKinds = []struct{ kind, resource string }{
	{"ValidatingWebhookConfiguration", "validatingwebhookconfigurations"},
	{"MutatingWebhookConfiguration", "mutatingwebhookconfigurations"},
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

// scanStaleWebhooks evaluates every RHOAI webhook configuration with the
// rules at the top of this file.
func scanStaleWebhooks(c *Client) webhookScan {
	var scan webhookScan
	upgrading, installErr := rhoaiInstallInFlight(c)
	if upgrading != "" {
		scan.Upgrading = upgrading
		return scan
	}
	configs, errs := listAdmissionConfigs(c)
	scan.Errors = errs
	env := &webhookEnv{
		c:               c,
		health:          newServiceHealthCache(c, staleServiceGrace),
		csvs:            map[string]csvPresence{},
		modules:         map[string]moduleState{},
		componentAPI:    cachedComponentAPI(c),
		installCheckErr: installErr,
	}
	var refs []string
	for _, cfg := range configs {
		refs = append(refs, cfg.services()...)
	}
	env.health.prefetch(refs)
	// A Service that cannot be read is reported. A Service that exists but
	// whose endpoints cannot be listed (e.g. a template without the
	// EndpointSlice rule) counts as serving: that never deletes anything.
	for _, ref := range refs {
		if h := env.health.lookup(ref); h.state == serviceUnknown && !h.exists && !containsString(scan.Errors, h.describe(ref)) {
			scan.Errors = append(scan.Errors, h.describe(ref))
		}
	}
	for _, cfg := range configs {
		if v := env.evaluate(cfg); v != nil {
			scan.Verdicts = append(scan.Verdicts, *v)
		}
	}
	return scan
}

// webhookDeletion is the outcome of deleteStaleWebhookConfigs.
type webhookDeletion struct {
	Deleted, Changed, Failed []string // config refs
	Logs                     []string
}

// deleteStaleWebhookConfigs deletes the deletable verdicts, each only if it
// still has the uid and resourceVersion that were evaluated, and logs why
// every other one was kept.
func deleteStaleWebhookConfigs(c *Client, verdicts []webhookVerdict) webhookDeletion {
	var d webhookDeletion
	for _, v := range verdicts {
		if !v.Deletable {
			d.Logs = append(d.Logs, fmt.Sprintf("Kept %s: %s", v.Config.ref(), v.Reason))
			continue
		}
		resource := "validatingwebhookconfigurations"
		if v.Config.Kind == "MutatingWebhookConfiguration" {
			resource = "mutatingwebhookconfigurations"
		}
		path := clusterPath("admissionregistration.k8s.io/v1", resource, v.Config.Metadata.Name)
		err := deleteExact(c, path, v.Config.Metadata.UID, v.Config.Metadata.ResourceVersion)
		switch {
		case err == nil:
			d.Deleted = append(d.Deleted, v.Config.ref())
			d.Logs = append(d.Logs, fmt.Sprintf("Deleted %s (Service %s; %s)", v.Config.ref(), v.downText(), v.Reason))
		case IsK8sError(err, http.StatusNotFound):
			d.Logs = append(d.Logs, fmt.Sprintf("%s was already gone", v.Config.ref()))
		case IsK8sError(err, http.StatusConflict):
			d.Changed = append(d.Changed, v.Config.ref())
			d.Logs = append(d.Logs, fmt.Sprintf("Kept %s: it changed after it was checked", v.Config.ref()))
		default:
			d.Failed = append(d.Failed, v.Config.ref())
			d.Logs = append(d.Logs, fmt.Sprintf("Error: could not delete %s: %v", v.Config.ref(), err))
		}
	}
	return d
}

// --- CRD conversion webhooks ---

type conversionCRD struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Scope      string `json:"scope"`
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

// conversionService returns "ns/name" of the CRD's conversion webhook
// Service, or "" when the CRD does not convert through a Service.
func (crd conversionCRD) conversionService() string {
	conv := crd.Spec.Conversion
	if conv == nil || conv.Strategy != "Webhook" || conv.Webhook == nil || conv.Webhook.ClientConfig.Service == nil {
		return ""
	}
	svc := conv.Webhook.ClientConfig.Service
	if svc.Namespace == "" || svc.Name == "" {
		return ""
	}
	return svc.Namespace + "/" + svc.Name
}

type deadConversion struct {
	CRD            string
	Service        string // ns/name
	Health         serviceHealth
	StoredVersions []string
}

func (d deadConversion) describe() string {
	return fmt.Sprintf("%s (Service %s)", d.CRD, d.Health.describe(d.Service))
}

// deadConversions returns the CRDs whose conversion Service is down. A
// Service that cannot be checked is returned in unknown (with its error).
func deadConversions(crds []conversionCRD, health *serviceHealthCache) (dead []deadConversion, unknown []deadConversion) {
	var refs []string
	for _, crd := range crds {
		if ref := crd.conversionService(); ref != "" {
			refs = append(refs, ref)
		}
	}
	health.prefetch(refs)
	for _, crd := range crds {
		ref := crd.conversionService()
		if ref == "" {
			continue
		}
		h := health.lookup(ref)
		d := deadConversion{CRD: crd.Metadata.Name, Service: ref, Health: h, StoredVersions: crd.Status.StoredVersions}
		switch {
		case h.down():
			dead = append(dead, d)
		case h.state == serviceUnknown:
			unknown = append(unknown, d)
		}
	}
	sort.Slice(dead, func(i, j int) bool { return dead[i].CRD < dead[j].CRD })
	return dead, unknown
}

// rhoaiCRDSelectors select the CRDs RHOAI installs: the operator's own
// (OLM package label, e.g. DSC and DSCI) and those the platform applies for
// modules. Listing all CRDs would download every schema (13 MB live).
var rhoaiCRDSelectors = []string{
	"operators.coreos.com/" + SubName + "." + SubNS,
	"platform.opendatahub.io/part-of",
}

// listRHOAIConversionCRDs lists the RHOAI CRDs (diagnostics).
func listRHOAIConversionCRDs(c *Client) ([]conversionCRD, error) {
	var items []conversionCRD
	seen := map[string]bool{}
	for _, sel := range rhoaiCRDSelectors {
		body, _, err := c.do(http.MethodGet, "/apis/apiextensions.k8s.io/v1/customresourcedefinitions", "", nil, url.Values{"labelSelector": {sel}})
		if err != nil {
			return nil, err
		}
		var list struct {
			Items []conversionCRD `json:"items"`
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
	return items, nil
}

// listNamespacedConversionCRDs lists every namespaced CRD (all of them, page
// by page): a namespace delete has to list every namespaced type, whoever
// installed it.
func listNamespacedConversionCRDs(c *Client) ([]conversionCRD, error) {
	var items []conversionCRD
	cont := ""
	for {
		body, _, err := c.get("/apis/apiextensions.k8s.io/v1/customresourcedefinitions?limit=100" + continueParam(cont))
		if err != nil {
			return nil, fmt.Errorf("list CRDs: %w", err)
		}
		var list struct {
			Metadata struct {
				Continue string `json:"continue"`
			} `json:"metadata"`
			Items []conversionCRD `json:"items"`
		}
		if err := json.Unmarshal(body, &list); err != nil {
			return nil, fmt.Errorf("parse CRD list: %w", err)
		}
		for _, crd := range list.Items {
			if crd.Spec.Scope == "Namespaced" {
				items = append(items, crd)
			}
		}
		if list.Metadata.Continue == "" {
			return items, nil
		}
		cont = list.Metadata.Continue
	}
}

// brokenConversionWebhooks is the namespace-deletion guard: it returns the
// namespaced CRDs whose conversion webhook cannot serve right now (Service
// missing or without a ready endpoint, no grace period). The namespace
// controller has to list every namespaced type, and requests that need
// conversion fail while the webhook is down, so a namespace deleted then
// can hang in Terminating (notes D3). Any read error is returned so callers
// fail closed.
func brokenConversionWebhooks(c *Client) ([]string, error) {
	crds, err := listNamespacedConversionCRDs(c)
	if err != nil {
		return nil, err
	}
	dead, unknown := deadConversions(crds, newServiceHealthCache(c, 0))
	if len(unknown) > 0 {
		return nil, fmt.Errorf("check conversion Service of CRD %s: %w", unknown[0].CRD, unknown[0].Health.err)
	}
	var out []string
	for _, d := range dead {
		out = append(out, d.describe())
	}
	return out, nil
}
