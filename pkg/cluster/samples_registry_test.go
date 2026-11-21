package cluster

import (
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/mlflow"
)

func TestMCPSample_AddRemove(t *testing.T) {
	env := newSamplesEnv(t)
	ok, msg, steps := env.run(t, "add", SampleMCPServer)
	if !ok {
		t.Fatalf("%s\n%s", msg, stepMessages(steps))
	}
	srv := env.mlflow.mcp[mlflow.MCPSampleName]
	if srv == nil || srv.versions[mlflow.MCPSampleVersion] != "active" || srv.endpoints != 1 || srv.tags[mlflow.SampleMarkerKey] != SampleMCPServer || srv.description != mlflow.MCPSampleDescription {
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
	want := []string{"PATCH /api/3.0/mlflow/mcp-servers/" + mlflow.MCPSampleName + "/versions/" + mlflow.MCPSampleVersion, "DELETE /api/3.0/mlflow/mcp-servers/" + mlflow.MCPSampleName}
	if got := env.mlflow.writes(n); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("remove writes %v, want deprecate then delete", got)
	}
	if env.mlflow.mcp[mlflow.MCPSampleName] != nil {
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
	if ok, msg, _ := env.run(t, "add", SampleMCPServer, mlflow.SampleSkill); !ok {
		t.Fatal(msg)
	}
	mcpGet := "GET /api/3.0/mlflow/mcp-servers/" + mlflow.MCPSampleName
	skillGet := "GET /api/3.0/mlflow/skills/" + mlflow.SkillSampleName
	env.mlflow.bareNext[mcpGet], env.mlflow.bareNext[skillGet] = 404, 404
	got := env.status(t)
	for _, id := range []string{SampleMCPServer, mlflow.SampleSkill} {
		if got[id].State != "unknown" {
			t.Errorf("%s status = %+v, want unknown", id, got[id])
		}
	}
	env.mlflow.bareNext[mcpGet], env.mlflow.bareNext[skillGet] = 404, 404
	ok, msg, steps := env.run(t, "remove", SampleMCPServer, mlflow.SampleSkill)
	if ok || strings.Contains(stepMessages(steps), "nothing to remove") {
		t.Errorf("remove succeeded on a bare 404: %s\n%s", msg, stepMessages(steps))
	}
	if env.mlflow.mcp[mlflow.MCPSampleName] == nil || env.mlflow.skills[mlflow.SkillSampleName] == nil {
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
				if strings.Contains(path, "/mcp-servers/") && env.mlflow.mcp[mlflow.MCPSampleName] == nil {
					env.mlflow.mcp[mlflow.MCPSampleName] = tc.mcp
				}
				if strings.Contains(path, "/skills/") && env.mlflow.skills[mlflow.SkillSampleName] == nil {
					env.mlflow.skills[mlflow.SkillSampleName] = tc.skill
				}
			}
			theirMCP := len(tc.mcp.versions)
			theirSkill := tc.skill.versions
			ok, msg, steps := env.run(t, "add", SampleMCPServer, mlflow.SampleSkill)
			if ok || !strings.Contains(msg, "not_managed") || !strings.Contains(stepMessages(steps), "Someone else created") {
				t.Fatalf("%s\n%s", msg, stepMessages(steps))
			}
			srv, sk := env.mlflow.mcp[mlflow.MCPSampleName], env.mlflow.skills[mlflow.SkillSampleName]
			if srv == nil || srv.tags[mlflow.SampleMarkerKey] != "" || len(srv.versions) != theirMCP {
				t.Errorf("their MCP server was changed: %+v", srv)
			}
			if _, left := srv.versions[mlflow.MCPSampleVersion]; left {
				t.Error("the tool's MCP version was left in their server")
			}
			if sk == nil || sk.tags[mlflow.SampleMarkerKey] != "" || sk.versions-len(sk.deleted) != theirSkill {
				t.Errorf("their skill was changed: %+v", sk)
			}
		})
	}
}

func TestMCPSample_PartialIsCompleted(t *testing.T) {
	env := newSamplesEnv(t)
	env.mlflow.mcp[mlflow.MCPSampleName] = &fakeMCP{tags: map[string]string{mlflow.SampleMarkerKey: SampleMCPServer}, versions: map[string]string{mlflow.MCPSampleVersion: "deprecated"}}
	s := env.status(t)[SampleMCPServer]
	if s.State != "partial" || len(s.Missing) != 2 {
		t.Errorf("status %+v", s)
	}
	if ok, msg, _ := env.run(t, "add", SampleMCPServer); !ok {
		t.Fatal(msg)
	}
	if srv := env.mlflow.mcp[mlflow.MCPSampleName]; srv.versions[mlflow.MCPSampleVersion] != "active" || srv.endpoints != 1 {
		t.Errorf("server %+v", srv)
	}
}

