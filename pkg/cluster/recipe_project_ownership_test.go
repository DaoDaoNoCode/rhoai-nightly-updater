package cluster

import (
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/features"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kueue"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/recipe"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/storage"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The playground's pieces in a recipe project, as RHOAI 3.7.0-ea.1 made
// them live: the OGX operator owns its Deployment lsd-genai-playground as
// the OGXServer's controller; the dashboard's gen-ai BFF makes the
// pgvector claim and Deployment and points them at the OGXServer
// (pgvector.SetOwnerReferences: no controller, blockOwnerDeletion false).
// Each Deployment owns a ReplicaSet, which owns the pod. The rule reads
// neither controller nor blockOwnerDeletion.
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
	f.putJSON(tRecipeDepList+"/genai-pgvector", `{"metadata":{"uid":"dep-pg","ownerReferences":[`+ogx+`,"blockOwnerDeletion":false}]}}`)
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
			res, err := recipe.DeleteProject(c, "recipe-dev", s.Project.UID, []string{"Pod mine"}, nil)
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

// The owners the first check finds count at the check just before the
// delete although they are gone by then: the playground's OGXServer
// deleted and its record dropped, and the garbage collector has removed
// one ReplicaSet but not yet its pod, the claim or the other pieces.
// Without them that check refuses.
func TestRecipeProject_DeleteKeepsTheFirstCheckOwners(t *testing.T) {
	f, c, uid := createdRecipeProject(t)
	putRecipePlayground(f, "ogx-uid")
	recordRecipePlayground(t, c, "ogx-uid", true)
	confirmed := []string{"Pod mine"}
	problem, _, left, owners, err := recipe.ProjectTeardownProblem(c, "recipe-dev", uid, confirmed, nil)
	if err != nil || problem != "" || strings.Join(left, ",") != "Pod mine" {
		t.Fatalf("first check %q %v %v", problem, left, err)
	}
	if strings.Join(owners, ",") != "Deployment/dep-lsd,Deployment/dep-pg,OGXServer/ogx-uid,ReplicaSet/rs-lsd,ReplicaSet/rs-pg" {
		t.Fatalf("owners %v", owners)
	}
	f.deleteObject(tOGXServer)
	recordRecipePlayground(t, c, "", false)
	f.deleteObject(tRecipeRSList + "/lsd-genai-playground-7f9b")

	res, err := recipe.DeleteProject(c, "recipe-dev", uid, confirmed, nil)
	if err != nil || res.Success || res.ErrorCode != recipe.ErrorCodeConfirmContents || !strings.Contains(res.Message, "PersistentVolumeClaim genai-pgvector-storage") {
		t.Fatalf("without the first check's owners: %+v %v", res, err)
	}
	// An owner UID only: a server made again under the name is not the tool's.
	f.putJSON(tOGXServer, `{"metadata":{"uid":"new-ogx-uid"}}`)
	f.putJSON("/api/v1/namespaces/recipe-dev/persistentvolumeclaims/theirs", `{"metadata":{"ownerReferences":[{"kind":"OGXServer","name":"lsd-genai-playground","uid":"new-ogx-uid"}]}}`)
	res, err = recipe.DeleteProject(c, "recipe-dev", uid, confirmed, owners)
	if err != nil || res.Success || res.ErrorCode != recipe.ErrorCodeConfirmContents || res.Message != "Project recipe-dev still holds what this tool did not create: PersistentVolumeClaim theirs. Deleting the project deletes them too; confirm them to go ahead." {
		t.Fatalf("a server made again: %+v %v", res, err)
	}
	if len(f.callsTo("DELETE", tRecipeNS)) != 0 {
		t.Fatal("deleted with unconfirmed contents")
	}
	f.deleteObject("/api/v1/namespaces/recipe-dev/persistentvolumeclaims/theirs")
	res, err = recipe.DeleteProject(c, "recipe-dev", uid, confirmed, owners)
	if err != nil || !res.Success || f.has(tRecipeNS) {
		t.Fatalf("with the first check's owners: %+v %v", res, err)
	}
}

// Clean up everything with a part that deletes an owner in a recipe
// project (the playground, the pipeline server, the model) and the project: the
// part's dependents the garbage collector has not removed yet do not stop
// the project's delete, as the first check found their owner; a pod
// someone made in the project between the two checks still does.
func TestCleanUp_RecipeProjectKeepsTheOwnersOfEarlierParts(t *testing.T) {
	const dspaPath = "/apis/datasciencepipelinesapplications.opendatahub.io/v1/namespaces/recipe-dev/datasciencepipelinesapplications/dspa"
	const llmPath = "/apis/serving.kserve.io/v1alpha2/namespaces/recipe-dev/llminferenceservices/smol"
	for _, tc := range []struct {
		name     string
		pipeline bool
		model    bool
		intruder bool
	}{
		{name: "the playground"},
		{name: "the pipeline server", pipeline: true},
		{name: "the model", model: true},
		{name: "a pod made between the checks", intruder: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c, uid := createdRecipeProject(t)
			pg, ps, wp, md := cleanupPlayground, cleanupPipelineServer, pipelineWorkbenchesProblem, cleanupModels
			t.Cleanup(func() {
				cleanupPlayground, cleanupPipelineServer, pipelineWorkbenchesProblem, cleanupModels = pg, ps, wp, md
			})
			req := types.CleanupRequest{RecipeProjects: []string{"recipe-dev"}, RecipeProjectUIDs: map[string]string{"recipe-dev": uid},
				ConfirmProjectContents: map[string][]string{"recipe-dev": {"Pod mine"}}}
			var ran []string
			switch {
			case tc.model:
				// As KServe runs the tool's model (live RHOAI 3.7): Deployment
				// smol-kserve, owned by the LLMInferenceService, and its
				// ReplicaSet's pod with KServe's labels.
				recordRecipePlayground(t, c, "", false)
				f.putJSON("/api/v1/namespaces/recipe-dev/pods/mine", `{}`)
				f.putJSON(llmPath, `{"metadata":{"uid":"llm-uid","labels":{"app.kubernetes.io/managed-by":"rhoai-nightly-updater"}}}`)
				f.putJSON(tRecipeDepList+"/smol-kserve", `{"metadata":{"uid":"dep-smol","ownerReferences":[{"apiVersion":"serving.kserve.io/v1alpha2","kind":"LLMInferenceService","name":"smol","uid":"llm-uid","controller":true,"blockOwnerDeletion":true}]}}`)
				f.putJSON(tRecipeRSList+"/smol-kserve-9bb6646f9", `{"metadata":{"uid":"rs-smol","ownerReferences":[{"kind":"Deployment","name":"smol-kserve","uid":"dep-smol","controller":true}]}}`)
				f.putJSON("/api/v1/namespaces/recipe-dev/pods/smol-kserve-9bb6646f9-wzrps", `{"metadata":{"labels":{"app.kubernetes.io/part-of":"llminferenceservice","app.kubernetes.io/name":"smol"},"ownerReferences":[{"kind":"ReplicaSet","name":"smol-kserve-9bb6646f9","uid":"rs-smol","controller":true}]}}`)
				req.Models = []string{"recipe-dev"}
				cleanupModels = func(_ *Client, project string) (*types.OperationResponse, error) {
					ran = append(ran, "model:"+project)
					f.deleteObject(llmPath)
					return &types.OperationResponse{Success: true, Message: "Model deleted."}, nil
				}
			case tc.pipeline:
				recordRecipePlayground(t, c, "", false)
				f.putJSON("/api/v1/namespaces/recipe-dev/pods/mine", `{}`)
				f.putJSON(dspaPath, `{"metadata":{"uid":"dspa-uid","labels":{"app.kubernetes.io/managed-by":"rhoai-nightly-updater"}}}`)
				f.putJSON("/api/v1/namespaces/recipe-dev/persistentvolumeclaims/mariadb-dspa", `{"metadata":{"ownerReferences":[{"kind":"DataSciencePipelinesApplication","name":"dspa","uid":"dspa-uid"}]}}`)
				f.putJSON(tRecipeDepList+"/ds-pipeline-dspa", `{"metadata":{"uid":"dep-dspa","ownerReferences":[{"kind":"DataSciencePipelinesApplication","name":"dspa","uid":"dspa-uid","controller":true}]}}`)
				f.putJSON(tRecipeRSList+"/ds-pipeline-dspa-6c", `{"metadata":{"uid":"rs-dspa","ownerReferences":[{"kind":"Deployment","name":"ds-pipeline-dspa","uid":"dep-dspa","controller":true}]}}`)
				f.putJSON("/api/v1/namespaces/recipe-dev/pods/ds-pipeline-dspa-6c-x", `{"metadata":{"labels":{"dspa":"dspa"},"ownerReferences":[{"kind":"ReplicaSet","name":"ds-pipeline-dspa-6c","uid":"rs-dspa","controller":true}]}}`)
				req.PipelineServers = []string{"recipe-dev"}
				pipelineWorkbenchesProblem = func(*Client, string, map[string]string) (string, string, []storage.ConnectionWorkbench) {
					return "", "", nil
				}
				cleanupPipelineServer = func(_ *Client, project, _ string, _ map[string]string) (*types.OperationResponse, error) {
					ran = append(ran, "pipeline-server:"+project)
					f.deleteObject(dspaPath)
					return &types.OperationResponse{Success: true, Message: "Pipeline server deleted."}, nil
				}
			default:
				putRecipePlayground(f, "ogx-uid")
				recordRecipePlayground(t, c, "ogx-uid", true)
				req.Playgrounds = []string{"recipe-dev"}
				cleanupPlayground = func(c *Client, project string) (*types.OperationResponse, error) {
					ran = append(ran, "playground:"+project)
					// As the dashboard's delete and the wait for it leave it:
					// the server and the record gone, the rest not yet.
					f.deleteObject(tOGXServer)
					recordRecipePlayground(t, c, "", false)
					if tc.intruder {
						f.putJSON("/api/v1/namespaces/recipe-dev/pods/intruder", `{}`)
					}
					return &types.OperationResponse{Success: true, Message: "Playground deleted."}, nil
				}
			}
			res, err := CleanUp(c, req, nil)
			if err != nil || len(ran) != 1 {
				t.Fatalf("got %+v, %v, ran %v", res, err, ran)
			}
			if tc.intruder {
				if res.Success || !strings.Contains(res.Message, "still holds what this tool did not create: Pod intruder.") || strings.Contains(res.Message, "genai-pgvector") {
					t.Fatalf("got %+v", res)
				}
				if len(f.callsTo("DELETE", tRecipeNS)) != 0 || !f.has(tRecipeNS) {
					t.Fatal("deleted the project with a pod nobody confirmed")
				}
				return
			}
			if !res.Success || f.has(tRecipeNS) {
				t.Fatalf("got %+v, project still there: %v", res, f.has(tRecipeNS))
			}
		})
	}
}

