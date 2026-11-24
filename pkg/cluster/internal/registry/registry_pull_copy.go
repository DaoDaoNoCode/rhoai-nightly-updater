package registry

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// On ROSA HCP, the internal registry needs its own quay.io/rhoai credential.
//
// Workbench images for RHOAI appear as ImageStream tags in the applications
// namespace; each pull goes via the cluster image registry
// (image-registry.openshift-image-registry.svc:5000/<namespace>/<stream>:<tag>,
// matching what the dashboard records). Pull-through honors the
// ImageDigestMirrorSet (registry.redhat.io/rhoai → quay.io/rhoai) and
// authenticates using the global pull secret together with
// kubernetes.io/dockerconfigjson Secrets in the ImageStream namespace. With
// a hosted control plane, the updater stores its token in
// kube-system/additional-pull-secret—HyperShift mounts that on workers but
// not on the registry—so quay.io/rhoai was unreachable and nightly
// workbenches sat in ImagePullBackOff (F3, 2026-11-24: mirroring the entry
// into the applications namespace in a Secret fixed pulls). On self-managed
// clusters, workers and the registry both read openshift-config/pull-secret;
// no duplication is required.
//
// We materialize Secret RegistryPullCopyName in the DSCInitialization-named
// applications namespace, tag it with this tool (kube.ManagedByLabelKey), and
// store solely the quay.io/rhoai stanza. Secret reads there are limited to
// cluster-admins and OpenShift service accounts. Writes use only the signed-in
// user's token; an existing Secret with the same name but without our label
// stays untouched (fail closed).
const RegistryPullCopyName = "rhoai-nightly-updater-pull"

// On clusters without a hosted control plane, EnsureRegistryPullCopy has
// nothing to do and RegistryPullCopyNotHosted conveys that.
const registryCopyPrefix = "Workbench images: "

// On hosted control planes, EnsureRegistryPullCopy persists auth—the base64
// quay.io/rhoai user:password—into the registry copy: create if absent,
// refresh when the labelled Secret already exists but differs, skip when
// content already matches. Callers get log lines plus an optional warning
// (empty when fine); logs and warnings never embed the secret. Elsewhere
// (self-managed or Red Hat–managed): silent no-op. When control-plane type
// cannot be determined: no write, warning explains why.
func EnsureRegistryPullCopy(c *kube.Client, auth string) (logs []string, warning string) {
	switch rhoai.CachedControlPlane(c) {
	case rhoai.ControlPlaneHosted:
	case "":
		return nil, registryCopyPrefix + "the updater could not read whether this cluster has a hosted control plane, so it did not copy the token for the internal registry."
	default:
		return nil, ""
	}
	if auth == "" {
		return nil, registryCopyPrefix + "the pull secret has no quay.io/rhoai credential to copy for the internal registry."
	}
	ns, err := rhoai.AppsNamespace(c)
	if errors.Is(err, rhoai.ErrNoDSCInitialization) {
		return []string{registryCopyPrefix + "RHOAI is not installed yet; the updater copies the token for the internal registry once it is (Update, the DataScienceCluster or a workbench setup)."}, ""
	}
	if err != nil {
		return nil, registryCopyPrefix + "could not find RHOAI's applications namespace, so the token was not copied for the internal registry: " + redactAuth(err.Error(), auth)
	}
	return writeRegistryPullCopy(c, ns, auth)
}

// EnsureRegistryPullCopyFromPullSecret loads quay.io/rhoai from
// kube-system/additional-pull-secret via c, then delegates to
// EnsureRegistryPullCopy—used when installing RHOAI, creating a
// DataScienceCluster, or configuring a workbench. Non-hosted clusters and
// missing pull secrets produce no messages; upstream flows validate the secret.
func EnsureRegistryPullCopyFromPullSecret(c *kube.Client) []string {
	if rhoai.CachedControlPlane(c) != rhoai.ControlPlaneHosted {
		return nil
	}
	body, exists, err := c.GetOptional(rhoai.NamespacedPath("v1", "secrets", "kube-system", "additional-pull-secret"))
	if err != nil {
		return []string{"Warning: " + registryCopyPrefix + "could not read kube-system/additional-pull-secret to copy its token for the internal registry: " + err.Error()}
	}
	if !exists {
		return nil
	}
	cfg, problem := DecodeDockerConfigSecret(body)
	if problem != "" {
		return nil
	}
	auths, _ := cfg["auths"].(map[string]any)
	auth := SelectQuayAuth(auths)
	if auth == "" {
		return nil
	}
	logs, warning := EnsureRegistryPullCopy(c, auth)
	if warning != "" {
		logs = append(logs, "Warning: "+warning)
	}
	return logs
}

