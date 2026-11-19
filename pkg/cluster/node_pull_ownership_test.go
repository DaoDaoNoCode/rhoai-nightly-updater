package cluster

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Whose the quay.io/rhoai entry of openshift-config/pull-secret is
// (node_pull_record.go): the tool removes, or replaces without asking,
// only the entry it recorded adding.

const nsPullSecretUID = "pull-secret-uid"

// nsAllCredentials are every credential of the fixtures, in every form.
var nsAllCredentials = []string{
	nsToken, nsTokenPadded, "rhoai+robot:upd4ter-s3cret", "upd4ter-s3cret",
	nsCloudAuth, "cl0ud-s3cret", nsRedHatAuth, "r3dhat-s3cret",
	nsOldRHOAI, "old-robot:0ld-s3cret", "0ld-s3cret",
}

// lockedBuffer is a bytes.Buffer for slog, which may write from several
// goroutines.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// nsLogs is what the package logged during the current test (set by
// nodeSetupFake).
var nsLogs *lockedBuffer

func captureNodeSetupLogs(t *testing.T) {
	t.Helper()
	nsLogs = &lockedBuffer{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(nsLogs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
}

// assertNoCredentials fails when a credential of the fixtures is in a log
// line, in one of outputs (plans, results, step events, as JSON), in one of
// the updater's ConfigMaps (its records and activity log) or in a request
// sent to them.
func assertNoCredentials(t *testing.T, f *resourceFake, outputs ...any) {
	t.Helper()
	var where []string
	var texts []string
	if nsLogs != nil {
		where, texts = append(where, "the log"), append(texts, nsLogs.String())
	}
	for i, o := range outputs {
		raw, _ := json.Marshal(o)
		where, texts = append(where, "output "+string(rune('0'+i))), append(texts, string(raw))
	}
	f.mu.Lock()
	for k, obj := range f.objects {
		if k.plural == "configmaps" {
			raw, _ := json.Marshal(obj)
			where, texts = append(where, "ConfigMap "+k.name), append(texts, string(raw))
		}
	}
	for _, call := range f.calls {
		if strings.Contains(call.Path, "/configmaps") {
			where, texts = append(where, call.Method+" "+call.Path), append(texts, call.Body)
		}
	}
	f.mu.Unlock()
	for i, text := range texts {
		for _, cred := range nsAllCredentials {
			if strings.Contains(text, cred) {
				t.Errorf("%s carries the credential %q: %s", where[i], cred, text)
			}
		}
	}
}

// nsSetPullSecret makes openshift-config/pull-secret the cluster's two
// logins and extra, with a fixed UID and resourceVersion 42.
func nsSetPullSecret(t *testing.T, f *resourceFake, extra map[string]any) {
	t.Helper()
	f.put(nsPullSecretPath, map[string]any{"type": "kubernetes.io/dockerconfigjson",
		"metadata": map[string]any{"resourceVersion": "42", "uid": nsPullSecretUID},
		"data":     map[string]any{".dockerconfigjson": nsDockerConfig(t, map[string]any{"auths": clusterLogins(extra)})},
	})
}

// nsPutRecord stores the tool's record of having written auth under key
// into the Secret with uid.
func nsPutRecord(t *testing.T, f *resourceFake, uid, key, auth string) {
	t.Helper()
	rec := nodePullRecord{SecretUID: uid, Key: key, Salt: "5a17", AuthSHA256: hashPullAuth("5a17", auth), At: "2026-11-18T10:00:00Z", By: "admin"}
	raw, _ := json.Marshal(rec)
	f.put(testSnapshotCM, map[string]any{"data": map[string]any{nodePullRecordKey: string(raw), "snapshot": `{"images":{}}`}})
}

// nsReadRecord reads the tool's record from the fake (nil when none).
func nsReadRecord(t *testing.T, f *resourceFake) *nodePullRecord {
	t.Helper()
	cm := f.get(testSnapshotCM)
	data, _ := cm["data"].(map[string]any)
	raw, _ := data[nodePullRecordKey].(string)
	if raw == "" {
		return nil
	}
	var rec nodePullRecord
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		t.Fatal(err)
	}
	return &rec
}

func nsPlan(t *testing.T, c *Client, req types.NodeSetupRequest) *types.NodeSetupPlan {
	t.Helper()
	plan, err := PlanNodeSetup(c, req)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

// Add on a cluster pull secret with a quay.io/rhoai entry this tool did not
// add, with another token: refused before any write (not even a dry run),
// whatever the reason the entry is not the tool's; the plan names the
// entry's key only and offers the replacement behind the acknowledgement.
func TestPullSecretAddLeavesAnEntryItDidNotAdd(t *testing.T) {
	for _, tc := range []struct {
		name   string
		key    string
		record func(t *testing.T, f *resourceFake)
		why    string
	}{
		{"a provisioner's entry, no record", "quay.io/rhoai", func(*testing.T, *resourceFake) {}, "this tool has no record of adding it"},
		{"an entry with a scheme, no record", "https://quay.io/rhoai/", func(*testing.T, *resourceFake) {}, "this tool has no record of adding it"},
		{"the Secret was made again", "quay.io/rhoai", func(t *testing.T, f *resourceFake) {
			nsPutRecord(t, f, "an-earlier-uid", "quay.io/rhoai", nsOldRHOAI)
		}, "was made again since this tool added its entry"},
		{"the tool's entry got another token since", "quay.io/rhoai", func(t *testing.T, f *resourceFake) {
			nsPutRecord(t, f, nsPullSecretUID, "quay.io/rhoai", base64.StdEncoding.EncodeToString([]byte("rhoai+robot:what-the-tool-wrote")))
		}, "its token was changed since this tool added it"},
		{"the tool's entry is gone, another is there", "https://quay.io/rhoai/", func(t *testing.T, f *resourceFake) {
			nsPutRecord(t, f, nsPullSecretUID, "quay.io/rhoai", nsOldRHOAI)
		}, "the entry this tool added is gone"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := nodeSetupFake(t, "HighlyAvailable", false)
			captureNodeSetupLogs(t)
			nsSetPullSecret(t, f, map[string]any{tc.key: map[string]any{"auth": nsOldRHOAI}})
			tc.record(t, f)
			before := nsReadRecord(t, f)

			plan := nsPlan(t, c, types.NodeSetupRequest{Action: NodeSetupPullAdd})
			if plan.Ready || plan.RefusalCode != "foreign_entry" || !plan.Replaceable || plan.EntrySetBy != EntrySetByOther ||
				!slices.Equal(plan.ExistingKeys, []string{tc.key}) || !strings.Contains(plan.Refusal, "that this tool did not add") ||
				!strings.Contains(plan.Refusal, tc.why) || !strings.Contains(plan.Refusal, "Check that the nodes can pull") ||
				!strings.Contains(plan.Refusal, "Nothing was changed") {
				t.Fatalf("plan %+v", plan)
			}
			res, steps := runNodeSetupCollect(t, c, types.NodeSetupRequest{Action: NodeSetupPullAdd, ResourceVersion: "42"})
			if res.Success || res.ErrorCode != "foreign_entry" {
				t.Fatalf("result %+v", res)
			}
			if w := writes(f); len(w) != 0 {
				t.Fatalf("writes %v", w)
			}
			if calls := f.callsTo(http.MethodPatch, nsPullSecretPath); len(calls) != 0 {
				t.Fatalf("not even a dry run may be sent: %+v", calls)
			}
			if authFromEntry(pullSecretAuths(t, f)[tc.key]) != nsOldRHOAI {
				t.Fatal("the entry changed")
			}
			if after := nsReadRecord(t, f); !jsonEqual(before, after) {
				t.Fatalf("record %+v, was %+v", after, before)
			}
			assertNoCredentials(t, f, plan, res, steps)
		})
	}
}

// With the acknowledgement, Add replaces an entry this tool did not add,
// says that its token is not kept and cannot be restored, and records the
// new entry as the tool's, which Remove then may take out.
func TestPullSecretAddReplacesAForeignEntryOnlyWhenAcknowledged(t *testing.T) {
	f, c := nodeSetupFake(t, "HighlyAvailable", false)
	captureNodeSetupLogs(t)
	nsSetPullSecret(t, f, map[string]any{"https://quay.io/rhoai/": map[string]any{"auth": nsOldRHOAI}})

	plan := nsPlan(t, c, types.NodeSetupRequest{Action: NodeSetupPullAdd, ReplaceExisting: true})
	if !plan.Ready || !plan.Replacing || plan.EntrySetBy != EntrySetByOther || len(plan.Changes) != 1 ||
		!strings.Contains(plan.Changes[0], "replace its quay.io/rhoai entry (https://quay.io/rhoai/), which this tool did not add") ||
		!strings.Contains(plan.Undo, "is not kept, and this tool cannot restore it") {
		t.Fatalf("plan %+v", plan)
	}
	if len(writes(f)) != 0 || nsReadRecord(t, f) != nil {
		t.Fatal("the plan changed something")
	}
	res, steps := runNodeSetupCollect(t, c, types.NodeSetupRequest{Action: NodeSetupPullAdd, ResourceVersion: "42", ReplaceExisting: true})
	if !res.Success || !strings.Contains(res.Message, "which this tool did not add") {
		t.Fatalf("result %+v", res)
	}
	auths := pullSecretAuths(t, f)
	if _, old := auths["https://quay.io/rhoai/"]; old || len(auths) != 3 || authFromEntry(auths["quay.io/rhoai"]) != nsTokenPadded {
		t.Fatalf("auths %v", auths)
	}
	rec := nsReadRecord(t, f)
	if rec == nil || rec.SecretUID != nsPullSecretUID || rec.Key != "quay.io/rhoai" || !rec.matches(nsTokenPadded) || rec.matches(nsOldRHOAI) {
		t.Fatalf("record %+v", rec)
	}
	if remove := nsPlan(t, c, types.NodeSetupRequest{Action: NodeSetupPullRemove}); !remove.Ready || remove.EntrySetBy != EntrySetByTool {
		t.Fatalf("remove plan %+v", remove)
	}
	assertNoCredentials(t, f, plan, res, steps)
}

// An entry that already has the updater's token, which the tool did not
// add: nothing to do, and the tool does not claim it (no record), so
// Remove leaves it.
func TestPullSecretAddLeavesAnEqualEntryUnclaimed(t *testing.T) {
	for _, existing := range []string{nsTokenPadded, nsToken} {
		f, c := nodeSetupFake(t, "HighlyAvailable", false)
		captureNodeSetupLogs(t)
		// The unpadded form is the same token only once padded: CRI-O
		// needs the padding, so it is not counted as the same.
		nsSetPullSecret(t, f, map[string]any{"quay.io/rhoai": map[string]any{"auth": existing}})
		plan := nsPlan(t, c, types.NodeSetupRequest{Action: NodeSetupPullAdd})
		if existing == nsToken {
			if plan.RefusalCode != "foreign_entry" {
				t.Fatalf("unpadded: plan %+v", plan)
			}
			continue
		}
		if !plan.NothingToDo || plan.Ready || plan.EntrySetBy != EntrySetByOther || !strings.Contains(plan.State, "This tool did not add it") {
			t.Fatalf("plan %+v", plan)
		}
		res, steps := runNodeSetupCollect(t, c, types.NodeSetupRequest{Action: NodeSetupPullAdd, ResourceVersion: "42"})
		if !res.Success || !strings.HasPrefix(res.Message, "Nothing to change") || len(writes(f)) != 0 || f.has(testSnapshotCM) {
			t.Fatalf("result %+v, writes %v, record written %v", res, writes(f), f.has(testSnapshotCM))
		}
		if remove := nsPlan(t, c, types.NodeSetupRequest{Action: NodeSetupPullRemove}); remove.Ready || remove.RefusalCode != "not_owned" {
			t.Fatalf("remove plan %+v", remove)
		}
		assertNoCredentials(t, f, plan, res, steps)
	}
}

// The tool's own entry, and the token saved under Pull secret changed
// since: Add updates it without an acknowledgement and records the new
// token.
func TestPullSecretAddUpdatesItsOwnEntry(t *testing.T) {
	f, c := nodeSetupFake(t, "HighlyAvailable", false)
	captureNodeSetupLogs(t)
	nsSetPullSecret(t, f, map[string]any{"quay.io/rhoai": map[string]any{"auth": nsOldRHOAI}})
	nsPutRecord(t, f, nsPullSecretUID, "quay.io/rhoai", nsOldRHOAI)

	plan := nsPlan(t, c, types.NodeSetupRequest{Action: NodeSetupPullAdd})
	if !plan.Ready || plan.Replaceable || plan.Replacing || plan.EntrySetBy != EntrySetByTool ||
		!strings.Contains(plan.State, "this tool added on 2026-11-18T10:00:00Z by admin, with an earlier token") ||
		!strings.Contains(plan.Changes[0], "update the quay.io/rhoai entry this tool added") {
		t.Fatalf("plan %+v", plan)
	}
	res, steps := runNodeSetupCollect(t, c, types.NodeSetupRequest{Action: NodeSetupPullAdd, ResourceVersion: "42"})
	if !res.Success || !strings.Contains(res.Message, "Updated the quay.io/rhoai entry this tool added") {
		t.Fatalf("result %+v", res)
	}
	if got := authFromEntry(pullSecretAuths(t, f)["quay.io/rhoai"]); got != nsTokenPadded {
		t.Fatal("the entry was not updated")
	}
	if rec := nsReadRecord(t, f); rec == nil || !rec.matches(nsTokenPadded) || rec.matches(nsOldRHOAI) || rec.SecretUID != nsPullSecretUID {
		t.Fatalf("record %+v", rec)
	}
	// The record's own write keeps the other keys of the snapshot ConfigMap.
	if data, _ := f.get(testSnapshotCM)["data"].(map[string]any); data["snapshot"] == nil {
		t.Fatalf("the snapshot was lost: %v", data)
	}
	assertNoCredentials(t, f, plan, res, steps)
}

// Remove takes out only the entry the tool added: another quay.io/rhoai
// entry and every other login stay, and the record is cleared.
func TestPullSecretRemoveTakesOutOnlyItsOwnEntry(t *testing.T) {
	f, c := nodeSetupFake(t, "HighlyAvailable", false)
	captureNodeSetupLogs(t)
	nsSetPullSecret(t, f, map[string]any{
		"quay.io/rhoai":           map[string]any{"auth": nsTokenPadded},
		"https://quay.io/rhoai/":  map[string]any{"auth": nsOldRHOAI},
		"quay.io/rhoai/other-app": map[string]any{"auth": nsOldRHOAI},
	})
	nsPutRecord(t, f, nsPullSecretUID, "quay.io/rhoai", nsTokenPadded)

	plan := nsPlan(t, c, types.NodeSetupRequest{Action: NodeSetupPullRemove})
	if !plan.Ready || plan.EntrySetBy != EntrySetByOther || !strings.Contains(plan.State, "this tool added on 2026-11-18T10:00:00Z by admin") ||
		!strings.Contains(plan.State, "https://quay.io/rhoai/, which this tool did not add, stays") ||
		!strings.Contains(plan.Changes[0], "remove the quay.io/rhoai entry this tool added; the other 4 registry logins stay") ||
		!strings.Contains(plan.NodeImpact, "images already on a node stay") {
		t.Fatalf("plan %+v", plan)
	}
	res, steps := runNodeSetupCollect(t, c, types.NodeSetupRequest{Action: NodeSetupPullRemove, ResourceVersion: "42"})
	if !res.Success {
		t.Fatalf("result %+v", res)
	}
	auths := pullSecretAuths(t, f)
	if _, ok := auths["quay.io/rhoai"]; ok || len(auths) != 4 || authFromEntry(auths["https://quay.io/rhoai/"]) != nsOldRHOAI || auths["quay.io/rhoai/other-app"] == nil {
		t.Fatalf("auths %v: only the tool's entry goes", auths)
	}
	if rec := nsReadRecord(t, f); rec != nil {
		t.Fatalf("the record was not cleared: %+v", rec)
	}
	// What stays is not the tool's: a second Remove refuses.
	if again := nsPlan(t, c, types.NodeSetupRequest{Action: NodeSetupPullRemove}); again.Ready || again.RefusalCode != "not_owned" {
		t.Fatalf("second plan %+v", again)
	}
	assertNoCredentials(t, f, plan, res, steps)
}

// Remove refuses, before any write, every entry that is not the one the
// tool recorded, and says how to remove it by hand.
func TestPullSecretRemoveRefusesAnEntryItDidNotAdd(t *testing.T) {
	for _, tc := range []struct {
		name   string
		key    string
		record func(t *testing.T, f *resourceFake)
		why    string
	}{
		{"no record (a provisioner's entry)", "quay.io/rhoai", func(*testing.T, *resourceFake) {}, "this tool has no record of adding it"},
		{"the Secret was made again", "quay.io/rhoai", func(t *testing.T, f *resourceFake) {
			nsPutRecord(t, f, "an-earlier-uid", "quay.io/rhoai", nsOldRHOAI)
		}, "was made again"},
		{"the token was changed since", "quay.io/rhoai", func(t *testing.T, f *resourceFake) {
			nsPutRecord(t, f, nsPullSecretUID, "quay.io/rhoai", nsTokenPadded)
		}, "its token was changed since this tool added it"},
		{"the tool's entry is gone, another is there", "https://quay.io/rhoai/", func(t *testing.T, f *resourceFake) {
			nsPutRecord(t, f, nsPullSecretUID, "quay.io/rhoai", nsOldRHOAI)
		}, "the entry this tool added is gone"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := nodeSetupFake(t, "HighlyAvailable", false)
			captureNodeSetupLogs(t)
			nsSetPullSecret(t, f, map[string]any{tc.key: map[string]any{"auth": nsOldRHOAI}})
			tc.record(t, f)
			before := nsReadRecord(t, f)
			plan := nsPlan(t, c, types.NodeSetupRequest{Action: NodeSetupPullRemove})
			if plan.Ready || plan.RefusalCode != "not_owned" || plan.EntrySetBy != EntrySetByOther ||
				!strings.HasPrefix(plan.Refusal, "The quay.io/rhoai entry in openshift-config/pull-secret was not added by this tool (or was changed since), so it is not removed here") ||
				!strings.Contains(plan.Refusal, tc.why) || !strings.Contains(plan.Refusal, "removal command under Do it yourself instead") {
				t.Fatalf("plan %+v", plan)
			}
			res, steps := runNodeSetupCollect(t, c, types.NodeSetupRequest{Action: NodeSetupPullRemove, ResourceVersion: "42"})
			if res.Success || res.ErrorCode != "not_owned" || len(writes(f)) != 0 || len(f.callsTo(http.MethodPatch, nsPullSecretPath)) != 0 {
				t.Fatalf("result %+v, writes %v", res, writes(f))
			}
			if authFromEntry(pullSecretAuths(t, f)[tc.key]) != nsOldRHOAI {
				t.Fatal("the entry changed")
			}
			if after := nsReadRecord(t, f); !jsonEqual(before, after) {
				t.Fatalf("record %+v, was %+v", after, before)
			}
			assertNoCredentials(t, f, plan, res, steps)
		})
	}
}

