package cluster

// Regression tests for the review-round findings on the quick resources:
// foreign objects appearing between the tool's check and its write, foreign
// objects in the minio namespace at teardown, MinIO endpoint aliases, and
// API calls that stall past an operation's deadline.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	minioSecretPath = "/api/v1/namespaces/minio/secrets/minio-secret"
	minioUIRoute    = "/apis/route.openshift.io/v1/namespaces/minio/routes/minio-ui"
	minioAPIRoute   = "/apis/route.openshift.io/v1/namespaces/minio/routes/minio-api"
	minioSvcPath    = "/api/v1/namespaces/minio/services/minio-service"
)

// foreignJSON is an object someone else created: no tool label, created
// with kubectl.
func foreignJSON(extra string) string {
	return `{"metadata":{"uid":"foreign-uid","resourceVersion":"77","labels":{"owner":"someone"},"creationTimestamp":"` + created + `","managedFields":` + managedFieldsJSON("kubectl-create", "Update") + `}` + extra + `}`
}

// assertForeignUntouched checks that the foreign object still exists with its
// UID, labels and content.
func assertForeignUntouched(t *testing.T, f *resourceFake, path, field, want string) {
	t.Helper()
	obj := f.get(path)
	if obj == nil {
		t.Fatalf("%s was deleted", path)
	}
	meta := obj["metadata"].(map[string]interface{})
	labels, _ := meta["labels"].(map[string]interface{})
	if meta["uid"] != "foreign-uid" || labels[managedByLabelKey] != nil || labels["owner"] != "someone" {
		t.Errorf("%s metadata changed: %v", path, meta)
	}
	if field != "" {
		raw, _ := json.Marshal(obj[field])
		if !strings.Contains(string(raw), want) {
			t.Errorf("%s %s changed: %s", path, field, raw)
		}
	}
}

func managedMinIONamespace(f *resourceFake) {
	putNamespace(f, toolLabelJSON, toolFieldManager)
}

func TestSetupMinIO_RefusesEveryUnlabelledObject(t *testing.T) {
	for _, path := range []string{s3PVCPath, minioSecretPath, s3DeployPath, minioDeployPath, minioSvcPath, minioUIRoute, minioAPIRoute, s3NPPath, minioNPPath} {
		t.Run(path[strings.LastIndex(path, "/")+1:], func(t *testing.T) {
			fastMinIOTimings(t)
			f, c := newResourceFake(t)
			managedMinIONamespace(f)
			f.putJSON(path, foreignJSON(`,"data":{"k":"dXNlcg=="}`))
			resp, _ := SetupMinIO(c)
			if resp.Success || resp.ErrorCode != "not_managed" || !strings.Contains(resp.Message, "Nothing was changed") {
				t.Fatalf("want a not_managed refusal, got %+v", resp)
			}
			name := path[strings.LastIndex(path, "/")+1:]
			if !strings.Contains(resp.Message, name) {
				t.Errorf("message must name %s: %q", name, resp.Message)
			}
			if m := f.mutations(); len(m) != 0 {
				t.Errorf("refusal must not write anything, got %v", m)
			}
			assertForeignUntouched(t, f, path, "data", "dXNlcg==")
		})
	}
}

// A foreign object created after the check and before the POST: the POST
// fails with 409, the object is re-read, and setup refuses.
func TestSetupMinIO_ForeignObjectCreatedBetweenCheckAndCreate(t *testing.T) {
	fastMinIOTimings(t)
	f, c := newResourceFake(t)
	readyAfterApply(f)
	var once sync.Once
	f.beforeServe = func(method, path string) {
		if method == http.MethodPost && path == "/api/v1/namespaces/minio/secrets" {
			once.Do(func() { f.putJSON(minioSecretPath, foreignJSON(`,"data":{"k":"dXNlcg=="}`)) })
		}
	}
	resp, _ := SetupMinIO(c)
	if resp.Success || resp.ErrorCode != "not_managed" || !strings.Contains(resp.Message, "Secret minio/minio-secret") {
		t.Fatalf("want not_managed, got %+v", resp)
	}
	assertForeignUntouched(t, f, minioSecretPath, "data", "dXNlcg==")
	if hasMutation(f, "PATCH "+minioSecretPath) {
		t.Error("the foreign secret must not be patched")
	}
}

