package cluster

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const (
	minioNamespace   = "minio"
	minioBucket      = "pipelines"
	minioServiceName = "minio-service"
	// minioServiceAPIPort and minioServiceUIPort are the ports of
	// minio-service. They are MinIO's ports, kept so that pipeline servers
	// configured with minio-service.minio.svc:9000 and the minio-ui Route
	// (targetPort "ui") keep working with no edits; the Service maps them
	// to SeaweedFS's ports, as Kubeflow Pipelines' own SeaweedFS Service
	// does "for backward compatibility"
	// (manifests/kustomize/third-party/seaweedfs/base/seaweedfs/seaweedfs-service.yaml).
	minioServiceAPIPort = 9000
	minioServiceUIPort  = 9090

	// Objects of the MinIO server earlier versions deployed. Setup replaces
	// the Deployment and its NetworkPolicy with SeaweedFS, and keeps the
	// data PVC (SeaweedFS cannot read MinIO's on-disk format) until
	// teardown, for a rollback or a manual copy.
	legacyMinIODeployment    = "minio"
	legacyMinIOPVC           = "minio-pvc"
	legacyMinIONetworkPolicy = "minio-ingress"
	legacyMinIOContainer     = "minio"
	minioAppSelector         = "app=minio"

	// The S3 storage: SeaweedFS ("weed mini", a single-process master,
	// volume, filer, S3 gateway and admin UI).
	s3Deployment    = "seaweedfs"
	s3PVC           = "seaweedfs-pvc"
	s3NetworkPolicy = "seaweedfs-ingress"
	s3Container     = "seaweedfs"
	s3AppLabel      = "seaweedfs"
	s3AppSelector   = "app=seaweedfs"
	s3Storage       = "20Gi"
	// s3Port is SeaweedFS's S3 port (weed/command/mini.go "s3.port") and
	// s3AdminPort its admin UI ("admin.port", 23646).
	s3Port      = 8333
	s3AdminPort = 23646
	// s3AdminUser is the admin UI login: the default of -admin.user
	// (weed/command/mini.go:565 at 4.48), set explicitly below.
	s3AdminUser = "admin"
	// seaweedfsDefaultImage is SeaweedFS 4.48
	// (ghcr.io/chrislusf/seaweedfs:4.48, Apache-2.0), pinned by its
	// multi-arch index digest (amd64, arm64, arm, 386). It is anonymously
	// pullable. Upstream Kubeflow Pipelines made SeaweedFS its default
	// object store in 2.15.0 (kubeflow/pipelines#11965) and runs this
	// version (#14635); its OpenShift overlay lets restricted-v2 assign the
	// UID (#12314), so no runAsUser is set here either. The image's
	// entrypoint.sh drops to user "seaweed" only when started as root; under
	// an assigned UID it runs weed directly, and /data must be writable,
	// which the fsGroup restricted-v2 sets on the PVC provides.
	seaweedfsDefaultImage = "ghcr.io/chrislusf/seaweedfs@sha256:4e61d15fd35994cb1e43e1e553dff106794841fd9a99ade2fc8c8bfce4d7872d"
)

var (
	// MinIOReadyTimeout bounds how long setup waits for the SeaweedFS pod. It
	// stays below the frontend's 120s request timeout. Tests shorten it.
	MinIOReadyTimeout = 90 * time.Second
	MinIOReadyPoll    = 3 * time.Second
	// MinIODeleteTimeout bounds how long teardown waits for the namespace
	// (or, when it is kept, the data PVC) to disappear before reporting that
	// deletion is still in progress.
	MinIODeleteTimeout = 60 * time.Second
	MinIODeletePoll    = 2 * time.Second
	// MinIOPodsGoneTimeout bounds how long a migration waits for the pods of
	// the replaced MinIO to exit before it removes their NetworkPolicy. It
	// fits in the frontend's 120s request timeout next to MinIOReadyTimeout.
	MinIOPodsGoneTimeout = 20 * time.Second

	// minioBucketCreator creates the pipelines bucket; tests replace it
	// because the in-cluster S3 endpoint is unreachable from unit tests.
	minioBucketCreator    = createMinioBucket
	minioBucketRetryDelay = 3 * time.Second
)

