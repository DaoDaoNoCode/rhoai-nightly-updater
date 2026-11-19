package cluster

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// A model set up on the Models tab to be reached "Through MaaS only" is
// created on the MaaS gateway alone and published as part of its setup
// (live, RHOAI 3.6, 2026-11-18). It never had a route of its own, so
// Unpublish leaves it with none (only inside the cluster), never on KServe's
// default gateway, and the Models tab's Delete unpublishes it first.

// maasOnlyModel is newMaaSCluster with the platform set up, the tool's
// inference gateway (not listing proj), and the tool's model in proj set up
// on the Models tab through MaaS only.
func maasOnlyModel(t *testing.T) *modelsCluster {
	t.Helper()
	mc := newMaaSCluster(t)
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup", Database: tCNPG}); !res.Success {
		t.Fatalf("maas setup: %+v", res)
	}
	if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true}); !res.Success {
		t.Fatalf("inference gateway setup: %+v", res)
	}
	res, events := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Project: "proj", Exposure: exposureMaaS})
	if !res.Success {
		t.Fatalf("model setup through MaaS only: %+v\n%s", res, finalSteps(events))
	}
	if got := modelRefsJSON(t, mc); got != tMaaSOnly {
		t.Fatalf("refs after setup = %s", got)
	}
	return mc
}

// routeGone reads the model's route through the API (so the simulated
// KServe reconciles it) and reports whether it is gone.
func routeGone(t *testing.T, mc *modelsCluster) bool {
	t.Helper()
	_, _, err := mc.c.get(tModelRoute)
	if err != nil && !IsK8sError(err, http.StatusNotFound) {
		t.Fatalf("read the model's route: %v", err)
	}
	return IsK8sError(err, http.StatusNotFound)
}

// requestsTo are the logged requests (method and path) whose path holds part.
func requestsTo(mc *modelsCluster, method, part string) []string {
	mc.f.mu.Lock()
	defer mc.f.mu.Unlock()
	var out []string
	for _, r := range mc.f.requests {
		if strings.HasPrefix(r, method+" ") && strings.Contains(r, part) {
			out = append(out, r)
		}
	}
	return out
}

// firstRequest is the position of the first logged request with method
// whose path holds part, or -1.
func firstRequest(mc *modelsCluster, method, part string) int {
	mc.f.mu.Lock()
	defer mc.f.mu.Unlock()
	for i, r := range mc.f.requests {
		if strings.HasPrefix(r, method+" ") && strings.Contains(r, part) {
			return i
		}
	}
	return -1
}

// toolModelItems are the Models tab's rows of proj.
func toolModelItems(t *testing.T, mc *modelsCluster) []types.ModelState {
	t.Helper()
	var models []llmisvcObject
	if m, found, err := readToolModel(mc.c, "proj", "qwen25-05b-instruct"); err != nil {
		t.Fatal(err)
	} else if found {
		models = append(models, m)
	}
	rec, err := readModelsRecord(mc.c)
	if err != nil {
		t.Fatal(err)
	}
	return modelItems(mc.c, models, map[string]bool{"proj": true}, rec)
}

// Publishing as part of a MaaS-only setup records that the model had no
// route; a publication from the MaaS tab still records the refs.
func TestMaaSOnlySetup_PublishRecordsNoRoute(t *testing.T) {
	mc := maasOnlyModel(t)
	if rec := rawMaaSRecord(mc); !strings.Contains(rec, `"modelRefs":{"proj/qwen25-05b-instruct":{"noRoute":true}}`) {
		t.Fatalf("record = %s", rec)
	}
	rec, err := readMaaSRecord(mc.c)
	if err != nil {
		t.Fatal(err)
	}
	if refs, ok := rec.modelRefs("proj", "qwen25-05b-instruct"); !ok || !refs.NoRoute || len(refs.Refs) != 0 {
		t.Fatalf("recorded = %+v, %v", refs, ok)
	}
	// The status says so, on both tabs.
	if items := toolModelItems(t, mc); len(items) != 1 || !items[0].MaaSOnlySetup || !items[0].MaaSPublished {
		t.Fatalf("models status = %+v", items)
	}
	ms, err := GetMaaSStatus(mc.c)
	if err != nil || len(ms.Items) != 1 || !ms.Items[0].MaaSOnlySetup || !ms.Items[0].Published {
		t.Fatalf("maas status = %+v, %v", ms.Items, err)
	}
}

