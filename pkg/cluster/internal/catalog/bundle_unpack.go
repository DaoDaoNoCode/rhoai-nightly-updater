package catalog

import (
	"fmt"
	"net/url"
	"slices"
	"strings"

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
// owner at 285-287, the ConfigMap at 672-677).
const BundleUnpackRefLabel = "operatorframework.io/bundle-unpack-ref"

// BundleUnpackTimeoutAnnotation on a namespace's only OperatorGroup sets
// OLM's bundle unpack timeout for the Subscriptions in that namespace, in
// place of the catalog operator's --bundle-unpack-timeout
// (bundle_unpacker.go:40-44, 307-319 and 962-987).
const BundleUnpackTimeoutAnnotation = "operatorframework.io/bundle-unpack-timeout"

// BundleUnpackPullFailures reads OLM's bundle-unpack pods in namespace, the
// namespace of the Subscription's CatalogSource, and words each one whose
// "pull" init container (the one that runs the bundle image) cannot pull
// it, for example "an OLM bundle unpack in openshift-marketplace cannot
// pull registry.redhat.io/...: ImagePullBackOff (manifest unknown)". Like
// OLM, which surfaces the waiting reasons of a pending unpack pod's init
// containers on the bundle lookup (bundle_unpacker.go:582-608 and 633-669),
// it reads only Pending pods.
//
// OLM 4.22 and older do not copy that reason to the Subscription: while
// the unpack Job runs, its BundleUnpacking condition reads only
// UnpackingInProgress, with no message (operator-framework-olm release-4.22,
// staging/operator-lifecycle-manager/pkg/controller/operators/catalog/operator.go:1423-1440),
// until the Job's activeDeadlineSeconds (the cluster's bundle unpack
// timeout) fails it and OLM sets BundleUnpackFailed (operator.go:1396-1421).
// The pods are the only place that says why the unpack waits.
//
// The pods are not tied to one bundle: the Update path does not know the
// bundle image the catalog resolves to (it knows the head CSV's name from
// the PackageManifest), and the Job's ConfigMap owner cannot be read with
// the existing permissions. So the result is worded as what is seen in the
// namespace, with the image, never as RHOAI's own unpack. The kubelet's
// message is never quoted: a registry error can carry a signed URL.
func BundleUnpackPullFailures(c *kube.Client, namespace string) ([]string, error) {
	path := rhoai.NamespacedPath("v1", "pods", namespace, "") + "?labelSelector=" + url.QueryEscape(BundleUnpackRefLabel)
	var items []struct {
		Status struct {
			Phase                 string `json:"phase"`
			InitContainerStatuses []struct {
				Name  string `json:"name"`
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
			if cs.Name != bundlePullContainer || waiting == nil || !imagePullFailureReasons[waiting.Reason] {
				continue
			}
			detail := waiting.Reason
			if waiting.Message != "" {
				detail += " (" + pullErrorKind(waiting.Message) + ")"
			}
			msg := fmt.Sprintf("an OLM bundle unpack in %s cannot pull %s: %s", namespace, kube.NonEmpty(cs.Image, "(image not reported)"), detail)
			if !slices.Contains(failures, msg) {
				failures = append(failures, msg)
			}
		}
	}
	return failures, nil
}

// bundlePullContainer is the init container of OLM's unpack pod that runs
// the bundle image (bundle_unpacker.go:192-195); the others run OLM's own
// images.
const bundlePullContainer = "pull"

// pullErrorKind is a display hint only: it names the kind of error a
// kubelet image pull message seems to describe, so the message itself is
// never shown. The kubelet's and the container runtime's wording is not an
// API and differs between versions and registries, so it is matched
// loosely, anything else is the neutral "pull error", and nothing is
// decided on it.
func pullErrorKind(message string) string {
	m := strings.ToLower(message)
	switch {
	case strings.Contains(m, "manifest unknown"):
		return "manifest unknown"
	case strings.Contains(m, "unauthorized"), strings.Contains(m, "authentication required"):
		return "unauthorized"
	case strings.Contains(m, "not found"):
		return "not found"
	case strings.Contains(m, "qps exceeded"):
		return "pull QPS exceeded"
	default:
		return "pull error"
	}
}
