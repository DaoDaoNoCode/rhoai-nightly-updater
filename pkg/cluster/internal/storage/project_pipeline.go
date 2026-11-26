package storage

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// A pipeline server on project S3 storage (the non-admin design, 4.4): set
// up by an admin or editor of the project with their own token, on the
// project's storage only. It reads the storage's key from Secret
// nightly-s3-credentials and writes it into the pipeline server's Secret
// nightly-dspa-s3 and the dashboard Connection nightly-s3-connection; it
// never reads the shared storage, sends nothing to any S3 endpoint and
// makes no key (IAM writes are off there). It stays on the storage it was
// set up on: the shared storage's setup refuses it, and its teardown is
// this one.

// AdminUsesSharedStorage is why a cluster-admin's Set up of a pipeline
// server on project storage is refused (rule 2).
const AdminUsesSharedStorage = "A cluster-admin's pipeline servers use the cluster's shared S3 storage: set it up on the Pipelines tab. A pipeline server on a project's own storage is set up by the project's admins and editors; a cluster-admin can delete one."

// ValidateProjectPipelineServerRequest is why req cannot run, "" when it
// can.
func ValidateProjectPipelineServerRequest(req types.ProjectPipelineServerRequest) string {
	switch req.Action {
	case "setup", "teardown":
	default:
		return "action must be setup or teardown"
	}
	if len(req.Project) > 63 || !projectNameRegex.MatchString(req.Project) {
		return "a valid project is required"
	}
	if req.Action != "teardown" && len(req.ConfirmWorkbenches) > 0 {
		return "confirmWorkbenches goes with teardown"
	}
	return ""
}

// RunProjectPipelineServer sets up or deletes the pipeline server of
// req.Project on its project storage with c (the user's own client),
// streaming one step: "pipeline-server".
func RunProjectPipelineServer(c *kube.Client, req types.ProjectPipelineServerRequest, onStep func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
	emit := func(status, msg, code string) {
		if onStep != nil {
			onStep(types.UpdateStepEvent{Step: "pipeline-server", Status: status, Message: msg, ErrorCode: code})
		}
	}
	if msg := ValidateProjectPipelineServerRequest(req); msg != "" {
		emit("failed", msg, "validation")
		return &types.OperationResponse{Success: false, Message: msg, ErrorCode: "validation"}, nil
	}
	var res *types.OperationResponse
	var err error
	if req.Action == "setup" {
		emit("running", fmt.Sprintf("Setting up the pipeline server in %s on its S3 storage", req.Project), "")
		res, err = setupProjectPipelineServer(c, req.Project)
	} else {
		emit("running", fmt.Sprintf("Deleting the pipeline server in %s", req.Project), "")
		res, err = teardownProjectPipelineServer(c, req.Project, req.ConfirmWorkbenches)
	}
	if err != nil {
		return nil, err
	}
	if res.Success {
		emit("success", res.Message, "")
	} else {
		emit("failed", res.Message, res.ErrorCode)
	}
	return res, nil
}

// projectS3Connection is the dashboard Connection of a pipeline server on
// project storage: dashboardS3Connection's shape (an s3 Connection the
// dashboard lists and model serving can use) with its own name, display
// name and the project storage's address.
func projectS3Connection(project string, keys S3Keys) map[string]any {
	labels := envkit.ToolLabels()
	labels[dashboardLabelKey] = "true"
	labels["opendatahub.io/managed"] = "true"
	return map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{
			"name":      ProjectConnectionSecretName,
			"namespace": project,
			"labels":    labels,
			"annotations": map[string]any{
				"openshift.io/display-name":          projectConnectionDisplayName,
				"openshift.io/description":           "This project's own S3 storage, which its pipeline server also uses (set up by RHOAI Nightly Updater). In-cluster address: reachable from this project's workbenches and model deployments, not from outside the cluster.",
				"opendatahub.io/connection-type-ref": "s3",
				"opendatahub.io/connection-type":     "s3",
			},
		},
		"stringData": map[string]any{
			S3AccessKeyField:     keys.AccessKey,
			S3SecretKeyField:     keys.SecretKey,
			"AWS_S3_ENDPOINT":    ProjectS3Endpoint(project),
			"AWS_DEFAULT_REGION": S3Region,
			"AWS_S3_BUCKET":      s3Bucket,
		},
	}
}

