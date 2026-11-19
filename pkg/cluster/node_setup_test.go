package cluster

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const (
	nsIDMSPath       = "/apis/config.openshift.io/v1/imagedigestmirrorsets/" + MirrorName
	nsIDMSList       = "/apis/config.openshift.io/v1/imagedigestmirrorsets"
	nsPullSecretPath = "/api/v1/namespaces/openshift-config/secrets/pull-secret"
	nsUpdaterPath    = "/api/v1/namespaces/kube-system/secrets/additional-pull-secret"
	nsSREWebhookPath = "/apis/admissionregistration.k8s.io/v1/validatingwebhookconfigurations/sre-imagecontentpolicies-validation"
	nsPoolsPath      = "/apis/machineconfiguration.openshift.io/v1/machineconfigpools"
)

// Credentials of the fixtures: the updater's token (unpadded, as a user may
// paste it), the cluster's own logins and a token already in the cluster
// pull secret.
var (
	nsToken       = base64.RawStdEncoding.EncodeToString([]byte("rhoai+robot:upd4ter-s3cret"))
	nsTokenPadded = base64.StdEncoding.EncodeToString([]byte("rhoai+robot:upd4ter-s3cret"))
	nsCloudAuth   = base64.StdEncoding.EncodeToString([]byte("cloud-user:cl0ud-s3cret"))
	nsRedHatAuth  = base64.StdEncoding.EncodeToString([]byte("rh-user:r3dhat-s3cret"))
	nsOldRHOAI    = base64.StdEncoding.EncodeToString([]byte("old-robot:0ld-s3cret"))
)

