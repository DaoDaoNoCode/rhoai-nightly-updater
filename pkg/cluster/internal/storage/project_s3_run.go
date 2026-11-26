package storage

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/authz"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The steps of a project storage action.
const (
	ProjectS3StepCheck   = "check"
	ProjectS3StepStorage = "storage"
	ProjectS3StepFence   = "fence"
	ProjectS3StepServer  = "server"
	ProjectS3StepReady   = "ready"
	ProjectS3StepDelete  = "delete"
)

// ErrorCodeProjectStorageInUse refuses a delete while a pipeline server
// uses the storage.
const ErrorCodeProjectStorageInUse = "in_use"

var projectNameRegex = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// ValidateProjectStorageRequest is why req cannot run, "" when it can.
func ValidateProjectStorageRequest(req types.ProjectStorageRequest) string {
	switch req.Action {
	case "setup", "repair", "delete":
	default:
		return "action must be setup, repair or delete"
	}
	if len(req.Project) > 63 || !projectNameRegex.MatchString(req.Project) {
		return "a valid project is required"
	}
	if req.DeleteData != "" && (req.Action != "delete" || req.DeleteData != ProjectS3PVC) {
		return fmt.Sprintf("deleteData goes with delete and names the storage's volume %s", ProjectS3PVC)
	}
	return ""
}

// projectS3Run is one project storage action.
type projectS3Run struct {
	c        *kube.Client
	project  string
	emit     func(types.UpdateStepEvent)
	logs     []string
	activity string
}

func (r *projectS3Run) step(id, status, msg string) {
	r.logs = append(r.logs, msg)
	if r.emit != nil {
		r.emit(types.UpdateStepEvent{Step: id, Status: status, Message: msg})
	}
}

func (r *projectS3Run) fail(id, msg, code, why string) (*types.OperationResponse, error) {
	if r.emit != nil {
		r.emit(types.UpdateStepEvent{Step: id, Status: "failed", Message: msg, ErrorCode: code})
	}
	r.logs = append(r.logs, msg)
	slog.Warn("project storage action stopped", "step", id, "reason", why)
	record.Action(r.c, r.activity, fmt.Sprintf("project=%s (%s)", r.project, why), false)
	return &types.OperationResponse{Success: false, Message: msg, Logs: r.logs, ErrorCode: code}, nil
}

func (r *projectS3Run) done(msg string) (*types.OperationResponse, error) {
	record.Action(r.c, r.activity, "project="+r.project, true)
	return &types.OperationResponse{Success: true, Message: msg, Logs: r.logs}, nil
}

// afterWriteBudget bounds each record write or undo after a create, on a
// post-operation context of its own (as the workbench's).
var afterWriteBudget = 15 * time.Second

func (r *projectS3Run) recordMade(write func(c *kube.Client) error) error {
	ctx, cancel := record.PostOperationContext(r.c, afterWriteBudget)
	defer cancel()
	return write(r.c.WithContext(ctx))
}

func (r *projectS3Run) undo(del func(c *kube.Client) error) error {
	if record.OperationLockLost(r.c) {
		return record.ErrOperationLockLost
	}
	ctx, cancel := record.PostOperationContext(r.c, afterWriteBudget)
	defer cancel()
	return del(r.c.WithContext(ctx))
}

// AdminCannotCreateProjectStorage is why a cluster-admin's Set up of
// project storage is refused (the owner's decision: no duplication of the
// shared storage).
const AdminCannotCreateProjectStorage = "A cluster-admin's pipeline servers use the cluster's shared S3 storage, so a cluster-admin does not set up project S3 storage: an admin of the project does. A cluster-admin can repair or delete one."