// An owned object replaced by a foreign one between the check and the
// update: the update carries the checked resourceVersion, fails with 409, and
// the re-check refuses.
func TestSetupMinIO_ObjectReplacedBetweenCheckAndUpdate(t *testing.T) {
	fastMinIOTimings(t)
	f, c := newResourceFake(t)
	readyAfterApply(f)
	managedMinIONamespace(f)
	f.putJSON(minioSecretPath, `{"metadata":{"labels":`+toolLabelJSON+`,"resourceVersion":"5"},"data":{"minio_root_user":"`+exampleAuth("u")+`","minio_root_password":"`+exampleAuth("p")+`"}}`)
	var once sync.Once
	f.beforeServe = func(method, path string) {
		if method == http.MethodPatch && path == minioSecretPath {
			once.Do(func() { f.putJSON(minioSecretPath, foreignJSON(`,"data":{"k":"dXNlcg=="}`)) })
		}
	}
	resp, _ := SetupMinIO(c)
	if resp.Success || resp.ErrorCode != "not_managed" {
		t.Fatalf("want not_managed, got %+v", resp)
	}
	assertForeignUntouched(t, f, minioSecretPath, "data", "dXNlcg==")
}

// Re-running setup on an install from a released version (unlabelled
// objects created by server-side apply, MinIO image quay.io/minio/minio:latest)
// migrates it to SeaweedFS: a new PVC, the same Secret, the Service switched
// to SeaweedFS, the MinIO Deployment and its policy removed, the MinIO PVC
// kept (and labelled, so teardown can delete it), the console Route kept,
// and the minio-api Route that exposed the S3 API removed.
func TestSetupMinIO_MigratesReleasedInstallAndKeepsMinIOVolume(t *testing.T) {
	fastMinIOTimings(t)
	f, c := newResourceFake(t)
	readyAfterApply(f)
	putNamespace(f, `{}`, legacyPostManager)
	applied := func(uid string) string {
		return `{"metadata":{"uid":"` + uid + `","creationTimestamp":"` + created + `","managedFields":` + managedFieldsJSON(toolFieldManager, "Apply") + `}}`
	}
	f.putJSON(minioDeployPath, `{"metadata":{"uid":"dep","creationTimestamp":"`+created+`","managedFields":`+managedFieldsJSON(toolFieldManager, "Apply")+`},
		"spec":{"template":{"spec":{"containers":[{"name":"minio","image":"quay.io/minio/minio:latest"}]}}},"status":{"readyReplicas":1}}`)
	f.putJSON(minioPVCPath, `{"metadata":{"uid":"pvc","creationTimestamp":"`+created+`","managedFields":`+managedFieldsJSON(toolFieldManager, "Apply")+`},"status":{"capacity":{"storage":"20Gi"}}}`)
	f.putJSON(minioNPPath, `{"metadata":{"uid":"np","labels":`+toolLabelJSON+`}}`)
	f.putJSON(minioSecretPath, `{"metadata":{"uid":"sec","creationTimestamp":"`+created+`","managedFields":`+managedFieldsJSON(toolFieldManager, "Apply")+`},"data":{"minio_root_user":"`+exampleAuth("old-user")+`","minio_root_password":"`+exampleAuth("EXAMPLE-old-pass")+`"}}`)
	f.putJSON(minioSvcPath, `{"metadata":{"uid":"svc","creationTimestamp":"`+created+`","managedFields":`+managedFieldsJSON(toolFieldManager, "Apply")+`},"spec":{"selector":{"app":"minio"},"clusterIP":"172.30.10.20"}}`)
	f.putJSON(minioAPIRoute, `{"metadata":{"uid":"api","creationTimestamp":"`+created+`","managedFields":`+managedFieldsJSON(toolFieldManager, "Apply")+`},"spec":{"host":"minio-api-minio.apps.example.com"}}`)
	f.putJSON(minioUIRoute, applied("ui"))
	if st := getMinIOStatus(c); !st.MigrationPending || !st.Ready || !strings.Contains(st.Warning, "Re-run setup to replace it with SeaweedFS") || !strings.Contains(st.Warning, "minio-api") {
		t.Errorf("status = %+v", st)
	}

	resp, _ := SetupMinIO(c)
	if !resp.Success {
		t.Fatalf("setup = %+v", resp)
	}
	for _, want := range []string{"MinIO from the earlier version was replaced", "not copied", "return 404", "PVC minio-pvc (20Gi), is kept"} {
		if !strings.Contains(resp.Message, want) {
			t.Errorf("message lacks %q: %q", want, resp.Message)
		}
	}
	if f.has(minioDeployPath) || f.has(minioNPPath) || !f.has(s3DeployPath) || !f.has(s3NPPath) {
		t.Errorf("MinIO deployment kept=%v, its policy kept=%v; SeaweedFS deployment=%v, policy=%v", f.has(minioDeployPath), f.has(minioNPPath), f.has(s3DeployPath), f.has(s3NPPath))
	}
	if f.has(minioAPIRoute) || !f.has(minioUIRoute) {
		t.Errorf("api route present=%v, console route present=%v", f.has(minioAPIRoute), f.has(minioUIRoute))
	}
	// The credentials stay, so pipeline servers' copies stay valid.
	sec, _ := json.Marshal(f.get(minioSecretPath))
	if !strings.Contains(string(sec), `"minio_root_user":"old-user"`) || !strings.Contains(string(sec), `"minio_root_password":"EXAMPLE-old-pass"`) {
		t.Errorf("credentials changed: %s", sec)
	}
	svc, _ := json.Marshal(f.get(minioSvcPath))
	if !strings.Contains(string(svc), `"selector":{"app":"seaweedfs"}`) || !strings.Contains(string(svc), `"uid":"svc"`) {
		t.Errorf("Service not switched in place: %s", svc)
	}
	pvc := f.get(minioPVCPath)["metadata"].(map[string]interface{})
	if pvc["uid"] != "pvc" {
		t.Errorf("the MinIO PVC must be kept: %v", pvc)
	}
	if hasMutation(f, "DELETE "+minioPVCPath) || hasMutation(f, "PATCH "+minioPVCPath) {
		t.Error("setup must not touch the MinIO PVC")
	}
	// The Service is switched only after SeaweedFS is ready, and MinIO is
	// removed only after that.
	order := strings.Join(f.mutations(), "\n")
	svcAt, delAt := strings.Index(order, "PATCH "+minioSvcPath), strings.Index(order, "DELETE "+minioDeployPath)
	if deployAt := strings.Index(order, "POST /apis/apps/v1/namespaces/minio/deployments"); deployAt < 0 || svcAt < deployAt || delAt < svcAt {
		t.Errorf("order of writes:\n%s", order)
	}
	st := getMinIOStatus(c)
	if st.MigrationPending || len(st.KeptPVCs) != 1 || st.KeptPVCs[0].Name != "minio-pvc" {
		t.Errorf("status after migration = %+v", st)
	}

	// Re-running it is a no-op for the migration.
	f.requests = nil
	resp, _ = SetupMinIO(c)
	if !resp.Success || strings.Contains(resp.Message, "was replaced") || hasMutation(f, "DELETE") {
		t.Fatalf("re-run = %+v, %v", resp, f.mutations())
	}
}

