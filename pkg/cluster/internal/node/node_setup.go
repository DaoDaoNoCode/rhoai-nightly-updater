package node

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/registry"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The one-click setup of what the nodes need for nightly images, on a
// cluster whose control plane runs on the cluster (self-managed; the image
// mirror also on a Red Hat-managed one): the ImageDigestMirrorSet that
// redirects registry.redhat.io/rhoai to quay.io/rhoai, and the quay.io/rhoai
// entry of the cluster pull secret, openshift-config/pull-secret, which the
// nodes pull with. Both have a removal. Everything runs with the signed-in
// user's own token (pkg/api withMutationAuth); the ServiceAccount has no
// access to openshift-config/pull-secret and no write on cluster config.
//
// The objects are exactly the ones OpenShift documents for these steps:
// "Configure image registry repository mirroring" (an ImageDigestMirrorSet
// created by a cluster-admin) and "Update the global cluster pull secret"
// (a new registry login appended to openshift-config/pull-secret), both in
// the OpenShift Container Platform 4.20 docs (Disconnected environments,
// Images). docs/research/node-setup.md has the sources for every claim
// below, including the node impact.
//
// Safeguards (each fails closed, before anything is written):
//   - the control plane is read again with the user's token; a hosted
//     control plane (Infrastructure controlPlaneTopology External) refuses,
//     as does an unreadable one (a hosted cluster cannot be ruled out);
//   - Red Hat-managed classic clusters (ROSA Classic, OSD) refuse the pull
//     secret actions: their docs have no procedure for changing it; so does
//     a cluster whose managed-cluster webhooks cannot be listed (it may be
//     one of them); the mirror actions are attempted there, and RBAC or
//     admission refuses them in the dry run;
//   - the mirror is not created when an ImageDigestMirrorSet or
//     ImageContentSourcePolicy already redirects the source to
//     quay.io/rhoai (that counts as done), when a mirror object maps a
//     parent scope of the source (registry.redhat.io, a wildcard host) that
//     the nodes use for it now (a mirror for exactly the source would take
//     those pulls away from it; node_setup_mirrors.go), nor when an IDMS
//     named rhoai-quay-mirror exists that does not redirect it; only an
//     IDMS named rhoai-quay-mirror with exactly this one source and mirror
//     is removed, with UID and resourceVersion preconditions;
//   - the pull secret must be readable, hold a non-empty registry list and
//     not be kept in sync by External Secrets; only the quay.io/rhoai entry
//     changes; the write is a merge patch of .dockerconfigjson carrying the
//     resourceVersion that was read (a change made meanwhile fails with a
//     conflict);
//   - the quay.io/rhoai entry is removed, or replaced without asking, only
//     when it is the one this tool added (node_pull_record.go: its record,
//     the Secret's UID and a hash of the token); an entry someone else set
//     (a provisioner's, say) is never removed, and is replaced only when
//     the user acknowledges that its token is not kept and cannot be
//     restored; an unreadable record refuses both;
//   - the token is the one the updater already holds in
//     kube-system/additional-pull-secret, checked with Quay first; it never
//     leaves the server, and every API error is redacted of every
//     credential in both Secrets;
//   - a server-side dry run of the write goes first, in the plan and again
//     in the run, so RBAC and admission (a 403, or a webhook's denial) refuse
//     before anything changes;
//   - the run carries the version of the object the user reviewed in the
//     plan; a change since then stops it with nothing changed.

// The one-click setup actions (types.NodeSetupRequest.Action).
const (
	MirrorCreate = "mirror-create"
	MirrorRemove = "mirror-remove"
	PullAdd      = "pull-secret-add"
	PullRemove   = "pull-secret-remove"
)

// ValidAction reports whether a is one of the actions.
func ValidAction(a string) bool {
	switch a {
	case MirrorCreate, MirrorRemove, PullAdd, PullRemove:
		return true
	}
	return false
}

// NeedsVersion: the action changes an existing object, so its
// stream request must carry the version the plan was made on.
func NeedsVersion(a string) bool {
	return a != MirrorCreate
}

const (
	// MirrorName is the ImageDigestMirrorSet this setup creates, the name
	// our docs (docs/QUICKSTART.md) and rosa-dev use on self-managed
	// clusters.
	MirrorName = "rhoai-quay-mirror"
	// MirrorTarget is where nightly images are published.
	MirrorTarget = "quay.io/rhoai"

	clusterPullSecretNS   = "openshift-config"
	clusterPullSecretName = "pull-secret"
	clusterPullSecretRef  = clusterPullSecretNS + "/" + clusterPullSecretName

	// externalSecretsManagerPrefix starts the field manager External
	// Secrets writes a Secret with (external-secrets
	// pkg/controllers/externalsecret, fieldOwnerTemplate
	// "externalsecrets.external-secrets.io/%v"): its next sync would undo
	// a change.
	externalSecretsManagerPrefix = "externalsecrets.external-secrets.io/"
)