// RunProjectStorage sets up, repairs or deletes the S3 storage of
// req.Project with c, the user's own client (a project admin's, or a
// cluster-admin's for repair and delete), streaming its steps: setup and
// repair "check", "storage", "fence", "server", "ready"; delete "check",
// "delete".
func RunProjectStorage(c *kube.Client, req types.ProjectStorageRequest, onStep func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
	r := &projectS3Run{c: c, project: req.Project, emit: onStep, activity: req.Action + "-project-s3"}
	if msg := ValidateProjectStorageRequest(req); msg != "" {
		return r.fail(ProjectS3StepCheck, msg, "validation", "invalid request")
	}
	if req.Action == "delete" {
		return r.delete(req.DeleteData == ProjectS3PVC)
	}
	return r.apply(req.Action == "repair")
}

// liveObject is one object of the storage as read.
type liveObject struct {
	meta  kube.ObjectMeta
	found bool
}

// readAll reads every object of the storage with the user's token and
// checks that each one present is the tool's (its recorded UID): a
// refusal message and code otherwise.
func (r *projectS3Run) readAll(rec ProjectStorageRecord) (map[string]*liveObject, string, string) {
	out := map[string]*liveObject{}
	for _, kind := range ProjectS3Kinds {
		meta, found, err := readProjectS3Object(r.c, kind, r.project)
		if err != nil {
			return nil, fmt.Sprintf("Cannot read %s %s in %s: %v.", kind, ProjectS3ObjectName(kind), r.project, err), kube.ErrorCodeFromK8sErr(err)
		}
		if found && !rec.Owns(kind, meta) {
			return nil, fmt.Sprintf("%s %s already exists in %s and was not created by this tool (or was deleted and made again), so it is left unchanged. Delete or rename it, then try again.", kind, ProjectS3ObjectName(kind), r.project), "not_managed"
		}
		if found && meta.Terminating() {
			return nil, fmt.Sprintf("%s %s in %s is being deleted; try again once it is gone.", kind, ProjectS3ObjectName(kind), r.project), "terminating"
		}
		out[kind] = &liveObject{meta: meta, found: found}
	}
	return out, "", ""
}

