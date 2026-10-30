package cluster

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// fixtureDigest is the digest of a published "repo:tag" build.
func fixtureDigest(key string) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(key)))
}

// fixtureCommit is the 40-hex source commit of a published build's labels.
func fixtureCommit(key string) string {
	sum := sha256.Sum256([]byte("commit:" + key))
	return fmt.Sprintf("%x", sum)[:40]
}

// fixtureCommitTag is the immutable tag a main build is also published under:
// OpenShift CI mirrors :main to :main-<7 hex>, Konflux tags its push builds
// with the full 40-hex commit next to :odh-stable.
func fixtureCommitTag(key string) string {
	_, tag, _ := strings.Cut(key, ":")
	switch tag {
	case "main":
		return "main-" + fixtureCommit(key)[:7]
	case "odh-stable":
		return fixtureCommit(key)
	}
	return ""
}

// dashboardTagMock is an in-memory quay.io for the opendatahub repositories:
// anonymous tokens, HEAD of a tag, the manifest and config blob of a digest.
type dashboardTagMock struct {
	mu        sync.Mutex
	digests   map[string]string            // "repo:tag" -> digest
	manifests map[string]bool              // every digest ever published: a manifest outlives its tags
	labels    map[string]map[string]string // digest -> config labels (none: a config without labels)
	heads     map[string]int               // HEAD requests per "repo:tag"
	delays    map[string]time.Duration     // repo -> latency of every request
}

// mockDashboardTags publishes exactly the given "repo:tag" builds (repo
// without the opendatahub/ prefix). Every tag has its own digest, so a switch
// between builds changes the image reference. A main build (tag main or
// odh-stable) also carries commit labels and its commit tag with the same
// digest, as the real pipelines publish them; a test changes that through
// the returned mock before deploying.
func mockDashboardTags(t *testing.T, published ...string) *dashboardTagMock {
	t.Helper()
	m := &dashboardTagMock{digests: map[string]string{}, manifests: map[string]bool{}, labels: map[string]map[string]string{}, heads: map[string]int{}, delays: map[string]time.Duration{}}
	for _, key := range published {
		digest := fixtureDigest(key)
		m.digests[key] = digest
		m.manifests[digest] = true
		if commitTag := fixtureCommitTag(key); commitTag != "" {
			repo, _, _ := strings.Cut(key, ":")
			m.labels[digest] = map[string]string{"vcs-ref": fixtureCommit(key), "git.url": "https://github.com/opendatahub-io/odh-dashboard", "build-date": "2026-10-29T06:00:00Z"}
			m.digests[repo+":"+commitTag] = digest
		}
	}
	// Labels and tag digests are cached per digest across tests.
	resetRegistryCaches()
	original := quayHTTPClient
	quayHTTPClient = &http.Client{Transport: m}
	t.Cleanup(func() {
		quayHTTPClient = original
		resetRegistryCaches()
	})
	return m
}

