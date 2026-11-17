package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Migrate storage moves the S3 storage an earlier version left in namespace
// minio (s3_legacy.go) to namespace seaweedfs (s3_storage.go), with its data
// and the pipeline servers this tool created. It runs in two operations,
// each resumable: re-running one does only what is still missing.
//
// Migrate:
//  1. Set up the new storage with the old storage's keys, so every client
//     only needs the new address.
//  2. Copy every bucket and object with an rclone Job in namespace
//     seaweedfs, then verify (migrateScript). Any difference stops here.
//  3. Repoint the pipeline servers this tool created and wait until DSPO
//     reports ObjectStoreAvailable for the new storage. One that does not
//     stops the run.
//
// Remove the old storage (after the user has read the summary): copy and
// verify once more (objects written since), delete the old storage's objects
// by UID, and the namespace minio only when the tool provably created it
// and the user chose to.
const (
	migrateJobName   = "seaweedfs-migrate"
	migrateContainer = "rclone"
	migrateAppLabel  = "seaweedfs-migrate"
	// rcloneDefaultImage is rclone 1.76.0 (docker.io/rclone/rclone:1.76.0,
	// MIT), pinned by its OCI index digest (linux amd64, arm64, arm/v6,
	// arm/v7, 386; image label org.opencontainers.image.version 1.76.0,
	// revision e982ff36). Alpine based, so the Job runs its script with
	// /bin/sh; it sets no user, so OpenShift's restricted-v2 SCC assigns one.
	rcloneDefaultImage = "docker.io/rclone/rclone@sha256:7cbeec24a44f206067597c0368be4de90ae19a64d5113c7d1938bd737a81e466"
	// migrateJobDeadline bounds the Job itself. The copy reads the data
	// once and the check twice (--download), up to the 20Gi of the storage's
	// PVC; a run that ends earlier is picked up again (resume).
	migrateJobDeadline = 3600
	// migrateJobTTL removes a finished Job the tool could not delete
	// itself (for example when the updater restarted mid-run).
	migrateJobTTL = 86400
	// dspaObjectStoreCondition is DSPO's object-storage condition type
	// (controllers/config/defaults.go ObjectStoreAvailable, rhoai-3.6).
	dspaObjectStoreCondition = "ObjectStoreAvailable"
)

var (
	// MigrateCopyTimeout bounds how long one run waits for the copy Job;
	// the rest of the 15-minute request deadline is for setup and the
	// pipeline servers. A copy still running then is picked up by the next
	// run. Tests shorten these.
	MigrateCopyTimeout = 8 * time.Minute
	MigratePoll        = 3 * time.Second
	// MigrateRepointTimeout bounds the wait for one pipeline server's
	// ObjectStoreAvailable condition after it was repointed.
	MigrateRepointTimeout = 2 * time.Minute
)

// migrateScript runs in the Job. The remotes "old" and "new" come from
// RCLONE_CONFIG_* variables (rclone docs, "Config file": environment
// variables), with the keys from secretKeyRef, so no credential is in the
// command, the arguments or the operation's output.
//   - copy --ignore-existing never deletes, and never overwrites an object
//     that already exists in the new storage (a re-run after pipeline
//     servers moved could otherwise replace newer data);
//   - check --one-way --download reads every object of the old storage and
//     its copy and compares the bytes, so it needs no hash support;
//     --combined lists each path with = (identical), + (missing in the new
//     storage), * (different) or ! (unreadable);
//   - size --json counts objects and bytes on both sides.
//
// The result is one JSON line in /dev/termination-log, which the kubelet
// puts in the pod status (terminationMessagePath), so no pod-log access is
// needed. Verified locally with this image against SeaweedFS 4.54 as a
// non-root UID without capabilities.
const migrateScript = `set -u
copy=0
rclone copy old: new: --ignore-existing || copy=$?
check=0
rclone check old: new: --one-way --download --combined /tmp/combined.txt || check=$?
count() { n=$(grep -c "^$1 " /tmp/combined.txt 2>/dev/null); echo "${n:-0}"; }
src=$(rclone size old: --json) || src=null
dst=$(rclone size new: --json) || dst=null
printf '{"copy":%d,"check":%d,"identical":%s,"missing":%s,"differ":%s,"errors":%s,"source":%s,"destination":%s}' \
  "$copy" "$check" "$(count =)" "$(count +)" "$(count '\*')" "$(count !)" "${src:-null}" "${dst:-null}" > /dev/termination-log
[ "$copy" -eq 0 ] && [ "$check" -eq 0 ]
`

// rcloneImage returns the copy Job's image, overridable with RCLONE_IMAGE
// (for example a mirror on a disconnected cluster). An override must be an
// rclone 1.x image with /bin/sh and grep.
func rcloneImage() string {
	if img := strings.TrimSpace(os.Getenv("RCLONE_IMAGE")); img != "" {
		return img
	}
	return rcloneDefaultImage
}

// MigrateOptions selects the operation: Migrate (the default) or, with
// RemoveOldStorage, the removal of the old storage after a migration.
type MigrateOptions struct {
	RemoveOldStorage bool
	// DeleteNamespace also deletes namespace minio, when this tool created
	// it (see legacyNamespaceOwned).
	DeleteNamespace bool
}

// rcloneProvider is rclone's S3 provider for a server. Both providers
// force path-style requests (backend/s3/provider/SeaweedFS.yaml and
// Minio.yaml, quirk force_path_style), which these in-cluster endpoints
// need; SeaweedFS's also selects list version 1 and does not trust multipart
// ETags as MD5 sums.
func rcloneProvider(server string) string {
	if server == "MinIO" {
		return "Minio"
	}
	return "SeaweedFS"
}

func migrateJobPath() string {
	return namespacedPath("batch/v1", "jobs", s3Namespace, migrateJobName)
}

func migrateJobCreatePath() string {
	return withToolFieldManager(namespacedPath("batch/v1", "jobs", s3Namespace, ""))
}

