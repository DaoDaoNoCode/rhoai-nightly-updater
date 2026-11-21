package argo

import (
	"fmt"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
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
const TrackingAnnotation = "argocd.argoproj.io/tracking-id"

// TrackingApplication returns the Argo CD application that the tracking
// annotation among annotations names for the object of this group, kind,
// namespace and name, or "" when none does. The annotation alone does not
// show that the application manages the object: argocd.go asks Argo CD, and
// every refusal reads that verdict (argoTracking.managedBy). The tracking id
// must name the object itself, as Argo CD checks it (controller/state.go,
// isSelfReferencedObj: namespace, name, group and kind): Argo CD ignores an
// annotation copied from another object, so the tool does too. namespace ""
// skips the namespace check: for a cluster-scoped object Argo CD writes its
// destination namespace there.
func TrackingApplication(annotations map[string]string, group, kind, namespace, name string) string {
	app, rest, ok := strings.Cut(annotations[TrackingAnnotation], ":")
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

// LastAppliedAnnotation is where client-side apply, which Argo CD uses by
// default, keeps what it applied; it diffs against it.
const LastAppliedAnnotation = "kubectl.kubernetes.io/last-applied-configuration"

// KeepTracking copies, when previous carries Argo CD's tracking (its
// tracking annotation, or the instance label that label tracking writes),
// its annotations (Argo CD's tracking id, installation id, sync options...,
// the last-applied annotation, and any other Git sets) and its labels from
// previous to obj, without overwriting what obj already has. A Subscription
// the tool recreates then stays part of the same application and matches
// what Argo CD applied (the chart's labels and annotations, its last apply),
// so Argo CD neither shows it as missing nor changes it again (seen on a live
// cluster without them). Finalizers and other runtime metadata are not
// copied.
func KeepTracking(previous, obj map[string]any) {
	prevMeta, _ := previous["metadata"].(map[string]any)
	prevAnn, _ := prevMeta["annotations"].(map[string]any)
	prevLabels, _ := prevMeta["labels"].(map[string]any)
	id, _ := prevAnn[TrackingAnnotation].(string)
	instance, _ := prevLabels[InstanceLabel].(string)
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

// GitOpsRefusal explains a refused action on an object Argo CD manages.
func GitOpsRefusal(object, app, action string) string {
	return fmt.Sprintf("%s is managed by Argo CD application %q, which would undo %s. Make the change in that application's Git repository instead. Nothing was changed.", object, app, action)
}

// ACMPolicyManager is the field manager the API server records for ACM's
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
const ACMPolicyManager = "config-policy-controller"

// ACMCatalogManager names that manager in messages.
const ACMCatalogManager = "an ACM policy (" + ACMPolicyManager + ")"

// ACMEnforcesSpec reports whether an ACM policy enforces the object's spec:
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
func ACMEnforcesSpec(meta kube.ObjectMeta) bool {
	return meta.OwnsField(ACMPolicyManager, "f:spec")
}

// OperatorGitOps is how Argo CD or ACM manages the operator's catalog and
// Subscription, read before an operator operation.
type OperatorGitOps struct {
	CatalogApp string // Argo CD application managing CatalogSource rhoai-catalog-dev
	// catalogACM: no Argo CD application manages the catalog, and an ACM
	// policy enforces its spec (acmEnforcesSpec). Argo CD wins when both do.
	CatalogACM      bool
	SubscriptionApp string // Argo CD application managing the rhods-operator Subscription
	// What Argo CD said about each tracking annotation (argocd.go);
	// catalogApp and subscriptionApp are their managedBy.
	CatalogArgo, SubscriptionArgo Tracking
}

// ReadOperatorGitOps reads which Argo CD applications manage the nightly
// CatalogSource and the operator Subscription (Argo CD's answer, argocd.go),
// and whether an ACM policy enforces the
// catalog (from the managedFields of the same GET). A missing object is not
// managed.
func ReadOperatorGitOps(c *kube.Client) (OperatorGitOps, error) {
	var g OperatorGitOps
	argoCD := NewCheck(c)
	for _, r := range []struct {
		path      string
		obj       Object
		tracking  *Tracking
		app       *string
		isCatalog bool
	}{
		{rhoai.NamespacedPath("operators.coreos.com/v1alpha1", "catalogsources", rhoai.CatalogNS, rhoai.CatalogName), CatalogObject, &g.CatalogArgo, &g.CatalogApp, true},
		{rhoai.SubscriptionPath(), SubscriptionObject, &g.SubscriptionArgo, &g.SubscriptionApp, false},
	} {
		meta, found, err := kube.ReadObjectMeta(c, r.path)
		if err != nil {
			return OperatorGitOps{}, err
		}
		if found {
			*r.tracking = argoCD.Object(meta.Annotations, meta.Labels, r.obj)
			*r.app = r.tracking.ManagedBy()
			if r.isCatalog {
				g.CatalogACM = g.CatalogApp == "" && ACMEnforcesSpec(meta)
			}
		}
	}
	return g, nil
}

func (g OperatorGitOps) Active() bool {
	return g.CatalogManaged() || g.SubscriptionApp != ""
}

// CatalogManaged reports whether Argo CD or an ACM policy sets the nightly
// catalog, so the catalog's image is not the updater's to change.
func (g OperatorGitOps) CatalogManaged() bool {
	return g.CatalogApp != "" || g.CatalogACM
}

// CatalogManager names who manages the nightly catalog, for messages:
// `Argo CD application "x"`, "an ACM policy (config-policy-controller)",
// or "" when neither does.
func (g OperatorGitOps) CatalogManager() string {
	switch {
	case g.CatalogApp != "":
		return fmt.Sprintf("Argo CD application %q", g.CatalogApp)
	case g.CatalogACM:
		return ACMCatalogManager
	}
	return ""
}

// CatalogSpecFrom names where the catalog's spec comes from, for "left as
// ... sets it": the ACM policy, else Git.
func (g OperatorGitOps) CatalogSpecFrom() string {
	if g.CatalogApp == "" && g.CatalogACM {
		return "the ACM policy"
	}
	return "Git"
}

// CatalogChanger says who most likely changed the catalog image while
// Update waited (operator.CatalogImageChangedError).
func (g OperatorGitOps) CatalogChanger() string {
	switch {
	case g.CatalogApp != "":
		return fmt.Sprintf("Argo CD application %q syncing a change from Git", g.CatalogApp)
	case g.CatalogACM:
		return ACMCatalogManager + " enforcing a change"
	case g.SubscriptionApp != "":
		return fmt.Sprintf("Argo CD application %q syncing a change from Git", g.SubscriptionApp)
	}
	return "another client"
}

// Refusal explains why an operation is refused while Argo CD manages the
// catalog or Subscription, or an ACM policy the catalog; "" when neither
// does. deletes says what the operation deletes, and for how long.
func (g OperatorGitOps) Refusal(operation, deletes string) string {
	object, app, note := "The operator Subscription", g.SubscriptionApp, g.SubscriptionArgo.UnconfirmedNote()
	if app == "" {
		object, app, note = "CatalogSource "+rhoai.CatalogName, g.CatalogApp, g.CatalogArgo.UnconfirmedNote()
	}
	if app == "" && g.CatalogACM {
		return fmt.Sprintf("%s is managed by %s, which puts back what differs from the policy. %s deletes %s, so %s is not available here. Nothing was changed.",
			object, ACMCatalogManager, operation, deletes, operation)
	}
	if app == "" {
		return ""
	}
	return fmt.Sprintf("%s is managed by Argo CD application %q. %s deletes %s, and Argo CD would recreate it from Git in the meantime, so %s is not available here. Nothing was changed.%s",
		object, app, operation, deletes, operation, note)
}