func (m *dashboardTagMock) RoundTrip(r *http.Request) (*http.Response, error) {
	m.mu.Lock()
	var wait time.Duration
	for repo, d := range m.delays {
		if strings.HasPrefix(r.URL.Path, "/v2/opendatahub/"+repo+"/") {
			wait = d
		}
	}
	m.mu.Unlock()
	if wait > 0 {
		select {
		case <-time.After(wait):
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	answer := func(status int, body string, header http.Header) (*http.Response, error) {
		if header == nil {
			header = http.Header{}
		}
		return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	}
	path := r.URL.Path
	switch {
	case path == "/v2/auth":
		return answer(http.StatusOK, `{"token":"anonymous"}`, nil)
	case strings.Contains(path, "/manifests/"):
		// /v2/opendatahub/<repo>/manifests/<tag or digest>
		repo, ref, _ := strings.Cut(strings.TrimPrefix(path, "/v2/opendatahub/"), "/manifests/")
		if r.Method == http.MethodHead {
			m.heads[repo+":"+ref]++
			digest, ok := m.digests[repo+":"+ref]
			if !ok {
				return answer(http.StatusNotFound, "", nil)
			}
			return answer(http.StatusOK, "", http.Header{"Docker-Content-Digest": []string{digest}})
		}
		if m.manifests[ref] {
			return answer(http.StatusOK, fmt.Sprintf(`{"config":{"digest":"cfg-%s"}}`, ref), http.Header{"Content-Type": []string{"application/vnd.oci.image.manifest.v1+json"}})
		}
		return answer(http.StatusNotFound, "", nil)
	case strings.Contains(path, "/blobs/cfg-"):
		digest := path[strings.LastIndex(path, "/blobs/cfg-")+len("/blobs/cfg-"):]
		labels, ok := m.labels[digest]
		if !ok {
			return answer(http.StatusOK, `{"config":{}}`, nil)
		}
		b, _ := json.Marshal(map[string]interface{}{"created": fakeImageCreated, "config": map[string]interface{}{"Labels": labels}})
		return answer(http.StatusOK, string(b), nil)
	}
	return answer(http.StatusNotFound, "", nil)
}

// headCount reports how many HEAD requests "repo:tag" received.
func (m *dashboardTagMock) headCount(key string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.heads[key]
}

// unpublish removes a tag.
func (m *dashboardTagMock) unpublish(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.digests, key)
}

// republish points a tag at another digest (a newer build under the same tag).
func (m *dashboardTagMock) republish(key, digest string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.digests[key] = digest
	m.manifests[digest] = true
}

// delay makes every request for a repository take d.
func (m *dashboardTagMock) delay(repo string, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.delays[repo] = d
}

// setLabels replaces the labels of a digest (nil: a config without labels).
func (m *dashboardTagMock) setLabels(digest string, labels map[string]string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if labels == nil {
		delete(m.labels, digest)
		return
	}
	m.labels[digest] = labels
}

// The fixture's three dashboard build repos.
var fixtureRepos = []string{"odh-dashboard", "odh-mod-arch-notebooks", "odh-mod-arch-future"}

func allFixtureTags(tag string) []string {
	var out []string
	for _, repo := range fixtureRepos {
		out = append(out, repo+":"+tag)
	}
	return out
}

func fixtureImage(f *dashboardDevFixture, deployment string) string {
	return f.workloads[deployment].Spec.Template.Spec.Containers[0].Image
}

func devImage(t *testing.T, state types.DashboardState, deployment string) types.DashboardDevImage {
	t.Helper()
	for _, image := range state.DevImages {
		if image.Deployment == deployment {
			return image
		}
	}
	t.Fatalf("no dev image for %s in %+v", deployment, state.DevImages)
	return types.DashboardDevImage{}
}

func mustDeploy(t *testing.T, c *Client, mode string, pr int, flavor string) *types.OperationResponse {
	t.Helper()
	result, err := deployDashboardBuild(c, mode, pr, flavor)
	if err != nil || !result.Success {
		t.Fatalf("deploy %s %d: result=%+v err=%v", mode, pr, result, err)
	}
	return result
}

// A04-2: a new build replaces the whole session. Containers without a build
// for the new target go back to their release image instead of keeping the
// previous session's build, and the state says which source each one runs.
func TestDashboardSwitchingBuildsResetsComponentsWithoutNewBuild(t *testing.T) {
	for _, tc := range []struct {
		name      string
		first     func(t *testing.T, c *Client)
		legacy    bool // session saved by a version without baselines
		published []string
	}{
		{name: "pr-to-pr", first: func(t *testing.T, c *Client) {
			mockDashboardTags(t, allFixtureTags("odh-pr-111")...)
			mustDeploy(t, c, "pr", 111, "")
		}},
		{name: "main-to-pr", first: func(t *testing.T, c *Client) {
			mockDashboardTags(t, allFixtureTags("odh-stable")...)
			mustDeploy(t, c, "main", 0, "")
		}},
		{name: "pr-to-pr-session-without-baseline", legacy: true, first: func(t *testing.T, c *Client) {
			mockDashboardTags(t, allFixtureTags("odh-pr-111")...)
			mustDeploy(t, c, "pr", 111, "")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newDashboardDevFixture(t)
			tc.first(t, c)
			if tc.legacy {
				session, _ := dashboardSession(&f.operator)
				session.Baseline = nil
				raw, _ := json.Marshal(session)
				f.operator.Metadata.Annotations[dashboardDevAnnotation] = string(raw)
			}
			// PR 222 only touched the host.
			mockDashboardTags(t, "odh-dashboard:odh-pr-222")
			result := mustDeploy(t, c, "pr", 222, "")
			if !strings.Contains(result.Message, "1 of 3") || !strings.Contains(result.Message, "release image") {
				t.Errorf("message does not say which containers run the PR: %q", result.Message)
			}
			if got := fixtureImage(f, dashboardDeploymentName); !strings.Contains(got, ":odh-pr-222@") {
				t.Fatalf("host = %s", got)
			}
			if got := fixtureImage(f, "notebooks-ui"); got != "release-notebooks" {
				t.Fatalf("notebooks kept the previous build: %s", got)
			}
			if got := fixtureImage(f, "future-workload"); got != "release-future" {
				t.Fatalf("future module kept the previous build: %s", got)
			}
			var state types.DashboardState
			populateDashboardDevState(c, &state)
			if state.PRNumber != 222 || state.DevMode != "pr" || !state.DevImagesMatchTarget || strings.Join(state.PRContainers, ",") != dashboardContainerName {
				t.Fatalf("state=%+v", state)
			}
			host := devImage(t, state, dashboardDeploymentName)
			notebooks := devImage(t, state, "notebooks-ui")
			if host.TargetSource != "pr" || host.Running != "pr" || host.RunningPR != 222 || host.TargetTag != "odh-pr-222" || host.Flavor != DashboardFlavorRHOAI {
				t.Errorf("host=%+v", host)
			}
			if notebooks.TargetSource != "baseline" || notebooks.Running != "release" || notebooks.TargetImage != "release-notebooks" || !notebooks.MatchesTarget {
				t.Errorf("notebooks=%+v", notebooks)
			}
			if *f.operator.Spec.Replicas != 0 {
				t.Fatal("operator resumed during a build switch")
			}
			session, _ := dashboardSession(&f.operator)
			if session.Replicas != 2 {
				t.Fatalf("original replica count lost across sessions: %d", session.Replicas)
			}
		})
	}
}

func TestDashboardPRToMainRetargetsEveryComponent(t *testing.T) {
	f, c := newDashboardDevFixture(t)
	mockDashboardTags(t, "odh-dashboard:odh-pr-111")
	mustDeploy(t, c, "pr", 111, "")
	mockDashboardTags(t, allFixtureTags("odh-stable")...)
	mustDeploy(t, c, "main", 0, "")
	for name, repo := range map[string]string{dashboardDeploymentName: "odh-dashboard", "notebooks-ui": "odh-mod-arch-notebooks", "future-workload": "odh-mod-arch-future"} {
		if got, want := fixtureImage(f, name), "quay.io/opendatahub/"+repo+":"+fixtureCommitTag(repo+":odh-stable")+"@"+fixtureDigest(repo+":odh-stable"); got != want {
			t.Fatalf("%s = %s, want %s", name, got, want)
		}
	}
	var state types.DashboardState
	populateDashboardDevState(c, &state)
	if state.DevMode != "main" || state.PRNumber != 0 || state.IsCustomPR || !state.DevImagesMatchTarget || state.Flavor != DashboardFlavorRHOAI {
		t.Fatalf("state=%+v", state)
	}
}

// A04-3: RHOAI (Konflux, BUILD_MODE=RHOAI) builds by default, OpenShift CI
// builds on request, and a per-component fallback that the state reports.
func TestDashboardDeployFlavorSelection(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mode       string
		flavor     string
		published  []string
		wantHost   string
		hostFlavor string
		wantNB     string
	}{
		{"rhoai-pr", "pr", "", append(allFixtureTags("odh-pr-5"), allFixtureTags("pr-5")...), ":odh-pr-5@", "rhoai", ":odh-pr-5@"},
		{"rhoai-pr-falls-back-per-component", "pr", "rhoai", []string{"odh-dashboard:pr-5", "odh-mod-arch-notebooks:odh-pr-5"}, ":pr-5@", "odh", ":odh-pr-5@"},
		{"odh-pr-ignores-konflux", "pr", "odh", []string{"odh-dashboard:pr-5", "odh-mod-arch-notebooks:odh-pr-5"}, ":pr-5@", "odh", "release-notebooks"},
		// Main builds are pinned under their commit tag (the full SHA for
		// Konflux, main-<7 hex> for OpenShift CI); the floating tag stays in the session.
		{"rhoai-main", "main", "", append(allFixtureTags("odh-stable"), allFixtureTags("main")...), ":" + fixtureCommitTag("odh-dashboard:odh-stable") + "@", "rhoai", ":" + fixtureCommitTag("odh-mod-arch-notebooks:odh-stable") + "@"},
		{"odh-main", "main", "odh", append(allFixtureTags("odh-stable"), allFixtureTags("main")...), ":" + fixtureCommitTag("odh-dashboard:main") + "@", "odh", ":" + fixtureCommitTag("odh-mod-arch-notebooks:main") + "@"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newDashboardDevFixture(t)
			mockDashboardTags(t, tc.published...)
			mustDeploy(t, c, tc.mode, 5, tc.flavor)
			if got := fixtureImage(f, dashboardDeploymentName); !strings.Contains(got, tc.wantHost) {
				t.Fatalf("host = %s, want %s", got, tc.wantHost)
			}
			if got := fixtureImage(f, "notebooks-ui"); !strings.Contains(got, tc.wantNB) {
				t.Fatalf("notebooks = %s, want %s", got, tc.wantNB)
			}
			var state types.DashboardState
			populateDashboardDevState(c, &state)
			wantFlavor := tc.flavor
			if wantFlavor == "" {
				wantFlavor = DashboardFlavorRHOAI
			}
			if state.Flavor != wantFlavor || state.DefaultFlavor != DashboardFlavorRHOAI || len(state.AvailableFlavors) != 2 {
				t.Errorf("flavor state=%q default=%q available=%v", state.Flavor, state.DefaultFlavor, state.AvailableFlavors)
			}
			host := devImage(t, state, dashboardDeploymentName)
			if host.Flavor != tc.hostFlavor || host.Running != tc.mode {
				t.Errorf("host flavor = %q running = %q, want %q %q", host.Flavor, host.Running, tc.hostFlavor, tc.mode)
			}
			if tc.mode == "main" {
				// The UI labels the row "Latest main" by the session's floating tag.
				if want := dashboardBuildTags("main", 0, tc.hostFlavor)[0]; host.TargetTag != want || !host.MatchesTarget {
					t.Errorf("host target tag = %q matches=%v, want %q", host.TargetTag, host.MatchesTarget, want)
				}
			}
		})
	}
	t.Run("invalid-flavor", func(t *testing.T) {
		f, c := newDashboardDevFixture(t)
		mockDashboardTags(t, allFixtureTags("odh-pr-5")...)
		result, err := deployDashboardBuild(c, "pr", 5, "upstream")
		if err != nil || result.Success || result.ErrorCode != "validation" || len(f.writes) > 0 {
			t.Fatalf("result=%+v err=%v writes=%v", result, err, f.writes)
		}
	})
}

