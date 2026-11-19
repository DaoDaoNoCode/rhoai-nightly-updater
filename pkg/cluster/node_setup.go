package cluster

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

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
//     secret actions: their docs have no procedure for changing it;
//   - the mirror is not created when any ImageDigestMirrorSet already
//     redirects the source (that counts as done), nor when one named
//     rhoai-quay-mirror exists that does not; only an IDMS named
//     rhoai-quay-mirror with exactly this one source and mirror is removed,
//     with UID and resourceVersion preconditions;
//   - the pull secret must be readable, hold a non-empty registry list and
//     not be kept in sync by External Secrets; only the quay.io/rhoai entry
//     changes; the write is a merge patch of .dockerconfigjson carrying the
//     resourceVersion that was read (a change made meanwhile fails with a
//     conflict);
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
	NodeSetupMirrorCreate = "mirror-create"
	NodeSetupMirrorRemove = "mirror-remove"
	NodeSetupPullAdd      = "pull-secret-add"
	NodeSetupPullRemove   = "pull-secret-remove"
)

// ValidNodeSetupAction reports whether a is one of the actions.
func ValidNodeSetupAction(a string) bool {
	switch a {
	case NodeSetupMirrorCreate, NodeSetupMirrorRemove, NodeSetupPullAdd, NodeSetupPullRemove:
		return true
	}
	return false
}

// NodeSetupNeedsVersion: the action changes an existing object, so its
// stream request must carry the version the plan was made on.
func NodeSetupNeedsVersion(a string) bool {
	return a != NodeSetupMirrorCreate
}

