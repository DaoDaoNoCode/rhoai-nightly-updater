package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// fakeOLM is a small stateful stand-in for the Kubernetes API with OLM. It
// keeps the Subscription, CSVs, InstallPlans and catalogs, records every
// request, and calls onSubscribe when a Subscription is applied so a test
// decides what OLM does next.
type fakeOLM struct {
	t  *testing.T
	mu sync.Mutex

	sub          map[string]interface{} // nil = no Subscription
	csvs         map[string]map[string]interface{}
	installPlans map[string]map[string]interface{}
	catalog      map[string]interface{} // nil = no nightly catalog
	operatorGrps []interface{}
	channels     string // PackageManifest channels JSON for every catalog
	stableChans  string // channels of the stable catalog (redhat-operators), "" = same as channels
	catalogPods  string // pod list JSON for olm.catalogSource pods
	dashboardOp  string // dashboard-operator Deployment JSON, "" = absent
	platform     string // Platform "default" JSON, "" = absent
	dsc          string // DataScienceCluster "default-dsc" JSON, "" = no DSC API
	failDelete   map[string]int
	stuckCSVs    map[string]bool // CSVs whose DELETE only sets deletionTimestamp (a finalizer that never finishes)
	recordedSub  string          // data.operator-subscription of the snapshot ConfigMap
	crds         string          // CRD list JSON, "" = 404
	csvSeq       int
	verifyState  string // state of verification catalogs, "" = READY
	mainChannels *string
	rejectDryRun bool

	onSubscribe func(f *fakeOLM, spec map[string]interface{})
	onApprove   func(f *fakeOLM, ip string)
	// intercept runs first (under the lock) and may answer the request itself.
	intercept func(f *fakeOLM, w http.ResponseWriter, r *http.Request) bool

	requests []fakeRequest
}

type fakeRequest struct {
	Method, Path string
	Query        url.Values
	Body         map[string]interface{}
}

func newFakeOLM(t *testing.T) *fakeOLM {
	return &fakeOLM{
		t:            t,
		csvs:         map[string]map[string]interface{}{},
		installPlans: map[string]map[string]interface{}{},
		operatorGrps: []interface{}{map[string]interface{}{"metadata": map[string]interface{}{"name": "rhods-operator"}, "spec": map[string]interface{}{}}},
		channels:     `[{"name":"stable-3.x","currentCSV":"rhods-operator.3.6.0"}]`,
		catalogPods:  `{"items":[]}`,
		failDelete:   map[string]int{},
		stuckCSVs:    map[string]bool{},
	}
}

// installed sets up a running operator on the nightly catalog.
func (f *fakeOLM) installed(csv string, spec map[string]interface{}) *fakeOLM {
	if spec == nil {
		spec = map[string]interface{}{}
	}
	full := map[string]interface{}{"channel": "stable-3.x", "installPlanApproval": "Automatic", "name": SubName, "source": CatalogName, "sourceNamespace": CatalogNS}
	for k, v := range spec {
		full[k] = v
	}
	f.sub = map[string]interface{}{"metadata": map[string]interface{}{"name": SubName, "uid": "uid-sub-installed"}, "spec": full,
		"status": map[string]interface{}{"state": "AtLatestKnown", "currentCSV": csv, "installedCSV": csv, "installPlanRef": map[string]interface{}{"name": "install-old"}}}
	f.addCSV(csv, "Succeeded")
	f.installPlans["install-old"] = map[string]interface{}{"spec": map[string]interface{}{"clusterServiceVersionNames": []string{csv}}, "status": map[string]interface{}{"phase": "Complete"}}
	f.catalog = map[string]interface{}{"spec": map[string]interface{}{"image": "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6@sha256:old"}}
	return f
}

func (f *fakeOLM) addCSV(name, phase string) {
	f.csvSeq++
	f.csvs[name] = map[string]interface{}{
		"metadata": map[string]interface{}{"name": name, "uid": fmt.Sprintf("uid-%s-%d", name, f.csvSeq)},
		"spec":     map[string]interface{}{"displayName": "Red Hat OpenShift AI", "version": strings.TrimPrefix(name, SubName+".")},
		"status":   map[string]interface{}{"phase": phase},
	}
}

