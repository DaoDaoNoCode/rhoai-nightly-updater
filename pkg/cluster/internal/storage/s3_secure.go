package storage

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
// The pipeline API server reads its S3 key from environment variables
// (secretKeyRef) only when it starts, and DSPO's config hash, which rolls it,
// does not cover credentials, so a server given its key here keeps the old,
// revoked one until its pods restart. For each recorded server on its own
// key whose API server pod started before the tool last wrote its
// credentials Secret (apiServerKeptOldKey), the success message and the
// server's step (Detail) give the command that restarts it
// (RestartAPIServerCommand), also on a later run; deleting pods would need
// a new RBAC row.
func SecureS3(c *kube.Client, removeLeftover []string, onStep func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
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
	step(SecureStepCheck, "running", "Checking the S3 storage and its pipeline servers")
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
	check := "OK: Found 1 pipeline server to give a key of its own"
	if len(recorded) != 1 {
		check = fmt.Sprintf("OK: Found %d pipeline servers to give each a key of its own", len(recorded))
	}
	if len(kept) > 0 {
		check += fmt.Sprintf("; left on the old key: %s", strings.Join(kept, ", "))
	}
	step(SecureStepCheck, "success", check)

	// summary says which servers have their own key, which failed or were
	// not checked, and which still use the old one, for the outcome of a
	// run that ended or stopped. runAgain adds how to finish.
	summary := func(done, skipped []string, failed map[string]string, runAgain bool) string {
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
			parts = append(parts, fmt.Sprintf("Given a key of their own: %s.", strings.Join(own, ", ")))
		}
		for _, n := range names {
			if why := failed[n]; why != "" {
				parts = append(parts, strings.TrimSuffix(why, ".")+".")
			}
		}
		if len(old) > 0 {
			parts = append(parts, fmt.Sprintf("Still on the old key, which no longer works: %s.", strings.Join(old, ", ")))
		}
		if runAgain && (len(old) > 0 || len(failed) > 0) {
			parts = append(parts, "Run Secure S3 storage again: it skips the servers already done.")
		}
		if len(kept) > 0 {
			parts = append(parts, fmt.Sprintf("Left on the old admin key, which no longer works: %s. %s", strings.Join(kept, ", "),
				kube.Verb(len(kept), "Its owner must give that pipeline server new S3 keys, or delete it and set it up again with this tool.",
					"Their owners must give those pipeline servers new S3 keys, or delete them and set them up again with this tool.")))
		}
		return strings.Join(parts, " ")
	}

	// Step 2: the new admin key, with IAM writes on, in one rollout.
	if ep.Secured {
		step(SecureStepRollout, "skipped", "The S3 storage already runs with a new admin key and per-server keys")
	} else {
		step(SecureStepRollout, "running", "Rolling out a new admin key and turning on per-server keys")
		keys, err := newAdminKeys(c)
		if err != nil {
			return stop(SecureStepRollout, fmt.Sprintf("Cannot make the new admin key: %v.%s", err, unchanged), "internal", "keys"), nil
		}
		res := setupS3Storage(c, keys, &opLog{observe: func(line string) { step(SecureStepRollout, "running", line) }}, s3RetrySecure, true)
		if !res.Success {
			return stop(SecureStepRollout, "The new admin key was not rolled out: "+res.Message+" No pipeline server got its own key yet.", res.ErrorCode, "rollout failed"), nil
		}
		step(SecureStepRollout, "success", "OK: The S3 storage runs with the new admin key; the old key no longer works")
	}

	// Step 3: a key of its own for every recorded pipeline server. A server
	// that fails is reported and the others go on.
	if c.Context().Err() != nil {
		return stop(SecureStepRollout, fmt.Sprintf("The run was cancelled before any pipeline server got its own key. %s Run Secure S3 storage again to continue.", summary(nil, nil, nil, false)), "in_progress", "cancelled"), nil
	}
	sw, err := adminSeaweed(c)
	if err != nil {
		return stop(SecureStepRollout, fmt.Sprintf("Cannot read the new admin key: %v.%s No pipeline server got its own key yet; run Secure S3 storage again.", err, kube.TemplateHint(err)), kube.ErrorCodeFromK8sErr(err), "admin key unreadable"), nil
	}
	var done, skipped, restart, stale []string
	failed := map[string]string{}
	// restartStep ends a server's step with msg, and, when its API server
	// pod started before its key was written, says so and carries the
	// command that restarts it (Detail).
	restartStep := func(id, msg string, d DSPA) {
		old, err := apiServerKeptOldKey(c, d)
		detail := ""
		switch {
		case err != nil:
			msg += fmt.Sprintf("; whether its API server restarted since its key was written could not be checked (%v)", err)
		case old:
			detail = RestartAPIServerCommand(d)
			restart = append(restart, detail)
			stale = append(stale, d.Meta.Namespace+"/"+d.Meta.Name)
			msg += "; its API server started before the key was written, so it keeps the old key, which no longer works, until its pods restart"
		}
		logs = append(logs, msg)
		if onStep != nil {
			onStep(types.UpdateStepEvent{Step: id, Status: "success", Message: msg, Detail: detail})
		}
	}
	for _, d := range recorded {
		name := d.Meta.Namespace + "/" + d.Meta.Name
		id := SecureServerStep(name)
		// A cancelled request (the server shutting down) ends the run here:
		// the next servers would only fail with the cancellation.
		if c.Context().Err() != nil {
			return stop(id, fmt.Sprintf("The run was cancelled before pipeline server %s. %s Run Secure S3 storage again to continue.", name, summary(done, skipped, failed, false)), "in_progress", "cancelled"), nil
		}
		if why := blocked[name]; why != "" {
			// Its Secrets were not checked; a server that already uses its
			// own key is not reported as failing.
			if own, err := usesOwnKey(c, sw, d); err == nil && own {
				skipped = append(skipped, name)
				step(id, "success", fmt.Sprintf("OK: Pipeline server %s already uses its own key; its Secrets were not checked: %s", name, why))
				continue
			}
			failed[name] = fmt.Sprintf("%s was not checked: %s", name, why)
			step(id, "failed", failed[name])
			continue
		}
		// Like Migrate storage: a server is started only while its worst
		// case is left (secureServerBudget), so the deadline cannot fall
		// between the writes of its two Secrets.
		if deadline, ok := c.Context().Deadline(); ok && kube.Clock().Until(deadline) < secureServerBudget {
			return stop(id, fmt.Sprintf("Not enough time is left in this run to give pipeline server %s its own key. %s Run Secure S3 storage again to continue: it skips the servers already done.", name, summary(done, skipped, failed, false)), "out_of_time", "out of time"), nil
		}
		step(id, "running", fmt.Sprintf("Giving pipeline server %s a key of its own", name))
		changed, err := rekeyServer(c, sw, d, func(line string) { step(id, "running", line) })
		switch {
		case err != nil:
			failed[name] = fmt.Sprintf("%s did not get its own key: %v", name, err)
			step(id, "failed", failed[name])
		case changed:
			done = append(done, name)
			restartStep(id, fmt.Sprintf("OK: Pipeline server %s has the key of SeaweedFS user %s in its Secrets", name, ScopedUserName(d.Meta.Namespace)), d)
		default:
			skipped = append(skipped, name)
			restartStep(id, fmt.Sprintf("OK: Pipeline server %s already uses its own key", name), d)
		}
	}

	// Keys of pipeline servers that no longer exist: only those the user
	// confirmed, and only if they are still leftovers now.
	left, leftErr := deleteConfirmedLeftovers(c, sw, removeLeftover)

	msg := "New admin key, kept out of every project."
	if s := summary(done, skipped, failed, true); s != "" {
		msg += " " + s
	}
	if leftErr != nil {
		msg += fmt.Sprintf(" The leftover keys could not all be removed: %v.", leftErr)
		if len(left) > 0 {
			msg += fmt.Sprintf(" Removed before that: %s.", strings.Join(left, ", "))
		}
	} else if len(left) > 0 {
		msg += fmt.Sprintf(" Removed the keys of pipeline servers that no longer exist: %s.", strings.Join(left, ", "))
	}
	if len(restart) > 0 {
		msg += fmt.Sprintf(" %s the old key, which no longer works, until %s pods restart: the pipeline API server reads its S3 key only when it starts. Delete the pods (the Deployment makes new ones): %s.",
			kube.Verb(len(restart), "The API server of "+strings.Join(stale, ", ")+" keeps using", "The API servers of "+strings.Join(stale, ", ")+" keep using"),
			kube.Verb(len(restart), "its", "their"), strings.Join(restart, "; "))
	}
	if len(done) > 0 {
		msg += fmt.Sprintf(" Restart the workbenches and model deployments that use Connection %s, and Elyra pipelines in workbenches, so they pick up the new key.", ConnectionSecretName)
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

// apiServerKeptOldKey reports whether a pod of d's pipeline API server
// (labels as RestartAPIServerCommand) started no later than the tool's last
// write of d's credentials Secret (the latest managedFields time of the
// tool's managers): such a pod read the previous key at start. No pod, or
// no such write, is false.
func apiServerKeptOldKey(c *kube.Client, d DSPA) (bool, error) {
	path, ok := toolDSPASecretPath(d.Meta.Namespace, d.SecretName)
	if !ok {
		return false, nil
	}
	meta, found, err := kube.ReadObjectMeta(c, path)
	if err != nil || !found {
		return false, err
	}
	var written time.Time
	for _, e := range meta.ManagedFields {
		t, perr := time.Parse(time.RFC3339, e.Time)
		if perr == nil && e.Subresource == "" && slices.Contains(envkit.ToolManagers, e.Manager) && t.After(written) {
			written = t
		}
	}
	if written.IsZero() {
		return false, nil
	}
	selector := fmt.Sprintf("app=ds-pipeline-%s,dspa=%s", d.Meta.Name, d.Meta.Name)
	body, _, err := c.Get(rhoai.NamespacedPath("v1", "pods", d.Meta.Namespace, "") + "?labelSelector=" + url.QueryEscape(selector))
	if err != nil {
		return false, err
	}
	var pods struct {
		Items []struct {
			Metadata kube.ObjectMeta `json:"metadata"`
			Status   struct {
				StartTime string `json:"startTime"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &pods); err != nil {
		return false, err
	}
	for _, p := range pods.Items {
		start, perr := time.Parse(time.RFC3339, p.Status.StartTime)
		if perr == nil && !p.Metadata.Terminating() && !start.After(written) {
			return true, nil
		}
	}
	return false, nil
}

// RestartAPIServerCommand deletes the pods of d's pipeline API server, which
// its Deployment then makes again, so they read the S3 key from its Secret
// anew. DSPO names the Deployment ds-pipeline-<DSPA name> and labels its pods
// app=ds-pipeline-<DSPA name>, dspa=<DSPA name> (red-hat-data-services
// data-science-pipelines-operator rhoai-3.6
// config/internal/apiserver/default/deployment.yaml.tmpl L20-23, with
// controllers/apiserver.go L42 apiServerDefaultResourceNamePrefix). Both
// names are Kubernetes names, so the command needs no quoting.
func RestartAPIServerCommand(d DSPA) string {
	return fmt.Sprintf("oc delete pod -n %s -l app=ds-pipeline-%s,dspa=%s", d.Meta.Namespace, d.Meta.Name, d.Meta.Name)
}

// usesOwnKey reports whether a recorded pipeline server uses a key of its
// own, as the status decides it (KeysState): its Connection's key is one
// of its user's, and the DSPA names it.
func usesOwnKey(c *kube.Client, sw *seaweedClient, d DSPA) (bool, error) {
	keyID, found, err := connectionKeyID(c, d.Meta.Namespace)
	if err != nil || !found || keyID == "" {
		return false, err
	}
	ids, _, err := sw.ListAccessKeys(ScopedUserName(d.Meta.Namespace))
	if err != nil {
		return false, err
	}
	return slices.Contains(ids, keyID) && d.Meta.Annotations[DSPAS3KeyAnnotation] == keyID, nil
}

// secureServerBudget is the time one server can take at worst: its key
// check makes five calls of up to s3SlowHTTPClient's 45 seconds, and the
// IAM calls, two Secret writes and the annotation come on top.
var secureServerBudget = 5 * time.Minute

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
