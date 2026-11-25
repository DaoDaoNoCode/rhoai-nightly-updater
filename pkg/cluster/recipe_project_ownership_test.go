package cluster

import (
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/features"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/recipe"
)

// The playground's pieces in a recipe project, as RHOAI 3.7.0-ea.1 made
// them live: the OGXServer owns the pgvector claim (blockOwnerDeletion
// false) and the two Deployments; each Deployment owns a ReplicaSet, which
// owns the pod. The rule reads neither controller nor blockOwnerDeletion,
// so the claim's reference here has no controller field and the others
// set it true.
const (
	tOGXServer      = "/apis/ogx.io/v1beta1/namespaces/recipe-dev/ogxservers/lsd-genai-playground"
	tRecipeRSList   = "/apis/apps/v1/namespaces/recipe-dev/replicasets"
	tRecipeDepList  = "/apis/apps/v1/namespaces/recipe-dev/deployments"
	tSnapshotCM     = "/api/v1/namespaces/rhoai-nightly-updater/configmaps/rhoai-nightly-updater-snapshot"
	tPlaygroundLeft = "PersistentVolumeClaim genai-pgvector-storage,Pod genai-pgvector-5d8c-abcde,Pod lsd-genai-playground-7f9b-fghij,Pod mine"
)

func putRecipePlayground(f *resourceFake, serverUID string) {
	if serverUID != "" {
		f.putJSON(tOGXServer, `{"metadata":{"uid":"`+serverUID+`"}}`)
	}
	ogx := `{"apiVersion":"ogx.io/v1beta1","kind":"OGXServer","name":"lsd-genai-playground","uid":"ogx-uid"`
	f.putJSON("/api/v1/namespaces/recipe-dev/persistentvolumeclaims/genai-pgvector-storage", `{"metadata":{"ownerReferences":[`+ogx+`,"blockOwnerDeletion":false}]}}`)
	f.putJSON(tRecipeDepList+"/genai-pgvector", `{"metadata":{"uid":"dep-pg","ownerReferences":[`+ogx+`,"controller":true,"blockOwnerDeletion":true}]}}`)
	f.putJSON(tRecipeDepList+"/lsd-genai-playground", `{"metadata":{"uid":"dep-lsd","ownerReferences":[`+ogx+`,"controller":true,"blockOwnerDeletion":true}]}}`)
	f.putJSON(tRecipeRSList+"/genai-pgvector-5d8c", `{"metadata":{"uid":"rs-pg","ownerReferences":[{"apiVersion":"apps/v1","kind":"Deployment","name":"genai-pgvector","uid":"dep-pg","controller":true}]}}`)
	f.putJSON(tRecipeRSList+"/lsd-genai-playground-7f9b", `{"metadata":{"uid":"rs-lsd","ownerReferences":[{"apiVersion":"apps/v1","kind":"Deployment","name":"lsd-genai-playground","uid":"dep-lsd","controller":true}]}}`)
	f.putJSON("/api/v1/namespaces/recipe-dev/pods/genai-pgvector-5d8c-abcde", `{"metadata":{"ownerReferences":[{"apiVersion":"apps/v1","kind":"ReplicaSet","name":"genai-pgvector-5d8c","uid":"rs-pg","controller":true}]}}`)
	f.putJSON("/api/v1/namespaces/recipe-dev/pods/lsd-genai-playground-7f9b-fghij", `{"metadata":{"ownerReferences":[{"apiVersion":"apps/v1","kind":"ReplicaSet","name":"lsd-genai-playground-7f9b","uid":"rs-lsd","controller":true}]}}`)
	// The user's own pod in the project.
	f.putJSON("/api/v1/namespaces/recipe-dev/pods/mine", `{}`)
}

func recordRecipePlayground(t *testing.T, c *Client, uid string, has bool) {
	t.Helper()
	records := map[string]features.PlaygroundRecord{}
	if has {
		records["recipe-dev"] = features.PlaygroundRecord{RequestedAt: "2026-11-25T10:00:00Z", UID: uid}
	}
	if err := features.WritePlaygroundRecords(c, records); err != nil {
		t.Fatal(err)
	}
}