func writeRegistryPullCopy(c *kube.Client, ns, auth string) (logs []string, warning string) {
	where := ns + "/" + RegistryPullCopyName
	path := rhoai.NamespacedPath("v1", "secrets", ns, RegistryPullCopyName)
	cfg, err := json.Marshal(map[string]any{"auths": map[string]any{QuayRHOAI: map[string]any{"auth": auth}}})
	if err != nil {
		return nil, registryCopyPrefix + "could not build the internal registry's copy: " + err.Error()
	}
	cfgB64 := base64.StdEncoding.EncodeToString(cfg)
	redact := func(err error) string { return redactAuth(err.Error(), auth, cfgB64, string(cfg)) }

	body, found, err := c.GetOptional(path)
	if err != nil {
		return nil, fmt.Sprintf("%scould not read Secret %s: %s. The token was not copied for the internal registry.", registryCopyPrefix, where, redact(err))
	}
	secret := map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"name":      RegistryPullCopyName,
			"namespace": ns,
			"labels":    map[string]any{kube.ManagedByLabelKey: kube.ManagedByLabelValue},
		},
		"type": "kubernetes.io/dockerconfigjson",
		"data": map[string]any{".dockerconfigjson": cfgB64},
	}
	if !found {
		data, err := json.Marshal(secret)
		if err != nil {
			return nil, registryCopyPrefix + "could not build the internal registry's copy: " + err.Error()
		}
		if _, _, err := c.Post(rhoai.NamespacedPath("v1", "secrets", ns, ""), data); err != nil {
			return nil, fmt.Sprintf("%scould not create Secret %s: %s. The internal registry cannot pull nightly workbench images until it exists.", registryCopyPrefix, where, redact(err))
		}
		return []string{fmt.Sprintf("OK: %scopied the quay.io/rhoai token to Secret %s, so the internal registry can pull nightly workbench images", registryCopyPrefix, where)}, ""
	}

	var existing struct {
		Metadata kube.ObjectMeta `json:"metadata"`
		Type     string          `json:"type"`
	}
	if err := json.Unmarshal(body, &existing); err != nil {
		return nil, fmt.Sprintf("%scould not parse Secret %s, so it was not changed.", registryCopyPrefix, where)
	}
	if !existing.Metadata.HasToolLabel() {
		return nil, fmt.Sprintf("%sSecret %s exists and was not created by this updater (it lacks the label %s=%s), so it was not changed. If it should hold the updater's token, delete it (oc delete secret %s -n %s) and save the pull secret again.",
			registryCopyPrefix, where, kube.ManagedByLabelKey, kube.ManagedByLabelValue, RegistryPullCopyName, ns)
	}
	if existing.Metadata.Terminating() {
		return nil, fmt.Sprintf("%sSecret %s is being deleted, so it was not updated; save the pull secret again once it is gone.", registryCopyPrefix, where)
	}
	if existing.Type != "kubernetes.io/dockerconfigjson" {
		return nil, fmt.Sprintf("%sSecret %s has type %s, not kubernetes.io/dockerconfigjson, so it was not changed. Delete it (oc delete secret %s -n %s) and save the pull secret again.", registryCopyPrefix, where, existing.Type, RegistryPullCopyName, ns)
	}
	if holdsOnly(body, auth) {
		return []string{fmt.Sprintf("OK: %sSecret %s already holds the token, so the internal registry can pull nightly workbench images", registryCopyPrefix, where)}, ""
	}
	// Overwrite only our Secret while preserving unrelated labels and
	// annotations; stamping resourceVersion forces concurrent edits to
	// conflict rather than clobber each other.
	meta := secret["metadata"].(map[string]any)
	labels := map[string]any{}
	for k, v := range existing.Metadata.Labels {
		labels[k] = v
	}
	labels[kube.ManagedByLabelKey] = kube.ManagedByLabelValue
	meta["labels"] = labels
	if len(existing.Metadata.Annotations) > 0 {
		meta["annotations"] = existing.Metadata.Annotations
	}
	meta["resourceVersion"] = existing.Metadata.ResourceVersion
	data, err := json.Marshal(secret)
	if err != nil {
		return nil, registryCopyPrefix + "could not build the internal registry's copy: " + err.Error()
	}
	if _, _, err := c.Put(path, data); err != nil {
		return nil, fmt.Sprintf("%scould not update Secret %s: %s. The internal registry may still use the previous token.", registryCopyPrefix, where, redact(err))
	}
	return []string{fmt.Sprintf("OK: %supdated Secret %s with the new token for the internal registry", registryCopyPrefix, where)}, ""
}

