package cluster

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/storage"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Project S3 storage and its pipeline server (the non-admin design,
// section 4; B5).

const (
	psNS          = "/api/v1/namespaces/proj"
	psPVCs        = psNS + "/persistentvolumeclaims"
	psSecrets     = psNS + "/secrets"
	psServices    = psNS + "/services"
	psPolicies    = "/apis/networking.k8s.io/v1/namespaces/proj/networkpolicies"
	psDeployments = "/apis/apps/v1/namespaces/proj/deployments"
	psDSPAs       = "/apis/datasciencepipelinesapplications.opendatahub.io/v1/namespaces/proj/datasciencepipelinesapplications"
	psApps        = "redhat-ods-applications"
)

// psObjectPaths are the project storage's objects, by kind, in the
// order setup writes them.
var psObjectPaths = []struct{ kind, collection, path string }{
	{"PersistentVolumeClaim", psPVCs, psPVCs + "/" + storage.ProjectS3PVC},
	{"Secret", psSecrets, psSecrets + "/" + storage.ProjectS3Secret},
	{"NetworkPolicy", psPolicies, psPolicies + "/" + storage.ProjectS3NetworkPolicy},
	{"Deployment", psDeployments, psDeployments + "/" + storage.ProjectS3Deployment},
	{"Service", psServices, psServices + "/" + storage.ProjectS3Service},
}

// newProjectStorageEnv is a fake cluster with RHOAI (its applications
// namespace from the DSCInitialization), pipelines Managed and project
// proj, a project admin's client (alice, not a cluster-admin), and a
// Deployment controller that makes nightly-s3 ready on its next read.
func newProjectStorageEnv(t *testing.T) (*resourceFake, *Client) {
	t.Helper()
	f, c := newResourceFake(t)
	putManagedDSC(f)
	f.putProject("proj")
	f.putJSON(psNS, `{"metadata":{"name":"proj","uid":"uid-proj","labels":{"opendatahub.io/dashboard":"true"}},"status":{"phase":"Active"}}`)
	f.onGet = func(k fakeKey, obj map[string]any) {
		if k.plural == "deployments" && k.ns == "proj" && k.name == storage.ProjectS3Deployment {
			obj["status"] = map[string]any{"readyReplicas": 1, "observedGeneration": 1}
		}
	}
	u := userClientOf(c, fakeUserToken)
	u.SetUsername("alice")
	u.SetProjectUser("uid-proj")
	return f, u
}

func runPS(t *testing.T, c *Client, req types.ProjectStorageRequest) *types.OperationResponse {
	t.Helper()
	res, err := RunProjectStorage(c, req, nil)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func runPP(t *testing.T, c *Client, req types.ProjectPipelineServerRequest) *types.OperationResponse {
	t.Helper()
	res, err := RunProjectPipelineServer(c, req, nil)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// writesOf are the requests of f that changed something (no dry run, no
// read), with the record's ConfigMap left out.
func writesOf(f *resourceFake) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if c.Method == "GET" || c.dryRun() || strings.Contains(c.Path, "/configmaps") {
			continue
		}
		out = append(out, c.Method+" "+c.Path)
	}
	return out
}

func psRecord(t *testing.T, c *Client) storage.ProjectStorageRecorded {
	t.Helper()
	rec, err := storage.ReadProjectStorageRecord(c)
	if err != nil {
		t.Fatal(err)
	}
	e, _ := rec.Get("proj")
	return e
}

// Setup makes the five objects with the user's token, records each UID as
// a user's entry, and becomes ready; Repair changes nothing new; the
// NetworkPolicy is written and read back before the Deployment.
func TestNetworkPolicyWrittenBeforeDeployment(t *testing.T) {
	f, u := newProjectStorageEnv(t)
	res := runPS(t, u, types.ProjectStorageRequest{Project: "proj", Action: "setup"})
	if !res.Success {
		t.Fatalf("%+v", res)
	}
	want := []string{"POST " + psPVCs, "POST " + psSecrets, "POST " + psPolicies, "POST " + psDeployments, "POST " + psServices}
	if got := writesOf(f); !slices.Equal(got, want) {
		t.Fatalf("writes %v, want %v", got, want)
	}
	// The policy was read back after its create and before the Deployment's.
	f.mu.Lock()
	var order []string
	for _, c := range f.calls {
		if !c.dryRun() && (c.Path == psPolicies+"/"+storage.ProjectS3NetworkPolicy && c.Method == "GET" || c.Method == "POST") {
			order = append(order, c.Method+" "+c.Path)
		}
	}
	tokens := map[string]bool{}
	for _, c := range f.calls {
		if strings.Contains(c.Path, "/namespaces/proj/") {
			tokens[c.Token] = true
		}
	}
	f.mu.Unlock()
	created, deployed := slices.Index(order, "POST "+psPolicies), slices.Index(order, "POST "+psDeployments)
	if created < 0 || deployed < created || !slices.Contains(order[created:deployed], "GET "+psPolicies+"/"+storage.ProjectS3NetworkPolicy) {
		t.Fatalf("the policy was not confirmed between its create and the Deployment's: %v", order)
	}
	if len(tokens) != 1 || !tokens[fakeUserToken] {
		t.Fatalf("project requests sent with %v", tokens)
	}
	e := psRecord(t, u)
	if e.PVCUID == "" || e.SecretUID == "" || e.NetworkPolicyUID == "" || e.DeploymentUID == "" || e.ServiceUID == "" || e.ActorRole != record.ActorRoleUser || e.By != "alice" || e.ProjectUID != "uid-proj" {
		t.Fatalf("record %+v", e)
	}
	before := len(writesOf(f))
	if res := runPS(t, u, types.ProjectStorageRequest{Project: "proj", Action: "repair"}); !res.Success {
		t.Fatalf("repair: %+v", res)
	}
	for _, w := range writesOf(f)[before:] {
		if strings.HasPrefix(w, "POST ") || strings.HasPrefix(w, "DELETE ") {
			t.Errorf("repair of a complete storage made or deleted %s", w)
		}
	}

	// A policy that cannot be written stops the setup before the
	// Deployment, and what it made is deleted again.
	f2, u2 := newProjectStorageEnv(t)
	f2.fail["POST "+psPolicies] = 500
	res = runPS(t, u2, types.ProjectStorageRequest{Project: "proj", Action: "setup"})
	if res.Success || slices.Contains(writesOf(f2), "POST "+psDeployments) {
		t.Fatalf("%+v %v", res, writesOf(f2))
	}
	for _, o := range psObjectPaths {
		if f2.has(o.path) {
			t.Errorf("%s left after the fence failed", o.kind)
		}
	}
	if e := psRecord(t, u2); !e.Empty() {
		t.Errorf("record left %+v", e)
	}
}

