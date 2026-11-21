package cluster

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/storage"
)

// The DSPA list WaitPipelineServerReady polls.
const readyWaitDSPAList = "/apis/" + storage.DSPAAPIGroup + "/namespaces/proj/datasciencepipelinesapplications"

// putWaitDSPA stores a DSPA in project proj created at created, with the
// tool's label when tool, extra metadata and status.
func putWaitDSPA(f *resourceFake, name string, tool bool, created time.Time, extraMeta, status string) {
	labels := `{}`
	if tool {
		labels = toolLabelJSON
	}
	if status == "" {
		status = `{}`
	}
	f.putJSON(dspaPath("proj", name), `{"metadata":{"labels":`+labels+`,"creationTimestamp":"`+created.UTC().Format(time.RFC3339)+`","managedFields":`+managedFieldsJSON(envkit.ToolFieldManager, "Apply")+extraMeta+`},
		"spec":{"objectStorage":{"externalStorage":{"host":"`+storage.S3Host()+`","s3CredentialsSecret":{"secretName":"`+storage.DSPASecretName+`"}}}},"status":`+status+`}`)
}

const (
	dspaReadyStatus    = `{"conditions":[{"type":"Ready","status":"True","reason":"Ready"},{"type":"APIServerReady","status":"True"}]}`
	dspaNotReadyStatus = `{"conditions":[{"type":"Ready","status":"False","reason":"Deploying","message":"Waiting for the API server"},{"type":"APIServerReady","status":"False"}]}`
)

func fastPipelineReadyWait(t *testing.T) {
	t.Helper()
	t.Cleanup(storage.SetPipelineServerReadyTimingForTest(time.Minute, time.Second))
}

// Provisioning, then not ready, then ready: the wait polls until it is
// ready and returns its state.
func TestWaitPipelineServerReady_PollsUntilReady(t *testing.T) {
	fastPipelineReadyWait(t)
	f, c := newResourceFake(t)
	recent := time.Now()
	putWaitDSPA(f, storage.DSPAName, true, recent, "", "")
	// A server the dashboard created in the same project is not the tool's.
	putWaitDSPA(f, "other", false, recent, "", dspaReadyStatus)
	lists := 0
	f.beforeServe = func(method, path string) {
		if method != http.MethodGet || path != readyWaitDSPAList {
			return
		}
		lists++
		switch lists {
		case 2:
			putWaitDSPA(f, storage.DSPAName, true, recent, "", dspaNotReadyStatus)
		case 3:
			putWaitDSPA(f, storage.DSPAName, true, recent, "", dspaReadyStatus)
		}
	}
	state, err := storage.WaitPipelineServerReady(c, "proj")
	if err != nil {
		t.Fatal(err)
	}
	if !state.Ready || state.Name != storage.DSPAName || state.Namespace != "proj" || lists != 3 {
		t.Fatalf("state %+v after %d lists", state, lists)
	}
}

// Waiting cannot help: stuck (the API server is not ready DSPAStuckAfter
// after creation), being deleted, or no pipeline server of the tool's in
// the project. Each ends at the first poll with ErrPipelineServerFailed and
// says why.
func TestWaitPipelineServerReady_StopsAtATerminalState(t *testing.T) {
	cases := []struct {
		name string
		put  func(f *resourceFake)
		want string
	}{
		{"stuck", func(f *resourceFake) {
			putWaitDSPA(f, storage.DSPAName, true, time.Now().Add(-storage.DSPAStuckAfter-time.Minute), "", dspaNotReadyStatus)
		}, "is stuck: its API server is not ready 5m0s after it was created (Waiting for the API server)"},
		{"being deleted", func(f *resourceFake) {
			putWaitDSPA(f, storage.DSPAName, true, time.Now(), `,"deletionTimestamp":"2026-11-21T10:00:00Z"`, dspaNotReadyStatus)
		}, "the pipeline server dspa in proj is being deleted"},
		{"gone", func(f *resourceFake) {
			putWaitDSPA(f, "other", false, time.Now(), "", dspaNotReadyStatus)
		}, "project proj has no pipeline server set up by this tool"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fastPipelineReadyWait(t)
			f, c := newResourceFake(t)
			tc.put(f)
			_, err := storage.WaitPipelineServerReady(c, "proj")
			if !errors.Is(err, storage.ErrPipelineServerFailed) || errors.Is(err, storage.ErrPipelineServerNotReady) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v, want ErrPipelineServerFailed with %q", err, tc.want)
			}
			if n := len(f.callsTo(http.MethodGet, readyWaitDSPAList)); n != 1 {
				t.Fatalf("%d lists, want 1", n)
			}
		})
	}
}

