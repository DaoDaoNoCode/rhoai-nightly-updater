package kueue

import (
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/authz"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
)

// Reads in this file run with the caller's user token and include status;
// clients that are not user-backed are rejected via authz.ErrNotUserClient.
// The ServiceAccount has no Kueue permissions (see pkg/cluster
// rbac_coverage_test.go, rbacUserTokenReads).

func userOnly(c *kube.Client) error {
	if !c.ActsAsUser() {
		return authz.ErrNotUserClient
	}
	return nil
}

// Condition mirrors one status condition on a Kueue API object.
type Condition struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	Reason             string `json:"reason"`
	Message            string `json:"message"`
	LastTransitionTime string `json:"lastTransitionTime"`
}

func findCondition(conds []Condition, typ string) (Condition, bool) {
	for _, c := range conds {
		if c.Type == typ {
			return c, true
		}
	}
	return Condition{}, false
}

// Instance is the cluster-scoped Kueue CR that RHOAI installs.
type Instance struct {
	Metadata kube.ObjectMeta `json:"metadata"`
	Spec     struct {
		Config struct {
			WorkloadManagement *struct {
				LabelPolicy string `json:"labelPolicy"`
			} `json:"workloadManagement"`
		} `json:"config"`
	} `json:"spec"`
	Status struct {
		Conditions []Condition `json:"conditions"`
	} `json:"status"`
}

func (k Instance) labelPolicy() string {
	if w := k.Spec.Config.WorkloadManagement; w != nil {
		return w.LabelPolicy
	}
	return ""
}

// readInstance fetches the cluster Kueue CR (kueue.openshift.io/v1); found
// is false when the object or its API group is still missing.
func readInstance(c *kube.Client) (Instance, bool, error) {
	if err := userOnly(c); err != nil {
		return Instance{}, false, err
	}
	var k Instance
	found, err := c.GetIntoOptional(rhoai.ClusterPath(OperatorAPI, "kueues", instanceName), &k)
	if err != nil && kube.IsK8sNotFound(err) {
		return k, false, nil
	}
	return k, found, err
}

// v1beta2Served reads Kueue's API discovery document with the user's
// token. group is false only when the API server answers that the group is
// not served (404); any other failure, or a document that does not parse,
// is an error, never "absent" (fail closed). served: v1beta2, the version
// the dashboard reads, is among its versions.
func v1beta2Served(c *kube.Client) (served, group bool, err error) {
	if err := userOnly(c); err != nil {
		return false, false, err
	}
	body, found, err := c.GetOptional("/apis/" + APIGroup)
	if err != nil {
		return false, false, fmt.Errorf("read the API group %s: %w", APIGroup, err)
	}
	if !found {
		return false, false, nil
	}
	var doc rhoai.APIGroupDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return false, false, fmt.Errorf("parse the API group %s: %w", APIGroup, err)
	}
	versions := doc.OrderedVersions()
	if len(versions) == 0 {
		return false, false, fmt.Errorf("the API group %s lists no version", APIGroup)
	}
	return slices.Contains(versions, APIVersion), true, nil
}

// Queue represents a ResourceFlavor, ClusterQueue, or LocalQueue from the API.
type Queue struct {
	Metadata kube.ObjectMeta `json:"metadata"`
	Spec     struct {
		ClusterQueue   string `json:"clusterQueue"`
		ResourceGroups []struct {
			Flavors []struct {
				Name      string `json:"name"`
				Resources []struct {
					Name         string        `json:"name"`
					NominalQuota kube.Quantity `json:"nominalQuota"`
				} `json:"resources"`
			} `json:"flavors"`
		} `json:"resourceGroups"`
	} `json:"spec"`
	Status struct {
		Conditions []Condition `json:"conditions"`
	} `json:"status"`
}

func (q Queue) active() (bool, string) {
	cond, ok := findCondition(q.Status.Conditions, "Active")
	if !ok {
		return false, "Kueue has not reported it yet"
	}
	return cond.Status == "True", kube.FirstNonEmpty(cond.Message, cond.Reason)
}

// nominalQuota is the ClusterQueue's nominal CPU, memory and ephemeral
// storage quota for the tool's flavor, as the API returns them ("" when
// not set).
func (q Queue) nominalQuota() (cpu, memory, ephemeral string) {
	for _, g := range q.Spec.ResourceGroups {
		for _, f := range g.Flavors {
			if f.Name != FlavorName {
				continue
			}
			for _, r := range f.Resources {
				switch r.Name {
				case "cpu":
					cpu = string(r.NominalQuota)
				case "memory":
					memory = string(r.NominalQuota)
				case "ephemeral-storage":
					ephemeral = string(r.NominalQuota)
				}
			}
		}
	}
	return cpu, memory, ephemeral
}

