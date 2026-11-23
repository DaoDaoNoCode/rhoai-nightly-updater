package storage

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const (
	// DSPAName is the name odh-dashboard gives its pipeline server
	// (frontend/src/concepts/pipelines/const.ts DEFAULT_PIPELINE_DEFINITION_NAME,
	// createPipelinesCR in api/pipelines/k8s.ts, rhoai-3.6). The dashboard
	// finds a project's pipeline server by listing DSPAs
	// (concepts/pipelines/context/usePipelineNamespaceCR.ts), but
	// odh-notebook-controller reads only the DSPA named "dspa": it builds a
	// workbench's Elyra runtime config (Secret ds-pipeline-config) from it and
	// binds the workbench to Role ds-pipeline-user-access-dspa, which DSPO
	// names ds-pipeline-user-access-<DSPA name> (red-hat-data-services/
	// kubeflow rhoai-3.6 components/odh-notebook-controller/controllers/
	// notebook_dspa_secret.go dspaInstanceName L42/L51, notebook_rbac.go
	// L149). Any other name leaves workbenches without pipelines. Setup
	// refuses a project that already has a DSPA, so the name never meets
	// the dashboard's; the tool's label marks the one it created.
	DSPAName = "dspa"
	// DSPASecretName stays the tool's own: odh-dashboard's Delete pipeline
	// server deletes "dashboard-dspa-secret" by name (concepts/pipelines/
	// utils.ts deleteServer), and a dashboard secret must never be mistaken
	// for the tool's. The DSPA owns it (setDSPAOwner).
	DSPASecretName = "nightly-dspa-s3"
	// PreviousDSPAName is the name the tool used before it used "dspa"
	// (2.x and early 3.0.0). Such servers keep working and are still the
	// tool's everywhere (status, teardown, Migrate storage, Clean up); the
	// Pipelines tab offers to recreate them as "dspa".
	PreviousDSPAName = "nightly-dspa"
	// Names used by the earliest versions; see dspaInfo.managedByTool. The
	// DSPA name is the current one again.
	LegacyDSPAName       = DSPAName
	LegacyDSPASecretName = "dashboard-dspa-secret"
	DSPAAPIGroup         = "datasciencepipelinesapplications.opendatahub.io/v1"
	// DSPAFinalizer is removed by the data-science-pipelines-operator once
	// it has cleaned up (DSPO controllers/dspipeline_controller.go, rhoai-3.6).
	DSPAFinalizer = "datasciencepipelinesapplications.opendatahub.io/finalizer"
	// dspoSelector selects the data-science-pipelines-operator Deployment
	// in the applications namespace (live RHOAI 3.6 label).
	dspoSelector = "app.kubernetes.io/name=data-science-pipelines-operator"
	// DSPAStuckAfter matches odh-dashboard's SERVER_TIMEOUT (5 minutes,
	// frontend/src/utilities/const.ts), after which the dashboard reports a
	// pipeline server whose API server is still not ready as having issues.
	DSPAStuckAfter = 5 * time.Minute
	// dspaAPIServerCondition is DSPO's APIServerReady condition. The
	// dashboard's pipelines pages work once it is True (dspaLoaded), and its
	// timeout is measured against it, not against Ready
	// (concepts/pipelines/context/usePipelineNamespaceCR.ts L12-37, rhoai-3.6).
	dspaAPIServerCondition = "APIServerReady"
)

var (
	// pipelineServerDeleteTimeout bounds how long teardown waits for the
	// DSPA finalizer to complete. Tests shorten it.
	pipelineServerDeleteTimeout = 60 * time.Second
	pipelineServerDeletePoll    = 2 * time.Second

	ErrDSPACRDMissing = errors.New("the DataSciencePipelinesApplication CRD is not installed")
)

// DSPA is the part of a DataSciencePipelinesApplication the tool reads.
type DSPA struct {
	Meta kube.ObjectMeta
	Host string
	// Scheme and Port of the external object storage. DSPO appends Port to
	// Host when it is set (controllers/storage.go joinHostPort); the tool
	// never sets it and puts the port in Host.
	Scheme     string
	Port       string
	SecretName string
	ExternalDB bool
	Ready      *DSPACondition
	// APIServer is DSPO's APIServerReady condition (dspaAPIServerCondition).
	APIServer *DSPACondition
	// ObjectStore is DSPO's ObjectStoreAvailable condition: True once its
	// connection check to the object storage passed (or was skipped by the
	// DSPA's own settings), for the generation in ObservedGeneration
	// (controllers/dspastatus/dspa_status.go, rhoai-3.6).
	ObjectStore *DSPACondition
	// APIURL is the pipeline API's external address, which DSPO reports in
	// status.components.apiServer.externalUrl (api/v1 ComponentStatus): the
	// host for the KFP SDK.
	APIURL string
}

type DSPACondition struct {
	Status             string `json:"status"`
	Reason             string `json:"reason"`
	Message            string `json:"message"`
	ObservedGeneration int64  `json:"observedGeneration"`
}

// ManagedByTool reports whether this tool created the DSPA. Current versions
// label it. Older versions named it "dspa", pointed it at the tool's storage
// in namespace minio (legacyS3Host) with secret "dashboard-dspa-secret", and
// set no label (pipeline_server.go:235 at 5051672). odh-dashboard creates
// its pipeline server with the same names, so the proof is who wrote the
// object storage: the tool's managers (envkit.ToolManagers) must own both
// spec.objectStorage.externalStorage.host and its s3CredentialsSecret's
// secretName. A DSPA the dashboard created, or whose storage someone else
// changed since, is not the tool's.
func (d DSPA) ManagedByTool() bool {
	if d.Meta.HasToolLabel() {
		return true
	}
	storage := []string{"f:spec", "f:objectStorage", "f:externalStorage"}
	return d.Meta.Name == LegacyDSPAName &&
		d.Host == LegacyS3Host() &&
		d.SecretName == LegacyDSPASecretName &&
		d.Meta.OwnsFieldPath(envkit.ToolManagers, append(storage, "f:host")...) &&
		d.Meta.OwnsFieldPath(envkit.ToolManagers, append(storage, "f:s3CredentialsSecret", "f:secretName")...)
}

