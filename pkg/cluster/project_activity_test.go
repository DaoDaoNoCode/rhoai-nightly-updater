package cluster

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The activity entries of the non-admin design (B4): scope and actorRole,
// the users' bucket, and a cancelled action's reason.

func adminEntries() []types.ActivityEntry {
	var out []types.ActivityEntry
	for i := range 10 {
		out = append(out, types.ActivityEntry{User: "root", Action: "update", Detail: fmt.Sprintf("admin %d", i)})
	}
	return out
}

func userEntry(user string, i int, detail string) types.ActivityEntry {
	return types.ActivityEntry{User: user, Action: "setup-workbench", Detail: fmt.Sprintf("%s %d %s", user, i, detail), ActorRole: record.ActorRoleUser,
		Scope: &types.ActivityScope{Kind: types.ActivityScopeProject, Project: user + "-project", ProjectUID: "uid-" + user}}
}

// Many users' entries, also large ones that push the log over its byte
// bound, never evict a cluster-admin's entry: the users' bucket is trimmed
// first.
func TestUserEntriesCannotEvictAdminEntries(t *testing.T) {
	admin := adminEntries()
	entries := append([]types.ActivityEntry{}, admin...)
	big := strings.Repeat("x", 16<<10) // 40 of these alone are over MaxActivityBytes
	for i := range 500 {
		entries = append(entries, userEntry(fmt.Sprintf("u%d", i%100), i, big))
	}
	got := record.TrimActivity(entries)
	kept := 0
	users := 0
	for _, e := range got {
		if e.ActorRole == record.ActorRoleUser {
			users++
			continue
		}
		kept++
	}
	if kept != len(admin) {
		t.Fatalf("kept %d of %d admin entries", kept, len(admin))
	}
	if users == 0 || users >= record.MaxUserActivityEntries {
		t.Fatalf("kept %d user entries; the byte bound should have trimmed them", users)
	}
	if data, _ := json.Marshal(got); len(data) > record.MaxActivityBytes {
		t.Fatalf("log of %d bytes", len(data))
	}
	// The newest users' entries are the ones kept.
	if last := got[len(got)-1]; !strings.HasPrefix(last.Detail, "u99 499 ") {
		t.Fatalf("newest entry dropped: %q", last.Detail[:20])
	}
	// The users' entries do not count in the admins' categories either.
	for range 100 {
		got = record.TrimActivity(append(got, userEntry("flood", 0, "")))
	}
	if n := countAdmin(got); n != len(admin) {
		t.Fatalf("a flood of one user's entries left %d admin entries", n)
	}
}

func countAdmin(entries []types.ActivityEntry) int {
	n := 0
	for _, e := range entries {
		if e.ActorRole != record.ActorRoleUser {
			n++
		}
	}
	return n
}

// The users' bucket keeps at most 40 entries, at most 5 of one user, the
// newest.
func TestUserBucketCapsTotalAndPerUser(t *testing.T) {
	var entries []types.ActivityEntry
	for i := range 10 {
		for u := range 20 {
			entries = append(entries, userEntry(fmt.Sprintf("u%02d", u), i, ""))
		}
	}
	entries = append(entries, adminEntries()...)
	got := record.TrimActivity(entries)
	per := map[string][]string{}
	total := 0
	for _, e := range got {
		if e.ActorRole == record.ActorRoleUser {
			total++
			per[e.User] = append(per[e.User], e.Detail)
		}
	}
	if total != record.MaxUserActivityEntries {
		t.Fatalf("kept %d user entries", total)
	}
	for user, details := range per {
		if len(details) > record.MaxUserActivityEntriesPerUser {
			t.Fatalf("%s: %d entries", user, len(details))
		}
		// The newest of theirs: rounds 9 down.
		if !strings.HasPrefix(details[len(details)-1], user+" 9 ") {
			t.Fatalf("%s: newest dropped: %v", user, details)
		}
	}
	// One user alone: 5.
	var one []types.ActivityEntry
	for i := range 30 {
		one = append(one, userEntry("solo", i, ""))
	}
	if got := record.TrimActivity(one); len(got) != record.MaxUserActivityEntriesPerUser || got[0].Detail != "solo 25 " {
		t.Fatalf("one user's entries: %d, oldest %q", len(got), got[0].Detail)
	}
	if countAdmin(record.TrimActivity(entries)) != len(adminEntries()) {
		t.Fatal("admin entries lost")
	}
}

