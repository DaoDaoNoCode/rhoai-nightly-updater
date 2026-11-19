package cluster

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"slices"
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
//  1. Set up the new storage with fresh keys (or keep the keys of the new
//     storage when it is already set up). Never the old storage's keys:
//     whoever knew those (anyone who could read minio-secret, and whoever
//     planted the storage, see legacyNamespaceOurs) gets no access to the
//     new storage. The old keys are read only for the copy Job.
//  2. Copy every bucket and object with an rclone Job in namespace
//     seaweedfs, then verify (migrateScript). Any difference stops here.
//  3. Repoint the pipeline servers this tool created, with the new
//     storage's keys in their Secret, and wait until DSPO reports
//     ObjectStoreAvailable for the new storage. One that does not stops
//     the run.
//
// Remove the old storage (after the user has read the summary): copy and
// verify once more (objects written since), delete the old storage's objects
// by UID, and the namespace minio only when the tool provably created it
// and the user chose to. A removal that stopped part-way is finished by
// running it again (removeOldStorage).
const (
	migrateJobName = "seaweedfs-migrate"
	// migrateSourceSecret holds the old storage's keys for the copy Job's
	// "old" remote: a Job reads Secrets of its own namespace only. It is
	// owned by the Job (ownerReferences), so it goes with the Job.
	migrateSourceSecret = "seaweedfs-migrate-source"
	migrateContainer    = "rclone"
	migrateAppLabel     = "seaweedfs-migrate"
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
	// pipeline servers, and a server is moved only while a whole
	// MigrateRepointTimeout wait still fits (repoint). A copy still running
	// then is picked up by the next Migrate run. Tests shorten these.
	MigrateCopyTimeout = 8 * time.Minute
	MigratePoll        = 3 * time.Second
	// MigrateRepointTimeout bounds the wait for one pipeline server's
	// ObjectStoreAvailable condition after it was repointed.
	MigrateRepointTimeout = 2 * time.Minute
	// removeAfterCopyReserve is the part of the operation's deadline the
	// removal keeps for what follows its final copy: the deletions (a few
	// API calls each) and the PVC wait (S3DeleteTimeout, one minute).
	removeAfterCopyReserve = 2 * time.Minute
)

// migrateScript runs in the Job. The remotes "old" and "new" come from
// RCLONE_CONFIG_* variables (rclone docs, "Config file": environment
// variables), with the keys from secretKeyRef, so no credential is in the
// command, the arguments or the operation's output.
//   - copy never deletes (docs/content/commands/rclone_copy.md at v1.76.0:
//     "Doesn't delete files from the destination"). COPY_MODE (copyMode)
//     says whether it may overwrite: "overwrite" transfers every object
//     that is not identical (by size and modification time or MD5,
//     rclone_copy.md), so an object changed in the old storage since an
//     earlier copy is copied again; "ignore-existing" skips every object
//     that exists in the new storage "no matter the content"
//     (docs/content/docs.md, --ignore-existing), so it never replaces what
//     a moved pipeline server wrote there;
//   - check --one-way --download reads every object of the old storage and
//     its copy and compares the bytes, so it needs no hash support;
//     --combined lists each path with = (identical), + (missing in the new
//     storage), * (different) or ! (unreadable); --one-way reports nothing
//     for objects only in the new storage (fs/operations/check.go DstOnly);
//     the first three different paths go into differSample (base64, so any
//     byte of a key is safe in the JSON);
//   - size --json counts objects and bytes on both sides, for the summary
//     only: it lists again after the check (copyResult.problem).
//
// The result is one JSON line in /dev/termination-log, which the kubelet
// puts in the pod status (terminationMessagePath), so no pod-log access is
// needed; the kubelet truncates it at 4096 bytes (Kubernetes docs,
// "Determine the Reason for Pod Failure"), and it stays under 1.2 KB.
// Verified locally with this image against SeaweedFS 4.54 as a non-root
// UID without capabilities.
const migrateScript = `set -u
copy=0
case "$COPY_MODE" in
overwrite) rclone copy old: new: --create-empty-src-dirs --metadata || copy=$? ;;
ignore-existing) rclone copy old: new: --create-empty-src-dirs --metadata --ignore-existing || copy=$? ;;
*) echo "unknown COPY_MODE '$COPY_MODE'" >&2; exit 2 ;;
esac
check=0
rclone check old: new: --one-way --download --combined /tmp/combined.txt || check=$?
count() { n=$(grep -c "^$1 " /tmp/combined.txt 2>/dev/null); echo "${n:-0}"; }
sample=$(grep '^\* ' /tmp/combined.txt 2>/dev/null | head -n 3 | cut -c3-202 | base64 | tr -d '\n')
src=$(rclone size old: --json) || src=null
dst=$(rclone size new: --json) || dst=null
buckets=-1
if rclone lsf old: --dirs-only > /tmp/old-buckets.txt && rclone lsf new: --dirs-only > /tmp/new-buckets.txt; then
  buckets=$(grep -cvxF -f /tmp/new-buckets.txt /tmp/old-buckets.txt)
fi
printf '{"copy":%d,"check":%d,"identical":%s,"missing":%s,"differ":%s,"errors":%s,"missingBuckets":%s,"differSample":"%s","source":%s,"destination":%s}' \
  "$copy" "$check" "$(count =)" "$(count +)" "$(count '\*')" "$(count !)" "${buckets:-0}" "$sample" "${src:-null}" "${dst:-null}" > /dev/termination-log
[ "$copy" -eq 0 ] && [ "$check" -eq 0 ]
`

// copyMode is the copy Job's COPY_MODE (migrateScript): copyOverwrite only
// before Migrate storage has ever finished a verified copy and while no
// pipeline server uses the new storage (it then holds only copies, so a
// changed object is copied again); copyIgnoreExisting afterwards, and
// always for the removal's final copy: by then pipeline servers and the
// user's own clients may write newer data there, which a copy must never
// overwrite. An object that differs is then reported (copyResult.problem).
const (
	copyOverwrite      = "overwrite"
	copyIgnoreExisting = "ignore-existing"
)

// migrationCopiedAnnotation, on Secret seaweedfs/seaweedfs-s3-credentials,
// records when Migrate storage finished a verified copy (RFC 3339). The
// Secret is the new storage's own object the ServiceAccount may patch by
// name; its presence ends copyOverwrite and lets the status offer the
// removal (legacyStorageState).
const migrationCopiedAnnotation = "rhoai-nightly-updater/migration-copied"

// finalCopyAnnotation marks a copy Job the removal started (fresh): a later
// removal waits for it instead of starting over (copyAndVerify).
const finalCopyAnnotation = "rhoai-nightly-updater/final-copy"

// migrationCopied reports whether Migrate storage has finished a verified
// copy into the new storage.
func migrationCopied(c *Client) (bool, error) {
	meta, found, err := readObjectMeta(c, namespacedPath("v1", "secrets", s3Namespace, s3Secret))
	if err != nil || !found {
		return false, err
	}
	return meta.Annotations[migrationCopiedAnnotation] != "", nil
}

// recordMigrationCopied sets migrationCopiedAnnotation (a merge patch of
// one annotation: setup's server-side apply owns the rest of the Secret).
func recordMigrationCopied(c *Client) error {
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]string{migrationCopiedAnnotation: clk.Now().UTC().Format(time.RFC3339)}}})
	_, _, err := c.patch(namespacedPath("v1", "secrets", s3Namespace, s3Secret), patch)
	return err
}

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
	// ConfirmProjects are the projects whose pipeline server the user
	// confirmed as this tool's in the dialog: Migrate moves an unrecorded
	// pipeline server (pipeline_record.go) only in one of them.
	ConfirmProjects []string
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