func (q Queue) usesFlavor(name string) bool {
	for _, g := range q.Spec.ResourceGroups {
		for _, f := range g.Flavors {
			if f.Name == name {
				return true
			}
		}
	}
	return false
}

func readFlavor(c *kube.Client) (Queue, bool, error) {
	if err := userOnly(c); err != nil {
		return Queue{}, false, err
	}
	var q Queue
	found, err := c.GetIntoOptional(rhoai.ClusterPath(kueueAPI, "resourceflavors", FlavorName), &q)
	return q, found, err
}

func readClusterQueue(c *kube.Client) (Queue, bool, error) {
	if err := userOnly(c); err != nil {
		return Queue{}, false, err
	}
	var q Queue
	found, err := c.GetIntoOptional(rhoai.ClusterPath(kueueAPI, "clusterqueues", ClusterQueueName), &q)
	return q, found, err
}

func readLocalQueue(c *kube.Client, project string) (Queue, bool, error) {
	if err := userOnly(c); err != nil {
		return Queue{}, false, err
	}
	var q Queue
	found, err := c.GetIntoOptional(rhoai.NamespacedPath(kueueAPI, "localqueues", project, LocalQueueName), &q)
	return q, found, err
}

// listClusterQueues returns all ClusterQueue resources.
func listClusterQueues(c *kube.Client) ([]Queue, error) {
	if err := userOnly(c); err != nil {
		return nil, err
	}
	var items []Queue
	err := c.ListInto(rhoai.ClusterPath(kueueAPI, "clusterqueues", ""), &items)
	return items, err
}

// listLocalQueues returns LocalQueues in namespace ns, or across all
// namespaces when ns is empty.
func listLocalQueues(c *kube.Client, ns string) ([]Queue, error) {
	if err := userOnly(c); err != nil {
		return nil, err
	}
	path := rhoai.ClusterPath(kueueAPI, "localqueues", "")
	if ns != "" {
		path = rhoai.NamespacedPath(kueueAPI, "localqueues", ns, "")
	}
	var items []Queue
	err := c.ListInto(path, &items)
	return items, err
}

// Workload is the shape used when listing Kueue Workload status.
type Workload struct {
	Metadata kube.ObjectMeta `json:"metadata"`
	Spec     struct {
		QueueName string `json:"queueName"`
	} `json:"spec"`
	Status struct {
		Conditions []Condition `json:"conditions"`
	} `json:"status"`
}

// listWorkloads enumerates Workloads in the given project.
func listWorkloads(c *kube.Client, project string) ([]Workload, error) {
	if err := userOnly(c); err != nil {
		return nil, err
	}
	var items []Workload
	err := c.ListInto(rhoai.NamespacedPath(kueueAPI, "workloads", project, ""), &items)
	return items, err
}

// rayContainer is a Ray pod's container: its image and resources (the
// quantities as the API returns them).
type rayContainer struct {
	Image     string `json:"image"`
	Resources struct {
		Requests map[string]kube.Quantity `json:"requests"`
		Limits   map[string]kube.Quantity `json:"limits"`
	} `json:"resources"`
}

// rayGroup is a head or worker group's pod template.
type rayGroup struct {
	Template struct {
		Spec struct {
			Containers []rayContainer `json:"containers"`
		} `json:"spec"`
	} `json:"template"`
}

// rayClusterShape is a RayCluster's spec, or a RayJob's rayClusterSpec.
type rayClusterShape struct {
	HeadGroupSpec    rayGroup   `json:"headGroupSpec"`
	WorkerGroupSpecs []rayGroup `json:"workerGroupSpecs"`
}

// Ray captures a RayCluster or RayJob as returned by reads.
type Ray struct {
	Metadata kube.ObjectMeta `json:"metadata"`
	Spec     struct {
		rayClusterShape
		RayClusterSpec *rayClusterShape `json:"rayClusterSpec"`
	} `json:"spec"`
	Status struct {
		State               string `json:"state"`
		JobDeploymentStatus string `json:"jobDeploymentStatus"`
		JobStatus           string `json:"jobStatus"`
		Message             string `json:"message"`
	} `json:"status"`
}

