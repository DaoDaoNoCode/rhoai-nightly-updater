package cluster

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const (
	defaultPipelineProject = "test-pipelines"
	dspaName               = "dspa"
	dspaSecretName         = "dashboard-dspa-secret"
	dspaAPIGroup           = "datasciencepipelinesapplications.opendatahub.io/v1"
)

func findDSPAProjects(c *Client) ([]string, error) {
	projects, err := GetDSProjects(c)
	if err != nil {
		return nil, fmt.Errorf("list projects for pipeline dependency check: %w", err)
	}
	var withDSPA []string
	for _, p := range projects {
		dspaPath := namespacedPath(dspaAPIGroup, "datasciencepipelinesapplications", p, dspaName)
		if _, _, err := c.get(dspaPath); err == nil {
			withDSPA = append(withDSPA, p)
		} else if !IsK8sError(err, 404) {
			return nil, fmt.Errorf("check pipeline server in %s: %w", p, err)
		}
	}
	return withDSPA, nil
}

func getPipelineServerStatus(c *Client, project string) types.ResourceState {
	state := types.ResourceState{Namespace: project}

	nsPath := "/api/v1/namespaces/" + project
	_, _, err := c.get(nsPath)
	if err != nil {
		state.Message = "Not deployed"
		return state
	}

	dspaPath := namespacedPath(dspaAPIGroup, "datasciencepipelinesapplications", project, dspaName)
	body, _, err := c.get(dspaPath)
	if err != nil {
		state.Message = "Project exists but no pipeline server"
		return state
	}
	state.Deployed = true

	var dspa struct {
		Status struct {
			Conditions []struct {
				Type    string `json:"type"`
				Status  string `json:"status"`
				Message string `json:"message"`
				Reason  string `json:"reason"`
			} `json:"conditions"`
		} `json:"status"`
	}
	if json.Unmarshal(body, &dspa) == nil {
		for _, cond := range dspa.Status.Conditions {
			if cond.Type == "Ready" {
				if cond.Status == "True" {
					state.Ready = true
					state.Message = "Running"
				} else if cond.Reason != "" {
					state.Message = cond.Reason
				} else {
					state.Message = "Not ready"
				}
				break
			}
		}
	}
	if state.Message == "" {
		state.Message = "Provisioning"
	}

	// Get dashboard URL for pipeline page link
	if state.Ready {
		routePath := namespacedPath("route.openshift.io/v1", "routes", dashboardNamespace, "rhods-dashboard")
		if routeBody, _, err := c.get(routePath); err == nil {
			var route struct {
				Spec struct {
					Host string `json:"host"`
				} `json:"spec"`
			}
			if json.Unmarshal(routeBody, &route) == nil && route.Spec.Host != "" {
				state.UIRoute = "https://" + route.Spec.Host + "/develop-train/pipelines/definitions/" + project
			}
		}
	}

	return state
}

