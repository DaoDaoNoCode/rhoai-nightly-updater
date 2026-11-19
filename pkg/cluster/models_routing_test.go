package cluster

import (
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// modelCallSim answers the "Can it be called?" requests of a test, in
// place of the model's server behind its gateway.
type modelCallSim struct {
	mu sync.Mutex
	// status and body answer every request (200 and a chat answer by
	// default); err fails it without an answer. answers, when set, are
	// used one per request first.
	status  int
	body    string
	err     error
	answers []simAnswer
	// requests are "URL token body" of each request (not the requests
	// without a token through a gateway, which are counted in probes).
	requests []string
	// open: the gateway answers a request without a token like one with
	// it (no AuthPolicy enforced); otherwise it refuses it with 401.
	open   bool
	probes int
}

type simAnswer struct {
	status int
	body   string
	err    error
}

const testModelToken = "model-own-token"

const kuadrantGroupDoc = `{"kind":"APIGroup","name":"kuadrant.io","versions":[{"groupVersion":"kuadrant.io/v1","version":"v1"},{"groupVersion":"kuadrant.io/v1beta1","version":"v1beta1"}],"preferredVersion":{"groupVersion":"kuadrant.io/v1","version":"v1"}}`

// simulateRouting gives a models cluster what routing needs: Connectivity
// Link's API and its Kuadrant CR, gateways whose status shows in lists,
// token Secrets the token controller has filled, and a model that answers.
func simulateRouting(t *testing.T, mc *modelsCluster) {
	t.Helper()
	f := mc.f
	f.putJSON("/apis/kuadrant.io/v1beta1/namespaces/kuadrant-system/kuadrants/kuadrant", `{"status":{"conditions":[{"type":"Ready","status":"True"}]}}`)
	f.fail["GET /apis/kuadrant.io/v1/kuadrants"] = 404
	if f.groups == nil {
		f.groups = map[string]string{}
	}
	f.groups["/apis/kuadrant.io"] = kuadrantGroupDoc
	f.listOnGet = map[string]bool{"gateways": true}
	prev := f.onGet
	f.onGet = func(k fakeKey, obj map[string]any) {
		if prev != nil {
			prev(k, obj)
		}
		if k.plural == "secrets" && obj["type"] == tokenSecretType {
			ensureMap(obj, "data")["token"] = base64.StdEncoding.EncodeToString([]byte(testModelToken))
		}
	}
	mc.calls = &modelCallSim{status: 200, body: `{"choices":[{"message":{"content":"Hi"}}]}`}
	saved := modelCallHTTPClient
	t.Cleanup(func() { modelCallHTTPClient = saved })
	sim := mc.calls
	modelCallHTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		data, _ := io.ReadAll(r.Body)
		sim.mu.Lock()
		defer sim.mu.Unlock()
		// The gateway's AuthPolicy refuses a call without a token; the
		// in-cluster address (no gateway) does not check.
		if r.Header.Get("Authorization") == "" && !strings.HasSuffix(r.URL.Hostname(), ".svc.cluster.local") {
			sim.probes++
			if !sim.open {
				return &http.Response{StatusCode: 401, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}, Request: r}, nil
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(sim.body)), Header: http.Header{}, Request: r}, nil
		}
		sim.requests = append(sim.requests, r.URL.String()+" "+strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")+" "+string(data))
		a := simAnswer{status: sim.status, body: sim.body, err: sim.err}
		if len(sim.answers) > 0 {
			a, sim.answers = sim.answers[0], sim.answers[1:]
		}
		if a.err != nil {
			return nil, a.err
		}
		return &http.Response{StatusCode: a.status, Body: io.NopCloser(strings.NewReader(a.body)), Header: http.Header{}, Request: r}, nil
	})}
}

// withoutKuadrant takes Connectivity Link off the cluster.
func withoutKuadrant(mc *modelsCluster) {
	delete(mc.f.groups, "/apis/kuadrant.io")
	delete(mc.f.objects, fakeKey{gv: "kuadrant.io/v1beta1", ns: "kuadrant-system", plural: "kuadrants", name: "kuadrant"})
}

func (s *modelCallSim) sent() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

var errSimDNS = errors.New("dial tcp: lookup inference.apps.example.com: no such host")
