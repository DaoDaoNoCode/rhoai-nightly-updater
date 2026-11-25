package cluster

import (
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/features"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/serving"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// kuadrantListV1beta1 is the list path Set up polls when waiting on Kuadrant's API
// (KuadrantServedVersion), using the version served by the fake.
const kuadrantListV1beta1 = "/apis/kuadrant.io/v1beta1/kuadrants"

// runKuadrantAPIWait drives Set up against a cluster that lacks the Kuadrant CR.
// After Set up begins waiting on Kuadrant's API, answer handles every list read
// in that wait (poll counts the reads of the v1beta1 list from 1, retries
// included); return false so the fake still responds. Every read must ask
// for limit=1, which the API server serves from etcd while the watch cache
// initializes.
func runKuadrantAPIWait(t *testing.T, answer func(poll int, w http.ResponseWriter, r *http.Request) bool) (*types.OperationResponse, []UpdateStepEvent) {
	t.Helper()
	mc := maasClusterWithoutKuadrant(t, time.Now().Add(-24*time.Hour), "True")
	var waiting atomic.Bool
	waited := false
	polls := 0
	mc.f.intercept = func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		if !waiting.Load() || r.Method != http.MethodGet || !strings.HasPrefix(r.URL.Path, "/apis/kuadrant.io/") || !strings.HasSuffix(r.URL.Path, "/kuadrants") {
			return false
		}
		if r.URL.Query().Get("limit") != "1" {
			t.Errorf("%s without limit=1", r.URL)
		}
		if r.URL.Path == kuadrantListV1beta1 {
			polls++
		}
		return answer(polls, w, r)
	}
	var events []UpdateStepEvent
	res, err := RunMaaS(mc.c, types.MaaSRequest{Action: "setup", Database: tCNPG}, func(e UpdateStepEvent) {
		events = append(events, e)
		if e.Step == "kuadrant" {
			// The window is the wait: its reads, until the step's next message.
			waiting.Store(e.Message == "Waiting for Connectivity Link's Kuadrant API...")
			waited = waited || waiting.Load()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if !waited {
		t.Fatalf("Set up did not wait for Kuadrant's API; kuadrant step: %q", kuadrantMessages(events))
	}
	return res, events
}

// notServedYet sends 404 on every version because the CRD has not appeared yet.
func notServedYet(w http.ResponseWriter) bool {
	w.WriteHeader(http.StatusNotFound)
	_, _ = io.WriteString(w, k8sNotFound)
	return true
}

// When a read fails, the wait aborts early: user-facing text records the instant
// and cause of the stop, rather than claiming the API stayed unserved through timeout.
func TestMaaSSetup_KuadrantAPIWaitStoppedByAnErrorSaysWhen(t *testing.T) {
	res, events := runKuadrantAPIWait(t, func(poll int, w http.ResponseWriter, r *http.Request) bool {
		if poll < 3 {
			return notServedYet(w)
		}
		// Missing Retry-After on 503 means no retry per client-go's checkWait.
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"the server is currently unable to handle the request","reason":"ServiceUnavailable","code":503}`)
		return true
	})
	want := "Stopped waiting for Connectivity Link's Kuadrant API after 0:10: k8s API error 503 (ServiceUnavailable): the server is currently unable to handle the request. Set up again to continue; the MaaS tab shows Connectivity Link's state."
	if res.Success || res.Message != want || res.ErrorCode != "not_ready" {
		t.Fatalf("setup: %+v\nwant message %q", res, want)
	}
	if !strings.HasSuffix(finalSteps(events), "kuadrant=failed") {
		t.Fatalf("steps = %s", finalSteps(events))
	}
}

// F9: immediately after Connectivity Link install, listing the fresh CRD can return
// 429 with "storage is (re)initializing" plus Retry-After while the watch cache warms.
// Honoring that header lets the wait continue until Set up creates the Kuadrant CR.
func TestMaaSSetup_KuadrantAPIWaitRidesOutTheInitializingCache(t *testing.T) {
	res, events := runKuadrantAPIWait(t, func(poll int, w http.ResponseWriter, r *http.Request) bool {
		if poll > 3 || r.URL.Path != kuadrantListV1beta1 {
			return false
		}
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"kind":"Status","apiVersion":"v1","metadata":{},"status":"Failure","message":"storage is (re)initializing","reason":"TooManyRequests","details":{"retryAfterSeconds":1},"code":429}`)
		return true
	})
	if !res.Success {
		t.Fatalf("setup: %+v; kuadrant step: %q", res, kuadrantMessages(events))
	}
}

// On full timeout expiry, the outcome message names the elapsed limit explicitly.
func TestMaaSSetup_KuadrantAPIWaitTimeoutSaysTheTimeout(t *testing.T) {
	res, _ := runKuadrantAPIWait(t, func(_ int, w http.ResponseWriter, _ *http.Request) bool { return notServedYet(w) })
	want := "Connectivity Link's Kuadrant API is not served after 10:00. Set up again to continue; the MaaS tab shows Connectivity Link's state."
	if res.Success || res.Message != want || res.ErrorCode != "not_ready" {
		t.Fatalf("setup: %+v\nwant message %q", res, want)
	}
}

// initializing429 answers 429 "storage is (re)initializing" with
// Retry-After secs, as the API server does while a watch cache initializes.
func initializing429(w http.ResponseWriter, secs string) bool {
	w.Header().Set("Retry-After", secs)
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = io.WriteString(w, `{"kind":"Status","apiVersion":"v1","metadata":{},"status":"Failure","message":"storage is (re)initializing","reason":"TooManyRequests","details":{"retryAfterSeconds":`+secs+`},"code":429}`)
	return true
}

// A 429 that outlasts the client's retries (11 attempts) or its wait
// budget (two 30 s hints) means "not served yet": the wait polls again
// and Set up goes on once the list answers.
func TestMaaSSetup_KuadrantAPIWaitPollsThroughThrottling(t *testing.T) {
	for _, tc := range []struct {
		name       string
		retryAfter string
		answers    int // consecutive 429s
	}{
		{"15 answers with Retry-After 1", "1", 15},
		{"two answers with Retry-After 30", "30", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, events := runKuadrantAPIWait(t, func(read int, w http.ResponseWriter, r *http.Request) bool {
				if read > tc.answers || r.URL.Path != kuadrantListV1beta1 {
					return false
				}
				return initializing429(w, tc.retryAfter)
			})
			if !res.Success {
				t.Fatalf("setup: %+v; kuadrant step: %q", res, kuadrantMessages(events))
			}
		})
	}
}