// migrateJob is the copy Job. The old and the new storage have the same
// keys (step 1 checks it), so both remotes read them from the new storage's
// Secret, the only one in the Job's namespace. The endpoints carry their
// scheme: rclone prepends https:// to one without (backend/s3/s3.go:1597
// at v1.76.0). Pods can reach both S3 ports: NetworkPolicy
// seaweedfs-ingress admits port 8333 from every pod in every namespace
// (namespaceSelector {}), and the old storage's policies do the same for
// its port.
func migrateJob(oldServer string) map[string]interface{} {
	secretEnv := func(name, key string) map[string]interface{} {
		return map[string]interface{}{"name": name, "valueFrom": map[string]interface{}{"secretKeyRef": map[string]interface{}{"name": s3Secret, "key": key}}}
	}
	remote := func(name, provider, endpoint string) []map[string]interface{} {
		prefix := "RCLONE_CONFIG_" + name + "_"
		return []map[string]interface{}{
			{"name": prefix + "TYPE", "value": "s3"},
			{"name": prefix + "PROVIDER", "value": provider},
			{"name": prefix + "ENDPOINT", "value": endpoint},
			{"name": prefix + "REGION", "value": s3Region},
			secretEnv(prefix+"ACCESS_KEY_ID", s3AccessKeyField),
			secretEnv(prefix+"SECRET_ACCESS_KEY", s3SecretKeyField),
		}
	}
	env := append(remote("OLD", rcloneProvider(oldServer), "http://"+legacyS3Host()), remote("NEW", "SeaweedFS", s3Endpoint())...)
	// The assigned UID has no home directory in the image.
	env = append(env, map[string]interface{}{"name": "HOME", "value": "/tmp"})
	podLabels := map[string]interface{}{"app": migrateAppLabel, managedByLabelKey: managedByLabelValue}
	return map[string]interface{}{
		"apiVersion": "batch/v1", "kind": "Job",
		"metadata": map[string]interface{}{"name": migrateJobName, "namespace": s3Namespace, "labels": toolLabels()},
		"spec": map[string]interface{}{
			"backoffLimit":            0,
			"activeDeadlineSeconds":   migrateJobDeadline,
			"ttlSecondsAfterFinished": migrateJobTTL,
			"template": map[string]interface{}{
				"metadata": map[string]interface{}{"labels": podLabels},
				"spec": map[string]interface{}{
					"restartPolicy": "Never",
					// The copy needs no Kubernetes API access.
					"automountServiceAccountToken": false,
					"containers": []map[string]interface{}{{
						"name":    migrateContainer,
						"image":   rcloneImage(),
						"command": []string{"/bin/sh", "-c", migrateScript},
						"env":     env,
						"resources": map[string]interface{}{
							"requests": map[string]interface{}{"cpu": "100m", "memory": "128Mi"},
							"limits":   map[string]interface{}{"cpu": "500m", "memory": "512Mi"},
						},
						"securityContext":          restrictedSecurityContext(),
						"terminationMessagePolicy": "File",
					}},
				},
			},
		},
	}
}

// copySize is `rclone size --json`.
type copySize struct {
	Count    int64 `json:"count"`
	Bytes    int64 `json:"bytes"`
	Sizeless int64 `json:"sizeless"`
}

// copyResult is the Job's termination message (migrateScript).
type copyResult struct {
	Copy        int       `json:"copy"`
	Check       int       `json:"check"`
	Identical   int64     `json:"identical"`
	Missing     int64     `json:"missing"`
	Differ      int64     `json:"differ"`
	Errors      int64     `json:"errors"`
	Source      *copySize `json:"source"`
	Destination *copySize `json:"destination"`
}

// problem explains why the copy is not verified, or returns "". Every
// object of the old storage must be in the new one, byte for byte. With
// strict (nothing uses the new storage yet, so it holds only copies) both
// sides must also hold the same number of objects and bytes; once pipeline
// servers write to the new storage it may hold more.
func (r copyResult) problem(strict bool) string {
	switch {
	case r.Copy != 0:
		return fmt.Sprintf("rclone copy failed (exit code %d)", r.Copy)
	case r.Source == nil || r.Destination == nil:
		return "the object counts could not be read"
	case r.Missing > 0 || r.Differ > 0 || r.Errors > 0:
		return fmt.Sprintf("the check found %s missing in the new storage, %s different and %s unreadable", countNoun(int(r.Missing), "object", "objects"), countNoun(int(r.Differ), "object", "objects"), countNoun(int(r.Errors), "object", "objects"))
	case r.Check != 0:
		return fmt.Sprintf("rclone check failed (exit code %d)", r.Check)
	case r.Identical != r.Source.Count:
		return fmt.Sprintf("the check found %d of the %d objects identical", r.Identical, r.Source.Count)
	case strict && (r.Destination.Count != r.Source.Count || r.Destination.Bytes != r.Source.Bytes):
		return fmt.Sprintf("the old storage holds %s, the new one %s", sizeText(*r.Source), sizeText(*r.Destination))
	case r.Destination.Count < r.Source.Count:
		return fmt.Sprintf("the old storage holds %s, the new one only %s", sizeText(*r.Source), sizeText(*r.Destination))
	}
	return ""
}

func sizeText(s copySize) string {
	return fmt.Sprintf("%s (%s)", countNoun(int(s.Count), "object", "objects"), countNoun(int(s.Bytes), "byte", "bytes"))
}

// migrateRun is one run of Migrate or of the removal: it streams each step
// and collects the log lines for the response.
type migrateRun struct {
	c    *Client
	emit func(UpdateStepEvent)
	logs []string
}

func (m *migrateRun) step(id, status, msg string) {
	m.logs = append(m.logs, msg)
	if m.emit != nil {
		m.emit(UpdateStepEvent{Step: id, Status: status, Message: msg})
	}
}

func (m *migrateRun) fail(id, msg, code, activity string) (*types.OperationResponse, error) {
	if m.emit != nil {
		m.emit(UpdateStepEvent{Step: id, Status: "failed", Message: msg, ErrorCode: code})
	}
	m.logs = append(m.logs, msg)
	slog.Warn("s3 storage migration stopped", "step", id, "reason", activity, "user", getUser(m.c))
	recordS3Activity(m.c, "migrate-storage", fmt.Sprintf("%s (%s)", legacyNamespace, activity), false)
	return &types.OperationResponse{Success: false, Message: msg, Logs: m.logs, ErrorCode: code}, nil
}

// readLegacyKeys reads the old storage's keys from minio-secret.
func readLegacyKeys(c *Client) (s3Keys, error) {
	path, _ := legacyGetPath("Secret", legacySecret)
	body, _, err := c.get(path)
	if err != nil {
		return s3Keys{}, fmt.Errorf("read Secret %s/%s: %w", legacyNamespace, legacySecret, err)
	}
	var secret struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(body, &secret); err != nil {
		return s3Keys{}, fmt.Errorf("parse Secret %s/%s: %w", legacyNamespace, legacySecret, err)
	}
	keys := s3Keys{AccessKey: decodeBase64Field(secret.Data[legacyAccessKeyField]), SecretKey: decodeBase64Field(secret.Data[legacySecretKeyField])}
	if keys.AccessKey == "" || keys.SecretKey == "" {
		return s3Keys{}, fmt.Errorf("secret %s/%s has no %s or %s", legacyNamespace, legacySecret, legacyAccessKeyField, legacySecretKeyField)
	}
	return keys, nil
}

