package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/ext"
	"gopkg.in/yaml.v3"
)

// The admission policy of deploy/rbac-gitops.yaml, evaluated with cel-go,
// the CEL library the API server evaluates ValidatingAdmissionPolicies
// with. The API server passes object and oldObject as unstructured maps
// (objectToResolveVal in k8s.io/apiserver pkg/admission/plugin/cel/filter.go)
// and adds the strings extension (k8s.io/apiserver pkg/cel/environment/base.go),
// as this environment does. It is not the API server: enforcement itself is
// only testable on a cluster.

type policyCase struct {
	Name      string                 `json:"name"`
	Allow     bool                   `json:"allow"`
	User      string                 `json:"user"`
	Request   map[string]string      `json:"request"`
	Object    map[string]interface{} `json:"object"`
	OldObject map[string]interface{} `json:"oldObject"`
}

type compiledPolicy struct {
	matchConditions []cel.Program
	variables       []struct {
		name string
		prg  cel.Program
	}
	validations []cel.Program
}

// loadGitOpsPolicy renders deploy/rbac-gitops.yaml with its parameter
// defaults and compiles the policy's expressions.
func loadGitOpsPolicy(t *testing.T) compiledPolicy {
	t.Helper()
	data, err := os.ReadFile(rbacGitOpsRel)
	if err != nil {
		t.Fatal(err)
	}
	var params struct {
		Parameters []templateParameter `yaml:"parameters"`
	}
	if err := yaml.Unmarshal(data, &params); err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, p := range params.Parameters {
		text = strings.ReplaceAll(text, "${"+p.Name+"}", p.Value)
	}
	var tmpl struct {
		Objects []struct {
			Kind string `yaml:"kind"`
			Spec struct {
				MatchConditions []struct{ Expression string }       `yaml:"matchConditions"`
				Variables       []struct{ Name, Expression string } `yaml:"variables"`
				Validations     []struct{ Expression string }       `yaml:"validations"`
			} `yaml:"spec"`
		} `yaml:"objects"`
	}
	if err := yaml.Unmarshal([]byte(text), &tmpl); err != nil {
		t.Fatal(err)
	}
	env, err := cel.NewEnv(
		cel.Variable("object", cel.DynType), cel.Variable("oldObject", cel.DynType), cel.Variable("request", cel.DynType),
		cel.Variable("variables", cel.MapType(cel.StringType, cel.DynType)),
		ext.Strings(),
	)
	if err != nil {
		t.Fatal(err)
	}
	compile := func(expr string) cel.Program {
		ast, iss := env.Compile(expr)
		if iss.Err() != nil {
			t.Fatalf("compile %q: %v", expr, iss.Err())
		}
		prg, err := env.Program(ast)
		if err != nil {
			t.Fatal(err)
		}
		return prg
	}
	var p compiledPolicy
	found := false
	for _, o := range tmpl.Objects {
		if o.Kind != "ValidatingAdmissionPolicy" {
			continue
		}
		found = true
		for _, m := range o.Spec.MatchConditions {
			p.matchConditions = append(p.matchConditions, compile(m.Expression))
		}
		for _, v := range o.Spec.Variables {
			p.variables = append(p.variables, struct {
				name string
				prg  cel.Program
			}{v.Name, compile(v.Expression)})
		}
		for _, v := range o.Spec.Validations {
			p.validations = append(p.validations, compile(v.Expression))
		}
	}
	if !found || len(p.validations) == 0 {
		t.Fatal("deploy/rbac-gitops.yaml has no ValidatingAdmissionPolicy with validations")
	}
	return p
}

