package cluster

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const (
	infrastructurePath = "/apis/config.openshift.io/v1/infrastructures/cluster"
	webhookConfigsPath = "/apis/admissionregistration.k8s.io/v1/validatingwebhookconfigurations"
)

// Validating webhook configurations: none, a self-managed cluster's own,
// and Red Hat SRE's on ROSA Classic and OSD (managed-cluster-validating-webhooks
// build/resources.go: "sre-<hook>", webhook "<hook>.managed.openshift.io").
var (
	noWebhooks      = mockResponse{body: `{"items":[]}`}
	otherWebhooks   = mockResponse{body: `{"items":[{"metadata":{"name":"vdatasciencecluster.opendatahub.io"},"webhooks":[{"name":"datasciencecluster-v1.opendatahub.io"}]}]}`}
	managedWebhooks = mockResponse{body: `{"items":[{"metadata":{"name":"autoscaling.openshift.io"},"webhooks":[{"name":"clusterautoscalers.autoscaling.openshift.io"}]},{"metadata":{"name":"sre-imagecontentpolicies-validation"},"webhooks":[{"name":"imagecontentpolicies-validation.managed.openshift.io"}]}]}`}
)

// The command the setup card and Diagnostics show for a self-managed
// cluster. It is pinned here so that a change is a deliberate one;
// scripts/test-node-pull-secret.sh runs the same file against a stub oc.
const wantNodePullSecretCommand = `printf 'quay.io/rhoai token (the one you saved under Pull secret; base64 user:password, with its = padding): ' && read -rs QUAY_AUTH && echo
[ -n "$QUAY_AUTH" ] && oc get secret pull-secret -n openshift-config -o json \
  | QUAY_AUTH="$QUAY_AUTH" jq '.data[".dockerconfigjson"] |= (@base64d | fromjson | .auths["quay.io/rhoai"] = {auth: $ENV.QUAY_AUTH} | tojson | @base64)' \
  | oc replace -f -
unset QUAY_AUTH`

func TestNodePullSecretCommandIsExactlyTheScript(t *testing.T) {
	if rhoai.NodePullSecretCommand != wantNodePullSecretCommand {
		t.Fatalf("command changed:\n%s\nwant:\n%s", rhoai.NodePullSecretCommand, wantNodePullSecretCommand)
	}
	// The token never appears as an argument (ps shows arguments) and
	// nothing runs without one.
	if strings.Contains(rhoai.NodePullSecretCommand, "--arg") || !strings.Contains(rhoai.NodePullSecretCommand, `[ -n "$QUAY_AUTH" ] &&`) {
		t.Fatal("the token must reach jq through its environment, and only when it is set")
	}
}

func TestReadControlPlane(t *testing.T) {
	for _, tc := range []struct {
		name     string
		resp     mockResponse
		webhooks mockResponse
		want     string
		wantOK   bool
	}{
		{"hosted control plane", mockResponse{body: `{"status":{"controlPlaneTopology":"External","infrastructureTopology":"HighlyAvailable"}}`}, noWebhooks, rhoai.ControlPlaneHosted, true},
		{"highly available", mockResponse{body: `{"status":{"controlPlaneTopology":"HighlyAvailable"}}`}, noWebhooks, rhoai.ControlPlaneSelfManaged, true},
		{"single node", mockResponse{body: `{"status":{"controlPlaneTopology":"SingleReplica"}}`}, noWebhooks, rhoai.ControlPlaneSelfManaged, true},
		{"other webhooks only", mockResponse{body: `{"status":{"controlPlaneTopology":"HighlyAvailable"}}`}, otherWebhooks, rhoai.ControlPlaneSelfManaged, true},
		{"Red Hat-managed classic", mockResponse{body: `{"status":{"controlPlaneTopology":"HighlyAvailable"}}`}, managedWebhooks, rhoai.ControlPlaneManaged, true},
		{"hosted with SRE webhooks", mockResponse{body: `{"status":{"controlPlaneTopology":"External"}}`}, managedWebhooks, rhoai.ControlPlaneHosted, true},
		{"webhooks unreadable", mockResponse{body: `{"status":{"controlPlaneTopology":"HighlyAvailable"}}`}, mockResponse{statusCode: 403, body: `{"kind":"Status","code":403,"reason":"Forbidden"}`}, "", false},
		{"no topology field", mockResponse{body: `{"status":{}}`}, noWebhooks, "", false},
		{"forbidden", mockResponse{statusCode: 403, body: `{"kind":"Status","code":403,"reason":"Forbidden"}`}, noWebhooks, "", false},
		{"garbled", mockResponse{body: `not json`}, noWebhooks, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, cleanup := newMockClient(map[string]mockResponse{infrastructurePath: tc.resp, webhookConfigsPath: tc.webhooks})
			defer cleanup()
			got, err := rhoai.ReadControlPlane(c)
			if got != tc.want || (err == nil) != tc.wantOK {
				t.Fatalf("got %q, %v; want %q (ok %v)", got, err, tc.want, tc.wantOK)
			}
			if cached := rhoai.CachedControlPlane(c); cached != tc.want {
				t.Fatalf("cached %q, want %q", cached, tc.want)
			}
		})
	}
}

