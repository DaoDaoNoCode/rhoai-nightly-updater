package record

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
)

// The caps of users' records (the non-admin design, decision D6 and
// amendment A4).
//
// The snapshot ConfigMap records what the updater made, by project, so it
// changes and deletes only its own objects. A project action run by a
// signed-in user who is not a cluster-admin (kube.Client.ProjectUser)
// writes its entries there with ActorRoleUser. Those entries are bounded,
// so users cannot fill the ConfigMap (1 MiB) and make the admins' changes
// fail: at most MaxUserRecordProjects projects hold users' entries, and at
// most MaxUserRecordProjectsPerUser of them one user's. A setup that would
// add a project beyond either cap is refused before it writes anything. A
// cluster-admin's entries (no actorRole, or ActorRoleAdmin) are outside
// both caps: an admin's setup is never refused by them.
const (
	MaxUserRecordProjects        = 100
	MaxUserRecordProjectsPerUser = 10
)

// The actorRole of a record entry. An entry without one was written by a
// cluster-admin: before project actions, only cluster-admins could change
// anything through the updater.
const (
	ActorRoleAdmin = "admin"
	ActorRoleUser  = "user"
)

// ActorRole is the actorRole of an entry written with c.
func ActorRole(c *kube.Client) string {
	if c.ProjectUser() {
		return ActorRoleUser
	}
	return ActorRoleAdmin
}

// UserRecordKeys are the snapshot keys whose entries project actions
// write, each a JSON object keyed by "<project>" or "<project>/<name>",
// whose values have "by" and "actorRole": the workbench and the
// playground (one entry per project each), projectStorages (project S3
// storage, the non-admin design section 4.5, one per project) and models
// (one per catalog model in the project, at most four). Each class has an
// entry ceiling of its own (ceilingOf); all of them share one byte budget
// (MaxUserRecordBytes).
var UserRecordKeys = []string{"workbenches", "playgrounds", "projectStorages", "models"}

// RecordOwner is one entry of UserRecordKeys: its project (and, for a
// user's entry, the project's namespace UID), who wrote it and with which
// role.
type RecordOwner struct {
	// Key is the snapshot key the entry is in (one of UserRecordKeys);
	// Name is the entry's own key there ("<project>" or
	// "<project>/<name>").
	Key        string
	Name       string
	Project    string
	ProjectUID string
	By         string
	ActorRole  string
	// Bytes is what the entry takes in the ConfigMap: its name and its
	// JSON value (0 for an entry not read from it).
	Bytes int
}

// key is what the caps count an entry by: its project's namespace UID, so
// a project deleted and made again under the same name is another project;
// the name for an entry without one.
func (o RecordOwner) key() string {
	if o.ProjectUID != "" {
		return "uid:" + o.ProjectUID
	}
	return "name:" + o.Project
}

// ReadRecordOwners reads the entries of UserRecordKeys from the snapshot
// ConfigMap (none when it is missing).
func ReadRecordOwners(c *kube.Client) ([]RecordOwner, error) {
	data, found, err := ReadSnapshotData(c)
	if err != nil {
		if kube.IsDecodeError(err) {
			return nil, fmt.Errorf("parse the snapshot ConfigMap: %w", err)
		}
		return nil, fmt.Errorf("read this tool's records: %w", err)
	}
	if !found {
		return nil, nil
	}
	var out []RecordOwner
	for _, key := range UserRecordKeys {
		raw := data[key]
		if raw == "" {
			continue
		}
		entries := map[string]json.RawMessage{}
		if err := json.Unmarshal([]byte(raw), &entries); err != nil {
			return nil, fmt.Errorf("parse this tool's records (%s): %w", key, err)
		}
		for k, v := range entries {
			var e struct {
				By         string `json:"by"`
				ActorRole  string `json:"actorRole"`
				ProjectUID string `json:"projectUID"`
			}
			if err := json.Unmarshal(v, &e); err != nil {
				return nil, fmt.Errorf("parse this tool's records (%s): %w", key, err)
			}
			project, _, _ := strings.Cut(k, "/")
			out = append(out, RecordOwner{Key: key, Name: k, Project: project, ProjectUID: e.ProjectUID, By: e.By, ActorRole: e.ActorRole, Bytes: entryBytes(k, v)})
		}
	}
	return out, nil
}

