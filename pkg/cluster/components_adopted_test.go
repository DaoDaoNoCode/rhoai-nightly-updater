package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// rsServer serves the ReplicaSet list of namespace ns, honouring the
// metadata.name field selector, and counts the requests.
type rsServer struct {
	mu      sync.Mutex
	sets    []map[string]interface{}
	calls   int
	queries []string
	fail    bool
}

func (s *rsServer) client(t *testing.T) *Client {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if r.URL.Path != "/apis/apps/v1/namespaces/ns/replicasets" {
			http.NotFound(w, r)
			return
		}
		s.calls++
		s.queries = append(s.queries, r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		if s.fail {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"kind":"Status","code":403,"message":"forbidden"}`))
			return
		}
		name := strings.TrimPrefix(r.URL.Query().Get("fieldSelector"), "metadata.name=")
		items := []interface{}{}
		for _, rs := range s.sets {
			if rs["metadata"].(map[string]interface{})["name"] == name {
				items = append(items, rs)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"items": items})
	}))
	t.Cleanup(srv.Close)
	old := replicaSetOwnerCache
	replicaSetOwnerCache = newLRU[string, string](512)
	t.Cleanup(func() { replicaSetOwnerCache = old })
	return &Client{baseURL: srv.URL, token: "t", httpClient: srv.Client(), ctx: context.Background()}
}

func replicaSet(name, uid, deploymentUID string) map[string]interface{} {
	refs := []interface{}{}
	if deploymentUID != "" {
		refs = append(refs, map[string]interface{}{"apiVersion": "apps/v1", "kind": "Deployment", "name": "d", "uid": deploymentUID, "controller": true})
	}
	return map[string]interface{}{"metadata": map[string]interface{}{"name": name, "uid": uid, "ownerReferences": refs}}
}

func adoptedPod(name, rs, rsUID string, labels map[string]string) types.PodInfo {
	return types.PodInfo{Name: name, Labels: labels, OwnerKind: "ReplicaSet", OwnerName: rs, OwnerUID: rsUID}
}

var cmLabels = map[string]string{"control-plane": "controller-manager"}

func trustyDeployment() types.DeploymentInfo {
	return types.DeploymentInfo{Name: "trustyai-operator-module-controller-manager", Namespace: "ns", UID: "dep-trusty", MatchLabels: cmLabels}
}

// A Deployment adopted an existing ReplicaSet (its name does not follow
// <deployment>-<hash>): its pods are the Deployment's. A pod whose
// ReplicaSet another Deployment controls is not.
func TestOwnedByDeployment_AdoptedReplicaSet(t *testing.T) {
	s := &rsServer{sets: []map[string]interface{}{
		replicaSet("handmade-rs", "rs-adopted", "dep-trusty"),
		replicaSet("feast-operator-controller-manager-846574fbc4", "rs-feast", "dep-feast"),
		replicaSet("orphan-rs", "rs-orphan", ""),
	}}
	c := s.client(t)
	dep := trustyDeployment()
	for _, tc := range []struct {
		name string
		pod  types.PodInfo
		want bool
	}{
		{"pod of an adopted ReplicaSet", adoptedPod("handmade-rs-abcde", "handmade-rs", "rs-adopted", cmLabels), true},
		{"another operator's pod matching the broad selector", adoptedPod("feast-x", "feast-operator-controller-manager-846574fbc4", "rs-feast", cmLabels), false},
		{"pod of a ReplicaSet no Deployment controls", adoptedPod("orphan-x", "orphan-rs", "rs-orphan", cmLabels), false},
		{"a ReplicaSet recreated under the same name (other UID)", adoptedPod("handmade-rs-old", "handmade-rs", "rs-gone", cmLabels), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := podMatchesDeployment(c, tc.pod, dep); got != tc.want {
				t.Errorf("podMatchesDeployment = %v, want %v", got, tc.want)
			}
		})
	}
	for _, q := range s.queries {
		if !strings.HasPrefix(q, "fieldSelector=metadata.name%3D") {
			t.Errorf("ReplicaSet lookup without a name field selector: %q", q)
		}
	}
}

// A lookup that fails does not match the pod (fail closed) and logs why at
// debug level.
func TestOwnedByDeployment_LookupFailureFailsClosed(t *testing.T) {
	s := &rsServer{fail: true}
	c := s.client(t)
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })

	if podMatchesDeployment(c, adoptedPod("handmade-rs-abcde", "handmade-rs", "rs-adopted", cmLabels), trustyDeployment()) {
		t.Fatal("a pod whose ReplicaSet cannot be read was matched")
	}
	if log := buf.String(); !strings.Contains(log, "level=DEBUG") || !strings.Contains(log, "replicaset=handmade-rs") {
		t.Errorf("no debug log for the failed lookup: %q", log)
	}
	// A failure is not cached: the next refresh tries again.
	s.mu.Lock()
	s.fail = false
	s.sets = []map[string]interface{}{replicaSet("handmade-rs", "rs-adopted", "dep-trusty")}
	s.mu.Unlock()
	if !podMatchesDeployment(c, adoptedPod("handmade-rs-abcde", "handmade-rs", "rs-adopted", cmLabels), trustyDeployment()) {
		t.Error("the pod is not matched once its ReplicaSet can be read")
	}
}

// The live case: 8 foreign pods match one broad selector. Pods that follow
// the naming rule cost nothing; each other ReplicaSet is read once, and a
// second refresh within the TTL makes no API call.
func TestMatchPodsToDeployments_LookupCost(t *testing.T) {
	operators := []string{"feast", "mcp-lifecycle", "mlflow", "ogx", "ray", "trainer", "workbenches", "kserve"}
	s := &rsServer{}
	var pods []types.PodInfo
	for _, op := range operators {
		rs := op + "-operator-controller-manager-6b9c7"
		s.sets = append(s.sets, replicaSet(rs, "rs-"+op, "dep-"+op))
		pods = append(pods, adoptedPod(rs+"-x1", rs, "rs-"+op, map[string]string{"control-plane": "controller-manager", "pod-template-hash": "6b9c7"}))
	}
	// Two pods of one ReplicaSet share a lookup.
	pods = append(pods, adoptedPod(operators[0]+"-operator-controller-manager-6b9c7-x2", operators[0]+"-operator-controller-manager-6b9c7", "rs-"+operators[0], map[string]string{"control-plane": "controller-manager", "pod-template-hash": "6b9c7"}))
	// The Deployment's own pod, by the naming rule.
	pods = append(pods, adoptedPod("trustyai-operator-module-controller-manager-b57d5b597-x", "trustyai-operator-module-controller-manager-b57d5b597", "rs-trusty", map[string]string{"control-plane": "controller-manager", "pod-template-hash": "b57d5b597"}))
	c := s.client(t)

	refresh := func() []types.PodInfo {
		deps := []types.DeploymentInfo{trustyDeployment()}
		matchPodsToDeployments(c, pods, "ns", deps)
		return deps[0].Pods
	}
	if got := refresh(); len(got) != 1 || got[0].Name != "trustyai-operator-module-controller-manager-b57d5b597-x" {
		t.Fatalf("first refresh matched %v", got)
	}
	if s.calls != len(operators) {
		t.Errorf("first refresh: %d ReplicaSet lookups, want %d (one per foreign ReplicaSet)", s.calls, len(operators))
	}
	before := s.calls
	if got := refresh(); len(got) != 1 {
		t.Fatalf("second refresh matched %v", got)
	}
	if s.calls != before {
		t.Errorf("second refresh: %d ReplicaSet lookups, want 0", s.calls-before)
	}
}