// What the nodes do when each change rolls out, with the default node
// disruption policies (machine-config-operator @4dabc4b5 pkg/apihelpers
// defaultClusterPolicies; OpenShift 4.20 Machine configuration, "applies
// the update without draining or rebooting the node: ... Changes to the
// global pull secret"; Disconnected environments: "The MCO does not drain
// the nodes for ImageDigestMirrorSet"; pkg/daemon/drain.go
// isSafeContainerRegistryConfChanges, drainRequired). The nodes of a pool
// update one at a time by default: the pool's maxUnavailable, default 1
// (openshift/api @41d93514 machineconfiguration/v1/types.go:427-439, which
// also says the drain respects PodDisruptionBudgets). The machine config
// daemon waits up to an hour for a node's drain, then fails that node's
// update (pkg/daemon/drain.go performDrain, :96-117: "failed to drain
// node ... after 1 hour", event FailedToDrain).
const (
	poolPace             = "one node at a time by default (the pool's maxUnavailable)"
	pullSecretNodeImpact = "The Machine Config Operator writes the new cluster pull secret to every node of each machine config pool, " + poolPace + ", without draining or rebooting them (OpenShift applies pull secret changes that way). oc get mcp shows UPDATED True for every pool once all nodes have it; the kubelet uses it for the next pulls."
	pullRemoveNodeImpact = pullSecretNodeImpact + " After that the nodes cannot pull from quay.io/rhoai: images already on a node stay and running pods keep running, but a new nightly, or a pod that starts on a node without its image, fails with an unauthorized pull error."
	mirrorCreateImpact   = "The Machine Config Operator adds the mirror to /etc/containers/registries.conf on every node and reloads CRI-O, without draining or rebooting them: a new digest-only mirror is one of the changes OpenShift applies without a drain. oc get mcp shows UPDATED True for every pool once all nodes have it."
	mirrorRemoveDrain    = "The Machine Config Operator removes the mirror from every node. Removing a mirror drains each node first (its pods are evicted and start again on other nodes), then reloads CRI-O and makes the node schedulable again, without a reboot, " + poolPace + "; a single-node cluster is not drained. " +
		"A PodDisruptionBudget that allows no eviction holds a node's drain: the machine config daemon waits up to an hour for it, then fails that node's update (event FailedToDrain)."
	mirrorRemoveImpact = mirrorRemoveDrain + " After that, registry.redhat.io/rhoai images are pulled from registry.redhat.io again, where nightly builds do not exist: installing or updating a nightly fails until the mirror is back. Images already on a node stay."
	// mirrorRemoveKeptImpact: another object still redirects the source to
	// quay.io/rhoai. The MCO merges the mirrors of every IDMS and ICSP with
	// the same source (runtime-utils registries.go mergedDigestMirrorSets),
	// so the merged list keeps quay.io/rhoai; the removed object only added
	// an edge quay.io/rhoai -> source that the remaining ones imply, which
	// changes the order of a sorted list only when those objects list their
	// mirrors in conflicting orders (registries.go mergedMirrors, topo.go
	// Sorted). The MCO does not update a MachineConfig whose rendered
	// content is the same (container-runtime-config
	// container_runtime_config_controller.go syncIgnitionConfig,
	// :1185-1201), so then no node is updated.
	mirrorRemoveKeptImpact = "%s also redirects registry.redhat.io/rhoai to quay.io/rhoai and stays: the Machine Config Operator merges the mirrors of every object with the same source, so the nodes keep quay.io/rhoai. Unless those objects list their mirrors in conflicting orders, /etc/containers/registries.conf stays exactly as it is, the Machine Config Operator does not change the nodes' configuration, and no node is drained or updated."
)

// SetupActivity names each action in the activity log.
var SetupActivity = map[string]string{
	MirrorCreate: "create-image-mirror",
	MirrorRemove: "remove-image-mirror",
	PullAdd:      "add-node-pull-secret",
	PullRemove:   "remove-node-pull-secret",
}

// nodeSetupWork is what one action would do: the plan's words, and the
// write that does it.
type nodeSetupWork struct {
	plan types.NodeSetupPlan
	// write sends the change; query is ?dryRun=All for the dry run.
	write func(c *kube.Client, query url.Values) error
	// verify reads the object back after the write.
	verify func(c *kube.Client) error
	// done says what was done, for the outcome and the activity log.
	done string
	// record, when set, updates the tool's own record after the write
	// (node_pull_record.go); recordFailed says what its failure means.
	record       func(c *kube.Client) error
	recordFailed string
	// credentials to redact from every message.
	credentials []string
}

func (w *nodeSetupWork) refuse(code, msg string) *nodeSetupWork {
	w.plan.Ready, w.plan.RefusalCode, w.plan.Refusal = false, code, msg
	return w
}

func (w *nodeSetupWork) nothing(state string) {
	w.plan.Ready, w.plan.NothingToDo, w.plan.State = false, true, state
}

func (w *nodeSetupWork) redact(err error) string {
	return kube.RedactSecrets(err.Error(), w.credentials...)
}

// PlanSetup works out what req's action would do, with c (the user's
// client): the reads, the decision and a server-side dry run of the write.
// It changes nothing.
func PlanSetup(c *kube.Client, req types.NodeSetupRequest) (*types.NodeSetupPlan, error) {
	w, err := prepareNodeSetup(c, req)
	if err != nil {
		return nil, err
	}
	if w.plan.Ready {
		dryRunNodeSetup(c, w)
	}
	if w.plan.Ready {
		w.plan.Pools = stuckMachineConfigPools(c)
	}
	return &w.plan, nil
}

// prepareNodeSetup does the reads and the decision of action. An error is
// for a request that cannot be answered at all; every refusal is in the
// plan.
func prepareNodeSetup(c *kube.Client, req types.NodeSetupRequest) (*nodeSetupWork, error) {
	action := req.Action
	if !ValidAction(action) {
		return nil, fmt.Errorf("unknown action %q", action)
	}
	w := &nodeSetupWork{plan: types.NodeSetupPlan{Action: action}}
	cp, refusal, code := nodeSetupPlatform(c, action)
	w.plan.ControlPlane = cp
	if refusal != "" {
		return w.refuse(code, refusal), nil
	}
	switch action {
	case MirrorCreate:
		prepareMirrorCreate(c, w)
	case MirrorRemove:
		prepareMirrorRemove(c, w)
	case PullAdd, PullRemove:
		preparePullSecret(c, w, action == PullAdd, req.ReplaceExisting)
	}
	return w, nil
}

