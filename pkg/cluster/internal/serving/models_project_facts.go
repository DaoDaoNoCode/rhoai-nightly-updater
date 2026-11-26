package serving

import (
	"context"
	"fmt"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/authz"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The cluster facts of a project's model action (the non-admin design,
// section 3.2: "Model fits the cluster"; and whether a MaaSModelRef in the
// project targets a model, before its delete). A
// user's project action reads them with the facts client (c.Facts(), the
// ServiceAccount's read-only copy), because a project user's token may not
// list the cluster's Gateways, nodes or pods. What is read lists other
// tenants underneath (the Gateway's allowed projects, every node and pod),
// so the answer is reduced to the requested project: yes or no with a
// fixed code, fits or may not fit. Neither names another project, a node,
// a pod or a gateway.

// The answers of ModelCapacityFact.
const (
	CapacityFits      = "fits"
	CapacityMayNotFit = "may-not-fit"
	CapacityUnknown   = "unknown"
)

// ProjectModelExposures is a user's answer of GET
// /api/resources/models/exposures for one of their projects (pkg/api
// checks the project with the projects API and their token first): the
// one way a project model action reaches model, only inside the cluster,
// whether the cluster has room for it (a verdict, read with u.Facts()),
// and which image a deploy runs (the plan's kind and reason, read with
// u.Facts()). A model reached through the inference gateway is a
// cluster-admin's (ClusterAdminModelWords). It names no other project,
// node or pod. A client of the ServiceAccount's token is refused before
// any request (authz.ErrNotUserClient).
func ProjectModelExposures(u *kube.Client, project, model string) (*types.ModelExposures, string, string) {
	if !u.ActsAsUser() {
		return nil, authz.ErrNotUserClient.Error(), "internal"
	}
	entry, ok := CatalogEntry(model)
	if !ok {
		return nil, "unknown model", "validation"
	}
	if _, msg, code := ProjectNamespace(u, project); msg != "" {
		return nil, msg, code
	}
	in := types.ModelExposureOption{Key: ExposureInternal, Exposure: ExposureInternal, Available: true, Title: "Only inside the cluster",
		Description: InternalDescription,
		Warning:     "Calls to the in-cluster address are not authenticated: any pod on the cluster whose network can reach the project can use the model."}
	out := &types.ModelExposures{Project: project, Model: entry.Name, Options: []types.ModelExposureOption{in},
		Default: ExposureInternal, DefaultReason: ClusterAdminModelWords, Capacity: ModelCapacityFact(u, entry, project)}
	// The image a deploy runs (the setup's own plan, without the lookup):
	// the preset and the plan are cluster facts, read with the facts
	// client; only the plan's kind and reason are given.
	if preset := discoverCPUPreset(u.Facts()); preset.problem == "" {
		plan := statusImagePlan(u.Facts(), preset.image)
		out.ImagePlan, out.ImagePlanReason = plan.Kind, plan.Reason
	}
	return out, "", ""
}

// ProjectMaaSRefFact is whether some MaaSModelRef in project, anyone's,
// targets the LLMInferenceService model (spec.modelRef): a publication to
// MaaS someone made without this tool, which a project's delete must not
// leave pointing at nothing. It is read with c.Facts() (the
// ServiceAccount's list of MaaSModelRefs, in that project only) and
// reduced to yes or no: no name, no other project. A cluster without the
// MaaS API has none; any other failed read is an error (callers refuse:
// fail closed), logged by status only.
func ProjectMaaSRefFact(c *kube.Client, project, model string) (bool, error) {
	targeted, err := maasRefTargets(c.Facts(), project, model)
	if err != nil {
		kube.LogFactReadFailure("cannot read whether a MaaSModelRef targets a project's model", "MaaSModelRefs", err)
	}
	return targeted, err
}

// maasRefTargets lists project's MaaSModelRefs with f (the facts client)
// and says whether one targets model.
func maasRefTargets(f *kube.Client, project, model string) (bool, error) {
	var list struct {
		Items []struct {
			Spec struct {
				ModelRef struct {
					Kind string `json:"kind"`
					Name string `json:"name"`
				} `json:"modelRef"`
			} `json:"spec"`
		} `json:"items"`
	}
	found, err := f.GetIntoOptional(rhoai.NamespacedPath(MaaSAPI(f), "maasmodelrefs", project, ""), &list)
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}
	for _, it := range list.Items {
		if it.Spec.ModelRef.Name == model && (it.Spec.ModelRef.Kind == "" || it.Spec.ModelRef.Kind == "LLMInferenceService") {
			return true, nil
		}
	}
	return false, nil
}

// The cluster's capacity, shared by every project's model action: one
// snapshot of the nodes and running pods per cluster, kept
// capacitySnapshotTTL, read with the facts client; concurrent refreshes
// are one read (capacityFlights). Each verdict is computed from it.
var (
	capacitySnapshots = kube.NewLRU[string, *capacitySnapshot](4)
	capacityFlights   kube.FlightGroup[*capacitySnapshot]
)

const capacitySnapshotTTL = 30 * time.Second

// sharedCapacity is the cluster's capacity snapshot, read with f (the
// facts client) when the kept one is older than capacitySnapshotTTL.
func sharedCapacity(f *kube.Client) (*capacitySnapshot, error) {
	key := f.BaseURL()
	if s, ok := capacitySnapshots.Get(key); ok {
		return s, nil
	}
	s, err, _ := capacityFlights.Do(f.Context(), key, func(context.Context) (*capacitySnapshot, error) {
		if s, ok := capacitySnapshots.Get(key); ok {
			return s, nil
		}
		s, err := readCapacity(f)
		if err == nil {
			capacitySnapshots.Add(key, s, capacitySnapshotTTL)
		}
		return s, err
	})
	return s, err
}

// ModelCapacityFact is whether some schedulable amd64 worker has the
// model's CPU, memory and download requests free ("fits"), none seems to
// ("may-not-fit"), or it cannot be told ("unknown"). The model's own pods
// in project are not counted (setting it up again replaces them). It reads
// the nodes and pods with c.Facts() and returns only the answer: no node,
// pod or project name.
func ModelCapacityFact(c *kube.Client, entry CatalogModel, project string) string {
	snap, err := sharedCapacity(c.Facts())
	if err != nil {
		kube.LogFactReadFailure("cannot read the nodes and pods for a project's model", "capacity", err)
		return CapacityUnknown
	}
	if RoomWarning(entry, snap.room(project, entry.Name)) != "" {
		return CapacityMayNotFit
	}
	return CapacityFits
}

// CapacityFactWords says a "may-not-fit" answer for people, from the
// catalog's requests only.
func CapacityFactWords(entry CatalogModel, fact string) string {
	if fact != CapacityMayNotFit {
		return ""
	}
	return fmt.Sprintf("No worker of the cluster seems to have room for %s: it needs %s CPUs and %s of memory free. It may stay pending until room frees up or a cluster-admin adds a node.", entry.DisplayName, entry.CPU, memoryWords(entry.MemoryRequest))
}