// ErrorCodeRecordsFull is the errorCode of a setup refused by a cap.
const ErrorCodeRecordsFull = "records_full"

// SweepInterval is how often the background check runs that drops the
// entries of projects that are gone (pkg/api's maasPruneInterval is set
// from it), named in the ceiling's refusal.
const SweepInterval = 5 * time.Minute

// The users' records budget. Every user's entry persisted counts, those
// of projects that are gone included (which the project caps do not
// count). One byte budget holds them all: MaxUserRecordBytes (500 KiB of
// the snapshot ConfigMap's 1 MiB, each entry's name and JSON value), which
// leaves more than 500 KiB to the cluster-admins' records; every write of a
// user's entry is checked against it, a growing one too (a model's token
// Secret UID). Each class also has an entry ceiling, the most its live
// entries reach in MaxUserRecordProjects (100) projects, and a size for
// one entry:
//   - workbenches and playgrounds: 200 (both keys in each project), at
//     most MaxUserRecordEntryBytes (2 KiB) each;
//   - projectStorages: 100 (one per project), at most
//     MaxUserProjectStorageEntryBytes (1 KiB) each;
//   - models: 400 (the four catalog models in each project), at most
//     MaxUserModelEntryBytes (1 KiB) each.
const (
	MaxUserRecordEntryBytes         = 2048
	MaxUserRecordEntries            = 200
	MaxUserProjectStorageEntryBytes = 1024
	MaxUserProjectStorageEntries    = 100
	MaxUserModelEntryBytes          = 1024
	MaxUserModelEntries             = 400
	MaxUserRecordBytes              = 500 << 10
)

// entryBytes is what an entry takes in its key's JSON object: its quoted
// name, the colon and comma, and its value.
func entryBytes(name string, value []byte) int { return len(name) + 4 + len(value) }

// userRecordCeiling is the hard ceiling of one class of users' entries:
// how many may be persisted, and how large one may be.
type userRecordCeiling struct {
	entries, bytes int
}

// ceilingOf is the ceiling of the entries of snapshot key key, and the
// keys that share it.
func ceilingOf(key string) (userRecordCeiling, []string) {
	switch key {
	case "projectStorages":
		return userRecordCeiling{MaxUserProjectStorageEntries, MaxUserProjectStorageEntryBytes}, []string{"projectStorages"}
	case "models":
		return userRecordCeiling{MaxUserModelEntries, MaxUserModelEntryBytes}, []string{"models"}
	}
	return userRecordCeiling{MaxUserRecordEntries, MaxUserRecordEntryBytes}, []string{"workbenches", "playgrounds"}
}

// RecordsFullError is a refusal by the caps of users' records.
type RecordsFullError struct{ Msg string }

func (e *RecordsFullError) Error() string { return e.Msg }

// ErrorCode is the errorCode of an error of a record write: records_full
// for a refusal by the caps, else the API error's.
func ErrorCode(err error) string {
	if _, ok := errors.AsType[*RecordsFullError](err); ok {
		return ErrorCodeRecordsFull
	}
	return kube.FirstNonEmpty(kube.ErrorCodeFromK8sErr(err), "internal")
}

// ErrorText says a record write's error to c's user: a refusal by the
// caps as it is; for a project user any other error by its HTTP status or
// category only, since it comes from a ServiceAccount request whose text
// may name what they may not see (logged here); for a cluster-admin the
// error.
func ErrorText(c *kube.Client, err error) string {
	if full, ok := errors.AsType[*RecordsFullError](err); ok {
		return full.Msg
	}
	if c.ProjectUser() {
		kube.LogFactReadFailure("a record write of a project user's action failed", "snapshot", err)
		return "this tool's record could not be written (" + kube.FactReadProblem(err) + ")"
	}
	return err.Error()
}