// When SeaweedFS cannot start, the MinIO being replaced keeps serving: the
// Service is not switched and nothing of MinIO is removed.
func TestSetupMinIO_MigrationFailsClosedWhenSeaweedFSIsNotReady(t *testing.T) {
	fastMinIOTimings(t)
	f, c := newResourceFake(t)
	notReadyAfterApply(f)
	managedMinIONamespace(f)
	putMinIODeployment(f, toolFieldManager, 1)
	f.putJSON(minioNPPath, `{"metadata":{"labels":`+toolLabelJSON+`}}`)
	f.putJSON(minioSvcPath, `{"metadata":{"labels":`+toolLabelJSON+`},"spec":{"selector":{"app":"minio"}}}`)
	resp, _ := SetupMinIO(c)
	if resp.Success || !strings.Contains(resp.Message, "MinIO from the earlier version still serves the pipeline servers") {
		t.Fatalf("setup = %+v", resp)
	}
	if !f.has(minioDeployPath) || !f.has(minioNPPath) || hasMutation(f, "PATCH "+minioSvcPath) {
		t.Errorf("MinIO must keep serving: %v", f.mutations())
	}
	if st := getMinIOStatus(c); !st.MigrationPending || !st.Deployed {
		t.Errorf("status = %+v", st)
	}
}

