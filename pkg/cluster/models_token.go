package cluster

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
)

// Token authentication, made as the dashboard's "Require token
// authentication" makes it for an LLMInferenceService (odh-dashboard
// rhoai-3.6 a9ea0d86):
//   - names: ServiceAccount <model>-sa, Role <model>-view-role, RoleBinding
//     <model>-view (packages/model-serving/src/concepts/auth.ts getTokenNames
//     L47-68);
//   - the Role: get on that one llminferenceservice, labelled
//     opendatahub.io/dashboard=true (auth.ts generateRole L75-98);
//   - the RoleBinding: that Role to that ServiceAccount, same label
//     (packages/k8s-core/src/api/roleBindings.ts
//     generateRoleBindingServiceAccount); the ServiceAccount has no label
//     (k8s-core api/serviceAccounts.ts assembleServiceAccount);
//   - the token: Secret <k8s name of the display name>-<model>-sa of type
//     kubernetes.io/service-account-token, annotated
//     kubernetes.io/service-account.name=<model>-sa and
//     openshift.io/display-name, labelled opendatahub.io/dashboard=true
//     (k8s-core api/secrets.ts assembleSecretSA L13-36), with the wizard's
//     default display name "default-name" (deploymentWizard/fields/
//     TokenAuthenticationField.tsx L77, L216);
//   - each owned by the LLMInferenceService, blockOwnerDeletion false
//     (auth.ts setUpTokenAuth L197-235, k8s-core api/k8sUtils.ts
//     addOwnerReference), so garbage collection removes them with it;
//   - made after the model, as llmd-serving deploy.ts L363-378 does.
//
// The dashboard lists a model's tokens as the Secrets labelled
// opendatahub.io/dashboard=true whose kubernetes.io/service-account.name is
// <model>-sa (auth.ts useDeploymentAuthTokens L283-308), and its Edit
// keeps authentication on only while that list is not empty
// (deploymentWizard/utils.ts getTokenAuthenticationFromDeployment L63-80,
// llmd-serving model.ts applyTokenAuthentication: an empty list writes
// security.opendatahub.io/enable-auth: "false"). Without these objects,
// saving the tool's model in the dashboard turned its authentication off.
//
// The objects also carry the tool label. Which are the tool's: the
// ServiceAccount, Role and RoleBinding are owned by the tool's model (an
// ownerReference to its recorded UID); the Secret is the one whose UID the
// tool recorded when it created it. Its existence is checked with a
// dry-run delete carrying the recorded UID as a precondition; the tool
// reads it (get on these names, since 4.0.0) only for "Can it be called?"
// (models_call.go), which sends the model's own token to it. That does
// not keep a leaked token of this tool from minting tokens: "create" on
// Secrets anywhere plus the named "get" on other Secrets lets it create a
// token Secret of such a name for any ServiceAccount and read it back
// (SECURITY.md, within the accepted "leaked SA token is about as good as
// cluster-admin" risk).
const (
	tokenDisplayName  = "default-name"
	dashboardLabel    = "opendatahub.io/dashboard"
	enableAuthAnnot   = "security.opendatahub.io/enable-auth"
	saNameAnnotation  = "kubernetes.io/service-account.name"
	tokenSecretType   = "kubernetes.io/service-account-token"
	rbacAPIVersion    = "rbac.authorization.k8s.io/v1"
	placeholderUID    = "00000000-0000-0000-0000-000000000000"
	tokenPartSA       = "ServiceAccount"
	tokenPartRole     = "Role"
	tokenPartBinding  = "RoleBinding"
	tokenPartSecret   = "Secret"
	tokenServiceAcctS = "-sa"
)

func tokenSAName(model string) string      { return model + tokenServiceAcctS }
func tokenRoleName(model string) string    { return model + "-view-role" }
func tokenBindingName(model string) string { return model + "-view" }
func tokenSecretName(model string) string  { return tokenDisplayName + "-" + tokenSAName(model) }

// The paths of a catalog model's token objects; only the catalog's names,
// written out so the RBAC rules (resourceNames) can be checked against them
// (rbac_coverage_test.go).
func tokenSAPath(project, model string) (string, bool) {
	switch model {
	case "smollm2-135m-instruct":
		return namespacedPath("v1", "serviceaccounts", project, "smollm2-135m-instruct-sa"), true
	case "qwen25-05b-instruct":
		return namespacedPath("v1", "serviceaccounts", project, "qwen25-05b-instruct-sa"), true
	case "qwen3-1-7b":
		return namespacedPath("v1", "serviceaccounts", project, "qwen3-1-7b-sa"), true
	case "granite-embedding-125m-english":
		return namespacedPath("v1", "serviceaccounts", project, "granite-embedding-125m-english-sa"), true
	}
	return "", false
}

