package cluster

import (
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/storage"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// G7: pipeline servers that wear this tool's label yet lack a UID in this
// tool's bookkeeping—whether installed before 3.0.1, relabelled, or
// re-created under the same name by someone else in the project—get removed
// only after the user confirms they belong to this tool, per Migrate storage
// and Set up again.
func TestTeardownPipelineServer_AnUnrecordedServerNeedsConfirmation(t *testing.T) {
	fastPipelineTimings(t)
	f, c := newResourceFake(t)
	putDSPA(f, "proj", storage.DSPAName, toolLabelJSON, envkit.ToolFieldManager, "Apply", storage.S3Host(), storage.DSPASecretName, "", "")
	resp, _ := TeardownPipelineServer(c, "proj", "", nil)
	if resp.Success || resp.ErrorCode != "not_managed" || !strings.Contains(resp.Message, "no record") || !strings.Contains(resp.Message, "Nothing was deleted") {
		t.Fatalf("unconfirmed = %+v", resp)
	}
	if hasMutation(f, "DELETE "+dspaPath("proj", storage.DSPAName)) {
		t.Fatal("deleted an unrecorded pipeline server without confirmation")
	}
	f.finalizeOnGet = true
	if resp, _ := TeardownPipelineServer(c, "proj", dspaUID(f, "proj", storage.DSPAName), nil); !resp.Success {
		t.Fatalf("confirmed = %+v", resp)
	}
}

// toolDSPAUID is the UID of the pipeline server a teardown in ns acts on
// (the first of this tool's; "" when there is none): what the user
// confirms in the Delete dialog.
func toolDSPAUID(c *Client, ns string) string {
	all, _ := storage.ListDSPAs(c, ns)
	for _, d := range all {
		if d.ManagedByTool() {
			return d.Meta.UID
		}
	}
	return ""
}

// dspaUID is the UID of pipeline server name in ns, as the fake gave it.
func dspaUID(f *resourceFake, ns, name string) string {
	uid, _ := ensureMap(f.get(dspaPath(ns, name)), "metadata")["uid"].(string)
	return uid
}

// A confirmation names the pipeline server the user saw: its UID. The
// status gives it with Unrecorded; a server made again since (another UID)
// is not the one confirmed, so the ownership check still runs and refuses.
func TestTeardownPipelineServer_AConfirmationIsOfTheServerTheUserSaw(t *testing.T) {
	fastPipelineTimings(t)
	f, c := newResourceFake(t)
	putDSProject(f, "proj")
	putDSPA(f, "proj", storage.DSPAName, toolLabelJSON, envkit.ToolFieldManager, "Apply", storage.S3Host(), storage.DSPASecretName, "", "")
	st, err := GetResourcesStatus(c)
	if err != nil {
		t.Fatal(err)
	}
	uid := dspaUID(f, "proj", storage.DSPAName)
	if len(st.PipelineServers) != 1 || !st.PipelineServers[0].Unrecorded || st.PipelineServers[0].UID != uid || uid == "" {
		t.Fatalf("status = %+v (uid %q)", st.PipelineServers, uid)
	}
	resp, _ := TeardownPipelineServer(c, "proj", "uid-of-the-server-the-user-saw", nil)
	if resp.Success || resp.ErrorCode != "not_managed" || !strings.Contains(resp.Message, "not the one you confirmed") || !strings.Contains(resp.Message, "Nothing was deleted") {
		t.Fatalf("other uid = %+v", resp)
	}
	if hasMutation(f, "DELETE "+dspaPath("proj", storage.DSPAName)) {
		t.Fatal("deleted a pipeline server the user did not confirm")
	}
}

// Fail closed: when the record cannot be loaded, treat every pipeline server
// as unrecorded.
func TestTeardownPipelineServer_AnUnreadableRecordRefuses(t *testing.T) {
	fastPipelineTimings(t)
	f, c := newResourceFake(t)
	putDSPA(f, "proj", storage.DSPAName, toolLabelJSON, envkit.ToolFieldManager, "Apply", storage.S3Host(), storage.DSPASecretName, "", "")
	f.fail["GET "+rhoai.NamespacedPath("v1", "configmaps", record.GetActivityNamespace(), record.SnapshotConfigMapName)] = 500
	resp, _ := TeardownPipelineServer(c, "proj", "", nil)
	if resp.Success || !strings.Contains(resp.Message, "Nothing was deleted") {
		t.Fatalf("got %+v", resp)
	}
	if hasMutation(f, "DELETE "+dspaPath("proj", storage.DSPAName)) {
		t.Fatal("deleted with the record unreadable")
	}
}

// Clean up propagates the confirmation flag only for projects the user
// affirmed; all other projects receive no confirmation.
func TestCleanUp_PassesTheConfirmationOfUnrecordedPipelineServers(t *testing.T) {
	defer func(f func(*Client, string, string, []string) (*types.OperationResponse, error)) {
		cleanupPipelineServer = f
	}(cleanupPipelineServer)
	got := map[string]string{}
	cleanupPipelineServer = func(_ *Client, project, confirmedUID string, _ []string) (*types.OperationResponse, error) {
		got[project] = confirmedUID
		return &types.OperationResponse{Success: true}, nil
	}
	_, c := newResourceFake(t)
	if res, _ := CleanUp(c, types.CleanupRequest{PipelineServers: []string{"a", "b"}, ConfirmUnrecorded: map[string]string{"b": "uid-b"}}, nil); !res.Success {
		t.Fatalf("clean-up = %+v", res)
	}
	if got["a"] != "" || got["b"] != "uid-b" {
		t.Fatalf("confirmed = %v", got)
	}
}
