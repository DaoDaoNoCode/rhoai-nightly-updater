package cluster

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/authz"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/features"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/storage"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// putProject stores a data science project as the projects API lists it
// (the fake does not filter by user: tests that need another user's view
// use failAs or leave the project out).
func (f *resourceFake) putProject(name string) {
	f.putJSON("/apis/project.openshift.io/v1/projects/"+name, `{"metadata":{"name":"`+name+`","labels":{"opendatahub.io/dashboard":"true"}},"status":{"phase":"Active"}}`)
}

// deleteObject removes the object stored at path.
func (f *resourceFake) deleteObject(path string) {
	k, _ := parseFakePath(path)
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objects, k)
}

// userClientOf is c acting with the signed-in user's own token, with c
// (the ServiceAccount's) for the updater's own records and the cluster
// facts, as pkg/api withUserRead and withMutationAuth build it.
func userClientOf(c *Client, token string) *Client {
	u := *c
	editTestClient(&u, func(o *testClientOptions) {
		o.Token, o.AsUser, o.Bookkeeper, o.Facts, o.SignedInUser, o.StatusRead = token, true, c, c, nil, false
	})
	return &u
}

// The projects API with the user's token and the data science label;
// terminating projects are left out; the namespaces API is not used.
func TestListUserDSProjects(t *testing.T) {
	k8s, c := newResourceFake(t)
	k8s.putProject("beta")
	k8s.putProject("alpha")
	k8s.putJSON("/apis/project.openshift.io/v1/projects/plain", `{"metadata":{"name":"plain"},"status":{"phase":"Active"}}`)
	k8s.putJSON("/apis/project.openshift.io/v1/projects/going", `{"metadata":{"name":"going","labels":{"opendatahub.io/dashboard":"true"}},"status":{"phase":"Terminating"}}`)
	u := userClientOf(c, fakeUserToken)
	got, err := ListUserDSProjects(u)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ProjectNames(got), []string{"alpha", "beta"}) || got[0].UID == "" || got[0].UID == got[1].UID {
		t.Fatalf("projects %v", got)
	}
	for _, call := range k8s.calls {
		if call.Token != fakeUserToken || call.Path != "/apis/project.openshift.io/v1/projects" || call.Query.Get("labelSelector") != "opendatahub.io/dashboard=true" {
			t.Errorf("unexpected call %+v", call)
		}
	}
	// No project: an empty list, not null.
	_, c2 := newResourceFake(t)
	if got, err := ListUserDSProjects(userClientOf(c2, fakeUserToken)); err != nil || got == nil || len(got) != 0 {
		t.Fatalf("no projects: %v %v", got, err)
	}
}

// Refused with the ServiceAccount's client, before any request.
func TestUserReadsRefuseTheServiceAccount(t *testing.T) {
	k8s, c := newResourceFake(t)
	if _, err := ListUserDSProjects(c); !errors.Is(err, authz.ErrNotUserClient) {
		t.Fatalf("got %v", err)
	}
	if _, err := GetProjectStatus(c, c, "p"); !errors.Is(err, authz.ErrNotUserClient) {
		t.Fatalf("project status: %v", err)
	}
	if _, err := GetPlaygroundStatus(c, c, ""); !errors.Is(err, authz.ErrNotUserClient) {
		t.Fatalf("playground status: %v", err)
	}
	if _, err := GetMLflowSamplesStatus(c, c, "p"); !errors.Is(err, authz.ErrNotUserClient) {
		t.Fatalf("samples status: %v", err)
	}
	if _, err := GetWorkbenchStatus(c, c, "p"); !errors.Is(err, authz.ErrNotUserClient) {
		t.Fatalf("workbench status: %v", err)
	}
	if len(k8s.calls) != 0 {
		t.Fatalf("calls: %+v", k8s.calls)
	}
}

