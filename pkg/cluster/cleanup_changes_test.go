package cluster

import (
	"context"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/recipe"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/storage"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// trackedOperationClient is c as pkg/api withMutationAuth hands it to an
// operation: its requests are recorded in the returned Changes, and the
// updater's records are written with a bookkeeper of their own.
func trackedOperationClient(c *Client) (*Client, *Changes) {
	o := c.TestOptions()
	c.SetBookkeeping(newTestClient(testClientOptions{BaseURL: o.BaseURL, Token: fakeSAToken, HTTPClient: o.HTTPClient, Ctx: context.Background()}))
	ctx, ch := TrackChanges(context.Background())
	return c.WithContext(ctx), ch
}

// The clean-up's confirmation refusals (N9) send nothing that may change
// the cluster, the activity record of the refusal included, so pkg/api gives
// the rate-limit slot back and the user's confirmed retry runs at once. The
// confirmed clean-up does change it.
func TestCleanUp_ConfirmRefusalsSendNoChange(t *testing.T) {
	t.Run("project contents", func(t *testing.T) {
		f, c, uid := createdRecipeProject(t)
		f.putJSON("/apis/kubeflow.org/v1/namespaces/recipe-dev/notebooks/wb", `{}`)
		req := types.CleanupRequest{RecipeProjects: []string{"recipe-dev"}, RecipeProjectUIDs: map[string]string{"recipe-dev": uid}}
		op, changes := trackedOperationClient(c)
		res, err := CleanUp(op, req, nil)
		if err != nil || res.Success || res.ErrorCode != recipe.ErrorCodeConfirmContents {
			t.Fatalf("got %+v, %v", res, err)
		}
		if changes.Made() {
			t.Fatal("the refusal sent a change")
		}
		req.ConfirmProjectContents = map[string][]string{"recipe-dev": {"Notebook wb"}}
		op, changes = trackedOperationClient(c)
		if res, err := CleanUp(op, req, nil); err != nil || !res.Success || f.has(tRecipeNS) {
			t.Fatalf("confirmed: %+v, %v", res, err)
		}
		if !changes.Made() {
			t.Fatal("the confirmed clean-up deleted the project without counting a change")
		}
	})
	t.Run("connection workbenches", func(t *testing.T) {
		fastPipelineTimings(t)
		f, c := newResourceFake(t)
		putConnectionUsers(f)
		op, changes := trackedOperationClient(c)
		res, err := CleanUp(op, types.CleanupRequest{PipelineServers: []string{"proj"}, ConfirmUnrecorded: map[string]string{"proj": toolDSPAUID(c, "proj")}}, nil)
		if err != nil || res.Success || res.ErrorCode != storage.ErrorCodeConfirmWorkbenches {
			t.Fatalf("got %+v, %v", res, err)
		}
		if changes.Made() {
			t.Fatal("the refusal sent a change")
		}
	})
}