// A04-1 and A04-4: who/when is recorded at deploy time, a later deploy keeps
// the start, and an RHOAI update while paused is reported as stale.
func TestDashboardOverrideTracksUsersAndRHOAIUpdates(t *testing.T) {
	f, c := newDashboardDevFixture(t)
	f.operator.Metadata.Annotations[platformVersionAnnotation] = "3.6.0"
	f.operator.Spec.Template.Spec.Containers[0].Image = "operator@sha256:v1"
	c.SetUsername("alice")
	mockDashboardTags(t, allFixtureTags("odh-pr-111")...)
	mustDeploy(t, c, "pr", 111, "")

	override, err := DashboardOverrideSummary(c)
	if err != nil || override == nil {
		t.Fatalf("override=%+v err=%v", override, err)
	}
	if !override.Active || !override.OperatorPaused || !override.SessionRecorded || override.Stale || override.StartedBy != "alice" || override.UpdatedBy != "alice" || override.StartedAt == "" || override.PRNumber != 111 || override.Mode != "pr" || override.Flavor != "rhoai" || len(override.Components) != 3 || override.ReleaseVersionAtStart != "3.6.0" {
		t.Fatalf("override=%+v", override)
	}
	if override.LastAction == nil || override.LastAction.Action != "deploy-pr" || override.LastAction.By != "alice" {
		t.Fatalf("last action=%+v", override.LastAction)
	}
	for _, component := range override.Components {
		if component.Source != "pr" || component.Tag != "odh-pr-111" || !strings.Contains(component.Image, "@sha256:") {
			t.Errorf("component=%+v", component)
		}
	}

	// RHOAI update while paused: the platform re-applies the Deployment with
	// new images and version but keeps replicas at 0.
	f.operator.Metadata.Annotations[platformVersionAnnotation] = "3.6.1"
	f.operator.Spec.Template.Spec.Containers[0].Image = "operator@sha256:v2"
	for i, env := range f.operator.Spec.Template.Spec.Containers[0].Env {
		f.operator.Spec.Template.Spec.Containers[0].Env[i].Value = env.Value + "-v2"
	}
	override, _ = DashboardOverrideSummary(c)
	if !override.Stale || len(override.StaleReasons) != 3 || override.ReleaseVersion != "3.6.1" || !strings.Contains(strings.Join(override.StaleReasons, ";"), "3.6.0 to 3.6.1") {
		t.Fatalf("stale override=%+v", override)
	}
	var state types.DashboardState
	populateDashboardDevState(c, &state)
	if state.Override == nil || !state.Override.Stale {
		t.Fatalf("state does not carry the stale override: %+v", state.Override)
	}

	// A new deploy on the updated release resets unbuilt components to the
	// new release image and refreshes the session, keeping who started it.
	c.SetUsername("bob")
	mockDashboardTags(t, "odh-dashboard:odh-pr-222")
	mustDeploy(t, c, "pr", 222, "")
	if got := fixtureImage(f, "notebooks-ui"); got != "release-notebooks-v2" {
		t.Fatalf("notebooks reset to %s, want the updated release image", got)
	}
	override, _ = DashboardOverrideSummary(c)
	if override.Stale || override.StartedBy != "alice" || override.UpdatedBy != "bob" || override.PRNumber != 222 {
		t.Fatalf("override after redeploy=%+v", override)
	}

	c.SetUsername("carol")
	result, err := RevertDashboardImage(c)
	if err != nil || !result.Success || *f.operator.Spec.Replicas != 2 {
		t.Fatalf("revert=%+v err=%v", result, err)
	}
	override, _ = DashboardOverrideSummary(c)
	if override.Active || override.SessionRecorded || override.LastAction == nil || override.LastAction.Action != "revert" || override.LastAction.By != "carol" || override.LastAction.At == "" {
		t.Fatalf("override after revert=%+v last=%+v", override, override.LastAction)
	}
}

