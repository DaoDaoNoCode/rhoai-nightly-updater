package cluster

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
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
	hardwareProfileNameAnnotation      = "opendatahub.io/hardware-profile-name"
	hardwareProfileNamespaceAnnotation = "opendatahub.io/hardware-profile-namespace"
	hardwareProfileVersionAnnotation   = "opendatahub.io/hardware-profile-resource-version"
	hardwareProfileDisabledAnnotation  = "opendatahub.io/disabled"
)

// hardwareProfile is the profile a model is recorded under; zero when none.
type hardwareProfile struct {
	Name, Namespace, ResourceVersion string
}

type hardwareProfileObject struct {
	Metadata objectMeta `json:"metadata"`
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
func (p hardwareProfileObject) visibleForModelServing() bool {
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

// fits says why the profile cannot hold the model, or "" when it can: it
// must be enabled, describe only CPU and memory (no accelerator), add no
// scheduling (the tool would have to apply it as the wizard does), and its
// ranges must contain the model's requests and limits.
func (p hardwareProfileObject) fits(entry catalogModel) string {
	if p.Metadata.Annotations[hardwareProfileDisabledAnnotation] == "true" {
		return "disabled"
	}
	if !p.visibleForModelServing() {
		return "the dashboard shows it only for " + p.Metadata.Annotations[hardwareProfileVisibilityAnnotation]
	}
	if s := p.Spec.Scheduling; s != nil && (s.Kueue != nil || (s.Node != nil && (len(s.Node.NodeSelector) > 0 || len(s.Node.Tolerations) > 0))) {
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
		lo, okLo := parseQuantity(quantityString(id.MinCount))
		hi, okHi := parseQuantity(quantityString(id.MaxCount))
		for _, v := range values {
			n, _ := parseQuantity(v)
			if (okLo && n < lo) || (okHi && n > hi) {
				return fmt.Sprintf("%s %s is outside %s-%s", id.Identifier, v, quantityString(id.MinCount), firstNonEmpty(quantityString(id.MaxCount), "any"))
			}
		}
	}
	if !seen["cpu"] || !seen["memory"] {
		return "it does not set both CPU and memory"
	}
	return ""
}

// pickHardwareProfile finds, in the dashboard's namespace, the first
// profile by name that fits the model (see fits). It returns the profile,
// or a zero profile and why none was used; a read error is a reason too:
// the profile is a label in the dashboard, never a reason to stop.
func pickHardwareProfile(c *Client, entry catalogModel) (hardwareProfile, string) {
	ns, err := appsNamespace(c)
	if err != nil {
		return hardwareProfile{}, fmt.Sprintf("the dashboard's namespace is not known (%v)", err)
	}
	body, _, err := c.get(namespacedPath("infrastructure.opendatahub.io/v1", "hardwareprofiles", ns, ""))
	if err != nil {
		return hardwareProfile{}, fmt.Sprintf("the hardware profiles in %s cannot be read (%v)", ns, err)
	}
	var list struct {
		Items []hardwareProfileObject `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return hardwareProfile{}, "the hardware profiles cannot be parsed"
	}
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Metadata.Name < list.Items[j].Metadata.Name })
	var why []string
	for _, p := range list.Items {
		reason := p.fits(entry)
		if reason == "" {
			return hardwareProfile{Name: p.Metadata.Name, Namespace: ns, ResourceVersion: p.Metadata.ResourceVersion}, ""
		}
		why = append(why, p.Metadata.Name+": "+reason)
	}
	if len(why) == 0 {
		return hardwareProfile{}, "the cluster has no hardware profiles"
	}
	return hardwareProfile{}, fmt.Sprintf("no hardware profile allows %s CPUs and %s-%s of memory (%s)", entry.CPU, entry.MemoryRequest, entry.Memory, strings.Join(why, "; "))
}
