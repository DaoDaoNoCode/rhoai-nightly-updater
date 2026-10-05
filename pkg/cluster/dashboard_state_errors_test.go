package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// legacyAnnotationServer simulates a cluster without dashboard-operator whose
// rhods-dashboard keeps its annotations across strategic merge patches.
func legacyAnnotationServer(t *testing.T, initial map[string]string) (*Client, func() map[string]string) {
	t.Helper()
	var mu sync.Mutex
	annotations := map[string]string{}
	for k, v := range initial {
		annotations[k] = v
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/deployments/dashboard-operator"):
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"kind":"Status","status":"Failure","reason":"NotFound","code":404}`)
		case strings.HasSuffix(r.URL.Path, "/deployments/rhods-operator"):
			io.WriteString(w, `{"spec":{"template":{"spec":{"containers":[{"env":[{"name":"RELATED_IMAGE_ODH_DASHBOARD_IMAGE","value":"release-dashboard"}]}]}}}}`)
		case strings.HasSuffix(r.URL.Path, "/deployments/rhods-dashboard"):
			if r.Method == http.MethodPatch {
				var patch struct {
					Metadata struct {
						Annotations map[string]*string `json:"annotations"`
					} `json:"metadata"`
				}
				_ = json.NewDecoder(r.Body).Decode(&patch)
				for k, v := range patch.Metadata.Annotations {
					if v == nil {
						delete(annotations, k)
					} else {
						annotations[k] = *v
					}
				}
			}
			json.NewEncoder(w).Encode(map[string]interface{}{
				"metadata": map[string]interface{}{"annotations": annotations},
				"spec":     map[string]interface{}{"template": map[string]interface{}{"spec": map[string]interface{}{"containers": []map[string]string{{"name": "rhods-dashboard", "image": "release-dashboard"}}}}},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"kind":"Status","status":"Failure","reason":"NotFound","code":404}`)
		}
	}))
	t.Cleanup(srv.Close)
	return &Client{baseURL: srv.URL, httpClient: srv.Client(), ctx: context.Background()}, func() map[string]string {
		mu.Lock()
		defer mu.Unlock()
		out := map[string]string{}
		for k, v := range annotations {
			out[k] = v
		}
		return out
	}
}

// A04-8: the legacy revert restores opendatahub.io/managed to its original
// value, or removes it, instead of forcing "true".
func TestLegacyRevertRestoresOriginalManagedAnnotation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		initial map[string]string
		deploys int
		want    string
		wantSet bool
	}{
		{name: "absent", initial: nil, deploys: 1},
		{name: "absent-two-deploys", initial: nil, deploys: 2},
		{name: "explicit-true", initial: map[string]string{"opendatahub.io/managed": "true"}, deploys: 2, want: "true", wantSet: true},
		{name: "left-false-by-old-tool", initial: map[string]string{"opendatahub.io/managed": "false"}, deploys: 1},
		{name: "revert-without-deploy-record", initial: map[string]string{"opendatahub.io/managed": "false"}, deploys: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			allPRImagesPublished(t)
			c, annotations := legacyAnnotationServer(t, tc.initial)
			for i := 0; i < tc.deploys; i++ {
				result, err := DeployPRImage(c, 42)
				if err != nil || !result.Success {
					t.Fatalf("deploy=%+v err=%v", result, err)
				}
				if got := annotations()["opendatahub.io/managed"]; got != "false" {
					t.Fatalf("deploy left managed=%q", got)
				}
			}
			result, err := RevertDashboardImage(c)
			if err != nil || !result.Success {
				t.Fatalf("revert=%+v err=%v", result, err)
			}
			got := annotations()
			value, set := got["opendatahub.io/managed"]
			if set != tc.wantSet || value != tc.want {
				t.Fatalf("managed after revert = %q (set=%v), want %q (set=%v)", value, set, tc.want, tc.wantSet)
			}
			if _, ok := got[legacyOriginalManagedAnnotation]; ok {
				t.Fatal("revert left the saved original")
			}
		})
	}
}

// A04-9: "not deployed" (404 on rhods-dashboard) is distinguished from RBAC,
// throttling, API and network failures.
func TestDashboardStateErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		wantStatus int
		wantCode   string
	}{
		{"not-deployed", http.StatusNotFound, http.StatusNotFound, "dashboard_not_deployed"},
		{"forbidden", http.StatusForbidden, http.StatusForbidden, "forbidden"},
		{"unauthorized", http.StatusUnauthorized, http.StatusBadGateway, "unauthorized"},
		{"throttled", http.StatusTooManyRequests, http.StatusTooManyRequests, "rate_limited"},
		{"server-error", http.StatusInternalServerError, http.StatusBadGateway, "upstream_error"},
		{"unavailable", http.StatusServiceUnavailable, http.StatusBadGateway, "upstream_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprintf(w, `{"kind":"Status","status":"Failure","code":%d}`, tc.status)
			}))
			defer srv.Close()
			c := &Client{baseURL: srv.URL, httpClient: srv.Client(), ctx: context.Background()}
			_, err := GetDashboardState(c)
			if err == nil {
				t.Fatal("expected an error")
			}
			status, code, message := DashboardStateError(err)
			if status != tc.wantStatus || code != tc.wantCode || message == "" {
				t.Fatalf("status=%d code=%q message=%q", status, code, message)
			}
			// The page shows its "not deployed" hint only for this prefix.
			if strings.Contains(message, "failed to get dashboard state") != (tc.wantCode == "dashboard_not_deployed") {
				t.Fatalf("message %q", message)
			}
		})
	}
	t.Run("network", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		url := srv.URL
		srv.Close()
		c := &Client{baseURL: url, httpClient: &http.Client{}, ctx: context.Background()}
		_, err := GetDashboardState(c)
		if status, code, _ := DashboardStateError(err); status != http.StatusServiceUnavailable || code != "network" {
			t.Fatalf("err=%v status=%d code=%q", err, status, code)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		release := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}))
		defer srv.Close()
		defer close(release)
		c := &Client{baseURL: srv.URL, httpClient: &http.Client{Timeout: 50 * time.Millisecond}, ctx: context.Background()}
		_, err := GetDashboardState(c)
		if status, code, _ := DashboardStateError(err); status != http.StatusGatewayTimeout || code != "timeout" {
			t.Fatalf("err=%v status=%d code=%q", err, status, code)
		}
	})
	t.Run("parse-error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `not json`)
		}))
		defer srv.Close()
		c := &Client{baseURL: srv.URL, httpClient: srv.Client(), ctx: context.Background()}
		_, err := GetDashboardState(c)
		if status, code, _ := DashboardStateError(err); status != http.StatusInternalServerError || code != "internal" || errors.Is(err, ErrDashboardNotDeployed) {
			t.Fatalf("err=%v status=%d code=%q", err, status, code)
		}
	})
}
