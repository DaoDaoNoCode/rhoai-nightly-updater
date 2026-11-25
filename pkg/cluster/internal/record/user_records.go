package record

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
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
// whose values have "by" and "actorRole".
var UserRecordKeys = []string{"workbenches", "playgrounds"}

// RecordOwner is one entry of UserRecordKeys: its project, who wrote it
// and with which role.
type RecordOwner struct {
	Project   string
	By        string
	ActorRole string
}

// ReadRecordOwners reads the entries of UserRecordKeys from the snapshot
// ConfigMap (none when it is missing).
func ReadRecordOwners(c *kube.Client) ([]RecordOwner, error) {
	var cm struct {
		Data map[string]string `json:"data"`
	}
	found, err := c.Bookkeeping().GetIntoOptional(rhoai.NamespacedPath("v1", "configmaps", GetActivityNamespace(), SnapshotConfigMapName), &cm)
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
		raw := cm.Data[key]
		if raw == "" {
			continue
		}
		entries := map[string]struct {
			By        string `json:"by"`
			ActorRole string `json:"actorRole"`
		}{}
		if err := json.Unmarshal([]byte(raw), &entries); err != nil {
			return nil, fmt.Errorf("parse this tool's records (%s): %w", key, err)
		}
		for k, e := range entries {
			project, _, _ := strings.Cut(k, "/")
			out = append(out, RecordOwner{Project: project, By: e.By, ActorRole: e.ActorRole})
		}
	}
	return out, nil
}

// ErrorCodeRecordsFull is the errorCode of a setup refused by a cap.
const ErrorCodeRecordsFull = "records_full"

// UserRecordCapRefusal says why a setup by user (who is not a
// cluster-admin) in project must not add a record entry, "" when it may.
// owners are the recorded entries (ReadRecordOwners). A project that
// already holds a user's entry adds nothing to the total, nor one of
// user's to theirs. exists reports whether a counted project still exists;
// it is asked only when a cap would refuse, and a project that is gone is
// not counted (its entries are left over from a deleted project). An error
// of exists refuses (fail closed).
func UserRecordCapRefusal(owners []RecordOwner, project, user string, exists func(project string) (bool, error)) (string, error) {
	all, mine := map[string]bool{}, map[string]bool{}
	for _, o := range owners {
		if o.ActorRole != ActorRoleUser {
			continue
		}
		all[o.Project] = true
		if o.By == user {
			mine[o.Project] = true
		}
	}
	if mine[project] || all[project] && len(mine) < MaxUserRecordProjectsPerUser {
		return "", nil
	}
	live := func(set map[string]bool) (int, error) {
		n := 0
		names := make([]string, 0, len(set))
		for p := range set {
			names = append(names, p)
		}
		slices.Sort(names)
		for _, p := range names {
			ok, err := exists(p)
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
	if !all[project] && len(all) >= MaxUserRecordProjects {
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

// ProjectExists reports whether project's namespace exists, read with the
// facts client (the ServiceAccount's get on namespaces): only to tell
// whether a recorded project still counts in the caps, never shown.
func ProjectExists(c *kube.Client, project string) (bool, error) {
	return namespaceExists(c.Facts(), project)
}

// namespaceExists reads project's namespace with f, the facts client.
func namespaceExists(f *kube.Client, project string) (bool, error) {
	found, err := f.GetIntoOptional("/api/v1/namespaces/"+project, &struct{}{})
	if err != nil {
		return false, fmt.Errorf("check whether a recorded project exists: %w", err)
	}
	return found, nil
}

// snapshotWriter serializes this process's read-change-write cycles of
// the snapshot ConfigMap's keys (UpdateSnapshot): project actions in
// different projects run at once, and each record key is applied whole.
var snapshotWriter = make(podWriter, 1)

// UpdateSnapshot runs change (a read of a snapshot key, its change and
// its write) once this process's other changes of the snapshot ended, so
// two project actions never write the same key from the same old value.
func UpdateSnapshot(c *kube.Client, change func() error) error {
	return snapshotWriter.do(c.Context(), change)
}
