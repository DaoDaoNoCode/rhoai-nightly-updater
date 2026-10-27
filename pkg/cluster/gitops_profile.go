package cluster

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// ErrorCodeProfileGitOps marks a request the gitops profile refuses
// (docs/GITOPS.md, "Profiles"): the route is off in that profile, or Update
// would have to take a path that profile does not have.
const ErrorCodeProfileGitOps = "profile_gitops"

// UpdateStreamGitOpsProfile is Update in the gitops profile, the profile the
// Helm chart runs on clusters where Argo CD manages RHOAI. It runs only
// Update's Argo CD path (updateUnderGitOps): install the build the catalog
// Git manages serves, with the steps and checks Update uses there, and
// nothing that the profile's RBAC (deploy/rbac-gitops.yaml) leaves out:
//
//   - it refuses unless Argo CD or an ACM policy manages CatalogSource
//     rhoai-catalog-dev (readOperatorGitOps), so the path that writes that
//     catalog never runs;
//
//   - it never creates the operator namespace or OperatorGroup (Git does)
//     and refuses when they are missing or unusable;
//
//   - it never ends a Dashboard Dev session (that patches Deployments); it
//     refuses while one is active.
//
//   - it reads no Secret and contacts no registry: the cluster's own pull
//     secrets are used (gitOpsProfileNoPullSecret), and the build comes
//     from the catalog's PackageManifest;
//
//   - it never writes a CatalogSource: instead of a temporary verification
//     catalog it reads the build from the catalog Git manages
//     (liveCatalogTarget);
//
//   - before it deletes anything it checks that the admission policy allows
//     the Subscription it creates and the one the restore would re-apply.
//
// The rest is UpdateStreamWithOptions's: the same prerequisites, the
// recovery and the activity entry.
// pkg/cluster/rbac_coverage_test.go derives the RBAC this function reaches
// and compares it with deploy/rbac-gitops.yaml.
func UpdateStreamGitOpsProfile(c *Client, image string, emit func(UpdateStepEvent)) (result *types.OperationResponse, opErr error) {
	logs := []string{}
	tracker := &stepTracker{emit: emit}
	emit = tracker.send
	var recovery *operatorRecovery
	defer func() {
		tracker.finish(c, &result, &opErr, logs, recovery, func(ok bool, reason string) { recordUpdateActivity(c, image, ok, reason) })
	}()
	fail := func(msg, code string) (*types.OperationResponse, error) {
		logs = append(logs, msg)
		return &types.OperationResponse{Success: false, Message: msg, Logs: logs, ErrorCode: code}, nil
	}

	logs = append(logs, fmt.Sprintf("Target image: %s", image))
	logs = append(logs, "Profile: gitops (Update installs the build the managed catalog serves; nothing else is changed)")

	// --- Step 1: validate_prerequisites ---
	emit(UpdateStepEvent{Step: "validate_prerequisites", Status: "running", Message: "Checking prerequisites..."})
	g, err := readOperatorGitOps(c)
	if err != nil {
		return fail("Cannot check whether Argo CD manages the operator: "+err.Error()+". Nothing was changed.", errorCodeFromK8sErr(err))
	}
	if !g.catalogManaged() {
		return fail(gitOpsProfileUnmanagedCatalog, ErrorCodeProfileGitOps)
	}
	logs = append(logs, fmt.Sprintf("OK: %s manages CatalogSource %s", sentenceStart(g.catalogManager()), CatalogName))

	active, err := DashboardDevActive(c)
	if err != nil {
		return fail(fmt.Sprintf("Cannot check for a Dashboard Dev session: %v. Nothing was changed.", err), errorCodeFromK8sErr(err))
	}
	if active {
		return fail("A Dashboard Dev session is active (dashboard-operator is paused), and Update would keep the dashboard paused under the new operator. "+
			"The gitops profile cannot end the session: end it with the full profile, or scale Deployment dashboard-operator in redhat-ods-applications back up. Nothing was changed.", errorCodeDashboardDevActive)
	}

	// No pull secret read: the profile reads no Secret and contacts no
	// registry (gitOpsProfileNoPullSecret).
	logs = append(logs, gitOpsProfileNoPullSecret)
	emit(UpdateStepEvent{Step: "validate_prerequisites", Status: "running", Message: gitOpsProfileNoPullSecret})

	idms, err := getIDMS(c)
	if err != nil {
		return fail(fmt.Sprintf("Failed to check IDMS: %v", err), errorCodeFromK8sErr(err))
	}
	if !idms.Exists {
		return fail(fmt.Sprintf("No IDMS found mirroring %s. Create it first (docs/QUICKSTART.md). Nothing was changed.", IDMSSource), "prerequisites")
	}
	logs = append(logs, fmt.Sprintf("OK: IDMS exists (%s)", idms.Name))

	ogLog, ogProblem, ogCode := checkOperatorNamespaceAndGroup(c)
	if ogProblem != "" {
		return fail(ogProblem, ogCode)
	}
	logs = append(logs, ogLog)

	currentCSV, csvCheckErr := getCSV(c)
	if csvCheckErr != nil {
		return fail("Cannot identify installed CSV: "+csvCheckErr.Error(), "prerequisites")
	}

	gitOps, channelOverride, gitOpsProblem, gitOpsErr := updateGitOpsPlan(c, image, currentCSV, &logs)
	if gitOpsErr != nil {
		return fail("Cannot check whether Argo CD manages the operator: "+gitOpsErr.Error()+". Nothing was changed.", errorCodeFromK8sErr(gitOpsErr))
	}
	if gitOpsProblem != "" {
		return fail(gitOpsProblem, ErrorCodeGitOpsManaged)
	}
	if !gitOps.catalogManaged() {
		// Argo CD and ACM stopped managing the catalog since the first read.
		return fail(gitOpsProfileUnmanagedCatalog, ErrorCodeProfileGitOps)
	}

	// No verification catalog: the gitops profile never writes a
	// CatalogSource (OLM rewrites spec.image from a CatalogSource's
	// olm.catalogImageTemplate annotation, which an admission policy cannot
	// see; operator-lifecycle-manager
	// pkg/controller/operators/catalogtemplate/operator.go). The build is
	// read from the managed catalog once it serves the image.
	from := gitOps.catalogSpecFrom()
	emit(UpdateStepEvent{Step: "validate_prerequisites", Status: "running", Message: "Waiting for the catalog " + from + " manages to serve the build..."})
	target, preflightErr := liveCatalogTarget(c, image, channelOverride, gitOps.catalogChanger(), emit, &logs)
	if preflightErr != nil {
		var changed *errCatalogImageChanged
		switch {
		case c.ctx.Err() != nil:
			return &types.OperationResponse{Success: false, Message: "Operation stopped while waiting for the catalog", Logs: logs}, c.ctx.Err()
		case errors.As(preflightErr, &changed):
			return fail("Update stopped: "+changed.Error()+". Nothing was changed.", ErrorCodeGitOpsManaged)
		case isCatalogImagePullError(preflightErr):
			return fail(preflightErr.Error()+". Nothing was changed.", "catalog_image_pull")
		}
		hint := ""
		if channelOverride != "" {
			hint = fmt.Sprintf(" Argo CD application %q sets the Subscription's channel %s in Git, so Update keeps it: the catalog must publish that channel.", gitOps.subscriptionApp, channelOverride)
		}
		return fail("The catalog "+from+" manages does not serve a usable build: "+preflightErr.Error()+". Nothing was changed."+hint, "validation")
	}
	logs = append(logs, fmt.Sprintf("OK: The catalog serves %s on channel %s", target.HeadCSV, target.Channel))
	blocked, note := downgradeCheck(currentCSV, extractTagFromRef(image), target.HeadCSV)
	if note != "" {
		logs = append(logs, note)
	}
	if blocked != "" {
		return fail(blocked, "validation")
	}
	captured, recoveryErr := captureOperatorRecovery(c)
	if recoveryErr != nil {
		return fail(recoveryErr.Error()+". Nothing was changed.", "prerequisites")
	}
	if blocked := foreignSubscriptionRefusal(c); blocked != "" {
		return fail(blocked, "prerequisites")
	}
	// The admission policy must allow what Update writes back: the new
	// Subscription and, on failure, the previous one (checked again on the
	// live Subscription right before the first deletion).
	if msg := gitOpsSubscriptionsProblem(buildSubscription(captured.subscriptionBase(), CatalogName, target.Channel), captured.subscription); msg != "" {
		return fail(msg+" Nothing was changed.", ErrorCodeProfileGitOps)
	}
	emit(UpdateStepEvent{Step: "validate_prerequisites", Status: "success", Message: "Prerequisites validated"})

	// --- Step 2: save_snapshot ---
	emit(UpdateStepEvent{Step: "save_snapshot", Status: "running", Message: "Saving deployment snapshot..."})
	if snapErr := SaveDeploymentSnapshot(c); snapErr != nil {
		slog.Warn("failed to save deployment snapshot", "error", snapErr)
		logs = append(logs, fmt.Sprintf("Warning: failed to save deployment snapshot: %v", snapErr))
		emit(UpdateStepEvent{Step: "save_snapshot", Status: "success", Message: "Snapshot save failed (non-fatal)", Detail: snapErr.Error()})
	} else {
		logs = append(logs, "OK: Deployment snapshot saved")
		emit(UpdateStepEvent{Step: "save_snapshot", Status: "success", Message: "Snapshot saved"})
	}

	recovery = captured
	if note := captured.recordedNote(); note != "" {
		logs = append(logs, note)
	}
	return updateUnderGitOps(c, image, target, currentCSV, gitOps, recovery, &logs, emit, fail, gitOpsWriteProblem(c))
}

