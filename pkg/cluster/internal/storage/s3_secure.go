package storage

import (
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

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
	for _, d := range recorded {
		project := d.Meta.Namespace
		for _, name := range []string{DSPASecretName, ConnectionSecretName} {
			cur, err := readProjectS3Secret(c, project, name)
			if err != nil {
				return fail(fmt.Sprintf("Cannot read Secret %s/%s: %v.%s%s", project, name, err, kube.TemplateHint(err), unchanged), kube.ErrorCodeFromK8sErr(err), "secret read failed"), nil
			}
			if cur.found && !cur.meta.HasToolLabel() {
				return fail(fmt.Sprintf("Secret %s/%s exists and was not created by this tool, so pipeline server %s/%s cannot get its own key.%s", project, name, project, d.Meta.Name, unchanged), "not_managed", "foreign secret"), nil
			}
			obj := dspaS3Secret(project, S3Keys{})
			if name == ConnectionSecretName {
				obj = dashboardS3Connection(project, S3Keys{})
			}
			setDSPAOwner(obj, d.Meta.Name, d.Meta.UID)
			s := &projectSecret{c: c, project: project, name: name, obj: obj, meta: cur.meta, found: cur.found}
			if _, err := writeProjectSecret(s, projectSecretPath(project, name), envkit.WithToolFieldManager(rhoai.NamespacedPath("v1", "secrets", project, "")), true); err != nil {
				msg, code := pipelineWriteFailure(fmt.Sprintf("secret %s", name), project, err)
				return fail(msg+unchanged, code, "secret dry run failed"), nil
			}
		}
	}
	names := make([]string, 0, len(recorded))
	for _, d := range recorded {
		names = append(names, d.Meta.Namespace+"/"+d.Meta.Name)
	}
	check := fmt.Sprintf("OK: %s to give a key of its own", kube.CountNoun(len(recorded), "pipeline server", "pipeline servers"))
	if len(kept) > 0 {
		check += fmt.Sprintf("; left on the old key: %s", strings.Join(kept, "; "))
	}
	step(SecureStepCheck, "success", check)

	// summary says which servers have their own key and which still use the
	// old one, for the outcome of a run that ended or stopped.
	summary := func(done, skipped []string) string {
		var own, old, parts []string
		for _, n := range names {
			if slices.Contains(done, n) || slices.Contains(skipped, n) {
				own = append(own, n)
			} else {
				old = append(old, n)
			}
		}
		if len(own) > 0 {
			parts = append(parts, fmt.Sprintf("On a key of their own: %s.", strings.Join(own, ", ")))
		}
		if len(old) > 0 {
			parts = append(parts, fmt.Sprintf("Still on the old key: %s; run Secure S3 storage again: it skips the servers already done.", strings.Join(old, ", ")))
		}
		if len(kept) > 0 {
			parts = append(parts, fmt.Sprintf("Not changed by this tool: %s. The old admin key no longer works there: their owner must give those pipeline servers new S3 keys.", strings.Join(kept, "; ")))
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

	// Step 3: a key of its own for every recorded pipeline server.
	sw, err := adminSeaweed(c)
	if err != nil {
		return stop(SecureStepRollout, fmt.Sprintf("Cannot read the new admin key: %v.%s No pipeline server got its own key yet; run Secure S3 storage again.", err, kube.TemplateHint(err)), kube.ErrorCodeFromK8sErr(err), "admin key unreadable"), nil
	}
	var done, skipped []string
	for _, d := range recorded {
		name := d.Meta.Namespace + "/" + d.Meta.Name
		id := SecureServerStep(name)
		step(id, "running", fmt.Sprintf("Giving pipeline server %s a key of its own...", name))
		changed, err := rekeyServer(c, sw, d, func(line string) { step(id, "running", line) })
		if err != nil {
			return stop(id, fmt.Sprintf("Pipeline server %s did not get its own key: %v. The storage is secured. %s", name, err, summary(done, skipped)), "s3_keys", "rekey failed"), nil
		}
		if changed {
			done = append(done, name)
			step(id, "success", fmt.Sprintf("OK: %s uses SeaweedFS user %s", name, ScopedUserName(d.Meta.Namespace)))
		} else {
			skipped = append(skipped, name)
			step(id, "success", fmt.Sprintf("OK: %s already uses its own key", name))
		}
	}

	msg := "New admin key, kept out of every project."
	if s := summary(done, skipped); s != "" {
		msg += " " + s
	}
	if len(done) > 0 {
		msg += fmt.Sprintf(" Restart the workbenches and model deployments that use Connection %s, and workbenches that run pipelines, so they pick up the new key.", ConnectionSecretName)
	}
	slog.Info("s3 storage secured", "user", kube.GetUser(c), "rekeyed", len(done))
	record.Action(c, "secure-s3", fmt.Sprintf("namespace=%s (%d pipeline servers rekeyed)", S3Namespace, len(done)), true)
	return &types.OperationResponse{Success: true, Message: msg, Logs: logs}, nil
}

// The steps Secure S3 storage streams: the checks, the rollout, then one
// per recorded pipeline server (SecureServerStep).
const (
	SecureStepCheck   = "check"
	SecureStepRollout = "rollout"
)

// SecureServerStep is the step of one pipeline server ("namespace/name").
func SecureServerStep(name string) string { return "server:" + name }
