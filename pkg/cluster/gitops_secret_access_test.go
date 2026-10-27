package cluster

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// rbacObjects returns the Template's objects of kind in namespace (the
// parameter as written), by name.
func rbacObjects(t *testing.T, path, kind, namespace string) map[string]map[string]interface{} {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var tmpl struct {
		Objects []map[string]interface{} `yaml:"objects"`
	}
	if err := yaml.Unmarshal(data, &tmpl); err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]interface{}{}
	for _, o := range tmpl.Objects {
		meta, _ := o["metadata"].(map[string]interface{})
		if o["kind"] == kind && meta["namespace"] == namespace {
			out[meta["name"].(string)] = o
		}
	}
	return out
}

// An install moved from install.sh keeps the Role and RoleBinding its
// Template created in the pull secret's namespace unless the chart renders
// objects of the same names: Argo CD only applies, adopts and prunes what
// it renders. The gitops RBAC renders that Role with no rules, so the first
// sync empties it.
func TestGitOpsRBACEmptiesTheInstallShPullSecretRole(t *testing.T) {
	const ns = "${PULL_SECRET_NAMESPACE}"
	full := rbacObjects(t, templateRel, "Role", ns)
	gitops := rbacObjects(t, rbacGitOpsRel, "Role", ns)
	if len(full) == 0 {
		t.Fatal("deploy/template.yaml has no Role in the pull secret's namespace any more; this test is stale")
	}
	for name := range full {
		role, ok := gitops[name]
		if !ok {
			t.Fatalf("deploy/rbac-gitops.yaml does not render Role %s in %s, so a moved install keeps install.sh's grant", name, ns)
		}
		if rules, _ := role["rules"].([]interface{}); len(rules) != 0 {
			t.Fatalf("Role %s in %s grants %v; the gitops profile reads no Secret", name, ns, rules)
		}
	}
	for name := range rbacObjects(t, templateRel, "RoleBinding", ns) {
		if _, ok := rbacObjects(t, rbacGitOpsRel, "RoleBinding", ns)[name]; !ok {
			t.Fatalf("deploy/rbac-gitops.yaml does not render RoleBinding %s in %s", name, ns)
		}
	}
}

// The gitops profile's Diagnostics say when the ServiceAccount can still
// read Secrets, from SelfSubjectAccessReviews.
func TestCheckServiceAccountSecretAccess(t *testing.T) {
	for name, allow := range map[string]func(verb, ns, res string) bool{
		"none": func(string, string, string) bool { return false },
		"left by install.sh": func(verb, ns, res string) bool {
			return verb == "get" && ns == "kube-system" && res == "additional-pull-secret"
		},
		"cluster-wide reading": func(verb, ns, _ string) bool { return verb == "list" && ns == "" },
	} {
		t.Run(name, func(t *testing.T) {
			f, c := newFakeAPI(t)
			f.handle("POST", clusterPath("authorization.k8s.io/v1", "selfsubjectaccessreviews", ""), func(_ *http.Request, body []byte) (int, string) {
				var review struct {
					Kind string `json:"kind"`
					Spec struct {
						ResourceAttributes struct {
							Resource, Verb, Namespace, Name string
						} `json:"resourceAttributes"`
					} `json:"spec"`
				}
				_ = json.Unmarshal(body, &review)
				ra := review.Spec.ResourceAttributes
				if review.Kind != "SelfSubjectAccessReview" || ra.Resource != "secrets" {
					t.Errorf("unexpected review %s", body)
				}
				ok := allow(ra.Verb, ra.Namespace, ra.Name)
				b, _ := json.Marshal(map[string]interface{}{"status": map[string]interface{}{"allowed": ok}})
				return 201, string(b)
			})
			out := checkServiceAccountSecretAccess(c)
			if name == "none" {
				if out.check.Status != "pass" || len(out.problems) != 0 {
					t.Fatalf("%+v", out)
				}
				return
			}
			if out.check.Status != "fail" || len(out.problems) != 1 || out.problems[0].ID != "gitops-secret-access" {
				t.Fatalf("%+v", out)
			}
			if name == "left by install.sh" && !strings.Contains(out.check.Detail, "Secret kube-system/additional-pull-secret") {
				t.Fatalf("detail %q", out.check.Detail)
			}
		})
	}
	f, c := newFakeAPI(t)
	f.status("POST", clusterPath("authorization.k8s.io/v1", "selfsubjectaccessreviews", ""), http.StatusServiceUnavailable, "ServiceUnavailable")
	if out := checkServiceAccountSecretAccess(c); out.check.Status != "warn" || len(out.problems) != 0 {
		t.Fatalf("API error: %+v", out)
	}
}
