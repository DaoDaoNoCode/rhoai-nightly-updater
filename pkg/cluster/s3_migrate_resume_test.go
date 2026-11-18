package cluster

import (
	"strings"
	"testing"
)

const pvcProtection = "kubernetes.io/pvc-protection"

// migratedForRemoval is a cluster after Migrate storage succeeded and the
// hand-made pipeline server moved away: the old storage may be removed.
// The old SeaweedFS mounts seaweedfs-pvc, so it carries the pvc-protection
// finalizer; the simulated PVC-protection controller removes it once the
// Deployment is gone (pkg/controller/volume/pvcprotection releases a PVC
// when no pod uses it). minio-pvc is mounted by nothing.
func migratedForRemoval(t *testing.T) (*resourceFake, *Client) {
	t.Helper()
	fastMigrateTimings(t)
	f, c := newResourceFake(t)
	putMigrationCase(f)
	delete(f.objects, fakeKey{gv: dspaAPIGroup, ns: "hand-made", plural: "datasciencepipelinesapplications", name: legacyDSPAName})
	f.putJSON(secretPath("juntao-test", legacyDSPASecretName), `{"metadata":{"creationTimestamp":"`+created+`","managedFields":`+managedFieldsJSON(legacyPostManager, "Update")+`}}`)
	ensureMap(f.get(legacyS3PVCPath), "metadata")["finalizers"] = []interface{}{pvcProtection}
	simulateMigrate(f, &migrateSim{result: copyOK, objectStore: "True"})
	if ok, msg, _, _ := runMigrate(t, c, MigrateOptions{}); !ok {
		t.Fatal(msg)
	}
	return f, c
}

// releasePVCWhenServerGone plays the PVC-protection controller for
// seaweedfs-pvc: once the old SeaweedFS Deployment is gone (with its pod),
// a PVC that is being deleted goes. stuck keeps it (a pod that does not
// stop).
func releasePVCWhenServerGone(f *resourceFake, stuck *bool) {
	next := f.beforeServe
	f.beforeServe = func(method, path string) {
		next(method, path)
		f.mu.Lock()
		defer f.mu.Unlock()
		dep := fakeKey{gv: "apps/v1", ns: legacyNamespace, plural: "deployments", name: s3Deployment}
		pvc := fakeKey{gv: "v1", ns: legacyNamespace, plural: "persistentvolumeclaims", name: legacySeaweedFSPVC}
		if obj := f.objects[pvc]; obj != nil && f.objects[dep] == nil && !*stuck && ensureMap(obj, "metadata")["deletionTimestamp"] != nil {
			delete(f.objects, pvc)
		}
	}
}

func jobsCreated(f *resourceFake) int {
	n := 0
	for _, m := range f.mutations() {
		if m == "POST /apis/batch/v1/namespaces/seaweedfs/jobs" {
			n++
		}
	}
	return n
}

// newStorageIntact fails when the new storage lost an object.
func newStorageIntact(t *testing.T, f *resourceFake) {
	t.Helper()
	for _, p := range []string{s3DeployPath, s3PVCPath, s3SecretPath} {
		if !f.has(p) {
			t.Errorf("the new storage lost %s", p)
		}
	}
}

func noTearDownAdvice(t *testing.T, msg string) {
	t.Helper()
	if strings.Contains(strings.ToLower(msg), "tear down") || strings.Contains(strings.ToLower(msg), "teardown") {
		t.Errorf("the migration flow points at Tear down, which deletes the new storage too: %s", msg)
	}
}

