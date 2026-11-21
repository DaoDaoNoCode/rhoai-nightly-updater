package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Every GET route has a tier, and every tier entry is a GET route: a new
// GET route fails here until someone decides who may read it.
func TestEveryGetRouteHasATier(t *testing.T) {
	gets := map[string]bool{}
	for _, rt := range routes {
		if !strings.HasPrefix(rt.Pattern, http.MethodGet+" ") {
			continue
		}
		gets[rt.Pattern] = true
		if _, ok := getRouteTiers[rt.Pattern]; !ok {
			t.Errorf("%s has no tier in getRouteTiers (route_tiers.go): decide who may read it", rt.Pattern)
		}
	}
	for p := range getRouteTiers {
		if !gets[p] {
			t.Errorf("getRouteTiers lists %s, which is not a registered GET route", p)
		}
	}
	if len(gets) < 30 {
		t.Fatalf("only %d GET routes found", len(gets))
	}
}

// The design's admin-only list (docs/research/per-user-views.md, API) is
// exactly the admin-only tier; the open tiers are exactly the design's.
func TestRouteTiersMatchTheDesign(t *testing.T) {
	want := map[readTier][]string{
		tierAdmin: {
			"GET /api/activity", "GET /api/components", "GET /api/components/dsc", "GET /api/components/dsc/plan",
			"GET /api/dashboard/builds", "GET /api/dashboard/state", "GET /api/debug", "GET /api/diagnostics",
			"GET /api/latest-nightly", "GET /api/nightly-tags", "GET /api/resources/maas", "GET /api/resources/maas/groups",
			"GET /api/resources/models/exposures", "GET /api/resources/prerequisites", "GET /api/resources/s3/admin-password",
			"GET /api/setup/dsc/preview", "GET /api/status", "GET /api/test-pull-secret", "GET /api/verify-nodes",
		},
		tierSummary:       {"GET /api/build-explorer/contains", "GET /api/build-explorer/content", "GET /api/build-explorer/tags", "GET /api/summary"},
		tierUserToken:     {"GET /api/resources/playground", "GET /api/resources/projects", "GET /api/resources/samples/mlflow", "GET /api/user/permissions"},
		tierNoClusterRead: {"GET /api/resources/samples/pipeline/spec", "GET /api/update-check"},
		tierPerRole:       {"GET /api/operation", "GET /api/resources/status"},
	}
	got := map[readTier][]string{}
	for p, tier := range getRouteTiers {
		got[tier] = append(got[tier], p)
	}
	for tier, list := range want {
		sort.Strings(got[tier])
		if strings.Join(got[tier], "\n") != strings.Join(list, "\n") {
			t.Errorf("tier %s:\n got %v\nwant %v", tier, got[tier], list)
		}
	}
	if len(got) != len(want) {
		t.Errorf("tiers in use: %v", got)
	}
}

// stubHandler records whether it ran.
func stubHandler(ran *bool) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		*ran = true
		w.WriteHeader(http.StatusOK)
	}
}

func getAs(t testing.TB, path, token string) *http.Request {
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
	r.Header.Set("X-Forwarded-Access-Token", token)
	return r
}

// setAdmin makes the mutation gate answer admin for "user:admin" and no
// for everyone else.
func setAdmin(t *testing.T) {
	t.Helper()
	mutationPermission = func(_ context.Context, token string) (bool, error) { return token == "user:admin", nil }
}

// Each tier's gate, in front of a stub handler: admin-only routes refuse a
// non-admin with 403 admin_only before the handler runs (so before any
// ServiceAccount read); every other tier lets them through.
func TestTierGateInFrontOfEveryGetRoute(t *testing.T) {
	setupDevMode(t)
	setAdmin(t)
	for pattern, tier := range getRouteTiers {
		_, path, _ := strings.Cut(pattern, " ")
		for _, who := range []string{"user:admin", "user:carol"} {
			ran := false
			w := httptest.NewRecorder()
			handlerFor(ProfileFull, Route{Pattern: pattern, Handler: stubHandler(&ran)})(w, getAs(t, path, who))
			refused := tier == tierAdmin && who != "user:admin"
			switch {
			case refused && (ran || w.Code != http.StatusForbidden || decodeError(t, w)["errorCode"] != adminOnlyCode):
				t.Errorf("%s as %s: ran=%v %d %s", pattern, who, ran, w.Code, w.Body.String())
			case !refused && (!ran || w.Code != http.StatusOK):
				t.Errorf("%s as %s: the handler did not run (%d %s)", pattern, who, w.Code, w.Body.String())
			}
		}
	}
}

