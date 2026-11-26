package cluster

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/serving"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/workbench"
)

// putModelRecord stores the models record (snapshot key "models") next to
// what the snapshot ConfigMap holds already.
func putModelRecord(t *testing.T, f *resourceFake, entries serving.ModelsRecord) {
	t.Helper()
	path := "/api/v1/namespaces/" + record.GetActivityNamespace() + "/configmaps/" + record.SnapshotConfigMapName
	data := map[string]string{}
	if cm := f.get(path); cm != nil {
		if d, ok := cm["data"].(map[string]any); ok {
			for k, v := range d {
				data[k], _ = v.(string)
			}
		}
	}
	raw, _ := json.Marshal(entries)
	data[serving.ModelsRecordKey] = string(raw)
	cm, _ := json.Marshal(map[string]any{"metadata": map[string]any{"name": record.SnapshotConfigMapName}, "data": data})
	f.putJSON(path, string(cm))
}

// A user's model entries count in the caps by project, as the workbench's
// and the playground's do: the four catalog models of one project are one
// project, and a user at their cap is refused a model in another project.
func TestModelRecordEntriesCountInTheCaps(t *testing.T) {
	f, c := newResourceFake(t)
	rec := serving.ModelsRecord{}
	for i := range record.MaxUserRecordProjectsPerUser {
		p := fmt.Sprintf("alice-%d", i)
		f.putJSON("/api/v1/namespaces/"+p, `{"metadata":{"name":"`+p+`","uid":"uid-`+p+`"}}`)
		for _, m := range serving.ModelCatalog {
			rec[serving.ModelRecordKey(p, m.Name)] = serving.RecordedModel{UID: "m", By: "alice", ActorRole: record.ActorRoleUser, ProjectUID: "uid-" + p}
		}
	}
	putModelRecord(t, f, rec)
	owners, err := record.ReadRecordOwners(c)
	if err != nil {
		t.Fatal(err)
	}
	if len(owners) != record.MaxUserRecordProjectsPerUser*len(serving.ModelCatalog) {
		t.Fatalf("model entries read as owners: %d", len(owners))
	}
	for _, o := range owners {
		if o.Bytes == 0 || o.ActorRole != record.ActorRoleUser {
			t.Fatalf("an owner without its size or role: %+v", o)
		}
	}
	u := userClientOf(c, fakeUserToken)
	u.SetUsername("alice")
	u.SetProjectUser("uid-new")
	var full *record.RecordsFullError
	if err := record.AdmitUserEntry(u, "new", nil, []byte(`{}`)); !errors.As(err, &full) || !strings.Contains(full.Msg, "10 projects") {
		t.Fatalf("an 11th project of the same user: %v", err)
	}
	same := userClientOf(c, fakeUserToken)
	same.SetUsername("alice")
	same.SetProjectUser("uid-alice-0")
	if err := record.AdmitUserEntry(same, "alice-0", nil, []byte(`{}`)); err != nil {
		t.Fatalf("another model in a project that holds the user's entries: %v", err)
	}
}

// The ceiling counts bytes too: users' entries, stale ones included, never
// take more than MaxUserRecordBytes of the snapshot ConfigMap.
func TestUserRecordBytesCeiling(t *testing.T) {
	f, c := newResourceFake(t)
	stale := map[string]workbench.Recorded{}
	pad := strings.Repeat("x", 1500)
	for i := range record.MaxUserRecordBytes / 1500 {
		stale[fmt.Sprintf("gone-%d/%s", i, workbench.Name)] = workbench.Recorded{UID: "u", By: fmt.Sprintf("u-%d-%s", i, pad), ActorRole: record.ActorRoleUser, ProjectUID: fmt.Sprintf("uid-gone-%d", i)}
	}
	putOwnerRecords(t, f, stale)
	u := userClientOf(c, fakeUserToken)
	u.SetUsername("alice")
	u.SetProjectUser("uid-proj")
	var full *record.RecordsFullError
	if err := record.AdmitUserEntry(u, "proj", nil, []byte(`{}`)); !errors.As(err, &full) || !strings.Contains(full.Msg, "KiB of 500)") {
		t.Fatalf("a new entry past the bytes ceiling: %v", err)
	}
	admin := userClientOf(c, fakeUserToken)
	admin.SetUsername("kube:admin")
	if err := record.AdmitUserEntry(admin, "proj", nil, []byte(`{}`)); err != nil {
		t.Fatalf("a cluster-admin's entry is outside the ceiling: %v", err)
	}
}

// The background check forgets a user's model entry whose project is
// gone, or was made again under its name (another namespace UID) without
// the recorded model; it keeps one whose model still lives there, a
// published one, a legacy entry of a live project and one it cannot check.
func TestSweepDropsModelEntriesOfGoneProjects(t *testing.T) {
	f, c := newResourceFake(t)
	m := serving.ModelCatalog[0].Name
	f.putJSON("/api/v1/namespaces/live", `{"metadata":{"uid":"uid-live"}}`)
	f.putJSON("/api/v1/namespaces/remade", `{"metadata":{"uid":"uid-remade-new"}}`)
	f.putJSON("/api/v1/namespaces/remade-live", `{"metadata":{"uid":"uid-rl-new"}}`)
	f.putJSON("/apis/serving.kserve.io/v1alpha2/namespaces/remade-live/llminferenceservices/"+m, `{"metadata":{"name":"`+m+`","namespace":"remade-live","uid":"model-live"}}`)
	f.putJSON("/api/v1/namespaces/remade-unreadable", `{"metadata":{"uid":"uid-ru-new"}}`)
	f.fail["GET /apis/serving.kserve.io/v1alpha2/namespaces/remade-unreadable/llminferenceservices/"+m] = 500
	f.putJSON("/api/v1/namespaces/legacy", `{"metadata":{"uid":"whatever"}}`)
	putModelRecord(t, f, serving.ModelsRecord{
		serving.ModelRecordKey("live", m):              {UID: "a", ActorRole: record.ActorRoleUser, ProjectUID: "uid-live"},
		serving.ModelRecordKey("gone", m):              {UID: "b", ActorRole: record.ActorRoleUser, ProjectUID: "uid-gone"},
		serving.ModelRecordKey("remade", m):            {UID: "c", ActorRole: record.ActorRoleUser, ProjectUID: "uid-remade-old"},
		serving.ModelRecordKey("remade-live", m):       {UID: "model-live", ActorRole: record.ActorRoleUser, ProjectUID: "uid-rl-old"},
		serving.ModelRecordKey("remade-unreadable", m): {UID: "d", ActorRole: record.ActorRoleUser, ProjectUID: "uid-ru-old"},
		serving.ModelRecordKey("legacy", m):            {UID: "e"},
	})
	SweepGoneProjectRecords(c)
	rec, err := serving.ReadModelsRecord(c)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for k := range rec {
		got = append(got, k)
	}
	slices.Sort(got)
	want := []string{"legacy/" + m, "live/" + m, "remade-live/" + m, "remade-unreadable/" + m}
	if !slices.Equal(got, want) {
		t.Fatalf("model entries after the sweep: %v, want %v", got, want)
	}
}