func tokenRolePath(project, model string) (string, bool) {
	switch model {
	case "smollm2-135m-instruct":
		return namespacedPath("rbac.authorization.k8s.io/v1", "roles", project, "smollm2-135m-instruct-view-role"), true
	case "qwen25-05b-instruct":
		return namespacedPath("rbac.authorization.k8s.io/v1", "roles", project, "qwen25-05b-instruct-view-role"), true
	case "qwen3-1-7b":
		return namespacedPath("rbac.authorization.k8s.io/v1", "roles", project, "qwen3-1-7b-view-role"), true
	case "granite-embedding-125m-english":
		return namespacedPath("rbac.authorization.k8s.io/v1", "roles", project, "granite-embedding-125m-english-view-role"), true
	}
	return "", false
}

func tokenBindingPath(project, model string) (string, bool) {
	switch model {
	case "smollm2-135m-instruct":
		return namespacedPath("rbac.authorization.k8s.io/v1", "rolebindings", project, "smollm2-135m-instruct-view"), true
	case "qwen25-05b-instruct":
		return namespacedPath("rbac.authorization.k8s.io/v1", "rolebindings", project, "qwen25-05b-instruct-view"), true
	case "qwen3-1-7b":
		return namespacedPath("rbac.authorization.k8s.io/v1", "rolebindings", project, "qwen3-1-7b-view"), true
	case "granite-embedding-125m-english":
		return namespacedPath("rbac.authorization.k8s.io/v1", "rolebindings", project, "granite-embedding-125m-english-view"), true
	}
	return "", false
}

func tokenSecretPath(project, model string) (string, bool) {
	switch model {
	case "smollm2-135m-instruct":
		return namespacedPath("v1", "secrets", project, "default-name-smollm2-135m-instruct-sa"), true
	case "qwen25-05b-instruct":
		return namespacedPath("v1", "secrets", project, "default-name-qwen25-05b-instruct-sa"), true
	case "qwen3-1-7b":
		return namespacedPath("v1", "secrets", project, "default-name-qwen3-1-7b-sa"), true
	case "granite-embedding-125m-english":
		return namespacedPath("v1", "secrets", project, "default-name-granite-embedding-125m-english-sa"), true
	}
	return "", false
}

// tokenCreatePath is the collection a token object is created in.
func tokenCreatePath(kind, project string) string {
	switch kind {
	case tokenPartSA:
		return withToolFieldManager(namespacedPath("v1", "serviceaccounts", project, ""))
	case tokenPartRole:
		return withToolFieldManager(namespacedPath("rbac.authorization.k8s.io/v1", "roles", project, ""))
	case tokenPartBinding:
		return withToolFieldManager(namespacedPath("rbac.authorization.k8s.io/v1", "rolebindings", project, ""))
	}
	return withToolFieldManager(namespacedPath("v1", "secrets", project, ""))
}

// tokenObjectPath is the path of the ServiceAccount, Role or RoleBinding;
// the Secret has its own (tokenSecretPathOf): it is never read.
func tokenObjectPath(kind, project, model string) string {
	var p string
	switch kind {
	case tokenPartSA:
		p, _ = tokenSAPath(project, model)
	case tokenPartRole:
		p, _ = tokenRolePath(project, model)
	case tokenPartBinding:
		p, _ = tokenBindingPath(project, model)
	}
	return p
}

func tokenSecretPathOf(project, model string) string {
	p, _ := tokenSecretPath(project, model)
	return p
}

func tokenObjectName(kind, model string) string {
	switch kind {
	case tokenPartSA:
		return tokenSAName(model)
	case tokenPartRole:
		return tokenRoleName(model)
	case tokenPartBinding:
		return tokenBindingName(model)
	}
	return tokenSecretName(model)
}

// tokenParts are the objects in the order they are made (the binding needs
// the Role, the token the ServiceAccount).
var tokenParts = []string{tokenPartSA, tokenPartRole, tokenPartBinding, tokenPartSecret}

