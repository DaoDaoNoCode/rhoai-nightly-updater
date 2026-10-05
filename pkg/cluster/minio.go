package cluster

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
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
	// minioDefaultImage is Open Data Hub's MinIO build
	// quay.io/opendatahub/minio:RELEASE.2019-08-14T20-37-41Z-license-compliance,
	// pinned by digest. It is the image every data-science-pipelines-operator
	// sample DSPA uses (README "Deploy a Minio Object Storage component",
	// config/samples/*, rhoai-3.6 branch) and it is anonymously pullable
	// (quay.io pull token grants "pull"; manifest GET returns 200).
	// quay.io/minio/minio no longer allows anonymous pulls (401).
	minioDefaultImage = "quay.io/opendatahub/minio@sha256:587abc14be9bbeed794473cf7290c40e377062f2f77f5e4e27742a77680f08e0"
)

var (
	// MinIOReadyTimeout bounds how long setup waits for the MinIO pod. It
	// stays below the frontend's 120s request timeout. Tests shorten it.
	MinIOReadyTimeout = 90 * time.Second
	MinIOReadyPoll    = 3 * time.Second
	// MinIODeleteTimeout bounds how long teardown waits for the namespace
	// to disappear before reporting that deletion is still in progress.
	MinIODeleteTimeout = 60 * time.Second
	MinIODeletePoll    = 2 * time.Second

	// minioBucketCreator creates the pipelines bucket; tests replace it
	// because the in-cluster S3 endpoint is unreachable from unit tests.
	minioBucketCreator    = createMinioBucket
	minioBucketRetryDelay = 3 * time.Second
)

// minioImage returns the MinIO image, overridable with MINIO_IMAGE (for
// example a mirror on a disconnected cluster).
func minioImage() string {
	if img := strings.TrimSpace(os.Getenv("MINIO_IMAGE")); img != "" {
		return img
	}
	return minioDefaultImage
}