// The policy lets in TCP 8333 only, from the project's pods and from
// RHOAI's applications namespace by its name label, and nothing else.
func TestNetworkPolicyOnlyS3PortFromProjectAndApps(t *testing.T) {
	m := storage.ProjectS3Manifests("proj", "apps-ns", storage.S3Keys{})
	assertJSONEqual(t, m["NetworkPolicy"]["spec"], `{
		"podSelector":{"matchLabels":{"app":"nightly-s3"}},
		"policyTypes":["Ingress"],
		"ingress":[{"from":[{"podSelector":{}},{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"apps-ns"}}}],
			"ports":[{"protocol":"TCP","port":8333}]}]}`)
	assertJSONEqual(t, m["Service"]["spec"], `{"selector":{"app":"nightly-s3"},"type":"ClusterIP","ports":[{"name":"s3","port":8333,"targetPort":8333}]}`)
	// The written policy names the applications namespace the facts
	// client read; without one the setup refuses before any write.
	f, u := newProjectStorageEnv(t)
	if res := runPS(t, u, types.ProjectStorageRequest{Project: "proj", Action: "setup"}); !res.Success {
		t.Fatalf("%+v", res)
	}
	raw, _ := json.Marshal(f.get(psPolicies + "/" + storage.ProjectS3NetworkPolicy)["spec"])
	if !strings.Contains(string(raw), `"kubernetes.io/metadata.name":"`+psApps+`"`) {
		t.Fatalf("policy %s", raw)
	}
	f2, u2 := newProjectStorageEnv(t)
	f2.deleteObject(dsciV2List + "/default-dsci")
	res := runPS(t, u2, types.ProjectStorageRequest{Project: "proj", Action: "setup"})
	if res.Success || res.ErrorCode != "prerequisites_unknown" || len(writesOf(f2)) != 0 {
		t.Fatalf("no applications namespace: %+v %v", res, writesOf(f2))
	}
	// Drift shows in the status; Repair applies it again.
	f.put(psPolicies+"/"+storage.ProjectS3NetworkPolicy, mergeMeta(f.get(psPolicies+"/"+storage.ProjectS3NetworkPolicy), map[string]any{"spec": map[string]any{"podSelector": map[string]any{}, "ingress": []any{map[string]any{}}}}))
	if st := GetProjectStorageStatus(u, "proj"); !strings.Contains(st.NetworkPolicyProblem, "was changed") {
		t.Fatalf("drift not reported: %+v", st)
	}
	if res := runPS(t, u, types.ProjectStorageRequest{Project: "proj", Action: "repair"}); !res.Success {
		t.Fatalf("repair %+v", res)
	}
	if st := GetProjectStorageStatus(u, "proj"); st.NetworkPolicyProblem != "" || !st.Ready || !st.ManagedByTool {
		t.Fatalf("after repair %+v", st)
	}
}

// mergeMeta is obj with fields replaced (metadata kept).
func mergeMeta(obj map[string]any, fields map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range obj {
		out[k] = v
	}
	for k, v := range fields {
		out[k] = v
	}
	return out
}

// The pod runs weed mini without the IAM-write flags, with its bucket.
func TestNoIAMWriteFlags(t *testing.T) {
	args := storage.ProjectS3Args()
	if slices.Contains(args, storage.S3IAMWritesArg) || slices.Contains(args, storage.S3SelfServiceArg) || !slices.Contains(args, "-s3.iam.readOnly=true") {
		t.Fatalf("args %v", args)
	}
	raw, _ := json.Marshal(storage.ProjectS3Manifests("proj", "apps", storage.S3Keys{})["Deployment"])
	for _, bad := range []string{storage.S3IAMWritesArg, storage.S3SelfServiceArg, "runAsUser", "12535", "WEED_ADMIN"} {
		if strings.Contains(string(raw), `"`+bad) {
			t.Errorf("the Deployment has %s: %s", bad, raw)
		}
	}
	if !strings.Contains(string(raw), "@sha256:") {
		t.Errorf("the image is not pinned by digest: %s", raw)
	}
}

// The updater never sends a request to any S3 endpoint for project
// storage: not on setup, not for its pipeline server, not on delete.
func TestNoToolCallToProjectS3(t *testing.T) {
	f, u := newProjectStorageEnv(t)
	if res := runPS(t, u, types.ProjectStorageRequest{Project: "proj", Action: "setup"}); !res.Success {
		t.Fatalf("%+v", res)
	}
	if res := runPP(t, u, types.ProjectPipelineServerRequest{Project: "proj", Action: "setup"}); !res.Success {
		t.Fatalf("%+v", res)
	}
	if res := runPP(t, u, types.ProjectPipelineServerRequest{Project: "proj", Action: "teardown"}); !res.Success {
		t.Fatalf("%+v", res)
	}
	if res := runPS(t, u, types.ProjectStorageRequest{Project: "proj", Action: "delete", DeleteData: storage.ProjectS3PVC}); !res.Success {
		t.Fatalf("%+v", res)
	}
	if len(f.sw.calls) != 0 {
		t.Fatalf("S3 requests: %v", f.sw.calls)
	}
}

// Every Secret the project storage and its pipeline server write (from
// the RBAC scan of their code) has a name no ServiceAccount rule of
// deploy/template.yaml lets it get, in any namespace.
func TestProjectStorageNamesMatchNoSARule(t *testing.T) {
	names := map[string]bool{}
	for _, id := range []string{"project-s3.setup", "project-s3.repair", "project-s3.delete", "project-pipeline-server.setup", "project-pipeline-server.teardown"} {
		for _, r := range projectActionScan(t, id) {
			if r.Resource == "secrets" && r.Group == "" && (r.Verb == "create" || r.Verb == "patch" || r.Verb == "update") && r.Name != "" {
				names[r.Name] = true
			}
		}
	}
	for _, want := range []string{storage.ProjectS3Secret, storage.DSPASecretName, storage.ProjectConnectionSecretName} {
		if !names[want] {
			t.Errorf("the scan does not find the write of Secret %s", want)
		}
	}
	grants := templateGrants(t)
	for name := range names {
		for _, g := range grants {
			if g.allows(rbacRequest{Verb: "get", Resource: "secrets", Namespace: "some-project", Name: name}) {
				t.Errorf("the ServiceAccount may get Secret %s in a project (%s)", name, g.source)
			}
		}
	}
	// The four names the ServiceAccount may get in any namespace.
	for _, n := range []string{"seaweedfs-s3-credentials", "minio-secret", "rhoai-nightly-updater-s3", "maas-db-config"} {
		if names[n] {
			t.Errorf("project storage writes Secret %s, which the ServiceAccount may read", n)
		}
	}
}

