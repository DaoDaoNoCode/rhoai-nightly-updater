package cluster

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/operator"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
)

func TestNightlyCatalogNeverFallsBackToStable(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"API error", 503, `{}`},
		{"stable package only", 200, strings.ReplaceAll(wrapPkgManifestList(`{"status":{"channels":[{"name":"fast","currentCSV":"rhods-operator.v3.7.0"}]}}`), rhoai.CatalogName, "redhat-operators")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var ambiguousQuery bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/packagemanifests/"+rhoai.SubName) {
					ambiguousQuery = true
				}
				if r.URL.Query().Get("labelSelector") != "catalog="+rhoai.CatalogName || r.URL.Query().Get("fieldSelector") != "metadata.name="+rhoai.SubName {
					t.Error("missing catalog selectors")
				}
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			c := newTestClient(testClientOptions{BaseURL: srv.URL, HTTPClient: srv.Client(), Ctx: context.Background()})
			ch, _ := operator.DetectNightlyChannel(c, "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.7")
			exists, _ := operator.ChannelExistsInCatalog(c, "fast")
			if ch != "" || exists || ambiguousQuery {
				t.Fatalf("channel=%q exists=%v ambiguous=%v", ch, exists, ambiguousQuery)
			}
		})
	}
}

func TestInstallPreflightFailureDoesNotRemoveOperator(t *testing.T) {
	for _, action := range []string{"update", "reinstall"} {
		t.Run(action, func(t *testing.T) {
			image := "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.5"
			responses, paths := buildUpdateStreamMocks(image)
			listPath, _ := pkgManifestPaths()
			responses[listPath] = mockResponse{body: `{"items":[]}`}
			c, requests, cleanup := newRecordingMockClient(responses)
			defer cleanup()
			result, err := operator.UpdateStream(c, image, func(UpdateStepEvent) {})
			if action == "reinstall" {
				result, err = Reinstall(c, "nightly", image, "")
			}
			if err != nil || result.Success {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			for _, req := range *requests {
				if req.Method == "DELETE" && (req.Path == paths["sub"] || req.Path == paths["cs"] || strings.Contains(req.Path, "/clusterserviceversions/")) {
					t.Fatalf("operator removed after failed preflight: %+v", req)
				}
			}
		})
	}
}

func TestInstallCleanupFailureRestoresPreviousDesiredState(t *testing.T) {
	for _, action := range []string{"update", "reinstall"} {
		t.Run(action, func(t *testing.T) {
			image := "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.5"
			responses, paths := buildUpdateStreamMocks(image)
			listPath, _ := pkgManifestPaths()
			responses[listPath] = mockResponse{body: wrapPkgManifestList(`{"status":{"channels":[{"name":"fast","currentCSV":"rhods-operator.v3.5.0"}]}}`)}
			responses["DELETE "+paths["csvDelete"]] = mockResponse{statusCode: 403, body: `{"kind":"Status","status":"Failure","message":"denied"}`}
			c, requests, cleanup := newRecordingMockClient(responses)
			defer cleanup()
			var events []UpdateStepEvent
			emit := func(e UpdateStepEvent) { events = append(events, e) }
			var resultMessage string
			var success bool
			if action == "update" {
				r, err := operator.UpdateStream(c, image, emit)
				if err != nil {
					t.Fatal(err)
				}
				success = r.Success
				resultMessage = r.Message
			} else {
				r, err := ReinstallStream(c, "nightly", image, "", emit)
				if err != nil {
					t.Fatal(err)
				}
				success = r.Success
				resultMessage = r.Message
			}
			if success || !strings.Contains(resultMessage, "restored") {
				t.Fatal(resultMessage)
			}
			var restored bool
			for _, e := range events {
				if e.Step == "restore_previous_operator" && e.Status == "success" {
					restored = true
				}
			}
			var subPatches int
			for _, req := range *requests {
				if req.Method == "PATCH" && req.Path == paths["sub"] {
					subPatches++
				}
			}
			if !restored || subPatches == 0 {
				t.Fatalf("restored=%v subscription patches=%d", restored, subPatches)
			}
		})
	}
}

func TestCSVUsesInstalledSubscriptionIdentity(t *testing.T) {
	subPath := rhoai.NamespacedPath("operators.coreos.com/v1alpha1", "subscriptions", rhoai.SubNS, rhoai.SubName)
	listPath := rhoai.NamespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", rhoai.SubNS, "")
	body, _ := json.Marshal(map[string]any{"items": []any{
		map[string]any{"metadata": map[string]string{"name": "rhods-operator.v3.3.0"}, "spec": map[string]string{"displayName": "Red Hat OpenShift AI", "version": "3.3.0"}},
		map[string]any{"metadata": map[string]string{"name": "rhods-operator.v3.7.0"}, "spec": map[string]string{"displayName": "Red Hat OpenShift AI", "version": "3.7.0"}},
	}})
	c, cleanup := newMockClient(map[string]mockResponse{subPath: {body: `{"status":{"installedCSV":"rhods-operator.v3.7.0"}}`}, listPath: {body: string(body)}})
	defer cleanup()
	csv, err := rhoai.GetCSV(c)
	if err != nil || csv.Version != "3.7.0" {
		t.Fatalf("CSV=%+v err=%v", csv, err)
	}
}

func TestCSVStaleSubscriptionIdentity(t *testing.T) {
	for _, tc := range []struct {
		name    string
		items   string
		wantErr bool
	}{
		{"surviving RHOAI CSV", `[{"metadata":{"name":"rhods-operator.v3.6.0"},"spec":{"displayName":"Red Hat OpenShift AI","version":"3.6.0"}}]`, true},
		{"renamed display name", `[{"metadata":{"name":"rhods-operator.v3.6.0"},"spec":{"displayName":"New branding","version":"3.6.0"}}]`, true},
		{"no CSVs", `[]`, false},
		{"unrelated operator", `[{"metadata":{"name":"other-operator.v1.0.0"},"spec":{"displayName":"Other Operator"}}]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			subPath := rhoai.NamespacedPath("operators.coreos.com/v1alpha1", "subscriptions", rhoai.SubNS, rhoai.SubName)
			listPath := rhoai.NamespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", rhoai.SubNS, "")
			c, cleanup := newMockClient(map[string]mockResponse{
				subPath:  {body: `{"status":{"installedCSV":"rhods-operator.v3.5.0"}}`},
				listPath: {body: `{"items":` + tc.items + `}`},
			})
			defer cleanup()
			csv, err := rhoai.GetCSV(c)
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "rhods-operator.v3.5.0") || !strings.Contains(err.Error(), "rhods-operator.v3.6.0") {
					t.Fatalf("expected stale identity error naming both CSVs; CSV=%+v err=%v", csv, err)
				}
			} else if err != nil || csv.Name != "" || csv.Phase != "Not Found" {
				t.Fatalf("expected no installed RHOAI CSV; CSV=%+v err=%v", csv, err)
			}
		})
	}
}

func TestInstallStaleCSVIdentityDoesNotRemoveOperator(t *testing.T) {
	for _, action := range []string{"update", "reinstall"} {
		t.Run(action, func(t *testing.T) {
			image := "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.5"
			responses, paths := buildUpdateStreamMocks(image)
			responses[paths["sub"]] = mockResponse{body: `{"spec":{"source":"rhoai-catalog-dev","channel":"fast"},"status":{"installedCSV":"rhods-operator.v3.4.0"}}`}
			c, requests, cleanup := newRecordingMockClient(responses)
			defer cleanup()
			var message string
			if action == "update" {
				result, err := operator.UpdateStream(c, image, func(UpdateStepEvent) {})
				if err != nil || result == nil || result.Success {
					t.Fatalf("result=%+v err=%v", result, err)
				}
				message = result.Message
			} else {
				result, err := ReinstallStream(c, "nightly", image, "", func(UpdateStepEvent) {})
				if err != nil || result == nil || result.Success {
					t.Fatalf("result=%+v err=%v", result, err)
				}
				message = result.Message
			}
			if !strings.Contains(message, "Cannot identify installed CSV") {
				t.Fatalf("unexpected failure: %s", message)
			}
			for _, req := range *requests {
				if req.Method == "DELETE" && (req.Path == paths["sub"] || req.Path == paths["cs"] || strings.Contains(req.Path, "/clusterserviceversions/")) {
					t.Fatalf("operator removed with stale identity: %+v", req)
				}
			}
		})
	}
}
