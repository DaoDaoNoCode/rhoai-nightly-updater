package cluster

import (
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/storage"
)

// putOldToolServer stores a pipeline server this tool set up under its
// earlier name, nightly-dspa, with its labelled S3 secret.
func putOldToolServer(f *resourceFake, ns, extraMeta string) {
	putDSPA(f, ns, storage.PreviousDSPAName, toolLabelJSON, envkit.ToolFieldManager, "Apply", storage.S3Host(), storage.DSPASecretName, extraMeta,
		`{"conditions":[{"type":"APIServerReady","status":"True"},{"type":"Ready","status":"True"}]}`)
	f.putJSON(secretPath(ns, storage.DSPASecretName), `{"metadata":{"labels":`+toolLabelJSON+`}}`)
}

func requestIndex(f *resourceFake, req string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, r := range f.requests {
		if r == req {
			return i
		}
	}
	return -1
}

// The status names a server with the earlier name, so the Pipelines tab can
// explain that workbench pipelines need dspa and offer Recreate.
func TestGetResourcesStatus_FlagsTheEarlierName(t *testing.T) {
	f, c := newResourceFake(t)
	for _, ns := range []string{"old", "new"} {
		putDSProject(f, ns)
	}
	putOldToolServer(f, "old", "")
	putDSPA(f, "new", storage.DSPAName, toolLabelJSON, envkit.ToolFieldManager, "Apply", storage.S3Host(), storage.DSPASecretName, "", "")
	st, err := GetResourcesStatus(c)
	if err != nil || len(st.PipelineServers) != 2 {
		t.Fatalf("st=%+v err=%v", st, err)
	}
	for _, ps := range st.PipelineServers {
		if want := ps.Namespace == "old"; ps.NonStandardName != want {
			t.Errorf("%s/%s NonStandardName = %v, want %v", ps.Namespace, ps.Name, ps.NonStandardName, want)
		}
	}
}

func TestRecreatePipelineServer_ReplacesTheEarlierName(t *testing.T) {
	fastPipelineTimings(t)
	f, c := newResourceFake(t)
	putManagedDSC(f)
	putReadyManagedS3(f)
	putDSProject(f, "proj")
	putOldToolServer(f, "proj", "")

	resp, _ := RecreatePipelineServer(c, "proj", toolDSPAUID(c, "proj"), nil)
	if !resp.Success {
		t.Fatalf("recreate failed: %+v", resp)
	}
	if f.has(dspaPath("proj", storage.PreviousDSPAName)) {
		t.Error("nightly-dspa is still there")
	}
	if !f.has(dspaPath("proj", storage.DSPAName)) {
		t.Fatal("dspa was not created")
	}
	assertOwnedByDSPA(t, f, secretPath("proj", storage.DSPASecretName), "proj", storage.DSPAName)
	// Everything the new server needs was checked with the API server
	// before the old one was deleted.
	del := requestIndex(f, "DELETE "+dspaPath("proj", storage.PreviousDSPAName))
	dry := requestIndex(f, "DRYRUN POST /apis/"+storage.DSPAAPIGroup+"/namespaces/proj/datasciencepipelinesapplications")
	if del < 0 || dry < 0 || dry > del {
		t.Errorf("dry run of dspa at %d, delete of nightly-dspa at %d: the dry run must come first", dry, del)
	}
	if !strings.Contains(resp.Message, storage.DSPAName) || !strings.Contains(resp.Message, storage.PreviousDSPAName) {
		t.Errorf("message = %q", resp.Message)
	}
}

