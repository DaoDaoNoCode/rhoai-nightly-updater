package cluster

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const tOperationCM = "/api/v1/namespaces/rhoai-nightly-updater/configmaps/rhoai-nightly-updater-operation"

// tRecipeRun is an attempt (operation op) of run r1.
func tRecipeRun(op string) *types.RecipeRun {
	return &types.RecipeRun{ID: "r1", OperationID: op, BootID: "boot-1", State: types.RecipeRunRunning,
		Steps: []types.RecipeRunStep{{ID: "rhoai", Title: "RHOAI", Status: types.RecipeStepRunning}}}
}

func storedRecipeRun(t *testing.T, f *resourceFake) (string, map[string]any) {
	t.Helper()
	cm := f.get(tOperationCM)
	if cm == nil {
		t.Fatal("no operation ConfigMap")
	}
	data, _ := cm["data"].(map[string]any)
	raw, _ := data["recipeRun"].(string)
	return raw, data
}

// The record is bounded: a run whose step messages are long is written
// with the messages shortened, under MaxRecipeRunBytes, through the real
// encoder and ConfigMap write.
func TestRecipeRunRecord_Bounded(t *testing.T) {
	t.Setenv("NAMESPACE", "rhoai-nightly-updater")
	f, c := newResourceFake(t)
	run := tRecipeRun("op1")
	for i := range 60 {
		run.Steps = append(run.Steps, types.RecipeRunStep{ID: "s", Status: types.RecipeStepFailed, Message: strings.Repeat("m", 4000+i)})
	}
	if err := record.SaveRecipeRun(c, run, true); err != nil {
		t.Fatal(err)
	}
	raw, _ := storedRecipeRun(t, f)
	if len(raw) == 0 || len(raw) > record.MaxRecipeRunBytes {
		t.Fatalf("stored %d bytes", len(raw))
	}
	got, err := record.ReadRecipeRun(c)
	if err != nil || got.ID != "r1" || len(got.Steps) != 61 || len(got.Steps[1].Message) > 1024 {
		t.Fatalf("read back %v (err %v)", got, err)
	}
}

// A write that conflicts with another writer (the lease heartbeat, the
// marker) is read again and retried, keeping the other writer's keys.
func TestRecipeRunRecord_RetriesAConflict(t *testing.T) {
	t.Setenv("NAMESPACE", "rhoai-nightly-updater")
	f, c := newResourceFake(t)
	if err := record.SaveRecipeRun(c, tRecipeRun("op1"), true); err != nil {
		t.Fatal(err)
	}
	interfered := false
	f.beforeServe = func(method, path string) {
		if method == http.MethodPatch && path == tOperationCM && !interfered {
			interfered = true
			f.mu.Lock()
			cm := f.objects[fakeKey{gv: "v1", ns: "rhoai-nightly-updater", plural: "configmaps", name: "rhoai-nightly-updater-operation"}]
			cm["data"].(map[string]any)["lock"] = `{"id":"heartbeat"}`
			f.bumpRV(cm["metadata"].(map[string]any))
			f.mu.Unlock()
		}
	}
	next := tRecipeRun("op1")
	next.Steps[0].Status = types.RecipeStepSuccess
	if err := record.SaveRecipeRun(c, next, false); err != nil {
		t.Fatal(err)
	}
	if n := len(f.callsTo("PATCH", tOperationCM)); n < 2 {
		t.Fatalf("%d patches: the conflict was not retried", n)
	}
	raw, data := storedRecipeRun(t, f)
	var got types.RecipeRun
	if err := json.Unmarshal([]byte(raw), &got); err != nil || got.Steps[0].Status != types.RecipeStepSuccess || data["lock"] != `{"id":"heartbeat"}` {
		t.Fatalf("stored %s, data %v", raw, data)
	}
}

// A late write of an earlier attempt of a run (same run ID, another
// operation or process) does not overwrite the attempt that resumed it;
// the resumed attempt's own writes go through.
func TestRecipeRunRecord_AttemptsAreIsolated(t *testing.T) {
	t.Setenv("NAMESPACE", "rhoai-nightly-updater")
	f, c := newResourceFake(t)
	if err := record.SaveRecipeRun(c, tRecipeRun("op2"), true); err != nil { // the resumed attempt
		t.Fatal(err)
	}
	stale := tRecipeRun("op1")
	stale.State = types.RecipeRunFailed
	if err := record.SaveRecipeRun(c, stale, false); !errors.Is(err, record.ErrRecipeRunReplaced) {
		t.Fatalf("stale attempt's write: %v", err)
	}
	otherBoot := tRecipeRun("op2")
	otherBoot.BootID = "boot-0"
	if err := record.SaveRecipeRun(c, otherBoot, false); !errors.Is(err, record.ErrRecipeRunReplaced) {
		t.Fatalf("another process's write: %v", err)
	}
	if err := record.SaveRecipeRun(c, tRecipeRun("op2"), false); err != nil {
		t.Fatalf("the attempt's own write: %v", err)
	}
	raw, _ := storedRecipeRun(t, f)
	if !strings.Contains(raw, `"operationId":"op2"`) || strings.Contains(raw, `"state":"failed"`) {
		t.Fatalf("stored %s", raw)
	}
}
