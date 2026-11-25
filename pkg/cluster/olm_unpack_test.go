package cluster

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/clock/clocktest"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/catalog"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/operator"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
)

const testBundleImage = "registry.redhat.io/rhoai/odh-operator-bundle@sha256:0123abcd"

// unpackingSubscription is the status OLM 4.22 gives a Subscription while
// its bundle unpack Job runs: BundleUnpacking True with no message
// (operator-framework-olm release-4.22, catalog/operator.go:1423-1440), and
// no InstallPlan or CSV yet.
func unpackingSubscription(f *fakeOLM) {
	f.sub["status"] = map[string]any{"conditions": []any{
		map[string]any{"type": "CatalogSourcesUnhealthy", "status": "False", "reason": "AllCatalogSourcesHealthy", "message": "all available catalogsources are healthy"},
		map[string]any{"type": "BundleUnpacking", "status": "True", "reason": "UnpackingInProgress"},
	}}
}

// unpackPodsPullFailing is OLM's unpack pods as the field case had them:
// one Pending pod whose "pull" init container (the bundle image) cannot
// be pulled, a second one with the same failure, one whose containers are
// only starting, one whose OLM "util" container (not the bundle) cannot be
// pulled, and an older pod that already finished.
const unpackPodsPullFailing = `{"items":[
 {"metadata":{"name":"a1"},"status":{"phase":"Pending","initContainerStatuses":[
   {"name":"util","image":"registry.redhat.io/openshift4/ose-operator-framework-tools-rhel9@sha256:1","ready":true,"state":{"terminated":{"reason":"Completed"}}},
   {"name":"pull","image":"` + testBundleImage + `","ready":false,"state":{"waiting":{"reason":"ImagePullBackOff","message":"Back-off pulling image \"` + testBundleImage + `\": manifest unknown"}}}]}},
 {"metadata":{"name":"a2"},"status":{"phase":"Pending","initContainerStatuses":[
   {"name":"pull","image":"` + testBundleImage + `","ready":false,"state":{"waiting":{"reason":"ImagePullBackOff","message":"Back-off pulling image \"` + testBundleImage + `\": manifest unknown"}}}]}},
 {"metadata":{"name":"b1"},"status":{"phase":"Pending","initContainerStatuses":[
   {"name":"pull","image":"registry.redhat.io/rhoai/other-bundle@sha256:9","ready":false,"state":{"waiting":{"reason":"PodInitializing"}}}]}},
 {"metadata":{"name":"d1"},"status":{"phase":"Pending","initContainerStatuses":[
   {"name":"util","image":"registry.redhat.io/openshift4/util-image@sha256:7","ready":false,"state":{"waiting":{"reason":"ErrImagePull","message":"unauthorized"}}}]}},
 {"metadata":{"name":"c1"},"status":{"phase":"Failed","initContainerStatuses":[
   {"name":"pull","image":"registry.redhat.io/rhoai/old-bundle@sha256:8","ready":false,"state":{"waiting":{"reason":"ErrImagePull","message":"old"}}}]}}
]}`

// unpackPodLists counts the reads of OLM's bundle-unpack pods.
func unpackPodLists(f *fakeOLM) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.requests {
		if r.Method == http.MethodGet && strings.HasSuffix(r.Path, "/namespaces/"+rhoai.CatalogNS+"/pods") && r.Query.Get("labelSelector") == catalog.BundleUnpackRefLabel {
			n++
		}
	}
	return n
}