// The new image under the old template: the new names are not granted yet.
// Setup refuses before changing anything and says how to fix it.
func TestSetupMinIO_ForbiddenByOldTemplateNamesTheFix(t *testing.T) {
	fastMinIOTimings(t)
	f, c := newResourceFake(t)
	managedMinIONamespace(f)
	putMinIODeployment(f, toolFieldManager, 1)
	f.fail["GET "+s3NPPath] = 403
	resp, _ := SetupMinIO(c)
	if resp.Success || resp.ErrorCode != "forbidden" || !strings.Contains(resp.Message, "nothing was changed") || !strings.Contains(resp.Message, "make upgrade") {
		t.Fatalf("setup = %+v", resp)
	}
	if m := f.mutations(); len(m) != 0 {
		t.Errorf("nothing may change: %v", m)
	}
}

// An interrupted migration (SeaweedFS serves, MinIO removed, its policy
// left) is finished by a re-run.
func TestSetupMinIO_RemovesLeftoverMinIOPolicy(t *testing.T) {
	fastMinIOTimings(t)
	f, c := newResourceFake(t)
	readyAfterApply(f)
	managedMinIONamespace(f)
	f.putJSON(minioNPPath, `{"metadata":{"labels":`+toolLabelJSON+`}}`)
	if resp, _ := SetupMinIO(c); !resp.Success || f.has(minioNPPath) {
		t.Fatalf("setup = %+v, policy kept = %v", resp, f.has(minioNPPath))
	}
}

func TestSetupMinIO_RouteRemovalFailureIsReported(t *testing.T) {
	fastMinIOTimings(t)
	f, c := newResourceFake(t)
	readyAfterApply(f)
	managedMinIONamespace(f)
	f.putJSON(minioAPIRoute, `{"metadata":{"uid":"api-uid","labels":`+toolLabelJSON+`}}`)
	f.fail["DELETE "+minioAPIRoute] = 403
	resp, _ := SetupMinIO(c)
	if resp.Success || resp.ErrorCode != "partial_failure" || !strings.Contains(resp.Message, "minio-api") {
		t.Fatalf("want partial_failure naming minio-api, got %+v", resp)
	}
}

