// Package workbench provisions a workbench inside a data science project the
// same way the RHOAI dashboard Create workbench form does (odh-dashboard rhoai-3.6
// frontend/src/api/k8s/notebooks.ts assembleNotebook and api/k8s/pvcs.ts
// assemblePvc), brings it up and down using the dashboard's JSON patch format,
// derives status the way the dashboard does, and removes solely resources this
// code path created (UIDs are stored in the tool's namespace).
//
// Side effects the cluster applies automatically are never emitted here:
// the odh-notebook-controller webhook fixes the image digest, injects
// kube-rbac-proxy, the ServiceAccount, the trusted CA bundle and the
// pipeline runtime images; workbenches-operator webhooks apply the
// hardware profile's scheduling (Kueue label, tolerations, node selector)
// and the Connection's envFrom (from the opendatahub.io/connections
// annotation); the reconciler creates the HTTPRoute, ReferenceGrant,
// NetworkPolicies and the ds-pipeline-config Secret.
package workbench

import (
	"fmt"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
)

// Canonical object names: at most one workbench from this tool per project.
const (
	Name        = "nightly-workbench"
	StorageName = "nightly-workbench-storage"
	DisplayName = "Nightly workbench (RHOAI Nightly Updater)"
)

// Constants aligned with dashboard and workbench controllers
// (odh-dashboard rhoai-3.6; kubeflow rhoai-3.6 components/odh-notebook-controller).
const (
	notebookAPI = "kubeflow.org/v1"
	// rootMountPath is the container working directory (pages/projects/pvc/const.ts ROOT_MOUNT_PATH);
	// storageMountPath is the PVC mount point (spawner/storage/const.ts MOUNT_PATH_PREFIX).
	rootMountPath    = "/opt/app-root/src"
	storageMountPath = "/opt/app-root/src/"
	// Elyra pipeline configuration (concepts/pipelines/elyra/utils.ts): the
	// controller materializes Secret ds-pipeline-config from the project's DSPA.
	elyraVolumeName = "elyra-dsp-details"
	elyraMountPath  = "/opt/app-root/runtimes"
	PipelineSecret  = "ds-pipeline-config"
	// StoppedAnnotation halts the workbench; on create the webhook sets it to
	// controllerLock until reconciliation runs once.
	StoppedAnnotation = "kubeflow-resource-stopped"
	controllerLock    = "odh-notebook-controller-lock"
	// ConnectionsAnnotation enumerates the workbench's Connections.
	ConnectionsAnnotation = "opendatahub.io/connections"
	// Hardware profile annotation keys (packages/hardware-profiles
	// shared/utils.ts applyHardwareProfileConfig).
	profileNameAnnotation      = "opendatahub.io/hardware-profile-name"
	profileNamespaceAnnotation = "opendatahub.io/hardware-profile-namespace"
	profileVersionAnnotation   = "opendatahub.io/hardware-profile-resource-version"
	imageDisplayAnnotation     = "opendatahub.io/image-display-name"
	imageSelectionAnnotation   = "notebooks.opendatahub.io/last-image-selection"
	// kubeRBACProxy names the sidecar odh-notebook-controller adds for
	// inject-auth (notebook_mutating_webhook.go ContainerNameKubeRbacProxy):
	// RHOAI 3.x only.
	kubeRBACProxy = "kube-rbac-proxy"
)

// UsernameTranslate mirrors the dashboard usernameTranslate
// (frontend/src/utilities/notebookControllerUtils.ts L35-49), byte for byte:
// encodeURIComponent, then manual escaping for ! ' ( ) * - . _ ~ ("~" as %7f,
// matching the dashboard), replace every "%" with "-", then lowercase.
func UsernameTranslate(username string) string {
	enc := encodeURIComponent(username)
	r := strings.NewReplacer("!", "%21", "'", "%27", "(", "%28", ")", "%29", "*", "%2a", "-", "%2d", ".", "%2e", "_", "%5f", "~", "%7f")
	enc = r.Replace(enc)
	return strings.ToLower(strings.ReplaceAll(enc, "%", "-"))
}