// The field case on OLM 4.22: no InstallPlan, no CSV, no failure condition,
// while the unpack pod cannot pull the bundle. The wait names the
// Subscription's conditions and the pull error, and its give-up message says
// that OLM was still unpacking and what to check.
func TestUpdateStream_ReportsTheBundleUnpackPullFailure(t *testing.T) {
	const newCSV = "rhods-operator.3.6.1"
	f := newFakeOLM(t).installed("rhods-operator.3.6.0", map[string]any{"channel": "fast"})
	f.channels = `[{"name":"stable-3.x","currentCSV":"` + newCSV + `"}]`
	f.unpackPods = unpackPodsPullFailing
	f.onSubscribe = func(f *fakeOLM, spec map[string]any) {
		if spec["channel"] != "fast" { // the restored Subscription is not the attempt
			unpackingSubscription(f)
		}
	}
	emit, events := eventsRecorder()
	result, err := operator.UpdateStream(f.client(context.Background()), testNightlyImage, emit)
	if err != nil || result == nil || result.Success || result.ErrorCode != "install_timeout" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	wantState := "OLM is unpacking the operator bundle; Subscription conditions: CatalogSourcesUnhealthy=False (AllCatalogSourcesHealthy), BundleUnpacking=True (UnpackingInProgress). " +
		"an OLM bundle unpack in " + rhoai.CatalogNS + " cannot pull " + testBundleImage + ": ImagePullBackOff (manifest unknown)"
	for _, want := range []string{
		"last state: " + wantState,
		"OLM was still unpacking the operator bundle",
		"had reported no verdict: it reports BundleUnpackFailed only after its bundle unpack timeout, which the cluster sets",
		"the annotation " + catalog.BundleUnpackTimeoutAnnotation + " on the OperatorGroup in " + rhoai.SubNS,
		"oc get pods -n " + rhoai.CatalogNS + " -l " + catalog.BundleUnpackRefLabel,
	} {
		if !strings.Contains(result.Message, want) {
			t.Fatalf("message lacks %q:\n%s", want, result.Message)
		}
	}
	if n := strings.Count(result.Message, "cannot pull"); n != 1 {
		t.Fatalf("%d pull failures reported, want the one image once:\n%s", n, result.Message)
	}
	for _, unwanted := range []string{"PodInitializing", "old-bundle", "other-bundle", "util-image", "Back-off", "kept because OLM is still installing"} {
		if strings.Contains(result.Message, unwanted) {
			t.Fatalf("message reports %q:\n%s", unwanted, result.Message)
		}
	}
	shown := false
	for _, e := range events() {
		shown = shown || e.Step == "verify_installplan" && e.Status == "running" && e.Message == wantState
	}
	if !shown {
		t.Fatalf("the wait did not show the state while it ran: %v", events())
	}
	if lastStatus(events(), operator.RestoreStepName) != "success" {
		t.Fatalf("no InstallPlan yet: the previous operator must be restored: %v", events())
	}
}

