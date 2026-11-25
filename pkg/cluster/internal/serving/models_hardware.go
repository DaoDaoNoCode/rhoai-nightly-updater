package serving

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/authz"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
)

// The annotations the dashboard's deploy wizard writes for the hardware
// profile a deployment uses (odh-dashboard
// packages/hardware-profiles/shared/utils.ts applyHardwareProfileConfig).
// Its Deployments page reads the name and namespace. A model with none of
// them (no profile fits it: Qwen3's 12-14 GiB against the default
// profile's 8 GiB on 3.6) is shown so (odh-dashboard rhoai-3.6 a9ea0d86):
//   - the table's Hardware profile column says "No hardware profile":
//     useHardwareProfileBindingState returns no binding state without a
//     name or resource-version annotation, so HardwareProfileTableColumn
//     renders only HardwareProfileDetailsPopover, whose table-view trigger
//     without a profile is "No hardware profile" (L102-114). Its "Custom"
//     (HardwareProfileTableColumn.tsx L60) is the name in a binding-state
//     label, shown only for a deleted, disabled or updated profile;
//   - the expanded row says "Hardware profile: Unknown"
//     (packages/model-serving deployments/row/HardwareProfileNameValue.tsx
//     L37-38): useHardwareProfileConfig matches the model's resources to a
//     profile (matchToHardwareProfile), and none holds them.
//
// The user accepted this for such a model: no profile is created for it.
const (
	HardwareProfileNameAnnotation      = "opendatahub.io/hardware-profile-name"
	HardwareProfileNamespaceAnnotation = "opendatahub.io/hardware-profile-namespace"
	HardwareProfileVersionAnnotation   = "opendatahub.io/hardware-profile-resource-version"
	hardwareProfileDisabledAnnotation  = "opendatahub.io/disabled"
)

// HardwareProfile is the profile a model is recorded under; zero when none.
type HardwareProfile struct {
	Name, Namespace, ResourceVersion string
}

type HardwareProfileObject struct {
	Metadata kube.ObjectMeta `json:"metadata"`
	Spec     struct {
		Identifiers []struct {
			Identifier string `json:"identifier"`
			MinCount   any    `json:"minCount"`
			MaxCount   any    `json:"maxCount"`
		} `json:"identifiers"`
		Scheduling *struct {
			Type string `json:"type"`
			Node *struct {
				NodeSelector map[string]string `json:"nodeSelector"`
				Tolerations  []any             `json:"tolerations"`
			} `json:"node"`
			Kueue *struct {
				LocalQueueName string `json:"localQueueName"`
			} `json:"kueue"`
		} `json:"scheduling"`
	} `json:"spec"`
}

// quantityString reads a count that the API returns as a number or a string.
func quantityString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return fmt.Sprintf("%g", t)
	default:
		return ""
	}
}

// hardwareProfileVisibilityAnnotation lists, as a JSON array, the dashboard
// features a profile is offered in (the dashboard's values: "workbench",
// "model-serving").
const hardwareProfileVisibilityAnnotation = "opendatahub.io/dashboard-feature-visibility"

// visibleForModelServing follows the dashboard's isHardwareProfileVisibleIn
// (rhoai-3.6 packages/hardware-profiles): no annotation, one that does not
// parse, or an empty list is visible everywhere; a list without
// "model-serving" hides the profile from model serving. The dashboard
// ignores values it does not know; this check counts them, so such a
// profile is skipped (the model then records another one, or none): the
// safe side.
func (p HardwareProfileObject) visibleForModelServing() bool {
	raw, ok := p.Metadata.Annotations[hardwareProfileVisibilityAnnotation]
	if !ok {
		return true
	}
	var in []string
	if json.Unmarshal([]byte(raw), &in) != nil || len(in) == 0 {
		return true
	}
	return slices.Contains(in, "model-serving")
}

// Fits says why the profile cannot hold the model, or "" when it can: it
// must be enabled, describe only CPU and memory (no accelerator), add no
// node scheduling (the tool would have to apply it as the wizard does), and
// its ranges must contain the model's requests and limits. Whether a Queue
// profile fits the project is the Kueue rule's (kueueFilter).
func (p HardwareProfileObject) Fits(entry CatalogModel) string {
	if p.Metadata.Annotations[hardwareProfileDisabledAnnotation] == "true" {
		return "disabled"
	}
	if !p.visibleForModelServing() {
		return "the dashboard shows it only for " + p.Metadata.Annotations[hardwareProfileVisibilityAnnotation]
	}
	if s := p.Spec.Scheduling; s != nil && (s.Node != nil && (len(s.Node.NodeSelector) > 0 || len(s.Node.Tolerations) > 0)) {
		return "it adds scheduling rules"
	}
	want := map[string][]string{"cpu": {entry.CPU, entry.CPU}, "memory": {entry.MemoryRequest, entry.Memory}}
	seen := map[string]bool{}
	for _, id := range p.Spec.Identifiers {
		values, ok := want[id.Identifier]
		if !ok {
			return "it is for " + id.Identifier
		}
		seen[id.Identifier] = true
		lo, okLo := ParseQuantity(quantityString(id.MinCount))
		hi, okHi := ParseQuantity(quantityString(id.MaxCount))
		for _, v := range values {
			n, _ := ParseQuantity(v)
			if (okLo && n < lo) || (okHi && n > hi) {
				return fmt.Sprintf("%s %s is outside %s-%s", id.Identifier, v, quantityString(id.MinCount), kube.FirstNonEmpty(quantityString(id.MaxCount), "any"))
			}
		}
	}
	if !seen["cpu"] || !seen["memory"] {
		return "it does not set both CPU and memory"
	}
	return ""
}