// Without the record the tool cannot tell whose the entry is: both actions
// refuse before any write.
func TestPullSecretOwnershipFailsClosedWithoutTheRecord(t *testing.T) {
	for _, action := range []string{NodeSetupPullAdd, NodeSetupPullRemove} {
		for _, entry := range []map[string]any{nil, {"quay.io/rhoai": map[string]any{"auth": nsOldRHOAI}}} {
			f, c := nodeSetupFake(t, "HighlyAvailable", false)
			nsSetPullSecret(t, f, entry)
			nsPutRecord(t, f, nsPullSecretUID, "quay.io/rhoai", nsOldRHOAI)
			f.fail["GET "+testSnapshotCM] = 403
			plan := nsPlan(t, c, types.NodeSetupRequest{Action: action})
			if plan.Ready || plan.NothingToDo || plan.RefusalCode != "unreadable" || !strings.Contains(plan.Refusal, "cannot tell who set the quay.io/rhoai entry") {
				t.Fatalf("%s, entry %v: plan %+v", action, entry, plan)
			}
			res, _ := runNodeSetupCollect(t, c, types.NodeSetupRequest{Action: action, ResourceVersion: "42"})
			if res.Success || len(writes(f)) != 0 || len(f.callsTo(http.MethodPatch, nsPullSecretPath)) != 0 {
				t.Fatalf("%s: result %+v, writes %v", action, res, writes(f))
			}
		}
	}
}