// The answer for an owner does not depend on the path that met it first:
// Deployment x leads through Deployment y to the server in three
// references from the pod (Pod, x, y), but ReplicaSet a, which the pod
// also names, reaches x only at the bound, so a's path is one reference
// too long and the pod stays listed, in either order of its references.
// A walk that skips an owner already met would take a's x as done in one
// of the orders.
func TestRecipeProjectLeftovers_ReferenceOrderDoesNotMatter(t *testing.T) {
	f, c := projectFake(t)
	putRecipePlayground(f, "ogx-uid")
	recordRecipePlayground(t, c, "ogx-uid", true)
	f.putJSON(tRecipeRSList+"/a", `{"metadata":{"uid":"rs-a","ownerReferences":[{"kind":"Deployment","name":"x","uid":"dep-x"}]}}`)
	f.putJSON(tRecipeDepList+"/x", `{"metadata":{"uid":"dep-x","ownerReferences":[{"kind":"Deployment","name":"y","uid":"dep-y"}]}}`)
	f.putJSON(tRecipeDepList+"/y", `{"metadata":{"uid":"dep-y","ownerReferences":[{"kind":"OGXServer","name":"lsd-genai-playground","uid":"ogx-uid"}]}}`)
	depX := `{"kind":"Deployment","name":"x","uid":"dep-x"}`
	rsA := `{"kind":"ReplicaSet","name":"a","uid":"rs-a"}`
	f.putJSON("/api/v1/namespaces/recipe-dev/pods/x-first", `{"metadata":{"ownerReferences":[`+depX+`,`+rsA+`]}}`)
	f.putJSON("/api/v1/namespaces/recipe-dev/pods/a-first", `{"metadata":{"ownerReferences":[`+rsA+`,`+depX+`]}}`)
	f.putJSON("/api/v1/namespaces/recipe-dev/pods/x-only", `{"metadata":{"ownerReferences":[`+depX+`]}}`)
	left, err := recipe.ProjectLeftovers(c, "recipe-dev")
	if err != nil || strings.Join(left, ",") != "Pod a-first,Pod mine,Pod x-first" {
		t.Fatalf("leftovers %v %v", left, err)
	}
}

