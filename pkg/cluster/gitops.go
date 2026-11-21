package cluster

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Argo CD writes this annotation on every object it syncs when it tracks
// resources by annotation (the "annotation" and "annotation+label" methods;
// the default of upstream Argo CD since 3.0 and of OpenShift GitOps since
// 1.19, which defaulted to "label" before). Its value is
// "<application>:<group>/<kind>:<namespace>/<name>"; the application part is
// "<namespace>_<name>" for applications outside Argo CD's own namespace.
//
// Neither this annotation nor the label method's app.kubernetes.io/instance
// decides whether Argo CD manages an object: argocd.go asks Argo CD's
// Applications, and the marks only name the application.
const argoTrackingAnnotation = "argocd.argoproj.io/tracking-id"

// argoApplication returns the Argo CD application that the tracking
// annotation among annotations names for the object of this group, kind,
// namespace and name, or "" when none does. The annotation alone does not
// show that the application manages the object: argocd.go asks Argo CD, and
// every refusal reads that verdict (argoTracking.managedBy). The tracking id
// must name the object itself, as Argo CD checks it (controller/state.go,
// isSelfReferencedObj: namespace, name, group and kind): Argo CD ignores an
// annotation copied from another object, so the tool does too. namespace ""
// skips the namespace check: for a cluster-scoped object Argo CD writes its
// destination namespace there.
func argoApplication(annotations map[string]string, group, kind, namespace, name string) string {
	app, rest, ok := strings.Cut(annotations[argoTrackingAnnotation], ":")
	if !ok || app == "" {
		return ""
	}
	groupKind, nsName, ok := strings.Cut(rest, ":")
	if !ok {
		return ""
	}
	idGroup, idKind, ok := strings.Cut(groupKind, "/")
	if !ok {
		return ""
	}
	idNS, idName, ok := strings.Cut(nsName, "/")
	if !ok || idGroup != group || idKind != kind || idName != name || (namespace != "" && idNS != namespace) {
		return ""
	}
	return app
}

// lastAppliedAnnotation is where client-side apply, which Argo CD uses by
// default, keeps what it applied; it diffs against it.
const lastAppliedAnnotation = "kubectl.kubernetes.io/last-applied-configuration"

// keepArgoTracking copies, when previous carries Argo CD's tracking (its
// tracking annotation, or the instance label that label tracking writes),
// its annotations (Argo CD's tracking id, installation id, sync options...,
// the last-applied annotation, and any other Git sets) and its labels from
// previous to obj, without overwriting what obj already has. A Subscription
// the tool recreates then stays part of the same application and matches
// what Argo CD applied (the chart's labels and annotations, its last apply),
// so Argo CD neither shows it as missing nor changes it again (seen on a live
// cluster without them). Finalizers and other runtime metadata are not
// copied.
func keepArgoTracking(previous, obj map[string]any) {
	prevMeta, _ := previous["metadata"].(map[string]any)
	prevAnn, _ := prevMeta["annotations"].(map[string]any)
	prevLabels, _ := prevMeta["labels"].(map[string]any)
	id, _ := prevAnn[argoTrackingAnnotation].(string)
	instance, _ := prevLabels[argoInstanceLabel].(string)
	if id == "" && instance == "" {
		return
	}
	meta, _ := obj["metadata"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
		obj["metadata"] = meta
	}
	ann, _ := meta["annotations"].(map[string]any)
	if ann == nil {
		ann = map[string]any{}
		meta["annotations"] = ann
	}
	for k, v := range prevAnn {
		if _, set := ann[k]; !set {
			ann[k] = v
		}
	}
	if len(prevLabels) > 0 {
		labels, _ := meta["labels"].(map[string]any)
		if labels == nil {
			labels = map[string]any{}
			meta["labels"] = labels
		}
		for k, v := range prevLabels {
			if _, set := labels[k]; !set {
				labels[k] = v
			}
		}
	}
}

// gitOpsRefusal explains a refused action on an object Argo CD manages.
func gitOpsRefusal(object, app, action string) string {
	return fmt.Sprintf("%s is managed by Argo CD application %q, which would undo %s. Make the change in that application's Git repository instead. Nothing was changed.", object, app, action)
}

// acmPolicyManager is the field manager the API server records for ACM's
// configuration policy controller, which applies an enforced
// ConfigurationPolicy (remediationAction: enforce) on a managed cluster. It
// creates and updates the objects without a fieldManager
// (open-cluster-management-io/config-policy-controller
// controllers/configurationpolicy_controller.go: res.Update(ctx,
// obj.existingObj, metav1.UpdateOptions{FieldValidation: ...})), so the API
// server takes the manager from the User-Agent up to its first "/"
// (kubernetes staging/src/k8s.io/apiserver/pkg/endpoints/handlers/create.go,
// managerOrUserAgent), which client-go starts with the binary's name
// (staging/src/k8s.io/client-go/rest/config.go, DefaultKubernetesUserAgent):
// /usr/local/bin/config-policy-controller in the controller's image
// (build/Dockerfile). Seen on a live cluster where a policy enforces the
// nightly CatalogSource: its managedFields are exactly
// config-policy-controller (Update, f:spec) and catalog (Update, f:status),
// and it has no Argo CD annotation.
const acmPolicyManager = "config-policy-controller"