func nsDockerConfig(t *testing.T, cfg map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// clusterLogins is a cluster pull secret's .auths: two of the cluster's
// own logins, and extra entries.
func clusterLogins(extra map[string]any) map[string]any {
	auths := map[string]any{
		"cloud.openshift.com": map[string]any{"auth": nsCloudAuth, "email": "owner@example.com"},
		"registry.redhat.io":  map[string]any{"auth": nsRedHatAuth},
	}
	for k, v := range extra {
		auths[k] = v
	}
	return auths
}

// nodeSetupFake is a cluster with the given topology ("" = Infrastructure
// absent), Red Hat SRE's webhooks when managed, the updater's token in
// additional-pull-secret, and a cluster pull secret with two logins.
func nodeSetupFake(t *testing.T, topology string, managed bool) (*resourceFake, *Client) {
	t.Helper()
	f, c := newResourceFake(t)
	if topology != "" {
		f.putJSON(infrastructurePath, `{"status":{"controlPlaneTopology":"`+topology+`"}}`)
	}
	if managed {
		f.putJSON(nsSREWebhookPath, `{"webhooks":[{"name":"imagecontentpolicies-validation.managed.openshift.io"}]}`)
	}
	f.put(nsUpdaterPath, map[string]any{"type": "kubernetes.io/dockerconfigjson", "data": map[string]any{
		".dockerconfigjson": nsDockerConfig(t, map[string]any{"auths": map[string]any{"quay.io/rhoai": map[string]any{"auth": nsToken}}}),
	}})
	f.put(nsPullSecretPath, map[string]any{"type": "kubernetes.io/dockerconfigjson",
		"metadata": map[string]any{"resourceVersion": "42"},
		"data":     map[string]any{".dockerconfigjson": nsDockerConfig(t, map[string]any{"auths": clusterLogins(nil)})},
	})
	orig := verifyQuayCredentials
	verifyQuayCredentials = func(string) error { return nil }
	t.Cleanup(func() { verifyQuayCredentials = orig })
	return f, c
}

// pullSecretAuths decodes the stored cluster pull secret's .auths.
func pullSecretAuths(t *testing.T, f *resourceFake) map[string]any {
	t.Helper()
	obj := f.get(nsPullSecretPath)
	data, _ := obj["data"].(map[string]any)
	raw, err := base64.StdEncoding.DecodeString(data[".dockerconfigjson"].(string))
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	auths, _ := cfg["auths"].(map[string]any)
	return auths
}

func runNodeSetupCollect(t *testing.T, c *Client, req types.NodeSetupRequest) (*types.OperationResponse, []UpdateStepEvent) {
	t.Helper()
	var steps []UpdateStepEvent
	res, err := RunNodeSetup(c, req, func(e UpdateStepEvent) { steps = append(steps, e) })
	if err != nil {
		t.Fatal(err)
	}
	return res, steps
}

// writes are the requests that would change the cluster (not the
// updater's own activity ConfigMap, not dry runs).
func writes(f *resourceFake) []string { return f.mutations() }

func TestMirrorCreateOnSelfManaged(t *testing.T) {
	f, c := nodeSetupFake(t, "HighlyAvailable", false)
	plan, err := PlanNodeSetup(c, NodeSetupMirrorCreate)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Ready || plan.ControlPlane != ControlPlaneSelfManaged || len(plan.Changes) != 1 || !strings.Contains(plan.NodeImpact, "without draining or rebooting") || !strings.Contains(plan.Undo, "oc delete imagedigestmirrorset rhoai-quay-mirror") {
		t.Fatalf("plan %+v", plan)
	}
	if w := writes(f); len(w) != 0 {
		t.Fatalf("the plan wrote %v", w)
	}
	if len(f.callsTo(http.MethodPost, nsIDMSList)) != 1 {
		t.Fatal("the plan made no server-side dry run of the create")
	}

	res, steps := runNodeSetupCollect(t, c, types.NodeSetupRequest{Action: NodeSetupMirrorCreate})
	if !res.Success || !strings.Contains(res.Message, "Created ImageDigestMirrorSet rhoai-quay-mirror") {
		t.Fatalf("result %+v", res)
	}
	var ids []string
	for _, s := range steps {
		if s.Status == "success" {
			ids = append(ids, s.Step)
		}
	}
	if !slices.Equal(ids, []string{"check", "dry_run", "apply", "verify"}) {
		t.Fatalf("steps %v", ids)
	}
	obj := f.get(nsIDMSPath)
	spec, _ := json.Marshal(obj["spec"])
	if string(spec) != `{"imageDigestMirrors":[{"mirrors":["quay.io/rhoai"],"source":"registry.redhat.io/rhoai"}]}` {
		t.Fatalf("spec %s", spec)
	}
	// The dry run went first, then the create, with the user's client.
	calls := f.callsTo(http.MethodPost, nsIDMSList)
	if len(calls) != 3 || calls[1].Query.Get("dryRun") != "All" || calls[2].Query.Get("dryRun") != "" {
		t.Fatalf("calls %+v", calls)
	}

	// Done now: a second plan has nothing to do.
	again, _ := PlanNodeSetup(c, NodeSetupMirrorCreate)
	if !again.NothingToDo || again.Ready || !strings.Contains(again.State, "rhoai-quay-mirror already redirects") {
		t.Fatalf("second plan %+v", again)
	}
}

func TestMirrorCreateCountsAnExistingMirrorAndRefusesAForeignName(t *testing.T) {
	t.Run("another IDMS redirects the source", func(t *testing.T) {
		f, c := nodeSetupFake(t, "HighlyAvailable", false)
		f.putJSON("/apis/config.openshift.io/v1/imagedigestmirrorsets/team-mirrors", `{"spec":{"imageDigestMirrors":[{"source":"registry.redhat.io/rhoai","mirrors":["mirror.example.com/rhoai"]}]}}`)
		res, _ := runNodeSetupCollect(t, c, types.NodeSetupRequest{Action: NodeSetupMirrorCreate})
		if !res.Success || !strings.Contains(res.Message, "team-mirrors already redirects") || len(writes(f)) != 0 {
			t.Fatalf("result %+v, writes %v", res, writes(f))
		}
	})
	t.Run("rhoai-quay-mirror exists for something else", func(t *testing.T) {
		f, c := nodeSetupFake(t, "HighlyAvailable", false)
		f.putJSON(nsIDMSPath, `{"spec":{"imageDigestMirrors":[{"source":"registry.example.com/other","mirrors":["quay.io/other"]}]}}`)
		plan, _ := PlanNodeSetup(c, NodeSetupMirrorCreate)
		if plan.Ready || plan.RefusalCode != "foreign" || !strings.Contains(plan.Refusal, "does not redirect registry.redhat.io/rhoai") {
			t.Fatalf("plan %+v", plan)
		}
		res, _ := runNodeSetupCollect(t, c, types.NodeSetupRequest{Action: NodeSetupMirrorCreate})
		if res.Success || res.ErrorCode != "foreign" || len(writes(f)) != 0 {
			t.Fatalf("result %+v, writes %v", res, writes(f))
		}
	})
}

func TestNodeSetupPerPlatform(t *testing.T) {
	for _, tc := range []struct {
		name     string
		topology string
		managed  bool
		action   string
		wantCode string // "" = ready
		wantCP   string
	}{
		{"hosted: mirror", "External", false, NodeSetupMirrorCreate, "hosted", ControlPlaneHosted},
		{"hosted: pull secret", "External", false, NodeSetupPullAdd, "hosted", ControlPlaneHosted},
		{"hosted with SRE webhooks", "External", true, NodeSetupMirrorCreate, "hosted", ControlPlaneHosted},
		{"topology unreadable: mirror", "", false, NodeSetupMirrorCreate, "unknown_topology", ""},
		{"topology unreadable: pull secret removal", "", false, NodeSetupPullRemove, "unknown_topology", ""},
		{"managed classic: mirror allowed", "HighlyAvailable", true, NodeSetupMirrorCreate, "", ControlPlaneManaged},
		{"managed classic: pull secret refused", "HighlyAvailable", true, NodeSetupPullAdd, "managed", ControlPlaneManaged},
		{"managed classic: pull secret removal refused", "HighlyAvailable", true, NodeSetupPullRemove, "managed", ControlPlaneManaged},
		{"single node", "SingleReplica", false, NodeSetupPullAdd, "", ControlPlaneSelfManaged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := nodeSetupFake(t, tc.topology, tc.managed)
			plan, err := PlanNodeSetup(c, tc.action)
			if err != nil {
				t.Fatal(err)
			}
			if plan.RefusalCode != tc.wantCode || plan.Ready != (tc.wantCode == "") || plan.ControlPlane != tc.wantCP {
				t.Fatalf("plan %+v", plan)
			}
			if tc.wantCode != "" {
				if !strings.Contains(plan.Refusal, "Nothing was changed") {
					t.Fatalf("refusal %q does not say nothing changed", plan.Refusal)
				}
				res, _ := runNodeSetupCollect(t, c, types.NodeSetupRequest{Action: tc.action, ResourceVersion: "42"})
				if res.Success || res.ErrorCode != tc.wantCode {
					t.Fatalf("run %+v", res)
				}
			}
			if len(writes(f)) != 0 {
				t.Fatalf("writes %v", writes(f))
			}
		})
	}
}