// encodeURIComponent matches JavaScript: all bytes except A-Z a-z 0-9 - _ . ! ~ * ' ( )
// become percent-encoded UTF-8 using upper-case hex digits.
func encodeURIComponent(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || strings.IndexByte("-_.!~*'()", ch) >= 0 {
			b.WriteByte(ch)
			continue
		}
		// Each byte of the UTF-8 encoding, as encodeURIComponent does
		// ("é" is %C3%A9).
		fmt.Fprintf(&b, "%%%02X", ch)
	}
	return b.String()
}

// Plan captures what setup will persist, decided before the first API call.
type Plan struct {
	Project string
	// Username holds the signed-in user's name (opendatahub.io/username).
	Username string
	Image    ImageChoice
	Profile  ProfileChoice
	// StorageSize and StorageClass (empty string means the cluster default).
	StorageSize  string
	StorageClass string
	// Connection: wire the tool's S3 Connection; Pipelines: attach
	// Elyra settings; MLflow: associate MLflow.
	Connection bool
	Pipelines  bool
	MLflow     bool
}

// ImageChoice identifies an ImageStream tag used to run the workbench.
type ImageChoice struct {
	Stream, Namespace, Tag string
	// Repository comes from the stream's status.dockerImageRepository.
	Repository  string
	DisplayName string
	// BuildCommit is the tag's opendatahub.io/notebook-build-commit.
	BuildCommit string
}

// ProfileChoice describes the hardware profile and the resources it defines.
type ProfileChoice struct {
	Name, Namespace, ResourceVersion, DisplayName string
	// Resources map identifier to defaultCount as stored on the profile
	// (numeric or string).
	Resources map[string]any
	// Order preserves profile identifier ordering for presentation.
	Order []string
	Queue string
}

func (p Plan) imageURL() string { return p.Image.Repository + ":" + p.Image.Tag }

// NotebookManifest matches the Notebook assembleNotebook builds for this plan
// (rhoai-3.6 api/k8s/notebooks.ts L34-195) after applyHardwareProfileConfig,
// plus this tool's label. Intentional gaps:
//   - no envFrom: the dashboard puts only form-supplied environment
//     variables there; Connection envFrom is added by the connection
//     webhook (from opendatahub.io/connections);
//   - opendatahub.io/workbench-image-namespace is "" for images in the
//     applications namespace, where the dashboard sends null: a string
//     map cannot hold null, and the webhook reads "" as it reads null
//     (the applications namespace).
func NotebookManifest(p Plan) map[string]any {
	image := p.imageURL()
	base := "/notebook/" + p.Project + "/" + Name
	connections := ""
	if p.Connection {
		connections = p.Project + "/" + ConnectionSecret
	}
	annotations := map[string]any{
		"openshift.io/display-name":            DisplayName,
		"openshift.io/description":             "",
		imageSelectionAnnotation:               p.Image.Stream + ":" + p.Image.Tag,
		"notebooks.opendatahub.io/inject-auth": "true",
		"opendatahub.io/user":                  UsernameTranslate(p.Username),
		"opendatahub.io/username":              p.Username,
		"notebooks.opendatahub.io/last-image-version-git-commit-selection": p.Image.BuildCommit,
		ConnectionsAnnotation:  connections,
		imageDisplayAnnotation: p.Image.DisplayName,
	}
	if p.MLflow {
		annotations["opendatahub.io/mlflow-instance"] = "mlflow"
	}
	imageNamespace := ""
	if p.Image.Namespace == p.Project {
		imageNamespace = p.Project
	}
	annotations["opendatahub.io/workbench-image-namespace"] = imageNamespace
	if p.Profile.Name != "" {
		annotations[profileNameAnnotation] = p.Profile.Name
		annotations[profileNamespaceAnnotation] = p.Profile.Namespace
		annotations[profileVersionAnnotation] = p.Profile.ResourceVersion
	}
	volumes := []any{
		map[string]any{"name": StorageName, "persistentVolumeClaim": map[string]any{"claimName": StorageName}},
	}
	mounts := []any{
		map[string]any{"name": StorageName, "mountPath": storageMountPath},
	}
	if p.Pipelines {
		volumes = append(volumes, elyraVolume())
		mounts = append(mounts, elyraMount())
	}
	volumes = append(volumes, map[string]any{"name": "shm", "emptyDir": map[string]any{"medium": "Memory"}})
	mounts = append(mounts, map[string]any{"name": "shm", "mountPath": "/dev/shm"})
	probe := func() map[string]any {
		return map[string]any{
			"initialDelaySeconds": 10, "periodSeconds": 5, "timeoutSeconds": 1, "successThreshold": 1, "failureThreshold": 3,
			"httpGet": map[string]any{"scheme": "HTTP", "path": base + "/api", "port": "notebook-port"},
		}
	}
	container := map[string]any{
		"image":           image,
		"imagePullPolicy": "Always",
		"workingDir":      rootMountPath,
		"name":            Name,
		"env": []any{
			map[string]any{"name": "NOTEBOOK_ARGS", "value": NotebookArgs(p.Project)},
			map[string]any{"name": "JUPYTER_IMAGE", "value": image},
		},
		"volumeMounts":   mounts,
		"ports":          []any{map[string]any{"name": "notebook-port", "containerPort": 8888, "protocol": "TCP"}},
		"livenessProbe":  probe(),
		"readinessProbe": probe(),
	}
	if len(p.Profile.Resources) > 0 {
		requests, limits := map[string]any{}, map[string]any{}
		for k, v := range p.Profile.Resources {
			requests[k], limits[k] = v, v
		}
		container["resources"] = map[string]any{"requests": requests, "limits": limits}
	}
	return map[string]any{
		"apiVersion": notebookAPI,
		"kind":       "Notebook",
		"metadata": map[string]any{
			"name":      Name,
			"namespace": p.Project,
			"labels": map[string]any{
				"app":                        Name,
				"opendatahub.io/odh-managed": "true",
				"opendatahub.io/dashboard":   "true",
				kube.ManagedByLabelKey:       kube.ManagedByLabelValue,
			},
			"annotations": annotations,
		},
		"spec": map[string]any{
			"template": map[string]any{
				"spec": map[string]any{
					"enableServiceLinks": false,
					"containers":         []any{container},
					"volumes":            volumes,
				},
			},
		},
	}
}