func TestDashboardOverrideSummaryStates(t *testing.T) {
	for _, tc := range []struct {
		name        string
		replicas    int
		annotation  string
		active      bool
		recorded    bool
		sessionErr  bool
		operatorErr int // HTTP status for the operator GET
		wantNil     bool
		wantErr     bool
	}{
		{name: "running-no-session", replicas: 1},
		{name: "paused-outside-tool", replicas: 0, active: true},
		{name: "corrupt-annotation", replicas: 0, annotation: "{not json", active: true, sessionErr: true},
		{name: "other-operator-annotation", replicas: 0, annotation: `{"operatorUID":"old","replicas":1}`, active: true, sessionErr: true},
		{name: "not-installed", operatorErr: http.StatusNotFound, wantNil: true},
		{name: "api-error", operatorErr: http.StatusInternalServerError, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newDashboardDevFixture(t)
			f.operator.Spec.Replicas = &tc.replicas
			if tc.annotation != "" {
				f.operator.Metadata.Annotations[dashboardDevAnnotation] = tc.annotation
			}
			if tc.operatorErr != 0 {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(tc.operatorErr)
					io.WriteString(w, `{"kind":"Status","status":"Failure"}`)
				}))
				t.Cleanup(srv.Close)
				c = &Client{baseURL: srv.URL, httpClient: srv.Client(), ctx: context.Background()}
			}
			override, err := DashboardOverrideSummary(c)
			if (err != nil) != tc.wantErr || (override == nil) != (tc.wantNil || tc.wantErr) {
				t.Fatalf("override=%+v err=%v", override, err)
			}
			if override == nil {
				return
			}
			if override.Active != tc.active || override.SessionRecorded != tc.recorded || (override.SessionError != "") != tc.sessionErr {
				t.Fatalf("override=%+v", override)
			}
		})
	}
}