// gitOpsWriteProblem is Update's check before each deletion in the gitops
// profile: the Subscriptions it would write pass this pod's copy of the
// policy (gitOpsSubscriptionsProblem), and the policy on the cluster is
// still that copy. Argo CD applies a changed policy (wave -1) before it
// replaces this pod, and this pod may still be running an Update then: with
// an allowlist that dropped a variable, it would delete a Subscription it
// could not create again.
func gitOpsWriteProblem(c *Client) func(newSub, restoreSub map[string]interface{}) string {
	return func(newSub, restoreSub map[string]interface{}) string {
		if msg := gitOpsSubscriptionsProblem(newSub, restoreSub); msg != "" {
			return msg
		}
		if err := CheckGitOpsAdmissionPolicy(c); err != nil {
			return "The admission policy on the cluster is not the one this pod checks Subscriptions with: " + err.Error() + ". Argo CD may be rolling out a change; run Update again once it has replaced this pod."
		}
		return ""
	}
}

// UpdateDryRunGitOpsProfile is Update's dry run in the gitops profile: the
// checks UpdateStreamGitOpsProfile makes before its first change, with
// reads only. Update's own dry run cannot be used there: it reaches the
// namespace and OperatorGroup creation (ensureOperatorNamespaceAndGroup)
// and the server-side dry-run apply of the catalog, which the profile's
// RBAC does not grant.
func UpdateDryRunGitOpsProfile(c *Client, image string) (*types.OperationResponse, error) {
	logs := []string{fmt.Sprintf("Target image: %s", image), "[DRY-RUN] Profile: gitops"}
	fail := func(msg, code string) (*types.OperationResponse, error) {
		logs = append(logs, "[DRY-RUN] FAILED: "+msg)
		return &types.OperationResponse{Success: false, Message: msg, Logs: logs, ErrorCode: code}, nil
	}
	g, err := readOperatorGitOps(c)
	if err != nil {
		return fail("Cannot check whether Argo CD manages the operator: "+err.Error(), errorCodeFromK8sErr(err))
	}
	if !g.catalogManaged() {
		return fail(gitOpsProfileUnmanagedCatalog, ErrorCodeProfileGitOps)
	}
	logs = append(logs, fmt.Sprintf("OK: %s manages CatalogSource %s", sentenceStart(g.catalogManager()), CatalogName))
	if active, err := DashboardDevActive(c); err == nil && active {
		logs = append(logs, "[DRY-RUN] WARNING: A Dashboard Dev session is active. Update refuses to run while it is (the gitops profile cannot end it).")
	}
	logs = append(logs, gitOpsProfileNoPullSecret)
	idms, err := getIDMS(c)
	if err != nil {
		return fail(fmt.Sprintf("Failed to check IDMS: %v", err), errorCodeFromK8sErr(err))
	}
	if !idms.Exists {
		return fail(fmt.Sprintf("No IDMS found mirroring %s. Create it first (docs/QUICKSTART.md).", IDMSSource), "prerequisites")
	}
	logs = append(logs, fmt.Sprintf("OK: IDMS exists (%s)", idms.Name))
	ogLog, ogProblem, ogCode := checkOperatorNamespaceAndGroup(c)
	if ogProblem != "" {
		return fail(ogProblem, ogCode)
	}
	logs = append(logs, ogLog)
	currentCSV, err := getCSV(c)
	if err != nil {
		return fail("Cannot identify installed CSV: "+err.Error(), "prerequisites")
	}
	planned, channelOverride, problem, err := updateGitOpsPlan(c, image, currentCSV, &logs)
	if err != nil {
		return fail("Cannot check whether Argo CD manages the operator: "+err.Error(), errorCodeFromK8sErr(err))
	}
	if problem != "" {
		return fail(problem, ErrorCodeGitOpsManaged)
	}
	live, found, err := readSubscriptionObject(c)
	if err != nil {
		return fail("Cannot read the Subscription: "+err.Error(), errorCodeFromK8sErr(err))
	}
	if found {
		if msg := gitOpsSubscriptionsProblem(buildSubscription(live, CatalogName, nonEmpty(channelOverride, "unknown")), live); msg != "" {
			return fail(msg, ErrorCodeProfileGitOps)
		}
		logs = append(logs, "OK: The admission policy allows recreating the Subscription and restoring it")
	}
	// The build comes from the catalog itself (its PackageManifest), as in
	// Update (liveCatalogTarget), not from the image in the registry.
	bundle := ""
	if target, problem, _ := liveCatalogTargetOnce(c, image, channelOverride); problem == "" {
		bundle = target.HeadCSV
		logs = append(logs, fmt.Sprintf("OK: The catalog serves %s on channel %s", target.HeadCSV, target.Channel))
	} else {
		logs = append(logs, fmt.Sprintf("[DRY-RUN] Note: the catalog does not serve this build yet (%s), so the version check uses the release tag; Update waits for the catalog and checks the version it serves", problem))
	}
	blocked, note := downgradeCheck(currentCSV, extractTagFromRef(image), bundle)
	if note != "" {
		logs = append(logs, note)
	}
	if blocked != "" {
		return fail(blocked, "validation")
	}
	channel := nonEmpty(channelOverride, "the one the build publishes")
	if channelOverride != "" {
		channel += " (kept from Git)"
	}
	from := planned.catalogSpecFrom()
	if bundle != "" {
		channel += ", startingCSV " + bundle
	}
	logs = append(logs, "OK: CatalogSource left as "+from+" sets it",
		fmt.Sprintf("[DRY-RUN] Would wait until the catalog %s manages serves the build, delete the old CSV and InstallPlan, then delete and at once recreate the Subscription with channel %s, keeping its config, approval mode and Argo CD tracking", from, channel))
	return &types.OperationResponse{Success: true, Message: "Dry run complete. No changes were made.", Logs: logs}, nil
}

