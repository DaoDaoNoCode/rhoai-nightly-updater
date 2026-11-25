package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Per-project keys: for every pipeline server this app tracks, it creates a
// dedicated SeaweedFS IAM user, rnu-dspa-<project>, whose inline policy
// limits calls to those Data Science Pipelines issues against objects in
// bucket pipelines. That user's access key is stored in two project Secrets
// (nightly-dspa-s3 and the dashboard Connection); pipeline projects never
// receive the storage admin key.
//
// Legacy installs used IAM read-only ("weed mini" default) and distributed
// the admin key across pipeline projects. Secure S3 storage (SecureS3)
// transitions to hardened mode: redeploy with a fresh admin key and IAM
// writes enabled, then issue a scoped key per tracked pipeline server. It is
// not a standalone operation. Bucket policy scope remains whole-bucket: one
// project's credentials can still read and write another project's artifacts
// (SECURITY.md, "S3 storage keys").
const (
	// ScopedUserPrefix is the leading segment of each IAM user name this tool
	// creates for a pipeline server. ListUsers surfaces the fixed admin account
	// as admin-<first 8 of its key>; the prefix distinguishes those users.
	ScopedUserPrefix = "rnu-dspa-"
	// scopedPolicyName is the inline policy identifier attached to the user.
	scopedPolicyName = "rnu-pipelines"
	// scopedNamespaceTag records the project on the IAM user for operators
	// auditing users (AWS IAM disallows commas in tag values, so several names
	// cannot be comma-delimited).
	scopedNamespaceTag = "rnu-namespace"
	// On a pipeline server, DSPAS3UserAnnotation and DSPAS3KeyAnnotation record
	// the SeaweedFS user and key ID. They are triggers, not authoritative state
	// (Secrets hold live keys): patching them updates the DSPA, prompting the
	// data-science-pipelines operator to reconcile and re-verify object storage
	// with the new credentials. Updating a Secret by itself is insufficient (the
	// operator only watches Secrets it owns).
	DSPAS3UserAnnotation = "rhoai-nightly-updater/s3-user"
	DSPAS3KeyAnnotation  = "rhoai-nightly-updater/s3-key-id"
	// Secured storage passes two "weed mini" toggles
	// (weed/command/mini.go at 4.54): enable IAM writes (defaults to read-only)
	// so the admin key may create users; disable self-service so scoped users
	// cannot rotate their own keys (self-service defaults on).
	S3IAMWritesArg   = "-s3.iam.readOnly=false"
	S3SelfServiceArg = "-s3.iam.selfServiceRequiresGrant=true"
	// s3ProbePrefix is the key prefix for objects created and removed during key probes.
	s3ProbePrefix = "rhoai-nightly-updater-probe/"
	// dspoProbeObject is what the data-science-pipelines operator's connection
	// check StatObjects (controllers/storage.go ConnectAndQueryObjStore,
	// rhoai-3.7); the probe succeeds when the object is missing (NoSuchKey).
	dspoProbeObject = "some-random-object"
)