// Revert must return to operator management from any state and be safe to repeat.
func TestDashboardRevertRecoversFromAnyState(t *testing.T) {
	for _, tc := range []struct {
		name         string
		setup        func(t *testing.T, f *dashboardDevFixture, c *Client)
		wantReplicas int
	}{
		{"after-successful-revert", func(t *testing.T, f *dashboardDevFixture, c *Client) {
			mockDashboardTags(t, allFixtureTags("odh-pr-1")...)
			mustDeploy(t, c, "pr", 1, "")
			if r, err := RevertDashboardImage(c); err != nil || !r.Success {
				t.Fatalf("first revert=%+v err=%v", r, err)
			}
		}, 2},
		{"never-deployed", func(t *testing.T, f *dashboardDevFixture, c *Client) {}, 2},
		{"corrupt-annotation-while-paused", func(t *testing.T, f *dashboardDevFixture, c *Client) {
			zero := 0
			f.operator.Spec.Replicas = &zero
			f.operator.Metadata.Annotations[dashboardDevAnnotation] = "{not json"
		}, dashboardOperatorDefaultReplicas},
		{"annotation-for-another-operator", func(t *testing.T, f *dashboardDevFixture, c *Client) {
			zero := 0
			f.operator.Spec.Replicas = &zero
			f.operator.Metadata.Annotations[dashboardDevAnnotation] = `{"operatorUID":"old","replicas":3}`
		}, dashboardOperatorDefaultReplicas},
		{"annotation-lost-while-paused", func(t *testing.T, f *dashboardDevFixture, c *Client) {
			mockDashboardTags(t, allFixtureTags("odh-pr-1")...)
			mustDeploy(t, c, "pr", 1, "")
			delete(f.operator.Metadata.Annotations, dashboardDevAnnotation)
		}, dashboardOperatorDefaultReplicas},
		{"dashboard-removed-while-paused", func(t *testing.T, f *dashboardDevFixture, c *Client) {
			mockDashboardTags(t, allFixtureTags("odh-pr-1")...)
			mustDeploy(t, c, "pr", 1, "")
			// Deleting the Dashboard CR (DSC disable or uninstall) removes its
			// workloads; the CR itself waits for the paused operator's finalizer.
			for name := range f.workloads {
				delete(f.workloads, name)
			}
		}, 2},
		{"operator-patch-conflicts", func(t *testing.T, f *dashboardDevFixture, c *Client) {
			mockDashboardTags(t, allFixtureTags("odh-pr-1")...)
			mustDeploy(t, c, "pr", 1, "")
			f.operatorConflicts = 2
		}, 2},
		{"old-pr-flow-unmanaged-workload", func(t *testing.T, f *dashboardDevFixture, c *Client) {
			d := f.workloads["notebooks-ui"]
			d.Metadata.Annotations = map[string]string{"opendatahub.io/managed": "false"}
			f.workloads["notebooks-ui"] = d
			u := f.workloads["unrelated"]
			u.Metadata.Annotations = map[string]string{"opendatahub.io/managed": "false"}
			u.Metadata.OwnerReferences = nil
			f.workloads["unrelated"] = u
		}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newDashboardDevFixture(t)
			tc.setup(t, f, c)
			for attempt := 1; attempt <= 2; attempt++ {
				result, err := RevertDashboardImage(c)
				if err != nil || !result.Success {
					t.Fatalf("revert %d=%+v err=%v", attempt, result, err)
				}
				if *f.operator.Spec.Replicas != tc.wantReplicas {
					t.Fatalf("revert %d: replicas=%d want %d", attempt, *f.operator.Spec.Replicas, tc.wantReplicas)
				}
				if _, ok := f.operator.Metadata.Annotations[dashboardDevAnnotation]; ok {
					t.Fatalf("revert %d left the session annotation", attempt)
				}
			}
			if _, ok := f.workloads["notebooks-ui"].Metadata.Annotations["opendatahub.io/managed"]; ok {
				t.Fatal("managed annotation left on a Dashboard workload")
			}
			if tc.name == "old-pr-flow-unmanaged-workload" && f.workloads["unrelated"].Metadata.Annotations["opendatahub.io/managed"] != "false" {
				t.Fatal("revert changed a workload the Dashboard does not own")
			}
			var state types.DashboardState
			populateDashboardDevState(c, &state)
			if state.OperatorPaused || (state.Override != nil && state.Override.Active) {
				t.Fatalf("still overridden after revert: %+v", state.Override)
			}
		})
	}
}

