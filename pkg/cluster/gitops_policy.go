package cluster

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
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

// CheckGitOpsAdmissionPolicy returns nil when the gitops profile's admission
// policy is in place: the ValidatingAdmissionPolicy exists with
// failurePolicy Fail (unset means Fail), and its binding exists, names it
// and has validationActions Deny. It reads both by name. It cannot prove
// that the API server enforces them (a policy that does not compile denies
// by failurePolicy Fail); it proves that nobody removed or weakened them.
func CheckGitOpsAdmissionPolicy(c *Client) error {
	name := gitOpsAdmissionPolicyName()
	if name == "" {
		return fmt.Errorf("%s is not set, so the admission policy cannot be checked", GitOpsAdmissionPolicyEnv)
	}
	var policy struct {
		Spec struct {
			FailurePolicy string `json:"failurePolicy"`
		} `json:"spec"`
	}
	body, _, err := c.get(clusterPath("admissionregistration.k8s.io/v1", "validatingadmissionpolicies", name))
	if err != nil {
		return fmt.Errorf("read ValidatingAdmissionPolicy %s: %w", name, err)
	}
	if err := json.Unmarshal(body, &policy); err != nil {
		return fmt.Errorf("parse ValidatingAdmissionPolicy %s: %w", name, err)
	}
	if fp := policy.Spec.FailurePolicy; fp != "" && fp != "Fail" {
		return fmt.Errorf("ValidatingAdmissionPolicy %s has failurePolicy %s, not Fail", name, fp)
	}
	var binding struct {
		Spec struct {
			PolicyName        string   `json:"policyName"`
			ValidationActions []string `json:"validationActions"`
		} `json:"spec"`
	}
	body, _, err = c.get(clusterPath("admissionregistration.k8s.io/v1", "validatingadmissionpolicybindings", name))
	if err != nil {
		return fmt.Errorf("read ValidatingAdmissionPolicyBinding %s: %w", name, err)
	}
	if err := json.Unmarshal(body, &binding); err != nil {
		return fmt.Errorf("parse ValidatingAdmissionPolicyBinding %s: %w", name, err)
	}
	if binding.Spec.PolicyName != name {
		return fmt.Errorf("ValidatingAdmissionPolicyBinding %s binds %q, not the policy %s", name, binding.Spec.PolicyName, name)
	}
	if !containsString(binding.Spec.ValidationActions, "Deny") {
		return fmt.Errorf("ValidatingAdmissionPolicyBinding %s has validationActions %v, without Deny", name, binding.Spec.ValidationActions)
	}
	return nil
}