// holdsOnly reports whether the Secret docker config contains a single auth
// entry for quay.io/rhoai, regardless of registry-key spelling.
func holdsOnly(body []byte, auth string) bool {
	cfg, problem := DecodeDockerConfigSecret(body)
	if problem != "" {
		return false
	}
	auths, _ := cfg["auths"].(map[string]any)
	if len(auths) != 1 {
		return false
	}
	for key, entry := range auths {
		if NormalizeKey(key) != QuayRHOAI || AuthFromEntry(entry) != auth {
			return false
		}
	}
	return true
}

// redactAuth strips auth, its base64-decoded form, the password portion, and
// any extra strings from msg before returning it (API errors sometimes echo
// request bodies).
func redactAuth(msg, auth string, more ...string) string {
	values := append([]string{auth}, more...)
	if decoded, err := base64.StdEncoding.DecodeString(auth); err == nil {
		values = append(values, string(decoded))
		if _, pass, ok := strings.Cut(string(decoded), ":"); ok {
			values = append(values, pass)
		}
	}
	return kube.RedactSecrets(msg, values...)
}

// ReadRegistryPullCopy inspects registry-copy state using u—the cluster-admin
// user client, because the updater ServiceAccount lacks Secret list/get in the
// applications namespace. Pass quayAuth from the live pull secret to learn
// whether the copy matches. The result exposes flags and namespace only, not
// credential material.
func ReadRegistryPullCopy(u *kube.Client, quayAuth string) *types.RegistryPullCopyInfo {
	if u == nil || !u.ActsAsUser() {
		return nil // never with the ServiceAccount's token
	}
	ns, err := rhoai.AppsNamespace(u)
	if errors.Is(err, rhoai.ErrNoDSCInitialization) {
		return &types.RegistryPullCopyInfo{Name: RegistryPullCopyName, NoApplicationsNamespace: true}
	}
	if err != nil {
		return &types.RegistryPullCopyInfo{Name: RegistryPullCopyName, Problem: "Could not find RHOAI's applications namespace: " + redactAuth(err.Error(), quayAuth)}
	}
	info := &types.RegistryPullCopyInfo{Name: RegistryPullCopyName, Namespace: ns}
	body, found, err := u.GetOptional(rhoai.NamespacedPath("v1", "secrets", ns, RegistryPullCopyName))
	if err != nil {
		info.Problem = fmt.Sprintf("Could not read Secret %s/%s: %s", ns, RegistryPullCopyName, redactAuth(err.Error(), quayAuth))
		return info
	}
	if !found {
		return info
	}
	info.Exists = true
	meta, err := kube.ParseObjectMeta(body)
	if err == nil {
		info.Ours = meta.HasToolLabel()
	}
	info.Current = quayAuth != "" && holdsOnly(body, quayAuth)
	return info
}