// MigrateS3Storage runs Migrate, or with opts.RemoveOldStorage the removal
// of the old storage, streaming each step to onStep.
func MigrateS3Storage(c *Client, opts MigrateOptions, onStep func(UpdateStepEvent)) (*types.OperationResponse, error) {
	m := &migrateRun{c: c, emit: onStep}
	m.step("check", "running", fmt.Sprintf("Checking the storage in namespace '%s'...", legacyNamespace))
	legacy, err := readLegacyStorage(c)
	if err != nil {
		return m.fail("check", fmt.Sprintf("Cannot read the storage in namespace '%s'; nothing was changed: %v.%s", legacyNamespace, err, templateHint(err)), errorCodeFromK8sErr(err), "read failed")
	}
	if legacy == nil {
		m.step("check", "success", fmt.Sprintf("No storage from an earlier version in namespace '%s'.", legacyNamespace))
		recordS3Activity(c, "migrate-storage", legacyNamespace+" (nothing to do)", true)
		return &types.OperationResponse{Success: true, Message: fmt.Sprintf("Nothing to do: there is no storage from an earlier version in namespace '%s'.", legacyNamespace), Logs: m.logs}, nil
	}
	if legacy.terminating {
		return m.fail("check", fmt.Sprintf("Namespace '%s' is being deleted, so its storage cannot be migrated. Nothing was changed.", legacyNamespace), "terminating", "namespace terminating")
	}
	dspas, err := listDSPAs(c, "")
	if err != nil && !errors.Is(err, errDSPACRDMissing) {
		return m.fail("check", fmt.Sprintf("Cannot list the pipeline servers; nothing was changed: %v.%s", err, templateHint(err)), errorCodeFromK8sErr(err), "pipeline server list failed")
	}
	if legacy.addressErr != nil && len(dspas) > 0 {
		return m.fail("check", fmt.Sprintf("Cannot read the old storage's Routes and Service, so which pipeline servers use it is unknown; nothing was changed: %v.%s", legacy.addressErr, templateHint(legacy.addressErr)), errorCodeFromK8sErr(legacy.addressErr), "address read failed")
	}
	if opts.RemoveOldStorage {
		return m.removeOldStorage(legacy, dspas, opts.DeleteNamespace)
	}
	return m.migrate(legacy, dspas)
}

// sourceReady checks that the old storage's server can be copied from and
// reads its keys.
func (m *migrateRun) sourceReady(legacy *legacyStorage, step string) (s3Keys, *types.OperationResponse) {
	switch {
	case legacy.server == "":
		resp, _ := m.fail(step, fmt.Sprintf("No server runs in namespace '%s' any more, so there is nothing to copy from. Nothing was changed. Tear down removes what is left.", legacyNamespace), "prerequisites", "no server")
		return s3Keys{}, resp
	case !legacy.ready:
		resp, _ := m.fail(step, fmt.Sprintf("%s in namespace '%s' is not running, so its objects cannot be copied. Nothing was changed. Start it again (Deployment %s), then retry.", legacy.server, legacyNamespace, map[string]string{"SeaweedFS": s3Deployment, "MinIO": legacyMinIODeployment}[legacy.server]), "prerequisites", "server not ready")
		return s3Keys{}, resp
	}
	keys, err := readLegacyKeys(m.c)
	if err != nil {
		resp, _ := m.fail(step, fmt.Sprintf("Cannot read the old storage's keys; nothing was changed: %v.%s", err, templateHint(err)), firstNonEmpty(errorCodeFromK8sErr(err), "prerequisites"), "keys unreadable")
		return s3Keys{}, resp
	}
	return keys, nil
}

// newStorageMatches reports whether the new storage is ready with the given
// keys; an error response when it holds other keys.
func (m *migrateRun) newStorageMatches(keys s3Keys, step string) (bool, *types.OperationResponse) {
	stored, found, err := readS3Keys(m.c)
	if err != nil {
		resp, _ := m.fail(step, fmt.Sprintf("Cannot read the new storage's keys: %v.%s", err, templateHint(err)), errorCodeFromK8sErr(err), "new keys unreadable")
		return false, resp
	}
	if found && stored != keys {
		resp, _ := m.fail(step, fmt.Sprintf("The storage in namespace '%s' has other keys than the one in namespace '%s', so clients that keep their keys could not use it. Nothing was changed. Tear down the storage in '%s' (it holds no migrated data yet unless you put some there), then migrate again.", s3Namespace, legacyNamespace, s3Namespace), "conflict", "keys differ")
		return false, resp
	}
	state := getS3Status(m.c)
	return found && state.Ready && state.ManagedByTool && state.RepairNeeded == "", nil
}

// migrate runs steps 1 to 3.
func (m *migrateRun) migrate(legacy *legacyStorage, dspas []dspaInfo) (*types.OperationResponse, error) {
	keys, resp := m.sourceReady(legacy, "check")
	if resp != nil {
		return resp, nil
	}
	m.step("check", "success", fmt.Sprintf("OK: %s in namespace '%s' is running", legacy.server, legacyNamespace))

	// Step 1: the new storage, with the old keys.
	m.step("storage", "running", fmt.Sprintf("Setting up the storage in namespace '%s' with the same keys...", s3Namespace))
	ready, resp := m.newStorageMatches(keys, "storage")
	if resp != nil {
		return resp, nil
	}
	if ready {
		m.step("storage", "success", "OK: The new storage is already set up with the same keys")
	} else {
		log := &opLog{observe: func(line string) {
			if m.emit != nil {
				m.emit(UpdateStepEvent{Step: "storage", Status: "running", Message: line})
			}
		}}
		res, err := setupS3Storage(m.c, keys, log)
		m.logs = append(m.logs, log.lines...)
		if err != nil {
			return nil, err
		}
		if !res.Success {
			return m.fail("storage", "Setting up the new storage did not finish: "+res.Message+" The old storage is unchanged; run Migrate storage again to continue.", res.ErrorCode, "setup failed")
		}
		m.step("storage", "success", "OK: The new storage is ready")
	}

	// Step 2: copy and verify.
	result, resp := m.copyAndVerify(legacy, dspas, "copy")
	if resp != nil {
		return resp, nil
	}

	// Step 3: the pipeline servers.
	repointed, listed, resp := m.repoint(legacy, dspas, keys)
	if resp != nil {
		return resp, nil
	}

	msg := fmt.Sprintf("Migrated %s to namespace '%s'.", sizeText(*result.Source), s3Namespace)
	if len(repointed) > 0 {
		msg += fmt.Sprintf(" %s now %s it: %s.", countNoun(len(repointed), "pipeline server", "pipeline servers"), verb(len(repointed), "uses", "use"), strings.Join(repointed, ", "))
	}
	if len(listed) > 0 {
		msg += fmt.Sprintf(" Not changed by this tool, still on the old storage: %s.", strings.Join(listed, "; "))
	}
	msg += fmt.Sprintf(" The old storage in namespace '%s' is kept until you remove it.", legacyNamespace)
	slog.Info("s3 storage migrated", "user", getUser(m.c), "objects", result.Source.Count)
	recordS3Activity(m.c, "migrate-storage", fmt.Sprintf("%s -> %s (%s, %d pipeline servers)", legacyNamespace, s3Namespace, sizeText(*result.Source), len(repointed)), true)
	return &types.OperationResponse{Success: true, Message: msg, Logs: m.logs}, nil
}

