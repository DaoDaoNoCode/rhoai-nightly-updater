package cluster

import (
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/serving"
)

// viewerMissing is what a project viewer misses for action id in project
// team-a: every request that is not a read, as the preflight reports it.
func viewerMissing(t *testing.T, id string) []MissingPermission {
	t.Helper()
	a, ok := ProjectActionByID(id)
	if !ok {
		t.Fatalf("no action %s", id)
	}
	var out []MissingPermission
	for _, r := range a.Requests {
		if r.Verb == "get" || r.Verb == "list" || r.Scope != ScopeProject {
			continue
		}
		req, err := r.accessRequest("team-a", "apps")
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, MissingPermission{Request: req, Text: missingText(req, r.Source, "team-a"), Source: r.Source, Target: r.Target})
	}
	return out
}

// A refusal names only what the request needs (the model it asks for),
// each object once with its verbs, and who grants it once.
func TestMissingSummaryNamesOnlyTheRequestedModel(t *testing.T) {
	missing := MissingFor(viewerMissing(t, "models.setup"), "smollm2-135m-instruct", "team-a")
	got := MissingSummary("Deploy a model", "team-a", missing)
	want := "Deploy a model in team-a needs permissions your OpenShift account does not have there: create llminferenceservices.serving.kserve.io; create, delete and patch llminferenceservices.serving.kserve.io smollm2-135m-instruct. Ask an admin of project team-a to give you the edit role there. Nothing was changed."
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	for _, m := range serving.ModelCatalog {
		if m.Name != "smollm2-135m-instruct" && strings.Contains(got, m.Name) {
			t.Errorf("names another model %s", m.Name)
		}
	}
	// Without a target (the capabilities): the chosen model, unnamed.
	caps := MissingSummaryFor("Deploy a model", "team-a", MissingFor(viewerMissing(t, "models.setup"), "", "team-a"))
	if caps != "Deploy a model in team-a needs permissions your OpenShift account does not have there: create, delete and patch llminferenceservices.serving.kserve.io. Ask an admin of project team-a to give you the edit role there." {
		t.Errorf("capabilities: %q", caps)
	}
	// Only other models' permissions missing: nothing this request needs.
	other := MissingFor([]MissingPermission{{Request: AccessRequest{Verb: "patch", Group: "serving.kserve.io", Resource: "llminferenceservices", Name: "qwen3-1-7b", Namespace: "team-a"}, Target: true}}, "smollm2-135m-instruct", "team-a")
	if len(other) != 0 {
		t.Errorf("another model's permission counts: %+v", other)
	}
}

// A user who is not an admin of the project is told only that, in one
// sentence, for the project S3 storage's actions.
func TestMissingSummaryForAProjectAdminAction(t *testing.T) {
	missing := append(viewerMissing(t, "project-s3.delete"), MissingPermission{Request: AccessRequest{Verb: "create", Group: "rbac.authorization.k8s.io", Resource: "rolebindings", Namespace: "team-a"}, Source: SourceProjectAdmin})
	if got := MissingSummaryFor("Delete the project's S3 storage", "team-a", missing); got != "Only an admin of project team-a can delete the project's S3 storage; ask one of them." {
		t.Fatalf("got %q", got)
	}
}

// Requests outside the project are said apart, with who grants them.
func TestMissingSummaryOutsideTheProject(t *testing.T) {
	missing := []MissingPermission{{Request: AccessRequest{Verb: "list", Group: "image.openshift.io", Resource: "imagestreams", Namespace: "apps"}}}
	got := MissingSummaryFor("Set up the workbench", "team-a", missing)
	if got != "Set up the workbench needs reads RHOAI lets every signed-in user make, which your account may not: list imagestreams.image.openshift.io in namespace apps. Ask a cluster-admin." {
		t.Fatalf("got %q", got)
	}
}