// migrateJob is the copy Job. The "old" remote reads the old storage's keys
// from migrateSourceSecret, the "new" one the new storage's from its
// Secret; both are in the Job's namespace. The endpoints carry their
// scheme: rclone prepends https:// to one without (backend/s3/s3.go:1597
// at v1.76.0). Pods can reach both S3 ports: NetworkPolicy
// seaweedfs-ingress admits port 8333 from every pod in every namespace
// (namespaceSelector {}), and the old storage's policies do the same for
// its port.
func migrateJob(oldServer, mode string, final bool) map[string]any {
	secretEnv := func(name, secret, key string) map[string]any {
		return map[string]any{"name": name, "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": secret, "key": key}}}
	}
	remote := func(name, provider, endpoint, secret string) []map[string]any {
		prefix := "RCLONE_CONFIG_" + name + "_"
		return []map[string]any{
			{"name": prefix + "TYPE", "value": "s3"},
			{"name": prefix + "PROVIDER", "value": provider},
			{"name": prefix + "ENDPOINT", "value": endpoint},
			{"name": prefix + "REGION", "value": s3Region},
			secretEnv(prefix+"ACCESS_KEY_ID", secret, s3AccessKeyField),
			secretEnv(prefix+"SECRET_ACCESS_KEY", secret, s3SecretKeyField),
		}
	}
	env := append(remote("OLD", rcloneProvider(oldServer), "http://"+legacyS3Host(), migrateSourceSecret), remote("NEW", "SeaweedFS", s3Endpoint(), s3Secret)...)
	// The assigned UID has no home directory in the image.
	env = append(env, map[string]any{"name": "HOME", "value": "/tmp"}, map[string]any{"name": "COPY_MODE", "value": mode})
	podLabels := map[string]any{"app": migrateAppLabel, managedByLabelKey: managedByLabelValue}
	return map[string]any{
		"apiVersion": "batch/v1", "kind": "Job",
		"metadata": migrateJobMeta(final),
		"spec": map[string]any{
			"backoffLimit":            0,
			"activeDeadlineSeconds":   migrateJobDeadline,
			"ttlSecondsAfterFinished": migrateJobTTL,
			"template": map[string]any{
				"metadata": map[string]any{"labels": podLabels},
				"spec": map[string]any{
					"restartPolicy": "Never",
					// The copy needs no Kubernetes API access.
					"automountServiceAccountToken": false,
					"containers": []map[string]any{{
						"name":    migrateContainer,
						"image":   rcloneImage(),
						"command": []string{"/bin/sh", "-c", migrateScript},
						"env":     env,
						"resources": map[string]any{
							"requests": map[string]any{"cpu": "100m", "memory": "128Mi"},
							"limits":   map[string]any{"cpu": "500m", "memory": "512Mi"},
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
	Copy      int   `json:"copy"`
	Check     int   `json:"check"`
	Identical int64 `json:"identical"`
	Missing   int64 `json:"missing"`
	Differ    int64 `json:"differ"`
	Errors    int64 `json:"errors"`
	// MissingBuckets counts the old storage's buckets that are not in the
	// new one (an empty bucket has no object for the check to miss); -1
	// when a bucket list could not be read.
	MissingBuckets int64     `json:"missingBuckets"`
	Source         *copySize `json:"source"`
	Destination    *copySize `json:"destination"`
	// DifferSample is the first three paths the check found different,
	// one per line, base64 encoded (migrateScript).
	DifferSample string `json:"differSample"`
}

// differPaths decodes DifferSample.
func (r copyResult) differPaths() []string {
	raw, err := base64.StdEncoding.DecodeString(r.DifferSample)
	if err != nil {
		return nil
	}
	var paths []string
	for p := range strings.SplitSeq(strings.ToValidUTF8(string(raw), "?"), "\n") {
		if p = strings.TrimSpace(p); p != "" {
			paths = append(paths, p)
		}
	}
	return paths
}

// copyProblem is why a copy is not verified: what the check found, and
// whether running again can fix it (retry) or what to do instead (fix).
type copyProblem struct {
	what  string
	retry bool
	fix   string
}

// problem explains why the copy is not verified, or returns nil. Every
// object of the old storage must be in the new one, byte for byte: the
// one-way check proves that. The new storage may hold more (objects written
// there directly, or deleted from the old storage after an earlier copy);
// they are reported, not counted against the copy (extraObjects). mode is
// the Job's copyMode: with copyIgnoreExisting an object that differs is
// never copied again, so no retry can fix it.
func (r copyResult) problem(mode string) *copyProblem {
	n := func(v int64) string { return countNoun(int(v), "object", "objects") }
	switch {
	case r.Copy != 0:
		return &copyProblem{what: fmt.Sprintf("rclone copy failed (exit code %d)", r.Copy), retry: true}
	case r.MissingBuckets < 0:
		return &copyProblem{what: "the bucket lists of the two storages could not be read", retry: true}
	case r.MissingBuckets > 0:
		return &copyProblem{what: fmt.Sprintf("%s of the old storage %s missing in the new storage", countNoun(int(r.MissingBuckets), "bucket", "buckets"), verb(int(r.MissingBuckets), "is", "are")), retry: true}
	case r.Differ > 0 && mode != copyOverwrite:
		what := fmt.Sprintf("the check found %s different in the new storage", n(r.Differ))
		if paths := r.differPaths(); len(paths) > 0 {
			what += fmt.Sprintf(" (for example %s)", strings.Join(paths, ", "))
		}
		if r.Missing > 0 || r.Errors > 0 {
			what += fmt.Sprintf(", %s missing and %s unreadable", n(r.Missing), n(r.Errors))
		}
		return &copyProblem{what: what, fix: "They changed in the old storage after an earlier copy, or were written to the new storage since. The copy never overwrites objects in the new storage once the migration has finished or pipeline servers write there, so a retry gives the same result. For each one, copy the old storage's version into the new storage yourself if that is the one to keep, or delete it from the old storage if the new storage's version is the one to keep; then run it again."}
	case r.Missing > 0 || r.Differ > 0 || r.Errors > 0:
		return &copyProblem{what: fmt.Sprintf("the check found %s missing in the new storage, %s different and %s unreadable (written or changed in the old storage during the copy, or not readable then)", n(r.Missing), n(r.Differ), n(r.Errors)), retry: true}
	case r.Check != 0:
		return &copyProblem{what: fmt.Sprintf("rclone check failed (exit code %d)", r.Check), retry: true}
	}
	// check exited 0 with no +, * or ! line: every object it listed in the
	// old storage is in the new one, byte for byte. The sizes come from
	// separate listings taken after it, so objects written to the old
	// storage in between would make them disagree with the check; they are
	// used only for the summary (summary, extraObjects).
	return nil
}

// checked is the number of objects of the old storage the check found
// identical: all lines of its one-way --combined report once the check
// passed.
func (r copyResult) checked() int64 { return r.Identical + r.Missing + r.Differ + r.Errors }

// summary describes what was copied: the old storage's objects and bytes
// (size --json), or the checked count when the size could not be read.
func (r copyResult) summary() string {
	if r.Source != nil {
		return sizeText(*r.Source)
	}
	return countNoun(int(r.checked()), "object", "objects")
}

// extraObjects returns how many more objects the new storage holds than
// the old one, or 0 (counted by size --json, information only).
func (r copyResult) extraObjects() int64 {
	if r.Source == nil || r.Destination == nil || r.Destination.Count <= r.Source.Count {
		return 0
	}
	return r.Destination.Count - r.Source.Count
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
	// op is the action that runs this again: "Migrate storage" or "Remove
	// old storage".
	op string
	// kept says what a stop before the end left unchanged.
	kept string
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
	recordActivity(m.c, "migrate-storage", fmt.Sprintf("%s (%s)", legacyNamespace, activity), false)
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
	m := &migrateRun{c: c, emit: onStep, op: "Migrate storage", kept: "no pipeline server was moved and the old storage is kept"}
	if opts.RemoveOldStorage {
		m.op, m.kept = "Remove old storage", "nothing was deleted"
	}
	m.step("check", "running", fmt.Sprintf("Checking the storage in namespace %s...", legacyNamespace))
	legacy, err := readLegacyStorage(c)
	if err != nil {
		return m.fail("check", fmt.Sprintf("Cannot read the storage in namespace %s; nothing was changed: %v.%s", legacyNamespace, err, templateHint(err)), errorCodeFromK8sErr(err), "read failed")
	}
	if legacy == nil {
		m.step("check", "success", fmt.Sprintf("No storage from an earlier version in namespace %s.", legacyNamespace))
		recordActivity(c, "migrate-storage", legacyNamespace+" (nothing to do)", true)
		return &types.OperationResponse{Success: true, Message: fmt.Sprintf("Nothing to do: there is no storage from an earlier version in namespace %s.", legacyNamespace), Logs: m.logs}, nil
	}
	if legacy.terminating {
		return m.fail("check", fmt.Sprintf("Namespace %s is being deleted, so its storage cannot be migrated. Nothing was changed.", legacyNamespace), "terminating", "namespace terminating")
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
	return m.migrate(legacy, dspas, opts.ConfirmProjects)
}

// sourceReady checks that the old storage's server can be copied from and
// reads its keys.
func (m *migrateRun) sourceReady(legacy *legacyStorage, step string) (s3Keys, *types.OperationResponse) {
	switch {
	case legacy.server == "":
		resp, _ := m.fail(step, fmt.Sprintf("No server runs in namespace %s any more, so its objects cannot be copied. Nothing was changed, and a retry gives the same result while no server runs there.", legacyNamespace), "prerequisites", "no server")
		return s3Keys{}, resp
	case !legacy.ready:
		resp, _ := m.fail(step, fmt.Sprintf("%s in namespace %s is not running, so its objects cannot be copied. Nothing was changed. Start it again (Deployment %s), then retry.", legacy.server, legacyNamespace, map[string]string{"SeaweedFS": s3Deployment, "MinIO": legacyMinIODeployment}[legacy.server]), "prerequisites", "server not ready")
		return s3Keys{}, resp
	}
	keys, err := readLegacyKeys(m.c)
	if err != nil {
		resp, _ := m.fail(step, fmt.Sprintf("Cannot read the old storage's keys; nothing was changed: %v.%s", err, templateHint(err)), firstNonEmpty(errorCodeFromK8sErr(err), "prerequisites"), "keys unreadable")
		return s3Keys{}, resp
	}
	return keys, nil
}

// newStorageReady reports whether the new storage is set up by this tool
// and ready. Its keys are its own (step 1 of Migrate): they are never
// compared with the old storage's. Neither flow points at Tear down here:
// it deletes both storages.
func (m *migrateRun) newStorageReady(step string) (bool, *types.OperationResponse) {
	_, found, err := readS3Keys(m.c)
	if err != nil {
		resp, _ := m.fail(step, fmt.Sprintf("Cannot read the new storage's keys: %v.%s", err, templateHint(err)), errorCodeFromK8sErr(err), "new keys unreadable")
		return false, resp
	}
	state := getS3Status(m.c)
	return found && state.Ready && state.ManagedByTool && state.RepairNeeded == "", nil
}

// migrate runs steps 1 to 3.
func (m *migrateRun) migrate(legacy *legacyStorage, dspas []dspaInfo, confirmed []string) (*types.OperationResponse, error) {
	oldKeys, resp := m.sourceReady(legacy, "check")
	if resp != nil {
		return resp, nil
	}
	// Which pipeline servers get the new storage's keys: recorded ones, and
	// unrecorded ones only in a project the user confirmed. Read before
	// anything is written; a record that cannot be read stops the run.
	rec, err := readPipelineRecord(m.c)
	if err != nil {
		return m.fail("check", fmt.Sprintf("Cannot read this tool's record of its pipeline servers, so which ones are this tool's is unknown; nothing was changed: %v.%s", err, templateHint(err)), errorCodeFromK8sErr(err), "pipeline record unreadable")
	}
	owned := func(d dspaInfo) bool {
		return d.managedByTool() && (rec.owns(d.Meta) || slices.Contains(confirmed, d.Meta.Namespace))
	}
	m.step("check", "success", fmt.Sprintf("OK: %s in namespace %s is running", legacy.server, legacyNamespace))

	// Step 1: the new storage, with keys of its own: the ones it has when
	// it is set up already, else new random ones (s3Credentials).
	m.step("storage", "running", fmt.Sprintf("Setting up the storage in namespace %s with new keys...", s3Namespace))
	ready, resp := m.newStorageReady("storage")
	if resp != nil {
		return resp, nil
	}
	if ready {
		m.step("storage", "success", "OK: The new storage is already set up")
	} else {
		keys, err := s3Credentials(m.c)
		if err != nil {
			return m.fail("storage", fmt.Sprintf("Cannot make the new storage's keys; nothing was changed: %v.%s", err, templateHint(err)), errorCodeFromK8sErr(err), "keys")
		}
		log := &opLog{observe: func(line string) {
			if m.emit != nil {
				m.emit(UpdateStepEvent{Step: "storage", Status: "running", Message: line})
			}
		}}
		res, err := setupS3Storage(m.c, keys, log, s3RetryMigrate)
		m.logs = append(m.logs, log.lines...)
		if err != nil {
			return nil, err
		}
		if !res.Success {
			// res.Message names Migrate storage as the way to continue (s3RetryMigrate).
			return m.fail("storage", "Setting up the new storage did not finish: "+res.Message+" The old storage is unchanged.", res.ErrorCode, "setup failed")
		}
		m.step("storage", "success", "OK: The new storage is ready")
	}

	// Step 2: copy and verify. From now on the new storage may get data
	// of its own, so no later copy overwrites there (copyMode).
	result, resp := m.copyAndVerify(legacy, dspas, oldKeys, "copy", false)
	if resp != nil {
		return resp, nil
	}
	if err := recordMigrationCopied(m.c); err != nil {
		return m.fail("copy", fmt.Sprintf("The copy is verified, but it could not be recorded on Secret %s/%s: %v. No pipeline server was moved; run Migrate storage again.%s", s3Namespace, s3Secret, err, templateHint(err)), errorCodeFromK8sErr(err), "record failed")
	}

	// Step 3: the pipeline servers, with the new storage's keys.
	newKeys, found, err := readS3Keys(m.c)
	if err != nil || !found {
		if err == nil {
			err = fmt.Errorf("secret %s/%s is gone", s3Namespace, s3Secret)
		}
		return m.fail("repoint", fmt.Sprintf("Cannot read the new storage's keys; no pipeline server was moved: %v.%s", err, templateHint(err)), errorCodeFromK8sErr(err), "new keys unreadable")
	}
	repointed, listed, resp := m.repoint(legacy, dspas, newKeys, owned)
	if resp != nil {
		return resp, nil
	}

	msg := fmt.Sprintf("Migrated %s to namespace %s.", result.summary(), s3Namespace)
	if len(repointed) > 0 {
		msg += fmt.Sprintf(" %s now %s it: %s.", countNoun(len(repointed), "pipeline server", "pipeline servers"), verb(len(repointed), "uses", "use"), strings.Join(repointed, ", "))
	}
	if len(listed) > 0 {
		msg += fmt.Sprintf(" Not changed by this tool, still on the old storage: %s.", strings.Join(listed, "; "))
	}
	msg += fmt.Sprintf(" The old storage in namespace %s is kept: delete it later from the S3 storage's menu, once nothing else uses it.", legacyNamespace)
	slog.Info("s3 storage migrated", "user", getUser(m.c), "objects", result.checked())
	recordActivity(m.c, "migrate-storage", fmt.Sprintf("%s -> %s (%s, %d pipeline servers)", legacyNamespace, s3Namespace, result.summary(), len(repointed)), true)
	return &types.OperationResponse{Success: true, Message: msg, Logs: m.logs}, nil
}

// copyAndVerify runs the copy Job (or waits for one a previous run left
// running with the same copyMode) and verifies its result. The Job is
// deleted by UID once its result is read.
//
// fresh (the removal's final copy) always starts a new Job: one an earlier
// run started began copying before objects written since, so its result
// says nothing about them. Objects written after the fresh copy began are
// still not covered; nothing in this tool's grants stops writes to the old
// storage before its Service is deleted (see the removal checklist). A
// fresh copy is not resumed by the next run, so it may use the operation's
// remaining time, less removeAfterCopyReserve, when that is longer than
// MigrateCopyTimeout.
func (m *migrateRun) copyAndVerify(legacy *legacyStorage, dspas []dspaInfo, oldKeys s3Keys, step string, fresh bool) (copyResult, *types.OperationResponse) {
	c := m.c
	fail := func(msg, code, activity string) (copyResult, *types.OperationResponse) {
		resp, _ := m.fail(step, msg, code, activity)
		return copyResult{}, resp
	}
	m.step(step, "running", fmt.Sprintf("Copying every bucket and object from %s to %s (Job %s)...", legacyNamespace, s3Namespace, migrateJobName))

	// Whether a pipeline server uses the new storage yet decides whether
	// the copy may overwrite objects there (copyMode). Data connections and
	// workbenches cannot be seen; the removal's checklist asks the user to
	// move them only after the migration.
	endpoints, err := readS3Endpoints(c)
	if err != nil {
		return fail(fmt.Sprintf("Cannot read the new storage's Service; the copy did not start: %v.%s", err, templateHint(err)), errorCodeFromK8sErr(err), "endpoints unreadable")
	}
	copied, err := migrationCopied(c)
	if err != nil {
		return fail(fmt.Sprintf("Cannot read Secret %s/%s; the copy did not start: %v.%s", s3Namespace, s3Secret, err, templateHint(err)), errorCodeFromK8sErr(err), "secret read failed")
	}
	mode := copyOverwrite
	if fresh || copied {
		mode = copyIgnoreExisting
	}
	for _, d := range dspas {
		if endpoints.address.usedBy(d.Meta.Namespace, d.Host) {
			mode = copyIgnoreExisting
		}
	}

	job, found, err := readMigrateJob(c)
	if err != nil {
		return fail(fmt.Sprintf("Cannot read Job %s/%s; the copy did not start: %v.%s", s3Namespace, migrateJobName, err, templateHint(err)), errorCodeFromK8sErr(err), "job read failed")
	}
	if found && !job.Meta.hasToolLabel() {
		return fail(fmt.Sprintf("Job %s/%s exists and was not created by this tool, so it was left unchanged and the copy did not start. Delete it, then retry.", s3Namespace, migrateJobName), "not_managed", "foreign job")
	}
	running := found && !job.Meta.terminating() && !job.finished()
	switch {
	case running && job.Mode == mode && !fresh && !job.FinalCopy:
		m.step(step, "running", "A copy started by an earlier run is still running: waiting for it")
	case running && job.Mode == mode && fresh && job.FinalCopy:
		// Started by an earlier removal, after the user confirmed it: its
		// result proves this removal's copy too.
		m.step(step, "running", "The final copy an earlier removal started is still running: waiting for it")
	default:
		if found {
			// A finished Job whose result no run read (the request ended
			// first): its copy may be out of date, so run a new one. A
			// running one with another copyMode is stopped first: one that
			// overwrites must not run once pipeline servers write to the new
			// storage. Foreground deletion keeps the Job until its pod is
			// gone (the Job controller sets blockOwnerDeletion on its pods:
			// job_controller.go:1803 and controller_ref.go:59-67 at
			// v1.35.0), so the two copies never overlap.
			policy := "Background"
			switch {
			case running && fresh:
				policy = "Foreground"
				m.step(step, "running", "A copy an earlier run started is still running: stopping it, so the final copy starts now")
			case running:
				policy = "Foreground"
				m.step(step, "running", "A copy an earlier run started with other settings is still running: stopping it")
			}
			if !job.Meta.terminating() {
				if _, err := deleteWithUIDPropagation(c, migrateJobPath(), job.Meta.UID, policy); err != nil && !IsK8sError(err, 404) {
					return fail(fmt.Sprintf("Cannot delete the Job %s/%s of an earlier run: %v.%s", s3Namespace, migrateJobName, err, templateHint(err)), errorCodeFromK8sErr(err), "old job delete failed")
				}
			}
			gone, err := waitForDeletion(c, migrateJobPath(), S3DeleteTimeout, S3DeletePoll)
			if err != nil || !gone {
				return fail(fmt.Sprintf("Job %s/%s of an earlier run is still being deleted; retry in a minute.", s3Namespace, migrateJobName), "in_progress", "old job lingering")
			}
		}
		obj := migrateJob(legacy.server, mode, fresh)
		data, _ := json.Marshal(obj)
		if _, _, err := c.do(http.MethodPost, migrateJobCreatePath(), "application/json", data, url.Values{"dryRun": {"All"}}); err != nil {
			return fail(fmt.Sprintf("The API server rejects the copy Job: %v. Nothing was changed.%s", err, templateHint(err)), firstNonEmpty(errorCodeFromK8sErr(err), "validation"), "job dry run failed")
		}
		// The old keys first, so the Job's pod finds them when it starts.
		if err := writeMigrateSource(c, oldKeys); err != nil {
			return fail(fmt.Sprintf("Cannot write the copy Job's Secret %s/%s with the old storage's keys; the copy did not start: %v.%s", s3Namespace, migrateSourceSecret, err, templateHint(err)), errorCodeFromK8sErr(err), "source secret failed")
		}
		body, _, err := c.post(migrateJobCreatePath(), data)
		if err != nil {
			return fail(fmt.Sprintf("Cannot create the copy Job: %v.%s", err, templateHint(err)), errorCodeFromK8sErr(err), "job create failed")
		}
		if job, err = parseMigrateJob(body); err != nil {
			return fail(fmt.Sprintf("Cannot read the copy Job: %v", err), "", "job parse failed")
		}
		// The Secret goes with the Job (garbage collection), whichever way
		// the Job ends: deleted here, by a later run, or by its TTL.
		if err := ownMigrateSource(c, job.Meta.UID); err != nil {
			m.deleteMigrateJob(job)
			return fail(fmt.Sprintf("Cannot give the copy Job's Secret %s/%s to the Job; the copy was stopped: %v.%s", s3Namespace, migrateSourceSecret, err, templateHint(err)), errorCodeFromK8sErr(err), "source secret owner failed")
		}
	}

	timeout := MigrateCopyTimeout
	if deadline, ok := c.ctx.Deadline(); ok && fresh && clk.Until(deadline)-removeAfterCopyReserve > timeout {
		timeout = clk.Until(deadline) - removeAfterCopyReserve
	}
	job, issue, err := waitMigrateJob(c, job, timeout)
	switch {
	case errors.Is(err, errS3CannotStart):
		m.deleteMigrateJob(job)
		return fail(fmt.Sprintf("The copy Job's pod cannot start (%s: %s). Nothing else was changed. Fix the cause (for example set RCLONE_IMAGE to a pullable rclone image), then run %s again.", issue.Reason, truncateText(issue.Message, 300), m.op), "not_ready", "job cannot start")
	case errors.Is(err, errS3NotReady) && fresh:
		return fail(fmt.Sprintf("The final copy and check are still running after %s, so %s. The copy keeps running: run %s again to keep waiting for it; the old storage is deleted only once it has passed.", timeout.Round(time.Second), m.kept, m.op), "in_progress", "final copy still running")
	case errors.Is(err, errS3NotReady):
		return fail(fmt.Sprintf("The copy is still running after %s. Nothing else was changed. Run %s again to keep waiting for it.", timeout, m.op), "in_progress", "copy still running")
	case err != nil:
		return fail(fmt.Sprintf("Stopped waiting for the copy Job: %v.%s", err, templateHint(err)), errorCodeFromK8sErr(err), "job wait failed")
	}
	result, readErr := readMigrateResult(c, job)
	m.deleteMigrateJob(job)
	if readErr != nil {
		return fail(fmt.Sprintf("The copy Job ended without a readable result (%v). Nothing else was changed; run %s again.", readErr, m.op), "", "no result")
	}
	if p := result.problem(job.Mode); p != nil {
		msg := fmt.Sprintf("The copy is not complete: %s. Nothing else was changed: %s.", p.what, m.kept)
		if p.retry {
			msg += fmt.Sprintf(" Run %s again to retry.", m.op)
		} else {
			msg += " " + p.fix
		}
		return fail(msg, "copy_mismatch", "copy mismatch: "+p.what)
	}
	if extra := result.extraObjects(); extra > 0 {
		m.step(step, "running", fmt.Sprintf("Note: the new storage holds %s more than the old one (written there directly, or deleted from the old storage after an earlier copy); they are kept", countNoun(int(extra), "object", "objects")))
	}
	m.step(step, "success", fmt.Sprintf("OK: %s copied and checked byte for byte", countNoun(int(result.checked()), "object", "objects")))
	return result, nil
}

// migrateSourcePath is the copy Job's Secret with the old storage's keys.
func migrateSourcePath() string {
	return namespacedPath("v1", "secrets", "seaweedfs", "seaweedfs-migrate-source")
}

// writeMigrateSource writes the copy Job's Secret with the old storage's
// keys: one this tool wrote earlier (an earlier run's, still owned by that
// Job, which is gone or being replaced) is deleted by UID first, and a new
// one is created. One someone else made is left alone and stops the copy.
func writeMigrateSource(c *Client, keys s3Keys) error {
	meta, found, err := readObjectMeta(c, migrateSourcePath())
	if err != nil {
		return err
	}
	if found {
		if !meta.hasToolLabel() {
			return fmt.Errorf("it exists and was not created by this tool")
		}
		if _, err := deleteWithUID(c, migrateSourcePath(), meta.UID); err != nil && !IsK8sError(err, 404) {
			return fmt.Errorf("delete an earlier run's copy: %w", err)
		}
	}
	data, _ := json.Marshal(map[string]any{
		"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
		"metadata":   map[string]any{"name": migrateSourceSecret, "namespace": s3Namespace, "labels": toolLabels()},
		"stringData": map[string]any{s3AccessKeyField: keys.AccessKey, s3SecretKeyField: keys.SecretKey},
	})
	_, _, err = c.post(withToolFieldManager(namespacedPath("v1", "secrets", s3Namespace, "")), data)
	return err
}

// ownMigrateSource makes the copy Job (jobUID) the owner of its Secret, so
// garbage collection deletes the Secret with the Job. OpenShift's
// OwnerReferencesPermissionEnforcement admission asks for delete on the
// Secret to set it (the rule names it); blockOwnerDeletion is not set, so
// nothing is asked of the Job.
func ownMigrateSource(c *Client, jobUID string) error {
	meta, found, err := readObjectMeta(c, migrateSourcePath())
	if err != nil {
		return err
	}
	if !found || !meta.hasToolLabel() {
		return fmt.Errorf("secret %s/%s is gone or not this tool's", s3Namespace, migrateSourceSecret)
	}
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{
		"resourceVersion": meta.ResourceVersion,
		"ownerReferences": []any{map[string]any{"apiVersion": "batch/v1", "kind": "Job", "name": migrateJobName, "uid": jobUID}},
	}})
	_, _, err = c.patch(migrateSourcePath(), patch)
	return err
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
	// Mode is the copyMode the Job was created with (its COPY_MODE), "" for
	// a Job without one.
	Mode string
	// FinalCopy: a removal started it (finalCopyAnnotation).
	FinalCopy bool
}

// migrateJobMeta is the copy Job's metadata; a removal's final copy carries
// finalCopyAnnotation.
func migrateJobMeta(final bool) map[string]any {
	meta := map[string]any{"name": migrateJobName, "namespace": s3Namespace, "labels": toolLabels()}
	if final {
		meta["annotations"] = map[string]any{finalCopyAnnotation: "true"}
	}
	return meta
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
			Template struct {
				Spec struct {
					Containers []struct {
						Name string `json:"name"`
						Env  []struct {
							Name  string `json:"name"`
							Value string `json:"value"`
						} `json:"env"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
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
	j := migrateJobInfo{Meta: obj.Metadata, Selector: obj.Spec.Selector.MatchLabels, FinalCopy: obj.Metadata.Annotations[finalCopyAnnotation] == "true"}
	for _, ctr := range obj.Spec.Template.Spec.Containers {
		for _, e := range ctr.Env {
			if ctr.Name == migrateContainer && e.Name == "COPY_MODE" {
				j.Mode = e.Value
			}
		}
	}
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

// waitMigrateJob waits, bounded by timeout, until the Job has
// finished. It returns errS3CannotStart as soon as its pod reports a
// terminal waiting reason (ImagePullBackOff, ...), and errS3NotReady on
// timeout.
func waitMigrateJob(c *Client, job migrateJobInfo, timeout time.Duration) (migrateJobInfo, workloadPodIssue, error) {
	ctx, cancel := clk.WithTimeout(c.ctx, timeout)
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
		if clk.Sleep(ctx, MigratePoll) != nil {
			if c.ctx.Err() != nil {
				return job, workloadPodIssue{}, c.ctx.Err()
			}
			return job, workloadPodIssue{}, errS3NotReady
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

// unrecordedReason is why Migrate storage leaves a pipeline server that
// looks like this tool's but has no record (pipeline_record.go).
const unrecordedReason = "not recorded as this tool's: confirm its project in Migrate storage to move it"

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
func (m *migrateRun) repoint(legacy *legacyStorage, dspas []dspaInfo, keys s3Keys, owned func(dspaInfo) bool) (moved, listed []string, resp *types.OperationResponse) {
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
			if why, ok := repointable(d); ok && owned(d) {
				todo = append(todo, d)
			} else if ok {
				// Its label (or field managers) say this tool's, which a
				// project editor can write: without the record or the user's
				// confirmation, the storage's keys stay out of that project.
				listed = append(listed, name+" ("+unrecordedReason+")")
			} else {
				listed = append(listed, name+" ("+why+")")
			}
		case owned(d) && endpoints.address.usedBy(d.Meta.Namespace, d.Host) && !d.Meta.terminating() && !d.objectStoreReady():
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
		d       dspaInfo
		secrets []*projectSecret
		patch   []byte
	}
	plans := make([]*plan, 0, len(todo))
	for _, d := range todo {
		project := d.Meta.Namespace
		// The same Secrets setup writes (the S3 secret, and the dashboard
		// Connection with the new keys), owned by the DSPA they are for (it
		// exists here), so they go with the DSPA whoever deletes it.
		secrets := []*projectSecret{
			{c: c, project: project, name: dspaSecretName, what: "secret", obj: dspaS3Secret(project, keys)},
			{c: c, project: project, name: connectionSecretName, what: "connection", obj: dashboardS3Connection(project, keys)},
		}
		for _, s := range secrets {
			setDSPAOwner(s.obj, d.Meta.Name, d.Meta.UID)
			if err := readProjectSecret(s, projectSecretPath(project, s.name)); err != nil {
				return failed(fmt.Sprintf("Cannot read Secret %s/%s; no pipeline server was moved: %v.%s", project, s.name, err, templateHint(err)), errorCodeFromK8sErr(err), s.what+" read failed")
			}
			if s.found && !s.meta.hasToolLabel() {
				return failed(fmt.Sprintf("Secret %s/%s exists and was not created by this tool, so pipeline server %s/%s was not moved and nothing was changed.", project, s.name, project, d.Meta.Name), "not_managed", "foreign "+s.what)
			}
		}
		patch, _ := json.Marshal(map[string]any{
			"metadata": map[string]any{"resourceVersion": d.Meta.ResourceVersion, "labels": toolLabels(),
				"annotations": map[string]any{dspaConnectionAnnotation: connectionSecretName}},
			"spec": map[string]any{"objectStorage": map[string]any{"externalStorage": map[string]any{
				"host": s3Host(),
				"s3CredentialsSecret": map[string]any{
					"secretName": dspaSecretName,
					"accessKey":  s3AccessKeyField,
					"secretKey":  s3SecretKeyField,
				},
			}}},
		})
		plans = append(plans, &plan{d: d, patch: patch, secrets: secrets})
	}
	// writeSecrets writes the plan's Secrets; on failure it returns the
	// message and code for the one that failed.
	writeSecrets := func(p *plan, dryRun bool) (string, string, error) {
		for _, s := range p.secrets {
			if _, err := writeProjectSecret(s, projectSecretPath(p.d.Meta.Namespace, s.name), withToolFieldManager(namespacedPath("v1", "secrets", p.d.Meta.Namespace, "")), dryRun); err != nil {
				msg, code := pipelineWriteFailure(fmt.Sprintf("secret %s", s.name), p.d.Meta.Namespace, err)
				return msg, code, err
			}
		}
		return "", "", nil
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
		if msg, code, err := writeSecrets(p, true); err != nil {
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
		// The operation's deadline (15 minutes, pkg/api/auth_handlers.go) can
		// come before the last wait ends: setup, a copy wait of
		// MigrateCopyTimeout and a MigrateRepointTimeout wait per server.
		// A server is moved only when its whole wait fits, so a run never
		// ends with one moved but not yet checked.
		if deadline, ok := c.ctx.Deadline(); ok && clk.Until(deadline) < MigrateRepointTimeout {
			return failed(fmt.Sprintf("Not enough time is left in this run to move pipeline server %s and wait until it reaches the new storage (up to %s). Already moved: %s. The old storage is kept; run Migrate storage again to continue.", name, MigrateRepointTimeout, done), "in_progress", "out of time")
		}
		m.step("repoint", "running", fmt.Sprintf("Moving pipeline server %s...", name))
		if msg, code, err := writeSecrets(p, false); err != nil {
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
		d, err := waitObjectStore(c, p.d.Meta.Namespace, p.d.Meta.Name, repointedGeneration(body, p.d.Meta.Generation))
		if err != nil {
			return failed(fmt.Sprintf("Pipeline server %s was moved to the new storage, but %s. The old storage is kept. Check the pipeline server's conditions, then run Migrate storage again.", name, err.Error()), "not_ready", "moved server not ready")
		}
		moved = append(moved, name)
		if err := recordPipelineServer(c, d.Meta); err != nil {
			m.step("repoint", "running", fmt.Sprintf("Warning: %s could not be recorded as this tool's (%v); the next Migrate storage asks you to confirm its project again", name, err))
		}
		note := ""
		if d.ObjectStore.Message != "" {
			note = " (" + d.ObjectStore.Message + ")"
		}
		m.step("repoint", "running", fmt.Sprintf("OK: %s reaches the new storage%s", name, note))
	}
	m.step("repoint", "success", fmt.Sprintf("OK: %s moved", countNoun(len(moved), "pipeline server", "pipeline servers")))
	return moved, listed, nil
}

// repointedGeneration is the generation DSPO must have observed for a
// repointed DSPA's ObjectStoreAvailable to count: the one in the patch
// response, or before+1 when that cannot be read. The patch carries the
// resourceVersion that was read (a precondition) and changes spec, so it is
// the first change after generation before and the API server sets
// before+1; a condition from before the patch (generation before or
// lower) is never accepted, even when the response cannot be parsed.
func repointedGeneration(patchResponse []byte, before int64) int64 {
	if meta, err := parseObjectMeta(patchResponse); err == nil && meta.Generation > before {
		return meta.Generation
	}
	return before + 1
}

func objectStoreText(d dspaInfo) string {
	switch {
	case d.ObjectStore == nil:
		return "the data-science-pipelines operator reports no ObjectStoreAvailable condition yet"
	case d.ObjectStore.ObservedGeneration < d.Meta.Generation:
		return "the data-science-pipelines operator has not checked the change yet"
	}
	return fmt.Sprintf("ObjectStoreAvailable is %s: %s", d.ObjectStore.Status, firstNonEmpty(truncateText(d.ObjectStore.Message, 300), d.ObjectStore.Reason))
}

// waitObjectStore waits, bounded by MigrateRepointTimeout, until DSPO
// reports ObjectStoreAvailable=True for generation (or a later one). A False
// condition does not end the wait: DSPO requeues and checks again, for
// example while the new Service's address propagates.
func waitObjectStore(c *Client, project, name string, generation int64) (dspaInfo, error) {
	ctx, cancel := clk.WithTimeout(c.ctx, MigrateRepointTimeout)
	defer cancel()
	cc := c.WithContext(ctx)
	last := dspaInfo{Meta: objectMeta{Generation: generation}}
	for {
		list, err := listDSPAs(cc, project)
		if err != nil && ctx.Err() == nil && !IsNetworkError(err) {
			return last, fmt.Errorf("its status cannot be read: %w", err)
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
		if clk.Sleep(ctx, MigratePoll) != nil {
			if c.ctx.Err() != nil {
				return last, c.ctx.Err()
			}
			return last, fmt.Errorf("after %s %s", MigrateRepointTimeout, objectStoreText(last))
		}
	}
}

// toolNamespaceDeletable reports whether a removal may offer to delete a
// storage namespace (minio when the old storage is removed, seaweedfs when
// the storage is torn down): it carries the tool's label and one of the
// tool's managers wrote it (this or an earlier version created it, or
// labelled one it had created). Who created a namespace cannot be proven
// from its metadata (an entry's time is the manager's last write), so the
// deletion is the user's informed choice: the dialog says it deletes
// everything in the namespace, lists what the tool can see there, and
// starts unticked. reason says why it is not offered.
func toolNamespaceDeletable(name string, meta objectMeta) (bool, string) {
	switch {
	case !meta.hasToolLabel():
		return false, fmt.Sprintf("Namespace %s is kept: it does not carry this tool's label (%s=%s).", name, managedByLabelKey, managedByLabelValue)
	case !meta.hasManager(toolManagers):
		return false, fmt.Sprintf("Namespace %s is kept: it carries this tool's label, but its managedFields show no write by this tool.", name)
	}
	return true, ""
}

// legacyNamespaceDeletable is toolNamespaceDeletable for namespace minio.
func legacyNamespaceDeletable(meta objectMeta) (bool, string) {
	return toolNamespaceDeletable(legacyNamespace, meta)
}

// readLegacyNamespace reads namespace minio's metadata.
func readLegacyNamespace(c *Client) (objectMeta, bool, error) {
	return readObjectMeta(c, "/api/v1/namespaces/"+legacyNamespace)
}

// legacyRemovalPhases is the order Remove old storage deletes the objects
// earlier versions created in namespace minio (the same set as
// legacyStorageObjects). The server's Deployments go only after everything
// that can fail on its own, so a run that stops before them leaves a server
// the next run copies from again; one that stops after them is finished by
// the next run without a copy (removeOldStorage).
//
//  1. Before the server: the Routes (clients outside the cluster no longer
//     reach it), the data PVCs, its keys and last its Service (clients in
//     the cluster no longer reach it by name). Deleting a PVC that a pod
//     uses only marks it: the kubernetes.io/pvc-protection finalizer keeps
//     it until no pod object uses it, and the kubelet starts no new pod with
//     a PVC that is being deleted
//     (pkg/controller/volume/pvcprotection/pvc_protection_controller.go:382-394
//     at v1.35.0; Kubernetes docs, Persistent Volumes, "Storage Object in
//     Use Protection"). So the server keeps serving the data until its
//     Deployment goes, and a PVC that is being deleted records that the
//     deletion began. The PVCs come before the keys and the Service, which
//     the copy needs: a run that stops before the PVCs leaves everything
//     the next copy needs, one that stops after them leaves the PVCs marked.
//     The pipeline servers' old credentials Secrets are deleted first in
//     this phase (deleteLegacyDSPASecrets).
//  2. The server: its Deployments.
//  3. After the server: once the PVCs are gone, the pods that used them are
//     gone too (see 1), and the NetworkPolicies follow. Deleting them while
//     the server runs would open every port of it to every pod: a pod no
//     policy selects accepts all ingress (Kubernetes docs, Network
//     Policies, "The two sorts of pod isolation"). A storage without data
//     PVCs has nothing to wait for, so its policies may go while its pods
//     are still stopping.
var legacyRemovalPhases = struct{ beforeServer, server, afterServer []legacyObjectName }{
	beforeServer: []legacyObjectName{
		{"Route", legacyUIRoute},
		{"Route", legacyAPIRoute},
		{"PersistentVolumeClaim", legacySeaweedFSPVC},
		{"PersistentVolumeClaim", legacyMinIOPVC},
		{"Secret", legacySecret},
		{"Service", legacyService},
	},
	server: []legacyObjectName{
		{"Deployment", s3Deployment},
		{"Deployment", legacyMinIODeployment},
	},
	afterServer: []legacyObjectName{
		{"NetworkPolicy", s3NetworkPolicy},
		{"NetworkPolicy", legacyMinIONetworkPolicy},
	},
}

// legacyObjectName names one object of the earlier storage.
type legacyObjectName struct{ kind, name string }

// copySource tells whether the old storage can still be copied from: its
// server is ready, its Service exists (the copy Job reaches the server
// through it) and its keys are readable. why says what is missing, or "".
type copySource struct {
	keys      s3Keys
	keysFound bool
	why       string
}

func readCopySource(c *Client, legacy *legacyStorage) (copySource, error) {
	var s copySource
	keys, err := readLegacyKeys(c)
	switch {
	case err == nil:
		s.keys, s.keysFound = keys, true
	case !IsK8sError(err, http.StatusNotFound):
		return s, err
	}
	switch {
	case legacy.server == "":
		s.why = "no server runs there any more"
	case !legacy.ready:
		s.why = legacy.server + " there is not running"
	case !legacy.serviceFound:
		s.why = fmt.Sprintf("its Service %s is gone", legacyService)
	case !s.keysFound:
		s.why = fmt.Sprintf("its Secret %s with the keys is gone", legacySecret)
	}
	return s, nil
}

// removeOldStorage deletes the old storage once nothing of this tool needs
// it: a last copy and check, then every object the tool created there (by
// UID, in legacyRemovalPhases order), and the namespace when asked and
// provably the tool's.
//
// A run that stopped part-way is finished by the next one. Whether it may
// skip the copy is derived from the cluster, not recorded: when the old
// storage can no longer be copied from (copySource) and every data PVC that
// is left is being deleted, a removal got past the copy. Phase 1 marks the
// PVCs only after a verified copy, and before it deletes anything the copy
// needs, so a run never stops in a state that is neither copiable nor
// marked. If someone else deleted a PVC, its deletion was requested already
// and completes once no pod uses it; finishing the removal only completes
// what was started. A data PVC that is not being deleted while the
// storage cannot be copied from may hold objects that were never copied:
// the run stops and deletes nothing.
func (m *migrateRun) removeOldStorage(legacy *legacyStorage, dspas []dspaInfo, deleteNamespace bool) (*types.OperationResponse, error) {
	c := m.c
	source, err := readCopySource(c, legacy)
	if err != nil {
		return m.fail("check", fmt.Sprintf("Cannot read the old storage's keys; nothing was deleted: %v.%s", err, templateHint(err)), firstNonEmpty(errorCodeFromK8sErr(err), "prerequisites"), "keys unreadable")
	}
	resume := source.why != ""
	if pending := legacy.copiablePVCsNotDeleting(); resume && len(pending) > 0 {
		return m.fail("check", sourceGoneMessage(legacy, source.why, pending), "prerequisites", "source gone, data not copied")
	}
	ready, resp := m.newStorageReady("check")
	if resp != nil {
		return resp, nil
	}
	if !ready {
		return m.fail("check", fmt.Sprintf("The storage in namespace %s is not ready, so the old storage is kept. Run Migrate storage first.", s3Namespace), "prerequisites", "new storage not ready")
	}
	if reason := s3TeardownBlocker(dspas, legacy.address); reason != "" {
		instead := "Run Migrate storage to move those this tool created; point the others at the new storage or delete them."
		reason = strings.Replace(strings.Replace(reason, "Delete them first.", instead, 1), "Delete it first.", instead, 1)
		return m.fail("check", "The old storage is still in use, so nothing was deleted. "+reason, "prerequisites", "pipeline servers on the old storage")
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

	var result copyResult
	if resume {
		m.step("copy", "skipped", fmt.Sprintf("Not copied again: the old storage cannot be copied from any more (%s), and its data volumes are already being deleted", source.why))
	} else {
		// Objects written to the old storage since the migration (hand-made
		// clients) are copied before anything is deleted, by a copy that
		// starts now (fresh).
		r, resp := m.copyAndVerify(legacy, dspas, source.keys, "copy", true)
		if resp != nil {
			return resp, nil
		}
		result = r
	}

	m.step("remove", "running", fmt.Sprintf("Deleting the old storage in namespace %s...", legacyNamespace))
	var d storageDeletion
	stopped := func(stage string) (*types.OperationResponse, error) {
		code := "delete_failed"
		if len(d.deleted) > 0 {
			code = "partial_failure"
		}
		return m.fail("remove", fmt.Sprintf("Some objects of the old storage could not be deleted: %s.%s Run Remove old storage again to continue.%s", strings.Join(d.failed, "; "), stage, templateHint(d.forbidden)), code, "delete failed")
	}
	// deleteAll deletes objects in order and stops at the first failure, so
	// the state a run leaves is one legacyRemovalPhases describes.
	deleteAll := func(objects []legacyObjectName) {
		for _, o := range objects {
			var logs []string
			d.deleteOwnedObject(c, legacyNamespace, o.kind, o.name, &logs)
			for _, l := range logs {
				m.step("remove", "running", l)
			}
			if len(d.failed) > 0 {
				return
			}
		}
	}

	// Phase 1: everything that can fail on its own, while the server runs.
	m.deleteLegacyDSPASecrets(dspas, &d)
	if len(d.failed) == 0 {
		deleteAll(legacyRemovalPhases.beforeServer)
	}
	if len(d.failed) > 0 {
		return stopped(fmt.Sprintf(" The server's Deployment in namespace %s was not deleted.", legacyNamespace))
	}
	// Phase 2: the server.
	deleteAll(legacyRemovalPhases.server)
	if len(d.failed) > 0 {
		return stopped("")
	}
	// Phase 3: its PVCs (and with them its pods), then its NetworkPolicies.
	if len(d.pvcUIDs) > 0 {
		remaining, err := waitPVCsGone(c, d.pvcUIDs)
		if err != nil {
			return m.fail("remove", fmt.Sprintf("The old storage's server was deleted, but whether its PVCs are gone cannot be checked: %v. Run Remove old storage again to continue.%s", err, templateHint(err)), firstNonEmpty(errorCodeFromK8sErr(err), "in_progress"), "PVC wait failed")
		}
		if len(remaining) > 0 {
			return m.fail("remove", fmt.Sprintf("The old storage's server was deleted, but PVC %s %s still terminating after %s. Run Remove old storage again to continue.", strings.Join(remaining, ", "), verb(len(remaining), "is", "are"), S3DeleteTimeout), "in_progress", "PVCs terminating")
		}
	}
	deleteAll(legacyRemovalPhases.afterServer)
	if len(d.failed) > 0 {
		return stopped("")
	}
	if len(d.deleted) > 0 {
		m.step("remove", "success", fmt.Sprintf("OK: The old storage is deleted (%s)", strings.Join(d.deleted, ", ")))
	} else {
		m.step("remove", "success", "OK: The old storage is deleted")
	}

	nsMsg := m.removeNamespace(deleteNamespace)
	if after, ok := strings.CutPrefix(nsMsg, "!"); ok {
		return m.fail("namespace", after, "partial_failure", "namespace delete failed")
	}
	var msg string
	if resume {
		msg = fmt.Sprintf("The rest of the old storage in namespace %s was removed. Its data volumes were already being deleted, so nothing was copied this time.", legacyNamespace)
	} else {
		msg = fmt.Sprintf("The old storage in namespace %s was removed; its %s are in namespace %s.", legacyNamespace, result.summary(), s3Namespace)
	}
	if len(d.kept) > 0 {
		msg += fmt.Sprintf(" Left in place because this tool did not create them: %s.", strings.Join(d.kept, ", "))
	}
	msg += " " + nsMsg
	slog.Info("old s3 storage removed", "user", getUser(c), "deleted", strings.Join(d.deleted, ", "), "resumed", resume)
	recordActivity(c, "migrate-storage", fmt.Sprintf("%s removed (%s)", legacyNamespace, strings.TrimSuffix(nsMsg, ".")), true)
	return &types.OperationResponse{Success: true, Message: strings.TrimSpace(msg), Logs: m.logs}, nil
}

// sourceGoneMessage explains why the removal stops when the old storage
// cannot be copied from and its data PVCs (pending) are not being deleted.
func sourceGoneMessage(legacy *legacyStorage, why string, pending []string) string {
	msg := fmt.Sprintf("The old storage in namespace %s cannot be copied from (%s), and its data %s %s %s not being deleted, so %s may hold objects that were never copied. Nothing was deleted.",
		legacyNamespace, why, verb(len(pending), "volume", "volumes"), strings.Join(pending, ", "), verb(len(pending), "is", "are"), verb(len(pending), "it", "they"))
	// A server whose data volume is being deleted cannot start again (a
	// new pod cannot use a PVC with a deletionTimestamp): only the manual
	// path is left then.
	if legacy.server != "" && !legacy.ready && !legacy.anyPVCTerminating() {
		deployment := map[string]string{"SeaweedFS": s3Deployment, "MinIO": legacyMinIODeployment}[legacy.server]
		return msg + fmt.Sprintf(" Start the server again (Deployment %s), then run Remove old storage again.", deployment)
	}
	cmds := make([]string, 0, len(pending))
	for _, p := range pending {
		cmds = append(cmds, fmt.Sprintf("`oc delete pvc %s -n %s`", p, legacyNamespace))
	}
	return msg + fmt.Sprintf(" A retry gives the same result. If you have checked that namespace %s holds everything you need, delete the %s yourself (%s), then run Remove old storage again to remove the rest.",
		s3Namespace, verb(len(pending), "volume", "volumes"), strings.Join(cmds, ", "))
}

// deleteLegacyDSPASecrets deletes the credentials Secret of the pipeline
// servers an earlier version named "dspa": they now use nightly-dspa-s3
// (repoint).
func (m *migrateRun) deleteLegacyDSPASecrets(dspas []dspaInfo, d *storageDeletion) {
	var logs []string
	for _, ds := range dspas {
		if ds.Meta.Name == legacyDSPAName && ds.managedByTool() && ds.SecretName == dspaSecretName {
			var inProject []dspaInfo
			for _, o := range dspas {
				if o.Meta.Namespace == ds.Meta.Namespace {
					inProject = append(inProject, o)
				}
			}
			if note := deleteDSPASecret(m.c, ds.Meta.Namespace, &dspaInfo{Meta: objectMeta{UID: "none"}, SecretName: legacyDSPASecretName}, inProject, &logs); note != "" {
				d.failed = append(d.failed, strings.TrimSpace(note))
			}
		}
	}
	for _, l := range logs {
		m.step("remove", "running", l)
	}
}

// removeNamespace deletes namespace minio when asked and owned by the tool,
// with a UID precondition. It returns the sentence for the result; one
// starting with "!" reports a failure.
func (m *migrateRun) removeNamespace(requested bool) string {
	c := m.c
	meta, found, err := readLegacyNamespace(c)
	switch {
	case err != nil:
		return fmt.Sprintf("!Cannot read namespace %s, so it was kept: %v.%s", legacyNamespace, err, templateHint(err))
	case !found:
		return ""
	}
	owned, reason := legacyNamespaceDeletable(meta)
	switch {
	case !requested && owned:
		m.step("namespace", "skipped", fmt.Sprintf("Namespace %s is kept, as chosen", legacyNamespace))
		return fmt.Sprintf("Namespace %s was kept, as chosen; delete it with `oc delete project %s` once you have checked it holds nothing else.", legacyNamespace, legacyNamespace)
	case !owned:
		m.step("namespace", "skipped", reason)
		return reason
	case meta.terminating():
		return fmt.Sprintf("Namespace %s is already being deleted.", legacyNamespace)
	}
	m.step("namespace", "running", fmt.Sprintf("Deleting namespace %s...", legacyNamespace))
	if _, err := deleteWithUID(c, "/api/v1/namespaces/"+legacyNamespace, meta.UID); err != nil && !IsK8sError(err, 404) {
		// Nothing of the old storage is left, so a retry finds nothing to
		// remove and is not offered (readLegacyStorage returns nil).
		return fmt.Sprintf("!The old storage was removed, but namespace %s could not be deleted: %v.%s A retry finds nothing left to remove; delete the namespace with `oc delete project %s` once you have checked it holds nothing else.", legacyNamespace, err, templateHint(err), legacyNamespace)
	}
	m.step("namespace", "success", fmt.Sprintf("OK: Namespace %s is being deleted, with everything else in it", legacyNamespace))
	return fmt.Sprintf("Namespace %s is being deleted, with everything else in it.", legacyNamespace)
}