func (d DSPA) hasFinalizer() bool {
	return slices.Contains(d.Meta.Finalizers, DSPAFinalizer)
}

// toolDSPAPath returns the path of a pipeline server only for the names this
// tool creates (or created in older versions), so teardown can never reach
// another DSPA, and the RBAC rule stays limited to these names.
func toolDSPAPath(project, name string) (string, bool) {
	switch name {
	case DSPAName:
		return rhoai.NamespacedPath(DSPAAPIGroup, "datasciencepipelinesapplications", project, DSPAName), true
	case PreviousDSPAName:
		return rhoai.NamespacedPath(DSPAAPIGroup, "datasciencepipelinesapplications", project, PreviousDSPAName), true
	}
	return "", false
}

// toolDSPASecretPath is toolDSPAPath for the pipeline server's S3 secret.
func toolDSPASecretPath(project, name string) (string, bool) {
	switch name {
	case DSPASecretName:
		return rhoai.NamespacedPath("v1", "secrets", project, DSPASecretName), true
	case LegacyDSPASecretName:
		return rhoai.NamespacedPath("v1", "secrets", project, LegacyDSPASecretName), true
	}
	return "", false
}

func dspaCollectionPath(namespace string) string {
	if namespace == "" {
		return rhoai.ClusterPath(DSPAAPIGroup, "datasciencepipelinesapplications", "")
	}
	return rhoai.NamespacedPath(DSPAAPIGroup, "datasciencepipelinesapplications", namespace, "")
}