// copyAndVerify runs the copy Job (or waits for one a previous run left
// running) and verifies its result. The Job is deleted by UID once its
// result is read.
func (m *migrateRun) copyAndVerify(legacy *legacyStorage, dspas []dspaInfo, step string) (copyResult, *types.OperationResponse) {
	c := m.c
	fail := func(msg, code, activity string) (copyResult, *types.OperationResponse) {
		resp, _ := m.fail(step, msg, code, activity)
		return copyResult{}, resp
	}
	m.step(step, "running", fmt.Sprintf("Copying every bucket and object from '%s' to '%s' (Job %s)...", legacyNamespace, s3Namespace, migrateJobName))

	// Whether anything uses the new storage yet decides how strictly the
	// sizes must match (copyResult.problem).
	endpoints, err := readS3Endpoints(c)
	if err != nil {
		return fail(fmt.Sprintf("Cannot read the new storage's Service; the copy did not start: %v.%s", err, templateHint(err)), errorCodeFromK8sErr(err), "endpoints unreadable")
	}
	strict := true
	for _, d := range dspas {
		if endpoints.address.usedBy(d.Meta.Namespace, d.Host) {
			strict = false
		}
	}

	job, found, err := readMigrateJob(c)
	if err != nil {
		return fail(fmt.Sprintf("Cannot read Job %s/%s; the copy did not start: %v.%s", s3Namespace, migrateJobName, err, templateHint(err)), errorCodeFromK8sErr(err), "job read failed")
	}
	if found && !job.Meta.hasToolLabel() {
		return fail(fmt.Sprintf("Job %s/%s exists and was not created by this tool, so it was left unchanged and the copy did not start. Delete it, then retry.", s3Namespace, migrateJobName), "not_managed", "foreign job")
	}
	if found && !job.Meta.terminating() && !job.finished() {
		m.step(step, "running", "A copy started by an earlier run is still running: waiting for it")
	} else {
		if found {
			// A finished Job whose result no run read (the request ended
			// first): its copy may be out of date, so run a new one.
			if !job.Meta.terminating() {
				if _, err := deleteWithUID(c, migrateJobPath(), job.Meta.UID); err != nil && !IsK8sError(err, 404) {
					return fail(fmt.Sprintf("Cannot delete the finished Job %s/%s of an earlier run: %v.%s", s3Namespace, migrateJobName, err, templateHint(err)), errorCodeFromK8sErr(err), "old job delete failed")
				}
			}
			gone, err := waitForDeletion(c, migrateJobPath(), S3DeleteTimeout, S3DeletePoll)
			if err != nil || !gone {
				return fail(fmt.Sprintf("Job %s/%s of an earlier run is still being deleted; retry in a minute.", s3Namespace, migrateJobName), "in_progress", "old job lingering")
			}
		}
		obj := migrateJob(legacy.server)
		data, _ := json.Marshal(obj)
		if _, _, err := c.do(http.MethodPost, migrateJobCreatePath(), "application/json", data, url.Values{"dryRun": {"All"}}); err != nil {
			return fail(fmt.Sprintf("The API server rejects the copy Job: %v. Nothing was changed.%s", err, templateHint(err)), firstNonEmpty(errorCodeFromK8sErr(err), "validation"), "job dry run failed")
		}
		body, _, err := c.post(migrateJobCreatePath(), data)
		if err != nil {
			return fail(fmt.Sprintf("Cannot create the copy Job: %v.%s", err, templateHint(err)), errorCodeFromK8sErr(err), "job create failed")
		}
		if job, err = parseMigrateJob(body); err != nil {
			return fail(fmt.Sprintf("Cannot read the copy Job: %v", err), "", "job parse failed")
		}
	}

	job, issue, err := waitMigrateJob(c, job)
	switch {
	case errors.Is(err, errS3CannotStart):
		m.deleteMigrateJob(job)
		return fail(fmt.Sprintf("The copy Job's pod cannot start (%s: %s). Nothing else was changed. Fix the cause (for example set RCLONE_IMAGE to a pullable rclone image), then run Migrate storage again.", issue.Reason, truncateMessage(issue.Message)), "not_ready", "job cannot start")
	case errors.Is(err, errS3NotReady):
		return fail(fmt.Sprintf("The copy is still running after %s. Nothing else was changed. Run Migrate storage again to keep waiting for it.", MigrateCopyTimeout), "in_progress", "copy still running")
	case err != nil:
		return fail(fmt.Sprintf("Stopped waiting for the copy Job: %v.%s", err, templateHint(err)), errorCodeFromK8sErr(err), "job wait failed")
	}
	result, readErr := readMigrateResult(c, job)
	m.deleteMigrateJob(job)
	if readErr != nil {
		return fail(fmt.Sprintf("The copy Job ended without a readable result (%v). Nothing else was changed; run Migrate storage again.", readErr), "", "no result")
	}
	if p := result.problem(strict); p != "" {
		return fail(fmt.Sprintf("The copy is not complete: %s. Nothing else was changed: no pipeline server was moved and the old storage is kept. Run Migrate storage again to retry.", p), "copy_mismatch", "copy mismatch: "+p)
	}
	m.step(step, "success", fmt.Sprintf("OK: %s copied and checked byte for byte", sizeText(*result.Source)))
	return result, nil
}