// Only an admin of the project may set up, repair or delete its storage
// (the preflight's create rolebindings review); editors may run its
// pipeline server; a cluster-admin may not set either up.
func TestProjectStorageNeedsProjectAdmin(t *testing.T) {
	for _, id := range []string{"project-s3.setup", "project-s3.repair", "project-s3.delete"} {
		a, _ := ProjectActionByID(id)
		if !slices.Contains(a.Requests, projectAdminCheck) {
			t.Errorf("%s does not check for an admin of the project", id)
		}
	}
	for _, id := range []string{"project-pipeline-server.setup", "project-pipeline-server.teardown"} {
		a, _ := ProjectActionByID(id)
		if slices.Contains(a.Requests, projectAdminCheck) {
			t.Errorf("%s needs an admin of the project; editors use the storage", id)
		}
	}
	// An editor: the review of create rolebindings is denied.
	sf := &ssarFake{allow: func(r AccessRequest) bool { return r.Resource != "rolebindings" }}
	u := newSSARClient(t, sf)
	reviews, err := ReviewProjectActions(u, []string{"project-s3.setup", "project-pipeline-server.setup"}, "team-a", "apps")
	if err != nil {
		t.Fatal(err)
	}
	m := reviews["project-s3.setup"].Missing
	if len(m) != 1 || !strings.Contains(m[0].Text, "ask an admin of project team-a") || len(reviews["project-pipeline-server.setup"].Missing) != 0 {
		t.Fatalf("%+v", reviews)
	}
	// A cluster-admin's client (no project user): Set up refused before
	// anything is read or written; a pipeline server's too.
	f, c := newProjectStorageEnv(t)
	admin := userClientOf(c.Bookkeeping(), fakeUserToken)
	res := runPS(t, admin, types.ProjectStorageRequest{Project: "proj", Action: "setup"})
	if res.Success || res.ErrorCode != "not_for_cluster_admins" || len(writesOf(f)) != 0 {
		t.Fatalf("%+v %v", res, writesOf(f))
	}
	if res := runPP(t, admin, types.ProjectPipelineServerRequest{Project: "proj", Action: "setup"}); res.Success || res.ErrorCode != "not_for_cluster_admins" {
		t.Fatalf("%+v", res)
	}
	// ... and repairs and deletes one a project admin made.
	if res := runPS(t, c, types.ProjectStorageRequest{Project: "proj", Action: "setup"}); !res.Success {
		t.Fatalf("%+v", res)
	}
	if res := runPS(t, admin, types.ProjectStorageRequest{Project: "proj", Action: "repair"}); !res.Success {
		t.Fatalf("admin repair %+v", res)
	}
	if res := runPS(t, admin, types.ProjectStorageRequest{Project: "proj", Action: "delete"}); !res.Success {
		t.Fatalf("admin delete %+v", res)
	}
	if !f.has(psPVCs + "/" + storage.ProjectS3PVC) {
		t.Fatal("the volume went without its name typed")
	}
}

// Each create of a setup that fails, and a record write that fails,
// leaves nothing this run made unrecorded: it is deleted again by UID.
func TestProjectStorageRollbackPerWrite(t *testing.T) {
	for i, o := range psObjectPaths {
		t.Run(o.kind, func(t *testing.T) {
			f, u := newProjectStorageEnv(t)
			f.fail["POST "+o.collection] = 500
			res := runPS(t, u, types.ProjectStorageRequest{Project: "proj", Action: "setup"})
			if res.Success {
				t.Fatal("setup went through")
			}
			e := psRecord(t, u)
			for j, p := range psObjectPaths {
				obj := f.get(p.path)
				if j >= i && obj != nil {
					t.Errorf("%s made after the failure", p.kind)
				}
				if obj != nil && e.UID(p.kind) != obj["metadata"].(map[string]any)["uid"] {
					t.Errorf("%s left unrecorded", p.kind)
				}
			}
			for j := range i {
				if f.has(psObjectPaths[j].path) {
					t.Errorf("%s not deleted again", psObjectPaths[j].kind)
				}
			}
		})
	}
	t.Run("record", func(t *testing.T) {
		f, u := newProjectStorageEnv(t)
		f.fail["PATCH /api/v1/namespaces/"+record.GetActivityNamespace()+"/configmaps/"+record.SnapshotConfigMapName] = 500
		res := runPS(t, u, types.ProjectStorageRequest{Project: "proj", Action: "setup"})
		if res.Success || f.has(psPVCs+"/"+storage.ProjectS3PVC) || slices.Contains(writesOf(f), "POST "+psSecrets) {
			t.Fatalf("%+v %v", res, writesOf(f))
		}
	})
	t.Run("pod refused", func(t *testing.T) {
		f, u := newProjectStorageEnv(t)
		f.onGet = func(k fakeKey, obj map[string]any) {
			if k.plural == "deployments" && k.name == storage.ProjectS3Deployment {
				obj["status"] = map[string]any{"conditions": []any{map[string]any{"type": "ReplicaFailure", "status": "True", "reason": "FailedCreate", "message": `pods "nightly-s3-x" is forbidden: exceeded quota: compute`}}}
			}
		}
		res := runPS(t, u, types.ProjectStorageRequest{Project: "proj", Action: "setup"})
		if res.Success || !strings.Contains(res.Message, "exceeded quota") {
			t.Fatalf("%+v", res)
		}
		for _, p := range psObjectPaths {
			if f.has(p.path) {
				t.Errorf("%s left after the pod was refused", p.kind)
			}
		}
	})
}