// Under Argo CD, Update waits with the same WaitForInstall: the same
// evidence reaches its result.
func TestUpdateUnderGitOps_ReportsTheBundleUnpackPullFailure(t *testing.T) {
	f := gitOpsFake(t)
	f.unpackPods = unpackPodsPullFailing
	f.onSubscribe = func(f *fakeOLM, _ map[string]any) { unpackingSubscription(f) }
	result, err := operator.UpdateStream(f.client(context.Background()), gitOpsImage, func(UpdateStepEvent) {})
	if err != nil || result.Success || result.ErrorCode != "install_timeout" ||
		!strings.Contains(result.Message, "cannot pull "+testBundleImage+": ImagePullBackOff (manifest unknown)") ||
		!strings.Contains(result.Message, "OLM was still unpacking the operator bundle") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

// Before OLM unpacks anything the wait reports the conditions it set and
// does not read the unpack pods.
func TestWaitForInstall_ReportsConditionsWhileResolving(t *testing.T) {
	f := newFakeOLM(t)
	f.unpackPods = unpackPodsPullFailing
	f.sub = map[string]any{"metadata": map[string]any{"name": rhoai.SubName}, "spec": map[string]any{"sourceNamespace": rhoai.CatalogNS}, "status": map[string]any{"conditions": []any{
		cond("CatalogSourcesUnhealthy", "UnhealthyCatalogSourceFound", "targeted catalogsource openshift-marketplace/rhoai-catalog-dev unhealthy"),
	}}}
	var logs []string
	out := operator.WaitForInstall(f.client(context.Background()), func(UpdateStepEvent) {}, &logs, &operator.Recovery{}, "")
	want := "last state: Waiting for OLM to resolve the Subscription; Subscription conditions: CatalogSourcesUnhealthy=True (UnhealthyCatalogSourceFound: targeted catalogsource openshift-marketplace/rhoai-catalog-dev unhealthy))."
	if out.ErrorCode != "install_timeout" || !strings.Contains(out.Message, want) || strings.Contains(out.Message, "unpacking") {
		t.Fatalf("outcome = %+v", out)
	}
	if n := unpackPodLists(f); n != 0 {
		t.Fatalf("the unpack pods were read %d times before OLM unpacked anything", n)
	}
}

// Unreadable unpack pods are said, not hidden.
func TestWaitForInstall_SaysWhenUnpackPodsCannotBeRead(t *testing.T) {
	f := newFakeOLM(t)
	f.sub = map[string]any{"metadata": map[string]any{"name": rhoai.SubName}, "spec": map[string]any{"sourceNamespace": rhoai.CatalogNS}, "status": map[string]any{}}
	unpackingSubscription(f)
	f.intercept = func(_ *fakeOLM, w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Query().Get("labelSelector") != catalog.BundleUnpackRefLabel {
			return false
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"kind":"Status","status":"Failure","reason":"Forbidden","message":"pods is forbidden","code":403}`))
		return true
	}
	var logs []string
	out := operator.WaitForInstall(f.client(context.Background()), func(UpdateStepEvent) {}, &logs, &operator.Recovery{}, "")
	if out.ErrorCode != "install_timeout" || !strings.Contains(out.Message, "OLM's bundle unpack pods in "+rhoai.CatalogNS+" could not be read") ||
		!strings.Contains(out.Message, "OLM was still unpacking the operator bundle") {
		t.Fatalf("outcome = %+v", out)
	}
	if unpackPodLists(f) == 0 {
		t.Fatal("the unpack pods were not read")
	}
}

// Registry error text is never quoted, from the unpack pod or from the
// Subscription's bundle conditions (OLM 4.23 and later put the pod's pull
// failure into BundleUnpacking): it can carry a signed URL. Only reasons
// and a loose hint at the kind of error reach the progress, the logs and
// the result.
func TestWaitForInstall_NeverQuotesRegistryErrors(t *testing.T) {
	// A signed download URL as a registry error can quote it (fake values).
	signature := "X-Amz-Signature=" + strings.Repeat("0", 8)
	signedURL := "https://cdn.registry.example/blobs/sha256:1?X-Amz-Credential=example&" + signature
	leaked := func(t *testing.T, out string, logs []string, events []UpdateStepEvent) {
		t.Helper()
		texts := append([]string{out}, logs...)
		for _, e := range events {
			texts = append(texts, e.Message, e.Detail)
		}
		for _, text := range texts {
			if strings.Contains(text, "https://") || strings.Contains(text, signature) || strings.Contains(text, "X-Amz-Credential") || strings.Contains(text, "cdn.registry.example") {
				t.Fatalf("registry error text leaked: %q", text)
			}
		}
	}
	for _, tc := range []struct{ message, kind string }{
		{"reading manifest sha256:1 in " + signedURL + ": manifest unknown", "manifest unknown"},
		{"fetching blob " + signedURL + ": unauthorized: authentication required", "unauthorized"},
		{"pinging container registry " + signedURL + ": 404 Not Found", "not found"},
		{"pull QPS exceeded at " + signedURL, "pull QPS exceeded"},
		{"Get " + signedURL + ": dial tcp: i/o timeout", "pull error"},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			f := newFakeOLM(t)
			f.sub = map[string]any{"metadata": map[string]any{"name": rhoai.SubName}, "spec": map[string]any{"sourceNamespace": rhoai.CatalogNS}, "status": map[string]any{"conditions": []any{
				cond("BundleUnpacking", "UnpackingInProgress", "Bundle image pull failed: "+tc.message),
			}}}
			f.unpackPods = `{"items":[{"metadata":{"name":"a1"},"status":{"phase":"Pending","initContainerStatuses":[
			  {"name":"pull","image":"` + testBundleImage + `","ready":false,"state":{"waiting":{"reason":"ErrImagePull","message":` + jsonString(tc.message) + `}}}]}}]}`
			emit, events := eventsRecorder()
			var logs []string
			out := operator.WaitForInstall(f.client(context.Background()), emit, &logs, &operator.Recovery{}, "")
			for _, want := range []string{
				"cannot pull " + testBundleImage + ": ErrImagePull (" + tc.kind + ")",
				"BundleUnpacking=True (UnpackingInProgress, " + tc.kind + ")",
			} {
				if !strings.Contains(out.Message, want) {
					t.Fatalf("message lacks %q:\n%s", want, out.Message)
				}
			}
			leaked(t, out.Message, logs, events())
		})
	}
	t.Run("BundleUnpackFailed", func(t *testing.T) {
		f := newFakeOLM(t)
		f.sub = map[string]any{"metadata": map[string]any{"name": rhoai.SubName}, "spec": map[string]any{"sourceNamespace": rhoai.CatalogNS}, "status": map[string]any{"conditions": []any{
			cond("BundleUnpackFailed", "BundleUnpackFailed", "bundle unpacking failed. Reason: DeadlineExceeded, and Message: pull of "+signedURL+" failed: unauthorized"),
		}}}
		emit, events := eventsRecorder()
		var logs []string
		out := operator.WaitForInstall(f.client(context.Background()), emit, &logs, &operator.Recovery{}, "")
		if want := "OLM could not unpack the operator bundle (BundleUnpackFailed, Job DeadlineExceeded, unauthorized)"; out.ErrorCode != "bundle_unpack_failed" || !strings.Contains(out.Message, want) {
			t.Fatalf("outcome %+v lacks %q", out, want)
		}
		leaked(t, out.Message, logs, events())
	})
}

// A diagnostic read that stalls near the cutoff ends with it: the wait
// reports install_timeout before the operation's deadline, with the last
// state it had, not an error of the cut read.
func TestWaitForInstall_StalledDiagnosticReadEndsAtTheCutoff(t *testing.T) {
	fake, ok := kube.Clock().(*clocktest.Fake)
	if !ok {
		t.Fatal("the tests' clock is not a clocktest.Fake")
	}
	f := newFakeOLM(t)
	f.sub = map[string]any{"metadata": map[string]any{"name": rhoai.SubName}, "spec": map[string]any{"sourceNamespace": rhoai.CatalogNS}, "status": map[string]any{}}
	unpackingSubscription(f)
	f.unpackPods = unpackPodsPullFailing
	ctx, cancel := kube.Clock().WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	opDeadline, _ := ctx.Deadline()
	cutoff := opDeadline.Add(-operator.InstallDeadlineReserve)
	stalled := false
	f.intercept = func(f *fakeOLM, _ http.ResponseWriter, r *http.Request) bool {
		if stalled || r.URL.Query().Get("labelSelector") != catalog.BundleUnpackRefLabel || kube.Clock().Until(cutoff) > time.Second {
			return false
		}
		// The API server stalls on the pod list until the client gives up;
		// meanwhile the time passes the cutoff, not the operation's deadline.
		stalled = true
		fake.Advance(kube.Clock().Until(cutoff) + time.Second)
		<-r.Context().Done()
		return true
	}
	var logs []string
	out := operator.WaitForInstall(f.client(ctx), func(UpdateStepEvent) {}, &logs, &operator.Recovery{}, "")
	if !stalled {
		t.Fatal("the pod list never stalled")
	}
	if out.ErrorCode != "install_timeout" || !strings.Contains(out.Message, "operation's time budget") || ctx.Err() != nil {
		t.Fatalf("outcome %+v, operation context %v", out, ctx.Err())
	}
	if !strings.Contains(out.Message, "cannot pull "+testBundleImage) || strings.Contains(out.Message, "could not be read") {
		t.Fatalf("the last state is not the one before the cut read: %s", out.Message)
	}
}

// The wait assumes no OLM unpack timeout: it ends on OLM's verdict or at
// the operation's deadline. On the tests' fake clock, with the production
// poll interval and settle period and the 15-minute operation deadline,
// the unpack pod cannot pull the bundle until recoverAt (OLM then installs
// the CSV) or OLM reports BundleUnpackFailed at failAt.
func TestUpdateStream_WaitsForOLMsVerdictOrTheOperationDeadline(t *testing.T) {
	const newCSV = "rhods-operator.3.6.1"
	for _, tc := range []struct {
		name                  string
		recoverAt, failAt     time.Duration
		wantCode              string
		endsAfter, endsBefore time.Duration // the wait's end, after the Subscription was created
	}{
		// The field case: the Job's next pod pulled the bundle 8m16s in.
		{name: "pull recovers after 8m16s", recoverAt: 8*time.Minute + 16*time.Second, endsAfter: 8 * time.Minute, endsBefore: 9 * time.Minute},
		// A cluster whose OLM unpack timeout is longer than 10 minutes.
		{name: "pull recovers after 13 minutes", recoverAt: 13 * time.Minute, endsAfter: 13 * time.Minute, endsBefore: 14 * time.Minute},
		{name: "OLM's verdict ends the wait early", failAt: 2 * time.Minute, wantCode: "bundle_unpack_failed", endsAfter: 2 * time.Minute, endsBefore: 3 * time.Minute},
		{name: "the operation's time runs out", wantCode: "install_timeout", endsAfter: 14 * time.Minute, endsBefore: record.OperationDeadline},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prevPoll, prevSettle := operator.InstallPlanPollInterval, operator.CSVSucceededSettle
			operator.InstallPlanPollInterval, operator.CSVSucceededSettle = 5*time.Second, 20*time.Second
			t.Cleanup(func() { operator.InstallPlanPollInterval, operator.CSVSucceededSettle = prevPoll, prevSettle })

			f := newFakeOLM(t).installed("rhods-operator.3.6.0", map[string]any{"channel": "fast"})
			f.channels = `[{"name":"stable-3.x","currentCSV":"` + newCSV + `"}]`
			f.unpackPods = unpackPodsPullFailing
			var subscribed, ended time.Time
			f.onSubscribe = func(f *fakeOLM, spec map[string]any) {
				if spec["channel"] != "fast" { // the restored Subscription is not the attempt
					subscribed = kube.Clock().Now()
					unpackingSubscription(f)
				}
			}
			decided := false
			f.intercept = func(f *fakeOLM, _ http.ResponseWriter, r *http.Request) bool {
				if decided || subscribed.IsZero() || r.Method != http.MethodGet || r.URL.Path != rhoai.SubscriptionPath() {
					return false
				}
				switch since := kube.Clock().Since(subscribed); {
				case tc.recoverAt > 0 && since >= tc.recoverAt:
					decided = true
					f.unpackPods = ""
					olmInstalls(newCSV)(f, nil)
				case tc.failAt > 0 && since >= tc.failAt:
					decided = true
					f.sub["status"] = map[string]any{"conditions": []any{cond("BundleUnpackFailed", "BundleUnpackFailed", "bundle unpacking failed. Reason: DeadlineExceeded")}}
				}
				return false
			}
			rec, events := eventsRecorder()
			emit := func(e UpdateStepEvent) {
				if e.Step == "verify_installplan" && e.Status != "running" && ended.IsZero() {
					ended = kube.Clock().Now()
				}
				rec(e)
			}
			ctx, cancel := kube.Clock().WithTimeout(context.Background(), record.OperationDeadline)
			defer cancel()
			result, err := operator.UpdateStream(f.client(ctx), testNightlyImage, emit)
			if err != nil || result == nil || result.Success != (tc.wantCode == "") || result.ErrorCode != tc.wantCode {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if took := ended.Sub(subscribed); took < tc.endsAfter || took >= tc.endsBefore {
				t.Fatalf("the wait ended %s after the Subscription was created, want between %s and %s", took, tc.endsAfter, tc.endsBefore)
			}
			sawPullFailure := false
			for _, e := range events() {
				sawPullFailure = sawPullFailure || strings.Contains(e.Message, "cannot pull "+testBundleImage)
			}
			if !sawPullFailure {
				t.Fatalf("the pull failure was not shown while it lasted: %v", events())
			}
			if restored := lastStatus(events(), operator.RestoreStepName) == "success"; restored != (tc.wantCode != "") {
				t.Fatalf("restore ran=%v: %v", restored, events())
			}
			if tc.wantCode == "install_timeout" && !strings.Contains(result.Message, "operation's time budget") {
				t.Fatalf("message: %s", result.Message)
			}
		})
	}
}

// The unpack pods are read in the namespace of the Subscription's
// CatalogSource, and the OperatorGroup's unpack-timeout annotation is
// shown when it sets one.
func TestWaitForInstall_ReadsUnpackPodsInTheCatalogsNamespace(t *testing.T) {
	const ns = "team-catalogs"
	f := newFakeOLM(t)
	f.operatorGrps = []any{map[string]any{"metadata": map[string]any{"name": "rhods-operator", "annotations": map[string]any{catalog.BundleUnpackTimeoutAnnotation: "30m"}}, "spec": map[string]any{}}}
	f.sub = map[string]any{"metadata": map[string]any{"name": rhoai.SubName}, "spec": map[string]any{"source": "team-catalog", "sourceNamespace": ns}, "status": map[string]any{}}
	unpackingSubscription(f)
	f.intercept = func(_ *fakeOLM, w http.ResponseWriter, r *http.Request) bool {
		if r.URL.Path != "/api/v1/namespaces/"+ns+"/pods" || r.URL.Query().Get("labelSelector") != catalog.BundleUnpackRefLabel {
			return false
		}
		_, _ = w.Write([]byte(unpackPodsPullFailing))
		return true
	}
	var logs []string
	out := operator.WaitForInstall(f.client(context.Background()), func(UpdateStepEvent) {}, &logs, &operator.Recovery{}, "")
	for _, want := range []string{
		"an OLM bundle unpack in " + ns + " cannot pull " + testBundleImage + ": ImagePullBackOff (manifest unknown)",
		`OperatorGroup rhods-operator has the annotation ` + catalog.BundleUnpackTimeoutAnnotation + `: "30m"`,
		"oc get pods -n " + ns + " -l " + catalog.BundleUnpackRefLabel,
	} {
		if !strings.Contains(out.Message, want) {
			t.Fatalf("message lacks %q:\n%s", want, out.Message)
		}
	}
	if n := unpackPodLists(f); n != 0 {
		t.Fatalf("the pods of %s were read %d times", rhoai.CatalogNS, n)
	}
}