// The playground a recipe requested is the tool's by the recorded UID
// alone: its claim and the pods of its Deployments are left out only when
// the record's UID is the live OGXServer's. A record without a UID (the
// time rule is not used), of another server, no record, or no server
// leaves them listed; without a root the ReplicaSets and Deployments are
// not read.
func TestRecipeProjectLeftovers_PlaygroundOwnerChain(t *testing.T) {
	for _, tc := range []struct {
		name, recordUID, serverUID string
		recorded, readsLinks       bool
		want                       string
	}{
		{"the recorded server", "ogx-uid", "ogx-uid", true, true, "Pod mine"},
		{"a record without a UID yet", "", "ogx-uid", true, false, tPlaygroundLeft},
		{"a record of another server", "old-uid", "ogx-uid", true, false, tPlaygroundLeft},
		{"no record", "", "ogx-uid", false, false, tPlaygroundLeft},
		{"no server", "ogx-uid", "", true, false, tPlaygroundLeft},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := projectFake(t)
			putRecipePlayground(f, tc.serverUID)
			recordRecipePlayground(t, c, tc.recordUID, tc.recorded)
			left, err := recipe.ProjectLeftovers(c, "recipe-dev")
			if err != nil || strings.Join(left, ",") != tc.want {
				t.Fatalf("leftovers %v %v", left, err)
			}
			reads := len(f.callsTo("GET", tRecipeRSList)) + len(f.callsTo("GET", tRecipeDepList))
			if (reads > 0) != tc.readsLinks {
				t.Fatalf("read the ReplicaSets and Deployments %d times", reads)
			}
		})
	}
}

// A chain that does not reach the recorded server leaves the object
// listed: an owner that is not there, a reference whose kind is not the
// listed object's, a cycle of references (the walk ends), and a root
// further than three references away.
func TestRecipeProjectLeftovers_BrokenOwnerChains(t *testing.T) {
	f, c := projectFake(t)
	putRecipePlayground(f, "ogx-uid")
	recordRecipePlayground(t, c, "ogx-uid", true)
	rs := func(name, kind, uid string) string {
		return `{"metadata":{"ownerReferences":[{"apiVersion":"apps/v1","kind":"` + kind + `","name":"` + name + `","uid":"` + uid + `"}]}}`
	}
	// The ReplicaSet the pod names is gone.
	f.putJSON("/api/v1/namespaces/recipe-dev/pods/orphan", rs("gone", "ReplicaSet", "rs-gone"))
	// The reference names a Deployment by a ReplicaSet's UID.
	f.putJSON("/api/v1/namespaces/recipe-dev/pods/wrong-kind", rs("genai-pgvector-5d8c", "Deployment", "rs-pg"))
	// A ReplicaSet and a Deployment that own each other.
	f.putJSON(tRecipeRSList+"/loop", `{"metadata":{"uid":"rs-loop","ownerReferences":[{"kind":"Deployment","name":"loop","uid":"dep-loop"}]}}`)
	f.putJSON(tRecipeDepList+"/loop", `{"metadata":{"uid":"dep-loop","ownerReferences":[{"kind":"ReplicaSet","name":"loop","uid":"rs-loop"}]}}`)
	f.putJSON("/api/v1/namespaces/recipe-dev/pods/loop-1", rs("loop", "ReplicaSet", "rs-loop"))
	// Pod, ReplicaSet, Deployment, ReplicaSet, then the server: four references.
	f.putJSON(tRecipeRSList+"/far", `{"metadata":{"uid":"rs-far","ownerReferences":[{"kind":"OGXServer","name":"lsd-genai-playground","uid":"ogx-uid"}]}}`)
	f.putJSON(tRecipeDepList+"/far", `{"metadata":{"uid":"dep-far","ownerReferences":[{"kind":"ReplicaSet","name":"far","uid":"rs-far"}]}}`)
	f.putJSON(tRecipeRSList+"/far-2", `{"metadata":{"uid":"rs-far-2","ownerReferences":[{"kind":"Deployment","name":"far","uid":"dep-far"}]}}`)
	f.putJSON("/api/v1/namespaces/recipe-dev/pods/far-2-1", rs("far-2", "ReplicaSet", "rs-far-2"))
	// The ReplicaSet of a playground pod is gone.
	f.mu.Lock()
	delete(f.objects, fakeKey{gv: "apps/v1", plural: "replicasets", ns: "recipe-dev", name: "lsd-genai-playground-7f9b"})
	f.mu.Unlock()

	left, err := recipe.ProjectLeftovers(c, "recipe-dev")
	want := "Pod far-2-1,Pod loop-1,Pod lsd-genai-playground-7f9b-fghij,Pod mine,Pod orphan,Pod wrong-kind"
	if err != nil || strings.Join(left, ",") != want {
		t.Fatalf("leftovers %v %v", left, err)
	}
}

