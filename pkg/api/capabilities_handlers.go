package api

import (
	"crypto/sha256"
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
	verdicts map[string]actionVerdict
	expires  time.Time
}

// capabilitiesCache keeps the capabilities' answers by token hash and
// project; never the token itself.
type capabilitiesCache struct {
	mu      sync.Mutex
	entries map[[32]byte]capabilitiesEntry
}

var capabilities = &capabilitiesCache{entries: map[[32]byte]capabilitiesEntry{}}

func capabilitiesKey(token, project string) [32]byte {
	return sha256.Sum256([]byte(token + "\x00" + project))
}

func (c *capabilitiesCache) get(key [32]byte, now time.Time) (map[string]actionVerdict, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok || now.After(e.expires) {
		return nil, false
	}
	return e.verdicts, true
}

func (c *capabilitiesCache) put(key [32]byte, verdicts map[string]actionVerdict, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= 512 {
		c.entries = map[[32]byte]capabilitiesEntry{}
	}
	c.entries[key] = capabilitiesEntry{verdicts: verdicts, expires: now.Add(capabilitiesTTL)}
}

func (c *capabilitiesCache) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = map[[32]byte]capabilitiesEntry{}
}

// capabilityVerdicts is projectActionVerdicts for every project action,
// from the cache (a minute, by token hash and project) when it holds an
// answer that could be told.
func capabilityVerdicts(u *cluster.Client, admin bool, ids []string, token, project string) map[string]actionVerdict {
	key := capabilitiesKey(token, project)
	if admin {
		key = capabilitiesKey("admin\x00"+token, project)
	}
	if v, ok := capabilities.get(key, clk.Now()); ok {
		return v
	}
	verdicts := projectActionVerdicts(u, admin, ids, project)
	if !verdictsFailed(verdicts) {
		capabilities.put(key, verdicts, clk.Now())
	}
	return verdicts
}

// verdictsFailed reports whether an answer could not be told (a review or
// a read failed): such an answer is not kept.
func verdictsFailed(v map[string]actionVerdict) bool {
	for _, a := range v {
		if a.code == "authorization_unavailable" || a.code == "session_expired" || a.code == "prerequisites_unknown" {
			return true
		}
	}
	return false
}

// HandleProjectCapabilities answers what the signed-in user may do in one
// of their data science projects (?project=; tier user-token): for each
// project action, whether it is allowed, what their RBAC misses and what
// blocks it on the cluster. It runs the gate's own checks
// (withProjectMutationAuth) in the gate's order, through the same
// functions: the project must be one of the user's (the projects API with
// their token) before anything else is read, and a project that does not
// exist gets the same answer as one that is not theirs; then the role, the
// sign-in and the project's UID (what keeps every action from starting);
// then per action the prerequisites with the ServiceAccount's facts client
// and the access reviews with their token (projectActionVerdicts: a
// failed read refuses only the actions that need it; a failed access
// review refuses them all, fail closed). Cached a minute per token hash
// and project.
// Advisory: each action's gate checks again.
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
	ids := make([]string, 0, len(actions))
	for _, a := range actions {
		ids = append(ids, a.ID)
	}
	// What keeps every action from starting.
	allReason, allCode := "", ""
	if msg, code, _ := sessionProblem(r.Context(), token); msg != "" {
		allReason, allCode = msg, code
	} else if _, _, code, reason := projectStartProblem(u, project, admin); reason != "" {
		allReason, allCode = reason, code
	}
	var verdicts map[string]actionVerdict
	if allReason == "" {
		verdicts = capabilityVerdicts(u, admin, ids, token, project)
	}
	for _, a := range actions {
		cp := types.ProjectCapability{Label: a.Label, Missing: []string{}, BlockedBy: []string{}}
		v := verdictOf(verdicts, a.ID)
		if allReason != "" {
			v = actionVerdict{code: allCode, reason: allReason}
		}
		for _, m := range v.missing {
			cp.Missing = append(cp.Missing, m.Text)
		}
		if v.code == "prerequisites" || v.code == "prerequisites_unknown" {
			cp.BlockedBy = append(cp.BlockedBy, v.code)
		}
		cp.Reason, cp.Code = v.reason, v.code
		cp.Allowed = v.reason == ""
		out.Actions[a.ID] = cp
	}
	writeJSON(w, out, "project-capabilities")
})
