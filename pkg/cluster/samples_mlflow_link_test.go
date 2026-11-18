package cluster

import "testing"

// A sample's link opens its experiment in the dashboard's MLflow pages
// (odh-dashboard rhoai-3.6 frontend/src/routes/pipelines/mlflow.ts
// mlflowExperimentRoute: /develop-train/mlflow/experiments/<id>/runs with
// workspace and searchFilter), and MLflow's own UI only when the
// dashboard's address is unknown.
func TestMLflowSampleLinks(t *testing.T) {
	withDash := &mlflowTarget{ui: "https://rh-ai.apps.example.com/mlflow", dashboard: "https://rh-ai.apps.example.com", project: "my proj"}
	if got, want := withDash.experimentLink("7"), "https://rh-ai.apps.example.com/develop-train/mlflow/experiments/7/runs?searchFilter=&workspace=my+proj"; got != want {
		t.Errorf("with the dashboard: %s, want %s", got, want)
	}
	noDash := &mlflowTarget{ui: "https://mlflow.apps.example.com", project: "dev"}
	if got, want := noDash.experimentLink("7"), "https://mlflow.apps.example.com/#/experiments/7?workspace=dev"; got != want {
		t.Errorf("without the dashboard: %s, want %s", got, want)
	}
}
