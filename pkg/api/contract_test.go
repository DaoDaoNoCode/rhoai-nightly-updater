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
	"strings"
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
	// post is the body of a POST; "" is a GET.
	post string
	// before, when set, changes the stubs for this case only.
	before func(t *testing.T)
}

func TestFrontendContract(t *testing.T) {
	setupDevMode(t)
	setAdmin(t)
	stubContractData(t)
	started := time.Date(2026, 11, 20, 14, 5, 0, 0, time.UTC)
	stubRunningOperation(t, started)
	stubRecipeContract(t, started)

	cases := []contractCase{
		{"summary.json", "/api/summary", "user:carol", HandleSummary, "", nil},
		{"operation-user.json", "/api/operation", "user:carol", HandleOperation, "", nil},
		{"user-permissions-user.json", "/api/user/permissions", "user:carol", HandleUserPermissions, "", nil},
		{"user-permissions-admin.json", "/api/user/permissions", "user:admin", HandleUserPermissions, "", nil},
		{"project-status-user.json", "/api/resources/status?project=team-a", "user:carol", HandleResourcesStatus, "", nil},
		{"project-status-problems-user.json", "/api/resources/status?project=team-denied", "user:carol", HandleResourcesStatus, "", nil},
		{"playground-user.json", "/api/resources/playground?project=team-a", "user:carol", HandlePlaygroundStatus, "", nil},
		{"playground-problem-user.json", "/api/resources/playground?project=team-denied", "user:carol", HandlePlaygroundStatus, "", nil},
		{"samples-mlflow-user.json", "/api/resources/samples/mlflow?project=team-a", "user:carol", HandleMLflowSamplesStatus, "", nil},
		// The Model registry tab (read with the user's token; the page is a cluster-admin's).
		{"model-registry-admin.json", "/api/resources/model-registry", "user:admin", HandleModelRegistryStatus, "", nil},
		// Admin-only answers the Recipes page reads.
		{"recipe-run-admin.json", "/api/recipes/run", "user:admin", HandleRecipeRunStatus, "", nil},
		{"recipe-run-stopping-admin.json", "/api/recipes/run", "user:admin", HandleRecipeRunStatus, "", stubStoppingRecipeRun},
		{"recipe-run-ended-admin.json", "/api/recipes/run", "user:admin", HandleRecipeRunStatus, "", stubEndedRecipeRun},
		{"recipe-run-features-admin.json", "/api/recipes/run", "user:admin", HandleRecipeRunStatus, "", stubFeaturesRecipeRun},
		// The Workbench tab's status (admin-only, read with the admin's token).
		{"workbench-admin.json", "/api/resources/workbench?project=team-a", "user:admin", HandleWorkbenchStatus, "", nil},
		// The Distributed workloads tab's status (admin-only, the admin's token).
		{"kueue-admin.json", "/api/resources/kueue?project=team-a", "user:admin", HandleKueueStatus, "", nil},
		// The Observability tab's status (admin-only, the ServiceAccount's reads).
		{"observability-admin.json", "/api/resources/observability", "user:admin", HandleObservabilityStatus, "", nil},
		{"recipe-plan-admin.json", "/api/recipes/plan", "user:admin", HandleRecipePlan, `{"recipe":` + testRecipe + `}`, nil},
	}
	update := *updateContract || os.Getenv("UPDATE_CONTRACT") == "1"
	written := map[string]bool{}
	for _, tc := range cases {
		if tc.before != nil {
			tc.before(t)
		}
		w := httptest.NewRecorder()
		req := getAs(t, tc.path, tc.token)
		if tc.post != "" {
			req = postJSON(t, tc.path, tc.token, tc.post)
		}
		tc.h(w, req)
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
	origSummary, origProject, origPlayground, origSamples, origRegistry, origWorkbench, origKueue, origObservability := getSummary, getProjectStatus, getPlaygroundStatus, getMLflowSamplesStatus, getModelRegistryStatus, getWorkbenchStatus, getKueueStatus, getObservabilityStatus
	t.Cleanup(func() {
		getSummary, getProjectStatus, getPlaygroundStatus, getMLflowSamplesStatus, getModelRegistryStatus, getWorkbenchStatus, getKueueStatus, getObservabilityStatus = origSummary, origProject, origPlayground, origSamples, origRegistry, origWorkbench, origKueue, origObservability
	})
	getObservabilityStatus = func(_ *cluster.Client) (*types.ObservabilityStatus, error) {
		return &types.ObservabilityStatus{
			Blocked:   &types.ComponentBlock{Component: "monitoring", DSC: "default-dsci", State: "Managed", Reason: "shown only when blocked"},
			Namespace: "redhat-ods-monitoring", Configured: true, ManagedByTool: true, External: false, UsesS3Storage: true, Ready: false,
			Parts: []types.MaaSPart{
				{ID: "loki-operator", Title: "Loki Operator", State: "ready", Message: "Version 6.7.0 runs in openshift-operators-redhat", ManagedByTool: true, NotManaged: false},
				{ID: "metrics-collector", Title: "Metrics collector (OpenTelemetry)", State: "problem", Message: "This is a product bug.", Warning: "a warning",
					Commands: "oc delete pod -n openshift-opentelemetry-operator -l control-plane=controller-manager", DocURL: "https://github.com/opendatahub-io/odh-observability/blob/main/docs/observability-modes.md#metrics"},
			},
			SetupPlan: []string{"Wait until RHOAI's operator reports the monitoring stack available"}, SetupBlockedReason: "a reason",
			CleanupPlan: []string{"Delete Secret redhat-ods-monitoring/rhoai-nightly-updater-usage-logs"}, CleanupKept: []string{"The operators"}, CleanupBlockedReason: "a reason",
			Volumes:      []types.ObservabilityVolume{{Name: "storage-data-science-perses-0", Size: "1Gi", StorageClass: "gp3-csi", SetUpByTool: true}},
			VolumesTotal: "1Gi",
			Conditions:   []types.ObservabilityCondition{{Type: "Ready", Status: "True", Reason: "Available", Message: "a message"}},
			DashboardURL: "https://rh-ai.apps.example.com",
		}, nil
	}
	getKueueStatus = func(_, _ *cluster.Client, project string) (*types.KueueStatus, error) {
		f := false
		return &types.KueueStatus{
			Project:  project,
			Instance: types.KueueInstance{Found: true, Available: true, KueueReady: "True", V1beta2: true, Ready: true},
			Objects: []types.KueueObject{
				{Kind: "ResourceFlavor", Name: "nightly-kueue-cpu", Present: true, ManagedByTool: true, Active: true},
				{Kind: "ClusterQueue", Name: "nightly-kueue", Present: true, ManagedByTool: true, Active: true},
				{Kind: "LocalQueue", Name: "nightly-kueue", Namespace: project, Present: true, ManagedByTool: true, Active: true},
			},
			Quota: "14 CPUs, 44Gi of memory and 12Gi of ephemeral storage",
			Label: &types.KueueLabel{Present: true, AddedByTool: true},
			Samples: []types.KueueSample{
				{Kind: "raycluster", Name: "nightly-raycluster", Present: true, ManagedByTool: true, Image: "quay.io/modh/ray:2.47.1-py311-cu121", State: "Admitted", RayState: "ready"},
				{Kind: "rayjob", Name: "nightly-rayjob"},
			},
			OptIn:           types.KueueOptIn{DisableKueue: &f, SetByTool: true, Previous: "unset", On: true, Profile: types.KueueObject{Kind: "HardwareProfile", Name: "nightly-kueue-cpu", Namespace: "redhat-ods-applications", Present: true, ManagedByTool: true, Active: true}},
			DefaultRayImage: "quay.io/modh/ray:2.47.1-py311-cu121",
			DashboardURL:    "https://rh-ai.apps.example.com",
		}, nil
	}
	getModelRegistryStatus = func(_, _ *cluster.Client) (*types.ModelRegistryStatus, error) {
		return &types.ModelRegistryStatus{
			Name: "nightly-registry", Namespace: "rhoai-model-registries", Component: "aiHub", State: "ready", ManagedByTool: true,
			Address: "https://rh-ai.apps.example.com/model-registry/nightly-registry", DashboardURL: "https://rh-ai.apps.example.com",
			Sample: &types.ModelRegistrySample{Model: "qwen25-05b-instruct", Version: "v1", Registered: true, Complete: true, State: "LIVE", ID: "1",
				URI: "hf://Qwen/Qwen2.5-0.5B-Instruct:7ae557604adf67be50417f59c2c2f167def9a775"},
		}, nil
	}
	getWorkbenchStatus = func(_, _ *cluster.Client, project string) (*types.WorkbenchStatus, error) {
		return &types.WorkbenchStatus{
			Project: project,
			Items: []types.WorkbenchState{{
				ResourceState: types.ResourceState{Deployed: true, Ready: true, Namespace: project, Name: "nightly-workbench", ManagedByTool: true, UID: "nb-uid"},
				State:         "running", URL: "https://rh-ai.apps.example.com/notebook/" + project + "/nightly-workbench",
				Image: "Jupyter | Minimal | CPU | Python 3.12 (2025.2)", HardwareProfile: "Default", CPU: "2", Memory: "4Gi", ConnectionAttached: true,
				Storage: &types.WorkbenchStorage{Name: "nightly-workbench-storage", Size: "20Gi", StorageClass: "gp3-csi", ManagedByTool: true},
			}},
			Plan: &types.WorkbenchPlan{Image: "Jupyter | Minimal | CPU | Python 3.12", ImageStream: "s2i-minimal-notebook", ImageTag: "2025.2",
				HardwareProfile: "Default", HardwareProfileName: "redhat-ods-applications/default-profile", CPU: "2", Memory: "4Gi", Queue: "lq",
				StorageSize: "20Gi", StorageClass: "gp3-csi", Connection: true, Pipelines: true, MLflow: true},
			ItemsProblem: "Cannot read the workbench in team-b: k8s API error 503.", ItemsUnreadable: []string{"team-b"},
		}, nil
	}
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
		st.Items = []types.PlaygroundState{{ResourceState: types.ResourceState{Deployed: true, Ready: true, Namespace: project, Name: "lsd-genai-playground", Message: "Ready", ManagedByTool: true}, Phase: "Ready", Requested: true,
			Models: []types.PlaygroundModelRef{{Name: "qwen25-05b-instruct", Source: "namespace"}}}}
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

// stubRecipeContract gives the recipe answers representative data: a run
// that another updater pod runs (the operation's lease, stubRunningOperation),
// and a plan with a step of every action and a problem.
func stubRecipeContract(t *testing.T, started time.Time) {
	t.Helper()
	origRead, origPlan, origRecipes, origOwnership, origOwned := readRecipeRun, planRecipe, recipes, recipeOwnership, recipeOwnedProjects
	recipes = &recipeRunner{}
	t.Cleanup(func() {
		readRecipeRun, planRecipe, recipes, recipeOwnership, recipeOwnedProjects = origRead, origPlan, origRecipes, origOwnership, origOwned
	})
	// The projects recipes created that the cluster still has: this run's
	// and an earlier run's.
	recipeOwnedProjects = func(*cluster.Client) ([]types.RecipeOwnedProject, error) {
		return []types.RecipeOwnedProject{
			{Name: "recipe-dev", UID: "0f3c1d2e-uid", Recipe: "dev", CreatedAt: "2026-11-20T14:15:00Z"},
			{Name: "recipe-old", UID: "7a8b9c0d-uid", Recipe: "old", CreatedAt: "2026-11-19T09:00:00Z"},
		}, nil
	}
	// What the cluster shows of an ended run: its project, and the
	// component it turned on.
	recipeOwnership = func(_, _ *cluster.Client, run *types.RecipeRun) (map[string]bool, *types.CleanupRequest) {
		return map[string]bool{"project": true, "prerequisites": true}, &types.CleanupRequest{RecipeProjects: []string{run.Project.Name}, RecipeProjectUIDs: map[string]string{run.Project.Name: run.Project.UID}, Components: run.Components}
	}
	pinned := &types.RecipePinnedBuild{Tag: "rhoai-3.6", Image: "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6@sha256:" + strings.Repeat("a", 64), Digest: "sha256:" + strings.Repeat("a", 64)}
	at := func(m int) string { return started.Add(time.Duration(m) * time.Minute).Format(time.RFC3339) }
	readRecipeRun = func(*cluster.Client) (*types.RecipeRun, error) {
		rcp, _ := cluster.ValidateRecipe([]byte(testRecipe))
		return &types.RecipeRun{
			ID: "run1", OperationID: "op1", BootID: "another-process", Pod: "pod-2", User: "alice",
			StartedAt: at(0), ResumedAt: at(9), State: types.RecipeRunRunning, ErrorCode: "", Recipe: rcp, Pinned: pinned,
			Project: &types.RecipeProjectRef{Name: "recipe-dev", UID: "0f3c1d2e-uid", Owned: true},
			Steps: []types.RecipeRunStep{
				{ID: "rhoai", Title: "RHOAI", Status: types.RecipeStepSkipped, Message: "Already set up: RHOAI 3.6.0 is installed; the recipe keeps it.", StartedAt: at(9), FinishedAt: at(9), TypicalSeconds: 420, MaxSeconds: 900},
				{ID: "dsc", Title: "DataScienceCluster", Status: types.RecipeStepSuccess, Message: "DataScienceCluster created with default components", StartedAt: at(9), FinishedAt: at(10), TypicalSeconds: 60, MaxSeconds: 420},
				{ID: "project", Title: "Project recipe-dev", Status: types.RecipeStepFailed, Message: "Creating project recipe-dev failed.", ErrorCode: "forbidden", StartedAt: at(10), FinishedAt: at(10), TypicalSeconds: 10, MaxSeconds: 60},
				{ID: "prerequisites", Title: "What the features need", Status: types.RecipeStepRunning, StartedAt: at(11), TypicalSeconds: 300, MaxSeconds: 900},
				{ID: "s3", Title: "S3 storage", Status: types.RecipeStepPending, TypicalSeconds: 60, MaxSeconds: 300},
			},
			// An earlier attempt turned the component on.
			Components: []string{"aipipelines"},
		}, nil
	}
	planRecipe = func(_ *cluster.Client, r types.EnvironmentRecipe, _ *types.RecipePinnedBuild) (*types.RecipePlan, cluster.RecipeState) {
		return &types.RecipePlan{
			Recipe: r, Pinned: pinned, TypicalSeconds: 780, MaxSeconds: 3120,
			Warnings: []types.RecipeWarning{
				{Kind: types.RecipeWarningAdvice, Message: "Project recipe-dev also gets a pipeline server. Kueue manages only what is sent to a queue, so they keep running. A project of its own is still safer."},
				{Kind: types.RecipeWarningCluster, Message: "The dashboard's Kueue setting is turned on: in every new project on this cluster, workbenches and model deployments can only use the queue hardware profile."},
			},
			Steps: []types.RecipePlanStep{
				{ID: "rhoai", Title: "RHOAI", Action: types.RecipeStepRun, TypicalSeconds: 420, MaxSeconds: 900},
				{ID: "dsc", Title: "DataScienceCluster", Action: types.RecipeStepLater, Reason: "Checked when the run gets here: it depends on a step before it.", TypicalSeconds: 60, MaxSeconds: 420},
				{ID: "project", Title: "Project recipe-dev", Action: types.RecipeStepBlocked, Reason: "Project recipe-dev exists and was not created by a recipe.", Fix: &types.RecipeFix{Code: types.RecipeFixProjectForeign}, TypicalSeconds: 10, MaxSeconds: 60},
				{ID: "s3", Title: "S3 storage", Action: types.RecipeStepSkip, Reason: "The S3 storage is set up and ready.", TypicalSeconds: 60, MaxSeconds: 300},
			},
			Problems: []types.RecipeProblem{
				{Message: "Project recipe-dev: Project recipe-dev exists and was not created by a recipe.", Fix: &types.RecipeFix{Code: types.RecipeFixProjectForeign}},
				{Field: "spec.rhoai.build", Message: "Cannot resolve the build on Quay.", Fix: &types.RecipeFix{Code: types.RecipeFixBuildUnresolvable}},
				{Message: "The one-time cluster setup is not done: additional-pull-secret is missing in kube-system. Run the one-time setup on the Status page first.", Fix: &types.RecipeFix{Code: types.RecipeFixOneTimeSetupMissing, Page: "/"}},
			},
		}, cluster.RecipeState{Recipe: r}
	}
}

// stubEndedRecipeRun is a run that failed after a resume: what its
// attempts set up is the run's, and the answer carries its Clean up.
// stubStoppingRecipeRun is the run of recipe-run-admin.json (the case
// before, whose stub it wraps) without its failed step, recorded by its
// pod with ending stopped: Stop was asked for there.
func stubStoppingRecipeRun(t *testing.T) {
	t.Helper()
	read := readRecipeRun
	readRecipeRun = func(c *cluster.Client) (*types.RecipeRun, error) {
		run, err := read(c)
		if run != nil {
			run.Ending = "stopped"
			s := &run.Steps[2]
			s.Status, s.Message, s.ErrorCode = types.RecipeStepSuccess, "Project recipe-dev created.", ""
		}
		return run, err
	}
}

func stubEndedRecipeRun(t *testing.T) {
	t.Helper()
	rcp, _ := cluster.ValidateRecipe([]byte(testRecipe))
	at := func(m int) string { return time.Date(2026, 11, 20, 14, 5+m, 0, 0, time.UTC).Format(time.RFC3339) }
	readRecipeRun = func(*cluster.Client) (*types.RecipeRun, error) {
		return &types.RecipeRun{
			ID: "run1", OperationID: "op2", BootID: "another-process", Pod: "pod-2", User: "alice",
			StartedAt: at(0), ResumedAt: at(20), FinishedAt: at(31), State: types.RecipeRunFailed, ErrorCode: "timeout",
			StopReason: "Step 5 of 5 (Pipeline server) failed: The pipeline server in recipe-dev was set up but is not ready. Resume waits again.",
			Recipe:     rcp,
			Project:    &types.RecipeProjectRef{Name: "recipe-dev", UID: "0f3c1d2e-uid", Owned: true},
			Steps: []types.RecipeRunStep{
				{ID: "rhoai", Title: "RHOAI", Status: types.RecipeStepSkipped, Message: "Already set up: RHOAI 3.6.0 is installed; the recipe keeps it.", StartedAt: at(20), FinishedAt: at(20), TypicalSeconds: 420, MaxSeconds: 900},
				{ID: "project", Title: "Project recipe-dev", Status: types.RecipeStepSkipped, Message: "Already set up: Project recipe-dev was created by a recipe.", StartedAt: at(20), FinishedAt: at(20), TypicalSeconds: 10, MaxSeconds: 60},
				{ID: "prerequisites", Title: "What the features need", Status: types.RecipeStepSkipped, Message: "Already set up: Everything the features need is there.", StartedAt: at(20), FinishedAt: at(20), TypicalSeconds: 300, MaxSeconds: 900},
				{ID: "s3", Title: "S3 storage", Status: types.RecipeStepSuccess, Message: "S3 storage is ready.", StartedAt: at(20), FinishedAt: at(21), TypicalSeconds: 60, MaxSeconds: 300},
				{ID: "pipeline-server", Title: "Pipeline server", Status: types.RecipeStepFailed, Message: "The pipeline server in recipe-dev was set up but is not ready. Resume waits again.", ErrorCode: "timeout", StartedAt: at(21), FinishedAt: at(31), TypicalSeconds: 120, MaxSeconds: 600},
			},
			Components: []string{"aipipelines"},
		}, nil
	}
}

// testRecipeFeatures is a recipe with every feature of recipes phase 2.
const testRecipeFeatures = `{"apiVersion":"rhoai-nightly-updater/v1alpha1","kind":"EnvironmentRecipe","metadata":{"name":"full"},
"spec":{"rhoai":{"build":"latest"},"dataScienceCluster":{"create":true},"project":{"mode":"create","name":"recipe-full"},
"features":{"pipelines":{"enabled":true},"mlflow":{"enabled":true,"samples":true},"models":{"enabled":true,"model":"qwen3-1-7b"},
"playground":{"enabled":true},"maas":{"enabled":true,"database":"development","publish":true},"modelRegistry":{"enabled":true,"sample":true},"workbench":{"enabled":true},
"kueue":{"enabled":true,"rayCluster":true,"dashboardOptIn":true}},
"dashboardDev":{"deploy":"pr","pr":4321}}}`

// stubFeaturesRecipeRun is a run of every feature that succeeded: its
// steps are the runner's own (titles and figures), and its Clean up
// offers the project, the MaaS platform and the model registry it created,
// and the components it turned on that are still as it left them (kserve,
// and kueue in its own on state, Unmanaged).
func stubFeaturesRecipeRun(t *testing.T) {
	t.Helper()
	rcp, problems := cluster.ValidateRecipe([]byte(testRecipeFeatures))
	if len(problems) > 0 {
		t.Fatal(problems)
	}
	at := func(m int) string { return time.Date(2026, 11, 20, 15, m, 0, 0, time.UTC).Format(time.RFC3339) }
	var steps []types.RecipeRunStep
	for i, s := range cluster.RecipeSteps(cluster.RecipeState{Recipe: rcp, KueueFirst: true}) {
		typical, most := s.Seconds()
		st := types.RecipeRunStep{ID: s.ID, Title: s.Title, Status: types.RecipeStepSuccess, Message: s.Title + " is set up.", StartedAt: at(i * 3), FinishedAt: at(i*3 + 2), TypicalSeconds: typical, MaxSeconds: most}
		if s.ID == "rhoai" {
			st.Status, st.Message = types.RecipeStepSkipped, "Already set up: RHOAI 3.6.0 is installed; the recipe keeps it."
		}
		steps = append(steps, st)
	}
	readRecipeRun = func(*cluster.Client) (*types.RecipeRun, error) {
		return &types.RecipeRun{
			ID: "run2", OperationID: "op3", BootID: "another-process", Pod: "pod-2", User: "alice",
			StartedAt: at(0), FinishedAt: at(len(steps) * 3), State: types.RecipeRunSucceeded, StopReason: "Every step of full is set up.",
			Recipe: rcp, Project: &types.RecipeProjectRef{Name: "recipe-full", UID: "1a2b3c4d-uid", Owned: true}, Steps: steps,
			Exposure:     "default-gateway",
			CreatedParts: []types.RecipeCreatedPart{{Part: types.RecipePartMaaSPlatform, UID: "creds-uid"}, {Part: types.RecipePartModelRegistry, UID: "registry-uid"}, {Part: types.RecipePartKueue, UID: "clusterqueue-uid"}},
			Components:   []string{"aipipelines", "kserve", "kueue"},
			ComponentIDs: map[string]string{"aipipelines": "5f0c2a9e1b7d4c36", "kserve": "a41e9c07d2b85f13", "kueue": "c3d8e15a90f2b674"},
		}, nil
	}
	recipeOwnership = func(_, _ *cluster.Client, run *types.RecipeRun) (map[string]bool, *types.CleanupRequest) {
		return map[string]bool{"project": true, "maas": true, "model-registry": true, "prerequisites": true, "kueue-prereq": true, "kueue": true},
			&types.CleanupRequest{RecipeProjects: []string{run.Project.Name}, RecipeProjectUIDs: map[string]string{run.Project.Name: run.Project.UID},
				MaaSPlatform: true, ModelRegistry: true, Components: []string{"kserve", "kueue"},
				ComponentIDs: map[string]string{"kserve": "a41e9c07d2b85f13", "kueue": "c3d8e15a90f2b674"},
				Kueue:        true, KueueUID: "clusterqueue-uid",
				RecipeRunID: run.ID, MaaSPlatformUID: "creds-uid", ModelRegistryUID: "registry-uid"}
	}
}
