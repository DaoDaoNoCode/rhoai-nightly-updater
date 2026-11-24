package registry

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// On ROSA HCP, mirror the quay.io/rhoai credential where the internal registry can use it.
//
// RHOAI workbench images live as ImageStream tags in the applications
// namespace; each workbench consumes them via the internal registry
// (image-registry.openshift-image-registry.svc:5000/<namespace>/<stream>:<tag>,
// as recorded by the dashboard). That registry pull-through honors the
// cluster ImageDigestMirrorSet (registry.redhat.io/rhoai to
// quay.io/rhoai) and signs in using the global pull secret together with
// kubernetes.io/dockerconfigjson Secrets from the ImageStream namespace. On
// hosted control planes the updater token resides in
// kube-system/additional-pull-secret, which HyperShift supplies to nodes
// alone, leaving the registry without quay.io/rhoai credentials and nightly
// workbenches stuck in ImagePullBackOff (observed F3, 2026-11-24: adding a
// Secret with the same entry in the applications namespace restored pulls). On
// self-managed clusters nodes and registry alike read
// openshift-config/pull-secret, so nothing gets duplicated.
//
// The maintained copy is Secret RegistryPullCopyName in the applications
// namespace named by DSCInitialization, marked with this tool's label
// (kube.ManagedByLabelKey), and limited to a precise quay.io/rhoai auths entry (never
// a blanket quay.io or repo-specific login). Creation and removal rely on the
// signed-in user's token alone, and a same-named Secret lacking the tool
// label is never modified (fail closed).
const RegistryPullCopyName = "rhoai-nightly-updater-pull"

// When ExcludeImageSecretAnnotation is "true" on a Secret, OpenShift omits it
// from pull secrets handed to the registry.
const ExcludeImageSecretAnnotation = "openshift.io/image.excludeImageSecret"

// All log lines about the registry copy use registryCopyPrefix as their lead-in.
const registryCopyPrefix = "Workbench images: "

// copyLater describes when a deferred copy eventually runs.
const copyLater = "the updater copies it when the DataScienceCluster or a workbench is set up, or with Copy for the internal registry under Pull secret"

