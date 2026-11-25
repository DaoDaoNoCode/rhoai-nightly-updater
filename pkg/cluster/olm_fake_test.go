package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/catalog"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/dashdev"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
)

// fakeOLM is a small stateful stand-in for the Kubernetes API with OLM. It
// keeps the Subscription, CSVs, InstallPlans and catalogs, records every
// request, and calls onSubscribe when a Subscription is applied so a test
// decides what OLM does next.
type fakeOLM struct {
	t  *testing.T
	mu sync.Mutex

	sub          map[string]any // nil = no Subscription
	csvs         map[string]map[string]any
	installPlans map[string]map[string]any
	catalog      map[string]any // nil = no nightly catalog
	operatorGrps []any
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
	// gitOpsPods: the nightly catalog's registry pod list follows its
	// image, as OLM replaces the pod (catalogPods is ignored for it).
	gitOpsPods bool
	catalogRV  int // resourceVersion of the nightly catalog
	// csvAnnotations go on every CSV the fake creates (the bundle's
	// containerImage and createdAt, which identify a nightly build).
	csvAnnotations map[string]any

	onSubscribe func(f *fakeOLM, spec map[string]any)
	onApprove   func(f *fakeOLM, ip string)
	// intercept runs first (under the lock) and may answer the request itself.
	intercept func(f *fakeOLM, w http.ResponseWriter, r *http.Request) bool
	// admission: the objects served at these paths (servesGitOpsPolicy).
	admission map[string]map[string]any
	// topology: Infrastructure cluster's status.controlPlaneTopology, ""
	// = Infrastructure not readable (403).
	topology string
	// argo answers Argo CD's (and ACM's) reads of argocd.go; nil = 404.
	argo *fakeArgoCD

	requests []fakeRequest
}

type fakeRequest struct {
	Method, Path string
	Query        url.Values
	Body         map[string]any
}

func newFakeOLM(t *testing.T) *fakeOLM {
	isolateClusterCaches(t)
	return &fakeOLM{
		t:            t,
		csvs:         map[string]map[string]any{},
		installPlans: map[string]map[string]any{},
		operatorGrps: []any{map[string]any{"metadata": map[string]any{"name": "rhods-operator"}, "spec": map[string]any{}}},
		channels:     `[{"name":"stable-3.x","currentCSV":"rhods-operator.3.6.0"}]`,
		catalogPods:  `{"items":[]}`,
		// OLM runs the registry pod of the catalog's current image: Update
		// waits for it (waitForCatalogServing).
		gitOpsPods: true,
		failDelete: map[string]int{},
		stuckCSVs:  map[string]bool{},
	}
}

// installed sets up a running operator on the nightly catalog.
func (f *fakeOLM) installed(csv string, spec map[string]any) *fakeOLM {
	if spec == nil {
		spec = map[string]any{}
	}
	full := map[string]any{"channel": "stable-3.x", "installPlanApproval": "Automatic", "name": rhoai.SubName, "source": rhoai.CatalogName, "sourceNamespace": rhoai.CatalogNS}
	maps.Copy(full, spec)
	f.sub = map[string]any{"metadata": map[string]any{"name": rhoai.SubName, "uid": "uid-sub-installed"}, "spec": full,
		"status": map[string]any{"state": "AtLatestKnown", "currentCSV": csv, "installedCSV": csv, "installPlanRef": map[string]any{"name": "install-old"}}}
	f.addCSV(csv, "Succeeded")
	f.installPlans["install-old"] = map[string]any{"spec": map[string]any{"clusterServiceVersionNames": []string{csv}}, "status": map[string]any{"phase": "Complete"}}
	f.catalog = map[string]any{"spec": map[string]any{"image": "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6@sha256:old"}}
	return f
}

func (f *fakeOLM) addCSV(name, phase string) {
	f.csvSeq++
	meta := map[string]any{"name": name, "uid": fmt.Sprintf("uid-%s-%d", name, f.csvSeq)}
	if f.csvAnnotations != nil {
		ann := map[string]any{}
		maps.Copy(ann, f.csvAnnotations)
		meta["annotations"] = ann
	}
	f.csvs[name] = map[string]any{
		"metadata": meta,
		"spec":     map[string]any{"displayName": "Red Hat OpenShift AI", "version": strings.TrimPrefix(name, rhoai.SubName+".")},
		"status":   map[string]any{"phase": phase},
	}
}