// SetupPipelineServer creates a pipeline server (DSPA) in the given project.
func SetupPipelineServer(c *Client, project string) (*types.OperationResponse, error) {
	logs := []string{}

	// Check MinIO is ready
	minioStatus := getMinIOStatus(c)
	if !minioStatus.Ready {
		RecordActivity(c, types.ActivityEntry{
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			User:      getUser(c),
			Action:    "setup-pipeline-server",
			Detail:    fmt.Sprintf("project=%s (MinIO not ready)", project),
			Success:   false,
		})
		return &types.OperationResponse{
			Success: false, Message: "MinIO is not deployed or not ready. Set up MinIO first.",
			Logs: logs, ErrorCode: "prerequisites",
		}, nil
	}

	// Step 1: Create DS project namespace (if it doesn't exist)
	logs = append(logs, fmt.Sprintf("Creating project '%s'...", project))
	nsData, _ := json.Marshal(map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata": map[string]interface{}{
			"name": project,
			"labels": map[string]interface{}{
				"opendatahub.io/dashboard":    "true",
				"kubernetes.io/metadata.name": project,
				"modelmesh-enabled":           "true",
			},
			"annotations": map[string]interface{}{
				"openshift.io/description":  "Data Science project with pipeline server",
				"openshift.io/display-name": project,
			},
		},
	})
	_, status, err := c.post("/api/v1/namespaces", nsData)
	if err != nil && !IsK8sError(err, 409) {
		RecordActivity(c, types.ActivityEntry{
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			User:      getUser(c),
			Action:    "setup-pipeline-server",
			Detail:    fmt.Sprintf("project=%s (namespace creation failed)", project),
			Success:   false,
		})
		return &types.OperationResponse{
			Success: false, Message: fmt.Sprintf("Failed to create project: %v", err),
			Logs: logs, ErrorCode: errorCodeFromK8sErr(err),
		}, nil
	}
	if status == 409 || IsK8sError(err, 409) {
		// Check if namespace is Terminating
		nsGetPath := "/api/v1/namespaces/" + project
		nsBody, _, nsGetErr := c.get(nsGetPath)
		if nsGetErr == nil {
			var ns struct {
				Status struct {
					Phase string `json:"phase"`
				} `json:"status"`
			}
			if json.Unmarshal(nsBody, &ns) == nil && ns.Status.Phase == "Terminating" {
				return &types.OperationResponse{
					Success: false, Message: fmt.Sprintf("Namespace '%s' is terminating. Wait for it to fully delete before re-creating.", project),
					Logs: logs, ErrorCode: "terminating",
				}, nil
			}
		}
		logs = append(logs, "Project already exists — reusing")
	} else {
		logs = append(logs, "OK: Project created")
	}

	// Step 2: Read MinIO credentials
	logs = append(logs, "Reading MinIO credentials...")
	secretPath := namespacedPath("v1", "secrets", minioNamespace, "minio-secret")
	secretBody, _, err := c.get(secretPath)
	if err != nil {
		return &types.OperationResponse{
			Success: false, Message: fmt.Sprintf("Failed to read MinIO secret: %v", err),
			Logs: logs, ErrorCode: errorCodeFromK8sErr(err),
		}, nil
	}

	var minioSecret struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(secretBody, &minioSecret); err != nil {
		return &types.OperationResponse{
			Success: false, Message: "Failed to parse MinIO secret",
			Logs: logs,
		}, nil
	}

	accessKey := decodeBase64Field(minioSecret.Data["minio_root_user"])
	secretKey := decodeBase64Field(minioSecret.Data["minio_root_password"])
	if accessKey == "" || secretKey == "" {
		return &types.OperationResponse{
			Success: false, Message: "MinIO credentials are empty. Check that minio-secret has fields: minio_root_user, minio_root_password.",
			Logs: logs, ErrorCode: "prerequisites",
		}, nil
	}
	logs = append(logs, "OK: MinIO credentials read")

	// Step 3: Create DSPA secret
	logs = append(logs, "Creating pipeline server secret...")
	dspaSecret := map[string]interface{}{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]interface{}{
			"name":      dspaSecretName,
			"namespace": project,
			"labels":    map[string]interface{}{"opendatahub.io/dashboard": "true"},
		},
		"stringData": map[string]interface{}{
			"AWS_ACCESS_KEY_ID":     accessKey,
			"AWS_SECRET_ACCESS_KEY": secretKey,
		},
	}
	dspaSecretPath := namespacedPath("v1", "secrets", project, dspaSecretName)
	_, _, applyErr := c.apply(dspaSecretPath, dspaSecret)
	if applyErr != nil {
		data, _ := json.Marshal(dspaSecret)
		collectionPath := namespacedPath("v1", "secrets", project, "")
		_, _, createErr := c.post(collectionPath, data)
		if createErr != nil && !IsK8sError(createErr, 409) {
			return &types.OperationResponse{
				Success: false, Message: fmt.Sprintf("Failed to create DSPA secret: %v", createErr),
				Logs: logs, ErrorCode: errorCodeFromK8sErr(createErr),
			}, nil
		}
	}
	logs = append(logs, "OK: Secret created")

	// Step 4: Create DSPA
	logs = append(logs, "Creating pipeline server (DSPA)...")
	s3Host := fmt.Sprintf("%s.%s.svc:9000", minioServiceName, minioNamespace)
	dspa := map[string]interface{}{
		"apiVersion": "datasciencepipelinesapplications.opendatahub.io/v1",
		"kind":       "DataSciencePipelinesApplication",
		"metadata":   map[string]interface{}{"name": dspaName, "namespace": project},
		"spec": map[string]interface{}{
			"dspVersion": "v2",
			"objectStorage": map[string]interface{}{
				"externalStorage": map[string]interface{}{
					"host":   s3Host,
					"scheme": "http",
					"bucket": minioBucket,
					"region": "us-east-1",
					"s3CredentialsSecret": map[string]interface{}{
						"accessKey":  "AWS_ACCESS_KEY_ID",
						"secretKey":  "AWS_SECRET_ACCESS_KEY",
						"secretName": dspaSecretName,
					},
				},
			},
			"apiServer": map[string]interface{}{
				"enableSamplePipeline": false,
				"cacheEnabled":         true,
				"managedPipelines": map[string]interface{}{
					"instructLab": map[string]interface{}{"state": "Removed"},
				},
				"pipelineStore": "database",
			},
		},
	}
	dspaPath := namespacedPath("datasciencepipelinesapplications.opendatahub.io/v1", "datasciencepipelinesapplications", project, dspaName)
	_, _, applyErr = c.apply(dspaPath, dspa)
	if applyErr != nil {
		data, _ := json.Marshal(dspa)
		collectionPath := namespacedPath("datasciencepipelinesapplications.opendatahub.io/v1", "datasciencepipelinesapplications", project, "")
		_, _, createErr := c.post(collectionPath, data)
		if createErr != nil && !IsK8sError(createErr, 409) {
			return &types.OperationResponse{
				Success: false, Message: fmt.Sprintf("Failed to create DSPA: %v", createErr),
				Logs: logs, ErrorCode: errorCodeFromK8sErr(createErr),
			}, nil
		}
	}
	logs = append(logs, "OK: Pipeline server created")
	logs = append(logs, "The pipeline server will take 1-3 minutes to become ready.")

	slog.Info("pipeline server setup", "project", project, "user", getUser(c))
	RecordActivity(c, types.ActivityEntry{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		User:      getUser(c),
		Action:    "setup-pipeline-server",
		Detail:    fmt.Sprintf("project=%s dspa=%s", project, dspaName),
		Success:   true,
	})

	return &types.OperationResponse{
		Success: true, Message: "Pipeline server created. It will take 1-3 minutes to become ready.",
		Logs: logs,
	}, nil
}