// nodeSetupPlatform reads the control plane with the user's token and says
// why action cannot run on it ("" when it can). The platform is unknown
// ("") when the topology is not External but the managed-cluster webhooks
// cannot be listed. The pull secret actions are then refused
// ("unknown_platform"): the cluster may be ROSA Classic or OSD, where the
// cluster pull secret is not changed, and nothing on the API server would
// stop the write. The mirror actions are attempted: they are allowed on
// those clusters too, and RBAC or admission refuses them in the dry run if
// this cluster does not allow them.
//
// The hosted check must come first. ROSA HCP also has webhooks named
// *.managed.openshift.io: managed-cluster-validating-webhooks @eb1c03ee
// build/resources.go:1022-1078 packages every HypershiftEnabled webhook for
// hosted clusters, with the same names as the classic SelectorSyncSet
// (:937). Only the topology tells them apart (TestNodeSetupHostedCheckComesFirst).
func nodeSetupPlatform(c *kube.Client, action string) (cp, refusal, code string) {
	mirror := action == MirrorCreate || action == MirrorRemove
	hosted, err := rhoai.ReadTopologyHosted(c)
	if err != nil {
		return "", "The updater cannot read whether this cluster has a hosted control plane (" + err.Error() + "). On a hosted control plane this would not reach the nodes, so it is not done. Nothing was changed; use the instructions instead.", "unknown_topology"
	}
	if hosted {
		if mirror {
			return rhoai.ControlPlaneHosted, "This cluster has a hosted control plane (ROSA HCP): its image mirror is configured through OpenShift Cluster Manager with rosa create image-mirror, not with an object in the cluster. Nothing was changed.", "hosted"
		}
		return rhoai.ControlPlaneHosted, "This cluster has a hosted control plane (ROSA HCP): its nodes get kube-system/additional-pull-secret, which the Pull secret item sets. HyperShift merges it with the cluster's own pull secret for the nodes of AWS and Azure node pools that replace their nodes on an update (not in-place ones); where both have a quay.io/rhoai entry, the cluster's own wins. Nothing was changed.", "hosted"
	}
	managed, err := rhoai.ReadRedHatManaged(c)
	switch {
	case err != nil && !mirror:
		return "", "The updater cannot list the validating webhook configurations (" + err.Error() + "), so it cannot tell whether Red Hat runs this cluster (ROSA Classic or OpenShift Dedicated), where it does not change the cluster pull secret. Nothing was changed. The command under Do it yourself instead does it, if you choose to.", "unknown_platform"
	case err != nil:
		return "", "", ""
	case managed && !mirror:
		return rhoai.ControlPlaneManaged, "Red Hat runs this cluster (ROSA Classic or OpenShift Dedicated). Red Hat creates its cluster pull secret, and the documentation of these services has no procedure for changing it, so the updater does not change it. The command under Do it yourself instead does it, if you choose to. Nothing was changed.", "managed"
	case managed:
		return rhoai.ControlPlaneManaged, "", ""
	}
	return rhoai.ControlPlaneSelfManaged, "", ""
}

// mirrorObject is the ImageDigestMirrorSet the setup creates, as OpenShift
// documents it (Disconnected environments, "Configure image registry
// repository mirroring"), with the tool's label.
func mirrorObject() map[string]any {
	return map[string]any{
		"apiVersion": "config.openshift.io/v1",
		"kind":       "ImageDigestMirrorSet",
		"metadata": map[string]any{
			"name":   MirrorName,
			"labels": map[string]any{"app.kubernetes.io/managed-by": "rhoai-nightly-updater"},
		},
		"spec": map[string]any{
			"imageDigestMirrors": []any{map[string]any{"source": rhoai.IDMSSource, "mirrors": []any{MirrorTarget}}},
		},
	}
}

// idmsItem is the part of an ImageDigestMirrorSet the setup reads.
type idmsItem struct {
	Metadata struct {
		Name            string `json:"name"`
		UID             string `json:"uid"`
		ResourceVersion string `json:"resourceVersion"`
	} `json:"metadata"`
	Spec struct {
		ImageDigestMirrors []struct {
			Source             string   `json:"source"`
			Mirrors            []string `json:"mirrors"`
			MirrorSourcePolicy string   `json:"mirrorSourcePolicy"`
		} `json:"imageDigestMirrors"`
	} `json:"spec"`
}

// isSetupMirror: exactly the object mirrorObject describes (the name, the
// one source and the one mirror, the default source policy), whoever
// created it (the docs' and rosa-dev's commands make the same object
// without the label). Only such an object is removed: anything more is
// someone's own mirror configuration.
func (m idmsItem) isSetupMirror() bool {
	if m.Metadata.Name != MirrorName || len(m.Spec.ImageDigestMirrors) != 1 {
		return false
	}
	e := m.Spec.ImageDigestMirrors[0]
	return e.Source == rhoai.IDMSSource && len(e.Mirrors) == 1 && e.Mirrors[0] == MirrorTarget &&
		(e.MirrorSourcePolicy == "" || e.MirrorSourcePolicy == "AllowContactingSource")
}

func listIDMS(c *kube.Client) ([]idmsItem, error) {
	var items []idmsItem
	if err := c.ListInto(rhoai.ClusterPath("config.openshift.io/v1", "imagedigestmirrorsets", ""), &items); err != nil {
		if kube.IsDecodeError(err) {
			return nil, fmt.Errorf("parse ImageDigestMirrorSets: %w", err)
		}
		return nil, err
	}
	return items, nil
}