// A user's project status: that project's objects only, every read of
// them with the user's token; a part they may not read says so and the
// rest is shown; a project that is not theirs is refused before any read
// of its objects.
func TestGetProjectStatus(t *testing.T) {
	k8s, c := newResourceFake(t)
	k8s.putProject("proj")
	k8s.putJSON("/apis/datasciencepipelinesapplications.opendatahub.io/v1/namespaces/proj/datasciencepipelinesapplications/dspa",
		`{"metadata":{"name":"dspa","namespace":"proj","uid":"u1","labels":`+toolLabelJSON+`},"status":{"conditions":[{"type":"Ready","status":"True"}]}}`)
	k8s.putJSON("/apis/datasciencepipelinesapplications.opendatahub.io/v1/namespaces/proj/datasciencepipelinesapplications/theirs",
		`{"metadata":{"name":"theirs","namespace":"proj","uid":"u2"}}`)
	k8s.putJSON("/apis/datasciencepipelinesapplications.opendatahub.io/v1/namespaces/other/datasciencepipelinesapplications/dspa",
		`{"metadata":{"name":"dspa","namespace":"other","uid":"u3"}}`)
	k8s.putJSON("/apis/serving.kserve.io/v1alpha2/namespaces/proj/llminferenceservices/qwen",
		`{"metadata":{"name":"qwen","namespace":"proj"},"status":{"conditions":[{"type":"Ready","status":"True"}]}}`)
	k8s.putJSON("/apis/serving.kserve.io/v1alpha2/namespaces/other/llminferenceservices/secret-model",
		`{"metadata":{"name":"secret-model","namespace":"other"}}`)
	k8s.putJSON("/apis/serving.kserve.io/v1beta1/namespaces/proj/inferenceservices/iris",
		`{"metadata":{"name":"iris","namespace":"proj"},"status":{"url":"https://iris.example.com","conditions":[{"type":"Ready","status":"False","reason":"Loading"}]}}`)
	u := userClientOf(c, fakeUserToken)
	k8s.failAs[fakeUserToken+" GET /api/v1/namespaces/proj/secrets/"+storage.ConnectionSecretName] = 403

	st, err := GetProjectStatus(c, u, "proj")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.PipelineServers) != 2 || st.PipelineServersProblem != "" {
		t.Fatalf("pipeline servers %+v %q", st.PipelineServers, st.PipelineServersProblem)
	}
	for _, ps := range st.PipelineServers {
		switch ps.Name {
		case "dspa":
			if !ps.ManagedByTool || ps.ConnectionState != ConnectionUnreadable || !strings.Contains(ps.ConnectionMessage, "may not read") {
				t.Errorf("the tool's: %+v", ps)
			}
		case "theirs":
			if ps.ManagedByTool || ps.ConnectionState != "" {
				t.Errorf("someone else's: %+v", ps)
			}
		default:
			t.Errorf("another project's pipeline server: %+v", ps)
		}
	}
	if len(st.Models) != 1 || st.Models[0].Name != "qwen" || !st.Models[0].Ready {
		t.Errorf("models %+v", st.Models)
	}
	if len(st.InferenceServices) != 1 || st.InferenceServices[0].Name != "iris" || st.InferenceServices[0].Ready || st.InferenceServices[0].Message != "Loading" || st.InferenceServices[0].URL == "" {
		t.Errorf("inference services %+v", st.InferenceServices)
	}
	for _, call := range k8s.calls {
		own := strings.HasPrefix(call.Path, "/api/v1/namespaces/"+record.GetActivityNamespace()+"/configmaps/")
		if strings.Contains(call.Path, "/namespaces/proj/") && call.Token != fakeUserToken {
			t.Errorf("%s read with %q, not the user's token", call.Path, call.Token)
		}
		if strings.Contains(call.Path, "/namespaces/other/") || (call.Path == "/api/v1/namespaces" && call.Query.Get("labelSelector") != "") {
			t.Errorf("read outside the project: %s", call.Path)
		}
		if own && call.Token != fakeSAToken {
			t.Errorf("the tool's record was read with %q", call.Token)
		}
	}

	// A part the user may not read.
	k8s.failAs[fakeUserToken+" GET /apis/serving.kserve.io/v1beta1/namespaces/proj/inferenceservices"] = 403
	st, err = GetProjectStatus(c, u, "proj")
	if err != nil || st.InferenceServices != nil || !strings.Contains(st.InferenceServicesProblem, "may not read the InferenceServices") || st.InferenceServicesProblemCode != types.ProblemForbiddenForUser || st.ModelsProblemCode != "" || len(st.Models) != 1 {
		t.Fatalf("refused part: %+v, %v", st, err)
	}

	// Not the user's project.
	before := len(k8s.calls)
	if _, err := GetProjectStatus(c, u, "other"); !errors.Is(err, ErrNotDSProject) {
		t.Fatalf("other project: %v", err)
	}
	for _, call := range k8s.calls[before:] {
		if call.Path != "/apis/project.openshift.io/v1/projects" {
			t.Errorf("read for a project that is not the user's: %s", call.Path)
		}
	}
}