// The topology is read once per cluster; a failed read is retried.
func TestCachedControlPlaneCachesOnlyAnswers(t *testing.T) {
	a := &statusAPI{csvList: `{"items":[]}`, failPath: map[string]int{infrastructurePath: 503}}
	c := newStatusAPI(t, a)
	if cp := rhoai.CachedControlPlane(c); cp != "" {
		t.Fatalf("unreadable topology = %q, want unknown", cp)
	}
	a.mu.Lock()
	delete(a.failPath, infrastructurePath)
	a.infra = "SingleReplica"
	a.mu.Unlock()
	for i := range 3 {
		if cp := rhoai.CachedControlPlane(c); cp != rhoai.ControlPlaneSelfManaged {
			t.Fatalf("poll %d: %q", i, cp)
		}
	}
	if n := a.count(infrastructurePath); n != 2 {
		t.Fatalf("Infrastructure read %d times, want 2 (the failure, then one answer)", n)
	}
}

func TestGetStatus_NodePullSecretPerTopology(t *testing.T) {
	t.Setenv("STABLE_SOURCE", "redhat-operators")
	t.Setenv("STABLE_CHANNEL", "")
	for _, tc := range []struct {
		name      string
		topology  string // "" = 403
		webhooks  string
		csImage   string
		wantCP    string
		wantNodes bool
		wantPull  bool
	}{
		{"hosted control plane", "External", "", "", rhoai.ControlPlaneHosted, false, false},
		{"self-managed, highly available", "HighlyAvailable", "", "", rhoai.ControlPlaneSelfManaged, true, false},
		{"self-managed, single node", "SingleReplica", "", "", rhoai.ControlPlaneSelfManaged, true, false},
		{"Red Hat-managed classic", "HighlyAvailable", managedWebhooks.body, "", rhoai.ControlPlaneManaged, true, false},
		{"unreadable", "", "", "", "", true, false},
		{"self-managed with a READY nightly catalog", "HighlyAvailable", "", "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6@sha256:abc", rhoai.ControlPlaneSelfManaged, true, true},
		{"unreadable with a READY nightly catalog", "", "", "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.6@sha256:abc", "", true, true},
		{"self-managed, catalog from another registry", "HighlyAvailable", "", "registry.example.com/fbc:1", rhoai.ControlPlaneSelfManaged, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			installFakeRegistry(t, newFakeRegistry())
			a := &statusAPI{csvList: `{"items":[]}`, infra: tc.topology, webhooks: tc.webhooks, csImage: tc.csImage}
			if tc.topology == "" {
				a.failPath = map[string]int{infrastructurePath: 403}
			}
			status, err := GetStatus(newStatusAPI(t, a))
			if err != nil {
				t.Fatal(err)
			}
			if status.Cluster.ControlPlane != tc.wantCP {
				t.Fatalf("controlPlane %q, want %q", status.Cluster.ControlPlane, tc.wantCP)
			}
			nodes := status.PullSecret.Nodes
			if (nodes != nil) != tc.wantNodes {
				t.Fatalf("nodes %+v, want set %v", nodes, tc.wantNodes)
			}
			if nodes != nil && (nodes.Command != rhoai.NodePullSecretCommand || nodes.RemoveCommand != rhoai.NodePullSecretRemoveCommand || nodes.Pulled != tc.wantPull) {
				t.Fatalf("nodes %+v, want the command and pulled %v", nodes, tc.wantPull)
			}
			// An unknown topology is not an error the status reports.
			if len(status.Errors) != 0 {
				t.Fatalf("errors %v", status.Errors)
			}
		})
	}
}