// The record cannot be written after Add: the change stands, and the
// outcome says the tool will not remove the entry.
func TestPullSecretAddSaysWhenItCannotRecordTheEntry(t *testing.T) {
	f, c := nodeSetupFake(t, "HighlyAvailable", false)
	f.fail["PATCH "+testSnapshotCM] = 403
	res, _ := runNodeSetupCollect(t, c, types.NodeSetupRequest{Action: NodeSetupPullAdd, ResourceVersion: "42"})
	if !res.Success || !strings.Contains(res.Message, "could not record that it added the entry, so it will not remove it") {
		t.Fatalf("result %+v", res)
	}
	if authFromEntry(pullSecretAuths(t, f)["quay.io/rhoai"]) != nsTokenPadded {
		t.Fatal("the entry was not added")
	}
	if remove := nsPlan(t, c, types.NodeSetupRequest{Action: NodeSetupPullRemove}); remove.RefusalCode != "not_owned" {
		t.Fatalf("remove plan %+v", remove)
	}
}

// The live case (a self-managed OpenShift 4.22 sandbox): its provisioner
// put a quay.io/rhoai entry with its own token into the cluster pull
// secret, and an ImageDigestMirrorSet quay-registry with four mappings,
// one of them registry.redhat.io/rhoai to quay.io/rhoai. The mirror counts
// as set up (Create adds no second one, Remove does not delete it), and
// the entry is neither replaced nor removed.
func TestNodeSetupOnAClusterProvisionedBySomeoneElse(t *testing.T) {
	const quayRegistry = `{"metadata":{"name":"quay-registry"},"spec":{"imageDigestMirrors":[
		{"source":"registry.redhat.io/rhoai","mirrors":["quay.io/rhoai"],"mirrorSourcePolicy":"NeverContactSource"},
		{"source":"registry.redhat.io/rhods","mirrors":["quay.io/rhoai"],"mirrorSourcePolicy":"NeverContactSource"},
		{"source":"registry.stage.redhat.io","mirrors":["quay.io/rhoai"]},
		{"source":"brew.registry.redhat.io/rh-osbs","mirrors":["quay.io/rhoai"]}]}}`
	f, c := nodeSetupFake(t, "HighlyAvailable", false)
	captureNodeSetupLogs(t)
	f.putJSON("/apis/config.openshift.io/v1/imagedigestmirrorsets/quay-registry", quayRegistry)
	nsSetPullSecret(t, f, map[string]any{"quay.io/rhoai": map[string]any{"auth": nsOldRHOAI}})

	// The status reads the same mirror.
	if im, err := getIDMS(c); err != nil || !im.Exists || im.Name != "quay-registry" || im.Source != "registry.redhat.io/rhoai" {
		t.Fatalf("status mirror %+v, %v", im, err)
	}

	var outputs []any
	create := nsPlan(t, c, types.NodeSetupRequest{Action: NodeSetupMirrorCreate})
	if !create.NothingToDo || create.Ready || !strings.Contains(create.State, "ImageDigestMirrorSet quay-registry already redirects registry.redhat.io/rhoai") {
		t.Fatalf("mirror create plan %+v", create)
	}
	res, steps := runNodeSetupCollect(t, c, types.NodeSetupRequest{Action: NodeSetupMirrorCreate})
	if !res.Success || !strings.HasPrefix(res.Message, "Nothing to change") {
		t.Fatalf("mirror create %+v", res)
	}
	outputs = append(outputs, create, res, steps)

	remove := nsPlan(t, c, types.NodeSetupRequest{Action: NodeSetupMirrorRemove})
	if remove.Ready || remove.RefusalCode != "foreign" || !strings.Contains(remove.Refusal, "quay-registry, which this setup did not create") {
		t.Fatalf("mirror remove plan %+v", remove)
	}
	res, steps = runNodeSetupCollect(t, c, types.NodeSetupRequest{Action: NodeSetupMirrorRemove, ResourceVersion: "1"})
	if res.Success {
		t.Fatalf("mirror remove %+v", res)
	}
	outputs = append(outputs, remove, res, steps)

	add := nsPlan(t, c, types.NodeSetupRequest{Action: NodeSetupPullAdd})
	if add.RefusalCode != "foreign_entry" || !add.Replaceable || !slices.Equal(add.ExistingKeys, []string{"quay.io/rhoai"}) {
		t.Fatalf("add plan %+v", add)
	}
	res, steps = runNodeSetupCollect(t, c, types.NodeSetupRequest{Action: NodeSetupPullAdd, ResourceVersion: "42"})
	if res.Success || res.ErrorCode != "foreign_entry" {
		t.Fatalf("add %+v", res)
	}
	outputs = append(outputs, add, res, steps)

	pullRemove := nsPlan(t, c, types.NodeSetupRequest{Action: NodeSetupPullRemove})
	if pullRemove.RefusalCode != "not_owned" {
		t.Fatalf("pull remove plan %+v", pullRemove)
	}
	res, steps = runNodeSetupCollect(t, c, types.NodeSetupRequest{Action: NodeSetupPullRemove, ResourceVersion: "42"})
	if res.Success || res.ErrorCode != "not_owned" {
		t.Fatalf("pull remove %+v", res)
	}
	outputs = append(outputs, pullRemove, res, steps)

	if w := writes(f); len(w) != 0 {
		t.Fatalf("writes %v", w)
	}
	if n := len(f.callsTo(http.MethodPost, nsIDMSList)) + len(f.callsTo(http.MethodPatch, nsPullSecretPath)); n != 0 {
		t.Fatalf("%d writes or dry runs sent", n)
	}
	if !f.has("/apis/config.openshift.io/v1/imagedigestmirrorsets/quay-registry") || f.has(nsIDMSPath) {
		t.Fatal("the mirrors changed")
	}
	assertNoCredentials(t, f, outputs...)
}