func TestDashboardRevertReportsFailures(t *testing.T) {
	t.Run("operator-read-error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/deployments") {
				io.WriteString(w, `{"items":[]}`)
				return
			}
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `{"kind":"Status","status":"Failure","message":"forbidden"}`)
		}))
		defer srv.Close()
		c := &Client{baseURL: srv.URL, httpClient: srv.Client(), ctx: context.Background()}
		result, err := revertDashboardOperator(c, nil)
		if err != nil || result.Success || result.ErrorCode != "forbidden" {
			t.Fatalf("result=%+v err=%v", result, err)
		}
	})
	t.Run("resume-rejected", func(t *testing.T) {
		f, c := newDashboardDevFixture(t)
		mockDashboardTags(t, allFixtureTags("odh-pr-1")...)
		mustDeploy(t, c, "pr", 1, "")
		f.failOperatorPatch = http.StatusInternalServerError
		result, err := RevertDashboardImage(c)
		if err != nil || result.Success || *f.operator.Spec.Replicas != 0 || f.operator.Metadata.Annotations[dashboardDevAnnotation] == "" {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		f.failOperatorPatch = 0
		if result, err := RevertDashboardImage(c); err != nil || !result.Success || *f.operator.Spec.Replicas != 2 {
			t.Fatalf("retry=%+v err=%v", result, err)
		}
	})
	t.Run("workload-annotation-cleanup-fails", func(t *testing.T) {
		f, c := newDashboardDevFixture(t)
		mockDashboardTags(t, allFixtureTags("odh-pr-1")...)
		mustDeploy(t, c, "pr", 1, "")
		d := f.workloads["notebooks-ui"]
		d.Metadata.Annotations = map[string]string{"opendatahub.io/managed": "false"}
		f.workloads["notebooks-ui"] = d
		f.failWorkload = "notebooks-ui"
		result, err := RevertDashboardImage(c)
		// The operator resumes anyway; the session stays so Revert is offered again.
		if err != nil || result.Success || result.ErrorCode != "partial_failure" || *f.operator.Spec.Replicas != 2 || f.operator.Metadata.Annotations[dashboardDevAnnotation] == "" {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		f.failWorkload = ""
		if result, err := RevertDashboardImage(c); err != nil || !result.Success || f.operator.Metadata.Annotations[dashboardDevAnnotation] != "" {
			t.Fatalf("retry=%+v err=%v", result, err)
		}
	})
}

// An interrupted deploy (failed patch, updater restart) is finished by
// deploying again and keeps the original replica count and session start.
func TestDashboardInterruptedDeployCanBeRetried(t *testing.T) {
	f, c := newDashboardDevFixture(t)
	c.SetUsername("alice")
	mockDashboardTags(t, allFixtureTags("odh-pr-7")...)
	f.failWorkload = "notebooks-ui"
	result, err := deployDashboardBuild(c, "pr", 7, "")
	if err != nil || result.Success || result.ErrorCode != "partial_failure" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	started, _ := dashboardSession(&f.operator)
	var state types.DashboardState
	populateDashboardDevState(c, &state)
	if state.DevImagesMatchTarget || devImage(t, state, "notebooks-ui").MatchesTarget {
		t.Fatalf("partial deploy reported as applied: %+v", state.DevImages)
	}
	// A restarted updater has a new client and no in-memory state.
	f.failWorkload = ""
	restarted := &Client{baseURL: c.baseURL, httpClient: c.httpClient, ctx: context.Background()}
	restarted.SetUsername("bob")
	mustDeploy(t, restarted, "pr", 7, "")
	session, _ := dashboardSession(&f.operator)
	if session.Replicas != 2 || session.StartedAt != started.StartedAt || session.StartedBy != "alice" || session.UpdatedBy != "bob" {
		t.Fatalf("session=%+v", session)
	}
	state = types.DashboardState{}
	populateDashboardDevState(restarted, &state)
	if !state.DevImagesMatchTarget {
		t.Fatalf("retry did not finish the deploy: %+v", state.DevImages)
	}
	if result, err := RevertDashboardImage(restarted); err != nil || !result.Success || *f.operator.Spec.Replicas != 2 {
		t.Fatalf("revert=%+v err=%v", result, err)
	}
}

func dashboardTestPod(name, container, image string, ready bool, status map[string]interface{}) map[string]interface{} {
	cs := map[string]interface{}{"name": container, "ready": ready, "restartCount": 0, "state": map[string]interface{}{"running": map[string]interface{}{}}}
	for k, v := range status {
		cs[k] = v
	}
	return map[string]interface{}{
		"metadata": map[string]interface{}{"name": name, "labels": map[string]string{"app": "notebooks"}, "creationTimestamp": "2026-10-05T10:00:00Z"},
		"spec":     map[string]interface{}{"containers": []map[string]string{{"name": container, "image": image}}},
		"status":   map[string]interface{}{"containerStatuses": []interface{}{cs}},
	}
}

// A04-7: a stuck rollout reports the Deployment's ProgressDeadlineExceeded
// condition and the waiting reason of the newest pod.
func TestDashboardStuckRolloutReportsReasons(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      map[string]interface{}
		conditions  []interface{}
		stuck       bool
		wantReason  string
		wantMessage string
		restarts    int
	}{
		{name: "image-pull", status: map[string]interface{}{"state": map[string]interface{}{"waiting": map[string]string{"reason": "ImagePullBackOff", "message": "Back-off pulling image"}}}, stuck: true, wantReason: "ImagePullBackOff", wantMessage: "Back-off pulling image"},
		{name: "crash-loop", status: map[string]interface{}{"restartCount": 4, "state": map[string]interface{}{"waiting": map[string]string{"reason": "CrashLoopBackOff"}}, "lastState": map[string]interface{}{"terminated": map[string]string{"reason": "Error"}}}, wantReason: "CrashLoopBackOff", wantMessage: "last exit: Error", restarts: 4},
		{name: "unschedulable", conditions: []interface{}{map[string]string{"type": "PodScheduled", "status": "False", "reason": "Unschedulable", "message": "0/3 nodes are available"}}, wantReason: "Unschedulable", wantMessage: "0/3 nodes are available"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newDashboardDevFixture(t)
			mockDashboardTags(t, allFixtureTags("odh-pr-9")...)
			mustDeploy(t, c, "pr", 9, "")
			d := f.workloads["notebooks-ui"]
			d.Spec.Selector.MatchLabels = map[string]string{"app": "notebooks"}
			d.Status.ReadyReplicas = 0
			if tc.stuck {
				d.Status.Conditions = append(d.Status.Conditions, struct {
					Type    string `json:"type"`
					Status  string `json:"status"`
					Reason  string `json:"reason"`
					Message string `json:"message"`
				}{"Progressing", "False", "ProgressDeadlineExceeded", `ReplicaSet "notebooks-ui-2" has timed out progressing.`})
			}
			f.workloads["notebooks-ui"] = d
			newImage := d.Spec.Template.Spec.Containers[0].Image
			oldPod := dashboardTestPod("notebooks-old", "notebooks-ui", "release-notebooks", true, nil)
			newPod := dashboardTestPod("notebooks-new", "notebooks-ui", newImage, false, tc.status)
			if tc.conditions != nil {
				newPod["status"] = map[string]interface{}{"conditions": tc.conditions}
			}
			other := dashboardTestPod("other", "notebooks-ui", newImage, false, nil)
			other["metadata"].(map[string]interface{})["labels"] = map[string]string{"app": "other"}
			f.pods = []map[string]interface{}{oldPod, newPod, other}

			var state types.DashboardState
			populateDashboardDevState(c, &state)
			image := devImage(t, state, "notebooks-ui")
			if image.PodName != "notebooks-new" || image.WaitingReason != tc.wantReason || image.WaitingMessage != tc.wantMessage || image.Restarts != tc.restarts || image.RolloutStuck != tc.stuck {
				t.Fatalf("image=%+v", image)
			}
			if state.RolloutStuck != tc.stuck || (tc.stuck && !strings.Contains(state.StuckReason, "notebooks-ui") && !strings.Contains(state.StuckReason, tc.wantReason)) {
				t.Fatalf("stuck=%v reason=%q", state.RolloutStuck, state.StuckReason)
			}
		})
	}
	t.Run("ready-workloads-skip-pod-list", func(t *testing.T) {
		f, c := newDashboardDevFixture(t)
		var state types.DashboardState
		populateDashboardDevState(c, &state)
		if f.podListCalls != 0 || state.RolloutStuck {
			t.Fatalf("pod lists=%d stuck=%v", f.podListCalls, state.RolloutStuck)
		}
	})
}

