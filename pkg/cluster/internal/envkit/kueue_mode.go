package envkit

import (
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
)

// Same Kueue hardware-profile rule the dashboard uses everywhere this app mirrors
// dashboard forms (Workbench Create workbench, Models deployment): odh-dashboard rhoai-3.6
// packages/hardware-profiles/shared/kueueUtils.ts kueueFilteringState and
// filterProfilesByKueue.

// KueueProjectLabel signals that Kueue may schedule workloads for that namespace
// (packages/k8s-core k8sTypes.ts KnownLabels.KUEUE_MANAGED).
const KueueProjectLabel = "kueue.openshift.io/managed"

// KueueMode mirrors dashboard kueueFilteringState (kueueUtils.ts L56-97).
type KueueMode int

const (
	OnlyNonKueue KueueMode = iota
	OnlyKueue
	NoProfiles
)

// KueueModeFor maps kueueFilteringState: namespaces labeled kueue.openshift.io/managed=true get
// Queue-only profiles while Kueue runs, else none; unlabeled namespaces get
// non-Queue profiles only.
func KueueModeFor(projectKueue, kueueAvailable bool) KueueMode {
	switch {
	case projectKueue && kueueAvailable:
		return OnlyKueue
	case projectKueue:
		return NoProfiles
	}
	return OnlyNonKueue
}

// ComponentOn matches dashboard isAreaAvailable for a single component—returns true when
// status.components.<key>.managementState is Managed or Unmanaged; falls back to spec
// when status omits the component.
func ComponentOn(dsc rhoai.DSCRead, key string) bool {
	status, _ := dsc.Object["status"].(map[string]any)
	comps, _ := status["components"].(map[string]any)
	if comp, ok := comps[key].(map[string]any); ok {
		if s, _ := comp["managementState"].(string); s != "" {
			return s == "Managed" || s == "Unmanaged"
		}
	}
	s, _ := ComponentManagement(dsc.Object, key)
	return s == "Managed" || s == "Unmanaged"
}

// KueueAvailable follows the dashboard Kueue area check (frontend/src/concepts/areas
// const.ts SupportedArea.KUEUE): dashboardConfig.disableKueue is false (its
// default is true, so unset counts as true) and the DataScienceCluster kueue
// component is on.
func KueueAvailable(dsc rhoai.DSCRead, disableKueue *bool) bool {
	return ComponentOn(dsc, "kueue") && disableKueue != nil && !*disableKueue
}
