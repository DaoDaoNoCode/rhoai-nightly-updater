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
func SecureS3(c *kube.Client) (*types.OperationResponse, error) {
	log := &opLog{}
	fail := func(msg, code, activity string) (*types.OperationResponse, error) {
		slog.Warn("secure s3 storage stopped", "reason", activity, "user", kube.GetUser(c))
		record.Action(c, "secure-s3", fmt.Sprintf("namespace=%s (%s)", S3Namespace, activity), false)
		return &types.OperationResponse{Success: false, Message: msg, Logs: log.lines, ErrorCode: code}, nil
	}
	unchanged := " Nothing was changed."

	// Phase one: run all validation before applying changes.
	log.add("Checking the S3 storage and its pipeline servers...")
	legacy, err := ReadLegacy(c)
	if err != nil {
		return fail(fmt.Sprintf("Cannot check for storage from an earlier version in namespace %s: %v.%s%s", LegacyNamespace, err, kube.TemplateHint(err), unchanged), kube.ErrorCodeFromK8sErr(err), "earlier storage check failed")
	}
	if legacy != nil {
		return fail("Storage from an earlier version is still in namespace "+LegacyNamespace+": Migrate storage moves the data and leaves the storage secured."+unchanged, "prerequisites", "migration pending")
	}
	state, ep, epErr := S3StatusAndEndpoints(c)
	switch {
	case !state.Deployed || !state.ManagedByTool:
		return fail("The S3 storage is not set up by this tool."+unchanged, "prerequisites", "not deployed")
	case state.Terminating || !state.Ready:
		return fail("The S3 storage is not ready ("+kube.FirstNonEmpty(state.Message, "not running")+"). Repair it first."+unchanged, "prerequisites", "not ready")
	case epErr != nil:
		return fail(fmt.Sprintf("Cannot read the S3 storage's Service, so which pipeline servers use it is unknown: %v.%s%s", epErr, kube.TemplateHint(epErr), unchanged), kube.ErrorCodeFromK8sErr(epErr), "endpoints unreadable")
	}
	dspas, err := ListDSPAs(c, "")
	if err != nil && !errors.Is(err, ErrDSPACRDMissing) {
		return fail(fmt.Sprintf("Cannot list the pipeline servers: %v.%s%s", err, kube.TemplateHint(err), unchanged), kube.ErrorCodeFromK8sErr(err), "pipeline server list failed")
	}
	rec, err := ReadPipelineRecord(c)
	if err != nil {
		return fail(fmt.Sprintf("Cannot read this tool's record of its pipeline servers: %v.%s%s", err, kube.TemplateHint(err), unchanged), kube.ErrorCodeFromK8sErr(err), "pipeline record unreadable")
	}
	recorded, kept := keyServers(ep, dspas, rec)
	for _, d := range recorded {
		project := d.Meta.Namespace
		for _, name := range []string{DSPASecretName, ConnectionSecretName} {
			cur, err := readProjectS3Secret(c, project, name)
			if err != nil {
				return fail(fmt.Sprintf("Cannot read Secret %s/%s: %v.%s%s", project, name, err, kube.TemplateHint(err), unchanged), kube.ErrorCodeFromK8sErr(err), "secret read failed")
			}
			if cur.found && !cur.meta.HasToolLabel() {
				return fail(fmt.Sprintf("Secret %s/%s exists and was not created by this tool, so pipeline server %s/%s cannot get its own key.%s", project, name, project, d.Meta.Name, unchanged), "not_managed", "foreign secret")
			}
			obj := dspaS3Secret(project, S3Keys{})
			if name == ConnectionSecretName {
				obj = dashboardS3Connection(project, S3Keys{})
			}
			setDSPAOwner(obj, d.Meta.Name, d.Meta.UID)
			s := &projectSecret{c: c, project: project, name: name, obj: obj, meta: cur.meta, found: cur.found}
			if _, err := writeProjectSecret(s, projectSecretPath(project, name), envkit.WithToolFieldManager(rhoai.NamespacedPath("v1", "secrets", project, "")), true); err != nil {
				msg, code := pipelineWriteFailure(fmt.Sprintf("secret %s", name), project, err)
				return fail(msg+unchanged, code, "secret dry run failed")
			}
		}
	}
	log.add(fmt.Sprintf("OK: %s to give a key of its own", kube.CountNoun(len(recorded), "pipeline server", "pipeline servers")))

	// Phase two: single rollout for the admin credential and enabled IAM writes.
	if ep.Secured {
		log.add("OK: The S3 storage already runs with a new admin key and IAM writes on")
	} else {
		log.add("Rolling out a new admin key with IAM writes on...")
		keys, err := newAdminKeys(c)
		if err != nil {
			return fail(fmt.Sprintf("Cannot make the new admin key: %v.%s", err, unchanged), "internal", "keys")
		}
		res, err := setupS3Storage(c, keys, log, s3RetrySecure, true)
		if err != nil {
			return nil, err
		}
		if !res.Success {
			return fail("The new admin key was not rolled out: "+res.Message+" No pipeline server got its own key yet.", res.ErrorCode, "rollout failed")
		}
		log.add("OK: The S3 storage runs with the new admin key; the old key no longer works")
	}

	// Phase three: issue per-server keys for each entry in the record.
	sw, err := adminSeaweed(c)
	if err != nil {
		return fail(fmt.Sprintf("Cannot read the new admin key: %v.%s No pipeline server got its own key yet; run Secure S3 storage again.", err, kube.TemplateHint(err)), kube.ErrorCodeFromK8sErr(err), "admin key unreadable")
	}
	var done, skipped []string
	for _, d := range recorded {
		name := d.Meta.Namespace + "/" + d.Meta.Name
		log.add(fmt.Sprintf("Giving pipeline server %s a key of its own...", name))
		changed, err := rekeyServer(c, sw, d)
		if err != nil {
			already := "none"
			if len(done)+len(skipped) > 0 {
				already = strings.Join(append(append([]string{}, done...), skipped...), ", ")
			}
			log.add(fmt.Sprintf("Stopped at %s: %v", name, err))
			return fail(fmt.Sprintf("Pipeline server %s did not get its own key: %v. Already done: %s. The storage is secured; run Secure S3 storage again to finish the rest.", name, err, already), "s3_keys", "rekey failed")
		}
		if changed {
			done = append(done, name)
			log.add(fmt.Sprintf("OK: %s uses SeaweedFS user %s", name, ScopedUserName(d.Meta.Namespace)))
		} else {
			skipped = append(skipped, name)
			log.add(fmt.Sprintf("OK: %s already uses its own key", name))
		}
	}

	msg := "The S3 storage is secured: it has a new admin key, which is in no project."
	if len(done) > 0 {
		msg += fmt.Sprintf(" Now on a key of their own: %s.", strings.Join(done, ", "))
	}
	if len(skipped) > 0 {
		msg += fmt.Sprintf(" Already on their own key: %s.", strings.Join(skipped, ", "))
	}
	if len(kept) > 0 {
		msg += fmt.Sprintf(" Not changed by this tool, their old key no longer works: %s.", strings.Join(kept, "; "))
	}
	if len(done) > 0 {
		msg += fmt.Sprintf(" Restart the workbenches and model deployments that use Connection %s, and workbenches that run pipelines, so they pick up the new key.", ConnectionSecretName)
	}
	slog.Info("s3 storage secured", "user", kube.GetUser(c), "rekeyed", len(done))
	record.Action(c, "secure-s3", fmt.Sprintf("namespace=%s (%d pipeline servers rekeyed)", S3Namespace, len(done)), true)
	return &types.OperationResponse{Success: true, Message: msg, Logs: log.lines}, nil
}