// The gitops profile does not use the pull secret and does not read the
// topology (deploy/rbac-gitops.yaml does not grant it).
func TestGetStatusGitOps_DoesNotReadControlPlane(t *testing.T) {
	t.Setenv("STABLE_SOURCE", "redhat-operators")
	t.Setenv("STABLE_CHANNEL", "")
	a := &statusAPI{csvList: `{"items":[]}`, infra: "HighlyAvailable"}
	status, err := GetStatusGitOps(newStatusAPI(t, a))
	if err != nil {
		t.Fatal(err)
	}
	if a.count(infrastructurePath) != 0 || status.PullSecret.Nodes != nil || status.Cluster.ControlPlane != "" {
		t.Fatalf("read %d times, status %+v %+v", a.count(infrastructurePath), status.PullSecret, status.Cluster)
	}
}

// The status carries the tool's record of the entry it added to the
// nodes' pull secret (read with the ServiceAccount from its own
// ConfigMap), so the card offers the removal only for that entry; the
// gitops profile and a hosted control plane do not read it.
func TestGetStatus_NodePullRecord(t *testing.T) {
	t.Setenv("STABLE_SOURCE", "redhat-operators")
	t.Setenv("STABLE_CHANNEL", "")
	saved := paddedAuth(exampleAuth("user:pass"))
	record := func(auth string) string {
		raw, _ := json.Marshal(nodePullRecord{SecretUID: "u", Key: "quay.io/rhoai", Salt: "aa", AuthSHA256: hashPullAuth("aa", auth), At: "2026-11-18T10:00:00Z", By: "admin"})
		cm, _ := json.Marshal(map[string]any{"data": map[string]string{nodePullRecordKey: string(raw)}})
		return string(cm)
	}
	snapshotPath := rhoai.NamespacedPath("v1", "configmaps", getActivityNamespace(), snapshotConfigMapName)
	for _, tc := range []struct {
		name     string
		topology string
		snapshot string
		fail     bool
		want     *types.NodePullSecretRecord
	}{
		{"no record", "HighlyAvailable", "", false, nil},
		{"recorded with the saved token", "HighlyAvailable", record(saved), false, &types.NodePullSecretRecord{At: "2026-11-18T10:00:00Z", By: "admin", SameToken: true}},
		{"recorded with an earlier token", "HighlyAvailable", record(nsOldRHOAI), false, &types.NodePullSecretRecord{At: "2026-11-18T10:00:00Z", By: "admin"}},
		{"record unreadable: none shown", "HighlyAvailable", record(saved), true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			installFakeRegistry(t, newFakeRegistry())
			a := &statusAPI{csvList: `{"items":[]}`, infra: tc.topology, snapshot: tc.snapshot}
			if tc.fail {
				a.failPath = map[string]int{snapshotPath: 403}
			}
			status, err := GetStatus(newStatusAPI(t, a))
			if err != nil {
				t.Fatal(err)
			}
			if status.PullSecret.Nodes == nil || !kube.JSONEqual(status.PullSecret.Nodes.AddedByTool, tc.want) {
				t.Fatalf("nodes %+v, want addedByTool %+v", status.PullSecret.Nodes, tc.want)
			}
			if len(status.Errors) != 0 {
				t.Fatalf("errors %v", status.Errors)
			}
		})
	}
	t.Run("gitops profile: not read", func(t *testing.T) {
		a := &statusAPI{csvList: `{"items":[]}`, infra: "HighlyAvailable", snapshot: record(saved)}
		if _, err := GetStatusGitOps(newStatusAPI(t, a)); err != nil {
			t.Fatal(err)
		}
		if n := a.count(snapshotPath); n != 0 {
			t.Fatalf("snapshot read %d times", n)
		}
	})
}