// olmInstalls returns an onSubscribe that installs the channel head the way
// OLM does: InstallPlan Complete, CSV Succeeded, status filled in.
func olmInstalls(csv string) func(*fakeOLM, map[string]interface{}) {
	return func(f *fakeOLM, _ map[string]interface{}) {
		f.installPlans["install-new"] = map[string]interface{}{"spec": map[string]interface{}{"clusterServiceVersionNames": []string{csv}}, "status": map[string]interface{}{"phase": "Complete"}}
		f.addCSV(csv, "Succeeded")
		f.sub["status"] = map[string]interface{}{"currentCSV": csv, "installedCSV": csv, "installPlanRef": map[string]interface{}{"name": "install-new"}, "state": "AtLatestKnown"}
	}
}

func (f *fakeOLM) client(ctx context.Context) *Client {
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	f.t.Cleanup(srv.Close)
	return &Client{baseURL: srv.URL, httpClient: srv.Client(), ctx: ctx}
}

func notFound(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNotFound)
	_, _ = io.WriteString(w, `{"kind":"Status","status":"Failure","reason":"NotFound","code":404}`)
}

// uidPreconditionMet reports whether a DELETE body's preconditions.uid, if
// any, matches obj's metadata.uid, as the API server checks it.
func uidPreconditionMet(body, obj map[string]interface{}) bool {
	pre, _ := body["preconditions"].(map[string]interface{})
	want, _ := pre["uid"].(string)
	if want == "" {
		return true
	}
	meta, _ := obj["metadata"].(map[string]interface{})
	return meta["uid"] == want
}

