package cluster

import (
	"net/http"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/serving"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// A model with no route of its own goes back to none when it is
// unpublished: one set up through MaaS only whose publish stopped before it
// recorded anything, and one set up "Only inside the cluster" and then
// published on the MaaS tab.

// maasOnlyPublishStopped is maasOnlyModel whose publish, part of the
// setup, stopped at its check (the subscription cannot be read), before
// it recorded anything: the model is on the MaaS gateway alone, and the
// MaaS record knows nothing of it.
func maasOnlyPublishStopped(t *testing.T) *modelsCluster {
	t.Helper()
	mc := newMaaSCluster(t)
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup", Database: tCNPG}); !res.Success {
		t.Fatalf("maas setup: %+v", res)
	}
	if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true}); !res.Success {
		t.Fatalf("inference gateway setup: %+v", res)
	}
	mc.f.fail["GET "+tMaaSSub] = http.StatusInternalServerError
	res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Project: "proj", Exposure: serving.ExposureMaaS})
	delete(mc.f.fail, "GET "+tMaaSSub)
	if res.Success || !strings.Contains(res.Message, "Set it up again on the Models tab to publish it, or delete the model.") {
		t.Fatalf("setup with a publish that stops: %+v", res)
	}
	if got := modelRefsJSON(t, mc); got != tMaaSOnly {
		t.Fatalf("refs = %s", got)
	}
	if rec := rawMaaSRecord(mc); strings.Contains(rec, "modelRefs") || strings.Contains(rec, "proj/") {
		t.Fatalf("record = %s", rec)
	}
	return mc
}

func allMessages(res *types.OperationResponse, events []UpdateStepEvent) string {
	all := strings.Join(res.Logs, " ") + " " + res.Message
	for _, e := range events {
		all += " " + e.Message
	}
	return all
}

// The MaaS tab's Unpublish (the row is partly published) leaves the model
// with no route, never on KServe's default gateway.
func TestMaaSOnlyPublishStopped_UnpublishRemovesTheRoute(t *testing.T) {
	mc := maasOnlyPublishStopped(t)
	if items := toolModelItems(t, mc); len(items) != 1 || !items[0].MaaSOnlySetup {
		t.Fatalf("models status = %+v", items)
	}
	res, events := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "unpublish", Project: "proj"})
	if !res.Success {
		t.Fatalf("unpublish: %+v", res)
	}
	if _, ok := modelSpec(t, mc)["router"]; ok {
		t.Fatalf("router after unpublish = %v", modelSpec(t, mc)["router"])
	}
	if !routeGone(t, mc) {
		t.Fatal("the model's route is still there")
	}
	if all := allMessages(res, events); !strings.Contains(all, "no outside route") || strings.Contains(all, "KServe's default gateway") {
		t.Fatalf("messages = %s", all)
	}
}

// The Models tab's Delete unpublishes it first, then deletes it (it used
// to refuse: the record did not say the model had no route).
func TestMaaSOnlyPublishStopped_DeleteUnpublishesFirst(t *testing.T) {
	mc := maasOnlyPublishStopped(t)
	res, events := runModels(t, mc.c, types.ModelsRequest{Action: "teardown", Project: "proj", Model: "qwen25-05b-instruct"})
	if !res.Success || finalSteps(events) != "unpublish=success model=success" {
		t.Fatalf("teardown: %+v %s", res, finalSteps(events))
	}
	if mc.f.has(testModelPath) {
		t.Fatal("the model is still there")
	}
}

// Set up again on the Models tab publishes it and records no route, so a
// later Unpublish removes the route.
func TestMaaSOnlyPublishStopped_SetUpAgainThenUnpublish(t *testing.T) {
	mc := maasOnlyPublishStopped(t)
	if res, events := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Project: "proj", Exposure: serving.ExposureMaaS}); !res.Success {
		t.Fatalf("set up again: %+v %s", res, finalSteps(events))
	}
	if rec := rawMaaSRecord(mc); !strings.Contains(rec, `"proj/qwen25-05b-instruct":{"noRoute":true}`) {
		t.Fatalf("record = %s", rec)
	}
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "unpublish", Project: "proj"}); !res.Success {
		t.Fatalf("unpublish: %+v", res)
	}
	if _, ok := modelSpec(t, mc)["router"]; ok || !routeGone(t, mc) {
		t.Fatalf("router after unpublish = %v", modelSpec(t, mc)["router"])
	}
}

// publish's record for a model on the MaaS gateway alone with nothing
// recorded: no route, also when it is not the Models setup's own publish;
// a publication by an earlier build (recorded as published) keeps what it
// replaced.
func TestMaaSPublish_UnrecordedMaaSOnlyRecordsNoRoute(t *testing.T) {
	var rec serving.MaaSRecord
	if got, ok := rec.RouteBefore("proj", "m", "", serving.MaaSOnlyRefs()); !ok || !got.NoRoute {
		t.Fatalf("unrecorded = %+v, %v", got, ok)
	}
	rec.SetPublished("proj", "m", true)
	if got, ok := rec.RouteBefore("proj", "m", "", serving.MaaSOnlyRefs()); ok {
		t.Fatalf("earlier build's publication = %+v", got)
	}
	var none serving.MaaSRecord
	if got, ok := none.RouteBefore("proj", "m", "", nil); ok {
		t.Fatalf("not on the MaaS gateway = %+v", got)
	}
}

