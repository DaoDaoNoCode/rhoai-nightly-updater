package storage

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

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
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
	MigrateJobName = "seaweedfs-migrate"
	// migrateSourceSecret holds the old storage's keys for the copy Job's
	// "old" remote: a Job reads Secrets of its own namespace only. It is
	// owned by the Job (ownerReferences), so it goes with the Job.
	migrateSourceSecret = "seaweedfs-migrate-source"
	MigrateContainer    = "rclone"
	MigrateAppLabel     = "seaweedfs-migrate"
	// RcloneDefaultImage is rclone 1.76.0 (docker.io/rclone/rclone:1.76.0,
	// MIT), pinned by its OCI index digest (linux amd64, arm64, arm/v6,
	// arm/v7, 386; image label org.opencontainers.image.version 1.76.0,
	// revision e982ff36). Alpine based, so the Job runs its script with
	// /bin/sh; it sets no user, so OpenShift's restricted-v2 SCC assigns one.
	RcloneDefaultImage = "docker.io/rclone/rclone@sha256:7cbeec24a44f206067597c0368be4de90ae19a64d5113c7d1938bd737a81e466"
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
	// migrateRepointTimeout wait still fits (repoint). A copy still running
	// then is picked up by the next Migrate run. Tests shorten these.
	MigrateCopyTimeout = 8 * time.Minute
	migratePoll        = 3 * time.Second
	// migrateRepointTimeout bounds the wait for one pipeline server's
	// ObjectStoreAvailable condition after it was repointed.
	migrateRepointTimeout = 2 * time.Minute
	// removeAfterCopyReserve is the part of the operation's deadline the
	// removal keeps for what follows its final copy: the deletions (a few
	// API calls each) and the PVC wait (s3DeleteTimeout, one minute).
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
	CopyOverwrite      = "overwrite"
	CopyIgnoreExisting = "ignore-existing"
)

// migrationCopiedAnnotation, on Secret seaweedfs/seaweedfs-s3-credentials,
// records when Migrate storage finished a verified copy (RFC 3339). The
// Secret is the new storage's own object the ServiceAccount may patch by
// name; its presence ends copyOverwrite and lets the status offer the
// removal (legacyStorageState).
const MigrationCopiedAnnotation = "rhoai-nightly-updater/migration-copied"

// FinalCopyAnnotation marks a copy Job the removal started (fresh): a later
// removal waits for it instead of starting over (copyAndVerify).
const FinalCopyAnnotation = "rhoai-nightly-updater/final-copy"

// MigrationCopied reports whether Migrate storage has finished a verified
// copy into the new storage.
func MigrationCopied(c *kube.Client) (bool, error) {
	meta, found, err := kube.ReadObjectMeta(c, rhoai.NamespacedPath("v1", "secrets", S3Namespace, S3Secret))
	if err != nil || !found {
		return false, err
	}
	return meta.Annotations[MigrationCopiedAnnotation] != "", nil
}

// recordMigrationCopied sets migrationCopiedAnnotation (a merge patch of
// one annotation: setup's server-side apply owns the rest of the Secret).
func recordMigrationCopied(c *kube.Client) error {
	return c.PatchJSON(rhoai.NamespacedPath("v1", "secrets", S3Namespace, S3Secret), map[string]any{"metadata": map[string]any{"annotations": map[string]string{MigrationCopiedAnnotation: kube.Clock().Now().UTC().Format(time.RFC3339)}}})
}

// RcloneImage returns the copy Job's image, overridable with RCLONE_IMAGE
// (for example a mirror on a disconnected cluster). An override must be an
// rclone 1.x image with /bin/sh and grep.
func RcloneImage() string {
	if img := strings.TrimSpace(os.Getenv("RCLONE_IMAGE")); img != "" {
		return img
	}
	return RcloneDefaultImage
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

func MigrateJobPath() string {
	return rhoai.NamespacedPath("batch/v1", "jobs", S3Namespace, MigrateJobName)
}

func migrateJobCreatePath() string {
	return envkit.WithToolFieldManager(rhoai.NamespacedPath("batch/v1", "jobs", S3Namespace, ""))
}

// MigrateJob is the copy Job. The "old" remote reads the old storage's keys
// from migrateSourceSecret, the "new" one the new storage's from its
// Secret; both are in the Job's namespace. The endpoints carry their
// scheme: rclone prepends https:// to one without (backend/s3/s3.go:1597
// at v1.76.0). Pods can reach both S3 ports: NetworkPolicy
// seaweedfs-ingress admits port 8333 from every pod in every namespace
// (namespaceSelector {}), and the old storage's policies do the same for
// its port.
func MigrateJob(oldServer, mode string, final bool) map[string]any {
	secretEnv := func(name, secret, key string) map[string]any {
		return map[string]any{"name": name, "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": secret, "key": key}}}
	}
	remote := func(name, provider, endpoint, secret string) []map[string]any {
		prefix := "RCLONE_CONFIG_" + name + "_"
		return []map[string]any{
			{"name": prefix + "TYPE", "value": "s3"},
			{"name": prefix + "PROVIDER", "value": provider},
			{"name": prefix + "ENDPOINT", "value": endpoint},
			{"name": prefix + "REGION", "value": S3Region},
			secretEnv(prefix+"ACCESS_KEY_ID", secret, S3AccessKeyField),
			secretEnv(prefix+"SECRET_ACCESS_KEY", secret, S3SecretKeyField),
		}
	}
	env := append(remote("OLD", rcloneProvider(oldServer), "http://"+LegacyS3Host(), migrateSourceSecret), remote("NEW", "SeaweedFS", S3Endpoint(), S3Secret)...)
	// The assigned UID has no home directory in the image.
	env = append(env, map[string]any{"name": "HOME", "value": "/tmp"}, map[string]any{"name": "COPY_MODE", "value": mode})
	podLabels := map[string]any{"app": MigrateAppLabel, kube.ManagedByLabelKey: kube.ManagedByLabelValue}
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
						"name":    MigrateContainer,
						"image":   RcloneImage(),
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

// CopySize is `rclone size --json`.
type CopySize struct {
	Count    int64 `json:"count"`
	Bytes    int64 `json:"bytes"`
	Sizeless int64 `json:"sizeless"`
}

// CopyResult is the Job's termination message (migrateScript).
type CopyResult struct {
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
	Source         *CopySize `json:"source"`
	Destination    *CopySize `json:"destination"`
	// DifferSample is the first three paths the check found different,
	// one per line, base64 encoded (migrateScript).
	DifferSample string `json:"differSample"`
}

// differPaths decodes DifferSample.
func (r CopyResult) differPaths() []string {
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
	What  string
	Retry bool
	Fix   string
}