// Delete refuses while a pipeline server uses the storage, deletes by
// UID, keeps the volume unless its name is typed, and keeps an object it
// did not record.
func TestProjectStorageDelete(t *testing.T) {
	f, u := newProjectStorageEnv(t)
	if res := runPS(t, u, types.ProjectStorageRequest{Project: "proj", Action: "setup"}); !res.Success {
		t.Fatalf("%+v", res)
	}
	f.putJSON(psDSPAs+"/someone", `{"metadata":{"name":"someone"},"spec":{"objectStorage":{"externalStorage":{"host":"nightly-s3.proj.svc.cluster.local:8333"}}}}`)
	before := len(writesOf(f))
	if res := runPS(t, u, types.ProjectStorageRequest{Project: "proj", Action: "delete", DeleteData: storage.ProjectS3PVC}); res.Success || res.ErrorCode != storage.ErrorCodeProjectStorageInUse || len(writesOf(f)) != before {
		t.Fatalf("in use: %+v", res)
	}
	f.deleteObject(psDSPAs + "/someone")
	if res := runPS(t, u, types.ProjectStorageRequest{Project: "proj", Action: "delete"}); !res.Success {
		t.Fatalf("%+v", res)
	}
	for _, o := range psObjectPaths[1:] {
		if f.has(o.path) {
			t.Errorf("%s left", o.kind)
		}
	}
	if !f.has(psObjectPaths[0].path) || psRecord(t, u).PVCUID == "" {
		t.Fatal("the volume or its record went without its name typed")
	}
	if res := runPS(t, u, types.ProjectStorageRequest{Project: "proj", Action: "delete", DeleteData: storage.ProjectS3PVC}); !res.Success || f.has(psObjectPaths[0].path) {
		t.Fatalf("%+v", res)
	}
	if rec, _ := storage.ReadProjectStorageRecord(u); len(rec) != 0 {
		t.Fatalf("record left %+v", rec)
	}
	// Not the tool's: a Secret of that name someone made stays.
	f.putJSON(psSecrets+"/"+storage.ProjectS3Secret, `{"metadata":{"labels":`+toolLabelJSON+`}}`)
	if res := runPS(t, u, types.ProjectStorageRequest{Project: "proj", Action: "setup"}); res.Success || res.ErrorCode != "not_managed" {
		t.Fatalf("a forged Secret was taken over: %+v", res)
	}
}

// A pipeline server set up on project storage gets the project
// storage's Connection, everywhere the tool reads or writes one.
func TestConnectionNameFollowsTheStorage(t *testing.T) {
	onProject := storage.DSPA{Meta: kubeMeta("proj", map[string]string{storage.DSPAConnectionAnnotation: storage.ProjectConnectionSecretName}), Host: "nightly-s3.proj.svc:8333"}
	shared := storage.DSPA{Meta: kubeMeta("proj", map[string]string{storage.DSPAConnectionAnnotation: storage.ConnectionSecretName}), Host: storage.S3Host()}
	if storage.ConnectionNameOf(onProject) != storage.ProjectConnectionSecretName || storage.ConnectionNameOf(shared) != storage.ConnectionSecretName {
		t.Fatal("ConnectionNameOf")
	}
	if st := storage.PipelineServerState(onProject, ""); st.Connection != storage.ProjectConnectionSecretName || !st.ProjectStorage {
		t.Fatalf("state %+v", st)
	}
	f, u := newProjectStorageEnv(t)
	if res := runPS(t, u, types.ProjectStorageRequest{Project: "proj", Action: "setup"}); !res.Success {
		t.Fatalf("%+v", res)
	}
	if res := runPP(t, u, types.ProjectPipelineServerRequest{Project: "proj", Action: "setup"}); !res.Success {
		t.Fatalf("%+v", res)
	}
	conn := f.get(psSecrets + "/" + storage.ProjectConnectionSecretName)
	if conn == nil || f.has(psSecrets+"/"+storage.ConnectionSecretName) {
		t.Fatal("the Connection is not the project storage's")
	}
	data, _ := conn["data"].(map[string]any)
	endpoint, _ := data["AWS_S3_ENDPOINT"].(string)
	if raw, _ := json.Marshal(conn); !strings.Contains(string(raw), "Project S3 storage (RHOAI Nightly Updater)") || storage.DecodeBase64Field(endpoint) != storage.ProjectS3Endpoint("proj") {
		t.Fatalf("connection %s", raw)
	}
	dspa := f.get(psDSPAs + "/dspa")
	if raw, _ := json.Marshal(dspa); !strings.Contains(string(raw), `"host":"nightly-s3.proj.svc:8333"`) || !strings.Contains(string(raw), storage.ProjectConnectionSecretName) {
		t.Fatalf("dspa %s", raw)
	}
	// The user's project status reads the project storage's Connection.
	st, err := GetProjectStatus(u, u, "proj")
	if err != nil || len(st.PipelineServers) != 1 || st.PipelineServers[0].Connection != storage.ProjectConnectionSecretName || st.PipelineServers[0].ConnectionState != ConnectionPresent {
		t.Fatalf("%+v %v", st, err)
	}
	// Clean up's workbench check and the teardown use it too.
	if conn, err := storage.ToolConnectionOf(u, "proj"); err != nil || conn != storage.ProjectConnectionSecretName {
		t.Fatalf("ToolConnectionOf %q %v", conn, err)
	}
	if res := runPP(t, u, types.ProjectPipelineServerRequest{Project: "proj", Action: "teardown"}); !res.Success || f.has(psSecrets+"/"+storage.ProjectConnectionSecretName) || f.has(psDSPAs+"/dspa") {
		t.Fatalf("%+v", res)
	}
	// No code reads the shared Connection's name directly but the
	// functions that are about the shared storage alone.
	assertConnectionConstantUsers(t)
}