// TeardownPipelineServer removes the DSPA and secret from the given project.
func TeardownPipelineServer(c *Client, project string) (*types.OperationResponse, error) {
	logs := []string{}

	// Delete DSPA
	logs = append(logs, "Deleting pipeline server...")
	dspaPath := namespacedPath("datasciencepipelinesapplications.opendatahub.io/v1", "datasciencepipelinesapplications", project, dspaName)
	_, delErr := c.delete(dspaPath)
	if delErr != nil && !IsK8sError(delErr, 404) {
		RecordActivity(c, types.ActivityEntry{
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			User:      getUser(c),
			Action:    "teardown-pipeline-server",
			Detail:    fmt.Sprintf("project=%s (DSPA delete failed)", project),
			Success:   false,
		})
		return &types.OperationResponse{
			Success: false, Message: fmt.Sprintf("Failed to delete DSPA: %v", delErr),
			Logs: logs, ErrorCode: errorCodeFromK8sErr(delErr),
		}, nil
	}
	logs = append(logs, "OK: DSPA deleted")

	// Delete secret
	logs = append(logs, "Deleting pipeline secret...")
	secretPath := namespacedPath("v1", "secrets", project, dspaSecretName)
	_, delErr = c.delete(secretPath)
	if delErr != nil && !IsK8sError(delErr, 404) {
		logs = append(logs, fmt.Sprintf("Warning: failed to delete secret: %v", delErr))
	} else {
		logs = append(logs, "OK: Secret deleted")
	}

	slog.Info("pipeline server teardown", "project", project, "user", getUser(c))
	RecordActivity(c, types.ActivityEntry{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		User:      getUser(c),
		Action:    "teardown-pipeline-server",
		Detail:    project,
		Success:   true,
	})

	return &types.OperationResponse{
		Success: true, Message: "Pipeline server removed from '" + project + "'.",
		Logs: logs,
	}, nil
}