// Problem explains why the copy is not verified, or returns nil. Every
// object of the old storage must be in the new one, byte for byte: the
// one-way check proves that. The new storage may hold more (objects written
// there directly, or deleted from the old storage after an earlier copy);
// they are reported, not counted against the copy (extraObjects). mode is
// the Job's copyMode: with copyIgnoreExisting an object that differs is
// never copied again, so no retry can fix it.
func (r CopyResult) Problem(mode string) *copyProblem {
	n := func(v int64) string { return kube.CountNoun(int(v), "object", "objects") }
	switch {
	case r.Copy != 0:
		return &copyProblem{What: fmt.Sprintf("rclone copy failed (exit code %d)", r.Copy), Retry: true}
	case r.MissingBuckets < 0:
		return &copyProblem{What: "the bucket lists of the two storages could not be read", Retry: true}
	case r.MissingBuckets > 0:
		return &copyProblem{What: fmt.Sprintf("%s of the old storage %s missing in the new storage", kube.CountNoun(int(r.MissingBuckets), "bucket", "buckets"), kube.Verb(int(r.MissingBuckets), "is", "are")), Retry: true}
	case r.Differ > 0 && mode != CopyOverwrite:
		what := fmt.Sprintf("the check found %s different in the new storage", n(r.Differ))
		if paths := r.differPaths(); len(paths) > 0 {
			what += fmt.Sprintf(" (for example %s)", strings.Join(paths, ", "))
		}
		if r.Missing > 0 || r.Errors > 0 {
			what += fmt.Sprintf(", %s missing and %s unreadable", n(r.Missing), n(r.Errors))
		}
		return &copyProblem{What: what, Fix: "They changed in the old storage after an earlier copy, or were written to the new storage since. The copy never overwrites objects in the new storage once the migration has finished or pipeline servers write there, so a retry gives the same result. For each one, copy the old storage's version into the new storage yourself if that is the one to keep, or delete it from the old storage if the new storage's version is the one to keep; then run it again."}
	case r.Missing > 0 || r.Differ > 0 || r.Errors > 0:
		return &copyProblem{What: fmt.Sprintf("the check found %s missing in the new storage, %s different and %s unreadable (written or changed in the old storage during the copy, or not readable then)", n(r.Missing), n(r.Differ), n(r.Errors)), Retry: true}
	case r.Check != 0:
		return &copyProblem{What: fmt.Sprintf("rclone check failed (exit code %d)", r.Check), Retry: true}
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
func (r CopyResult) checked() int64 { return r.Identical + r.Missing + r.Differ + r.Errors }

// Summary describes what was copied: the old storage's objects and bytes
// (size --json), or the checked count when the size could not be read.
func (r CopyResult) Summary() string {
	if r.Source != nil {
		return sizeText(*r.Source)
	}
	return kube.CountNoun(int(r.checked()), "object", "objects")
}

// ExtraObjects returns how many more objects the new storage holds than
// the old one, or 0 (counted by size --json, information only).
func (r CopyResult) ExtraObjects() int64 {
	if r.Source == nil || r.Destination == nil || r.Destination.Count <= r.Source.Count {
		return 0
	}
	return r.Destination.Count - r.Source.Count
}

func sizeText(s CopySize) string {
	return fmt.Sprintf("%s (%s)", kube.CountNoun(int(s.Count), "object", "objects"), kube.CountNoun(int(s.Bytes), "byte", "bytes"))
}

// migrateRun is one run of Migrate or of the removal: it streams each step
// and collects the log lines for the response.
type migrateRun struct {
	c    *kube.Client
	emit func(types.UpdateStepEvent)
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
		m.emit(types.UpdateStepEvent{Step: id, Status: status, Message: msg})
	}
}

// stepLog is an opLog whose lines are step id's progress and go into the
// run's log.
func (m *migrateRun) stepLog(id string) *opLog {
	return &opLog{observe: func(line string) {
		m.logs = append(m.logs, line)
		if m.emit != nil {
			m.emit(types.UpdateStepEvent{Step: id, Status: "running", Message: line})
		}
	}}
}

// secureNewStorage makes sure the new storage is secured (a new admin key
// and the secured settings, when an earlier version's Migrate storage set
// it up without them) and that the recorded pipeline servers already on it
// use keys of their own. It returns the response of a stop, or nil.
func (m *migrateRun) secureNewStorage(dspas []DSPA, rec PipelineRecord) *types.OperationResponse {
	_, ep, err := S3StatusAndEndpoints(m.c)
	if err != nil {
		resp, _ := m.fail("storage", fmt.Sprintf("Cannot read the new storage's Service: %v.%s", err, kube.TemplateHint(err)), kube.ErrorCodeFromK8sErr(err), "endpoints unreadable")
		return resp
	}
	if !ep.Secured {
		m.step("storage", "running", "Securing the new storage: a new admin key, and keys of their own for its pipeline servers")
		keys, err := newAdminKeys(m.c)
		if err != nil {
			resp, _ := m.fail("storage", fmt.Sprintf("Cannot make the new storage's admin key; nothing was changed: %v.%s", err, kube.TemplateHint(err)), kube.ErrorCodeFromK8sErr(err), "keys")
			return resp
		}
		if res := setupS3Storage(m.c, keys, m.stepLog("storage"), s3RetryMigrate, true); !res.Success {
			resp, _ := m.fail("storage", "Securing the new storage did not finish: "+res.Message+" The old storage is unchanged.", res.ErrorCode, "secure failed")
			return resp
		}
		m.step("storage", "running", "OK: The new storage is secured")
	}
	recorded, _ := keyServers(ep, dspas, rec)
	if len(recorded) == 0 {
		return nil
	}
	sw, err := adminSeaweed(m.c)
	if err != nil {
		resp, _ := m.fail("storage", fmt.Sprintf("Cannot read the new storage's keys: %v.%s", err, kube.TemplateHint(err)), kube.ErrorCodeFromK8sErr(err), "new keys unreadable")
		return resp
	}
	for _, d := range recorded {
		name := d.Meta.Namespace + "/" + d.Meta.Name
		changed, err := rekeyServer(m.c, sw, d, nil)
		if err != nil {
			resp, _ := m.fail("storage", fmt.Sprintf("Pipeline server %s, already on the new storage, could not get a key of its own: %v. Run Migrate storage again.", name, err), "s3_keys", "rekey failed")
			return resp
		}
		if changed {
			m.step("storage", "running", fmt.Sprintf("OK: %s has a key of its own", name))
		}
	}
	return nil
}

func (m *migrateRun) fail(id, msg, code, activity string) (*types.OperationResponse, error) {
	if m.emit != nil {
		m.emit(types.UpdateStepEvent{Step: id, Status: "failed", Message: msg, ErrorCode: code})
	}
	m.logs = append(m.logs, msg)
	slog.Warn("s3 storage migration stopped", "step", id, "reason", activity, "user", kube.GetUser(m.c))
	record.Action(m.c, "migrate-storage", fmt.Sprintf("%s (%s)", LegacyNamespace, activity), false)
	return &types.OperationResponse{Success: false, Message: msg, Logs: m.logs, ErrorCode: code}, nil
}

// readLegacyKeys reads the old storage's keys from minio-secret.
func readLegacyKeys(c *kube.Client) (S3Keys, error) {
	path, _ := legacyGetPath("Secret", legacySecret)
	var secret struct {
		Data map[string]string `json:"data"`
	}
	if err := c.GetInto(path, &secret); err != nil {
		if kube.IsDecodeError(err) {
			return S3Keys{}, fmt.Errorf("parse Secret %s/%s: %w", LegacyNamespace, legacySecret, err)
		}
		return S3Keys{}, fmt.Errorf("read Secret %s/%s: %w", LegacyNamespace, legacySecret, err)
	}
	keys := S3Keys{AccessKey: DecodeBase64Field(secret.Data[legacyAccessKeyField]), SecretKey: DecodeBase64Field(secret.Data[legacySecretKeyField])}
	if keys.AccessKey == "" || keys.SecretKey == "" {
		return S3Keys{}, fmt.Errorf("secret %s/%s has no %s or %s", LegacyNamespace, legacySecret, legacyAccessKeyField, legacySecretKeyField)
	}
	return keys, nil
}

// Migrate runs Migrate, or with opts.RemoveOldStorage the removal
// of the old storage, streaming each step to onStep.
func Migrate(c *kube.Client, opts MigrateOptions, onStep func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
	m := &migrateRun{c: c, emit: onStep, op: "Migrate storage", kept: "no pipeline server was moved and the old storage is kept"}
	if opts.RemoveOldStorage {
		m.op, m.kept = "Remove old storage", "nothing was deleted"
	}
	m.step("check", "running", fmt.Sprintf("Checking the storage in namespace %s", LegacyNamespace))
	legacy, err := ReadLegacy(c)
	if err != nil {
		return m.fail("check", fmt.Sprintf("Cannot read the storage in namespace %s; nothing was changed: %v.%s", LegacyNamespace, err, kube.TemplateHint(err)), kube.ErrorCodeFromK8sErr(err), "read failed")
	}
	if legacy == nil {
		m.step("check", "success", fmt.Sprintf("No storage from an earlier version in namespace %s.", LegacyNamespace))
		record.Action(c, "migrate-storage", LegacyNamespace+" (nothing to do)", true)
		return &types.OperationResponse{Success: true, Message: fmt.Sprintf("Nothing to do: there is no storage from an earlier version in namespace %s.", LegacyNamespace), Logs: m.logs}, nil
	}
	if legacy.terminating {
		return m.fail("check", fmt.Sprintf("Namespace %s is being deleted, so its storage cannot be migrated. Nothing was changed.", LegacyNamespace), "terminating", "namespace terminating")
	}
	dspas, err := ListDSPAs(c, "")
	if err != nil && !errors.Is(err, ErrDSPACRDMissing) {
		return m.fail("check", fmt.Sprintf("Cannot list the pipeline servers; nothing was changed: %v.%s", err, kube.TemplateHint(err)), kube.ErrorCodeFromK8sErr(err), "pipeline server list failed")
	}
	if legacy.AddressErr != nil && len(dspas) > 0 {
		return m.fail("check", fmt.Sprintf("Cannot read the old storage's Routes and Service, so which pipeline servers use it is unknown; nothing was changed: %v.%s", legacy.AddressErr, kube.TemplateHint(legacy.AddressErr)), kube.ErrorCodeFromK8sErr(legacy.AddressErr), "address read failed")
	}
	if opts.RemoveOldStorage {
		return m.removeOldStorage(legacy, dspas, opts.DeleteNamespace)
	}
	return m.migrate(legacy, dspas, opts.ConfirmProjects)
}

// sourceReady checks that the old storage's server can be copied from and
// reads its keys.
func (m *migrateRun) sourceReady(legacy *Legacy, step string) (S3Keys, *types.OperationResponse) {
	switch {
	case legacy.Server == "":
		resp, _ := m.fail(step, fmt.Sprintf("No server runs in namespace %s any more, so its objects cannot be copied. Nothing was changed, and a retry gives the same result while no server runs there.", LegacyNamespace), "prerequisites", "no server")
		return S3Keys{}, resp
	case !legacy.Ready:
		resp, _ := m.fail(step, fmt.Sprintf("%s in namespace %s is not running, so its objects cannot be copied. Nothing was changed. Start it again (Deployment %s), then retry.", legacy.Server, LegacyNamespace, map[string]string{"SeaweedFS": S3Deployment, "MinIO": legacyMinIODeployment}[legacy.Server]), "prerequisites", "server not ready")
		return S3Keys{}, resp
	}
	keys, err := readLegacyKeys(m.c)
	if err != nil {
		resp, _ := m.fail(step, fmt.Sprintf("Cannot read the old storage's keys; nothing was changed: %v.%s", err, kube.TemplateHint(err)), kube.FirstNonEmpty(kube.ErrorCodeFromK8sErr(err), "prerequisites"), "keys unreadable")
		return S3Keys{}, resp
	}
	return keys, nil
}

// newStorageReady reports whether the new storage is set up by this tool
// and ready. Its keys are its own (step 1 of Migrate): they are never
// compared with the old storage's. Neither flow points at Tear down here:
// it deletes both storages.
func (m *migrateRun) newStorageReady(step string) (bool, *types.OperationResponse) {
	_, found, err := ReadS3Keys(m.c)
	if err != nil {
		resp, _ := m.fail(step, fmt.Sprintf("Cannot read the new storage's keys: %v.%s", err, kube.TemplateHint(err)), kube.ErrorCodeFromK8sErr(err), "new keys unreadable")
		return false, resp
	}
	state := S3Status(m.c)
	return found && state.Ready && state.ManagedByTool && state.RepairNeeded == "", nil
}

// migrate runs steps 1 to 3.
func (m *migrateRun) migrate(legacy *Legacy, dspas []DSPA, confirmed []string) (*types.OperationResponse, error) {
	oldKeys, resp := m.sourceReady(legacy, "check")
	if resp != nil {
		return resp, nil
	}
	// Which pipeline servers get the new storage's keys: recorded ones, and
	// unrecorded ones only in a project the user confirmed. Read before
	// anything is written; a record that cannot be read stops the run.
	rec, err := ReadPipelineRecord(m.c)
	if err != nil {
		return m.fail("check", fmt.Sprintf("Cannot read this tool's record of its pipeline servers, so which ones are this tool's is unknown; nothing was changed: %v.%s", err, kube.TemplateHint(err)), kube.ErrorCodeFromK8sErr(err), "pipeline record unreadable")
	}
	owned := func(d DSPA) bool {
		return d.ManagedByTool() && (rec.Owns(d.Meta) || slices.Contains(confirmed, d.Meta.Namespace))
	}
	m.step("check", "success", fmt.Sprintf("OK: %s in namespace %s is running", legacy.Server, LegacyNamespace))

	// Step 1: the new storage, with keys of its own: the ones it has when
	// it is set up already, else new random ones (s3Credentials).
	m.step("storage", "running", fmt.Sprintf("Setting up the storage in namespace %s with new keys", S3Namespace))
	ready, resp := m.newStorageReady("storage")
	if resp != nil {
		return resp, nil
	}
	if ready {
		m.step("storage", "success", "OK: The new storage is already set up")
	} else {
		keys, secured, _, err := s3SetupKeys(m.c)
		if err != nil {
			return m.fail("storage", fmt.Sprintf("Cannot make the new storage's keys; nothing was changed: %v.%s", err, kube.TemplateHint(err)), kube.ErrorCodeFromK8sErr(err), "keys")
		}
		res := setupS3Storage(m.c, keys, m.stepLog("storage"), s3RetryMigrate, secured)
		if !res.Success {
			// res.Message names Migrate storage as the way to continue (s3RetryMigrate).
			return m.fail("storage", "Setting up the new storage did not finish: "+res.Message+" The old storage is unchanged.", res.ErrorCode, "setup failed")
		}
		m.step("storage", "success", "OK: The new storage is ready")
	}
	// The new storage ends secured: its admin key goes into no project, and
	// every pipeline server moved to it gets a key of its own. New storage
	// that an earlier version's Migrate storage set up is not secured yet:
	// it gets a new admin key and the secured settings here, and the
	// recorded pipeline servers it already moved get their own keys.
	if resp := m.secureNewStorage(dspas, rec); resp != nil {
		return resp, nil
	}

	// Step 2: copy and verify. From now on the new storage may get data
	// of its own, so no later copy overwrites there (copyMode).
	result, resp := m.copyAndVerify(legacy, dspas, oldKeys, "copy", false)
	if resp != nil {
		return resp, nil
	}
	if err := recordMigrationCopied(m.c); err != nil {
		return m.fail("copy", fmt.Sprintf("The copy is verified, but it could not be recorded on Secret %s/%s: %v. No pipeline server was moved; run Migrate storage again.%s", S3Namespace, S3Secret, err, kube.TemplateHint(err)), kube.ErrorCodeFromK8sErr(err), "record failed")
	}

	// Step 3: the pipeline servers, each with a key of its own.
	sw, err := adminSeaweed(m.c)
	if err != nil {
		return m.fail("repoint", fmt.Sprintf("Cannot read the new storage's keys; no pipeline server was moved: %v.%s", err, kube.TemplateHint(err)), kube.ErrorCodeFromK8sErr(err), "new keys unreadable")
	}
	repointed, listed, resp := m.repoint(legacy, dspas, sw, owned)
	if resp != nil {
		return resp, nil
	}

	msg := fmt.Sprintf("Migrated %s to namespace %s.", result.Summary(), S3Namespace)
	if len(repointed) > 0 {
		msg += fmt.Sprintf(" %s now %s it: %s.", kube.CountNoun(len(repointed), "pipeline server", "pipeline servers"), kube.Verb(len(repointed), "uses", "use"), strings.Join(repointed, ", "))
	}
	if len(listed) > 0 {
		msg += fmt.Sprintf(" Not changed by this tool, still on the old storage: %s.", strings.Join(listed, "; "))
	}
	msg += fmt.Sprintf(" The old storage in namespace %s is kept: delete it later from the S3 storage's menu, once nothing else uses it.", LegacyNamespace)
	slog.Info("s3 storage migrated", "user", kube.GetUser(m.c), "objects", result.checked())
	record.Action(m.c, "migrate-storage", fmt.Sprintf("%s -> %s (%s, %d pipeline servers)", LegacyNamespace, S3Namespace, result.Summary(), len(repointed)), true)
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
func (m *migrateRun) copyAndVerify(legacy *Legacy, dspas []DSPA, oldKeys S3Keys, step string, fresh bool) (CopyResult, *types.OperationResponse) {
	c := m.c
	fail := func(msg, code, activity string) (CopyResult, *types.OperationResponse) {
		resp, _ := m.fail(step, msg, code, activity)
		return CopyResult{}, resp
	}
	m.step(step, "running", fmt.Sprintf("Copying every bucket and object from %s to %s (Job %s)", LegacyNamespace, S3Namespace, MigrateJobName))

	// Whether a pipeline server uses the new storage yet decides whether
	// the copy may overwrite objects there (copyMode). Data connections and
	// workbenches cannot be seen; the removal's checklist asks the user to
	// move them only after the migration.
	endpoints, err := readS3Endpoints(c)
	if err != nil {
		return fail(fmt.Sprintf("Cannot read the new storage's Service; the copy did not start: %v.%s", err, kube.TemplateHint(err)), kube.ErrorCodeFromK8sErr(err), "endpoints unreadable")
	}
	copied, err := MigrationCopied(c)
	if err != nil {
		return fail(fmt.Sprintf("Cannot read Secret %s/%s; the copy did not start: %v.%s", S3Namespace, S3Secret, err, kube.TemplateHint(err)), kube.ErrorCodeFromK8sErr(err), "secret read failed")
	}
	mode := CopyOverwrite
	if fresh || copied {
		mode = CopyIgnoreExisting
	}
	for _, d := range dspas {
		if endpoints.Address.UsedBy(d.Meta.Namespace, d.Host) {
			mode = CopyIgnoreExisting
		}
	}

	job, found, err := readMigrateJob(c)
	if err != nil {
		return fail(fmt.Sprintf("Cannot read Job %s/%s; the copy did not start: %v.%s", S3Namespace, MigrateJobName, err, kube.TemplateHint(err)), kube.ErrorCodeFromK8sErr(err), "job read failed")
	}
	if found && !job.Meta.HasToolLabel() {
		return fail(fmt.Sprintf("Job %s/%s exists and was not created by this tool, so it was left unchanged and the copy did not start. Delete it, then retry.", S3Namespace, MigrateJobName), "not_managed", "foreign job")
	}
	running := found && !job.Meta.Terminating() && !job.finished()
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
			if !job.Meta.Terminating() {
				if _, err := kube.DeleteWithUIDPropagation(c, MigrateJobPath(), job.Meta.UID, policy); err != nil && !kube.IsK8sNotFound(err) {
					return fail(fmt.Sprintf("Cannot delete the Job %s/%s of an earlier run: %v.%s", S3Namespace, MigrateJobName, err, kube.TemplateHint(err)), kube.ErrorCodeFromK8sErr(err), "old job delete failed")
				}
			}
			gone, err := kube.WaitForDeletion(c, MigrateJobPath(), s3DeleteTimeout, s3DeletePoll)
			if err != nil || !gone {
				return fail(fmt.Sprintf("Job %s/%s of an earlier run is still being deleted; retry in a minute.", S3Namespace, MigrateJobName), "in_progress", "old job lingering")
			}
		}
		obj := MigrateJob(legacy.Server, mode, fresh)
		data, _ := json.Marshal(obj)
		if _, _, err := c.Do(http.MethodPost, migrateJobCreatePath(), "application/json", data, url.Values{"dryRun": {"All"}}); err != nil {
			return fail(fmt.Sprintf("The API server rejects the copy Job: %v. Nothing was changed.%s", err, kube.TemplateHint(err)), kube.FirstNonEmpty(kube.ErrorCodeFromK8sErr(err), "validation"), "job dry run failed")
		}
		// The old keys first, so the Job's pod finds them when it starts.
		if err := writeMigrateSource(c, oldKeys); err != nil {
			return fail(fmt.Sprintf("Cannot write the copy Job's Secret %s/%s with the old storage's keys; the copy did not start: %v.%s", S3Namespace, migrateSourceSecret, err, kube.TemplateHint(err)), kube.ErrorCodeFromK8sErr(err), "source secret failed")
		}
		body, _, err := c.Post(migrateJobCreatePath(), data)
		if err != nil {
			return fail(fmt.Sprintf("Cannot create the copy Job: %v.%s", err, kube.TemplateHint(err)), kube.ErrorCodeFromK8sErr(err), "job create failed")
		}
		if job, err = parseMigrateJob(body); err != nil {
			return fail(fmt.Sprintf("Cannot read the copy Job: %v", err), "", "job parse failed")
		}
		// The Secret goes with the Job (garbage collection), whichever way
		// the Job ends: deleted here, by a later run, or by its TTL.
		if err := ownMigrateSource(c, job.Meta.UID); err != nil {
			m.deleteMigrateJob(job)
			return fail(fmt.Sprintf("Cannot give the copy Job's Secret %s/%s to the Job; the copy was stopped: %v.%s", S3Namespace, migrateSourceSecret, err, kube.TemplateHint(err)), kube.ErrorCodeFromK8sErr(err), "source secret owner failed")
		}
	}

	timeout := MigrateCopyTimeout
	if deadline, ok := c.Context().Deadline(); ok && fresh && kube.Clock().Until(deadline)-removeAfterCopyReserve > timeout {
		timeout = kube.Clock().Until(deadline) - removeAfterCopyReserve
	}
	job, issue, err := waitMigrateJob(c, job, timeout)
	switch {
	case errors.Is(err, errS3CannotStart):
		m.deleteMigrateJob(job)
		return fail(fmt.Sprintf("The copy Job's pod cannot start (%s: %s). Nothing else was changed. Fix the cause (for example set RCLONE_IMAGE to a pullable rclone image), then run %s again.", issue.Reason, kube.TruncateText(issue.Message, 300), m.op), "not_ready", "job cannot start")
	case errors.Is(err, ErrS3NotReady) && fresh:
		return fail(fmt.Sprintf("The final copy and check are still running after %s, so %s. The copy keeps running: run %s again to keep waiting for it; the old storage is deleted only once it has passed.", timeout.Round(time.Second), m.kept, m.op), "in_progress", "final copy still running")
	case errors.Is(err, ErrS3NotReady):
		return fail(fmt.Sprintf("The copy is still running after %s. Nothing else was changed. Run %s again to keep waiting for it.", timeout, m.op), "in_progress", "copy still running")
	case err != nil:
		return fail(fmt.Sprintf("Stopped waiting for the copy Job: %v.%s", err, kube.TemplateHint(err)), kube.ErrorCodeFromK8sErr(err), "job wait failed")
	}
	result, readErr := readMigrateResult(c, job)
	m.deleteMigrateJob(job)
	if readErr != nil {
		return fail(fmt.Sprintf("The copy Job ended without a readable result (%v). Nothing else was changed; run %s again.", readErr, m.op), kube.ErrorCodeFromK8sErr(readErr), "no result")
	}
	if p := result.Problem(job.Mode); p != nil {
		msg := fmt.Sprintf("The copy is not complete: %s. Nothing else was changed: %s.", p.What, m.kept)
		if p.Retry {
			msg += fmt.Sprintf(" Run %s again to retry.", m.op)
		} else {
			msg += " " + p.Fix
		}
		return fail(msg, "copy_mismatch", "copy mismatch: "+p.What)
	}
	if extra := result.ExtraObjects(); extra > 0 {
		m.step(step, "running", fmt.Sprintf("Note: the new storage holds %s more than the old one (written there directly, or deleted from the old storage after an earlier copy); they are kept", kube.CountNoun(int(extra), "object", "objects")))
	}
	m.step(step, "success", fmt.Sprintf("OK: %s copied and checked byte for byte", kube.CountNoun(int(result.checked()), "object", "objects")))
	return result, nil
}