// An object is the tool's only when every one of its references leads to
// a root, as the garbage collector keeps an object while any owner is
// there: a claim owned by the server and a user's Notebook stays listed,
// a claim owned by the server alone does not.
func TestRecipeProjectLeftovers_EveryReferenceMustLeadToARoot(t *testing.T) {
	f, c := projectFake(t)
	putRecipePlayground(f, "ogx-uid")
	recordRecipePlayground(t, c, "ogx-uid", true)
	f.putJSON("/apis/kubeflow.org/v1/namespaces/recipe-dev/notebooks/my-wb", `{"metadata":{"uid":"nb-uid"}}`)
	ogx := `{"kind":"OGXServer","name":"lsd-genai-playground","uid":"ogx-uid"}`
	f.putJSON("/api/v1/namespaces/recipe-dev/persistentvolumeclaims/shared", `{"metadata":{"ownerReferences":[{"kind":"Notebook","name":"my-wb","uid":"nb-uid"},`+ogx+`]}}`)
	f.putJSON("/api/v1/namespaces/recipe-dev/persistentvolumeclaims/servers", `{"metadata":{"ownerReferences":[`+ogx+`]}}`)
	left, err := recipe.ProjectLeftovers(c, "recipe-dev")
	if err != nil || strings.Join(left, ",") != "Notebook my-wb,PersistentVolumeClaim shared,Pod mine" {
		t.Fatalf("leftovers %v %v", left, err)
	}
}