// A read the decision needs that fails refuses (fail closed): the
// playground record, the OGXServer, the ReplicaSets and the Deployments.
// Clean up then deletes nothing (the record shares the snapshot ConfigMap
// with the record of recipe projects, whose read refuses first).
func TestRecipeProjectLeftovers_PlaygroundReadsFailClosed(t *testing.T) {
	const unknownContents = "cannot tell what else is in project recipe-dev"
	for _, tc := range []struct{ name, path, want, refusal string }{
		{"the record", tSnapshotCM, "playground records", "read the record of the projects recipes created"},
		{"the OGXServer", tOGXServer, "OGXServer", unknownContents},
		{"the ReplicaSets", tRecipeRSList, "ReplicaSets", unknownContents},
		{"the Deployments", tRecipeDepList, "Deployments", unknownContents},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := projectFake(t)
			s := &recipe.State{Recipe: tRecipe(t, "create")}
			if res := ensureProject(t, c, s); !res.Success {
				t.Fatal(res.Message)
			}
			putRecipePlayground(f, "ogx-uid")
			recordRecipePlayground(t, c, "ogx-uid", true)
			f.fail["GET "+tc.path] = 500
			if left, err := recipe.ProjectLeftovers(c, "recipe-dev"); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("leftovers %v %v", left, err)
			}
			res, err := recipe.DeleteProject(c, "recipe-dev", s.Project.UID, []string{"Pod mine"})
			if err != nil || res.Success || !strings.Contains(res.Message, tc.refusal) {
				t.Fatalf("delete %+v %v", res, err)
			}
			if len(f.callsTo("DELETE", tRecipeNS)) != 0 {
				t.Fatal("deleted the project although a read failed")
			}
		})
	}
}

// The tool's pipeline server is a root too: a claim it owns, and now the
// pods of a Deployment it owns, are left out; a DSPA without the tool's
// label is no root.
func TestRecipeProjectLeftovers_PipelineServerRoots(t *testing.T) {
	f, c := projectFake(t)
	recordRecipePlayground(t, c, "", false)
	f.putJSON("/apis/datasciencepipelinesapplications.opendatahub.io/v1/namespaces/recipe-dev/datasciencepipelinesapplications/dspa", `{"metadata":{"uid":"dspa-uid","labels":{"app.kubernetes.io/managed-by":"rhoai-nightly-updater"}}}`)
	f.putJSON("/apis/datasciencepipelinesapplications.opendatahub.io/v1/namespaces/recipe-dev/datasciencepipelinesapplications/theirs", `{"metadata":{"uid":"theirs-uid"}}`)
	f.putJSON("/api/v1/namespaces/recipe-dev/persistentvolumeclaims/mariadb-dspa", `{"metadata":{"ownerReferences":[{"kind":"DataSciencePipelinesApplication","name":"dspa","uid":"dspa-uid"}]}}`)
	f.putJSON("/api/v1/namespaces/recipe-dev/persistentvolumeclaims/mariadb-theirs", `{"metadata":{"ownerReferences":[{"kind":"DataSciencePipelinesApplication","name":"theirs","uid":"theirs-uid"}]}}`)
	f.putJSON(tRecipeDepList+"/ds-pipeline-dspa", `{"metadata":{"uid":"dep-dspa","ownerReferences":[{"kind":"DataSciencePipelinesApplication","name":"dspa","uid":"dspa-uid","controller":true}]}}`)
	f.putJSON(tRecipeRSList+"/ds-pipeline-dspa-6c", `{"metadata":{"uid":"rs-dspa","ownerReferences":[{"kind":"Deployment","name":"ds-pipeline-dspa","uid":"dep-dspa","controller":true}]}}`)
	f.putJSON("/api/v1/namespaces/recipe-dev/pods/ds-pipeline-dspa-6c-x", `{"metadata":{"ownerReferences":[{"kind":"ReplicaSet","name":"ds-pipeline-dspa-6c","uid":"rs-dspa","controller":true}]}}`)
	left, err := recipe.ProjectLeftovers(c, "recipe-dev")
	if err != nil || strings.Join(left, ",") != "PersistentVolumeClaim mariadb-theirs" {
		t.Fatalf("leftovers %v %v", left, err)
	}
}
