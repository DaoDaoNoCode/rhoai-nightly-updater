package api

import (
	"bytes"
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The frontend's fixtures of the per-user API answers are generated here,
// from the Go types through the real handlers (so the role's reductions
// apply), and checked in under frontend/src/test/contract/. The frontend's
// tests load them (frontend/src/test/contract.ts), so a change of a Go type
// that the UI does not follow fails a frontend test, and a change here that
// is not regenerated fails this test.
//
// Regenerate after a deliberate change:
//
//	go test ./pkg/api -run TestFrontendContract -update-contract
//
// (or UPDATE_CONTRACT=1 go test ./pkg/api -run TestFrontendContract).
var updateContract = flag.Bool("update-contract", false, "rewrite frontend/src/test/contract/*.json from the Go types")

const contractDir = "../../frontend/src/test/contract"

// contractCase is one fixture: the handler, the request, and the stubs
// that give it representative data.
type contractCase struct {
	file  string
	path  string
	token string
	h     http.HandlerFunc
}

func TestFrontendContract(t *testing.T) {
	setupDevMode(t)
	setAdmin(t)
	stubContractData(t)
	started := time.Date(2026, 11, 20, 14, 5, 0, 0, time.UTC)
	stubRunningOperation(t, started)

	cases := []contractCase{
		{"summary.json", "/api/summary", "user:carol", HandleSummary},
		{"operation-user.json", "/api/operation", "user:carol", HandleOperation},
		{"user-permissions-user.json", "/api/user/permissions", "user:carol", HandleUserPermissions},
		{"user-permissions-admin.json", "/api/user/permissions", "user:admin", HandleUserPermissions},
		{"project-status-user.json", "/api/resources/status?project=team-a", "user:carol", HandleResourcesStatus},
		{"project-status-problems-user.json", "/api/resources/status?project=team-denied", "user:carol", HandleResourcesStatus},
		{"playground-user.json", "/api/resources/playground?project=team-a", "user:carol", HandlePlaygroundStatus},
		{"playground-problem-user.json", "/api/resources/playground?project=team-denied", "user:carol", HandlePlaygroundStatus},
		{"samples-mlflow-user.json", "/api/resources/samples/mlflow?project=team-a", "user:carol", HandleMLflowSamplesStatus},
	}
	update := *updateContract || os.Getenv("UPDATE_CONTRACT") == "1"
	written := map[string]bool{}
	for _, tc := range cases {
		w := httptest.NewRecorder()
		tc.h(w, getAs(t, tc.path, tc.token))
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", tc.file, w.Code, w.Body.String())
		}
		var v any
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Fatalf("%s: %v", tc.file, err)
		}
		got, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, '\n')
		path := filepath.Join(contractDir, tc.file)
		written[tc.file] = true
		if update {
			if err := os.MkdirAll(contractDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, got, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: %v; generate it with -update-contract", path, err)
			continue
		}
		if !bytes.Equal(want, got) {
			t.Errorf("%s differs from what the Go types give; if the change is deliberate, regenerate with `go test ./pkg/api -run TestFrontendContract -update-contract` and update the frontend.\ngot:\n%s", path, got)
		}
	}
	// No stale fixture.
	entries, _ := os.ReadDir(contractDir)
	var stale []string
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".json" && !written[e.Name()] {
			stale = append(stale, e.Name())
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("fixtures no case writes (remove them): %v", stale)
	}
}

