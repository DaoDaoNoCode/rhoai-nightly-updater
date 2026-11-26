package serving

import (
	"fmt"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/authz"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The cluster facts of a project's model action (the non-admin design,
// section 3.2: "Gateway admits project P", "Model fits the cluster"). A
// user's project action reads them with the facts client (c.Facts(), the
// ServiceAccount's read-only copy), because a project user's token may not
// list the cluster's Gateways, nodes or pods. What is read lists other
// tenants underneath (the Gateway's allowed projects, every node and pod),
// so the answer is reduced to the requested project: yes or no with a
// fixed code, fits or may not fit. Neither names another project, a node,
// a pod or a gateway.

// The codes of types.ProjectGatewayFact.Problem.
const (
	GatewayNotConfigured    = "not-configured"
	GatewayMissing          = "missing"
	GatewayNotReady         = "not-ready"
	GatewayNotForModels     = "not-for-models"
	GatewayNotAdmitted      = "not-admitted"
	GatewayAdmissionUnknown = "admission-unknown"
	GatewayAuthUnknown      = "auth-unknown"
	GatewayAuthMissing      = "auth-missing"
	GatewayUnreadable       = "unreadable"
)

// The answers of ModelCapacityFact.
const (
	CapacityFits      = "fits"
	CapacityMayNotFit = "may-not-fit"
	CapacityUnknown   = "unknown"
)

// ProjectGatewayFact is whether the cluster's default inference gateway
// (the one KServe's configuration names, inferenceservice-config's
// ingress.kserveIngressGateway) admits project, whose namespace labels are
// labels (read with the user's token), and can carry a model with token
// authentication now: it exists and is programmed, it is not MaaS's or the
// operator's, its listeners let the project's routes attach (evaluated
// here, for this project only), RHOAI adds its token checks there
// (openshift-ingress, not labelled opendatahub.io/managed=false), and Red
// Hat Connectivity Link is set up. Everything is read with c.Facts(). host
// is the gateway's external hostname when it admits the project ("" when
// it has none): the cluster's inference address, not another tenant's.
// A read that fails is a no ("unreadable", fail closed); its text is
// logged by status only.
func ProjectGatewayFact(c *kube.Client, project string, labels map[string]string) (fact types.ProjectGatewayFact, host string) {
	// Each read is passed c.Facts() itself: the RBAC scan takes the
	// functions a facts client is passed to as the ServiceAccount's.
	no := func(code string) (types.ProjectGatewayFact, string) {
		return types.ProjectGatewayFact{Problem: code}, ""
	}
	def, err := readKServeGateway(c.Facts())
	if err != nil {
		kube.LogFactReadFailure("cannot read KServe's default gateway for a project's model", "inferenceservice-config", err)
		return no(GatewayUnreadable)
	}
	if !def.Known {
		return no(GatewayNotConfigured)
	}
	gws, err := listAllGateways(c.Facts())
	if err != nil {
		kube.LogFactReadFailure("cannot list the Gateways for a project's model", "Gateways", err)
		return no(GatewayUnreadable)
	}
	g, found := findGateway(gws, def.Namespace, def.Name)
	switch {
	case !found:
		return no(GatewayMissing)
	case gatewayExclusion(g) != "":
		return no(GatewayNotForModels)
	case gatewayProblem(g) != "":
		return no(GatewayNotReady)
	}
	switch g.Admits(project, labels) {
	case "no":
		return no(GatewayNotAdmitted)
	case "unknown":
		return no(GatewayAdmissionUnknown)
	}
	if g.Metadata.Labels[ODHManagedLabel] == "false" || g.Metadata.Namespace != inferenceGatewayNamespace {
		return no(GatewayAuthUnknown)
	}
	ok, _, err := authReadiness(c.Facts())
	if err != nil {
		kube.LogFactReadFailure("cannot read whether Connectivity Link is set up for a project's model", "Kuadrant", err)
		return no(GatewayUnreadable)
	}
	if !ok {
		return no(GatewayAuthMissing)
	}
	return types.ProjectGatewayFact{Admits: true}, g.externalHost()
}

// GatewayFactWords says fact for project's user, with who can change it.
func GatewayFactWords(project string, fact types.ProjectGatewayFact) string {
	switch fact.Problem {
	case "":
		return ""
	case GatewayNotConfigured:
		return "KServe's configuration names no default inference gateway on this cluster, so a model cannot be reached through it. A cluster-admin sets one up."
	case GatewayMissing:
		return "The cluster's inference gateway is not set up. A cluster-admin sets it up (Models tab)."
	case GatewayNotReady:
		return "The cluster's inference gateway cannot carry a model now. A cluster-admin checks it (Models tab)."
	case GatewayNotForModels:
		return "KServe's default gateway on this cluster is not one for models. A cluster-admin sets up the inference gateway."
	case GatewayNotAdmitted:
		return fmt.Sprintf("The cluster's inference gateway does not admit project %s. A cluster-admin can allow project %s on the inference gateway (Models tab, Allow).", project, project)
	case GatewayAdmissionUnknown:
		return fmt.Sprintf("Whether the cluster's inference gateway admits project %s cannot be told (it chooses projects in a way this tool does not evaluate). A cluster-admin can allow project %s on it.", project, project)
	case GatewayAuthUnknown:
		return "Whether the cluster's inference gateway checks the callers' tokens cannot be told, so this tool does not route a project's model through it. A cluster-admin decides how models are reached there."
	case GatewayAuthMissing:
		return "Red Hat Connectivity Link, which checks the callers' tokens on the inference gateway, is not set up. A cluster-admin sets it up (Models tab)."
	}
	return "Whether the cluster's inference gateway admits this project cannot be checked now; try again later."
}

// ProjectModelExposures is a user's answer of GET
// /api/resources/models/exposures for one of their projects (pkg/api
// checks the project with the projects API and their token first): the
// ways a project model action can reach model there. "Only inside the
// cluster" is always there; the cluster's inference gateway only when it
// already admits the project (ProjectGatewayFact), and it needs token
// authentication. MaaS and other gateways are a cluster-admin's (the
// Models tab). The project's labels are read with u (the user's token),
// the gateway and the room with u.Facts(); the answer names no other
// project, node, pod or gateway. A client of the ServiceAccount's token is
// refused before any request (authz.ErrNotUserClient).
func ProjectModelExposures(u *kube.Client, project, model string) (*types.ModelExposures, string, string) {
	if !u.ActsAsUser() {
		return nil, authz.ErrNotUserClient.Error(), "internal"
	}
	entry, ok := CatalogEntry(model)
	if !ok {
		return nil, "unknown model", "validation"
	}
	labels, msg, code := ProjectNamespace(u, project)
	if msg != "" {
		return nil, msg, code
	}
	fact, host := ProjectGatewayFact(u, project, labels)
	out := &types.ModelExposures{Project: project, Model: entry.Name, Gateway: &fact, Capacity: ModelCapacityFact(u, entry, project)}
	gw := types.ModelExposureOption{Key: ExposureDefault, Exposure: ExposureDefault, Title: "Through the cluster's inference gateway",
		Description: fmt.Sprintf("Reachable from outside the cluster. Each call needs an OpenShift token that can see %s. Needs token authentication, which needs the project's admin role.", project),
		Available:   fact.Admits, Reason: GatewayFactWords(project, fact)}
	if fact.Admits && host != "" {
		gw.Address = fmt.Sprintf("https://%s/%s/%s", host, project, entry.Name)
	}
	in := types.ModelExposureOption{Key: ExposureInternal, Exposure: ExposureInternal, Available: true, Title: "Only inside the cluster",
		Description: "No outside address. Programs on this cluster (for example a workbench) can call it without a token.",
		Warning:     "Calls to the in-cluster address are not authenticated: any pod on the cluster whose network can reach the project can use the model."}
	out.Options = []types.ModelExposureOption{gw, in}
	out.Default, out.DefaultReason = ExposureInternal, "the cluster's inference gateway can't be used from this project."
	if fact.Admits {
		out.Default, out.DefaultReason = ExposureDefault, "works from your laptop with an OpenShift token, the way the dashboard deploys models; it needs token authentication."
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

// capacityFacts keeps ModelCapacityFact's answers a minute (it lists every
// node and pod of the cluster), filled only with the facts client.
var capacityFacts = kube.NewLRU[string, string](16)

// ModelCapacityFact is whether some schedulable amd64 worker has the
// model's CPU, memory and download requests free ("fits"), none seems to
// ("may-not-fit"), or it cannot be told ("unknown"). The model's own pods
// in project are not counted (setting it up again replaces them). It reads
// the nodes and pods with c.Facts() and returns only the answer: no node,
// pod or project name.
func ModelCapacityFact(c *kube.Client, entry CatalogModel, project string) string {
	key := capacityKey(c, entry.Name, project)
	if v, ok := capacityFacts.Get(key); ok {
		return v
	}
	free, err := freeRoom(c.Facts(), project, entry.Name)
	if err != nil {
		kube.LogFactReadFailure("cannot read the nodes and pods for a project's model", "capacity", err)
		return CapacityUnknown
	}
	v := CapacityFits
	if RoomWarning(entry, free) != "" {
		v = CapacityMayNotFit
	}
	capacityFacts.Add(key, v, capacityFactTTL)
	return v
}

const capacityFactTTL = time.Minute

// CapacityFactWords says a "may-not-fit" answer for people, from the
// catalog's requests only.
func CapacityFactWords(entry CatalogModel, fact string) string {
	if fact != CapacityMayNotFit {
		return ""
	}
	return fmt.Sprintf("No worker of the cluster seems to have room for %s: it needs %s CPUs and %s of memory free. It may stay pending until room frees up or a cluster-admin adds a node.", entry.DisplayName, entry.CPU, memoryWords(entry.MemoryRequest))
}
