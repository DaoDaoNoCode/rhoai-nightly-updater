// Package kueue configures distributed workloads (Kueue) in a single namespace
// to match RHOAI 3.x self-managed queue guidance (Managing OpenShift AI,
// "Manage workloads with Kueue" and "Manage distributed workloads"; RHOAI 3.5
// is the latest published doc set): ResourceFlavor, ClusterQueue with CPU and
// memory quotas, LocalQueue in that namespace, and label
// kueue.openshift.io/managed=true on the project. Sample RayCluster and RayJob
// objects targeting the LocalQueue can populate the dashboard Distributed
// workloads view and the Observe & monitor, Infrastructure page. An optional
// step flips the dashboard Kueue switch (disableKueue: false) and registers a
// Queue hardware profile for the LocalQueue.
//
// Cluster-wide prerequisites (cert-manager, Red Hat build of Kueue operator,
// DataScienceCluster kueue Unmanaged) are handled by the features package
// (feature_setup.go, feature "kueue"). RHOAI owns creation of the Kueue CR
// (rhods-operator rhoai-3.6 internal/controller/components/kueue
// kueue_config.go); this package does not create or modify that object.
//
// Each resource created here is stamped with the tool label and its UID is
// stored in the tool namespace (kueue_record.go): ownership is label plus UID,
// not name alone. All API access uses the authenticated user's token.
package kueue

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/serving"
)

// Objects owned by the tool. LocalQueue is intentionally not "default": Kueue
// (v0.19, RHBoK 1.5) injects kueue.x-k8s.io/queue-name: default on new objects
// for enabled integrations (Deployment, StatefulSet, Pod, job kinds) in managed
// namespaces that define a LocalQueue with that name (kubernetes-sigs/kueue
// v0.19.0 pkg/controller/jobframework/defaults.go,
// constants.DefaultLocalQueueName), which would enqueue model servers,
// workbenches, and pipeline pods in the project.
const (
	FlavorName       = "nightly-kueue-cpu"
	ClusterQueueName = "nightly-kueue"
	LocalQueueName   = "nightly-kueue"
	RayClusterName   = "nightly-raycluster"
	RayJobName       = "nightly-rayjob"
	ProfileName      = "nightly-kueue-cpu"
	// ProfileDisplayName is the Queue hardware profile's name in the dashboard.
	ProfileDisplayName = "Kueue CPU (RHOAI Nightly Updater)"
	// DefaultProject is the project name the UI tab proposes by default.
	DefaultProject = "nightly-kueue"
	// DefaultRayImage is the amd64 Ray image documented for CodeFlare SDK use
	// (Working with distributed workloads); it includes CUDA and is bulky. Doc
	// CPU tags (2.52.1-py311-cpu, -py312-cpu) target IBM Power (ppc64le only),
	// so they are not chosen here. Callers may override.
	DefaultRayImage = "quay.io/modh/ray:2.47.1-py311-cu121"
)

// API groups and versions. The dashboard consumes Kueue resources at v1beta2
// only (odh-dashboard rhoai-3.6 packages/k8s-core/src/api/models.ts), hence
// manifests use v1beta2 and setup aborts when the cluster lacks that version
// (RHBoK 1.2 exposes v1beta1 only).
const (
	APIGroup        = "kueue.x-k8s.io"
	APIVersion      = "v1beta2"
	kueueAPI        = "kueue.x-k8s.io/v1beta2"
	OperatorAPI     = "kueue.openshift.io/v1"
	rayAPI          = "ray.io/v1"
	profileAPI      = "infrastructure.opendatahub.io/v1"
	QueueNameLabel  = "kueue.x-k8s.io/queue-name"
	ProjectLabel    = envkit.KueueProjectLabel
	dashboardLabel  = "opendatahub.io/dashboard"
	instanceName    = "cluster"
	dashboardConfig = "odh-dashboard-config"
	// dashboardManager is the field manager applied to disableKueue so the
	// API server can report whether the value still originates from this tool.
	dashboardManager = "rhoai-nightly-updater-kueue-dashboard"
)