// acmCatalogManager names that manager in messages.
const acmCatalogManager = "an ACM policy (" + acmPolicyManager + ")"

// acmEnforcesSpec reports whether an ACM policy enforces the object's spec:
// config-policy-controller owns f:spec in its managedFields. The policies
// themselves are not read: their copies on a managed cluster can hold
// rendered hub templates, and the managedFields entry is on the object
// anyway, in the same GET.
//
// Known limit: the entry outlives the policy that wrote it (a policy removed
// with pruneObjectBehavior None, the default, leaves the object and its
// managedFields as they are, until another manager takes over those fields). The updater then still treats the catalog as
// managed, which is the safe direction: it refuses to replace the catalog,
// and the gitops profile only installs what the catalog serves.
func acmEnforcesSpec(meta objectMeta) bool {
	return meta.ownsField(acmPolicyManager, "f:spec")
}

// operatorGitOps is how Argo CD or ACM manages the operator's catalog and
// Subscription, read before an operator operation.
type operatorGitOps struct {
	catalogApp string // Argo CD application managing CatalogSource rhoai-catalog-dev
	// catalogACM: no Argo CD application manages the catalog, and an ACM
	// policy enforces its spec (acmEnforcesSpec). Argo CD wins when both do.
	catalogACM      bool
	subscriptionApp string // Argo CD application managing the rhods-operator Subscription
	// What Argo CD said about each tracking annotation (argocd.go);
	// catalogApp and subscriptionApp are their managedBy.
	catalogArgo, subscriptionArgo argoTracking
}

func (g operatorGitOps) active() bool {
	return g.catalogManaged() || g.subscriptionApp != ""
}

// catalogManaged reports whether Argo CD or an ACM policy sets the nightly
// catalog, so the catalog's image is not the updater's to change.
func (g operatorGitOps) catalogManaged() bool {
	return g.catalogApp != "" || g.catalogACM
}

// catalogManager names who manages the nightly catalog, for messages:
// `Argo CD application "x"`, "an ACM policy (config-policy-controller)",
// or "" when neither does.
func (g operatorGitOps) catalogManager() string {
	switch {
	case g.catalogApp != "":
		return fmt.Sprintf("Argo CD application %q", g.catalogApp)
	case g.catalogACM:
		return acmCatalogManager
	}
	return ""
}

// catalogSpecFrom names where the catalog's spec comes from, for "left as
// ... sets it": the ACM policy, else Git.
func (g operatorGitOps) catalogSpecFrom() string {
	if g.catalogApp == "" && g.catalogACM {
		return "the ACM policy"
	}
	return "Git"
}

// catalogChanger says who most likely changed the catalog image while
// Update waited (errCatalogImageChanged).
func (g operatorGitOps) catalogChanger() string {
	switch {
	case g.catalogApp != "":
		return fmt.Sprintf("Argo CD application %q syncing a change from Git", g.catalogApp)
	case g.catalogACM:
		return acmCatalogManager + " enforcing a change"
	case g.subscriptionApp != "":
		return fmt.Sprintf("Argo CD application %q syncing a change from Git", g.subscriptionApp)
	}
	return "another client"
}

