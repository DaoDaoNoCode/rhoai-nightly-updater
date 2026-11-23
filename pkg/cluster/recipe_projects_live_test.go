package cluster

import (
	"net/http"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
)

// recordFiveProjects seeds five recipe projects in the snapshot, each paired
// with cluster state: live still has the stored UID, managed-by label, and
// recipe annotation; gone has no namespace; remade reuses the name with a
// different UID; unlabelled dropped the tool label; going carries a
// deletionTimestamp.
func recordFiveProjects(t *testing.T, f *resourceFake, c *Client) {
	t.Helper()
	const owned = `"labels":{"app.kubernetes.io/managed-by":"rhoai-nightly-updater"},"annotations":{"rhoai-nightly-updater/recipe":"dev"}`
	f.putJSON("/api/v1/namespaces/live", `{"metadata":{"uid":"uid-live",`+owned+`}}`)
	f.putJSON("/api/v1/namespaces/remade", `{"metadata":{"uid":"uid-other",`+owned+`}}`)
	f.putJSON("/api/v1/namespaces/unlabelled", `{"metadata":{"uid":"uid-unlabelled","annotations":{"rhoai-nightly-updater/recipe":"dev"}}}`)
	f.putJSON("/api/v1/namespaces/going", `{"metadata":{"uid":"uid-going","deletionTimestamp":"2026-11-23T05:40:20Z",`+owned+`}}`)
	if err := record.WriteSnapshotKey(c, "recipeProject", "test", `{"projects":[`+
		`{"name":"live","uid":"uid-live","recipe":"dev","createdAt":"2026-11-23T05:20:00Z"},`+
		`{"name":"gone","uid":"uid-gone","recipe":"dev"},`+
		`{"name":"remade","uid":"uid-remade","recipe":"dev"},`+
		`{"name":"unlabelled","uid":"uid-unlabelled","recipe":"dev"},`+
		`{"name":"going","uid":"uid-going","recipe":"dev"}]}`); err != nil {
		t.Fatal(err)
	}
}

func recordedProjectNames(t *testing.T, c *Client) string {
	t.Helper()
	raw, err := record.ReadSnapshotKey(c, "recipeProject")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, part := range strings.Split(raw, `"name":"`)[1:] {
		names = append(names, part[:strings.Index(part, `"`)])
	}
	return strings.Join(names, " ")
}

// F4/F5: RecipeOwnedProjects mirrors what the cluster still owns—namespaces
// that match the recorded UID, still carry the updater label and recipe
// annotation, and are not terminating. Missing namespaces never appear. The
// call is read-only.
func TestRecipeOwnedProjects_OnlyWhatTheClusterHas(t *testing.T) {
	f, c := projectFake(t)
	recordFiveProjects(t, f, c)
	before := len(f.mutations())
	got, err := RecipeOwnedProjects(c)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "live" || got[0].UID != "uid-live" || got[0].Recipe != "dev" || got[0].CreatedAt != "2026-11-23T05:20:00Z" {
		t.Fatalf("listed %+v", got)
	}
	if len(f.mutations()) != before {
		t.Fatalf("a read wrote: %v", f.mutations()[before:])
	}
}

// When any namespace GET fails, the whole listing aborts (fail closed) and
// the error propagates instead of partial results.
func TestRecipeOwnedProjects_UnreadableNamespaceFails(t *testing.T) {
	f, c := projectFake(t)
	recordFiveProjects(t, f, c)
	f.fail = map[string]int{"GET /api/v1/namespaces/going": http.StatusInternalServerError}
	if got, err := RecipeOwnedProjects(c); err == nil || got != nil {
		t.Fatalf("got %+v, %v", got, err)
	}
}

// F5: ForgetGoneRecipeProjects prunes snapshot rows for namespaces that
// vanished or were recreated under the same name; rows for live namespaces,
// ones mid-delete, and unreadable ones stay put because unreadable checks
// fail closed.
func TestForgetGoneRecipeProjects(t *testing.T) {
	f, c := projectFake(t)
	recordFiveProjects(t, f, c)
	f.putJSON("/api/v1/namespaces/unreadable", `{"metadata":{"uid":"uid-unreadable"}}`)
	raw, _ := record.ReadSnapshotKey(c, "recipeProject")
	if err := record.WriteSnapshotKey(c, "recipeProject", "test", strings.Replace(raw, `]}`, `,{"name":"unreadable","uid":"uid-unreadable","recipe":"dev"}]}`, 1)); err != nil {
		t.Fatal(err)
	}
	f.fail = map[string]int{"GET /api/v1/namespaces/unreadable": http.StatusInternalServerError}
	forgot, err := ForgetGoneRecipeProjects(c)
	if err != nil || strings.Join(forgot, " ") != "gone remade" {
		t.Fatalf("forgot %v, %v", forgot, err)
	}
	if got := recordedProjectNames(t, c); got != "live unlabelled going unreadable" {
		t.Fatalf("record keeps %q", got)
	}
	// Second run should be a no-op: snapshot already trimmed, no new writes.
	before := len(f.mutations())
	if forgot, err := ForgetGoneRecipeProjects(c); err != nil || len(forgot) != 0 || len(f.mutations()) != before {
		t.Fatalf("second pass forgot %v, %v, wrote %v", forgot, err, f.mutations()[before:])
	}
}

// Corrupt or unreadable recipeProject snapshots leave ForgetGoneRecipeProjects
// unchanged and report failure rather than dropping entries.
func TestForgetGoneRecipeProjects_UnreadableRecord(t *testing.T) {
	f, c := projectFake(t)
	if err := record.WriteSnapshotKey(c, "recipeProject", "test", `{not json`); err != nil {
		t.Fatal(err)
	}
	before := len(f.mutations())
	if forgot, err := ForgetGoneRecipeProjects(c); err == nil || len(forgot) != 0 || len(f.mutations()) != before {
		t.Fatalf("forgot %v, %v", forgot, err)
	}
}

// F4: GetResourcesStatus exposes surviving recipe projects on the Environment
// view so "Clean up everything" sees the same live set as RecipeOwnedProjects.
func TestGetResourcesStatus_ListsTheRecipeProjectsThatExist(t *testing.T) {
	f, c := projectFake(t)
	recordFiveProjects(t, f, c)
	st, err := GetResourcesStatus(c)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.RecipeProjects) != 1 || st.RecipeProjects[0].Name != "live" || st.RecipeProjectsError != "" {
		t.Fatalf("recipe projects %+v (%q)", st.RecipeProjects, st.RecipeProjectsError)
	}
	f.fail = map[string]int{"GET /api/v1/namespaces/live": http.StatusInternalServerError}
	st, err = GetResourcesStatus(c)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.RecipeProjects) != 0 || !strings.Contains(st.RecipeProjectsError, "live") {
		t.Fatalf("unreadable: %+v (%q)", st.RecipeProjects, st.RecipeProjectsError)
	}
}
