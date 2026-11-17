package cluster

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The samples' MLflow and KFP calls are authorized by those servers with
// access reviews of the app's token, so the RBAC coverage test cannot derive
// them from Kubernetes paths. They come from the fakes' rule tables instead
// (mlflowFakeRules, kfpFakeRoutes), which every samples test runs through:
// a call outside the tables fails its test, a name outside a rule's names
// fails it too, TestSampleFakeRulesAreAllUsed fails for an entry no code
// path uses, and the fakes decide every review with templateAllow, so each
// add, remove and status test also proves deploy/template.yaml sufficient.

const mlflowAuthGroup = "mlflow.kubeflow.org"

// otherServerRequests are the access reviews the samples' calls need to pass.
// A rule with a name parser is checked without a name first and, when that
// is denied, per name (auth/core.py:812-851); the named review is the one
// the narrow grants answer, so it is listed. SearchRuns' fallback is a named
// get (collection_filters.py:123-139).
func otherServerRequests() []rbacRequest {
	var out []rbacRequest
	for _, rule := range mlflowFakeRules {
		for _, c := range rule.checks {
			if rule.name == "" {
				out = append(out, rbacRequest{Verb: c.verb, Group: mlflowAuthGroup, Resource: c.resource, Namespace: anyNS, Site: "MLflow " + rule.key()})
				continue
			}
			verb := c.verb
			if rule.name == "body:experiment_ids:get" {
				verb = "get"
			}
			for _, n := range rule.names {
				out = append(out, rbacRequest{Verb: verb, Group: mlflowAuthGroup, Resource: c.resource, Namespace: anyNS, Name: n, Site: "MLflow " + rule.key()})
			}
		}
	}
	for _, rt := range kfpFakeRoutes {
		out = append(out, rbacRequest{Verb: rt.verb, Group: "datasciencepipelinesapplications.opendatahub.io", Resource: "datasciencepipelinesapplications/api", Namespace: anyNS, Name: dspaName, NamedReview: true, Site: "KFP " + rt.method + " " + rt.pattern})
	}
	return out
}

// templateAllow decides an access review with deploy/template.yaml's rules.
func templateAllow(t *testing.T) func(resource, verb, namespace, name string) bool {
	t.Helper()
	grants := templateGrants(t)
	return func(resource, verb, namespace, name string) bool {
		group := mlflowAuthGroup
		if strings.HasPrefix(resource, "datasciencepipelinesapplications") {
			group = "datasciencepipelinesapplications.opendatahub.io"
		}
		r := rbacRequest{Verb: verb, Group: group, Resource: resource, Namespace: namespace, Name: name, NamedReview: group != mlflowAuthGroup}
		for _, g := range grants {
			if g.allows(r) {
				return true
			}
		}
		return false
	}
}

func TestTemplateGrantsTheSamplesNoUnnamedDelete(t *testing.T) {
	allow := templateAllow(t)
	for _, res := range []string{"experiments", "datasets", "registeredmodels", "mcpservers", "skills"} {
		for _, verb := range []string{"delete", "update", "get", "list"} {
			if allow(res, verb, "any-project", "") {
				t.Errorf("the template grants %s %s on every MLflow object", verb, res)
			}
		}
	}
	for _, name := range []string{demoExperiment, "someone-elses"} {
		if allow("experiments", "delete", "p", name) {
			t.Errorf("delete on experiment %q is granted", name)
		}
	}
	if allow("datasciencepipelinesapplications/api", "get", "p", legacyDSPAName) {
		t.Error("the KFP API of DSPAs the tool did not create (dspa) is granted")
	}
	for _, verb := range []string{"update", "patch"} {
		if allow("datasciencepipelinesapplications/api", verb, "p", dspaName) {
			t.Errorf("%s on the KFP API is granted but never used", verb)
		}
	}
}

// TestSampleFakeRulesAreAllUsed runs every samples path through the fakes,
// with the template's RBAC, and checks that each rule-table entry is used.
func TestSampleFakeRulesAreAllUsed(t *testing.T) {
	env, k := newPipelineEnv(t)
	steps := []struct {
		action  string
		samples []string
	}{
		{"add", mlflowSampleIDs},
		{"remove", []string{SampleTraining, SampleMCPServer, SampleSkill}},
		{"add", []string{SampleTraining}}, // restores the experiment
		{"regenerate", []string{SampleGenAIDemo}},
	}
	for _, s := range steps {
		if ok, msg, st := env.run(t, s.action, s.samples...); !ok {
			t.Fatalf("%s %v: %s\n%s", s.action, s.samples, msg, stepMessages(st))
		}
	}
	env.mlflow.mlflowDeleteDemo()
	if ok, msg, _ := env.run(t, "add", SampleGenAIDemo); !ok { // restore + reset
		t.Fatal(msg)
	}
	env.status(t)
	for _, action := range []string{"run", "remove"} {
		if resp, st := runPipeline(t, env, action); !resp.Success {
			t.Fatalf("%s: %+v\n%s", action, resp, stepMessages(st))
		}
	}
	pipelineStatus(t, env)

	var unused []string
	for _, rule := range mlflowFakeRules {
		if !env.mlflow.used[rule.key()] {
			unused = append(unused, rule.key())
		}
	}
	for _, rt := range kfpFakeRoutes {
		used := false
		for _, r := range k.requests {
			method, path, _ := strings.Cut(r, " ")
			if method == rt.method && regexp.MustCompile(rt.pattern).MatchString(path) {
				used = true
			}
		}
		if !used {
			unused = append(unused, rt.method+" "+rt.pattern)
		}
	}
	sort.Strings(unused)
	if len(unused) > 0 {
		t.Errorf("rule-table entries no samples path uses (remove them, or the grants they justify):\n%s", strings.Join(unused, "\n"))
	}
}