// Unknown platform: the topology is not External but the webhooks cannot
// be listed. The action is attempted; RBAC or admission refuses it in the
// dry run, before anything changes.
func TestNodeSetupUnknownPlatformIsAttemptedAndItsRefusalHandled(t *testing.T) {
	f, c := nodeSetupFake(t, "HighlyAvailable", false)
	f.fail["GET /apis/admissionregistration.k8s.io/v1/validatingwebhookconfigurations"] = 403
	f.failDryRun["POST "+nsIDMSList] = 403
	f.failMessage["DRYRUN POST "+nsIDMSList] = `admission webhook "imagecontentpolicies-validation.managed.openshift.io" denied the request: Managed OpenShift customers may not create ImageContentSourcePolicy, ImageDigestMirrorSet, or ImageTagMirrorSet resources that configure mirrors that would conflict with system registries`
	plan, _ := PlanNodeSetup(c, NodeSetupMirrorCreate)
	if plan.ControlPlane != "" || plan.Ready || plan.RefusalCode != "forbidden" ||
		!strings.Contains(plan.Refusal, "denied the request") || !strings.Contains(plan.Refusal, "Do it yourself instead") {
		t.Fatalf("plan %+v", plan)
	}
	res, _ := runNodeSetupCollect(t, c, types.NodeSetupRequest{Action: NodeSetupMirrorCreate})
	if res.Success || res.ErrorCode != "forbidden" || len(writes(f)) != 0 {
		t.Fatalf("run %+v, writes %v", res, writes(f))
	}
}