// NotebookArgs reproduces the dashboard NOTEBOOK_ARGS string, including newlines and
// indentation (template literal in notebooks.ts L131-135).
func NotebookArgs(project string) string {
	return "--ServerApp.port=8888\n" +
		"                  --ServerApp.token=''\n" +
		"                  --ServerApp.password=''\n" +
		"                  --ServerApp.base_url=/notebook/" + project + "/" + Name + "\n" +
		"                  --ServerApp.quit_button=False"
}

func elyraVolume() map[string]any {
	return map[string]any{"name": elyraVolumeName, "secret": map[string]any{"secretName": PipelineSecret, "optional": true}}
}

func elyraMount() map[string]any {
	return map[string]any{"name": elyraVolumeName, "mountPath": elyraMountPath}
}

// PVCManifest follows assemblePvc for workbench storage
// (api/k8s/pvcs.ts L24-84; storage display name falls back to the
// workbench name), plus this tool's label. storageClassName is absent when
// no rule selects a class, matching dashboard undefined.
func PVCManifest(p Plan) map[string]any {
	spec := map[string]any{
		"accessModes": []any{"ReadWriteOnce"},
		"resources":   map[string]any{"requests": map[string]any{"storage": p.StorageSize}},
		"volumeMode":  "Filesystem",
	}
	if p.StorageClass != "" {
		spec["storageClassName"] = p.StorageClass
	}
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"metadata": map[string]any{
			"name":      StorageName,
			"namespace": p.Project,
			"labels": map[string]any{
				"opendatahub.io/dashboard": "true",
				kube.ManagedByLabelKey:     kube.ManagedByLabelValue,
			},
			"annotations": map[string]any{"openshift.io/display-name": DisplayName},
		},
		"spec": spec,
	}
}

// OpenURL is the dashboard 3.x workbench entry point: same origin as the UI plus
// /notebook/<project>/<name> (concepts/notebooks/utils.ts
// getRoutePathForWorkbench), or "" when no dashboard URL is known.
func OpenURL(dashboardURL, project string) string {
	if !strings.HasPrefix(dashboardURL, "https://") {
		return ""
	}
	return strings.TrimRight(dashboardURL, "/") + "/notebook/" + project + "/" + Name
}
