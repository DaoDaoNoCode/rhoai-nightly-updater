package cluster

// Regression tests for the review-round findings on the quick resources:
// foreign objects appearing between the tool's check and its write, and API
// calls that stall past an operation's deadline. The S3 storage's are in
// s3_storage_test.go.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
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
	meta := obj["metadata"].(map[string]any)
	labels, _ := meta["labels"].(map[string]any)
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
			putManagedDSC(f)
			putReadyManagedS3(f)
			putDSProject(f, "p1")
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
		putManagedDSC(f)
		putReadyManagedS3(f)
		putDSProject(f, "p1")
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
		putManagedDSC(f)
		putReadyManagedS3(f)
		putDSProject(f, "p1")
		putDSPA(f, "p1", dspaName, toolLabelJSON, toolFieldManager, "Apply", s3Host(), dspaSecretName, "", "")
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

// stallServer answers every request with status at once, or, for status
// 0, like an API server that stopped responding: each request takes 5s on
// the fake clock (unless its context ends first), then gets an empty 200.
func stallServer(t *testing.T, status int) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != 0 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"kind":"Status","code":` + itoa(status) + `}`))
		}
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(testClientOptions{BaseURL: srv.URL, HTTPClient: srv.Client(), Ctx: context.Background()})
	if status == 0 {
		return withLatency(c, 5*time.Second)
	}
	return c
}

func TestWaitForDeletion_DeadlineAndErrors(t *testing.T) {
	t.Run("stalled API call does not overrun the deadline", func(t *testing.T) {
		c := stallServer(t, 0)
		start := clk.Now()
		gone, err := waitForDeletion(c, "/api/v1/namespaces/minio", 200*time.Millisecond, 10*time.Millisecond)
		if gone || err != nil {
			t.Fatalf("gone=%v err=%v, want a plain timeout", gone, err)
		}
		if d := clk.Since(start); d != 200*time.Millisecond {
			t.Errorf("waited %s, want the 200ms deadline", d)
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
		ctx, cancel := clk.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		gone, err := waitForDeletion(c.WithContext(ctx), "/x", time.Second, 10*time.Millisecond)
		if gone || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("gone=%v err=%v, want the caller's context error", gone, err)
		}
	})
}

func TestTeardownPipelineServer_ForbiddenWhileWaitingIsAnError(t *testing.T) {
	fastPipelineTimings(t)
	f, c := newResourceFake(t)
	putDSPA(f, "p1", dspaName, toolLabelJSON, toolFieldManager, "Apply", s3Host(), dspaSecretName, `,"finalizers":["`+dspaFinalizer+`"]`, "")
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