func TestMirrorRemove(t *testing.T) {
	ours := `{"metadata":{"labels":{"app.kubernetes.io/managed-by":"rhoai-nightly-updater"}},"spec":{"imageDigestMirrors":[{"source":"registry.redhat.io/rhoai","mirrors":["quay.io/rhoai"]}]}}`
	t.Run("the setup's mirror", func(t *testing.T) {
		f, c := nodeSetupFake(t, "HighlyAvailable", false)
		f.putJSON(nsIDMSPath, ours)
		plan, _ := PlanNodeSetup(c, NodeSetupMirrorRemove)
		if !plan.Ready || plan.UID == "" || plan.ResourceVersion == "" || !strings.Contains(plan.NodeImpact, "drains each node") || !strings.Contains(plan.NodeImpact, "single-node cluster is not drained") {
			t.Fatalf("plan %+v", plan)
		}
		if !f.has(nsIDMSPath) {
			t.Fatal("the plan deleted it")
		}
		res, _ := runNodeSetupCollect(t, c, types.NodeSetupRequest{Action: NodeSetupMirrorRemove, UID: plan.UID, ResourceVersion: plan.ResourceVersion})
		if !res.Success || f.has(nsIDMSPath) {
			t.Fatalf("result %+v, still there %v", res, f.has(nsIDMSPath))
		}
		dels := f.callsTo(http.MethodDelete, nsIDMSPath)
		if len(dels) != 3 || !strings.Contains(dels[2].Body, `"uid":"`+plan.UID+`"`) || !strings.Contains(dels[2].Body, `"resourceVersion":"`+plan.ResourceVersion+`"`) {
			t.Fatalf("deletes %+v", dels)
		}
	})
	t.Run("the docs' command made it, without the label", func(t *testing.T) {
		f, c := nodeSetupFake(t, "HighlyAvailable", false)
		f.putJSON(nsIDMSPath, `{"spec":{"imageDigestMirrors":[{"source":"registry.redhat.io/rhoai","mirrors":["quay.io/rhoai"]}]}}`)
		if plan, _ := PlanNodeSetup(c, NodeSetupMirrorRemove); !plan.Ready {
			t.Fatalf("plan %+v", plan)
		}
	})
	t.Run("it changed after the review", func(t *testing.T) {
		f, c := nodeSetupFake(t, "HighlyAvailable", false)
		f.putJSON(nsIDMSPath, ours)
		plan, _ := PlanNodeSetup(c, NodeSetupMirrorRemove)
		res, _ := runNodeSetupCollect(t, c, types.NodeSetupRequest{Action: NodeSetupMirrorRemove, UID: plan.UID, ResourceVersion: "7"})
		if res.Success || res.ErrorCode != "conflict" || !f.has(nsIDMSPath) || len(writes(f)) != 0 {
			t.Fatalf("result %+v", res)
		}
	})
	for _, tc := range []struct{ name, obj, other, code string }{
		{"more than the setup's mirror", `{"spec":{"imageDigestMirrors":[{"source":"registry.redhat.io/rhoai","mirrors":["quay.io/rhoai"]},{"source":"registry.example.com/x","mirrors":["quay.io/x"]}]}}`, "", "foreign"},
		{"a second mirror for the source", `{"spec":{"imageDigestMirrors":[{"source":"registry.redhat.io/rhoai","mirrors":["quay.io/rhoai","mirror.example.com/rhoai"]}]}}`, "", "foreign"},
		{"never contact the source", `{"spec":{"imageDigestMirrors":[{"source":"registry.redhat.io/rhoai","mirrors":["quay.io/rhoai"],"mirrorSourcePolicy":"NeverContactSource"}]}}`, "", "foreign"},
		{"only someone else's mirror", "", `{"spec":{"imageDigestMirrors":[{"source":"registry.redhat.io/rhoai","mirrors":["quay.io/rhoai"]}]}}`, "foreign"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := nodeSetupFake(t, "HighlyAvailable", false)
			if tc.obj != "" {
				f.putJSON(nsIDMSPath, tc.obj)
			}
			if tc.other != "" {
				f.putJSON("/apis/config.openshift.io/v1/imagedigestmirrorsets/ocm-mirror", tc.other)
			}
			plan, _ := PlanNodeSetup(c, NodeSetupMirrorRemove)
			if plan.Ready || plan.RefusalCode != tc.code {
				t.Fatalf("plan %+v", plan)
			}
			res, _ := runNodeSetupCollect(t, c, types.NodeSetupRequest{Action: NodeSetupMirrorRemove, ResourceVersion: "1"})
			if res.Success || len(writes(f)) != 0 {
				t.Fatalf("result %+v, writes %v", res, writes(f))
			}
		})
	}
	t.Run("nothing to remove", func(t *testing.T) {
		_, c := nodeSetupFake(t, "HighlyAvailable", false)
		if plan, _ := PlanNodeSetup(c, NodeSetupMirrorRemove); !plan.NothingToDo {
			t.Fatalf("plan %+v", plan)
		}
	})
}

