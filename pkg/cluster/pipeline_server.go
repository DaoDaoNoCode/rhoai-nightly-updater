package cluster

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const (
	// dspaName and dspaSecretName are unique to this tool. odh-dashboard
	// creates its pipeline server as "dspa" with secret "dashboard-dspa-secret"
	// (frontend/src/concepts/pipelines/const.ts DEFAULT_PIPELINE_DEFINITION_NAME
	// and configurePipelinesServer/const.ts DSPA_SECRET_NAME), which older
	// versions of this tool also used. The dashboard finds a project's
	// pipeline server by listing DSPAs, not by name
	// (concepts/pipelines/context/usePipelineNamespaceCR.ts), so the
	// pipelines UI still works with this name.
	dspaName       = "nightly-dspa"
	dspaSecretName = "nightly-dspa-s3"
	// Names used by older versions; see dspaInfo.managedByTool.
	legacyDSPAName       = "dspa"
	legacyDSPASecretName = "dashboard-dspa-secret"
	dspaAPIGroup         = "datasciencepipelinesapplications.opendatahub.io/v1"
	// dspaFinalizer is removed by the data-science-pipelines-operator once
	// it has cleaned up (DSPO controllers/dspipeline_controller.go, rhoai-3.6).
	dspaFinalizer = "datasciencepipelinesapplications.opendatahub.io/finalizer"
	// dspoSelector selects the data-science-pipelines-operator Deployment
	// in the applications namespace (live RHOAI 3.6 label).
	dspoSelector = "app.kubernetes.io/name=data-science-pipelines-operator"
	// dspaStuckAfter matches odh-dashboard's SERVER_TIMEOUT (5 minutes,
	// frontend/src/utilities/const.ts), after which the dashboard reports a
	// pipeline server that is still not ready as having issues.
	dspaStuckAfter = 5 * time.Minute
)

var (
	// PipelineServerDeleteTimeout bounds how long teardown waits for the
	// DSPA finalizer to complete. Tests shorten it.
	PipelineServerDeleteTimeout = 60 * time.Second
	PipelineServerDeletePoll    = 2 * time.Second

	errDSPACRDMissing = errors.New("the DataSciencePipelinesApplication CRD is not installed")
)

// minioS3Host is the in-cluster S3 endpoint of the tool's MinIO.
func minioS3Host() string {
	return fmt.Sprintf("%s.%s.svc:9000", minioServiceName, minioNamespace)
}

// dspaInfo is the part of a DataSciencePipelinesApplication the tool reads.
type dspaInfo struct {
	Meta       objectMeta
	Host       string
	SecretName string
	ExternalDB bool
	Ready      *dspaCondition
}