// Unpublish of a model set up through MaaS only removes its route: it is
// then reached only inside the cluster, as the "Only inside the cluster"
// exposure writes it. The inference gateway (which never listed the
// project) is not touched, and the message says what is true.
func TestMaaSOnlySetup_UnpublishRemovesTheRoute(t *testing.T) {
	mc := maasOnlyModel(t)
	gwBefore, _ := json.Marshal(mc.f.get(testGatewayPath)["spec"])
	var mu sync.Mutex
	var patches []map[string]any
	prev := mc.f.onMergePatch
	mc.f.onMergePatch = func(r *http.Request, k fakeKey, patch, next map[string]any) {
		if prev != nil {
			prev(r, k, patch, next)
		}
		if spec, ok := patch["spec"].(map[string]any); ok && k.plural == "llminferenceservices" {
			if _, router := spec["router"]; router {
				mu.Lock()
				patches = append(patches, patch)
				mu.Unlock()
			}
		}
	}
	mc.f.requests = nil
	res, events := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "unpublish", Project: "proj"})
	if !res.Success {
		t.Fatalf("unpublish: %+v", res)
	}
	if got := finalSteps(events); got != "check=success maas=success model=success gateway=success" {
		t.Fatalf("steps = %s", got)
	}
	spec := modelSpec(t, mc)
	if _, ok := spec["router"]; ok {
		t.Fatalf("model router after unpublish = %v", spec["router"])
	}
	if !routeGone(t, mc) {
		t.Fatal("the model's route is still there")
	}
	// One write of the router (the resourceVersion guard is checked by the
	// fake and in TestMaaSOnlySetup_ModelChangedMeanwhileIsAConflict).
	if len(patches) != 1 {
		t.Fatalf("router writes = %v", patches)
	}
	if meta, _ := patches[0]["metadata"].(map[string]any); meta["annotations"] != nil {
		t.Fatalf("token authentication changed with Connectivity Link set up: %v", patches[0])
	}
	all := strings.Join(res.Logs, " ") + " " + res.Message
	for _, e := range events {
		all += " " + e.Message
	}
	if !strings.Contains(all, "The model has no outside route now, as before it was published: it answers only at its in-cluster address.") {
		t.Fatalf("no route message: %s", all)
	}
	for _, wrong := range []string{"KServe's default gateway", "routes as before publishing", "Allow proj", "direct address does not answer"} {
		if strings.Contains(all, wrong) {
			t.Errorf("message says %q: %s", wrong, all)
		}
	}
	// The inference gateway, whose list never had proj, is left alone.
	if w := append(requestsTo(mc, "PATCH", "/gateways/openshift-ai-inference"), requestsTo(mc, "PUT", "/gateways/openshift-ai-inference")...); len(w) != 0 {
		t.Fatalf("inference gateway writes = %v", w)
	}
	if gwAfter, _ := json.Marshal(mc.f.get(testGatewayPath)["spec"]); string(gwAfter) != string(gwBefore) || strings.Contains(string(gwAfter), `"proj"`) {
		t.Fatalf("inference gateway = %s, was %s", gwAfter, gwBefore)
	}
	if rec := rawMaaSRecord(mc); strings.Contains(rec, "modelRefs") || strings.Contains(rec, `"published"`) {
		t.Fatalf("record after unpublish = %s", rec)
	}
	if items := toolModelItems(t, mc); len(items) != 1 || items[0].Exposure != exposureInternal || items[0].MaaSOnlySetup || items[0].MaaSPublished {
		t.Fatalf("models status = %+v", items)
	}
}

// With Connectivity Link gone by the time it is unpublished, the model gets
// what the "Only inside the cluster" exposure writes then: token
// authentication off, since KServe would not start it otherwise.
func TestMaaSOnlySetup_UnpublishWithoutConnectivityLinkTurnsTokenAuthOff(t *testing.T) {
	mc := maasOnlyModel(t)
	mc.f.mu.Lock()
	delete(mc.f.groups, "/apis/kuadrant.io")
	mc.f.mu.Unlock()
	apiVersionsCache.Purge()
	res, events := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "unpublish", Project: "proj"})
	if !res.Success {
		t.Fatalf("unpublish: %+v", res)
	}
	model, _, err := readToolModel(mc.c, "proj", "qwen25-05b-instruct")
	if err != nil {
		t.Fatal(err)
	}
	if model.Spec.Router != nil || model.Metadata.Annotations[enableAuthAnnot] != "false" {
		t.Fatalf("model router %+v, annotations %v", model.Spec.Router, model.Metadata.Annotations)
	}
	all := ""
	for _, e := range events {
		all += " " + e.Message
	}
	if !strings.Contains(all, "token authentication is off") {
		t.Fatalf("messages = %s", all)
	}
}