// A server whose marker tag cannot be set is deleted again, so no unmarked
// entry the tool made stays behind.
func TestMCPSample_UnmarkedServerIsRolledBack(t *testing.T) {
	env := newSamplesEnv(t)
	env.mlflow.failNext["POST /api/3.0/mlflow/mcp-servers/"+mlflow.MCPSampleName+"/tags"] = 500
	if ok, msg, _ := env.run(t, "add", SampleMCPServer); ok {
		t.Fatalf("got %s", msg)
	}
	if env.mlflow.mcp[mlflow.MCPSampleName] != nil {
		t.Error("an unmarked server stayed behind")
	}
}

func TestRegistrySamples_ForeignAndDenied(t *testing.T) {
	env := newSamplesEnv(t)
	env.mlflow.mcp[mlflow.MCPSampleName] = &fakeMCP{tags: map[string]string{}, versions: map[string]string{mlflow.MCPSampleVersion: "active"}, endpoints: 1}
	env.mlflow.skills[mlflow.SkillSampleName] = &fakeSkill{tags: map[string]string{}, versions: 1}
	for _, action := range []string{"add", "remove"} {
		n := len(env.mlflow.requests)
		ok, msg, _ := env.run(t, action, SampleMCPServer, mlflow.SampleSkill)
		if ok || !strings.Contains(msg, "not_managed") || len(env.mlflow.writes(n)) != 0 {
			t.Errorf("%s: %s writes %v", action, msg, env.mlflow.writes(n))
		}
	}

	env2 := newSamplesEnv(t)
	env2.mlflow.userAllow = func(resource, verb, ns, name string) bool { return verb != "update" }
	ok, msg, steps := env2.run(t, "add", SampleMCPServer, mlflow.SampleSkill)
	if ok || !strings.Contains(msg, "forbidden") || strings.Count(stepMessages(steps), "403 PERMISSION_DENIED") != 2 {
		t.Errorf("%s\n%s", msg, stepMessages(steps))
	}
}

func TestSkillSample_AddRemoveAndMissingAPI(t *testing.T) {
	env := newSamplesEnv(t)
	if ok, msg, steps := env.run(t, "add", mlflow.SampleSkill); !ok {
		t.Fatalf("%s\n%s", msg, stepMessages(steps))
	}
	sk := env.mlflow.skills[mlflow.SkillSampleName]
	if sk == nil || sk.versions != 1 || sk.tags[mlflow.SampleMarkerKey] != mlflow.SampleSkill || sk.source["ref"] != mlflow.SkillSampleRef || sk.source["subpath"] != mlflow.SkillSampleSubpath {
		t.Fatalf("skill %+v", sk)
	}
	if s := env.status(t)[mlflow.SampleSkill]; s.State != "present" {
		t.Errorf("status %+v", s)
	}
	n := len(env.mlflow.requests)
	if ok, _, _ := env.run(t, "add", mlflow.SampleSkill); !ok || len(env.mlflow.writes(n)) != 0 {
		t.Errorf("second add wrote %v", env.mlflow.writes(n))
	}
	if ok, msg, _ := env.run(t, "remove", mlflow.SampleSkill); !ok || env.mlflow.skills[mlflow.SkillSampleName] != nil {
		t.Errorf("remove: %s", msg)
	}

	env.mlflow.noSkills = true
	if ok, msg, steps := env.run(t, "add", mlflow.SampleSkill); ok || !strings.Contains(stepMessages(steps), "no Skill Registry") {
		t.Errorf("add without the API: %s\n%s", msg, stepMessages(steps))
	}
	if ok, msg, _ := env.run(t, "remove", mlflow.SampleSkill); !ok {
		t.Errorf("remove without the API: %s", msg)
	}
}

func TestMLflowSamples_AddAllThenRemoveAll(t *testing.T) {
	env := newSamplesEnv(t)
	ok, msg, steps := env.run(t, "add", mlflow.SampleSkill, SampleMCPServer, SampleTraining, SampleGenAIDemo)
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
	if ok, msg, _ := env.run(t, "remove", SampleTraining, SampleMCPServer, mlflow.SampleSkill); !ok {
		t.Fatal(msg)
	}
	st := env.status(t)
	if st[SampleGenAIDemo].State != "present" || st[SampleTraining].State != "removed" || st[SampleMCPServer].State != "absent" || st[mlflow.SampleSkill].State != "absent" {
		t.Errorf("after remove: %+v", st)
	}
}