// apply is Set up (repair false) and Repair: it creates what is missing
// and applies again what exists (each object's recorded UID proves it is
// the tool's), NetworkPolicy before the Deployment. Every write is
// dry-run first. A failure after a create deletes what this run created
// again (by UID) or leaves it recorded as the tool's: nothing this run
// made is left unrecorded.
func (r *projectS3Run) apply(repair bool) (*types.OperationResponse, error) {
	c, project := r.c, r.project
	what := "Set up"
	if repair {
		what = "Repair"
	}
	r.step(ProjectS3StepCheck, "running", fmt.Sprintf("Checking the S3 storage of %s", project))
	apps, err := projectAppsNamespace(c)
	if err != nil {
		return r.fail(ProjectS3StepCheck, fmt.Sprintf("Cannot set the storage's NetworkPolicy, which lets in RHOAI's applications namespace: %v. Nothing was changed.", err), "prerequisites_unknown", "applications namespace unknown")
	}
	rec, err := ReadProjectStorageRecord(c)
	if err != nil {
		return r.fail(ProjectS3StepCheck, record.ErrorText(c, err)+" Nothing was changed.", record.ErrorCode(err), "record read failed")
	}
	live, msg, code := r.readAll(rec)
	if msg != "" {
		return r.fail(ProjectS3StepCheck, msg+" Nothing was changed.", code, "objects not this tool's")
	}
	present := 0
	for _, o := range live {
		if o.found {
			present++
		}
	}
	entry, recorded := rec.Get(project)
	switch {
	case !c.ProjectUser() && (!repair || present == 0):
		if !repair {
			return r.fail(ProjectS3StepCheck, AdminCannotCreateProjectStorage+" Nothing was changed.", "not_for_cluster_admins", "cluster-admin setup")
		}
		return r.fail(ProjectS3StepCheck, fmt.Sprintf("%s has no S3 storage of this tool to repair. Nothing was changed.", project), "validation", "nothing to repair")
	case repair && present == 0:
		return r.fail(ProjectS3StepCheck, fmt.Sprintf("%s has no S3 storage of this tool to repair; use Set up. Nothing was changed.", project), "validation", "nothing to repair")
	}
	if present < len(ProjectS3Kinds) && c.ProjectUser() {
		var prior *record.RecordOwner
		if recorded {
			prior = &record.RecordOwner{Key: ProjectS3RecordKey, Project: project, ProjectUID: entry.ProjectUID, By: entry.By, ActorRole: entry.ActorRole}
		}
		if err := record.AdmitUserEntryFor(c, ProjectS3RecordKey, project, prior, nil); err != nil {
			return r.fail(ProjectS3StepCheck, record.ErrorText(c, err)+" Nothing was changed.", record.ErrorCode(err), "records full")
		}
	}
	keys := S3Keys{}
	if live[kindSecret].found {
		k, _, _, err := ReadProjectS3Keys(c, project)
		if err != nil {
			return r.fail(ProjectS3StepCheck, fmt.Sprintf("Cannot read Secret %s in %s: %v. Nothing was changed.", ProjectS3Secret, project, err), kube.ErrorCodeFromK8sErr(err), "secret read failed")
		}
		keys = k
	}
	if keys.AccessKey == "" || keys.SecretKey == "" || keys.AdminPassword == "" || keys.ChecksumKey == "" {
		fresh, err := newProjectS3Keys()
		if err != nil {
			return r.fail(ProjectS3StepCheck, fmt.Sprintf("Cannot make a key: %v. Nothing was changed.", err), "internal", "key generation failed")
		}
		// A key the storage already serves stays: only what is missing
		// is made (a Secret without a key, or the first setup).
		if keys.AccessKey == "" || keys.SecretKey == "" {
			keys.AccessKey, keys.SecretKey = fresh.AccessKey, fresh.SecretKey
		}
		keys.AdminPassword = kube.FirstNonEmpty(keys.AdminPassword, fresh.AdminPassword)
		keys.ChecksumKey = kube.FirstNonEmpty(keys.ChecksumKey, fresh.ChecksumKey)
	}
	manifests := ProjectS3Manifests(project, apps, keys)
	owned := func(kind string) func(kube.ObjectMeta) bool {
		return func(m kube.ObjectMeta) bool { return rec.Owns(kind, m) }
	}
	write := func(kind string, dryRun bool) (string, error) {
		o := live[kind]
		reader := func() (kube.ObjectMeta, bool, error) { return readProjectS3Object(c, kind, project) }
		action, meta, found, err := envkit.EnsureToolObject(c, fmt.Sprintf("%s %s/%s", kind, project, ProjectS3ObjectName(kind)), projectS3Path(kind, project),
			projectS3CreatePath(kind, project), manifests[kind], o.meta, o.found, reader, owned(kind), dryRun)
		o.meta, o.found = meta, found
		return action, err
	}
	for _, kind := range ProjectS3Kinds {
		if _, err := write(kind, true); err != nil {
			msg, code := pipelineWriteFailure(fmt.Sprintf("%s %s", kind, ProjectS3ObjectName(kind)), project, err)
			return r.fail(ProjectS3StepCheck, "The API server refuses the storage: "+msg+" Nothing was changed.", code, kind+" dry run failed")
		}
	}
	r.step(ProjectS3StepCheck, "success", fmt.Sprintf("OK: The API server accepts the storage (RHOAI's applications namespace: %s)", apps))

	// The writes, each recorded right after its create.
	var created []string
	// rollback deletes what this run created, newest first. The
	// Deployment goes in the foreground and its pod must be gone before
	// the NetworkPolicy goes: while that is not certain, the policy stays
	// (recorded), so the pod never runs unfenced.
	rollback := func() string {
		var kept []string
		serverMayRun := false
		for i := len(created) - 1; i >= 0; i-- {
			kind := created[i]
			uid := live[kind].meta.UID
			if kind == kindNetworkPolicy && serverMayRun {
				kept = append(kept, fmt.Sprintf("%s %s", kind, ProjectS3ObjectName(kind)))
				continue
			}
			err := r.undo(func(dc *kube.Client) error {
				policy := "Background"
				if kind == kindDeployment {
					policy = "Foreground"
				}
				_, err := kube.DeleteWithUIDPropagation(dc, projectS3Path(kind, project), uid, policy)
				if err != nil && !kube.IsK8sNotFound(err) {
					return err
				}
				if kind == kindDeployment {
					gone, err := kube.WaitForGone(dc, projectS3GoneTimeout, projectS3GonePoll, func(cc *kube.Client) (bool, error) { return projectS3ServerGone(cc, project) })
					if err != nil {
						return err
					}
					if !gone {
						return errProjectS3ServerRunning
					}
				}
				return updateProjectStorageRecord(dc, project, func(e *ProjectStorageRecorded) {
					if e.UID(kind) == uid {
						e.setUID(kind, "")
					}
				})
			})
			if err != nil {
				kept = append(kept, fmt.Sprintf("%s %s", kind, ProjectS3ObjectName(kind)))
				if kind == kindDeployment {
					serverMayRun = true
				}
			}
		}
		if len(kept) == 0 {
			return " What this run created was deleted again."
		}
		return fmt.Sprintf(" %s stayed, recorded as this tool's: Delete removes %s.", strings.Join(kept, ", "), kube.Verb(len(kept), "it", "them"))
	}
	stepOf := map[string]string{kindPVC: ProjectS3StepStorage, kindSecret: ProjectS3StepStorage, kindNetworkPolicy: ProjectS3StepFence, kindDeployment: ProjectS3StepServer, kindService: ProjectS3StepServer}
	for _, kind := range ProjectS3Kinds {
		id := stepOf[kind]
		name := ProjectS3ObjectName(kind)
		r.step(id, "running", fmt.Sprintf("Writing %s %s", kind, name))
		action, err := write(kind, false)
		if err != nil {
			msg, code := pipelineWriteFailure(fmt.Sprintf("%s %s", kind, name), project, err)
			return r.fail(id, msg+rollback(), code, kind+" write failed")
		}
		if action == "created" {
			meta := live[kind].meta
			created = append(created, kind)
			err := r.recordMade(func(rc *kube.Client) error {
				return updateProjectStorageRecord(rc, project, func(e *ProjectStorageRecorded) {
					e.setUID(kind, meta.UID)
					e.At, e.By, e.ActorRole, e.ProjectUID = kube.NowRFC3339(), kube.GetUser(c), record.ActorRole(c), c.ProjectUID()
				})
			})
			if err != nil {
				created = created[:len(created)-1]
				undoErr := r.undo(func(dc *kube.Client) error {
					_, err := kube.DeleteWithUID(dc, projectS3Path(kind, project), meta.UID)
					return err
				})
				if undoErr != nil && !kube.IsK8sNotFound(undoErr) {
					return r.fail(id, fmt.Sprintf("%s %s was created, but this tool could not record it (%s) nor delete it again (%v). Delete it yourself (oc delete %s %s -n %s).%s", kind, name, record.ErrorText(c, err), undoErr, strings.ToLower(kind), name, project, rollback()), record.ErrorCode(err), "record failed")
				}
				return r.fail(id, fmt.Sprintf("%s %s was created, but this tool could not record it as its own (%s), so it was deleted again.%s", kind, name, record.ErrorText(c, err), rollback()), record.ErrorCode(err), "record failed")
			}
		}
		// The fence is confirmed before the pod may start: read back as
		// written.
		if kind == kindNetworkPolicy {
			body, found, err := c.GetOptional(projectS3Path(kindNetworkPolicy, project))
			var problem string
			switch {
			case err != nil:
				problem = fmt.Sprintf("it cannot be read back: %v", err)
			case !found:
				problem = "it is gone"
			default:
				problem = projectS3PolicyProblem(body, apps)
			}
			if problem != "" {
				return r.fail(id, fmt.Sprintf("NetworkPolicy %s was written but is not in place (%s), so the storage's pod was not started.%s", ProjectS3NetworkPolicy, problem, rollback()), "conflict", "fence not confirmed")
			}
		}
		r.step(id, "success", fmt.Sprintf("OK: %s %s %s", kind, name, action))
	}

	// The pod: ready, or why not.
	r.step(ProjectS3StepReady, "running", "Waiting for the storage's pod to be ready")
	var last projectS3Deployment
	var failure string
	ready, err := kube.WaitForGone(c, ProjectS3ReadyTimeout, projectS3ReadyPoll, func(cc *kube.Client) (bool, error) {
		d, found, err := readProjectS3Deployment(cc, project)
		if err != nil || !found {
			return false, err
		}
		last = d
		if f := d.replicaFailure(); f != "" && definiteRefusal(f) {
			failure = f
			return true, nil
		}
		return d.ready(), nil
	})
	switch {
	case failure != "":
		return r.fail(ProjectS3StepReady, fmt.Sprintf("The storage's pod cannot be created in %s: %s.%s", project, failure, rollback()), "forbidden", "pod refused")
	case err != nil:
		return r.fail(ProjectS3StepReady, fmt.Sprintf("Cannot read whether the storage is ready: %v. It stays, recorded as this tool's; its status shows when it is.", err), kube.ErrorCodeFromK8sErr(err), "readiness unreadable")
	case !ready:
		why := kube.FirstNonEmpty(last.replicaFailure(), "the pod is not ready yet")
		return r.fail(ProjectS3StepReady, fmt.Sprintf("The storage is not ready after %s (%s). It stays, recorded as this tool's; its status shows when it is ready, and Delete removes it.", ProjectS3ReadyTimeout, why), "in_progress", "not ready")
	}
	r.step(ProjectS3StepReady, "success", "OK: The storage is ready")
	slog.Info("project storage applied", "project", project, "repair", repair, "user", kube.GetUser(c))
	return r.done(fmt.Sprintf("%s done: S3 storage %s in %s is ready (bucket %s). Set up its pipeline server next.", what, ProjectS3Deployment, project, s3Bucket))
}

