package workbench

import (
	"fmt"
	"net/url"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/authz"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/storage"
)

// Callers in this file always hit the API with the end user's bearer token—
// Workbench status for admins included—so anything else is rejected
// (authz.ErrNotUserClient). No ServiceAccount-backed client gets read access
// on the user's behalf (see pkg/cluster rbac_coverage_test.go rbacUserTokenReads).

func userOnly(c *kube.Client) error {
	if !c.ActsAsUser() {
		return authz.ErrNotUserClient
	}
	return nil
}

// Notebook captures only the Notebook fields this package unmarshals.
type Notebook struct {
	Metadata kube.ObjectMeta `json:"metadata"`
	Spec     struct {
		Template struct {
			Spec struct {
				Containers []struct {
					Name      string `json:"name"`
					Image     string `json:"image"`
					Resources struct {
						Requests map[string]any `json:"requests"`
					} `json:"resources"`
				} `json:"containers"`
				Volumes []Volume `json:"volumes"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
}

// NotebookThere reports whether project has a workbench named Name (any
// owner) that is not being deleted, read with the user's token through the
// list the Workbench tab reads.
func NotebookThere(c *kube.Client, project string) (bool, error) {
	if err := userOnly(c); err != nil {
		return false, err
	}
	var items []struct {
		Metadata kube.ObjectMeta `json:"metadata"`
	}
	if err := c.ListInto(rhoai.NamespacedPath(notebookAPI, "notebooks", project, ""), &items); err != nil {
		if kube.IsK8sNotFound(err) {
			return false, nil
		}
		return false, err
	}
	for _, n := range items {
		if n.Metadata.Name == Name && !n.Metadata.Terminating() {
			return true, nil
		}
	}
	return false, nil
}

// ListProfiles is listProfiles, for the recipe plan's check of the
// hardware profiles the dashboard will offer a workbench (ChooseProfile).
func ListProfiles(c *kube.Client, namespace string) ([]HardwareProfile, error) {
	return listProfiles(c, namespace)
}

// ListLocalQueues is listLocalQueues, for the same check.
func ListLocalQueues(c *kube.Client, project string) ([]string, error) {
	return listLocalQueues(c, project)
}

func notebookPath(project string) string {
	return rhoai.NamespacedPath(notebookAPI, "notebooks", project, Name)
}

func storagePath(project string) string {
	return rhoai.NamespacedPath("v1", "persistentvolumeclaims", project, StorageName)
}

// readNotebook fetches the nightly-workbench Notebook in the given project.
func readNotebook(c *kube.Client, project string) (Notebook, bool, error) {
	if err := userOnly(c); err != nil {
		return Notebook{}, false, err
	}
	var nb Notebook
	found, err := c.GetIntoOptional(notebookPath(project), &nb)
	return nb, found, err
}

// PVC holds the PersistentVolumeClaim fields this package cares about.
type PVC struct {
	Metadata kube.ObjectMeta `json:"metadata"`
	Spec     struct {
		StorageClassName string `json:"storageClassName"`
		Resources        struct {
			Requests map[string]string `json:"requests"`
		} `json:"resources"`
	} `json:"spec"`
}

// readStorage fetches the nightly-workbench-storage PVC for a project.
func readStorage(c *kube.Client, project string) (PVC, bool, error) {
	if err := userOnly(c); err != nil {
		return PVC{}, false, err
	}
	var pvc PVC
	found, err := c.GetIntoOptional(storagePath(project), &pvc)
	return pvc, found, err
}

// Pod models the pod subset status inspection needs for a workbench.
type Pod struct {
	Metadata kube.ObjectMeta `json:"metadata"`
	Spec     struct {
		Volumes []Volume `json:"volumes"`
	} `json:"spec"`
	Status struct {
		Conditions []struct {
			Type    string `json:"type"`
			Status  string `json:"status"`
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"conditions"`
		ContainerStatuses []struct {
			Name  string `json:"name"`
			Ready bool   `json:"ready"`
			State struct {
				Running *struct{} `json:"running"`
				Waiting *struct {
					Reason  string `json:"reason"`
					Message string `json:"message"`
				} `json:"waiting"`
			} `json:"state"`
		} `json:"containerStatuses"`
	} `json:"status"`
}

// listPods returns pods for the workbench, filtered by the same label the
// dashboard uses (api/k8s/pods.ts: notebook-name=<name>).
func listPods(c *kube.Client, project string) ([]Pod, error) {
	if err := userOnly(c); err != nil {
		return nil, err
	}
	var pods []Pod
	err := c.ListInto(rhoai.NamespacedPath("v1", "pods", project, "")+"?labelSelector="+url.QueryEscape("notebook-name="+Name), &pods)
	return pods, err
}

// projectLabels loads namespace labels for a project, or a human message plus
// error code when a workbench cannot exist there (missing, terminating, or not
// labelled as a data science project).
func projectLabels(c *kube.Client, project string) (map[string]string, string, string) {
	if err := userOnly(c); err != nil {
		return nil, err.Error(), "internal"
	}
	var ns struct {
		Metadata kube.ObjectMeta `json:"metadata"`
		Status   struct {
			Phase string `json:"phase"`
		} `json:"status"`
	}
	found, err := c.GetIntoOptional("/api/v1/namespaces/"+project, &ns)
	switch {
	case err != nil:
		return nil, fmt.Sprintf("Cannot read project %s: %v.", project, err), kube.ErrorCodeFromK8sErr(err)
	case !found:
		return nil, fmt.Sprintf("Project %s does not exist. Choose a data science project.", project), "validation"
	case ns.Metadata.Terminating() || ns.Status.Phase == "Terminating":
		return nil, fmt.Sprintf("Project %s is being deleted.", project), "terminating"
	case ns.Metadata.Labels["opendatahub.io/dashboard"] != "true":
		return nil, fmt.Sprintf("Namespace %s is not a data science project (it lacks the label opendatahub.io/dashboard=true), so the dashboard would not show a workbench there. Choose a data science project.", project), "validation"
	}
	return ns.Metadata.Labels, "", ""
}

// listImageStreams returns notebook images offered in the dashboard:
// ImageStreams in the applications namespace tagged opendatahub.io/notebook-image=true
// (utilities/useImageStreams.ts).
func listImageStreams(c *kube.Client, apps string) ([]ImageStream, error) {
	if err := userOnly(c); err != nil {
		return nil, err
	}
	var items []ImageStream
	err := c.ListInto(rhoai.NamespacedPath("image.openshift.io/v1", "imagestreams", apps, "")+"?labelSelector="+url.QueryEscape(notebookImageLabel+"=true"), &items)
	return items, err
}

// listProfiles returns HardwareProfiles in a namespace—global ones live in the
// applications namespace; project-scoped ones live in the project.
func listProfiles(c *kube.Client, namespace string) ([]HardwareProfile, error) {
	if err := userOnly(c); err != nil {
		return nil, err
	}
	var items []HardwareProfile
	err := c.ListInto(rhoai.NamespacedPath("infrastructure.opendatahub.io/v1", "hardwareprofiles", namespace, ""), &items)
	return items, err
}

// listLocalQueues returns Kueue LocalQueue names in the project
// (kueue.x-k8s.io v1beta2; matches the dashboard LocalQueueModel). Missing API
// on the cluster yields an empty list.
func listLocalQueues(c *kube.Client, project string) ([]string, error) {
	if err := userOnly(c); err != nil {
		return nil, err
	}
	var items []struct {
		Metadata kube.ObjectMeta `json:"metadata"`
	}
	if err := c.ListInto(rhoai.NamespacedPath("kueue.x-k8s.io/v1beta2", "localqueues", project, ""), &items); err != nil {
		if kube.IsK8sNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	names := make([]string, 0, len(items))
	for _, q := range items {
		names = append(names, q.Metadata.Name)
	}
	return names, nil
}

// listStorageClasses enumerates StorageClass objects cluster-wide.
func listStorageClasses(c *kube.Client) ([]StorageClass, error) {
	if err := userOnly(c); err != nil {
		return nil, err
	}
	var items []StorageClass
	err := c.ListInto(rhoai.ClusterPath("storage.k8s.io/v1", "storageclasses", ""), &items)
	return items, err
}

// dashboardConfig mirrors workbench-related fields from OdhDashboardConfig
// (opendatahub.io/v1alpha odh-dashboard-config in the applications namespace).
type dashboardConfig struct {
	Spec struct {
		DashboardConfig struct {
			DisableKueue *bool `json:"disableKueue"`
		} `json:"dashboardConfig"`
		NotebookController struct {
			PVCSize          string `json:"pvcSize"`
			StorageClassName string `json:"storageClassName"`
		} `json:"notebookController"`
	} `json:"spec"`
}

// readDashboardConfig loads that config; a missing resource is a zero value.
func readDashboardConfig(c *kube.Client, apps string) (dashboardConfig, error) {
	if err := userOnly(c); err != nil {
		return dashboardConfig{}, err
	}
	var cfg dashboardConfig
	_, err := c.GetIntoOptional(rhoai.NamespacedPath("opendatahub.io/v1alpha", "odhdashboardconfigs", apps, "odh-dashboard-config"), &cfg)
	return cfg, err
}

// secretMeta is the part of a Secret these reads use (its metadata only).
type secretMeta struct {
	Metadata kube.ObjectMeta `json:"metadata"`
}

// pipelineConfigExists reports whether Secret ds-pipeline-config is present:
// the dashboard's canEnablePipelines (CanEnableElyraPipelinesCheck.tsx: the
// Secret can be read).
func pipelineConfigExists(c *kube.Client, project string) (bool, error) {
	if err := userOnly(c); err != nil {
		return false, err
	}
	var sec secretMeta
	found, err := c.GetIntoOptional(rhoai.NamespacedPath("v1", "secrets", project, PipelineSecret), &sec)
	if kube.IsK8sError(err, 403) {
		return false, nil
	}
	return found, err
}

// toolConnection is the tool's S3 Connection in the project, "" when it
// has none: the shared storage's Secret rhoai-nightly-updater-s3 or, for a
// pipeline server on the project's own storage, nightly-s3-connection,
// with the tool label and owned by the pipeline server's DSPA
// (pipeline_server.go setDSPAOwner). The setup of a pipeline server makes
// one or the other, never both.
func toolConnection(c *kube.Client, project string) (string, error) {
	if err := userOnly(c); err != nil {
		return "", err
	}
	for _, path := range []string{
		rhoai.NamespacedPath("v1", "secrets", project, storage.ConnectionSecretName),
		rhoai.NamespacedPath("v1", "secrets", project, storage.ProjectConnectionSecretName),
	} {
		var sec secretMeta
		found, err := c.GetIntoOptional(path, &sec)
		if err != nil {
			return "", err
		}
		if found && pipelineOwned(sec.Metadata) {
			return sec.Metadata.Name, nil
		}
	}
	return "", nil
}

// pipelineOwned: meta is the tool's and owned by a pipeline server.
func pipelineOwned(meta kube.ObjectMeta) bool {
	if !meta.HasToolLabel() || meta.Terminating() {
		return false
	}
	for _, o := range meta.OwnerReferences {
		if o.Kind == "DataSciencePipelinesApplication" {
			return true
		}
	}
	return false
}

// Volume is the part of a pod volume this package reads.
type Volume struct {
	Name   string `json:"name"`
	Secret *struct {
		SecretName string `json:"secretName"`
	} `json:"secret"`
	PersistentVolumeClaim *struct {
		ClaimName string `json:"claimName"`
	} `json:"persistentVolumeClaim"`
}

// listNotebooks lists the project's Notebooks (to find those that use its
// workbench storage).
func listNotebooks(c *kube.Client, project string) ([]Notebook, error) {
	if err := userOnly(c); err != nil {
		return nil, err
	}
	var items []Notebook
	if err := c.ListInto(rhoai.NamespacedPath(notebookAPI, "notebooks", project, ""), &items); err != nil {
		if kube.IsK8sNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return items, nil
}

// listProjectPods lists the project's pods (to find those that mount its
// workbench storage).
func listProjectPods(c *kube.Client, project string) ([]Pod, error) {
	if err := userOnly(c); err != nil {
		return nil, err
	}
	var pods []Pod
	err := c.ListInto(rhoai.NamespacedPath("v1", "pods", project, ""), &pods)
	return pods, err
}