// gitOpsProfileUnmanagedCatalog is the refusal when neither Argo CD nor an
// ACM policy manages the nightly catalog: Update would then replace the
// catalog itself, which the gitops profile never does.
const gitOpsProfileUnmanagedCatalog = "This updater runs the gitops profile, which only installs the build that CatalogSource " + CatalogName +
	" serves when Argo CD or an ACM policy manages that catalog, and neither does here (no " + argoTrackingAnnotation + " annotation names it, and " +
	acmPolicyManager + " does not own its spec). Manage the catalog with Argo CD or an ACM policy, or deploy the updater with the full profile. Nothing was changed."

// gitOpsProfileNoPullSecret is what Update and its dry run say in the gitops
// profile, which never reads kube-system/additional-pull-secret (or any
// Secret) and never contacts a registry, so it could not check the
// credential with Quay either. It does not need it: on the clusters it runs on, the Quay credentials
// are in the cluster's global pull secret (openshift-config/pull-secret), and
// may also be a pull secret the CatalogSource names, which Git or ACM manage.
// The updater does not read those (the profile reads as few Secrets as
// possible), and does not have to: before anything is deleted the catalog
// must be READY and serving the build from a registry pod that runs its image
// (liveCatalogTarget), which proves the catalog image pulls; a missing
// credential for the bundle or operator images fails the install, which
// Update reports and recovers from like any failed install (the restore).
const gitOpsProfileNoPullSecret = "Pull secret: not read. The gitops profile pulls with the cluster's own pull secrets, which Git or ACM manage here, and does not check them"

