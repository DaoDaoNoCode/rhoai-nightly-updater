package cluster

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const (
	minioNamespace   = "minio"
	minioStorage     = "20Gi"
	minioBucket      = "pipelines"
	minioServiceName = "minio-service"
	minioAppSelector = "app=minio"
	// minioDefaultImage is Red Hat's Project Hummingbird build of the final
	// open-source MinIO release, RELEASE.2025-10-15T17-29-55Z
	// (quay.io/hummingbird-community/minio:release.2025-10-15t17-29-55z,
	// built from github.com/minio/minio at that tag by
	// gitlab.com/redhat/hummingbird/containers images/minio), pinned by its
	// multi-arch index digest. It is anonymously pullable (quay.io anonymous
	// pull token; index, linux/amd64 manifest and config GET all return 200)
	// and runs as a non-root user, so restricted-v2 can assign the UID.
	// It is not affected by CVE-2023-28434 (fixed in
	// RELEASE.2023-03-20T20-16-18Z, GHSA-2pxw-r47w-4p8c) or CVE-2025-62506
	// (fixed in this release, GHSA-jjjj-jwhf-8rgr). The official
	// quay.io/minio/minio and docker.io/minio/minio repositories no longer
	// allow anonymous pulls (401), and minio/minio is archived: advisories
	// published after this release are fixed only in the commercial AIStor
	// (for example GHSA-hv4r-mvr4-25vw), which is why the S3 API is not
	// exposed outside the cluster and the root user is random.
	//
	// Released versions deployed quay.io/minio/minio:latest with the same
	// container contract (MINIO_ROOT_*, console on :9090), so this release
	// reads their volumes ("xl-single" backend). The 2019 release
	// (quay.io/opendatahub/minio, the DSPO README sample) is not an option:
	// it is affected by CVE-2023-28434, and its "fs" backend is unreadable
	// by newer releases (checked: RELEASE.2025-10-15 exits with "Unable to
	// use the drive ... drive not found" on a volume the 2019 release
	// initialised).
	minioDefaultImage = "quay.io/hummingbird-community/minio@sha256:25268b5a6539d9ffc7d23b89a2ba846d12a49aac4e81172336700222818d5f45"
	// minioConsolePort is the console port released versions used.
	minioConsolePort = 9090
)

var (
	// MinIOReadyTimeout bounds how long setup waits for the MinIO pod. It
	// stays below the frontend's 120s request timeout. Tests shorten it.
	MinIOReadyTimeout = 90 * time.Second
	MinIOReadyPoll    = 3 * time.Second
	// MinIODeleteTimeout bounds how long teardown waits for the namespace
	// (or, when it is kept, the data PVC) to disappear before reporting that
	// deletion is still in progress.
	MinIODeleteTimeout = 60 * time.Second
	MinIODeletePoll    = 2 * time.Second

	// minioBucketCreator creates the pipelines bucket; tests replace it
	// because the in-cluster S3 endpoint is unreachable from unit tests.
	minioBucketCreator    = createMinioBucket
	minioBucketRetryDelay = 3 * time.Second
)