const (
	// MirrorName is the ImageDigestMirrorSet this setup creates, the name
	// the docs and rosa-dev use on self-managed clusters.
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
// disruption policies (machine-config-operator pkg/apihelpers
// defaultClusterPolicies; OpenShift 4.20 Machine configuration, "applies
// the update without draining or rebooting the node: ... Changes to the
// global pull secret"; Disconnected environments: "The MCO does not drain
// the nodes for ImageDigestMirrorSet"; pkg/daemon/drain.go
// isSafeContainerRegistryConfChanges, drainRequired).
const (
	pullSecretNodeImpact = "The Machine Config Operator writes the new cluster pull secret to every node, one node at a time in each machine config pool, without draining or rebooting them (OpenShift applies pull secret changes that way). oc get mcp shows UPDATED True for every pool once all nodes have it; the kubelet uses it for the next pulls."
	pullRemoveNodeImpact = pullSecretNodeImpact + " After that the nodes cannot pull from quay.io/rhoai: images already on a node stay and running pods keep running, but a new nightly, or a pod that starts on a node without its image, fails with an unauthorized pull error."
	mirrorCreateImpact   = "The Machine Config Operator adds the mirror to /etc/containers/registries.conf on every node and reloads CRI-O, without draining or rebooting them: a new digest-only mirror is one of the changes OpenShift applies without a drain. oc get mcp shows UPDATED True for every pool once all nodes have it."
	mirrorRemoveImpact   = "The Machine Config Operator removes the mirror from every node. Removing a mirror drains each node first (its pods are evicted and start again on other nodes), then reloads CRI-O and makes the node schedulable again, without a reboot, one node at a time in each pool; a single-node cluster is not drained. After that, registry.redhat.io/rhoai images are pulled from registry.redhat.io again, where nightly builds do not exist: installing or updating a nightly fails until the mirror is back. Images already on a node stay."
)

// nodeSetupActivity names each action in the activity log.
var nodeSetupActivity = map[string]string{
	NodeSetupMirrorCreate: "create-image-mirror",
	NodeSetupMirrorRemove: "remove-image-mirror",
	NodeSetupPullAdd:      "add-node-pull-secret",
	NodeSetupPullRemove:   "remove-node-pull-secret",
}

// nodeSetupWork is what one action would do: the plan's words, and the
// write that does it.
type nodeSetupWork struct {
	plan types.NodeSetupPlan
	// write sends the change; query is ?dryRun=All for the dry run.
	write func(c *Client, query url.Values) error
	// verify reads the object back after the write.
	verify func(c *Client) error
	// done says what was done, for the outcome and the activity log.
	done string
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
	return redactSecrets(err.Error(), w.credentials...)
}

// PlanNodeSetup works out what action would do, with c (the user's
// client): the reads, the decision and a server-side dry run of the write.
// It changes nothing.
func PlanNodeSetup(c *Client, action string) (*types.NodeSetupPlan, error) {
	w, err := prepareNodeSetup(c, action)
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
func prepareNodeSetup(c *Client, action string) (*nodeSetupWork, error) {
	if !ValidNodeSetupAction(action) {
		return nil, fmt.Errorf("unknown action %q", action)
	}
	w := &nodeSetupWork{plan: types.NodeSetupPlan{Action: action}}
	cp, refusal, code := nodeSetupPlatform(c, action)
	w.plan.ControlPlane = cp
	if refusal != "" {
		return w.refuse(code, refusal), nil
	}
	switch action {
	case NodeSetupMirrorCreate:
		prepareMirrorCreate(c, w)
	case NodeSetupMirrorRemove:
		prepareMirrorRemove(c, w)
	case NodeSetupPullAdd, NodeSetupPullRemove:
		preparePullSecret(c, w, action == NodeSetupPullAdd)
	}
	return w, nil
}

// nodeSetupPlatform reads the control plane with the user's token and says
// why action cannot run on it ("" when it can). The platform is unknown
// ("") when the topology is not External but the managed-cluster webhooks
// cannot be listed: the action is then attempted, and RBAC or admission
// refuses it in the dry run if the cluster does not allow it.
func nodeSetupPlatform(c *Client, action string) (cp, refusal, code string) {
	mirror := action == NodeSetupMirrorCreate || action == NodeSetupMirrorRemove
	hosted, err := readTopologyHosted(c)
	if err != nil {
		return "", "The updater cannot read whether this cluster has a hosted control plane (" + err.Error() + "). On a hosted control plane this would not reach the nodes, so it is not done. Nothing was changed; use the instructions instead.", "unknown_topology"
	}
	if hosted {
		if mirror {
			return ControlPlaneHosted, "This cluster has a hosted control plane (ROSA HCP): its image mirror is configured through OpenShift Cluster Manager with rosa create image-mirror, not with an object in the cluster. Nothing was changed.", "hosted"
		}
		return ControlPlaneHosted, "This cluster has a hosted control plane (ROSA HCP): its nodes get kube-system/additional-pull-secret, which the Pull secret item sets; HyperShift merges it into their pull secret. Nothing was changed.", "hosted"
	}
	managed, err := readRedHatManaged(c)
	switch {
	case err != nil:
		return "", "", ""
	case managed && !mirror:
		return ControlPlaneManaged, "Red Hat runs this cluster (ROSA Classic or OpenShift Dedicated). Red Hat creates its cluster pull secret, and the documentation of these services has no procedure for changing it, so the updater does not change it. The command under Do it yourself instead does it, if you choose to. Nothing was changed.", "managed"
	case managed:
		return ControlPlaneManaged, "", ""
	}
	return ControlPlaneSelfManaged, "", ""
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
			"imageDigestMirrors": []any{map[string]any{"source": IDMSSource, "mirrors": []any{MirrorTarget}}},
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

func (m idmsItem) redirectsSource() bool {
	for _, e := range m.Spec.ImageDigestMirrors {
		if e.Source == IDMSSource {
			return true
		}
	}
	return false
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
	return e.Source == IDMSSource && len(e.Mirrors) == 1 && e.Mirrors[0] == MirrorTarget &&
		(e.MirrorSourcePolicy == "" || e.MirrorSourcePolicy == "AllowContactingSource")
}

func listIDMS(c *Client) ([]idmsItem, error) {
	body, _, err := c.get(clusterPath("config.openshift.io/v1", "imagedigestmirrorsets", ""))
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []idmsItem `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("parse ImageDigestMirrorSets: %w", err)
	}
	return list.Items, nil
}

func prepareMirrorCreate(c *Client, w *nodeSetupWork) {
	items, err := listIDMS(c)
	if err != nil {
		w.refuse(refusalCodeFor(err), "Cannot list the ImageDigestMirrorSets: "+err.Error()+". Nothing was changed.")
		return
	}
	for _, m := range items {
		if m.redirectsSource() {
			w.nothing(fmt.Sprintf("ImageDigestMirrorSet %s already redirects %s, so the mirror is set up.", m.Metadata.Name, IDMSSource))
			return
		}
	}
	for _, m := range items {
		if m.Metadata.Name == MirrorName {
			w.refuse("foreign", fmt.Sprintf("An ImageDigestMirrorSet named %s already exists and does not redirect %s. It is someone else's mirror configuration, so it is not changed; look at it with oc get imagedigestmirrorset %s -o yaml.", MirrorName, IDMSSource, MirrorName))
			return
		}
	}
	body, _ := json.Marshal(mirrorObject())
	w.write = func(c *Client, query url.Values) error {
		_, _, err := c.do(http.MethodPost, clusterPath("config.openshift.io/v1", "imagedigestmirrorsets", ""), "application/json", body, query)
		return err
	}
	w.plan.Ready = true
	w.plan.State = fmt.Sprintf("No ImageDigestMirrorSet redirects %s.", IDMSSource)
	w.plan.Changes = []string{fmt.Sprintf("Create ImageDigestMirrorSet %s (cluster-wide): pulls of %s images by digest go to %s.", MirrorName, IDMSSource, MirrorTarget)}
	w.plan.NodeImpact = mirrorCreateImpact
	w.plan.Undo = "Remove the image mirror (the same item offers it once it exists), or oc delete imagedigestmirrorset " + MirrorName + ". Removing it drains the nodes one at a time."
	w.done = "Created ImageDigestMirrorSet " + MirrorName + "."
	w.verify = func(c *Client) error {
		_, _, err := c.get(clusterPath("config.openshift.io/v1", "imagedigestmirrorsets", MirrorName))
		return err
	}
}

func prepareMirrorRemove(c *Client, w *nodeSetupWork) {
	items, err := listIDMS(c)
	if err != nil {
		w.refuse(refusalCodeFor(err), "Cannot list the ImageDigestMirrorSets: "+err.Error()+". Nothing was changed.")
		return
	}
	var ours *idmsItem
	var others []string
	for i, m := range items {
		switch {
		case m.Metadata.Name == MirrorName:
			ours = &items[i]
		case m.redirectsSource():
			others = append(others, m.Metadata.Name)
		}
	}
	sort.Strings(others)
	switch {
	case ours == nil && len(others) > 0:
		w.refuse("foreign", fmt.Sprintf("The mirror of %s is ImageDigestMirrorSet %s, which this setup did not create, so it is not deleted here. If you mean to remove it: oc delete imagedigestmirrorset %s", IDMSSource, others[0], others[0]))
		return
	case ours == nil:
		w.nothing(fmt.Sprintf("No ImageDigestMirrorSet redirects %s.", IDMSSource))
		return
	case !ours.isSetupMirror():
		w.refuse("foreign", fmt.Sprintf("ImageDigestMirrorSet %s holds more than this setup's mirror (%s to %s), so it is someone's own mirror configuration and is not deleted. Look at it with oc get imagedigestmirrorset %s -o yaml.", MirrorName, IDMSSource, MirrorTarget, MirrorName))
		return
	}
	opts, _ := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "DeleteOptions",
		"preconditions": map[string]any{"uid": ours.Metadata.UID, "resourceVersion": ours.Metadata.ResourceVersion}})
	w.write = func(c *Client, query url.Values) error {
		_, _, err := c.do(http.MethodDelete, clusterPath("config.openshift.io/v1", "imagedigestmirrorsets", MirrorName), "application/json", opts, query)
		return err
	}
	w.plan.Ready = true
	w.plan.UID, w.plan.ResourceVersion = ours.Metadata.UID, ours.Metadata.ResourceVersion
	w.plan.State = fmt.Sprintf("ImageDigestMirrorSet %s redirects %s to %s.", MirrorName, IDMSSource, MirrorTarget)
	if len(others) > 0 {
		w.plan.State += fmt.Sprintf(" %s also redirects it and stays, so the nodes keep a mirror for it.", strings.Join(others, ", "))
	}
	w.plan.Changes = []string{fmt.Sprintf("Delete ImageDigestMirrorSet %s (cluster-wide).", MirrorName)}
	w.plan.NodeImpact = mirrorRemoveImpact
	w.plan.Undo = "Create the image mirror again (one click, no drain)."
	w.done = "Deleted ImageDigestMirrorSet " + MirrorName + "."
	w.verify = func(c *Client) error {
		_, _, err := c.get(clusterPath("config.openshift.io/v1", "imagedigestmirrorsets", MirrorName))
		if IsK8sError(err, http.StatusNotFound) {
			return nil
		}
		if err == nil {
			return errors.New("it is still there")
		}
		return err
	}
}

// updaterQuayToken is the quay.io/rhoai credential the updater holds in
// kube-system/additional-pull-secret (the one the Pull secret item saved
// and its Quay lookups use), padded as CRI-O needs it; "" with the reason
// when there is none.
func updaterQuayToken(c *Client) (string, string, string) {
	body, _, err := c.get(namespacedPath("v1", "secrets", "kube-system", "additional-pull-secret"))
	if err != nil {
		if IsK8sError(err, http.StatusNotFound) {
			return "", "no_token", "The updater has no quay.io/rhoai token yet: save it under Pull secret first. Nothing was changed."
		}
		return "", refusalCodeFor(err), "Cannot read the updater's token in kube-system/additional-pull-secret: " + err.Error() + ". Nothing was changed."
	}
	cfg, problem := decodeDockerConfigSecret(body)
	if problem != "" {
		return "", "no_token", "kube-system/additional-pull-secret is not usable (" + problem + "): replace the token under Pull secret first. Nothing was changed."
	}
	auths, _ := cfg["auths"].(map[string]any)
	auth := selectQuayAuth(auths)
	if auth == "" {
		return "", "no_token", "kube-system/additional-pull-secret has no quay.io/rhoai token: save it under Pull secret first. Nothing was changed."
	}
	decoded, err := base64.StdEncoding.DecodeString(auth)
	if err != nil {
		decoded, err = base64.RawStdEncoding.DecodeString(auth)
		if err != nil {
			return "", "no_token", "The updater's quay.io/rhoai token is not base64: replace it under Pull secret first. Nothing was changed."
		}
		auth = base64.StdEncoding.EncodeToString(decoded)
	}
	if user, pass, ok := strings.Cut(string(decoded), ":"); !ok || user == "" || pass == "" {
		return "", "no_token", "The updater's quay.io/rhoai token does not decode to user:password: replace it under Pull secret first. Nothing was changed."
	}
	return auth, "", ""
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
func readClusterPullSecret(c *Client) (*clusterPullSecret, []string, string, string) {
	body, _, err := c.get(namespacedPath("v1", "secrets", clusterPullSecretNS, clusterPullSecretName))
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
	cfg, problem := decodeDockerConfigSecret(body)
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
		if normalizeRegistryKey(k) == quayRHOAIRegistry {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

func preparePullSecret(c *Client, w *nodeSetupWork, add bool) {
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
		if err := verifyQuayCredentials(token); err != nil {
			if isQuayCredentialRejection(err) {
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
		w.refuse(code, redactSecrets(msg, w.credentials...))
		return
	}
	w.plan.ResourceVersion, w.plan.UID = ps.resourceVersion, ps.uid
	keys := rhoaiKeys(ps.auths)
	others := len(ps.auths) - len(keys)
	current := ""
	if len(keys) == 1 {
		current = authFromEntry(ps.auths[keys[0]])
	}
	newAuths := map[string]any{}
	for k, v := range ps.auths {
		if normalizeRegistryKey(k) != quayRHOAIRegistry {
			newAuths[k] = v
		}
	}
	kept := fmt.Sprintf("the other %s %s as %s", countNoun(others, "registry login", "registry logins"), verb(others, "stays", "stay"), verb(others, "it is", "they are"))
	if add {
		if len(keys) == 1 && keys[0] == quayRHOAIRegistry && current == token {
			w.nothing(clusterPullSecretRef + " already has the quay.io/rhoai entry with the updater's token, so the nodes have it (or get it as their pools update).")
			return
		}
		newAuths[quayRHOAIRegistry] = map[string]any{"auth": token}
		switch {
		case len(keys) == 0:
			w.plan.State = clusterPullSecretRef + " has no quay.io/rhoai entry."
			w.plan.Changes = []string{"Secret " + clusterPullSecretRef + ": add a quay.io/rhoai entry with the token saved under Pull secret; " + kept + "."}
			w.plan.Undo = "Remove from the nodes' pull secret (the same item offers it): it removes only the quay.io/rhoai entry."
		default:
			w.plan.State = clusterPullSecretRef + " has a quay.io/rhoai entry with another token."
			w.plan.Changes = []string{"Secret " + clusterPullSecretRef + ": replace the token of its quay.io/rhoai entry with the one saved under Pull secret; " + kept + "."}
			w.plan.Undo = "The token it has now is not kept. Remove from the nodes' pull secret takes the entry out; to go back to the earlier token, save it under Pull secret and add it again."
		}
		w.plan.NodeImpact = pullSecretNodeImpact
		w.done = "Added the quay.io/rhoai entry to " + clusterPullSecretRef + "."
	} else {
		if len(keys) == 0 {
			w.nothing(clusterPullSecretRef + " has no quay.io/rhoai entry.")
			return
		}
		w.plan.State = clusterPullSecretRef + " has a quay.io/rhoai entry."
		w.plan.Changes = []string{"Secret " + clusterPullSecretRef + ": remove its quay.io/rhoai entry; " + kept + "."}
		w.plan.NodeImpact = pullRemoveNodeImpact
		w.plan.Undo = "Add to the nodes' pull secret again (it copies the token saved under Pull secret)."
		w.done = "Removed the quay.io/rhoai entry from " + clusterPullSecretRef + "."
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
	w.write = func(c *Client, query url.Values) error {
		_, _, err := c.do(http.MethodPatch, namespacedPath("v1", "secrets", clusterPullSecretNS, clusterPullSecretName), "application/merge-patch+json", patch, query)
		return err
	}
	w.plan.Ready = true
	w.verify = func(c *Client) error {
		got, _, code, msg := readClusterPullSecret(c)
		if got == nil {
			return fmt.Errorf("%s (%s)", msg, code)
		}
		keys := rhoaiKeys(got.auths)
		switch {
		case add && (len(keys) != 1 || authFromEntry(got.auths[keys[0]]) != token):
			return errors.New("the quay.io/rhoai entry is not the updater's token")
		case !add && len(keys) != 0:
			return errors.New("the quay.io/rhoai entry is still there")
		case len(got.auths)-len(keys) != others:
			return fmt.Errorf("it has %d other registry logins, not %d", len(got.auths)-len(keys), others)
		}
		return nil
	}
}

// refusalCodeFor maps a failed read to a plan's refusal code.
func refusalCodeFor(err error) string {
	if IsK8sError(err, http.StatusForbidden) {
		return "forbidden"
	}
	return "unreadable"
}

// writeNodeSetup sends the work's write, as a server-side dry run or for
// real.
func writeNodeSetup(c *Client, w *nodeSetupWork, dryRun bool) error {
	var q url.Values
	if dryRun {
		q = url.Values{"dryRun": {"All"}}
	}
	return w.write(c, q)
}

// dryRunNodeSetup runs the server-side dry run and turns its refusal into
// the plan's.
func dryRunNodeSetup(c *Client, w *nodeSetupWork) {
	if err := writeNodeSetup(c, w, true); err != nil {
		w.refuse(writeRefusalCode(err), writeRefusal(w, err, true))
	}
}

// writeRefusalCode: "forbidden" for a 403 (the user's RBAC, or an
// admission webhook's denial, which the API server also answers with 403)
// or a 422 that names an admission check; "conflict" for a 409;
// otherwise "failed".
func writeRefusalCode(err error) string {
	var k *K8sError
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
func stuckMachineConfigPools(c *Client) []string {
	body, _, err := c.get(clusterPath("machineconfiguration.openshift.io/v1", "machineconfigpools", ""))
	if err != nil {
		return nil
	}
	var list struct {
		Items []struct {
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
		} `json:"items"`
	}
	if json.Unmarshal(body, &list) != nil {
		return nil
	}
	var out []string
	for _, p := range list.Items {
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

// RunNodeSetup runs action with c (the user's client), streaming its steps:
// "check" (the reads and the decision, and that nothing changed since the
// plan), "dry_run", "apply", "verify". It records the outcome in the
// activity log, unless there was nothing to do.
func RunNodeSetup(c *Client, req types.NodeSetupRequest, emit func(UpdateStepEvent)) (*types.OperationResponse, error) {
	fail := func(step, code, msg string) (*types.OperationResponse, error) {
		emit(UpdateStepEvent{Step: step, Status: "failed", Message: msg, ErrorCode: code})
		return &types.OperationResponse{Success: false, Message: msg, ErrorCode: code, Logs: []string{}}, nil
	}
	emit(UpdateStepEvent{Step: "check", Status: "running", Message: "Reading what is on the cluster"})
	w, err := prepareNodeSetup(c, req.Action)
	if err != nil {
		return nil, err
	}
	activity := nodeSetupActivity[req.Action]
	switch {
	case w.plan.NothingToDo:
		emit(UpdateStepEvent{Step: "check", Status: "success", Message: w.plan.State})
		return &types.OperationResponse{Success: true, Message: "Nothing to change: " + w.plan.State, Logs: []string{}}, nil
	case !w.plan.Ready:
		return fail("check", w.plan.RefusalCode, w.plan.Refusal)
	case NodeSetupNeedsVersion(req.Action) && (req.ResourceVersion != w.plan.ResourceVersion || (req.UID != "" && req.UID != w.plan.UID)):
		return fail("check", "conflict", "It changed since you reviewed it, so nothing was changed. Review it again.")
	}
	emit(UpdateStepEvent{Step: "check", Status: "success", Message: w.plan.State})

	emit(UpdateStepEvent{Step: "dry_run", Status: "running", Message: "Asking the API server to check the change (dry run)"})
	if err := writeNodeSetup(c, w, true); err != nil {
		return fail("dry_run", writeRefusalCode(err), writeRefusal(w, err, true))
	}
	emit(UpdateStepEvent{Step: "dry_run", Status: "success", Message: "The API server accepts it"})

	emit(UpdateStepEvent{Step: "apply", Status: "running", Message: strings.Join(w.plan.Changes, " ")})
	if err := writeNodeSetup(c, w, false); err != nil {
		msg := writeRefusal(w, err, false)
		recordActivity(c, activity, msg, false)
		return fail("apply", writeRefusalCode(err), msg)
	}
	emit(UpdateStepEvent{Step: "apply", Status: "success", Message: w.done})
	recordActivity(c, activity, w.done, true)

	emit(UpdateStepEvent{Step: "verify", Status: "running", Message: "Reading it back"})
	if err := w.verify(c); err != nil {
		msg := w.done + " Reading it back did not show the change: " + w.redact(err) + ". Check the Status page."
		emit(UpdateStepEvent{Step: "verify", Status: "failed", Message: msg})
		return &types.OperationResponse{Success: false, Message: msg, ErrorCode: "verify", Logs: []string{}}, nil
	}
	note := "The nodes take it one at a time: oc get mcp shows UPDATED True for every pool once all have it."
	if pools := stuckMachineConfigPools(c); len(pools) > 0 {
		note = "These machine config pools will not take it until that is resolved: " + strings.Join(pools, ", ") + ". The others take it one node at a time (oc get mcp)."
	}
	emit(UpdateStepEvent{Step: "verify", Status: "success", Message: note})
	return &types.OperationResponse{Success: true, Message: w.done + " " + note, Logs: []string{}}, nil
}
