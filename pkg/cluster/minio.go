package cluster

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const (
	minioNamespace   = "minio"
	minioStorage     = "20Gi"
	minioBucket      = "pipelines"
	minioServiceName = "minio-service"
)

// minioCredentials returns the MinIO root user and password.
// Values are read from MINIO_ROOT_USER / MINIO_ROOT_PASSWORD env vars.
// When the env vars are unset, cryptographically random credentials are
// generated so that no secret is baked into the binary.
func minioCredentials() (user, password string) {
	user = os.Getenv("MINIO_ROOT_USER")
	password = os.Getenv("MINIO_ROOT_PASSWORD")
	if user == "" {
		user = "minio"
	}
	if password == "" {
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			// Extremely unlikely; fall back to a fixed value only as last resort.
			password = "minio-generated-fallback"
		} else {
			password = hex.EncodeToString(b)
		}
	}
	return user, password
}

func getMinIOStatus(c *Client) types.ResourceState {
	state := types.ResourceState{Namespace: minioNamespace}

	// Check namespace
	nsPath := "/api/v1/namespaces/" + minioNamespace
	nsBody, _, err := c.get(nsPath)
	if err != nil {
		state.Message = "Not deployed"
		return state
	}

	// Check if namespace is terminating
	var ns struct {
		Status struct{ Phase string `json:"phase"` } `json:"status"`
	}
	if json.Unmarshal(nsBody, &ns) == nil && ns.Status.Phase == "Terminating" {
		state.Message = "Terminating"
		return state
	}

	// Check deployment
	deployPath := namespacedPath("apps/v1", "deployments", minioNamespace, "minio")
	body, _, err := c.get(deployPath)
	if err != nil {
		state.Message = "Namespace exists but MinIO not deployed"
		return state
	}
	state.Deployed = true

	var deploy struct {
		Status struct {
			ReadyReplicas int `json:"readyReplicas"`
			Replicas      int `json:"replicas"`
		} `json:"status"`
	}
	if err := json.Unmarshal(body, &deploy); err == nil {
		if deploy.Status.ReadyReplicas >= 1 {
			state.Ready = true
			state.Message = "Running"
		} else {
			state.Message = fmt.Sprintf("%d/%d ready", deploy.Status.ReadyReplicas, deploy.Status.Replicas)
		}
	}

	// Get routes
	apiRoutePath := namespacedPath("route.openshift.io/v1", "routes", minioNamespace, "minio-api")
	if routeBody, _, err := c.get(apiRoutePath); err == nil {
		var route struct {
			Spec struct{ Host string `json:"host"` } `json:"spec"`
		}
		if json.Unmarshal(routeBody, &route) == nil && route.Spec.Host != "" {
			state.APIRoute = "https://" + route.Spec.Host
		}
	}

	uiRoutePath := namespacedPath("route.openshift.io/v1", "routes", minioNamespace, "minio-ui")
	if routeBody, _, err := c.get(uiRoutePath); err == nil {
		var route struct {
			Spec struct{ Host string `json:"host"` } `json:"spec"`
		}
		if json.Unmarshal(routeBody, &route) == nil && route.Spec.Host != "" {
			state.UIRoute = "https://" + route.Spec.Host
		}
	}

	return state
}