// admits evaluates the policy as the API server would with failurePolicy
// Fail: a match condition that is false skips the policy, and a validation
// that is false or fails denies.
func (p compiledPolicy) admits(c policyCase) (bool, string) {
	user := c.User
	if user == "" {
		user = "system:serviceaccount:rhoai-nightly-updater:rhoai-nightly-updater"
	}
	request := map[string]interface{}{
		"userInfo":  map[string]interface{}{"username": user},
		"operation": c.Request["operation"],
		"resource":  map[string]interface{}{"group": "operators.coreos.com", "resource": c.Request["resource"]},
		"namespace": c.Request["namespace"],
		"name":      c.Request["name"],
	}
	vars := map[string]interface{}{"request": request, "object": nilIfEmpty(c.Object), "oldObject": nilIfEmpty(c.OldObject)}
	for _, m := range p.matchConditions {
		out, _, err := m.Eval(vars)
		if err != nil {
			return false, "match condition error: " + err.Error()
		}
		if out.Value() != true {
			return true, "skipped"
		}
	}
	variables := map[string]interface{}{}
	vars["variables"] = variables
	for _, v := range p.variables {
		out, _, err := v.prg.Eval(vars)
		if err != nil {
			return false, "variable " + v.name + ": " + err.Error()
		}
		variables[v.name] = out
	}
	for i, prg := range p.validations {
		out, _, err := prg.Eval(vars)
		if err != nil {
			return false, fmt.Sprintf("validation %d error: %v", i, err)
		}
		if out.Value() != true {
			return false, fmt.Sprintf("validation %d is false", i)
		}
	}
	return true, ""
}

func nilIfEmpty(m map[string]interface{}) interface{} {
	if m == nil {
		return nil
	}
	return m
}