func TestParseDashboardBuild(t *testing.T) {
	for _, tc := range []struct {
		image, kind, flavor string
		pr                  int
	}{
		{"quay.io/opendatahub/odh-dashboard:odh-pr-12@sha256:abc", "pr", "rhoai", 12},
		{"quay.io/opendatahub/odh-dashboard:pr-12", "pr", "odh", 12},
		{"quay.io/opendatahub/odh-dashboard:odh-stable@sha256:abc", "main", "rhoai", 0},
		{"quay.io/opendatahub/odh-dashboard:main", "main", "odh", 0},
		// The commit tags Deploy pins main builds under.
		{"quay.io/opendatahub/odh-dashboard:21c06a3be1de89a8e18cf95cc7eef5a78add9481@sha256:abc", "main", "rhoai", 0},
		{"quay.io/opendatahub/odh-dashboard:main-21c06a3@sha256:abc", "main", "odh", 0},
		{"quay.io/opendatahub/odh-dashboard:main-21c06a3be1de89a8e18cf95cc7eef5a78add9481", "main", "odh", 0},
		{"quay.io/opendatahub/odh-dashboard:main-21c06a", "", "", 0},
		{"quay.io/opendatahub/odh-dashboard:21c06a3be1de89a8e18cf95cc7eef5a78add948", "", "", 0},
		{"quay.io/opendatahub/odh-dashboard:21C06A3BE1DE89A8E18CF95CC7EEF5A78ADD9481", "", "", 0},
		{"quay.io/opendatahub/odh-dashboard:odh-pr-21c06a3be1de89a8e18cf95cc7eef5a78add9481", "", "", 0},
		{"quay.io/opendatahub/odh-dashboard:pr-0", "", "", 0},
		{"quay.io/opendatahub/other:pr-12", "", "", 0},
		{"registry.redhat.io/rhoai/odh-dashboard-rhel9@sha256:abc", "", "", 0},
	} {
		kind, pr, flavor, _ := parseDashboardBuild("opendatahub/odh-dashboard", tc.image)
		if kind != tc.kind || pr != tc.pr || flavor != tc.flavor {
			t.Errorf("%s: kind=%q pr=%d flavor=%q", tc.image, kind, pr, flavor)
		}
	}
}