func TestCatalogPulledFromQuay(t *testing.T) {
	for _, tc := range []struct {
		cs   types.CatalogSourceInfo
		want bool
	}{
		{types.CatalogSourceInfo{Exists: true, State: "READY", Image: "quay.io/rhoai/rhoai-fbc-fragment@sha256:a"}, true},
		{types.CatalogSourceInfo{Exists: true, State: "TRANSIENT_FAILURE", Image: "quay.io/rhoai/rhoai-fbc-fragment@sha256:a"}, false},
		{types.CatalogSourceInfo{Exists: true, State: "READY", Image: "quay.io/rhoai-other/x@sha256:a"}, false},
		{types.CatalogSourceInfo{Exists: false, State: "READY", Image: "quay.io/rhoai/x"}, false},
	} {
		if got := rhoai.CatalogPulledFromQuay(tc.cs); got != tc.want {
			t.Errorf("%+v: %v, want %v", tc.cs, got, tc.want)
		}
	}
}

// The catalog pull error of Update names the pull secret the nodes use.
func TestUpdateStream_CatalogImagePullAdvicePerTopology(t *testing.T) {
	for _, tc := range []struct {
		name     string
		topology string
		want     []string
		notWant  []string
	}{
		{"hosted control plane", "External", []string{"(Setup step 1)"}, []string{"openshift-config/pull-secret"}},
		{"self-managed", "HighlyAvailable", []string{"This cluster is self-managed", "openshift-config/pull-secret", "Add to the nodes' pull secret under Pull secret in the cluster setup"}, []string{"Setup step 1", "If it is self-managed"}},
		{"single node", "SingleReplica", []string{"This cluster is self-managed", "openshift-config/pull-secret"}, []string{"Setup step 1"}},
		{"unreadable", "", []string{"could not read whether this cluster has a hosted control plane", "If it is self-managed", "openshift-config/pull-secret", "the command under Pull secret"}, []string{"This cluster is self-managed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeOLM(t).installed("rhods-operator.3.6.0", nil)
			f.topology = tc.topology
			f.verifyState = "TRANSIENT_FAILURE"
			f.catalogPods = `{"items":[{"status":{"containerStatuses":[{"state":{"waiting":{"reason":"ErrImagePull","message":"unauthorized: access to the requested resource is not authorized"}}}]}}]}`
			result, err := UpdateStream(f.client(context.Background()), testNightlyImage, func(UpdateStepEvent) {})
			if err != nil || result.Success || result.ErrorCode != "catalog_image_pull" {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if !strings.HasPrefix(result.Message, "Replacement catalog validation failed: the catalog image "+testNightlyImage+" cannot be pulled (ErrImagePull: unauthorized") ||
				!strings.HasSuffix(result.Message, "Nothing was changed.") {
				t.Fatalf("message %q", result.Message)
			}
			for _, s := range tc.want {
				if !strings.Contains(result.Message, s) {
					t.Errorf("message lacks %q: %s", s, result.Message)
				}
			}
			for _, s := range tc.notWant {
				if strings.Contains(result.Message, s) {
					t.Errorf("message has %q: %s", s, result.Message)
				}
			}
		})
	}
}

// The gitops profile sets up no pull secret, so its pull error does not
// point to the setup.
func TestLiveCatalogTarget_GitOpsProfilePullAdvice(t *testing.T) {
	f := newFakeOLM(t).installed("rhods-operator.3.6.0", nil)
	f.catalog["spec"].(map[string]any)["image"] = testNightlyImage
	f.catalogPods = `{"items":[{"status":{"containerStatuses":[{"state":{"waiting":{"reason":"ImagePullBackOff","message":"unauthorized"}}}]}}]}`
	var logs []string
	_, err := liveCatalogTarget(f.client(context.Background()), testNightlyImage, "", "Argo CD application rhoai", func(UpdateStepEvent) {}, &logs)
	if !isCatalogImagePullError(err) {
		t.Fatalf("err %v", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "which Git or ACM manage here") || strings.Contains(msg, "Setup step 1") || strings.Contains(msg, "the command under Pull secret") {
		t.Fatalf("message %q", msg)
	}
	for _, r := range f.requests {
		if r.Path == infrastructurePath {
			t.Fatal("the gitops profile read the control plane topology")
		}
	}
}
