package cluster

import (
	"strings"
	"testing"
)

func TestMCPSample_AddRemove(t *testing.T) {
	env := newSamplesEnv(t)
	ok, msg, steps := env.run(t, "add", SampleMCPServer)
	if !ok {
		t.Fatalf("%s\n%s", msg, stepMessages(steps))
	}
	srv := env.mlflow.mcp[mcpSampleName]
	if srv == nil || srv.versions[mcpSampleVersion] != "active" || srv.endpoints != 1 || srv.tags[sampleMarkerKey] != SampleMCPServer || srv.description != mcpSampleDescription {
		t.Fatalf("server %+v", srv)
	}
	if s := env.status(t)[SampleMCPServer]; s.State != "present" || s.Actions[0] != "remove" {
		t.Errorf("status %+v", s)
	}
	n := len(env.mlflow.requests)
	if ok, msg, _ := env.run(t, "add", SampleMCPServer); !ok || len(env.mlflow.writes(n)) != 0 {
		t.Errorf("second add: %s writes %v", msg, env.mlflow.writes(n))
	}

	n = len(env.mlflow.requests)
	if ok, msg, steps := env.run(t, "remove", SampleMCPServer); !ok {
		t.Fatalf("%s\n%s", msg, stepMessages(steps))
	}
	want := []string{"PATCH /api/3.0/mlflow/mcp-servers/" + mcpSampleName + "/versions/" + mcpSampleVersion, "DELETE /api/3.0/mlflow/mcp-servers/" + mcpSampleName}
	if got := env.mlflow.writes(n); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("remove writes %v, want deprecate then delete", got)
	}
	if env.mlflow.mcp[mcpSampleName] != nil {
		t.Error("server not deleted")
	}
	if ok, msg, _ := env.run(t, "remove", SampleMCPServer); !ok || !strings.Contains(msg, "removed") {
		t.Errorf("second remove: %s", msg)
	}
}

// A 404 without MLflow's error body (the gateway's while MLflow restarts)
// is not "no registry": the status cannot tell, and Remove fails instead of
// reporting success with the entry still there.
func TestRegistrySamples_BareNotFoundIsAnError(t *testing.T) {
	env := newSamplesEnv(t)
	if ok, msg, _ := env.run(t, "add", SampleMCPServer, SampleSkill); !ok {
		t.Fatal(msg)
	}
	mcpGet := "GET /api/3.0/mlflow/mcp-servers/" + mcpSampleName
	skillGet := "GET /api/3.0/mlflow/skills/" + skillSampleName
	env.mlflow.bareNext[mcpGet], env.mlflow.bareNext[skillGet] = 404, 404
	got := env.status(t)
	for _, id := range []string{SampleMCPServer, SampleSkill} {
		if got[id].State != "unknown" {
			t.Errorf("%s status = %+v, want unknown", id, got[id])
		}
	}
	env.mlflow.bareNext[mcpGet], env.mlflow.bareNext[skillGet] = 404, 404
	ok, msg, steps := env.run(t, "remove", SampleMCPServer, SampleSkill)
	if ok || strings.Contains(stepMessages(steps), "nothing to remove") {
		t.Errorf("remove succeeded on a bare 404: %s\n%s", msg, stepMessages(steps))
	}
	if env.mlflow.mcp[mcpSampleName] == nil || env.mlflow.skills[skillSampleName] == nil {
		t.Error("an entry was deleted")
	}
}

// Someone creates the MCP server or skill between the tool's read and its
// version POST: the version goes into their parent. The tool must not tag
// (claim) it; it deletes only the version it added.
func TestRegistrySamples_ParentCreatedMeanwhileIsNotClaimed(t *testing.T) {
	for _, tc := range []struct {
		name string
		// theirs is what someone else creates just before the version POST.
		mcp   *fakeMCP
		skill *fakeSkill
	}{
		{"with their own version", &fakeMCP{tags: map[string]string{}, versions: map[string]string{"0.9.0": "active"}, createdBy: "alice"}, &fakeSkill{tags: map[string]string{}, versions: 1, createdBy: "alice"}},
		{"empty, created by someone else", &fakeMCP{tags: map[string]string{}, versions: map[string]string{}, createdBy: "alice"}, &fakeSkill{tags: map[string]string{}, createdBy: "alice"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newSamplesEnv(t)
			env.mlflow.onRequest = func(method, path string) {
				if method != "POST" || !strings.HasSuffix(path, "/versions") {
					return
				}
				if strings.Contains(path, "/mcp-servers/") && env.mlflow.mcp[mcpSampleName] == nil {
					env.mlflow.mcp[mcpSampleName] = tc.mcp
				}
				if strings.Contains(path, "/skills/") && env.mlflow.skills[skillSampleName] == nil {
					env.mlflow.skills[skillSampleName] = tc.skill
				}
			}
			theirMCP := len(tc.mcp.versions)
			theirSkill := tc.skill.versions
			ok, msg, steps := env.run(t, "add", SampleMCPServer, SampleSkill)
			if ok || !strings.Contains(msg, "not_managed") || !strings.Contains(stepMessages(steps), "Someone else created") {
				t.Fatalf("%s\n%s", msg, stepMessages(steps))
			}
			srv, sk := env.mlflow.mcp[mcpSampleName], env.mlflow.skills[skillSampleName]
			if srv == nil || srv.tags[sampleMarkerKey] != "" || len(srv.versions) != theirMCP {
				t.Errorf("their MCP server was changed: %+v", srv)
			}
			if _, left := srv.versions[mcpSampleVersion]; left {
				t.Error("the tool's MCP version was left in their server")
			}
			if sk == nil || sk.tags[sampleMarkerKey] != "" || sk.versions-len(sk.deleted) != theirSkill {
				t.Errorf("their skill was changed: %+v", sk)
			}
		})
	}
}

