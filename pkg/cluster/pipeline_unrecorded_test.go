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
	resp, _ := TeardownPipelineServer(c, "proj", false)
	if resp.Success || resp.ErrorCode != "not_managed" || !strings.Contains(resp.Message, "no record") || !strings.Contains(resp.Message, "Nothing was deleted") {
		t.Fatalf("unconfirmed = %+v", resp)
	}
	if hasMutation(f, "DELETE "+dspaPath("proj", storage.DSPAName)) {
		t.Fatal("deleted an unrecorded pipeline server without confirmation")
	}
	f.finalizeOnGet = true
	if resp, _ := TeardownPipelineServer(c, "proj", true); !resp.Success {
		t.Fatalf("confirmed = %+v", resp)
	}
}

// Fail closed: when the record cannot be loaded, treat every pipeline server
// as unrecorded.
func TestTeardownPipelineServer_AnUnreadableRecordRefuses(t *testing.T) {
	fastPipelineTimings(t)
	f, c := newResourceFake(t)
	putDSPA(f, "proj", storage.DSPAName, toolLabelJSON, envkit.ToolFieldManager, "Apply", storage.S3Host(), storage.DSPASecretName, "", "")
	f.fail["GET "+rhoai.NamespacedPath("v1", "configmaps", record.GetActivityNamespace(), record.SnapshotConfigMapName)] = 500
	resp, _ := TeardownPipelineServer(c, "proj", false)
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
	defer func(f func(*Client, string, bool) (*types.OperationResponse, error)) { cleanupPipelineServer = f }(cleanupPipelineServer)
	got := map[string]bool{}
	cleanupPipelineServer = func(_ *Client, project string, confirmed bool) (*types.OperationResponse, error) {
		got[project] = confirmed
		return &types.OperationResponse{Success: true}, nil
	}
	_, c := newResourceFake(t)
	if res, _ := CleanUp(c, types.CleanupRequest{PipelineServers: []string{"a", "b"}, ConfirmUnrecorded: []string{"b"}}, nil); !res.Success {
		t.Fatalf("clean-up = %+v", res)
	}
	if got["a"] || !got["b"] {
		t.Fatalf("confirmed = %v", got)
	}
}