// connectionConstantUsers are the functions that may name
// storage.ConnectionSecretName: each is about the shared storage only, or
// chooses between the two names.
var connectionConstantUsers = map[string]string{
	"pipeline_server.go:":                        "its declaration",
	"project_s3.go:ConnectionNameOf":             "chooses the name",
	"project_s3.go:ConnectionSecretPath":         "the path of either name",
	"connection_workbenches.go:ToolConnectionOf": "chooses the name",
	"pipeline_server.go:setupPipelineServer":     "shared storage setup (one on project storage is refused first)",
	"pipeline_server.go:TeardownPipelineServer":  "shared storage teardown (one on project storage is handed to its own first)",
	"pipeline_server.go:dashboardS3Connection":   "the shared storage's Connection object",
	"pipeline_server.go:projectSecretPath":       "the paths of both names",
	"s3_keys.go:readProjectS3Secret":             "shared storage keys (keyServers: servers on the shared storage only)",
	"s3_keys.go:connectionKeyID":                 "shared storage keys: called for servers on the shared storage (keyServers, UsedBy) and its leftover SeaweedFS users",
	"s3_secure.go:checkProjectSecret":            "Secure S3 storage: servers on the shared storage only",
	"s3_keys.go:rekeyServer":                     "shared storage keys (keyServers: servers on the shared storage only)",
	"s3_secure.go:SecureS3":                      "Secure S3 storage: servers on the shared storage only",
	"s3_secure.go:secureSecretPath":              "Secure S3 storage: servers on the shared storage only",
	"s3_migrate.go:repoint":                      "Migrate storage: servers on the old shared storage only",
	"workbench_plan.go:":                         "its declaration",
	"workbench_reads.go:toolConnection":          "reads both names",
	"workbench.go:NotebookManifest":              "the plan's ConnectionName, the shared name only when it has none",
	"workbench_run.go:describe":                  "the plan's ConnectionName, the shared name only when it has none",
	"workbench_run.go:withoutEntry":              "matches both names",
	"resources.go:GetResourcesStatusFor":         "the shared storage's Connection (a project storage's is read with the user's token)",
}

func assertConnectionConstantUsers(t *testing.T) {
	t.Helper()
	files, _ := filepath.Glob("internal/storage/*.go")
	more, _ := filepath.Glob("internal/workbench/*.go")
	files = append(files, append(more, "user_reads.go", "resources.go", "cleanup.go")...)
	fset := token.NewFileSet()
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		base := filepath.Base(path)
		check := func(fn string, n ast.Node) {
			ast.Inspect(n, func(x ast.Node) bool {
				var name string
				switch v := x.(type) {
				case *ast.Ident:
					name = v.Name
				case *ast.SelectorExpr:
					name = v.Sel.Name
				}
				if name == "ConnectionSecretName" || name == "ConnectionSecret" {
					if _, ok := connectionConstantUsers[base+":"+fn]; !ok {
						t.Errorf("%s:%s names %s; a pipeline server on project storage has %s (storage.ConnectionNameOf)", base, fn, name, storage.ProjectConnectionSecretName)
					}
				}
				return true
			})
		}
		for _, d := range file.Decls {
			switch decl := d.(type) {
			case *ast.FuncDecl:
				check(decl.Name.Name, decl.Body)
			case *ast.GenDecl:
				for _, s := range decl.Specs {
					if vs, ok := s.(*ast.ValueSpec); ok && len(vs.Values) > 0 {
						check("", vs)
					}
				}
			}
		}
	}
}

// A pipeline server stays on the storage it was set up on: the shared
// setup leaves one on project storage alone, the project setup one on the
// shared storage; the shared teardown deletes one on project storage as
// one (its own Connection, no shared key).
func TestPipelineServerStaysOnItsStorage(t *testing.T) {
	f, u := newProjectStorageEnv(t)
	if res := runPS(t, u, types.ProjectStorageRequest{Project: "proj", Action: "setup"}); !res.Success {
		t.Fatalf("%+v", res)
	}
	if res := runPP(t, u, types.ProjectPipelineServerRequest{Project: "proj", Action: "setup"}); !res.Success {
		t.Fatalf("%+v", res)
	}
	admin := userClientOf(u.Bookkeeping(), fakeUserToken)
	before := len(writesOf(f))
	for _, recreate := range []bool{false, true} {
		var res *types.OperationResponse
		if recreate {
			res, _ = RecreatePipelineServer(admin, "proj", "", nil)
		} else {
			res, _ = SetupPipelineServer(admin, "proj")
		}
		if res.Success || !strings.Contains(res.Message, "stays on it") || len(writesOf(f)) != before {
			t.Fatalf("recreate=%v: %+v %v", recreate, res, writesOf(f)[before:])
		}
	}
	res, err := TeardownPipelineServer(admin, "proj", "", nil)
	if err != nil || !res.Success || f.has(psDSPAs+"/dspa") || f.has(psSecrets+"/"+storage.ProjectConnectionSecretName) {
		t.Fatalf("%+v %v", res, err)
	}
	if len(f.sw.calls) != 0 {
		t.Fatalf("the shared storage was called: %v", f.sw.calls)
	}

	// The reverse: a tool's pipeline server on the shared storage.
	f2, u2 := newProjectStorageEnv(t)
	if res := runPS(t, u2, types.ProjectStorageRequest{Project: "proj", Action: "setup"}); !res.Success {
		t.Fatalf("%+v", res)
	}
	f2.putJSON(psDSPAs+"/dspa", `{"metadata":{"name":"dspa","labels":`+toolLabelJSON+`},"spec":{"objectStorage":{"externalStorage":{"host":"`+storage.S3Host()+`"}}}}`)
	before = len(writesOf(f2))
	if res := runPP(t, u2, types.ProjectPipelineServerRequest{Project: "proj", Action: "setup"}); res.Success || !strings.Contains(res.Message, "shared S3 storage and stays on it") || len(writesOf(f2)) != before {
		t.Fatalf("%+v", res)
	}
}

// Shared-storage actions ignore a pipeline server on project storage: it
// does not hold the shared storage (its teardown's blocker) and gets no
// key from Secure S3 storage.
func TestSharedStorageIgnoresProjectStorage(t *testing.T) {
	d := storage.DSPA{Meta: kubeMeta("proj", nil), Host: "nightly-s3.proj.svc:8333"}
	d.Meta.Labels = map[string]string{"app.kubernetes.io/managed-by": "rhoai-nightly-updater"}
	shared := storage.Address{Service: "seaweedfs-s3", Namespace: storage.S3Namespace}
	if live, deleting := storage.Users([]storage.DSPA{d}, shared); len(live)+len(deleting) != 0 {
		t.Fatalf("the shared storage counts it: %v %v", live, deleting)
	}
	if !storage.OnProjectStorage(d) || storage.OnProjectStorage(storage.DSPA{Meta: kubeMeta("other", nil), Host: "nightly-s3.proj.svc:8333"}) {
		t.Fatal("OnProjectStorage reads another project's storage as its own")
	}
}

func kubeMeta(ns string, annotations map[string]string) (m kube.ObjectMeta) {
	m.Namespace, m.Name, m.Annotations, m.UID = ns, "dspa", annotations, "uid-dspa"
	return m
}