// SetupMinIO deploys MinIO with a bucket for pipeline artifacts.
func SetupMinIO(c *Client) (*types.OperationResponse, error) {
	logs := []string{}

	// Step 1: Create namespace
	logs = append(logs, "Creating namespace...")
	nsPath := "/api/v1/namespaces"
	nsData, _ := json.Marshal(map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata":   map[string]interface{}{"name": minioNamespace},
	})
	_, status, err := c.post(nsPath, nsData)
	if err != nil && !IsK8sError(err, 409) {
		return &types.OperationResponse{
			Success: false, Message: fmt.Sprintf("Failed to create namespace: %v", err),
			Logs: logs, ErrorCode: errorCodeFromK8sErr(err),
		}, nil
	}
	if status == 409 || IsK8sError(err, 409) {
		// Check if namespace is Terminating
		nsGetPath := "/api/v1/namespaces/" + minioNamespace
		nsBody, _, nsGetErr := c.get(nsGetPath)
		if nsGetErr == nil {
			var ns struct {
				Status struct{ Phase string `json:"phase"` } `json:"status"`
			}
			if json.Unmarshal(nsBody, &ns) == nil && ns.Status.Phase == "Terminating" {
				return &types.OperationResponse{
					Success: false, Message: "Namespace 'minio' is terminating. Wait for it to fully delete before re-creating.",
					Logs: logs, ErrorCode: "terminating",
				}, nil
			}
		}
		logs = append(logs, "Namespace already exists — reusing")
	} else {
		logs = append(logs, "OK: Namespace created")
	}

	// Step 2: Apply resources
	minioUser, minioPass := minioCredentials()
	resources := []struct {
		name string
		obj  map[string]interface{}
	}{
		{"PVC", map[string]interface{}{
			"apiVersion": "v1", "kind": "PersistentVolumeClaim",
			"metadata": map[string]interface{}{"name": "minio-pvc", "namespace": minioNamespace},
			"spec": map[string]interface{}{
				"accessModes": []string{"ReadWriteOnce"},
				"resources":   map[string]interface{}{"requests": map[string]interface{}{"storage": minioStorage}},
				"volumeMode":  "Filesystem",
			},
		}},
		{"Secret", map[string]interface{}{
			"apiVersion": "v1", "kind": "Secret",
			"metadata": map[string]interface{}{"name": "minio-secret", "namespace": minioNamespace},
			"stringData": map[string]interface{}{
				"minio_root_user":     minioUser,
				"minio_root_password": minioPass,
			},
		}},
		{"Deployment", map[string]interface{}{
			"apiVersion": "apps/v1", "kind": "Deployment",
			"metadata": map[string]interface{}{"name": "minio", "namespace": minioNamespace},
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
							"image": "quay.io/minio/minio:latest",
							"args":  []string{"server", "/data", "--console-address", ":9090"},
							"env": []map[string]interface{}{
								{"name": "MINIO_ROOT_USER", "valueFrom": map[string]interface{}{"secretKeyRef": map[string]interface{}{"name": "minio-secret", "key": "minio_root_user"}}},
								{"name": "MINIO_ROOT_PASSWORD", "valueFrom": map[string]interface{}{"secretKeyRef": map[string]interface{}{"name": "minio-secret", "key": "minio_root_password"}}},
							},
							"ports": []map[string]interface{}{
								{"containerPort": 9000, "protocol": "TCP"},
								{"containerPort": 9090, "protocol": "TCP"},
							},
							"volumeMounts": []map[string]interface{}{
								{"name": "data", "mountPath": "/data", "subPath": "minio"},
							},
							"resources": map[string]interface{}{
								"limits":   map[string]interface{}{"cpu": "250m", "memory": "1Gi"},
								"requests": map[string]interface{}{"cpu": "20m", "memory": "100Mi"},
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
			"metadata": map[string]interface{}{"name": minioServiceName, "namespace": minioNamespace},
			"spec": map[string]interface{}{
				"selector": map[string]interface{}{"app": "minio"},
				"type":     "ClusterIP",
				"ports": []map[string]interface{}{
					{"name": "api", "port": 9000, "targetPort": 9000},
					{"name": "ui", "port": 9090, "targetPort": 9090},
				},
			},
		}},
		{"API Route", map[string]interface{}{
			"apiVersion": "route.openshift.io/v1", "kind": "Route",
			"metadata": map[string]interface{}{"name": "minio-api", "namespace": minioNamespace},
			"spec": map[string]interface{}{
				"to":   map[string]interface{}{"kind": "Service", "name": minioServiceName, "weight": 100},
				"port": map[string]interface{}{"targetPort": "api"},
				"tls":  map[string]interface{}{"termination": "edge", "insecureEdgeTerminationPolicy": "Redirect"},
			},
		}},
		{"UI Route", map[string]interface{}{
			"apiVersion": "route.openshift.io/v1", "kind": "Route",
			"metadata": map[string]interface{}{"name": "minio-ui", "namespace": minioNamespace},
			"spec": map[string]interface{}{
				"to":   map[string]interface{}{"kind": "Service", "name": minioServiceName, "weight": 100},
				"port": map[string]interface{}{"targetPort": "ui"},
				"tls":  map[string]interface{}{"termination": "edge", "insecureEdgeTerminationPolicy": "Redirect"},
			},
		}},
	}

	for _, r := range resources {
		logs = append(logs, fmt.Sprintf("Applying %s...", r.name))
		data, _ := json.Marshal(r.obj)
		kind := r.obj["kind"].(string)
		meta := r.obj["metadata"].(map[string]interface{})
		name := meta["name"].(string)

		apiGroup := r.obj["apiVersion"].(string)
		var resourceType string
		switch kind {
		case "PersistentVolumeClaim":
			resourceType = "persistentvolumeclaims"
		case "Secret":
			resourceType = "secrets"
		case "Deployment":
			resourceType = "deployments"
		case "Service":
			resourceType = "services"
		case "Route":
			resourceType = "routes"
		}

		path := namespacedPath(apiGroup, resourceType, minioNamespace, name)

		// Try apply first (update existing), fall back to post (create new)
		_, _, applyErr := c.apply(path, r.obj)
		if applyErr != nil {
			// apply failed — try create
			collectionPath := namespacedPath(apiGroup, resourceType, minioNamespace, "")
			_, _, createErr := c.post(collectionPath, data)
			if createErr != nil && !IsK8sError(createErr, 409) {
				return &types.OperationResponse{
					Success: false, Message: fmt.Sprintf("Failed to create %s: %v", r.name, createErr),
					Logs: logs, ErrorCode: errorCodeFromK8sErr(createErr),
				}, nil
			}
		}
		logs = append(logs, fmt.Sprintf("OK: %s applied", r.name))
	}

	// Step 3: Quick readiness check (3 polls, 5s apart). The frontend auto-polls
	// /api/resources/status, so no need to block for the full startup time.
	logs = append(logs, "Checking if MinIO is ready...")
	ready := false
	for i := 0; i < 3; i++ {
		deployPath := namespacedPath("apps/v1", "deployments", minioNamespace, "minio")
		body, _, err := c.get(deployPath)
		if err == nil {
			var deploy struct {
				Status struct{ ReadyReplicas int `json:"readyReplicas"` } `json:"status"`
			}
			if json.Unmarshal(body, &deploy) == nil && deploy.Status.ReadyReplicas >= 1 {
				ready = true
				break
			}
		}
		select {
		case <-c.ctx.Done():
			return &types.OperationResponse{
				Success: false, Message: "Request cancelled while waiting for MinIO",
				Logs: logs,
			}, nil
		case <-time.After(5 * time.Second):
		}
	}
	if !ready {
		logs = append(logs, "MinIO not ready yet — the frontend will show status updates as it starts.")
		slog.Info("minio setup complete (not yet ready)", "user", getUser(c))
		RecordActivity(c, types.ActivityEntry{
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			User:      getUser(c),
			Action:    "setup-minio",
			Detail:    fmt.Sprintf("namespace=%s (deployed, awaiting ready)", minioNamespace),
			Success:   true,
		})
		return &types.OperationResponse{
			Success: true, Message: "MinIO deployed. It is still starting — refresh to check status.",
			Logs: logs,
		}, nil
	}
	logs = append(logs, "OK: MinIO is running")

	// Step 4: Create bucket via S3 API (try once, retry once if it fails)
	logs = append(logs, fmt.Sprintf("Creating bucket '%s'...", minioBucket))
	bucketErr := createMinioBucket(c, minioBucket)
	if bucketErr != nil {
		// One retry after a short pause
		select {
		case <-c.ctx.Done():
		case <-time.After(3 * time.Second):
			bucketErr = createMinioBucket(c, minioBucket)
		}
	}

	if bucketErr != nil {
		logs = append(logs, fmt.Sprintf("Bucket creation failed: %v — you can create it from the MinIO console.", bucketErr))
		slog.Warn("minio bucket creation failed", "error", bucketErr, "user", getUser(c))
		RecordActivity(c, types.ActivityEntry{
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			User:      getUser(c),
			Action:    "setup-minio",
			Detail:    fmt.Sprintf("namespace=%s bucket=%s (bucket failed)", minioNamespace, minioBucket),
			Success:   false,
		})
		return &types.OperationResponse{
			Success: false, Message: fmt.Sprintf("MinIO deployed but bucket creation failed: %v. You can create the bucket manually from the MinIO console.", bucketErr),
			Logs: logs,
		}, nil
	}

	logs = append(logs, fmt.Sprintf("OK: Bucket '%s' created", minioBucket))
	slog.Info("minio setup complete", "user", getUser(c))
	RecordActivity(c, types.ActivityEntry{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		User:      getUser(c),
		Action:    "setup-minio",
		Detail:    fmt.Sprintf("namespace=%s bucket=%s", minioNamespace, minioBucket),
		Success:   true,
	})

	return &types.OperationResponse{
		Success: true, Message: fmt.Sprintf("MinIO deployed with bucket '%s'. Console credentials are stored in secret 'minio-secret' in namespace '%s'.", minioBucket, minioNamespace),
		Logs: logs,
	}, nil
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

// TeardownMinIO deletes the MinIO namespace and all its resources.
func TeardownMinIO(c *Client) (*types.OperationResponse, error) {
	logs := []string{}

	// Check if any pipeline servers depend on MinIO
	dspaProjects := findDSPAProjects(c)
	if len(dspaProjects) > 0 {
		return &types.OperationResponse{
			Success: false, Message: fmt.Sprintf("Cannot tear down MinIO while %d pipeline server(s) are still running. Tear them down first.", len(dspaProjects)),
			Logs: logs, ErrorCode: "prerequisites",
		}, nil
	}

	logs = append(logs, "Deleting namespace 'minio'...")
	nsPath := "/api/v1/namespaces/" + minioNamespace
	_, delErr := c.delete(nsPath)
	if delErr != nil {
		if IsK8sError(delErr, 404) {
			logs = append(logs, "Namespace already absent")
		} else {
			return &types.OperationResponse{
				Success: false, Message: fmt.Sprintf("Failed to delete namespace: %v", delErr),
				Logs: logs, ErrorCode: errorCodeFromK8sErr(delErr),
			}, nil
		}
	} else {
		logs = append(logs, "OK: Namespace deleted (resources will be cleaned up)")
	}

	slog.Info("minio teardown", "user", getUser(c))
	RecordActivity(c, types.ActivityEntry{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		User:      getUser(c),
		Action:    "teardown-minio",
		Detail:    minioNamespace,
		Success:   true,
	})

	return &types.OperationResponse{
		Success: true, Message: "MinIO namespace deleted.",
		Logs: logs,
	}, nil
}