// projectStorageUsers are the pipeline servers of project that use its
// storage, read with c; an error fails closed.
func projectStorageUsers(c *kube.Client, project string) ([]string, error) {
	dspas, err := ListProjectDSPAs(c, project)
	if errors.Is(err, ErrDSPACRDMissing) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, d := range dspas {
		if OnProjectStorage(d) {
			out = append(out, d.Meta.Name)
		}
	}
	slices.Sort(out)
	return out, nil
}

// delete deletes the project's storage objects the tool recorded, by UID:
// the Deployment first and, once it (and so its pod) is gone, the
// Service, the Secret and last the NetworkPolicy, so the pod never runs
// unfenced; the volume only with deleteData (the user typed its name). It
// refuses while a pipeline server uses the storage (fail closed).
func (r *projectS3Run) delete(deleteData bool) (*types.OperationResponse, error) {
	c, project := r.c, r.project
	r.step(ProjectS3StepCheck, "running", fmt.Sprintf("Checking the S3 storage of %s", project))
	rec, err := ReadProjectStorageRecord(c)
	if err != nil {
		return r.fail(ProjectS3StepCheck, record.ErrorText(c, err)+" Nothing was deleted.", record.ErrorCode(err), "record read failed")
	}
	users, err := projectStorageUsers(c, project)
	if err != nil {
		return r.fail(ProjectS3StepCheck, fmt.Sprintf("Cannot check which pipeline servers in %s use the storage: %v. Nothing was deleted.", project, err), kube.ErrorCodeFromK8sErr(err), "pipeline servers unreadable")
	}
	if len(users) > 0 {
		return r.fail(ProjectS3StepCheck, fmt.Sprintf("Pipeline server %s in %s uses this storage; delete the pipeline server first. Nothing was deleted.", strings.Join(users, ", "), project), ErrorCodeProjectStorageInUse, "in use")
	}
	live := map[string]*liveObject{}
	var kept []string
	for _, kind := range ProjectS3Kinds {
		meta, found, err := readProjectS3Object(c, kind, project)
		if err != nil {
			return r.fail(ProjectS3StepCheck, fmt.Sprintf("Cannot read %s %s in %s: %v. Nothing was deleted.", kind, ProjectS3ObjectName(kind), project, err), kube.ErrorCodeFromK8sErr(err), "read failed")
		}
		if found && !rec.Owns(kind, meta) {
			kept = append(kept, fmt.Sprintf("%s %s (not created by this tool)", kind, ProjectS3ObjectName(kind)))
			found = false
			// A Deployment of that name that is not the recorded one may
			// run a pod the NetworkPolicy fences: the policy stays while
			// it exists (checked below).
		}
		live[kind] = &liveObject{meta: meta, found: found}
	}
	if _, recorded := rec.Get(project); !recorded {
		if len(kept) > 0 {
			return r.fail(ProjectS3StepCheck, fmt.Sprintf("This tool has no S3 storage in %s; %s %s left unchanged. Nothing was deleted.", project, strings.Join(kept, ", "), kube.Verb(len(kept), "is", "are")), "not_managed", "not this tool's")
		}
		r.step(ProjectS3StepCheck, "success", "No S3 storage of this tool in this project")
		return r.done(fmt.Sprintf("No S3 storage of this tool remains in %s.", project))
	}
	if !deleteData && live[kindPVC].found {
		kept = append(kept, fmt.Sprintf("PersistentVolumeClaim %s (its data; delete it by typing its name)", ProjectS3PVC))
	}
	r.step(ProjectS3StepCheck, "success", "OK: The storage is this tool's and no pipeline server uses it")

	del := func(kind, policy string) error {
		o := live[kind]
		if !o.found {
			return nil
		}
		r.step(ProjectS3StepDelete, "running", fmt.Sprintf("Deleting %s %s", kind, ProjectS3ObjectName(kind)))
		if _, err := kube.DeleteWithUIDPropagation(c, projectS3Path(kind, project), o.meta.UID, policy); err != nil && !kube.IsK8sNotFound(err) {
			return fmt.Errorf("delete %s %s: %w", kind, ProjectS3ObjectName(kind), err)
		}
		return nil
	}
	forget := func(kinds ...string) error {
		return updateProjectStorageRecord(c, project, func(e *ProjectStorageRecorded) {
			for _, k := range kinds {
				e.setUID(k, "")
			}
		})
	}
	stop := func(err error, done []string) (*types.OperationResponse, error) {
		note := ""
		if ferr := forget(done...); ferr != nil {
			note = fmt.Sprintf(" This tool's record could not be updated (%s).", record.ErrorText(c, ferr))
		}
		code := kube.ErrorCodeFromK8sErr(err)
		if kube.IsK8sConflict(err) || kube.IsK8sError(err, http.StatusPreconditionFailed) {
			code = "conflict"
		}
		return r.fail(ProjectS3StepDelete, fmt.Sprintf("Cannot finish deleting the S3 storage of %s: %v. Delete again to continue.%s", project, err, note), code, "delete failed")
	}
	// Foreground: the Deployment stays until its ReplicaSet and pod are
	// gone. Its recorded UID stays until no Deployment of that name and no
	// pod of the storage is left (projectS3ServerGone), and only then do
	// the Service, the Secret and the NetworkPolicy go: a delete that
	// times out, or whose check fails, keeps the policy and the record, and
	// a retry waits again (a Deployment already terminating is not deleted
	// twice).
	if o := live[kindDeployment]; o.found && !o.meta.Terminating() {
		if err := del(kindDeployment, "Foreground"); err != nil {
			return stop(err, nil)
		}
	}
	r.step(ProjectS3StepDelete, "running", "Waiting until the storage's pod is gone")
	gone, err := kube.WaitForGone(c, projectS3GoneTimeout, projectS3GonePoll, func(cc *kube.Client) (bool, error) { return projectS3ServerGone(cc, project) })
	if err != nil {
		return r.fail(ProjectS3StepDelete, fmt.Sprintf("Cannot check that the storage's pod in %s is gone (%v); its Service, Secret and NetworkPolicy stay. Delete again to finish.", project, err), kube.FirstNonEmpty(kube.ErrorCodeFromK8sErr(err), "in_progress"), "pod check failed")
	}
	if !gone {
		return r.fail(ProjectS3StepDelete, fmt.Sprintf("Deployment %s or its pod in %s is still there after %s; its NetworkPolicy stays until the pod is gone. Delete again to finish.", ProjectS3Deployment, project, projectS3GoneTimeout), "in_progress", "deployment terminating")
	}
	done := []string{kindDeployment}
	for _, kind := range []string{kindService, kindSecret, kindNetworkPolicy} {
		if err := del(kind, "Background"); err != nil {
			return stop(err, done)
		}
		done = append(done, kind)
	}
	if deleteData {
		if err := del(kindPVC, "Background"); err != nil {
			return stop(err, done)
		}
		done = append(done, kindPVC)
	}
	if err := forget(done...); err != nil {
		r.logs = append(r.logs, fmt.Sprintf("Warning: this tool's record could not be updated: %s", record.ErrorText(c, err)))
	}
	// Its pipeline server's record goes with it: none uses the storage
	// (checked above), so an entry left is of one deleted elsewhere.
	if err := forgetProjectPipelineServer(c, project); err != nil {
		r.logs = append(r.logs, fmt.Sprintf("Warning: this tool's record of the pipeline server could not be cleared: %s", record.ErrorText(c, err)))
	}
	r.step(ProjectS3StepDelete, "success", "OK: The storage was deleted")
	slog.Info("project storage deleted", "project", project, "deleteData", deleteData, "user", kube.GetUser(c))
	msg := fmt.Sprintf("S3 storage %s was deleted from %s.", ProjectS3Deployment, project)
	if deleteData {
		msg += fmt.Sprintf(" Its volume %s and the data on it were deleted too.", ProjectS3PVC)
	}
	if len(kept) > 0 {
		msg += fmt.Sprintf(" Kept: %s.", strings.Join(kept, "; "))
	}
	return r.done(msg)
}