// tokenObject is one token object as the dashboard writes it, owned by the
// model (modelUID; a placeholder for a dry run before the model exists).
func tokenObject(kind, project, model, modelUID string) map[string]any {
	labels := toolLabels()
	if kind != tokenPartSA {
		labels[dashboardLabel] = "true"
	}
	meta := map[string]any{
		"name":      tokenObjectName(kind, model),
		"namespace": project,
		"labels":    labels,
		"ownerReferences": []any{map[string]any{
			"apiVersion": llmisvcAPIVersion, "kind": "LLMInferenceService", "name": model,
			"uid": firstNonEmpty(modelUID, placeholderUID), "blockOwnerDeletion": false,
		}},
	}
	obj := map[string]any{"metadata": meta}
	switch kind {
	case tokenPartSA:
		obj["apiVersion"], obj["kind"] = "v1", "ServiceAccount"
	case tokenPartRole:
		obj["apiVersion"], obj["kind"] = rbacAPIVersion, "Role"
		obj["rules"] = []any{map[string]any{
			"verbs": []any{"get"}, "apiGroups": []any{llmisvcGroup},
			"resources": []any{"llminferenceservices"}, "resourceNames": []any{model},
		}}
	case tokenPartBinding:
		obj["apiVersion"], obj["kind"] = rbacAPIVersion, "RoleBinding"
		obj["roleRef"] = map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": tokenRoleName(model)}
		obj["subjects"] = []any{map[string]any{"kind": "ServiceAccount", "name": tokenSAName(model)}}
	default:
		obj["apiVersion"], obj["kind"], obj["type"] = "v1", "Secret", tokenSecretType
		meta["annotations"] = map[string]any{saNameAnnotation: tokenSAName(model), "openshift.io/display-name": tokenDisplayName}
	}
	return obj
}

// tokenPlan is which token objects a setup makes (the others are there
// and the model's).
type tokenPlan struct {
	missing map[string]bool
}

// probeWithUID tells, without reading it, whether the object at path is
// the one with uid: "ours", "gone", or "other" (another object of the
// name). A server-side dry-run delete checks the UID precondition and
// deletes nothing (Kubernetes API concepts, "Dry-run"; apiserver
// DryRunnableStorage.Delete checks preconditions on a dry run).
func probeWithUID(c *Client, path, uid string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"apiVersion": "v1", "kind": "DeleteOptions", "preconditions": map[string]any{"uid": uid},
	})
	_, _, err := c.do(http.MethodDelete, path, "application/json", body, url.Values{"dryRun": {"All"}})
	switch {
	case err == nil:
		return "ours", nil
	case IsK8sError(err, http.StatusNotFound):
		return "gone", nil
	case IsK8sError(err, http.StatusConflict):
		return "other", nil
	}
	return "", err
}

// planModelToken reads which token objects exist: the ServiceAccount, Role
// and RoleBinding must be the model's (modelUID; none for a new model), the
// Secret the recorded one. Any other object of those names stops the
// setup. It returns a refusal, or "".
func planModelToken(c *Client, project, model, modelUID, secretUID string) (tokenPlan, string, string) {
	plan := tokenPlan{missing: map[string]bool{}}
	for _, kind := range tokenParts[:3] {
		path := tokenObjectPath(kind, project, model)
		meta, found, err := readObjectMeta(c, path)
		if err != nil {
			return plan, fmt.Sprintf("Cannot read %s %s/%s: %v. Nothing was changed.%s", kind, project, tokenObjectName(kind, model), err, templateHint(err)), errorCodeFromK8sErr(err)
		}
		if !found {
			plan.missing[kind] = true
			continue
		}
		if modelUID == "" || !meta.ownedBy(modelUID) {
			return plan, foreignTokenRefusal(kind, project, model), "not_managed"
		}
	}
	plan.missing[tokenPartSecret] = true
	if secretUID != "" {
		state, err := probeWithUID(c, tokenSecretPathOf(project, model), secretUID)
		if err != nil {
			return plan, fmt.Sprintf("Cannot check Secret %s/%s: %v. Nothing was changed.%s", project, tokenSecretName(model), err, templateHint(err)), errorCodeFromK8sErr(err)
		}
		if state == "other" {
			return plan, foreignTokenRefusal(tokenPartSecret, project, model), "not_managed"
		}
		plan.missing[tokenPartSecret] = state == "gone"
	}
	return plan, "", ""
}

func foreignTokenRefusal(kind, project, model string) string {
	return fmt.Sprintf("Project %s already has a %s %s that this tool did not make for this model. The model's token authentication uses that name (as the dashboard names it), so nothing was changed; delete it, or set the model up in another project.", project, kind, tokenObjectName(kind, model))
}