// deleteMigrateJob deletes the copy Job by UID; its pod goes with it
// (background propagation).
func (m *migrateRun) deleteMigrateJob(job migrateJobInfo) {
	if _, err := deleteWithUID(m.c, migrateJobPath(), job.Meta.UID); err != nil && !IsK8sError(err, 404) {
		m.logs = append(m.logs, fmt.Sprintf("Warning: cannot delete Job %s/%s: %v (it removes itself a day after it finished)", s3Namespace, migrateJobName, err))
		return
	}
	m.logs = append(m.logs, fmt.Sprintf("OK: Job %s/%s deleted", s3Namespace, migrateJobName))
}

// migrateJobInfo is the part of the copy Job the tool reads.
type migrateJobInfo struct {
	Meta     objectMeta
	Selector map[string]string
	// Conditions Complete and Failed end the Job (batch/v1 JobConditionType).
	Complete, Failed bool
	FailReason       string
}

func (j migrateJobInfo) finished() bool { return j.Complete || j.Failed }

// selector is the Job's pod selector as a label selector string. The API
// server sets spec.selector for every Job without manualSelector.
func (j migrateJobInfo) selector() string {
	pairs := make([]string, 0, len(j.Selector))
	for k, v := range j.Selector {
		pairs = append(pairs, k+"="+v)
	}
	sort.Strings(pairs)
	return strings.Join(pairs, ",")
}

func parseMigrateJob(body []byte) (migrateJobInfo, error) {
	var obj struct {
		Metadata objectMeta `json:"metadata"`
		Spec     struct {
			Selector struct {
				MatchLabels map[string]string `json:"matchLabels"`
			} `json:"selector"`
		} `json:"spec"`
		Status struct {
			Conditions []struct {
				Type    string `json:"type"`
				Status  string `json:"status"`
				Reason  string `json:"reason"`
				Message string `json:"message"`
			} `json:"conditions"`
		} `json:"status"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		return migrateJobInfo{}, err
	}
	j := migrateJobInfo{Meta: obj.Metadata, Selector: obj.Spec.Selector.MatchLabels}
	for _, cond := range obj.Status.Conditions {
		if cond.Status != "True" {
			continue
		}
		switch cond.Type {
		case "Complete":
			j.Complete = true
		case "Failed":
			j.Failed, j.FailReason = true, firstNonEmpty(cond.Reason, cond.Message)
		}
	}
	return j, nil
}

func readMigrateJob(c *Client) (migrateJobInfo, bool, error) {
	body, _, err := c.get(migrateJobPath())
	if IsK8sError(err, 404) {
		return migrateJobInfo{}, false, nil
	}
	if err != nil {
		return migrateJobInfo{}, false, err
	}
	j, err := parseMigrateJob(body)
	return j, err == nil, err
}

// waitMigrateJob waits, bounded by MigrateCopyTimeout, until the Job has
// finished. It returns errS3CannotStart as soon as its pod reports a
// terminal waiting reason (ImagePullBackOff, ...), and errS3NotReady on
// timeout.
func waitMigrateJob(c *Client, job migrateJobInfo) (migrateJobInfo, workloadPodIssue, error) {
	ctx, cancel := context.WithTimeout(c.ctx, MigrateCopyTimeout)
	defer cancel()
	cc := c.WithContext(ctx)
	for {
		if job.finished() {
			return job, workloadPodIssue{}, nil
		}
		if sel := job.selector(); sel != "" {
			if issue, err := inspectPods(cc, s3Namespace, sel, rcloneImage()); err == nil && issue.Terminal {
				return job, issue, errS3CannotStart
			}
		}
		select {
		case <-ctx.Done():
			if c.ctx.Err() != nil {
				return job, workloadPodIssue{}, c.ctx.Err()
			}
			return job, workloadPodIssue{}, errS3NotReady
		case <-time.After(MigratePoll):
		}
		next, found, err := readMigrateJob(cc)
		switch {
		case ctx.Err() != nil:
			continue
		case err != nil && !IsNetworkError(err):
			return job, workloadPodIssue{}, err
		case err == nil && !found:
			return job, workloadPodIssue{}, fmt.Errorf("the copy Job %s/%s disappeared while the copy was running", s3Namespace, migrateJobName)
		case err == nil:
			if next.Meta.UID != job.Meta.UID {
				return job, workloadPodIssue{}, fmt.Errorf("the copy Job %s/%s was replaced while the copy was running", s3Namespace, migrateJobName)
			}
			job = next
		}
	}
}

// readMigrateResult reads the copy's result from the termination message of
// the Job's pod.
func readMigrateResult(c *Client, job migrateJobInfo) (copyResult, error) {
	sel := job.selector()
	if sel == "" {
		return copyResult{}, fmt.Errorf("the Job has no pod selector")
	}
	body, _, err := c.get(namespacedPath("v1", "pods", s3Namespace, "") + "?labelSelector=" + url.QueryEscape(sel))
	if err != nil {
		return copyResult{}, fmt.Errorf("list its pods: %w", err)
	}
	var list struct {
		Items []struct {
			Status struct {
				ContainerStatuses []struct {
					Name  string `json:"name"`
					State struct {
						Terminated *struct {
							ExitCode int    `json:"exitCode"`
							Reason   string `json:"reason"`
							Message  string `json:"message"`
						} `json:"terminated"`
					} `json:"state"`
				} `json:"containerStatuses"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return copyResult{}, fmt.Errorf("parse its pods: %w", err)
	}
	reason := firstNonEmpty(job.FailReason, "no pod finished")
	for _, p := range list.Items {
		for _, cs := range p.Status.ContainerStatuses {
			if cs.Name != migrateContainer || cs.State.Terminated == nil {
				continue
			}
			t := cs.State.Terminated
			var r copyResult
			if err := json.Unmarshal([]byte(strings.TrimSpace(t.Message)), &r); err == nil {
				return r, nil
			}
			reason = firstNonEmpty(t.Reason, job.FailReason, fmt.Sprintf("exit code %d", t.ExitCode))
		}
	}
	return copyResult{}, errors.New(reason)
}

// repointable reports whether the tool may move a pipeline server that
// uses the old storage, or why not.
func repointable(d dspaInfo) (string, bool) {
	switch {
	case !d.managedByTool():
		return "not created by this tool", false
	case d.Meta.terminating():
		return "being deleted", false
	case d.Port != "":
		return "its object storage has its own port setting", false
	case !strings.EqualFold(d.Scheme, "http"):
		return fmt.Sprintf("its object storage uses scheme %q", d.Scheme), false
	}
	if _, ok := toolDSPAPath(d.Meta.Namespace, d.Meta.Name); !ok {
		return "not a name this tool creates", false
	}
	return "", true
}