func TestPullSecretAddKeepsEveryOtherLogin(t *testing.T) {
	f, c := nodeSetupFake(t, "HighlyAvailable", false)
	plan, _ := PlanNodeSetup(c, NodeSetupPullAdd)
	if !plan.Ready || plan.ResourceVersion != "42" || !strings.Contains(plan.Changes[0], "add a quay.io/rhoai entry") || !strings.Contains(plan.Changes[0], "the other 2 registry logins stay as they are") {
		t.Fatalf("plan %+v", plan)
	}
	raw, _ := json.Marshal(plan)
	for _, cred := range []string{nsToken, nsTokenPadded, "upd4ter-s3cret", nsCloudAuth, "cl0ud-s3cret"} {
		if strings.Contains(string(raw), cred) {
			t.Fatalf("the plan carries a credential: %s", raw)
		}
	}
	res, _ := runNodeSetupCollect(t, c, types.NodeSetupRequest{Action: NodeSetupPullAdd, ResourceVersion: plan.ResourceVersion})
	if !res.Success {
		t.Fatalf("result %+v", res)
	}
	auths := pullSecretAuths(t, f)
	want := clusterLogins(map[string]any{"quay.io/rhoai": map[string]any{"auth": nsTokenPadded}})
	gotJSON, _ := json.Marshal(auths)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("auths %s, want %s (the padded token, every other login kept)", gotJSON, wantJSON)
	}
	// A merge patch of the one data key, with the resourceVersion read, after
	// its dry run.
	patches := f.callsTo(http.MethodPatch, nsPullSecretPath)
	if len(patches) != 3 || patches[1].Query.Get("dryRun") != "All" || patches[2].ContentType != "application/merge-patch+json" ||
		!strings.Contains(patches[2].Body, `"resourceVersion":"42"`) || strings.Contains(patches[2].Body, `"type"`) {
		t.Fatalf("patches %+v", patches)
	}
	if again, _ := PlanNodeSetup(c, NodeSetupPullAdd); !again.NothingToDo {
		t.Fatalf("second plan %+v", again)
	}
}

func TestPullSecretAddReplacesAnotherToken(t *testing.T) {
	f, c := nodeSetupFake(t, "HighlyAvailable", false)
	f.put(nsPullSecretPath, map[string]any{"metadata": map[string]any{"resourceVersion": "42"}, "data": map[string]any{
		".dockerconfigjson": nsDockerConfig(t, map[string]any{"auths": clusterLogins(map[string]any{"https://quay.io/rhoai/": map[string]any{"auth": nsOldRHOAI}})}),
	}})
	plan, _ := PlanNodeSetup(c, NodeSetupPullAdd)
	if !plan.Ready || !strings.Contains(plan.Changes[0], "replace the token") || !strings.Contains(plan.Undo, "is not kept") {
		t.Fatalf("plan %+v", plan)
	}
	if res, _ := runNodeSetupCollect(t, c, types.NodeSetupRequest{Action: NodeSetupPullAdd, ResourceVersion: "42"}); !res.Success {
		t.Fatalf("result %+v", res)
	}
	auths := pullSecretAuths(t, f)
	if _, old := auths["https://quay.io/rhoai/"]; old || len(auths) != 3 || authFromEntry(auths["quay.io/rhoai"]) != nsTokenPadded {
		t.Fatalf("auths %v", auths)
	}
}