// migrateSourcePath is the copy Job's Secret with the old storage's keys.
func migrateSourcePath() string {
	return rhoai.NamespacedPath("v1", "secrets", "seaweedfs", "seaweedfs-migrate-source")
}

// writeMigrateSource writes the copy Job's Secret with the old storage's
// keys: one this tool wrote earlier (an earlier run's, still owned by that
// Job, which is gone or being replaced) is deleted by UID first, and a new
// one is created. One someone else made is left alone and stops the copy.
func writeMigrateSource(c *kube.Client, keys S3Keys) error {
	meta, found, err := kube.ReadObjectMeta(c, migrateSourcePath())
	if err != nil {
		return err
	}
	if found {
		if !meta.HasToolLabel() {
			return fmt.Errorf("it exists and was not created by this tool")
		}
		if _, err := kube.DeleteWithUID(c, migrateSourcePath(), meta.UID); err != nil && !kube.IsK8sNotFound(err) {
			return fmt.Errorf("delete an earlier run's copy: %w", err)
		}
	}
	data, _ := json.Marshal(map[string]any{
		"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
		"metadata":   map[string]any{"name": migrateSourceSecret, "namespace": S3Namespace, "labels": envkit.ToolLabels()},
		"stringData": map[string]any{S3AccessKeyField: keys.AccessKey, S3SecretKeyField: keys.SecretKey},
	})
	_, _, err = c.Post(envkit.WithToolFieldManager(rhoai.NamespacedPath("v1", "secrets", S3Namespace, "")), data)
	return err
}