// ExactQuayRHOAIAuth returns the auths credential whose key matches
// quay.io/rhoai exactly (aside from scheme or a trailing slash), or "" if absent.
// Unlike SelectQuayAuth it does not degrade to repository-scoped or
// generic quay.io logins—only the owner-approved entry is eligible for copy.
func ExactQuayRHOAIAuth(auths map[string]any) string {
	keys := make([]string, 0, len(auths))
	for k := range auths {
		if NormalizeKey(k) == QuayRHOAI {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		if auth := AuthFromEntry(auths[k]); auth != "" {
			return auth
		}
	}
	return ""
}

// EnsureRegistryPullCopy persists auth (quay.io/rhoai credential as base64
// user:password) into the registry mirror on hosted control planes: create if
// missing, refresh when the tool owns the Secret but the token differs, skip when
// auth already matches. Returned logs and warnings never embed credentials (warning is
// "" when absent). Self-managed or Red Hat managed (non-hosted): no action, no
// messages. Without a user token or when control-plane type cannot be determined:
// nothing is written and the warning explains why.
func EnsureRegistryPullCopy(c *kube.Client, auth string) (logs []string, warning, code string) {
	if !c.ActsAsUser() {
		return nil, registryCopyPrefix + "the token is copied for the internal registry only with the signed-in user's own token, so it was not copied.", "forbidden"
	}
	switch rhoai.CachedControlPlane(c) {
	case rhoai.ControlPlaneHosted:
	case "":
		return nil, registryCopyPrefix + "the updater could not read whether this cluster has a hosted control plane, so it did not copy the token for the internal registry.", "prerequisites"
	default:
		return nil, "", ""
	}
	if auth == "" {
		return []string{registryCopyPrefix + "the pull secret has no quay.io/rhoai entry, so nothing was copied for the internal registry; add the quay.io/rhoai token under Pull secret."}, "", ""
	}
	ns, err := rhoai.AppsNamespace(c)
	if errors.Is(err, rhoai.ErrNoDSCInitialization) {
		return []string{registryCopyPrefix + "RHOAI has no applications namespace yet, so the token was not copied for the internal registry yet; " + copyLater + "."}, "", ""
	}
	if err != nil {
		return nil, registryCopyPrefix + "could not find RHOAI's applications namespace, so the token was not copied for the internal registry: " + kube.RedactSecrets(err.Error(), credentialForms(auth)...), kube.ErrorCodeOr(err, "internal")
	}
	return writeRegistryPullCopy(c, ns, auth)
}

// CopyChanged: logs (of the copy) say it created or updated the Secret.
func CopyChanged(logs []string) bool {
	for _, l := range logs {
		if strings.HasPrefix(l, "OK: "+registryCopyPrefix+"copied ") || strings.HasPrefix(l, "OK: "+registryCopyPrefix+"updated ") {
			return true
		}
	}
	return false
}

// copyFromPullSecret aliases CopyFromPullSecret so tests can swap the implementation
// and observe which flows trigger copying.
var copyFromPullSecret = CopyFromPullSecret

// EnsureRegistryPullCopyFromPullSecret is what Update, Reinstall, Refresh,
// DataScienceCluster creation, and workbench setup invoke: CopyFromPullSecret,
// promoting any warning to a "Warning: " log line.
func EnsureRegistryPullCopyFromPullSecret(c *kube.Client) []string {
	logs, warning, _ := copyFromPullSecret(c)
	if warning != "" {
		logs = append(logs, "Warning: "+warning)
	}
	return logs
}

// CopyFromPullSecret delegates to EnsureRegistryPullCopy using the precise
// quay.io/rhoai slot from kube-system/additional-pull-secret via c. Non-hosted
// clusters, or when the pull secret is missing or not a readable config, stay
// silent (callers validate the secret on their own).
func CopyFromPullSecret(c *kube.Client) (logs []string, warning, code string) {
	if !c.ActsAsUser() {
		return nil, registryCopyPrefix + "the token is copied for the internal registry only with the signed-in user's own token, so it was not copied.", "forbidden"
	}
	switch rhoai.CachedControlPlane(c) {
	case rhoai.ControlPlaneHosted:
	case "":
		return nil, registryCopyPrefix + "the updater could not read whether this cluster has a hosted control plane, so it did not copy the token for the internal registry.", "prerequisites"
	default:
		return nil, "", ""
	}
	body, exists, err := c.GetOptional(rhoai.NamespacedPath("v1", "secrets", "kube-system", "additional-pull-secret"))
	if err != nil {
		return nil, registryCopyPrefix + "could not read kube-system/additional-pull-secret to copy its token for the internal registry: " + kube.RedactSecrets(err.Error(), existingCredentialForms(body)...), kube.ErrorCodeOr(err, "internal")
	}
	if !exists {
		return []string{registryCopyPrefix + "kube-system/additional-pull-secret does not exist, so nothing was copied for the internal registry; save the quay.io/rhoai token under Pull secret."}, "", ""
	}
	cfg, problem := DecodeDockerConfigSecret(body)
	if problem != "" {
		return nil, fmt.Sprintf("%skube-system/additional-pull-secret is not a readable pull secret (%s), so nothing was copied for the internal registry; replace the token under Pull secret.", registryCopyPrefix, problem), "validation"
	}
	auths, _ := cfg["auths"].(map[string]any)
	return EnsureRegistryPullCopy(c, ExactQuayRHOAIAuth(auths))
}

func writeRegistryPullCopy(c *kube.Client, ns, auth string) (logs []string, warning, code string) {
	where := ns + "/" + RegistryPullCopyName
	path := rhoai.NamespacedPath("v1", "secrets", ns, RegistryPullCopyName)
	cfg, err := json.Marshal(map[string]any{"auths": map[string]any{QuayRHOAI: map[string]any{"auth": auth}}})
	if err != nil {
		return nil, registryCopyPrefix + "could not build the internal registry's copy: " + err.Error(), "internal"
	}
	cfgB64 := base64.StdEncoding.EncodeToString(cfg)
	// Track each representation of the incoming credential and, after read,
	// those stored in the copy—admission webhooks may echo the object or its
	// prior revision in denial messages.
	secrets := append(credentialForms(auth), cfgB64, string(cfg))
	redact := func(err error) string { return kube.RedactSecrets(err.Error(), secrets...) }
	done := []string{fmt.Sprintf("OK: %sSecret %s holds the quay.io/rhoai token, so the internal registry can pull nightly workbench images", registryCopyPrefix, where)}

	body, found, err := c.GetOptional(path)
	secrets = append(secrets, existingCredentialForms(body)...)
	if err != nil {
		return nil, fmt.Sprintf("%scould not read Secret %s: %s. The token was not copied for the internal registry.", registryCopyPrefix, where, redact(err)), kube.ErrorCodeOr(err, "internal")
	}
	if !found {
		data, err := json.Marshal(map[string]any{
			"apiVersion": "v1",
			"kind":       "Secret",
			"metadata": map[string]any{
				"name":      RegistryPullCopyName,
				"namespace": ns,
				"labels":    map[string]any{kube.ManagedByLabelKey: kube.ManagedByLabelValue},
			},
			"type": "kubernetes.io/dockerconfigjson",
			"data": map[string]any{".dockerconfigjson": cfgB64},
		})
		if err != nil {
			return nil, registryCopyPrefix + "could not build the internal registry's copy: " + err.Error(), "internal"
		}
		_, _, err = c.Post(rhoai.NamespacedPath("v1", "secrets", ns, ""), data)
		switch {
		case err == nil:
			return []string{fmt.Sprintf("OK: %scopied the quay.io/rhoai token to Secret %s, so the internal registry can pull nightly workbench images", registryCopyPrefix, where)}, "", ""
		case kube.IsK8sNotFound(err):
			return []string{fmt.Sprintf("%sRHOAI's applications namespace %s does not exist yet, so the token was not copied for the internal registry yet; %s.", registryCopyPrefix, ns, copyLater)}, "", ""
		case kube.IsK8sConflict(err) && rereadHolds(c, path, auth, &secrets):
			return done, "", "" // another writer created it with this token in the meantime
		}
		return nil, fmt.Sprintf("%scould not create Secret %s: %s. The internal registry cannot pull nightly workbench images until it exists.", registryCopyPrefix, where, redact(err)), kube.ErrorCodeOr(err, "internal")
	}

	if problem, code := copyProblem(body, where, ns); problem != "" {
		return nil, registryCopyPrefix + problem, code
	}
	if holdsOnly(body, auth) {
		return []string{fmt.Sprintf("OK: %sSecret %s already holds the token, so the internal registry can pull nightly workbench images", registryCopyPrefix, where)}, "", ""
	}
	// Patch data only on the tool-owned copy: reuse the fetched object
	// (labels, annotations, finalizers, owner references) and its
	// resourceVersion so concurrent edits surface as conflicts rather than
	// silent overwrites.
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, fmt.Sprintf("%scould not parse Secret %s, so it was not changed.", registryCopyPrefix, where), "internal"
	}
	obj["data"] = map[string]any{".dockerconfigjson": cfgB64}
	delete(obj, "stringData")
	data, err := json.Marshal(obj)
	if err != nil {
		return nil, registryCopyPrefix + "could not build the internal registry's copy: " + err.Error(), "internal"
	}
	if _, _, err := c.Put(path, data); err != nil {
		if kube.IsK8sConflict(err) && rereadHolds(c, path, auth, &secrets) {
			return done, "", ""
		}
		return nil, fmt.Sprintf("%scould not update Secret %s: %s. The internal registry may still use the previous token.", registryCopyPrefix, where, redact(err)), kube.ErrorCodeOr(err, "internal")
	}
	return []string{fmt.Sprintf("OK: %supdated Secret %s with the new token for the internal registry", registryCopyPrefix, where)}, "", ""
}