// Every refusal comes before the old server is deleted.
func TestRecreatePipelineServer_RefusalsDeleteNothing(t *testing.T) {
	cases := []struct {
		name     string
		setup    func(f *resourceFake)
		wantCode string
	}{
		{"new pipeline server rejected by the API server", func(f *resourceFake) {
			putReadyManagedS3(f)
			putDSProject(f, "proj")
			putOldToolServer(f, "proj", "")
			f.failDryRun["POST /apis/"+storage.DSPAAPIGroup+"/namespaces/proj/datasciencepipelinesapplications"] = 422
		}, ""},
		{"S3 storage not ready", func(f *resourceFake) {
			putDSProject(f, "proj")
			putOldToolServer(f, "proj", "")
		}, "prerequisites"},
		{"not a Data Science project", func(f *resourceFake) {
			putReadyManagedS3(f)
			putOldToolServer(f, "proj", "")
		}, "validation"},
		{"old server already terminating", func(f *resourceFake) {
			putReadyManagedS3(f)
			putDSProject(f, "proj")
			putOldToolServer(f, "proj", `,"deletionTimestamp":"`+created+`","finalizers":["`+storage.DSPAFinalizer+`"]`)
		}, "terminating"},
		{"another pipeline server in the project", func(f *resourceFake) {
			putReadyManagedS3(f)
			putDSProject(f, "proj")
			putOldToolServer(f, "proj", "")
			putDSPA(f, "proj", "other", `{}`, "Mozilla", "Update", "s3.amazonaws.com", "secret-abcdef", "", "")
		}, "not_managed"},
		{"nightly-dspa not created by this tool", func(f *resourceFake) {
			putReadyManagedS3(f)
			putDSProject(f, "proj")
			putDSPA(f, "proj", storage.PreviousDSPAName, `{}`, "Mozilla", "Update", "s3.amazonaws.com", "secret-abcdef", "", "")
		}, "not_managed"},
		{"operator not running for the finalizer", func(f *resourceFake) {
			putReadyManagedS3(f)
			putDSProject(f, "proj")
			putOldToolServer(f, "proj", `,"finalizers":["`+storage.DSPAFinalizer+`"]`)
			putDSPO(f, 0)
		}, "prerequisites"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fastPipelineTimings(t)
			f, c := newResourceFake(t)
			putManagedDSC(f)
			tc.setup(f)
			resp, _ := RecreatePipelineServer(c, "proj", toolDSPAUID(c, "proj"), nil)
			if resp.Success || (tc.wantCode != "" && resp.ErrorCode != tc.wantCode) {
				t.Fatalf("got %+v", resp)
			}
			for _, m := range f.mutations() {
				if strings.HasPrefix(m, "DELETE") || strings.Contains(m, "datasciencepipelinesapplications") || strings.Contains(m, "/secrets") {
					t.Errorf("refusal changed %s", m)
				}
			}
		})
	}
}

// An old server whose finalizer does not finish in time: nothing new is
// created next to it, and the message says what to do.
func TestRecreatePipelineServer_OldServerStillTerminating(t *testing.T) {
	fastPipelineTimings(t)
	f, c := newResourceFake(t)
	putManagedDSC(f)
	putReadyManagedS3(f)
	putDSProject(f, "proj")
	putOldToolServer(f, "proj", `,"finalizers":["`+storage.DSPAFinalizer+`"]`)
	putDSPO(f, 1)
	resp, _ := RecreatePipelineServer(c, "proj", toolDSPAUID(c, "proj"), nil)
	if resp.Success || resp.ErrorCode != "in_progress" || !strings.Contains(resp.Message, "Set up") {
		t.Fatalf("got %+v", resp)
	}
	if f.has(dspaPath("proj", storage.DSPAName)) {
		t.Error("dspa created while nightly-dspa is still terminating")
	}
}

// The earlier name's teardown still works.
func TestTeardownPipelineServer_EarlierName(t *testing.T) {
	fastPipelineTimings(t)
	f, c := newResourceFake(t)
	putOldToolServer(f, "proj", "")
	resp, _ := TeardownPipelineServer(c, "proj", toolDSPAUID(c, "proj"), nil)
	if !resp.Success || f.has(dspaPath("proj", storage.PreviousDSPAName)) || f.has(secretPath("proj", storage.DSPASecretName)) {
		t.Fatalf("got %+v", resp)
	}
	if !strings.Contains(resp.Message, "mariadb-"+storage.PreviousDSPAName) {
		t.Errorf("message = %q", resp.Message)
	}
}