// objectStoreReady reports whether DSPO's ObjectStoreAvailable condition is
// True for the DSPA's current generation.
func (d dspaInfo) objectStoreReady() bool {
	return d.objectStoreReadyAt(d.Meta.Generation)
}

// objectStoreReadyAt reports whether ObjectStoreAvailable is True for
// generation or a later one. DSPO stamps every condition with the
// generation it reconciled (dspa_status.go GetConditions), so a True from
// before a change does not count.
func (d dspaInfo) objectStoreReadyAt(generation int64) bool {
	return d.ObjectStore != nil && d.ObjectStore.Status == "True" && d.ObjectStore.ObservedGeneration >= generation
}

// repoint moves the pipeline servers this tool created from the old storage
// to the new one. It returns the moved ones and those it left (with why).
// Every write is dry-run first. Each server is waited for before the next
// is moved; one that does not reach the new storage stops the run, so the
// old storage stays.
func (m *migrateRun) repoint(legacy *legacyStorage, dspas []dspaInfo, keys s3Keys) (moved, listed []string, resp *types.OperationResponse) {
	c := m.c
	failed := func(msg, code, activity string) ([]string, []string, *types.OperationResponse) {
		r, _ := m.fail("repoint", msg, code, activity)
		return moved, listed, r
	}
	m.step("repoint", "running", "Moving the pipeline servers this tool created to the new storage...")
	endpoints, err := readS3Endpoints(c)
	if err != nil {
		return failed(fmt.Sprintf("Cannot read the new storage's Service: %v.%s", err, templateHint(err)), errorCodeFromK8sErr(err), "endpoints unreadable")
	}
	var todo []dspaInfo
	for _, d := range dspas {
		name := d.Meta.Namespace + "/" + d.Meta.Name
		switch {
		case legacy.address.usedBy(d.Meta.Namespace, d.Host):
			if why, ok := repointable(d); ok {
				todo = append(todo, d)
			} else {
				listed = append(listed, name+" ("+why+")")
			}
		case d.managedByTool() && endpoints.address.usedBy(d.Meta.Namespace, d.Host) && !d.Meta.terminating() && !d.objectStoreReady():
			// Moved by an earlier run that stopped: it must work before
			// anything else happens.
			return failed(fmt.Sprintf("Pipeline server %s already uses the new storage but does not reach it (%s). The old storage is kept. Check the pipeline server's conditions, then run Migrate storage again.", name, objectStoreText(d)), "not_ready", "moved server not ready")
		}
	}
	sort.Slice(todo, func(i, j int) bool {
		return todo[i].Meta.Namespace+"/"+todo[i].Meta.Name < todo[j].Meta.Namespace+"/"+todo[j].Meta.Name
	})
	sort.Strings(listed)
	if len(todo) == 0 {
		m.step("repoint", "success", "No pipeline server of this tool uses the old storage")
		return nil, listed, nil
	}

	type plan struct {
		d           dspaInfo
		secret      map[string]interface{}
		secretMeta  objectMeta
		secretFound bool
		patch       []byte
	}
	plans := make([]*plan, 0, len(todo))
	for _, d := range todo {
		project := d.Meta.Namespace
		secretPath := namespacedPath("v1", "secrets", project, dspaSecretName)
		secretMeta, secretFound, err := readObjectMeta(c, secretPath)
		if err != nil {
			return failed(fmt.Sprintf("Cannot read Secret %s/%s; no pipeline server was moved: %v.%s", project, dspaSecretName, err, templateHint(err)), errorCodeFromK8sErr(err), "secret read failed")
		}
		if secretFound && !secretMeta.hasToolLabel() {
			return failed(fmt.Sprintf("Secret %s/%s exists and was not created by this tool, so pipeline server %s/%s was not moved and nothing was changed.", project, dspaSecretName, project, d.Meta.Name), "not_managed", "foreign secret")
		}
		patch, _ := json.Marshal(map[string]interface{}{
			"metadata": map[string]interface{}{"resourceVersion": d.Meta.ResourceVersion, "labels": toolLabels()},
			"spec": map[string]interface{}{"objectStorage": map[string]interface{}{"externalStorage": map[string]interface{}{
				"host": s3Host(),
				"s3CredentialsSecret": map[string]interface{}{
					"secretName": dspaSecretName,
					"accessKey":  s3AccessKeyField,
					"secretKey":  s3SecretKeyField,
				},
			}}},
		})
		plans = append(plans, &plan{
			d: d, patch: patch, secretMeta: secretMeta, secretFound: secretFound,
			secret: map[string]interface{}{
				"apiVersion": "v1", "kind": "Secret",
				"metadata":   map[string]interface{}{"name": dspaSecretName, "namespace": project, "labels": toolLabels()},
				"stringData": map[string]interface{}{s3AccessKeyField: keys.AccessKey, s3SecretKeyField: keys.SecretKey},
			},
		})
	}
	writeSecret := func(p *plan, dryRun bool) error {
		project := p.d.Meta.Namespace
		secretPath := namespacedPath("v1", "secrets", project, dspaSecretName)
		read := func() (objectMeta, bool, error) { return readObjectMeta(c, secretPath) }
		_, meta, found, err := ensureToolObject(c, fmt.Sprintf("Secret %s/%s", project, dspaSecretName), secretPath,
			withToolFieldManager(namespacedPath("v1", "secrets", project, "")), p.secret, p.secretMeta, p.secretFound, read, objectMeta.hasToolLabel, dryRun)
		p.secretMeta, p.secretFound = meta, found
		return err
	}
	patchDSPA := func(p *plan, dryRun bool) ([]byte, error) {
		q := url.Values{"fieldManager": {toolFieldManager}}
		if dryRun {
			q.Set("dryRun", "All")
		}
		path, ok := toolDSPAPath(p.d.Meta.Namespace, p.d.Meta.Name)
		if !ok {
			return nil, fmt.Errorf("pipeline server %s is not a name this tool creates", p.d.Meta.Name)
		}
		body, _, err := c.do(http.MethodPatch, path, "application/merge-patch+json", p.patch, q)
		return body, err
	}

	m.step("repoint", "running", "Checking every change with the API server (dry run)...")
	for _, p := range plans {
		name := p.d.Meta.Namespace + "/" + p.d.Meta.Name
		if err := writeSecret(p, true); err != nil {
			msg, code := pipelineWriteFailure(fmt.Sprintf("secret '%s'", dspaSecretName), p.d.Meta.Namespace, err)
			return failed(msg+" No pipeline server was moved.", code, "secret dry run failed")
		}
		if _, err := patchDSPA(p, true); err != nil {
			code := errorCodeFromK8sErr(err)
			if IsK8sError(err, http.StatusConflict) {
				code = "conflict"
			}
			return failed(fmt.Sprintf("The API server rejects the change to pipeline server %s: %v. No pipeline server was moved.%s", name, err, templateHint(err)), firstNonEmpty(code, "validation"), "DSPA dry run failed")
		}
	}
	m.step("repoint", "running", "OK: The API server accepts every change")

	for _, p := range plans {
		name := p.d.Meta.Namespace + "/" + p.d.Meta.Name
		done := "none"
		if len(moved) > 0 {
			done = strings.Join(moved, ", ")
		}
		m.step("repoint", "running", fmt.Sprintf("Moving pipeline server %s...", name))
		if err := writeSecret(p, false); err != nil {
			msg, code := pipelineWriteFailure(fmt.Sprintf("secret '%s'", dspaSecretName), p.d.Meta.Namespace, err)
			return failed(fmt.Sprintf("%s Already moved: %s. The old storage is kept; run Migrate storage again.", msg, done), code, "secret write failed")
		}
		body, err := patchDSPA(p, false)
		if err != nil {
			code := errorCodeFromK8sErr(err)
			if IsK8sError(err, http.StatusConflict) {
				code = "conflict"
			}
			return failed(fmt.Sprintf("Cannot move pipeline server %s: %v. Already moved: %s. The old storage is kept; run Migrate storage again.%s", name, err, done, templateHint(err)), code, "DSPA patch failed")
		}
		meta, _ := parseObjectMeta(body)
		d, err := waitObjectStore(c, p.d.Meta.Namespace, p.d.Meta.Name, meta.Generation)
		if err != nil {
			return failed(fmt.Sprintf("Pipeline server %s was moved to the new storage, but %s. The old storage is kept. Check the pipeline server's conditions, then run Migrate storage again.", name, err.Error()), "not_ready", "moved server not ready")
		}
		moved = append(moved, name)
		note := ""
		if d.ObjectStore.Message != "" {
			note = " (" + d.ObjectStore.Message + ")"
		}
		m.step("repoint", "running", fmt.Sprintf("OK: %s reaches the new storage%s", name, note))
	}
	m.step("repoint", "success", fmt.Sprintf("OK: %s moved", countNoun(len(moved), "pipeline server", "pipeline servers")))
	return moved, listed, nil
}

