package serving

import (
	"encoding/json"
	"fmt"
	"net/url"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
)

// A model's image pull that fails is not at once a reason to stop: live
// (ROSA HCP, 2026-11-18) a model's pod landed on a node created three
// minutes earlier, failed with ErrImagePull, then ImagePullBackOff, and
// pulled by itself a few minutes later, once the cluster's credentials for
// quay.io/rhoai had reached the new node. So the setup waits on a failing
// pull for a grace window, longer on a young node, and only then fails
// with what to check. A registry that does not have the image (manifest
// unknown) still fails at once: waiting cannot help.
var (
	// ModelImagePullGrace is how long a failing pull is waited on.
	ModelImagePullGrace = 2 * time.Minute
	// ModelImagePullGraceYoungNode is the window on a node that joined less
	// than ModelYoungNodeAge ago.
	ModelImagePullGraceYoungNode = 8 * time.Minute
	ModelYoungNodeAge            = 15 * time.Minute
)

// modelPullGrace is the window for a pull failing on node, and a note on
// the node's age ("" when it is not young or cannot be read). The node is
// read by name with the list this app already has on nodes (a field
// selector, no get rule).
func modelPullGrace(c *kube.Client, node string) (time.Duration, string) {
	if node == "" {
		return ModelImagePullGrace, ""
	}
	q := url.Values{"fieldSelector": {"metadata.name=" + node}}
	body, _, err := c.Get("/api/v1/nodes?" + q.Encode())
	if err != nil {
		return ModelImagePullGrace, ""
	}
	var list struct {
		Items []struct {
			Metadata kube.ObjectMeta `json:"metadata"`
		} `json:"items"`
	}
	if json.Unmarshal(body, &list) != nil || len(list.Items) != 1 {
		return ModelImagePullGrace, ""
	}
	created, err := time.Parse(time.RFC3339, list.Items[0].Metadata.CreationTimestamp)
	if err != nil {
		return ModelImagePullGrace, ""
	}
	age := kube.Clock().Now().Sub(created)
	if age < 0 || age >= ModelYoungNodeAge {
		return ModelImagePullGrace, ""
	}
	return ModelImagePullGraceYoungNode, fmt.Sprintf(" Node %s joined the cluster %s ago: a new node can get its pull credentials a few minutes after it joins.", node, minutesText(age))
}

// minutesText is a duration in whole minutes, in words.
func minutesText(d time.Duration) string {
	m := int(d.Round(time.Minute) / time.Minute)
	switch {
	case m < 1:
		return "less than a minute"
	case m == 1:
		return "1 minute"
	}
	return fmt.Sprintf("%d minutes", m)
}