type dspaCondition struct {
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

// managedByTool reports whether this tool created the DSPA. Current versions
// label it. Older versions named it "dspa", pointed it at the tool's MinIO
// with secret "dashboard-dspa-secret", and created it either by server-side
// apply (fieldManager rhoai-nightly-updater) or, when that apply failed, by a
// POST without User-Agent (manager "Go-http-client"). odh-dashboard creates
// its "dspa" from the browser, so neither manager matches it.
func (d dspaInfo) managedByTool() bool {
	if d.Meta.hasToolLabel() {
		return true
	}
	return d.Meta.Name == legacyDSPAName &&
		d.Host == minioS3Host() &&
		d.SecretName == legacyDSPASecretName &&
		(d.Meta.createdByToolApply() || d.Meta.createdBy(legacyPostManager, "Update"))
}

func (d dspaInfo) hasFinalizer() bool {
	for _, f := range d.Meta.Finalizers {
		if f == dspaFinalizer {
			return true
		}
	}
	return false
}

// toolDSPAPath returns the path of a pipeline server only for the names this
// tool creates (or created in older versions), so teardown can never reach
// another DSPA, and the RBAC rule stays limited to these names.
func toolDSPAPath(project, name string) (string, bool) {
	switch name {
	case dspaName:
		return namespacedPath(dspaAPIGroup, "datasciencepipelinesapplications", project, dspaName), true
	case legacyDSPAName:
		return namespacedPath(dspaAPIGroup, "datasciencepipelinesapplications", project, legacyDSPAName), true
	}
	return "", false
}

// toolDSPASecretPath is toolDSPAPath for the pipeline server's S3 secret.
func toolDSPASecretPath(project, name string) (string, bool) {
	switch name {
	case dspaSecretName:
		return namespacedPath("v1", "secrets", project, dspaSecretName), true
	case legacyDSPASecretName:
		return namespacedPath("v1", "secrets", project, legacyDSPASecretName), true
	}
	return "", false
}

func dspaCollectionPath(namespace string) string {
	if namespace == "" {
		return clusterPath(dspaAPIGroup, "datasciencepipelinesapplications", "")
	}
	return namespacedPath(dspaAPIGroup, "datasciencepipelinesapplications", namespace, "")
}

// listDSPAs lists pipeline servers in namespace, or cluster-wide when
// namespace is "". A missing CRD returns errDSPACRDMissing.
func listDSPAs(c *Client, namespace string) ([]dspaInfo, error) {
	body, _, err := c.get(dspaCollectionPath(namespace))
	if err != nil {
		if IsK8sError(err, 404) {
			return nil, errDSPACRDMissing
		}
		return nil, fmt.Errorf("list pipeline servers: %w", err)
	}
	var list struct {
		Items []struct {
			Metadata objectMeta `json:"metadata"`
			Spec     struct {
				ObjectStorage struct {
					ExternalStorage *struct {
						Host                string `json:"host"`
						S3CredentialsSecret struct {
							SecretName string `json:"secretName"`
						} `json:"s3CredentialsSecret"`
					} `json:"externalStorage"`
				} `json:"objectStorage"`
				Database struct {
					ExternalDB *json.RawMessage `json:"externalDB"`
				} `json:"database"`
			} `json:"spec"`
			Status struct {
				Conditions []struct {
					Type string `json:"type"`
					dspaCondition
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("parse pipeline server list: %w", err)
	}
	out := make([]dspaInfo, 0, len(list.Items))
	for _, item := range list.Items {
		d := dspaInfo{Meta: item.Metadata, ExternalDB: item.Spec.Database.ExternalDB != nil}
		if es := item.Spec.ObjectStorage.ExternalStorage; es != nil {
			d.Host = es.Host
			d.SecretName = es.S3CredentialsSecret.SecretName
		}
		for _, cond := range item.Status.Conditions {
			if cond.Type == "Ready" {
				cc := cond.dspaCondition
				d.Ready = &cc
			}
		}
		out = append(out, d)
	}
	return out, nil
}

// minioEndpoints are the addresses of the tool's MinIO besides its service
// DNS names: the hosts of its Routes and the ClusterIP of minio-service.
type minioEndpoints struct {
	apiHost   string // Route minio-api (earlier versions)
	uiHost    string // Route minio-ui
	clusterIP string
}

// readMinIOEndpoints reads the Route hosts and the service ClusterIP. Missing
// or unreadable objects are skipped: the DNS names still match.
func readMinIOEndpoints(c *Client) minioEndpoints {
	e := minioEndpoints{
		apiHost: routeHost(c, minioNamespace, "minio-api"),
		uiHost:  routeHost(c, minioNamespace, "minio-ui"),
	}
	if body, _, err := c.get(namespacedPath("v1", "services", minioNamespace, minioServiceName)); err == nil {
		var svc struct {
			Spec struct {
				ClusterIP string `json:"clusterIP"`
			} `json:"spec"`
		}
		if json.Unmarshal(body, &svc) == nil && svc.Spec.ClusterIP != "None" {
			e.clusterIP = svc.Spec.ClusterIP
		}
	}
	return e
}

// endpointHostname returns the lower-case host name of an S3 endpoint given
// as "host", "host:port" or "scheme://host[:port][/path]".
func endpointHostname(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if !strings.Contains(s, "://") {
		s = "//" + s
	}
	if u, err := url.Parse(s); err == nil {
		return strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	}
	// Unparseable (for example a bad port): cut the scheme, path and port by
	// hand so a typo cannot hide a dependency.
	s = s[strings.Index(s, "//")+2:]
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	if i := strings.LastIndex(s, "@"); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.Index(s, ":"); i >= 0 && !strings.HasPrefix(s, "[") {
		s = s[:i]
	}
	return strings.TrimSuffix(strings.ToLower(strings.Trim(s, "[]")), ".")
}

// usesMinIO reports whether a pipeline server in namespace dspaNamespace with
// object-storage host host talks to the tool's MinIO, whatever form the
// address takes: the service DNS name in any of its forms
// (https://kubernetes.io/docs/concepts/services-networking/dns-pod-service/#namespaces-of-services),
// with or without scheme and port, the ClusterIP, or a Route host.
func (e minioEndpoints) usesMinIO(dspaNamespace, host string) bool {
	h := endpointHostname(host)
	if h == "" {
		return false
	}
	svc := minioServiceName + "." + minioNamespace
	switch h {
	case svc, svc + ".svc", svc + ".svc.cluster.local":
		return true
	case minioServiceName:
		// A bare service name resolves in the client's own namespace: the
		// pod DNS search path starts with <namespace>.svc.cluster.local.
		return dspaNamespace == minioNamespace
	}
	if e.clusterIP != "" && h == e.clusterIP {
		return true
	}
	for _, r := range []string{e.apiHost, e.uiHost} {
		if r != "" && h == strings.TrimSuffix(strings.ToLower(r), ".") {
			return true
		}
	}
	return false
}

// minioTeardownBlocker explains why MinIO cannot be torn down because of
// pipeline servers, or returns "". Any DSPA that stores artifacts in the
// tool's MinIO (see usesMinIO), or lives in the minio namespace, blocks it,
// whoever created it. Terminating ones block too: only a running
// data-science-pipelines operator removes their finalizer (DSPO
// dspipeline_controller.go), so MinIO waits until they are really gone
// (teardown order DSPA, then MinIO).
func minioTeardownBlocker(dspas []dspaInfo, endpoints minioEndpoints) string {
	var live, deleting []string
	for _, d := range dspas {
		if !endpoints.usesMinIO(d.Meta.Namespace, d.Host) && d.Meta.Namespace != minioNamespace {
			continue
		}
		name := d.Meta.Namespace + "/" + d.Meta.Name
		if d.Meta.terminating() {
			deleting = append(deleting, name)
		} else {
			live = append(live, name)
		}
	}
	sort.Strings(live)
	sort.Strings(deleting)
	var parts []string
	if len(live) > 0 {
		parts = append(parts, fmt.Sprintf("%d pipeline server(s) use this MinIO: %s. Tear them down first.", len(live), strings.Join(live, ", ")))
	}
	if len(deleting) > 0 {
		parts = append(parts, fmt.Sprintf("%d pipeline server(s) are still being deleted: %s. Wait until they are gone; the data-science-pipelines operator must be running to finish their cleanup.", len(deleting), strings.Join(deleting, ", ")))
	}
	return strings.Join(parts, " ")
}

// pipelineServerState converts a tool-owned DSPA into its status entry.
func pipelineServerState(d dspaInfo, dashboardHost string) types.ResourceState {
	state := types.ResourceState{
		Namespace:     d.Meta.Namespace,
		Name:          d.Meta.Name,
		Deployed:      true,
		ManagedByTool: true,
		Terminating:   d.Meta.terminating(),
	}
	if !d.ExternalDB {
		// DSPO's MariaDB PVC is named mariadb-<dspa> and owned by the DSPA
		// (config/internal/mariadb/default/pvc.yaml.tmpl), so it is
		// garbage-collected with it.
		state.DataPVCs = []string{"mariadb-" + d.Meta.Name}
	}
	switch {
	case state.Terminating:
		state.Message = "Terminating"
		if d.hasFinalizer() {
			state.Message = "Terminating (waiting for the data-science-pipelines operator to finish cleanup)"
		}
	case d.Ready == nil:
		state.Message = "Provisioning"
	case d.Ready.Status == "True":
		state.Ready = true
		state.Message = "Running"
		if dashboardHost != "" {
			state.UIRoute = "https://" + dashboardHost + "/develop-train/pipelines/definitions/" + d.Meta.Namespace
		}
	default:
		// The Ready reason is often a success-sounding word such as
		// "MinimumReplicasAvailable"; the message carries the cause.
		state.Message = firstNonEmpty(truncateMessage(d.Ready.Message), d.Ready.Reason, "Not ready")
	}
	if !state.Ready && !state.Terminating {
		if created, err := time.Parse(time.RFC3339, d.Meta.CreationTimestamp); err == nil && time.Since(created) > dspaStuckAfter {
			state.TerminalError = true
		}
	}
	return state
}

func recordPipelineActivity(c *Client, action, detail string, success bool) {
	RecordActivity(c, types.ActivityEntry{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		User:      getUser(c),
		Action:    action,
		Detail:    detail,
		Success:   success,
	})
}

// SetupPipelineServer creates a pipeline server (DSPA) in the given project.
func SetupPipelineServer(c *Client, project string) (*types.OperationResponse, error) {
	logs := []string{}
	fail := func(msg, code, activity string) (*types.OperationResponse, error) {
		recordPipelineActivity(c, "setup-pipeline-server", fmt.Sprintf("project=%s (%s)", project, activity), false)
		return &types.OperationResponse{Success: false, Message: msg, Logs: logs, ErrorCode: code}, nil
	}

	minio := getMinIOStatus(c)
	if !minio.Deployed || !minio.ManagedByTool || !minio.Ready {
		return fail("MinIO is not deployed or not ready. Set up MinIO first.", "prerequisites", "MinIO not ready")
	}

	// Never add a second pipeline server next to one this tool did not
	// create: odh-dashboard shows only the first DSPA of a project.
	existing, err := listDSPAs(c, project)
	switch {
	case errors.Is(err, errDSPACRDMissing):
		return fail("Data Science Pipelines is not installed (no DataSciencePipelinesApplication CRD). Set the aipipelines component to Managed in the DataScienceCluster first.", "prerequisites", "DSPA CRD missing")
	case err != nil:
		return fail(fmt.Sprintf("Cannot check for existing pipeline servers: %v", err), errorCodeFromK8sErr(err), "list failed")
	}
	var dspaMeta objectMeta
	dspaFound := false
	for _, d := range existing {
		if d.Meta.Name == dspaName && d.Meta.hasToolLabel() {
			if d.Meta.terminating() {
				return fail(fmt.Sprintf("The pipeline server in '%s' is still being deleted. Wait for it to finish, then retry.", project), "terminating", "terminating")
			}
			dspaMeta, dspaFound = d.Meta, true
			continue
		}
		if d.managedByTool() {
			return fail(fmt.Sprintf("Project '%s' already has a pipeline server ('%s') created by an earlier version of this tool. Tear it down first to recreate it.", project, d.Meta.Name), "validation", "legacy pipeline server exists")
		}
		return fail(fmt.Sprintf("Project '%s' already has a pipeline server ('%s') that this tool did not create. It was left unchanged.", project, d.Meta.Name), "not_managed", "unmanaged pipeline server exists")
	}

	// Step 1: Create DS project namespace (if it doesn't exist)
	logs = append(logs, fmt.Sprintf("Creating project '%s'...", project))
	nsData, _ := json.Marshal(map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata": map[string]interface{}{
			"name": project,
			"labels": map[string]interface{}{
				"opendatahub.io/dashboard": "true",
				// RHOAI 3.x serves models with KServe only. odh-dashboard
				// requires modelmesh-enabled=false for KServe projects
				// (packages/kserve/extensions.ts, rhoai-3.6 branch).
				"modelmesh-enabled": "false",
			},
			"annotations": map[string]interface{}{
				"openshift.io/description":  "Data Science project with pipeline server",
				"openshift.io/display-name": project,
			},
		},
	})
	_, _, err = c.post(withToolFieldManager("/api/v1/namespaces"), nsData)
	if err != nil && !IsK8sError(err, 409) {
		return fail(fmt.Sprintf("Failed to create project: %v", err), errorCodeFromK8sErr(err), "namespace creation failed")
	}
	if IsK8sError(err, 409) {
		nsBody, _, nsGetErr := c.get("/api/v1/namespaces/" + project)
		if nsGetErr != nil {
			return fail(fmt.Sprintf("Cannot read project '%s': %v", project, nsGetErr), errorCodeFromK8sErr(nsGetErr), "namespace read failed")
		}
		var ns struct {
			Metadata objectMeta `json:"metadata"`
			Status   struct {
				Phase string `json:"phase"`
			} `json:"status"`
		}
		if json.Unmarshal(nsBody, &ns) == nil && (ns.Metadata.terminating() || ns.Status.Phase == "Terminating") {
			return fail(fmt.Sprintf("Namespace '%s' is terminating. Wait for it to fully delete before re-creating.", project), "terminating", "namespace terminating")
		}
		logs = append(logs, "Project already exists — reusing")
	} else {
		logs = append(logs, "OK: Project created")
	}

	// Step 2: Read MinIO credentials
	logs = append(logs, "Reading MinIO credentials...")
	secretBody, _, err := c.get(namespacedPath("v1", "secrets", minioNamespace, "minio-secret"))
	if err != nil {
		return fail(fmt.Sprintf("Failed to read MinIO secret: %v", err), errorCodeFromK8sErr(err), "MinIO secret read failed")
	}
	var minioSecret struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(secretBody, &minioSecret); err != nil {
		return fail("Failed to parse MinIO secret", "", "MinIO secret parse failed")
	}
	accessKey := decodeBase64Field(minioSecret.Data["minio_root_user"])
	secretKey := decodeBase64Field(minioSecret.Data["minio_root_password"])
	if accessKey == "" || secretKey == "" {
		return fail("MinIO credentials are empty. Check that minio-secret has fields: minio_root_user, minio_root_password.", "prerequisites", "MinIO credentials empty")
	}
	logs = append(logs, "OK: MinIO credentials read")

	// Step 3: Create the DSPA credentials secret, without taking over a
	// secret of the same name that someone else created.
	logs = append(logs, "Creating pipeline server secret...")
	dspaSecretPath := namespacedPath("v1", "secrets", project, dspaSecretName)
	readSecret := func() (objectMeta, bool, error) {
		body, _, getErr := c.get(dspaSecretPath)
		if IsK8sError(getErr, 404) {
			return objectMeta{}, false, nil
		}
		if getErr != nil {
			return objectMeta{}, false, getErr
		}
		meta, perr := parseObjectMeta(body)
		return meta, true, perr
	}
	secretMeta, secretFound, err := readSecret()
	if err != nil {
		return fail(fmt.Sprintf("Cannot read secret '%s': %v", dspaSecretName, err), errorCodeFromK8sErr(err), "secret read failed")
	}
	if secretFound && !secretMeta.hasToolLabel() {
		return fail(fmt.Sprintf("Secret '%s' already exists in '%s' and was not created by this tool. It was left unchanged.", dspaSecretName, project), "not_managed", "secret exists")
	}
	dspaSecret := map[string]interface{}{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]interface{}{
			"name":      dspaSecretName,
			"namespace": project,
			"labels":    toolLabels(),
		},
		"stringData": map[string]interface{}{
			"AWS_ACCESS_KEY_ID":     accessKey,
			"AWS_SECRET_ACCESS_KEY": secretKey,
		},
	}
	// POST when absent, resourceVersion-guarded apply when owned, so a
	// secret someone creates after the check above is never overwritten.
	secretAction, err := ensureToolObject(c, fmt.Sprintf("Secret %s/%s", project, dspaSecretName), dspaSecretPath,
		withToolFieldManager(namespacedPath("v1", "secrets", project, "")), dspaSecret, secretMeta, secretFound, readSecret, objectMeta.hasToolLabel)
	if err != nil {
		msg, code := pipelineWriteFailure(fmt.Sprintf("secret '%s'", dspaSecretName), project, err)
		return fail(msg, code, "secret write failed")
	}
	logs = append(logs, "OK: Secret "+secretAction)

	// Step 4: Create DSPA. fieldValidation=Strict (and server-side apply,
	// which always validates strictly) rejects fields the CRD does not
	// declare, so a manifest that drifts from the CRD fails here instead of
	// being silently pruned.
	logs = append(logs, "Creating pipeline server (DSPA)...")
	dspa := map[string]interface{}{
		"apiVersion": dspaAPIGroup,
		"kind":       "DataSciencePipelinesApplication",
		"metadata": map[string]interface{}{
			"name":      dspaName,
			"namespace": project,
			"labels":    toolLabels(),
		},
		"spec": map[string]interface{}{
			"dspVersion": "v2",
			"objectStorage": map[string]interface{}{
				"externalStorage": map[string]interface{}{
					"host":   minioS3Host(),
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
				"pipelineStore":        "database",
			},
		},
	}
	dspaPath := namespacedPath(dspaAPIGroup, "datasciencepipelinesapplications", project, dspaName)
	readDSPA := func() (objectMeta, bool, error) {
		body, _, getErr := c.get(dspaPath)
		if IsK8sError(getErr, 404) {
			return objectMeta{}, false, nil
		}
		if getErr != nil {
			return objectMeta{}, false, getErr
		}
		meta, perr := parseObjectMeta(body)
		return meta, true, perr
	}
	dspaAction, err := ensureToolObject(c, fmt.Sprintf("DataSciencePipelinesApplication %s/%s", project, dspaName), dspaPath,
		withToolFieldManager(namespacedPath(dspaAPIGroup, "datasciencepipelinesapplications", project, ""))+"&fieldValidation=Strict",
		dspa, dspaMeta, dspaFound, readDSPA, objectMeta.hasToolLabel)
	if err != nil {
		msg, code := pipelineWriteFailure(fmt.Sprintf("pipeline server '%s'", dspaName), project, err)
		return fail(fmt.Sprintf("%s The credentials secret '%s' is in place; re-run setup, or tear down to clean up.", msg, dspaSecretName), code, "DSPA write failed")
	}
	logs = append(logs, "OK: Pipeline server "+dspaAction)
	logs = append(logs, "The pipeline server will take 1-3 minutes to become ready.")

	slog.Info("pipeline server setup", "project", project, "user", getUser(c))
	recordPipelineActivity(c, "setup-pipeline-server", fmt.Sprintf("project=%s dspa=%s", project, dspaName), true)

	return &types.OperationResponse{
		Success: true, Message: "Pipeline server created. It will take 1-3 minutes to become ready.",
		Logs: logs,
	}, nil
}

// dspoRunning reports whether a data-science-pipelines-operator replica is
// ready, so a DSPA finalizer will be processed.
func dspoRunning(c *Client) (bool, error) {
	body, _, err := c.get(namespacedPath("apps/v1", "deployments", dashboardNamespace, "") + "?labelSelector=" + url.QueryEscape(dspoSelector))
	if err != nil {
		return false, err
	}
	var list struct {
		Items []struct {
			Status struct {
				ReadyReplicas int `json:"readyReplicas"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return false, err
	}
	for _, d := range list.Items {
		if d.Status.ReadyReplicas > 0 {
			return true, nil
		}
	}
	return false, nil
}

// TeardownPipelineServer removes the tool-created DSPA and its credentials
// secret from the given project. It is safe to re-run: a DSPA that is already
// terminating is not deleted again, and missing objects count as removed.
func TeardownPipelineServer(c *Client, project string) (*types.OperationResponse, error) {
	logs := []string{}
	fail := func(msg, code, activity string) (*types.OperationResponse, error) {
		recordPipelineActivity(c, "teardown-pipeline-server", fmt.Sprintf("project=%s (%s)", project, activity), false)
		return &types.OperationResponse{Success: false, Message: msg, Logs: logs, ErrorCode: code}, nil
	}

	all, err := listDSPAs(c, project)
	if errors.Is(err, errDSPACRDMissing) {
		all, err = nil, nil
	}
	if err != nil {
		return fail(fmt.Sprintf("Cannot list pipeline servers in '%s'; nothing was deleted: %v", project, err), errorCodeFromK8sErr(err), "list failed")
	}
	var target *dspaInfo
	var unmanaged []string
	for i := range all {
		if all[i].managedByTool() {
			if target == nil {
				target = &all[i]
			}
		} else {
			unmanaged = append(unmanaged, all[i].Meta.Name)
		}
	}
	if target == nil {
		if len(unmanaged) > 0 {
			return fail(fmt.Sprintf("The pipeline server in '%s' (%s) was not created by this tool, so it was not deleted.", project, strings.Join(unmanaged, ", ")), "not_managed", "not managed")
		}
		logs = append(logs, "No pipeline server created by this tool in this project")
		// A previous run may have removed the DSPA but not its secret.
		orphan := &dspaInfo{Meta: objectMeta{UID: "none"}, SecretName: dspaSecretName}
		if note := deleteDSPASecret(c, project, orphan, all, &logs); note != "" {
			return fail(fmt.Sprintf("No pipeline server created by this tool remains in '%s'.%s", project, note), "partial_failure", "orphan secret")
		}
		return &types.OperationResponse{Success: true, Message: fmt.Sprintf("No pipeline server created by this tool remains in '%s'.", project), Logs: logs}, nil
	}

	dspaPath, ok := toolDSPAPath(project, target.Meta.Name)
	if !ok {
		return fail(fmt.Sprintf("Pipeline server '%s' does not have a name this tool creates, so it was not deleted.", target.Meta.Name), "not_managed", "unexpected DSPA name")
	}
	if target.Meta.terminating() {
		logs = append(logs, fmt.Sprintf("Pipeline server '%s' is already being deleted", target.Meta.Name))
	} else {
		// The DSPA finalizer is removed only by the running operator. Deleting
		// while it is down would leave the DSPA stuck in Terminating.
		if target.hasFinalizer() {
			running, opErr := dspoRunning(c)
			if opErr != nil {
				return fail(fmt.Sprintf("Cannot verify that the data-science-pipelines operator is running; nothing was deleted: %v", opErr), errorCodeFromK8sErr(opErr), "operator check failed")
			}
			if !running {
				return fail("The data-science-pipelines operator is not running, so the pipeline server's finalizer could not complete and it would stay in Terminating. Nothing was deleted. Make sure the aipipelines component is Managed and its operator is ready, then retry.", "prerequisites", "operator not running")
			}
		}
		logs = append(logs, fmt.Sprintf("Deleting pipeline server '%s'...", target.Meta.Name))
		if _, delErr := deleteWithUID(c, dspaPath, target.Meta.UID); delErr != nil && !IsK8sError(delErr, 404) {
			code := errorCodeFromK8sErr(delErr)
			if IsK8sError(delErr, 409) {
				code = "conflict"
			}
			return fail(fmt.Sprintf("Failed to delete pipeline server: %v", delErr), code, "DSPA delete failed")
		}
		logs = append(logs, "OK: Deletion requested")
	}

	// The DSPA finalizer does not read the credentials secret, so it can go
	// now; doing it before waiting keeps a timed-out teardown from leaving the
	// secret behind with no pipeline server left to show a Tear down button.
	secretNote := deleteDSPASecret(c, project, target, all, &logs)

	gone, waitErr := waitForDeletion(c, dspaPath, PipelineServerDeleteTimeout, PipelineServerDeletePoll)
	if waitErr != nil {
		recordPipelineActivity(c, "teardown-pipeline-server", fmt.Sprintf("project=%s (cannot confirm deletion: %v)", project, waitErr), false)
		return &types.OperationResponse{
			Success:   false,
			Message:   fmt.Sprintf("Deletion of pipeline server '%s' was requested, but whether it is gone cannot be checked: %v.%s", target.Meta.Name, waitErr, secretNote),
			Logs:      logs,
			ErrorCode: firstNonEmpty(errorCodeFromK8sErr(waitErr), "in_progress"),
		}, nil
	}
	if !gone {
		recordPipelineActivity(c, "teardown-pipeline-server", fmt.Sprintf("project=%s (still terminating)", project), false)
		return &types.OperationResponse{
			Success:   false,
			Message:   fmt.Sprintf("Deletion of pipeline server '%s' was requested, but it is still terminating after %s (waiting for the data-science-pipelines operator to finish cleanup). The status shows it until it is gone.%s", target.Meta.Name, PipelineServerDeleteTimeout, secretNote),
			Logs:      logs,
			ErrorCode: "in_progress",
		}, nil
	}
	logs = append(logs, "OK: Pipeline server deleted")

	slog.Info("pipeline server teardown", "project", project, "dspa", target.Meta.Name, "user", getUser(c))
	recordPipelineActivity(c, "teardown-pipeline-server", fmt.Sprintf("project=%s dspa=%s", project, target.Meta.Name), secretNote == "")

	msg := fmt.Sprintf("Pipeline server removed from '%s'.", project)
	if !target.ExternalDB {
		msg += fmt.Sprintf(" Its database PVC 'mariadb-%s' (pipeline runs and experiments) was deleted with it.", target.Meta.Name)
	}
	if secretNote != "" {
		return &types.OperationResponse{Success: false, Message: msg + secretNote, Logs: logs, ErrorCode: "partial_failure"}, nil
	}
	return &types.OperationResponse{Success: true, Message: msg, Logs: logs}, nil
}

// deleteDSPASecret deletes the DSPA's credentials secret when the tool created
// it and no other live pipeline server in the project uses it. It returns a
// note for the response when the secret had to be left behind on an error.
func deleteDSPASecret(c *Client, project string, target *dspaInfo, all []dspaInfo, logs *[]string) string {
	name := target.SecretName
	if name == "" {
		return ""
	}
	for _, d := range all {
		if d.Meta.UID != target.Meta.UID && !d.Meta.terminating() && d.SecretName == name {
			*logs = append(*logs, fmt.Sprintf("Kept secret '%s': pipeline server '%s' also uses it", name, d.Meta.Name))
			return ""
		}
	}
	path, ok := toolDSPASecretPath(project, name)
	if !ok {
		*logs = append(*logs, fmt.Sprintf("Kept secret '%s': this tool never creates a secret with that name", name))
		return ""
	}
	body, _, err := c.get(path)
	if IsK8sError(err, 404) {
		*logs = append(*logs, "OK: Secret already absent")
		return ""
	}
	if err != nil {
		*logs = append(*logs, fmt.Sprintf("Warning: cannot read secret '%s': %v", name, err))
		return fmt.Sprintf(" The credentials secret '%s' could not be checked and was left in place: %v.", name, err)
	}
	meta, err := parseObjectMeta(body)
	if err != nil {
		return fmt.Sprintf(" The credentials secret '%s' could not be parsed and was left in place.", name)
	}
	if !meta.hasToolLabel() && !meta.createdByToolApply() && !meta.createdBy(legacyPostManager, "Update") {
		*logs = append(*logs, fmt.Sprintf("Kept secret '%s': it was not created by this tool", name))
		return ""
	}
	*logs = append(*logs, fmt.Sprintf("Deleting secret '%s'...", name))
	if _, err := deleteWithUID(c, path, meta.UID); err != nil && !IsK8sError(err, 404) {
		*logs = append(*logs, fmt.Sprintf("Warning: failed to delete secret: %v", err))
		return fmt.Sprintf(" The credentials secret '%s' could not be deleted: %v. Re-run teardown or delete it manually.", name, err)
	}
	*logs = append(*logs, "OK: Secret deleted")
	return ""
}

// pipelineWriteFailure turns an ensureToolObject error into a message and an
// error code.
func pipelineWriteFailure(what, project string, err error) (string, string) {
	var foreign *foreignObjectError
	switch {
	case errors.As(err, &foreign):
		return fmt.Sprintf("A %s was created in '%s' by someone else while setup was running. It was left unchanged.", what, project), "not_managed"
	case errors.Is(err, errObjectChanged):
		return fmt.Sprintf("The %s in '%s' kept changing while setup was updating it, so it was not changed. Re-run setup.", what, project), "conflict"
	case IsK8sError(err, 409):
		return fmt.Sprintf("The %s in '%s' changed while setup was updating it, so it was not changed. Re-run setup.", what, project), "conflict"
	}
	return fmt.Sprintf("Failed to create or update the %s in '%s': %v.", what, project, err), errorCodeFromK8sErr(err)
}
