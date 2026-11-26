package cluster

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/clock/clocktest"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/workbench"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// deadlineAfter reaches the operation's deadline (it moves the injected
// clock past it) once the create of path has its whole answer.
type deadlineAfter struct {
	base http.RoundTripper
	path string
	clk  *clocktest.Fake
	by   time.Duration
}

func (d *deadlineAfter) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := d.base.RoundTrip(r)
	if err != nil || r.Method != http.MethodPost || r.URL.Path != d.path || r.URL.Query().Get("dryRun") != "" {
		return resp, err
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	d.clk.Advance(d.by)
	return resp, nil
}

// The operation's deadline right after a create of the workbench setup:
// the object is still recorded as this tool's (or deleted again), on the
// post-operation context, never left unrecorded. Before, the record write
// ran on the operation's own context, which had ended, and so did the
// delete meant to undo the create.
func TestWorkbenchSetupRecordsWhatItMadeAtItsDeadline(t *testing.T) {
	t.Setenv("NAMESPACE", "rhoai-nightly-updater")
	for _, tc := range []struct{ name, path, object string }{
		{"after the storage's create", wbPVCs, wbPVC},
		{"after the workbench's create", wbNotebooks, wbNotebook},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clk := clocktest.NewFake()
			t.Cleanup(kube.SetClockForTest(clk))
			f, c := newResourceFake(t)
			putWorkbenchCluster(t, f, true)
			ctx, cancel := clk.WithDeadline(context.Background(), clk.Now().Add(15*time.Minute))
			defer cancel()
			editTestClient(c, func(o *testClientOptions) {
				o.HTTPClient = &http.Client{Transport: &deadlineAfter{base: o.HTTPClient.Transport, path: tc.path, clk: clk, by: 16 * time.Minute}}
				o.Ctx = ctx
			})
			u := userClientOf(c, fakeUserToken)
			u.SetUsername("alice")
			if _, err := RunWorkbench(u, types.WorkbenchRequest{Action: "setup", Project: "proj"}, nil); err != nil {
				t.Fatal(err)
			}
			if ctx.Err() == nil {
				t.Fatal("the deadline was not reached")
			}
			obj := f.get(tc.object)
			if obj == nil {
				return // deleted again
			}
			rec, err := workbench.ReadRecord(c.WithContext(context.Background()))
			if err != nil {
				t.Fatal(err)
			}
			e, _ := rec.Get("proj")
			uid := obj["metadata"].(map[string]any)["uid"]
			if uid != e.StorageUID && uid != e.UID {
				t.Fatalf("%s (uid %v) is left unrecorded: %+v", tc.object, uid, e)
			}
		})
	}
}