// checkOperatorNamespaceAndGroup is ensureOperatorNamespaceAndGroup without
// the creating half: in the gitops profile Git creates the operator
// namespace and its OperatorGroup, so a missing one is a refusal. It returns
// the log line, or the problem and its error code.
func checkOperatorNamespaceAndGroup(c *Client) (ok, problem, errorCode string) {
	nsMissing, groups, problem, errorCode := readOperatorNamespaceAndGroups(c)
	switch {
	case problem != "":
		return "", problem, errorCode
	case nsMissing:
		return "", fmt.Sprintf("Namespace %s does not exist. The gitops profile does not create it: define it, with its OperatorGroup, in Git. Nothing was changed.", SubNS), "prerequisites"
	case len(groups) == 0:
		return "", fmt.Sprintf("Namespace %s has no OperatorGroup. The gitops profile does not create one: define a global OperatorGroup in Git. Nothing was changed.", SubNS), "prerequisites"
	}
	ok, problem = operatorGroupsUsable(groups)
	if problem != "" {
		return "", problem, "prerequisites"
	}
	return ok, "", ""
}

// liveCatalogTarget is the gitops profile's replacement for the
// verification catalog: it waits until the catalog Git or an ACM policy
// manages serves image (its spec.image is image, a registry pod runs it and
// is Ready, no pod runs another image, the catalog is READY: the same
// conditions waitForCatalogServing uses), then reads the channel's head and
// build from that catalog's PackageManifest. The channel is override (Git's)
// or the one the image's release publishes. A change of the catalog image
// meanwhile returns an *errCatalogImageChanged (by changedBy); a failing
// pull a *catalogImagePullError.
//
// Unlike a fresh verification catalog, the PackageManifest of a catalog
// whose pod was just replaced may still list the previous build for a short
// while (PackageManifestPropagationWait). OLM installs from the registry pod
// itself, so then another build than the one read here is installed, and
// verifyUpdateInstall reports install_mismatch (the operator is kept) when
// the installed CSV's build annotations differ.
func liveCatalogTarget(c *Client, image, override, changedBy string, emit func(UpdateStepEvent), logs *[]string) (catalogTarget, error) {
	deadline := time.Now().Add(CatalogReadyTimeout + PackageManifestPropagationWait)
	var pull pullFailureWatch
	last := ""
	for {
		target, problem, changedTo := liveCatalogTargetOnce(c, image, override)
		switch {
		case changedTo != "":
			return catalogTarget{}, &errCatalogImageChanged{by: changedBy, image: changedTo}
		case problem == "":
			return target, nil
		}
		if problem != last {
			last = problem
			*logs = append(*logs, "  "+problem)
			emit(UpdateStepEvent{Step: "validate_prerequisites", Status: "running", Message: "Catalog: " + problem})
		}
		if err := pull.check(c, CatalogName, image); err != nil {
			return catalogTarget{}, err
		}
		if !time.Now().Before(deadline) {
			return catalogTarget{}, errors.New(problem)
		}
		select {
		case <-c.ctx.Done():
			return catalogTarget{}, c.ctx.Err()
		case <-time.After(CatalogPollInterval):
		}
	}
}

