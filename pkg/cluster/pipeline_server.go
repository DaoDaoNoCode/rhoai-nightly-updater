package cluster

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
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
	// pipeline server whose API server is still not ready as having issues.
	dspaStuckAfter = 5 * time.Minute
	// dspaAPIServerCondition is DSPO's APIServerReady condition. The
	// dashboard's pipelines pages work once it is True (dspaLoaded), and its
	// timeout is measured against it, not against Ready
	// (concepts/pipelines/context/usePipelineNamespaceCR.ts L12-37, rhoai-3.6).
	dspaAPIServerCondition = "APIServerReady"
)

var (
	// PipelineServerDeleteTimeout bounds how long teardown waits for the
	// DSPA finalizer to complete. Tests shorten it.
	PipelineServerDeleteTimeout = 60 * time.Second
	PipelineServerDeletePoll    = 2 * time.Second

	errDSPACRDMissing = errors.New("the DataSciencePipelinesApplication CRD is not installed")
)

// dspaInfo is the part of a DataSciencePipelinesApplication the tool reads.
type dspaInfo struct {
	Meta objectMeta
	Host string
	// Scheme and Port of the external object storage. DSPO appends Port to
	// Host when it is set (controllers/storage.go joinHostPort); the tool
	// never sets it and puts the port in Host.
	Scheme     string
	Port       string
	SecretName string
	ExternalDB bool
	Ready      *dspaCondition
	// APIServer is DSPO's APIServerReady condition (dspaAPIServerCondition).
	APIServer *dspaCondition
	// ObjectStore is DSPO's ObjectStoreAvailable condition: True once its
	// connection check to the object storage passed (or was skipped by the
	// DSPA's own settings), for the generation in ObservedGeneration
	// (controllers/dspastatus/dspa_status.go, rhoai-3.6).
	ObjectStore *dspaCondition
	// APIURL is the pipeline API's external address, which DSPO reports in
	// status.components.apiServer.externalUrl (api/v1 ComponentStatus): the
	// host for the KFP SDK.
	APIURL string
}

type dspaCondition struct {
	Status             string `json:"status"`
	Reason             string `json:"reason"`
	Message            string `json:"message"`
	ObservedGeneration int64  `json:"observedGeneration"`
}

// managedByTool reports whether this tool created the DSPA. Current versions
// label it. Older versions named it "dspa", pointed it at the tool's storage
// in namespace minio (legacyS3Host) with secret "dashboard-dspa-secret", and
// set no label (pipeline_server.go:235 at 5051672). odh-dashboard creates
// its pipeline server with the same names, so the proof is who wrote the
// object storage: the tool's managers (toolManagers) must own both
// spec.objectStorage.externalStorage.host and its s3CredentialsSecret's
// secretName. A DSPA the dashboard created, or whose storage someone else
// changed since, is not the tool's.
func (d dspaInfo) managedByTool() bool {
	if d.Meta.hasToolLabel() {
		return true
	}
	storage := []string{"f:spec", "f:objectStorage", "f:externalStorage"}
	return d.Meta.Name == legacyDSPAName &&
		d.Host == legacyS3Host() &&
		d.SecretName == legacyDSPASecretName &&
		d.Meta.ownsFieldPath(toolManagers, append(storage, "f:host")...) &&
		d.Meta.ownsFieldPath(toolManagers, append(storage, "f:s3CredentialsSecret", "f:secretName")...)
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
						Scheme              string `json:"scheme"`
						Port                string `json:"port"`
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
				Components struct {
					APIServer struct {
						ExternalURL string `json:"externalUrl"`
					} `json:"apiServer"`
				} `json:"components"`
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
		d := dspaInfo{Meta: item.Metadata, ExternalDB: item.Spec.Database.ExternalDB != nil, APIURL: item.Status.Components.APIServer.ExternalURL}
		if es := item.Spec.ObjectStorage.ExternalStorage; es != nil {
			d.Host, d.Scheme, d.Port = es.Host, es.Scheme, es.Port
			d.SecretName = es.S3CredentialsSecret.SecretName
		}
		for _, cond := range item.Status.Conditions {
			cc := cond.dspaCondition
			switch cond.Type {
			case "Ready":
				d.Ready = &cc
			case dspaAPIServerCondition:
				d.APIServer = &cc
			case dspaObjectStoreCondition:
				d.ObjectStore = &cc
			}
		}
		out = append(out, d)
	}
	return out, nil
}