// ListDSPAs lists pipeline servers in namespace, or cluster-wide when
// namespace is "". A missing CRD returns errDSPACRDMissing.
func ListDSPAs(c *kube.Client, namespace string) ([]DSPA, error) {
	var list struct {
		Items []struct {
			Metadata kube.ObjectMeta `json:"metadata"`
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
					DSPACondition
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	found, err := c.GetIntoOptional(dspaCollectionPath(namespace), &list)
	if err != nil {
		if kube.IsDecodeError(err) {
			return nil, fmt.Errorf("parse pipeline server list: %w", err)
		}
		return nil, fmt.Errorf("list pipeline servers: %w", err)
	}
	if !found {
		return nil, ErrDSPACRDMissing
	}
	out := make([]DSPA, 0, len(list.Items))
	for _, item := range list.Items {
		d := DSPA{Meta: item.Metadata, ExternalDB: item.Spec.Database.ExternalDB != nil, APIURL: item.Status.Components.APIServer.ExternalURL}
		if es := item.Spec.ObjectStorage.ExternalStorage; es != nil {
			d.Host, d.Scheme, d.Port = es.Host, es.Scheme, es.Port
			d.SecretName = es.S3CredentialsSecret.SecretName
		}
		for _, cond := range item.Status.Conditions {
			cc := cond.DSPACondition
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
func readRoute(c *kube.Client, namespace, name string) (routeSpec, error) {
	body, exists, err := c.GetOptional(rhoai.NamespacedPath("route.openshift.io/v1", "routes", namespace, name))
	switch {
	case err != nil:
		return routeSpec{}, fmt.Errorf("read Route %s: %w", name, err)
	case !exists:
		return routeSpec{}, nil
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

// PipelineServerState converts a tool-owned DSPA into its status entry.
func PipelineServerState(d DSPA, dashboardURL string) types.ResourceState {
	state := types.ResourceState{
		Namespace:     d.Meta.Namespace,
		Name:          d.Meta.Name,
		Deployed:      true,
		ManagedByTool: true,
		Terminating:   d.Meta.Terminating(),
		// Workbench pipelines need the name dspa (see dspaName).
		NonStandardName: d.Meta.Name != DSPAName,
	}
	if d.Meta.Annotations[DSPAConnectionAnnotation] == ConnectionSecretName {
		state.Connection = ConnectionSecretName
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
		case kube.SnippetURL(d.APIURL):
			state.APIRoute = d.APIURL
		case d.APIURL != "":
			state.Warning = kube.UnsafeAddressWarning("pipeline server's API address")
		}
	default:
		// The Ready reason is often a success-sounding word such as
		// "MinimumReplicasAvailable"; the message carries the cause.
		state.Message = kube.FirstNonEmpty(kube.TruncateText(d.Ready.Message, 300), d.Ready.Reason, "Not ready")
	}
	// Stuck, like the dashboard's hasServerTimedOut: the API server is not
	// ready 5 minutes after creation. Ready=False with a ready API server
	// (another component restarting) is not.
	apiServerReady := d.APIServer != nil && d.APIServer.Status == "True"
	if !state.Ready && !state.Terminating && !apiServerReady {
		if created, err := time.Parse(time.RFC3339, d.Meta.CreationTimestamp); err == nil && time.Since(created) > DSPAStuckAfter {
			state.TerminalError = true
		}
	}
	return state
}

// SetupPipelineServer creates a pipeline server (DSPA) in the given project.
func SetupPipelineServer(c *kube.Client, project string) (*types.OperationResponse, error) {
	return setupPipelineServer(c, project, false, "")
}

// RecreatePipelineServer replaces the pipeline server the tool set up in
// project under its earlier name (previousDSPAName) with one named dspaName,
// so workbench pipelines work. Every check and a dry run of every new write
// come first; only then is the old server deleted (its database, and so its
// run history, goes with it; artifacts in the S3 storage stay) and the new
// one set up. Without an old server it is a plain setup.
func RecreatePipelineServer(c *kube.Client, project, confirmedUID string) (*types.OperationResponse, error) {
	return setupPipelineServer(c, project, true, confirmedUID)
}

func setupPipelineServer(c *kube.Client, project string, recreate bool, confirmedUID string) (*types.OperationResponse, error) {
	logs := []string{}
	op := "setup-pipeline-server"
	if recreate {
		op = "recreate-pipeline-server"
	}
	fail := func(msg, code, activity string) (*types.OperationResponse, error) {
		record.Action(c, op, fmt.Sprintf("project=%s (%s)", project, activity), false)
		return &types.OperationResponse{Success: false, Message: msg, Logs: logs, ErrorCode: code}, nil
	}

	// A component that is not Managed leaves its CRD behind: the DSPA
	// would be created and never start.
	if block := envkit.SetupComponentBlock(c, envkit.PipelinesComponent); block != nil {
		return fail(block.Reason+" Nothing was changed.", "prerequisites", "component not managed")
	}
	storage := S3Status(c)
	if !storage.Deployed || !storage.ManagedByTool || !storage.Ready {
		return fail("The S3 storage is not deployed or not ready. Set it up first (or, with storage from an earlier version, migrate it).", "prerequisites", "S3 storage not ready")
	}
	// Only a Data Science project gets a pipeline server, as with the
	// MLflow samples: the project is the user's (made in the dashboard),
	// and setup never creates or changes it.
	if ok, err := envkit.IsDSProject(c, project); err != nil {
		return fail(fmt.Sprintf("Cannot list the projects; nothing was changed: %v", err), kube.ErrorCodeFromK8sErr(err), "project list failed")
	} else if !ok {
		return fail(fmt.Sprintf("%s is not a Data Science project. Create it in the RHOAI dashboard first. Nothing was changed.", project), "validation", "not a Data Science project")
	}

	// Never add a second pipeline server next to one this tool did not
	// create: odh-dashboard shows only the first DSPA of a project.
	existing, err := ListDSPAs(c, project)
	switch {
	case errors.Is(err, ErrDSPACRDMissing):
		return fail("Data Science Pipelines is not installed (no DataSciencePipelinesApplication CRD). Set the aipipelines component to Managed in the DataScienceCluster first.", "prerequisites", "DSPA CRD missing")
	case err != nil:
		return fail(fmt.Sprintf("Cannot check for existing pipeline servers: %v", err), kube.ErrorCodeFromK8sErr(err), "list failed")
	}
	var dspaMeta kube.ObjectMeta
	dspaFound := false
	var old *DSPA
	for i, d := range existing {
		switch {
		case d.Meta.Name == DSPAName && d.Meta.HasToolLabel():
			if d.Meta.Terminating() {
				return fail(fmt.Sprintf("The pipeline server in %s is still being deleted. Wait for it to finish, then retry.", project), "terminating", "terminating")
			}
			dspaMeta, dspaFound = d.Meta, true
		case d.Meta.Name == PreviousDSPAName && d.ManagedByTool():
			if d.Meta.Terminating() {
				return fail(fmt.Sprintf("Pipeline server %s in %s is still being deleted. Wait for it to finish, then set up again. Nothing was changed.", d.Meta.Name, project), "terminating", "earlier name terminating")
			}
			if !recreate {
				return fail(fmt.Sprintf("Project %s already has pipeline server %s, which an earlier version of this tool set up. Workbench pipelines need it to be named %s: use Recreate with the standard name on the Pipelines tab (its run history is lost), or keep it as it is. Nothing was changed.", project, d.Meta.Name, DSPAName), "validation", "earlier name exists")
			}
			old = &existing[i]
		case d.ManagedByTool():
			return fail(fmt.Sprintf("Project %s already has a pipeline server (%s) created by an earlier version of this tool. Delete it first to recreate it.", project, d.Meta.Name), "validation", "legacy pipeline server exists")
		default:
			return fail(fmt.Sprintf("Project %s already has a pipeline server (%s) that this tool did not create. It was left unchanged.", project, d.Meta.Name), "not_managed", "unmanaged pipeline server exists")
		}
	}
	if old != nil && dspaFound {
		return fail(fmt.Sprintf("Project %s has both %s and %s from this tool; delete one of them first. Nothing was changed.", project, PreviousDSPAName, DSPAName), "validation", "both names exist")
	}

	// Step 1: Read the S3 credentials
	logs = append(logs, "Reading S3 storage credentials...")
	keys, found, err := ReadS3Keys(c)
	if err != nil {
		return fail(fmt.Sprintf("Failed to read the S3 storage credentials: %v.%s", err, kube.TemplateHint(err)), kube.ErrorCodeFromK8sErr(err), "S3 secret read failed")
	}
	if !found || keys.AccessKey == "" || keys.SecretKey == "" {
		return fail(fmt.Sprintf("The S3 credentials are empty. Check that Secret %s in namespace %s has the keys %s and %s, or repair the S3 storage.", S3Secret, S3Namespace, S3AccessKeyField, S3SecretKeyField), "prerequisites", "S3 credentials empty")
	}
	logs = append(logs, "OK: S3 storage credentials read")

	// Step 2: The DSPA credentials secret and the dashboard Connection,
	// without taking over a secret of the same name someone else created.
	logs = append(logs, "Creating pipeline server secret and dashboard connection...")
	secrets := []*projectSecret{
		{c: c, project: project, name: DSPASecretName, what: "secret", obj: dspaS3Secret(project, keys)},
		{c: c, project: project, name: ConnectionSecretName, what: "connection", obj: dashboardS3Connection(project, keys)},
	}
	for _, s := range secrets {
		if err := readProjectSecret(s, projectSecretPath(project, s.name)); err != nil {
			return fail(fmt.Sprintf("Cannot read secret %s: %v", s.name, err), kube.ErrorCodeFromK8sErr(err), s.what+" read failed")
		}
		if s.found && !s.meta.HasToolLabel() {
			return fail(fmt.Sprintf("Secret %s already exists in %s and was not created by this tool. It was left unchanged.", s.name, project), "not_managed", s.what+" exists")
		}
		// Owned by the DSPA once it exists (below, or now on a re-run).
		if dspaFound {
			setDSPAOwner(s.obj, DSPAName, dspaMeta.UID)
		}
	}
	secretsOwned := dspaFound
	// Step 3 prepares the DSPA; both writes are dry-run first (below).
	// fieldValidation=Strict (and server-side apply, which always validates
	// strictly) rejects fields the CRD does not declare, so a manifest that
	// drifts from the CRD fails instead of being silently pruned.
	apiServer := map[string]any{
		"enableSamplePipeline": false,
		"cacheEnabled":         true,
		// odh-dashboard's default is "kubernetes" (Pipeline and
		// PipelineVersion objects; ConfigurePipelinesServerModal.tsx
		// FORM_DEFAULTS storeYamlInKubernetes: true). The tool keeps its
		// pipeline definitions in the database, DSPO's own default.
		"pipelineStore": "database",
	}
	spec := map[string]any{
		"dspVersion": "v2",
		"objectStorage": map[string]any{
			"externalStorage": map[string]any{
				"host":   S3Host(),
				"scheme": "http",
				"bucket": s3Bucket,
				"region": S3Region,
				"s3CredentialsSecret": map[string]any{
					"accessKey":  S3AccessKeyField,
					"secretKey":  S3SecretKeyField,
					"secretName": DSPASecretName,
				},
			},
		},
		"apiServer": apiServer,
	}
	logs = append(logs, addDashboardDSPASettings(c, spec, apiServer)...)
	dspa := map[string]any{
		"apiVersion": DSPAAPIGroup,
		"kind":       "DataSciencePipelinesApplication",
		"metadata": map[string]any{
			"name":      DSPAName,
			"namespace": project,
			"labels":    envkit.ToolLabels(),
			// Which Connection goes with it, for the status: the status
			// reads only the DSPA list, so it costs no request per project.
			"annotations": map[string]any{DSPAConnectionAnnotation: ConnectionSecretName},
		},
		"spec": spec,
	}
	dspaPath := rhoai.NamespacedPath(DSPAAPIGroup, "datasciencepipelinesapplications", project, DSPAName)
	readDSPA := func() (kube.ObjectMeta, bool, error) {
		body, present, getErr := c.GetOptional(dspaPath)
		if getErr != nil {
			return kube.ObjectMeta{}, false, getErr
		}
		if !present {
			return kube.ObjectMeta{}, false, nil
		}
		meta, perr := kube.ParseObjectMeta(body)
		return meta, true, perr
	}
	// POST when absent, resourceVersion-guarded apply when owned, so an
	// object someone creates after the checks above is never overwritten.
	// Every write is sent as a server-side dry run first, so a pipeline
	// server the API server rejects leaves no secret behind.
	writeDSPA := func(dryRun bool) (string, error) {
		action, meta, found, err := envkit.EnsureToolObject(c, fmt.Sprintf("DataSciencePipelinesApplication %s/%s", project, DSPAName), dspaPath,
			envkit.WithToolFieldManager(rhoai.NamespacedPath(DSPAAPIGroup, "datasciencepipelinesapplications", project, ""))+"&fieldValidation=Strict",
			dspa, dspaMeta, dspaFound, readDSPA, kube.ObjectMeta.HasToolLabel, dryRun)
		dspaMeta, dspaFound = meta, found
		return action, err
	}
	logs = append(logs, "Checking the secrets and the pipeline server with the API server (dry run)...")
	for _, s := range secrets {
		if _, err := writeProjectSecret(s, projectSecretPath(project, s.name), envkit.WithToolFieldManager(rhoai.NamespacedPath("v1", "secrets", project, "")), true); err != nil {
			msg, code := pipelineWriteFailure(fmt.Sprintf("secret %s", s.name), project, err)
			return fail(msg+" Nothing was changed.", code, s.what+" dry run failed")
		}
	}
	if _, err := writeDSPA(true); err != nil {
		msg, code := pipelineWriteFailure(fmt.Sprintf("pipeline server %s", DSPAName), project, err)
		return fail(msg+" Nothing was changed.", code, "DSPA dry run failed")
	}
	if old != nil {
		return replacePipelineServer(c, project, old, logs, confirmedUID)
	}
	for _, s := range secrets {
		action, err := writeProjectSecret(s, projectSecretPath(project, s.name), envkit.WithToolFieldManager(rhoai.NamespacedPath("v1", "secrets", project, "")), false)
		if err != nil {
			msg, code := pipelineWriteFailure(fmt.Sprintf("secret %s", s.name), project, err)
			return fail(msg, code, s.what+" write failed")
		}
		logs = append(logs, fmt.Sprintf("OK: Secret %s %s", s.name, action))
	}

	logs = append(logs, "Creating pipeline server (DSPA)...")
	dspaAction, err := writeDSPA(false)
	if err != nil {
		msg, code := pipelineWriteFailure(fmt.Sprintf("pipeline server %s", DSPAName), project, err)
		return fail(fmt.Sprintf("%s Secrets %s and %s are in place; re-run setup, or delete the pipeline server to clean up.", msg, DSPASecretName, ConnectionSecretName), code, "DSPA write failed")
	}
	logs = append(logs, "OK: Pipeline server "+dspaAction)
	// Recorded as the tool's (pipeline_record.go): Migrate storage writes
	// the storage's keys only into a recorded or confirmed pipeline
	// server's project. A record that fails leaves it unrecorded, which
	// Migrate storage then asks about: nothing is trusted without it.
	recordNote := ""
	if err := recordPipelineServer(c, dspaMeta); err != nil {
		logs = append(logs, fmt.Sprintf("Warning: the pipeline server could not be recorded as this tool's (%v); Migrate storage will ask you to confirm its project.", err))
		recordNote = " It could not be recorded as this tool's; Migrate storage will ask you to confirm its project."
	}

	// The secrets are created before the DSPA, which reads the first; once
	// the DSPA exists it becomes their owner, so the garbage collector
	// deletes them with the DSPA, also when the dashboard deletes it.
	if !secretsOwned {
		for _, s := range secrets {
			logs = append(logs, fmt.Sprintf("Making pipeline server %s the owner of secret %s...", DSPAName, s.name))
			setDSPAOwner(s.obj, DSPAName, dspaMeta.UID)
			if _, err := writeProjectSecret(s, projectSecretPath(project, s.name), envkit.WithToolFieldManager(rhoai.NamespacedPath("v1", "secrets", project, "")), false); err != nil {
				record.Action(c, "setup-pipeline-server", fmt.Sprintf("project=%s (%s owner not set: %v)", project, s.what, err), false)
				return &types.OperationResponse{
					Success: false, ErrorCode: "partial_failure", Logs: logs,
					Message: fmt.Sprintf("Pipeline server created, but secret %s could not be linked to it: %v. Deleting the pipeline server in the RHOAI dashboard would leave the secret behind; Delete pipeline server in this tool removes it too.", s.name, err),
				}, nil
			}
			logs = append(logs, "OK: Owned by the pipeline server")
		}
	}
	logs = append(logs, "The pipeline server will take 1-3 minutes to become ready.")

	slog.Info("pipeline server setup", "project", project, "user", kube.GetUser(c))
	record.Action(c, "setup-pipeline-server", fmt.Sprintf("project=%s dspa=%s", project, DSPAName), true)

	return &types.OperationResponse{
		Success: true, Message: "Pipeline server created. It will take 1-3 minutes to become ready." + recordNote,
		Logs: logs,
	}, nil
}

// replacePipelineServer is Recreate's second half, after every check and dry
// run passed: it deletes the old server with the regular teardown (which
// checks the operator before deleting and waits for the finalizer), then
// sets up the new one from scratch, so every check runs again on what is
// there now.
func replacePipelineServer(c *kube.Client, project string, old *DSPA, logs []string, confirmedUID string) (*types.OperationResponse, error) {
	logs = append(logs, "OK: The API server accepts the new pipeline server", fmt.Sprintf("Deleting pipeline server %s...", old.Meta.Name))
	down, err := TeardownPipelineServer(c, project, confirmedUID)
	if err != nil {
		return nil, err
	}
	logs = append(logs, down.Logs...)
	if !down.Success {
		record.Action(c, "recreate-pipeline-server", fmt.Sprintf("project=%s (teardown of %s: %s)", project, old.Meta.Name, down.ErrorCode), false)
		msg := down.Message
		if !strings.Contains(msg, "Nothing was deleted") {
			msg += fmt.Sprintf(" Pipeline server %s was not created; once %s is gone, use Set up.", DSPAName, old.Meta.Name)
		}
		return &types.OperationResponse{Success: false, Message: msg, Logs: logs, ErrorCode: down.ErrorCode}, nil
	}
	up, err := setupPipelineServer(c, project, false, "")
	if err != nil {
		return nil, err
	}
	up.Logs = append(logs, up.Logs...)
	if !up.Success {
		up.Message = fmt.Sprintf("Pipeline server %s was deleted, but %s could not be set up: %s Use Set up to try again.", old.Meta.Name, DSPAName, up.Message)
		record.Action(c, "recreate-pipeline-server", fmt.Sprintf("project=%s (setup failed)", project), false)
		return up, nil
	}
	record.Action(c, "recreate-pipeline-server", fmt.Sprintf("project=%s %s -> %s", project, old.Meta.Name, DSPAName), true)
	up.Message = fmt.Sprintf("Pipeline server %s replaced by %s, which workbench pipelines use. It will take 1-3 minutes to become ready.", old.Meta.Name, DSPAName)
	return up, nil
}

// dspoRunning reports whether a data-science-pipelines-operator replica is
// ready, so a DSPA finalizer will be processed.
func dspoRunning(c *kube.Client) (bool, error) {
	ns, err := rhoai.AppsNamespace(c)
	if err != nil {
		return false, err
	}
	var items []struct {
		Status struct {
			ReadyReplicas int `json:"readyReplicas"`
		} `json:"status"`
	}
	if err := c.ListInto(rhoai.NamespacedPath("apps/v1", "deployments", ns, "")+"?labelSelector="+url.QueryEscape(dspoSelector), &items); err != nil {
		return false, err
	}
	for _, d := range items {
		if d.Status.ReadyReplicas > 0 {
			return true, nil
		}
	}
	return false, nil
}

// TeardownPipelineServer removes the tool-created DSPA and its credentials
// secret from the given project. It is safe to re-run: a DSPA that is already
// terminating is not deleted again, and missing objects count as removed.
// One that carries the tool's label but whose UID the tool did not record
// (set up before 3.0.1, or labelled by someone in the project) is deleted
// only when confirmedUID is its UID (the user confirmed that very server;
// one made again since is not it): the label alone is something a project
// editor can set.
func TeardownPipelineServer(c *kube.Client, project, confirmedUID string) (*types.OperationResponse, error) {
	logs := []string{}
	fail := func(msg, code, activity string) (*types.OperationResponse, error) {
		record.Action(c, "teardown-pipeline-server", fmt.Sprintf("project=%s (%s)", project, activity), false)
		return &types.OperationResponse{Success: false, Message: msg, Logs: logs, ErrorCode: code}, nil
	}

	all, err := ListDSPAs(c, project)
	if errors.Is(err, ErrDSPACRDMissing) {
		all, err = nil, nil
	}
	if err != nil {
		return fail(fmt.Sprintf("Cannot list pipeline servers in %s; nothing was deleted: %v", project, err), kube.ErrorCodeFromK8sErr(err), "list failed")
	}
	var target *DSPA
	var unmanaged []string
	for i := range all {
		if all[i].ManagedByTool() {
			if target == nil {
				target = &all[i]
			}
		} else {
			unmanaged = append(unmanaged, all[i].Meta.Name)
		}
	}
	if target == nil {
		if len(unmanaged) > 0 {
			return fail(fmt.Sprintf("The pipeline server in %s (%s) was not created by this tool, so it was not deleted.", project, strings.Join(unmanaged, ", ")), "not_managed", "not managed")
		}
		logs = append(logs, "No pipeline server created by this tool in this project")
		// A previous run may have removed the DSPA but not its secret.
		orphan := &DSPA{Meta: kube.ObjectMeta{UID: "none"}, SecretName: DSPASecretName}
		note := deleteDSPASecret(c, project, orphan, all, &logs) + deleteConnection(c, project, &logs)
		if note != "" {
			return fail(fmt.Sprintf("No pipeline server created by this tool remains in %s.%s", project, note), "partial_failure", "orphan secret")
		}
		return &types.OperationResponse{Success: true, Message: fmt.Sprintf("No pipeline server created by this tool remains in %s.", project), Logs: logs}, nil
	}

	dspaPath, ok := toolDSPAPath(project, target.Meta.Name)
	if !ok {
		return fail(fmt.Sprintf("Pipeline server %s does not have a name this tool creates, so it was not deleted.", target.Meta.Name), "not_managed", "unexpected DSPA name")
	}
	if confirmedUID == "" || confirmedUID != target.Meta.UID {
		rec, err := ReadPipelineRecord(c)
		if err != nil {
			return fail(fmt.Sprintf("Cannot check that pipeline server %s in %s is the one this tool set up: %v. Nothing was deleted.", target.Meta.Name, project, err), kube.ErrorCodeFromK8sErr(err), "record read failed")
		}
		if !rec.Owns(target.Meta) && confirmedUID != "" {
			return fail(fmt.Sprintf("Pipeline server %s in %s is not the one you confirmed as this tool's: it was deleted and made again since, and this tool has no record that it set it up. Check it again in the Delete dialog. Nothing was deleted.", target.Meta.Name, project), "not_managed", "not the confirmed server")
		}
		if !rec.Owns(target.Meta) {
			return fail(fmt.Sprintf("Pipeline server %s in %s carries this tool's label, but this tool has no record that it set it up (set up before version 3.0.1, or labelled by someone in %s). Confirm it as this tool's in the Delete dialog. Nothing was deleted.", target.Meta.Name, project, project), "not_managed", "unrecorded")
		}
	}
	if target.Meta.Terminating() {
		logs = append(logs, fmt.Sprintf("Pipeline server %s is already being deleted", target.Meta.Name))
	} else {
		// The DSPA finalizer is removed only by the running operator. Deleting
		// while it is down would leave the DSPA stuck in Terminating.
		if target.hasFinalizer() {
			running, opErr := dspoRunning(c)
			if opErr != nil {
				return fail(fmt.Sprintf("Cannot verify that the data-science-pipelines operator is running; nothing was deleted: %v", opErr), kube.ErrorCodeFromK8sErr(opErr), "operator check failed")
			}
			if !running {
				return fail("The data-science-pipelines operator is not running, so the pipeline server's finalizer could not complete and it would stay in Terminating. Nothing was deleted. Make sure the aipipelines component is Managed and its operator is ready, then retry.", "prerequisites", "operator not running")
			}
		}
		logs = append(logs, fmt.Sprintf("Deleting pipeline server %s...", target.Meta.Name))
		if _, delErr := kube.DeleteWithUID(c, dspaPath, target.Meta.UID); delErr != nil && !kube.IsK8sNotFound(delErr) {
			code := kube.ErrorCodeFromK8sErr(delErr)
			if kube.IsK8sConflict(delErr) {
				code = "conflict"
			}
			return fail(fmt.Sprintf("Failed to delete pipeline server: %v", delErr), code, "DSPA delete failed")
		}
		logs = append(logs, "OK: Deletion requested")
	}

	// The DSPA finalizer does not read the credentials secret, so it can go
	// now; doing it before waiting keeps a timed-out teardown from leaving the
	// secret behind with no pipeline server left to show a Tear down button.
	secretNote := deleteDSPASecret(c, project, target, all, &logs) + deleteConnection(c, project, &logs)

	gone, waitErr := kube.WaitForDeletion(c, dspaPath, pipelineServerDeleteTimeout, pipelineServerDeletePoll)
	if waitErr != nil {
		record.Action(c, "teardown-pipeline-server", fmt.Sprintf("project=%s (cannot confirm deletion: %v)", project, waitErr), false)
		return &types.OperationResponse{
			Success:   false,
			Message:   fmt.Sprintf("Deletion of pipeline server %s was requested, but whether it is gone cannot be checked: %v.%s", target.Meta.Name, waitErr, secretNote),
			Logs:      logs,
			ErrorCode: kube.FirstNonEmpty(kube.ErrorCodeFromK8sErr(waitErr), "in_progress"),
		}, nil
	}
	if !gone {
		record.Action(c, "teardown-pipeline-server", fmt.Sprintf("project=%s (still terminating)", project), false)
		return &types.OperationResponse{
			Success:   false,
			Message:   fmt.Sprintf("Deletion of pipeline server %s was requested, but it is still terminating after %s (waiting for the data-science-pipelines operator to finish cleanup). The status shows it until it is gone.%s", target.Meta.Name, pipelineServerDeleteTimeout, secretNote),
			Logs:      logs,
			ErrorCode: "in_progress",
		}, nil
	}
	logs = append(logs, "OK: Pipeline server deleted")
	if err := forgetPipelineServer(c, project, target.Meta.Name); err != nil {
		// A stale entry names a UID that no longer exists: it trusts nothing.
		logs = append(logs, fmt.Sprintf("Warning: this tool's record of the pipeline server could not be cleared: %v", err))
	}

	slog.Info("pipeline server teardown", "project", project, "dspa", target.Meta.Name, "user", kube.GetUser(c))
	record.Action(c, "teardown-pipeline-server", fmt.Sprintf("project=%s dspa=%s", project, target.Meta.Name), secretNote == "")

	msg := fmt.Sprintf("Pipeline server removed from %s.", project)
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
func deleteDSPASecret(c *kube.Client, project string, target *DSPA, all []DSPA, logs *[]string) string {
	name := target.SecretName
	if name == "" {
		return ""
	}
	for _, d := range all {
		if d.Meta.UID != target.Meta.UID && !d.Meta.Terminating() && d.SecretName == name {
			*logs = append(*logs, fmt.Sprintf("Kept secret %s: pipeline server %s also uses it", name, d.Meta.Name))
			return ""
		}
	}
	path, ok := toolDSPASecretPath(project, name)
	if !ok {
		*logs = append(*logs, fmt.Sprintf("Kept secret %s: this tool never creates a secret with that name", name))
		return ""
	}
	body, found, err := c.GetOptional(path)
	if err != nil {
		*logs = append(*logs, fmt.Sprintf("Warning: cannot read secret %s: %v", name, err))
		return fmt.Sprintf(" The credentials secret %s could not be checked and was left in place: %v.", name, err)
	}
	if !found {
		*logs = append(*logs, "OK: Secret already absent")
		return ""
	}
	meta, err := kube.ParseObjectMeta(body)
	if err != nil {
		return fmt.Sprintf(" The credentials secret %s could not be parsed and was left in place.", name)
	}
	// The tool's label, or an earlier version's managers own its keys.
	if !meta.HasToolLabel() && !meta.OwnsFieldPath(envkit.ToolManagers, "f:data") && !meta.OwnsFieldPath(envkit.ToolManagers, "f:stringData") {
		*logs = append(*logs, fmt.Sprintf("Kept secret %s: it was not created by this tool", name))
		return ""
	}
	*logs = append(*logs, fmt.Sprintf("Deleting secret %s...", name))
	if _, err := kube.DeleteWithUID(c, path, meta.UID); err != nil && !kube.IsK8sNotFound(err) {
		*logs = append(*logs, fmt.Sprintf("Warning: failed to delete secret: %v", err))
		return fmt.Sprintf(" The credentials secret %s could not be deleted: %v. Re-run teardown or delete it manually.", name, err)
	}
	*logs = append(*logs, "OK: Secret deleted")
	return ""
}

// dspaS3Secret is the pipeline server's copy of the S3 keys. Besides the
// tool's label it carries opendatahub.io/dashboard=true, as odh-dashboard's
// own pipeline-server secret does (configurePipelinesServer/utils.ts
// createObjectStorageSecret, assembleSecret in api/k8s/secrets.ts,
// rhoai-3.6); without a connection-type annotation the dashboard does not
// list it as a Connection (connectionTypeUtils.ts isConnection).
func dspaS3Secret(project string, keys S3Keys) map[string]any {
	labels := envkit.ToolLabels()
	labels[dashboardLabelKey] = "true"
	return map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{
			"name":      DSPASecretName,
			"namespace": project,
			"labels":    labels,
		},
		"stringData": map[string]any{
			S3AccessKeyField: keys.AccessKey,
			S3SecretKeyField: keys.SecretKey,
		},
	}
}

// dashboardLabelKey marks objects odh-dashboard shows (KnownLabels.
// DASHBOARD_RESOURCE).
const dashboardLabelKey = "opendatahub.io/dashboard"

const (
	// ConnectionSecretName is the dashboard S3 Connection the tool creates
	// next to each pipeline server: the shared S3 storage, usable from the
	// project's workbenches and model deployments.
	ConnectionSecretName = "rhoai-nightly-updater-s3"
	// connectionDisplayName is how the dashboard lists it.
	connectionDisplayName = "Shared S3 storage (RHOAI Nightly Updater)"
	// DSPAConnectionAnnotation on the DSPA names the Connection that goes
	// with it (set when the tool wrote both).
	DSPAConnectionAnnotation = "rhoai-nightly-updater/connection"
)

// dashboardS3Connection is a dashboard Connection of type s3 to the shared
// S3 storage, in the shape odh-dashboard's assembleConnectionSecret gives
// one (packages/k8s-core/src/connectionTypeUtils.ts L343-391, rhoai-3.6):
// an Opaque Secret labelled opendatahub.io/dashboard=true and, because its
// keys make it S3 model-serving compatible (S3ConnectionTypeKeys),
// opendatahub.io/managed=true; annotations openshift.io/display-name,
// openshift.io/description, opendatahub.io/connection-type-ref s3 (the
// ConfigMap manifests/base/connection-types/s3.yaml) and
// opendatahub.io/connection-type s3; the s3 type's fields as data. The
// endpoint is the in-cluster Service: it works from pods, not from outside
// the cluster.
func dashboardS3Connection(project string, keys S3Keys) map[string]any {
	labels := envkit.ToolLabels()
	labels[dashboardLabelKey] = "true"
	labels["opendatahub.io/managed"] = "true"
	return map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{
			"name":      ConnectionSecretName,
			"namespace": project,
			"labels":    labels,
			"annotations": map[string]any{
				"openshift.io/display-name":          connectionDisplayName,
				"openshift.io/description":           "The cluster's S3 storage, which this project's pipeline server also uses (set up by RHOAI Nightly Updater). In-cluster address: reachable from workbenches and model deployments, not from outside the cluster.",
				"opendatahub.io/connection-type-ref": "s3",
				"opendatahub.io/connection-type":     "s3",
			},
		},
		"stringData": map[string]any{
			S3AccessKeyField:     keys.AccessKey,
			S3SecretKeyField:     keys.SecretKey,
			"AWS_S3_ENDPOINT":    S3Endpoint(),
			"AWS_DEFAULT_REGION": S3Region,
			"AWS_S3_BUCKET":      s3Bucket,
		},
	}
}

// projectSecret is one Secret the tool writes in a pipeline project: read
// its current metadata, then write it with envkit.EnsureToolObject.
type projectSecret struct {
	c             *kube.Client
	project, name string
	what          string // "secret" or "connection", for the activity log
	obj           map[string]any
	meta          kube.ObjectMeta
	found         bool
}

// projectSecretPath is the path of one of the two Secrets the tool writes in
// a pipeline project; any other name gets the S3 secret's, so no other name
// reaches the API (and the RBAC coverage test resolves the paths
// statically). The read and write helpers take it as an argument.
func projectSecretPath(project, name string) string {
	if name == ConnectionSecretName {
		return rhoai.NamespacedPath("v1", "secrets", project, ConnectionSecretName)
	}
	return rhoai.NamespacedPath("v1", "secrets", project, DSPASecretName)
}

func readProjectSecret(s *projectSecret, path string) error {
	meta, found, err := kube.ReadObjectMeta(s.c, path)
	if err == nil {
		s.meta, s.found = meta, found
	}
	return err
}

func writeProjectSecret(s *projectSecret, path, createPath string, dryRun bool) (string, error) {
	reader := func() (kube.ObjectMeta, bool, error) { return kube.ReadObjectMeta(s.c, path) }
	action, meta, found, err := envkit.EnsureToolObject(s.c, fmt.Sprintf("Secret %s/%s", s.project, s.name), path,
		createPath, s.obj, s.meta, s.found, reader, kube.ObjectMeta.HasToolLabel, dryRun)
	s.meta, s.found = meta, found
	return action, err
}

// setDSPAOwner makes the DSPA name (uid) the owner of obj, a manifest in the
// DSPA's namespace. odh-dashboard's Delete pipeline server deletes the DSPA
// and only the secret names it generates itself (concepts/pipelines/utils.ts
// deleteServer, rhoai-3.6); with the owner reference the garbage collector
// deletes the tool's objects with the DSPA, whoever deletes it.
//
// Neither controller nor blockOwnerDeletion is set: the operator does not
// manage these objects, and blockOwnerDeletion would need update on the
// DSPA's finalizers. OwnerReferencesPermissionEnforcement lets a create set
// owner references freely and an update change them only with delete on the
// object being written (kubernetes plugin/pkg/admission/gc/gc_admission.go
// Validate), which the tool has for these Secrets by name.
func setDSPAOwner(obj map[string]any, name, uid string) {
	meta, _ := obj["metadata"].(map[string]any)
	meta["ownerReferences"] = []any{map[string]any{
		"apiVersion": DSPAAPIGroup,
		"kind":       "DataSciencePipelinesApplication",
		"name":       name,
		"uid":        uid,
	}}
}

// DSPACRDName is the DataSciencePipelinesApplication CRD.
const DSPACRDName = "datasciencepipelinesapplications.datasciencepipelinesapplications.opendatahub.io"

// dspaSpecSchema returns the OpenAPI schema of spec in the served v1 of the
// installed DSPA CRD. It lists CRDs by name (the cluster-wide list the
// diagnostics already use), so it needs no rule of its own.
func dspaSpecSchema(c *kube.Client) (map[string]any, error) {
	body, _, err := c.Do(http.MethodGet, "/apis/apiextensions.k8s.io/v1/customresourcedefinitions", "", nil,
		url.Values{"fieldSelector": {"metadata.name=" + DSPACRDName}})
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []struct {
			Spec struct {
				Versions []struct {
					Name   string `json:"name"`
					Served bool   `json:"served"`
					Schema struct {
						OpenAPIV3Schema map[string]any `json:"openAPIV3Schema"`
					} `json:"schema"`
				} `json:"versions"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, err
	}
	version := strings.TrimPrefix(DSPAAPIGroup, "datasciencepipelinesapplications.opendatahub.io/")
	for _, item := range list.Items {
		for _, v := range item.Spec.Versions {
			if v.Name != version || !v.Served {
				continue
			}
			props, _ := v.Schema.OpenAPIV3Schema["properties"].(map[string]any)
			if spec, ok := props["spec"].(map[string]any); ok {
				return spec, nil
			}
		}
	}
	return nil, fmt.Errorf("the CRD has no served %s schema", version)
}

// schemaHasField reports whether an OpenAPI object schema declares the
// field at path (nested properties).
func schemaHasField(schema map[string]any, path ...string) bool {
	node := schema
	for _, key := range path {
		props, _ := node["properties"].(map[string]any)
		next, ok := props[key].(map[string]any)
		if !ok {
			return false
		}
		node = next
	}
	return true
}

// addDashboardDSPASettings sets the fields odh-dashboard's Configure
// pipeline server sends that the tool otherwise leaves to the operator
// (configurePipelinesServer/utils.ts createDSPipelineResourceSpec with
// ConfigurePipelinesServerModal.tsx FORM_DEFAULTS, rhoai-3.6):
// apiServer.managedPipelines.enabled true and mlflow {integrationMode
// AUTODETECT, injectUserEnvVars false}. Each is set only when the installed
// CRD declares it (managedPipelines.enabled since DSPO rhoai-3.6, mlflow
// since rhoai-3.5): the strict write rejects an unknown field, and an
// omitted one gets the operator's default, which is the same value
// (api/v1 dspipeline_types.go ManagedPipelinesEnabled, MLflowConfig
// kubebuilder defaults). It returns log lines.
func addDashboardDSPASettings(c *kube.Client, spec, apiServer map[string]any) []string {
	schema, err := dspaSpecSchema(c)
	if err != nil {
		slog.Info("cannot read the DSPA CRD; leaving managed pipelines and MLflow to the operator's defaults", "error", err)
		return []string{fmt.Sprintf("Note: cannot read the DataSciencePipelinesApplication CRD (%v); managed pipelines and the MLflow integration keep the operator's defaults", err)}
	}
	if schemaHasField(schema, "apiServer", "managedPipelines", "enabled") {
		apiServer["managedPipelines"] = map[string]any{"enabled": true}
	}
	if schemaHasField(schema, "mlflow", "integrationMode") && schemaHasField(schema, "mlflow", "injectUserEnvVars") {
		spec["mlflow"] = map[string]any{"integrationMode": "AUTODETECT", "injectUserEnvVars": false}
	}
	return nil
}

// deleteConnection deletes the dashboard Connection the tool created in
// project (its label, by UID). The DSPA owns it, so the garbage collector
// would delete it too; deleting it here also covers one an interrupted
// setup left without an owner. It returns a note for the response when it
// had to be left behind on an error.
func deleteConnection(c *kube.Client, project string, logs *[]string) string {
	path := rhoai.NamespacedPath("v1", "secrets", project, ConnectionSecretName)
	meta, found, err := kube.ReadObjectMeta(c, path)
	switch {
	case err != nil:
		*logs = append(*logs, fmt.Sprintf("Warning: cannot read connection %s: %v", ConnectionSecretName, err))
		return fmt.Sprintf(" The dashboard connection %s could not be checked and was left in place: %v.", ConnectionSecretName, err)
	case !found:
		return ""
	case !meta.HasToolLabel():
		*logs = append(*logs, fmt.Sprintf("Kept secret %s: it was not created by this tool", ConnectionSecretName))
		return ""
	}
	*logs = append(*logs, fmt.Sprintf("Deleting dashboard connection %s...", ConnectionSecretName))
	if _, err := kube.DeleteWithUID(c, path, meta.UID); err != nil && !kube.IsK8sNotFound(err) {
		*logs = append(*logs, fmt.Sprintf("Warning: failed to delete connection: %v", err))
		return fmt.Sprintf(" The dashboard connection %s could not be deleted: %v. Re-run teardown or delete it manually.", ConnectionSecretName, err)
	}
	*logs = append(*logs, "OK: Connection deleted")
	return ""
}

// pipelineWriteFailure turns an envkit.EnsureToolObject error into a message and an
// error code.
func pipelineWriteFailure(what, project string, err error) (string, string) {
	var foreign *envkit.ForeignObjectError
	switch {
	case errors.As(err, &foreign):
		return fmt.Sprintf("A %s was created in %s by someone else while setup was running. It was left unchanged.", what, project), "not_managed"
	case errors.Is(err, envkit.ErrObjectChanged):
		return fmt.Sprintf("The %s in %s kept changing while setup was updating it, so it was not changed. Re-run setup.", what, project), "conflict"
	case kube.IsK8sConflict(err):
		return fmt.Sprintf("The %s in %s changed while setup was updating it, so it was not changed. Re-run setup.", what, project), "conflict"
	}
	return fmt.Sprintf("Failed to create or update the %s in %s: %v.", what, project, err), kube.ErrorCodeFromK8sErr(err)
}