// stubContractData gives every per-user handler representative data: each
// field set, so a renamed or retyped field shows in the fixtures.
func stubContractData(t *testing.T) {
	t.Helper()
	origSummary, origProject, origPlayground, origSamples := getSummary, getProjectStatus, getPlaygroundStatus, getMLflowSamplesStatus
	t.Cleanup(func() {
		getSummary, getProjectStatus, getPlaygroundStatus, getMLflowSamplesStatus = origSummary, origProject, origPlayground, origSamples
	})
	getSummary = func(*cluster.Client) (*types.Summary, error) {
		return &types.Summary{
			RHOAI: types.SummaryRHOAI{Installed: true, Version: "3.6.0", Channel: "beta", Phase: "Succeeded"},
			Nightly: types.SummaryNightly{
				InstalledBuild: &types.SummaryBuild{Tag: "rhoai-3.6", BuildDate: "2026-11-19T03:12:00Z"},
				LatestBuild:    &types.SummaryBuild{Tag: "rhoai-3.6", BuildDate: "2026-11-20T03:10:00Z"},
				NewerAvailable: true,
			},
			DSC: types.SummaryDSC{Exists: true, Ready: false, Components: []types.SummaryComponent{
				{Name: "dashboard", ManagementState: "Managed", Ready: true},
				{Name: "kserve", ManagementState: "Managed", Ready: false},
				{Name: "ray", ManagementState: "Removed", Ready: false},
			}},
		}, nil
	}
	getProjectStatus = func(_, _ *cluster.Client, project string) (*types.ProjectStatus, error) {
		if project == "team-denied" {
			return &types.ProjectStatus{Project: project,
				PipelineServersProblem: "Your OpenShift account may not read the pipeline servers of project team-denied.", PipelineServersProblemCode: types.ProblemForbiddenForUser,
				ModelsProblem: "Model serving is not installed on this cluster: it has no LLMInferenceService API.", ModelsProblemCode: types.ProblemNotInstalled,
				InferenceServicesProblem: "Cannot read the InferenceServices of project team-denied: k8s API error 503 (ServiceUnavailable)", InferenceServicesProblemCode: types.ProblemError,
				Models: []types.ModelState{}, InferenceServices: nil,
			}, nil
		}
		return &types.ProjectStatus{
			Project:      project,
			DashboardURL: "https://rh-ai.apps.example.com",
			PipelineServers: []types.ResourceState{{Deployed: true, Ready: true, Namespace: project, Name: "dspa",
				APIRoute: "https://ds-pipeline-dspa-team-a.apps.example.com", UIRoute: "https://rh-ai.apps.example.com/develop-train/pipelines/definitions/team-a", Message: "Running",
				ManagedByTool: true, ConnectionState: cluster.ConnectionPresent}},
			Models: []types.ModelState{{ResourceState: types.ResourceState{Deployed: true, Ready: true, Namespace: project, Name: "qwen25-05b-instruct", Message: "Running", ManagedByTool: true},
				DisplayName: "Qwen2.5 0.5B Instruct (CPU)", URL: "https://qwen.apps.example.com", ImageKind: "preset"}},
			InferenceServices: []types.ProjectInferenceService{{Name: "iris", Ready: false, Message: "Loading", URL: "https://iris.apps.example.com"}},
		}, nil
	}
	getPlaygroundStatus = func(_, _ *cluster.Client, project string) (*types.PlaygroundStatus, error) {
		st := &types.PlaygroundStatus{Project: project, OperatorReady: "True", OperatorMessage: "not shown to users", GenAIUI: "available"}
		if project == "team-denied" {
			st.APIProblem, st.APICode = "The dashboard's gen-ai API refused the request (HTTP 403: Forbidden).", "forbidden"
			return st, nil
		}
		st.Items = []types.PlaygroundState{{ResourceState: types.ResourceState{Deployed: true, Ready: true, Namespace: project, Name: "lsd-genai-playground", Message: "Ready", ManagedByTool: true}, Phase: "Ready", Requested: true}}
		st.Models = []types.PlaygroundModel{{Name: "qwen25-05b-instruct", DisplayName: "Qwen2.5 0.5B Instruct (CPU)", Source: "namespace", Status: "Running", Ready: true, Chat: true}}
		return st, nil
	}
	getMLflowSamplesStatus = func(_, _ *cluster.Client, project string) (*types.MLflowSamplesStatus, error) {
		return &types.MLflowSamplesStatus{Project: project, Available: true, MLflowURL: "https://mlflow.apps.example.com/mlflow", Samples: []types.SampleStatus{
			{ID: cluster.SampleGenAIDemo, Name: "Agent and prompt demo", State: "absent", Actions: []string{"add"}},
			{ID: cluster.SampleTraining, Name: "Training experiment and registered model", State: "present", Link: "https://mlflow.apps.example.com/mlflow/#/experiments/3?workspace=team-a", Actions: []string{"remove"}},
			{ID: cluster.SampleMCPServer, Name: "MCP server", State: "unknown", Message: "Cannot read the sample's state: MLflow answered 403 PERMISSION_DENIED.", Actions: []string{}},
		}}, nil
	}
}

// stubRunningOperation makes another updater process run an operation.
func stubRunningOperation(t *testing.T, started time.Time) {
	t.Helper()
	readOperationState = func(*cluster.Client) (*cluster.OperationRecord, error) {
		return &cluster.OperationRecord{Lease: &types.OperationLease{ID: "op1", BootID: "another-process", Pod: "pod-2", User: "alice",
			Type: "update", Label: "Update to nightly", Target: "rhoai-3.6", StartedAt: started.Format(time.RFC3339), HeartbeatAt: clk.Now().UTC().Format(time.RFC3339)}}, nil
	}
}