// A user who is not a cluster-admin and has no project: the Playground
// status shows the operator's readiness and that the feature is blocked
// as fixed values only, never the DataScienceCluster's condition message
// or its name, which the ServiceAccount read.
func TestPlaygroundStatusForUserHasNoServiceAccountText(t *testing.T) {
	const sentinel = "SENTINEL-from-the-dsc"
	for _, dsc := range []string{
		// The operator reports not ready, with a message.
		`{"metadata":{"name":"dsc-` + strings.ToLower(sentinel) + `"},"spec":{"components":{"ogx":{"managementState":"Managed"}}},"status":{"conditions":[{"type":"OGXReady","status":"False","message":"` + sentinel + `"}]}}`,
		// The component is off: the block's reason names the DSC.
		`{"metadata":{"name":"dsc-` + strings.ToLower(sentinel) + `"},"spec":{"components":{"ogx":{"managementState":"Removed"}}},"status":{"conditions":[{"type":"OGXReady","status":"False","message":"` + sentinel + `"}]}}`,
	} {
		e := newPlaygroundEnv(t)
		e.k8s.deleteObject("/apis/project.openshift.io/v1/projects/proj")
		e.k8s.deleteObject("/apis/project.openshift.io/v1/projects/other")
		e.k8s.putJSON("/apis/datasciencecluster.opendatahub.io/v2/datascienceclusters/default-dsc", dsc)
		features.PurgePlaygroundStatus()
		full, err := GetPlaygroundStatus(e.c, e.reader, "")
		if err != nil {
			t.Fatal(err)
		}
		// The admin's answer keeps it (the precondition of this test).
		raw, _ := json.Marshal(full)
		if !strings.Contains(strings.ToLower(string(raw)), strings.ToLower(sentinel)) {
			t.Fatalf("the fixture did not reach the status: %s", raw)
		}
		user := PlaygroundStatusForUser(full)
		b, _ := json.Marshal(user)
		if strings.Contains(strings.ToLower(string(b)), strings.ToLower(sentinel)) {
			t.Errorf("a user's playground status carries the ServiceAccount's text: %s", b)
		}
		if user.OperatorReady != "False" || len(user.Items) != 0 {
			t.Errorf("fixed values lost: %s", b)
		}
		if full.Blocked != nil && (user.Blocked == nil || user.Blocked.State != "Removed" || user.Blocked.DSC != "") {
			t.Errorf("block: %+v", user.Blocked)
		}
		// The cached admin answer is unchanged.
		if again, _ := json.Marshal(full); string(again) != string(raw) {
			t.Error("the sanitizer changed the cached status")
		}
	}
	if PlaygroundStatusForUser(nil) != nil {
		t.Error("nil")
	}
}

// A 403 to the user's token is the user's: forbidden_for_user, never the
// ServiceAccount's template hint.
func TestUserReadForbiddenIsTheUsers(t *testing.T) {
	k8s, c := newResourceFake(t)
	k8s.failAs[fakeUserToken+" GET /apis/project.openshift.io/v1/projects"] = 403
	_, err := ListUserDSProjects(userClientOf(c, fakeUserToken))
	if code, ec := kube.HTTPStatusForError(err); code != 403 || ec != kube.ErrorCodeForbiddenForUser {
		t.Fatalf("got %d %s (%v)", code, ec, err)
	}
	if kube.TemplateHint(err) != "" {
		t.Fatalf("template hint on the user's refusal: %q", kube.TemplateHint(err))
	}
}
