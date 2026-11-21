package components

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
)

// Stale admission webhooks and CRD conversion webhooks: the one
// implementation used by diagnostics (report and the delete-stale-webhooks
// fix), Reinstall (cleanup after the CSV is deleted) and the MinIO teardown
// guard (namespace deletion). It follows RHOAI_OPERATOR_NOTES §3.4.
//
// A webhook configuration is stale only when ALL of these hold:
//  1. Not one of its webhooks can be served: every webhook calls a Service
//     (clientConfig.service), and every such Service is NotFound, or exists
//     but has had no ready endpoint for longer than StaleServiceGrace (a
//     restarting pod is not staleness). Deleting a configuration removes all
//     of its webhooks, so one webhook that may still serve keeps it: a
//     serving Service, a Service that cannot be checked, and any webhook
//     called through clientConfig.url (admissionregistration/v1
//     WebhookClientConfig), whose availability the tool cannot prove.
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
//  4. The configuration itself is older than StaleServiceGrace, so a
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

// StaleServiceGrace is how long a Service must have had no ready endpoint,
// and how old a webhook configuration must be, before it counts as stale.
var StaleServiceGrace = 5 * time.Minute

// webhookNow is the clock for grace periods (tests replace it).
var webhookNow = time.Now

type ServiceState int

const (
	// ServiceServing: a ready endpoint exists, or the Service cannot be
	// proven down (no selector, ExternalName, endpoints changed within the
	// grace period).
	ServiceServing ServiceState = iota
	// ServiceMissing: the Service is NotFound.
	ServiceMissing
	// ServiceNoEndpoints: the Service exists but has had no ready endpoint
	// for at least the grace period.
	ServiceNoEndpoints
	// ServiceUnknown: a lookup failed; callers must not treat it as down.
	ServiceUnknown
)

type serviceHealth struct {
	State ServiceState
	// Exists is true when the Service was read; with ServiceUnknown it
	// means only its endpoints could not be checked.
	Exists bool
	Err    error
}

func (h serviceHealth) down() bool {
	return h.State == ServiceMissing || h.State == ServiceNoEndpoints
}

// Describe renders a down Service for evidence and messages.
func (h serviceHealth) Describe(ref string) string {
	switch h.State {
	case ServiceMissing:
		return ref + " not found"
	case ServiceNoEndpoints:
		return ref + " has no ready endpoints"
	case ServiceUnknown:
		return fmt.Sprintf("%s could not be checked (%v)", ref, h.Err)
	}
	return ref + " is serving"
}

// CheckServiceHealth reads one Service and, when it exists, its
// EndpointSlices. grace 0 counts a Service without ready endpoints as down
// at once (the namespace guard).
func CheckServiceHealth(c *kube.Client, ref string, grace time.Duration) serviceHealth {
	ns, name, _ := strings.Cut(ref, "/")
	body, found, err := c.GetOptional(rhoai.NamespacedPath("v1", "services", ns, name))
	if err != nil {
		return serviceHealth{State: ServiceUnknown, Err: err}
	}
	if !found {
		return serviceHealth{State: ServiceMissing}
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
		return serviceHealth{State: ServiceUnknown, Err: fmt.Errorf("parse Service %s: %w", ref, err)}
	}
	// An ExternalName Service is resolved through DNS; the tool cannot
	// prove it does not serve.
	if svc.Spec.Type == "ExternalName" {
		return serviceHealth{State: ServiceServing, Exists: true}
	}
	body, _, err = c.Get(rhoai.NamespacedPath("discovery.k8s.io/v1", "endpointslices", ns, "") + "?labelSelector=" + url.QueryEscape("kubernetes.io/service-name="+name))
	if err != nil {
		return serviceHealth{State: ServiceUnknown, Exists: true, Err: fmt.Errorf("list endpoints of Service %s: %w", ref, err)}
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
		return serviceHealth{State: ServiceUnknown, Exists: true, Err: fmt.Errorf("parse endpoints of Service %s: %w", ref, err)}
	}
	// The last time the endpoints changed: the newest of the Service and
	// slice creation times, the EndpointSlice controller's
	// endpoints.kubernetes.io/last-change-trigger-time annotation and the
	// slices' managedFields update times (live on OCP 4.22).
	last, _ := ParseK8sTime(svc.Metadata.CreationTimestamp)
	for _, s := range slices.Items {
		for _, ep := range s.Endpoints {
			if ep.Conditions.Ready == nil || *ep.Conditions.Ready {
				return serviceHealth{State: ServiceServing, Exists: true}
			}
		}
		times := []string{s.Metadata.CreationTimestamp, s.Metadata.Annotations["endpoints.kubernetes.io/last-change-trigger-time"]}
		for _, mf := range s.Metadata.ManagedFields {
			times = append(times, mf.Time)
		}
		for _, ts := range times {
			if t, ok := ParseK8sTime(ts); ok && t.After(last) {
				last = t
			}
		}
	}
	if grace > 0 && (last.IsZero() || webhookNow().Sub(last) < grace) {
		// Too recent (or of unknown age) to tell a restart from an outage.
		return serviceHealth{State: ServiceServing, Exists: true}
	}
	return serviceHealth{State: ServiceNoEndpoints, Exists: true}
}