// applyModelToken makes the missing token objects, owned by the model
// (modelUID), in the dashboard's order. A dry run (modelUID may be "")
// checks each create with the API server, including the RBAC escalation
// check on the Role and the RoleBinding; a RoleBinding whose Role does not
// exist yet answers 404 then, which the dashboard's dry run accepts too
// (auth.ts createRoleBindingIfMissing L145-147). It returns the Secret's
// UID when it made one, or a failure message and code.
func applyModelToken(c *Client, project, model, modelUID string, plan tokenPlan, dryRun bool) (string, string, string) {
	var query url.Values
	if dryRun {
		query = url.Values{"dryRun": {"All"}}
	}
	secretUID := ""
	for _, kind := range tokenParts {
		if !plan.missing[kind] {
			continue
		}
		desc := fmt.Sprintf("%s %s/%s", kind, project, tokenObjectName(kind, model))
		data, _ := json.Marshal(tokenObject(kind, project, model, modelUID))
		body, _, err := c.do(http.MethodPost, tokenCreatePath(kind, project), "application/json", data, query)
		switch {
		case err == nil:
			if kind == tokenPartSecret && !dryRun {
				meta, perr := parseObjectMeta(body)
				if perr != nil || meta.UID == "" {
					return "", fmt.Sprintf("%s was created, but its UID could not be read from the answer. Delete it (oc delete secret %s -n %s) and set up again.", desc, tokenSecretName(model), project), "internal"
				}
				secretUID = meta.UID
			}
		case dryRun && kind == tokenPartBinding && IsK8sError(err, http.StatusNotFound) && plan.missing[tokenPartRole]:
			// The Role is made first in the real run.
		case IsK8sError(err, http.StatusConflict) && kind != tokenPartSecret && !dryRun:
			// Made meanwhile: it must be the model's.
			meta, found, rerr := readObjectMeta(c, tokenObjectPath(kind, project, model))
			if rerr != nil || !found || !meta.ownedBy(modelUID) {
				return "", foreignTokenRefusal(kind, project, model), "not_managed"
			}
		case IsK8sError(err, http.StatusConflict):
			return "", foreignTokenRefusal(kind, project, model), "not_managed"
		default:
			verb := "Failed to create"
			if dryRun {
				verb = "The API server rejects"
			}
			return "", fmt.Sprintf("%s %s: %v.%s", verb, desc, err, templateHint(err)), errorCodeFromK8sErr(err)
		}
	}
	return secretUID, "", ""
}

// deleteModelToken deletes the token objects of the tool's model before
// the model goes. Garbage collection would remove them with the model
// (ownerReferences); deleting them first also covers a model whose
// ownerReferences were edited. Only the model's own (ownerReference to
// modelUID) and the recorded Secret (UID precondition) are deleted; a
// failure is logged, not fatal, since garbage collection still applies.
func deleteModelToken(c *Client, project, model, modelUID, secretUID string) []string {
	var logs []string
	for _, kind := range tokenParts[:3] {
		path := tokenObjectPath(kind, project, model)
		meta, found, err := readObjectMeta(c, path)
		if err != nil || !found || !meta.ownedBy(modelUID) {
			continue
		}
		if _, err := deleteWithUID(c, path, meta.UID); err != nil && !IsK8sError(err, http.StatusNotFound) {
			slog.Warn("cannot delete a model's token object; garbage collection removes it with the model", "kind", kind, "project", project, "model", model, "error", err)
			continue
		}
		logs = append(logs, fmt.Sprintf("OK: %s %s/%s deleted", kind, project, tokenObjectName(kind, model)))
	}
	if secretUID != "" {
		_, err := deleteWithUID(c, tokenSecretPathOf(project, model), secretUID)
		switch {
		case err == nil:
			logs = append(logs, fmt.Sprintf("OK: Secret %s/%s deleted", project, tokenSecretName(model)))
		case IsK8sError(err, http.StatusNotFound), IsK8sError(err, http.StatusConflict):
		default:
			slog.Warn("cannot delete a model's token Secret; garbage collection removes it with the model", "project", project, "model", model, "error", err)
		}
	}
	return logs
}

// tokenWords says what setup made for the token, for the run's step.
func tokenWords(project, model string, plan tokenPlan) string {
	var made []string
	for _, kind := range tokenParts {
		if plan.missing[kind] {
			made = append(made, kind+" "+tokenObjectName(kind, model))
		}
	}
	if len(made) == 0 {
		return ""
	}
	return fmt.Sprintf(" Token authentication as the dashboard makes it: %s in %s, owned by the model.", strings.Join(made, ", "), project)
}