// After a conflict, rereadHolds fetches the copy again and reports true when it
// is tool-owned, valid, and contains exactly auth. Any credentials found are
// appended to secrets.
func rereadHolds(c *kube.Client, path, auth string, secrets *[]string) bool {
	body, found, err := c.GetOptional(path)
	*secrets = append(*secrets, existingCredentialForms(body)...)
	if err != nil || !found {
		return false
	}
	problem, _ := copyProblem(body, "", "")
	return problem == "" && holdsOnly(body, auth)
}

// copyProblem states why body must not be altered (where is ns/name for
// messaging): wrong owner, terminating, wrong type, or excluded from registry
// pull secrets. "" means modification is allowed.
func copyProblem(body []byte, where, ns string) (problem, code string) {
	var s struct {
		Metadata kube.ObjectMeta `json:"metadata"`
		Type     string          `json:"type"`
	}
	if err := json.Unmarshal(body, &s); err != nil {
		return fmt.Sprintf("could not parse Secret %s, so it was not changed.", where), "internal"
	}
	switch {
	case !s.Metadata.HasToolLabel():
		return fmt.Sprintf("Secret %s exists and was not created by this updater (it lacks the label %s=%s), so it was not changed. If it should hold the updater's token, delete it (oc delete secret %s -n %s) and copy the token again.",
			where, kube.ManagedByLabelKey, kube.ManagedByLabelValue, RegistryPullCopyName, ns), "not_managed"
	case s.Metadata.Terminating():
		return fmt.Sprintf("Secret %s is being deleted, so it was not updated; copy the token again once it is gone.", where), "terminating"
	case s.Type != "kubernetes.io/dockerconfigjson":
		return fmt.Sprintf("Secret %s has type %s, not the type this tool writes (kubernetes.io/dockerconfigjson), so it was not changed. Delete it (oc delete secret %s -n %s) and copy the token again.", where, s.Type, RegistryPullCopyName, ns), "validation"
	case s.Metadata.Annotations[ExcludeImageSecretAnnotation] == "true":
		return fmt.Sprintf("Secret %s has the annotation %s=true, so the internal registry does not use it; it was not changed. Remove the annotation (oc annotate secret %s -n %s %s-) if the registry should use it.", where, ExcludeImageSecretAnnotation, RegistryPullCopyName, ns, ExcludeImageSecretAnnotation), "validation"
	}
	return "", ""
}