// sentenceStart returns s with its first letter in upper case.
func sentenceStart(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// readOperatorGitOps reads which Argo CD applications manage the nightly
// CatalogSource and the operator Subscription (Argo CD's answer, argocd.go),
// and whether an ACM policy enforces the
// catalog (from the managedFields of the same GET). A missing object is not
// managed.
func readOperatorGitOps(c *Client) (operatorGitOps, error) {
	var g operatorGitOps
	argo := newArgoCheck(c)
	for _, r := range []struct {
		path      string
		obj       argoObject
		tracking  *argoTracking
		app       *string
		isCatalog bool
	}{
		{namespacedPath("operators.coreos.com/v1alpha1", "catalogsources", CatalogNS, CatalogName), argoCatalogObject, &g.catalogArgo, &g.catalogApp, true},
		{subscriptionPath(), argoSubscriptionObject, &g.subscriptionArgo, &g.subscriptionApp, false},
	} {
		meta, found, err := readObjectMeta(c, r.path)
		if err != nil {
			return operatorGitOps{}, err
		}
		if found {
			*r.tracking = argo.object(meta.Annotations, meta.Labels, r.obj)
			*r.app = r.tracking.managedBy()
			if r.isCatalog {
				g.catalogACM = g.catalogApp == "" && acmEnforcesSpec(meta)
			}
		}
	}
	return g, nil
}

// refusal explains why an operation is refused while Argo CD manages the
// catalog or Subscription, or an ACM policy the catalog; "" when neither
// does. deletes says what the operation deletes, and for how long.
func (g operatorGitOps) refusal(operation, deletes string) string {
	object, app, note := "The operator Subscription", g.subscriptionApp, g.subscriptionArgo.unconfirmedNote()
	if app == "" {
		object, app, note = "CatalogSource "+CatalogName, g.catalogApp, g.catalogArgo.unconfirmedNote()
	}
	if app == "" && g.catalogACM {
		return fmt.Sprintf("%s is managed by %s, which puts back what differs from the policy. %s deletes %s, so %s is not available here. Nothing was changed.",
			object, acmCatalogManager, operation, deletes, operation)
	}
	if app == "" {
		return ""
	}
	return fmt.Sprintf("%s is managed by Argo CD application %q. %s deletes %s, and Argo CD would recreate it from Git in the meantime, so %s is not available here. Nothing was changed.%s",
		object, app, operation, deletes, operation, note)
}

// readCatalogServes reads the head of the nightly catalog's channel (the
// Subscription's) and whether the installed CSV is that build. It returns nil
// when the Subscription has no channel or the catalog does not list it; read
// errors are logged, not returned, because the rest of the status stands.
func readCatalogServes(c *Client, sub types.SubscriptionInfo, cs types.CatalogSourceInfo, csv types.CSVInfo) *types.CatalogServes {
	if sub.Channel == "" || sub.Source != CatalogName || !cs.Exists {
		return nil
	}
	channels, err := catalogChannels(c, CatalogName)
	if err != nil {
		slog.Warn("catalog head: reading the PackageManifest failed", "error", err)
		return nil
	}
	entry := findChannel(channels, sub.Channel)
	if entry == nil {
		return nil
	}
	head, _ := entry["currentCSV"].(string)
	if head == "" {
		return nil
	}
	desc, _ := entry["currentCSVDesc"].(map[string]any)
	ann, _ := desc["annotations"].(map[string]any)
	s := &types.CatalogServes{Channel: sub.Channel, CSV: head}
	s.OperatorImage, _ = ann["containerImage"].(string)
	s.Built, _ = ann["createdAt"].(string)
	// Right after Argo CD changed the catalog image, the PackageManifest
	// still lists the old build until OLM has replaced the registry pod; the
	// comparison below would then wrongly match the installed operator.
	if cs.Image != "" {
		if ready, stale, ok := readCatalogPods(c, cs.Image); ok && (!ready || stale > 0) {
			s.Updating = true
			return s
		}
	}
	if csv.Name == "" || csv.Phase == "Not Found" {
		return s
	}
	if csv.Name != head {
		s.Installed = new(bool)
		return s
	}
	meta, found, err := readObjectMeta(c, namespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", SubNS, csv.Name))
	if err != nil {
		slog.Warn("catalog head: reading the installed CSV failed", "csv", csv.Name, "error", err)
		return s
	}
	headPrint := buildFingerprint(s.OperatorImage, s.Built)
	installedPrint := buildFingerprint(meta.Annotations["containerImage"], meta.Annotations["createdAt"])
	if found && headPrint != "" && installedPrint != "" {
		installed := headPrint == installedPrint
		s.Installed = &installed
	}
	return s
}

// errCatalogImageChanged: the CatalogSource image changed while the tool
// waited for the catalog to serve the build, which happens when Argo CD syncs
// a new image from Git, or an ACM policy enforces one, meanwhile. by is
// operatorGitOps.catalogChanger.
type errCatalogImageChanged struct{ by, image string }

func (e *errCatalogImageChanged) Error() string {
	return fmt.Sprintf("the CatalogSource image was changed to %s while Update waited, most likely by %s. "+
		"Run Update again to install the build the catalog serves now", e.image, e.by)
}

// catalogServing is what the nightly catalog serves, as one poll reads it.
type catalogServing struct {
	image       string // spec.image
	state       string // connectionState.lastObservedState
	podReady    bool   // a registry pod runs image and is Ready
	stalePods   int    // registry pods that run another image and are not terminating
	head        string // currentCSV of the target channel
	fingerprint string // headFingerprint of that channel
}

// servesTarget reports whether the catalog serves the verified build.
func (s catalogServing) servesTarget(image string, target catalogTarget) bool {
	return s.image == image && s.state == "READY" && s.podReady && s.stalePods == 0 &&
		s.head == target.HeadCSV && (target.Fingerprint == "" || s.fingerprint == target.Fingerprint)
}

func (s catalogServing) describe(target catalogTarget) string {
	switch {
	case !s.podReady || s.stalePods > 0:
		return "OLM is replacing the catalog pod"
	case s.state != "READY":
		return "catalog state " + nonEmpty(s.state, "unknown")
	case s.head != target.HeadCSV:
		return fmt.Sprintf("channel %s lists %s, waiting for %s", target.Channel, nonEmpty(s.head, "nothing"), target.HeadCSV)
	default:
		return "waiting for the catalog to list the new build"
	}
}

// readCatalogPods reads the nightly catalog's registry pods: whether one
// runs image and is Ready, and how many run another image and are not
// terminating. ok is false when the list could not be read.
func readCatalogPods(c *Client, image string) (ready bool, stale int, ok bool) {
	path := namespacedPath("v1", "pods", CatalogNS, "") + "?labelSelector=" + url.QueryEscape("olm.catalogSource="+CatalogName)
	body, _, err := c.Get(path)
	if err != nil {
		return false, 0, false
	}
	{
		var pods struct {
			Items []struct {
				Metadata struct {
					DeletionTimestamp string `json:"deletionTimestamp"`
				} `json:"metadata"`
				Spec struct {
					InitContainers []struct {
						Image string `json:"image"`
					} `json:"initContainers"`
					Containers []struct {
						Image string `json:"image"`
					} `json:"containers"`
				} `json:"spec"`
				Status struct {
					Conditions []struct {
						Type   string `json:"type"`
						Status string `json:"status"`
					} `json:"conditions"`
				} `json:"status"`
			} `json:"items"`
		}
		if json.Unmarshal(body, &pods) != nil {
			return false, 0, false
		}
		for _, p := range pods.Items {
			if p.Metadata.DeletionTimestamp != "" {
				continue
			}
			// The catalog image is the main container, or the second
			// init container when OLM extracts its content.
			serves := false
			for _, ctr := range append(p.Spec.InitContainers, p.Spec.Containers...) {
				serves = serves || ctr.Image == image
			}
			if !serves {
				stale++
				continue
			}
			for _, cond := range p.Status.Conditions {
				if cond.Type == "Ready" && cond.Status == "True" {
					ready = true
				}
			}
		}
	}
	return ready, stale, true
}

// readCatalogServing reads the CatalogSource, its registry pods and its
// PackageManifest entry for target's channel. Read errors leave fields
// empty; the caller keeps polling.
func readCatalogServing(c *Client, image string, target catalogTarget) catalogServing {
	var s catalogServing
	if cs, err := getCatalogSource(c); err == nil {
		s.image, s.state = cs.Image, cs.State
	}
	s.podReady, s.stalePods, _ = readCatalogPods(c, image)
	if channels, err := catalogChannels(c, CatalogName); err == nil {
		if entry := findChannel(channels, target.Channel); entry != nil {
			s.head, _ = entry["currentCSV"].(string)
			s.fingerprint = headFingerprint(entry)
		}
	}
	return s
}

// waitForCatalogServing waits until the CatalogSource serves the verified
// build: its registry pod runs image and is Ready, the catalog is READY, and
// its PackageManifest lists target's head with the same fingerprint as the
// verification catalog. A catalog whose image Argo CD changed in place
// reports READY and its old channels until OLM has replaced the pod, so READY
// alone proves nothing. It returns false on timeout; the error is the context
// error, a *catalogImagePullError, or an *errCatalogImageChanged (by
// changedBy) when the image changed meanwhile. pullAdvice words the pull
// error (pullFailureWatch.advice).
func waitForCatalogServing(c *Client, image string, target catalogTarget, changedBy string, emit func(UpdateStepEvent), logs *[]string, pullAdvice func(*Client) string) (bool, error) {
	deadline := kube.Clock().Now().Add(CatalogReadyTimeout + PackageManifestPropagationWait)
	pull := pullFailureWatch{advice: pullAdvice}
	last := ""
	for kube.Clock().Now().Before(deadline) {
		if err := kube.Clock().Sleep(c.Context(), CatalogPollInterval); err != nil {
			return false, err
		}
		s := readCatalogServing(c, image, target)
		if s.image != "" && s.image != image {
			return false, &errCatalogImageChanged{by: changedBy, image: s.image}
		}
		if s.servesTarget(image, target) {
			return true, nil
		}
		if msg := s.describe(target); msg != last {
			last = msg
			*logs = append(*logs, "  "+msg)
			emit(UpdateStepEvent{Step: "wait_catalog_ready", Status: "running", Message: "CatalogSource: " + msg})
		}
		if err := pull.check(c, CatalogName, image); err != nil {
			return false, err
		}
	}
	return false, nil
}