// A reference counts only with the root's kind as well as its UID: the
// server's UID named as a Deployment, or a RayCluster's as a RayJob, does
// not reach the root.
func TestRecipeProjectLeftovers_RootsMatchByKind(t *testing.T) {
	f, c, uid := createdRecipeProject(t)
	putRecipePlayground(f, "ogx-uid")
	recordRecipePlayground(t, c, "ogx-uid", true)
	recordRecipeRay(t, c, "recipe-dev", uid)
	f.putJSON("/api/v1/namespaces/recipe-dev/persistentvolumeclaims/other-kind", `{"metadata":{"ownerReferences":[{"kind":"Deployment","name":"lsd-genai-playground","uid":"ogx-uid"}]}}`)
	f.putJSON("/api/v1/namespaces/recipe-dev/pods/ray-other-kind", `{"metadata":{"ownerReferences":[{"kind":"RayJob","name":"nightly-raycluster","uid":"rc-uid"}]}}`)
	left, err := recipe.ProjectLeftovers(c, "recipe-dev")
	if err != nil || strings.Join(left, ",") != "PersistentVolumeClaim other-kind,Pod mine,Pod ray-other-kind" {
		t.Fatalf("leftovers %v %v", left, err)
	}
}

// recordRecipeRay records the distributed workloads setup's Ray samples
// (RayCluster rc-uid, RayJob rj-uid) for project (with projectUID) as its
// setup does.
func recordRecipeRay(t *testing.T, c *Client, project, projectUID string) {
	t.Helper()
	if err := record.WriteSnapshotKey(c, kueue.RecordKey, "test", `{"project":"`+project+`","projectUID":"`+projectUID+`","rayClusterUID":"rc-uid","rayJobUID":"rj-uid"}`); err != nil {
		t.Fatal(err)
	}
}