// minioImage returns the MinIO image, overridable with MINIO_IMAGE (for
// example a mirror on a disconnected cluster). An override must be a MinIO
// release that reads MINIO_ROOT_USER/MINIO_ROOT_PASSWORD and supports
// --console-address (RELEASE.2021-07-08 or later).
func minioImage() string {
	if img := strings.TrimSpace(os.Getenv("MINIO_IMAGE")); img != "" {
		return img
	}
	return minioDefaultImage
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// minioCredentials returns the MinIO root user and password. Explicit
// MINIO_ROOT_USER / MINIO_ROOT_PASSWORD env vars win. Otherwise credentials
// already stored in minio-secret are reused, because pipeline servers copy
// them into their own secrets and MinIO picks up a changed secret on restart.
// Only a first-time setup generates them, both random, so no credential is
// baked into the binary or guessable.
func minioCredentials(c *Client) (user, password string, err error) {
	user = os.Getenv("MINIO_ROOT_USER")
	password = os.Getenv("MINIO_ROOT_PASSWORD")
	if user == "" || password == "" {
		body, _, getErr := c.get(namespacedPath("v1", "secrets", minioNamespace, "minio-secret"))
		switch {
		case getErr == nil:
			var secret struct {
				Data map[string]string `json:"data"`
			}
			if jsonErr := json.Unmarshal(body, &secret); jsonErr != nil {
				return "", "", fmt.Errorf("parse existing minio-secret: %w", jsonErr)
			}
			if user == "" {
				user = decodeBase64Field(secret.Data["minio_root_user"])
			}
			if password == "" {
				password = decodeBase64Field(secret.Data["minio_root_password"])
			}
		case !IsK8sError(getErr, 404):
			return "", "", fmt.Errorf("read existing minio-secret: %w", getErr)
		}
	}
	if user == "" {
		suffix, randErr := randomHex(6)
		if randErr != nil {
			return "", "", fmt.Errorf("generate MinIO user: %w", randErr)
		}
		user = "minio-" + suffix
	}
	if password == "" {
		if password, err = randomHex(16); err != nil {
			return "", "", fmt.Errorf("generate MinIO password: %w", err)
		}
	}
	return user, password, nil
}

// minioNamespaceInfo describes the minio namespace and who created it.
type minioNamespaceInfo struct {
	Exists  bool
	Meta    objectMeta
	Phase   string
	Managed bool
	// Legacy is true for a namespace created by an older version of the
	// tool, which set no ownership label.
	Legacy bool
}

// lookupMinIONamespace reads the minio namespace and decides whether this
// tool created it. Older versions created it with a POST that set no
// User-Agent (manager "Go-http-client") and then server-side applied
// Deployment "minio" with fieldManager rhoai-nightly-updater. Both markers
// are required, so a namespace someone else created is never claimed.
func lookupMinIONamespace(c *Client) (minioNamespaceInfo, error) {
	body, _, err := c.get("/api/v1/namespaces/" + minioNamespace)
	if IsK8sError(err, 404) {
		return minioNamespaceInfo{}, nil
	}
	if err != nil {
		return minioNamespaceInfo{}, fmt.Errorf("read namespace %s: %w", minioNamespace, err)
	}
	var nsObj struct {
		Metadata objectMeta `json:"metadata"`
		Status   struct {
			Phase string `json:"phase"`
		} `json:"status"`
	}
	if err := json.Unmarshal(body, &nsObj); err != nil {
		return minioNamespaceInfo{}, fmt.Errorf("parse namespace %s: %w", minioNamespace, err)
	}
	meta := nsObj.Metadata
	info := minioNamespaceInfo{Exists: true, Meta: meta, Phase: nsObj.Status.Phase}
	if meta.hasToolLabel() {
		info.Managed = true
		return info, nil
	}
	if !meta.createdBy(legacyPostManager, "Update") {
		return info, nil
	}
	deployBody, _, err := c.get(namespacedPath("apps/v1", "deployments", minioNamespace, "minio"))
	if IsK8sError(err, 404) {
		return info, nil
	}
	if err != nil {
		return info, fmt.Errorf("read deployment %s/minio: %w", minioNamespace, err)
	}
	if deployMeta, perr := parseObjectMeta(deployBody); perr == nil && deployMeta.createdByToolApply() {
		info.Managed = true
		info.Legacy = true
	}
	return info, nil
}

func (n minioNamespaceInfo) terminating() bool {
	return n.Meta.terminating() || n.Phase == "Terminating"
}

// minioObjectOwned reports whether the tool created an object in the minio
// namespace: it carries the ownership label, or an earlier version created
// it by server-side apply (see createdBy).
func minioObjectOwned(m objectMeta) bool {
	return m.hasToolLabel() || m.createdByToolApply()
}

// listMinIOPVCs lists the PVCs in the minio namespace. PVCs are listed rather
// than read by name, so the RBAC rules need no "get" on them.
func listMinIOPVCs(c *Client) ([]objectMeta, error) {
	body, _, err := c.get(namespacedPath("v1", "persistentvolumeclaims", minioNamespace, ""))
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []struct {
			Metadata objectMeta `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("parse PVC list: %w", err)
	}
	out := make([]objectMeta, 0, len(list.Items))
	for _, item := range list.Items {
		out = append(out, item.Metadata)
	}
	return out, nil
}

// readMinIOPVC returns minio-pvc, and whether it exists.
func readMinIOPVC(c *Client) (objectMeta, bool, error) {
	pvcs, err := listMinIOPVCs(c)
	if err != nil {
		return objectMeta{}, false, err
	}
	for _, m := range pvcs {
		if m.Name == "minio-pvc" {
			return m, true, nil
		}
	}
	return objectMeta{}, false, nil
}

type minioDeployment struct {
	Metadata struct {
		Generation int64 `json:"generation"`
	} `json:"metadata"`
	Spec struct {
		Replicas *int `json:"replicas"`
		Template struct {
			Spec struct {
				Containers []struct {
					Name  string `json:"name"`
					Image string `json:"image"`
				} `json:"containers"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
	Status struct {
		ObservedGeneration int64 `json:"observedGeneration"`
		Replicas           int   `json:"replicas"`
		ReadyReplicas      int   `json:"readyReplicas"`
		UpdatedReplicas    int   `json:"updatedReplicas"`
	} `json:"status"`
}

// rolledOut reports whether the current template has a ready replica.
func (d minioDeployment) rolledOut() bool {
	return d.Status.ObservedGeneration >= d.Metadata.Generation &&
		d.Status.UpdatedReplicas >= 1 && d.Status.ReadyReplicas >= 1
}

func (d minioDeployment) image() string {
	for _, ctr := range d.Spec.Template.Spec.Containers {
		if ctr.Name == "minio" {
			return ctr.Image
		}
	}
	return ""
}

func getMinIOStatus(c *Client) types.ResourceState {
	state, _, _ := minioStatusAndEndpoints(c)
	return state
}

// minioStatusAndEndpoints returns MinIO's status and the addresses a
// pipeline server may use to reach it (for the teardown dependency check).
// The error is non-nil when those addresses could not be read.
func minioStatusAndEndpoints(c *Client) (types.ResourceState, minioEndpoints, error) {
	state := types.ResourceState{Namespace: minioNamespace}

	ns, err := lookupMinIONamespace(c)
	if err != nil {
		state.Message = "Cannot read MinIO status: " + err.Error()
		return state, minioEndpoints{}, nil
	}
	if !ns.Exists {
		state.Message = "Not deployed"
		return state, minioEndpoints{}, nil
	}
	state.ManagedByTool = ns.Managed
	if ns.terminating() {
		state.Terminating = true
		state.Message = "Terminating"
		state.SetupBlockedReason = "Namespace 'minio' is still being deleted."
		return state, minioEndpoints{}, nil
	}
	if !ns.Managed {
		state.SetupBlockedReason = "Namespace 'minio' exists but was not created by this tool, so setup will not modify it."
		state.TeardownBlockedReason = "Namespace 'minio' was not created by this tool."
	}

	var (
		wg           sync.WaitGroup
		deployBody   []byte
		deployErr    error
		apiHost      string
		uiHost       string
		endpoints    minioEndpoints
		endpointsErr error
		pvcs         []objectMeta
		pvcErr       error
	)
	wg.Add(3)
	go func() {
		defer wg.Done()
		deployBody, _, deployErr = c.get(namespacedPath("apps/v1", "deployments", minioNamespace, "minio"))
	}()
	go func() {
		defer wg.Done()
		endpoints, endpointsErr = readMinIOEndpoints(c)
		apiHost, uiHost = endpoints.apiHost, endpoints.uiHost
	}()
	go func() {
		defer wg.Done()
		if ns.Managed {
			pvcs, pvcErr = listMinIOPVCs(c)
		}
	}()
	wg.Wait()

	if apiHost != "" {
		state.APIRoute = "https://" + apiHost
		state.Warning = "Route 'minio-api' exposes MinIO's S3 API outside the cluster. Re-run MinIO setup to remove it; setup keeps it while a pipeline server uses its host."
	}
	if uiHost != "" {
		state.UIRoute = "https://" + uiHost
	}
	if ns.Managed && pvcErr == nil {
		for _, m := range pvcs {
			if m.Name == "minio-pvc" && minioObjectOwned(m) {
				state.DataPVCs = append(state.DataPVCs, m.Name)
			}
		}
	}

	if deployErr != nil {
		if IsK8sError(deployErr, 404) {
			state.Message = "Namespace exists but MinIO not deployed"
		} else {
			state.Message = "Cannot read MinIO deployment: " + deployErr.Error()
		}
		if !ns.Managed {
			state.Message = state.SetupBlockedReason
		}
		return state, endpoints, endpointsErr
	}
	state.Deployed = true

	var deploy minioDeployment
	if err := json.Unmarshal(deployBody, &deploy); err != nil {
		state.Message = "Cannot parse MinIO deployment"
		return state, endpoints, endpointsErr
	}
	state.CurrentImage = deploy.image()
	if ns.Managed && state.CurrentImage != "" && state.CurrentImage != minioImage() {
		state.Warning = strings.TrimSpace(fmt.Sprintf("MinIO runs %s, not the image this version deploys (%s). Re-run MinIO setup to update it; the data PVC is kept. %s", state.CurrentImage, minioImage(), state.Warning))
	}
	if deploy.Status.ReadyReplicas >= 1 {
		state.Ready = true
		state.Message = "Running"
		return state, endpoints, endpointsErr
	}
	state.Message = fmt.Sprintf("%d/%d ready", deploy.Status.ReadyReplicas, deploy.Status.Replicas)
	if issue, err := inspectPods(c, minioNamespace, minioAppSelector, ""); err == nil {
		applyPodIssue(&state, issue)
	}
	return state, endpoints, endpointsErr
}

// minioApplyPath returns the API path of one object from minioResources.
// Only the fixed objects the tool creates are accepted; the literal names
// keep the RBAC rules limited to them (pkg/cluster/rbac_coverage_test.go).
func minioApplyPath(obj map[string]interface{}) (string, bool) {
	meta, _ := obj["metadata"].(map[string]interface{})
	name, _ := meta["name"].(string)
	switch kind, _ := obj["kind"].(string); {
	case kind == "PersistentVolumeClaim" && name == "minio-pvc":
		return namespacedPath("v1", "persistentvolumeclaims", minioNamespace, "minio-pvc"), true
	case kind == "Secret" && name == "minio-secret":
		return namespacedPath("v1", "secrets", minioNamespace, "minio-secret"), true
	case kind == "Deployment" && name == "minio":
		return namespacedPath("apps/v1", "deployments", minioNamespace, "minio"), true
	case kind == "Service" && name == minioServiceName:
		return namespacedPath("v1", "services", minioNamespace, minioServiceName), true
	case kind == "Route" && name == "minio-ui":
		return namespacedPath("route.openshift.io/v1", "routes", minioNamespace, "minio-ui"), true
	}
	return "", false
}

// minioCreatePath returns the collection path a minioResources object is
// created at with POST.
func minioCreatePath(kind string) (string, bool) {
	switch kind {
	case "PersistentVolumeClaim":
		return withToolFieldManager(namespacedPath("v1", "persistentvolumeclaims", minioNamespace, "")), true
	case "Secret":
		return withToolFieldManager(namespacedPath("v1", "secrets", minioNamespace, "")), true
	case "Deployment":
		return withToolFieldManager(namespacedPath("apps/v1", "deployments", minioNamespace, "")), true
	case "Service":
		return withToolFieldManager(namespacedPath("v1", "services", minioNamespace, "")), true
	case "Route":
		return withToolFieldManager(namespacedPath("route.openshift.io/v1", "routes", minioNamespace, "")), true
	}
	return "", false
}

// minioGetPath returns the path a named object in the minio namespace is
// read at. PVCs are not read by name (see listMinIOPVCs).
func minioGetPath(kind, name string) (string, bool) {
	switch {
	case kind == "Secret" && name == "minio-secret":
		return namespacedPath("v1", "secrets", minioNamespace, "minio-secret"), true
	case kind == "Deployment" && name == "minio":
		return namespacedPath("apps/v1", "deployments", minioNamespace, "minio"), true
	case kind == "Service" && name == minioServiceName:
		return namespacedPath("v1", "services", minioNamespace, minioServiceName), true
	case kind == "Route" && name == "minio-api":
		return namespacedPath("route.openshift.io/v1", "routes", minioNamespace, "minio-api"), true
	case kind == "Route" && name == "minio-ui":
		return namespacedPath("route.openshift.io/v1", "routes", minioNamespace, "minio-ui"), true
	}
	return "", false
}

// minioDeletePath returns the path of a tool object teardown (or setup, for
// a Route that is no longer wanted) may delete.
func minioDeletePath(kind, name string) (string, bool) {
	switch {
	case kind == "PersistentVolumeClaim" && name == "minio-pvc":
		return namespacedPath("v1", "persistentvolumeclaims", minioNamespace, "minio-pvc"), true
	case kind == "Secret" && name == "minio-secret":
		return namespacedPath("v1", "secrets", minioNamespace, "minio-secret"), true
	case kind == "Deployment" && name == "minio":
		return namespacedPath("apps/v1", "deployments", minioNamespace, "minio"), true
	case kind == "Service" && name == minioServiceName:
		return namespacedPath("v1", "services", minioNamespace, minioServiceName), true
	case kind == "Route" && name == "minio-api":
		return namespacedPath("route.openshift.io/v1", "routes", minioNamespace, "minio-api"), true
	case kind == "Route" && name == "minio-ui":
		return namespacedPath("route.openshift.io/v1", "routes", minioNamespace, "minio-ui"), true
	}
	return "", false
}

// readMinIOObject reads one named object in the minio namespace.
func readMinIOObject(c *Client, kind, name string) (objectMeta, bool, error) {
	if kind == "PersistentVolumeClaim" {
		return readMinIOPVC(c)
	}
	path, ok := minioGetPath(kind, name)
	if !ok {
		return objectMeta{}, false, fmt.Errorf("internal error: unexpected MinIO object %s %s", kind, name)
	}
	body, _, err := c.get(path)
	if IsK8sError(err, 404) {
		return objectMeta{}, false, nil
	}
	if err != nil {
		return objectMeta{}, false, err
	}
	meta, err := parseObjectMeta(body)
	return meta, true, err
}

type minioResource struct {
	name string // shown in logs, for example "PVC"
	kind string
	obj  map[string]interface{}
}

// minioResources returns the objects SetupMinIO creates or updates, in
// order. The container contract is the one released versions used
// ("server /data --console-address :9090", MINIO_ROOT_USER and
// MINIO_ROOT_PASSWORD), so re-running setup on their installs only changes
// the image. Only the console gets a Route: the S3 API stays inside the
// cluster, where pipeline servers reach it through the service.
func minioResources(user, password string) []minioResource {
	meta := func(name string) map[string]interface{} {
		return map[string]interface{}{"name": name, "namespace": minioNamespace, "labels": toolLabels()}
	}
	secretEnv := func(name, key string) map[string]interface{} {
		return map[string]interface{}{"name": name, "valueFrom": map[string]interface{}{"secretKeyRef": map[string]interface{}{"name": "minio-secret", "key": key}}}
	}
	return []minioResource{
		{"PVC", "PersistentVolumeClaim", map[string]interface{}{
			"apiVersion": "v1", "kind": "PersistentVolumeClaim",
			"metadata": meta("minio-pvc"),
			"spec": map[string]interface{}{
				"accessModes": []string{"ReadWriteOnce"},
				"resources":   map[string]interface{}{"requests": map[string]interface{}{"storage": minioStorage}},
				"volumeMode":  "Filesystem",
			},
		}},
		{"Secret", "Secret", map[string]interface{}{
			"apiVersion": "v1", "kind": "Secret",
			"metadata": meta("minio-secret"),
			"stringData": map[string]interface{}{
				"minio_root_user":     user,
				"minio_root_password": password,
			},
		}},
		{"Deployment", "Deployment", map[string]interface{}{
			"apiVersion": "apps/v1", "kind": "Deployment",
			"metadata": meta("minio"),
			"spec": map[string]interface{}{
				"replicas": 1,
				"selector": map[string]interface{}{"matchLabels": map[string]interface{}{"app": "minio"}},
				"strategy": map[string]interface{}{"type": "Recreate"},
				"template": map[string]interface{}{
					"metadata": map[string]interface{}{"labels": map[string]interface{}{"app": "minio"}},
					"spec": map[string]interface{}{
						"volumes": []map[string]interface{}{
							{"name": "data", "persistentVolumeClaim": map[string]interface{}{"claimName": "minio-pvc"}},
						},
						"containers": []map[string]interface{}{{
							"name":  "minio",
							"image": minioImage(),
							"args":  []string{"server", "/data", "--console-address", fmt.Sprintf(":%d", minioConsolePort)},
							"env": []map[string]interface{}{
								secretEnv("MINIO_ROOT_USER", "minio_root_user"),
								secretEnv("MINIO_ROOT_PASSWORD", "minio_root_password"),
							},
							"ports": []map[string]interface{}{
								{"containerPort": 9000, "protocol": "TCP"},
								{"containerPort": minioConsolePort, "protocol": "TCP"},
							},
							"volumeMounts": []map[string]interface{}{
								{"name": "data", "mountPath": "/data", "subPath": "minio"},
							},
							"resources": map[string]interface{}{
								"limits":   map[string]interface{}{"cpu": "250m", "memory": "1Gi"},
								"requests": map[string]interface{}{"cpu": "20m", "memory": "100Mi"},
							},
							// Restricted pod security without a fixed UID, so
							// OpenShift's restricted-v2 SCC assigns one.
							"securityContext": map[string]interface{}{
								"allowPrivilegeEscalation": false,
								"capabilities":             map[string]interface{}{"drop": []string{"ALL"}},
								"runAsNonRoot":             true,
								"seccompProfile":           map[string]interface{}{"type": "RuntimeDefault"},
							},
							"readinessProbe": map[string]interface{}{"tcpSocket": map[string]interface{}{"port": 9000}, "initialDelaySeconds": 5, "periodSeconds": 5},
							"livenessProbe":  map[string]interface{}{"tcpSocket": map[string]interface{}{"port": 9000}, "initialDelaySeconds": 30, "periodSeconds": 5},
						}},
						"restartPolicy": "Always",
					},
				},
			},
		}},
		{"Service", "Service", map[string]interface{}{
			"apiVersion": "v1", "kind": "Service",
			"metadata": meta(minioServiceName),
			"spec": map[string]interface{}{
				"selector": map[string]interface{}{"app": "minio"},
				"type":     "ClusterIP",
				"ports": []map[string]interface{}{
					{"name": "api", "port": 9000, "targetPort": 9000},
					{"name": "ui", "port": minioConsolePort, "targetPort": minioConsolePort},
				},
			},
		}},
		{"Console Route", "Route", map[string]interface{}{
			"apiVersion": "route.openshift.io/v1", "kind": "Route",
			"metadata": meta("minio-ui"),
			"spec": map[string]interface{}{
				"to":   map[string]interface{}{"kind": "Service", "name": minioServiceName, "weight": 100},
				"port": map[string]interface{}{"targetPort": "ui"},
				"tls":  map[string]interface{}{"termination": "edge", "insecureEdgeTerminationPolicy": "Redirect"},
			},
		}},
	}
}

func recordMinIOActivity(c *Client, action, detail string, success bool) {
	RecordActivity(c, types.ActivityEntry{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		User:      getUser(c),
		Action:    action,
		Detail:    detail,
		Success:   success,
	})
}

// minioForeignHint tells the user how to resolve a refusal over an object
// that has a name the tool uses but no ownership label.
const minioForeignHint = "If an earlier version of this tool created it, label it with `oc label -n minio <kind>/<name> app.kubernetes.io/managed-by=rhoai-nightly-updater` and re-run setup; otherwise delete or rename it."

// SetupMinIO deploys MinIO with a bucket for pipeline artifacts. It reports
// success only once the MinIO pod is ready and the bucket exists. Re-running
// it updates the objects it created, so it also repairs a partial setup. It
// never changes an object it did not create: every object is checked first,
// absent ones are created with POST, and updates are guarded by the
// resourceVersion that was checked (see ensureToolObject).
func SetupMinIO(c *Client) (*types.OperationResponse, error) {
	logs := []string{}
	fail := func(msg, code, activity string) (*types.OperationResponse, error) {
		slog.Warn("minio setup failed", "reason", activity, "user", getUser(c))
		recordMinIOActivity(c, "setup-minio", fmt.Sprintf("namespace=%s (%s)", minioNamespace, activity), false)
		return &types.OperationResponse{Success: false, Message: msg, Logs: logs, ErrorCode: code}, nil
	}

	// Step 1: Namespace. Never adopt a namespace this tool did not create.
	logs = append(logs, "Creating namespace...")
	ns, err := lookupMinIONamespace(c)
	if err != nil {
		return fail(fmt.Sprintf("Cannot check namespace '%s': %v", minioNamespace, err), errorCodeFromK8sErr(err), "namespace check failed")
	}
	if !ns.Exists {
		nsData, _ := json.Marshal(map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Namespace",
			"metadata":   map[string]interface{}{"name": minioNamespace, "labels": toolLabels()},
		})
		_, _, err := c.post(withToolFieldManager("/api/v1/namespaces"), nsData)
		switch {
		case err == nil:
			logs = append(logs, "OK: Namespace created")
			ns = minioNamespaceInfo{Exists: true, Managed: true}
		case IsK8sError(err, 409):
			// Created concurrently: decide ownership again.
			if ns, err = lookupMinIONamespace(c); err != nil {
				return fail(fmt.Sprintf("Cannot check namespace '%s': %v", minioNamespace, err), errorCodeFromK8sErr(err), "namespace check failed")
			}
		default:
			return fail(fmt.Sprintf("Failed to create namespace: %v", err), errorCodeFromK8sErr(err), "namespace creation failed")
		}
	}
	if ns.Exists && ns.terminating() {
		return fail("Namespace 'minio' is terminating. Wait for it to fully delete before re-creating.", "terminating", "namespace terminating")
	}
	if !ns.Managed {
		return fail("Namespace 'minio' already exists and was not created by this tool. Nothing was changed. Remove it yourself, or use a cluster without it.", "not_managed", "namespace not managed")
	}

	// Step 2: Check every object the tool writes before writing any, so a
	// refusal leaves everything unchanged.
	type current struct {
		meta  objectMeta
		found bool
	}
	existing := map[string]current{}
	var foreign, deleting []string
	for _, ref := range []struct{ kind, name string }{
		{"PersistentVolumeClaim", "minio-pvc"},
		{"Secret", "minio-secret"},
		{"Deployment", "minio"},
		{"Service", minioServiceName},
		{"Route", "minio-ui"},
		{"Route", "minio-api"},
	} {
		meta, found, err := readMinIOObject(c, ref.kind, ref.name)
		if err != nil {
			return fail(fmt.Sprintf("Cannot read %s %s/%s; nothing was changed: %v", ref.kind, minioNamespace, ref.name, err), errorCodeFromK8sErr(err), "read failed")
		}
		existing[ref.kind+"/"+ref.name] = current{meta, found}
		switch {
		case !found:
		case !minioObjectOwned(meta):
			foreign = append(foreign, ref.kind+" "+ref.name)
		case meta.terminating():
			deleting = append(deleting, ref.kind+" "+ref.name)
		}
	}
	if len(foreign) > 0 {
		return fail(fmt.Sprintf("Namespace 'minio' already contains %s, not created by this tool. Nothing was changed. %s", strings.Join(foreign, ", "), minioForeignHint), "not_managed", "foreign objects: "+strings.Join(foreign, ", "))
	}
	if len(deleting) > 0 {
		return fail(fmt.Sprintf("%s in namespace 'minio' is still being deleted. Wait until it is gone, then retry.", strings.Join(deleting, ", ")), "terminating", "objects terminating")
	}
	if ns.Legacy {
		patch, _ := json.Marshal(map[string]interface{}{"metadata": map[string]interface{}{"labels": toolLabels()}})
		if _, _, err := c.patch("/api/v1/namespaces/"+minioNamespace, patch); err != nil {
			return fail(fmt.Sprintf("Failed to label namespace created by an earlier version: %v", err), errorCodeFromK8sErr(err), "namespace label failed")
		}
		logs = append(logs, "OK: Namespace from an earlier version labelled as managed by this tool")
	} else if ns.Meta.Name != "" {
		logs = append(logs, "Namespace already exists (created by this tool) — reusing")
	}

	// Step 3: Create or update the objects.
	minioUser, minioPass, credErr := minioCredentials(c)
	if credErr != nil {
		return fail(fmt.Sprintf("Cannot determine MinIO credentials: %v", credErr), errorCodeFromK8sErr(credErr), "credentials")
	}
	var applied []string
	for _, r := range minioResources(minioUser, minioPass) {
		logs = append(logs, fmt.Sprintf("Applying %s...", r.name))
		path, ok := minioApplyPath(r.obj)
		createPath, createOK := minioCreatePath(r.kind)
		if !ok || !createOK {
			return fail(fmt.Sprintf("Internal error: unexpected MinIO object %s %v", r.kind, r.obj["metadata"]), "internal", "unknown object")
		}
		name, _ := r.obj["metadata"].(map[string]interface{})["name"].(string)
		cur := existing[r.kind+"/"+name]
		kind := r.kind
		read := func() (objectMeta, bool, error) { return readMinIOObject(c, kind, name) }
		action, err := ensureToolObject(c, fmt.Sprintf("%s %s/%s", r.kind, minioNamespace, name), path, createPath, r.obj, cur.meta, cur.found, read, minioObjectOwned)
		if err != nil {
			done := "nothing"
			if len(applied) > 0 {
				done = strings.Join(applied, ", ")
			}
			var foreignErr *foreignObjectError
			if errors.As(err, &foreignErr) {
				return fail(fmt.Sprintf("%s was created by someone else while setup was running, so it was left unchanged. Already applied: %s. %s", foreignErr.Desc, done, minioForeignHint), "not_managed", r.name+" became foreign")
			}
			code := errorCodeFromK8sErr(err)
			if errors.Is(err, errObjectChanged) || IsK8sError(err, 409) {
				code = "conflict"
			}
			return fail(fmt.Sprintf("Failed to apply %s: %v. Already applied: %s. Re-run setup to retry, or tear down to remove it.", r.name, err, done), code, "apply "+r.name+" failed")
		}
		applied = append(applied, r.name)
		logs = append(logs, fmt.Sprintf("OK: %s %s", r.name, action))
	}

	// Step 4: Wait for the pod. Stop early when it cannot start.
	logs = append(logs, fmt.Sprintf("Waiting up to %s for MinIO to become ready...", MinIOReadyTimeout))
	issue, err := waitMinIOReady(c, minioImage())
	if err != nil {
		if errors.Is(err, errMinIONotReady) {
			reason := "the pod is still starting"
			if issue.Reason != "" {
				reason = issue.Reason
				if issue.Message != "" {
					reason += ": " + truncateMessage(issue.Message)
				}
			}
			logs = append(logs, "MinIO is not ready: "+reason)
			return fail(fmt.Sprintf("MinIO was deployed but is not ready after %s (%s). The status shows the current state.", MinIOReadyTimeout, reason), "timeout", "not ready")
		}
		if errors.Is(err, errMinIOCannotStart) {
			msg := issue.Reason
			if issue.Message != "" {
				msg += ": " + truncateMessage(issue.Message)
			}
			logs = append(logs, "MinIO cannot start: "+msg)
			return fail(fmt.Sprintf("MinIO was deployed but its pod cannot start (%s). Fix the cause (for example set MINIO_IMAGE to a pullable image) and re-run setup, or tear it down.", msg), "not_ready", issue.Reason)
		}
		return fail(fmt.Sprintf("Stopped waiting for MinIO: %v", err), errorCodeFromK8sErr(err), "wait failed")
	}
	logs = append(logs, "OK: MinIO is running")

	// Step 5: Create bucket via S3 API (try once, retry once if it fails)
	logs = append(logs, fmt.Sprintf("Creating bucket '%s'...", minioBucket))
	bucketErr := minioBucketCreator(c, minioBucket)
	if bucketErr != nil {
		select {
		case <-c.ctx.Done():
		case <-time.After(minioBucketRetryDelay):
			bucketErr = minioBucketCreator(c, minioBucket)
		}
	}
	if bucketErr != nil {
		logs = append(logs, fmt.Sprintf("Bucket creation failed: %v — you can create it from the MinIO console.", bucketErr))
		return fail(fmt.Sprintf("MinIO deployed but bucket creation failed: %v. You can create the bucket manually from the MinIO console.", bucketErr), "", "bucket failed")
	}
	logs = append(logs, fmt.Sprintf("OK: Bucket '%s' created", minioBucket))

	// Step 6: Remove the minio-api Route earlier versions created: it
	// exposed the S3 API outside the cluster. A pipeline server whose
	// object storage points at the Route's host would lose its artifacts
	// store, so the Route is kept while any DSPA uses it, and also when
	// that cannot be checked (fail closed).
	keptRouteNote := ""
	if cur := existing["Route/minio-api"]; cur.found {
		users, err := minioAPIRouteUsers(c)
		if err != nil {
			return fail(fmt.Sprintf("MinIO is running with bucket '%s', but Route minio-api, which exposes its S3 API outside the cluster, was kept: cannot check whether a pipeline server uses it: %v. Re-run setup to retry.", minioBucket, err), "partial_failure", "route consumer check failed")
		}
		if len(users) > 0 {
			keptRouteNote = fmt.Sprintf(" Route minio-api, which exposes MinIO's S3 API outside the cluster, was kept because pipeline server(s) %s use its host. Point their object storage at host %s with scheme http (the in-cluster service), then re-run setup to remove the Route.", strings.Join(users, ", "), minioS3Host())
			logs = append(logs, "Kept Route minio-api: used by "+strings.Join(users, ", "))
		}
	}
	if cur := existing["Route/minio-api"]; cur.found && keptRouteNote == "" {
		path, _ := minioDeletePath("Route", "minio-api")
		if _, err := deleteWithUID(c, path, cur.meta.UID); err != nil && !IsK8sError(err, 404) {
			return fail(fmt.Sprintf("MinIO is running with bucket '%s', but Route minio-api, which exposes its S3 API outside the cluster, could not be removed: %v. Delete it with `oc delete route -n minio minio-api`, or re-run setup.", minioBucket, err), "partial_failure", "route cleanup failed")
		}
		logs = append(logs, "OK: Route minio-api removed (it exposed the S3 API outside the cluster)")
	}

	slog.Info("minio setup complete", "user", getUser(c))
	recordMinIOActivity(c, "setup-minio", fmt.Sprintf("namespace=%s bucket=%s", minioNamespace, minioBucket), true)

	return &types.OperationResponse{
		Success: true, Message: fmt.Sprintf("MinIO deployed with bucket '%s'. Console credentials are stored in secret 'minio-secret' in namespace '%s'.%s", minioBucket, minioNamespace, keptRouteNote),
		Logs: logs,
	}, nil
}

// minioAPIRouteUsers returns the pipeline servers (namespace/name, sorted)
// whose object-storage host is the host of Route minio-api. A missing DSPA
// CRD means there are none; any other read error is returned.
func minioAPIRouteUsers(c *Client) ([]string, error) {
	host, err := readRouteHost(c, minioNamespace, "minio-api")
	if err != nil {
		return nil, err
	}
	if host == "" {
		return nil, nil
	}
	dspas, err := listDSPAs(c, "")
	if errors.Is(err, errDSPACRDMissing) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	routeHostname := strings.TrimSuffix(strings.ToLower(host), ".")
	var users []string
	for _, d := range dspas {
		// Only the Route host counts here: a DSPA on the service DNS name
		// or ClusterIP does not need the Route.
		if endpointHostname(d.Host) == routeHostname {
			users = append(users, d.Meta.Namespace+"/"+d.Meta.Name)
		}
	}
	sort.Strings(users)
	return users, nil
}

var (
	errMinIONotReady    = errors.New("minio not ready before timeout")
	errMinIOCannotStart = errors.New("minio pod cannot start")
)

// waitMinIOReady polls the MinIO Deployment until its current template has a
// ready replica. It returns errMinIOCannotStart as soon as a pod of image
// reports a terminal waiting reason (ImagePullBackOff, CrashLoopBackOff, ...),
// and errMinIONotReady on timeout, with the last observed pod issue. Every
// request runs under one context bounded by MinIOReadyTimeout, so a request
// that starts just before the deadline cannot overrun it.
func waitMinIOReady(c *Client, image string) (workloadPodIssue, error) {
	ctx, cancel := context.WithTimeout(c.ctx, MinIOReadyTimeout)
	defer cancel()
	cc := c.WithContext(ctx)
	var last workloadPodIssue
	expired := func() (workloadPodIssue, error) {
		if c.ctx.Err() != nil {
			return last, c.ctx.Err()
		}
		return last, errMinIONotReady
	}
	for {
		body, _, err := cc.get(namespacedPath("apps/v1", "deployments", minioNamespace, "minio"))
		if ctx.Err() != nil {
			return expired()
		}
		if err == nil {
			var deploy minioDeployment
			if json.Unmarshal(body, &deploy) == nil {
				if deploy.rolledOut() {
					return workloadPodIssue{}, nil
				}
				// Only judge pods once the controller has seen the new
				// template, and only pods running the image just applied, so
				// an old pod from an earlier setup is not mistaken for this one.
				if deploy.Status.ObservedGeneration >= deploy.Metadata.Generation {
					if issue, perr := inspectPods(cc, minioNamespace, minioAppSelector, image); perr == nil && issue.Reason != "" {
						last = issue
						if issue.Terminal {
							return issue, errMinIOCannotStart
						}
					}
				}
			}
		} else if !IsK8sError(err, 404) && !IsNetworkError(err) {
			return last, err
		}
		select {
		case <-ctx.Done():
			return expired()
		case <-time.After(MinIOReadyPoll):
		}
	}
}

func createMinioBucket(c *Client, bucket string) error {
	secretPath := namespacedPath("v1", "secrets", minioNamespace, "minio-secret")
	body, _, err := c.get(secretPath)
	if err != nil {
		return fmt.Errorf("read minio secret: %w", err)
	}

	var secret struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(body, &secret); err != nil {
		return fmt.Errorf("parse minio secret: %w", err)
	}

	accessKey := decodeBase64Field(secret.Data["minio_root_user"])
	secretKey := decodeBase64Field(secret.Data["minio_root_password"])
	if accessKey == "" || secretKey == "" {
		return fmt.Errorf("empty MinIO credentials in secret")
	}

	endpoint := fmt.Sprintf("http://%s.%s.svc:9000", minioServiceName, minioNamespace)
	return s3PutBucket(c.ctx, endpoint, accessKey, secretKey, bucket)
}

// minioKeptNamespaceHint ends every teardown message: the namespace is never
// deleted, because the tool cannot see everything in it without reading
// every Secret in the cluster (a cluster-wide list grant).
const minioKeptNamespaceHint = " Namespace 'minio' was kept: it may hold objects this tool did not create. Delete it with `oc delete project minio` once you've checked it's empty."

// minioTeardownObjects are the objects teardown removes, in order. The
// Deployment goes first, so its pod releases the PVC (whose
// kubernetes.io/pvc-protection finalizer waits for that).
var minioTeardownObjects = []struct{ kind, name string }{
	{"Deployment", "minio"},
	{"Route", "minio-ui"},
	{"Route", "minio-api"},
	{"Service", minioServiceName},
	{"Secret", "minio-secret"},
	{"PersistentVolumeClaim", "minio-pvc"},
}

// TeardownMinIO removes the MinIO objects this tool created, and with them
// the data PVC, when no pipeline server uses MinIO. Each object is read by
// name, deleted only when it carries the tool's label (or an earlier
// version's server-side-apply marker), and deleted with a UID precondition,
// so an object someone else created or recreated is never removed. The minio
// namespace itself is never deleted (see minioKeptNamespaceHint); it stays
// labelled, so setup can reuse it. Re-running it is safe.
func TeardownMinIO(c *Client) (*types.OperationResponse, error) {
	logs := []string{}
	fail := func(msg, code, activity string) (*types.OperationResponse, error) {
		recordMinIOActivity(c, "teardown-minio", fmt.Sprintf("%s (%s)", minioNamespace, activity), false)
		return &types.OperationResponse{Success: false, Message: msg, Logs: logs, ErrorCode: code}, nil
	}

	ns, err := lookupMinIONamespace(c)
	if err != nil {
		return fail(fmt.Sprintf("Cannot check namespace '%s'; nothing was deleted: %v", minioNamespace, err), errorCodeFromK8sErr(err), "namespace check failed")
	}
	if !ns.Exists {
		logs = append(logs, "Namespace already absent")
		return &types.OperationResponse{Success: true, Message: "MinIO is not deployed; nothing to remove.", Logs: logs}, nil
	}
	if !ns.Managed {
		return fail("Namespace 'minio' was not created by this tool, so nothing in it was deleted.", "not_managed", "not managed")
	}
	if ns.terminating() {
		logs = append(logs, "Namespace is being deleted")
		return &types.OperationResponse{
			Success: false, Message: "Namespace 'minio' is being deleted." + namespaceDeletionDetail(c), Logs: logs, ErrorCode: "in_progress",
		}, nil
	}

	// Teardown order is MLflow, pipeline servers, then MinIO. Pipeline
	// servers that store artifacts here would break, and a DSPA that is still
	// terminating must finish first. A missing DSPA CRD means there are none.
	dspas, err := listDSPAs(c, "")
	if err != nil && !errors.Is(err, errDSPACRDMissing) {
		return fail(fmt.Sprintf("Cannot verify pipeline dependencies; MinIO was not deleted: %v", err), errorCodeFromK8sErr(err), "dependency check failed")
	}
	// Pipeline servers may reach MinIO through a Route host or the
	// ClusterIP. When those cannot be read and any pipeline server exists, a
	// dependency cannot be ruled out: refuse rather than delete the PVC a
	// pipeline server may still store its artifacts in.
	endpoints, err := readMinIOEndpoints(c)
	if err != nil && len(dspas) > 0 {
		return fail(fmt.Sprintf("Cannot verify which pipeline servers use MinIO (its Route hosts or service address could not be read); nothing was deleted: %v", err), errorCodeFromK8sErr(err), "dependency check failed")
	}
	if reason := minioTeardownBlocker(dspas, endpoints); reason != "" {
		return fail("Cannot tear down MinIO yet. "+reason, "prerequisites", "pipeline servers depend on it")
	}

	// A namespace from an earlier version is recognised by its Deployment;
	// label it before that Deployment goes, so setup can still reuse it.
	if ns.Legacy {
		patch, _ := json.Marshal(map[string]interface{}{"metadata": map[string]interface{}{"labels": toolLabels()}})
		if _, _, err := c.patch("/api/v1/namespaces/"+minioNamespace, patch); err != nil {
			return fail(fmt.Sprintf("Failed to label namespace 'minio', created by an earlier version, before removing MinIO; nothing was deleted: %v", err), errorCodeFromK8sErr(err), "namespace label failed")
		}
		logs = append(logs, "OK: Namespace from an earlier version labelled as managed by this tool")
	}

	var deleted, kept, failed []string
	pvcUID := ""
	for _, o := range minioTeardownObjects {
		desc := o.kind + " " + o.name
		meta, found, err := readMinIOObject(c, o.kind, o.name)
		switch {
		case err != nil:
			failed = append(failed, fmt.Sprintf("%s: cannot read it: %v", desc, err))
			continue
		case !found:
			continue
		case !minioObjectOwned(meta):
			kept = append(kept, desc)
			logs = append(logs, fmt.Sprintf("Kept %s: it was not created by this tool", desc))
			continue
		case meta.terminating():
			logs = append(logs, desc+" is already being deleted")
		default:
			path, ok := minioDeletePath(o.kind, o.name)
			if !ok {
				continue
			}
			logs = append(logs, fmt.Sprintf("Deleting %s...", desc))
			if _, err := deleteWithUID(c, path, meta.UID); err != nil && !IsK8sError(err, 404) {
				failed = append(failed, fmt.Sprintf("%s: %v", desc, err))
				logs = append(logs, fmt.Sprintf("Failed to delete %s: %v", desc, err))
				continue
			}
			deleted = append(deleted, desc)
			logs = append(logs, fmt.Sprintf("OK: %s deleted", desc))
		}
		if o.kind == "PersistentVolumeClaim" {
			pvcUID = meta.UID
		}
	}

	keptNote := ""
	if len(kept) > 0 {
		keptNote = fmt.Sprintf(" Left in place because this tool did not create them: %s.", strings.Join(kept, ", "))
	}
	dataNote := ""
	if pvcUID != "" {
		dataNote = " PVC minio-pvc and all stored objects (pipeline artifacts, models, test files) are deleted with it."
	}
	if len(failed) > 0 {
		recordMinIOActivity(c, "teardown-minio", fmt.Sprintf("%s (failed: %s)", minioNamespace, strings.Join(failed, "; ")), false)
		code := "partial_failure"
		if len(deleted) == 0 {
			code = "delete_failed"
		}
		return &types.OperationResponse{
			Success: false, Message: fmt.Sprintf("Some MinIO objects could not be removed: %s. Re-run teardown to retry.%s%s", strings.Join(failed, "; "), keptNote, minioKeptNamespaceHint),
			Logs: logs, ErrorCode: code,
		}, nil
	}
	if len(deleted) == 0 && pvcUID == "" {
		recordMinIOActivity(c, "teardown-minio", minioNamespace+" (nothing to remove)", true)
		return &types.OperationResponse{Success: true, Message: "No MinIO objects created by this tool remain." + keptNote + minioKeptNamespaceHint, Logs: logs}, nil
	}
	slog.Info("minio teardown", "user", getUser(c), "deleted", strings.Join(deleted, ", "))
	recordMinIOActivity(c, "teardown-minio", minioNamespace+" (objects deleted, namespace kept)", true)

	if pvcUID != "" {
		gone, err := waitForGone(c, MinIODeleteTimeout, MinIODeletePoll, func(cc *Client) (bool, error) {
			pvcs, err := listMinIOPVCs(cc)
			if err != nil {
				return false, err
			}
			for _, m := range pvcs {
				if m.UID == pvcUID {
					return false, nil
				}
			}
			return true, nil
		})
		if err != nil {
			return &types.OperationResponse{
				Success: false, Message: fmt.Sprintf("MinIO's objects were deleted, but whether its PVC is gone cannot be checked: %v.%s%s%s", err, dataNote, keptNote, minioKeptNamespaceHint),
				Logs: logs, ErrorCode: firstNonEmpty(errorCodeFromK8sErr(err), "in_progress"),
			}, nil
		}
		if !gone {
			logs = append(logs, "PVC minio-pvc is still terminating")
			return &types.OperationResponse{
				Success: false, Message: fmt.Sprintf("MinIO's objects were deleted, but PVC minio-pvc is still terminating after %s; re-run teardown to check again.%s%s%s", MinIODeleteTimeout, dataNote, keptNote, minioKeptNamespaceHint),
				Logs: logs, ErrorCode: "in_progress",
			}, nil
		}
	}
	logs = append(logs, "OK: MinIO removed")
	return &types.OperationResponse{Success: true, Message: "MinIO removed." + dataNote + keptNote + minioKeptNamespaceHint, Logs: logs}, nil
}

// namespaceDeletionDetail returns the namespace controller's reported
// reasons (the NamespaceDeletion*Failure, NamespaceContentRemaining and
// NamespaceFinalizersRemaining conditions) for a minio namespace that is
// terminating (deleted by someone else), or "".
func namespaceDeletionDetail(c *Client) string {
	body, _, err := c.get("/api/v1/namespaces/" + minioNamespace)
	if err != nil {
		return ""
	}
	var ns struct {
		Status struct {
			Conditions []struct {
				Type    string `json:"type"`
				Status  string `json:"status"`
				Message string `json:"message"`
			} `json:"conditions"`
		} `json:"status"`
	}
	if json.Unmarshal(body, &ns) != nil {
		return ""
	}
	var details []string
	for _, cond := range ns.Status.Conditions {
		if cond.Status == "True" && cond.Message != "" {
			details = append(details, cond.Type+": "+truncateMessage(cond.Message))
		}
	}
	if len(details) == 0 {
		return ""
	}
	return " Namespace status: " + strings.Join(details, "; ") + "."
}