// GetProjectStorageStatus is the S3 storage of project, read with c (the
// user's token: viewers may not read its Secret, which shows as
// unreadable), the record with c's bookkeeping, and RHOAI's applications
// namespace with c's facts client for the NetworkPolicy check.
func GetProjectStorageStatus(c *kube.Client, project string) *types.ProjectStorageStatus {
	st := &types.ProjectStorageStatus{Project: project, Name: ProjectS3Deployment, Bucket: s3Bucket, Endpoint: ProjectS3Endpoint(project), DataVolume: ProjectS3PVC, Objects: []types.ProjectStorageObject{}}
	if !c.ActsAsUser() {
		st.Problem, st.ProblemCode = authz.ErrNotUserClient.Error(), types.ProblemError
		return st
	}
	rec, recErr := ReadProjectStorageRecord(c)
	if recErr != nil {
		kube.LogFactReadFailure("cannot read the project storages record for a status", "snapshot", recErr)
		st.Problem, st.ProblemCode = "This tool's record of the storage cannot be read ("+kube.FactReadProblem(recErr)+"), so whether it is the tool's is unknown.", types.ProblemError
	}
	_, recorded := rec.Get(project)
	st.Recorded = recorded
	var deploy projectS3Deployment
	deployFound := false
	for _, kind := range ProjectS3Kinds {
		o := types.ProjectStorageObject{Kind: kind, Name: ProjectS3ObjectName(kind)}
		if kind == kindDeployment {
			d, found, err := readProjectS3Deployment(c, project)
			if err != nil {
				o.Problem = projectReadProblem(kind, err)
			}
			deploy, deployFound = d, found
			o.Present, o.Owned = found, found && rec.Owns(kind, d.Metadata)
			st.Terminating = st.Terminating || d.Metadata.Terminating()
		} else {
			meta, found, err := readProjectS3Object(c, kind, project)
			if err != nil {
				o.Problem = projectReadProblem(kind, err)
			}
			o.Present, o.Owned = found, found && rec.Owns(kind, meta)
			st.Terminating = st.Terminating || meta.Terminating()
		}
		if o.Present {
			st.Deployed = true
		}
		st.Objects = append(st.Objects, o)
	}
	st.ManagedByTool = recorded && st.Deployed
	for _, o := range st.Objects {
		if o.Present && !o.Owned {
			st.ManagedByTool = false
		}
	}
	switch {
	case !st.Deployed:
		st.Message = "Not set up"
	case st.Terminating:
		st.Message = "Being deleted"
	case !deployFound:
		st.Message = fmt.Sprintf("Deployment %s is missing; Repair makes it again.", ProjectS3Deployment)
	case deploy.ready():
		st.Ready = true
		st.Message = "Running"
	default:
		st.Message = kube.FirstNonEmpty(deploy.replicaFailure(), "Starting")
	}
	var missing []string
	for _, o := range st.Objects {
		if !o.Present && o.Problem == "" && st.Deployed {
			missing = append(missing, o.Kind+" "+o.Name)
		}
	}
	if len(missing) > 0 && !st.Terminating {
		st.Missing = missing
	}
	// The fence: as setup applies it, or what differs.
	if st.Deployed && !st.Terminating {
		st.NetworkPolicyProblem = projectStorageFenceProblem(c, project)
	}
	if users, err := projectStorageUsers(c, project); err != nil {
		st.UsersProblem = projectReadProblem("DataSciencePipelinesApplication", err)
	} else {
		st.UsedBy = users
	}
	return st
}

