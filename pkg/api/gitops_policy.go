package api

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
)

// In the gitops profile the admission policy of deploy/rbac-gitops.yaml is
// what keeps the ServiceAccount to Subscription rhods-operator and the
// operator's own InstallPlans and CSVs. Its RBAC alone could not. So Update
// runs only while the policy and its binding are in place: checked at
// startup, every gitOpsPolicyInterval, and again by every Update before it
// takes the lock. Reads do not need it.

// checkGitOpsPolicy reads the policy and its binding; tests replace it.
var checkGitOpsPolicy = cluster.CheckGitOpsAdmissionPolicy

const gitOpsPolicyInterval = time.Minute

// gitOpsPolicyState is the result of the last check: "" until the first,
// "enforced", or the problem.
var gitOpsPolicyState struct {
	mu      sync.Mutex
	problem string
}

func setGitOpsPolicyState(problem string) {
	gitOpsPolicyState.mu.Lock()
	defer gitOpsPolicyState.mu.Unlock()
	gitOpsPolicyState.problem = problem
}

// gitOpsPolicyStatus is what /api/version reports in the gitops profile:
// "" before the first check, "enforced", or the problem.
func gitOpsPolicyStatus() string {
	gitOpsPolicyState.mu.Lock()
	defer gitOpsPolicyState.mu.Unlock()
	return gitOpsPolicyState.problem
}

// runGitOpsPolicyCheck checks the policy with c and records the result. It
// returns the problem, or "".
func runGitOpsPolicyCheck(c *cluster.Client) string {
	if err := checkGitOpsPolicy(c); err != nil {
		problem := "The gitops profile's admission policy is not in place (" + err.Error() + "), so Update is off until Argo CD syncs it again (docs/GITOPS.md, section 11)."
		setGitOpsPolicyState(problem)
		return problem
	}
	setGitOpsPolicyState("enforced")
	return ""
}

// StartGitOpsPolicyWatch checks the admission policy now and then every
// minute until ctx ends, logging a missing one. It does nothing outside the
// gitops profile.
func StartGitOpsPolicyWatch(ctx context.Context) {
	if activeProfile != ProfileGitOps {
		return
	}
	check := func() {
		token := getClusterToken()
		if token == "" {
			setGitOpsPolicyState("The gitops profile's admission policy could not be checked: no ServiceAccount token.")
			return
		}
		checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if problem := runGitOpsPolicyCheck(cluster.NewClientWithContext(checkCtx, token)); problem != "" {
			slog.Error("admission policy check failed", "problem", problem)
		}
	}
	check()
	go func() {
		ticker := time.NewTicker(gitOpsPolicyInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				check()
			}
		}
	}()
}

// refuseWithoutGitOpsPolicy answers 503 admission_policy_missing and
// returns true when the policy is not in place, checked now with c.
func refuseWithoutGitOpsPolicy(c *cluster.Client, w http.ResponseWriter) bool {
	problem := runGitOpsPolicyCheck(c)
	if problem == "" {
		return false
	}
	slog.Warn("Update refused: the admission policy is not in place", "problem", problem)
	writeError(w, problem+" Nothing was changed.", http.StatusServiceUnavailable, "admission_policy_missing")
	return true
}