// The removal fails after the old server's Deployment is deleted; the next
// run does not copy again (there is nothing left to copy from) and finishes.
func TestRemoveOldStorage_ResumesAfterTheServerIsDeleted(t *testing.T) {
	t.Run("a NetworkPolicy cannot be deleted", func(t *testing.T) {
		f, c := migratedForRemoval(t)
		stuck := false
		releasePVCWhenServerGone(f, &stuck)
		f.fail["DELETE "+legacyS3NPPath] = 403
		ok, msg, code, _ := runMigrate(t, c, MigrateOptions{RemoveOldStorage: true})
		if ok || code != "partial_failure" || !strings.Contains(msg, "NetworkPolicy minio/seaweedfs-ingress") || !strings.Contains(msg, "Run Remove old storage again to continue") || !strings.Contains(msg, "make upgrade") {
			t.Fatalf("got %v %s: %s", ok, code, msg)
		}
		noTearDownAdvice(t, msg)
		if f.has(legacyS3DeployPath) || f.has(legacyS3PVCPath) || !f.has(legacyS3NPPath) {
			t.Fatal("the failure did not happen after the server was deleted")
		}
		newStorageIntact(t, f)

		delete(f.fail, "DELETE "+legacyS3NPPath)
		jobs := jobsCreated(f)
		ok, msg, _, steps := runMigrate(t, c, MigrateOptions{RemoveOldStorage: true})
		if !ok || !strings.Contains(msg, "The rest of the old storage in namespace 'minio' was removed") || !strings.Contains(msg, "nothing was copied this time") {
			t.Fatalf("re-run: %v %s", ok, msg)
		}
		if jobsCreated(f) != jobs {
			t.Error("the re-run started a copy although no server is left to copy from")
		}
		if !hasStep(steps, "copy", "Not copied again") {
			t.Error("the re-run does not say why it did not copy")
		}
		if f.has(legacyS3NPPath) || f.has(legacyMinIONPPath) {
			t.Error("the NetworkPolicy is left")
		}
		newStorageIntact(t, f)
	})
	t.Run("a PVC is still terminating at the bound", func(t *testing.T) {
		f, c := migratedForRemoval(t)
		stuck := true
		releasePVCWhenServerGone(f, &stuck)
		ok, msg, code, _ := runMigrate(t, c, MigrateOptions{RemoveOldStorage: true})
		if ok || code != "in_progress" || !strings.Contains(msg, "PVC minio/seaweedfs-pvc is still terminating") || !strings.Contains(msg, "Run Remove old storage again to continue") {
			t.Fatalf("got %v %s: %s", ok, code, msg)
		}
		noTearDownAdvice(t, msg)
		if f.has(legacyS3DeployPath) || !f.has(legacyS3NPPath) {
			t.Fatal("the server is left, or the NetworkPolicy went before its pods")
		}

		stuck = false
		jobs := jobsCreated(f)
		ok, msg, _, _ = runMigrate(t, c, MigrateOptions{RemoveOldStorage: true})
		if !ok || jobsCreated(f) != jobs || f.has(legacyS3PVCPath) || f.has(legacyS3NPPath) {
			t.Fatalf("re-run: %v %s", ok, msg)
		}
		newStorageIntact(t, f)
	})
}

// A failure before the server's Deployment leaves it running; the PVCs are
// marked for deletion, so the next run finishes even though the copy can no
// longer run (its Secret is gone).
func TestRemoveOldStorage_StopsBeforeTheServerOnAnEarlierFailure(t *testing.T) {
	f, c := migratedForRemoval(t)
	stuck := false
	releasePVCWhenServerGone(f, &stuck)
	f.fail["DELETE "+legacySvcPath] = 403
	ok, msg, _, _ := runMigrate(t, c, MigrateOptions{RemoveOldStorage: true})
	if ok || !strings.Contains(msg, "Service minio/minio-service") || !strings.Contains(msg, "The server's Deployment in namespace 'minio' was not deleted") {
		t.Fatalf("got %v: %s", ok, msg)
	}
	if hasExactMutation(f, "DELETE "+legacyS3DeployPath) || !f.has(legacyS3DeployPath) {
		t.Fatal("the server was deleted after an earlier deletion failed")
	}
	if pvc := f.get(legacyS3PVCPath); pvc == nil || ensureMap(pvc, "metadata")["deletionTimestamp"] == nil {
		t.Fatal("the data PVC was not marked for deletion before the copy's Secret and Service")
	}

	delete(f.fail, "DELETE "+legacySvcPath)
	jobs := jobsCreated(f)
	ok, msg, _, _ = runMigrate(t, c, MigrateOptions{RemoveOldStorage: true})
	if !ok || jobsCreated(f) != jobs {
		t.Fatalf("re-run: %v %s (copies %d -> %d)", ok, msg, jobs, jobsCreated(f))
	}
	for _, p := range []string{legacyS3DeployPath, legacySvcPath, legacyS3PVCPath, legacyS3NPPath} {
		if f.has(p) {
			t.Errorf("%s is left", p)
		}
	}
	newStorageIntact(t, f)
}