// holdsOnly reports whether the Secret config has a single auths entry for
// quay.io/rhoai (any key spelling) whose auth matches.
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

// credentialForms expands auth into auth itself, decoded user:password, and password alone.
func credentialForms(auth string) []string {
	out := []string{auth}
	if decoded, err := base64.StdEncoding.DecodeString(auth); err == nil {
		out = append(out, string(decoded))
		if _, pass, ok := strings.Cut(string(decoded), ":"); ok {
			out = append(out, pass)
		}
	}
	return out
}

// existingCredentialForms collects every redactable credential shape from a
// dockerconfigjson Secret body: encoded and JSON config plus per-entry auth
// (with decoded variants), passwords, and tokens.
func existingCredentialForms(body []byte) []string {
	var s struct {
		Data map[string]string `json:"data"`
	}
	if len(body) == 0 || json.Unmarshal(body, &s) != nil {
		return nil
	}
	b64 := s.Data[".dockerconfigjson"]
	if b64 == "" {
		return nil
	}
	out := []string{b64}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return out
	}
	out = append(out, string(raw))
	var cfg struct {
		Auths map[string]map[string]any `json:"auths"`
	}
	if json.Unmarshal(raw, &cfg) != nil {
		return out
	}
	for _, entry := range cfg.Auths {
		if a, _ := entry["auth"].(string); a != "" {
			out = append(out, credentialForms(a)...)
		}
		for _, f := range []string{"password", "identitytoken", "registrytoken"} {
			if v, _ := entry[f].(string); v != "" {
				out = append(out, v)
			}
		}
	}
	return out
}

// RemoveRegistryPullCopy removes the tool's copy using a UID precondition and the
// signed-in user's token. A same-named Secret without the tool label yields
// code "not_managed"; missing Secret or namespace means nothing to delete. Logs
// never contain credentials.
func RemoveRegistryPullCopy(c *kube.Client) (logs []string, problem, code string) {
	if !c.ActsAsUser() {
		return nil, "The internal registry's copy is removed only with the signed-in user's own token.", "forbidden"
	}
	ns, err := rhoai.AppsNamespace(c)
	if errors.Is(err, rhoai.ErrNoDSCInitialization) {
		return []string{"RHOAI has no applications namespace, so there is no copy to remove."}, "", ""
	}
	if err != nil {
		return nil, "Could not find RHOAI's applications namespace: " + err.Error(), kube.ErrorCodeOr(err, "internal")
	}
	where := ns + "/" + RegistryPullCopyName
	path := rhoai.NamespacedPath("v1", "secrets", ns, RegistryPullCopyName)
	body, found, err := c.GetOptional(path)
	secrets := existingCredentialForms(body)
	if err != nil {
		return nil, fmt.Sprintf("Could not read Secret %s: %s. Nothing was removed.", where, kube.RedactSecrets(err.Error(), secrets...)), kube.ErrorCodeOr(err, "internal")
	}
	if !found {
		return []string{fmt.Sprintf("Secret %s does not exist; nothing to remove.", where)}, "", ""
	}
	meta, err := kube.ParseObjectMeta(body)
	if err != nil || !meta.HasToolLabel() {
		return nil, fmt.Sprintf("Secret %s was not created by this updater (it lacks the label %s=%s), so it is not removed.", where, kube.ManagedByLabelKey, kube.ManagedByLabelValue), "not_managed"
	}
	// The UID and resourceVersion as checked: a Secret relabelled or made
	// again since the check is not deleted. After a conflict it is read
	// once more and removed only if it is still the tool's.
	for attempt := 0; ; attempt++ {
		_, err := kube.DeleteWithPreconditions(c, path, meta.UID, meta.ResourceVersion)
		if err == nil || kube.IsK8sNotFound(err) {
			break
		}
		if !kube.IsK8sConflict(err) {
			return nil, fmt.Sprintf("Could not remove Secret %s: %s.", where, kube.RedactSecrets(err.Error(), secrets...)), kube.ErrorCodeOr(err, "internal")
		}
		if attempt > 0 {
			return nil, fmt.Sprintf("Secret %s changed while it was being removed, so it was not removed. Try again.", where), "conflict"
		}
		body, found, err = c.GetOptional(path)
		secrets = append(secrets, existingCredentialForms(body)...)
		if err != nil {
			return nil, fmt.Sprintf("Could not read Secret %s again: %s. Nothing was removed.", where, kube.RedactSecrets(err.Error(), secrets...)), kube.ErrorCodeOr(err, "internal")
		}
		if !found {
			break
		}
		if meta, err = kube.ParseObjectMeta(body); err != nil || !meta.HasToolLabel() {
			return nil, fmt.Sprintf("Secret %s changed and is no longer this updater's (it lacks the label %s=%s), so it is not removed.", where, kube.ManagedByLabelKey, kube.ManagedByLabelValue), "not_managed"
		}
	}
	return []string{fmt.Sprintf("OK: removed Secret %s. Workbenches cannot pull nightly images through the internal registry until the token is copied again.", where)}, "", ""
}

