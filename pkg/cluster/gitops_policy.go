package cluster

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// The gitops profile's admission policy (deploy/rbac-gitops.yaml) bounds
// what the updater's ServiceAccount may write, whatever its RBAC. Update
// checks the same rules before it deletes anything, so a Subscription it
// deletes can always be created again: the new one, and the previous one the
// restore re-applies. TestGitOpsPolicyPredicateMatchesTheCEL evaluates the
// policy's CEL and this predicate on the same cases.

// SubscriptionEnvAllowlistEnv is the environment variable (Template
// parameter SUBSCRIPTION_ENV_ALLOWLIST) with the comma-separated names of
// the environment variables a Subscription's spec.config may set in the
// gitops profile. The chart writes the same value into the policy.
const SubscriptionEnvAllowlistEnv = "SUBSCRIPTION_ENV_ALLOWLIST"

// DefaultSubscriptionEnvAllowlist is the Template's default: the one
// variable the AI Grid GitOps chart sets on the RHOAI Subscription
// (opendatahub-io/odh-gitops charts/rhai-on-openshift-chart/templates/operator/operator.yaml).
const DefaultSubscriptionEnvAllowlist = "DISABLE_DSC_CONFIG"

// subscriptionEnvAllowlist reads the allowlist. It splits on commas only, as
// the policy's split(',') does, so both see the same names.
func subscriptionEnvAllowlist() []string {
	v, ok := os.LookupEnv(SubscriptionEnvAllowlistEnv)
	if !ok {
		v = DefaultSubscriptionEnvAllowlist
	}
	return strings.Split(v, ",")
}

// gitOpsSubscriptionProblem says why the policy would refuse the
// ServiceAccount creating or updating Subscription name in namespace with
// spec; "" when it allows it. The rule: Subscription rhods-operator in the
// operator namespace, for package rhods-operator from CatalogSource
// rhoai-catalog-dev in the marketplace namespace, with no spec.config, or
// one that holds only env (no envFrom, volumes, volumeMounts, resources,
// nodeSelector, tolerations, affinity, selector or annotations: the
// SubscriptionConfig fields of operator-framework/api
// pkg/operators/v1alpha1/subscription_types.go, which OLM applies to the
// operator's Deployment), each entry only name and value, the name in
// allowedEnv.
func gitOpsSubscriptionProblem(namespace, name string, spec map[string]interface{}, allowedEnv []string) string {
	if namespace != SubNS || name != SubName {
		return fmt.Sprintf("the gitops profile may only write Subscription %s in %s, not %s in %s", SubName, SubNS, name, namespace)
	}
	if spec == nil {
		return "the Subscription has no spec"
	}
	for field, want := range map[string]string{"name": SubName, "source": CatalogName, "sourceNamespace": CatalogNS} {
		if got, _ := spec[field].(string); got != want {
			return fmt.Sprintf("its spec.%s is %q, and the gitops profile may only write a Subscription with %s %q", field, nonEmpty(got, "unset"), field, want)
		}
	}
	raw, present := spec["config"]
	if !present || raw == nil {
		return ""
	}
	config, ok := raw.(map[string]interface{})
	if !ok {
		return "its spec.config is not an object"
	}
	var other []string
	for k := range config {
		if k != "env" {
			other = append(other, k)
		}
	}
	if len(other) > 0 {
		sort.Strings(other)
		return fmt.Sprintf("its spec.config sets %s, and the gitops profile allows only env there", strings.Join(other, ", "))
	}
	rawEnv, present := config["env"]
	if !present || rawEnv == nil {
		return ""
	}
	env, ok := rawEnv.([]interface{})
	if !ok {
		return "its spec.config.env is not a list"
	}
	for _, e := range env {
		entry, ok := e.(map[string]interface{})
		if !ok {
			return "an entry of spec.config.env is not an object"
		}
		for k := range entry {
			if k != "name" && k != "value" {
				return fmt.Sprintf("an entry of spec.config.env sets %s; the gitops profile allows only name and value", k)
			}
		}
		n, isString := entry["name"].(string)
		if !isString {
			return "an entry of spec.config.env has no name"
		}
		if !containsString(allowedEnv, n) {
			return fmt.Sprintf("its spec.config.env sets %q, which is not in %s (%s)", n, SubscriptionEnvAllowlistEnv, strings.Join(allowedEnv, ","))
		}
	}
	return ""
}

