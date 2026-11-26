package cluster

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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
	// An entry that stays the same user's but grows (a model's token
	// Secret UID) is checked too.
	gone0 := userClientOf(c, fakeUserToken)
	gone0.SetUsername("u-0-" + pad)
	gone0.SetProjectUser("uid-gone-0")
	prior := &record.RecordOwner{Project: "gone-0", ProjectUID: "uid-gone-0", By: "u-0-" + pad, ActorRole: record.ActorRoleUser}
	if err := record.AdmitUserEntryNamed(gone0, "workbenches", "gone-0/"+workbench.Name, "gone-0", prior, []byte(strings.Repeat("y", 1900))); !errors.As(err, &full) {
		t.Fatalf("a user's entry growing past the budget: %v", err)
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

// Two updater pods (one drains while its replacement runs) write the
// models record at once: the other pod's entry lands between this pod's
// read and its write. The write is conditional on what it read, so it
// conflicts, and UpdateSnapshot reads, merges and writes again: both
// entries stay.
func TestSnapshotWritesKeepAnotherPodsEntry(t *testing.T) {
	f, c := newResourceFake(t)
	m := serving.ModelCatalog[0].Name
	putModelRecord(t, f, serving.ModelsRecord{serving.ModelRecordKey("first", m): {UID: "a"}})
	path := "/api/v1/namespaces/" + record.GetActivityNamespace() + "/configmaps/" + record.SnapshotConfigMapName
	otherPod := c.WithContext(t.Context())
	interleaved, patches := false, 0
	f.beforeServe = func(method, p string) {
		if method != http.MethodPatch || p != path {
			return
		}
		patches++
		if interleaved {
			return
		}
		interleaved = true
		// The other pod's own read-change-write, outside this pod's writer.
		rec, err := serving.ReadModelsRecord(otherPod)
		if err != nil {
			t.Error(err)
			return
		}
		rec[serving.ModelRecordKey("other-pod", m)] = serving.RecordedModel{UID: "b"}
		cm := f.get(path)
		raw, _ := json.Marshal(rec)
		cm["data"].(map[string]any)[serving.ModelsRecordKey] = string(raw)
		f.bumpRV(cm["metadata"].(map[string]any))
	}
	err := record.UpdateSnapshot(c, func(tc *Client) error {
		rec, err := serving.ReadModelsRecord(tc)
		if err != nil {
			return err
		}
		rec[serving.ModelRecordKey("this-pod", m)] = serving.RecordedModel{UID: "c"}
		return serving.WriteModelsRecord(tc, rec)
	})
	if err != nil {
		t.Fatal(err)
	}
	rec, _ := serving.ReadModelsRecord(c)
	for _, p := range []string{"first", "other-pod", "this-pod"} {
		if _, ok := rec[serving.ModelRecordKey(p, m)]; !ok {
			t.Errorf("%s's entry is lost: %v", p, rec)
		}
	}
	if patches != 2 {
		t.Errorf("writes: %d, want the conflicting one and its retry", patches)
	}
}

// One budget for every user's entry: users fill each of their keys until
// admission refuses (entry ceilings per class, MaxUserRecordBytes in all,
// entries of projects that are gone, which the project caps do not count),
// next to a typical cluster-admin set of records, and the snapshot
// ConfigMap still fits in 1 MiB.
func TestUserRecordsFillTheBudgetUnder1MiB(t *testing.T) {
	f, c := newResourceFake(t)
	path := "/api/v1/namespaces/" + record.GetActivityNamespace() + "/configmaps/" + record.SnapshotConfigMapName
	pad := func(n int) string { return strings.Repeat("x", n) }
	// A typical cluster-admin set: their own entries of the same keys, and
	// the other records (components snapshot, MaaS, pipeline servers).
	data := map[string]map[string]json.RawMessage{}
	admin := func(key, name string, size int) {
		if data[key] == nil {
			data[key] = map[string]json.RawMessage{}
		}
		data[key][name] = json.RawMessage(fmt.Sprintf(`{"uid":"u","by":"kube:admin","pad":%q}`, pad(size)))
	}
	for i := range 50 {
		admin("workbenches", fmt.Sprintf("admin-%d/nightly-workbench", i), 150)
		admin("playgrounds", fmt.Sprintf("admin-%d", i), 300)
		admin("models", fmt.Sprintf("admin-%d/qwen3-1-7b", i), 150)
		admin("pipelineServers", fmt.Sprintf("admin-%d/dspa", i), 200)
	}
	extra := map[string]string{"components": pad(120 << 10), "maas": pad(40 << 10), "subscription": pad(4 << 10)}
	store := func() {
		out := map[string]string{}
		for k, v := range extra {
			out[k] = v
		}
		for k, entries := range data {
			raw, _ := json.Marshal(entries)
			out[k] = string(raw)
		}
		cm, _ := json.Marshal(map[string]any{"metadata": map[string]any{"name": record.SnapshotConfigMapName}, "data": out})
		f.putJSON(path, string(cm))
	}
	store()
	keys := map[string]string{"workbenches": "/nightly-workbench", "playgrounds": "", "projectStorages": "/nightly-s3", "models": "/qwen3-1-7b"}
	full := map[string]bool{}
	for i := 0; len(full) < len(keys); i++ {
		for key, suffix := range keys {
			if full[key] {
				continue
			}
			project := fmt.Sprintf("gone-%s-%d", key, i)
			u := userClientOf(c, fakeUserToken)
			u.SetUsername(fmt.Sprintf("user-%s-%d", key, i))
			u.SetProjectUser("uid-" + project)
			entry := []byte(fmt.Sprintf(`{"uid":"u","by":"user-%s-%d","actorRole":"user","projectUID":"uid-%s","pad":%q}`, key, i, project, pad(850)))
			var refused *record.RecordsFullError
			switch err := record.AdmitUserEntryNamed(u, key, project+suffix, project, nil, entry); {
			case errors.As(err, &refused):
				full[key] = true
			case err != nil:
				t.Fatal(err)
			default:
				if data[key] == nil {
					data[key] = map[string]json.RawMessage{}
				}
				data[key][project+suffix] = entry
				store()
			}
		}
	}
	owners, err := record.ReadRecordOwners(c)
	if err != nil {
		t.Fatal(err)
	}
	users := 0
	for _, o := range owners {
		if o.ActorRole == record.ActorRoleUser {
			users += o.Bytes
		}
	}
	if users > record.MaxUserRecordBytes {
		t.Errorf("users' entries take %d bytes, more than the budget %d", users, record.MaxUserRecordBytes)
	}
	cm, _ := json.Marshal(f.get(path))
	if len(cm) >= 1<<20 {
		t.Errorf("the snapshot ConfigMap takes %d bytes, 1 MiB or more", len(cm))
	}
	t.Logf("users' entries %d KiB, ConfigMap %d KiB", users>>10, len(cm)>>10)
}

// Two pods write the first record when no snapshot ConfigMap exists yet:
// the other pod creates it between this pod's read and its write. This
// pod's create-only request is refused (AlreadyExists), and the change
// runs again from a fresh read and merges: both entries stay.
func TestSnapshotFirstWritesFromTwoPodsKeepBothEntries(t *testing.T) {
	f, c := newResourceFake(t)
	m := serving.ModelCatalog[0].Name
	path := "/api/v1/namespaces/" + record.GetActivityNamespace() + "/configmaps/" + record.SnapshotConfigMapName
	collection := "/api/v1/namespaces/" + record.GetActivityNamespace() + "/configmaps"
	interleaved, creates := false, 0
	f.beforeServe = func(method, p string) {
		if method != http.MethodPost || p != collection {
			return
		}
		creates++
		if interleaved {
			return
		}
		interleaved = true
		// The other pod's create, with its own entry.
		putModelRecord(t, f, serving.ModelsRecord{serving.ModelRecordKey("other-pod", m): {UID: "b"}})
	}
	err := record.UpdateSnapshot(c, func(tc *Client) error {
		rec, err := serving.ReadModelsRecord(tc)
		if err != nil {
			return err
		}
		rec[serving.ModelRecordKey("this-pod", m)] = serving.RecordedModel{UID: "c"}
		return serving.WriteModelsRecord(tc, rec)
	})
	if err != nil {
		t.Fatal(err)
	}
	rec, _ := serving.ReadModelsRecord(c)
	for _, p := range []string{"other-pod", "this-pod"} {
		if _, ok := rec[serving.ModelRecordKey(p, m)]; !ok {
			t.Errorf("%s's entry is lost: %v", p, rec)
		}
	}
	if creates != 1 || f.get(path) == nil {
		t.Errorf("creates %d", creates)
	}
}

// A sweep that conflicts with another pod's write reports only what its
// successful attempt removed: here the other pod already removed the gone
// project's entry, so this sweep removed nothing and writes nothing more.
func TestSweepRetryReportsOnlyItsOwnRemovals(t *testing.T) {
	f, c := newResourceFake(t)
	m := serving.ModelCatalog[0].Name
	putModelRecord(t, f, serving.ModelsRecord{serving.ModelRecordKey("gone-a", m): {UID: "a"}, serving.ModelRecordKey("live", m): {UID: "l"}})
	path := "/api/v1/namespaces/" + record.GetActivityNamespace() + "/configmaps/" + record.SnapshotConfigMapName
	patches := 0
	f.beforeServe = func(method, p string) {
		if method != http.MethodPatch || p != path {
			return
		}
		patches++
		if patches == 1 {
			cm := f.get(path)
			raw, _ := json.Marshal(serving.ModelsRecord{serving.ModelRecordKey("live", m): {UID: "l"}})
			cm["data"].(map[string]any)[serving.ModelsRecordKey] = string(raw)
			f.bumpRV(cm["metadata"].(map[string]any))
		}
	}
	dropped, err := serving.ForgetGoneModels(c, func(project, _ string, _ serving.RecordedModel) bool { return project == "gone-a" })
	if err != nil {
		t.Fatal(err)
	}
	if len(dropped) != 0 || patches != 1 {
		t.Errorf("dropped %v after %d writes, want none after the one that conflicted", dropped, patches)
	}
}

// A create whose answer does not say the ConfigMap's resourceVersion: the
// attempt's next write is not sent (neither unconditional nor on a made-up
// version); the change runs again from a fresh read, and its writes then
// carry the version read.
func TestSnapshotCreateWithoutVersionRestartsBeforeWritingAgain(t *testing.T) {
	f, c := newResourceFake(t)
	collection := "/api/v1/namespaces/" + record.GetActivityNamespace() + "/configmaps"
	swallowed := false
	f.intercept = func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		if swallowed || r.Method != http.MethodPost || r.URL.Path != collection {
			return false
		}
		swallowed = true
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
		return true
	}
	runs := 0
	err := record.UpdateSnapshot(c, func(tc *Client) error {
		runs++
		if _, err := record.ReadSnapshotKey(tc, "first"); err != nil {
			return err
		}
		if err := record.WriteSnapshotKey(tc, "first", "test-first", "1"); err != nil {
			return err
		}
		return record.WriteSnapshotKey(tc, "second", "test-second", "2")
	})
	if err != nil {
		t.Fatal(err)
	}
	if runs != 2 {
		t.Errorf("the change ran %d times, want again after the create without a version", runs)
	}
	for _, call := range f.calls {
		if call.Method == http.MethodPatch && strings.HasSuffix(call.Path, "/"+record.SnapshotConfigMapName) && !strings.Contains(call.Body, "resourceVersion") {
			t.Errorf("an unconditional apply was sent: %s", call.Body)
		}
		if strings.Contains(call.Body, `"resourceVersion":"0"`) {
			t.Errorf("an apply on a made-up version was sent: %s", call.Body)
		}
	}
	for _, key := range []string{"first", "second"} {
		if v, _ := record.ReadSnapshotKey(c, key); v == "" {
			t.Errorf("key %s is missing", key)
		}
	}
}

// A sweep whose change fails reports no removals.
func TestFailedSweepReportsNoRemovals(t *testing.T) {
	f, c := newResourceFake(t)
	m := serving.ModelCatalog[0].Name
	putModelRecord(t, f, serving.ModelsRecord{serving.ModelRecordKey("gone-a", m): {UID: "a"}})
	f.fail["PATCH /api/v1/namespaces/"+record.GetActivityNamespace()+"/configmaps/"+record.SnapshotConfigMapName] = http.StatusInternalServerError
	dropped, err := serving.ForgetGoneModels(c, func(string, string, serving.RecordedModel) bool { return true })
	if err == nil || dropped != nil {
		t.Fatalf("dropped %v, err %v", dropped, err)
	}
}
