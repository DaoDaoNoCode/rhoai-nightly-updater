package cluster

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
)

const runningCopyJob = `{"metadata":{"uid":"earlier-job","labels":` + toolLabelJSON + `},"spec":{"template":{"spec":{"containers":[{"name":"rclone","env":[{"name":"COPY_MODE","value":"ignore-existing"}]}]}}}}`

// finishJobOnCreate lets a new copy Job complete once it is created.
func finishJobOnCreate(f *resourceFake, sim *migrateSim) {
	next := f.beforeServe
	f.beforeServe = func(method, path string) {
		if method == "POST" && strings.HasSuffix(path, "/jobs") {
			f.mu.Lock()
			sim.jobRunning = false
			f.mu.Unlock()
		}
		next(method, path)
	}
}

// The removal's final copy is always a new Job: one left running by an
// earlier run started copying before objects written since.
func TestRemoveOldStorage_FinalCopyIsAlwaysANewJob(t *testing.T) {
	f, c := migratedForRemoval(t)
	stuck := false
	f.putJSON(migrateJobPathT, runningCopyJob)
	sim := &migrateSim{result: copyOK, objectStore: "True", jobRunning: true}
	simulateMigrate(f, sim)
	finishJobOnCreate(f, sim)
	releasePVCWhenServerGone(f, &stuck)
	ok, msg, _, steps := runMigrate(t, c, MigrateOptions{RemoveOldStorage: true})
	if !ok || !hasStep(steps, "copy", "stopping it, so the final copy starts now") {
		t.Fatalf("got %v: %s", ok, msg)
	}
	m := strings.Join(f.mutations(), "\n")
	del := strings.Index(m, "DELETE "+migrateJobPathT)
	post := strings.LastIndex(m, "POST /apis/batch/v1/namespaces/seaweedfs/jobs")
	if del < 0 || post < del || post > strings.Index(m, "DELETE "+legacyS3DeployPath) {
		t.Errorf("the earlier Job was not replaced by a new one before the deletion: %s", m)
	}
}

// A final copy that does not finish keeps running, and nothing is deleted;
// the next removal waits for that copy instead of starting over, so a
// storage too large for one run can still be removed.
func TestRemoveOldStorage_FinalCopyResumes(t *testing.T) {
	f, c := migratedForRemoval(t)
	sim := &migrateSim{result: copyOK, objectStore: "True", jobRunning: true}
	simulateMigrate(f, sim)
	ok, msg, code, _ := runMigrate(t, c, MigrateOptions{RemoveOldStorage: true})
	if ok || code != "in_progress" || !strings.Contains(msg, "still running") || !strings.Contains(msg, "nothing was deleted") || !strings.Contains(msg, "again to keep waiting") {
		t.Fatalf("got %v %s: %s", ok, code, msg)
	}
	job := f.get(migrateJobPathT)
	if job == nil || ensureMap(ensureMap(job, "metadata"), "annotations")[finalCopyAnnotation] != "true" {
		t.Fatalf("the unfinished final copy was not left running with its mark: %v", job)
	}
	noWritesIn(t, f, legacyNamespace)

	before := len(f.mutations())
	finishJobAfterReads(f, sim, 1)
	stuck := false
	releasePVCWhenServerGone(f, &stuck)
	ok, msg, _, steps := runMigrate(t, c, MigrateOptions{RemoveOldStorage: true})
	if !ok || !hasStep(steps, "copy", "The final copy an earlier removal started is still running: waiting for it") {
		t.Fatalf("got %v: %s\n%s", ok, msg, stepMessages(steps))
	}
	for _, m := range f.mutations()[before:] {
		if strings.HasPrefix(m, "POST /apis/batch/v1/namespaces/seaweedfs/jobs") {
			t.Errorf("a new final copy was started instead of waiting for the running one: %s", m)
		}
	}
}

// The removal's final copy never overwrites the new storage, even when no
// pipeline server uses it: by then the user writes there.
func TestRemoveOldStorage_FinalCopyNeverOverwrites(t *testing.T) {
	f, c := migratedForRemoval(t)
	for _, k := range []fakeKey{
		{gv: dspaAPIGroup, ns: "juntao-test", plural: "datasciencepipelinesapplications", name: legacyDSPAName},
		{gv: dspaAPIGroup, ns: "team-a", plural: "datasciencepipelinesapplications", name: previousDSPAName},
	} {
		delete(f.objects, k)
	}
	sim := &migrateSim{result: copyOK, objectStore: "True"}
	simulateMigrate(f, sim)
	stuck := false
	releasePVCWhenServerGone(f, &stuck)
	runMigrate(t, c, MigrateOptions{RemoveOldStorage: true})
	if sim.mode != copyIgnoreExisting {
		t.Errorf("the final copy ran with COPY_MODE %q", sim.mode)
	}
}

// The final copy may use the operation's remaining time, less what the
// deletions need, when that is longer than MigrateCopyTimeout.
func TestRemoveOldStorage_FinalCopyUsesTheRemainingTime(t *testing.T) {
	f, c := migratedForRemoval(t)
	stuck := false
	old := removeAfterCopyReserve
	removeAfterCopyReserve = 50 * time.Millisecond
	t.Cleanup(func() { removeAfterCopyReserve = old })
	sim := &migrateSim{result: copyOK, objectStore: "True", jobRunning: true}
	simulateMigrate(f, sim)
	releasePVCWhenServerGone(f, &stuck)
	// The copy takes 400ms on the clock: longer than MigrateCopyTimeout
	// (200ms here) but within the remaining time.
	finishJobAfter(f, sim, 2*MigrateCopyTimeout)
	ctx, cancel := kube.Clock().WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ok, msg, _, _ := runMigrate(t, c.WithContext(ctx), MigrateOptions{RemoveOldStorage: true})
	if !ok {
		t.Fatalf("got %v: %s", ok, msg)
	}
}

// A pipeline server is moved only when its whole wait still fits in the
// operation's deadline; otherwise the run stops before moving it.
func TestMigrate_StopsBeforeARepointThatCannotFinishInTime(t *testing.T) {
	fastMigrateTimings(t)
	MigrateRepointTimeout = 10 * time.Second
	f, c := newResourceFake(t)
	putMigrationCase(f)
	simulateMigrate(f, &migrateSim{result: copyOK, objectStore: "True"})
	ctx, cancel := kube.Clock().WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ok, msg, code, _ := runMigrate(t, c.WithContext(ctx), MigrateOptions{})
	if ok || code != "in_progress" || !strings.Contains(msg, "Not enough time is left in this run to move pipeline server juntao-test/dspa") || !strings.Contains(msg, "run Migrate storage again to continue") {
		t.Fatalf("got %v %s: %s", ok, code, msg)
	}
	noChangeToPipelineServers(t, f)
}

// The generation the condition must reach after a repoint: never the
// pre-patch one, even when the patch response cannot be parsed.
func TestRepointedGeneration(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want int64
	}{
		{"from the response", `{"metadata":{"generation":7}}`, 7},
		{"unparsable response", `not json`, 4},
		{"no generation in the response", `{"metadata":{}}`, 4},
		{"a stale generation in the response", `{"metadata":{"generation":3}}`, 4},
	} {
		if got := repointedGeneration([]byte(tc.body), 3); got != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, got, tc.want)
		}
	}
	stale := dspaInfo{Meta: objectMeta{Generation: 3}, ObjectStore: &dspaCondition{Status: "True", ObservedGeneration: 3}}
	if stale.objectStoreReadyAt(repointedGeneration([]byte(`not json`), 3)) {
		t.Error("a condition from before the patch counts")
	}
}