// ownMigrateSource makes the copy Job (jobUID) the owner of its Secret, so
// garbage collection deletes the Secret with the Job. OpenShift's
// OwnerReferencesPermissionEnforcement admission asks for delete on the
// Secret to set it (the rule names it); blockOwnerDeletion is not set, so
// nothing is asked of the Job.
func ownMigrateSource(c *kube.Client, jobUID string) error {
	meta, found, err := kube.ReadObjectMeta(c, migrateSourcePath())
	if err != nil {
		return err
	}
	if !found || !meta.HasToolLabel() {
		return fmt.Errorf("secret %s/%s is gone or not this tool's", S3Namespace, migrateSourceSecret)
	}
	return c.PatchJSON(migrateSourcePath(), map[string]any{"metadata": map[string]any{
		"resourceVersion": meta.ResourceVersion,
		"ownerReferences": []any{map[string]any{"apiVersion": "batch/v1", "kind": "Job", "name": MigrateJobName, "uid": jobUID}},
	}})
}

// deleteMigrateJob deletes the copy Job by UID; its pod goes with it
// (background propagation).
func (m *migrateRun) deleteMigrateJob(job migrateJobInfo) {
	if _, err := kube.DeleteWithUID(m.c, MigrateJobPath(), job.Meta.UID); err != nil && !kube.IsK8sNotFound(err) {
		m.logs = append(m.logs, fmt.Sprintf("Warning: cannot delete Job %s/%s: %v (it removes itself a day after it finished)", S3Namespace, MigrateJobName, err))
		return
	}
	m.logs = append(m.logs, fmt.Sprintf("OK: Job %s/%s deleted", S3Namespace, MigrateJobName))
}

