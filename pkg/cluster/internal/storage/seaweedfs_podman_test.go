//go:build seaweedfs_podman

package storage

// End-to-end check that scoped keys behave correctly against the SeaweedFS
// container image the app ships (SeaweedFSDefaultImage), exercised via Podman.
// Pipelines skip this target because it requires Podman and an image pull; invoke it with
//
//	make test-seaweedfs-podman
//
// or go test -tags seaweedfs_podman -run TestSeaweedFSPodman -v ./pkg/cluster/internal/storage/
//
// The test launches "weed mini" using arguments from S3Resources—first without
// credentials, then with a rotated admin key matching Secure S3 storage—and
// mounts a persistent named volume so restarts retain filer state, asserting
// invariants the implementation depends on.

import (
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"
)

type podmanSeaweed struct {
	t         *testing.T
	name, vol string
	endpoint  string
}

func (p *podmanSeaweed) run(args ...string) string {
	p.t.Helper()
	out, err := exec.Command("podman", args...).CombinedOutput()
	if err != nil {
		p.t.Fatalf("podman %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// start tears down any existing instance and brings the container back up with the supplied admin credentials and S3Resources argument list.
func (p *podmanSeaweed) start(admin S3Keys, secured bool) {
	p.t.Helper()
	_ = exec.Command("podman", "rm", "-f", p.name).Run()
	var args []string
	for _, r := range S3Resources(admin, secured) {
		if r.Kind == "Deployment" {
			ctr := r.Obj["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]map[string]any)[0]
			args = ctr["args"].([]string)
		}
	}
	run := []string{"run", "-d", "--name", p.name, "-p", "127.0.0.1::8333", "-v", p.vol + ":/data",
		"-e", "AWS_ACCESS_KEY_ID=" + admin.AccessKey, "-e", "AWS_SECRET_ACCESS_KEY=" + admin.SecretKey,
		"-e", "WEED_ADMIN_USER=admin", "-e", "WEED_ADMIN_PASSWORD=" + admin.AdminPassword, SeaweedFSDefaultImage}
	p.run(append(run, args...)...)
	port := p.run("port", p.name, "8333/tcp")
	p.endpoint = "http://" + port[strings.LastIndex(port, "\n")+1:]
	for i := 0; i < 120; i++ {
		if resp, err := http.Get(p.endpoint + "/status"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				p.t.Logf("started %s (secured=%v) at %s; args %v", p.name, secured, p.endpoint, args)
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	p.t.Fatalf("SeaweedFS did not start:\n%s", p.run("logs", p.name))
}

func TestSeaweedFSPodman(t *testing.T) {
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman is not installed")
	}
	suffix, _ := randomHex(4)
	p := &podmanSeaweed{t: t, name: "rnu-sw-test-" + suffix, vol: "rnu-sw-test-" + suffix}
	t.Cleanup(func() {
		_ = exec.Command("podman", "rm", "-f", p.name).Run()
		_ = exec.Command("podman", "volume", "rm", "-f", p.vol).Run()
	})
	ctx := context.Background()
	t.Cleanup(SetS3ToolEndpointForTest("")) // overwritten after the mapped port is determined
	setEndpoint := func() { s3ToolEndpoint = func() string { return p.endpoint } }
	admin1 := S3Keys{AccessKey: "seaweedfs-old" + suffix, SecretKey: "old-secret-" + suffix, AdminPassword: "ui-" + suffix}
	admin2 := S3Keys{AccessKey: "seaweedfs-new" + suffix, SecretKey: "new-secret-" + suffix, AdminPassword: "ui-" + suffix}

	// 1. Legacy unsecured layout: IAM mutations must fail.
	p.start(admin1, false)
	setEndpoint()
	old := newSeaweedClient(ctx, p.endpoint, admin1)
	if err := old.CreateUser("rnu-dspa-before"); err == nil {
		t.Fatal("CreateUser worked with IAM read-only")
	} else {
		t.Logf("unsecured storage refuses IAM writes: %v", err)
	}
	if err := s3PutBucket(ctx, p.endpoint, admin1.AccessKey, admin1.SecretKey, s3Bucket); err != nil {
		t.Fatalf("old admin key on unsecured storage: %v", err)
	}

	// 2. Mimic Secure S3 migration: restart under secured S3Resources with a fresh admin key.
	p.start(admin2, true)
	if err := s3PutBucket(ctx, p.endpoint, admin1.AccessKey, admin1.SecretKey, "after-rotation"); err == nil {
		t.Fatal("the old admin key still works after the rotation")
	} else {
		t.Logf("old admin key after the rotation: %v", err)
	}
	if _, err := old.ListUsers(); err == nil {
		t.Fatal("the old admin key can still call IAM")
	}
	admin := newSeaweedClient(ctx, p.endpoint, admin2)

	// 3. Scoped-key provisioning the way Set up and Secure S3 storage paths exercise it.
	for _, project := range []string{"team-a", strings.Repeat("p", 63)} {
		keys, fresh, err := provisionScopedKey(admin, project, S3Keys{})
		if err != nil || !fresh {
			t.Fatalf("provisionScopedKey(%s): %v fresh=%v", project, err, fresh)
		}
		if err := probeScopedKey(ctx, keys, ScopedUserName(project)); err != nil {
			t.Fatalf("probe of the new key of %s: %v", project, err)
		}
		again, fresh, err := provisionScopedKey(admin, project, keys)
		if err != nil || fresh || again != keys {
			t.Fatalf("a re-run must keep the working key: %v fresh=%v", err, fresh)
		}
		t.Logf("user %s (%d chars): key works, DSPO-style HEAD answers 404, re-run keeps it", ScopedUserName(project), len(ScopedUserName(project)))
	}
	keys, _, _ := provisionScopedKey(admin, "team-a", S3Keys{})
	// Passing stale credentials should rotate to a new key and remove superseded access keys.
	ids, _, _ := admin.ListAccessKeys(ScopedUserName("team-a"))
	if len(ids) != 1 || ids[0] != keys.AccessKey {
		t.Fatalf("after a fresh key the user has keys %v, want only %s", ids, keys.AccessKey)
	}

	// 4. Operations that must be blocked when using a scoped key.
	scoped := newSeaweedClient(ctx, p.endpoint, keys)
	denied := map[string]error{}
	_, denied["IAM CreateUser"] = scoped.iam("CreateUser", map[string][]string{"UserName": {"evil"}})
	_, denied["IAM CreateAccessKey (self)"] = scoped.CreateAccessKey(ScopedUserName("team-a"))
	denied["IAM PutUserPolicy (self)"] = scoped.PutUserPolicy(ScopedUserName("team-a"), "esc", `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["*"],"Resource":["*"]}]}`)
	denied["IAM DeleteUser (other)"] = scoped.DeleteUser(ScopedUserName(strings.Repeat("p", 63)))
	denied["CreateBucket"] = s3PutBucket(ctx, p.endpoint, keys.AccessKey, keys.SecretKey, "scoped-new")
	for what, err := range denied {
		if err == nil {
			t.Errorf("a scoped key may %s", what)
		} else {
			t.Logf("scoped key denied %s: %v", what, err)
		}
	}
	if users, err := admin.ListUsers(); err != nil || strings.Contains(strings.Join(users, ","), "evil") {
		t.Fatalf("users after the denied calls: %v %v", users, err)
	} else {
		t.Logf("users: %v", users)
	}

	// 5. Container restart should preserve IAM users, access keys, and policies thanks to volume-backed filer metadata.
	p.start(admin2, true)
	setEndpoint()
	admin = newSeaweedClient(ctx, p.endpoint, admin2)
	ids, found, err := admin.ListAccessKeys(ScopedUserName("team-a"))
	if err != nil || !found || len(ids) != 1 || ids[0] != keys.AccessKey {
		t.Fatalf("after a restart: keys %v found=%v err=%v", ids, found, err)
	}
	if err := probeScopedKey(ctx, keys, ScopedUserName("team-a")); err != nil {
		t.Fatalf("the scoped key after a restart: %v", err)
	}
	t.Log("after a restart: user, key and policy kept; the key still works")

	// 6. Cleanup removes access keys before the user; the scoped credential must no longer authenticate afterward.
	if err := deleteScopedUser(admin, "team-a"); err != nil {
		t.Fatal(err)
	}
	if err := probeScopedKey(ctx, keys, ScopedUserName("team-a")); err == nil {
		t.Fatal("the key works after its user was deleted")
	}
	deleted, err := deleteScopedUsers(admin)
	if err != nil {
		t.Fatal(err)
	}
	users, _ := admin.ListUsers()
	t.Logf("deleted the remaining tool users %v; left: %v", deleted, users)
	for _, u := range users {
		if strings.HasPrefix(u, ScopedUserPrefix) {
			t.Errorf("tool user %s left", u)
		}
	}
	fmt.Println("SeaweedFS", SeaweedFSDefaultImage, "checks passed")
}