// Operator describes the Red Hat build of Kueue OLM package kueue-operator in
// openshift-kueue-operator with AllNamespaces install only (openshift/kueue-operator
// release-1.5 bundle/manifests/kueue-operator.clusterserviceversion.yaml L43,
// L325-333), so the OperatorGroup created here has an empty spec. Channels track
// minor releases (stable-v1.N); the package default channel is selected. RHOAI
// treats the operator as present only under OLM (rhods-operator rhoai-3.6
// kueue_controller_actions.go checkPreConditions: OperatorCondition
// kueue-operator.*). A kueue-controller-manager serving the API without its
// CSV implies a foreign install (upstream manifests or RHOAI 2.x embedded
// Kueue); setup rejects that because dual controllers conflict.
var Operator = envkit.OperatorSpec{ID: "kueue-operator", Title: "Red Hat build of Kueue", Pkg: "kueue-operator",
	SubName: "kueue-operator", SubNames: []string{"kueue-operator"}, Namespace: "openshift-kueue-operator",
	Source: "redhat-operators", Group: "openshift-kueue-operator", API: APIGroup, Controller: "kueue-controller-manager"}

// Sample sizing: the CodeFlare SDK's defaults for a Ray cluster
// (project-codeflare/codeflare-sdk v0.40.1, commit 9e2bb55e407a,
// src/codeflare_sdk/ray/cluster/config.py lines 325-337): head 1 CPU
// request / 2 limit and memory 5 / 8, each worker 1 / 1 CPU and memory
// 3 / 6. The SDK writes memory in decimal gigabytes (5G, config.py
// L475-478); here it is rounded to GiB (5Gi, a little more). Live, a head with 1 CPU and 2Gi (requests =
// limits) was OOMKilled about 11 s after start (2026-11-24). Requests stay
// below limits as CodeFlare sets them. The samples set neither num-cpus nor
// memory in rayStartParams, so KubeRay derives `ray start --num-cpus/--memory`
// from these container limits (ray-project/kuberay ray-operator
// controllers/ray/common/pod.go generateRayStartCommand), keeping Ray's view in
// step with the pod size. The RayJob adds its submitter pod.
var (
	headSize      = podSize{CPUMilli: 1000, CPULimitMilli: 2000, MemoryMi: 5 * 1024, MemoryLimitMi: 8 * 1024}
	workerSize    = podSize{CPUMilli: 1000, CPULimitMilli: 1000, MemoryMi: 3 * 1024, MemoryLimitMi: 6 * 1024}
	submitterSize = podSize{CPUMilli: 500, CPULimitMilli: 500, MemoryMi: 1024, MemoryLimitMi: 1024}
)

const (
	// Default capacity on the opt-in hardware profile (what the dashboard
	// fills in for a workbench on it): one workload should fit alongside
	// both samples.
	profileCPUMilli = 1000
	profileMemoryMi = 2048
	// A workbench on that profile also runs the kube-rbac-proxy sidecar
	// that RHOAI 3's notebook controller adds for inject-auth, with requests
	// of 100m CPU and 64Mi (kubeflow odh-notebook-controller, RHOAI
	// 3.7-ea.1, notebook_controller.go L69-72 and
	// notebook_mutating_webhook.go L487-494); Kueue counts every container
	// of the workbench's pod.
	sidecarCPUMilli = 100
	sidecarMemoryMi = 64
)

// modelSize is the largest model a recipe deploys (the Models catalog,
// serving.ModelCatalog): its main container's CPU, memory requested and
// its limit, and the ephemeral storage it requests for the download
// (models_storage.go), each the catalog's largest. In a Kueue project the
// dashboard deploys a model only with a Queue hardware profile of a
// LocalQueue the project has (envkit.KueueModeFor); RHOAI's hardware
// profile webhook then labels it with the queue and keeps its own
// requests (rhods-operator rhoai-3.6 internal/webhook/hardwareprofile
// mutating.go L833-884, applyIdentifiersToRequests L1121-1141), KServe
// copies kueue.x-k8s.io labels to its Deployment and pod template (kserve
// rhoai-3.6 pkg/controller/v1alpha2/llmisvc workload_single_node.go
// L414-445), and Kueue's Deployment and Pod integrations (on in RHOAI,
// rhods-operator kueue_config.go L28-33) queue its pod. So the tool's
// Queue profile's ranges hold it (serving Fits) and the quota counts it.
type modelSize struct {
	CPUMilli, MemoryMi, MemoryLimitMi, EphemeralMi int
}

