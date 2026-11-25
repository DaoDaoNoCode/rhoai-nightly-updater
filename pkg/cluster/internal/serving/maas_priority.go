package serving

import (
	"fmt"
	"log/slog"
	"sort"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// maasSubscriptionPriority is the spec.priority every write of this tool's
// MaaSSubscription sets: Publish, Change limit, Change who can use them,
// Unpublish of one of several models, the background prune, and Set to the
// lowest priority. It is never computed from the other subscriptions.
//
// maas-api binds a key minted without naming a subscription to the
// caller's subscription with the highest spec.priority; on equal
// priorities, to the one with the largest single token limit (unlimited
// counts as the largest), then the first name (models-as-a-service
// rhoai-3.7-ea.1 dd6be8b and main 7e56355,
// maas-api/internal/subscription/selector.go sortSubscriptionsByPriority;
// docs concepts/api-key-authentication.md "Subscription Binding and
// Priority"). It reads a negative spec.priority as 0 (selector.go, "priority
// >= 0"), although the CRD has no minimum. 0 is the CRD's default
// (maas-controller/api/maas/v1alpha1/maassubscription_types.go
// "+kubebuilder:default=0") and so the lowest value maas-api honours: a
// subscription someone creates later without a priority ties with this
// tool's, which every signed-in user is in, instead of losing to it, and one
// with priority 1 or more wins. On a tie, maas-api takes the larger token
// limit, then the name; naming the subscription when minting the key avoids
// the choice.
const maasSubscriptionPriority = 0

// maasSubscriptionListed is a MaaSSubscription in models-as-a-service as
// listed: its spec.priority as written (unset reads as 0, the CRD's
// default).
type maasSubscriptionListed struct {
	Name     string
	Priority int64
	Deleting bool
}

// maasOtherSubscriptions lists the MaaSSubscriptions in models-as-a-service
// that are not this tool's, by name.
func maasOtherSubscriptions(c *kube.Client) ([]maasSubscriptionListed, error) {
	var list struct {
		Items []struct {
			Metadata kube.ObjectMeta `json:"metadata"`
			Spec     struct {
				Priority *int64 `json:"priority"`
			} `json:"spec"`
		} `json:"items"`
	}
	found, err := c.GetIntoOptional(rhoai.NamespacedPath(MaaSAPI(c), "maassubscriptions", MaaSTenantNamespace, ""), &list)
	if err != nil {
		if kube.IsDecodeError(err) {
			return nil, fmt.Errorf("parse the MaaSSubscription list: %w", err)
		}
		return nil, err
	}
	if !found {
		return nil, nil
	}
	var out []maasSubscriptionListed
	for _, it := range list.Items {
		if ToolOwned(it.Metadata) {
			continue
		}
		var priority int64
		if it.Spec.Priority != nil {
			priority = *it.Spec.Priority
		}
		out = append(out, maasSubscriptionListed{Name: it.Metadata.Name, Priority: priority, Deleting: it.Metadata.Terminating()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// maasSubscriptionNames are the subscriptions' names.
func maasSubscriptionNames(subs []maasSubscriptionListed) []string {
	var out []string
	for _, s := range subs {
		out = append(out, s.Name)
	}
	return out
}

// maasPriorityTies are the subscriptions maas-api ranks level with this
// tool's at priority 0: spec.priority 0, unset or negative (read as 0), not
// being deleted (maas-api leaves those out).
func maasPriorityTies(subs []maasSubscriptionListed) []string {
	var out []string
	for _, s := range subs {
		if s.Priority <= maasSubscriptionPriority && !s.Deleting {
			out = append(out, s.Name)
		}
	}
	return out
}

// maasPriorityState is what the MaaS tab shows about the priority of this
// tool's subscription sub (found and this tool's): its value, the fix while
// it is not 0 (an earlier version wrote 10, or a value below the other
// subscriptions'), and the other subscriptions it ties with.
func maasPriorityState(c *kube.Client, sub maasToolSub) *types.MaaSSubscriptionPriority {
	out := &types.MaaSSubscriptionPriority{Value: sub.priority}
	if sub.priority != maasSubscriptionPriority {
		out.Fix = maasPriorityAction(sub.priority)
	}
	others, err := maasOtherSubscriptions(c)
	if err != nil {
		slog.Debug("cannot list the MaaSSubscriptions for the priority note", "error", err)
		return out
	}
	out.Ties = maasPriorityTies(others)
	return out
}

// maasPriorityAction is the Set to the lowest priority action.
func maasPriorityAction(from int64) *types.MaaSAction {
	return &types.MaaSAction{
		ID: "subscription-priority", Label: "Set to the lowest priority (0)",
		Summary: fmt.Sprintf("Sets the priority of this tool's subscription from %d to 0, the lowest MaaS honours, so that a key minted without naming a subscription goes to the caller's own subscription when it has a priority of 1 or more, and, at 0 too, to the one with the larger token limit, then by name. Keys made before keep the subscription they were bound to: revoke them and make them again.", from),
		Changes: []string{fmt.Sprintf("MaaSSubscription %s/%s: spec.priority %d to 0 (a merge patch with the read resourceVersion, after a server-side dry run); nothing else of it changes.", MaaSTenantNamespace, MaaSSubscriptionName, from)},
	}
}

// maasNotReadyRowReason is why a model row cannot be published while the
// platform is not ready: short, since the part above the table says the
// rest.
func maasNotReadyRowReason(st *types.MaaSStatus) string {
	if !st.Platform.Deployed && !st.Platform.ManagedByTool {
		return "Set up the MaaS platform first."
	}
	for _, p := range st.Parts {
		if p.State == "ready" || (p.ID == "authorino-tls" && p.State == "unknown") {
			continue
		}
		return "The MaaS platform is not ready: " + p.Title + " (see above)."
	}
	return "The MaaS platform is not ready."
}

// maasNotReadyReason names the first part that keeps the platform from
// being ready, with its message.
func maasNotReadyReason(st *types.MaaSStatus) string {
	for _, p := range st.Parts {
		if p.State == "ready" || (p.ID == "authorino-tls" && p.State == "unknown") {
			continue
		}
		fix := "Set up on the MaaS tab does what is missing."
		switch {
		case len(p.Actions) > 0:
			fix = p.Actions[0].Label + " on the MaaS tab fixes it."
		case p.State == "waiting":
			fix = "It is still starting; publish once it is ready."
		case p.State == "manual" || p.State == "problem" || p.State == "unknown":
			fix = "The MaaS tab shows what it needs."
		}
		return fmt.Sprintf("The MaaS platform is not ready: %s: %s. %s", p.Title, trimPeriod(p.Message), fix)
	}
	return "The MaaS platform is not ready (" + st.Platform.Message + ")."
}

func trimPeriod(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '.' || s[len(s)-1] == ' ') {
		s = s[:len(s)-1]
	}
	return s
}
