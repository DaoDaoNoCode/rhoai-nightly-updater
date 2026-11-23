package storage

import (
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// SecureS3 transitions S3 storage that was configured under a prior release
// into the secured configuration (the Secure S3 storage action; it does not
// run unless invoked):
//
//  1. Preconditions, applied before mutating anything: storage belongs to
//     this tool and is ready; nothing is still migrating out of namespace
//     minio; every pipeline server the record tracks on that storage, plus
//     both associated Secrets, can be fetched and carry this tool's labels
//     (writes to each Secret are validated via dry-run). On error, leave
//     cluster state as-is.
//  2. Deploy a fresh admin credential with IAM writes enabled and
//     self-service disabled, then wait until the pod is healthy (omit when
//     storage is already secured, e.g. a second run). Prior admin credentials
//     cease to work.
//  3. Per recorded pipeline server (rekeyServer): configure its SeaweedFS
//     user, policy, tag, and replacement key against the storage; patch the
//     two Secrets in place; annotate the DSPA so the operator re-validates
//     storage. Servers whose Secrets already store a dedicated key are left
//     alone, so a partial run can be resumed later.
//
// Pipeline servers absent from the record still hold the superseded key, which
// is now invalid; the success message names them. Workloads still using the
// old credential in running pods (workbenches, model deployments, pipeline
// API server) retain it until they recycle; this action does not restart them.
func SecureS3(c *kube.Client, onStep func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
	var logs []string
	step := func(id, status, msg string) {
		logs = append(logs, msg)
		if onStep != nil {
			onStep(types.UpdateStepEvent{Step: id, Status: status, Message: msg})
		}
	}
	stop := func(id, msg, code, activity string) *types.OperationResponse {
		if onStep != nil {
			onStep(types.UpdateStepEvent{Step: id, Status: "failed", Message: msg, ErrorCode: code})
		}
		logs = append(logs, msg)
		slog.Warn("secure s3 storage stopped", "step", id, "reason", activity, "user", kube.GetUser(c))
		record.Action(c, "secure-s3", fmt.Sprintf("namespace=%s (%s)", S3Namespace, activity), false)
		return &types.OperationResponse{Success: false, Message: msg, Logs: logs, ErrorCode: code}
	}
	unchanged := " Nothing was changed."
	fail := func(msg, code, activity string) *types.OperationResponse {
		return stop(SecureStepCheck, msg, code, activity)
	}

	// Step 1: every check before any change.
	step(SecureStepCheck, "running", "Checking the S3 storage and its pipeline servers...")
	legacy, err := ReadLegacy(c)
	if err != nil {
		return fail(fmt.Sprintf("Cannot check for storage from an earlier version in namespace %s: %v.%s%s", LegacyNamespace, err, kube.TemplateHint(err), unchanged), kube.ErrorCodeFromK8sErr(err), "earlier storage check failed"), nil
	}
	if legacy != nil {
		return fail("Storage from an earlier version is still in namespace "+LegacyNamespace+": Migrate storage moves the data and leaves the storage secured."+unchanged, "prerequisites", "migration pending"), nil
	}
	state, ep, epErr := S3StatusAndEndpoints(c)
	switch {
	case !state.Deployed || !state.ManagedByTool:
		return fail("The S3 storage is not set up by this tool."+unchanged, "prerequisites", "not deployed"), nil
	case state.Terminating || !state.Ready:
		return fail("The S3 storage is not ready ("+kube.FirstNonEmpty(state.Message, "not running")+"). Repair it first."+unchanged, "prerequisites", "not ready"), nil
	case epErr != nil:
		return fail(fmt.Sprintf("Cannot read the S3 storage's Service, so which pipeline servers use it is unknown: %v.%s%s", epErr, kube.TemplateHint(epErr), unchanged), kube.ErrorCodeFromK8sErr(epErr), "endpoints unreadable"), nil
	}
	dspas, err := ListDSPAs(c, "")
	if err != nil && !errors.Is(err, ErrDSPACRDMissing) {
		return fail(fmt.Sprintf("Cannot list the pipeline servers: %v.%s%s", err, kube.TemplateHint(err), unchanged), kube.ErrorCodeFromK8sErr(err), "pipeline server list failed"), nil
	}
	rec, err := ReadPipelineRecord(c)
	if err != nil {
		return fail(fmt.Sprintf("Cannot read this tool's record of its pipeline servers: %v.%s%s", err, kube.TemplateHint(err), unchanged), kube.ErrorCodeFromK8sErr(err), "pipeline record unreadable"), nil
	}
	recorded, kept := keyServers(ep, dspas, rec)
	// Each server's Secrets are checked (and their writes dry-run). Before
	// the rollout any problem stops the run with nothing changed. Once the
	// storage is secured (a re-run), a problem holds back only that server:
	// the others still get their keys.
	blocked := map[string]string{}
	for _, d := range recorded {
		project := d.Meta.Namespace
		name := project + "/" + d.Meta.Name
		for _, secretName := range []string{DSPASecretName, ConnectionSecretName} {
			msg, code, activity := checkProjectSecret(c, d, secretName)
			if msg == "" {
				continue
			}
			if !ep.Secured {
				return fail(msg+unchanged, code, activity), nil
			}
			blocked[name] = msg
			break
		}
	}
	names := make([]string, 0, len(recorded))
	for _, d := range recorded {
		names = append(names, d.Meta.Namespace+"/"+d.Meta.Name)
	}
	check := fmt.Sprintf("OK: %s to give a key of its own", kube.CountNoun(len(recorded), "pipeline server", "pipeline servers"))
	if len(kept) > 0 {
		check += fmt.Sprintf("; not set up by this tool, left on the old key: %s", strings.Join(kept, "; "))
	}
	step(SecureStepCheck, "success", check)

	// summary says which servers have their own key, which failed and which
	// still use the old one, for the outcome of a run that ended or stopped.
	summary := func(done, skipped []string, failed map[string]string) string {
		var own, old, parts []string
		for _, n := range names {
			switch {
			case slices.Contains(done, n) || slices.Contains(skipped, n):
				own = append(own, n)
			case failed[n] == "":
				old = append(old, n)
			}
		}
		if len(own) > 0 {
			parts = append(parts, fmt.Sprintf("On a key of their own: %s.", strings.Join(own, ", ")))
		}
		for _, n := range names {
			if why := failed[n]; why != "" {
				parts = append(parts, fmt.Sprintf("%s still uses the old key: %s.", n, strings.TrimSuffix(why, ".")))
			}
		}
		if len(old) > 0 {
			parts = append(parts, fmt.Sprintf("Still on the old key: %s.", strings.Join(old, ", ")))
		}
		if len(old) > 0 || len(failed) > 0 {
			parts = append(parts, "Run Secure S3 storage again: it skips the servers already done.")
		}
		if len(kept) > 0 {
			parts = append(parts, fmt.Sprintf("Not set up by this tool: %s. The old admin key no longer works there: %s.", strings.Join(kept, "; "),
				kube.Verb(len(kept), "its owner must give that pipeline server new S3 keys", "their owners must give those pipeline servers new S3 keys")))
		}
		return strings.Join(parts, " ")
	}

	// Step 2: the new admin key, with IAM writes on, in one rollout.
	if ep.Secured {
		step(SecureStepRollout, "skipped", "The S3 storage already runs with a new admin key and per-server keys")
	} else {
		step(SecureStepRollout, "running", "Rolling out a new admin key and turning on per-server keys...")
		keys, err := newAdminKeys(c)
		if err != nil {
			return stop(SecureStepRollout, fmt.Sprintf("Cannot make the new admin key: %v.%s", err, unchanged), "internal", "keys"), nil
		}
		res, err := setupS3Storage(c, keys, &opLog{observe: func(line string) { step(SecureStepRollout, "running", line) }}, s3RetrySecure, true)
		if err != nil {
			return nil, err
		}
		if !res.Success {
			return stop(SecureStepRollout, "The new admin key was not rolled out: "+res.Message+" No pipeline server got its own key yet.", res.ErrorCode, "rollout failed"), nil
		}
		step(SecureStepRollout, "success", "OK: The S3 storage runs with the new admin key; the old key no longer works")
	}

	// Step 3: a key of its own for every recorded pipeline server. A server
	// that fails is reported and the others go on.
	sw, err := adminSeaweed(c)
	if err != nil {
		return stop(SecureStepRollout, fmt.Sprintf("Cannot read the new admin key: %v.%s No pipeline server got its own key yet; run Secure S3 storage again.", err, kube.TemplateHint(err)), kube.ErrorCodeFromK8sErr(err), "admin key unreadable"), nil
	}
	var done, skipped []string
	failed := map[string]string{}
	for _, d := range recorded {
		name := d.Meta.Namespace + "/" + d.Meta.Name
		id := SecureServerStep(name)
		if why := blocked[name]; why != "" {
			failed[name] = why
			step(id, "failed", why)
			continue
		}
		// Like Migrate storage: a server is started only while the time for
		// its key check (up to s3SlowHTTPClient's wait per call) is left.
		if deadline, ok := c.Context().Deadline(); ok && kube.Clock().Until(deadline) < secureServerBudget {
			return stop(id, fmt.Sprintf("Not enough time is left in this run to give pipeline server %s its own key. %s run Secure S3 storage again to continue.", name, summary(done, skipped, failed)), "in_progress", "out of time"), nil
		}
		step(id, "running", fmt.Sprintf("Giving pipeline server %s a key of its own...", name))
		changed, err := rekeyServer(c, sw, d, func(line string) { step(id, "running", line) })
		switch {
		case err != nil:
			failed[name] = err.Error()
			step(id, "failed", fmt.Sprintf("%s did not get its own key: %v", name, err))
		case changed:
			done = append(done, name)
			step(id, "success", fmt.Sprintf("OK: %s uses SeaweedFS user %s", name, ScopedUserName(d.Meta.Namespace)))
		default:
			skipped = append(skipped, name)
			step(id, "success", fmt.Sprintf("OK: %s already uses its own key", name))
		}
	}

	// Users of pipeline servers that are gone (deleted while SeaweedFS
	// failed, or outside this tool): their keys still work until deleted.
	left, leftErr := leftoverUsers(sw, recorded)
	if leftErr == nil && len(left) > 0 {
		leftErr = deleteUsers(sw, left)
	}

	msg := "New admin key, kept out of every project."
	if s := summary(done, skipped, failed); s != "" {
		msg += " " + s
	}
	if leftErr != nil {
		msg += fmt.Sprintf(" The keys of deleted pipeline servers could not be checked or deleted: %v.", leftErr)
	} else if len(left) > 0 {
		msg += fmt.Sprintf(" Deleted the keys of pipeline servers that are gone: %s.", strings.Join(left, ", "))
	}
	if len(done) > 0 {
		msg += fmt.Sprintf(" Restart the workbenches and model deployments that use Connection %s, and workbenches that run pipelines, so they pick up the new key.", ConnectionSecretName)
	}
	slog.Info("s3 storage secured", "user", kube.GetUser(c), "rekeyed", len(done), "failed", len(failed))
	record.Action(c, "secure-s3", fmt.Sprintf("namespace=%s (%d pipeline servers rekeyed, %d failed)", S3Namespace, len(done), len(failed)), len(failed) == 0)
	if len(failed) > 0 {
		// The storage is secured; some servers still need a run: a warning,
		// not a stop.
		return &types.OperationResponse{Success: true, Message: msg, Logs: logs, ErrorCode: "partial"}, nil
	}
	return &types.OperationResponse{Success: true, Message: msg, Logs: logs}, nil
}

// secureServerBudget is the time a server's key needs at most: its key
// check makes up to five calls of up to s3SlowHTTPClient's wait.
var secureServerBudget = 2 * time.Minute

// checkProjectSecret checks one of a recorded pipeline server's Secrets
// for Secure S3 storage: readable, this tool's, and its write accepted (a
// dry run). It returns why not, or "".
func checkProjectSecret(c *kube.Client, d DSPA, name string) (msg, code, activity string) {
	project := d.Meta.Namespace
	cur, err := readProjectS3Secret(c, project, name)
	if err != nil {
		return fmt.Sprintf("Cannot read Secret %s/%s: %v.%s", project, name, err, kube.TemplateHint(err)), kube.ErrorCodeFromK8sErr(err), "secret read failed"
	}
	if cur.found && !cur.meta.HasToolLabel() {
		return fmt.Sprintf("Secret %s/%s exists and was not created by this tool, so pipeline server %s/%s cannot get its own key.", project, name, project, d.Meta.Name), "not_managed", "foreign secret"
	}
	obj := dspaS3Secret(project, S3Keys{})
	if name == ConnectionSecretName {
		obj = dashboardS3Connection(project, S3Keys{})
	}
	setDSPAOwner(obj, d.Meta.Name, d.Meta.UID)
	s := &projectSecret{c: c, project: project, name: name, obj: obj, meta: cur.meta, found: cur.found}
	if _, err := writeProjectSecret(s, projectSecretPath(project, name), envkit.WithToolFieldManager(rhoai.NamespacedPath("v1", "secrets", project, "")), true); err != nil {
		msg, code := pipelineWriteFailure(fmt.Sprintf("secret %s", name), project, err)
		return msg, code, "secret dry run failed"
	}
	return "", "", ""
}

// The steps Secure S3 storage streams: the checks, the rollout, then one
// per recorded pipeline server (SecureServerStep).
const (
	SecureStepCheck   = "check"
	SecureStepRollout = "rollout"
)

// SecureServerStep is the step of one pipeline server ("namespace/name").
func SecureServerStep(name string) string { return "server:" + name }