// isQueue: the profile sends workloads to a Kueue LocalQueue (spec.scheduling
// type Queue).
func (p HardwareProfileObject) isQueue() bool {
	return p.Spec.Scheduling != nil && p.Spec.Scheduling.Type == "Queue"
}

func (p HardwareProfileObject) queue() string {
	if p.isQueue() && p.Spec.Scheduling.Kueue != nil {
		return p.Spec.Scheduling.Kueue.LocalQueueName
	}
	return ""
}

// kueueFilter applies the dashboard's Kueue rule (envkit.KueueMode,
// filterProfilesByKueue): in a project of OnlyKueue only Queue profiles whose
// LocalQueue the project has (or that name none), elsewhere only the others.
// A Queue profile's queue is put on the model by RHOAI's HardwareProfile
// webhook (rhods-operator rhoai-3.6 internal/webhook/hardwareprofile
// mutating.go applyHardwareProfileToWorkload, for InferenceServices and
// LLMInferenceServices: label kueue.x-k8s.io/queue-name), as for a model the
// dashboard deploys.
func kueueFilter(p HardwareProfileObject, mode envkit.KueueMode, queues []string) string {
	switch {
	case mode == envkit.OnlyKueue && !p.isQueue():
		return "the project uses Kueue, and this profile does not send models to a queue"
	case mode == envkit.OnlyKueue && p.queue() != "" && !slices.Contains(queues, p.queue()):
		return "its LocalQueue " + p.queue() + " is not in the project"
	case mode != envkit.OnlyKueue && p.isQueue():
		return "it sends models to a Kueue queue, and the project does not use Kueue"
	}
	return ""
}

// projectKueueMode is the dashboard's Kueue rule for a model in project
// (envkit.KueueModeFor), and the project's LocalQueues when it uses Kueue.
// Unreadable settings are an error: the caller then records no profile.
func projectKueueMode(c *kube.Client, project, apps string) (envkit.KueueMode, []string, error) {
	meta, found, err := kube.ReadObjectMeta(c, "/api/v1/namespaces/"+project)
	if err != nil {
		return envkit.OnlyNonKueue, nil, err
	}
	labelled := found && meta.Labels[envkit.KueueProjectLabel] == "true"
	if !labelled {
		return envkit.OnlyNonKueue, nil, nil
	}
	dsc, err := rhoai.ReadFirstDSC(c)
	if err != nil {
		return envkit.OnlyNonKueue, nil, err
	}
	var cfg struct {
		Spec struct {
			DashboardConfig struct {
				DisableKueue *bool `json:"disableKueue"`
			} `json:"dashboardConfig"`
		} `json:"spec"`
	}
	if _, err := c.GetIntoOptional(rhoai.NamespacedPath("opendatahub.io/v1alpha", "odhdashboardconfigs", apps, "odh-dashboard-config"), &cfg); err != nil {
		return envkit.OnlyNonKueue, nil, err
	}
	mode := envkit.KueueModeFor(true, envkit.KueueAvailable(dsc, cfg.Spec.DashboardConfig.DisableKueue))
	if mode != envkit.OnlyKueue {
		return mode, nil, nil
	}
	// The dashboard keeps every Queue profile when the LocalQueues cannot
	// be read (fail-open); here no profile is recorded instead.
	queues, err := listModelLocalQueues(c, project)
	return mode, queues, err
}