// catalogModelSize is modelSize from the catalog; a value that does not
// parse counts as 0 (a test checks that every one parses).
func catalogModelSize() modelSize {
	var m modelSize
	ceil := func(v, unit float64) int { return int((v + unit - 1) / unit) }
	for _, e := range serving.ModelCatalog {
		cpu, _ := serving.ParseQuantity(e.CPU)
		req, _ := serving.ParseQuantity(e.MemoryRequest)
		limit, _ := serving.ParseQuantity(e.Memory)
		eph, _ := serving.ParseQuantity(e.EphemeralStorage)
		m.CPUMilli = max(m.CPUMilli, ceil(cpu*1000, 1))
		m.MemoryMi = max(m.MemoryMi, ceil(req, 1<<20))
		m.MemoryLimitMi = max(m.MemoryLimitMi, ceil(limit, 1<<20))
		m.EphemeralMi = max(m.EphemeralMi, ceil(eph, 1<<20))
	}
	return m
}

// Quota derives the ClusterQueue nominal limits so that everything this
// tool creates in the project can run at once: both samples (head plus
// one worker each, and the RayJob's submitter), one workbench on the
// opt-in profile (its container and the kube-rbac-proxy sidecar) and the
// largest catalog model on that profile (catalogModelSize: 4 CPUs, 12Gi
// requested, 6.2G of ephemeral storage) twice: the pod that serves and
// the one its rolling update adds. KServe sets the model Deployment's
// strategy only from spec.rolloutStrategy, which neither this tool nor
// the dashboard sets, or Recreate for confidential bare metal (kserve
// rhoai-3.7-ea.1 pkg/controller/v1alpha2/llmisvc workload_single_node.go
// L118-119 and L656-671, the same in rhoai-3.6;
// workload_confidential_odh.go L119-123), so the API server defaults it
// to RollingUpdate with maxSurge 25% (kubernetes release-1.33
// pkg/apis/apps/v1/defaults.go L44-64), rounded up to one extra pod at
// one replica (pkg/controller/deployment/util/deployment_util.go
// ResolveFenceposts L887-888). Kueue admits every pod of a Deployment as
// a Workload of its own (kubernetes-sigs/kueue release-0.19
// site/content/en/docs/tasks/run/deployment.md L15-17;
// pkg/controller/jobs/pod/pod_webhook.go L174-179 gates each pod), so the
// new pod needs quota while the old one still holds its share: without
// it, a changed model setup stayed SchedulingGated while the samples ran
// (live, RHOAI 3.7.0-ea.1). The workbench needs no such room: its
// StatefulSet deletes the old pod before it makes the new one
// (pkg/controller/statefulset/stateful_set_control.go L680-692). With
// the catalog of this version that is 13.6 CPUs and 44096Mi requested,
// rounded up to whole CPUs and GiB: 14 CPUs, 44Gi, and 12Gi of ephemeral
// storage. The documented example ClusterQueue has 9 CPUs and 36Gi
// (opendatahub-documentation
// modules/configuring-quota-management-for-distributed-workloads.adoc
// L157-174). Ephemeral storage is a covered resource because the model
// requests it, and Kueue refuses a workload that requests a resource its
// ClusterQueue does not cover (kubernetes-sigs/kueue release-0.19
// pkg/scheduler/flavorassigner/flavorassigner.go L801-806, "resource ...
// unavailable in ClusterQueue"; the docs: "You must specify a quota for
// each resource that the user can request", L177). Kueue models a
// RayCluster as a single Workload spanning its pods and counts their
// requests, not limits; free capacity on nodes is not validated
// (admission accounting only). Set up keeps a ClusterQueue that is there,
// so only a new setup gets this size; the tab flags one with another
// quota as made by an earlier version.
func Quota() (cpu, memory, ephemeral string) {
	m := catalogModelSize()
	milli := 2*(headSize.CPUMilli+workerSize.CPUMilli) + submitterSize.CPUMilli + profileCPUMilli + sidecarCPUMilli + 2*m.CPUMilli
	mi := 2*(headSize.MemoryMi+workerSize.MemoryMi) + submitterSize.MemoryMi + profileMemoryMi + sidecarMemoryMi + 2*m.MemoryMi
	return fmt.Sprint((milli + 999) / 1000), fmt.Sprintf("%dGi", (mi+1023)/1024), fmt.Sprintf("%dGi", (2*m.EphemeralMi+1023)/1024)
}