// olmInstalls returns an onSubscribe that installs the channel head the way
// OLM does: InstallPlan Complete, CSV Succeeded, status filled in.
func olmInstalls(csv string) func(*fakeOLM, map[string]any) {
	return func(f *fakeOLM, _ map[string]any) {
		f.installPlans["install-new"] = map[string]any{"spec": map[string]any{"clusterServiceVersionNames": []string{csv}}, "status": map[string]any{"phase": "Complete"}}
		f.addCSV(csv, "Succeeded")
		f.sub["status"] = map[string]any{"currentCSV": csv, "installedCSV": csv, "installPlanRef": map[string]any{"name": "install-new"}, "state": "AtLatestKnown"}
	}
}

func (f *fakeOLM) client(ctx context.Context) *Client {
	srv := newFakePlainServer(http.HandlerFunc(f.serve))
	f.t.Cleanup(srv.Close)
	// A new server may reuse an earlier test's address, which keys the cache.
	rhoai.ControlPlaneCache.Purge()
	return newTestClient(testClientOptions{BaseURL: srv.URL, HTTPClient: srv.Client(), Ctx: ctx})
}

func notFound(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNotFound)
	_, _ = io.WriteString(w, `{"kind":"Status","status":"Failure","reason":"NotFound","code":404}`)
}

// uidPreconditionMet reports whether a DELETE body's preconditions.uid, if
// any, matches obj's metadata.uid, as the API server checks it.
func uidPreconditionMet(body, obj map[string]any) bool {
	pre, _ := body["preconditions"].(map[string]any)
	want, _ := pre["uid"].(string)
	if want == "" {
		return true
	}
	meta, _ := obj["metadata"].(map[string]any)
	return meta["uid"] == want
}

// uidConflict answers like the API server when a UID precondition fails.
func uidConflict(w http.ResponseWriter) {
	w.WriteHeader(http.StatusConflict)
	_, _ = io.WriteString(w, `{"kind":"Status","status":"Failure","reason":"Conflict","message":"Precondition failed: UID in precondition does not match","code":409}`)
}