// listModelLocalQueues lists the names of a project's Kueue LocalQueues
// (kueue.x-k8s.io/v1beta2, the dashboard's LocalQueueModel), with the
// signed-in user's token only (a model's setup).
func listModelLocalQueues(c *kube.Client, project string) ([]string, error) {
	if !c.ActsAsUser() {
		return nil, authz.ErrNotUserClient
	}
	var items []struct {
		Metadata kube.ObjectMeta `json:"metadata"`
	}
	if err := c.ListInto(rhoai.NamespacedPath("kueue.x-k8s.io/v1beta2", "localqueues", project, ""), &items); err != nil {
		if kube.IsK8sNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	names := make([]string, 0, len(items))
	for _, q := range items {
		names = append(names, q.Metadata.Name)
	}
	return names, nil
}

// ListHardwareProfiles lists the hardware profiles in ns (the
// dashboard's namespace holds the global ones).
func ListHardwareProfiles(c *kube.Client, ns string) ([]HardwareProfileObject, error) {
	var items []HardwareProfileObject
	err := c.ListInto(rhoai.NamespacedPath("infrastructure.opendatahub.io/v1", "hardwareprofiles", ns, ""), &items)
	return items, err
}

// ChooseModelProfile is the profile a model of entry is recorded under:
// the first by name that the dashboard's Kueue rule keeps for the project
// (kueueFilter: mode, and its LocalQueues when mode is OnlyKueue) and that
// fits the model (Fits). It returns why none does otherwise.
func ChooseModelProfile(items []HardwareProfileObject, mode envkit.KueueMode, queues []string, entry CatalogModel) (HardwareProfileObject, string) {
	if mode == envkit.NoProfiles {
		return HardwareProfileObject{}, "the project uses Kueue (label kueue.openshift.io/managed=true) while Kueue is off in the dashboard, so the dashboard offers no hardware profile there"
	}
	items = slices.Clone(items)
	sort.Slice(items, func(i, j int) bool { return items[i].Metadata.Name < items[j].Metadata.Name })
	var why []string
	for _, p := range items {
		reason := kueueFilter(p, mode, queues)
		if reason == "" {
			reason = p.Fits(entry)
		}
		if reason == "" {
			return p, ""
		}
		why = append(why, p.Metadata.Name+": "+reason)
	}
	if len(why) == 0 {
		return HardwareProfileObject{}, "the cluster has no hardware profiles"
	}
	return HardwareProfileObject{}, fmt.Sprintf("no hardware profile allows %s CPUs and %s-%s of memory (%s)", entry.CPU, entry.MemoryRequest, entry.Memory, strings.Join(why, "; "))
}

// pickHardwareProfile finds, in the dashboard's namespace, the profile the
// model is recorded under (ChooseModelProfile). It returns the profile, or
// a zero profile and why none was used. Outside a Kueue project the
// profile is a label in the dashboard, never a reason to stop (a read
// error is a reason too). In a project the dashboard treats as Kueue's
// (envkit.KueueModeFor: OnlyKueue or NoProfiles) the dashboard deploys a
// model only with a Queue hardware profile of a LocalQueue the project
// has (the deploy form's hardware profile is required, odh-dashboard
// rhoai-3.7-ea.1 packages/hardware-profiles/shared/HardwareProfileFormSection.tsx
// L141, and HardwareProfileSelect.tsx L455-459 offers only those), so a
// model without one would run around Kueue's queue and quota: refusal is
// then why the model is not set up (fail closed, also when a read fails).
func pickHardwareProfile(c *kube.Client, project string, entry CatalogModel) (hw HardwareProfile, why, refusal string) {
	ns, err := rhoai.AppsNamespace(c)
	if err != nil {
		// Whether the project is Kueue's does not need the dashboard's
		// namespace: one that is (or cannot be read) refuses.
		meta, found, rerr := kube.ReadObjectMeta(c, "/api/v1/namespaces/"+project)
		if rerr != nil || (found && meta.Labels[envkit.KueueProjectLabel] == "true") {
			return HardwareProfile{}, "", fmt.Sprintf("Cannot tell which hardware profile the dashboard offers in project %s, which uses Kueue or cannot be read: the dashboard's namespace is not known (%v).", project, err)
		}
		return HardwareProfile{}, fmt.Sprintf("the dashboard's namespace is not known (%v)", err), ""
	}
	mode, queues, err := projectKueueMode(c, project, ns)
	if err != nil {
		return HardwareProfile{}, "", fmt.Sprintf("Cannot tell whether project %s uses Kueue: %v.", project, err)
	}
	refuse := func(why string) (HardwareProfile, string, string) {
		switch mode {
		case envkit.OnlyNonKueue:
			return HardwareProfile{}, why, ""
		case envkit.NoProfiles:
			return HardwareProfile{}, why, fmt.Sprintf("Project %s has the label %s=true while Kueue is off in the dashboard, so the dashboard offers no hardware profile there and deploys no model; this tool does not deploy one around Kueue either.", project, envkit.KueueProjectLabel)
		}
		return HardwareProfile{}, why, fmt.Sprintf("Project %s uses Kueue, where the dashboard deploys a model only with a Queue hardware profile of a LocalQueue the project has, and %s.", project, why)
	}
	items, err := ListHardwareProfiles(c, ns)
	if err != nil {
		if kube.IsDecodeError(err) {
			return refuse("the hardware profiles cannot be parsed")
		}
		return refuse(fmt.Sprintf("the hardware profiles in %s cannot be read (%v)", ns, err))
	}
	p, why := ChooseModelProfile(items, mode, queues, entry)
	if why != "" {
		return refuse(why)
	}
	return HardwareProfile{Name: p.Metadata.Name, Namespace: ns, ResourceVersion: p.Metadata.ResourceVersion}, "", ""
}