// intOrString returns a JSON IntOrString (a port number or name) as text.
func intOrString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return strings.TrimSpace(string(raw))
}

// routeSpec is the part of a Route the tool reads.
type routeSpec struct {
	found      bool
	host       string
	toService  string
	targetPort string
}

// readRoute reads a Route; found is false when the Route (or the Route API)
// does not exist. Any other read or parse error is returned.
func readRoute(c *Client, namespace, name string) (routeSpec, error) {
	body, _, err := c.get(namespacedPath("route.openshift.io/v1", "routes", namespace, name))
	switch {
	case IsK8sError(err, 404):
		return routeSpec{}, nil
	case err != nil:
		return routeSpec{}, fmt.Errorf("read Route %s: %w", name, err)
	}
	var route struct {
		Spec struct {
			Host string `json:"host"`
			To   struct {
				Name string `json:"name"`
			} `json:"to"`
			Port struct {
				TargetPort json.RawMessage `json:"targetPort"`
			} `json:"port"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(body, &route); err != nil {
		return routeSpec{}, fmt.Errorf("parse Route %s: %w", name, err)
	}
	return routeSpec{found: true, host: route.Spec.Host, toService: route.Spec.To.Name, targetPort: intOrString(route.Spec.Port.TargetPort)}, nil
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

// pipelineServerState converts a tool-owned DSPA into its status entry.
func pipelineServerState(d dspaInfo, dashboardURL string) types.ResourceState {
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
		if dashboardURL != "" {
			state.UIRoute = dashboardURL + "/develop-train/pipelines/definitions/" + d.Meta.Namespace
		}
		switch {
		case snippetURL(d.APIURL):
			state.APIRoute = d.APIURL
		case d.APIURL != "":
			state.Warning = unsafeAddressWarning("pipeline server's API address")
		}
	default:
		// The Ready reason is often a success-sounding word such as
		// "MinimumReplicasAvailable"; the message carries the cause.
		state.Message = firstNonEmpty(truncateMessage(d.Ready.Message), d.Ready.Reason, "Not ready")
	}
	// Stuck, like the dashboard's hasServerTimedOut: the API server is not
	// ready 5 minutes after creation. Ready=False with a ready API server
	// (another component restarting) is not.
	apiServerReady := d.APIServer != nil && d.APIServer.Status == "True"
	if !state.Ready && !state.Terminating && !apiServerReady {
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

	// A component that is not Managed leaves its CRD behind: the DSPA
	// would be created and never start.
	if block := setupComponentBlock(c, pipelinesComponent); block != nil {
		return fail(block.Reason+" Nothing was changed.", "prerequisites", "component not managed")
	}
	storage := getS3Status(c)
	if !storage.Deployed || !storage.ManagedByTool || !storage.Ready {
		return fail("The S3 storage is not deployed or not ready. Set it up first (or, with storage from an earlier version, migrate it).", "prerequisites", "S3 storage not ready")
	}
	// Only a Data Science project gets a pipeline server, as with the
	// MLflow samples: the project is the user's (made in the dashboard),
	// and setup never creates or changes it.
	if ok, err := isDSProject(c, project); err != nil {
		return fail(fmt.Sprintf("Cannot list the projects; nothing was changed: %v", err), errorCodeFromK8sErr(err), "project list failed")
	} else if !ok {
		return fail(fmt.Sprintf("'%s' is not a Data Science project. Create it in the RHOAI dashboard first. Nothing was changed.", project), "validation", "not a Data Science project")
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
			return fail(fmt.Sprintf("Project '%s' already has a pipeline server ('%s') created by an earlier version of this tool. Delete it first to recreate it.", project, d.Meta.Name), "validation", "legacy pipeline server exists")
		}
		return fail(fmt.Sprintf("Project '%s' already has a pipeline server ('%s') that this tool did not create. It was left unchanged.", project, d.Meta.Name), "not_managed", "unmanaged pipeline server exists")
	}

	// Step 1: Read the S3 credentials
	logs = append(logs, "Reading S3 storage credentials...")
	keys, found, err := readS3Keys(c)
	if err != nil {
		return fail(fmt.Sprintf("Failed to read the S3 storage credentials: %v.%s", err, templateHint(err)), errorCodeFromK8sErr(err), "S3 secret read failed")
	}
	if !found || keys.AccessKey == "" || keys.SecretKey == "" {
		return fail(fmt.Sprintf("The S3 credentials are empty. Check that Secret %s in namespace %s has the keys %s and %s, or repair the S3 storage.", s3Secret, s3Namespace, s3AccessKeyField, s3SecretKeyField), "prerequisites", "S3 credentials empty")
	}
	logs = append(logs, "OK: S3 storage credentials read")

	// Step 2: Create the DSPA credentials secret, without taking over a
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
			s3AccessKeyField: keys.AccessKey,
			s3SecretKeyField: keys.SecretKey,
		},
	}
	// Step 3 prepares the DSPA; both writes are dry-run first (below).
	// fieldValidation=Strict (and server-side apply, which always validates
	// strictly) rejects fields the CRD does not declare, so a manifest that
	// drifts from the CRD fails instead of being silently pruned.
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
					"host":   s3Host(),
					"scheme": "http",
					"bucket": s3Bucket,
					"region": s3Region,
					"s3CredentialsSecret": map[string]interface{}{
						"accessKey":  s3AccessKeyField,
						"secretKey":  s3SecretKeyField,
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
	// POST when absent, resourceVersion-guarded apply when owned, so an
	// object someone creates after the checks above is never overwritten.
	// Both writes are sent as server-side dry runs first, so a pipeline
	// server the API server rejects leaves no secret behind.
	writeSecret := func(dryRun bool) (string, error) {
		action, meta, found, err := ensureToolObject(c, fmt.Sprintf("Secret %s/%s", project, dspaSecretName), dspaSecretPath,
			withToolFieldManager(namespacedPath("v1", "secrets", project, "")), dspaSecret, secretMeta, secretFound, readSecret, objectMeta.hasToolLabel, dryRun)
		secretMeta, secretFound = meta, found
		return action, err
	}
	writeDSPA := func(dryRun bool) (string, error) {
		action, meta, found, err := ensureToolObject(c, fmt.Sprintf("DataSciencePipelinesApplication %s/%s", project, dspaName), dspaPath,
			withToolFieldManager(namespacedPath(dspaAPIGroup, "datasciencepipelinesapplications", project, ""))+"&fieldValidation=Strict",
			dspa, dspaMeta, dspaFound, readDSPA, objectMeta.hasToolLabel, dryRun)
		dspaMeta, dspaFound = meta, found
		return action, err
	}
	logs = append(logs, "Checking the secret and the pipeline server with the API server (dry run)...")
	if _, err := writeSecret(true); err != nil {
		msg, code := pipelineWriteFailure(fmt.Sprintf("secret '%s'", dspaSecretName), project, err)
		return fail(msg+" Nothing was changed.", code, "secret dry run failed")
	}
	if _, err := writeDSPA(true); err != nil {
		msg, code := pipelineWriteFailure(fmt.Sprintf("pipeline server '%s'", dspaName), project, err)
		return fail(msg+" Nothing was changed.", code, "DSPA dry run failed")
	}
	secretAction, err := writeSecret(false)
	if err != nil {
		msg, code := pipelineWriteFailure(fmt.Sprintf("secret '%s'", dspaSecretName), project, err)
		return fail(msg, code, "secret write failed")
	}
	logs = append(logs, "OK: Secret "+secretAction)

	logs = append(logs, "Creating pipeline server (DSPA)...")
	dspaAction, err := writeDSPA(false)
	if err != nil {
		msg, code := pipelineWriteFailure(fmt.Sprintf("pipeline server '%s'", dspaName), project, err)
		return fail(fmt.Sprintf("%s The credentials secret '%s' is in place; re-run setup, or delete the pipeline server to clean up.", msg, dspaSecretName), code, "DSPA write failed")
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
	// The tool's label, or an earlier version's managers own its keys.
	if !meta.hasToolLabel() && !meta.ownsFieldPath(toolManagers, "f:data") && !meta.ownsFieldPath(toolManagers, "f:stringData") {
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
