package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"regexp"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The Components page's DataScienceCluster manager: the component tree,
// plans of a change, and the changes themselves (pkg/cluster dsc_manager.go,
// dsc_hard_reset.go). Tests replace these.
var (
	getDSCManager          = cluster.GetDSCManager
	planDSCChange          = cluster.PlanDSCChange
	setDSCManagementState  = cluster.SetDSCManagementState
	fixDSC                 = cluster.FixDSC
	softResetDSC           = cluster.SoftResetDSC
	hardResetDSC           = cluster.HardResetDSC
	finishDSCHardReset     = cluster.FinishDSCHardReset
	dscPathRegex           = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]{0,62}(\.[A-Za-z][A-Za-z0-9]{0,62}){0,7}$`)
	managementStateRegex   = regexp.MustCompile(`^[A-Za-z]{1,32}$`)
	dscUIDRegex            = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)
	dscPlanTokenRegex      = regexp.MustCompile(`^[a-f0-9]{24}$`)
	dscPlanActions         = map[string]bool{"toggle": true, "fix": true, "soft-reset": true, "hard-reset": true}
	dscResetActions        = map[string]bool{"soft-reset": true, "hard-reset": true}
	dscChangeRequestMaxLen = int64(4096)
)

// HandleDSCManager returns the component tree of the DataScienceCluster,
// compared with the release default.
var HandleDSCManager = withAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	m, err := getDSCManager(c)
	if err != nil {
		slog.Error("dsc manager error", "error", err)
		code, errorCode := cluster.HTTPStatusForError(err)
		writeError(w, "Could not read the DataScienceCluster: "+err.Error(), code, errorCode)
		return
	}
	writeJSON(w, m, "dsc-manager")
})

// HandleDSCPlan returns what a change would do, computed from a fresh read:
// ?action=toggle&path=kserve.nim&to=Removed[&withParent=true], action=fix,
// or action=soft-reset / hard-reset with keepEnabled=true or false (keep
// the components the user turned on; required, as it changes the plan).
// Only cluster-admins, who may make the change, get a plan: it names what
// uses each component across projects.
var HandleDSCPlan = withAdminRead(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	action, path, to := q.Get("action"), q.Get("path"), q.Get("to")
	if !dscPlanActions[action] || (action == "toggle" && (!dscPathRegex.MatchString(path) || !managementStateRegex.MatchString(to))) {
		writeError(w, "Give action=toggle with path and to, or action=fix, soft-reset or hard-reset", http.StatusBadRequest, "validation")
		return
	}
	keep := q.Get("keepEnabled")
	if dscResetActions[action] && keep != "true" && keep != "false" {
		writeError(w, "Give keepEnabled=true or false: whether the reset keeps the components you turned on", http.StatusBadRequest, "validation")
		return
	}
	plan, err := planDSCChange(c, action, path, to, q.Get("withParent") == "true", dscResetActions[action] && keep == "true")
	if err != nil {
		code, errorCode := cluster.HTTPStatusForError(err)
		writeError(w, "Could not work out the change: "+err.Error(), code, errorCode)
		return
	}
	writeJSON(w, plan, "dsc-plan")
})

// HandleDSCManagement sets one managementState in the DataScienceCluster.
var HandleDSCManagement = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()
	r.Body = http.MaxBytesReader(w, r.Body, dscChangeRequestMaxLen)
	var req types.DSCToggleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !dscNameRegex.MatchString(req.Name) || !dscPathRegex.MatchString(req.Path) ||
		!managementStateRegex.MatchString(req.To) || (req.From != "" && !managementStateRegex.MatchString(req.From)) ||
		!dscUIDRegex.MatchString(req.UID) || !dscPlanTokenRegex.MatchString(req.Token) {
		writeError(w, "Give the DataScienceCluster's name and UID, a component path, the value you saw (from), the value to set (to) and the token of the change you reviewed", http.StatusBadRequest, "validation")
		return
	}
	beginOperation(w, req.Name+": "+req.Path+" to "+req.To)
	slog.Info("mutation", "op", "dsc-management", "dsc", req.Name, "path", req.Path, "from", req.From, "to", req.To, "withParent", req.WithParent)
	result, err := setDSCManagementState(c, req)
	if err != nil {
		code, errorCode := cluster.HTTPStatusForError(err)
		writeError(w, "Could not change the DataScienceCluster: "+err.Error(), code, errorCode)
		return
	}
	writeOperationResult(w, result, "dsc-management")
})

// decodePlanRequest reads the body of a reviewed-plan change; a reset
// must say whether it keeps the components the user turned on.
func decodePlanRequest(w http.ResponseWriter, r *http.Request, reset bool) (types.DSCPlanRequest, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, dscChangeRequestMaxLen)
	var req types.DSCPlanRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !dscNameRegex.MatchString(req.Name) || !dscUIDRegex.MatchString(req.UID) || !dscPlanTokenRegex.MatchString(req.Token) {
		writeError(w, "Give the DataScienceCluster's name and UID and the token of the change you reviewed", http.StatusBadRequest, "validation")
		return req, false
	}
	if reset && req.KeepEnabled == nil {
		writeError(w, "Give keepEnabled (true or false) as in the reset you reviewed", http.StatusBadRequest, "validation")
		return req, false
	}
	return req, true
}

// HandleDSCFix adds the components the release default sets and the
// DataScienceCluster leaves unset, and removes fields the CRD does not
// define: the plan the user reviewed.
var HandleDSCFix = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()
	req, ok := decodePlanRequest(w, r, false)
	if !ok {
		return
	}
	beginOperation(w, req.Name)
	slog.Info("mutation", "op", "dsc-fix", "dsc", req.Name)
	result, err := fixDSC(c, req)
	if err != nil {
		code, errorCode := cluster.HTTPStatusForError(err)
		writeError(w, "Could not change the DataScienceCluster: "+err.Error(), code, errorCode)
		return
	}
	writeOperationResult(w, result, "dsc-fix")
})

// HandleDSCSoftReset makes spec.components the release default in place.
var HandleDSCSoftReset = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()
	req, ok := decodePlanRequest(w, r, true)
	if !ok {
		return
	}
	beginOperation(w, req.Name)
	slog.Info("mutation", "op", "dsc-soft-reset", "dsc", req.Name, "keepEnabled", *req.KeepEnabled)
	result, err := softResetDSC(c, req)
	if err != nil {
		code, errorCode := cluster.HTTPStatusForError(err)
		writeError(w, "Could not change the DataScienceCluster: "+err.Error(), code, errorCode)
		return
	}
	writeOperationResult(w, result, "dsc-soft-reset")
})

// HandleDSCHardResetStream deletes the DataScienceCluster and creates the
// release default in its place, as one operation streaming its steps.
var HandleDSCHardResetStream = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()
	req, ok := decodePlanRequest(w, r, true)
	if !ok {
		return
	}
	if req.Confirm != req.Name {
		writeError(w, "Type the DataScienceCluster's name to confirm the hard reset", http.StatusBadRequest, "validation")
		return
	}
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("panic in dsc hard reset handler", "error", rec)
		}
	}()
	sseWriter, err := NewSSEWriter(w)
	if err != nil {
		writeError(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	defer sseWriter.Close()
	done := make(chan struct{})
	defer close(done)
	go sseHeartbeat(sseWriter, done)

	beginOperation(w, req.Name)
	slog.Info("mutation", "op", "dsc-hard-reset", "dsc", req.Name, "keepEnabled", *req.KeepEnabled)
	result, runErr := hardResetDSC(c, req, forwardSteps(sseWriter))
	sendOperationResult(sseWriter, result, runErr)
	if runErr != nil {
		slog.Error("dsc hard reset failed", "error", runErr)
	}
})

// HandleDSCHardResetFinish creates the DataScienceCluster a hard reset
// recorded and did not create (FinishDSCHardReset), while none exists.
var HandleDSCHardResetFinish = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
	if !lockCluster(w) {
		return
	}
	defer releaseClusterMutationLock()
	r.Body = http.MaxBytesReader(w, r.Body, dscChangeRequestMaxLen)
	var req types.DSCFinishRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !dscNameRegex.MatchString(req.Name) || !dscPlanTokenRegex.MatchString(req.Token) {
		writeError(w, "Give the DataScienceCluster's name and the token of the hard reset you reviewed", http.StatusBadRequest, "validation")
		return
	}
	beginOperation(w, req.Name)
	slog.Info("mutation", "op", "dsc-hard-reset-finish", "dsc", req.Name)
	result, err := finishDSCHardReset(c, req)
	if err != nil {
		code, errorCode := cluster.HTTPStatusForError(err)
		writeError(w, "Could not finish the hard reset: "+err.Error(), code, errorCode)
		return
	}
	writeOperationResult(w, result, "dsc-hard-reset-finish")
})