func objectStoreText(d dspaInfo) string {
	switch {
	case d.ObjectStore == nil:
		return "the data-science-pipelines operator reports no ObjectStoreAvailable condition yet"
	case d.ObjectStore.ObservedGeneration < d.Meta.Generation:
		return "the data-science-pipelines operator has not checked the change yet"
	}
	return fmt.Sprintf("ObjectStoreAvailable is %s: %s", d.ObjectStore.Status, firstNonEmpty(truncateMessage(d.ObjectStore.Message), d.ObjectStore.Reason))
}

// waitObjectStore waits, bounded by MigrateRepointTimeout, until DSPO
// reports ObjectStoreAvailable=True for generation (or a later one). A False
// condition does not end the wait: DSPO requeues and checks again, for
// example while the new Service's address propagates.
func waitObjectStore(c *Client, project, name string, generation int64) (dspaInfo, error) {
	ctx, cancel := context.WithTimeout(c.ctx, MigrateRepointTimeout)
	defer cancel()
	cc := c.WithContext(ctx)
	last := dspaInfo{Meta: objectMeta{Generation: generation}}
	for {
		list, err := listDSPAs(cc, project)
		if err != nil && ctx.Err() == nil && !IsNetworkError(err) {
			return last, fmt.Errorf("its status cannot be read: %v", err)
		}
		for _, d := range list {
			if d.Meta.Name != name {
				continue
			}
			if d.objectStoreReadyAt(generation) {
				return d, nil
			}
			last = d
		}
		select {
		case <-ctx.Done():
			if c.ctx.Err() != nil {
				return last, c.ctx.Err()
			}
			return last, fmt.Errorf("after %s %s", MigrateRepointTimeout, objectStoreText(last))
		case <-time.After(MigratePoll):
		}
	}
}

// legacyNamespaceOwned reports whether this tool created namespace minio:
// it carries the tool's label and its creation was recorded for one of the
// managers this tool's versions used (a POST with fieldManager
// rhoai-nightly-updater, or the earliest POSTs without User-Agent,
// "Go-http-client"; see createdBy). A label alone is not enough: anyone who
// can patch the namespace can add it. reason says why when it is not.
func legacyNamespaceOwned(meta objectMeta) (bool, string) {
	created := meta.createdBy(toolFieldManager, "Update") || meta.createdBy(legacyPostManager, "Update") || meta.createdByToolApply()
	switch {
	case meta.hasToolLabel() && created:
		return true, ""
	case created:
		return false, fmt.Sprintf("Namespace '%s' does not carry this tool's label, so it is kept.", legacyNamespace)
	default:
		return false, fmt.Sprintf("Namespace '%s' is kept: its metadata does not show that this tool created it (it may hold other things).", legacyNamespace)
	}
}

// readLegacyNamespace reads namespace minio's metadata.
func readLegacyNamespace(c *Client) (objectMeta, bool, error) {
	return readObjectMeta(c, "/api/v1/namespaces/"+legacyNamespace)
}