// projectStorageReady says why project's storage cannot serve a pipeline
// server yet, "" when it can: recorded as the tool's, every object present
// and the Deployment ready. Read with c (the user's token).
func projectStorageReady(c *kube.Client, project string) (string, string) {
	rec, err := ReadProjectStorageRecord(c)
	if err != nil {
		return record.ErrorText(c, err), record.ErrorCode(err)
	}
	notReady := fmt.Sprintf("The S3 storage of %s is not set up or not ready. An admin of %s sets it up first (S3 storage, on this page).", project, project)
	if _, ok := rec.Get(project); !ok {
		return notReady, "prerequisites"
	}
	for _, kind := range []string{kindSecret, kindNetworkPolicy, kindService} {
		meta, found, err := readProjectS3Object(c, kind, project)
		if err != nil {
			return fmt.Sprintf("Cannot read %s %s in %s: %v.", kind, ProjectS3ObjectName(kind), project, err), kube.ErrorCodeFromK8sErr(err)
		}
		if !found || !rec.Owns(kind, meta) || meta.Terminating() {
			return notReady, "prerequisites"
		}
	}
	d, found, err := readProjectS3Deployment(c, project)
	if err != nil {
		return fmt.Sprintf("Cannot read Deployment %s in %s: %v.", ProjectS3Deployment, project, err), kube.ErrorCodeFromK8sErr(err)
	}
	if !found || !rec.Owns(kindDeployment, d.Metadata) || d.Metadata.Terminating() || !d.ready() {
		return notReady, "prerequisites"
	}
	return "", ""
}