// The background check forgets the project storage of a project that is
// gone or was made again, and keeps one whose project lives.
func TestProjectStorageRecordPrunedWithItsProject(t *testing.T) {
	f, u := newProjectStorageEnv(t)
	if res := runPS(t, u, types.ProjectStorageRequest{Project: "proj", Action: "setup"}); !res.Success {
		t.Fatalf("%+v", res)
	}
	SweepGoneProjectRecords(u.Bookkeeping())
	if psRecord(t, u).Empty() {
		t.Fatal("a live project's storage was forgotten")
	}
	f.putJSON(psNS, `{"metadata":{"name":"proj","uid":"uid-again"}}`)
	SweepGoneProjectRecords(u.Bookkeeping())
	if !psRecord(t, u).Empty() {
		t.Fatal("the storage of a project made again is still recorded")
	}
}

// Clean up everything lists the recorded project storages (from the
// record: the ServiceAccount reads none of their objects) and deletes
// each with a cluster-admin's token, its volume only when confirmed.
func TestCleanUpListsProjectStorage(t *testing.T) {
	f, u := newProjectStorageEnv(t)
	if res := runPS(t, u, types.ProjectStorageRequest{Project: "proj", Action: "setup"}); !res.Success {
		t.Fatalf("%+v", res)
	}
	sa := u.Bookkeeping()
	st, err := GetResourcesStatus(sa)
	if err != nil || len(st.ProjectStorages) != 1 || st.ProjectStorages[0].Project != "proj" || st.ProjectStorages[0].DataVolume != storage.ProjectS3PVC || st.ProjectStorages[0].By != "alice" {
		t.Fatalf("%+v %v", st.ProjectStorages, err)
	}
	admin := userClientOf(sa, fakeUserToken)
	res, err := CleanUp(admin, types.CleanupRequest{ProjectStorages: []string{"proj"}}, nil)
	if err != nil || !res.Success || f.has(psDeployments+"/"+storage.ProjectS3Deployment) || !f.has(psPVCs+"/"+storage.ProjectS3PVC) {
		t.Fatalf("%+v %v", res, err)
	}
	res, err = CleanUp(admin, types.CleanupRequest{ProjectStorages: []string{"proj"}, ProjectStorageData: []string{"proj"}}, nil)
	if err != nil || !res.Success || f.has(psPVCs+"/"+storage.ProjectS3PVC) {
		t.Fatalf("%+v %v", res, err)
	}
	if st, _ := GetResourcesStatus(sa); len(st.ProjectStorages) != 0 {
		t.Fatalf("still listed: %+v", st.ProjectStorages)
	}
}

// A delete keeps the NetworkPolicy, the Service, the Secret and the
// Deployment's record while the storage's pod may still run, also on the
// next try; once no Deployment and no pod of the storage is left, it
// finishes. A Deployment of that name left without its record (still
// terminating) keeps the policy too.
func TestProjectStorageDeleteKeepsTheFenceUntilThePodIsGone(t *testing.T) {
	f, u := newProjectStorageEnv(t)
	if res := runPS(t, u, types.ProjectStorageRequest{Project: "proj", Action: "setup"}); !res.Success {
		t.Fatalf("%+v", res)
	}
	pod := psNS + "/pods/nightly-s3-abc"
	f.putJSON(pod, `{"metadata":{"labels":{"app":"nightly-s3"}}}`)
	policy := psPolicies + "/" + storage.ProjectS3NetworkPolicy
	for try := 1; try <= 2; try++ {
		res := runPS(t, u, types.ProjectStorageRequest{Project: "proj", Action: "delete"})
		if res.Success || res.ErrorCode != "in_progress" || !f.has(policy) || !f.has(psServices+"/"+storage.ProjectS3Service) || !f.has(psSecrets+"/"+storage.ProjectS3Secret) {
			t.Fatalf("try %d: %+v", try, res)
		}
		if e := psRecord(t, u); e.DeploymentUID == "" || e.NetworkPolicyUID == "" {
			t.Fatalf("try %d: the record lost the Deployment or the policy: %+v", try, e)
		}
	}
	// The garbage collector finishes the foreground deletion.
	f.deleteObject(pod)
	f.deleteObject(psDeployments + "/" + storage.ProjectS3Deployment)
	if res := runPS(t, u, types.ProjectStorageRequest{Project: "proj", Action: "delete"}); !res.Success || f.has(policy) {
		t.Fatalf("%+v", res)
	}

	// The record lost the Deployment (as before this fix) while it still
	// terminates: the policy stays.
	f2, u2 := newProjectStorageEnv(t)
	if res := runPS(t, u2, types.ProjectStorageRequest{Project: "proj", Action: "setup"}); !res.Success {
		t.Fatalf("%+v", res)
	}
	deploy := psDeployments + "/" + storage.ProjectS3Deployment
	f2.put(deploy, mergeMeta(f2.get(deploy), map[string]any{"metadata": map[string]any{"name": storage.ProjectS3Deployment, "namespace": "proj", "uid": "uid-other", "deletionTimestamp": "2026-11-26T00:00:00Z", "labels": map[string]any{"app.kubernetes.io/managed-by": "rhoai-nightly-updater"}}}))
	if res := runPS(t, u2, types.ProjectStorageRequest{Project: "proj", Action: "delete"}); res.Success || !f2.has(psPolicies+"/"+storage.ProjectS3NetworkPolicy) {
		t.Fatalf("the policy went while a Deployment of the storage was still there: %+v", res)
	}
}

// A setup whose pod is refused rolls back, but keeps the NetworkPolicy
// (recorded) while the Deployment's pod may still be there.
func TestProjectStorageRollbackKeepsTheFenceWhileThePodMayRun(t *testing.T) {
	f, u := newProjectStorageEnv(t)
	f.onGet = func(k fakeKey, obj map[string]any) {
		if k.plural == "deployments" && k.name == storage.ProjectS3Deployment {
			obj["status"] = map[string]any{"conditions": []any{map[string]any{"type": "ReplicaFailure", "status": "True", "reason": "FailedCreate", "message": "forbidden: exceeded quota"}}}
		}
	}
	f.putJSON(psNS+"/pods/nightly-s3-abc", `{"metadata":{"labels":{"app":"nightly-s3"}}}`)
	res := runPS(t, u, types.ProjectStorageRequest{Project: "proj", Action: "setup"})
	if res.Success || !f.has(psPolicies+"/"+storage.ProjectS3NetworkPolicy) || psRecord(t, u).NetworkPolicyUID == "" {
		t.Fatalf("%+v", res)
	}
}

