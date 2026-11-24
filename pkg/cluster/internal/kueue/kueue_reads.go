package kueue

import (
	"slices"

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

// v1beta2Served reports whether Kueue exposes v1beta2—the version this
// dashboard uses. served is also false while the API group is unavailable.
func v1beta2Served(c *kube.Client) (served, group bool, err error) {
	if err := userOnly(c); err != nil {
		return false, false, err
	}
	v := rhoai.ServedVersions(c, APIGroup, nil)
	if !v.Discovered {
		return false, false, nil
	}
	return slices.Contains(v.Versions, APIVersion), true, nil
}

// Queue represents a ResourceFlavor, ClusterQueue, or LocalQueue from the API.
type Queue struct {
	Metadata kube.ObjectMeta `json:"metadata"`
	Spec     struct {
		ClusterQueue   string `json:"clusterQueue"`
		ResourceGroups []struct {
			Flavors []struct {
				Name string `json:"name"`
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

// Ray captures a RayCluster or RayJob as returned by reads.
type Ray struct {
	Metadata kube.ObjectMeta `json:"metadata"`
	Spec     struct {
		HeadGroupSpec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Image string `json:"image"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"headGroupSpec"`
		RayClusterSpec *struct {
			HeadGroupSpec struct {
				Template struct {
					Spec struct {
						Containers []struct {
							Image string `json:"image"`
						} `json:"containers"`
					} `json:"spec"`
				} `json:"template"`
			} `json:"headGroupSpec"`
		} `json:"rayClusterSpec"`
	} `json:"spec"`
	Status struct {
		State               string `json:"state"`
		JobDeploymentStatus string `json:"jobDeploymentStatus"`
		JobStatus           string `json:"jobStatus"`
		Message             string `json:"message"`
	} `json:"status"`
}

func (r Ray) image() string {
	cs := r.Spec.HeadGroupSpec.Template.Spec.Containers
	if r.Spec.RayClusterSpec != nil {
		cs = r.Spec.RayClusterSpec.HeadGroupSpec.Template.Spec.Containers
	}
	if len(cs) == 0 {
		return ""
	}
	return cs[0].Image
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