func milliText(m int) string {
	if m%1000 == 0 {
		return fmt.Sprint(m / 1000)
	}
	return fmt.Sprintf("%dm", m)
}

func miText(mi int) string {
	if mi%1024 == 0 {
		return fmt.Sprintf("%dGi", mi/1024)
	}
	return fmt.Sprintf("%dMi", mi)
}

func toolLabels() map[string]any { return envkit.ToolLabels() }

// FlavorManifest matches the docs' empty ResourceFlavor (Managing OpenShift AI,
// "Configuring quota management for distributed workloads": an "Empty Kueue
// resource flavor").
func FlavorManifest() map[string]any {
	return map[string]any{
		"apiVersion": kueueAPI, "kind": "ResourceFlavor",
		"metadata": map[string]any{"name": FlavorName, "labels": toolLabels()},
		"spec":     map[string]any{},
	}
}

// ClusterQueueManifest follows the docs' ClusterQueue with per-resource quotas
// ("You must specify a quota for each resource that the user can request"),
// scoped to the project via namespaceSelector (shared clusters replace {} with
// a label selector). Preemption is off: workbenches require a non-preemptive
// ClusterQueue (they cannot suspend), and preemption defaults to Never.
func ClusterQueueManifest(project string) map[string]any {
	cpu, memory, ephemeral := Quota()
	return map[string]any{
		"apiVersion": kueueAPI, "kind": "ClusterQueue",
		"metadata": map[string]any{"name": ClusterQueueName, "labels": toolLabels()},
		"spec": map[string]any{
			"namespaceSelector": map[string]any{"matchLabels": map[string]any{"kubernetes.io/metadata.name": project}},
			"resourceGroups": []any{map[string]any{
				"coveredResources": []any{"cpu", "memory", "ephemeral-storage"},
				"flavors": []any{map[string]any{
					"name": FlavorName,
					"resources": []any{
						map[string]any{"name": "cpu", "nominalQuota": cpu},
						map[string]any{"name": "memory", "nominalQuota": memory},
						map[string]any{"name": "ephemeral-storage", "nominalQuota": ephemeral},
					},
				}},
			}},
		},
	}
}

// LocalQueueManifest is the documented LocalQueue in the project, wired to the
// tool's ClusterQueue.
func LocalQueueManifest(project string) map[string]any {
	return map[string]any{
		"apiVersion": kueueAPI, "kind": "LocalQueue",
		"metadata": map[string]any{"name": LocalQueueName, "namespace": project, "labels": toolLabels()},
		"spec":     map[string]any{"clusterQueue": ClusterQueueName},
	}
}

var rayVersionTag = regexp.MustCompile(`^(\d+\.\d+\.\d+)`)

// rayVersion returns the leading semver from an image tag ("2.47.1" from
// quay.io/modh/ray:2.47.1-py311-cu121), or "" when absent.
func rayVersion(image string) string {
	i := strings.LastIndex(image, ":")
	if i < 0 || strings.Contains(image[i:], "/") {
		return ""
	}
	return rayVersionTag.FindString(image[i+1:])
}

// podSize is a sample container's CPU (milli) and memory (MiB) requests
// and limits: the manifests are built from it and a live sample is
// compared with it.
type podSize struct {
	CPUMilli, CPULimitMilli, MemoryMi, MemoryLimitMi int
}

// quantities are the size as the manifests write it, by "requests" and
// "limits", then "cpu" and "memory".
func (z podSize) quantities() map[string]map[string]string {
	return map[string]map[string]string{
		"requests": {"cpu": milliText(z.CPUMilli), "memory": miText(z.MemoryMi)},
		"limits":   {"cpu": milliText(z.CPULimitMilli), "memory": miText(z.MemoryLimitMi)},
	}
}