// subscriptionSpec returns obj's spec (nil when it has none). obj may be a
// decoded object or one built by buildSubscription.
func subscriptionSpec(obj map[string]interface{}) map[string]interface{} {
	spec, _ := obj["spec"].(map[string]interface{})
	if spec == nil {
		return nil
	}
	// Normalise typed values (buildSubscription copies decoded JSON, but a
	// test or a caller may hold []map[string]interface{}): compare what the
	// API server would see.
	data, err := json.Marshal(spec)
	if err != nil {
		return spec
	}
	var out map[string]interface{}
	if json.Unmarshal(data, &out) != nil {
		return spec
	}
	return out
}

// gitOpsSubscriptionsProblem checks, before Update deletes anything, the
// Subscription it would create and the one the restore would re-apply
// (nil when there is none). It returns the refusal, or "".
func gitOpsSubscriptionsProblem(newSub, restoreSub map[string]interface{}) string {
	allowed := subscriptionEnvAllowlist()
	if why := gitOpsSubscriptionProblem(SubNS, SubName, subscriptionSpec(newSub), allowed); why != "" {
		return "The Subscription Update would create could not be written in the gitops profile: " + why + ". " + gitOpsPolicyFix
	}
	if restoreSub != nil {
		if why := gitOpsSubscriptionProblem(SubNS, SubName, subscriptionSpec(restoreSub), allowed); why != "" {
			return "The current Subscription could not be restored after a failure in the gitops profile: " + why + ". " + gitOpsPolicyFix
		}
	}
	return ""
}

const gitOpsPolicyFix = "Change the Subscription in Git, or add the variable to the chart's gitops.subscriptionEnvAllowlist (docs/GITOPS.md, section 11)."

// GitOpsAdmissionPolicyEnv is the environment variable with the name of the
// gitops profile's ValidatingAdmissionPolicy and of its binding
// (deploy/template.yaml sets it to <APP_NAME>-<NAMESPACE>-gitops, the names
// deploy/rbac-gitops.yaml gives them).
const GitOpsAdmissionPolicyEnv = "GITOPS_ADMISSION_POLICY"

func gitOpsAdmissionPolicyName() string {
	return os.Getenv(GitOpsAdmissionPolicyEnv)
}

// gitOpsPolicyTemplate is deploy/rbac-gitops.yaml, which main embeds and
// hands over with SetGitOpsPolicyTemplate: the admission policy and binding
// of this release.
var gitOpsPolicyTemplate []byte

// SetGitOpsPolicyTemplate sets the deploy/rbac-gitops.yaml that
// CheckGitOpsAdmissionPolicy compares the cluster's policy with.
func SetGitOpsPolicyTemplate(data []byte) {
	gitOpsPolicyTemplate = data
}

