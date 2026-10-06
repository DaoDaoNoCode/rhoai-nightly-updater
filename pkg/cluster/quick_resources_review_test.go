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
	for _, path := range []string{minioPVCPath, minioSecretPath, minioDeployPath, minioSvcPath, minioUIRoute, minioAPIRoute} {
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
	f.putJSON(minioSecretPath, `{"metadata":{"labels":`+toolLabelJSON+`,"resourceVersion":"5"},"data":{"minio_root_user":"dQ==","minio_root_password":"cA=="}}`)
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
// objects created by server-side apply, image quay.io/minio/minio:latest,
// which no longer pulls anonymously) switches only the image, labels the
// objects, keeps the PVC and the console Route, and removes the minio-api
// Route that exposed the S3 API.
func TestSetupMinIO_ReleasedInstallSwitchesImageAndDropsAPIRoute(t *testing.T) {
	fastMinIOTimings(t)
	f, c := newResourceFake(t)
	readyAfterApply(f)
	putNamespace(f, `{}`, legacyPostManager)
	applied := func(uid string) string {
		return `{"metadata":{"uid":"` + uid + `","creationTimestamp":"` + created + `","managedFields":` + managedFieldsJSON(toolFieldManager, "Apply") + `}}`
	}
	f.putJSON(minioDeployPath, `{"metadata":{"uid":"dep","creationTimestamp":"`+created+`","managedFields":`+managedFieldsJSON(toolFieldManager, "Apply")+`},
		"spec":{"template":{"spec":{"containers":[{"name":"minio","image":"quay.io/minio/minio:latest"}]}}}}`)
	f.putJSON(minioPVCPath, applied("pvc"))
	f.putJSON(minioAPIRoute, `{"metadata":{"uid":"api","creationTimestamp":"`+created+`","managedFields":`+managedFieldsJSON(toolFieldManager, "Apply")+`},"spec":{"host":"minio-api-minio.apps.example.com"}}`)
	f.putJSON(minioUIRoute, applied("ui"))
	if st := getMinIOStatus(c); !strings.Contains(st.Warning, "quay.io/minio/minio:latest") || !strings.Contains(st.Warning, "minio-api") {
		t.Errorf("status warning = %q", st.Warning)
	}

	resp, _ := SetupMinIO(c)
	if !resp.Success {
		t.Fatalf("setup = %+v", resp)
	}
	raw, _ := json.Marshal(f.get(minioDeployPath))
	if !strings.Contains(string(raw), minioDefaultImage) {
		t.Errorf("image not switched: %s", raw)
	}
	if f.has(minioAPIRoute) || !f.has(minioUIRoute) {
		t.Errorf("api route present=%v, console route present=%v", f.has(minioAPIRoute), f.has(minioUIRoute))
	}
	pvc := f.get(minioPVCPath)["metadata"].(map[string]interface{})
	if pvc["uid"] != "pvc" || pvc["labels"].(map[string]interface{})[managedByLabelKey] != managedByLabelValue {
		t.Errorf("the data PVC must be kept and labelled: %v", pvc)
	}
	if hasMutation(f, "DELETE "+minioPVCPath) || hasMutation(f, "POST /api/v1/namespaces/minio/persistentvolumeclaims") {
		t.Error("the data PVC must not be recreated")
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

func TestTeardownMinIO_ForeignObjectsKeepNamespace(t *testing.T) {
	toolNS := func(f *resourceFake) {
		managedMinIONamespace(f)
		f.putJSON(minioDeployPath, `{"metadata":{"uid":"dep-uid","labels":`+toolLabelJSON+`}}`)
		f.putJSON("/apis/apps/v1/namespaces/minio/replicasets/minio-abc", `{"metadata":{"uid":"rs-uid","ownerReferences":[{"kind":"Deployment","name":"minio","uid":"dep-uid"}]}}`)
		f.putJSON("/api/v1/namespaces/minio/pods/minio-abc-1", `{"metadata":{"labels":{"app":"minio"},"ownerReferences":[{"kind":"ReplicaSet","name":"minio-abc","uid":"rs-uid"}]}}`)
		f.putJSON(minioPVCPath, `{"metadata":{"uid":"pvc-uid","labels":`+toolLabelJSON+`}}`)
		f.putJSON(minioSecretPath, `{"metadata":{"labels":`+toolLabelJSON+`},"type":"Opaque"}`)
		f.putJSON(minioSvcPath, `{"metadata":{"labels":`+toolLabelJSON+`}}`)
		f.putJSON(minioUIRoute, `{"metadata":{"labels":`+toolLabelJSON+`}}`)
		// Created by OpenShift and RHOAI in every namespace.
		f.putJSON("/api/v1/namespaces/minio/secrets/default-dockercfg-x", `{"type":"kubernetes.io/dockercfg","metadata":{"ownerReferences":[{"kind":"ServiceAccount","name":"default","uid":"sa"}]}}`)
		for _, cm := range []string{"kube-root-ca.crt", "openshift-service-ca.crt", "odh-trusted-ca-bundle", "odh-kserve-custom-ca-bundle"} {
			f.putJSON("/api/v1/namespaces/minio/configmaps/"+cm, `{}`)
		}
	}
	toolPaths := []string{minioDeployPath, minioPVCPath, minioSecretPath, minioSvcPath, minioUIRoute}
	cases := []struct {
		name, path, body, kept string
	}{
		{"secret", "/api/v1/namespaces/minio/secrets/my-tls", `,"type":"kubernetes.io/tls"`, "Secret my-tls"},
		{"configmap", "/api/v1/namespaces/minio/configmaps/notes", ``, "ConfigMap notes"},
		{"deployment", "/apis/apps/v1/namespaces/minio/deployments/mc", ``, "Deployment mc"},
		{"statefulset", "/apis/apps/v1/namespaces/minio/statefulsets/db", ``, "StatefulSet db"},
		{"bare pod", "/api/v1/namespaces/minio/pods/debug", ``, "Pod debug"},
		{"service", "/api/v1/namespaces/minio/services/other", ``, "Service other"},
		{"route", "/apis/route.openshift.io/v1/namespaces/minio/routes/other", ``, "Route other"},
		{"unlabelled object with a tool name", minioSecretPath, `,"type":"Opaque"`, "Secret minio-secret"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fastMinIOTimings(t)
			f, c := newResourceFake(t)
			toolNS(f)
			f.putJSON(tc.path, foreignJSON(tc.body))
			resp, _ := TeardownMinIO(c)
			if !resp.Success || !strings.Contains(resp.Message, "was kept") || !strings.Contains(resp.Message, tc.kept) {
				t.Fatalf("teardown = %+v", resp)
			}
			if hasExactMutation(f, "DELETE "+minioNSPath) || !f.has(minioNSPath) {
				t.Fatal("the namespace must be kept")
			}
			assertForeignUntouched(t, f, tc.path, "", "")
			for _, p := range toolPaths {
				if p != tc.path && f.has(p) {
					t.Errorf("tool object %s was not deleted", p)
				}
			}
		})
	}
	t.Run("only tool and platform objects: namespace deleted", func(t *testing.T) {
		fastMinIOTimings(t)
		f, c := newResourceFake(t)
		toolNS(f)
		resp, _ := TeardownMinIO(c)
		if !resp.Success || !hasExactMutation(f, "DELETE "+minioNSPath) {
			t.Fatalf("teardown = %+v (%v)", resp, f.mutations())
		}
	})
	t.Run("inventory list forbidden deletes nothing", func(t *testing.T) {
		fastMinIOTimings(t)
		f, c := newResourceFake(t)
		toolNS(f)
		f.fail["GET /api/v1/namespaces/minio/secrets"] = 403
		resp, _ := TeardownMinIO(c)
		if resp.Success || resp.ErrorCode != "forbidden" || len(f.mutations()) != 0 {
			t.Fatalf("teardown = %+v, mutations %v", resp, f.mutations())
		}
	})
	t.Run("individual delete uses UID preconditions", func(t *testing.T) {
		fastMinIOTimings(t)
		f, c := newResourceFake(t)
		toolNS(f)
		f.putJSON("/api/v1/namespaces/minio/configmaps/notes", foreignJSON(""))
		// The PVC is replaced between the inventory and the delete.
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
	_, err := waitMinIOReady(c, minioDefaultImage)
	if err != errMinIONotReady {
		t.Fatalf("err = %v, want errMinIONotReady", err)
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
