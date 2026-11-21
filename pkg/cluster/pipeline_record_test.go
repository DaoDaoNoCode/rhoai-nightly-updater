package cluster

import (
	"net/http"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
)

// putForgedDSPA is a project editor's own pipeline server in project evil,
// made to look like this tool's: its label, a write under the tool's field
// manager (a client names its own fieldManager), on the old storage. The
// tool never recorded it.
func putForgedDSPA(f *resourceFake) {
	putDSPAOn(f, "evil", dspaName, toolLabelJSON, envkit.ToolFieldManager, dspaSecretName)
}

func noWriteInProject(t *testing.T, f *resourceFake, project string) {
	t.Helper()
	for _, m := range f.mutations() {
		if strings.Contains(m, "/namespaces/"+project+"/") {
			t.Errorf("written in %s: %s", project, m)
		}
	}
	for _, s := range []string{dspaSecretName, connectionSecretName} {
		if f.has(secretPath(project, s)) {
			t.Errorf("Secret %s/%s with the storage's keys was created", project, s)
		}
	}
}

// A pipeline server that only looks like this tool's (label and field
// manager, both of which its project's editors can write) is not moved:
// the storage-wide keys never reach its project. The status and the result
// name it for the user's confirmation.
func TestMigrate_AForgedPipelineServerGetsNoKeys(t *testing.T) {
	fastMigrateTimings(t)
	f, c := newResourceFake(t)
	putMigrationCase(f)
	putForgedDSPA(f)
	simulateMigrate(f, &migrateSim{result: copyOK, objectStore: "True"})

	st, err := GetResourcesStatus(c)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(st.LegacyStorage.Unrecorded, ","); got != "evil/dspa" {
		t.Fatalf("unrecorded = %q", got)
	}

	ok, msg, code, _ := runMigrate(t, c, MigrateOptions{})
	if !ok {
		t.Fatalf("migrate failed: %s (%s)", msg, code)
	}
	if !strings.Contains(msg, "evil/dspa ("+unrecordedReason+")") || !strings.Contains(msg, "2 pipeline servers now use it: juntao-test/dspa, team-a/nightly-dspa") {
		t.Fatalf("message: %s", msg)
	}
	noWriteInProject(t, f, "evil")
	if host := externalStorage(t, f, "evil", dspaName)["host"]; host != legacyS3Host() {
		t.Fatalf("the forged pipeline server was repointed to %v", host)
	}
	// Confirming another project changes nothing for it.
	if ok, msg, _, _ := runMigrate(t, c, MigrateOptions{ConfirmProjects: []string{"team-a"}}); !ok {
		t.Fatalf("second run: %s", msg)
	}
	noWriteInProject(t, f, "evil")
}

// An unrecorded pipeline server is moved when the user confirms its
// project, and recorded then: the next run needs no confirmation.
func TestMigrate_AConfirmedProjectIsMovedAndRecorded(t *testing.T) {
	fastMigrateTimings(t)
	f, c := newResourceFake(t)
	putMigrationCase(f)
	putForgedDSPA(f)
	simulateMigrate(f, &migrateSim{result: copyOK, objectStore: "True"})
	ok, msg, code, _ := runMigrate(t, c, MigrateOptions{ConfirmProjects: []string{"evil"}})
	if !ok {
		t.Fatalf("migrate failed: %s (%s)", msg, code)
	}
	if host := externalStorage(t, f, "evil", dspaName)["host"]; host != s3Host() {
		t.Fatalf("not moved: %v (%s)", host, msg)
	}
	rec, err := readPipelineRecord(c)
	if err != nil {
		t.Fatal(err)
	}
	uid, _ := ensureMap(f.get(dspaPath("evil", dspaName)), "metadata")["uid"].(string)
	if rec["evil/dspa"].UID != uid || uid == "" {
		t.Fatalf("record = %+v, uid %q", rec, uid)
	}
}

// The record cannot be read: Migrate storage stops before it writes
// anything (it cannot tell which pipeline servers are this tool's).
func TestMigrate_AnUnreadableRecordStopsBeforeAnyWrite(t *testing.T) {
	fastMigrateTimings(t)
	f, c := newResourceFake(t)
	putMigrationCase(f)
	simulateMigrate(f, &migrateSim{result: copyOK, objectStore: "True"})
	f.fail["GET "+rhoai.NamespacedPath("v1", "configmaps", record.GetActivityNamespace(), record.SnapshotConfigMapName)] = http.StatusInternalServerError
	f.requests = nil
	ok, msg, _, _ := runMigrate(t, c, MigrateOptions{ConfirmProjects: []string{"juntao-test"}})
	if ok || !strings.Contains(msg, "nothing was changed") {
		t.Fatalf("got %t: %s", ok, msg)
	}
	if m := f.mutations(); len(m) != 0 {
		t.Fatalf("written: %v", m)
	}
}

// Setup records the pipeline server it creates; teardown forgets it.
func TestPipelineServer_SetupRecordsAndTeardownForgets(t *testing.T) {
	fastPipelineTimings(t)
	f, c := newResourceFake(t)
	putManagedDSC(f)
	putReadyManagedS3(f)
	putDSProject(f, "proj")
	if res, err := SetupPipelineServer(c, "proj"); err != nil || !res.Success {
		t.Fatalf("setup: %+v, %v", res, err)
	}
	rec, _ := readPipelineRecord(c)
	uid, _ := ensureMap(f.get(dspaPath("proj", dspaName)), "metadata")["uid"].(string)
	if uid == "" || rec["proj/dspa"].UID != uid {
		t.Fatalf("record = %+v, uid %q", rec, uid)
	}
	f.finalizeOnGet = true
	if res, err := TeardownPipelineServer(c, "proj"); err != nil || !res.Success {
		t.Fatalf("teardown: %+v, %v", res, err)
	}
	if rec, _ := readPipelineRecord(c); len(rec) != 0 {
		t.Fatalf("record after teardown = %+v", rec)
	}
}