// minioCredentials returns the MinIO root user and password. Explicit
// MINIO_ROOT_USER / MINIO_ROOT_PASSWORD env vars win. Otherwise credentials
// already stored in minio-secret are reused, because pipeline servers copy
// them into their own secrets and MinIO picks up a changed secret on restart.
// Only a first-time setup generates a random password, so no secret is baked
// into the binary.
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
		user = "minio"
	}
	if password == "" {
		b := make([]byte, 16)
		if _, randErr := rand.Read(b); randErr != nil {
			return "", "", fmt.Errorf("generate MinIO password: %w", randErr)
		}
		password = hex.EncodeToString(b)
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

// minioOwnedPVCs lists the PVCs in the minio namespace, split into the ones
// this tool created and any others.
func minioOwnedPVCs(c *Client) (owned, foreign []string, err error) {
	body, _, err := c.get(namespacedPath("v1", "persistentvolumeclaims", minioNamespace, ""))
	if err != nil {
		return nil, nil, err
	}
	var list struct {
		Items []struct {
			Metadata objectMeta `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, nil, fmt.Errorf("parse PVC list: %w", err)
	}
	for _, item := range list.Items {
		if item.Metadata.hasToolLabel() || item.Metadata.createdByToolApply() {
			owned = append(owned, item.Metadata.Name)
		} else {
			foreign = append(foreign, item.Metadata.Name)
		}
	}
	return owned, foreign, nil
}

type minioDeployment struct {
	Metadata struct {
		Generation int64 `json:"generation"`
	} `json:"metadata"`
	Spec struct {
		Replicas *int `json:"replicas"`
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

func getMinIOStatus(c *Client) types.ResourceState {
	state := types.ResourceState{Namespace: minioNamespace}

	ns, err := lookupMinIONamespace(c)
	if err != nil {
		state.Message = "Cannot read MinIO status: " + err.Error()
		return state
	}
	if !ns.Exists {
		state.Message = "Not deployed"
		return state
	}
	state.ManagedByTool = ns.Managed
	if ns.terminating() {
		state.Terminating = true
		state.Message = "Terminating"
		state.SetupBlockedReason = "Namespace 'minio' is still being deleted."
		return state
	}
	if !ns.Managed {
		state.SetupBlockedReason = "Namespace 'minio' exists but was not created by this tool, so setup will not modify it."
		state.TeardownBlockedReason = "Namespace 'minio' was not created by this tool."
	}

	var (
		wg                 sync.WaitGroup
		deployBody         []byte
		deployErr          error
		apiHost, uiHost    string
		ownedPVCs, foreign []string
		pvcErr             error
	)
	wg.Add(3)
	go func() {
		defer wg.Done()
		deployBody, _, deployErr = c.get(namespacedPath("apps/v1", "deployments", minioNamespace, "minio"))
	}()
	go func() {
		defer wg.Done()
		apiHost = routeHost(c, minioNamespace, "minio-api")
		uiHost = routeHost(c, minioNamespace, "minio-ui")
	}()
	go func() {
		defer wg.Done()
		if ns.Managed {
			ownedPVCs, foreign, pvcErr = minioOwnedPVCs(c)
		}
	}()
	wg.Wait()

	if apiHost != "" {
		state.APIRoute = "https://" + apiHost
	}
	if uiHost != "" {
		state.UIRoute = "https://" + uiHost
	}
	if ns.Managed {
		switch {
		case pvcErr != nil:
			state.TeardownBlockedReason = "Cannot list the PVCs in namespace 'minio': " + pvcErr.Error()
		case len(foreign) > 0:
			state.TeardownBlockedReason = fmt.Sprintf("Namespace 'minio' contains PVC(s) not created by this tool: %s.", strings.Join(foreign, ", "))
		}
		state.DataPVCs = ownedPVCs
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
		return state
	}
	state.Deployed = true

	var deploy minioDeployment
	if err := json.Unmarshal(deployBody, &deploy); err != nil {
		state.Message = "Cannot parse MinIO deployment"
		return state
	}
	if deploy.Status.ReadyReplicas >= 1 {
		state.Ready = true
		state.Message = "Running"
		return state
	}
	state.Message = fmt.Sprintf("%d/%d ready", deploy.Status.ReadyReplicas, deploy.Status.Replicas)
	if issue, err := inspectPods(c, minioNamespace, minioAppSelector, ""); err == nil {
		applyPodIssue(&state, issue)
	}
	return state
}

// minioResources returns the objects SetupMinIO applies, in order.
func minioResources(user, password string) []struct {
	name string
	obj  map[string]interface{}
} {
	meta := func(name string) map[string]interface{} {
		return map[string]interface{}{"name": name, "namespace": minioNamespace, "labels": toolLabels()}
	}
	secretEnv := func(name, key string) map[string]interface{} {
		return map[string]interface{}{"name": name, "valueFrom": map[string]interface{}{"secretKeyRef": map[string]interface{}{"name": "minio-secret", "key": key}}}
	}
	return []struct {
		name string
		obj  map[string]interface{}
	}{
		{"PVC", map[string]interface{}{
			"apiVersion": "v1", "kind": "PersistentVolumeClaim",
			"metadata": meta("minio-pvc"),
			"spec": map[string]interface{}{
				"accessModes": []string{"ReadWriteOnce"},
				"resources":   map[string]interface{}{"requests": map[string]interface{}{"storage": minioStorage}},
				"volumeMode":  "Filesystem",
			},
		}},
		{"Secret", map[string]interface{}{
			"apiVersion": "v1", "kind": "Secret",
			"metadata": meta("minio-secret"),
			"stringData": map[string]interface{}{
				"minio_root_user":     user,
				"minio_root_password": password,
			},
		}},
		{"Deployment", map[string]interface{}{
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
							// Same container contract as the DSPO-managed MinIO
							// (config/internal/minio/default/deployment.yaml.tmpl):
							// "server /data" with MINIO_ACCESS_KEY/MINIO_SECRET_KEY.
							// This 2019 release serves the S3 API and its web
							// browser on port 9000. MINIO_ROOT_* is also set for a
							// newer image supplied through MINIO_IMAGE.
							"args": []string{"server", "/data"},
							"env": []map[string]interface{}{
								secretEnv("MINIO_ACCESS_KEY", "minio_root_user"),
								secretEnv("MINIO_SECRET_KEY", "minio_root_password"),
								secretEnv("MINIO_ROOT_USER", "minio_root_user"),
								secretEnv("MINIO_ROOT_PASSWORD", "minio_root_password"),
							},
							"ports": []map[string]interface{}{
								{"containerPort": 9000, "protocol": "TCP"},
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
		{"Service", map[string]interface{}{
			"apiVersion": "v1", "kind": "Service",
			"metadata": meta(minioServiceName),
			"spec": map[string]interface{}{
				"selector": map[string]interface{}{"app": "minio"},
				"type":     "ClusterIP",
				"ports": []map[string]interface{}{
					{"name": "api", "port": 9000, "targetPort": 9000},
				},
			},
		}},
		{"API Route", map[string]interface{}{
			"apiVersion": "route.openshift.io/v1", "kind": "Route",
			"metadata": meta("minio-api"),
			"spec": map[string]interface{}{
				"to":   map[string]interface{}{"kind": "Service", "name": minioServiceName, "weight": 100},
				"port": map[string]interface{}{"targetPort": "api"},
				"tls":  map[string]interface{}{"termination": "edge", "insecureEdgeTerminationPolicy": "Redirect"},
			},
		}},
		{"UI Route", map[string]interface{}{
			"apiVersion": "route.openshift.io/v1", "kind": "Route",
			"metadata": meta("minio-ui"),
			"spec": map[string]interface{}{
				"to":   map[string]interface{}{"kind": "Service", "name": minioServiceName, "weight": 100},
				"port": map[string]interface{}{"targetPort": "api"},
				"tls":  map[string]interface{}{"termination": "edge", "insecureEdgeTerminationPolicy": "Redirect"},
			},
		}},
	}
}

var minioResourcePlural = map[string]string{
	"PersistentVolumeClaim": "persistentvolumeclaims",
	"Secret":                "secrets",
	"Deployment":            "deployments",
	"Service":               "services",
	"Route":                 "routes",
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

// SetupMinIO deploys MinIO with a bucket for pipeline artifacts. It reports
// success only once the MinIO pod is ready and the bucket exists. Re-running
// it re-applies the same objects, so it also repairs a partial setup.
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
	if ns.Legacy {
		patch, _ := json.Marshal(map[string]interface{}{"metadata": map[string]interface{}{"labels": toolLabels()}})
		if _, _, err := c.patch("/api/v1/namespaces/"+minioNamespace, patch); err != nil {
			return fail(fmt.Sprintf("Failed to label namespace created by an earlier version: %v", err), errorCodeFromK8sErr(err), "namespace label failed")
		}
		logs = append(logs, "OK: Namespace from an earlier version labelled as managed by this tool")
	} else if ns.Meta.Name != "" {
		logs = append(logs, "Namespace already exists (created by this tool) — reusing")
	}

	// Step 2: Apply resources
	minioUser, minioPass, credErr := minioCredentials(c)
	if credErr != nil {
		return fail(fmt.Sprintf("Cannot determine MinIO credentials: %v", credErr), errorCodeFromK8sErr(credErr), "credentials")
	}
	var applied []string
	for _, r := range minioResources(minioUser, minioPass) {
		logs = append(logs, fmt.Sprintf("Applying %s...", r.name))
		meta := r.obj["metadata"].(map[string]interface{})
		path := namespacedPath(r.obj["apiVersion"].(string), minioResourcePlural[r.obj["kind"].(string)], minioNamespace, meta["name"].(string))
		if _, _, err := c.apply(path, r.obj); err != nil {
			done := "nothing"
			if len(applied) > 0 {
				done = strings.Join(applied, ", ")
			}
			return fail(fmt.Sprintf("Failed to apply %s: %v. Already applied: %s. Re-run setup to retry, or tear down to remove it.", r.name, err, done), errorCodeFromK8sErr(err), "apply "+r.name+" failed")
		}
		applied = append(applied, r.name)
		logs = append(logs, fmt.Sprintf("OK: %s applied", r.name))
	}

	// Step 3: Wait for the pod. Stop early when it cannot start.
	logs = append(logs, fmt.Sprintf("Waiting up to %s for MinIO to become ready...", MinIOReadyTimeout))
	issue, err := waitMinIOReady(c)
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

	// Step 4: Create bucket via S3 API (try once, retry once if it fails)
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
	slog.Info("minio setup complete", "user", getUser(c))
	recordMinIOActivity(c, "setup-minio", fmt.Sprintf("namespace=%s bucket=%s", minioNamespace, minioBucket), true)

	return &types.OperationResponse{
		Success: true, Message: fmt.Sprintf("MinIO deployed with bucket '%s'. Console credentials are stored in secret 'minio-secret' in namespace '%s'.", minioBucket, minioNamespace),
		Logs: logs,
	}, nil
}

var (
	errMinIONotReady    = errors.New("minio not ready before timeout")
	errMinIOCannotStart = errors.New("minio pod cannot start")
)

// waitMinIOReady polls the MinIO Deployment until its current template has a
// ready replica. It returns errMinIOCannotStart as soon as a pod of the
// current image reports a terminal waiting reason (ImagePullBackOff,
// CrashLoopBackOff, ...), and errMinIONotReady on timeout, with the last
// observed pod issue.
func waitMinIOReady(c *Client) (podIssue, error) {
	deadline := time.Now().Add(MinIOReadyTimeout)
	var last podIssue
	for {
		body, _, err := c.get(namespacedPath("apps/v1", "deployments", minioNamespace, "minio"))
		if err == nil {
			var deploy minioDeployment
			if json.Unmarshal(body, &deploy) == nil {
				if deploy.rolledOut() {
					return podIssue{}, nil
				}
				// Only judge pods once the controller has seen the new
				// template, and only pods running the image just applied, so
				// an old pod from an earlier setup is not mistaken for this one.
				if deploy.Status.ObservedGeneration >= deploy.Metadata.Generation {
					if issue, perr := inspectPods(c, minioNamespace, minioAppSelector, minioImage()); perr == nil && issue.Reason != "" {
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
		if time.Now().After(deadline) {
			return last, errMinIONotReady
		}
		select {
		case <-c.ctx.Done():
			return last, c.ctx.Err()
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

// TeardownMinIO deletes the MinIO namespace, and with it the MinIO data PVC,
// but only when this tool created the namespace, nothing in it belongs to
// someone else, and no live pipeline server uses it. Re-running it is safe.
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
		return fail("Namespace 'minio' was not created by this tool, so it was not deleted.", "not_managed", "not managed")
	}
	if ns.terminating() {
		logs = append(logs, "Namespace is already being deleted")
		return &types.OperationResponse{
			Success: false, Message: "Namespace 'minio' is still being deleted." + namespaceDeletionDetail(c), Logs: logs, ErrorCode: "in_progress",
		}, nil
	}

	// Teardown order is MLflow, pipeline servers, then MinIO. Pipeline
	// servers that store artifacts here would break, and a DSPA that is still
	// terminating must finish first. A missing DSPA CRD means there are none.
	dspas, err := listDSPAs(c, "")
	if err != nil && !errors.Is(err, errDSPACRDMissing) {
		return fail(fmt.Sprintf("Cannot verify pipeline dependencies; MinIO was not deleted: %v", err), errorCodeFromK8sErr(err), "dependency check failed")
	}
	if reason := minioTeardownBlocker(dspas); reason != "" {
		return fail("Cannot tear down MinIO yet. "+reason, "prerequisites", "pipeline servers depend on it")
	}

	// A namespace delete has to list every namespaced type. With a dead CRD
	// conversion webhook that fails and the namespace hangs in Terminating,
	// so refuse instead of starting a delete that cannot finish.
	broken, err := brokenConversionWebhooks(c)
	if err != nil {
		return fail(fmt.Sprintf("Cannot check CRD conversion webhooks, which can block namespace deletion; nothing was deleted: %v", err), errorCodeFromK8sErr(err), "conversion webhook check failed")
	}
	if len(broken) > 0 {
		return fail(fmt.Sprintf("Not deleting namespace 'minio': these CRD conversion webhooks cannot serve, so the namespace would hang in Terminating: %s. Re-enable the component that provides the webhook (or remove the stale CRD), then retry.", strings.Join(broken, "; ")), "prerequisites", "broken conversion webhooks")
	}

	owned, foreign, err := minioOwnedPVCs(c)
	if err != nil {
		return fail(fmt.Sprintf("Cannot list PVCs in namespace 'minio'; nothing was deleted: %v", err), errorCodeFromK8sErr(err), "PVC list failed")
	}
	if len(foreign) > 0 {
		return fail(fmt.Sprintf("Namespace 'minio' contains PVC(s) not created by this tool (%s), so it was not deleted.", strings.Join(foreign, ", ")), "not_managed", "foreign PVCs")
	}

	logs = append(logs, "Deleting namespace 'minio'...")
	nsPath := "/api/v1/namespaces/" + minioNamespace
	if _, delErr := deleteWithUID(c, nsPath, ns.Meta.UID); delErr != nil && !IsK8sError(delErr, 404) {
		code := errorCodeFromK8sErr(delErr)
		if IsK8sError(delErr, 409) {
			code = "conflict"
		}
		return fail(fmt.Sprintf("Failed to delete namespace: %v", delErr), code, "namespace delete failed")
	}
	logs = append(logs, "OK: Deletion requested")

	dataNote := ""
	if len(owned) > 0 {
		dataNote = fmt.Sprintf(" PVC(s) %s and all stored objects (pipeline artifacts, models, test files) are deleted with it.", strings.Join(owned, ", "))
	}
	slog.Info("minio teardown", "user", getUser(c))
	recordMinIOActivity(c, "teardown-minio", minioNamespace, true)

	// PVC deletion does not hang on kubernetes.io/pvc-protection here: the
	// namespace controller deletes the MinIO pod too, and the finalizer is
	// released once no pod uses the claim (Kubernetes "Storage Object in Use
	// Protection").
	if !waitForDeletion(c, nsPath, MinIODeleteTimeout, MinIODeletePoll) {
		logs = append(logs, "Namespace is still terminating")
		return &types.OperationResponse{
			Success: false, Message: fmt.Sprintf("Deletion of namespace 'minio' was requested but it is still terminating after %s; the status shows it until it is gone. Re-run teardown to check again.%s%s", MinIODeleteTimeout, namespaceDeletionDetail(c), dataNote),
			Logs: logs, ErrorCode: "in_progress",
		}, nil
	}
	logs = append(logs, "OK: Namespace deleted")
	return &types.OperationResponse{
		Success: true, Message: "MinIO namespace deleted." + dataNote,
		Logs: logs,
	}, nil
}

// namespaceDeletionDetail returns the namespace controller's reported
// reasons (the NamespaceDeletion*Failure, NamespaceContentRemaining and
// NamespaceFinalizersRemaining conditions) for a minio namespace that is
// still terminating, or "".
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