// ScopedPolicy is the pipeline-server user's inline IAM document: ListBucket
// plus Get/Put/DeleteObject on bucket contents. Tagging, ACL, and bucket-level
// APIs are omitted because KFP never needs them.
const ScopedPolicy = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:ListBucket","s3:GetObject","s3:PutObject","s3:DeleteObject"],"Resource":["arn:aws:s3:::pipelines","arn:aws:s3:::pipelines/*"]}]}`

// s3ToolEndpoint resolves the base URL for this tool's S3 and IAM traffic
// (S3BucketEndpoint); tests may override it.
var s3ToolEndpoint = func() string { return S3BucketEndpoint(os.Getenv) }

// ScopedUserName builds the IAM user label for the pipeline server in
// project. AWS caps user names at 64 runes; longer projects get a truncated
// name plus a short project hash (the tag still stores the full project).
func ScopedUserName(project string) string {
	name := ScopedUserPrefix + project
	if len(name) <= 64 {
		return name
	}
	// "." cannot be in a namespace name, so no project's own name can end
	// up as another project's hashed one.
	sum := sha256.Sum256([]byte(project))
	return name[:47] + "." + hex.EncodeToString(sum[:8])
}

// ArgsSecured is true when the "weed mini" argument list enables IAM writes and disables self-service.
func ArgsSecured(args []string) bool {
	return slices.Contains(args, S3IAMWritesArg) && slices.Contains(args, S3SelfServiceArg)
}

// s3SetupKeys returns the keys a setup (or Repair) writes and whether it
// applies the secured args. Without a storage Deployment it is a first
// setup, or one that stopped after writing the Secret: it is secured, and
// a key stored before (a stopped first setup's, or one a teardown that did
// not finish left behind, which pipeline projects may hold) is replaced by
// a new one. An existing Deployment without both IAM flags is storage an
// earlier version set up: Repair keeps it as it is (its stored key may be
// in pipeline projects), and only Secure S3 storage moves it, with a new
// key. Otherwise the stored keys are kept (S3Credentials).
func s3SetupKeys(c *kube.Client) (keys S3Keys, secured, rotated bool, err error) {
	body, exists, err := c.GetOptional(rhoai.NamespacedPath("apps/v1", "deployments", S3Namespace, S3Deployment))
	if err != nil {
		return S3Keys{}, false, false, err
	}
	if !exists {
		_, found, err := ReadS3Keys(c)
		if err != nil {
			return S3Keys{}, false, false, err
		}
		if found {
			keys, err = newAdminKeys(c)
		} else {
			keys, err = S3Credentials(c)
		}
		return keys, true, found, err
	}
	var deploy S3DeploymentSpec
	if err := json.Unmarshal(body, &deploy); err != nil {
		return S3Keys{}, false, false, fmt.Errorf("parse Deployment %s/%s: %w", S3Namespace, S3Deployment, err)
	}
	keys, err = S3Credentials(c)
	return keys, deploy.Secured(), false, err
}

// storageSecured reads the storage's Deployment: false when it is missing.
func storageSecured(c *kube.Client) (bool, error) {
	body, exists, err := c.GetOptional(rhoai.NamespacedPath("apps/v1", "deployments", S3Namespace, S3Deployment))
	if err != nil || !exists {
		return false, err
	}
	var deploy S3DeploymentSpec
	if err := json.Unmarshal(body, &deploy); err != nil {
		return false, fmt.Errorf("parse Deployment %s/%s: %w", S3Namespace, S3Deployment, err)
	}
	return deploy.Secured(), nil
}

// deleteProjectKey removes the project's pipeline-server SeaweedFS user
// (keys first, then the account) when no other pipeline server remains in
// the namespace (any surviving DSPA—not the one being removed—may still rely
// on the Secret bearing this user's credentials; otherwise the account is a
// leftover cleared via Remove leftover keys). Users tied to another project
// (per userProject) are left alone. Such accounts exist only under secured storage.
// On failure to delete, a message is returned for the API response.
func deleteProjectKey(c *kube.Client, project string, target *DSPA, all []DSPA, logs *[]string) string {
	user := ScopedUserName(project)
	secured, err := storageSecured(c)
	if err == nil && !secured {
		return ""
	}
	var sw *seaweedClient
	if err == nil {
		sw, err = adminSeaweed(c)
	}
	var tags map[string]string
	exists := false
	if err == nil {
		tags, exists, err = sw.ListUserTags(user)
	}
	if err == nil && !exists {
		return ""
	}
	if err == nil {
		for _, d := range all {
			if (target == nil || d.Meta.UID != target.Meta.UID) && !d.Meta.Terminating() {
				*logs = append(*logs, fmt.Sprintf("Kept SeaweedFS user %s: pipeline server %s remains in %s", user, d.Meta.Name, project))
				return ""
			}
		}
		// A tag naming another project, hashed name or not: that project's.
		if owner := tags[scopedNamespaceTag]; owner != "" && owner != project {
			*logs = append(*logs, fmt.Sprintf("Kept SeaweedFS user %s: it belongs to project %s", user, owner))
			return ""
		}
		err = deleteScopedUser(sw, project)
	}
	if err != nil {
		*logs = append(*logs, fmt.Sprintf("Warning: cannot delete SeaweedFS user %s: %v", user, err))
		return fmt.Sprintf(" Its S3 key (SeaweedFS user %s) could not be deleted: %v. Run Delete again to retry.", user, err)
	}
	*logs = append(*logs, fmt.Sprintf("OK: SeaweedFS user %s and its keys deleted", user))
	return ""
}

// newAdminKeys generates a fresh admin access/secret pair while preserving the admin UI password.
func newAdminKeys(c *kube.Client) (S3Keys, error) {
	suffix, err := randomHex(6)
	if err != nil {
		return S3Keys{}, fmt.Errorf("generate S3 access key: %w", err)
	}
	secret, err := randomHex(16)
	if err != nil {
		return S3Keys{}, fmt.Errorf("generate S3 secret key: %w", err)
	}
	return withAdminPassword(c, S3Keys{AccessKey: "seaweedfs-" + suffix, SecretKey: secret})
}

// adminSeaweed builds a client authenticated with the storage admin key from the cluster Secret.
func adminSeaweed(c *kube.Client) (*seaweedClient, error) {
	keys, found, err := ReadS3Keys(c)
	if err != nil {
		return nil, err
	}
	if !found || keys.AccessKey == "" || keys.SecretKey == "" {
		return nil, fmt.Errorf("no S3 keys in Secret %s/%s", S3Namespace, S3Secret)
	}
	return newSeaweedClient(c.Context(), s3ToolEndpoint(), keys), nil
}

// provisionScopedKey ensures the IAM user for project exists (policy + tag)
// and yields credentials for its pipeline server: reuse current when still
// registered on the user; otherwise mint a new pair (fresh) after revoking
// stale keys (interrupted runs may leave keys that never reached a Secret).
func provisionScopedKey(sw *seaweedClient, project string, current S3Keys) (keys S3Keys, fresh bool, err error) {
	user := ScopedUserName(project)
	return provisionBucketUser(sw, bucketUser{name: user, policyName: scopedPolicyName, policy: ScopedPolicy, tagKey: scopedNamespaceTag, tagValue: project,
		// If the user's tag references a different project (even when names
		// hash to the same value), treat that project as owner and skip
		// re-key/re-tag.
		foreign: func(other string) error {
			return fmt.Errorf("SeaweedFS user %s belongs to project %s, not %s; nothing was changed. Delete that user in the SeaweedFS admin UI if project %s no longer needs it, or set up the pipeline server in a project with another name", user, other, project, other)
		}}, current)
}

// bucketUser is a SeaweedFS IAM user this tool makes for one use of one
// bucket: its inline policy, and the tag that says whose it is.
type bucketUser struct {
	name, policyName, policy, tagKey, tagValue string
	// mustCreate: the user must not exist yet (its name was just drawn for
	// this setup); one that does is refused, never adopted.
	mustCreate bool
	// foreign is the error for a user whose tag names another owner.
	foreign func(other string) error
}

// provisionBucketUser ensures the user exists with its policy and tag, and
// yields its key: current when it is still registered on the user, else a
// new pair (fresh) after revoking the old ones (an interrupted run may leave
// keys that never reached a Secret).
func provisionBucketUser(sw *seaweedClient, u bucketUser, current S3Keys) (keys S3Keys, fresh bool, err error) {
	created, err := sw.createUser(u.name)
	if err != nil {
		return S3Keys{}, false, fmt.Errorf("create SeaweedFS user %s: %w", u.name, err)
	}
	tags, _, err := sw.ListUserTags(u.name)
	if err != nil {
		return S3Keys{}, false, fmt.Errorf("read the tags of SeaweedFS user %s: %w", u.name, err)
	}
	if other := tags[u.tagKey]; (other != "" && other != u.tagValue) || (u.mustCreate && !created) {
		return S3Keys{}, false, u.foreign(other)
	}
	if err := sw.TagUser(u.name, u.tagKey, u.tagValue); err != nil {
		return S3Keys{}, false, fmt.Errorf("tag SeaweedFS user %s: %w", u.name, err)
	}
	if err := sw.PutUserPolicy(u.name, u.policyName, u.policy); err != nil {
		return S3Keys{}, false, fmt.Errorf("set the policy of SeaweedFS user %s: %w", u.name, err)
	}
	ids, _, err := sw.ListAccessKeys(u.name)
	if err != nil {
		return S3Keys{}, false, fmt.Errorf("list the keys of SeaweedFS user %s: %w", u.name, err)
	}
	if current.AccessKey != "" && current.SecretKey != "" && slices.Contains(ids, current.AccessKey) {
		return current, false, nil
	}
	for _, id := range ids {
		if err := sw.DeleteAccessKey(u.name, id); err != nil {
			return S3Keys{}, false, fmt.Errorf("delete an old key of SeaweedFS user %s: %w", u.name, err)
		}
	}
	keys, err = sw.CreateAccessKey(u.name)
	if err != nil {
		return S3Keys{}, false, fmt.Errorf("create a key for SeaweedFS user %s: %w", u.name, err)
	}
	return keys, true, nil
}

// deleteScopedUser removes project's IAM user after clearing access keys
// (SeaweedFS blocks DeleteUser until keys are gone), then drops policy and
// tags. Absent users are treated as success.
func deleteScopedUser(sw *seaweedClient, project string) error {
	user := ScopedUserName(project)
	ids, found, err := sw.ListAccessKeys(user)
	if err != nil {
		return fmt.Errorf("list the keys of SeaweedFS user %s: %w", user, err)
	}
	if !found {
		return nil
	}
	for _, id := range ids {
		if err := sw.DeleteAccessKey(user, id); err != nil {
			return fmt.Errorf("delete a key of SeaweedFS user %s: %w", user, err)
		}
	}
	if err := sw.DeleteUser(user); err != nil {
		return fmt.Errorf("delete SeaweedFS user %s: %w", user, err)
	}
	return nil
}

// deleteScopedUsers tears down all tool-created IAM users (ScopedUserPrefix),
// revoking keys before each user. Returned slice lists removed user names.
func deleteScopedUsers(sw *seaweedClient) ([]string, error) {
	users, err := sw.ListUsers()
	if err != nil {
		return nil, err
	}
	var deleted []string
	for _, u := range users {
		if !strings.HasPrefix(u, ScopedUserPrefix) {
			continue
		}
		ids, _, err := sw.ListAccessKeys(u)
		if err != nil {
			return deleted, err
		}
		for _, id := range ids {
			if err := sw.DeleteAccessKey(u, id); err != nil {
				return deleted, err
			}
		}
		if err := sw.DeleteUser(u); err != nil {
			return deleted, err
		}
		deleted = append(deleted, u)
	}
	return deleted, nil
}

// probeScopedKey validates a freshly minted key against storage prior to
// writing Secrets: mirror the data-science-pipelines operator probe (HEAD
// some-random-object expects 404), then list, put, get, and delete an object
// under s3ProbePrefix.
func probeScopedKey(ctx context.Context, keys S3Keys, user string) error {
	sc := newSeaweedClient(ctx, s3ToolEndpoint(), keys)
	sc.http = s3SlowHTTPClient
	status, _, err := sc.object(http.MethodHead, s3Bucket, dspoProbeObject, nil, nil)
	if err != nil {
		return err
	}
	if status != http.StatusNotFound {
		return fmt.Errorf("the data-science-pipelines operator's check (HEAD %s/%s) answers HTTP %d instead of 404", s3Bucket, dspoProbeObject, status)
	}
	suffix, err := randomHex(4)
	if err != nil {
		return err
	}
	key := s3ProbePrefix + user + "-" + suffix
	content := []byte("rhoai-nightly-updater key check")
	steps := []struct {
		what, method, key string
		query             url.Values
		body              []byte
		ok                []int
	}{
		{"write a test object", http.MethodPut, key, nil, content, []int{http.StatusOK}},
		{"list the bucket", http.MethodGet, "", url.Values{"list-type": {"2"}, "prefix": {s3ProbePrefix}, "max-keys": {"1"}}, nil, []int{http.StatusOK}},
		{"read the test object", http.MethodGet, key, nil, nil, []int{http.StatusOK}},
		{"delete the test object", http.MethodDelete, key, nil, nil, []int{http.StatusOK, http.StatusNoContent}},
	}
	for _, s := range steps {
		status, body, err := sc.object(s.method, s3Bucket, s.key, s.query, s.body)
		if err != nil {
			return fmt.Errorf("%s: %w", s.what, err)
		}
		if !slices.Contains(s.ok, status) {
			return fmt.Errorf("%s: %w", s.what, parseServiceError(status, body))
		}
		if s.method == http.MethodGet && s.key == key && string(body) != string(content) {
			return fmt.Errorf("%s: it reads back other content", s.what)
		}
	}
	return nil
}

// projectS3Secret wraps one of the two pipeline-project Secrets that store S3 credentials.
type projectS3Secret struct {
	meta  kube.ObjectMeta
	found bool
	keys  S3Keys
}

// readProjectS3Secret loads name (DSPASecretName or ConnectionSecretName) from project and decodes its keys.
func readProjectS3Secret(c *kube.Client, project, name string) (projectS3Secret, error) {
	var secret struct {
		Metadata kube.ObjectMeta   `json:"metadata"`
		Data     map[string]string `json:"data"`
	}
	found, err := c.GetIntoOptional(projectSecretPath(project, name), &secret)
	if err != nil {
		return projectS3Secret{}, err
	}
	if !found {
		return projectS3Secret{}, nil
	}
	return projectS3Secret{meta: secret.Metadata, found: true, keys: S3Keys{
		AccessKey: DecodeBase64Field(secret.Data[S3AccessKeyField]), SecretKey: DecodeBase64Field(secret.Data[S3SecretKeyField]),
	}}, nil
}

// connectionKeyID reads the access key ID in project's dashboard
// Connection; found is false when it does not exist.
func connectionKeyID(c *kube.Client, project string) (id string, found bool, err error) {
	var secret struct {
		Data map[string]string `json:"data"`
	}
	found, err = c.GetIntoOptional(rhoai.NamespacedPath("v1", "secrets", project, ConnectionSecretName), &secret)
	if err != nil || !found {
		return "", found, err
	}
	return DecodeBase64Field(secret.Data[S3AccessKeyField]), true, nil
}

// keyServers partitions pipeline servers on storage endpoint ep into
// candidates for scoped keys (owned, recorded, object storage this tool
// manages) versus servers that retain existing credentials, with reasons
// for the latter.
func keyServers(ep S3Endpoints, dspas []DSPA, rec PipelineRecord) (recorded []DSPA, kept []string) {
	for _, d := range dspas {
		if d.Meta.Terminating() || d.Meta.Namespace == S3Namespace || !ep.Address.UsedBy(d.Meta.Namespace, d.Host) {
			continue
		}
		name := d.Meta.Namespace + "/" + d.Meta.Name
		why, ok := Repointable(d)
		switch {
		case ok && rec.Owns(d.Meta):
			recorded = append(recorded, d)
		case ok:
			// It may be this tool's (set up before 3.0.1, or its record
			// could not be written), but nothing proves it.
			kept = append(kept, name+" (not recorded by this tool)")
		case why == "not created by this tool":
			kept = append(kept, name+" (not set up by this tool)")
		default:
			kept = append(kept, name+" ("+why+")")
		}
	}
	sort.Slice(recorded, func(i, j int) bool {
		return recorded[i].Meta.Namespace+"/"+recorded[i].Meta.Name < recorded[j].Meta.Namespace+"/"+recorded[j].Meta.Name
	})
	sort.Strings(kept)
	return recorded, kept
}

// KeysState summarizes S3 key posture for status output: secured flag plus
// which pipeline servers run on dedicated credentials. A server counts as
// own-key when the access key ID in its project's Connection
// (rhoai-nightly-updater-s3, which every write of the keys writes after
// nightly-dspa-s3) matches a live key on its IAM user in SeaweedFS (DSPA
// annotations are ignored here). The status reads the Connection, not
// nightly-dspa-s3: the ServiceAccount may get the Connection by name
// already and gets no new grant for this. Unsecured storage marks every
// recorded server on the legacy key without live lookups. Returns nil when
// storage is foreign, absent, or pending migration.
func KeysState(c *kube.Client, state types.ResourceState, ep S3Endpoints, dspas []DSPA, rec PipelineRecord) *types.S3KeysState {
	if !state.Deployed || !state.ManagedByTool || state.Terminating {
		return nil
	}
	recorded, kept := keyServers(ep, dspas, rec)
	ks := &types.S3KeysState{Secured: ep.Secured, KeepOldKey: kept, Users: map[string]string{}}
	for _, d := range recorded {
		ks.Users[d.Meta.Namespace+"/"+d.Meta.Name] = ScopedUserName(d.Meta.Namespace)
	}
	if !ep.Secured {
		for _, d := range recorded {
			ks.OldKey = append(ks.OldKey, d.Meta.Namespace+"/"+d.Meta.Name)
		}
		ks.NeedsSecuring = true
	}
	if !state.Ready {
		// No live lookups against storage that is not running.
		ks.SecureBlockedReason = "The S3 storage is not ready."
		return ks
	}
	if !ep.Secured {
		return ks
	}
	sw, err := adminSeaweed(c)
	if err != nil {
		ks.CheckError = "Cannot check which key each pipeline server uses: " + err.Error()
		return ks
	}
	// One SeaweedFS request per server at most; the first SeaweedFS failure
	// ends the lookups (a storage that does not answer must not hold the
	// status up per server). A Kubernetes read error skips only its server.
	for _, d := range recorded {
		name := d.Meta.Namespace + "/" + d.Meta.Name
		keyID, found, err := connectionKeyID(c, d.Meta.Namespace)
		if err != nil {
			ks.CheckError = fmt.Sprintf("Cannot check which key %s uses: %v", name, err)
			continue
		}
		if !found {
			// Deleted in the dashboard: its pipeline Secret may still hold
			// the old key. Secure S3 storage writes it again.
			ks.OldKey = append(ks.OldKey, name)
			continue
		}
		ids, _, err := sw.ListAccessKeys(ScopedUserName(d.Meta.Namespace))
		if err != nil {
			ks.CheckError = fmt.Sprintf("Cannot check which key %s uses: %v", name, err)
			break
		}
		// Its own key, and the run that gave it finished: the DSPA names the
		// key (an annotation is compared for the status only; it never
		// decides who gets a key).
		if keyID != "" && slices.Contains(ids, keyID) && d.Meta.Annotations[DSPAS3KeyAnnotation] == keyID {
			ks.OwnKey = append(ks.OwnKey, name)
		} else {
			ks.OldKey = append(ks.OldKey, name)
		}
	}
	if ks.CheckError == "" {
		// Conservative: populate leftovers only if all reads succeeded.
		if left, err := leftoverUsers(sw, dspas); err == nil {
			for _, l := range left {
				ks.LeftoverUsers = append(ks.LeftoverUsers, l.user)
				ks.Leftovers = append(ks.Leftovers, types.S3LeftoverKey{User: l.user, Project: l.project, ConnectionInUse: connectionHoldsKeyOf(c, sw, l)})
			}
		}
	}
	ks.NeedsSecuring = len(ks.OldKey) > 0
	return ks
}

// connectionHoldsKeyOf is true when the leftover project still has a
// dashboard Connection referencing one of that user's keys—dropping the user
// would strand workbenches and deployments on that credential. Errors are
// treated as true so the UI surfaces a warning before confirm.
func connectionHoldsKeyOf(c *kube.Client, sw *seaweedClient, l leftover) bool {
	keyID, found, err := connectionKeyID(c, l.project)
	if err != nil {
		return true
	}
	if !found || keyID == "" {
		return false
	}
	ids, _, err := sw.ListAccessKeys(l.user)
	return err != nil || slices.Contains(ids, keyID)
}

// leftover is a SeaweedFS user of this tool whose project has no
// pipeline server at all.
type leftover struct{ user, project string }

// userProject returns the project a user of this tool belongs to. A name
// that is not hashed (ScopedUserName) is its project's; a hashed name's
// project is its scopedNamespaceTag, and only when that project hashes to
// this name. known is false when that cannot be told (no tag, or a tag
// that names another project than the name says): such a user is kept.
func userProject(sw *seaweedClient, user string) (project string, known bool, err error) {
	rest := strings.TrimPrefix(user, ScopedUserPrefix)
	if !strings.Contains(rest, ".") {
		tags, _, err := sw.ListUserTags(user)
		if err != nil {
			return "", false, err
		}
		if tag := tags[scopedNamespaceTag]; tag != "" && tag != rest {
			return tag, true, nil
		}
		return rest, true, nil
	}
	tags, found, err := sw.ListUserTags(user)
	if err != nil || !found {
		return "", false, err
	}
	project = tags[scopedNamespaceTag]
	if project == "" || ScopedUserName(project) != user {
		return "", false, nil
	}
	return project, true, nil
}

// leftoverUsers lists the users this tool made (ScopedUserPrefix) whose
// project (userProject) has no pipeline server at all (dspas: every DSPA
// in the cluster, whoever made it, recorded or not, on any storage; one
// being deleted does not count). A user whose project cannot be told is
// kept. Never the static admin. Any error: nothing is a leftover.
func leftoverUsers(sw *seaweedClient, dspas []DSPA) ([]leftover, error) {
	users, err := sw.ListUsers()
	if err != nil {
		return nil, err
	}
	live := map[string]bool{}
	liveNames := map[string]bool{}
	for _, d := range dspas {
		if !d.Meta.Terminating() {
			live[d.Meta.Namespace] = true
			liveNames[ScopedUserName(d.Meta.Namespace)] = true
		}
	}
	var out []leftover
	for _, u := range users {
		// A user named for a project that has a pipeline server needs no
		// tag lookup: it is not a leftover.
		if !strings.HasPrefix(u, ScopedUserPrefix) || liveNames[u] {
			continue
		}
		project, known, err := userProject(sw, u)
		if err != nil {
			return nil, err
		}
		if !known || live[project] || ScopedUserName(project) != u {
			continue
		}
		out = append(out, leftover{user: u, project: project})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].user < out[j].user })
	return out, nil
}

// deleteUsers deletes users by name: their keys, then the user. On an
// error it returns the names already deleted.
func deleteUsers(sw *seaweedClient, users []string) ([]string, error) {
	var deleted []string
	for _, u := range users {
		if !strings.HasPrefix(u, ScopedUserPrefix) {
			continue
		}
		ids, _, err := sw.ListAccessKeys(u)
		if err != nil {
			return deleted, err
		}
		for _, id := range ids {
			if err := sw.DeleteAccessKey(u, id); err != nil {
				return deleted, err
			}
		}
		if err := sw.DeleteUser(u); err != nil {
			return deleted, err
		}
		deleted = append(deleted, u)
	}
	return deleted, nil
}

// deleteConfirmedLeftovers deletes the users the user confirmed that are
// leftovers now: the pipeline servers are listed again here (under the
// caller's cluster lock), and right before each user is deleted its
// project's pipeline servers are read once more, so one created meanwhile
// keeps its user. Any read that fails, a DSPA API that answers 404
// included, stops it: nothing more is deleted. It returns the users
// deleted, also with an error.
func deleteConfirmedLeftovers(c *kube.Client, sw *seaweedClient, confirmed []string) ([]string, error) {
	if len(confirmed) == 0 {
		return nil, nil
	}
	dspas, err := ListDSPAs(c, "")
	if err != nil {
		return nil, fmt.Errorf("list the pipeline servers: %w", err)
	}
	fresh, err := leftoverUsers(sw, dspas)
	if err != nil {
		return nil, err
	}
	var deleted []string
	for _, l := range fresh {
		if !slices.Contains(confirmed, l.user) {
			continue
		}
		here, err := ListDSPAs(c, l.project)
		if err != nil {
			return deleted, fmt.Errorf("list the pipeline servers of %s: %w", l.project, err)
		}
		if slices.ContainsFunc(here, func(d DSPA) bool { return !d.Meta.Terminating() }) {
			continue
		}
		done, err := deleteUsers(sw, []string{l.user})
		deleted = append(deleted, done...)
		if err != nil {
			return deleted, err
		}
	}
	return deleted, nil
}

// RemoveLeftoverKeys deletes the users of pipeline servers that no longer
// exist (Remove leftover keys, an explicit action whose confirmation lists
// them): only those in confirmed that are still leftovers when read again
// here (deleteConfirmedLeftovers). Any read that fails deletes nothing
// more.
func RemoveLeftoverKeys(c *kube.Client, confirmed []string) (*types.OperationResponse, error) {
	fail := func(msg, code string) (*types.OperationResponse, error) {
		record.Action(c, "remove-leftover-s3-keys", "failed: "+msg, false)
		return &types.OperationResponse{Success: false, Message: msg, ErrorCode: code}, nil
	}
	if ok, err := storageSecured(c); err != nil || !ok {
		if err == nil {
			return fail("The S3 storage is not secured, so it has no keys of its own to remove. Nothing was changed.", "prerequisites")
		}
		return fail(fmt.Sprintf("Cannot read the S3 storage: %v. Nothing was changed.", err), kube.ErrorCodeFromK8sErr(err))
	}
	sw, err := adminSeaweed(c)
	if err != nil {
		return fail(fmt.Sprintf("Cannot read the admin key: %v. Nothing was changed.", err), kube.ErrorCodeFromK8sErr(err))
	}
	deleted, err := deleteConfirmedLeftovers(c, sw, confirmed)
	if err != nil {
		msg := fmt.Sprintf("Cannot remove the leftover keys: %v.", err)
		if len(deleted) > 0 {
			msg += fmt.Sprintf(" Removed before that: %s.", strings.Join(deleted, ", "))
		} else {
			msg += " Nothing was changed."
		}
		return fail(msg+" Run it again.", "s3_keys")
	}
	record.Action(c, "remove-leftover-s3-keys", strings.Join(deleted, ","), true)
	if len(deleted) == 0 {
		return &types.OperationResponse{Success: true, Message: "No leftover key to remove: their projects have a pipeline server again, or the keys are gone."}, nil
	}
	return &types.OperationResponse{Success: true, Message: fmt.Sprintf("Removed the keys of pipeline servers that no longer exist: %s.", strings.Join(deleted, ", "))}, nil
}

// rekeyServer assigns dedicated credentials to one recorded pipeline server:
// ensure IAM user/key (provisionScopedKey), optionally exercise the key
// (probeScopedKey), patch both project Secrets in place (resourceVersion-aware
// apply, DSPA ownership), and annotate the DSPA to force another storage
// check. Servers whose Secrets already match the scoped user are untouched
// (changed false). Failed probes abort before Secret mutation.
func rekeyServer(c *kube.Client, sw *seaweedClient, d DSPA, progress func(string)) (changed bool, err error) {
	if progress == nil {
		progress = func(string) {}
	}
	project := d.Meta.Namespace
	current := map[string]projectS3Secret{}
	for _, name := range []string{DSPASecretName, ConnectionSecretName} {
		s, err := readProjectS3Secret(c, project, name)
		if err != nil {
			return false, fmt.Errorf("read Secret %s/%s: %w", project, name, err)
		}
		if s.found && !s.meta.HasToolLabel() {
			return false, fmt.Errorf("secret %s/%s was not created by this tool", project, name)
		}
		current[name] = s
	}
	progress(fmt.Sprintf("Making sure of SeaweedFS user %s and its key", ScopedUserName(project)))
	keys, fresh, err := provisionScopedKey(sw, project, current[DSPASecretName].keys)
	if err != nil {
		return false, err
	}
	user := ScopedUserName(project)
	if !fresh && current[ConnectionSecretName].found && current[ConnectionSecretName].keys == keys {
		// Both Secrets hold its key; a run that stopped before the DSPA was
		// annotated is finished here.
		if d.Meta.Annotations[DSPAS3KeyAnnotation] == keys.AccessKey {
			return false, nil
		}
		progress("Asking the data-science-pipelines operator to check the storage again")
		if err := annotateDSPAKey(c, d, user, keys.AccessKey); err != nil {
			return true, fmt.Errorf("its Secrets have its key, but pipeline server %s/%s could not be annotated to check the storage again: %w", project, d.Meta.Name, err)
		}
		return true, nil
	}
	if fresh {
		progress("Checking the new key against the storage")
		if err := probeScopedKey(c.Context(), keys, user); err != nil {
			return false, fmt.Errorf("the new key of SeaweedFS user %s does not work (%w), so the project's Secrets were left unchanged", user, err)
		}
	}
	progress(fmt.Sprintf("Writing the key into Secrets %s and %s", DSPASecretName, ConnectionSecretName))
	for _, s := range []*projectSecret{
		{c: c, project: project, name: DSPASecretName, what: "secret", obj: dspaS3Secret(project, keys)},
		{c: c, project: project, name: ConnectionSecretName, what: "connection", obj: dashboardS3Connection(project, keys)},
	} {
		setDSPAOwner(s.obj, d.Meta.Name, d.Meta.UID)
		s.meta, s.found = current[s.name].meta, current[s.name].found
		if _, err := writeProjectSecret(s, projectSecretPath(project, s.name), envkit.WithToolFieldManager(rhoai.NamespacedPath("v1", "secrets", project, "")), false); err != nil {
			msg, _ := pipelineWriteFailure(fmt.Sprintf("secret %s", s.name), project, err)
			return true, fmt.Errorf("%s", strings.TrimSuffix(msg, "."))
		}
	}
	progress("Asking the data-science-pipelines operator to check the storage again")
	if err := annotateDSPAKey(c, d, user, keys.AccessKey); err != nil {
		return true, fmt.Errorf("its Secrets have the new key, but pipeline server %s/%s could not be annotated to check the storage again: %w", project, d.Meta.Name, err)
	}
	return true, nil
}

// annotateDSPAKey applies DSPAS3UserAnnotation and DSPAS3KeyAnnotation via
// merge patch (two annotation fields only, no resourceVersion: status churn
// from the operator would otherwise cause conflicts).
func annotateDSPAKey(c *kube.Client, d DSPA, user, keyID string) error {
	path, ok := toolDSPAPath(d.Meta.Namespace, d.Meta.Name)
	if !ok {
		return fmt.Errorf("pipeline server %s is not a name this tool creates", d.Meta.Name)
	}
	patch, _ := json.Marshal(map[string]any{"metadata": map[string]any{"annotations": map[string]any{DSPAS3UserAnnotation: user, DSPAS3KeyAnnotation: keyID}}})
	_, _, err := c.Do(http.MethodPatch, path, "application/merge-patch+json", patch, url.Values{"fieldManager": {envkit.ToolFieldManager}})
	return err
}
