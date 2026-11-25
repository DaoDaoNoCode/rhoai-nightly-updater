package cluster

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/features"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/mlflow"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/workbench"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The hand lists cite their source at a pinned release, and SECURITY.md
// names the same release, so a bump of either is a reviewed change of
// both. Every row cites where the request is made.
func TestHandListsMatchPinnedSource(t *testing.T) {
	doc, err := os.ReadFile("../../SECURITY.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, pinned := range []string{"rhoai-3.6", "efd01a3", "mlflow-kubernetes-plugins v2.1.1", "9694e137"} {
		if !strings.Contains(features.GenAISource+" "+mlflow.AuthRulesSource, pinned) {
			t.Errorf("the hand lists' source no longer names %q", pinned)
		}
		if !strings.Contains(string(doc), pinned) {
			t.Errorf("SECURITY.md does not name the hand lists' pinned source %q", pinned)
		}
	}
	for _, r := range features.GenAIRequests {
		if r.Cite == "" || len(r.Calls) == 0 {
			t.Errorf("gen-ai request %+v has no source or call", r)
		}
	}
	for _, r := range mlflow.AuthRules {
		if !strings.Contains(r.Cite, ".py:") || len(r.Actions) == 0 {
			t.Errorf("MLflow rule %s %s has no source lines or action", r.Method, r.Route)
		}
	}
}

// Each MLflow rule's actions are exactly the samples actions whose runs
// in the fake use it (the scenarios of TestSampleFakeRulesAreAllUsed): the
// preflight of an action reviews what its calls need, no more.
func TestMLflowRuleActionsMatchTheFake(t *testing.T) {
	env := newSamplesEnv(t)
	used := map[string]map[string]bool{}
	runWant := func(want bool, action string, samples ...string) {
		n := len(env.mlflow.requests)
		if ok, msg, st := env.run(t, action, samples...); ok != want {
			t.Fatalf("%s %v: %s\n%s", action, samples, msg, stepMessages(st))
		}
		for _, req := range env.mlflow.requests[n:] {
			method, path, _ := strings.Cut(req, " ")
			for _, rule := range mlflowFakeRules {
				if _, ok := rule.match(method, path); ok {
					if used[rule.key()] == nil {
						used[rule.key()] = map[string]bool{}
					}
					used[rule.key()][action] = true
				}
			}
		}
	}
	run := func(action string, samples ...string) { runWant(true, action, samples...) }
	run("add", mlflow.SampleIDs...)
	run("remove", SampleTraining, SampleMCPServer, mlflow.SampleSkill)
	run("add", SampleTraining)
	run("regenerate", SampleGenAIDemo)
	env.mlflow.models[mlflow.TrainingModel].aliases = map[string]string{}
	run("add", SampleTraining)
	env.mlflow.expByName(mlflow.DemoExperiment).tags[mlflow.DemoVersionTagPrefix+"retired_feature"] = "0"
	env.mlflow.mlflowDeleteDemo()
	run("add", SampleGenAIDemo)
	// Registry entries someone creates while the tool adds them: the add
	// deletes the version it added (and fails).
	run("remove", SampleMCPServer, mlflow.SampleSkill)
	env.mlflow.onRequest = func(method, path string) {
		if method == "POST" && strings.HasSuffix(path, "/versions") {
			if strings.Contains(path, "/mcp-servers/") && env.mlflow.mcp[mlflow.MCPSampleName] == nil {
				env.mlflow.mcp[mlflow.MCPSampleName] = &fakeMCP{tags: map[string]string{}, versions: map[string]string{}, createdBy: "alice"}
			}
			if strings.Contains(path, "/skills/") && env.mlflow.skills[mlflow.SkillSampleName] == nil {
				env.mlflow.skills[mlflow.SkillSampleName] = &fakeSkill{tags: map[string]string{}, createdBy: "alice"}
			}
		}
	}
	runWant(false, "add", SampleMCPServer, mlflow.SampleSkill)
	env.mlflow.onRequest = nil
	for _, rule := range mlflow.AuthRules {
		key := rule.Method + " " + rule.Route
		for _, a := range rule.Actions {
			if !used[key][a] {
				t.Errorf("%s lists %s, which no %s run uses", key, a, a)
			}
		}
		for a := range used[key] {
			found := false
			for _, b := range rule.Actions {
				found = found || a == b
			}
			if !found {
				t.Errorf("%s is used by %s, which it does not list (the preflight of %s misses it)", key, a, a)
			}
		}
	}
}