// resources is the container's resources field.
func (z podSize) resources() map[string]any {
	out := map[string]any{}
	for kind, values := range z.quantities() {
		m := map[string]any{}
		for name, v := range values {
			m[name] = v
		}
		out[kind] = m
	}
	return out
}

// matches compares a live container's requests and limits with the size,
// as amounts ("1000m" is "1"). A value that is missing differs; known is
// false when a value cannot be read (nothing can be said then).
func (z podSize) matches(requests, limits map[string]kube.Quantity) (same, known bool) {
	unknown := false
	for kind, values := range z.quantities() {
		live := requests
		if kind == "limits" {
			live = limits
		}
		for name, want := range values {
			got, ok := live[name]
			if !ok || got == "" {
				return false, true
			}
			eq, readable := kube.SameQuantity(string(got), want)
			switch {
			case !readable:
				unknown = true
			case !eq:
				return false, true
			}
		}
	}
	return !unknown, !unknown
}

// text is "1 CPU and 5Gi requested, up to 2 CPUs and 8Gi".
func (z podSize) text() string {
	return fmt.Sprintf("%s requested, up to %s", sizeText(z.CPUMilli, z.MemoryMi), sizeText(z.CPULimitMilli, z.MemoryLimitMi))
}

// sizeText is "1 CPU and 5Gi" for milli CPUs and mebibytes.
func sizeText(milli, mi int) string {
	unit := "CPUs"
	if milli == 1000 {
		unit = "CPU"
	}
	return fmt.Sprintf("%s %s and %s", milliText(milli), unit, miText(mi))
}

// RayClusterSpec is the shared cluster shape for both samples: head plus one
// worker on CPU only (mirrors odh-dashboard rhoai-3.6
// packages/cypress/cypress/fixtures/resources/yaml/ray-job.yaml), sized as the
// CodeFlare SDK's defaults.
func RayClusterSpec(image string) map[string]any {
	spec := map[string]any{
		"headGroupSpec": map[string]any{
			"rayStartParams": map[string]any{"dashboard-host": "0.0.0.0"},
			"template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{
				"name": "ray-head", "image": image,
				"ports": []any{
					map[string]any{"containerPort": 6379, "name": "gcs-server"},
					map[string]any{"containerPort": 8265, "name": "dashboard"},
					map[string]any{"containerPort": 10001, "name": "client"},
				},
				"resources": headSize.resources(),
			}}}},
		},
		"workerGroupSpecs": []any{map[string]any{
			"groupName": "small-group", "replicas": 1, "minReplicas": 1, "maxReplicas": 1,
			"rayStartParams": map[string]any{},
			"template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{
				"name": "ray-worker", "image": image,
				"resources": workerSize.resources(),
			}}}},
		}},
	}
	if v := rayVersion(image); v != "" {
		spec["rayVersion"] = v
	}
	return spec
}

func sampleMeta(name, project string) map[string]any {
	labels := toolLabels()
	labels[QueueNameLabel] = LocalQueueName
	return map[string]any{"name": name, "namespace": project, "labels": labels}
}

// RayClusterManifest is the sample RayCluster surfaced on the dashboard
// Distributed workloads page (Workload owner and pod owner are RayCluster per
// odh-dashboard rhoai-3.6 concepts/distributedWorkloads utils.ts), not under
// Infrastructure, Workloads.
func RayClusterManifest(project, image string) map[string]any {
	return map[string]any{"apiVersion": rayAPI, "kind": "RayCluster", "metadata": sampleMeta(RayClusterName, project), "spec": RayClusterSpec(image)}
}

// RayJobEntrypoint dumps cluster resources then sleeps five minutes so the
// job remains observable while running.
const RayJobEntrypoint = `python -c "import ray, time; ray.init(); print(ray.cluster_resources()); time.sleep(300)"`

