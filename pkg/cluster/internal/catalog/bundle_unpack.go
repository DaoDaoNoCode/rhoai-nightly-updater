package catalog

import (
	"fmt"
	"net/url"
	"slices"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
)

// BundleUnpackRefLabel is the label OLM puts on the Job, and on the pod
// template of the Job, that unpacks an operator bundle into a ConfigMap for
// an InstallPlan. Its value is the hash of the bundle image, which also
// names the Job and the ConfigMap; the ConfigMap is owned by the
// CatalogSource and lives in its namespace, and so do the Job and its pod
// (openshift/operator-framework-olm release-4.22,
// staging/operator-lifecycle-manager/pkg/controller/bundle/bundle_unpacker.go:
// the label at 51-52, the Job and pod labels at 96-112, the "pull" init
// container that runs the bundle image at 192-195, the Job's namespace and
// owner at 285-287, the ConfigMap at 672-677). The tool's Subscription
// always uses a catalog in rhoai.CatalogNS (BuildSubscription).
const BundleUnpackRefLabel = "operatorframework.io/bundle-unpack-ref"

// BundleUnpackPullFailures reads OLM's bundle-unpack pods in the catalog
// namespace and words each one whose container cannot pull its image, for
// example "OLM could not pull the operator bundle registry.redhat.io/...:
// ImagePullBackOff (...)". Like OLM, which surfaces the waiting reasons of
// a pending unpack pod's init containers on the bundle lookup
// (bundle_unpacker.go:582-608 and 633-669), it reads only Pending pods.
//
// OLM 4.22 and older do not copy that reason to the Subscription: while
// the unpack Job runs, its BundleUnpacking condition reads only
// UnpackingInProgress, with no message (operator-framework-olm release-4.22,
// staging/operator-lifecycle-manager/pkg/controller/operators/catalog/operator.go:1423-1440),
// until the Job's activeDeadlineSeconds (the catalog operator's
// --bundle-unpack-timeout, 10 minutes by default: cmd/catalog/start.go:82)
// fails it and OLM sets BundleUnpackFailed (operator.go:1396-1421). The
// pods are the only place that says why the unpack waits.
//
// The pods are not tied to one bundle: the tool does not know the bundle
// image the catalog resolves to, and the Job's ConfigMap owner cannot be
// read with the existing permissions. The message names the image instead.
// The list is the existing pod read in rhoai.CatalogNS (catalogPodPullFailure).
func BundleUnpackPullFailures(c *kube.Client) ([]string, error) {
	path := rhoai.NamespacedPath("v1", "pods", rhoai.CatalogNS, "") + "?labelSelector=" + url.QueryEscape(BundleUnpackRefLabel)
	var items []struct {
		Status struct {
			Phase                 string `json:"phase"`
			InitContainerStatuses []struct {
				Image string `json:"image"`
				containerWaitStatus
			} `json:"initContainerStatuses"`
		} `json:"status"`
	}
	if err := c.ListInto(path, &items); err != nil {
		return nil, err
	}
	var failures []string
	for _, pod := range items {
		if pod.Status.Phase != "Pending" {
			continue
		}
		for _, cs := range pod.Status.InitContainerStatuses {
			waiting := cs.State.Waiting
			if waiting == nil || !imagePullFailureReasons[waiting.Reason] {
				continue
			}
			detail := waiting.Reason
			if waiting.Message != "" {
				detail += " (" + kube.TruncateText(waiting.Message, 300) + ")"
			}
			msg := fmt.Sprintf("OLM could not pull the operator bundle %s: %s", kube.NonEmpty(cs.Image, "(image not reported)"), detail)
			if !slices.Contains(failures, msg) {
				failures = append(failures, msg)
			}
		}
	}
	return failures, nil
}