// expectedGitOpsAdmission renders the ValidatingAdmissionPolicy and binding
// of deploy/rbac-gitops.yaml as the chart does, with this pod's values:
// NAMESPACE is the pod's namespace, APP_NAME the rest of the policy's name
// (<APP_NAME>-<NAMESPACE>-gitops), SUBSCRIPTION_ENV_ALLOWLIST this pod's
// allowlist, OPERATOR_NAMESPACE and MARKETPLACE_NAMESPACE the namespaces the
// code writes to. Other parameters keep their defaults. It returns both
// specs.
func expectedGitOpsAdmission(name string) (policy, binding map[string]interface{}, err error) {
	if len(gitOpsPolicyTemplate) == 0 {
		return nil, nil, fmt.Errorf("this build has no deploy/rbac-gitops.yaml to compare the policy with")
	}
	ns := getActivityNamespace()
	app, ok := strings.CutSuffix(name, "-"+ns+"-gitops")
	if !ok || app == "" {
		return nil, nil, fmt.Errorf("%s is %q, not <APP_NAME>-%s-gitops", GitOpsAdmissionPolicyEnv, name, ns)
	}
	var tmpl struct {
		Parameters []struct {
			Name  string `yaml:"name"`
			Value string `yaml:"value"`
		} `yaml:"parameters"`
		Objects []map[string]interface{} `yaml:"objects"`
	}
	if err := yaml.Unmarshal(gitOpsPolicyTemplate, &tmpl); err != nil {
		return nil, nil, fmt.Errorf("parse deploy/rbac-gitops.yaml: %w", err)
	}
	values := map[string]string{}
	for _, p := range tmpl.Parameters {
		values[p.Name] = p.Value
	}
	values["NAMESPACE"] = ns
	values["APP_NAME"] = app
	values["OPERATOR_NAMESPACE"] = SubNS
	values["MARKETPLACE_NAMESPACE"] = CatalogNS
	values["SUBSCRIPTION_ENV_ALLOWLIST"] = strings.Join(subscriptionEnvAllowlist(), ",")
	for _, o := range tmpl.Objects {
		kind, _ := o["kind"].(string)
		if kind != "ValidatingAdmissionPolicy" && kind != "ValidatingAdmissionPolicyBinding" {
			continue
		}
		rendered, err := substituteParameters(o["spec"], values)
		if err != nil {
			return nil, nil, fmt.Errorf("render the %s of deploy/rbac-gitops.yaml: %w", kind, err)
		}
		spec, err := asJSONObject(rendered)
		if err != nil {
			return nil, nil, fmt.Errorf("render the %s of deploy/rbac-gitops.yaml: %w", kind, err)
		}
		if kind == "ValidatingAdmissionPolicy" {
			policy = spec
		} else {
			binding = spec
		}
	}
	if policy == nil || binding == nil {
		return nil, nil, fmt.Errorf("deploy/rbac-gitops.yaml has no ValidatingAdmissionPolicy and binding")
	}
	return policy, binding, nil
}