// MLflow refusing an add half-way is a clean refusal: the sample, absent
// before, is removed again by its own remove path, and the step says so.
func TestMLflowSamples403IsACleanRefusal(t *testing.T) {
	env := newSamplesEnv(t)
	// The experiment is created, then MLflow refuses the run.
	env.mlflow.userAllow = func(resource, verb, _, _ string) bool { return resource != "experiments" || verb != "update" }
	ok, msg, steps := env.run(t, "add", SampleTraining)
	if ok || !strings.Contains(msg, "forbidden") || !strings.Contains(stepMessages(steps), "What this run added was removed again") {
		t.Fatalf("%s\n%s", msg, stepMessages(steps))
	}
	if exp := env.mlflow.expByName(mlflow.TrainingExperiment); exp != nil && exp.stage != "deleted" {
		t.Fatalf("the refused add left experiment %+v", exp)
	}
	if env.mlflow.models[mlflow.TrainingModel] != nil {
		t.Fatal("the refused add left the registered model")
	}
}

// The dashboard's gen-ai API refusing the install is a clean refusal: the
// step says the dashboard refused, and the tool's record of the request is
// dropped (the API rolls back its own ConfigMaps, token_k8s_client.go
// :2143-2160), so nothing is left.
func TestPlaygroundGenAI403IsACleanRefusal(t *testing.T) {
	e := newPlaygroundEnv(t)
	e.g.installFail, e.g.installMessage = 403, "user cannot create ogxservers in proj"
	res, steps := e.run(t, setup())
	if res.Success || res.ErrorCode != "forbidden" || !strings.Contains(res.Message, "refused") || !strings.Contains(res.Message, "user cannot create ogxservers") {
		t.Fatalf("%+v", res)
	}
	if _, has := e.records(t)["proj"]; has {
		t.Fatal("the refused install left the tool's record")
	}
	if len(e.g.servers) != 0 {
		t.Fatal("the refused install left a playground")
	}
	last := steps[len(steps)-1]
	if last.Status != "failed" || last.Step != "install" {
		t.Fatalf("steps %+v", steps)
	}
}

// putUserRecords records a workbench of user in each of n projects, as a
// project user's setup writes them, and makes the projects exist.
func putUserRecords(t *testing.T, f *resourceFake, user string, n int) {
	t.Helper()
	rec := map[string]workbench.Recorded{}
	for i := range n {
		p := fmt.Sprintf("%s-%d", user, i)
		rec[p+"/"+workbench.Name] = workbench.Recorded{UID: "uid-" + p, By: user, ActorRole: record.ActorRoleUser}
		f.putJSON("/api/v1/namespaces/"+p, `{"metadata":{"name":"`+p+`"},"status":{"phase":"Active"}}`)
	}
	data, _ := json.Marshal(rec)
	cm, _ := json.Marshal(map[string]any{"metadata": map[string]any{"name": record.SnapshotConfigMapName}, "data": map[string]string{workbench.RecordKey: string(data)}})
	f.putJSON("/api/v1/namespaces/"+record.GetActivityNamespace()+"/configmaps/"+record.SnapshotConfigMapName, string(cm))
}

// A project user's 11th project is refused before any write; the same
// setup by a cluster-admin is not; a project user's setup records its
// entry as a user's and writes nothing outside the project.
func TestWorkbenchSetupRecordsCapsAndProjectUser(t *testing.T) {
	f, c := newResourceFake(t)
	putWorkbenchCluster(t, f, true)
	putUserRecords(t, f, "alice", record.MaxUserRecordProjectsPerUser)
	u := userClientOf(c, fakeUserToken)
	u.SetUsername("alice")
	u.SetProjectUser()
	before := len(f.mutations())
	res := wbSteps(t, u, types.WorkbenchRequest{Action: "setup", Project: "proj"})
	if res.Success || res.ErrorCode != record.ErrorCodeRecordsFull || !strings.Contains(res.Message, "Nothing was changed") || len(f.mutations()) != before {
		t.Fatalf("%+v writes %v", res, f.mutations()[before:])
	}

	f2, c2 := newResourceFake(t)
	putWorkbenchCluster(t, f2, true)
	putUserRecords(t, f2, "alice", record.MaxUserRecordProjectsPerUser)
	admin := userClientOf(c2, fakeUserToken)
	admin.SetUsername("alice")
	if res := wbSteps(t, admin, types.WorkbenchRequest{Action: "setup", Project: "proj"}); !res.Success {
		t.Fatalf("a cluster-admin was refused by the users' caps: %+v", res)
	}

	f3, c3 := newResourceFake(t)
	putWorkbenchCluster(t, f3, true)
	user := userClientOf(c3, fakeUserToken)
	user.SetUsername("bob")
	user.SetProjectUser()
	if res := wbSteps(t, user, types.WorkbenchRequest{Action: "setup", Project: "proj"}); !res.Success {
		t.Fatalf("a project user's setup: %+v", res)
	}
	for _, m := range f3.mutations() {
		if !strings.Contains(m, "/namespaces/proj/") && !strings.Contains(m, record.SnapshotConfigMapName) {
			t.Errorf("a project user's setup wrote outside the project: %s", m)
		}
	}
	rec, err := workbench.ReadRecord(c3)
	if err != nil {
		t.Fatal(err)
	}
	if e, _ := rec.Get("proj"); e.ActorRole != record.ActorRoleUser || e.By != "bob" {
		t.Fatalf("record %+v", e)
	}
}