// A wait throttled until its timeout says so, with the last answer.
func TestMaaSSetup_KuadrantAPIWaitThrottledUntilTheTimeout(t *testing.T) {
	res, _ := runKuadrantAPIWait(t, func(_ int, w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != kuadrantListV1beta1 {
			return notServedYet(w)
		}
		return initializing429(w, "1")
	})
	want := "Connectivity Link's Kuadrant API is not served after 10:00; the API server last answered: k8s API error 429 (TooManyRequests): storage is (re)initializing. Set up again to continue; the MaaS tab shows Connectivity Link's state."
	if res.Success || res.Message != want || res.ErrorCode != "not_ready" {
		t.Fatalf("setup: %+v\nwant message %q", res, want)
	}
}

// A set of prerequisites (models through a gateway) waits for Kuadrant's
// API with the same handling: Connectivity Link just installed, the list
// answers 429 with Retry-After for more than one request's retries, and
// the set goes on to create the Kuadrant CR.
func TestPrerequisitesSet_KuadrantAPIWaitPollsThroughThrottling(t *testing.T) {
	pc := newPrereqCluster(t)
	pc.sim.packages["rhcl-operator"] = rhclFakePackage()
	pc.putPackage("rhcl-operator")
	installed := false
	prev := pc.sim.onInstalled
	pc.sim.onInstalled = func(pkg, ns, from, to string) {
		prev(pkg, ns, from, to)
		if pkg == "rhcl-operator" {
			pc.f.groups = map[string]string{"/apis/kuadrant.io": kuadrantGroupDoc}
			installKuadrantCRDLocked(pc.f)
			installed = true
		}
	}
	pc.f.fail["GET /apis/kuadrant.io/v1/kuadrants"] = 404
	throttled := 0
	pc.f.intercept = func(w http.ResponseWriter, r *http.Request, _ []byte) bool {
		if !installed || r.Method != http.MethodGet || r.URL.Path != kuadrantListV1beta1 || r.URL.Query().Get("limit") != "1" || throttled >= 15 {
			return false
		}
		throttled++
		return initializing429(w, "1")
	}
	res, events := runPrereqSet(t, pc.c, []features.SetItem{{Feature: "pipelines"}, {Feature: "models", Exposure: serving.ExposureDefault}})
	if !res.Success || throttled != 15 {
		t.Fatalf("setup: %+v (%s), %d throttled reads", res, finalSteps(events), throttled)
	}
	if !pc.f.has("/apis/kuadrant.io/v1beta1/namespaces/kuadrant-system/kuadrants/kuadrant") {
		t.Fatal("no Kuadrant CR")
	}
}