func loadPolicyCases(t *testing.T) []policyCase {
	t.Helper()
	data, err := os.ReadFile("testdata/gitops-policy-cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Cases []policyCase `json:"cases"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	return file.Cases
}

// The policy's CEL and gitOpsSubscriptionProblem, which Update checks
// before it deletes anything, agree on every case, and each case gets the
// expected answer.
func TestGitOpsPolicyPredicateMatchesTheCEL(t *testing.T) {
	policy := loadGitOpsPolicy(t)
	cases := loadPolicyCases(t)
	seen := map[string]map[bool]bool{}
	for _, c := range cases {
		allowed, why := policy.admits(c)
		if allowed != c.Allow {
			t.Errorf("%s: the policy allowed=%v (%s), want %v", c.Name, allowed, why, c.Allow)
		}
		res := c.Request["resource"]
		if seen[res] == nil {
			seen[res] = map[bool]bool{}
		}
		seen[res][c.Allow] = true
		op := c.Request["operation"]
		if res == "subscriptions" && c.User == "" && (op == "CREATE" || op == "UPDATE") {
			spec, _ := c.Object["spec"].(map[string]interface{})
			problem := gitOpsSubscriptionProblem(c.Request["namespace"], c.Request["name"], spec, strings.Split(DefaultSubscriptionEnvAllowlist, ","))
			if (problem == "") != c.Allow {
				t.Errorf("%s: the Go predicate says %q, the policy allowed=%v", c.Name, problem, c.Allow)
			}
		}
	}
	for _, res := range []string{"subscriptions", "installplans", "clusterserviceversions"} {
		if !seen[res][true] || !seen[res][false] {
			t.Errorf("no allowed and denied case for %s", res)
		}
	}
	for _, res := range []string{"catalogsources", "operatorgroups"} {
		if !seen[res][false] {
			t.Errorf("no denied case for %s", res)
		}
	}
}

// The allowlist is read the way the policy reads it: split on commas, no
// trimming; unset means the Template's default.
func TestSubscriptionEnvAllowlist(t *testing.T) {
	t.Setenv(SubscriptionEnvAllowlistEnv, "A,B")
	if got := subscriptionEnvAllowlist(); strings.Join(got, "|") != "A|B" {
		t.Fatalf("got %v", got)
	}
	t.Setenv(SubscriptionEnvAllowlistEnv, "")
	if got := subscriptionEnvAllowlist(); len(got) != 1 || got[0] != "" {
		t.Fatalf("empty: got %v", got)
	}
	os.Unsetenv(SubscriptionEnvAllowlistEnv)
	if got := subscriptionEnvAllowlist(); strings.Join(got, ",") != DefaultSubscriptionEnvAllowlist {
		t.Fatalf("unset: got %v", got)
	}
}

const testGitOpsPolicyName = "rhoai-nightly-updater-ns1-gitops"

// useGitOpsPolicy sets up what main and the Template set up in a pod of the
// gitops profile: the embedded deploy/rbac-gitops.yaml, the namespace ns1
// and the policy's name.
func useGitOpsPolicy(t *testing.T) {
	t.Helper()
	data, err := os.ReadFile(rbacGitOpsRel)
	if err != nil {
		t.Fatal(err)
	}
	prev := gitOpsPolicyTemplate
	SetGitOpsPolicyTemplate(data)
	t.Cleanup(func() { SetGitOpsPolicyTemplate(prev) })
	t.Setenv("NAMESPACE", "ns1")
	t.Setenv(GitOpsAdmissionPolicyEnv, testGitOpsPolicyName)
}

// livePolicy returns the policy and binding of deploy/rbac-gitops.yaml as
// the API server returns them, with the defaults it fills in (as an
// OpenShift 4.21 cluster returns its policies).
func livePolicy(t *testing.T) (policy, binding map[string]interface{}) {
	t.Helper()
	p, b, err := expectedGitOpsAdmission(testGitOpsPolicyName)
	if err != nil {
		t.Fatal(err)
	}
	mc := p["matchConstraints"].(map[string]interface{})
	mc["namespaceSelector"] = map[string]interface{}{}
	mc["objectSelector"] = map[string]interface{}{}
	for _, r := range mc["resourceRules"].([]interface{}) {
		r.(map[string]interface{})["scope"] = "*"
	}
	return map[string]interface{}{"metadata": map[string]interface{}{"name": testGitOpsPolicyName, "uid": "u1"}, "spec": p},
		map[string]interface{}{"metadata": map[string]interface{}{"name": testGitOpsPolicyName, "uid": "u2"}, "spec": b}
}

// servesGitOpsPolicy makes f a cluster with the gitops profile's policy in
// place, for a pod set up by useGitOpsPolicy.
func servesGitOpsPolicy(t *testing.T, f *fakeOLM) *fakeOLM {
	t.Helper()
	useGitOpsPolicy(t)
	policy, binding := livePolicy(t)
	f.admission = map[string]map[string]interface{}{
		clusterPath("admissionregistration.k8s.io/v1", "validatingadmissionpolicies", testGitOpsPolicyName):       policy,
		clusterPath("admissionregistration.k8s.io/v1", "validatingadmissionpolicybindings", testGitOpsPolicyName): binding,
	}
	return f
}

// The policy and binding must be exactly this release's: each way of
// weakening them, or a policy rendered with another allowlist, is refused.
func TestCheckGitOpsAdmissionPolicy(t *testing.T) {
	spec := func(o map[string]interface{}) map[string]interface{} { return o["spec"].(map[string]interface{}) }
	for _, tc := range []struct {
		desc   string
		change func(policy, binding map[string]interface{})
		want   string
	}{
		{"in place", func(_, _ map[string]interface{}) {}, ""},
		{"failurePolicy unset (Fail)", func(p, _ map[string]interface{}) { delete(spec(p), "failurePolicy") }, ""},
		{"no policy", func(p, _ map[string]interface{}) { p["gone"] = true }, "read ValidatingAdmissionPolicy"},
		{"no binding", func(_, b map[string]interface{}) { b["gone"] = true }, "read ValidatingAdmissionPolicyBinding"},
		{"failurePolicy Ignore", func(p, _ map[string]interface{}) { spec(p)["failurePolicy"] = "Ignore" }, "spec.failurePolicy"},
		{"warn only", func(_, b map[string]interface{}) { spec(b)["validationActions"] = []interface{}{"Warn"} }, "spec.validationActions"},
		{"binds another", func(_, b map[string]interface{}) { spec(b)["policyName"] = "other" }, "spec.policyName"},
		{"binding narrowed to other namespaces", func(_, b map[string]interface{}) {
			spec(b)["matchResources"] = map[string]interface{}{"namespaceSelector": map[string]interface{}{"matchLabels": map[string]interface{}{"x": "y"}}}
		}, "spec.matchResources"},
		{"policy narrowed to other namespaces", func(p, _ map[string]interface{}) {
			spec(p)["matchConstraints"].(map[string]interface{})["namespaceSelector"] = map[string]interface{}{"matchLabels": map[string]interface{}{"x": "y"}}
		}, "spec.matchConstraints"},
		{"a match condition that skips everything", func(p, _ map[string]interface{}) {
			spec(p)["matchConditions"] = append(spec(p)["matchConditions"].([]interface{}), map[string]interface{}{"name": "never", "expression": "false"})
		}, "spec.matchConditions"},
		{"a validation removed", func(p, _ map[string]interface{}) {
			spec(p)["validations"] = spec(p)["validations"].([]interface{})[1:]
		}, "spec.validations"},
		{"another allowlist (a rollout in progress)", func(p, _ map[string]interface{}) {
			spec(p)["variables"] = []interface{}{map[string]interface{}{"name": "allowedEnv", "expression": "'DISABLE_DSC_CONFIG,RELATED_IMAGE_X'.split(',')"}}
		}, "spec.variables"},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			f := servesGitOpsPolicy(t, newFakeOLM(t))
			pPath := clusterPath("admissionregistration.k8s.io/v1", "validatingadmissionpolicies", testGitOpsPolicyName)
			bPath := clusterPath("admissionregistration.k8s.io/v1", "validatingadmissionpolicybindings", testGitOpsPolicyName)
			tc.change(f.admission[pPath], f.admission[bPath])
			for _, p := range []string{pPath, bPath} {
				if f.admission[p]["gone"] != nil {
					delete(f.admission, p)
				}
			}
			err := CheckGitOpsAdmissionPolicy(f.client(context.Background()))
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("err = %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
	t.Run("this pod's allowlist differs from the policy's", func(t *testing.T) {
		f := servesGitOpsPolicy(t, newFakeOLM(t))
		t.Setenv(SubscriptionEnvAllowlistEnv, "DISABLE_DSC_CONFIG,OTHER")
		if err := CheckGitOpsAdmissionPolicy(f.client(context.Background())); err == nil || !strings.Contains(err.Error(), "spec.variables") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("no policy name", func(t *testing.T) {
		useGitOpsPolicy(t)
		t.Setenv(GitOpsAdmissionPolicyEnv, "")
		if err := CheckGitOpsAdmissionPolicy(newFakeOLM(t).client(context.Background())); err == nil {
			t.Fatal("want an error")
		}
	})
	t.Run("a name of another namespace", func(t *testing.T) {
		useGitOpsPolicy(t)
		t.Setenv(GitOpsAdmissionPolicyEnv, "rhoai-nightly-updater-ns2-gitops")
		if err := CheckGitOpsAdmissionPolicy(newFakeOLM(t).client(context.Background())); err == nil || !strings.Contains(err.Error(), "not <APP_NAME>-ns1-gitops") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("a build without the policy file", func(t *testing.T) {
		useGitOpsPolicy(t)
		SetGitOpsPolicyTemplate(nil)
		if err := CheckGitOpsAdmissionPolicy(newFakeOLM(t).client(context.Background())); err == nil || !strings.Contains(err.Error(), "no deploy/rbac-gitops.yaml") {
			t.Fatalf("err = %v", err)
		}
	})
}

// The rendered policy is the chart's: every parameter is substituted, and
// the allowlist and ServiceAccount are this pod's.
func TestExpectedGitOpsAdmission(t *testing.T) {
	useGitOpsPolicy(t)
	t.Setenv(SubscriptionEnvAllowlistEnv, "A,B")
	policy, binding, err := expectedGitOpsAdmission(testGitOpsPolicyName)
	if err != nil {
		t.Fatal(err)
	}
	text := fmt.Sprint(policy, binding)
	if strings.Contains(text, "${") {
		t.Fatalf("unsubstituted parameter in %s", text)
	}
	for _, want := range []string{"'A,B'.split(',')", "system:serviceaccount:ns1:rhoai-nightly-updater", "sourceNamespace == '" + CatalogNS + "'", "request.namespace == '" + SubNS + "'"} {
		if !strings.Contains(text, want) {
			t.Errorf("rendered policy lacks %q", want)
		}
	}
	if binding["policyName"] != testGitOpsPolicyName {
		t.Errorf("binding = %v", binding)
	}
}