const tRayJob = "/apis/batch/v1/namespaces/recipe-dev/jobs/nightly-rayjob"

// putRecipeRayPods puts the Ray samples' pods as KubeRay runs them (live
// RHOAI 3.7): the RayCluster sample's head and worker owned by it; the
// RayJob's submitter, marked by KubeRay's originated-from labels, owned by
// Job nightly-rayjob, which the RayJob owns; and the head of the RayJob's
// own RayCluster (nightly-rayjob-abcde, which the RayJob controls and this
// tool does not record), which stays listed.
func putRecipeRayPods(f *resourceFake) {
	rc := `{"metadata":{"ownerReferences":[{"apiVersion":"ray.io/v1","kind":"RayCluster","name":"nightly-raycluster","uid":"rc-uid","controller":true}]}}`
	f.putJSON("/api/v1/namespaces/recipe-dev/pods/nightly-raycluster-head-7xk2p", rc)
	f.putJSON("/api/v1/namespaces/recipe-dev/pods/nightly-raycluster-small-group-worker-q8w4m", rc)
	f.putJSON(tRayJob, `{"metadata":{"uid":"job-uid","ownerReferences":[{"apiVersion":"ray.io/v1","kind":"RayJob","name":"nightly-rayjob","uid":"rj-uid","controller":true}]}}`)
	f.putJSON("/api/v1/namespaces/recipe-dev/pods/nightly-rayjob-9z5kd", `{"metadata":{"labels":{"ray.io/originated-from-crd":"RayJob","ray.io/originated-from-cr-name":"nightly-rayjob"},"ownerReferences":[{"apiVersion":"batch/v1","kind":"Job","name":"nightly-rayjob","uid":"job-uid","controller":true}]}}`)
	f.putJSON("/api/v1/namespaces/recipe-dev/pods/nightly-rayjob-abcde-head-k2m9x", `{"metadata":{"labels":{"ray.io/originated-from-crd":"RayJob","ray.io/originated-from-cr-name":"nightly-rayjob"},"ownerReferences":[{"apiVersion":"ray.io/v1","kind":"RayCluster","name":"nightly-rayjob-abcde","uid":"rjc-uid","controller":true}]}}`)
	f.putJSON("/api/v1/namespaces/recipe-dev/pods/mine", `{}`)
}

