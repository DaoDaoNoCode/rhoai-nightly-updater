package cluster

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/registry"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Tests for mirroring the quay.io/rhoai credential into the internal registry
// (field issue F3: ROSA HCP workbenches failed image pulls via the internal
// registry because it cannot read kube-system/additional-pull-secret).

const (
	copyAppsNS   = "custom-apps"              // DSCInitialization picks the apps namespace; not the shipped default
	copyNewAuth  = "bmV3dXNlcjpuZXctczNjcjN0" // decodes to newuser:new-s3cr3t
	copyOldAuth  = "b2xkdXNlcjpvbGQtczNjcjN0" // decodes to olduser:old-s3cr3t
	copyPassword = "new-s3cr3t"
)

var (
	copyPath     = "/api/v1/namespaces/" + copyAppsNS + "/secrets/" + registry.RegistryPullCopyName
	copyListPath = "/api/v1/namespaces/" + copyAppsNS + "/secrets"
	pullPath     = "/api/v1/namespaces/kube-system/secrets/additional-pull-secret"
)

// secretStore backs Secret resources on fakeAPI—namespace list POST, per-object
// GET, PUT guarded by resourceVersion, and PATCH for the pull secret's
// server-side apply path.
type secretStore struct {
	mu   sync.Mutex
	objs map[string]map[string]any
	rv   int
}

func (s *secretStore) put(ns, name string, obj map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rv++
	meta, _ := obj["metadata"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
		obj["metadata"] = meta
	}
	meta["name"], meta["namespace"], meta["resourceVersion"], meta["uid"] = name, ns, strconv.Itoa(s.rv), "uid-"+name
	s.objs["/api/v1/namespaces/"+ns+"/secrets/"+name] = obj
}

func (s *secretStore) get(path string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.objs[path]
}

func (s *secretStore) serve(f *fakeAPI, ns string, names ...string) {
	f.handle("POST", "/api/v1/namespaces/"+ns+"/secrets", func(_ *http.Request, body []byte) (int, string) {
		var obj map[string]any
		_ = json.Unmarshal(body, &obj)
		name, _ := obj["metadata"].(map[string]any)["name"].(string)
		if s.get("/api/v1/namespaces/"+ns+"/secrets/"+name) != nil {
			return http.StatusConflict, `{"kind":"Status","code":409,"reason":"AlreadyExists"}`
		}
		s.put(ns, name, obj)
		b, _ := json.Marshal(s.get("/api/v1/namespaces/" + ns + "/secrets/" + name))
		return http.StatusCreated, string(b)
	})
	for _, name := range names {
		path := "/api/v1/namespaces/" + ns + "/secrets/" + name
		f.handle("GET", path, func(*http.Request, []byte) (int, string) {
			obj := s.get(path)
			if obj == nil {
				return http.StatusNotFound, k8sNotFound
			}
			b, _ := json.Marshal(obj)
			return http.StatusOK, string(b)
		})
		write := func(r *http.Request, body []byte) (int, string) {
			var obj map[string]any
			_ = json.Unmarshal(body, &obj)
			old := s.get(path)
			if r.Method == http.MethodPut {
				if old == nil {
					return http.StatusNotFound, k8sNotFound
				}
				if obj["metadata"].(map[string]any)["resourceVersion"] != old["metadata"].(map[string]any)["resourceVersion"] {
					return http.StatusConflict, `{"kind":"Status","code":409,"reason":"Conflict"}`
				}
			}
			s.put(ns, name, obj)
			b, _ := json.Marshal(s.get(path))
			return http.StatusOK, string(b)
		}
		f.handle("PUT", path, write)
		f.handle("PATCH", path, write)
	}
}

func dockerSecret(auths map[string]any, labels map[string]any) map[string]any {
	cfg, _ := json.Marshal(map[string]any{"auths": auths})
	meta := map[string]any{}
	if labels != nil {
		meta["labels"] = labels
	}
	return map[string]any{"apiVersion": "v1", "kind": "Secret", "type": "kubernetes.io/dockerconfigjson", "metadata": meta,
		"data": map[string]any{".dockerconfigjson": base64.StdEncoding.EncodeToString(cfg)}}
}