// serviceHealthCache checks each Service once per evaluation, concurrently.
type serviceHealthCache struct {
	c       *kube.Client
	grace   time.Duration
	results map[string]serviceHealth
}

func NewServiceHealthCache(c *kube.Client, grace time.Duration) *serviceHealthCache {
	return &serviceHealthCache{c: c, grace: grace, results: map[string]serviceHealth{}}
}

// prefetch checks the given Services concurrently (at most 8 at a time).
func (s *serviceHealthCache) prefetch(refs []string) {
	var todo []string
	for _, ref := range refs {
		if _, ok := s.results[ref]; !ok && !slices.Contains(todo, ref) {
			todo = append(todo, ref)
		}
	}
	out := make([]serviceHealth, len(todo))
	kube.ParallelFor(len(todo), 8, func(i int) { out[i] = CheckServiceHealth(s.c, todo[i], s.grace) })
	for i, ref := range todo {
		s.results[ref] = out[i]
	}
}

func (s *serviceHealthCache) lookup(ref string) serviceHealth {
	if h, ok := s.results[ref]; ok {
		return h
	}
	h := CheckServiceHealth(s.c, ref, s.grace)
	s.results[ref] = h
	return h
}

// --- Admission webhook configurations ---

type AdmissionConfig struct {
	Kind     string `json:"-"`
	Resource string `json:"-"`
	Metadata struct {
		Name              string            `json:"name"`
		UID               string            `json:"uid"`
		ResourceVersion   string            `json:"resourceVersion"`
		CreationTimestamp string            `json:"creationTimestamp"`
		Labels            map[string]string `json:"labels"`
		Annotations       map[string]string `json:"annotations"`
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
			URL     *string `json:"url"`
			Service *struct {
				Namespace string `json:"namespace"`
				Name      string `json:"name"`
			} `json:"service"`
		} `json:"clientConfig"`
	} `json:"webhooks"`
}

func (a AdmissionConfig) Ref() string { return a.Kind + " " + a.Metadata.Name }

// isRHOAI selects the configurations to evaluate: OLM-installed for the
// RHOAI CSV, applied by the platform for a module, owned by an RHOAI object,
// or carrying an RHOAI name without any OLM owner (older installs).
func (a AdmissionConfig) isRHOAI() bool {
	labels := map[string]any{}
	for k, v := range a.Metadata.Labels {
		labels[k] = v
	}
	if IsRHOAIWebhook(a.Metadata.Name, labels) || a.Metadata.Labels["platform.opendatahub.io/part-of"] != "" {
		return true
	}
	for _, o := range a.Metadata.OwnerReferences {
		if strings.Contains(o.APIVersion, "opendatahub.io") {
			return true
		}
	}
	return false
}

// unverifiableHooks names the webhooks that do not call an in-cluster
// Service (clientConfig.url, or an incomplete service reference). The tool
// cannot prove those are down, so their configuration is never stale.
func (a AdmissionConfig) unverifiableHooks() []string {
	var out []string
	for _, wh := range a.Webhooks {
		if svc := wh.ClientConfig.Service; svc == nil || svc.Name == "" || svc.Namespace == "" {
			out = append(out, wh.Name)
		}
	}
	return out
}

func (a AdmissionConfig) services() []string {
	var out []string
	for _, wh := range a.Webhooks {
		if svc := wh.ClientConfig.Service; svc != nil && svc.Name != "" && svc.Namespace != "" {
			ref := svc.Namespace + "/" + svc.Name
			if !slices.Contains(out, ref) {
				out = append(out, ref)
			}
		}
	}
	sort.Strings(out)
	return out
}

// Blocking reports whether any webhook rejects requests when it cannot be
// called.
func (a AdmissionConfig) Blocking() bool {
	for _, wh := range a.Webhooks {
		// admissionregistration/v1 defaults failurePolicy to Fail.
		if wh.FailurePolicy == nil || *wh.FailurePolicy == "Fail" {
			return true
		}
	}
	return false
}