// The Ray samples of the tool's distributed workloads setup are roots by
// the UIDs its record holds, only when the record is of this project (and
// of this namespace, by UID, when it says): their head, worker and
// submitter pods are left out. The RayJob's own RayCluster is not
// recognised, so its head stays listed. A Job that is not there leaves the
// submitter listed; a Job read that fails refuses.
func TestRecipeProjectLeftovers_RaySamples(t *testing.T) {
	const (
		rayJobCluster = "Pod nightly-rayjob-abcde-head-k2m9x"
		rayPods       = "Pod mine,Pod nightly-raycluster-head-7xk2p,Pod nightly-raycluster-small-group-worker-q8w4m,Pod nightly-rayjob-9z5kd," + rayJobCluster
	)
	for _, tc := range []struct {
		name, project, projectUID, want string
		noJob                           bool
		jobFails                        bool
	}{
		{name: "the record of this project", project: "recipe-dev", projectUID: "<ns>", want: "Pod mine," + rayJobCluster},
		{name: "a record without the project's UID", project: "recipe-dev", want: "Pod mine," + rayJobCluster},
		{name: "a record of another project", project: "other", want: rayPods},
		{name: "a record of the project before it was made again", project: "recipe-dev", projectUID: "old-ns-uid", want: rayPods},
		{name: "the Job is gone", project: "recipe-dev", projectUID: "<ns>", noJob: true, want: "Pod mine,Pod nightly-rayjob-9z5kd," + rayJobCluster},
		{name: "the Job cannot be read", project: "recipe-dev", projectUID: "<ns>", jobFails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c, uid := createdRecipeProject(t)
			recordRecipePlayground(t, c, "", false)
			putRecipeRayPods(f)
			projectUID := tc.projectUID
			if projectUID == "<ns>" {
				projectUID = uid
			}
			recordRecipeRay(t, c, tc.project, projectUID)
			if tc.noJob {
				f.deleteObject(tRayJob)
			}
			if tc.jobFails {
				f.fail["GET "+tRayJob] = 500
			}
			left, err := recipe.ProjectLeftovers(c, "recipe-dev")
			if tc.jobFails {
				if err == nil || !strings.Contains(err.Error(), "read Job recipe-dev/nightly-rayjob") {
					t.Fatalf("leftovers %v %v", left, err)
				}
				return
			}
			if err != nil || strings.Join(left, ",") != tc.want {
				t.Fatalf("leftovers %v %v", left, err)
			}
			if jobReads := len(f.callsTo("GET", tRayJob)); (jobReads > 0) != (tc.project == "recipe-dev" && tc.projectUID != "old-ns-uid") {
				t.Fatalf("read the Job %d times", jobReads)
			}
		})
	}
}

// Clean up everything with the distributed workloads setup and the recipe
// project: the setup's part deletes the Ray samples and clears its record
// first, and the head, worker and submitter pods the garbage collector has
// not removed yet do not stop the project's delete.
func TestCleanUp_RecipeProjectAfterTheRaySamples(t *testing.T) {
	f, c, uid := createdRecipeProject(t)
	recordRecipePlayground(t, c, "", false)
	putRecipeRayPods(f)
	recordRecipeRay(t, c, "recipe-dev", uid)
	kq := cleanupKueue
	t.Cleanup(func() { cleanupKueue = kq })
	ran := 0
	cleanupKueue = func(c *Client) (*types.OperationResponse, error) {
		ran++
		f.deleteObject(tRayJob)
		if err := record.WriteSnapshotKey(c, kueue.RecordKey, "test", `{}`); err != nil {
			return nil, err
		}
		return &types.OperationResponse{Success: true, Message: "Distributed workloads setup deleted."}, nil
	}
	res, err := CleanUp(c, types.CleanupRequest{Kueue: true, RecipeProjects: []string{"recipe-dev"}, RecipeProjectUIDs: map[string]string{"recipe-dev": uid},
		ConfirmProjectContents: map[string][]string{"recipe-dev": {"Pod mine", "Pod nightly-rayjob-abcde-head-k2m9x"}}}, nil)
	if err != nil || !res.Success || ran != 1 || f.has(tRecipeNS) {
		t.Fatalf("got %+v, %v, kueue ran %d, project still there: %v", res, err, ran, f.has(tRecipeNS))
	}
}