// A failure before any PVC was marked leaves everything the copy needs: the
// next run copies and checks again before it deletes.
func TestRemoveOldStorage_CopiesAgainWhileTheServerCanBeCopiedFrom(t *testing.T) {
	f, c := migratedForRemoval(t)
	stuck := false
	releasePVCWhenServerGone(f, &stuck)
	f.fail["DELETE "+legacyUIRoutePath] = 500
	if ok, msg, _, _ := runMigrate(t, c, MigrateOptions{RemoveOldStorage: true}); ok || !strings.Contains(msg, "Route minio/minio-ui") {
		t.Fatalf("got %v: %s", ok, msg)
	}
	if !f.has(legacyS3DeployPath) || !f.has(legacySecretPath) || !f.has(legacySvcPath) {
		t.Fatal("something the copy needs was deleted after an earlier failure")
	}
	delete(f.fail, "DELETE "+legacyUIRoutePath)
	jobs := jobsCreated(f)
	if ok, msg, _, _ := runMigrate(t, c, MigrateOptions{RemoveOldStorage: true}); !ok || jobsCreated(f) != jobs+1 {
		t.Fatalf("re-run: %v %s", ok, msg)
	}
}

// The deletion order: what can fail on its own first, the PVCs before what
// the copy needs, the server's Deployment after all of them, and the
// NetworkPolicies only once the PVCs (and so the server's pods) are gone.
func TestRemoveOldStorage_DeletionOrder(t *testing.T) {
	f, c := migratedForRemoval(t)
	stuck := false
	releasePVCWhenServerGone(f, &stuck)
	if ok, msg, _, _ := runMigrate(t, c, MigrateOptions{RemoveOldStorage: true}); !ok {
		t.Fatal(msg)
	}
	at := func(want string) int {
		for i, m := range f.mutations() {
			if m == want {
				return i
			}
		}
		t.Fatalf("no %s in %v", want, f.mutations())
		return -1
	}
	order := []string{
		"DELETE " + secretPath("juntao-test", legacyDSPASecretName),
		"DELETE " + legacyUIRoutePath,
		"DELETE " + legacyS3PVCPath,
		"DELETE " + legacyMinIOPVCPath,
		"DELETE " + legacySecretPath,
		"DELETE " + legacySvcPath,
		"DELETE " + legacyS3DeployPath,
		"DELETE " + legacyS3NPPath,
	}
	for i := 1; i < len(order); i++ {
		if at(order[i-1]) > at(order[i]) {
			t.Errorf("%s came after %s", order[i-1], order[i])
		}
	}
	// The PVC was gone before the NetworkPolicy was deleted.
	gets := strings.Join(f.requests, "\n")
	if strings.Index(gets, "DELETE "+legacyS3NPPath) < strings.LastIndex(gets, "GET /api/v1/namespaces/minio/persistentvolumeclaims") {
		t.Error("the NetworkPolicy was deleted before the PVC wait ended")
	}
}

// The listing at the deadline fails with the expired context; the PVCs the
// last successful listing found are still reported.
func TestWaitPVCsGone_ReportsWhatIsLeftAtTheDeadline(t *testing.T) {
	fastS3Timings(t)
	f, c := newResourceFake(t)
	f.putJSON(legacyS3PVCPath, `{"metadata":{"uid":"pvc-1","finalizers":["`+pvcProtection+`"],"deletionTimestamp":"2026-11-17T00:00:00Z"}}`)
	remaining, err := waitPVCsGone(c, map[string]string{"pvc-1": legacyNamespace})
	if err != nil || strings.Join(remaining, ",") != "minio/seaweedfs-pvc" {
		t.Fatalf("remaining = %v, %v", remaining, err)
	}
}

func TestLegacyRemovalPhases_CoverEveryLegacyObject(t *testing.T) {
	seen := map[legacyObjectName]int{}
	for _, phase := range [][]legacyObjectName{legacyRemovalPhases.beforeServer, legacyRemovalPhases.server, legacyRemovalPhases.afterServer} {
		for _, o := range phase {
			seen[o]++
		}
	}
	for _, o := range legacyStorageObjects {
		if seen[legacyObjectName{o.kind, o.name}] != 1 {
			t.Errorf("%s %s is deleted %d times by Remove old storage", o.kind, o.name, seen[legacyObjectName{o.kind, o.name}])
		}
		delete(seen, legacyObjectName{o.kind, o.name})
	}
	if len(seen) > 0 {
		t.Errorf("Remove old storage deletes objects teardown does not know: %v", seen)
	}
}