func writeJSON(w http.ResponseWriter, v any) {
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeOLM) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var body map[string]any
	if r.Body != nil {
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &body)
	}
	p := r.URL.Path
	f.requests = append(f.requests, fakeRequest{Method: r.Method, Path: p, Query: r.URL.Query(), Body: body})
	if f.intercept != nil && f.intercept(f, w, r) {
		return
	}
	if f.argo != nil && f.argo.serve(w, r) {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if n := f.failDelete[p]; r.Method == http.MethodDelete && n > 0 {
		f.failDelete[p] = n - 1
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"kind":"Status","status":"Failure","message":"denied","code":403}`)
		return
	}
	if obj, ok := f.admission[p]; ok && r.Method == http.MethodGet {
		writeJSON(w, obj)
		return
	}
	last := p[strings.LastIndex(p, "/")+1:]
	switch {
	case p == "/apis/config.openshift.io/v1/infrastructures/cluster":
		if f.topology == "" {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"kind":"Status","status":"Failure","reason":"Forbidden","code":403}`)
			return
		}
		_, _ = fmt.Fprintf(w, `{"status":{"controlPlaneTopology":%q}}`, f.topology)
	case strings.HasSuffix(p, "/secrets/additional-pull-secret"):
		_, _ = io.WriteString(w, pullSecretBody(f.t, map[string]any{"quay.io/rhoai": map[string]any{"auth": exampleAuth("user:pass")}}))
	case strings.HasSuffix(p, "/imagedigestmirrorsets"):
		_, _ = fmt.Fprintf(w, `{"items":[{"metadata":{"name":"rhoai-mirror"},"spec":{"imageDigestMirrors":[{"source":%q}]}}]}`, rhoai.IDMSSource)
	case p == "/api/v1/namespaces/"+rhoai.SubNS:
		_, _ = io.WriteString(w, `{"metadata":{"name":"`+rhoai.SubNS+`"}}`)
	case strings.HasSuffix(p, "/operatorgroups"):
		writeJSON(w, map[string]any{"items": f.operatorGrps})
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
		if source == rhoai.CatalogName && f.mainChannels != nil {
			channels = *f.mainChannels
		}
		if source == catalog.StableSource() && f.stableChans != "" {
			channels = f.stableChans
		}
		_, _ = fmt.Fprintf(w, `{"items":[{"metadata":{"name":%q},"status":{"packageName":%q,"catalogSource":%q,"catalogSourceNamespace":%q,"defaultChannel":"stable-3.x","channels":%s}}]}`, rhoai.SubName, rhoai.SubName, source, rhoai.CatalogNS, channels)
	case strings.HasSuffix(p, "/namespaces/"+rhoai.CatalogNS+"/pods") && f.gitOpsPods && r.URL.Query().Get("labelSelector") == "olm.catalogSource="+rhoai.CatalogName:
		image := ""
		if f.catalog != nil {
			image, _ = f.catalog["spec"].(map[string]any)["image"].(string)
		}
		_, _ = fmt.Fprintf(w, `{"items":[{"metadata":{"name":"rhoai-catalog-dev-abc"},"spec":{"containers":[{"image":%q}]},"status":{"conditions":[{"type":"Ready","status":"True"}]}}]}`, image)
	case strings.HasSuffix(p, "/namespaces/"+rhoai.CatalogNS+"/pods"):
		_, _ = io.WriteString(w, f.catalogPods)
	case strings.HasSuffix(p, "/subscriptions/"+rhoai.SubName):
		f.serveSubscription(w, r, body)
	case strings.HasSuffix(p, "/clusterserviceversions"):
		var items []any
		for _, csv := range f.csvs {
			items = append(items, csv)
		}
		writeJSON(w, map[string]any{"items": items})
	case strings.Contains(p, "/clusterserviceversions/"):
		csv, ok := f.csvs[last]
		switch {
		case !ok:
			notFound(w)
		case r.Method == http.MethodDelete && !uidPreconditionMet(body, csv):
			uidConflict(w)
		case r.Method == http.MethodDelete && f.stuckCSVs[last]:
			csv["metadata"].(map[string]any)["deletionTimestamp"] = "2026-01-01T00:00:00Z"
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
			ip["spec"].(map[string]any)["approved"] = true
			if f.onApprove != nil {
				f.onApprove(f, last)
			}
			_, _ = io.WriteString(w, `{}`)
		default:
			writeJSON(w, ip)
		}
	case f.dashboardOp != "" && r.Method == http.MethodGet && dashboardModuleDiscoveryRoutes(fixtureAppsNS)[p] != "":
		// A dashboard module whose operator is dashboardOp.
		_, _ = io.WriteString(w, dashboardModuleDiscoveryRoutes(fixtureAppsNS)[p])
	case f.dashboardOp != "" && p == "/apis/apps/v1/namespaces/"+fixtureAppsNS+"/deployments":
		var op dashdev.Deployment
		_ = json.Unmarshal([]byte(f.dashboardOp), &op)
		op.Metadata.Name = fixtureOperatorName
		writeJSON(w, map[string]any{"items": []any{dashboardModuleOperatorListItem(op)}})
	case p == rhoai.NamespacedPath("apps/v1", "deployments", fixtureAppsNS, fixtureOperatorName):
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
	case strings.HasSuffix(p, "/configmaps/"+record.SnapshotConfigMapName) && r.Method == http.MethodPatch && r.URL.Query().Get("fieldManager") == record.SubscriptionSnapshotManager:
		data, _ := body["data"].(map[string]any)
		f.recordedSub, _ = data[record.SubscriptionSnapshotKey].(string)
		_, _ = io.WriteString(w, `{}`)
	case strings.HasSuffix(p, "/configmaps/"+record.SnapshotConfigMapName) && r.Method == http.MethodGet:
		writeJSON(w, map[string]any{"data": map[string]any{record.SubscriptionSnapshotKey: f.recordedSub}})
	case strings.Contains(p, "/configmaps/") && r.Method != http.MethodGet:
		_, _ = io.WriteString(w, `{}`)
	case p == "/apis/apiextensions.k8s.io/v1/customresourcedefinitions":
		if f.crds == "" {
			notFound(w)
			return
		}
		_, _ = io.WriteString(w, f.crds)
	case strings.HasSuffix(p, "/installplans"):
		var items []any
		for name, ip := range f.installPlans {
			meta, _ := ip["metadata"].(map[string]any)
			if meta == nil {
				meta = map[string]any{}
			}
			meta["name"] = name
			items = append(items, map[string]any{"metadata": meta, "spec": ip["spec"]})
		}
		writeJSON(w, map[string]any{"items": items})
	default:
		notFound(w)
	}
}