// uidConflict answers like the API server when a UID precondition fails.
func uidConflict(w http.ResponseWriter) {
	w.WriteHeader(http.StatusConflict)
	_, _ = io.WriteString(w, `{"kind":"Status","status":"Failure","reason":"Conflict","message":"Precondition failed: UID in precondition does not match","code":409}`)
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeOLM) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var body map[string]interface{}
	if r.Body != nil {
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &body)
	}
	p := r.URL.Path
	f.requests = append(f.requests, fakeRequest{Method: r.Method, Path: p, Query: r.URL.Query(), Body: body})
	if f.intercept != nil && f.intercept(f, w, r) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if n := f.failDelete[p]; r.Method == http.MethodDelete && n > 0 {
		f.failDelete[p] = n - 1
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"kind":"Status","status":"Failure","message":"denied","code":403}`)
		return
	}
	last := p[strings.LastIndex(p, "/")+1:]
	switch {
	case strings.HasSuffix(p, "/secrets/additional-pull-secret"):
		_, _ = io.WriteString(w, pullSecretBody(f.t, map[string]interface{}{"quay.io/rhoai": map[string]interface{}{"auth": exampleAuth("user:pass")}}))
	case strings.HasSuffix(p, "/imagedigestmirrorsets"):
		_, _ = fmt.Fprintf(w, `{"items":[{"metadata":{"name":"rhoai-mirror"},"spec":{"imageDigestMirrors":[{"source":%q}]}}]}`, IDMSSource)
	case p == "/api/v1/namespaces/"+SubNS:
		_, _ = io.WriteString(w, `{"metadata":{"name":"`+SubNS+`"}}`)
	case strings.HasSuffix(p, "/operatorgroups"):
		writeJSON(w, map[string]interface{}{"items": f.operatorGrps})
	case strings.Contains(p, "/operatorgroups/"):
		f.operatorGrps = append(f.operatorGrps, body)
		_, _ = io.WriteString(w, `{}`)
	case strings.HasSuffix(p, "/catalogsources"):
		_, _ = io.WriteString(w, `{"items":[]}`)
	case strings.Contains(p, "/catalogsources/"):
		f.serveCatalog(w, r, last, body)
	case strings.HasSuffix(p, "/packagemanifests"):
		source := strings.TrimPrefix(r.URL.Query().Get("labelSelector"), "catalog=")
		channels := f.channels
		if source == CatalogName && f.mainChannels != nil {
			channels = *f.mainChannels
		}
		if source == getStableSource() && f.stableChans != "" {
			channels = f.stableChans
		}
		_, _ = fmt.Fprintf(w, `{"items":[{"metadata":{"name":%q},"status":{"packageName":%q,"catalogSource":%q,"catalogSourceNamespace":%q,"defaultChannel":"stable-3.x","channels":%s}}]}`, SubName, SubName, source, CatalogNS, channels)
	case strings.HasSuffix(p, "/namespaces/"+CatalogNS+"/pods"):
		_, _ = io.WriteString(w, f.catalogPods)
	case strings.HasSuffix(p, "/subscriptions/"+SubName):
		f.serveSubscription(w, r, body)
	case strings.HasSuffix(p, "/clusterserviceversions"):
		var items []interface{}
		for _, csv := range f.csvs {
			items = append(items, csv)
		}
		writeJSON(w, map[string]interface{}{"items": items})
	case strings.Contains(p, "/clusterserviceversions/"):
		csv, ok := f.csvs[last]
		switch {
		case !ok:
			notFound(w)
		case r.Method == http.MethodDelete && !uidPreconditionMet(body, csv):
			uidConflict(w)
		case r.Method == http.MethodDelete && f.stuckCSVs[last]:
			csv["metadata"].(map[string]interface{})["deletionTimestamp"] = "2026-01-01T00:00:00Z"
			_, _ = io.WriteString(w, `{}`)
		case r.Method == http.MethodDelete:
			delete(f.csvs, last)
			_, _ = io.WriteString(w, `{}`)
		default:
			writeJSON(w, csv)
		}
	case strings.Contains(p, "/installplans/"):
		ip, ok := f.installPlans[last]
		switch {
		case !ok:
			notFound(w)
		case r.Method == http.MethodDelete:
			delete(f.installPlans, last)
			_, _ = io.WriteString(w, `{}`)
		case r.Method == http.MethodPatch:
			ip["spec"].(map[string]interface{})["approved"] = true
			if f.onApprove != nil {
				f.onApprove(f, last)
			}
			_, _ = io.WriteString(w, `{}`)
		default:
			writeJSON(w, ip)
		}
	case strings.HasSuffix(p, "/deployments/dashboard-operator"):
		if f.dashboardOp == "" {
			notFound(w)
			return
		}
		_, _ = io.WriteString(w, f.dashboardOp)
	case strings.HasSuffix(p, "/deployments") || strings.HasSuffix(p, "/pods") ||
		strings.HasSuffix(p, "webhookconfigurations"):
		_, _ = io.WriteString(w, `{"items":[]}`)
	case strings.HasSuffix(p, "/datascienceclusters") && f.dsc != "":
		_, _ = io.WriteString(w, `{"items":[{"metadata":{"name":"default-dsc"}}]}`)
	case strings.HasSuffix(p, "/datascienceclusters/default-dsc") && f.dsc != "":
		_, _ = io.WriteString(w, f.dsc)
	case strings.HasSuffix(p, "/platforms/default"):
		if f.platform == "" {
			notFound(w)
			return
		}
		_, _ = io.WriteString(w, f.platform)
	case strings.HasSuffix(p, "/configmaps/"+snapshotConfigMapName) && r.Method == http.MethodPatch && r.URL.Query().Get("fieldManager") == subscriptionSnapshotManager:
		data, _ := body["data"].(map[string]interface{})
		f.recordedSub, _ = data[subscriptionSnapshotKey].(string)
		_, _ = io.WriteString(w, `{}`)
	case strings.HasSuffix(p, "/configmaps/"+snapshotConfigMapName) && r.Method == http.MethodGet:
		writeJSON(w, map[string]interface{}{"data": map[string]interface{}{subscriptionSnapshotKey: f.recordedSub}})
	case strings.Contains(p, "/configmaps/") && r.Method != http.MethodGet:
		_, _ = io.WriteString(w, `{}`)
	case p == "/apis/apiextensions.k8s.io/v1/customresourcedefinitions":
		if f.crds == "" {
			notFound(w)
			return
		}
		_, _ = io.WriteString(w, f.crds)
	case strings.HasSuffix(p, "/installplans"):
		var items []interface{}
		for name, ip := range f.installPlans {
			meta, _ := ip["metadata"].(map[string]interface{})
			if meta == nil {
				meta = map[string]interface{}{}
			}
			meta["name"] = name
			items = append(items, map[string]interface{}{"metadata": meta, "spec": ip["spec"]})
		}
		writeJSON(w, map[string]interface{}{"items": items})
	default:
		notFound(w)
	}
}

func (f *fakeOLM) serveCatalog(w http.ResponseWriter, r *http.Request, name string, body map[string]interface{}) {
	verify := strings.Contains(name, "-verify-")
	switch r.Method {
	case http.MethodDelete:
		if !verify {
			if f.catalog == nil {
				notFound(w)
				return
			}
			f.catalog = nil
		}
		_, _ = io.WriteString(w, `{}`)
	case http.MethodPatch:
		if r.URL.Query().Get("dryRun") != "" {
			if f.rejectDryRun {
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = io.WriteString(w, `{"kind":"Status","status":"Failure","message":"spec.image: Invalid value","code":422}`)
				return
			}
			_, _ = io.WriteString(w, `{}`)
			return
		}
		if !verify {
			f.catalog = body
		}
		_, _ = io.WriteString(w, `{}`)
	default:
		if !verify && f.catalog == nil {
			notFound(w)
			return
		}
		image, state := "", "READY"
		if !verify {
			image, _ = f.catalog["spec"].(map[string]interface{})["image"].(string)
		} else if f.verifyState != "" {
			state = f.verifyState
		}
		_, _ = fmt.Fprintf(w, `{"spec":{"image":%q},"status":{"connectionState":{"lastObservedState":%q}}}`, image, state)
	}
}

func (f *fakeOLM) serveSubscription(w http.ResponseWriter, r *http.Request, body map[string]interface{}) {
	switch r.Method {
	case http.MethodDelete:
		if f.sub == nil {
			notFound(w)
			return
		}
		if !uidPreconditionMet(body, f.sub) {
			uidConflict(w)
			return
		}
		f.sub = nil
		_, _ = io.WriteString(w, `{}`)
	case http.MethodPatch:
		spec, _ := body["spec"].(map[string]interface{})
		// An apply keeps the UID of an existing Subscription; a new one
		// gets a new UID.
		uid := ""
		if f.sub != nil {
			uid, _ = f.sub["metadata"].(map[string]interface{})["uid"].(string)
		}
		if uid == "" {
			f.csvSeq++
			uid = fmt.Sprintf("uid-sub-%d", f.csvSeq)
		}
		f.sub = map[string]interface{}{"metadata": map[string]interface{}{"name": SubName, "uid": uid}, "spec": spec, "status": map[string]interface{}{}}
		if f.onSubscribe != nil {
			f.onSubscribe(f, spec)
		}
		_, _ = io.WriteString(w, `{}`)
	default:
		if f.sub == nil {
			notFound(w)
			return
		}
		writeJSON(w, f.sub)
	}
}

// writes returns the mutating requests, "METHOD /path".
func (f *fakeOLM) writes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, r := range f.requests {
		if r.Method != http.MethodGet && !strings.Contains(r.Path, "/configmaps") {
			out = append(out, r.Method+" "+r.Path)
		}
	}
	return out
}

// lastSubscriptionApply returns the spec of the last Subscription apply.
func (f *fakeOLM) subscriptionApplies() []map[string]interface{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]interface{}
	for _, r := range f.requests {
		if r.Method == http.MethodPatch && strings.HasSuffix(r.Path, "/subscriptions/"+SubName) {
			spec, _ := r.Body["spec"].(map[string]interface{})
			out = append(out, spec)
		}
	}
	return out
}

func eventsRecorder() (func(UpdateStepEvent), func() []UpdateStepEvent) {
	var mu sync.Mutex
	var events []UpdateStepEvent
	return func(e UpdateStepEvent) {
			mu.Lock()
			events = append(events, e)
			mu.Unlock()
		}, func() []UpdateStepEvent {
			mu.Lock()
			defer mu.Unlock()
			return append([]UpdateStepEvent(nil), events...)
		}
}

// lastStatus returns the last status emitted for step.
func lastStatus(events []UpdateStepEvent, step string) string {
	status := ""
	for _, e := range events {
		if e.Step == step {
			status = e.Status
		}
	}
	return status
}

// dashboardOperatorAbsent answers the Dashboard Dev guard's lookup with 404
// for mocks that return a generic body for unknown paths.
func dashboardOperatorAbsent(w http.ResponseWriter, r *http.Request) bool {
	if strings.HasSuffix(r.URL.Path, "/deployments/dashboard-operator") {
		notFound(w)
		return true
	}
	return false
}