func TestPullSecretChangedAfterTheReviewChangesNothing(t *testing.T) {
	t.Run("a different version than the plan's", func(t *testing.T) {
		f, c := nodeSetupFake(t, "HighlyAvailable", false)
		res, _ := runNodeSetupCollect(t, c, types.NodeSetupRequest{Action: NodeSetupPullAdd, ResourceVersion: "41"})
		if res.Success || res.ErrorCode != "conflict" || len(writes(f)) != 0 {
			t.Fatalf("result %+v, writes %v", res, writes(f))
		}
	})
	t.Run("changed between the read and the write", func(t *testing.T) {
		f, c := nodeSetupFake(t, "HighlyAvailable", false)
		changed := false
		f.beforeRequest = func(r *http.Request) {
			if r.Method == http.MethodPatch && r.URL.Query().Get("dryRun") == "" && !changed {
				changed = true
				obj := f.get(nsPullSecretPath)
				f.mu.Lock()
				obj["metadata"].(map[string]any)["resourceVersion"] = "43"
				f.mu.Unlock()
			}
		}
		res, _ := runNodeSetupCollect(t, c, types.NodeSetupRequest{Action: NodeSetupPullAdd, ResourceVersion: "42"})
		if res.Success || res.ErrorCode != "conflict" || !strings.Contains(res.Message, "Nothing was changed") {
			t.Fatalf("result %+v", res)
		}
		if _, ok := pullSecretAuths(t, f)["quay.io/rhoai"]; ok {
			t.Fatal("the entry was written over a concurrent change")
		}
	})
}

// Every way the cluster pull secret or the token can be unsafe to use
// refuses before any write.
func TestPullSecretFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, f *resourceFake)
		code  string
	}{
		{"cluster pull secret missing", func(t *testing.T, f *resourceFake) {
			f.mu.Lock()
			delete(f.objects, fakeKey{gv: "v1", ns: "openshift-config", plural: "secrets", name: "pull-secret"})
			f.mu.Unlock()
		}, "unreadable"},
		{"cluster pull secret forbidden", func(t *testing.T, f *resourceFake) { f.fail["GET "+nsPullSecretPath] = 403 }, "forbidden"},
		{"cluster pull secret garbled", func(t *testing.T, f *resourceFake) {
			f.put(nsPullSecretPath, map[string]any{"data": map[string]any{".dockerconfigjson": "bm90IGpzb24="}})
		}, "unreadable"},
		{"no registry logins", func(t *testing.T, f *resourceFake) {
			f.put(nsPullSecretPath, map[string]any{"data": map[string]any{".dockerconfigjson": nsDockerConfig(t, map[string]any{"auths": map[string]any{}})}})
		}, "unreadable"},
		{"kept in sync by External Secrets", func(t *testing.T, f *resourceFake) {
			f.put(nsPullSecretPath, map[string]any{
				"metadata": map[string]any{"managedFields": []any{map[string]any{"manager": "externalsecrets.external-secrets.io/cluster-pull-secret"}}},
				"data":     map[string]any{".dockerconfigjson": nsDockerConfig(t, map[string]any{"auths": clusterLogins(nil)})},
			})
		}, "external_secrets"},
		{"no updater token", func(t *testing.T, f *resourceFake) {
			f.mu.Lock()
			delete(f.objects, fakeKey{gv: "v1", ns: "kube-system", plural: "secrets", name: "additional-pull-secret"})
			f.mu.Unlock()
		}, "no_token"},
		{"Quay refuses the token", func(t *testing.T, f *resourceFake) {
			verifyQuayCredentials = func(string) error { return &quayAuthError{StatusCode: 401, Body: "unauthorized"} }
		}, "quay"},
		{"Quay cannot be asked", func(t *testing.T, f *resourceFake) {
			verifyQuayCredentials = func(string) error { return &quayAuthError{StatusCode: 503, Body: "down"} }
		}, "quay"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := nodeSetupFake(t, "HighlyAvailable", false)
			tc.setup(t, f)
			plan, _ := PlanNodeSetup(c, NodeSetupPullAdd)
			if plan.Ready || plan.RefusalCode != tc.code {
				t.Fatalf("plan %+v", plan)
			}
			res, _ := runNodeSetupCollect(t, c, types.NodeSetupRequest{Action: NodeSetupPullAdd, ResourceVersion: "42"})
			if res.Success || res.ErrorCode != tc.code || len(writes(f)) != 0 {
				t.Fatalf("result %+v, writes %v", res, writes(f))
			}
			if len(f.callsTo(http.MethodPatch, nsPullSecretPath)) != 0 {
				t.Fatal("not even a dry run may be sent")
			}
		})
	}
}