// Repair never makes a new key while a pipeline server uses the storage:
// its copies would keep the old one. It says to set the pipeline server
// up again, and changes nothing.
func TestProjectStorageRepairNeverRotatesAKeyInUse(t *testing.T) {
	f, u := newProjectStorageEnv(t)
	if res := runPS(t, u, types.ProjectStorageRequest{Project: "proj", Action: "setup"}); !res.Success {
		t.Fatalf("%+v", res)
	}
	if res := runPP(t, u, types.ProjectPipelineServerRequest{Project: "proj", Action: "setup"}); !res.Success {
		t.Fatalf("%+v", res)
	}
	secret := psSecrets + "/" + storage.ProjectS3Secret
	f.put(secret, mergeMeta(f.get(secret), map[string]any{"data": map[string]any{}}))
	before := len(writesOf(f))
	res := runPS(t, u, types.ProjectStorageRequest{Project: "proj", Action: "repair"})
	if res.Success || res.ErrorCode != storage.ErrorCodeProjectStorageInUse || !strings.Contains(res.Message, "set the pipeline server up again") || len(writesOf(f)) != before {
		t.Fatalf("%+v %v", res, writesOf(f)[before:])
	}
}

// Only the S3 gateway is on the pod network: one `weed server` whose
// master, volume and filer (HTTP and gRPC) listen on 127.0.0.1, with no
// Iceberg, Lance, WebDAV, SFTP, message queue, metrics or admin UI
// listener, so no other NetworkPolicy in the project can open a port that
// needs no authentication. Checked against the pinned SeaweedFS 4.54
// image: listening on every address only 8333 (S3, signed requests) and
// 18333 (the S3 gateway's gRPC, local callers only); 8080, 8888, 9333 and
// their gRPC ports on 127.0.0.1 (SECURITY.md, "Project S3 storage").
func TestProjectS3BindsOnlyS3OnThePodNetwork(t *testing.T) {
	want := []string{"server", "-ip=127.0.0.1", "-ip.bind=127.0.0.1", "-filer", "-s3", "-s3.port=8333", "-s3.ip.bind=0.0.0.0",
		"-s3.port.iceberg=0", "-s3.port.lance=0", "-s3.iam.readOnly=true", "-s3.autoCreateBucket=true", "-master.telemetry=false"}
	if got := storage.ProjectS3Args(); !slices.Equal(got, want) {
		t.Fatalf("args %v, want %v", got, want)
	}
	for _, a := range storage.ProjectS3Args() {
		for _, bad := range []string{"-webdav", "-sftp", "-mq.", "-metricsPort", "-admin", "-master.ip", "-volume.ip", "-filer.ip", "mini"} {
			if strings.HasPrefix(a, bad) {
				t.Errorf("argument %s opens a listener or a bind besides S3's", a)
			}
		}
		if strings.Contains(a, "0.0.0.0") && a != "-s3.ip.bind=0.0.0.0" {
			t.Errorf("argument %s binds to every address", a)
		}
	}
	dep := storage.ProjectS3Manifests("proj", "apps", storage.S3Keys{})["Deployment"]
	raw, _ := json.Marshal(dep)
	if !strings.Contains(string(raw), `"ports":[{"containerPort":8333,"name":"s3","protocol":"TCP"}]`) {
		t.Fatalf("container ports: %s", raw)
	}
}

// A storage that is not ready says why: its volume Pending, or its pod's
// waiting reason.
func TestProjectStorageStatusSaysWhyNotReady(t *testing.T) {
	f, u := newProjectStorageEnv(t)
	if res := runPS(t, u, types.ProjectStorageRequest{Project: "proj", Action: "setup"}); !res.Success {
		t.Fatalf("%+v", res)
	}
	f.onGet = nil
	deploy := psDeployments + "/" + storage.ProjectS3Deployment
	f.put(deploy, mergeMeta(f.get(deploy), map[string]any{"status": map[string]any{}}))
	f.putJSON(psNS+"/pods/nightly-s3-abc", `{"metadata":{"labels":{"app":"nightly-s3"}},"status":{"containerStatuses":[{"state":{"waiting":{"reason":"ImagePullBackOff","message":"Back-off pulling image"}}}]}}`)
	if st := GetProjectStorageStatus(u, "proj"); st.Ready || st.Message != "ImagePullBackOff: Back-off pulling image" {
		t.Fatalf("%+v", st)
	}
	pvc := psPVCs + "/" + storage.ProjectS3PVC
	f.put(pvc, mergeMeta(f.get(pvc), map[string]any{"status": map[string]any{"phase": "Pending"}}))
	if st := GetProjectStorageStatus(u, "proj"); !strings.Contains(st.Message, "nightly-s3-data is Pending") {
		t.Fatalf("%+v", st)
	}
}

// The background check forgets a pipeline server recorded on project
// storage when its project is gone or was made again (another UID), as it
// does the storage's entry; one on the shared storage stays while a
// namespace of its name exists.
func TestProjectPipelineRecordPrunedWithItsProject(t *testing.T) {
	f, u := newProjectStorageEnv(t)
	sa := u.Bookkeeping()
	if err := storage.WritePipelineRecord(sa, storage.PipelineRecord{
		"proj/dspa":   {UID: "u1", Storage: storage.RecordedOnProjectStorage, ActorRole: record.ActorRoleUser, ProjectUID: "uid-proj"},
		"shared/dspa": {UID: "u2"},
		"gone/dspa":   {UID: "u3", Storage: storage.RecordedOnProjectStorage, ProjectUID: "uid-gone"},
	}); err != nil {
		t.Fatal(err)
	}
	f.putJSON("/api/v1/namespaces/shared", `{"metadata":{"name":"shared","uid":"uid-shared-again"}}`)
	keys := func() []string {
		rec, err := storage.ReadPipelineRecord(sa)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for k := range rec {
			out = append(out, k)
		}
		slices.Sort(out)
		return out
	}
	SweepGoneProjectRecords(sa)
	if got := keys(); !slices.Equal(got, []string{"proj/dspa", "shared/dspa"}) {
		t.Fatalf("after the first check: %v", got)
	}
	f.putJSON(psNS, `{"metadata":{"name":"proj","uid":"uid-again"}}`)
	// The recorded pipeline server still lives in the namespace made
	// again: its record stays; and while the check fails too.
	f.putJSON(psDSPAs+"/dspa", `{"metadata":{"name":"dspa","uid":"u1"}}`)
	SweepGoneProjectRecords(sa)
	if got := keys(); !slices.Equal(got, []string{"proj/dspa", "shared/dspa"}) {
		t.Fatalf("a live pipeline server's record was forgotten: %v", got)
	}
	f.deleteObject(psDSPAs + "/dspa")
	f.fail["GET "+psDSPAs] = 500
	SweepGoneProjectRecords(sa)
	if got := keys(); !slices.Equal(got, []string{"proj/dspa", "shared/dspa"}) {
		t.Fatalf("forgotten although the check failed: %v", got)
	}
	delete(f.fail, "GET "+psDSPAs)
	SweepGoneProjectRecords(sa)
	if got := keys(); !slices.Equal(got, []string{"shared/dspa"}) {
		t.Fatalf("a project made again kept its project pipeline record: %v", got)
	}
}