func TestSetupPipelineServer_ForeignObjectsBetweenCheckAndWrite(t *testing.T) {
	cases := []struct {
		name, method, trigger, path string
	}{
		{"secret created before POST", http.MethodPost, "/api/v1/namespaces/p1/secrets", secretPath("p1", dspaSecretName)},
		{"DSPA created before POST", http.MethodPost, "/apis/" + dspaAPIGroup + "/namespaces/p1/datasciencepipelinesapplications", dspaPath("p1", dspaName)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newResourceFake(t)
			putReadyManagedMinIO(f)
			var once sync.Once
			f.beforeServe = func(method, path string) {
				if method == tc.method && path == tc.trigger {
					once.Do(func() { f.putJSON(tc.path, foreignJSON(`,"data":{"k":"dXNlcg=="}`)) })
				}
			}
			resp, _ := SetupPipelineServer(c, "p1")
			if resp.Success || resp.ErrorCode != "not_managed" {
				t.Fatalf("want not_managed, got %+v", resp)
			}
			assertForeignUntouched(t, f, tc.path, "data", "dXNlcg==")
		})
	}
	t.Run("owned secret replaced before update", func(t *testing.T) {
		f, c := newResourceFake(t)
		putReadyManagedMinIO(f)
		f.putJSON(secretPath("p1", dspaSecretName), `{"metadata":{"labels":`+toolLabelJSON+`,"resourceVersion":"3"}}`)
		var once sync.Once
		f.beforeServe = func(method, path string) {
			if method == http.MethodPatch && path == secretPath("p1", dspaSecretName) {
				once.Do(func() { f.putJSON(secretPath("p1", dspaSecretName), foreignJSON(`,"data":{"k":"dXNlcg=="}`)) })
			}
		}
		resp, _ := SetupPipelineServer(c, "p1")
		if resp.Success || resp.ErrorCode != "not_managed" {
			t.Fatalf("want not_managed, got %+v", resp)
		}
		assertForeignUntouched(t, f, secretPath("p1", dspaSecretName), "data", "dXNlcg==")
		if f.has(dspaPath("p1", dspaName)) {
			t.Error("no DSPA may be created after the secret was refused")
		}
	})
	t.Run("owned DSPA keeps changing", func(t *testing.T) {
		f, c := newResourceFake(t)
		putReadyManagedMinIO(f)
		putDSPA(f, "p1", dspaName, toolLabelJSON, toolFieldManager, "Apply", minioS3Host(), dspaSecretName, "", "")
		f.beforeServe = func(method, path string) {
			if method == http.MethodPatch && path == dspaPath("p1", dspaName) {
				f.mu.Lock()
				f.bumpRV(ensureMap(f.objects[fakeKey{gv: dspaAPIGroup, ns: "p1", plural: "datasciencepipelinesapplications", name: dspaName}], "metadata"))
				f.mu.Unlock()
			}
		}
		resp, _ := SetupPipelineServer(c, "p1")
		if resp.Success || resp.ErrorCode != "conflict" {
			t.Fatalf("want conflict, got %+v", resp)
		}
	})
}