// Deploy latest main pins the build under its commit tag when the registry
// publishes one for the same digest, so oc get deploy names the commit;
// otherwise it keeps the floating tag, as before. PR deploys are unchanged.
func TestDashboardMainDeploysUnderTheCommitTag(t *testing.T) {
	host := "odh-dashboard:odh-stable"
	for _, tc := range []struct {
		name    string
		flavor  string
		prepare func(m *dashboardTagMock)
		want    string // the host's tag
	}{
		{"rhoai", "", nil, fixtureCommitTag(host)},
		{"odh", "odh", nil, fixtureCommitTag("odh-dashboard:main")},
		{"commit-tag-missing", "", func(m *dashboardTagMock) { m.unpublish("odh-dashboard:" + fixtureCommitTag(host)) }, "odh-stable"},
		{"commit-tag-has-another-digest", "", func(m *dashboardTagMock) { m.republish("odh-dashboard:"+fixtureCommitTag(host), digestOf(7)) }, "odh-stable"},
		{"no-labels", "", func(m *dashboardTagMock) { m.setLabels(fixtureDigest(host), nil) }, "odh-stable"},
		{"commit-label-not-a-sha", "", func(m *dashboardTagMock) { m.setLabels(fixtureDigest(host), map[string]string{"vcs-ref": "v1.2.3"}) }, "odh-stable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newDashboardDevFixture(t)
			m := mockDashboardTags(t, append(allFixtureTags("odh-stable"), allFixtureTags("main")...)...)
			if tc.prepare != nil {
				tc.prepare(m)
			}
			mustDeploy(t, c, "main", 0, tc.flavor)
			floating := "odh-stable"
			if tc.flavor == "odh" {
				floating = "main"
			}
			if got, want := fixtureImage(f, dashboardDeploymentName), "quay.io/opendatahub/odh-dashboard:"+tc.want+"@"+fixtureDigest("odh-dashboard:"+floating); got != want {
				t.Fatalf("host = %s, want %s", got, want)
			}
			session, _ := dashboardSession(&f.operator)
			for _, b := range session.Bindings {
				if b.Tag != floating || b.Source != "main" || !strings.Contains(b.TargetImage, "@sha256:") {
					t.Fatalf("binding %+v: the session keeps the floating tag", b)
				}
			}
			var state types.DashboardState
			populateDashboardDevState(c, &state)
			if !state.DevImagesMatchTarget || state.DevMode != "main" {
				t.Fatalf("state=%+v", state)
			}
			if h := devImage(t, state, dashboardDeploymentName); h.Running != "main" || h.TargetTag != floating {
				t.Fatalf("host=%+v", h)
			}
		})
	}
	t.Run("pr-deploys-are-unchanged", func(t *testing.T) {
		f, c := newDashboardDevFixture(t)
		m := mockDashboardTags(t, allFixtureTags("odh-pr-5")...)
		mustDeploy(t, c, "pr", 5, "")
		if got, want := fixtureImage(f, dashboardDeploymentName), "quay.io/opendatahub/odh-dashboard:odh-pr-5@"+fixtureDigest("odh-dashboard:odh-pr-5"); got != want {
			t.Fatalf("host = %s, want %s", got, want)
		}
		if n := m.headCount("odh-dashboard:odh-pr-5"); n != 1 {
			t.Fatalf("PR deploy made %d HEAD requests for the PR tag, want 1 (no commit tag lookup)", n)
		}
	})
}

// Audit repro A04-2, through the public entry points: the second deployment's
// unbuilt components must not keep the first deployment's build.
func TestDashboardSecondDeployDoesNotKeepPreviousBuild(t *testing.T) {
	for _, first := range []string{"pr", "main"} {
		t.Run(first+"-then-pr", func(t *testing.T) {
			mockDashboardRegistry(t, nil)
			f, c := newDashboardDevFixture(t)
			var result *types.OperationResponse
			var err error
			if first == "pr" {
				result, err = DeployPRImage(c, 111)
			} else {
				result, err = DeployDashboardMain(c)
			}
			if err != nil || !result.Success {
				t.Fatalf("first deploy: %+v %v", result, err)
			}
			mockDashboardRegistry(t, map[string]int{"odh-mod-arch-notebooks": 404, "odh-mod-arch-future": 404})
			if result, err = DeployPRImage(c, 222); err != nil || !result.Success {
				t.Fatalf("deploy 222: %+v %v", result, err)
			}
			if got := f.workloads["notebooks-ui"].Spec.Template.Spec.Containers[0].Image; got != "release-notebooks" {
				t.Fatalf("notebooks runs %s after deploying PR #222, which has no notebooks build", got)
			}
			var state types.DashboardState
			populateDashboardDevState(c, &state)
			if state.PRNumber != 222 || !state.DevImagesMatchTarget {
				t.Fatalf("state=%+v", state)
			}
		})
	}
}