func TestMCPSample_PartialIsCompleted(t *testing.T) {
	env := newSamplesEnv(t)
	env.mlflow.mcp[mcpSampleName] = &fakeMCP{tags: map[string]string{sampleMarkerKey: SampleMCPServer}, versions: map[string]string{mcpSampleVersion: "deprecated"}}
	s := env.status(t)[SampleMCPServer]
	if s.State != "partial" || len(s.Missing) != 2 {
		t.Errorf("status %+v", s)
	}
	if ok, msg, _ := env.run(t, "add", SampleMCPServer); !ok {
		t.Fatal(msg)
	}
	if srv := env.mlflow.mcp[mcpSampleName]; srv.versions[mcpSampleVersion] != "active" || srv.endpoints != 1 {
		t.Errorf("server %+v", srv)
	}
}

// A server whose marker tag cannot be set is deleted again, so no unmarked
// entry the tool made stays behind.
func TestMCPSample_UnmarkedServerIsRolledBack(t *testing.T) {
	env := newSamplesEnv(t)
	env.mlflow.failNext["POST /api/3.0/mlflow/mcp-servers/"+mcpSampleName+"/tags"] = 500
	if ok, msg, _ := env.run(t, "add", SampleMCPServer); ok {
		t.Fatalf("got %s", msg)
	}
	if env.mlflow.mcp[mcpSampleName] != nil {
		t.Error("an unmarked server stayed behind")
	}
}

func TestRegistrySamples_ForeignAndDenied(t *testing.T) {
	env := newSamplesEnv(t)
	env.mlflow.mcp[mcpSampleName] = &fakeMCP{tags: map[string]string{}, versions: map[string]string{mcpSampleVersion: "active"}, endpoints: 1}
	env.mlflow.skills[skillSampleName] = &fakeSkill{tags: map[string]string{}, versions: 1}
	for _, action := range []string{"add", "remove"} {
		n := len(env.mlflow.requests)
		ok, msg, _ := env.run(t, action, SampleMCPServer, SampleSkill)
		if ok || !strings.Contains(msg, "not_managed") || len(env.mlflow.writes(n)) != 0 {
			t.Errorf("%s: %s writes %v", action, msg, env.mlflow.writes(n))
		}
	}

	env2 := newSamplesEnv(t)
	env2.mlflow.userAllow = func(resource, verb, ns, name string) bool { return verb != "update" }
	ok, msg, steps := env2.run(t, "add", SampleMCPServer, SampleSkill)
	if ok || !strings.Contains(msg, "forbidden") || strings.Count(stepMessages(steps), "403 PERMISSION_DENIED") != 2 {
		t.Errorf("%s\n%s", msg, stepMessages(steps))
	}
}

func TestSkillSample_AddRemoveAndMissingAPI(t *testing.T) {
	env := newSamplesEnv(t)
	if ok, msg, steps := env.run(t, "add", SampleSkill); !ok {
		t.Fatalf("%s\n%s", msg, stepMessages(steps))
	}
	sk := env.mlflow.skills[skillSampleName]
	if sk == nil || sk.versions != 1 || sk.tags[sampleMarkerKey] != SampleSkill || sk.source["ref"] != skillSampleRef || sk.source["subpath"] != skillSampleSubpath {
		t.Fatalf("skill %+v", sk)
	}
	if s := env.status(t)[SampleSkill]; s.State != "present" {
		t.Errorf("status %+v", s)
	}
	n := len(env.mlflow.requests)
	if ok, _, _ := env.run(t, "add", SampleSkill); !ok || len(env.mlflow.writes(n)) != 0 {
		t.Errorf("second add wrote %v", env.mlflow.writes(n))
	}
	if ok, msg, _ := env.run(t, "remove", SampleSkill); !ok || env.mlflow.skills[skillSampleName] != nil {
		t.Errorf("remove: %s", msg)
	}

	env.mlflow.noSkills = true
	if ok, msg, steps := env.run(t, "add", SampleSkill); ok || !strings.Contains(stepMessages(steps), "no Skill Registry") {
		t.Errorf("add without the API: %s\n%s", msg, stepMessages(steps))
	}
	if ok, msg, _ := env.run(t, "remove", SampleSkill); !ok {
		t.Errorf("remove without the API: %s", msg)
	}
}

func TestMLflowSamples_AddAllThenRemoveAll(t *testing.T) {
	env := newSamplesEnv(t)
	ok, msg, steps := env.run(t, "add", SampleSkill, SampleMCPServer, SampleTraining, SampleGenAIDemo)
	if !ok {
		t.Fatalf("%s\n%s", msg, stepMessages(steps))
	}
	var order []string
	for _, s := range steps {
		if s.Status == "success" && s.Step != "check" {
			order = append(order, s.Step)
		}
	}
	if strings.Join(order, ",") != "genai-demo,training,mcp-server,skill" {
		t.Errorf("order %v", order)
	}
	for id, s := range env.status(t) {
		if s.State != "present" {
			t.Errorf("%s = %+v", id, s)
		}
	}
	if ok, msg, _ := env.run(t, "remove", SampleTraining, SampleMCPServer, SampleSkill); !ok {
		t.Fatal(msg)
	}
	st := env.status(t)
	if st[SampleGenAIDemo].State != "present" || st[SampleTraining].State != "removed" || st[SampleMCPServer].State != "absent" || st[SampleSkill].State != "absent" {
		t.Errorf("after remove: %+v", st)
	}
}