// Only the tool's RayJob's submitter Job (named after the RayJob) is read,
// once, and only for a pod KubeRay marks as its submitter that nothing else
// leaves out: an ordinary batch pod, a submitter naming another Job, and
// labelled submitters that carry the tool's label or are being deleted
// send no Job request.
func TestRecipeProjectLeftovers_JobReadsOnlyForRayJobSubmitters(t *testing.T) {
	f, c, uid := createdRecipeProject(t)
	recordRecipePlayground(t, c, "", false)
	recordRecipeRay(t, c, "recipe-dev", uid)
	submitter := `"ray.io/originated-from-crd":"RayJob","ray.io/originated-from-cr-name":"nightly-rayjob"`
	f.putJSON("/apis/batch/v1/namespaces/recipe-dev/jobs/backup", `{"metadata":{"uid":"backup-uid"}}`)
	f.putJSON("/api/v1/namespaces/recipe-dev/pods/backup-x7k2p", `{"metadata":{"ownerReferences":[{"kind":"Job","name":"backup","uid":"backup-uid"}]}}`)
	f.putJSON("/api/v1/namespaces/recipe-dev/pods/other-job", `{"metadata":{"labels":{`+submitter+`},"ownerReferences":[{"kind":"Job","name":"backup","uid":"backup-uid"}]}}`)
	f.putJSON("/api/v1/namespaces/recipe-dev/pods/tool-labelled", `{"metadata":{"labels":{`+submitter+`,"app.kubernetes.io/managed-by":"rhoai-nightly-updater"},"ownerReferences":[{"kind":"Job","name":"nightly-rayjob","uid":"job-uid"}]}}`)
	f.putJSON("/api/v1/namespaces/recipe-dev/pods/going", `{"metadata":{"labels":{`+submitter+`},"deletionTimestamp":"2026-11-25T10:00:00Z","ownerReferences":[{"kind":"Job","name":"nightly-rayjob","uid":"job-uid"}]}}`)
	left, err := recipe.ProjectLeftovers(c, "recipe-dev")
	if err != nil || strings.Join(left, ",") != "Pod backup-x7k2p,Pod other-job" {
		t.Fatalf("leftovers %v %v", left, err)
	}
	for _, call := range f.calls {
		if strings.Contains(call.Path, "/jobs") {
			t.Fatalf("read a Job: %s %s", call.Method, call.Path)
		}
	}
}

// A reference to the submitter Job counts only with the Job's name as well
// as its UID: a second pod naming the same UID under another (invalid)
// name stays listed, and the Job is read once.
func TestRecipeProjectLeftovers_JobReferenceNeedsTheJobsName(t *testing.T) {
	f, c, uid := createdRecipeProject(t)
	recordRecipePlayground(t, c, "", false)
	recordRecipeRay(t, c, "recipe-dev", uid)
	putRecipeRayPods(f)
	f.putJSON("/api/v1/namespaces/recipe-dev/pods/same-uid", `{"metadata":{"labels":{"ray.io/originated-from-crd":"RayJob","ray.io/originated-from-cr-name":"nightly-rayjob"},"ownerReferences":[{"kind":"Job","name":"../../secrets/x","uid":"job-uid"}]}}`)
	f.putJSON("/api/v1/namespaces/recipe-dev/pods/nightly-rayjob-second", `{"metadata":{"labels":{"ray.io/originated-from-crd":"RayJob","ray.io/originated-from-cr-name":"nightly-rayjob"},"ownerReferences":[{"kind":"Job","name":"nightly-rayjob","uid":"job-uid"}]}}`)
	left, err := recipe.ProjectLeftovers(c, "recipe-dev")
	if err != nil || strings.Join(left, ",") != "Pod mine,Pod nightly-rayjob-abcde-head-k2m9x,Pod same-uid" {
		t.Fatalf("leftovers %v %v", left, err)
	}
	var reads []string
	for _, call := range f.calls {
		if strings.Contains(call.Path, "/jobs") {
			reads = append(reads, call.Method+" "+call.Path)
		}
	}
	if len(reads) != 1 || reads[0] != "GET "+tRayJob {
		t.Fatalf("Job requests %v", reads)
	}
}
