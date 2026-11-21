package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/catalog"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
)

func stableCatalogMock(source, channel, version string) mockResponse {
	return mockResponse{body: stableManifestJSON(source, channel, []map[string]any{
		{"name": channel, "currentCSV": "rhods-operator.v" + version, "currentCSVDesc": map[string]string{"version": version}},
	})}
}

func stableManifestJSON(source, defaultChannel string, channels []map[string]any) string {
	body, _ := json.Marshal(map[string]any{
		"items": []any{map[string]any{
			"metadata": map[string]string{"name": rhoai.SubName},
			"status": map[string]any{
				"packageName": rhoai.SubName, "catalogSource": source, "catalogSourceNamespace": rhoai.CatalogNS,
				"defaultChannel": defaultChannel, "channels": channels,
			},
		}},
	})
	return string(body)
}

func TestStableTargetDiscovery(t *testing.T) {
	channel := func(name, version string) map[string]any {
		return map[string]any{"name": name, "currentCSV": "rhods-operator.v" + version, "currentCSVDesc": map[string]string{"version": version}}
	}
	tests := []struct {
		name, defaultChannel, override, wantChannel, wantVersion string
		channels                                                 []map[string]any
		wantError                                                bool
	}{
		{"current release", "stable-3.4", "", "stable-3.5", "3.5.0", []map[string]any{channel("stable-3.4", "3.4.0"), channel("stable-3.5", "3.5.0")}, false},
		{"numeric versions", "stable-3.9", "", "stable-3.10", "3.10.2", []map[string]any{channel("stable-3.9", "3.9.9"), channel("stable-3.10", "3.10.2")}, false},
		{"future plain EA excluded", "beta", "", "stable-3.7", "3.7.0", []map[string]any{channel("beta", "3.8.0-ea"), channel("stable-3.7", "3.7.0"), channel("stable-9.1", "9.1.0-ea.1")}, false},
		{"preview excluded", "alpha", "", "stable", "3.5.0", []map[string]any{channel("alpha", "10.0.0"), channel("fast", "3.6.0-rc.1"), channel("stable", "3.5.0")}, false},
		{"newer production fast", "stable", "", "fast", "3.7.0", []map[string]any{channel("stable", "3.5.0"), channel("fast", "3.7.0")}, false},
		{"prefer catalog default on tie", "fast", "", "fast", "3.5.0", []map[string]any{channel("stable-3.5", "3.5.0"), channel("fast", "3.5.0")}, false},
		{"prefer stable on tie", "alpha", "", "stable-3.5", "3.5.0", []map[string]any{channel("fast", "3.5.0"), channel("stable-3.5", "3.5.0")}, false},
		{"configured older GA", "stable-3.5", "stable-3.3", "stable-3.3", "3.3.2", []map[string]any{channel("stable-3.5", "3.5.0"), channel("stable-3.3", "3.3.2")}, false},
		{"configured missing channel", "stable", "stable-missing", "", "", []map[string]any{channel("stable", "3.5.0")}, true},
		{"configured EA rejected", "beta", "beta", "", "", []map[string]any{channel("beta", "3.7.0-ea")}, true},
		{"missing description uses CSV", "stable", "", "stable", "4.12.0+build.42", []map[string]any{{"name": "stable", "currentCSV": "rhods-operator.v4.12.0+build.42"}}, false},
		{"no GA", "fast", "", "", "", []map[string]any{channel("fast", "3.7.0-ea")}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("STABLE_SOURCE", "redhat-operators")
			t.Setenv("STABLE_CHANNEL", tt.override)
			client, cleanup := newMockClient(map[string]mockResponse{
				rhoai.NamespacedPath("packages.operators.coreos.com/v1", "packagemanifests", rhoai.CatalogNS, ""): {body: stableManifestJSON("redhat-operators", tt.defaultChannel, tt.channels)},
			})
			defer cleanup()
			target, err := catalog.ResolveStableTarget(client)
			if (err != nil) != tt.wantError || target.Channel != tt.wantChannel || target.Version != tt.wantVersion || target.Pinned != (tt.override != "" && !tt.wantError) {
				t.Fatalf("target=%+v error=%v", target, err)
			}
		})
	}
}

func TestStableDiscoveryScopesCatalogAndRefreshes(t *testing.T) {
	t.Setenv("STABLE_SOURCE", "mirrored-operators")
	t.Setenv("STABLE_CHANNEL", "")
	version := "3.5.0"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if dashboardOperatorAbsent(w, r) {
			return
		}
		if r.URL.Query().Get("labelSelector") != "catalog=mirrored-operators" {
			t.Errorf("unscoped package query: %s", r.URL.String())
		}
		if r.URL.Query().Get("fieldSelector") != "metadata.name="+rhoai.SubName {
			t.Errorf("package name filter missing: %s", r.URL.String())
		}
		// Include a newer nightly package despite the selector, to verify source identity.
		var stable, nightly map[string]any
		json.Unmarshal([]byte(stableCatalogMock("mirrored-operators", "stable", version).body), &stable)
		json.Unmarshal([]byte(stableCatalogMock(rhoai.CatalogName, "stable", "99.0.0").body), &nightly)
		stable["items"] = append(nightly["items"].([]any), stable["items"].([]any)...)
		json.NewEncoder(w).Encode(stable)
	}))
	defer server.Close()
	c := newTestClient(testClientOptions{BaseURL: server.URL, HTTPClient: server.Client(), Ctx: context.Background()})
	for _, currentVersion := range []string{"3.5.0", "3.7.0"} {
		version = currentVersion
		target, err := catalog.ResolveStableTarget(c)
		if err != nil || target.Source != "mirrored-operators" || target.Version != currentVersion {
			t.Fatalf("target=%+v error=%v", target, err)
		}
	}
}