// A Deployment whose record cannot be written is deleted again in the
// foreground, and the NetworkPolicy stays (recorded) while a pod of the
// storage is still there.
func TestProjectStorageRecordFailureOfTheDeploymentKeepsTheFence(t *testing.T) {
	f, u := newProjectStorageEnv(t)
	f.putJSON(psNS+"/pods/nightly-s3-abc", `{"metadata":{"labels":{"app":"nightly-s3"}}}`)
	snapshot := "PATCH /api/v1/namespaces/" + record.GetActivityNamespace() + "/configmaps/" + record.SnapshotConfigMapName
	f.beforeRequest = func(r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == psDeployments && r.URL.Query().Get("dryRun") == "" {
			f.mu.Lock()
			f.fail[snapshot] = 500
			f.mu.Unlock()
		}
	}
	res := runPS(t, u, types.ProjectStorageRequest{Project: "proj", Action: "setup"})
	if res.Success || !f.has(psPolicies+"/"+storage.ProjectS3NetworkPolicy) {
		t.Fatalf("the policy went while a pod of the storage was there: %+v", res)
	}
	var foreground bool
	for _, c := range f.callsTo(http.MethodDelete, psDeployments+"/"+storage.ProjectS3Deployment) {
		foreground = foreground || strings.Contains(c.Body, `"Foreground"`)
	}
	if !foreground {
		t.Fatal("the unrecorded Deployment was not deleted in the foreground")
	}
}

// A Repair that recreates a missing NetworkPolicy and then fails keeps
// the policy while the storage's Deployment runs.
func TestProjectStorageRepairFailureKeepsTheFence(t *testing.T) {
	f, u := newProjectStorageEnv(t)
	if res := runPS(t, u, types.ProjectStorageRequest{Project: "proj", Action: "setup"}); !res.Success {
		t.Fatalf("%+v", res)
	}
	policy := psPolicies + "/" + storage.ProjectS3NetworkPolicy
	f.deleteObject(policy)
	// The Service's write fails once the policy was made again.
	f.beforeRequest = func(r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == psPolicies && r.URL.Query().Get("dryRun") == "" {
			f.mu.Lock()
			f.fail["PATCH "+psServices+"/"+storage.ProjectS3Service] = 500
			f.mu.Unlock()
		}
	}
	res := runPS(t, u, types.ProjectStorageRequest{Project: "proj", Action: "repair"})
	if res.Success || !f.has(policy) || psRecord(t, u).NetworkPolicyUID == "" {
		t.Fatalf("%+v", res)
	}
}

// A pipeline server recorded as set up on project storage stays on the
// project path when someone in the project points its host at the shared
// storage: the status still shows it on project storage (and recorded),
// the shared Set up refuses it, the project's Set up points it back, and
// a cluster-admin's Delete deletes it as a project storage's.
func TestPipelineServerRoutedByItsRecordNotItsHost(t *testing.T) {
	f, u := newProjectStorageEnv(t)
	if res := runPS(t, u, types.ProjectStorageRequest{Project: "proj", Action: "setup"}); !res.Success {
		t.Fatalf("%+v", res)
	}
	if res := runPP(t, u, types.ProjectPipelineServerRequest{Project: "proj", Action: "setup"}); !res.Success {
		t.Fatalf("%+v", res)
	}
	moveToShared := func() {
		dspa := f.get(psDSPAs + "/dspa")
		spec := dspa["spec"].(map[string]any)
		spec["objectStorage"].(map[string]any)["externalStorage"].(map[string]any)["host"] = storage.S3Host()
		f.put(psDSPAs+"/dspa", dspa)
	}
	moveToShared()
	sa := u.Bookkeeping()
	st, err := GetResourcesStatus(sa)
	if err != nil {
		t.Fatal(err)
	}
	var ps *types.ResourceState
	for i := range st.PipelineServers {
		if st.PipelineServers[i].Namespace == "proj" {
			ps = &st.PipelineServers[i]
		}
	}
	if ps == nil || !ps.ProjectStorage || ps.Unrecorded || ps.Connection != storage.ProjectConnectionSecretName {
		t.Fatalf("status %+v", ps)
	}
	admin := userClientOf(sa, fakeUserToken)
	before := len(writesOf(f))
	if res, _ := SetupPipelineServer(admin, "proj"); res.Success || !strings.Contains(res.Message, "stays on it") || len(writesOf(f)) != before {
		t.Fatalf("the shared Set up took a project storage's server: %+v %v", res, writesOf(f)[before:])
	}
	// The project's Set up points it back at the project's storage.
	if res := runPP(t, u, types.ProjectPipelineServerRequest{Project: "proj", Action: "setup"}); !res.Success {
		t.Fatalf("%+v", res)
	}
	if raw, _ := json.Marshal(f.get(psDSPAs + "/dspa")); !strings.Contains(string(raw), `"host":"nightly-s3.proj.svc:8333"`) {
		t.Fatalf("host not set back: %s", raw)
	}
	moveToShared()
	res, err := TeardownPipelineServer(admin, "proj", "", nil)
	if err != nil || !res.Success || f.has(psDSPAs+"/dspa") || f.has(psSecrets+"/"+storage.ProjectConnectionSecretName) {
		t.Fatalf("the admin's Delete: %+v %v", res, err)
	}
	if len(f.sw.calls) != 0 {
		t.Fatalf("the shared storage was called: %v", f.sw.calls)
	}
}