// cluster is the Ray cluster the sample runs: a RayCluster's own spec, or a
// RayJob's rayClusterSpec.
func (r Ray) cluster() rayClusterShape {
	if r.Spec.RayClusterSpec != nil {
		return *r.Spec.RayClusterSpec
	}
	return r.Spec.rayClusterShape
}

func (r Ray) image() string {
	cs := r.cluster().HeadGroupSpec.Template.Spec.Containers
	if len(cs) == 0 {
		return ""
	}
	return cs[0].Image
}

// sizedAsManifest reports whether the sample's head and first worker
// request and limit the CPU and memory this version's manifests set
// (headSize, workerSize), as amounts; a sample an earlier version made with
// other sizes is not. known is false when a value cannot be read.
func (r Ray) sizedAsManifest() (same, known bool) {
	cl := r.cluster()
	container := func(g rayGroup) (map[string]kube.Quantity, map[string]kube.Quantity, bool) {
		cs := g.Template.Spec.Containers
		if len(cs) == 0 {
			return nil, nil, false
		}
		return cs[0].Resources.Requests, cs[0].Resources.Limits, true
	}
	hr, hl, okHead := container(cl.HeadGroupSpec)
	if !okHead || len(cl.WorkerGroupSpecs) == 0 {
		return false, true
	}
	wr, wl, okWorker := container(cl.WorkerGroupSpecs[0])
	if !okWorker {
		return false, true
	}
	headSame, headKnown := headSize.matches(hr, hl)
	workerSame, workerKnown := workerSize.matches(wr, wl)
	switch {
	case (headKnown && !headSame) || (workerKnown && !workerSame):
		return false, true
	case !headKnown || !workerKnown:
		return false, false
	}
	return true, true
}

func samplePath(kind, project string) string {
	if kind == SampleRayJob {
		return rhoai.NamespacedPath(rayAPI, "rayjobs", project, RayJobName)
	}
	return rhoai.NamespacedPath(rayAPI, "rayclusters", project, RayClusterName)
}

func sampleCollection(kind, project string) string {
	if kind == SampleRayJob {
		return rhoai.NamespacedPath(rayAPI, "rayjobs", project, "")
	}
	return rhoai.NamespacedPath(rayAPI, "rayclusters", project, "")
}

// readSample loads a sample object; when the Ray API is unserved, the read
// behaves as if the resource were absent.
func readSample(c *kube.Client, kind, project string) (Ray, bool, error) {
	if err := userOnly(c); err != nil {
		return Ray{}, false, err
	}
	var r Ray
	found, err := c.GetIntoOptional(samplePath(kind, project), &r)
	if err != nil && kube.IsK8sNotFound(err) {
		return r, false, nil
	}
	return r, found, err
}

// Namespace is the project namespace object as fetched from the API.
type Namespace struct {
	Metadata kube.ObjectMeta `json:"metadata"`
	Status   struct {
		Phase string `json:"phase"`
	} `json:"status"`
}

func readNamespace(c *kube.Client, project string) (Namespace, bool, error) {
	if err := userOnly(c); err != nil {
		return Namespace{}, false, err
	}
	var ns Namespace
	found, err := c.GetIntoOptional("/api/v1/namespaces/"+project, &ns)
	return ns, found, err
}

// DashboardConfig holds the odh-dashboard-config fields the Kueue opt-in path consumes.
type DashboardConfig struct {
	Metadata kube.ObjectMeta `json:"metadata"`
	Spec     struct {
		DashboardConfig struct {
			DisableKueue *bool `json:"disableKueue"`
		} `json:"dashboardConfig"`
	} `json:"spec"`
}

func readDashboardConfig(c *kube.Client, apps string) (DashboardConfig, bool, error) {
	if err := userOnly(c); err != nil {
		return DashboardConfig{}, false, err
	}
	var cfg DashboardConfig
	found, err := c.GetIntoOptional(rhoai.NamespacedPath("opendatahub.io/v1alpha", "odhdashboardconfigs", apps, dashboardConfig), &cfg)
	return cfg, found, err
}

func profilePath(apps string) string {
	return rhoai.NamespacedPath(profileAPI, "hardwareprofiles", apps, ProfileName)
}

func readProfile(c *kube.Client, apps string) (kube.ObjectMeta, bool, error) {
	if err := userOnly(c); err != nil {
		return kube.ObjectMeta{}, false, err
	}
	var p struct {
		Metadata kube.ObjectMeta `json:"metadata"`
	}
	found, err := c.GetIntoOptional(profilePath(apps), &p)
	return p.Metadata, found, err
}