// substituteParameters replaces ${NAME} in every string of v, as `oc
// process` does for string parameters; an unknown parameter is an error.
func substituteParameters(v interface{}, values map[string]string) (interface{}, error) {
	switch t := v.(type) {
	case string:
		var err error
		out := templateParamRef.ReplaceAllStringFunc(t, func(ref string) string {
			name := ref[2 : len(ref)-1]
			val, ok := values[name]
			if !ok && err == nil {
				err = fmt.Errorf("unknown parameter %s", ref)
			}
			return val
		})
		return out, err
	case map[string]interface{}:
		out := make(map[string]interface{}, len(t))
		for k, e := range t {
			r, err := substituteParameters(e, values)
			if err != nil {
				return nil, err
			}
			out[k] = r
		}
		return out, nil
	case []interface{}:
		out := make([]interface{}, len(t))
		for i, e := range t {
			r, err := substituteParameters(e, values)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	}
	return v, nil
}

var templateParamRef = regexp.MustCompile(`\$\{[A-Za-z0-9_]+\}`)

// asJSONObject returns v as the API server would serialise it: a JSON
// object decoded into maps, slices, strings, float64s and bools.
func asJSONObject(v interface{}) (map[string]interface{}, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out map[string]interface{}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// withoutAdmissionDefaults removes, in place, the fields the API server
// fills in when they are not set, where they hold that default: in
// matchConstraints (a policy) and matchResources (a binding) namespaceSelector
// {}, objectSelector {}, matchPolicy Equivalent and each rule's scope "*",
// and a policy's failurePolicy Fail (SetDefaults_MatchResources,
// SetDefaults_Rule and SetDefaults_ValidatingAdmissionPolicySpec in
// k8s.io/kubernetes pkg/apis/admissionregistration/v1/defaults.go; the same
// fields an OpenShift 4.21 hub returns for its policies).
func withoutAdmissionDefaults(spec map[string]interface{}) map[string]interface{} {
	if spec["failurePolicy"] == "Fail" {
		delete(spec, "failurePolicy")
	}
	for _, key := range []string{"matchConstraints", "matchResources"} {
		m, _ := spec[key].(map[string]interface{})
		if m == nil {
			continue
		}
		for _, sel := range []string{"namespaceSelector", "objectSelector"} {
			if s, ok := m[sel].(map[string]interface{}); ok && len(s) == 0 {
				delete(m, sel)
			}
		}
		if m["matchPolicy"] == "Equivalent" {
			delete(m, "matchPolicy")
		}
		for _, rules := range []string{"resourceRules", "excludeResourceRules"} {
			list, _ := m[rules].([]interface{})
			for _, r := range list {
				if rule, _ := r.(map[string]interface{}); rule != nil && rule["scope"] == "*" {
					delete(rule, "scope")
				}
			}
		}
	}
	return spec
}

// differingKeys lists the top-level keys whose values differ between a and
// b, sorted.
func differingKeys(a, b map[string]interface{}) []string {
	var keys []string
	for k := range a {
		if !reflect.DeepEqual(a[k], b[k]) {
			keys = append(keys, "spec."+k)
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			keys = append(keys, "spec."+k)
		}
	}
	sort.Strings(keys)
	return keys
}

// CheckGitOpsAdmissionPolicy returns nil when the gitops profile's admission
// policy and its binding are on the cluster exactly as this release defines
// them (deploy/rbac-gitops.yaml, rendered with this pod's values; fields the
// API server defaults are ignored). So a binding narrowed to other
// namespaces, a policy that skips more requests, a weaker failurePolicy or
// a policy with another allowlist than this pod's are all refused: the last
// happens while Argo CD rolls out a changed allowlist, which it applies
// (wave -1) before it replaces this pod. It reads both by name. It cannot
// prove that the API server enforces them (a policy that does not compile
// denies by failurePolicy Fail); it proves that nobody removed or changed
// them.
func CheckGitOpsAdmissionPolicy(c *Client) error {
	name := gitOpsAdmissionPolicyName()
	if name == "" {
		return fmt.Errorf("%s is not set, so the admission policy cannot be checked", GitOpsAdmissionPolicyEnv)
	}
	wantPolicy, wantBinding, err := expectedGitOpsAdmission(name)
	if err != nil {
		return err
	}
	for _, o := range []struct {
		kind, resource string
		want           map[string]interface{}
	}{
		{"ValidatingAdmissionPolicy", "validatingadmissionpolicies", wantPolicy},
		{"ValidatingAdmissionPolicyBinding", "validatingadmissionpolicybindings", wantBinding},
	} {
		body, _, err := c.get(clusterPath("admissionregistration.k8s.io/v1", o.resource, name))
		if err != nil {
			return fmt.Errorf("read %s %s: %w", o.kind, name, err)
		}
		var live struct {
			Spec map[string]interface{} `json:"spec"`
		}
		if err := json.Unmarshal(body, &live); err != nil {
			return fmt.Errorf("parse %s %s: %w", o.kind, name, err)
		}
		got := withoutAdmissionDefaults(live.Spec)
		want := withoutAdmissionDefaults(o.want)
		if diff := differingKeys(want, got); len(diff) > 0 {
			return fmt.Errorf("%s %s differs from this release's (deploy/rbac-gitops.yaml) in %s", o.kind, name, strings.Join(diff, ", "))
		}
	}
	return nil
}