// removeOldStorage deletes the old storage once nothing of this tool needs
// it: a last copy and check, then every object the tool created there (by
// UID), and the namespace when asked and provably the tool's.
func (m *migrateRun) removeOldStorage(legacy *legacyStorage, dspas []dspaInfo, deleteNamespace bool) (*types.OperationResponse, error) {
	c := m.c
	keys, resp := m.sourceReady(legacy, "check")
	if resp != nil {
		return resp, nil
	}
	ready, resp := m.newStorageMatches(keys, "check")
	if resp != nil {
		return resp, nil
	}
	if !ready {
		return m.fail("check", fmt.Sprintf("The storage in namespace '%s' is not ready, so the old storage is kept. Run Migrate storage first.", s3Namespace), "prerequisites", "new storage not ready")
	}
	if reason := s3TeardownBlocker(dspas, legacy.address); reason != "" {
		return m.fail("check", "The old storage is still in use, so nothing was deleted. "+strings.Replace(reason, "Tear them down first.", "Run Migrate storage to move those this tool created; point the others at the new storage or tear them down.", 1), "prerequisites", "pipeline servers on the old storage")
	}
	endpoints, err := readS3Endpoints(c)
	if err != nil {
		return m.fail("check", fmt.Sprintf("Cannot read the new storage's Service; nothing was deleted: %v.%s", err, templateHint(err)), errorCodeFromK8sErr(err), "endpoints unreadable")
	}
	for _, d := range dspas {
		if d.managedByTool() && !d.Meta.terminating() && endpoints.address.usedBy(d.Meta.Namespace, d.Host) && !d.objectStoreReady() {
			return m.fail("check", fmt.Sprintf("Pipeline server %s/%s does not reach the new storage (%s), so the old storage is kept.", d.Meta.Namespace, d.Meta.Name, objectStoreText(d)), "not_ready", "moved server not ready")
		}
	}
	m.step("check", "success", "OK: Nothing of this tool uses the old storage")

	// Objects written to the old storage since the migration (hand-made
	// clients) are copied before anything is deleted.
	result, resp := m.copyAndVerify(legacy, dspas, "copy")
	if resp != nil {
		return resp, nil
	}

	m.step("remove", "running", fmt.Sprintf("Deleting the old storage in namespace '%s'...", legacyNamespace))
	var d storageDeletion
	var logs []string
	for _, o := range legacyStorageObjects {
		d.deleteOwnedObject(c, legacyNamespace, o.kind, o.name, &logs)
	}
	// The credentials Secret of pipeline servers an earlier version named
	// "dspa": they now use nightly-dspa-s3 (repoint).
	for _, ds := range dspas {
		if ds.Meta.Name == legacyDSPAName && ds.managedByTool() && ds.SecretName == dspaSecretName {
			var inProject []dspaInfo
			for _, o := range dspas {
				if o.Meta.Namespace == ds.Meta.Namespace {
					inProject = append(inProject, o)
				}
			}
			if note := deleteDSPASecret(c, ds.Meta.Namespace, &dspaInfo{Meta: objectMeta{UID: "none"}, SecretName: legacyDSPASecretName}, inProject, &logs); note != "" {
				d.failed = append(d.failed, strings.TrimSpace(note))
			}
		}
	}
	for _, l := range logs {
		m.step("remove", "running", l)
	}
	if len(d.failed) > 0 {
		code := "delete_failed"
		if len(d.deleted) > 0 {
			code = "partial_failure"
		}
		return m.fail("remove", fmt.Sprintf("Some objects of the old storage could not be deleted: %s. Run Remove old storage again.%s", strings.Join(d.failed, "; "), templateHint(d.forbidden)), code, "delete failed")
	}
	if len(d.pvcUIDs) > 0 {
		remaining, err := waitPVCsGone(c, d.pvcUIDs)
		if err != nil {
			return m.fail("remove", fmt.Sprintf("The old storage's objects were deleted, but whether its PVCs are gone cannot be checked: %v.%s", err, templateHint(err)), firstNonEmpty(errorCodeFromK8sErr(err), "in_progress"), "PVC wait failed")
		}
		if len(remaining) > 0 {
			return m.fail("remove", fmt.Sprintf("The old storage's objects were deleted, but PVC %s %s still terminating after %s. Run Remove old storage again to check.", strings.Join(remaining, ", "), verb(len(remaining), "is", "are"), S3DeleteTimeout), "in_progress", "PVCs terminating")
		}
	}
	m.step("remove", "success", fmt.Sprintf("OK: The old storage is deleted (%s)", strings.Join(d.deleted, ", ")))

	nsMsg := m.removeNamespace(deleteNamespace)
	if strings.HasPrefix(nsMsg, "!") {
		return m.fail("namespace", strings.TrimPrefix(nsMsg, "!"), "partial_failure", "namespace delete failed")
	}
	msg := fmt.Sprintf("The old storage in namespace '%s' was removed; its %s are in namespace '%s'.", legacyNamespace, sizeText(*result.Source), s3Namespace)
	if len(d.kept) > 0 {
		msg += fmt.Sprintf(" Left in place because this tool did not create them: %s.", strings.Join(d.kept, ", "))
	}
	msg += " " + nsMsg
	slog.Info("old s3 storage removed", "user", getUser(c), "deleted", strings.Join(d.deleted, ", "))
	recordS3Activity(c, "migrate-storage", fmt.Sprintf("%s removed (%s)", legacyNamespace, strings.TrimSuffix(nsMsg, ".")), true)
	return &types.OperationResponse{Success: true, Message: strings.TrimSpace(msg), Logs: m.logs}, nil
}

// removeNamespace deletes namespace minio when asked and owned by the tool,
// with a UID precondition. It returns the sentence for the result; one
// starting with "!" reports a failure.
func (m *migrateRun) removeNamespace(requested bool) string {
	c := m.c
	meta, found, err := readLegacyNamespace(c)
	switch {
	case err != nil:
		return fmt.Sprintf("!Cannot read namespace '%s', so it was kept: %v.%s", legacyNamespace, err, templateHint(err))
	case !found:
		return ""
	}
	owned, reason := legacyNamespaceOwned(meta)
	switch {
	case !requested && owned:
		m.step("namespace", "skipped", fmt.Sprintf("Namespace '%s' is kept, as chosen", legacyNamespace))
		return fmt.Sprintf("Namespace '%s' was kept, as chosen; delete it with `oc delete project %s` once you have checked it holds nothing else.", legacyNamespace, legacyNamespace)
	case !owned:
		m.step("namespace", "skipped", reason)
		return reason
	case meta.terminating():
		return fmt.Sprintf("Namespace '%s' is already being deleted.", legacyNamespace)
	}
	m.step("namespace", "running", fmt.Sprintf("Deleting namespace '%s'...", legacyNamespace))
	if _, err := deleteWithUID(c, "/api/v1/namespaces/"+legacyNamespace, meta.UID); err != nil && !IsK8sError(err, 404) {
		return fmt.Sprintf("!The old storage was removed, but namespace '%s' could not be deleted: %v.%s", legacyNamespace, err, templateHint(err))
	}
	m.step("namespace", "success", fmt.Sprintf("OK: Namespace '%s' is being deleted, with everything else in it", legacyNamespace))
	return fmt.Sprintf("Namespace '%s' is being deleted, with everything else in it.", legacyNamespace)
}