// s3Image returns the SeaweedFS image, overridable with SEAWEEDFS_IMAGE
// (for example a mirror on a disconnected cluster). An override must be a
// SeaweedFS 4.x release whose "weed mini" reads the admin credentials from
// WEED_ADMIN_* (see s3Resources). MINIO_IMAGE, which earlier versions read,
// is ignored: a MinIO image cannot serve this container contract.
func s3Image() string {
	if img := strings.TrimSpace(os.Getenv("SEAWEEDFS_IMAGE")); img != "" {
		return img
	}
	return seaweedfsDefaultImage
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// minioCredentials returns the S3 access key and secret key (also the admin
// UI password), stored under MinIO's key names in minio-secret. Explicit
// MINIO_ROOT_USER / MINIO_ROOT_PASSWORD env vars win. Otherwise credentials
// already stored in minio-secret are reused, because pipeline servers copy
// them into their own secrets; reusing them is what lets a migration from
// MinIO keep those pipeline servers working, and SeaweedFS picks up a
// changed secret on restart.
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
			return "", "", fmt.Errorf("generate S3 access key: %w", randErr)
		}
		user = "minio-" + suffix
	}
	if password == "" {
		if password, err = randomHex(16); err != nil {
			return "", "", fmt.Errorf("generate S3 secret key: %w", err)
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

// minioPVC is a PVC in the minio namespace and the size of its volume.
type minioPVC struct {
	Meta objectMeta
	// Size is the bound capacity, or the requested size while unbound.
	Size string
}

// listMinIOPVCs lists the PVCs in the minio namespace. PVCs are listed rather
// than read by name, so the RBAC rules need no "get" on them.
func listMinIOPVCs(c *Client) ([]minioPVC, error) {
	body, _, err := c.get(namespacedPath("v1", "persistentvolumeclaims", minioNamespace, ""))
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []struct {
			Metadata objectMeta `json:"metadata"`
			Spec     struct {
				Resources struct {
					Requests map[string]string `json:"requests"`
				} `json:"resources"`
			} `json:"spec"`
			Status struct {
				Capacity map[string]string `json:"capacity"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("parse PVC list: %w", err)
	}
	out := make([]minioPVC, 0, len(list.Items))
	for _, item := range list.Items {
		out = append(out, minioPVC{Meta: item.Metadata, Size: firstNonEmpty(item.Status.Capacity["storage"], item.Spec.Resources.Requests["storage"])})
	}
	return out, nil
}

// readMinIOPVC returns the named PVC in the minio namespace, and whether it
// exists.
func readMinIOPVC(c *Client, name string) (minioPVC, bool, error) {
	pvcs, err := listMinIOPVCs(c)
	if err != nil {
		return minioPVC{}, false, err
	}
	for _, p := range pvcs {
		if p.Meta.Name == name {
			return p, true, nil
		}
	}
	return minioPVC{}, false, nil
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

// image returns the image of the named container.
func (d minioDeployment) image(container string) string {
	for _, ctr := range d.Spec.Template.Spec.Containers {
		if ctr.Name == container {
			return ctr.Image
		}
	}
	return ""
}

func getMinIOStatus(c *Client) types.ResourceState {
	state, _, _ := minioStatusAndEndpoints(c)
	return state
}

// minioMigrationNote explains what re-running setup does to a MinIO an
// earlier version deployed.
const minioMigrationNote = "MinIO from an earlier version of this tool still runs here and serves the pipeline servers. Re-run setup to replace it with SeaweedFS. Setup starts fresh: the objects stored in MinIO are not copied, so artifacts, logs and cached outputs of earlier pipeline runs return 404 (new runs work). Pipeline servers keep working with no edits (same Service, credentials and bucket), and the MinIO data volume, PVC minio-pvc, is kept for a rollback or a manual copy until teardown."

// minioStatusAndEndpoints returns the status of the S3 storage and the
// addresses a pipeline server may use to reach it (for the teardown
// dependency check). The error is non-nil when those addresses could not be
// read. The status describes the SeaweedFS Deployment, or the MinIO
// Deployment of an earlier version while it has not been replaced
// (MigrationPending).
func minioStatusAndEndpoints(c *Client) (types.ResourceState, minioEndpoints, error) {
	state := types.ResourceState{Namespace: minioNamespace}

	ns, err := lookupMinIONamespace(c)
	if err != nil {
		state.Message = "Cannot read the S3 storage status: " + err.Error()
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
		s3Body       []byte
		s3Err        error
		legacyBody   []byte
		legacyErr    error
		legacyNP     objectMeta
		legacyNPOK   bool
		legacyNPErr  error
		apiHost      string
		uiHost       string
		endpoints    minioEndpoints
		endpointsErr error
		pvcs         []minioPVC
		pvcErr       error
	)
	wg.Add(5)
	go func() {
		defer wg.Done()
		s3Body, _, s3Err = c.get(namespacedPath("apps/v1", "deployments", minioNamespace, s3Deployment))
	}()
	go func() {
		defer wg.Done()
		legacyNP, legacyNPOK, legacyNPErr = readMinIOObject(c, "NetworkPolicy", legacyMinIONetworkPolicy)
	}()
	go func() {
		defer wg.Done()
		legacyBody, _, legacyErr = c.get(namespacedPath("apps/v1", "deployments", minioNamespace, legacyMinIODeployment))
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
		state.Warning = "Route 'minio-api' exposes the S3 API outside the cluster. Re-run setup to remove it; setup keeps it while a pipeline server uses its host."
	}
	if uiHost != "" {
		state.UIRoute = "https://" + uiHost
	}

	// An earlier version's MinIO, created by this tool, that setup has not
	// replaced yet.
	legacyOwned := false
	if legacyErr == nil && ns.Managed {
		if meta, perr := parseObjectMeta(legacyBody); perr == nil && minioObjectOwned(meta) && !meta.terminating() {
			legacyOwned = true
		}
	}
	if legacyOwned {
		state.MigrationPending = true
		state.Warning = strings.TrimSpace(minioMigrationNote + " " + state.Warning)
	}
	if ns.Managed && pvcErr == nil {
		for _, p := range pvcs {
			if !minioObjectOwned(p.Meta) || (p.Meta.Name != s3PVC && p.Meta.Name != legacyMinIOPVC) {
				continue
			}
			state.DataPVCs = append(state.DataPVCs, p.Meta.Name)
			if p.Meta.Name == legacyMinIOPVC && IsK8sError(legacyErr, 404) {
				state.KeptPVCs = append(state.KeptPVCs, types.KeptPVC{Name: p.Meta.Name, Size: p.Size})
			}
		}
	}

	// The Deployment the status describes: SeaweedFS, or else the MinIO of
	// an earlier version.
	deployBody, deployErr, container, selector, what := s3Body, s3Err, s3Container, s3AppSelector, "SeaweedFS"
	if IsK8sError(s3Err, 404) && !IsK8sError(legacyErr, 404) {
		deployBody, deployErr, container, selector, what = legacyBody, legacyErr, legacyMinIOContainer, minioAppSelector, "MinIO"
	}
	if deployErr != nil {
		if IsK8sError(deployErr, 404) {
			state.Message = "Namespace exists but the S3 storage is not deployed"
		} else {
			state.Message = fmt.Sprintf("Cannot read the %s deployment: %v", what, deployErr)
		}
		if !ns.Managed {
			state.Message = state.SetupBlockedReason
		}
		return state, endpoints, endpointsErr
	}
	state.Deployed = true

	var deploy minioDeployment
	if err := json.Unmarshal(deployBody, &deploy); err != nil {
		state.Message = "Cannot parse the " + what + " deployment"
		return state, endpoints, endpointsErr
	}
	state.CurrentImage = deploy.image(container)
	serving := ""
	if container == s3Container {
		state.UIUser = s3AdminUser
		if ns.Managed && state.CurrentImage != "" && state.CurrentImage != s3Image() {
			state.Warning = strings.TrimSpace(fmt.Sprintf("SeaweedFS runs %s, not the image this version deploys (%s). Re-run setup to update it; the data PVC is kept. %s", state.CurrentImage, s3Image(), state.Warning))
		}
		// SeaweedFS serves only through minio-service and Route minio-ui,
		// which setup switches once it is ready: a setup that stopped
		// before that, a failed migration (the Service still on MinIO) or a
		// manual rollback leaves it running but unused.
		if ns.Managed {
			switch {
			case deploy.Spec.Replicas != nil && *deploy.Spec.Replicas == 0:
				state.RepairNeeded = "Deployment seaweedfs is scaled to 0 replicas (for example by a manual rollback)."
			case endpointsErr == nil:
				serving = endpoints.s3ServingProblem()
				state.RepairNeeded = serving
			}
			if state.RepairNeeded == "" && legacyNPErr == nil && legacyNPOK && minioObjectOwned(legacyNP) && IsK8sError(legacyErr, 404) {
				state.RepairNeeded = "NetworkPolicy minio-ingress of the replaced MinIO was not removed yet (its pods were still shutting down)."
			}
		}
	}
	switch {
	case container == s3Container && ns.Managed && deploy.Spec.Replicas != nil && *deploy.Spec.Replicas == 0:
		// Nothing will start on its own: say so instead of "starting".
		state.Message = state.RepairNeeded
		state.TerminalError = true
		return state, endpoints, endpointsErr
	case deploy.Status.ReadyReplicas < 1:
	case serving != "":
		state.Message = serving
		state.TerminalError = true
		return state, endpoints, endpointsErr
	case container == s3Container && ns.Managed && endpointsErr != nil:
		state.Message = "SeaweedFS is running, but whether minio-service and Route minio-ui point at it cannot be checked: " + endpointsErr.Error()
		return state, endpoints, endpointsErr
	default:
		state.Ready = true
		state.Message = "Running"
		return state, endpoints, endpointsErr
	}
	state.Message = fmt.Sprintf("%d/%d ready", deploy.Status.ReadyReplicas, deploy.Status.Replicas)
	if issue, err := inspectPods(c, minioNamespace, selector, ""); err == nil {
		applyPodIssue(&state, issue)
	}
	return state, endpoints, endpointsErr
}

// minioApplyPath returns the API path of one object from s3Resources.
// Only the fixed objects the tool creates are accepted; the literal names
// keep the RBAC rules limited to them (pkg/cluster/rbac_coverage_test.go).
func minioApplyPath(obj map[string]interface{}) (string, bool) {
	meta, _ := obj["metadata"].(map[string]interface{})
	name, _ := meta["name"].(string)
	switch kind, _ := obj["kind"].(string); {
	case kind == "PersistentVolumeClaim" && name == s3PVC:
		return namespacedPath("v1", "persistentvolumeclaims", minioNamespace, s3PVC), true
	case kind == "Secret" && name == "minio-secret":
		return namespacedPath("v1", "secrets", minioNamespace, "minio-secret"), true
	case kind == "Deployment" && name == s3Deployment:
		return namespacedPath("apps/v1", "deployments", minioNamespace, s3Deployment), true
	case kind == "Service" && name == minioServiceName:
		return namespacedPath("v1", "services", minioNamespace, minioServiceName), true
	case kind == "NetworkPolicy" && name == s3NetworkPolicy:
		return namespacedPath("networking.k8s.io/v1", "networkpolicies", minioNamespace, s3NetworkPolicy), true
	case kind == "Route" && name == "minio-ui":
		return namespacedPath("route.openshift.io/v1", "routes", minioNamespace, "minio-ui"), true
	}
	return "", false
}

// minioCreatePath returns the collection path an s3Resources object is
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
	case "NetworkPolicy":
		return withToolFieldManager(namespacedPath("networking.k8s.io/v1", "networkpolicies", minioNamespace, "")), true
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
	case kind == "Deployment" && name == s3Deployment:
		return namespacedPath("apps/v1", "deployments", minioNamespace, s3Deployment), true
	case kind == "Deployment" && name == legacyMinIODeployment:
		return namespacedPath("apps/v1", "deployments", minioNamespace, legacyMinIODeployment), true
	case kind == "Service" && name == minioServiceName:
		return namespacedPath("v1", "services", minioNamespace, minioServiceName), true
	case kind == "Route" && name == "minio-api":
		return namespacedPath("route.openshift.io/v1", "routes", minioNamespace, "minio-api"), true
	case kind == "NetworkPolicy" && name == s3NetworkPolicy:
		return namespacedPath("networking.k8s.io/v1", "networkpolicies", minioNamespace, s3NetworkPolicy), true
	case kind == "NetworkPolicy" && name == legacyMinIONetworkPolicy:
		return namespacedPath("networking.k8s.io/v1", "networkpolicies", minioNamespace, legacyMinIONetworkPolicy), true
	case kind == "Route" && name == "minio-ui":
		return namespacedPath("route.openshift.io/v1", "routes", minioNamespace, "minio-ui"), true
	}
	return "", false
}

// minioDeletePath returns the path of a tool object teardown (or setup, for
// the MinIO it replaces and a Route that is no longer wanted) may delete.
func minioDeletePath(kind, name string) (string, bool) {
	switch {
	case kind == "PersistentVolumeClaim" && name == s3PVC:
		return namespacedPath("v1", "persistentvolumeclaims", minioNamespace, s3PVC), true
	case kind == "PersistentVolumeClaim" && name == legacyMinIOPVC:
		return namespacedPath("v1", "persistentvolumeclaims", minioNamespace, legacyMinIOPVC), true
	case kind == "Secret" && name == "minio-secret":
		return namespacedPath("v1", "secrets", minioNamespace, "minio-secret"), true
	case kind == "Deployment" && name == s3Deployment:
		return namespacedPath("apps/v1", "deployments", minioNamespace, s3Deployment), true
	case kind == "Deployment" && name == legacyMinIODeployment:
		return namespacedPath("apps/v1", "deployments", minioNamespace, legacyMinIODeployment), true
	case kind == "Service" && name == minioServiceName:
		return namespacedPath("v1", "services", minioNamespace, minioServiceName), true
	case kind == "Route" && name == "minio-api":
		return namespacedPath("route.openshift.io/v1", "routes", minioNamespace, "minio-api"), true
	case kind == "NetworkPolicy" && name == s3NetworkPolicy:
		return namespacedPath("networking.k8s.io/v1", "networkpolicies", minioNamespace, s3NetworkPolicy), true
	case kind == "NetworkPolicy" && name == legacyMinIONetworkPolicy:
		return namespacedPath("networking.k8s.io/v1", "networkpolicies", minioNamespace, legacyMinIONetworkPolicy), true
	case kind == "Route" && name == "minio-ui":
		return namespacedPath("route.openshift.io/v1", "routes", minioNamespace, "minio-ui"), true
	}
	return "", false
}

// readMinIOObject reads one named object in the minio namespace.
func readMinIOObject(c *Client, kind, name string) (objectMeta, bool, error) {
	if kind == "PersistentVolumeClaim" {
		p, found, err := readMinIOPVC(c, name)
		return p.Meta, found, err
	}
	path, ok := minioGetPath(kind, name)
	if !ok {
		return objectMeta{}, false, fmt.Errorf("internal error: unexpected S3 storage object %s %s", kind, name)
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
	// frontDoor marks the objects that route traffic to the server: they are
	// applied only once the server is ready, so a MinIO being replaced keeps
	// serving until SeaweedFS can take over.
	frontDoor bool
}

// s3Resources returns the objects SetupMinIO creates or updates, in order.
// Only the admin UI gets a Route: the S3 API stays inside the cluster, where
// pipeline servers reach it through minio-service.
func s3Resources(user, password string) []minioResource {
	meta := func(name string) map[string]interface{} {
		return map[string]interface{}{"name": name, "namespace": minioNamespace, "labels": toolLabels()}
	}
	secretEnv := func(name, key string) map[string]interface{} {
		return map[string]interface{}{"name": name, "valueFrom": map[string]interface{}{"secretKeyRef": map[string]interface{}{"name": "minio-secret", "key": key}}}
	}
	podLabels := map[string]interface{}{"app": s3AppLabel}
	return []minioResource{
		// A new PVC: SeaweedFS cannot read MinIO's on-disk format, so the
		// MinIO volume (minio-pvc) is left alone.
		{"PVC", "PersistentVolumeClaim", map[string]interface{}{
			"apiVersion": "v1", "kind": "PersistentVolumeClaim",
			"metadata": meta(s3PVC),
			"spec": map[string]interface{}{
				"accessModes": []string{"ReadWriteOnce"},
				"resources":   map[string]interface{}{"requests": map[string]interface{}{"storage": s3Storage}},
				"volumeMode":  "Filesystem",
			},
		}, false},
		// The keys are MinIO's names, kept so that existing pipeline servers
		// (whose secrets hold copies of these values) keep working.
		{"Secret", "Secret", map[string]interface{}{
			"apiVersion": "v1", "kind": "Secret",
			"metadata": meta("minio-secret"),
			"stringData": map[string]interface{}{
				"minio_root_user":     user,
				"minio_root_password": password,
			},
		}, false},
		// "weed mini" listens on many ports besides S3 (master 9333/19333,
		// volume 9340/19340, filer 8888/18888, worker gRPC 33646, Iceberg
		// 8181, metrics 9101), and the master and filer HTTP APIs need no
		// authentication. The pod is therefore fenced in before it starts:
		//   - S3 (8333) only from pods: the pipeline servers in their
		//     projects, the data-science-pipelines operator's object-storage
		//     health check (redhat-ods-applications), the router for a kept
		//     legacy minio-api Route, and this tool's bucket creation. Any
		//     namespace is allowed, because a pipeline server may live in any
		//     project; there is no S3 Route.
		//   - The admin UI (23646) only from the OpenShift router, through the
		//     minio-ui Route: namespaces labelled
		//     policy-group.network.openshift.io/ingress (router pods) or
		//     .../host-network (a HostNetwork router), as in the OpenShift
		//     docs "Allowing ingress from the Ingress Controller"
		//     (checked live: openshift-ingress carries the ingress label).
		//   - Nothing else.
		// The kubelet's probes always pass (traffic from the pod's node is
		// allowed by NetworkPolicy semantics), and `oc port-forward` does
		// not go through the pod network.
		{"NetworkPolicy", "NetworkPolicy", map[string]interface{}{
			"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy",
			"metadata": meta(s3NetworkPolicy),
			"spec": map[string]interface{}{
				"podSelector": map[string]interface{}{"matchLabels": podLabels},
				"policyTypes": []string{"Ingress"},
				"ingress": []map[string]interface{}{
					{
						"from":  []map[string]interface{}{{"namespaceSelector": map[string]interface{}{}}},
						"ports": []map[string]interface{}{{"protocol": "TCP", "port": s3Port}},
					},
					{
						"from": []map[string]interface{}{
							{"namespaceSelector": map[string]interface{}{"matchLabels": map[string]interface{}{"policy-group.network.openshift.io/ingress": ""}}},
							{"namespaceSelector": map[string]interface{}{"matchLabels": map[string]interface{}{"policy-group.network.openshift.io/host-network": ""}}},
						},
						"ports": []map[string]interface{}{{"protocol": "TCP", "port": s3AdminPort}},
					},
				},
			},
		}, false},
		{"Deployment", "Deployment", map[string]interface{}{
			"apiVersion": "apps/v1", "kind": "Deployment",
			"metadata": meta(s3Deployment),
			"spec": map[string]interface{}{
				"replicas": 1,
				"selector": map[string]interface{}{"matchLabels": podLabels},
				"strategy": map[string]interface{}{"type": "Recreate"},
				"template": map[string]interface{}{
					"metadata": map[string]interface{}{"labels": podLabels},
					"spec": map[string]interface{}{
						"volumes": []map[string]interface{}{
							{"name": "data", "persistentVolumeClaim": map[string]interface{}{"claimName": s3PVC}},
						},
						"containers": []map[string]interface{}{{
							"name":  s3Container,
							"image": s3Image(),
							// -bucket creates the bucket before the server
							// reports ready (weed/command/mini.go:1385-1393 at
							// 4.48); setup also creates it through the Service.
							"args": []string{"mini", "-dir=/data", "-webdav=false", "-bucket=" + minioBucket},
							"env": []map[string]interface{}{
								// The S3 server adds a static admin identity
								// from these (weed/s3api/auth_credentials.go:527-582
								// at 4.48), so the S3 keys stay the ones in
								// minio-secret.
								secretEnv("AWS_ACCESS_KEY_ID", "minio_root_user"),
								secretEnv("AWS_SECRET_ACCESS_KEY", "minio_root_password"),
								// The admin UI login, from the environment rather
								// than -admin.password, so the password is not on
								// the process command line: "weed mini" fills the
								// unset admin flags from viper key admin.user /
								// admin.password (weed/command/mini.go:1609-1611
								// and 1578-1580, weed/command/admin.go:783-790 at
								// 4.48), and viper reads WEED_-prefixed variables
								// with "." replaced by "_"
								// (weed/util/config.go:204-212). With a password
								// set, the UI requires a login.
								{"name": "WEED_ADMIN_USER", "value": s3AdminUser},
								secretEnv("WEED_ADMIN_PASSWORD", "minio_root_password"),
							},
							"ports": []map[string]interface{}{
								{"name": "s3", "containerPort": s3Port, "protocol": "TCP"},
								{"name": "admin", "containerPort": s3AdminPort, "protocol": "TCP"},
							},
							"volumeMounts": []map[string]interface{}{
								{"name": "data", "mountPath": "/data", "subPath": "seaweedfs"},
							},
							// Kubeflow Pipelines requests 64m/256Mi; the limit
							// leaves room for the admin UI and multipart
							// uploads (kubeflow/pipelines#13686 raised the CPU).
							"resources": map[string]interface{}{
								"limits":   map[string]interface{}{"cpu": "500m", "memory": "1Gi"},
								"requests": map[string]interface{}{"cpu": "64m", "memory": "256Mi"},
							},
							// Restricted pod security without a fixed UID, so
							// OpenShift's restricted-v2 SCC assigns one.
							"securityContext": map[string]interface{}{
								"allowPrivilegeEscalation": false,
								"capabilities":             map[string]interface{}{"drop": []string{"ALL"}},
								"runAsNonRoot":             true,
								"seccompProfile":           map[string]interface{}{"type": "RuntimeDefault"},
							},
							// GET /status on the S3 port
							// (weed/s3api/s3api_server.go:801 at 4.48).
							"readinessProbe": map[string]interface{}{"httpGet": map[string]interface{}{"path": "/status", "port": s3Port}, "initialDelaySeconds": 5, "periodSeconds": 5},
							"livenessProbe":  map[string]interface{}{"httpGet": map[string]interface{}{"path": "/status", "port": s3Port}, "initialDelaySeconds": 30, "periodSeconds": 10, "failureThreshold": 6},
						}},
						"restartPolicy": "Always",
					},
				},
			},
		}, false},
		{"Service", "Service", map[string]interface{}{
			"apiVersion": "v1", "kind": "Service",
			"metadata": meta(minioServiceName),
			"spec": map[string]interface{}{
				"selector": podLabels,
				"type":     "ClusterIP",
				"ports": []map[string]interface{}{
					{"name": "api", "port": minioServiceAPIPort, "targetPort": s3Port},
					{"name": "ui", "port": minioServiceUIPort, "targetPort": s3AdminPort},
				},
			},
		}, true},
		{"Admin UI Route", "Route", map[string]interface{}{
			"apiVersion": "route.openshift.io/v1", "kind": "Route",
			"metadata": meta("minio-ui"),
			"spec": map[string]interface{}{
				"to":   map[string]interface{}{"kind": "Service", "name": minioServiceName, "weight": 100},
				"port": map[string]interface{}{"targetPort": "ui"},
				"tls":  map[string]interface{}{"termination": "edge", "insecureEdgeTerminationPolicy": "Redirect"},
			},
		}, true},
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

// templateHint explains a 403 from the API server: the updater's own
// ServiceAccount was refused, which after an upgrade means its RBAC still
// comes from an older deploy/template.yaml.
func templateHint(err error) string {
	if !IsK8sError(err, http.StatusForbidden) {
		return ""
	}
	return " The updater's ServiceAccount is not allowed to do this. If the updater was upgraded recently, its permissions still come from an older deploy/template.yaml: an admin should run `make upgrade` to re-apply it, then retry."
}

// SetupMinIO deploys the S3 storage (SeaweedFS) with a bucket for pipeline
// artifacts. It reports success only once the SeaweedFS pod is ready and the
// bucket exists. Re-running it updates the objects it created, so it also
// repairs a partial setup. It never changes an object it did not create:
// every object is checked first, absent ones are created with POST, and
// updates are guarded by the resourceVersion that was checked (see
// ensureToolObject).
//
// When an earlier version's MinIO is deployed, setup replaces it and starts
// fresh: SeaweedFS gets a new PVC, minio-service is switched to it only once
// it is ready (until then MinIO keeps serving), and the MinIO Deployment and
// its NetworkPolicy are removed afterwards. The MinIO PVC is kept for a
// rollback or a manual copy; teardown deletes it. Objects stored in MinIO
// are not copied.
func SetupMinIO(c *Client) (*types.OperationResponse, error) {
	logs := []string{}
	fail := func(msg, code, activity string) (*types.OperationResponse, error) {
		slog.Warn("s3 storage setup failed", "reason", activity, "user", getUser(c))
		recordMinIOActivity(c, "setup-minio", fmt.Sprintf("namespace=%s (%s)", minioNamespace, activity), false)
		return &types.OperationResponse{Success: false, Message: msg, Logs: logs, ErrorCode: code}, nil
	}
	if strings.TrimSpace(os.Getenv("MINIO_IMAGE")) != "" {
		logs = append(logs, "Note: MINIO_IMAGE is set but no longer used; the S3 storage runs SeaweedFS (override its image with SEAWEEDFS_IMAGE).")
	}

	// Step 1: Namespace. Never adopt a namespace this tool did not create.
	logs = append(logs, "Creating namespace...")
	ns, err := lookupMinIONamespace(c)
	if err != nil {
		return fail(fmt.Sprintf("Cannot check namespace '%s': %v.%s", minioNamespace, err, templateHint(err)), errorCodeFromK8sErr(err), "namespace check failed")
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
				return fail(fmt.Sprintf("Cannot check namespace '%s': %v.%s", minioNamespace, err, templateHint(err)), errorCodeFromK8sErr(err), "namespace check failed")
			}
		default:
			return fail(fmt.Sprintf("Failed to create namespace: %v.%s", err, templateHint(err)), errorCodeFromK8sErr(err), "namespace creation failed")
		}
	}
	if ns.Exists && ns.terminating() {
		return fail("Namespace 'minio' is terminating. Wait for it to fully delete before re-creating.", "terminating", "namespace terminating")
	}
	if !ns.Managed {
		return fail("Namespace 'minio' already exists and was not created by this tool. Nothing was changed. Remove it yourself, or use a cluster without it.", "not_managed", "namespace not managed")
	}

	// Step 2: Check every object the tool writes or removes before writing
	// any, so a refusal leaves everything unchanged.
	type current struct {
		meta  objectMeta
		found bool
	}
	existing := map[string]current{}
	var foreign, deleting []string
	for _, ref := range []struct{ kind, name string }{
		{"PersistentVolumeClaim", s3PVC},
		{"Secret", "minio-secret"},
		{"NetworkPolicy", s3NetworkPolicy},
		{"Deployment", s3Deployment},
		{"Service", minioServiceName},
		{"Route", "minio-ui"},
		{"Route", "minio-api"},
		{"Deployment", legacyMinIODeployment},
		{"NetworkPolicy", legacyMinIONetworkPolicy},
	} {
		meta, found, err := readMinIOObject(c, ref.kind, ref.name)
		if err != nil {
			return fail(fmt.Sprintf("Cannot read %s %s/%s; nothing was changed: %v.%s", ref.kind, minioNamespace, ref.name, err, templateHint(err)), errorCodeFromK8sErr(err), "read failed")
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
			return fail(fmt.Sprintf("Failed to label namespace created by an earlier version: %v.%s", err, templateHint(err)), errorCodeFromK8sErr(err), "namespace label failed")
		}
		logs = append(logs, "OK: Namespace from an earlier version labelled as managed by this tool")
	} else if ns.Meta.Name != "" {
		logs = append(logs, "Namespace already exists (created by this tool) — reusing")
	}
	legacy := existing["Deployment/"+legacyMinIODeployment]
	if legacy.found {
		logs = append(logs, "MinIO from an earlier version found: SeaweedFS is deployed next to it on a new PVC, starting with an empty bucket; MinIO keeps serving until SeaweedFS is ready.")
	}

	// Step 3: Create or update the server objects. The Service and the
	// Route follow once the server is ready (step 5).
	user, password, credErr := minioCredentials(c)
	if credErr != nil {
		return fail(fmt.Sprintf("Cannot determine the S3 credentials: %v.%s", credErr, templateHint(credErr)), errorCodeFromK8sErr(credErr), "credentials")
	}
	var applied []string
	apply := func(r minioResource) (*types.OperationResponse, error) {
		logs = append(logs, fmt.Sprintf("Applying %s...", r.name))
		path, ok := minioApplyPath(r.obj)
		createPath, createOK := minioCreatePath(r.kind)
		if !ok || !createOK {
			return fail(fmt.Sprintf("Internal error: unexpected S3 storage object %s %v", r.kind, r.obj["metadata"]), "internal", "unknown object")
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
			return fail(fmt.Sprintf("Failed to apply %s: %v. Already applied: %s. Re-run setup to retry, or tear down to remove it.%s", r.name, err, done, templateHint(err)), code, "apply "+r.name+" failed")
		}
		applied = append(applied, r.name)
		logs = append(logs, fmt.Sprintf("OK: %s %s", r.name, action))
		return nil, nil
	}
	resources := s3Resources(user, password)
	for _, r := range resources {
		if r.frontDoor {
			continue
		}
		if resp, err := apply(r); resp != nil || err != nil {
			return resp, err
		}
	}

	// Step 4: Wait for the pod. Stop early when it cannot start.
	stillMinIO := ""
	if legacy.found {
		stillMinIO = " MinIO from the earlier version still serves the pipeline servers; nothing was switched over."
	}
	logs = append(logs, fmt.Sprintf("Waiting up to %s for SeaweedFS to become ready...", MinIOReadyTimeout))
	issue, err := waitS3Ready(c, s3Image())
	if err != nil {
		if errors.Is(err, errS3NotReady) {
			reason := "the pod is still starting"
			if issue.Reason != "" {
				reason = issue.Reason
				if issue.Message != "" {
					reason += ": " + truncateMessage(issue.Message)
				}
			}
			logs = append(logs, "SeaweedFS is not ready: "+reason)
			return fail(fmt.Sprintf("SeaweedFS was deployed but is not ready after %s (%s). The status shows the current state; re-run setup once it is ready.%s", MinIOReadyTimeout, reason, stillMinIO), "timeout", "not ready")
		}
		if errors.Is(err, errS3CannotStart) {
			msg := issue.Reason
			if issue.Message != "" {
				msg += ": " + truncateMessage(issue.Message)
			}
			logs = append(logs, "SeaweedFS cannot start: "+msg)
			return fail(fmt.Sprintf("SeaweedFS was deployed but its pod cannot start (%s). Fix the cause (for example set SEAWEEDFS_IMAGE to a pullable image) and re-run setup, or tear it down.%s", msg, stillMinIO), "not_ready", issue.Reason)
		}
		return fail(fmt.Sprintf("Stopped waiting for SeaweedFS: %v.%s%s", err, templateHint(err), stillMinIO), errorCodeFromK8sErr(err), "wait failed")
	}
	logs = append(logs, "OK: SeaweedFS is running")

	// Step 5: Route traffic to it: minio-service (the address pipeline
	// servers use) and the admin UI Route.
	for _, r := range resources {
		if !r.frontDoor {
			continue
		}
		if resp, err := apply(r); resp != nil || err != nil {
			return resp, err
		}
	}

	// Step 6: Create the bucket through the Service, as pipeline servers
	// reach it (try once, retry once if it fails). SeaweedFS also creates
	// it on start (-bucket).
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
		logs = append(logs, fmt.Sprintf("Bucket creation failed: %v — you can create it from the SeaweedFS admin UI.", bucketErr))
		return fail(fmt.Sprintf("SeaweedFS deployed but bucket creation failed: %v. Re-run setup to retry, or create bucket '%s' from the admin UI.", bucketErr, minioBucket), "", "bucket failed")
	}
	logs = append(logs, fmt.Sprintf("OK: Bucket '%s' created", minioBucket))

	// Step 7: Remove the MinIO of an earlier version, now that SeaweedFS
	// serves minio-service: its Deployment first, then, once its pods are
	// gone, the NetworkPolicy that fences them in. Its PVC is kept.
	migratedNote := ""
	if legacy.found {
		depPath, _ := minioDeletePath("Deployment", legacyMinIODeployment)
		if _, err := deleteWithUID(c, depPath, legacy.meta.UID); err != nil && !IsK8sError(err, 404) {
			return fail(fmt.Sprintf("SeaweedFS is running with bucket '%s' and serves minio-service, but the MinIO Deployment of the earlier version could not be removed: %v. Re-run setup to retry.%s", minioBucket, err, templateHint(err)), "partial_failure", "legacy deployment cleanup failed")
		}
		logs = append(logs, "OK: Deployment minio (MinIO from an earlier version) removed")
		migratedNote = " MinIO from the earlier version was replaced. SeaweedFS started fresh: objects stored in MinIO were not copied, so artifacts of earlier pipeline runs return 404; pipeline servers keep working with no edits."
		kept, found, err := readMinIOPVC(c, legacyMinIOPVC)
		switch {
		case err != nil:
			migratedNote += fmt.Sprintf(" Its data volume, PVC minio-pvc, was not touched (it could not be listed: %v).", err)
		case found && minioObjectOwned(kept.Meta):
			size := ""
			if kept.Size != "" {
				size = " (" + kept.Size + ")"
			}
			migratedNote += fmt.Sprintf(" Its data volume, PVC minio-pvc%s, is kept for a rollback or a manual copy; teardown deletes it.", size)
			logs = append(logs, "Kept PVC minio-pvc"+size+" (MinIO data) for a rollback or a manual copy")
		}
	}
	if cur := existing["NetworkPolicy/"+legacyMinIONetworkPolicy]; cur.found {
		// A MinIO pod that is still shutting down would be reachable on every
		// port without its policy, so the policy goes only once no pod with
		// its selector is left. A re-run (Repair) finishes it otherwise.
		logs = append(logs, fmt.Sprintf("Waiting up to %s for the MinIO pods to stop...", MinIOPodsGoneTimeout))
		gone, err := waitLegacyMinIOPodsGone(c)
		if err != nil {
			return fail(fmt.Sprintf("SeaweedFS is running with bucket '%s' and serves minio-service, but whether the old MinIO pods are gone cannot be checked: %v. NetworkPolicy minio-ingress was kept; re-run setup (Repair) to finish the cleanup.%s%s", minioBucket, err, templateHint(err), migratedNote), "partial_failure", "legacy pod check failed")
		}
		if !gone {
			logs = append(logs, "MinIO pods still running: kept NetworkPolicy minio-ingress")
			return fail(fmt.Sprintf("SeaweedFS is running with bucket '%s' and serves minio-service, but a MinIO pod of the earlier version is still shutting down after %s, so NetworkPolicy minio-ingress was kept to fence it in. Re-run setup (Repair) to finish the cleanup.%s", minioBucket, MinIOPodsGoneTimeout, migratedNote), "partial_failure", "legacy pods still running")
		}
		npPath, _ := minioDeletePath("NetworkPolicy", legacyMinIONetworkPolicy)
		if _, err := deleteWithUID(c, npPath, cur.meta.UID); err != nil && !IsK8sError(err, 404) {
			return fail(fmt.Sprintf("SeaweedFS is running with bucket '%s', but NetworkPolicy minio-ingress of the earlier MinIO could not be removed: %v. Re-run setup (Repair) to retry.%s%s", minioBucket, err, templateHint(err), migratedNote), "partial_failure", "legacy policy cleanup failed")
		}
		logs = append(logs, "OK: NetworkPolicy minio-ingress (MinIO from an earlier version) removed")
	}

	// Step 8: Remove the minio-api Route earlier versions created: it
	// exposed the S3 API outside the cluster. A pipeline server whose
	// object storage points at the Route's host would lose its artifacts
	// store, so the Route is kept while any DSPA uses it, and also when
	// that cannot be checked (fail closed).
	keptRouteNote := ""
	if cur := existing["Route/minio-api"]; cur.found {
		users, err := minioAPIRouteUsers(c)
		if err != nil {
			return fail(fmt.Sprintf("SeaweedFS is running with bucket '%s', but Route minio-api, which exposes the S3 API outside the cluster, was kept: cannot check whether a pipeline server uses it: %v. Re-run setup to retry.%s", minioBucket, err, migratedNote), "partial_failure", "route consumer check failed")
		}
		if len(users) > 0 {
			keptRouteNote = fmt.Sprintf(" Route minio-api, which exposes the S3 API outside the cluster, was kept because %s %s %s its host. Point their object storage at host %s with scheme http (the in-cluster service), then re-run setup to remove the Route.", verb(len(users), "pipeline server", "pipeline servers"), strings.Join(users, ", "), verb(len(users), "uses", "use"), minioS3Host())
			logs = append(logs, "Kept Route minio-api: used by "+strings.Join(users, ", "))
		}
	}
	if cur := existing["Route/minio-api"]; cur.found && keptRouteNote == "" {
		routePath, _ := minioDeletePath("Route", "minio-api")
		if _, err := deleteWithUID(c, routePath, cur.meta.UID); err != nil && !IsK8sError(err, 404) {
			return fail(fmt.Sprintf("SeaweedFS is running with bucket '%s', but Route minio-api, which exposes the S3 API outside the cluster, could not be removed: %v. Delete it with `oc delete route -n minio minio-api`, or re-run setup.%s", minioBucket, err, migratedNote), "partial_failure", "route cleanup failed")
		}
		logs = append(logs, "OK: Route minio-api removed (it exposed the S3 API outside the cluster)")
	}

	slog.Info("s3 storage setup complete", "user", getUser(c), "migrated", legacy.found)
	detail := fmt.Sprintf("namespace=%s bucket=%s", minioNamespace, minioBucket)
	if legacy.found {
		detail += " (replaced MinIO, kept minio-pvc)"
	}
	recordMinIOActivity(c, "setup-minio", detail, true)

	return &types.OperationResponse{
		Success: true, Message: fmt.Sprintf("S3 storage (SeaweedFS) deployed with bucket '%s'; pipeline servers reach it at %s. Sign in to the admin UI as '%s'; the password and the S3 keys are in secret 'minio-secret' in namespace '%s'.%s%s", minioBucket, minioS3Host(), s3AdminUser, minioNamespace, migratedNote, keptRouteNote),
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

// waitLegacyMinIOPodsGone waits, bounded by MinIOPodsGoneTimeout, until no
// pod with the replaced MinIO's selector (app=minio) is left, terminating or
// not. It returns false on timeout.
func waitLegacyMinIOPodsGone(c *Client) (bool, error) {
	return waitForGone(c, MinIOPodsGoneTimeout, MinIODeletePoll, func(cc *Client) (bool, error) {
		body, _, err := cc.get(namespacedPath("v1", "pods", minioNamespace, "") + "?labelSelector=" + url.QueryEscape(minioAppSelector))
		if err != nil {
			return false, err
		}
		var list struct {
			Items []json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(body, &list); err != nil {
			return false, fmt.Errorf("parse pod list: %w", err)
		}
		return len(list.Items) == 0, nil
	})
}

var (
	errS3NotReady    = errors.New("seaweedfs not ready before timeout")
	errS3CannotStart = errors.New("seaweedfs pod cannot start")
)

// waitS3Ready polls the SeaweedFS Deployment until its current template has
// a ready replica. It returns errS3CannotStart as soon as a pod of image
// reports a terminal waiting reason (ImagePullBackOff, CrashLoopBackOff, ...),
// and errS3NotReady on timeout, with the last observed pod issue. Every
// request runs under one context bounded by MinIOReadyTimeout, so a request
// that starts just before the deadline cannot overrun it.
func waitS3Ready(c *Client, image string) (workloadPodIssue, error) {
	ctx, cancel := context.WithTimeout(c.ctx, MinIOReadyTimeout)
	defer cancel()
	cc := c.WithContext(ctx)
	var last workloadPodIssue
	expired := func() (workloadPodIssue, error) {
		if c.ctx.Err() != nil {
			return last, c.ctx.Err()
		}
		return last, errS3NotReady
	}
	for {
		body, _, err := cc.get(namespacedPath("apps/v1", "deployments", minioNamespace, s3Deployment))
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
					if issue, perr := inspectPods(cc, minioNamespace, s3AppSelector, image); perr == nil && issue.Reason != "" {
						last = issue
						if issue.Terminal {
							return issue, errS3CannotStart
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
		return fmt.Errorf("empty S3 credentials in secret minio-secret")
	}

	endpoint := "http://" + minioS3Host()
	return s3PutBucket(c.ctx, endpoint, accessKey, secretKey, bucket)
}

// minioKeptNamespaceHint ends every teardown message: the namespace is never
// deleted, because the tool cannot see everything in it without reading
// every Secret in the cluster (a cluster-wide list grant).
const minioKeptNamespaceHint = " Namespace 'minio' was kept: it may hold objects this tool did not create. Delete it with `oc delete project minio` once you've checked it's empty."

// minioTeardownObjects are the objects teardown removes, in order: the
// SeaweedFS objects and those left by an earlier version's MinIO (its
// Deployment if setup has not replaced it yet, and its kept data PVC). The
// Deployments go first, so their pods release the PVCs (whose
// kubernetes.io/pvc-protection finalizer waits for that).
var minioTeardownObjects = []struct{ kind, name string }{
	{"Deployment", s3Deployment},
	{"Deployment", legacyMinIODeployment},
	{"NetworkPolicy", s3NetworkPolicy},
	{"NetworkPolicy", legacyMinIONetworkPolicy},
	{"Route", "minio-ui"},
	{"Route", "minio-api"},
	{"Service", minioServiceName},
	{"Secret", "minio-secret"},
	{"PersistentVolumeClaim", s3PVC},
	{"PersistentVolumeClaim", legacyMinIOPVC},
}

// TeardownMinIO removes the S3 storage objects this tool created, and with
// them the data PVCs (SeaweedFS's and the kept MinIO one), when no pipeline
// server uses the storage. Each object is read by name, deleted only when it
// carries the tool's label (or an earlier version's server-side-apply
// marker), and deleted with a UID precondition, so an object someone else
// created or recreated is never removed. The minio namespace itself is never
// deleted (see minioKeptNamespaceHint); it stays labelled, so setup can
// reuse it. Re-running it is safe.
func TeardownMinIO(c *Client) (*types.OperationResponse, error) {
	logs := []string{}
	fail := func(msg, code, activity string) (*types.OperationResponse, error) {
		recordMinIOActivity(c, "teardown-minio", fmt.Sprintf("%s (%s)", minioNamespace, activity), false)
		return &types.OperationResponse{Success: false, Message: msg, Logs: logs, ErrorCode: code}, nil
	}

	ns, err := lookupMinIONamespace(c)
	if err != nil {
		return fail(fmt.Sprintf("Cannot check namespace '%s'; nothing was deleted: %v.%s", minioNamespace, err, templateHint(err)), errorCodeFromK8sErr(err), "namespace check failed")
	}
	if !ns.Exists {
		logs = append(logs, "Namespace already absent")
		return &types.OperationResponse{Success: true, Message: "The S3 storage is not deployed; nothing to remove.", Logs: logs}, nil
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

	// Teardown order is MLflow, pipeline servers, then the S3 storage.
	// Pipeline servers that store artifacts here would break, and a DSPA
	// that is still terminating must finish first. A missing DSPA CRD means
	// there are none.
	dspas, err := listDSPAs(c, "")
	if err != nil && !errors.Is(err, errDSPACRDMissing) {
		return fail(fmt.Sprintf("Cannot verify pipeline dependencies; the S3 storage was not deleted: %v.%s", err, templateHint(err)), errorCodeFromK8sErr(err), "dependency check failed")
	}
	// Pipeline servers may reach the storage through a Route host or the
	// ClusterIP. When those cannot be read and any pipeline server exists, a
	// dependency cannot be ruled out: refuse rather than delete the PVC a
	// pipeline server may still store its artifacts in.
	endpoints, err := readMinIOEndpoints(c)
	if err != nil && len(dspas) > 0 {
		return fail(fmt.Sprintf("Cannot verify which pipeline servers use the S3 storage (its Route hosts or service address could not be read); nothing was deleted: %v.%s", err, templateHint(err)), errorCodeFromK8sErr(err), "dependency check failed")
	}
	if reason := minioTeardownBlocker(dspas, endpoints); reason != "" {
		return fail("Cannot tear down the S3 storage yet. "+reason, "prerequisites", "pipeline servers depend on it")
	}

	// A namespace from an earlier version is recognised by its Deployment;
	// label it before that Deployment goes, so setup can still reuse it.
	if ns.Legacy {
		patch, _ := json.Marshal(map[string]interface{}{"metadata": map[string]interface{}{"labels": toolLabels()}})
		if _, _, err := c.patch("/api/v1/namespaces/"+minioNamespace, patch); err != nil {
			return fail(fmt.Sprintf("Failed to label namespace 'minio', created by an earlier version, before removing the S3 storage; nothing was deleted: %v.%s", err, templateHint(err)), errorCodeFromK8sErr(err), "namespace label failed")
		}
		logs = append(logs, "OK: Namespace from an earlier version labelled as managed by this tool")
	}

	var deleted, kept, failed, pvcNames []string
	var forbidden error
	pvcUIDs := map[string]bool{}
	for _, o := range minioTeardownObjects {
		desc := o.kind + " " + o.name
		meta, found, err := readMinIOObject(c, o.kind, o.name)
		switch {
		case err != nil:
			failed = append(failed, fmt.Sprintf("%s: cannot read it: %v", desc, err))
			if IsK8sError(err, http.StatusForbidden) {
				forbidden = err
			}
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
				if IsK8sError(err, http.StatusForbidden) {
					forbidden = err
				}
				continue
			}
			deleted = append(deleted, desc)
			logs = append(logs, fmt.Sprintf("OK: %s deleted", desc))
		}
		if o.kind == "PersistentVolumeClaim" {
			pvcUIDs[meta.UID] = true
			pvcNames = append(pvcNames, o.name)
		}
	}

	keptNote := ""
	if len(kept) > 0 {
		keptNote = fmt.Sprintf(" Left in place because this tool did not create them: %s.", strings.Join(kept, ", "))
	}
	dataNote := ""
	if len(pvcNames) > 0 {
		dataNote = fmt.Sprintf(" %s %s and all stored objects (pipeline artifacts, models, test files) are deleted with %s.", verb(len(pvcNames), "PVC", "PVCs"), strings.Join(pvcNames, " and "), verb(len(pvcNames), "it", "them"))
	}
	if len(failed) > 0 {
		recordMinIOActivity(c, "teardown-minio", fmt.Sprintf("%s (failed: %s)", minioNamespace, strings.Join(failed, "; ")), false)
		code := "partial_failure"
		if len(deleted) == 0 {
			code = "delete_failed"
		}
		return &types.OperationResponse{
			Success: false, Message: fmt.Sprintf("Some S3 storage objects could not be removed: %s. Re-run teardown to retry.%s%s%s", strings.Join(failed, "; "), templateHint(forbidden), keptNote, minioKeptNamespaceHint),
			Logs: logs, ErrorCode: code,
		}, nil
	}
	if len(deleted) == 0 && len(pvcUIDs) == 0 {
		recordMinIOActivity(c, "teardown-minio", minioNamespace+" (nothing to remove)", true)
		return &types.OperationResponse{Success: true, Message: "No S3 storage objects created by this tool remain." + keptNote + minioKeptNamespaceHint, Logs: logs}, nil
	}
	slog.Info("s3 storage teardown", "user", getUser(c), "deleted", strings.Join(deleted, ", "))
	recordMinIOActivity(c, "teardown-minio", minioNamespace+" (objects deleted, namespace kept)", true)

	if len(pvcUIDs) > 0 {
		var remaining []string
		gone, err := waitForGone(c, MinIODeleteTimeout, MinIODeletePoll, func(cc *Client) (bool, error) {
			pvcs, err := listMinIOPVCs(cc)
			if err != nil {
				return false, err
			}
			remaining = remaining[:0]
			for _, p := range pvcs {
				if pvcUIDs[p.Meta.UID] {
					remaining = append(remaining, p.Meta.Name)
				}
			}
			return len(remaining) == 0, nil
		})
		if err != nil {
			return &types.OperationResponse{
				Success: false, Message: fmt.Sprintf("The S3 storage objects were deleted, but whether its PVCs are gone cannot be checked: %v.%s%s%s%s", err, templateHint(err), dataNote, keptNote, minioKeptNamespaceHint),
				Logs: logs, ErrorCode: firstNonEmpty(errorCodeFromK8sErr(err), "in_progress"),
			}, nil
		}
		if !gone {
			logs = append(logs, fmt.Sprintf("PVC %s still terminating", strings.Join(remaining, ", ")))
			return &types.OperationResponse{
				Success: false, Message: fmt.Sprintf("The S3 storage objects were deleted, but PVC %s %s still terminating after %s; re-run teardown to check again.%s%s%s", strings.Join(remaining, ", "), verb(len(remaining), "is", "are"), MinIODeleteTimeout, dataNote, keptNote, minioKeptNamespaceHint),
				Logs: logs, ErrorCode: "in_progress",
			}, nil
		}
	}
	logs = append(logs, "OK: S3 storage removed")
	return &types.OperationResponse{Success: true, Message: "S3 storage removed." + dataNote + keptNote + minioKeptNamespaceHint, Logs: logs}, nil
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