// RayJobManifest is the companion RayJob sample: it appears on Observe &
// monitor, Infrastructure, Workloads (packages/gpuaas infrastructureWorkloads.ts
// lists RayJobs), and its Workload shows on Distributed workloads without
// usage metrics there. The cluster is torn down when the job ends.
func RayJobManifest(project, image string) map[string]any {
	return map[string]any{"apiVersion": rayAPI, "kind": "RayJob", "metadata": sampleMeta(RayJobName, project), "spec": map[string]any{
		"entrypoint":               RayJobEntrypoint,
		"shutdownAfterJobFinishes": true,
		"submitterPodTemplate": map[string]any{"spec": map[string]any{
			"restartPolicy": "Never",
			"containers": []any{map[string]any{
				"name": "ray-job-submitter", "image": image,
				"resources": submitterSize.resources(),
			}},
		}},
		"rayClusterSpec": RayClusterSpec(image),
	}}
}

// ProfileManifest defines a Queue hardware profile targeting the tool LocalQueue
// in the applications namespace, matching what the dashboard Create hardware
// profile flow emits (odh-dashboard rhoai-3.6 frontend/src/api/k8s/hardwareProfiles.ts
// assembleHardwareProfile, globally visible), with the tool label added. Only
// namespaces that contain the LocalQueue surface the profile (kueueUtils.ts
// filterProfilesByKueue). Its ranges also hold the largest catalog model
// (catalogModelSize), which the dashboard deploys in a Kueue project only
// with such a profile.
func ProfileManifest(apps string, now string) map[string]any {
	m := catalogModelSize()
	return map[string]any{
		"apiVersion": profileAPI, "kind": "HardwareProfile",
		"metadata": map[string]any{
			"name": ProfileName, "namespace": apps, "labels": toolLabels(),
			"annotations": map[string]any{
				"opendatahub.io/display-name":                 ProfileDisplayName,
				"opendatahub.io/description":                  "Sends workloads to LocalQueue " + LocalQueueName + ", which this tool set up.",
				"opendatahub.io/disabled":                     "false",
				"opendatahub.io/modified-date":                now,
				"opendatahub.io/dashboard-feature-visibility": "[]",
			},
		},
		"spec": map[string]any{
			"identifiers": []any{
				map[string]any{"identifier": "cpu", "displayName": "CPU", "resourceType": "CPU", "defaultCount": milliText(profileCPUMilli), "minCount": milliText(profileCPUMilli), "maxCount": milliText(max(2000, m.CPUMilli))},
				map[string]any{"identifier": "memory", "displayName": "Memory", "resourceType": "Memory", "defaultCount": miText(profileMemoryMi), "minCount": "1Gi", "maxCount": miText(max(4096, m.MemoryLimitMi))},
			},
			"scheduling": map[string]any{"type": "Queue", "kueue": map[string]any{"localQueueName": LocalQueueName}},
		},
	}
}

// Sample kinds as named in setup requests.
const (
	SampleRayCluster = "raycluster"
	SampleRayJob     = "rayjob"
)

var projectName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// imageRef matches image references that contain no spaces or shell metacharacters.
var imageRef = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._/:@+-]*$`)

// ValidImage returns a human-readable reason when image cannot be used for samples, or "".
func ValidImage(image string) string {
	if len(image) > 512 || !imageRef.MatchString(image) {
		return "rayImage must be an image reference such as " + DefaultRayImage
	}
	return ""
}

// sampleText builds a display label for a sample kind.
func sampleText(kind string) string {
	if kind == SampleRayJob {
		return "RayJob " + RayJobName
	}
	return "RayCluster " + RayClusterName
}

func describeQuota() string {
	return quotaText(Quota())
}

// quotaText is "14 CPUs, 44Gi of memory and 12Gi of ephemeral storage";
// a quota not set reads "no" ("no ephemeral storage").
func quotaText(cpu, memory, ephemeral string) string {
	eph := "no ephemeral storage"
	if ephemeral != "" {
		eph = ephemeral + " of ephemeral storage"
	}
	return fmt.Sprintf("%s CPUs, %s of memory and %s", kube.FirstNonEmpty(cpu, "no"), kube.FirstNonEmpty(memory, "no"), eph)
}

// HasToolLabel reports whether the object carries the tool label (ownership half).
func hasToolLabel(m kube.ObjectMeta) bool { return m.HasToolLabel() }