// migrateJobInfo is the part of the copy Job the tool reads.
type migrateJobInfo struct {
	Meta     kube.ObjectMeta
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
	meta := map[string]any{"name": MigrateJobName, "namespace": S3Namespace, "labels": envkit.ToolLabels()}
	if final {
		meta["annotations"] = map[string]any{FinalCopyAnnotation: "true"}
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
		Metadata kube.ObjectMeta `json:"metadata"`
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
	j := migrateJobInfo{Meta: obj.Metadata, Selector: obj.Spec.Selector.MatchLabels, FinalCopy: obj.Metadata.Annotations[FinalCopyAnnotation] == "true"}
	for _, ctr := range obj.Spec.Template.Spec.Containers {
		for _, e := range ctr.Env {
			if ctr.Name == MigrateContainer && e.Name == "COPY_MODE" {
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
			j.Failed, j.FailReason = true, kube.FirstNonEmpty(cond.Reason, cond.Message)
		}
	}
	return j, nil
}

func readMigrateJob(c *kube.Client) (migrateJobInfo, bool, error) {
	body, found, err := c.GetOptional(MigrateJobPath())
	if err != nil {
		return migrateJobInfo{}, false, err
	}
	if !found {
		return migrateJobInfo{}, false, nil
	}
	j, err := parseMigrateJob(body)
	return j, err == nil, err
}

// waitMigrateJob waits, bounded by timeout, until the Job has
// finished. It returns errS3CannotStart as soon as its pod reports a
// terminal waiting reason (ImagePullBackOff, ...), and errS3NotReady on
// timeout.
func waitMigrateJob(c *kube.Client, job migrateJobInfo, timeout time.Duration) (migrateJobInfo, envkit.PodIssue, error) {
	ctx, cancel := kube.Clock().WithTimeout(c.Context(), timeout)
	defer cancel()
	cc := c.WithContext(ctx)
	for {
		if job.finished() {
			return job, envkit.PodIssue{}, nil
		}
		if sel := job.selector(); sel != "" {
			if issue, err := envkit.InspectPods(cc, S3Namespace, sel, RcloneImage()); err == nil && issue.Terminal {
				return job, issue, errS3CannotStart
			}
		}
		if kube.Clock().Sleep(ctx, migratePoll) != nil {
			if c.Context().Err() != nil {
				return job, envkit.PodIssue{}, c.Context().Err()
			}
			return job, envkit.PodIssue{}, ErrS3NotReady
		}
		next, found, err := readMigrateJob(cc)
		switch {
		case ctx.Err() != nil:
			continue
		case err != nil && !kube.IsNetworkError(err):
			return job, envkit.PodIssue{}, err
		case err == nil && !found:
			return job, envkit.PodIssue{}, fmt.Errorf("the copy Job %s/%s disappeared while the copy was running", S3Namespace, MigrateJobName)
		case err == nil:
			if next.Meta.UID != job.Meta.UID {
				return job, envkit.PodIssue{}, fmt.Errorf("the copy Job %s/%s was replaced while the copy was running", S3Namespace, MigrateJobName)
			}
			job = next
		}
	}
}

// readMigrateResult reads the copy's result from the termination message of
// the Job's pod.
func readMigrateResult(c *kube.Client, job migrateJobInfo) (CopyResult, error) {
	sel := job.selector()
	if sel == "" {
		return CopyResult{}, fmt.Errorf("the Job has no pod selector")
	}
	body, _, err := c.Get(rhoai.NamespacedPath("v1", "pods", S3Namespace, "") + "?labelSelector=" + url.QueryEscape(sel))
	if err != nil {
		return CopyResult{}, fmt.Errorf("list its pods: %w", err)
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
		return CopyResult{}, fmt.Errorf("parse its pods: %w", err)
	}
	reason := kube.FirstNonEmpty(job.FailReason, "no pod finished")
	for _, p := range list.Items {
		for _, cs := range p.Status.ContainerStatuses {
			if cs.Name != MigrateContainer || cs.State.Terminated == nil {
				continue
			}
			t := cs.State.Terminated
			var r CopyResult
			if err := json.Unmarshal([]byte(strings.TrimSpace(t.Message)), &r); err == nil {
				return r, nil
			}
			reason = kube.FirstNonEmpty(t.Reason, job.FailReason, fmt.Sprintf("exit code %d", t.ExitCode))
		}
	}
	return CopyResult{}, errors.New(reason)
}

// UnrecordedReason is why Migrate storage leaves a pipeline server that
// looks like this tool's but has no record (pipeline_record.go).
const UnrecordedReason = "not recorded as this tool's: confirm its project in Migrate storage to move it"

// Repointable reports whether the tool may move a pipeline server that
// uses the old storage, or why not.
func Repointable(d DSPA) (string, bool) {
	switch {
	case !d.ManagedByTool():
		return "not created by this tool", false
	case d.Meta.Terminating():
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
func (d DSPA) objectStoreReady() bool {
	return d.ObjectStoreReadyAt(d.Meta.Generation)
}

// ObjectStoreReadyAt reports whether ObjectStoreAvailable is True for
// generation or a later one. DSPO stamps every condition with the
// generation it reconciled (dspa_status.go GetConditions), so a True from
// before a change does not count.
func (d DSPA) ObjectStoreReadyAt(generation int64) bool {
	return d.ObjectStore != nil && d.ObjectStore.Status == "True" && d.ObjectStore.ObservedGeneration >= generation
}

// repoint moves the pipeline servers this tool created from the old storage
// to the new one. It returns the moved ones and those it left (with why).
// Every write is dry-run first. Each server is waited for before the next
// is moved; one that does not reach the new storage stops the run, so the
// old storage stays.
func (m *migrateRun) repoint(legacy *Legacy, dspas []DSPA, sw *seaweedClient, owned func(DSPA) bool) (moved, listed []string, resp *types.OperationResponse) {
	c := m.c
	failed := func(msg, code, activity string) ([]string, []string, *types.OperationResponse) {
		r, _ := m.fail("repoint", msg, code, activity)
		return moved, listed, r
	}
	m.step("repoint", "running", "Moving the pipeline servers this tool created to the new storage")
	endpoints, err := readS3Endpoints(c)
	if err != nil {
		return failed(fmt.Sprintf("Cannot read the new storage's Service: %v.%s", err, kube.TemplateHint(err)), kube.ErrorCodeFromK8sErr(err), "endpoints unreadable")
	}
	var todo []DSPA
	for _, d := range dspas {
		name := d.Meta.Namespace + "/" + d.Meta.Name
		switch {
		case legacy.Address.UsedBy(d.Meta.Namespace, d.Host):
			if why, ok := Repointable(d); ok && owned(d) {
				todo = append(todo, d)
			} else if ok {
				// Its label (or field managers) say this tool's, which a
				// project editor can write: without the record or the user's
				// confirmation, the storage's keys stay out of that project.
				listed = append(listed, name+" ("+UnrecordedReason+")")
			} else {
				listed = append(listed, name+" ("+why+")")
			}
		case owned(d) && endpoints.Address.UsedBy(d.Meta.Namespace, d.Host) && !d.Meta.Terminating() && !d.objectStoreReady():
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
		d       DSPA
		secrets []*projectSecret
		patch   []byte
	}
	plans := make([]*plan, 0, len(todo))
	for _, d := range todo {
		project := d.Meta.Namespace
		// The same Secrets setup writes (the S3 secret, and the dashboard
		// Connection), owned by the DSPA they are for (it exists here), so
		// they go with the DSPA whoever deletes it. The dry run sends them
		// without keys; each server's own key is made just before its move.
		secrets := []*projectSecret{
			{c: c, project: project, name: DSPASecretName, what: "secret", obj: dspaS3Secret(project, S3Keys{})},
			{c: c, project: project, name: ConnectionSecretName, what: "connection", obj: dashboardS3Connection(project, S3Keys{})},
		}
		for _, s := range secrets {
			setDSPAOwner(s.obj, d.Meta.Name, d.Meta.UID)
			if err := readProjectSecret(s, projectSecretPath(project, s.name)); err != nil {
				return failed(fmt.Sprintf("Cannot read Secret %s/%s; no pipeline server was moved: %v.%s", project, s.name, err, kube.TemplateHint(err)), kube.ErrorCodeFromK8sErr(err), s.what+" read failed")
			}
			if s.found && !s.meta.HasToolLabel() {
				return failed(fmt.Sprintf("Secret %s/%s exists and was not created by this tool, so pipeline server %s/%s was not moved and nothing was changed.", project, s.name, project, d.Meta.Name), "not_managed", "foreign "+s.what)
			}
		}
		patch, _ := json.Marshal(map[string]any{
			"metadata": map[string]any{"resourceVersion": d.Meta.ResourceVersion, "labels": envkit.ToolLabels(),
				"annotations": map[string]any{DSPAConnectionAnnotation: ConnectionSecretName, DSPAS3UserAnnotation: ScopedUserName(project)}},
			"spec": map[string]any{"objectStorage": map[string]any{"externalStorage": map[string]any{
				"host": S3Host(),
				"s3CredentialsSecret": map[string]any{
					"secretName": DSPASecretName,
					"accessKey":  S3AccessKeyField,
					"secretKey":  S3SecretKeyField,
				},
			}}},
		})
		plans = append(plans, &plan{d: d, patch: patch, secrets: secrets})
	}
	// writeSecrets writes the plan's Secrets; on failure it returns the
	// message and code for the one that failed.
	writeSecrets := func(p *plan, dryRun bool) (string, string, error) {
		for _, s := range p.secrets {
			if _, err := writeProjectSecret(s, projectSecretPath(p.d.Meta.Namespace, s.name), envkit.WithToolFieldManager(rhoai.NamespacedPath("v1", "secrets", p.d.Meta.Namespace, "")), dryRun); err != nil {
				msg, code := pipelineWriteFailure(fmt.Sprintf("secret %s", s.name), p.d.Meta.Namespace, err)
				return msg, code, err
			}
		}
		return "", "", nil
	}
	patchDSPA := func(p *plan, dryRun bool) ([]byte, error) {
		q := url.Values{"fieldManager": {envkit.ToolFieldManager}}
		if dryRun {
			q.Set("dryRun", "All")
		}
		path, ok := toolDSPAPath(p.d.Meta.Namespace, p.d.Meta.Name)
		if !ok {
			return nil, fmt.Errorf("pipeline server %s is not a name this tool creates", p.d.Meta.Name)
		}
		body, _, err := c.Do(http.MethodPatch, path, "application/merge-patch+json", p.patch, q)
		return body, err
	}

	m.step("repoint", "running", "Checking every change with the API server (dry run)")
	for _, p := range plans {
		name := p.d.Meta.Namespace + "/" + p.d.Meta.Name
		if msg, code, err := writeSecrets(p, true); err != nil {
			return failed(msg+" No pipeline server was moved.", code, "secret dry run failed")
		}
		if _, err := patchDSPA(p, true); err != nil {
			code := kube.ErrorCodeFromK8sErr(err)
			if kube.IsK8sConflict(err) {
				code = "conflict"
			}
			return failed(fmt.Sprintf("The API server rejects the change to pipeline server %s: %v. No pipeline server was moved.%s", name, err, kube.TemplateHint(err)), kube.FirstNonEmpty(code, "validation"), "DSPA dry run failed")
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
		// MigrateCopyTimeout and a migrateRepointTimeout wait per server.
		// A server is moved only when its whole wait fits, so a run never
		// ends with one moved but not yet checked.
		if deadline, ok := c.Context().Deadline(); ok && kube.Clock().Until(deadline) < migrateRepointTimeout {
			return failed(fmt.Sprintf("Not enough time is left in this run to move pipeline server %s and wait until it reaches the new storage (up to %s). Already moved: %s. The old storage is kept; run Migrate storage again to continue.", name, migrateRepointTimeout, done), "in_progress", "out of time")
		}
		m.step("repoint", "running", fmt.Sprintf("Moving pipeline server %s", name))
		project := p.d.Meta.Namespace
		keys, _, err := provisionScopedKey(sw, project, S3Keys{})
		if err == nil {
			err = probeScopedKey(c.Context(), keys, ScopedUserName(project))
		}
		if err != nil {
			return failed(fmt.Sprintf("Cannot give pipeline server %s a key of its own: %v. It was not moved. Already moved: %s. The old storage is kept; run Migrate storage again.", name, err, done), "s3_keys", "scoped key failed")
		}
		p.secrets[0].obj = dspaS3Secret(project, keys)
		p.secrets[1].obj = dashboardS3Connection(project, keys)
		// The move's patch names the key too, as a finished Secure S3
		// storage leaves it (KeysState compares it).
		var patch map[string]any
		if json.Unmarshal(p.patch, &patch) == nil {
			patch["metadata"].(map[string]any)["annotations"].(map[string]any)[DSPAS3KeyAnnotation] = keys.AccessKey
			p.patch, _ = json.Marshal(patch)
		}
		for _, s := range p.secrets {
			setDSPAOwner(s.obj, p.d.Meta.Name, p.d.Meta.UID)
		}
		if msg, code, err := writeSecrets(p, false); err != nil {
			return failed(fmt.Sprintf("%s Already moved: %s. The old storage is kept; run Migrate storage again.", msg, done), code, "secret write failed")
		}
		body, err := patchDSPA(p, false)
		if err != nil {
			code := kube.ErrorCodeFromK8sErr(err)
			if kube.IsK8sConflict(err) {
				code = "conflict"
			}
			return failed(fmt.Sprintf("Cannot move pipeline server %s: %v. Already moved: %s. The old storage is kept; run Migrate storage again.%s", name, err, done, kube.TemplateHint(err)), code, "DSPA patch failed")
		}
		d, err := waitObjectStore(c, p.d.Meta.Namespace, p.d.Meta.Name, RepointedGeneration(body, p.d.Meta.Generation))
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
	m.step("repoint", "success", fmt.Sprintf("OK: %s moved", kube.CountNoun(len(moved), "pipeline server", "pipeline servers")))
	return moved, listed, nil
}

// RepointedGeneration is the generation DSPO must have observed for a
// repointed DSPA's ObjectStoreAvailable to count: the one in the patch
// response, or before+1 when that cannot be read. The patch carries the
// resourceVersion that was read (a precondition) and changes spec, so it is
// the first change after generation before and the API server sets
// before+1; a condition from before the patch (generation before or
// lower) is never accepted, even when the response cannot be parsed.
func RepointedGeneration(patchResponse []byte, before int64) int64 {
	if meta, err := kube.ParseObjectMeta(patchResponse); err == nil && meta.Generation > before {
		return meta.Generation
	}
	return before + 1
}

func objectStoreText(d DSPA) string {
	switch {
	case d.ObjectStore == nil:
		return "the data-science-pipelines operator reports no ObjectStoreAvailable condition yet"
	case d.ObjectStore.ObservedGeneration < d.Meta.Generation:
		return "the data-science-pipelines operator has not checked the change yet"
	}
	return fmt.Sprintf("ObjectStoreAvailable is %s: %s", d.ObjectStore.Status, kube.FirstNonEmpty(kube.TruncateText(d.ObjectStore.Message, 300), d.ObjectStore.Reason))
}

// waitObjectStore waits, bounded by migrateRepointTimeout, until DSPO
// reports ObjectStoreAvailable=True for generation (or a later one). A False
// condition does not end the wait: DSPO requeues and checks again, for
// example while the new Service's address propagates.
func waitObjectStore(c *kube.Client, project, name string, generation int64) (DSPA, error) {
	ctx, cancel := kube.Clock().WithTimeout(c.Context(), migrateRepointTimeout)
	defer cancel()
	cc := c.WithContext(ctx)
	last := DSPA{Meta: kube.ObjectMeta{Generation: generation}}
	for {
		list, err := ListDSPAs(cc, project)
		if err != nil && ctx.Err() == nil && !kube.IsNetworkError(err) {
			return last, fmt.Errorf("its status cannot be read: %w", err)
		}
		for _, d := range list {
			if d.Meta.Name != name {
				continue
			}
			if d.ObjectStoreReadyAt(generation) {
				return d, nil
			}
			last = d
		}
		if kube.Clock().Sleep(ctx, migratePoll) != nil {
			if c.Context().Err() != nil {
				return last, c.Context().Err()
			}
			return last, fmt.Errorf("after %s %s", migrateRepointTimeout, objectStoreText(last))
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
func toolNamespaceDeletable(name string, meta kube.ObjectMeta) (bool, string) {
	switch {
	case !meta.HasToolLabel():
		return false, fmt.Sprintf("Namespace %s is kept: it does not carry this tool's label (%s=%s).", name, kube.ManagedByLabelKey, kube.ManagedByLabelValue)
	case !meta.HasManager(envkit.ToolManagers):
		return false, fmt.Sprintf("Namespace %s is kept: it carries this tool's label, but its managedFields show no write by this tool.", name)
	}
	return true, ""
}

// LegacyNamespaceDeletable is toolNamespaceDeletable for namespace minio.
func LegacyNamespaceDeletable(meta kube.ObjectMeta) (bool, string) {
	return toolNamespaceDeletable(LegacyNamespace, meta)
}

// readLegacyNamespace reads namespace minio's metadata.
func readLegacyNamespace(c *kube.Client) (kube.ObjectMeta, bool, error) {
	return kube.ReadObjectMeta(c, "/api/v1/namespaces/"+LegacyNamespace)
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
		{"PersistentVolumeClaim", LegacySeaweedFSPVC},
		{"PersistentVolumeClaim", LegacyMinIOPVC},
		{"Secret", legacySecret},
		{"Service", LegacyService},
	},
	server: []legacyObjectName{
		{"Deployment", S3Deployment},
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
	keys      S3Keys
	keysFound bool
	why       string
}

func readCopySource(c *kube.Client, legacy *Legacy) (copySource, error) {
	var s copySource
	keys, err := readLegacyKeys(c)
	switch {
	case err == nil:
		s.keys, s.keysFound = keys, true
	case !kube.IsK8sNotFound(err):
		return s, err
	}
	switch {
	case legacy.Server == "":
		s.why = "no server runs there any more"
	case !legacy.Ready:
		s.why = legacy.Server + " there is not running"
	case !legacy.serviceFound:
		s.why = fmt.Sprintf("its Service %s is gone", LegacyService)
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
func (m *migrateRun) removeOldStorage(legacy *Legacy, dspas []DSPA, deleteNamespace bool) (*types.OperationResponse, error) {
	c := m.c
	source, err := readCopySource(c, legacy)
	if err != nil {
		return m.fail("check", fmt.Sprintf("Cannot read the old storage's keys; nothing was deleted: %v.%s", err, kube.TemplateHint(err)), kube.FirstNonEmpty(kube.ErrorCodeFromK8sErr(err), "prerequisites"), "keys unreadable")
	}
	resume := source.why != ""
	if pending := legacy.CopiablePVCsNotDeleting(); resume && len(pending) > 0 {
		return m.fail("check", SourceGoneMessage(legacy, source.why, pending), "prerequisites", "source gone, data not copied")
	}
	ready, resp := m.newStorageReady("check")
	if resp != nil {
		return resp, nil
	}
	if !ready {
		return m.fail("check", fmt.Sprintf("The storage in namespace %s is not ready, so the old storage is kept. Run Migrate storage first.", S3Namespace), "prerequisites", "new storage not ready")
	}
	if reason := S3TeardownBlocker(dspas, legacy.Address); reason != "" {
		instead := "Run Migrate storage to move those this tool created; point the others at the new storage or delete them."
		reason = strings.Replace(strings.Replace(reason, "Delete them first.", instead, 1), "Delete it first.", instead, 1)
		return m.fail("check", "The old storage is still in use, so nothing was deleted. "+reason, "prerequisites", "pipeline servers on the old storage")
	}
	endpoints, err := readS3Endpoints(c)
	if err != nil {
		return m.fail("check", fmt.Sprintf("Cannot read the new storage's Service; nothing was deleted: %v.%s", err, kube.TemplateHint(err)), kube.ErrorCodeFromK8sErr(err), "endpoints unreadable")
	}
	for _, d := range dspas {
		if d.ManagedByTool() && !d.Meta.Terminating() && endpoints.Address.UsedBy(d.Meta.Namespace, d.Host) && !d.objectStoreReady() {
			return m.fail("check", fmt.Sprintf("Pipeline server %s/%s does not reach the new storage (%s), so the old storage is kept.", d.Meta.Namespace, d.Meta.Name, objectStoreText(d)), "not_ready", "moved server not ready")
		}
	}
	m.step("check", "success", "OK: Nothing of this tool uses the old storage")

	var result CopyResult
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

	m.step("remove", "running", fmt.Sprintf("Deleting the old storage in namespace %s", LegacyNamespace))
	var d storageDeletion
	stopped := func(stage string) (*types.OperationResponse, error) {
		code := "delete_failed"
		if len(d.deleted) > 0 {
			code = "partial_failure"
		}
		return m.fail("remove", fmt.Sprintf("Some objects of the old storage could not be deleted: %s.%s Run Remove old storage again to continue.%s", strings.Join(d.failed, "; "), stage, kube.TemplateHint(d.forbidden)), code, "delete failed")
	}
	// deleteAll deletes objects in order and stops at the first failure, so
	// the state a run leaves is one legacyRemovalPhases describes.
	deleteAll := func(objects []legacyObjectName) {
		for _, o := range objects {
			var logs []string
			d.deleteOwnedObject(c, LegacyNamespace, o.kind, o.name, &logs)
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
		return stopped(fmt.Sprintf(" The server's Deployment in namespace %s was not deleted.", LegacyNamespace))
	}
	// Phase 2: the server.
	deleteAll(legacyRemovalPhases.server)
	if len(d.failed) > 0 {
		return stopped("")
	}
	// Phase 3: its PVCs (and with them its pods), then its NetworkPolicies.
	if len(d.pvcUIDs) > 0 {
		remaining, err := WaitPVCsGone(c, d.pvcUIDs)
		if err != nil {
			return m.fail("remove", fmt.Sprintf("The old storage's server was deleted, but whether its PVCs are gone cannot be checked: %v. Run Remove old storage again to continue.%s", err, kube.TemplateHint(err)), kube.FirstNonEmpty(kube.ErrorCodeFromK8sErr(err), "in_progress"), "PVC wait failed")
		}
		if len(remaining) > 0 {
			return m.fail("remove", fmt.Sprintf("The old storage's server was deleted, but PVC %s %s still terminating after %s. Run Remove old storage again to continue.", strings.Join(remaining, ", "), kube.Verb(len(remaining), "is", "are"), s3DeleteTimeout), "in_progress", "PVCs terminating")
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
		msg = fmt.Sprintf("The rest of the old storage in namespace %s was removed. Its data volumes were already being deleted, so nothing was copied this time.", LegacyNamespace)
	} else {
		msg = fmt.Sprintf("The old storage in namespace %s was removed; its %s are in namespace %s.", LegacyNamespace, result.Summary(), S3Namespace)
	}
	if len(d.kept) > 0 {
		msg += fmt.Sprintf(" Left in place because this tool did not create them: %s.", strings.Join(d.kept, ", "))
	}
	msg += " " + nsMsg
	slog.Info("old s3 storage removed", "user", kube.GetUser(c), "deleted", strings.Join(d.deleted, ", "), "resumed", resume)
	record.Action(c, "migrate-storage", fmt.Sprintf("%s removed (%s)", LegacyNamespace, strings.TrimSuffix(nsMsg, ".")), true)
	return &types.OperationResponse{Success: true, Message: strings.TrimSpace(msg), Logs: m.logs}, nil
}

// SourceGoneMessage explains why the removal stops when the old storage
// cannot be copied from and its data PVCs (pending) are not being deleted.
func SourceGoneMessage(legacy *Legacy, why string, pending []string) string {
	msg := fmt.Sprintf("The old storage in namespace %s cannot be copied from (%s), and its data %s %s %s not being deleted, so %s may hold objects that were never copied. Nothing was deleted.",
		LegacyNamespace, why, kube.Verb(len(pending), "volume", "volumes"), strings.Join(pending, ", "), kube.Verb(len(pending), "is", "are"), kube.Verb(len(pending), "it", "they"))
	// A server whose data volume is being deleted cannot start again (a
	// new pod cannot use a PVC with a deletionTimestamp): only the manual
	// path is left then.
	if legacy.Server != "" && !legacy.Ready && !legacy.anyPVCTerminating() {
		deployment := map[string]string{"SeaweedFS": S3Deployment, "MinIO": legacyMinIODeployment}[legacy.Server]
		return msg + fmt.Sprintf(" Start the server again (Deployment %s), then run Remove old storage again.", deployment)
	}
	cmds := make([]string, 0, len(pending))
	for _, p := range pending {
		cmds = append(cmds, fmt.Sprintf("`oc delete pvc %s -n %s`", p, LegacyNamespace))
	}
	return msg + fmt.Sprintf(" A retry gives the same result. If you have checked that namespace %s holds everything you need, delete the %s yourself (%s), then run Remove old storage again to remove the rest.",
		S3Namespace, kube.Verb(len(pending), "volume", "volumes"), strings.Join(cmds, ", "))
}

// deleteLegacyDSPASecrets deletes the credentials Secret of the pipeline
// servers an earlier version named "dspa": they now use nightly-dspa-s3
// (repoint).
func (m *migrateRun) deleteLegacyDSPASecrets(dspas []DSPA, d *storageDeletion) {
	var logs []string
	for _, ds := range dspas {
		if ds.Meta.Name == LegacyDSPAName && ds.ManagedByTool() && ds.SecretName == DSPASecretName {
			var inProject []DSPA
			for _, o := range dspas {
				if o.Meta.Namespace == ds.Meta.Namespace {
					inProject = append(inProject, o)
				}
			}
			if note := deleteDSPASecret(m.c, ds.Meta.Namespace, &DSPA{Meta: kube.ObjectMeta{UID: "none"}, SecretName: LegacyDSPASecretName}, inProject, &logs); note != "" {
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
		return fmt.Sprintf("!Cannot read namespace %s, so it was kept: %v.%s", LegacyNamespace, err, kube.TemplateHint(err))
	case !found:
		return ""
	}
	owned, reason := LegacyNamespaceDeletable(meta)
	switch {
	case !requested && owned:
		m.step("namespace", "skipped", fmt.Sprintf("Namespace %s is kept, as chosen", LegacyNamespace))
		return fmt.Sprintf("Namespace %s was kept, as chosen; delete it with `oc delete project %s` once you have checked it holds nothing else.", LegacyNamespace, LegacyNamespace)
	case !owned:
		m.step("namespace", "skipped", reason)
		return reason
	case meta.Terminating():
		return fmt.Sprintf("Namespace %s is already being deleted.", LegacyNamespace)
	}
	m.step("namespace", "running", fmt.Sprintf("Deleting namespace %s", LegacyNamespace))
	if _, err := kube.DeleteWithUID(c, "/api/v1/namespaces/"+LegacyNamespace, meta.UID); err != nil && !kube.IsK8sNotFound(err) {
		// Nothing of the old storage is left, so a retry finds nothing to
		// remove and is not offered (readLegacyStorage returns nil).
		return fmt.Sprintf("!The old storage was removed, but namespace %s could not be deleted: %v.%s A retry finds nothing left to remove; delete the namespace with `oc delete project %s` once you have checked it holds nothing else.", LegacyNamespace, err, kube.TemplateHint(err), LegacyNamespace)
	}
	m.step("namespace", "success", fmt.Sprintf("OK: Namespace %s is being deleted, with everything else in it", LegacyNamespace))
	return fmt.Sprintf("Namespace %s is being deleted, with everything else in it.", LegacyNamespace)
}
