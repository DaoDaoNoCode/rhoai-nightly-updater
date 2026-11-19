package cluster

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Which quay.io/rhoai entry of openshift-config/pull-secret is this tool's.
//
// The cluster pull secret is the cluster's. An installer or a provisioner
// may have put a quay.io/rhoai entry there with a token of its own (a
// sandbox provisioner does, together with an ImageDigestMirrorSet of its
// own), and the tool must never remove or overwrite a setup it did not
// make. Nothing in a Docker config entry says who wrote it, and the
// Secret's managedFields name only the last writer of the whole
// .dockerconfigjson. So when Add to the nodes' pull secret writes the
// entry, the tool records it in its own namespace, which project users
// cannot write: the snapshot ConfigMap's data key "nodePullSecretEntry",
// server-side applied under a field manager of its own, as the pipeline
// and models records are (the RBAC is that ConfigMap's existing patch
// grant). The record holds the Secret's UID, the auths key written, a
// salted SHA-256 of the auth value written (never the value: the salt is
// random per record, so the hash cannot be looked up in a precomputed
// table), when and by whom.
//
// The entry is the tool's only when the record exists, the Secret's UID is
// the recorded one (a Secret made again is not the one the tool wrote to)
// and the entry under the recorded key has an auth value that hashes to
// the recorded hash (a token changed since is someone else's). Remove to
// the nodes' pull secret takes out only such an entry, and clears the
// record. Add replaces only such an entry without asking; another entry is
// replaced only when the user acknowledges that its token is not kept and
// the tool cannot restore it. An entry that already has the updater's
// token but that the tool did not write is left unclaimed.
const (
	nodePullRecordKey     = "nodePullSecretEntry"
	nodePullRecordManager = "rhoai-nightly-updater-node-pull-secret"
)

// nodePullRecord is the tool's record of the entry it wrote.
type nodePullRecord struct {
	// SecretUID is openshift-config/pull-secret's UID when the entry was
	// written.
	SecretUID string `json:"secretUID"`
	// Key is the auths key written (quay.io/rhoai).
	Key string `json:"key"`
	// Salt and AuthSHA256: hex SHA-256 of Salt followed by the auth value.
	Salt       string `json:"salt"`
	AuthSHA256 string `json:"authSHA256"`
	At         string `json:"at,omitempty"`
	By         string `json:"by,omitempty"`
}

// hashPullAuth is the record's hash of auth.
func hashPullAuth(salt, auth string) string {
	sum := sha256.Sum256([]byte(salt + auth))
	return hex.EncodeToString(sum[:])
}

// matches: auth is the value the tool recorded.
func (r *nodePullRecord) matches(auth string) bool {
	if r == nil || auth == "" || r.Salt == "" || r.AuthSHA256 == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(hashPullAuth(r.Salt, auth)), []byte(r.AuthSHA256)) == 1
}

// newNodePullRecord records that the tool wrote auth under quay.io/rhoai into the
// Secret with UID uid.
func newNodePullRecord(c *Client, uid, auth string) (nodePullRecord, error) {
	if uid == "" {
		return nodePullRecord{}, fmt.Errorf("%s has no UID", clusterPullSecretRef)
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nodePullRecord{}, err
	}
	s := hex.EncodeToString(salt)
	return nodePullRecord{SecretUID: uid, Key: quayRHOAIRegistry, Salt: s, AuthSHA256: hashPullAuth(s, auth), At: nowRFC3339(), By: getUser(c)}, nil
}

// readNodePullRecord reads the record (nil when there is none).
func readNodePullRecord(c *Client) (*nodePullRecord, error) {
	raw, err := readSnapshotKey(c, nodePullRecordKey)
	if err != nil {
		return nil, fmt.Errorf("read this tool's record of the entry it added to %s: %w", clusterPullSecretRef, err)
	}
	if raw == "" {
		return nil, nil
	}
	var rec nodePullRecord
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		return nil, fmt.Errorf("parse this tool's record of the entry it added to %s: %w", clusterPullSecretRef, err)
	}
	if rec.SecretUID == "" || rec.Key == "" || rec.AuthSHA256 == "" {
		return nil, nil
	}
	return &rec, nil
}

// writeNodePullRecord records rec; clearNodePullRecord drops it.
func writeNodePullRecord(c *Client, rec nodePullRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if err := writeSnapshotKey(c, nodePullRecordKey, nodePullRecordManager, string(data)); err != nil {
		return fmt.Errorf("record the entry added to %s: %w", clusterPullSecretRef, err)
	}
	return nil
}

func clearNodePullRecord(c *Client) error {
	if err := writeSnapshotKey(c, nodePullRecordKey, nodePullRecordManager, ""); err != nil {
		return fmt.Errorf("clear the record of the entry added to %s: %w", clusterPullSecretRef, err)
	}
	return nil
}

// Who set the quay.io/rhoai entry (types.NodeSetupPlan.EntrySetBy).
const (
	EntrySetByNone  = "none"
	EntrySetByTool  = "tool"
	EntrySetByOther = "other"
)

// pullEntries are the quay.io/rhoai entries of the cluster pull secret,
// split by who set them.
type pullEntries struct {
	// keys are every auths key that means quay.io/rhoai, sorted.
	keys []string
	// own is the key of the tool's entry ("" when none is the tool's).
	own string
	// foreign are the other keys, sorted.
	foreign []string
	// why says why no entry is the tool's, when there are entries.
	why string
}

// classifyPullEntries splits the entries of ps by rec.
func classifyPullEntries(ps *clusterPullSecret, rec *nodePullRecord) pullEntries {
	e := pullEntries{keys: rhoaiKeys(ps.auths)}
	if len(e.keys) == 0 {
		return e
	}
	switch {
	case rec == nil:
		e.why = "this tool has no record of adding it"
	case rec.SecretUID != ps.uid:
		e.why = clusterPullSecretRef + " was made again since this tool added its entry"
	default:
		entry, _ := ps.auths[rec.Key].(map[string]any)
		auth, _ := entry["auth"].(string)
		switch {
		case entry == nil:
			e.why = "the entry this tool added is gone; the one there now was added by someone else"
		case !rec.matches(auth):
			e.why = "its token was changed since this tool added it"
		default:
			e.own = rec.Key
		}
	}
	for _, k := range e.keys {
		if k != e.own {
			e.foreign = append(e.foreign, k)
		}
	}
	return e
}

// setBy is the plan's EntrySetBy.
func (e pullEntries) setBy() string {
	switch {
	case len(e.keys) == 0:
		return EntrySetByNone
	case e.own != "" && len(e.foreign) == 0:
		return EntrySetByTool
	}
	return EntrySetByOther
}

// nodePullRecordInfo is the record for the status, read with the
// ServiceAccount (the record is the tool's own; the cluster pull secret is
// not readable by it): nil when there is none. auth is the updater's
// token now, to say whether the recorded entry has it.
func nodePullRecordInfo(rec *nodePullRecord, auth string) *types.NodePullSecretRecord {
	if rec == nil {
		return nil
	}
	return &types.NodePullSecretRecord{At: rec.At, By: rec.By, SameToken: auth != "" && rec.matches(paddedAuth(auth))}
}