func setupProjectPipelineServer(c *kube.Client, project string) (*types.OperationResponse, error) {
	logs := []string{}
	const op = "setup-project-pipeline-server"
	fail := func(msg, code, activity string) (*types.OperationResponse, error) {
		record.Action(c, op, fmt.Sprintf("project=%s (%s)", project, activity), false)
		return &types.OperationResponse{Success: false, Message: msg, Logs: logs, ErrorCode: code}, nil
	}
	if !c.ProjectUser() {
		return fail(AdminUsesSharedStorage+" Nothing was changed.", "not_for_cluster_admins", "cluster-admin setup")
	}
	if reason, code := envkit.SetupComponentBlock(c, envkit.PipelinesComponent); reason != "" {
		return fail(reason+" Nothing was changed.", code, envkit.SetupBlockActivity(code))
	}
	if msg, code := projectStorageReady(c, project); msg != "" {
		return fail(msg+" Nothing was changed.", code, "project storage not ready")
	}
	existing, err := ListProjectDSPAs(c, project)
	switch {
	case errors.Is(err, ErrDSPACRDMissing):
		return fail("Data Science Pipelines is not installed (no DataSciencePipelinesApplication CRD). Nothing was changed.", "prerequisites", "DSPA CRD missing")
	case err != nil:
		return fail(fmt.Sprintf("Cannot check for existing pipeline servers: %v. Nothing was changed.", err), kube.ErrorCodeFromK8sErr(err), "list failed")
	}
	rec, err := ReadPipelineRecord(c)
	if err != nil {
		return fail(record.ErrorText(c, err)+" Nothing was changed.", record.ErrorCode(err), "record read failed")
	}
	var dspaMeta kube.ObjectMeta
	dspaFound := false
	for _, d := range existing {
		switch {
		// Recorded as set up on project storage: set up again there,
		// which also points its host back at the project's storage.
		case d.Meta.Name == DSPAName && d.Meta.HasToolLabel() && rec.OwnsOnProjectStorage(d.Meta):
			if d.Meta.Terminating() {
				return fail(fmt.Sprintf("The pipeline server in %s is still being deleted. Wait for it to finish, then retry. Nothing was changed.", project), "terminating", "terminating")
			}
			dspaMeta, dspaFound = d.Meta, true
		case d.ManagedByTool() && !OnProjectStorageRecorded(d, rec):
			return fail(fmt.Sprintf("The pipeline server in %s uses the cluster's shared S3 storage and stays on it; a cluster-admin manages it on the Pipelines tab. Nothing was changed.", project), "validation", "on shared storage")
		default:
			return fail(fmt.Sprintf("Project %s already has a pipeline server (%s) that this tool did not set up on the project's storage (or has no record of). It was left unchanged.", project, d.Meta.Name), "not_managed", "other pipeline server exists")
		}
	}

	logs = append(logs, "Reading the project storage's key")
	keys, _, found, err := ReadProjectS3Keys(c, project)
	if err != nil {
		return fail(fmt.Sprintf("Cannot read Secret %s in %s: %v. Nothing was changed.", ProjectS3Secret, project, err), kube.ErrorCodeFromK8sErr(err), "storage key read failed")
	}
	if !found || keys.AccessKey == "" || keys.SecretKey == "" {
		return fail(fmt.Sprintf("Secret %s in %s holds no S3 key; an admin of %s repairs the storage first. Nothing was changed.", ProjectS3Secret, project, project), "prerequisites", "storage key empty")
	}
	secrets := []*projectSecret{
		{c: c, project: project, name: DSPASecretName, what: "secret", obj: dspaS3Secret(project, keys)},
		{c: c, project: project, name: ProjectConnectionSecretName, what: "connection", obj: projectS3Connection(project, keys)},
	}
	secretCreate := envkit.WithToolFieldManager(rhoai.NamespacedPath("v1", "secrets", project, ""))
	for _, s := range secrets {
		if err := readProjectSecret(s, projectPipelineSecretPath(project, s.name)); err != nil {
			return fail(fmt.Sprintf("Cannot read secret %s: %v. Nothing was changed.", s.name, err), kube.ErrorCodeFromK8sErr(err), s.what+" read failed")
		}
		if s.found && !s.meta.HasToolLabel() {
			return fail(fmt.Sprintf("Secret %s already exists in %s and was not created by this tool. It was left unchanged.", s.name, project), "not_managed", s.what+" exists")
		}
		if dspaFound {
			setDSPAOwner(s.obj, DSPAName, dspaMeta.UID)
		}
	}
	spec := map[string]any{
		"dspVersion": "v2",
		"objectStorage": map[string]any{
			"externalStorage": map[string]any{
				"host":   ProjectS3Host(project),
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
		// As the shared storage's pipeline servers. The fields the
		// dashboard also sets (managed pipelines, MLflow) are left to the
		// operator's defaults, which are the same values: reading the CRD
		// to check for them is a cluster-scoped read a project user may
		// not make.
		"apiServer": map[string]any{"enableSamplePipeline": false, "cacheEnabled": true, "pipelineStore": "database"},
	}
	dspa := map[string]any{
		"apiVersion": DSPAAPIGroup,
		"kind":       "DataSciencePipelinesApplication",
		"metadata": map[string]any{
			"name":        DSPAName,
			"namespace":   project,
			"labels":      envkit.ToolLabels(),
			"annotations": map[string]any{DSPAConnectionAnnotation: ProjectConnectionSecretName},
		},
		"spec": spec,
	}
	dspaPath := rhoai.NamespacedPath(DSPAAPIGroup, "datasciencepipelinesapplications", project, DSPAName)
	readDSPA := func() (kube.ObjectMeta, bool, error) { return kube.ReadObjectMeta(c, dspaPath) }
	owned := func(m kube.ObjectMeta) bool { return m.HasToolLabel() && rec.OwnsOnProjectStorage(m) }
	writeDSPA := func(dryRun bool) (string, error) {
		action, meta, found, err := envkit.EnsureToolObject(c, fmt.Sprintf("DataSciencePipelinesApplication %s/%s", project, DSPAName), dspaPath,
			envkit.WithToolFieldManager(rhoai.NamespacedPath(DSPAAPIGroup, "datasciencepipelinesapplications", project, ""))+"&fieldValidation=Strict",
			dspa, dspaMeta, dspaFound, readDSPA, owned, dryRun)
		dspaMeta, dspaFound = meta, found
		return action, err
	}
	logs = append(logs, "Checking the secrets and the pipeline server with the API server (dry run)")
	for _, s := range secrets {
		if _, err := writeProjectSecret(s, projectPipelineSecretPath(project, s.name), secretCreate, true); err != nil {
			msg, code := pipelineWriteFailure("secret "+s.name, project, err)
			return fail(msg+" Nothing was changed.", code, s.what+" dry run failed")
		}
	}
	if _, err := writeDSPA(true); err != nil {
		msg, code := pipelineWriteFailure("pipeline server "+DSPAName, project, err)
		return fail(msg+" Nothing was changed.", code, "DSPA dry run failed")
	}

	// The writes. What this run creates is deleted again (by UID) when a
	// later write or the record fails.
	// made is an object this run created: a Secret by name, or the DSPA
	// (secret "").
	type made struct{ secret, uid, name string }
	var created []made
	rollback := func() string {
		var kept []string
		for i := len(created) - 1; i >= 0; i-- {
			m := created[i]
			if record.OperationLockLost(c) {
				kept = append(kept, m.name)
				continue
			}
			ctx, cancel := record.PostOperationContext(c, afterWriteBudget)
			path := dspaPath
			if m.secret != "" {
				path = projectPipelineSecretPath(project, m.secret)
			}
			_, err := kube.DeleteWithUID(c.WithContext(ctx), path, m.uid)
			cancel()
			if err != nil && !kube.IsK8sNotFound(err) {
				kept = append(kept, m.name)
			}
		}
		if len(kept) == 0 {
			return " What this run created was deleted again."
		}
		return fmt.Sprintf(" %s stayed; delete the pipeline server here to remove %s.", strings.Join(kept, ", "), kube.Verb(len(kept), "it", "them"))
	}
	for _, s := range secrets {
		action, err := writeProjectSecret(s, projectPipelineSecretPath(project, s.name), secretCreate, false)
		if err != nil {
			msg, code := pipelineWriteFailure("secret "+s.name, project, err)
			return fail(msg+rollback(), code, s.what+" write failed")
		}
		if action == "created" {
			created = append(created, made{s.name, s.meta.UID, "Secret " + s.name})
		}
		logs = append(logs, fmt.Sprintf("OK: Secret %s %s", s.name, action))
	}
	logs = append(logs, "Writing the pipeline server (DSPA)")
	dspaAction, err := writeDSPA(false)
	if err != nil {
		msg, code := pipelineWriteFailure("pipeline server "+DSPAName, project, err)
		return fail(msg+rollback(), code, "DSPA write failed")
	}
	if dspaAction == "created" {
		created = append(created, made{"", dspaMeta.UID, "DataSciencePipelinesApplication " + DSPAName})
	}
	logs = append(logs, "OK: Pipeline server "+dspaAction)
	ctx, cancel := record.PostOperationContext(c, afterWriteBudget)
	err = recordProjectPipelineServer(c.WithContext(ctx), dspaMeta)
	cancel()
	if err != nil {
		return fail(fmt.Sprintf("The pipeline server could not be recorded as this tool's (%s).%s", record.ErrorText(c, err), rollback()), record.ErrorCode(err), "record failed")
	}
	// The DSPA owns its Secrets, so the garbage collector deletes them
	// with it, also when the dashboard deletes it.
	for _, s := range secrets {
		if len(s.meta.OwnerReferences) == 1 && s.meta.OwnerReferences[0].UID == dspaMeta.UID {
			continue
		}
		setDSPAOwner(s.obj, DSPAName, dspaMeta.UID)
		if _, err := writeProjectSecret(s, projectPipelineSecretPath(project, s.name), secretCreate, false); err != nil {
			record.Action(c, op, fmt.Sprintf("project=%s (%s owner not set: %v)", project, s.what, err), false)
			return &types.OperationResponse{Success: false, ErrorCode: "partial_failure", Logs: logs,
				Message: fmt.Sprintf("Pipeline server set up, but secret %s could not be linked to it: %v. Deleting the pipeline server in the RHOAI dashboard would leave the secret behind; delete it here instead.", s.name, err)}, nil
		}
	}
	slog.Info("project pipeline server setup", "project", project, "user", kube.GetUser(c))
	record.Action(c, op, fmt.Sprintf("project=%s dspa=%s", project, DSPAName), true)
	return &types.OperationResponse{Success: true, Logs: logs,
		Message: fmt.Sprintf("Pipeline server %s set up in %s on the project's own S3 storage, with dashboard connection %s. It will take 1-3 minutes to become ready.", DSPAName, project, ProjectConnectionSecretName)}, nil
}

// recordProjectPipelineServer records meta, a pipeline server on project
// storage, as the tool's (one read-change-write through
// record.UpdateSnapshot: project actions in other projects write the same
// key).
func recordProjectPipelineServer(c *kube.Client, meta kube.ObjectMeta) error {
	if meta.UID == "" {
		return fmt.Errorf("record this tool's pipeline servers: %s/%s has no UID", meta.Namespace, meta.Name)
	}
	return record.UpdateSnapshot(c, func() error {
		rec, err := ReadPipelineRecord(c)
		if err != nil {
			return err
		}
		rec[meta.Namespace+"/"+meta.Name] = RecordedPipelineServer{UID: meta.UID, At: kube.NowRFC3339(), By: kube.GetUser(c), Storage: RecordedOnProjectStorage, ActorRole: record.ActorRole(c), ProjectUID: c.ProjectUID()}
		return WritePipelineRecord(c, rec)
	})
}

// forgetProjectPipelineServer drops project's pipeline server on project
// storage from the record (none recorded, or one on the shared storage:
// nothing is written).
func forgetProjectPipelineServer(c *kube.Client, project string) error {
	return record.UpdateSnapshot(c, func() error {
		rec, err := ReadPipelineRecord(c)
		if err != nil {
			return err
		}
		changed := false
		for _, name := range []string{DSPAName, PreviousDSPAName} {
			if e, ok := rec[project+"/"+name]; ok && e.Storage == RecordedOnProjectStorage {
				delete(rec, project+"/"+name)
				changed = true
			}
		}
		if !changed {
			return nil
		}
		return WritePipelineRecord(c, rec)
	})
}

// teardownProjectPipelineServer deletes project's pipeline server on its
// project storage, which the tool recorded, with c: its dashboard
// Connection comes off the confirmed workbenches first (as for one on the
// shared storage), then the DSPA goes by UID with its Secret and
// Connection. It never touches the shared storage. Deleting it does not
// first check that the data-science-pipelines operator runs (a project
// user cannot read its Deployment); a DSPA still terminating is reported.
func teardownProjectPipelineServer(c *kube.Client, project string, confirmedWorkbenches map[string]string) (*types.OperationResponse, error) {
	logs := []string{}
	const op = "teardown-project-pipeline-server"
	fail := func(msg, code, activity string) (*types.OperationResponse, error) {
		record.Action(c, op, fmt.Sprintf("project=%s (%s)", project, activity), false)
		return &types.OperationResponse{Success: false, Message: msg, Logs: logs, ErrorCode: code}, nil
	}
	all, err := ListProjectDSPAs(c, project)
	if errors.Is(err, ErrDSPACRDMissing) {
		all, err = nil, nil
	}
	if err != nil {
		return fail(fmt.Sprintf("Cannot list pipeline servers in %s; nothing was deleted: %v", project, err), kube.ErrorCodeFromK8sErr(err), "list failed")
	}
	rec, err := ReadPipelineRecord(c)
	if err != nil {
		return fail(fmt.Sprintf("Cannot read this tool's record of its pipeline servers (%s). Nothing was deleted.", record.ErrorText(c, err)), record.ErrorCode(err), "record read failed")
	}
	var target *DSPA
	for i := range all {
		if all[i].Meta.Name == DSPAName && all[i].Meta.HasToolLabel() && OnProjectStorageRecorded(all[i], rec) {
			target = &all[i]
		}
	}
	if target == nil {
		// A previous run may have deleted the DSPA but not its
		// Connection.
		wbs, problem, code := connectionWorkbenchesToDetach(c, project, ProjectConnectionSecretName, projectPipelineSecretPath(project, ProjectConnectionSecretName), confirmedWorkbenches)
		if problem != "" {
			return fail(problem, code, "workbenches not confirmed")
		}
		if msg, code := detachConnection(c, project, wbs, &logs); msg != "" {
			return fail(msg, code, "workbench change failed")
		}
		note := deleteConnection(c, ProjectConnectionSecretName, projectPipelineSecretPath(project, ProjectConnectionSecretName), &logs)
		if err := forgetProjectPipelineServer(c, project); err != nil {
			logs = append(logs, "Warning: this tool's record of the pipeline server could not be cleared: "+record.ErrorText(c, err))
		}
		if note != "" {
			return fail(fmt.Sprintf("No pipeline server of this tool on project storage remains in %s.%s", project, note), "partial_failure", "orphan connection")
		}
		record.Action(c, op, fmt.Sprintf("project=%s (none)", project), true)
		return &types.OperationResponse{Success: true, Message: fmt.Sprintf("No pipeline server of this tool on project storage remains in %s.%s", project, envOnlyNote(project, wbs)), Logs: logs}, nil
	}
	if !rec.OwnsOnProjectStorage(target.Meta) {
		return fail(fmt.Sprintf("Pipeline server %s in %s carries this tool's label, but this tool has no record that it set it up (or it was deleted and made again). It was left unchanged; delete it in the RHOAI dashboard. Nothing was deleted.", target.Meta.Name, project), "not_managed", "unrecorded")
	}
	wbs, problem, code := connectionWorkbenchesToDetach(c, project, ProjectConnectionSecretName, projectPipelineSecretPath(project, ProjectConnectionSecretName), confirmedWorkbenches)
	if problem != "" {
		return fail(problem, code, "workbenches not confirmed")
	}
	if msg, code := detachConnection(c, project, wbs, &logs); msg != "" {
		return fail(msg, code, "workbench change failed")
	}
	dspaPath := rhoai.NamespacedPath(DSPAAPIGroup, "datasciencepipelinesapplications", project, DSPAName)
	if !target.Meta.Terminating() {
		logs = append(logs, fmt.Sprintf("Deleting pipeline server %s", target.Meta.Name))
		if _, err := kube.DeleteWithUID(c, dspaPath, target.Meta.UID); err != nil && !kube.IsK8sNotFound(err) {
			code := kube.ErrorCodeFromK8sErr(err)
			if kube.IsK8sConflict(err) {
				code = "conflict"
			}
			return fail(fmt.Sprintf("Failed to delete pipeline server: %v", err), code, "DSPA delete failed")
		}
		logs = append(logs, "OK: Deletion requested")
	}
	note := deleteProjectPipelineSecret(c, project, &logs) + deleteConnection(c, ProjectConnectionSecretName, projectPipelineSecretPath(project, ProjectConnectionSecretName), &logs)
	gone, waitErr := kube.WaitForDeletion(c, dspaPath, pipelineServerDeleteTimeout, pipelineServerDeletePoll)
	if waitErr != nil || !gone {
		why := fmt.Sprintf("it is still terminating after %s (waiting for the data-science-pipelines operator to finish cleanup)", pipelineServerDeleteTimeout)
		if waitErr != nil {
			why = fmt.Sprintf("whether it is gone cannot be checked: %v", waitErr)
		}
		record.Action(c, op, fmt.Sprintf("project=%s (not gone)", project), false)
		return &types.OperationResponse{Success: false, Logs: logs, ErrorCode: "in_progress",
			Message: fmt.Sprintf("Deletion of pipeline server %s was requested, but %s. The status shows it until it is gone.%s", target.Meta.Name, why, note)}, nil
	}
	if err := forgetProjectPipelineServer(c, project); err != nil {
		logs = append(logs, "Warning: this tool's record of the pipeline server could not be cleared: "+record.ErrorText(c, err))
	}
	slog.Info("project pipeline server teardown", "project", project, "user", kube.GetUser(c))
	record.Action(c, op, fmt.Sprintf("project=%s dspa=%s", project, target.Meta.Name), note == "")
	msg := fmt.Sprintf("Pipeline server removed from %s; the project's S3 storage and its data stay.", project) + envOnlyNote(project, wbs)
	if !target.ExternalDB {
		msg += fmt.Sprintf(" Its database PVC 'mariadb-%s' (pipeline runs and experiments) was deleted with it.", target.Meta.Name)
	}
	if note != "" {
		return &types.OperationResponse{Success: false, Message: msg + note, Logs: logs, ErrorCode: "partial_failure"}, nil
	}
	return &types.OperationResponse{Success: true, Message: msg, Logs: logs}, nil
}

// projectPipelineSecretPath is the path of one of the two Secrets a
// pipeline server on project storage has: nightly-dspa-s3, or (any other
// name) its Connection nightly-s3-connection. Literal names only.
func projectPipelineSecretPath(project, name string) string {
	if name == DSPASecretName {
		return rhoai.NamespacedPath("v1", "secrets", project, DSPASecretName)
	}
	return rhoai.NamespacedPath("v1", "secrets", project, ProjectConnectionSecretName)
}

// deleteProjectPipelineSecret deletes Secret nightly-dspa-s3 of a pipeline
// server on project storage when it carries the tool's label (by UID). It
// returns a note for the response when it had to be left behind.
func deleteProjectPipelineSecret(c *kube.Client, project string, logs *[]string) string {
	path := projectPipelineSecretPath(project, DSPASecretName)
	meta, found, err := kube.ReadObjectMeta(c, path)
	switch {
	case err != nil:
		*logs = append(*logs, fmt.Sprintf("Warning: cannot read secret %s: %v", DSPASecretName, err))
		return fmt.Sprintf(" The credentials secret %s could not be checked and was left in place: %v.", DSPASecretName, err)
	case !found:
		return ""
	case !meta.HasToolLabel():
		*logs = append(*logs, fmt.Sprintf("Kept secret %s: it was not created by this tool", DSPASecretName))
		return ""
	}
	*logs = append(*logs, "Deleting secret "+DSPASecretName)
	if _, err := kube.DeleteWithUID(c, path, meta.UID); err != nil && !kube.IsK8sNotFound(err) {
		*logs = append(*logs, fmt.Sprintf("Warning: failed to delete secret: %v", err))
		return fmt.Sprintf(" The credentials secret %s could not be deleted: %v. Delete again, or delete it yourself.", DSPASecretName, err)
	}
	return ""
}
