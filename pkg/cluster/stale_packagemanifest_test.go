package cluster

import (
	"context"
	"strings"
	"testing"
)

const rolledBackCSV = "rhods-operator.3.5.0"

// catalogServesOlder: Git rolled the catalog back to an older version. The
// registry pod serves rolledBackCSV, but the PackageManifest (the
// packageserver's cache) still lists the previous head, gitOpsCSV, so the
// downgrade check passes on it. The fake resolves like OLM
// (operator-lifecycle-manager pkg/controller/registry/resolver/resolver.go,
// getSubscriptionVariables): with nothing installed, a startingCSV the
// served catalog does not have makes the Subscription unresolvable;
// without one, the served head is installed.
func catalogServesOlder(f *fakeOLM) {
	f.onSubscribe = func(f *fakeOLM, spec map[string]interface{}) {
		if pin, _ := spec["startingCSV"].(string); pin != "" && pin != rolledBackCSV {
			f.sub["status"] = map[string]interface{}{"conditions": []interface{}{cond("ResolutionFailed", "ConstraintsNotSatisfiable",
				"no operators found with name "+pin+" in channel beta of package rhods-operator in the catalog referenced by subscription rhods-operator")}}
			return
		}
		olmInstalls(rolledBackCSV)(f, spec)
	}
}

// Neither Update nor its restore lets OLM install the older build the
// catalog really serves: the new Subscription pins the verified head, the
// restored one the removed CSV.
func TestUpdateUnderGitOps_StalePackageManifestCannotDowngrade(t *testing.T) {
	for name, run := range map[string]func(f *fakeOLM) (string, string, bool){
		"full profile": func(f *fakeOLM) (string, string, bool) {
			r, _ := UpdateStream(f.client(context.Background()), gitOpsImage, func(UpdateStepEvent) {})
			return r.ErrorCode, r.Message, r.Success
		},
		"gitops profile": func(f *fakeOLM) (string, string, bool) {
			r, _ := UpdateStreamGitOpsProfile(servesGitOpsPolicy(t, f).client(context.Background()), gitOpsImage, func(UpdateStepEvent) {})
			return r.ErrorCode, r.Message, r.Success
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := gitOpsFake(t)
			catalogServesOlder(f)
			code, msg, ok := run(f)
			if ok || code != "resolution_failed" || !strings.Contains(msg, "no operators found with name "+gitOpsCSV) {
				t.Fatalf("code=%q ok=%v message=%q", code, ok, msg)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if _, installed := f.csvs[rolledBackCSV]; installed {
				t.Fatalf("OLM installed the older build %s", rolledBackCSV)
			}
			if f.sub == nil || f.sub["spec"].(map[string]interface{})["startingCSV"] != gitOpsCSV {
				t.Fatalf("restored Subscription = %v, want startingCSV %s (the removed CSV)", f.sub, gitOpsCSV)
			}
		})
	}
}