// prepareMirrorCreate decides Create the image mirror from every mirror
// object (node_setup_mirrors.go):
//   - an IDMS or ICSP that redirects the source to quay.io/rhoai: done
//     (one that maps it elsewhere only is not: the new IDMS adds
//     quay.io/rhoai, and the nodes merge both objects' mirrors for it);
//   - an object whose mirrors the nodes use for the source through a
//     parent scope: refused ("parent_scope"), since the new, more specific
//     entry would take those pulls away from that mirror;
//   - an IDMS named rhoai-quay-mirror that is not done: refused.
func prepareMirrorCreate(c *kube.Client, w *nodeSetupWork) {
	items, rules, what, err := listMirrorRules(c)
	if err != nil {
		w.refuse(refusalCodeFor(err), "Cannot list the "+what+": "+err.Error()+". Nothing was changed.")
		return
	}
	for _, r := range rules {
		if r.redirectsToQuay() {
			w.nothing(fmt.Sprintf("%s already redirects %s to %s, so the mirror is set up.", r.ref(), rhoai.IDMSSource, MirrorTarget))
			return
		}
	}
	if conflicts := policyConflictRules(rules); len(conflicts) > 0 {
		w.refuse("policy_conflict", policyConflictRefusal(conflicts))
		return
	}
	if parents := parentScopeRules(rules); len(parents) > 0 {
		w.refuse("parent_scope", ParentScopeRefusal(parents))
		return
	}
	for _, m := range items {
		if m.Metadata.Name == MirrorName {
			w.refuse("foreign", fmt.Sprintf("An ImageDigestMirrorSet named %s already exists and does not redirect %s to %s. It is someone else's mirror configuration, so it is not changed; look at it with oc get imagedigestmirrorset %s -o yaml.", MirrorName, rhoai.IDMSSource, MirrorTarget, MirrorName))
			return
		}
	}
	body, _ := json.Marshal(mirrorObject())
	w.write = func(c *kube.Client, query url.Values) error {
		_, _, err := c.Do(http.MethodPost, rhoai.ClusterPath("config.openshift.io/v1", "imagedigestmirrorsets", ""), "application/json", body, query)
		return err
	}
	w.plan.Ready = true
	w.plan.State = fmt.Sprintf("No ImageDigestMirrorSet or ImageContentSourcePolicy redirects %s to %s.", rhoai.IDMSSource, MirrorTarget)
	for _, r := range rules {
		// The same source mapped elsewhere: the mirrors of every IDMS and
		// ICSP with one source are merged (types_image_digest_mirror_set.go
		// :56-59; registries.go mergedDigestMirrorSets).
		if r.digest() && r.Source == rhoai.IDMSSource && r.hasRealMirror() {
			w.plan.State += fmt.Sprintf(" %s maps it to %s: OpenShift merges the mirrors of every object with the same source, so that mirror stays in use next to %s.", r.ref(), strings.Join(r.Mirrors, ", "), MirrorTarget)
		}
	}
	w.plan.Changes = []string{fmt.Sprintf("Create ImageDigestMirrorSet %s (cluster-wide): pulls of %s images by digest go to %s.", MirrorName, rhoai.IDMSSource, MirrorTarget)}
	w.plan.NodeImpact = mirrorCreateImpact
	w.plan.Undo = "Remove the image mirror (the same item offers it once it exists), or oc delete imagedigestmirrorset " + MirrorName + ". Removing it drains the nodes, " + poolPace + "."
	w.done = "Created ImageDigestMirrorSet " + MirrorName + "."
	w.verify = func(c *kube.Client) error {
		_, _, err := c.Get(rhoai.ClusterPath("config.openshift.io/v1", "imagedigestmirrorsets", MirrorName))
		return err
	}
}

// prepareMirrorRemove decides Remove the image mirror: only the IDMS this
// setup creates (isSetupMirror) is deleted. What the nodes do then
// depends on what stays (node_setup_mirrors.go): another IDMS or ICSP that
// redirects the source to quay.io/rhoai keeps registries.conf as it is;
// otherwise the remaining mirrors of the source, or of a parent scope,
// serve it, or none.
func prepareMirrorRemove(c *kube.Client, w *nodeSetupWork) {
	items, rules, what, err := listMirrorRules(c)
	if err != nil {
		w.refuse(refusalCodeFor(err), "Cannot list the "+what+": "+err.Error()+". Nothing was changed.")
		return
	}
	var ours *idmsItem
	for i, m := range items {
		if m.Metadata.Name == MirrorName {
			ours = &items[i]
		}
	}
	// What stays: every rule but those of the IDMS this setup creates.
	var remaining []MirrorRule
	var others []string
	for _, r := range rules {
		if r.Kind == KindIDMS && r.Name == MirrorName {
			continue
		}
		remaining = append(remaining, r)
		if r.redirectsToQuay() && !slices.Contains(others, r.ref()) {
			others = append(others, r.ref())
		}
	}
	sort.Strings(others)
	switch {
	case ours == nil && len(others) > 0:
		w.refuse("foreign", fmt.Sprintf("The mirror of %s is %s, which this setup did not create, so it is not deleted here. If you mean to remove it: oc delete %s", rhoai.IDMSSource, others[0], strings.ToLower(others[0])))
		return
	case ours == nil:
		w.nothing(fmt.Sprintf("No ImageDigestMirrorSet or ImageContentSourcePolicy redirects %s to %s.", rhoai.IDMSSource, MirrorTarget))
		return
	case !ours.isSetupMirror():
		w.refuse("foreign", fmt.Sprintf("ImageDigestMirrorSet %s holds more than this setup's mirror (%s to %s), so it is someone's own mirror configuration and is not deleted. Look at it with oc get imagedigestmirrorset %s -o yaml.", MirrorName, rhoai.IDMSSource, MirrorTarget, MirrorName))
		return
	}
	opts, _ := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "DeleteOptions",
		"preconditions": map[string]any{"uid": ours.Metadata.UID, "resourceVersion": ours.Metadata.ResourceVersion}})
	w.write = func(c *kube.Client, query url.Values) error {
		_, _, err := c.Do(http.MethodDelete, rhoai.ClusterPath("config.openshift.io/v1", "imagedigestmirrorsets", MirrorName), "application/json", opts, query)
		return err
	}
	w.plan.Ready = true
	w.plan.UID, w.plan.ResourceVersion = ours.Metadata.UID, ours.Metadata.ResourceVersion
	w.plan.State = fmt.Sprintf("ImageDigestMirrorSet %s redirects %s to %s.", MirrorName, rhoai.IDMSSource, MirrorTarget)
	w.plan.Changes = []string{fmt.Sprintf("Delete ImageDigestMirrorSet %s (cluster-wide).", MirrorName)}
	w.plan.NodeImpact, w.plan.Undo = mirrorRemoveImpactFor(remaining, others)
	w.done = "Deleted ImageDigestMirrorSet " + MirrorName + "."
	w.verify = func(c *kube.Client) error {
		_, found, err := c.GetOptional(rhoai.ClusterPath("config.openshift.io/v1", "imagedigestmirrorsets", MirrorName))
		if err != nil {
			return err
		}
		if found {
			return errors.New("it is still there")
		}
		return nil
	}
}

