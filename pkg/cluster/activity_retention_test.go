package cluster

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

func TestActivityRetentionPerCategory(t *testing.T) {
	var entries []types.ActivityEntry
	entries = append(entries, types.ActivityEntry{Action: "update", Detail: "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6@sha256:" + strings.Repeat("a", 64)})
	for i := 0; i < 200; i++ { // Dashboard Dev noise after the update
		entries = append(entries, types.ActivityEntry{Action: "deploy-dashboard-pr", Detail: fmt.Sprintf("PR %d", i)})
	}
	got := trimActivity(entries)
	if got[0].Action != "update" {
		t.Fatal("operator history was evicted by Dashboard Dev entries")
	}
	if n := len(got) - 1; n != activityRetention["dashboard-dev"] {
		t.Fatalf("kept %d dashboard-dev entries", n)
	}
	if got[len(got)-1].Detail != "PR 199" {
		t.Fatal("newest entry not kept")
	}
	for i := 0; i < 60; i++ {
		got = trimActivity(append(got, types.ActivityEntry{Action: "refresh"}))
	}
	ops := 0
	for _, e := range got {
		if activityCategory(e.Action) == "operator" {
			ops++
		}
	}
	if ops != activityRetention["operator"] {
		t.Fatalf("operator entries %d", ops)
	}
}

func TestActivityStaysFarBelowConfigMapLimit(t *testing.T) {
	var entries []types.ActivityEntry
	for i := 0; i < 1000; i++ {
		entries = append(entries, types.ActivityEntry{Action: []string{"update", "deploy-pr", "create-dsc", "fix-maas-gateway", "unknown-x"}[i%5], Detail: strings.Repeat("d", maxActivityDetail)})
	}
	data, _ := json.Marshal(trimActivity(entries))
	if len(data) > maxActivityBytes || len(data) > 1<<20/2 {
		t.Fatalf("activity log %d bytes", len(data))
	}
}

func TestDescribeActivity(t *testing.T) {
	digest := "b772df9a0123456789abcdef" + strings.Repeat("0", 40)
	for _, tc := range []struct {
		action, detail, label, category, build string
	}{
		{"update", "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6@sha256:" + digest, "Updated to nightly", "operator", "rhoai-3.6 · b772df9a0123"},
		{"reinstall", "to nightly quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.5", "Operator reinstalled", "operator", "rhoai-3.5"},
		{"reinstall", "to latest GA from redhat-operators", "Operator reinstalled", "operator", ""},
		{"refresh", "rhods-operator.3.6.0", "Operator refreshed", "operator", ""},
		{"repair-dsc", "default-dsc: removed", "DataScienceCluster repaired", "setup", ""},
		{"deploy-dashboard-main", "x", "Dashboard main deployed", "dashboard-dev", ""},
		{"brand-new-action", "", "brand new action", "other", ""},
	} {
		e := types.ActivityEntry{Action: tc.action, Detail: tc.detail, Success: true}
		describeActivity(&e)
		if e.Label != tc.label || e.Category != tc.category || e.Build != tc.build {
			t.Errorf("%s %q: label=%q category=%q build=%q", tc.action, tc.detail, e.Label, e.Category, e.Build)
		}
	}
}

// Every action the code records has a label and a category.
func TestEveryRecordedActionIsDescribed(t *testing.T) {
	for _, a := range []string{"update", "refresh", "reinstall", "rollback", "deploy-pr", "deploy-dashboard-pr", "deploy-dashboard-main",
		"revert-dashboard", "setup-minio", "teardown-minio", "setup-pipeline-server", "teardown-pipeline-server", "setup-mlflow",
		"teardown-mlflow", "deploy-mlflow-pr", "revert-mlflow", "create-pull-secret", "create-dsc", "repair-dsc", "assist-rollout",
		"fix-delete-stale-webhooks", "fix-recreate-subscription", "fix-delete-stale-installplans", "fix-maas-gateway",
		"disable-component", "restart-operator"} {
		if _, ok := activityActions[a]; !ok {
			t.Errorf("action %q has no label", a)
		}
	}
}

func TestOperationMarkerRoundTrip(t *testing.T) {
	t.Setenv("NAMESPACE", "test-ns")
	stored := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/namespaces/test-ns/configmaps/rhoai-nightly-updater-operation" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		switch r.Method {
		case http.MethodPatch:
			if r.Header.Get("Content-Type") != "application/apply-patch+yaml" {
				t.Errorf("not a server-side apply: %s", r.Header.Get("Content-Type"))
			}
			var cm struct {
				Data map[string]string `json:"data"`
			}
			json.NewDecoder(r.Body).Decode(&cm)
			stored = cm.Data["operation"]
			w.Write([]byte(`{}`))
		case http.MethodGet:
			if stored == "missing" {
				w.WriteHeader(404)
				w.Write([]byte(`{"kind":"Status","status":"Failure","code":404}`))
				return
			}
			data, _ := json.Marshal(map[string]interface{}{"data": map[string]string{"operation": stored}})
			w.Write(data)
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
	}))
	defer srv.Close()
	c := &Client{baseURL: srv.URL, httpClient: srv.Client(), ctx: t.Context()}

	marker := &types.OperationMarker{Type: "update", Label: "Update to nightly", User: "alice", StartedAt: "2026-10-05T10:00:00Z", Pod: "p1"}
	if err := SaveOperationMarker(c, marker); err != nil {
		t.Fatal(err)
	}
	got, _, err := GetOperationState(c)
	if err != nil || got == nil || *got != *marker {
		t.Fatalf("got %+v err=%v", got, err)
	}
	if err := SaveOperationMarker(c, nil); err != nil {
		t.Fatal(err)
	}
	if got, _, err := GetOperationState(c); err != nil || got != nil {
		t.Fatalf("cleared marker: %+v %v", got, err)
	}
	stored = "missing"
	if got, _, err := GetOperationState(c); err != nil || got != nil {
		t.Fatalf("missing ConfigMap: %+v %v", got, err)
	}
}

// A failed or refused action is not labelled as if it succeeded, and quick
// resources have their own category now that they have their own page.
func TestDescribeActivity_FailedLabelsAndCategories(t *testing.T) {
	cases := []struct {
		entry         types.ActivityEntry
		label, catego string
	}{
		{types.ActivityEntry{Action: "update", Success: true}, "Updated to nightly", "operator"},
		{types.ActivityEntry{Action: "update", Success: false}, "Update to nightly failed", "operator"},
		{types.ActivityEntry{Action: "teardown-minio", Success: false}, "S3 storage teardown failed", "test-resources"},
		{types.ActivityEntry{Action: "setup-mlflow", Success: true}, "MLflow set up", "test-resources"},
		{types.ActivityEntry{Action: "deploy-dashboard-main", Success: true}, "Dashboard main deployed", "dashboard-dev"},
		{types.ActivityEntry{Action: "assist-rollout", Success: false}, "Stuck rollout assisted", "diagnostics"},
	}
	for _, tc := range cases {
		e := tc.entry
		describeActivity(&e)
		if e.Label != tc.label || e.Category != tc.catego {
			t.Errorf("%s success=%v: got %q/%q, want %q/%q", e.Action, e.Success, e.Label, e.Category, tc.label, tc.catego)
		}
	}
}