// The status says whether the tool recorded adding the entry, and whether
// that entry has the token saved under Pull secret now.
func TestNodePullRecordInfo(t *testing.T) {
	saved := exampleAuth("user:pass")
	padded := paddedAuth(saved)
	for _, tc := range []struct {
		name string
		rec  *nodePullRecord
		want *types.NodePullSecretRecord
	}{
		{"no record", nil, nil},
		{"the saved token", &nodePullRecord{Salt: "aa", AuthSHA256: hashPullAuth("aa", padded), At: "t", By: "u"}, &types.NodePullSecretRecord{At: "t", By: "u", SameToken: true}},
		{"an earlier token", &nodePullRecord{Salt: "aa", AuthSHA256: hashPullAuth("aa", nsOldRHOAI), At: "t", By: "u"}, &types.NodePullSecretRecord{At: "t", By: "u"}},
	} {
		if got := nodePullRecordInfo(tc.rec, saved); !jsonEqual(got, tc.want) {
			t.Errorf("%s: %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// The salt makes two records of one token differ, and neither holds it.
func TestNodePullRecordHoldsNoToken(t *testing.T) {
	_, c := nodeSetupFake(t, "HighlyAvailable", false)
	a, err := newNodePullRecord(c, "u", "quay.io/rhoai", nsTokenPadded)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := newNodePullRecord(c, "u", "quay.io/rhoai", nsTokenPadded)
	if a.Salt == b.Salt || a.AuthSHA256 == b.AuthSHA256 || !a.matches(nsTokenPadded) || !b.matches(nsTokenPadded) || a.matches(nsOldRHOAI) {
		t.Fatalf("records %+v %+v", a, b)
	}
	raw, _ := json.Marshal(a)
	for _, cred := range nsAllCredentials {
		if strings.Contains(string(raw), cred) {
			t.Fatalf("the record holds %q: %s", cred, raw)
		}
	}
	if _, err := newNodePullRecord(c, "", "quay.io/rhoai", nsTokenPadded); err == nil {
		t.Fatal("a record without the Secret's UID")
	}
}