func TestStableDiscoveryFailureDoesNotRemoveOperator(t *testing.T) {
	t.Setenv("STABLE_CHANNEL", "")
	var deletes int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if dashboardOperatorAbsent(w, r) {
			return
		}
		if r.Method == "DELETE" {
			deletes++
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/subscriptions/"+rhoai.SubName):
			io.WriteString(w, `{"spec":{"source":"rhoai-catalog-dev","channel":"beta"}}`)
		case strings.HasSuffix(r.URL.Path, "/packagemanifests"):
			io.WriteString(w, `{"items":[]}`)
		default:
			io.WriteString(w, `{"items":[]}`)
		}
	}))
	defer server.Close()
	c := newTestClient(testClientOptions{BaseURL: server.URL, HTTPClient: server.Client(), Ctx: context.Background()})
	var failedValidation bool
	result, err := ReinstallStream(c, "stable", "", "", func(event UpdateStepEvent) {
		if event.Step == "validate_target" && event.Status == "failed" {
			failedValidation = true
		}
	})
	if err != nil || result.Success || !failedValidation || deletes != 0 {
		t.Fatalf("result=%+v error=%v deletes=%d failedValidation=%v", result, err, deletes, failedValidation)
	}
}

func TestStableDiscoveryRejectsUnavailableOrUnverifiedCatalog(t *testing.T) {
	t.Setenv("STABLE_SOURCE", "redhat-operators")
	t.Setenv("STABLE_CHANNEL", "")
	valid := stableCatalogMock("redhat-operators", "stable", "3.5.0").body
	for _, tt := range []struct {
		name     string
		response mockResponse
	}{
		{"forbidden", mockResponse{statusCode: http.StatusForbidden, body: `{"message":"forbidden","code":403}`}},
		{"invalid JSON", mockResponse{body: `invalid`}},
		{"wrong source", stableCatalogMock(rhoai.CatalogName, "stable", "99.0.0")},
		{"wrong namespace", mockResponse{body: strings.ReplaceAll(valid, rhoai.CatalogNS, "other-marketplace")}},
		{"missing source identity", mockResponse{body: strings.ReplaceAll(valid, `"catalogSource":"redhat-operators",`, "")}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, cleanup := newMockClient(map[string]mockResponse{
				rhoai.NamespacedPath("packages.operators.coreos.com/v1", "packagemanifests", rhoai.CatalogNS, ""): tt.response,
			})
			defer cleanup()
			target, err := catalog.ResolveStableTarget(c)
			if err == nil || target.Channel != "" {
				t.Fatalf("unverified target accepted: %+v error=%v", target, err)
			}
		})
	}
}

func TestStableReinstallMovesOlderChannelToLatestGA(t *testing.T) {
	t.Setenv("STABLE_SOURCE", "redhat-operators")
	t.Setenv("STABLE_CHANNEL", "")
	subPath := rhoai.NamespacedPath("operators.coreos.com/v1alpha1", "subscriptions", rhoai.SubNS, rhoai.SubName)
	var appliedChannel string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if dashboardOperatorAbsent(w, r) {
			return
		}
		switch {
		case r.Method == "PATCH" && r.URL.Path == subPath:
			var body struct {
				Spec struct {
					Channel string `json:"channel"`
				} `json:"spec"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			appliedChannel = body.Spec.Channel
			io.WriteString(w, `{}`)
		case r.URL.Path == subPath:
			io.WriteString(w, `{"spec":{"source":"redhat-operators","channel":"stable-3.4"},"status":{"currentCSV":"rhods-operator.v3.5.0","installedCSV":"rhods-operator.v3.5.0","installPlanRef":{"name":"install-ga"}}}`)
		case strings.Contains(r.URL.Path, "/clusterserviceversions/"):
			io.WriteString(w, succeededCSV)
		case strings.HasSuffix(r.URL.Path, "/packagemanifests"):
			io.WriteString(w, stableCatalogMock("redhat-operators", "stable-3.5", "3.5.0").body)
		default:
			io.WriteString(w, `{"items":[]}`)
		}
	}))
	defer server.Close()
	c := newTestClient(testClientOptions{BaseURL: server.URL, HTTPClient: server.Client(), Ctx: context.Background()})
	result, err := Reinstall(c, "stable", "", "")
	if err != nil || !result.Success || appliedChannel != "stable-3.5" {
		t.Fatalf("result=%+v error=%v channel=%q", result, err, appliedChannel)
	}
	if !strings.Contains(fmt.Sprint(result.Logs), "GA 3.5.0") {
		t.Fatalf("resolved GA version missing from logs: %v", result.Logs)
	}
}