// An entry written before entries had a scope and an actorRole reads as a
// cluster-admin's cluster action, and counts as one in the retention; the
// fields round-trip, and an old reader ignores them.
func TestOldEntriesReadAsClusterAdmin(t *testing.T) {
	var old []types.ActivityEntry
	if err := json.Unmarshal([]byte(`[{"timestamp":"2026-10-01T10:00:00Z","user":"alice","action":"update","detail":"x","success":true}]`), &old); err != nil {
		t.Fatal(err)
	}
	if old[0].Scope != nil || old[0].ActorRole != "" {
		t.Fatalf("decoded %+v", old[0])
	}
	e := old[0]
	record.DescribeActivity(&e)
	if e.Scope == nil || e.Scope.Kind != types.ActivityScopeCluster || e.ActorRole != record.ActorRoleAdmin {
		t.Fatalf("an old entry reads as %+v %q", e.Scope, e.ActorRole)
	}
	// Retention: an old entry is a cluster-admin's (not in the users' bucket).
	var entries []types.ActivityEntry
	entries = append(entries, old...)
	for i := range 60 {
		entries = append(entries, userEntry("bob", i, ""))
	}
	if got := record.TrimActivity(entries); got[0].Detail != "x" {
		t.Fatal("an old entry was trimmed as a user's")
	}
	// A new entry keeps its fields; the JSON of an old one is unchanged.
	raw, _ := json.Marshal(userEntry("bob", 1, ""))
	if !strings.Contains(string(raw), `"scope":{"kind":"project","project":"bob-project","projectUID":"uid-bob"}`) || !strings.Contains(string(raw), `"actorRole":"user"`) {
		t.Fatalf("new entry JSON: %s", raw)
	}
	raw, _ = json.Marshal(old[0])
	if strings.Contains(string(raw), "scope") || strings.Contains(string(raw), "actorRole") {
		t.Fatalf("old entry JSON grew fields: %s", raw)
	}
}

// storedActivity is the activity log the fake holds.
func storedActivity(t *testing.T, f *resourceFake) []types.ActivityEntry {
	t.Helper()
	cm := f.get("/api/v1/namespaces/" + record.GetActivityNamespace() + "/configmaps/" + record.ActivityConfigMapName)
	if cm == nil {
		return nil
	}
	raw, _ := cm["data"].(map[string]any)["entries"].(string)
	var out []types.ActivityEntry
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// Every new entry stores its scope and actorRole from the client that
// wrote it: a project action's (SetProjectScope; a user's: SetProjectUser)
// or a cluster operation's.
func TestActivityEntriesStoreScopeAndRole(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mark      func(u *Client)
		wantScope types.ActivityScope
		wantRole  string
	}{
		{"cluster operation", func(*Client) {}, types.ActivityScope{Kind: "cluster"}, record.ActorRoleAdmin},
		{"admin's project action", func(u *Client) { scopeTo(u, "proj", "uid-proj") }, types.ActivityScope{Kind: "project", Project: "proj", ProjectUID: "uid-proj"}, record.ActorRoleAdmin},
		{"user's project action", func(u *Client) { scopeTo(u, "proj", "uid-proj"); u.SetProjectUser("uid-proj") }, types.ActivityScope{Kind: "project", Project: "proj", ProjectUID: "uid-proj"}, record.ActorRoleUser},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newResourceFake(t)
			u := userClientOf(c, fakeUserToken)
			u.SetUsername("alice")
			tc.mark(u)
			record.Action(u, "setup-workbench", "workbench in proj", true)
			got := storedActivity(t, f)
			if len(got) != 1 || got[0].Scope == nil || *got[0].Scope != tc.wantScope || got[0].ActorRole != tc.wantRole || got[0].Reason != "" {
				t.Fatalf("stored %+v", got)
			}
		})
	}
}

// scopeTo gives u's context the scope of a project action (as pkg/api
// withProjectMutationAuth does).
func scopeTo(u *Client, project, uid string) {
	*u = *u.WithContext(WithActionScope(u.Context(), project, uid))
}