// Never ready: the wait ends after its timeout (on the fake clock),
// polling every interval, with the last state's message.
func TestWaitPipelineServerReady_TimesOut(t *testing.T) {
	t.Cleanup(storage.SetPipelineServerReadyTimingForTest(30*time.Second, 10*time.Second))
	f, c := newResourceFake(t)
	putWaitDSPA(f, storage.DSPAName, true, time.Now(), "", "")
	start := kube.Clock().Now()
	state, err := storage.WaitPipelineServerReady(c, "proj")
	if !errors.Is(err, storage.ErrPipelineServerNotReady) || !strings.Contains(err.Error(), "after 30s: the pipeline server in proj: Provisioning") {
		t.Fatalf("err %v", err)
	}
	if state.Ready || state.Message != "Provisioning" {
		t.Fatalf("state %+v", state)
	}
	if waited := kube.Clock().Since(start); waited < 30*time.Second {
		t.Fatalf("gave up after %s", waited)
	}
	if n := len(f.callsTo(http.MethodGet, readyWaitDSPAList)); n < 3 || n > 4 {
		t.Fatalf("%d lists in 30s at a 10s poll", n)
	}
}

// A read that fails for another reason than the network is returned at
// once: the API is not installed (ErrDSPACRDMissing) or not allowed. A
// network error is retried, and an ended client context is returned.
func TestWaitPipelineServerReady_ReadErrors(t *testing.T) {
	fastPipelineReadyWait(t)
	f, c := newResourceFake(t)
	f.fail["GET "+readyWaitDSPAList] = http.StatusNotFound
	if _, err := storage.WaitPipelineServerReady(c, "proj"); !errors.Is(err, storage.ErrDSPACRDMissing) {
		t.Fatalf("no CRD: %v", err)
	}
	f.fail["GET "+readyWaitDSPAList] = http.StatusForbidden
	_, err := storage.WaitPipelineServerReady(c, "proj")
	if err == nil || errors.Is(err, storage.ErrPipelineServerNotReady) || !kube.IsK8sError(err, http.StatusForbidden) {
		t.Fatalf("forbidden: %v", err)
	}
	if n := len(f.callsTo(http.MethodGet, readyWaitDSPAList)); n != 2 {
		t.Fatalf("%d lists, want one per wait", n)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := storage.WaitPipelineServerReady(c.WithContext(ctx), "proj"); !errors.Is(err, context.Canceled) {
		t.Fatalf("ended context: %v", err)
	}
}

// A list whose connection breaks (a network error) is retried at the next
// poll, not returned.
func TestWaitPipelineServerReady_RetriesANetworkError(t *testing.T) {
	fastPipelineReadyWait(t)
	f, _ := newResourceFake(t)
	putWaitDSPA(f, storage.DSPAName, true, time.Now(), "", dspaReadyStatus)
	lists := 0
	srv := newFakeServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == readyWaitDSPAList {
			lists++
			if lists == 1 {
				panic(http.ErrAbortHandler) // the server drops the connection
			}
		}
		f.serve(w, r)
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(testClientOptions{BaseURL: srv.URL, Token: fakeSAToken, HTTPClient: srv.Client(), Ctx: context.Background()})
	start := kube.Clock().Now()
	state, err := storage.WaitPipelineServerReady(c, "proj")
	if err != nil || !state.Ready || lists != 2 {
		t.Fatalf("state %+v, err %v after %d lists", state, err, lists)
	}
	// Retried by the wait one poll later, not by the HTTP transport.
	if waited := kube.Clock().Since(start); waited < time.Second {
		t.Fatalf("retried after %s", waited)
	}
}
