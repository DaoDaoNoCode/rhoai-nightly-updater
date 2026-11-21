package cluster

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// maasDocsSamplePriority is the priority of the docs' free sample
// subscription (docs/samples/maas-system/free), which this tool's
// subscription uses while it is the only one.
const maasDocsSamplePriority = 10

// maasSubscriptionPriority is the priority this tool's MaaSSubscription
// takes: the docs' sample's while no other subscription exists, else
// strictly below the lowest of the others'. A key minted without naming a
// subscription goes to the caller's highest-priority one, and equal
// priorities are a tie the controller flags (SpecPriorityDuplicate)
// (concepts/api-key-authentication.md, "Subscription Binding and
// Priority"): this tool's subscription, which every signed-in user is in,
// must never take a key meant for someone else's. It also returns the
// others' names. The API server validates the value (the publish dry run).
func maasSubscriptionPriority(c *Client) (int, []string, error) {
	var list struct {
		Items []struct {
			Metadata objectMeta `json:"metadata"`
			Spec     struct {
				Priority int `json:"priority"`
			} `json:"spec"`
		} `json:"items"`
	}
	found, err := c.getIntoOptional(namespacedPath(maasAPI(c), "maassubscriptions", maasTenantNamespace, ""), &list)
	if err != nil {
		if isDecodeError(err) {
			return 0, nil, fmt.Errorf("parse the MaaSSubscription list: %w", err)
		}
		return 0, nil, err
	}
	if !found {
		return maasDocsSamplePriority, nil, nil
	}
	var others []string
	lowest := 0
	for _, it := range list.Items {
		if toolOwned(it.Metadata) {
			continue
		}
		if len(others) == 0 || it.Spec.Priority < lowest {
			lowest = it.Spec.Priority
		}
		others = append(others, it.Metadata.Name)
	}
	sort.Strings(others)
	if len(others) == 0 {
		return maasDocsSamplePriority, nil, nil
	}
	return lowest - 1, others, nil
}

// maasOwnSubscriptionPriority is the priority of this tool's subscription
// as it is (Unpublish keeps it), or the docs' sample's.
func maasOwnSubscriptionPriority(c *Client) int {
	body, _, err := c.get(maasAccessPath(c, "MaaSSubscription"))
	if err != nil {
		return maasDocsSamplePriority
	}
	var sub struct {
		Spec struct {
			Priority *int `json:"priority"`
		} `json:"spec"`
	}
	if json.Unmarshal(body, &sub) != nil || sub.Spec.Priority == nil {
		return maasDocsSamplePriority
	}
	return *sub.Spec.Priority
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
