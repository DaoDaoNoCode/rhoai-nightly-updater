package api

import (
	"crypto/sha256"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// capabilitiesTTL is how long a user's capabilities in a project are
// reused (per token and project, like the other per-user reads).
const capabilitiesTTL = time.Minute

type capabilitiesEntry struct {
	missing map[string][]cluster.MissingPermission
	expires time.Time
}

// capabilitiesCache keeps the preflight's answers by token hash and
// project; never the token itself.
type capabilitiesCache struct {
	mu      sync.Mutex
	entries map[[32]byte]capabilitiesEntry
}

var capabilities = &capabilitiesCache{entries: map[[32]byte]capabilitiesEntry{}}

func capabilitiesKey(token, project string) [32]byte {
	return sha256.Sum256([]byte(token + "\x00" + project))
}

func (c *capabilitiesCache) get(key [32]byte, now time.Time) (map[string][]cluster.MissingPermission, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || now.After(e.expires) {
		return nil, false
	}
	return e.missing, true
}

func (c *capabilitiesCache) put(key [32]byte, missing map[string][]cluster.MissingPermission, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= 512 {
		c.entries = map[[32]byte]capabilitiesEntry{}
	}
	c.entries[key] = capabilitiesEntry{missing: missing, expires: now.Add(capabilitiesTTL)}
}

func (c *capabilitiesCache) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = map[[32]byte]capabilitiesEntry{}
}

// HandleProjectCapabilities answers what the signed-in user may do in one
// of their data science projects (?project=; tier user-token): for each
// project action, whether it is allowed, what their RBAC misses and what
// blocks it on the cluster. It shares the gate's checks and their order
// (withProjectMutationAuth): the project must be one of the user's (the
// projects API with their token) before anything else is read, and a
// project that does not exist gets the same answer as one that is not
// theirs; then the access reviews with their token (all actions' requests,
// each once, cached a minute); then the prerequisites with the
// ServiceAccount's facts client. Advisory only: each action's gate checks
// again.
var HandleProjectCapabilities = withAuth(func(sa *cluster.Client, w http.ResponseWriter, r *http.Request) {
	project := r.URL.Query().Get("project")
	if !namespaceRegex.MatchString(project) {
		writeError(w, "a valid project is required", http.StatusBadRequest, "validation")
		return
	}
	token := extractUserToken(r)
	u := userReadClient(r.Context(), sa, token, resolveUsername(r))
	if !userProjectCheck(w, u, project, "") {
		return
	}
	admin, ok := requestRole(w, r)
	if !ok {
		return
	}
	out := types.ProjectCapabilities{Project: project, Role: roleUser, Actions: map[string]types.ProjectCapability{}}
	if admin {
		out.Role = roleAdmin
	}
	actions := cluster.ProjectActions()
	var missing map[string][]cluster.MissingPermission
	if !admin {
		var ids []string
		for _, a := range actions {
			if a.ForUsers {
				ids = append(ids, a.ID)
			}
		}
		key := capabilitiesKey(token, project)
		cached, hit := capabilities.get(key, clk.Now())
		if hit {
			missing = cached
		} else {
			m, err := preflight(u, ids, project)
			if errors.Is(err, cluster.ErrUnauthenticated) {
				writeError(w, "Your OpenShift session has expired. Log in again.", http.StatusUnauthorized, "session_expired")
				return
			}
			if err != nil {
				slog.Warn("project capabilities: the access reviews failed", "project", project, "error", err)
				writeError(w, "Cannot check your permissions in "+project+": "+err.Error(), http.StatusServiceUnavailable, "authorization_unavailable")
				return
			}
			missing = m
			capabilities.put(key, m, clk.Now())
		}
	}
	type block struct{ reason, code string }
	blocks := map[string]block{}
	for _, a := range actions {
		cp := types.ProjectCapability{Label: a.Label, Missing: []string{}, BlockedBy: []string{}}
		switch {
		case !admin && !a.ForUsers:
			cp.Reason, cp.Code = a.Label+" is done by a cluster-admin in this tool.", adminOnlyCode
			out.Actions[a.ID] = cp
			continue
		case !admin && len(missing[a.ID]) > 0:
			for _, m := range missing[a.ID] {
				cp.Missing = append(cp.Missing, m.Text)
			}
			cp.Reason, cp.Code = cluster.MissingSummaryFor(a.Label, project, missing[a.ID]), missingPermissionsCode
		}
		b, seen := blocks[a.Feature]
		if !seen {
			b.reason, b.code = projectFeatureBlock(u, a.Feature)
			blocks[a.Feature] = b
		}
		if b.reason != "" {
			cp.BlockedBy = append(cp.BlockedBy, b.code)
			if cp.Reason == "" {
				cp.Reason, cp.Code = b.reason, b.code
			}
		}
		cp.Allowed = cp.Reason == ""
		out.Actions[a.ID] = cp
	}
	writeJSON(w, out, "project-capabilities")
})