// UserRecordCapRefusal says why entry (a user's, about to be written)
// must not be added, "" when it may. owners are the recorded entries
// (ReadRecordOwners), the entry's own old value included. A project that
// already holds a user's entry adds nothing to the total, nor one of the
// same user's to theirs. exists reports whether a counted entry's project
// still exists (by name and, when recorded, namespace UID); it is asked
// only when a cap would refuse, and a project that is gone is not counted.
// An error of exists refuses (fail closed).
func UserRecordCapRefusal(owners []RecordOwner, entry RecordOwner, exists func(RecordOwner) (bool, error)) (string, error) {
	all, mine := map[string]RecordOwner{}, map[string]RecordOwner{}
	for _, o := range owners {
		if o.ActorRole != ActorRoleUser {
			continue
		}
		all[o.key()] = o
		if o.By == entry.By {
			mine[o.key()] = o
		}
	}
	k := entry.key()
	_, inMine := mine[k]
	_, inAll := all[k]
	if inMine || inAll && len(mine) < MaxUserRecordProjectsPerUser {
		return "", nil
	}
	live := func(set map[string]RecordOwner) (int, error) {
		keys := make([]string, 0, len(set))
		for key := range set {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		n := 0
		for _, key := range keys {
			ok, err := exists(set[key])
			if err != nil {
				return 0, err
			}
			if ok {
				n++
			}
		}
		return n, nil
	}
	if len(mine) >= MaxUserRecordProjectsPerUser {
		n, err := live(mine)
		if err != nil {
			return "", err
		}
		if n >= MaxUserRecordProjectsPerUser {
			return fmt.Sprintf("You have this tool's objects in %d projects, the most it keeps for one user. Remove them from a project you no longer need (Delete on its Environment page), then set up again.", n), nil
		}
	}
	if !inAll && len(all) >= MaxUserRecordProjects {
		n, err := live(all)
		if err != nil {
			return "", err
		}
		if n >= MaxUserRecordProjects {
			return fmt.Sprintf("The updater's records of users' objects are full (%d projects). A cluster-admin runs Clean up on the Environment page to remove what is no longer needed.", n), nil
		}
	}
	return "", nil
}

// AdmitUserEntry is AdmitUserEntryFor of an entry of the workbenches or
// playgrounds key.
func AdmitUserEntry(c *kube.Client, project string, prior *RecordOwner, entryJSON []byte) error {
	return AdmitUserEntryFor(c, "", project, prior, entryJSON)
}

// AdmitUserEntryFor decides whether c (a project user's client) may write
// its entry of project in snapshot key key ("": workbenches or
// playgrounds), whose JSON is entryJSON (nil before the entry is
// made: the early check of a setup), where prior is the entry's old value
// (nil: none). A cluster-admin's entry is always admitted. An entry that
// stays the same user's in the same project needs no admission; a new
// one, or one that changes hands (a cluster-admin's or another user's
// entry taken over), is counted against the caps. Called inside
// UpdateSnapshot it makes the admission and the write one step for this
// process. A refusal is a *RecordsFullError; a failed read is an error
// whose text names only the read's HTTP status (the ServiceAccount's error
// is logged, not returned).
func AdmitUserEntryFor(c *kube.Client, key, project string, prior *RecordOwner, entryJSON []byte) error {
	return AdmitUserEntryNamed(c, key, "", project, prior, entryJSON)
}

// AdmitUserEntryNamed is AdmitUserEntryFor of the entry named name in key
// ("" for an entry named by its project, one per project). The byte
// budget (MaxUserRecordBytes) is checked on every write of a user's entry,
// one that stays theirs too (it may grow): the entry's old value, if any,
// is replaced by its new name and value.
func AdmitUserEntryNamed(c *kube.Client, key, name, project string, prior *RecordOwner, entryJSON []byte) error {
	if !c.ProjectUser() {
		return nil
	}
	ceiling, class := ceilingOf(key)
	if len(entryJSON) > ceiling.bytes {
		return &RecordsFullError{Msg: fmt.Sprintf("This tool's record of the setup would take %d bytes, more than the %d it keeps for one user's entry.", len(entryJSON), ceiling.bytes)}
	}
	entry := RecordOwner{Project: project, ProjectUID: c.ProjectUID(), By: kube.GetUser(c), ActorRole: ActorRoleUser}
	stays := prior != nil && prior.ActorRole == ActorRoleUser && prior.By == entry.By && prior.ProjectUID == entry.ProjectUID
	if stays && entryJSON == nil {
		return nil
	}
	owners, err := ReadRecordOwners(c)
	if err != nil {
		kube.LogFactReadFailure("cannot read this tool's records for the users' caps", "snapshot", err)
		return fmt.Errorf("cannot read this tool's records (%s)", kube.FactReadProblem(err))
	}
	if entryJSON != nil {
		name = kube.FirstNonEmpty(name, project)
		size := entryBytes(name, entryJSON)
		for _, o := range owners {
			replaced := slices.Contains(class, o.Key) && (o.Key == key || key == "") && o.Name == name
			if o.ActorRole == ActorRoleUser && !replaced {
				size += o.Bytes
			}
		}
		if size > MaxUserRecordBytes {
			return &RecordsFullError{Msg: fmt.Sprintf("The updater's records of users' objects are full (%d KiB of %d). Entries of deleted projects are removed by the updater's background check, which runs every %d minutes when no other operation runs; set up again after it. A cluster-admin can free entries now by deleting what is no longer needed (Clean up on the Environment page).", size>>10, MaxUserRecordBytes>>10, int(SweepInterval/time.Minute))}
		}
	}
	if stays {
		return nil
	}
	// The hard ceiling: every user's entry of the same class counts, stale
	// ones included, when this write adds one.
	if prior == nil || prior.ActorRole != ActorRoleUser {
		n := 0
		for _, o := range owners {
			if o.ActorRole == ActorRoleUser && slices.Contains(class, o.Key) {
				n++
			}
		}
		if n >= ceiling.entries {
			return &RecordsFullError{Msg: fmt.Sprintf("The updater's records of users' objects are full (%d entries). Entries of deleted projects are removed by the updater's background check, which runs every %d minutes when no other operation runs; set up again after it. A cluster-admin can free entries now by deleting workbenches and playgrounds no longer needed (Clean up on the Environment page).", n, int(SweepInterval/time.Minute))}
		}
	}
	msg, err := UserRecordCapRefusal(owners, entry, func(o RecordOwner) (bool, error) { return ProjectExists(c, o) })
	if err != nil {
		kube.LogFactReadFailure("cannot read whether a recorded project exists", "Namespace", err)
		return fmt.Errorf("cannot check this tool's records (%s)", kube.FactReadProblem(err))
	}
	if msg != "" {
		return &RecordsFullError{Msg: msg}
	}
	return nil
}

// ProjectExists reports whether o's project still exists: its namespace,
// read with the facts client (the ServiceAccount's get on namespaces), and,
// when o records one, with the same UID. Only to tell whether a recorded
// project still counts in the caps, never shown.
func ProjectExists(c *kube.Client, o RecordOwner) (bool, error) {
	uid, found, err := namespaceUID(c.Facts(), o.Project)
	if err != nil || !found {
		return false, err
	}
	return o.ProjectUID == "" || o.ProjectUID == uid, nil
}

// namespaceUID reads project's namespace with f, the facts client.
func namespaceUID(f *kube.Client, project string) (string, bool, error) {
	var ns struct {
		Metadata kube.ObjectMeta `json:"metadata"`
	}
	found, err := f.GetIntoOptional("/api/v1/namespaces/"+project, &ns)
	if err != nil {
		return "", false, err
	}
	return ns.Metadata.UID, found, nil
}

// snapshotWriter serializes this process's read-change-write cycles of
// the snapshot ConfigMap's keys (UpdateSnapshot): project actions in
// different projects run at once, and each record key is applied whole.
var snapshotWriter = make(podWriter, 1)