func TestTeardownMinIO_DeletesOnlyOwnedObjectsAndKeepsNamespace(t *testing.T) {
	toolPaths := []string{s3DeployPath, s3PVCPath, minioDeployPath, minioPVCPath, minioSecretPath, minioSvcPath, minioUIRoute, minioAPIRoute, s3NPPath, minioNPPath}
	toolNS := func(f *resourceFake) {
		managedMinIONamespace(f)
		for _, p := range toolPaths {
			f.putJSON(p, `{"metadata":{"labels":`+toolLabelJSON+`}}`)
		}
		// Someone else's object elsewhere in the namespace.
		f.putJSON("/api/v1/namespaces/minio/configmaps/notes", foreignJSON(""))
	}
	t.Run("all owned", func(t *testing.T) {
		fastMinIOTimings(t)
		f, c := newResourceFake(t)
		toolNS(f)
		resp, _ := TeardownMinIO(c)
		if !resp.Success || !strings.Contains(resp.Message, "Delete it with `oc delete project minio` once you've checked it's empty.") {
			t.Fatalf("teardown = %+v", resp)
		}
		for _, p := range toolPaths {
			if f.has(p) {
				t.Errorf("%s was not deleted", p)
			}
		}
		if !f.has(minioNSPath) || !f.has("/api/v1/namespaces/minio/configmaps/notes") {
			t.Error("the namespace and everything else in it must be kept")
		}
	})
	for _, path := range toolPaths {
		t.Run("unlabelled "+path[strings.LastIndex(path, "/")+1:]+" is kept", func(t *testing.T) {
			fastMinIOTimings(t)
			f, c := newResourceFake(t)
			toolNS(f)
			f.putJSON(path, foreignJSON(""))
			resp, _ := TeardownMinIO(c)
			if !resp.Success || !strings.Contains(resp.Message, "did not create them") {
				t.Fatalf("teardown = %+v", resp)
			}
			assertForeignUntouched(t, f, path, "", "")
			for _, p := range toolPaths {
				if p != path && f.has(p) {
					t.Errorf("tool object %s was not deleted", p)
				}
			}
		})
	}
	t.Run("object replaced between check and delete", func(t *testing.T) {
		fastMinIOTimings(t)
		f, c := newResourceFake(t)
		toolNS(f)
		f.beforeServe = func(method, path string) {
			if method == http.MethodDelete && path == minioPVCPath {
				f.putJSON(minioPVCPath, foreignJSON(""))
			}
		}
		resp, _ := TeardownMinIO(c)
		if resp.Success || resp.ErrorCode != "partial_failure" {
			t.Fatalf("teardown = %+v", resp)
		}
		assertForeignUntouched(t, f, minioPVCPath, "", "")
	})
}

func TestMinIOEndpointAliases(t *testing.T) {
	ep := minioEndpoints{apiHost: "minio-api-minio.apps.example.com", uiHost: "minio-ui-minio.apps.example.com", clusterIP: "172.30.10.20"}
	cases := []struct {
		ns, host string
		want     bool
	}{
		{"team", "minio-service.minio.svc:9000", true},
		{"team", "minio-service.minio", true},
		{"team", "minio-service.minio.svc", true},
		{"team", "minio-service.minio.svc.cluster.local", true},
		{"team", "minio-service.minio.svc.cluster.local.", true},
		{"team", "MINIO-SERVICE.Minio.SVC:9000", true},
		{"team", "http://minio-service.minio.svc.cluster.local:9000", true},
		{"team", "https://minio-service.minio:9000/pipelines", true},
		{"team", "minio-service.minio.svc:bad", true},
		{"team", "172.30.10.20:9000", true},
		{"team", "https://minio-api-minio.apps.example.com", true},
		{"team", "minio-ui-minio.apps.example.com:443", true},
		{"minio", "minio-service", true},
		{"minio", "minio-service:9000", true},
		{"team", "minio-service", false}, // resolves to team's own service
		{"team", "minio-service.other.svc", false},
		{"team", "s3.amazonaws.com", false},
		{"team", "minio-service.minio.example.com", false},
		{"team", "", false},
	}
	for _, tc := range cases {
		if got := ep.usesMinIO(tc.ns, tc.host); got != tc.want {
			t.Errorf("usesMinIO(%q, %q) = %v, want %v", tc.ns, tc.host, got, tc.want)
		}
	}
	dspas := []dspaInfo{
		{Meta: objectMeta{Namespace: "a", Name: "x"}, Host: "http://minio-service.minio.svc.cluster.local:9000"},
		{Meta: objectMeta{Namespace: "b", Name: "y"}, Host: "minio-api-minio.apps.example.com"},
		{Meta: objectMeta{Namespace: "c", Name: "z"}, Host: "s3.amazonaws.com"},
	}
	if got := minioTeardownBlocker(dspas, ep); !strings.Contains(got, "a/x, b/y") || strings.Contains(got, "c/z") {
		t.Errorf("blocker = %q", got)
	}
}