// updaterQuayToken is the quay.io/rhoai credential the updater holds in
// kube-system/additional-pull-secret (the one the Pull secret item saved
// and its Quay lookups use), padded as CRI-O needs it; "" with the reason
// when there is none.
func updaterQuayToken(c *kube.Client) (string, string, string) {
	body, found, err := c.GetOptional(rhoai.NamespacedPath("v1", "secrets", "kube-system", "additional-pull-secret"))
	if err != nil {
		return "", refusalCodeFor(err), "Cannot read the updater's token in kube-system/additional-pull-secret: " + err.Error() + ". Nothing was changed."
	}
	if !found {
		return "", "no_token", "The updater has no quay.io/rhoai token yet: save it under Pull secret first. Nothing was changed."
	}
	cfg, problem := registry.DecodeDockerConfigSecret(body)
	if problem != "" {
		return "", "no_token", "kube-system/additional-pull-secret is not usable (" + problem + "): replace the token under Pull secret first. Nothing was changed."
	}
	auths, _ := cfg["auths"].(map[string]any)
	auth := registry.SelectQuayAuth(auths)
	if auth == "" {
		return "", "no_token", "kube-system/additional-pull-secret has no quay.io/rhoai token: save it under Pull secret first. Nothing was changed."
	}
	auth = PaddedAuth(auth)
	decoded, err := base64.StdEncoding.DecodeString(auth)
	if err != nil {
		return "", "no_token", "The updater's quay.io/rhoai token is not base64: replace it under Pull secret first. Nothing was changed."
	}
	if user, pass, ok := strings.Cut(string(decoded), ":"); !ok || user == "" || pass == "" {
		return "", "no_token", "The updater's quay.io/rhoai token does not decode to user:password: replace it under Pull secret first. Nothing was changed."
	}
	return auth, "", ""
}

// PaddedAuth is auth with its base64 = padding, which CRI-O needs (auth
// as it is when it is not base64 without padding).
func PaddedAuth(auth string) string {
	if _, err := base64.StdEncoding.DecodeString(auth); err == nil {
		return auth
	}
	if decoded, err := base64.RawStdEncoding.DecodeString(auth); err == nil {
		return base64.StdEncoding.EncodeToString(decoded)
	}
	return auth
}

// clusterPullSecret is openshift-config/pull-secret as read.
type clusterPullSecret struct {
	resourceVersion string
	uid             string
	config          map[string]any
	auths           map[string]any
}

// readClusterPullSecret reads openshift-config/pull-secret and refuses
// (code, message) unless it is safe to change only one entry of it.
func readClusterPullSecret(c *kube.Client) (*clusterPullSecret, []string, string, string) {
	body, _, err := c.Get(rhoai.NamespacedPath("v1", "secrets", clusterPullSecretNS, clusterPullSecretName))
	if err != nil {
		return nil, nil, refusalCodeFor(err), "Cannot read " + clusterPullSecretRef + ": " + err.Error() + ". Nothing was changed in it."
	}
	var meta struct {
		Metadata struct {
			UID             string `json:"uid"`
			ResourceVersion string `json:"resourceVersion"`
			ManagedFields   []struct {
				Manager string `json:"manager"`
			} `json:"managedFields"`
		} `json:"metadata"`
	}
	_ = json.Unmarshal(body, &meta)
	cfg, problem := registry.DecodeDockerConfigSecret(body)
	var creds []string
	if cfg != nil {
		creds = dockerConfigCredentials(cfg)
	}
	for _, f := range meta.Metadata.ManagedFields {
		if strings.HasPrefix(f.Manager, externalSecretsManagerPrefix) {
			return nil, creds, "external_secrets", clusterPullSecretRef + " is kept in sync by External Secrets, whose next sync would undo this change, so it is not changed. Add the quay.io/rhoai login to what that ExternalSecret reads from instead (oc get externalsecrets -A)."
		}
	}
	auths, _ := cfg["auths"].(map[string]any)
	if problem != "" || len(auths) == 0 || meta.Metadata.ResourceVersion == "" {
		why := problem
		if why == "" {
			why = "no registry logins"
		}
		return nil, creds, "unreadable", clusterPullSecretRef + " has no readable list of registry logins (" + why + "). It is not changed: writing only the quay.io/rhoai entry would remove the cluster's own logins."
	}
	return &clusterPullSecret{resourceVersion: meta.Metadata.ResourceVersion, uid: meta.Metadata.UID, config: cfg, auths: auths}, creds, "", ""
}

// dockerConfigCredentials lists every credential in a Docker config, for
// redaction: each login's secret fields and their decoded forms.
func dockerConfigCredentials(cfg map[string]any) []string {
	var out []string
	auths, _ := cfg["auths"].(map[string]any)
	for _, entry := range auths {
		e, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		for _, field := range []string{"auth", "password", "identitytoken", "registrytoken"} {
			v, _ := e[field].(string)
			if v == "" {
				continue
			}
			out = append(out, v)
			if d, err := base64.StdEncoding.DecodeString(v); err == nil {
				out = append(out, string(d))
				if _, pass, ok := strings.Cut(string(d), ":"); ok {
					out = append(out, pass)
				}
			}
		}
	}
	if raw, err := json.Marshal(cfg); err == nil {
		out = append(out, string(raw), base64.StdEncoding.EncodeToString(raw))
	}
	return out
}