// crdName is the Kueue CRD whose OLM ownership tells who installed Kueue.
const crdName = "clusterqueues." + APIGroup

// OutsideOLM says whether Kueue's API is served by a Kueue that OLM did not
// install for package kueue-operator, whatever its controller is named:
// RHOAI counts Kueue only through OLM (rhods-operator rhoai-3.6
// kueue_controller_actions.go checkPreConditions), and two Kueue
// controllers clash. Called when no kueue-operator CSV or Subscription
// exists. OLM labels the CRDs it installs for an operator
// (operators.coreos.com/<package>.<namespace>, olm.managed=true) and
// annotates them operatorframework.io/installed-alongside-<hash>:
// <namespace>/<csv>; CRDs left by an uninstall keep them, and installing the
// operator again adopts them. A CRD without them, or no CRD although the
// API is served, is outside OLM (fail closed). Reads: API discovery and the
// CRD list (by name), which the ServiceAccount already has.
func OutsideOLM(c *kube.Client) (bool, string, error) {
	_, found, err := c.GetOptional("/apis/" + APIGroup)
	if err != nil {
		return false, "", fmt.Errorf("read the API group %s: %w", APIGroup, err)
	}
	if !found {
		return false, "", nil
	}
	var crds []struct {
		Metadata kube.ObjectMeta `json:"metadata"`
	}
	if err := c.ListInto("/apis/apiextensions.k8s.io/v1/customresourcedefinitions?fieldSelector="+url.QueryEscape("metadata.name="+crdName), &crds); err != nil {
		return false, "", fmt.Errorf("read CRD %s: %w", crdName, err)
	}
	for _, crd := range crds {
		if crd.Metadata.Name != crdName {
			continue
		}
		if olmInstalled(crd.Metadata) {
			return false, "", nil
		}
		return true, fmt.Sprintf("its CRD %s carries no OLM label of package %s", crdName, Operator.Pkg), nil
	}
	return true, fmt.Sprintf("the API is served without a CRD %s", crdName), nil
}

// olmInstalled: OLM installed the CRD for package kueue-operator.
func olmInstalled(m kube.ObjectMeta) bool {
	for k := range m.Labels {
		if strings.HasPrefix(k, "operators.coreos.com/"+Operator.Pkg+".") {
			return true
		}
	}
	for k, v := range m.Annotations {
		if strings.HasPrefix(k, "operatorframework.io/installed-alongside-") && strings.Contains(v, "/"+Operator.Pkg+".") {
			return true
		}
	}
	return false
}

// AutoQueuesRefusal is why Set up refuses while RHOAI creates a LocalQueue
// named default in every labelled project (AutoCreatedQueue): labelling
// the project makes it, Kueue then queues every new Deployment, StatefulSet
// and Pod there in it (kubernetes-sigs/kueue v0.19.0
// pkg/controller/jobframework/defaults.go), and Set up refuses a project
// with that queue. The refusal itself is not changed; it is said up front.
const AutoQueuesRefusal = "The DataScienceCluster sets kueue.autoCreateQueues: true, so RHOAI creates a LocalQueue named default in every project with the label kueue.openshift.io/managed=true, and Kueue then queues every new Deployment, StatefulSet and Pod of that project in it; this tool does not set up its queues in a project with that LocalQueue. Use RHOAI's queues (a Queue hardware profile for the LocalQueue default), or a cluster-admin sets autoCreateQueues to false (or defaultLocalQueueName to another name). Nothing was changed."

// AutoCreatedQueue is the LocalQueue RHOAI creates in every project with
// the label kueue.openshift.io/managed=true when the DataScienceCluster's
// spec.components.kueue.autoCreateQueues is true (rhods-operator rhoai-3.6
// api/components/v1alpha1/kueue_types.go L126-151, default false;
// internal/controller/components/kueue/kueue_controller_actions.go
// L153-204): its defaultLocalQueueName, "default" when unset. "" when
// autoCreateQueues is off or unset.
func AutoCreatedQueue(dsc rhoai.DSCRead) string {
	spec, _ := dsc.Object["spec"].(map[string]any)
	comps, _ := spec["components"].(map[string]any)
	k, _ := comps["kueue"].(map[string]any)
	if on, _ := k["autoCreateQueues"].(bool); !on {
		return ""
	}
	if name, _ := k["defaultLocalQueueName"].(string); name != "" {
		return name
	}
	return "default"
}