// An API error that quotes the object it was sent (a validation or webhook
// message) never carries a credential back.
func TestPullSecretErrorsAreRedacted(t *testing.T) {
	f, c := nodeSetupFake(t, "HighlyAvailable", false)
	f.failEcho["DRYRUN PATCH "+nsPullSecretPath] = 422
	plan, _ := PlanNodeSetup(c, NodeSetupPullAdd)
	if plan.Ready || !strings.Contains(plan.Refusal, "[redacted]") {
		t.Fatalf("plan %+v", plan)
	}
	for _, cred := range []string{nsToken, nsTokenPadded, "upd4ter-s3cret", nsCloudAuth, "cl0ud-s3cret", nsRedHatAuth, "r3dhat-s3cret"} {
		if strings.Contains(plan.Refusal, cred) {
			t.Fatalf("refusal carries %q: %s", cred, plan.Refusal)
		}
	}
	if len(f.echoed) == 0 {
		t.Fatal("the fake did not echo the request")
	}
}

func TestPullSecretRemove(t *testing.T) {
	f, c := nodeSetupFake(t, "HighlyAvailable", false)
	f.put(nsPullSecretPath, map[string]any{"metadata": map[string]any{"resourceVersion": "42"}, "data": map[string]any{
		".dockerconfigjson": nsDockerConfig(t, map[string]any{"auths": clusterLogins(map[string]any{
			"quay.io/rhoai":           map[string]any{"auth": nsOldRHOAI},
			"quay.io/rhoai/other-app": map[string]any{"auth": nsOldRHOAI},
		})}),
	}})
	plan, _ := PlanNodeSetup(c, NodeSetupPullRemove)
	if !plan.Ready || !strings.Contains(plan.NodeImpact, "images already on a node stay") || !strings.Contains(plan.Changes[0], "the other 3 registry logins") {
		t.Fatalf("plan %+v", plan)
	}
	if res, _ := runNodeSetupCollect(t, c, types.NodeSetupRequest{Action: NodeSetupPullRemove, ResourceVersion: "42"}); !res.Success {
		t.Fatalf("result %+v", res)
	}
	auths := pullSecretAuths(t, f)
	if _, ok := auths["quay.io/rhoai"]; ok || len(auths) != 3 || auths["quay.io/rhoai/other-app"] == nil {
		t.Fatalf("auths %v: only the quay.io/rhoai entry goes", auths)
	}
	if again, _ := PlanNodeSetup(c, NodeSetupPullRemove); !again.NothingToDo {
		t.Fatalf("second plan %+v", again)
	}
}

func TestNodeSetupNamesStuckMachineConfigPools(t *testing.T) {
	f, c := nodeSetupFake(t, "HighlyAvailable", false)
	f.putJSON(nsPoolsPath+"/worker", `{"spec":{"paused":true}}`)
	f.putJSON(nsPoolsPath+"/infra", `{"status":{"conditions":[{"type":"Degraded","status":"True"}]}}`)
	f.putJSON(nsPoolsPath+"/master", `{"status":{"conditions":[{"type":"Updated","status":"True"}]}}`)
	plan, _ := PlanNodeSetup(c, NodeSetupMirrorCreate)
	if !slices.Equal(plan.Pools, []string{"infra (degraded)", "worker (paused)"}) {
		t.Fatalf("pools %v", plan.Pools)
	}
}

func TestNodeSetupActionsAreLabelledInTheActivityLog(t *testing.T) {
	for action, recorded := range nodeSetupActivity {
		if !ValidNodeSetupAction(action) {
			t.Errorf("%s is not an action", action)
		}
		if a, ok := activityActions[recorded]; !ok || a[0] != "setup" {
			t.Errorf("%s: activity %q has no setup label", action, recorded)
		}
		if activityFailedLabels[recorded] == "" {
			t.Errorf("%s: no failed label", recorded)
		}
	}
}