func (f *fakeOLM) serveCatalog(w http.ResponseWriter, r *http.Request, name string, body map[string]any) {
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
			// An apply keeps annotations another manager set (Argo CD's
			// tracking annotation).
			old, _ := f.catalog["metadata"].(map[string]any)
			if ann := old["annotations"]; ann != nil {
				meta, _ := body["metadata"].(map[string]any)
				if meta == nil {
					meta = map[string]any{}
					body["metadata"] = meta
				}
				if meta["annotations"] == nil {
					meta["annotations"] = ann
				}
			}
			// The API server's answer: a catalog made now gets a new UID,
			// and every applied spec a new generation.
			meta, _ := body["metadata"].(map[string]any)
			if meta == nil {
				meta = map[string]any{}
				body["metadata"] = meta
			}
			f.catalogRV++
			uid, _ := old["uid"].(string)
			if f.catalog == nil || uid == "" {
				uid = fmt.Sprintf("uid-catalog-%d", f.catalogRV)
			}
			meta["uid"], meta["generation"] = uid, f.catalogRV
			f.catalog = body
			out, _ := json.Marshal(body)
			_, _ = w.Write(out)
			return
		}
		_, _ = io.WriteString(w, `{}`)
	default:
		if !verify && f.catalog == nil {
			notFound(w)
			return
		}
		image, state := "", "READY"
		meta := map[string]any{"name": name}
		if !verify {
			image, _ = f.catalog["spec"].(map[string]any)["image"].(string)
			if m, _ := f.catalog["metadata"].(map[string]any); m != nil {
				maps.Copy(meta, m)
			}
			meta["resourceVersion"] = fmt.Sprint(f.catalogRV)
		} else if f.verifyState != "" {
			state = f.verifyState
		}
		writeJSON(w, map[string]any{"metadata": meta, "spec": map[string]any{"image": image},
			"status": map[string]any{"connectionState": map[string]any{"lastObservedState": state}}})
	}
}

func (f *fakeOLM) serveSubscription(w http.ResponseWriter, r *http.Request, body map[string]any) {
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
		spec, _ := body["spec"].(map[string]any)
		// An apply keeps the UID of an existing Subscription; a new one
		// gets a new UID.
		uid := ""
		if f.sub != nil {
			uid, _ = f.sub["metadata"].(map[string]any)["uid"].(string)
		}
		if uid == "" {
			f.csvSeq++
			uid = fmt.Sprintf("uid-sub-%d", f.csvSeq)
		}
		meta := map[string]any{"name": rhoai.SubName, "uid": uid}
		bodyMeta, _ := body["metadata"].(map[string]any)
		for _, k := range []string{"annotations", "labels"} {
			if bodyMeta[k] != nil {
				meta[k] = bodyMeta[k]
			}
		}
		f.sub = map[string]any{"metadata": meta, "spec": spec, "status": map[string]any{}}
		// The API server answers an apply with the resulting object
		// (apiserver handlers/patch.go), before anything else changes it.
		applied, _ := json.Marshal(f.sub)
		if f.onSubscribe != nil {
			f.onSubscribe(f, spec)
		}
		_, _ = w.Write(applied)
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
func (f *fakeOLM) subscriptionApplies() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]any
	for _, r := range f.requests {
		if r.Method == http.MethodPatch && strings.HasSuffix(r.Path, "/subscriptions/"+rhoai.SubName) {
			spec, _ := r.Body["spec"].(map[string]any)
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
	// No component API: the cluster has no dashboard module (legacy flow).
	if p := r.URL.Path; moduleGroupPath(p) {
		notFound(w)
		return true
	}
	return false
}

// moduleGroupPath: the path is a module API group's discovery document or
// below it (both groups, component_cleanup.go).
func moduleGroupPath(p string) bool {
	for _, g := range rhoai.ModuleAPIGroups {
		if p == "/apis/"+g || strings.HasPrefix(p, "/apis/"+g+"/") {
			return true
		}
	}
	return false
}