// Teardown sees a pipeline server that uses the Route host.
func TestTeardownMinIO_BlockedByRouteHostDependency(t *testing.T) {
	fastMinIOTimings(t)
	f, c := newResourceFake(t)
	managedMinIONamespace(f)
	f.putJSON(minioUIRoute, `{"metadata":{"labels":`+toolLabelJSON+`},"spec":{"host":"minio-ui-minio.apps.example.com"}}`)
	putDSPA(f, "team", "dspa", `{}`, "Mozilla", "Update", "https://minio-ui-minio.apps.example.com", "s", "", "")
	resp, _ := TeardownMinIO(c)
	if resp.Success || resp.ErrorCode != "prerequisites" || !strings.Contains(resp.Message, "team/dspa") {
		t.Fatalf("teardown = %+v", resp)
	}
}

// stallServer answers 404 to nothing: every GET hangs until the client gives
// up, like an API server that stopped responding.
func stallServer(t *testing.T, status int) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != 0 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"kind":"Status","code":` + itoa(status) + `}`))
			return
		}
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	t.Cleanup(srv.Close)
	return &Client{baseURL: srv.URL, httpClient: srv.Client(), ctx: context.Background()}
}

func TestWaitForDeletion_DeadlineAndErrors(t *testing.T) {
	t.Run("stalled API call does not overrun the deadline", func(t *testing.T) {
		c := stallServer(t, 0)
		start := time.Now()
		gone, err := waitForDeletion(c, "/api/v1/namespaces/minio", 200*time.Millisecond, 10*time.Millisecond)
		if gone || err != nil {
			t.Fatalf("gone=%v err=%v, want a plain timeout", gone, err)
		}
		if d := time.Since(start); d > time.Second {
			t.Errorf("waited %s past a 200ms deadline", d)
		}
	})
	for _, status := range []int{401, 403, 500} {
		t.Run("HTTP "+itoa(status)+" is an error, not still-deleting", func(t *testing.T) {
			c := stallServer(t, status)
			gone, err := waitForDeletion(c, "/api/v1/namespaces/minio", time.Second, 10*time.Millisecond)
			if gone || !IsK8sError(err, status) {
				t.Fatalf("gone=%v err=%v", gone, err)
			}
		})
	}
	t.Run("caller context cancelled", func(t *testing.T) {
		c := stallServer(t, 0)
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		gone, err := waitForDeletion(c.WithContext(ctx), "/x", time.Second, 10*time.Millisecond)
		if gone || err == nil {
			t.Fatalf("gone=%v err=%v, want the caller's context error", gone, err)
		}
	})
}

func TestWaitMinIOReady_StalledAPIDoesNotOverrunDeadline(t *testing.T) {
	fastMinIOTimings(t)
	MinIOReadyTimeout = 200 * time.Millisecond
	c := stallServer(t, 0)
	start := time.Now()
	_, err := waitS3Ready(c, seaweedfsDefaultImage)
	if err != errS3NotReady {
		t.Fatalf("err = %v, want errS3NotReady", err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("waited %s past a 200ms deadline", d)
	}
}

func TestTeardownPipelineServer_ForbiddenWhileWaitingIsAnError(t *testing.T) {
	fastPipelineTimings(t)
	f, c := newResourceFake(t)
	putDSPA(f, "p1", dspaName, toolLabelJSON, toolFieldManager, "Apply", minioS3Host(), dspaSecretName, `,"finalizers":["`+dspaFinalizer+`"]`, "")
	putDSPO(f, 1)
	f.beforeServe = func(method, path string) {
		if method == http.MethodDelete && path == dspaPath("p1", dspaName) {
			f.mu.Lock()
			f.fail["GET "+dspaPath("p1", dspaName)] = 403
			f.mu.Unlock()
		}
	}
	resp, _ := TeardownPipelineServer(c, "p1")
	if resp.Success || resp.ErrorCode != "forbidden" || strings.Contains(resp.Message, "still terminating") {
		t.Fatalf("teardown = %+v", resp)
	}
}
