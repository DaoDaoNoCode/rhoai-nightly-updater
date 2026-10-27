package cluster

import (
	"encoding/json"
	"fmt"
	"strings"
)

// secretAccessCheckName is the gitops profile's Diagnostics check that the
// ServiceAccount cannot read Secrets. The profile never reads one
// (docs/GITOPS.md, section 11), but RBAC left by an earlier install can
// still grant it: install.sh's Template gives Role <APP_NAME>-<NAMESPACE> in
// kube-system get and patch on additional-pull-secret, which the chart
// empties on its first sync (deploy/rbac-gitops.yaml), and any other
// binding an administrator added stays.
const secretAccessCheckName = "ServiceAccount Secret access"

// secretAccessProbes are the questions asked: reading any Secret, cluster
// wide or by list, and the pull secret install.sh granted.
var secretAccessProbes = []struct{ verb, namespace, name, where string }{
	{"get", "", "", "Secrets in every namespace"},
	{"list", "", "", "Secrets in every namespace (list)"},
	{"get", "kube-system", "additional-pull-secret", "Secret kube-system/additional-pull-secret"},
}

// canI asks the API server whether this client's identity may do verb on
// secrets, with a SelfSubjectAccessReview. Every authenticated identity may
// create one: Kubernetes binds ClusterRole system:basic-user, which allows
// create on selfsubjectaccessreviews, to system:authenticated
// (plugin/pkg/auth/authorizer/rbac/bootstrappolicy/policy.go, basicUserRules
// and the system:basic-user ClusterRoleBinding), so the check needs no RBAC
// of its own.
func canI(c *Client, verb, namespace, name string) (bool, error) {
	body, _ := json.Marshal(map[string]interface{}{
		"apiVersion": "authorization.k8s.io/v1",
		"kind":       "SelfSubjectAccessReview",
		"spec": map[string]interface{}{"resourceAttributes": map[string]interface{}{
			"group": "", "resource": "secrets", "verb": verb, "namespace": namespace, "name": name,
		}},
	})
	resp, _, err := c.post(clusterPath("authorization.k8s.io/v1", "selfsubjectaccessreviews", ""), body)
	if err != nil {
		return false, err
	}
	var review struct {
		Status struct {
			Allowed bool `json:"allowed"`
		} `json:"status"`
	}
	if err := json.Unmarshal(resp, &review); err != nil {
		return false, fmt.Errorf("parse SelfSubjectAccessReview: %w", err)
	}
	return review.Status.Allowed, nil
}

// checkServiceAccountSecretAccess reports when the ServiceAccount can still
// read Secrets, which the gitops profile never needs.
func checkServiceAccountSecretAccess(c *Client) checkOutput {
	var allowed []string
	for _, p := range secretAccessProbes {
		ok, err := canI(c, p.verb, p.namespace, p.name)
		if err != nil {
			return checkOutput{check: CheckResult{Name: secretAccessCheckName, Status: "warn", Detail: fmt.Sprintf("Could not check: %v", err)}}
		}
		if ok {
			allowed = append(allowed, p.where)
		}
	}
	if len(allowed) == 0 {
		return checkOutput{check: CheckResult{Name: secretAccessCheckName, Status: "pass", Detail: "The updater's ServiceAccount cannot read Secrets"}}
	}
	detail := "The updater's ServiceAccount can read " + strings.Join(allowed, "; ")
	return checkOutput{
		check: CheckResult{Name: secretAccessCheckName, Status: "fail", Detail: detail},
		problems: []Problem{{
			ID:          "gitops-secret-access",
			Severity:    "warning",
			Title:       "The updater's ServiceAccount can still read Secrets",
			Description: "The gitops profile never reads a Secret, but some RBAC still lets its ServiceAccount read them. This is usually left from an install with install.sh, or a binding added by hand.",
			Evidence:    allowed,
			Fix:         "Sync the updater's Argo CD application (the chart empties the Role install.sh created in kube-system), then remove any other binding that grants the ServiceAccount access to Secrets (docs/GITOPS.md, section 10).",
			TechnicalCmd: "oc auth can-i get secret/additional-pull-secret -n kube-system --as=system:serviceaccount:<NAMESPACE>:<APP_NAME>\n" +
				"oc get rolebindings,clusterrolebindings -A -o wide | grep <APP_NAME>",
		}},
	}
}