// The router write carries the resourceVersion read with the refs: a model
// changed in between is a conflict, never overwritten.
func TestMaaSOnlySetup_ModelChangedMeanwhileIsAConflict(t *testing.T) {
	mc := maasOnlyModel(t)
	prev, bumped := mc.f.beforeServe, false
	mc.f.beforeServe = func(method, path string) {
		prev(method, path)
		// Someone changes the model just before the router write (the
		// unpublish's first write to it).
		if method == http.MethodPatch && path == testModelPath && !bumped {
			bumped = true
			mc.f.mu.Lock()
			k, _ := parseFakePath(testModelPath)
			ensureMap(mc.f.objects[k], "metadata")["resourceVersion"] = "999"
			mc.f.mu.Unlock()
		}
	}
	res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "unpublish", Project: "proj"})
	if res.Success || res.ErrorCode != "conflict" || !strings.Contains(res.Message, "Failed to remove the model's route") {
		t.Fatalf("unpublish = %+v", res)
	}
	if got := modelRefsJSON(t, mc); got != tMaaSOnly {
		t.Fatalf("refs = %s", got)
	}
}

// Someone added a gateway to the model while it was published: Unpublish
// refuses, as for any recorded publication, and writes nothing.
func TestMaaSOnlySetup_UnpublishRefusesWhenSomeoneAddedAGateway(t *testing.T) {
	mc := maasOnlyModel(t)
	setModelRefs(mc, []any{tMaaSGatewayRef, tTeamGatewayRef})
	mc.f.requests = nil
	res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "unpublish", Project: "proj"})
	if res.Success || res.ErrorCode != "conflict" || !strings.Contains(res.Message, "team/team-gateway") ||
		!strings.Contains(res.Message, "no route") || !strings.Contains(res.Message, "Nothing was changed") {
		t.Fatalf("unpublish = %+v", res)
	}
	if muts := mc.f.mutations(); len(muts) != 0 {
		t.Fatalf("wrote %v", muts)
	}
	if got := modelRefsJSON(t, mc); got != `[{"name":"maas-default-gateway","namespace":"openshift-ingress"},{"name":"team-gateway","namespace":"team"}]` {
		t.Fatalf("refs = %s", got)
	}
}

// The restore itself refuses too when the refs change between unpublish's
// check and its write (restoreModelRoute reads them again): no router write.
func TestMaaSOnlySetup_RestoreRefusesWhenTheRefsChangedMeanwhile(t *testing.T) {
	mc := maasOnlyModel(t)
	run := &maasRun{c: mc.c}
	setModelRefs(mc, []any{tMaaSGatewayRef, tTeamGatewayRef})
	mc.f.requests = nil
	msg, code := run.restoreModelRoute("proj", "qwen25-05b-instruct", maasModelRefs{NoRoute: true}, true, func(string, string, string) {})
	if code != "conflict" || !strings.Contains(msg, "team/team-gateway") {
		t.Fatalf("restore = %q, %q", msg, code)
	}
	if muts := mc.f.mutations(); len(muts) != 0 {
		t.Fatalf("wrote %v", muts)
	}
}