// A GET route nobody classified is admin-only (fail closed).
func TestUnclassifiedGetRouteIsAdminOnly(t *testing.T) {
	setupDevMode(t)
	setAdmin(t)
	if tierOf("GET /api/something-new") != tierAdmin {
		t.Fatal("an unclassified route is not admin-only")
	}
	ran := false
	h := handlerFor(ProfileFull, Route{Pattern: "GET /api/something-new", Handler: stubHandler(&ran)})
	w := httptest.NewRecorder()
	h(w, getAs(t, "/api/something-new", "user:carol"))
	if ran || w.Code != http.StatusForbidden || decodeError(t, w)["errorCode"] != adminOnlyCode || decodeError(t, w)["error"] != adminOnlyMessage {
		t.Fatalf("non-admin: ran=%v %d %s", ran, w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	h(w, getAs(t, "/api/something-new", "user:admin"))
	if !ran {
		t.Fatalf("admin: %d %s", w.Code, w.Body.String())
	}
}

// Through the registered mux: every admin-only route refuses a non-admin
// with admin_only (the real handlers never run: they would call a
// cluster that is not there).
func TestRegisteredAdminOnlyRoutesRefuseUsers(t *testing.T) {
	setupDevMode(t)
	setAdmin(t)
	mux := http.NewServeMux()
	Register(mux)
	n := 0
	for pattern, tier := range getRouteTiers {
		if tier != tierAdmin {
			continue
		}
		n++
		_, path, _ := strings.Cut(pattern, " ")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, getAs(t, path, "user:carol"))
		if w.Code != http.StatusForbidden || decodeError(t, w)["errorCode"] != adminOnlyCode {
			t.Errorf("%s: %d %s", pattern, w.Code, w.Body.String())
		}
	}
	if n < 19 {
		t.Fatalf("only %d admin-only routes", n)
	}
}

// The admin-only gate answers a failed check like every gate: 401 for an
// ended sign-in, 503 when the API server cannot be asked; nothing runs.
func TestAdminOnlyGateErrors(t *testing.T) {
	setupDevMode(t)
	for _, tc := range []struct {
		name  string
		token string
		perm  error
		code  int
		ec    string
	}{
		{"expired sign-in", expiredToken, nil, 401, "session_expired"},
		{"review unavailable", "user:carol", errors.New("connection refused"), 503, "authorization_unavailable"},
		{"review says the sign-in ended", "user:carol", cluster.ErrUnauthenticated, 401, "session_expired"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mutationPermission = func(context.Context, string) (bool, error) { return false, tc.perm }
			ran := false
			w := httptest.NewRecorder()
			adminOnly(stubHandler(&ran))(w, getAs(t, "/api/status", tc.token))
			if ran || w.Code != tc.code || decodeError(t, w)["errorCode"] != tc.ec {
				t.Fatalf("ran=%v %d %s", ran, w.Code, w.Body.String())
			}
		})
	}
}

// The gitops profile keeps its own table: every read there needs
// cluster-admin (withGitOpsAdmin), the summary is off.
func TestGitOpsProfileKeepsItsOwnGate(t *testing.T) {
	setupDevMode(t)
	setAdmin(t)
	ran := false
	h := handlerFor(ProfileGitOps, Route{Pattern: "GET /api/summary", Handler: stubHandler(&ran)})
	w := httptest.NewRecorder()
	h(w, getAs(t, "/api/summary", "user:admin"))
	if ran || w.Code != http.StatusForbidden || decodeError(t, w)["errorCode"] != cluster.ErrorCodeProfileGitOps {
		t.Fatalf("summary in the gitops profile: ran=%v %d %s", ran, w.Code, w.Body.String())
	}
}

func TestUserPermissionsReportsTheRole(t *testing.T) {
	setupDevMode(t)
	setAdmin(t)
	for token, want := range map[string]string{"user:admin": roleAdmin, "user:carol": roleUser} {
		w := httptest.NewRecorder()
		HandleUserPermissions(w, getAs(t, "/api/user/permissions", token))
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || w.Code != 200 {
			t.Fatalf("%s: %d %s", token, w.Code, w.Body.String())
		}
		if body["role"] != want || body["canMutate"] != (want == roleAdmin) {
			t.Errorf("%s: %v", token, body)
		}
	}
}