// rhoaiKeys are the auths keys that mean exactly quay.io/rhoai (also with
// a scheme or a trailing slash), sorted.
func rhoaiKeys(auths map[string]any) []string {
	var keys []string
	for k := range auths {
		if registry.NormalizeKey(k) == registry.QuayRHOAI {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

func preparePullSecret(c *kube.Client, w *nodeSetupWork, add, replaceExisting bool) {
	token := ""
	if add {
		var code, msg string
		token, code, msg = updaterQuayToken(c)
		if token == "" {
			w.refuse(code, msg)
			return
		}
		decoded, _ := base64.StdEncoding.DecodeString(token)
		_, pass, _ := strings.Cut(string(decoded), ":")
		w.credentials = append(w.credentials, token, string(decoded), pass)
		if err := registry.VerifyQuayCredentials(token); err != nil {
			if registry.IsCredentialRejection(err) {
				w.refuse("quay", "Quay refuses the token saved under Pull secret, so it is not given to the nodes: replace it there first. Nothing was changed.")
			} else {
				w.refuse("quay", "Quay could not be asked whether the saved token works ("+w.redact(err)+"), so it is not given to the nodes. Nothing was changed; try again.")
			}
			return
		}
	}
	ps, creds, code, msg := readClusterPullSecret(c)
	w.credentials = append(w.credentials, creds...)
	if ps == nil {
		w.refuse(code, kube.RedactSecrets(msg, w.credentials...))
		return
	}
	w.plan.ResourceVersion, w.plan.UID = ps.resourceVersion, ps.uid
	// Whose the entry is decides everything below: without the record the
	// tool cannot tell, so it changes nothing.
	rec, err := ReadPullRecord(c)
	if err != nil {
		w.refuse("unreadable", "Cannot read this tool's own record of the entry it added to "+clusterPullSecretRef+" ("+w.redact(err)+"), so it cannot tell who set the quay.io/rhoai entry there. Nothing was changed.")
		return
	}
	entries := classifyPullEntries(ps, rec)
	w.plan.EntrySetBy, w.plan.ExistingKeys = entries.setBy(), entries.keys
	var newAuths map[string]any
	if add {
		newAuths = planPullAdd(w, ps, rec, entries, token, replaceExisting)
	} else {
		newAuths = planPullRemove(w, ps, rec, entries)
	}
	if newAuths == nil {
		return
	}
	cfg := map[string]any{}
	for k, v := range ps.config {
		cfg[k] = v
	}
	cfg["auths"] = newAuths
	raw, err := json.Marshal(cfg)
	if err != nil {
		w.refuse("unreadable", "Could not prepare the new "+clusterPullSecretRef+". Nothing was changed in it.")
		return
	}
	encoded := base64.StdEncoding.EncodeToString(raw)
	w.credentials = append(w.credentials, string(raw), encoded)
	// A merge patch of one data key that carries the resourceVersion read:
	// the API server refuses it with a conflict when the Secret changed
	// since (https://kubernetes.io/docs/reference/using-api/api-concepts/#patch-and-apply).
	patch, _ := json.Marshal(map[string]any{
		"metadata": map[string]any{"resourceVersion": ps.resourceVersion},
		"data":     map[string]any{".dockerconfigjson": encoded},
	})
	w.write = func(c *kube.Client, query url.Values) error {
		_, _, err := c.Do(http.MethodPatch, rhoai.NamespacedPath("v1", "secrets", clusterPullSecretNS, clusterPullSecretName), "application/merge-patch+json", patch, query)
		return err
	}
	w.plan.Ready = true
	// The registry logins that are not quay.io/rhoai entries, and the
	// quay.io/rhoai entries the change keeps: the read-back must show both.
	others := len(ps.auths) - len(entries.keys)
	keptRHOAI := map[string]bool{}
	for k := range newAuths {
		if registry.NormalizeKey(k) == registry.QuayRHOAI && (!add || k != registry.QuayRHOAI) {
			keptRHOAI[k] = true
		}
	}
	w.verify = func(c *kube.Client) error {
		got, _, code, msg := readClusterPullSecret(c)
		if got == nil {
			return fmt.Errorf("%s (%s)", msg, code)
		}
		keys := rhoaiKeys(got.auths)
		switch {
		case add && (len(keys) != 1 || registry.AuthFromEntry(got.auths[registry.QuayRHOAI]) != token):
			return errors.New("the quay.io/rhoai entry is not the updater's token")
		case !add && got.auths[entries.own] != nil:
			return errors.New("the quay.io/rhoai entry this tool added is still there")
		case !add && len(keys) != len(keptRHOAI):
			return fmt.Errorf("it has %d quay.io/rhoai entries, not %d", len(keys), len(keptRHOAI))
		case len(got.auths)-len(keys) != others:
			return fmt.Errorf("it has %d other registry logins, not %d", len(got.auths)-len(keys), others)
		}
		return nil
	}
}

// keptLogins says that n other registry logins stay.
func keptLogins(n int) string {
	return fmt.Sprintf("the other %s %s as %s", kube.CountNoun(n, "registry login", "registry logins"), kube.Verb(n, "stays", "stay"), kube.Verb(n, "it is", "they are"))
}

// recordedWhen words when and by whom the record says the entry was added.
func recordedWhen(rec *PullRecord) string {
	if rec == nil {
		return ""
	}
	s := ""
	if rec.At != "" {
		s += " on " + rec.At
	}
	if rec.By != "" && rec.By != "unknown" {
		s += " by " + rec.By
	}
	return s
}

// planPullAdd decides Add to the nodes' pull secret: the new auths, or nil
// with the plan saying why nothing is written.
//   - no quay.io/rhoai entry: add one, and record it as the tool's;
//   - every entry has the updater's token already: nothing to do, and the
//     tool does not claim an entry it did not write;
//   - the tool's own entry, with an earlier token: update it (no
//     acknowledgement: it is the tool's), and record the new one;
//   - an entry someone else set (or one changed since the tool wrote it),
//     with another token: refused ("foreign_entry", Replaceable), unless the
//     request acknowledges replacing it (ReplaceExisting); the replacement
//     is then recorded as the tool's.
func planPullAdd(w *nodeSetupWork, ps *clusterPullSecret, rec *PullRecord, entries pullEntries, token string, replaceExisting bool) map[string]any {
	same := len(entries.keys) > 0
	for _, k := range entries.keys {
		if registry.AuthFromEntry(ps.auths[k]) != token {
			same = false
		}
	}
	keyList := strings.Join(entries.foreign, ", ")
	switch {
	case same && entries.setBy() == EntrySetByTool:
		w.nothing(clusterPullSecretRef + " already has the quay.io/rhoai entry this tool added" + recordedWhen(rec) + ", with the token saved under Pull secret, so the nodes have it (or get it as their pools update).")
		return nil
	case same:
		w.nothing(clusterPullSecretRef + " already has a quay.io/rhoai entry with the token saved under Pull secret, so the nodes have it (or get it as their pools update). This tool did not add it, so it is not counted as this tool's: Remove from the nodes' pull secret leaves it.")
		return nil
	}
	newAuths := map[string]any{}
	for k, v := range ps.auths {
		if registry.NormalizeKey(k) != registry.QuayRHOAI {
			newAuths[k] = v
		}
	}
	newAuths[registry.QuayRHOAI] = map[string]any{"auth": token}
	kept := keptLogins(len(ps.auths) - len(entries.keys))
	switch entries.setBy() {
	case EntrySetByNone:
		w.plan.State = clusterPullSecretRef + " has no quay.io/rhoai entry."
		w.plan.Changes = []string{"Secret " + clusterPullSecretRef + ": add a quay.io/rhoai entry with the token saved under Pull secret; " + kept + "."}
		w.plan.Undo = "Remove from the nodes' pull secret (the same item offers it): it removes only the entry this tool added."
		w.done = "Added the quay.io/rhoai entry to " + clusterPullSecretRef + "."
	case EntrySetByTool:
		w.plan.State = clusterPullSecretRef + " has the quay.io/rhoai entry this tool added" + recordedWhen(rec) + ", with an earlier token than the one saved under Pull secret now."
		w.plan.Changes = []string{"Secret " + clusterPullSecretRef + ": update the quay.io/rhoai entry this tool added to the token saved under Pull secret; " + kept + "."}
		w.plan.Undo = "The earlier token is not kept: to go back to it, save it under Pull secret and update the entry again. Remove from the nodes' pull secret takes the entry out."
		w.done = "Updated the quay.io/rhoai entry this tool added to " + clusterPullSecretRef + " to the token saved under Pull secret."
	default:
		why := ""
		if entries.why != "" {
			why = " (" + entries.why + ")"
		}
		if !replaceExisting {
			w.plan.Replaceable = true
			w.refuse("foreign_entry", clusterPullSecretRef+" already has a quay.io/rhoai entry ("+keyList+") that this tool did not add"+why+", with another token. It is not replaced: if that entry works, the nodes need nothing more, and Check that the nodes can pull tells. Nothing was changed. To replace it anyway, acknowledge that its token is not kept and that this tool cannot restore it.")
			return nil
		}
		w.plan.Replacing = true
		w.plan.State = clusterPullSecretRef + " has a quay.io/rhoai entry (" + keyList + ") that this tool did not add" + why + ", with another token."
		w.plan.Changes = []string{"Secret " + clusterPullSecretRef + ": replace its quay.io/rhoai entry (" + keyList + "), which this tool did not add, with one that has the token saved under Pull secret; " + kept + "."}
		w.plan.Undo = "The token it has now is not kept, and this tool cannot restore it: only whoever set it up has it (on a provisioned cluster, its provisioner). Remove from the nodes' pull secret then takes out the entry this tool added, which leaves the nodes without a quay.io/rhoai login."
		w.done = "Replaced the quay.io/rhoai entry of " + clusterPullSecretRef + ", which this tool did not add, with one that has the token saved under Pull secret."
	}
	w.plan.NodeImpact = pullSecretNodeImpact
	w.record = func(c *kube.Client) error {
		r, err := NewPullRecord(c, ps.uid, token)
		if err != nil {
			return err
		}
		return writeNodePullRecord(c, r)
	}
	w.recordFailed = "This tool could not record that it added the entry, so it will not remove it: to take it out, use the removal command under Do it yourself instead"
	return newAuths
}

// planPullRemove decides Remove from the nodes' pull secret: only the
// entry the tool added (classifyPullEntries) goes; any other quay.io/rhoai
// entry is left, and when there is no entry of the tool's at all, nothing
// is written ("not_owned"), with how to remove it by hand.
func planPullRemove(w *nodeSetupWork, ps *clusterPullSecret, rec *PullRecord, entries pullEntries) map[string]any {
	if len(entries.keys) == 0 {
		w.nothing(clusterPullSecretRef + " has no quay.io/rhoai entry.")
		return nil
	}
	if entries.own == "" {
		w.refuse("not_owned", "The quay.io/rhoai entry in "+clusterPullSecretRef+" was not added by this tool (or was changed since), so it is not removed here ("+entries.why+"). Nothing was changed. To remove it by hand, as a cluster-admin, run the removal command under Do it yourself instead: it removes whatever quay.io/rhoai entry is there, so first make sure the nodes do not need it.")
		return nil
	}
	newAuths := map[string]any{}
	for k, v := range ps.auths {
		if k != entries.own {
			newAuths[k] = v
		}
	}
	w.plan.State = clusterPullSecretRef + " has the quay.io/rhoai entry this tool added" + recordedWhen(rec) + "."
	if len(entries.foreign) > 0 {
		w.plan.State += " " + strings.Join(entries.foreign, ", ") + ", which this tool did not add, stays."
	}
	w.plan.Changes = []string{"Secret " + clusterPullSecretRef + ": remove the quay.io/rhoai entry this tool added; " + keptLogins(len(ps.auths)-1) + "."}
	w.plan.NodeImpact = pullRemoveNodeImpact
	w.plan.Undo = "Add to the nodes' pull secret again (it copies the token saved under Pull secret)."
	w.done = "Removed the quay.io/rhoai entry this tool added from " + clusterPullSecretRef + "."
	w.record = clearNodePullRecord
	w.recordFailed = "This tool could not clear its record of the entry; the record matches no entry now"
	return newAuths
}

// refusalCodeFor maps a failed read to a plan's refusal code.
func refusalCodeFor(err error) string {
	if kube.IsK8sError(err, http.StatusForbidden) {
		return "forbidden"
	}
	return "unreadable"
}

// writeNodeSetup sends the work's write, as a server-side dry run or for
// real.
func writeNodeSetup(c *kube.Client, w *nodeSetupWork, dryRun bool) error {
	var q url.Values
	if dryRun {
		q = url.Values{"dryRun": {"All"}}
	}
	return w.write(c, q)
}

// dryRunNodeSetup runs the server-side dry run and turns its refusal into
// the plan's.
func dryRunNodeSetup(c *kube.Client, w *nodeSetupWork) {
	if err := writeNodeSetup(c, w, true); err != nil {
		w.refuse(writeRefusalCode(err), writeRefusal(w, err, true))
	}
}

// writeRefusalCode: "forbidden" for a 403 (the user's RBAC, or an
// admission webhook's denial, which the API server also answers with 403)
// or a 422 that names an admission check; "conflict" for a 409;
// otherwise "failed".
func writeRefusalCode(err error) string {
	var k *kube.K8sError
	if errors.As(err, &k) {
		switch {
		case k.Status == http.StatusForbidden:
			return "forbidden"
		case k.Status == http.StatusUnprocessableEntity && strings.Contains(strings.ToLower(k.K8sMessage), "admission"):
			return "forbidden"
		case k.Status == http.StatusConflict:
			return "conflict"
		}
	}
	return "failed"
}

func writeRefusal(w *nodeSetupWork, err error, dryRun bool) string {
	what := "The cluster refused it"
	if dryRun {
		what = "The cluster refused it in a dry run"
	}
	switch writeRefusalCode(err) {
	case "forbidden":
		return what + ": " + w.redact(err) + ". Nothing was changed. Your account, or an admission policy of this cluster, does not allow it: use the instructions under Do it yourself instead, or ask the cluster's administrator."
	case "conflict":
		return "It changed since it was read (" + w.redact(err) + "). Nothing was changed; review it again."
	}
	return what + ": " + w.redact(err) + ". Nothing was changed."
}

// stuckMachineConfigPools names the machine config pools that will not
// take a change until someone acts: paused, or Degraded. Read with the
// user's token; an unreadable list names none.
func stuckMachineConfigPools(c *kube.Client) []string {
	var items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Spec struct {
			Paused bool `json:"paused"`
		} `json:"spec"`
		Status struct {
			Conditions []struct {
				Type   string `json:"type"`
				Status string `json:"status"`
			} `json:"conditions"`
		} `json:"status"`
	}
	if c.ListInto(rhoai.ClusterPath("machineconfiguration.openshift.io/v1", "machineconfigpools", ""), &items) != nil {
		return nil
	}
	var out []string
	for _, p := range items {
		degraded := false
		for _, cond := range p.Status.Conditions {
			if cond.Type == "Degraded" && cond.Status == "True" {
				degraded = true
			}
		}
		switch {
		case p.Spec.Paused:
			out = append(out, p.Metadata.Name+" (paused)")
		case degraded:
			out = append(out, p.Metadata.Name+" (degraded)")
		}
	}
	sort.Strings(out)
	return out
}

// RunSetup runs action with c (the user's client), streaming its steps:
// "check" (the reads and the decision, and that nothing changed since the
// plan), "dry_run", "apply", "verify". It records the outcome in the
// activity log, unless there was nothing to do.
func RunSetup(c *kube.Client, req types.NodeSetupRequest, emit func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
	fail := func(step, code, msg string) (*types.OperationResponse, error) {
		emit(types.UpdateStepEvent{Step: step, Status: "failed", Message: msg, ErrorCode: code})
		return &types.OperationResponse{Success: false, Message: msg, ErrorCode: code, Logs: []string{}}, nil
	}
	emit(types.UpdateStepEvent{Step: "check", Status: "running", Message: "Reading what is on the cluster"})
	w, err := prepareNodeSetup(c, req)
	if err != nil {
		return nil, err
	}
	activity := SetupActivity[req.Action]
	switch {
	case w.plan.NothingToDo:
		emit(types.UpdateStepEvent{Step: "check", Status: "success", Message: w.plan.State})
		return &types.OperationResponse{Success: true, Message: "Nothing to change: " + w.plan.State, Logs: []string{}}, nil
	case !w.plan.Ready:
		return fail("check", w.plan.RefusalCode, w.plan.Refusal)
	case NeedsVersion(req.Action) && (req.ResourceVersion != w.plan.ResourceVersion || (req.UID != "" && req.UID != w.plan.UID)):
		return fail("check", "conflict", "It changed since you reviewed it, so nothing was changed. Review it again.")
	}
	emit(types.UpdateStepEvent{Step: "check", Status: "success", Message: w.plan.State})

	emit(types.UpdateStepEvent{Step: "dry_run", Status: "running", Message: "Asking the API server to check the change (dry run)"})
	if err := writeNodeSetup(c, w, true); err != nil {
		return fail("dry_run", writeRefusalCode(err), writeRefusal(w, err, true))
	}
	emit(types.UpdateStepEvent{Step: "dry_run", Status: "success", Message: "The API server accepts it"})

	emit(types.UpdateStepEvent{Step: "apply", Status: "running", Message: strings.Join(w.plan.Changes, " ")})
	if err := writeNodeSetup(c, w, false); err != nil {
		msg := writeRefusal(w, err, false)
		record.RecordAction(c, activity, msg, false)
		return fail("apply", writeRefusalCode(err), msg)
	}
	done := w.done
	// The tool's own record follows the write at once (with the
	// ServiceAccount): the change is made, so it is the tool's from now on.
	if w.record != nil {
		if err := w.record(c); err != nil {
			done += " " + w.recordFailed + " (" + w.redact(err) + ")."
		}
	}
	emit(types.UpdateStepEvent{Step: "apply", Status: "success", Message: done})
	record.RecordAction(c, activity, done, true)

	emit(types.UpdateStepEvent{Step: "verify", Status: "running", Message: "Reading it back"})
	if err := w.verify(c); err != nil {
		msg := done + " Reading it back did not show the change: " + w.redact(err) + ". Check the Status page."
		emit(types.UpdateStepEvent{Step: "verify", Status: "failed", Message: msg})
		return &types.OperationResponse{Success: false, Message: msg, ErrorCode: "verify", Logs: []string{}}, nil
	}
	note := "The nodes take it " + poolPace + ": oc get mcp shows UPDATED True for every pool once all have it."
	if pools := stuckMachineConfigPools(c); len(pools) > 0 {
		note = "These machine config pools will not take it until that is resolved: " + strings.Join(pools, ", ") + ". The others take it " + poolPace + " (oc get mcp)."
	}
	emit(types.UpdateStepEvent{Step: "verify", Status: "success", Message: note})
	return &types.OperationResponse{Success: true, Message: done + " " + note, Logs: []string{}}, nil
}