// Delete on the Models tab unpublishes a model set up through MaaS only
// first, in the same operation, then deletes it.
func TestModelsTeardown_MaaSOnlyModelIsUnpublishedThenDeleted(t *testing.T) {
	mc := maasOnlyModel(t)
	mc.f.requests = nil
	res, events := runModels(t, mc.c, types.ModelsRequest{Action: "teardown", Project: "proj", Model: "qwen25-05b-instruct"})
	if !res.Success {
		t.Fatalf("teardown: %+v\n%s", res, finalSteps(events))
	}
	if got := finalSteps(events); got != "unpublish=success model=success" {
		t.Fatalf("steps = %s", got)
	}
	unpub, del := firstRequest(mc, "DELETE", "/maasmodelrefs/qwen25-05b-instruct"), firstRequest(mc, "DELETE", "/llminferenceservices/qwen25-05b-instruct")
	if unpub < 0 || del < 0 || unpub > del {
		t.Fatalf("MaaSModelRef delete at %d, model delete at %d", unpub, del)
	}
	if router := firstRequest(mc, "PATCH", "/llminferenceservices/qwen25-05b-instruct"); router < 0 || router > del {
		t.Fatalf("route removal at %d, model delete at %d", router, del)
	}
	if mc.f.has(tMaaSRef) || mc.f.has(testModelPath) {
		t.Fatal("the model reference or the model is still there")
	}
	if rec := rawMaaSRecord(mc); strings.Contains(rec, `"published"`) {
		t.Fatalf("record = %s", rec)
	}
	unpublishMsgs := ""
	for _, e := range events {
		if e.Step == "unpublish" {
			unpublishMsgs += " " + e.Message
		}
	}
	if !strings.Contains(unpublishMsgs, "unpublished from MaaS") {
		t.Fatalf("unpublish step messages = %s", unpublishMsgs)
	}
}

// Fail closed: when whether the model is published cannot be read, the
// teardown stops before it changes or deletes anything.
func TestModelsTeardown_MaaSCheckThatFailsDeletesNothing(t *testing.T) {
	mc := maasOnlyModel(t)
	mc.f.fail["GET "+tMaaSRef] = http.StatusInternalServerError
	mc.f.requests = nil
	res, events := runModels(t, mc.c, types.ModelsRequest{Action: "teardown", Project: "proj", Model: "qwen25-05b-instruct"})
	if res.Success || !strings.Contains(res.Message, "nothing was deleted") {
		t.Fatalf("teardown = %+v", res)
	}
	if got := finalSteps(events); got != "unpublish=failed" {
		t.Fatalf("steps = %s", got)
	}
	if muts := mc.f.mutations(); len(muts) != 0 {
		t.Fatalf("wrote %v", muts)
	}
	if !mc.f.has(testModelPath) || !mc.f.has(tMaaSRef) {
		t.Fatal("deleted on a failed check")
	}
}

// Fail closed: an unpublish that refuses (here a gateway someone added)
// stops the teardown, and the model is not deleted.
func TestModelsTeardown_RefusedUnpublishDeletesNothing(t *testing.T) {
	mc := maasOnlyModel(t)
	setModelRefs(mc, []any{tMaaSGatewayRef, tTeamGatewayRef})
	mc.f.requests = nil
	res, events := runModels(t, mc.c, types.ModelsRequest{Action: "teardown", Project: "proj", Model: "qwen25-05b-instruct"})
	if res.Success || res.ErrorCode != "conflict" || !strings.Contains(res.Message, "The model was not deleted.") || !strings.Contains(res.Message, "team/team-gateway") {
		t.Fatalf("teardown = %+v", res)
	}
	if got := finalSteps(events); got != "unpublish=failed" {
		t.Fatalf("steps = %s", got)
	}
	if muts := mc.f.mutations(); len(muts) != 0 {
		t.Fatalf("wrote %v", muts)
	}
	if !mc.f.has(testModelPath) || !mc.f.has(tMaaSRef) {
		t.Fatal("deleted after a refused unpublish")
	}
}

// Fail closed later in the unpublish: the subscription cannot be read, so
// the unpublish stops, and the model is not deleted.
func TestModelsTeardown_UnpublishThatFailsDeletesNothing(t *testing.T) {
	mc := maasOnlyModel(t)
	mc.f.fail["GET "+tMaaSSub] = http.StatusInternalServerError
	res, events := runModels(t, mc.c, types.ModelsRequest{Action: "teardown", Project: "proj", Model: "qwen25-05b-instruct"})
	if res.Success || !strings.Contains(res.Message, "The model was not deleted.") {
		t.Fatalf("teardown = %+v", res)
	}
	if got := finalSteps(events); got != "unpublish=failed" {
		t.Fatalf("steps = %s", got)
	}
	if len(requestsTo(mc, "DELETE", "/llminferenceservices/")) != 0 || !mc.f.has(testModelPath) {
		t.Fatal("the model was deleted after a failed unpublish")
	}
}