// projectStorageFenceProblem says how NetworkPolicy nightly-s3-ingress in
// project differs from what setup applies, "" when it does not or when
// it cannot be read (a part the user may not read says so elsewhere).
func projectStorageFenceProblem(c *kube.Client, project string) string {
	if !c.ActsAsUser() {
		return ""
	}
	body, found, err := c.GetOptional(projectS3Path(kindNetworkPolicy, project))
	if err != nil {
		return ""
	}
	if !found {
		return fmt.Sprintf("NetworkPolicy %s is missing, so every port of the storage's pod is reachable from the pod network. Repair applies it again.", ProjectS3NetworkPolicy)
	}
	apps, err := projectAppsNamespace(c)
	if err != nil {
		return fmt.Sprintf("Whether NetworkPolicy %s is as set up cannot be checked: %v.", ProjectS3NetworkPolicy, err)
	}
	if p := projectS3PolicyProblem(body, apps); p != "" {
		return p + " Repair applies it again."
	}
	return ""
}

// projectReadProblem explains a read of the user's that failed.
func projectReadProblem(what string, err error) string {
	if _, code := kube.HTTPStatusForError(err); code == kube.ErrorCodeForbiddenForUser {
		return fmt.Sprintf("Your OpenShift account may not read the %s.", what)
	}
	return fmt.Sprintf("Cannot read the %s: %v", what, err)
}

// errProjectS3ServerRunning: the storage's pod may still run.
var errProjectS3ServerRunning = errors.New("the storage's pod is not gone yet")

// projectS3ServerGone reports whether project has no Deployment
// nightly-s3 (whoever made it) and no pod of the storage (label
// app=nightly-s3), read with c (the user's token): only then may its
// NetworkPolicy go.
func projectS3ServerGone(c *kube.Client, project string) (bool, error) {
	_, found, err := readProjectS3Deployment(c, project)
	if err != nil || found {
		return false, err
	}
	var pods []struct {
		Metadata kube.ObjectMeta `json:"metadata"`
	}
	if err := c.ListInto(rhoai.NamespacedPath("v1", "pods", project, "")+"?labelSelector=app%3D"+projectS3AppLabel, &pods); err != nil {
		return false, err
	}
	return len(pods) == 0, nil
}