// IsRHOAIWebhook reports whether a webhook configuration was installed by OLM
// for the RHOAI operator CSV, or carries an RHOAI name without any OLM owner
// label (a leftover from older installs). It only selects candidates; the
// staleness rules above decide.
func IsRHOAIWebhook(name string, labels map[string]any) bool {
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
func rhoaiOLMOwner(labels map[string]any) string {
	owner, _ := labels["olm.owner"].(string)
	if owner != rhoai.SubName && !strings.HasPrefix(owner, rhoai.SubName+".") {
		return ""
	}
	if ns, _ := labels["olm.owner.namespace"].(string); ns != "" && ns != rhoai.SubNS {
		return ""
	}
	return owner
}

// WebhookVerdict describes a configuration none of whose webhooks can be
// served.
type WebhookVerdict struct {
	Config    AdmissionConfig
	Down      []string // every referenced Service, each described as down
	Deletable bool
	Reason    string // why it is (not) safe to delete
}

func (v WebhookVerdict) DownText() string { return strings.Join(v.Down, ", ") }

// WebhookScan is one evaluation of the RHOAI webhook configurations.
type WebhookScan struct {
	Verdicts []WebhookVerdict
	// Upgrading names the CSV install in flight; nothing was evaluated.
	Upgrading string
	Errors    []string
}

// webhookEnv caches the lookups shared by all configurations of one scan.
type webhookEnv struct {
	c            *kube.Client
	health       *serviceHealthCache
	csvs         map[string]csvPresence
	modules      map[string]moduleState
	componentAPI func() (rhoai.ComponentAPI, error)
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
func rhoaiInstallInFlight(c *kube.Client) (string, error) {
	csvs, err := listOperatorCSVs(c)
	if kube.IsK8sNotFound(err) {
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
	body, found, err := e.c.GetOptional(rhoai.NamespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", ns, name))
	switch {
	case err != nil:
		p.err = err
	case !found:
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
// module CR (either module API group, component_cleanup.go). "" when there
// is none.
func configModule(cfg AdmissionConfig) string {
	if m := cfg.Metadata.Labels["platform.opendatahub.io/part-of"]; m != "" && m != "platform" {
		return m
	}
	for _, o := range cfg.Metadata.OwnerReferences {
		if IsModuleAPIVersion(o.APIVersion) {
			return strings.ToLower(o.Kind)
		}
	}
	return ""
}

// platformConfig reports whether the platform itself (rhods-operator's
// Platform controller) applies the configuration.
func platformConfig(cfg AdmissionConfig) bool {
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

// evaluate returns nil when the configuration can still be served, or when
// any of its webhooks cannot be proven down (URL-backed hooks).
func (e *webhookEnv) evaluate(cfg AdmissionConfig) *WebhookVerdict {
	svcs := cfg.services()
	if len(svcs) == 0 || len(cfg.unverifiableHooks()) > 0 {
		return nil
	}
	var down []string
	for _, s := range svcs {
		h := e.health.lookup(s)
		if !h.down() {
			return nil
		}
		down = append(down, h.Describe(s))
	}
	v := &WebhookVerdict{Config: cfg, Down: down}
	labels := cfg.Metadata.Labels
	created, _ := ParseK8sTime(cfg.Metadata.CreationTimestamp)
	switch {
	case e.installCheckErr != nil:
		v.Reason = fmt.Sprintf("the tool could not check whether an operator install is in progress: %v", e.installCheckErr)
	case !created.IsZero() && webhookNow().Sub(created) < StaleServiceGrace:
		v.Reason = "it was created less than " + FormatDuration(StaleServiceGrace) + " ago, so its owner may still be setting it up"
	case labels["olm.owner"] != "":
		owner := labels["olm.owner"]
		if rhoaiOLMOwner(map[string]any{"olm.owner": owner, "olm.owner.namespace": labels["olm.owner.namespace"]}) == "" {
			v.Reason = fmt.Sprintf("it belongs to another operator's CSV (%s)", owner)
			break
		}
		p := e.csvPresent(kube.NonEmpty(labels["olm.owner.namespace"], rhoai.SubNS), owner)
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
		case errors.Is(st.err, errModuleKindUnknown):
			v.Reason = fmt.Sprintf("the tool cannot tell whether module %s is installed: %v", module, st.err)
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

func ListAdmissionConfigs(c *kube.Client) ([]AdmissionConfig, []string) {
	var out []AdmissionConfig
	var errs []string
	for _, wk := range webhookKinds {
		var items []AdmissionConfig
		if err := c.ListInto(rhoai.ClusterPath("admissionregistration.k8s.io/v1", wk.resource, ""), &items); err != nil {
			if kube.IsDecodeError(err) {
				errs = append(errs, fmt.Sprintf("parse %s: %v", wk.resource, err))
				continue
			}
			errs = append(errs, fmt.Sprintf("list %s: %v", wk.resource, err))
			continue
		}
		for _, item := range items {
			item.Kind, item.Resource = wk.kind, wk.resource
			if item.isRHOAI() {
				out = append(out, item)
			}
		}
	}
	return out, errs
}

// ScanStaleWebhooks evaluates every RHOAI webhook configuration with the
// rules at the top of this file.
func ScanStaleWebhooks(c *kube.Client) WebhookScan {
	var scan WebhookScan
	upgrading, installErr := rhoaiInstallInFlight(c)
	if upgrading != "" {
		scan.Upgrading = upgrading
		return scan
	}
	configs, errs := ListAdmissionConfigs(c)
	scan.Errors = errs
	env := &webhookEnv{
		c:               c,
		health:          NewServiceHealthCache(c, StaleServiceGrace),
		csvs:            map[string]csvPresence{},
		modules:         map[string]moduleState{},
		componentAPI:    CachedComponentAPI(c),
		installCheckErr: installErr,
	}
	var refs []string
	for _, cfg := range configs {
		refs = append(refs, cfg.services()...)
	}
	env.health.prefetch(refs)
	// A Service, or its endpoints, that cannot be read keeps its
	// configuration (it is never deletable), and is reported so the check
	// says it could not verify it instead of passing.
	for _, ref := range refs {
		if h := env.health.lookup(ref); h.State == ServiceUnknown && !slices.Contains(scan.Errors, h.Describe(ref)) {
			scan.Errors = append(scan.Errors, h.Describe(ref))
		}
	}
	for _, cfg := range configs {
		if v := env.evaluate(cfg); v != nil {
			scan.Verdicts = append(scan.Verdicts, *v)
		}
	}
	return scan
}

// webhookDeletion is the outcome of DeleteStaleWebhookConfigs.
type webhookDeletion struct {
	Deleted, Changed, Failed []string // config refs
	Logs                     []string
}

// DeleteStaleWebhookConfigs deletes the deletable verdicts, each only if it
// still has the uid and resourceVersion that were evaluated, and logs why
// every other one was kept.
func DeleteStaleWebhookConfigs(c *kube.Client, verdicts []WebhookVerdict) webhookDeletion {
	var d webhookDeletion
	for _, v := range verdicts {
		if !v.Deletable {
			d.Logs = append(d.Logs, fmt.Sprintf("Kept %s: %s", v.Config.Ref(), v.Reason))
			continue
		}
		resource := "validatingwebhookconfigurations"
		if v.Config.Kind == "MutatingWebhookConfiguration" {
			resource = "mutatingwebhookconfigurations"
		}
		path := rhoai.ClusterPath("admissionregistration.k8s.io/v1", resource, v.Config.Metadata.Name)
		err := kube.DeleteExact(c, path, v.Config.Metadata.UID, v.Config.Metadata.ResourceVersion)
		switch {
		case err == nil:
			d.Deleted = append(d.Deleted, v.Config.Ref())
			d.Logs = append(d.Logs, fmt.Sprintf("Deleted %s (Service %s; %s)", v.Config.Ref(), v.DownText(), v.Reason))
		case kube.IsK8sNotFound(err):
			d.Logs = append(d.Logs, fmt.Sprintf("%s was already gone", v.Config.Ref()))
		case kube.IsK8sConflict(err):
			d.Changed = append(d.Changed, v.Config.Ref())
			d.Logs = append(d.Logs, fmt.Sprintf("Kept %s: it changed after it was checked", v.Config.Ref()))
		default:
			d.Failed = append(d.Failed, v.Config.Ref())
			d.Logs = append(d.Logs, fmt.Sprintf("Error: could not delete %s: %v", v.Config.Ref(), err))
		}
	}
	return d
}

// --- CRD conversion webhooks ---

type ConversionCRD struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Group string `json:"group"`
		Names struct {
			Plural   string `json:"plural"`
			Kind     string `json:"kind"`
			ListKind string `json:"listKind"`
		} `json:"names"`
		Versions []struct {
			Name    string `json:"name"`
			Served  bool   `json:"served"`
			Storage bool   `json:"storage"`
		} `json:"versions"`
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
		AcceptedNames  struct {
			ListKind string `json:"listKind"`
		} `json:"acceptedNames"`
	} `json:"status"`
}

// ListKind is the kind the API server gives a list of the CRD's objects
// ("" when the CRD names none): status.acceptedNames.listKind, which
// apiextensions-apiserver pkg/apiserver/customresource_handler.go serves
// the list as (it refuses to serve a CRD whose accepted listKind is
// empty), else spec.names.listKind, else spec.names.kind + "List", the
// default (apiextensions/v1 CustomResourceDefinitionNames.ListKind:
// "Defaults to `kind`List"; v1/defaults.go).
func (crd ConversionCRD) ListKind() string {
	switch {
	case crd.Status.AcceptedNames.ListKind != "":
		return crd.Status.AcceptedNames.ListKind
	case crd.Spec.Names.ListKind != "":
		return crd.Spec.Names.ListKind
	case crd.Spec.Names.Kind != "":
		return crd.Spec.Names.Kind + "List"
	}
	return ""
}

// ConversionService returns "ns/name" of the CRD's conversion webhook
// Service, or "" when the CRD does not convert through a Service.
func (crd ConversionCRD) ConversionService() string {
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

// ObjectsPath is the path listing the CRD's objects in every namespace, at
// its storage version when served (an object stored at the version read
// needs no conversion), else at the first served version; "" when the
// CRD serves none.
func (crd ConversionCRD) ObjectsPath() string {
	version := ""
	for _, v := range crd.Spec.Versions {
		if v.Served && (version == "" || v.Storage) {
			version = v.Name
		}
	}
	if version == "" || crd.Spec.Group == "" || crd.Spec.Names.Plural == "" {
		return ""
	}
	return "/apis/" + crd.Spec.Group + "/" + version + "/" + crd.Spec.Names.Plural
}

type DeadConversion struct {
	CRD            string
	Service        string // ns/name
	Health         serviceHealth
	StoredVersions []string
	ListPath       string // ConversionCRD.ObjectsPath
}

// DeadConversions returns the CRDs whose conversion Service is down. A
// Service that cannot be checked is returned in unknown (with its error).
func DeadConversions(crds []ConversionCRD, health *serviceHealthCache) (dead []DeadConversion, unknown []DeadConversion) {
	var refs []string
	for _, crd := range crds {
		if ref := crd.ConversionService(); ref != "" {
			refs = append(refs, ref)
		}
	}
	health.prefetch(refs)
	for _, crd := range crds {
		ref := crd.ConversionService()
		if ref == "" {
			continue
		}
		h := health.lookup(ref)
		d := DeadConversion{CRD: crd.Metadata.Name, Service: ref, Health: h, StoredVersions: crd.Status.StoredVersions, ListPath: crd.ObjectsPath()}
		switch {
		case h.down():
			dead = append(dead, d)
		case h.State == ServiceUnknown:
			unknown = append(unknown, d)
		}
	}
	sort.Slice(dead, func(i, j int) bool { return dead[i].CRD < dead[j].CRD })
	return dead, unknown
}

// RHOAICRDSelectors select the CRDs RHOAI installs: the operator's own
// (OLM package label, e.g. DSC and DSCI) and those the platform applies for
// modules. Listing all CRDs would download every schema (13 MB live).
var RHOAICRDSelectors = []string{
	"operators.coreos.com/" + rhoai.SubName + "." + rhoai.SubNS,
	"platform.opendatahub.io/part-of",
}

// ListRHOAIConversionCRDs lists the RHOAI CRDs (diagnostics).
func ListRHOAIConversionCRDs(c *kube.Client) ([]ConversionCRD, error) {
	var items []ConversionCRD
	seen := map[string]bool{}
	for _, sel := range RHOAICRDSelectors {
		body, _, err := c.Do(http.MethodGet, "/apis/apiextensions.k8s.io/v1/customresourcedefinitions", "", nil, url.Values{"labelSelector": {sel}})
		if err != nil {
			return nil, err
		}
		var list struct {
			Items []ConversionCRD `json:"items"`
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

// operatorCSV is a CSV of the rhods-operator package in SubNS.
type operatorCSV struct {
	Name    string
	Version string
	Phase   string
	Reason  string
	Message string
}

func listOperatorCSVs(c *kube.Client) ([]operatorCSV, error) {
	var items []struct {
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
	}
	if err := c.ListInto(rhoai.NamespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", rhoai.SubNS, ""), &items); err != nil {
		return nil, err
	}
	var out []operatorCSV
	for _, it := range items {
		if !strings.HasPrefix(it.Metadata.Name, rhoai.SubName+".") {
			continue
		}
		out = append(out, operatorCSV{Name: it.Metadata.Name, Version: it.Spec.Version, Phase: it.Status.Phase, Reason: it.Status.Reason, Message: it.Status.Message})
	}
	return out, nil
}