// A model published on the MaaS tab (it had a route of its own) is still
// refused: it is unpublished there, and nothing is deleted.
func TestModelsTeardown_ModelPublishedOnTheMaaSTabStillRefuses(t *testing.T) {
	mc := publishedMaaSCluster(t)
	mc.f.requests = nil
	res, events := runModels(t, mc.c, types.ModelsRequest{Action: "teardown", Project: "proj", Model: "qwen25-05b-instruct"})
	if res.Success || res.ErrorCode != "in_use" || !strings.Contains(res.Message, "Unpublish it on the MaaS tab first; nothing was deleted.") {
		t.Fatalf("teardown = %+v", res)
	}
	if got := finalSteps(events); got != "model=failed" {
		t.Fatalf("steps = %s", got)
	}
	if muts := mc.f.mutations(); len(muts) != 0 {
		t.Fatalf("wrote %v", muts)
	}
	if items := toolModelItems(t, mc); len(items) != 1 || items[0].MaaSOnlySetup || !items[0].MaaSPublished {
		t.Fatalf("models status = %+v", items)
	}
}

// Records of other publications keep their meaning: refs recorded as none
// (published from KServe's default gateway) go back to the default, and a
// publication by an earlier build (nothing recorded) to what it replaced.
func TestMaaSUnpublish_OtherRecordsAreUnaffected(t *testing.T) {
	mc := publishedMaaSCluster(t)
	if rec := rawMaaSRecord(mc); !strings.Contains(rec, `"modelRefs":{"proj/qwen25-05b-instruct":{}}`) {
		t.Fatalf("record = %s", rec)
	}
	res, events := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "unpublish", Project: "proj"})
	if !res.Success {
		t.Fatalf("unpublish: %+v", res)
	}
	all := ""
	for _, e := range events {
		all += " " + e.Message
	}
	if !strings.Contains(all, "The model routes as before publishing: KServe's default gateway (no refs).") || strings.Contains(all, "no outside route") {
		t.Fatalf("messages = %s", all)
	}
	if gw := modelGateway(t, mc); len(gw) != 0 {
		t.Fatalf("model gateway after unpublish = %v", gw)
	}
	if got := routeParents(t, mc); got != "openshift-ai-inference" {
		t.Fatalf("route = %s", got)
	}

	// An earlier build's record (published, no refs) of a model on the MaaS
	// gateway alone: back to the default, as before.
	mc = publishedMaaSCluster(t)
	rec, _ := readMaaSRecord(mc.c)
	rec.ModelRefs = nil
	if err := writeMaaSRecord(mc.c, rec); err != nil {
		t.Fatal(err)
	}
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "unpublish", Project: "proj"}); !res.Success {
		t.Fatalf("unpublish of an earlier build's publication: %+v", res)
	}
	if gw := modelGateway(t, mc); len(gw) != 0 {
		t.Fatalf("model gateway after unpublish = %v", gw)
	}
}

// Clean up unpublishes first and then deletes the model: the model's
// teardown finds it unpublished, so the unpublish runs once.
func TestCleanUp_MaaSOnlyModelIsUnpublishedOnce(t *testing.T) {
	mc := maasOnlyModel(t)
	mc.f.requests = nil
	var steps []string
	res, err := CleanUp(mc.c, types.CleanupRequest{MaaSPublished: []string{"proj"}, Models: []string{"proj"}}, func(e UpdateStepEvent) {
		if e.Status != "running" {
			steps = append(steps, e.Step+"="+e.Status)
		}
	})
	if err != nil || !res.Success {
		t.Fatalf("clean-up: %+v, %v", res, err)
	}
	if got := strings.Join(steps, " "); got != "maas-unpublish:proj=success model:proj=success" {
		t.Fatalf("steps = %s", got)
	}
	if n := len(requestsTo(mc, "DELETE", "/maasmodelrefs/")); n != 1 {
		t.Fatalf("MaaSModelRef deletes = %d", n)
	}
	// One unpublish: the authorization policy and the subscription (the last
	// model leaves them) are deleted once each.
	if n := len(requestsTo(mc, "DELETE", "/maasauthpolicies/")) + len(requestsTo(mc, "DELETE", "/maassubscriptions/")); n != 2 {
		t.Fatalf("authorization policy and subscription deletes = %d", n)
	}
	if mc.f.has(testModelPath) {
		t.Fatal("the model is still there")
	}
}