// A user who is not a cluster-admin sees whether an operation runs, its
// label and start, and nothing else: not who runs it, its target, its pod,
// an interrupted one or the last one. An admin sees everything.
func TestOperationForAUserNamesNobody(t *testing.T) {
	setupDevMode(t)
	setAdmin(t)
	started := time.Now().UTC().Add(-2 * time.Minute).Truncate(time.Second)
	readOperationState = func(*cluster.Client) (*cluster.OperationRecord, error) {
		return &cluster.OperationRecord{
			Lease: &types.OperationLease{ID: "op1", BootID: "another-process", Pod: "updater-pod-7", User: "alice",
				Type: "playground", Label: "Gen AI playground", Target: "setup playground in alice-project",
				StartedAt: started.Format(time.RFC3339), HeartbeatAt: clk.Now().UTC().Format(time.RFC3339)},
			Marker: &types.OperationMarker{Type: "update", Label: "Update to nightly", User: "dave", Pod: "old-pod",
				StartedAt: time.Now().UTC().Add(-time.Hour).Format(time.RFC3339), BootID: "crashed"},
			LastCompleted: &types.CompletedOperation{ID: "op0", User: "erin", Target: "erin-project", Label: "x"},
		}, nil
	}
	w := httptest.NewRecorder()
	HandleOperation(w, getAs(t, "/api/operation", "user:carol"))
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var raw map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"running": true, "label": "Gen AI playground", "startedAt": started.Format(time.RFC3339)}
	if len(raw) != len(want) {
		t.Fatalf("a user's operation has other fields: %v", raw)
	}
	for k, v := range want {
		if raw[k] != v {
			t.Errorf("%s = %v, want %v", k, raw[k], v)
		}
	}
	for _, name := range []string{"alice", "dave", "erin", "updater-pod", "old-pod", "project"} {
		if strings.Contains(w.Body.String(), name) {
			t.Errorf("a user's operation names %q: %s", name, w.Body.String())
		}
	}

	w = httptest.NewRecorder()
	HandleOperation(w, getAs(t, "/api/operation", "user:admin"))
	for _, name := range []string{"alice", "dave", "erin", "alice-project"} {
		if !strings.Contains(w.Body.String(), name) {
			t.Errorf("an admin's operation lost %q: %s", name, w.Body.String())
		}
	}

	// Nothing running: running false, no label.
	readOperationState = func(*cluster.Client) (*cluster.OperationRecord, error) { return &cluster.OperationRecord{}, nil }
	w = httptest.NewRecorder()
	HandleOperation(w, getAs(t, "/api/operation", "user:carol"))
	if strings.TrimSpace(w.Body.String()) != `{"running":false,"label":"","startedAt":""}` {
		t.Errorf("idle: %s", w.Body.String())
	}
}

// When the role cannot be checked, a user's answer is the reduced one;
// an ended sign-in is 401.
func TestOperationRoleCheckFailsClosed(t *testing.T) {
	setupDevMode(t)
	readOperationState = func(*cluster.Client) (*cluster.OperationRecord, error) {
		return &cluster.OperationRecord{LastCompleted: &types.CompletedOperation{User: "erin"}}, nil
	}
	mutationPermission = func(context.Context, string) (bool, error) { return true, errors.New("unavailable") }
	w := httptest.NewRecorder()
	HandleOperation(w, getAs(t, "/api/operation", "user:admin"))
	if w.Code != 200 || strings.Contains(w.Body.String(), "erin") {
		t.Fatalf("role unknown: %d %s", w.Code, w.Body.String())
	}
	mutationPermission = func(context.Context, string) (bool, error) { return false, cluster.ErrUnauthenticated }
	w = httptest.NewRecorder()
	HandleOperation(w, getAs(t, "/api/operation", "user:admin"))
	if w.Code != 401 || decodeError(t, w)["errorCode"] != "session_expired" {
		t.Fatalf("ended sign-in: %d %s", w.Code, w.Body.String())
	}
}

// The summary's answer is exactly types.Summary's fields, with the
// updater's version.
func TestSummaryHandler(t *testing.T) {
	setupDevMode(t)
	setAdmin(t)
	orig := getSummary
	t.Cleanup(func() { getSummary = orig })
	getSummary = func(*cluster.Client) (*types.Summary, error) {
		return &types.Summary{RHOAI: types.SummaryRHOAI{Installed: true, Version: "3.6.0", Channel: "beta", Phase: "Succeeded"},
			DSC: types.SummaryDSC{Exists: true, Components: []types.SummaryComponent{{Name: "dashboard", ManagementState: "Managed", Ready: true}}}}, nil
	}
	w := httptest.NewRecorder()
	HandleSummary(w, getAs(t, "/api/summary", "user:carol"))
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var raw map[string]map[string]any
	var top map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &top)
	if top["updaterVersion"] != Version {
		t.Errorf("updaterVersion %v", top["updaterVersion"])
	}
	delete(top, "updaterVersion")
	b, _ := json.Marshal(top)
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	keys := func(m map[string]any) string {
		var k []string
		for key := range m {
			k = append(k, key)
		}
		sort.Strings(k)
		return strings.Join(k, ",")
	}
	for part, want := range map[string]string{
		"rhoai":   "channel,installed,phase,version",
		"nightly": "installedBuild,latestBuild,newerAvailable,problem",
		"dsc":     "components,exists,ready",
	} {
		if got := keys(raw[part]); got != want {
			t.Errorf("%s fields %s, want %s", part, got, want)
		}
	}
	if len(raw) != 3 {
		t.Errorf("summary parts: %v", top)
	}

	getSummary = func(*cluster.Client) (*types.Summary, error) {
		return nil, &cluster.K8sError{Status: 503}
	}
	w = httptest.NewRecorder()
	HandleSummary(w, getAs(t, "/api/summary", "user:carol"))
	if w.Code != http.StatusBadGateway || decodeError(t, w)["errorCode"] != "cluster_unavailable" {
		t.Fatalf("error: %d %s", w.Code, w.Body.String())
	}
}
