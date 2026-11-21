package cluster

import (
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/mlflow"
)

// A sample's link opens its experiment in the dashboard's MLflow pages
// (odh-dashboard rhoai-3.6 frontend/src/routes/pipelines/mlflow.ts
// mlflowExperimentRoute: /develop-train/mlflow/experiments/<id>/runs with
// workspace and searchFilter), and MLflow's own UI only when the
// dashboard's address is unknown.
func TestMLflowSampleLinks(t *testing.T) {
	withDash := &mlflow.Target{UI: "https://rh-ai.apps.example.com/mlflow", Dashboard: "https://rh-ai.apps.example.com", Project: "my proj"}
	if got, want := withDash.ExperimentLink("7"), "https://rh-ai.apps.example.com/develop-train/mlflow/experiments/7/runs?searchFilter=&workspace=my+proj"; got != want {
		t.Errorf("with the dashboard: %s, want %s", got, want)
	}
	noDash := &mlflow.Target{UI: "https://mlflow.apps.example.com", Project: "dev"}
	if got, want := noDash.ExperimentLink("7"), "https://mlflow.apps.example.com/#/experiments/7?workspace=dev"; got != want {
		t.Errorf("without the dashboard: %s, want %s", got, want)
	}
}