// When the old storage cannot be copied from and its data PVC is not being
// deleted (someone else deleted its server), it may hold objects that were
// never copied: nothing is deleted, and the message gives the manual fix.
func TestRemoveOldStorage_ServerGoneWithoutRemovalDeletesNothing(t *testing.T) {
	t.Run("server deleted by someone else", func(t *testing.T) {
		f, c := migratedForRemoval(t)
		f.mu.Lock()
		delete(f.objects, fakeKey{gv: "apps/v1", ns: legacyNamespace, plural: "deployments", name: s3Deployment})
		f.mu.Unlock()
		before := len(f.mutations())
		ok, msg, code, _ := runMigrate(t, c, MigrateOptions{RemoveOldStorage: true})
		if ok || code != "prerequisites" || len(f.mutations()) != before {
			t.Fatalf("got %v %s: %s; writes %v", ok, code, msg, f.mutations()[before:])
		}
		for _, want := range []string{"no server runs there any more", "seaweedfs-pvc is not being deleted", "Nothing was deleted", "A retry gives the same result", "`oc delete pvc seaweedfs-pvc -n minio`"} {
			if !strings.Contains(msg, want) {
				t.Errorf("message lacks %q: %s", want, msg)
			}
		}
		noTearDownAdvice(t, msg)
	})
	t.Run("server not running", func(t *testing.T) {
		f, c := migratedForRemoval(t)
		f.putJSON(legacyS3DeployPath, `{"metadata":{"labels":`+toolLabelJSON+`,"generation":1},"status":{"readyReplicas":0}}`)
		before := len(f.mutations())
		ok, msg, _, _ := runMigrate(t, c, MigrateOptions{RemoveOldStorage: true})
		if ok || len(f.mutations()) != before || !strings.Contains(msg, "SeaweedFS there is not running") || !strings.Contains(msg, "Start the server again (Deployment seaweedfs)") {
			t.Fatalf("got %v: %s", ok, msg)
		}
	})
}

func TestMigrate_MessagesNeverPointAtTearDown(t *testing.T) {
	t.Run("no server", func(t *testing.T) {
		fastMigrateTimings(t)
		f, c := newResourceFake(t)
		putMigrationCase(f)
		f.mu.Lock()
		delete(f.objects, fakeKey{gv: "apps/v1", ns: legacyNamespace, plural: "deployments", name: s3Deployment})
		f.mu.Unlock()
		ok, msg, _, _ := runMigrate(t, c, MigrateOptions{})
		if ok || !strings.Contains(msg, "No server runs in namespace 'minio' any more") || !strings.Contains(msg, "a retry gives the same result") {
			t.Fatalf("got %v: %s", ok, msg)
		}
		noTearDownAdvice(t, msg)
	})
	t.Run("other keys", func(t *testing.T) {
		fastMigrateTimings(t)
		f, c := newResourceFake(t)
		putMigrationCase(f)
		putReadyManagedS3(f)
		ok, msg, _, _ := runMigrate(t, c, MigrateOptions{})
		// Follow-up 7: it names the ways forward, by hand.
		if ok || !strings.Contains(msg, "a retry gives the same result") || !strings.Contains(msg, "`oc delete project seaweedfs`") || !strings.Contains(msg, "rclone") || !strings.Contains(msg, "Secret 'minio-secret' in 'minio'") {
			t.Fatalf("got %v: %s", ok, msg)
		}
		noTearDownAdvice(t, msg)
	})
	t.Run("namespace delete fails", func(t *testing.T) {
		f, c := migratedForRemoval(t)
		stuck := false
		releasePVCWhenServerGone(f, &stuck)
		f.putJSON(legacyNSPath, liveMinioNamespace)
		f.fail["DELETE "+legacyNSPath] = 500
		ok, msg, _, _ := runMigrate(t, c, MigrateOptions{RemoveOldStorage: true, DeleteNamespace: true})
		if ok || !strings.Contains(msg, "A retry finds nothing left to remove") || !strings.Contains(msg, "`oc delete project minio`") {
			t.Fatalf("got %v: %s", ok, msg)
		}
		noTearDownAdvice(t, msg)
	})
}