// ReadRegistryPullCopy is the status of the registry copy, read with u (the
// signed-in cluster-admin's own client: the ServiceAccount may not read
// Secrets in the applications namespace). It compares the copy with the
// exact quay.io/rhoai entry of kube-system/additional-pull-secret, the one
// Copy writes, read with u too. Only booleans, the namespace and a reason
// are returned, never the credential.
func ReadRegistryPullCopy(u *kube.Client) *types.RegistryPullCopyInfo {
	if u == nil || !u.ActsAsUser() {
		return nil // never with the ServiceAccount's token
	}
	var quayAuth string
	var pullForms []string
	if body, exists, err := u.GetOptional(rhoai.NamespacedPath("v1", "secrets", "kube-system", "additional-pull-secret")); err == nil && exists {
		pullForms = existingCredentialForms(body)
		if cfg, problem := DecodeDockerConfigSecret(body); problem == "" {
			auths, _ := cfg["auths"].(map[string]any)
			quayAuth = ExactQuayRHOAIAuth(auths)
		}
	}
	redact := func(msg string) string { return kube.RedactSecrets(msg, pullForms...) }
	ns, err := rhoai.AppsNamespace(u)
	if errors.Is(err, rhoai.ErrNoDSCInitialization) {
		return &types.RegistryPullCopyInfo{Name: RegistryPullCopyName, NoApplicationsNamespace: true}
	}
	if err != nil {
		return &types.RegistryPullCopyInfo{Name: RegistryPullCopyName, Problem: "Could not find RHOAI's applications namespace: " + redact(err.Error())}
	}
	info := &types.RegistryPullCopyInfo{Name: RegistryPullCopyName, Namespace: ns, NoQuayRHOAIEntry: quayAuth == ""}
	body, found, err := u.GetOptional(rhoai.NamespacedPath("v1", "secrets", ns, RegistryPullCopyName))
	if err != nil {
		info.Problem = fmt.Sprintf("Could not read Secret %s/%s: %s", ns, RegistryPullCopyName, kube.RedactSecrets(redact(err.Error()), existingCredentialForms(body)...))
		return info
	}
	if !found {
		return info
	}
	info.Exists = true
	var s struct {
		Metadata kube.ObjectMeta `json:"metadata"`
		Type     string          `json:"type"`
	}
	if json.Unmarshal(body, &s) == nil {
		info.Ours = s.Metadata.HasToolLabel()
		switch {
		case s.Metadata.Terminating():
			info.Unusable = "It is being deleted."
		case s.Type != "kubernetes.io/dockerconfigjson":
			info.Unusable = fmt.Sprintf("It has type %s, not the type this tool writes (kubernetes.io/dockerconfigjson).", s.Type)
		case s.Metadata.Annotations[ExcludeImageSecretAnnotation] == "true":
			info.Unusable = "It has the annotation " + ExcludeImageSecretAnnotation + "=true, so the internal registry does not use it."
		}
	}
	info.Current = info.Unusable == "" && quayAuth != "" && holdsOnly(body, quayAuth)
	return info
}