func secretAuths(t *testing.T, obj map[string]any) map[string]any {
	t.Helper()
	data, _ := obj["data"].(map[string]any)
	raw, err := base64.StdEncoding.DecodeString(data[".dockerconfigjson"].(string))
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Auths map[string]any `json:"auths"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg.Auths
}

var toolLabel = map[string]any{kube.ManagedByLabelKey: kube.ManagedByLabelValue}

type copyCluster struct {
	f       *fakeAPI
	c       *Client
	secrets *secretStore
}

// newCopyCluster wires fakeAPI and Client for topology (External = hosted
// control plane). When withDSCI is true, DSCInitialization uses copyAppsNS;
// otherwise no DSCInitialization is present. Requests run as the interactive user.
func newCopyCluster(t *testing.T, topology string, withDSCI bool) *copyCluster {
	t.Helper()
	f, c := newFakeAPI(t)
	editTestClient(c, func(o *testClientOptions) { o.AsUser = true })
	t.Cleanup(registry.SetVerifyQuayCredentialsForTest(func(string) error { return nil }))
	f.json("GET", "/apis/config.openshift.io/v1/infrastructures/cluster", http.StatusOK, fmt.Sprintf(`{"status":{"controlPlaneTopology":%q}}`, topology))
	f.json("GET", "/apis/admissionregistration.k8s.io/v1/validatingwebhookconfigurations", http.StatusOK, `{"items":[]}`)
	if withDSCI {
		f.json("GET", "/apis/dscinitialization.opendatahub.io/v2/dscinitializations", http.StatusOK,
			fmt.Sprintf(`{"items":[{"metadata":{"name":"default-dsci"},"spec":{"applicationsNamespace":%q}}]}`, copyAppsNS))
	}
	s := &secretStore{objs: map[string]map[string]any{}}
	s.serve(f, "kube-system", "additional-pull-secret")
	s.serve(f, copyAppsNS, registry.RegistryPullCopyName)
	return &copyCluster{f: f, c: c, secrets: s}
}

// assertNoToken errors if out contains any variant of the test credentials.
func assertNoToken(t *testing.T, out string) {
	t.Helper()
	for _, v := range []string{copyNewAuth, copyOldAuth, copyPassword, "old-s3cr3t", "newuser:new-s3cr3t"} {
		if strings.Contains(out, v) {
			t.Errorf("the output shows a credential (%q):\n%s", v, out)
		}
	}
}

func resultText(res *types.OperationResponse) string {
	return res.Message + "\n" + strings.Join(res.Logs, "\n")
}

func TestRegistryPullCopy_HostedSetupWritesBoth(t *testing.T) {
	k := newCopyCluster(t, "External", true)
	res, err := CreatePullSecret(k.c, copyNewAuth)
	if err != nil || !res.Success {
		t.Fatalf("CreatePullSecret = %+v, %v", res, err)
	}
	if got := k.f.writes(); strings.Join(got, ",") != "POST "+copyListPath+",POST /api/v1/namespaces/kube-system/secrets" {
		t.Fatalf("writes = %v, want the pull secret and the registry copy created", got)
	}
	cp := k.secrets.get(copyPath)
	if cp["type"] != "kubernetes.io/dockerconfigjson" {
		t.Errorf("copy type = %v", cp["type"])
	}
	if labels, _ := cp["metadata"].(map[string]any)["labels"].(map[string]any); labels[kube.ManagedByLabelKey] != kube.ManagedByLabelValue {
		t.Errorf("the copy lacks the tool's label: %v", cp["metadata"])
	}
	auths := secretAuths(t, cp)
	if len(auths) != 1 || registry.AuthFromEntry(auths["quay.io/rhoai"]) != copyNewAuth {
		t.Errorf("the copy must hold only the quay.io/rhoai entry with the token: %v", auths)
	}
	out := resultText(res)
	if !strings.Contains(out, "copied the quay.io/rhoai token to Secret "+copyAppsNS+"/"+registry.RegistryPullCopyName) {
		t.Errorf("the logs do not say the token was copied:\n%s", out)
	}
	if res.Message != "Pull secret configured successfully." {
		t.Errorf("message = %q", res.Message)
	}
	assertNoToken(t, out)
}

func TestRegistryPullCopy_ReplacingTheTokenUpdatesBoth(t *testing.T) {
	k := newCopyCluster(t, "External", true)
	k.secrets.put("kube-system", "additional-pull-secret", dockerSecret(map[string]any{
		"quay.io/rhoai":        map[string]any{"auth": copyOldAuth},
		"registry.example.com": map[string]any{"auth": exampleAuth("someone:else")},
	}, nil))
	k.secrets.put(copyAppsNS, registry.RegistryPullCopyName, dockerSecret(map[string]any{"quay.io/rhoai": map[string]any{"auth": copyOldAuth}},
		map[string]any{kube.ManagedByLabelKey: kube.ManagedByLabelValue, "team": "kept"}))

	res, err := CreatePullSecret(k.c, copyNewAuth)
	if err != nil || !res.Success {
		t.Fatalf("CreatePullSecret = %+v, %v", res, err)
	}
	if got := k.f.writes(); strings.Join(got, ",") != "PATCH "+pullPath+",PUT "+copyPath {
		t.Fatalf("writes = %v, want the pull secret applied and the copy updated", got)
	}
	cp := k.secrets.get(copyPath)
	if auths := secretAuths(t, cp); len(auths) != 1 || registry.AuthFromEntry(auths["quay.io/rhoai"]) != copyNewAuth {
		t.Errorf("the copy was not updated to the new token: %v", auths)
	}
	if labels := cp["metadata"].(map[string]any)["labels"].(map[string]any); labels["team"] != "kept" || labels[kube.ManagedByLabelKey] != kube.ManagedByLabelValue {
		t.Errorf("the copy's labels were not kept: %v", labels)
	}
	put := k.f.requests("PUT", copyPath)
	if len(put) != 1 || !strings.Contains(put[0].Body, `"resourceVersion":"2"`) {
		t.Errorf("the update must carry the resourceVersion it read: %v", put)
	}
	out := resultText(res)
	if !strings.Contains(out, "updated Secret "+copyAppsNS+"/"+registry.RegistryPullCopyName) {
		t.Errorf("the logs do not say the copy was updated:\n%s", out)
	}
	assertNoToken(t, out)

	// A duplicate save must not trigger another write to the registry copy.
	before := len(k.f.requests("PUT", copyPath))
	if res, err := CreatePullSecret(k.c, copyNewAuth); err != nil || !res.Success || !strings.Contains(resultText(res), "already holds the token") {
		t.Fatalf("second save = %+v, %v", res, err)
	}
	if len(k.f.requests("PUT", copyPath)) != before {
		t.Error("a copy that already holds the token was written again")
	}
}

func TestRegistryPullCopy_ForeignSecretIsNeverTouched(t *testing.T) {
	for _, tc := range []struct {
		name   string
		labels map[string]any
	}{{"no labels", nil}, {"another manager", map[string]any{kube.ManagedByLabelKey: "someone-else"}}} {
		t.Run(tc.name, func(t *testing.T) {
			k := newCopyCluster(t, "External", true)
			k.secrets.put(copyAppsNS, registry.RegistryPullCopyName, dockerSecret(map[string]any{"quay.io/rhoai": map[string]any{"auth": copyOldAuth}}, tc.labels))
			res, err := CreatePullSecret(k.c, copyNewAuth)
			if err != nil || !res.Success {
				t.Fatalf("the pull secret itself must still be saved: %+v, %v", res, err)
			}
			for _, w := range k.f.writes() {
				if strings.Contains(w, copyAppsNS) {
					t.Errorf("a Secret the tool did not create was written: %s", w)
				}
			}
			if auths := secretAuths(t, k.secrets.get(copyPath)); registry.AuthFromEntry(auths["quay.io/rhoai"]) != copyOldAuth {
				t.Errorf("the foreign Secret changed: %v", auths)
			}
			want := "Secret " + copyAppsNS + "/" + registry.RegistryPullCopyName + " exists and was not created by this updater"
			if !strings.Contains(res.Message, want) || !strings.Contains(res.Message, "so it was not changed") {
				t.Errorf("the message does not refuse plainly: %q", res.Message)
			}
			assertNoToken(t, resultText(res))

			// EnsureRegistryPullCopyFromPullSecret must also leave the Secret alone.
			logs := registry.EnsureRegistryPullCopyFromPullSecret(k.c)
			if !strings.Contains(strings.Join(logs, "\n"), want) {
				t.Errorf("logs = %v", logs)
			}
			for _, w := range k.f.writes() {
				if strings.Contains(w, copyAppsNS) {
					t.Errorf("a Secret the tool did not create was written: %s", w)
				}
			}
		})
	}
}

func TestRegistryPullCopy_SelfManagedIsUnchanged(t *testing.T) {
	k := newCopyCluster(t, "HighlyAvailable", true)
	res, err := CreatePullSecret(k.c, copyNewAuth)
	if err != nil || !res.Success || res.Message != "Pull secret configured successfully." {
		t.Fatalf("CreatePullSecret = %+v, %v", res, err)
	}
	if got := k.f.writes(); strings.Join(got, ",") != "POST /api/v1/namespaces/kube-system/secrets" {
		t.Fatalf("writes = %v, want only the pull secret", got)
	}
	if n := len(k.f.requests("GET", copyPath)); n != 0 {
		t.Errorf("the registry copy was read on a self-managed cluster (%d times)", n)
	}
	if strings.Contains(resultText(res), "Workbench images") {
		t.Errorf("a self-managed cluster gets no word about the copy:\n%s", resultText(res))
	}
	if logs := registry.EnsureRegistryPullCopyFromPullSecret(k.c); len(logs) != 0 {
		t.Errorf("the install flows say something on a self-managed cluster: %v", logs)
	}
}

func TestRegistryPullCopy_UnknownControlPlaneWritesNothing(t *testing.T) {
	k := newCopyCluster(t, "External", true)
	k.f.status("GET", "/apis/config.openshift.io/v1/infrastructures/cluster", http.StatusForbidden, "Forbidden")
	res, err := CreatePullSecret(k.c, copyNewAuth)
	if err != nil || !res.Success {
		t.Fatalf("CreatePullSecret = %+v, %v", res, err)
	}
	if got := k.f.writes(); strings.Join(got, ",") != "POST /api/v1/namespaces/kube-system/secrets" {
		t.Fatalf("writes = %v, want only the pull secret", got)
	}
	if !strings.Contains(res.Message, "could not read whether this cluster has a hosted control plane") {
		t.Errorf("message = %q", res.Message)
	}
}

func TestRegistryPullCopy_BeforeRHOAIIsInstalled(t *testing.T) {
	k := newCopyCluster(t, "External", false)
	res, err := CreatePullSecret(k.c, copyNewAuth)
	if err != nil || !res.Success || res.Message != "Pull secret configured successfully." {
		t.Fatalf("CreatePullSecret = %+v, %v", res, err)
	}
	if got := k.f.writes(); strings.Join(got, ",") != "POST /api/v1/namespaces/kube-system/secrets" {
		t.Fatalf("writes = %v, want only the pull secret", got)
	}
	if !strings.Contains(resultText(res), "RHOAI is not installed yet") {
		t.Errorf("the logs do not say when the copy is made:\n%s", resultText(res))
	}

	// After DSCInitialization exposes applicationsNamespace, install/DSC/workbench
	// code paths should populate the copy from additional-pull-secret.
	k.f.json("GET", "/apis/dscinitialization.opendatahub.io/v2/dscinitializations", http.StatusOK,
		fmt.Sprintf(`{"items":[{"metadata":{"name":"default-dsci"},"spec":{"applicationsNamespace":%q}}]}`, copyAppsNS))
	logs := registry.EnsureRegistryPullCopyFromPullSecret(k.c)
	if auths := secretAuths(t, k.secrets.get(copyPath)); registry.AuthFromEntry(auths["quay.io/rhoai"]) != copyNewAuth {
		t.Fatalf("the copy was not made from the pull secret: %v (logs %v)", auths, logs)
	}
	assertNoToken(t, strings.Join(logs, "\n"))
}

func TestRegistryPullCopy_ErrorsAreRedacted(t *testing.T) {
	k := newCopyCluster(t, "External", true)
	k.f.handle("POST", copyListPath, func(_ *http.Request, body []byte) (int, string) {
		msg, _ := json.Marshal(map[string]any{"kind": "Status", "status": "Failure", "code": 422, "reason": "Invalid", "message": "admission webhook denied " + string(body) + " " + copyNewAuth + " newuser:new-s3cr3t"})
		return http.StatusUnprocessableEntity, string(msg)
	})
	res, err := CreatePullSecret(k.c, copyNewAuth)
	if err != nil || !res.Success {
		t.Fatalf("CreatePullSecret = %+v, %v", res, err)
	}
	if !strings.Contains(res.Message, "could not create Secret "+copyAppsNS+"/"+registry.RegistryPullCopyName) || !strings.Contains(res.Message, kube.RedactedText) {
		t.Errorf("message = %q", res.Message)
	}
	assertNoToken(t, resultText(res))
}

func TestReadRegistryPullCopy(t *testing.T) {
	k := newCopyCluster(t, "External", true)
	if got := registry.ReadRegistryPullCopy(k.c, copyNewAuth); got == nil || got.Exists || got.Namespace != copyAppsNS {
		t.Fatalf("missing copy = %+v", got)
	}
	k.secrets.put(copyAppsNS, registry.RegistryPullCopyName, dockerSecret(map[string]any{"quay.io/rhoai": map[string]any{"auth": copyOldAuth}}, toolLabel))
	if got := registry.ReadRegistryPullCopy(k.c, copyNewAuth); !got.Exists || !got.Ours || got.Current {
		t.Errorf("an older token = %+v", got)
	}
	if got := registry.ReadRegistryPullCopy(k.c, copyOldAuth); !got.Exists || !got.Ours || !got.Current {
		t.Errorf("the current token = %+v", got)
	}
	k.secrets.put(copyAppsNS, registry.RegistryPullCopyName, dockerSecret(map[string]any{"quay.io/rhoai": map[string]any{"auth": copyOldAuth}}, nil))
	if got := registry.ReadRegistryPullCopy(k.c, copyOldAuth); !got.Exists || got.Ours {
		t.Errorf("a foreign Secret = %+v", got)
	}
	b, _ := json.Marshal(registry.ReadRegistryPullCopy(k.c, copyOldAuth))
	assertNoToken(t, string(b))

	// ReadRegistryPullCopy must not run when the client uses the ServiceAccount token.
	editTestClient(k.c, func(o *testClientOptions) { o.AsUser = false })
	before := len(k.f.requests("GET", copyPath))
	if got := registry.ReadRegistryPullCopy(k.c, copyOldAuth); got != nil {
		t.Errorf("the ServiceAccount's client read the copy: %+v", got)
	}
	if len(k.f.requests("GET", copyPath)) != before {
		t.Error("a request was sent with the ServiceAccount's token")
	}

	// Without DSCInitialization there is no applications namespace for the copy.
	k2 := newCopyCluster(t, "External", false)
	if got := registry.ReadRegistryPullCopy(k2.c, copyOldAuth); got == nil || !got.NoApplicationsNamespace || got.Exists {
		t.Errorf("without RHOAI = %+v", got)
	}
}

// getStatus should surface the registry copy on hosted CP only, using the
// interactive user's Client—not the ServiceAccount-backed one.
func TestGetStatus_RegistryCopyOnlyOnHCPWithTheUsersClient(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cp       string
		withUser bool
		want     bool
	}{
		{"hosted, signed-in user", rhoai.ControlPlaneHosted, true, true},
		{"hosted, no user client", rhoai.ControlPlaneHosted, false, false},
		{"self-managed", rhoai.ControlPlaneSelfManaged, true, false},
		{"unknown", "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newStatusAPI(t, &statusAPI{csvList: `{"items":[]}`})
			if tc.withUser {
				editTestClient(c, func(o *testClientOptions) {
					o.SignedInUser = newTestClient(testClientOptions{BaseURL: o.BaseURL, HTTPClient: o.HTTPClient, Ctx: o.Ctx, Token: "user", AsUser: true})
				})
			}
			var calledWith *Client
			status, err := getStatus(c, statusRegistryReads{
				pullSecret: func(*Client) (types.PullSecretInfo, string, error) {
					return types.PullSecretInfo{Exists: true, Valid: true}, copyNewAuth, nil
				},
				controlPlane: func(*Client) string { return tc.cp },
				registryCopy: func(u *Client, _ string) *types.RegistryPullCopyInfo {
					calledWith = u
					return &types.RegistryPullCopyInfo{Name: registry.RegistryPullCopyName, Exists: true}
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := status.PullSecret.Registry != nil; got != tc.want {
				t.Fatalf("registry copy shown = %v, want %v", got, tc.want)
			}
			if calledWith != nil && !calledWith.ActsAsUser() {
				t.Fatal("the copy was read with the ServiceAccount's client")
			}
		})
	}
}