// A model set up "Only inside the cluster" has no route: Publish on the
// MaaS tab gives it one on the MaaS gateway alone (writing only the refs
// would make none) and records that it had none, so Unpublish removes it
// again; the same again after that.
func TestMaaSPublish_InternalModelGetsARouteAndLosesItAgain(t *testing.T) {
	mc := newMaaSCluster(t)
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup", Database: tCNPG}); !res.Success {
		t.Fatalf("maas setup: %+v", res)
	}
	mc.f.put("/api/v1/namespaces/proj/services/qwen25-05b-instruct-kserve-workload-svc", map[string]any{
		"spec": map[string]any{"ports": []any{map[string]any{"name": "https", "appProtocol": "https", "port": 8000}}},
	})
	if res, events := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Project: "proj", Exposure: serving.ExposureInternal}); !res.Success {
		t.Fatalf("internal setup: %+v %s", res, finalSteps(events))
	}
	for round := 1; round <= 2; round++ {
		res, events := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "publish", Project: "proj"})
		if !res.Success {
			t.Fatalf("round %d publish: %+v %s", round, res, finalSteps(events))
		}
		if got := modelRefsJSON(t, mc); got != tMaaSOnly {
			t.Fatalf("round %d refs = %s", round, got)
		}
		if got := routeParents(t, mc); got != serving.MaaSGatewayName {
			t.Fatalf("round %d route = %s", round, got)
		}
		if rec := rawMaaSRecord(mc); !strings.Contains(rec, `"proj/qwen25-05b-instruct":{"noRoute":true}`) {
			t.Fatalf("round %d record = %s", round, rec)
		}
		res, events = runMaaSOp(t, mc.c, types.MaaSRequest{Action: "unpublish", Project: "proj"})
		if !res.Success {
			t.Fatalf("round %d unpublish: %+v", round, res)
		}
		if all := allMessages(res, events); strings.Contains(all, "KServe's default gateway") || !strings.Contains(all, "no outside route") {
			t.Fatalf("round %d messages = %s", round, all)
		}
		if _, ok := modelSpec(t, mc)["router"]; ok || !routeGone(t, mc) {
			t.Fatalf("round %d router after unpublish = %v", round, modelSpec(t, mc)["router"])
		}
	}
}

// Set up through MaaS only, Unpublish (no route), then Publish on the MaaS
// tab and Unpublish again: the route comes back and goes again.
func TestMaaSOnlySetup_UnpublishPublishUnpublish(t *testing.T) {
	mc := maasOnlyModel(t)
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "unpublish", Project: "proj"}); !res.Success {
		t.Fatalf("unpublish: %+v", res)
	}
	if res, events := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "publish", Project: "proj"}); !res.Success {
		t.Fatalf("publish: %+v %s", res, finalSteps(events))
	}
	if got := routeParents(t, mc); got != serving.MaaSGatewayName {
		t.Fatalf("route = %s", got)
	}
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "unpublish", Project: "proj"}); !res.Success {
		t.Fatalf("unpublish again: %+v", res)
	}
	if _, ok := modelSpec(t, mc)["router"]; ok || !routeGone(t, mc) {
		t.Fatalf("router = %v", modelSpec(t, mc)["router"])
	}
}

// Unpublish again after a try that removed the route but stopped waiting:
// nothing is written, and the message says the model has no route (not
// "KServe's default gateway").
func TestMaaSOnlySetup_RestoreAfterTheRouteIsGone(t *testing.T) {
	mc := maasOnlyModel(t)
	obj := mc.f.get(testModelPath)
	delete(ensureMap(obj, "spec"), "router")
	mc.f.put(testModelPath, obj)
	mc.f.requests = nil
	var said []string
	run := &serving.Run{Client: mc.c}
	msg, code := run.RestoreModelRoute("proj", "qwen25-05b-instruct", serving.MaaSModelRefs{NoRoute: true}, true, func(_, _, m string) { said = append(said, m) })
	if msg != "" || code != "" {
		t.Fatalf("restore = %q, %q", msg, code)
	}
	if muts := mc.f.mutations(); len(muts) != 0 {
		t.Fatalf("wrote %v", muts)
	}
	if all := strings.Join(said, " "); !strings.Contains(all, "no outside route") || strings.Contains(all, "default gateway") {
		t.Fatalf("said = %s", all)
	}
}

// The Models operation's log holds the inner MaaS run's lines (its "OK:"
// lines included), each once.
func TestModels_InnerMaaSRunsKeepTheirLogs(t *testing.T) {
	mc := newMaaSCluster(t)
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup", Database: tCNPG}); !res.Success {
		t.Fatalf("maas setup: %+v", res)
	}
	if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true}); !res.Success {
		t.Fatalf("inference gateway setup: %+v", res)
	}
	res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Project: "proj", Exposure: serving.ExposureMaaS})
	if !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	logs := strings.Join(res.Logs, "\n")
	if !strings.Contains(logs, "OK: MaaSModelRef proj/qwen25-05b-instruct") {
		t.Fatalf("setup logs lack the publish's lines:\n%s", logs)
	}
	if n := strings.Count(logs, "Checking the platform and the model"); n != 1 {
		t.Fatalf("publish check line %d times:\n%s", n, logs)
	}
	res, _ = runModels(t, mc.c, types.ModelsRequest{Action: "teardown", Project: "proj", Model: "qwen25-05b-instruct"})
	if !res.Success {
		t.Fatalf("teardown: %+v", res)
	}
	logs = strings.Join(res.Logs, "\n")
	if !strings.Contains(logs, "OK: MaaSAuthPolicy") || strings.Count(logs, "Checking the publication") != 1 {
		t.Fatalf("teardown logs:\n%s", logs)
	}
}