// liveCatalogTargetOnce is one poll of liveCatalogTarget: the target when
// the catalog serves image now, else why not (problem), or the catalog's
// other image (changedTo). The gitops dry run reads it once.
func liveCatalogTargetOnce(c *Client, image, override string) (target catalogTarget, problem, changedTo string) {
	cs, err := getCatalogSource(c)
	switch {
	case err != nil:
		return catalogTarget{}, "cannot read the CatalogSource: " + err.Error(), ""
	case !cs.Exists:
		return catalogTarget{}, "CatalogSource " + CatalogName + " does not exist", ""
	case cs.Image != image:
		return catalogTarget{}, "the CatalogSource image is " + cs.Image, cs.Image
	case cs.State != "READY":
		return catalogTarget{}, "catalog state " + nonEmpty(cs.State, "unknown"), ""
	}
	ready, stale, ok := readCatalogPods(c, image)
	switch {
	case !ok:
		return catalogTarget{}, "cannot read the catalog pods", ""
	case !ready || stale > 0:
		return catalogTarget{}, "OLM is replacing the catalog pod", ""
	}
	channels, err := catalogChannels(c, CatalogName)
	switch {
	case err != nil:
		return catalogTarget{}, err.Error(), ""
	case len(channels) == 0:
		return catalogTarget{}, "the catalog does not list the " + SubName + " package yet", ""
	}
	channel := override
	if channel == "" {
		channel, err = detectBestChannel(channels, image)
	}
	entry := findChannel(channels, channel)
	head, _ := entry["currentCSV"].(string)
	switch {
	case err != nil:
		return catalogTarget{}, err.Error(), ""
	case entry == nil:
		return catalogTarget{}, fmt.Sprintf("channel %q is not in the catalog (available: %s)", channel, strings.Join(channelNames(channels), ", ")), ""
	case head == "":
		return catalogTarget{}, fmt.Sprintf("channel %q has no head", channel), ""
	}
	return catalogTarget{Channel: channel, HeadCSV: head, OwnedCRDs: ownedCRDVersions(entry), Fingerprint: headFingerprint(entry)}, "", ""
}
