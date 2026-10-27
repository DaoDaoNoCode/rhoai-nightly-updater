package cluster

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// updateGitOpsPlan reads whether Argo CD manages the nightly CatalogSource or
// the operator Subscription, and how Update must work with it. Git is then
// the source of truth for the catalog image and the Subscription:
//   - image must be the one the catalog has (Git's choice); Update installs
//     the build the catalog serves and never changes the catalog, which
//     Argo CD would put back (problem otherwise);
//   - a managed Subscription keeps its channel (channelOverride), because
//     Argo CD would put Git's channel back, and must already install from the
//     nightly catalog (problem otherwise);
//   - while nothing is installed yet and OLM is still working on the
//     Subscription's InstallPlan (the first install Argo CD started), Update
//     waits its turn (problem): deleting that Subscription would orphan the
//     half-created CSV.
func updateGitOpsPlan(c *Client, image string, currentCSV types.CSVInfo, logs *[]string) (g operatorGitOps, channelOverride, problem string, err error) {
	g, err = readOperatorGitOps(c)
	if err != nil || !g.active() {
		return g, "", "", err
	}
	app := nonEmpty(g.catalogApp, g.subscriptionApp)
	cs, err := getCatalogSource(c)
	if err != nil {
		return g, "", "", err
	}
	switch {
	case !cs.Exists:
		return g, "", fmt.Sprintf("Argo CD application %q manages the operator, but CatalogSource %s does not exist, so there is no build to install. Create the catalog in Git first. Nothing was changed.", app, CatalogName), nil
	case cs.Image != image && g.catalogApp != "":
		return g, "", fmt.Sprintf("Argo CD application %q sets the image of CatalogSource %s in Git, and it is %s now. Update installs the build that catalog serves; to run %s, change the image in Git, then run Update once the catalog serves it. Nothing was changed.",
			app, CatalogName, cs.Image, image), nil
	case cs.Image != image:
		return g, "", fmt.Sprintf("Argo CD application %q manages the Subscription, so Update only installs the build CatalogSource %s serves, and its image is %s now. To run %s, change the catalog image first. Nothing was changed.",
			app, CatalogName, cs.Image, image), nil
	}
	if g.catalogApp != "" {
		*logs = append(*logs, fmt.Sprintf("Argo CD application %q sets the catalog image in Git: Update installs the build the catalog serves and leaves the catalog as it is", g.catalogApp))
	} else {
		*logs = append(*logs, fmt.Sprintf("Argo CD application %q manages the Subscription: Update installs the build the catalog serves and leaves the catalog as it is", g.subscriptionApp))
	}
	if g.subscriptionApp != "" {
		sub, err := readSubscription(c)
		if err != nil {
			return g, "", "", err
		}
		switch {
		case sub.info.Source != CatalogName:
			return g, "", fmt.Sprintf("Argo CD application %q sets the Subscription's source to %s in Git. Update installs from %s, and Argo CD would switch the source back. Set the source to %s in Git first. Nothing was changed.",
				g.subscriptionApp, nonEmpty(sub.info.Source, "nothing"), CatalogName, CatalogName), nil
		case sub.info.Channel == "":
			return g, "", fmt.Sprintf("The Subscription managed by Argo CD application %q has no channel, so Update cannot keep it. Set the channel in Git first. Nothing was changed.", g.subscriptionApp), nil
		}
		channelOverride = sub.info.Channel
		*logs = append(*logs, fmt.Sprintf("Argo CD application %q manages the Subscription: Update keeps its channel %s and recreates the Subscription only once the old CSV is gone", g.subscriptionApp, channelOverride))
	}
	if currentCSV.Name == "" || currentCSV.Phase == "Not Found" {
		if ip, phase, err := pendingInstallPlan(c); err != nil {
			return g, "", "", err
		} else if ip != "" {
			return g, "", fmt.Sprintf("OLM is still installing the build Git set (InstallPlan %s is %s), and Update would delete the Subscription it works on. Wait for it to finish, or check Diagnostics if it is stuck. Nothing was changed.", ip, nonEmpty(phase, "pending")), nil
		}
	}
	return g, channelOverride, "", nil
}

// pendingInstallPlan returns the Subscription's current InstallPlan and its
// phase while OLM is still working on it ("" once it is Complete or Failed,
// or when there is none).
func pendingInstallPlan(c *Client) (name, phase string, err error) {
	body, _, err := c.get(subscriptionPath())
	if IsK8sError(err, 404) {
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	var sub struct {
		Status struct {
			InstallPlanRef struct {
				Name string `json:"name"`
			} `json:"installPlanRef"`
		} `json:"status"`
	}
	if json.Unmarshal(body, &sub) != nil || sub.Status.InstallPlanRef.Name == "" {
		return "", "", nil
	}
	name = sub.Status.InstallPlanRef.Name
	body, _, err = c.get(namespacedPath("operators.coreos.com/v1alpha1", "installplans", SubNS, name))
	if IsK8sError(err, 404) {
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	var ip struct {
		Status struct {
			Phase string `json:"phase"`
		} `json:"status"`
	}
	if json.Unmarshal(body, &ip) != nil {
		return "", "", nil
	}
	switch ip.Status.Phase {
	case "Complete", "Failed":
		return "", "", nil
	}
	return name, ip.Status.Phase, nil
}

// updateUnderGitOps runs the changing steps of Update when Argo CD manages
// the catalog or the Subscription. The catalog already has the image Git
// chose (updateGitOpsPlan), so it is left alone; Argo CD recreates a managed
// object as soon as it is deleted (automated sync with self-heal), so
// nothing it manages is deleted for longer than one request:
//  1. wait until the catalog serves the verified build (OLM may still be
//     replacing the registry pod after Argo CD changed the image);
//  2. check once more that the catalog image and the Subscription are what
//     was verified, then delete the old InstallPlan and CSV;
//  3. only then is the Subscription deleted and at once created again from
//     its live spec (Git's, as Argo CD last synced it), with the verified
//     channel and Argo CD's annotations and labels. If Argo CD recreates it
//     first, it has the same spec, and the apply below leaves it as it is.
//
// A Subscription is recreated because OLM does not install a deleted CSV
// again for the existing one: it stays UpgradePending while it references an
// InstallPlan (nothingToUpdate in OLM's catalog operator). This is also why a
// nightly build of the same version as the installed one never installs by
// itself: the resolver only follows replaces/skipRange of the installed CSV.
//
// policy, when set (the gitops profile), checks the Subscription Update would
// create and the one the restore would re-apply against the profile's
// admission policy (gitOpsSubscriptionsProblem): before the first deletion,
// and again on the live Subscription before it is deleted.
func updateUnderGitOps(c *Client, image string, target catalogTarget, currentCSV types.CSVInfo, g operatorGitOps,
	recovery *operatorRecovery, logs *[]string, emit func(UpdateStepEvent), fail func(msg, code string) (*types.OperationResponse, error),
	policy func(newSub, restoreSub map[string]interface{}) string,
) (*types.OperationResponse, error) {
	stopped := func(msg string) (*types.OperationResponse, error) {
		return &types.OperationResponse{Success: false, Message: msg, Logs: *logs}, c.ctx.Err()
	}
	app := nonEmpty(g.catalogApp, g.subscriptionApp)

	// --- Step 3: apply_catalog_source (nothing to write) ---
	*logs = append(*logs, "OK: CatalogSource left as Git sets it")
	emit(UpdateStepEvent{Step: "apply_catalog_source", Status: "success", Message: "CatalogSource left as Git sets it", Detail: image})

	// --- Steps 4 and 5: wait_catalog_ready, detect_channel ---
	emit(UpdateStepEvent{Step: "wait_catalog_ready", Status: "running", Message: "Waiting for the catalog to serve the build..."})
	*logs = append(*logs, "Waiting for the catalog to serve the build...")
	serving, err := waitForCatalogServing(c, image, target, app, emit, logs)
	var changed *errCatalogImageChanged
	switch {
	case errors.As(err, &changed):
		return fail("Update stopped: "+changed.Error()+". The operator was not changed.", ErrorCodeGitOpsManaged)
	case err != nil && isCatalogImagePullError(err):
		return fail(err.Error(), "catalog_image_pull")
	case err != nil:
		return stopped("Operation stopped while waiting for the CatalogSource")
	case !serving:
		return fail(fmt.Sprintf("The catalog did not serve %s on channel %s within %s. The operator was not changed.", target.HeadCSV, target.Channel, CatalogReadyTimeout+PackageManifestPropagationWait), "timeout")
	}
	*logs = append(*logs, fmt.Sprintf("OK: The catalog serves %s on channel %s", target.HeadCSV, target.Channel))
	emit(UpdateStepEvent{Step: "wait_catalog_ready", Status: "success", Message: "CatalogSource serves the build"})
	emit(UpdateStepEvent{Step: "detect_channel", Status: "success", Message: fmt.Sprintf("Channel: %s", target.Channel), Detail: target.Channel})

	// Last checks before anything is deleted: Git may have moved on while
	// the catalog was verified.
	if cs, err := getCatalogSource(c); err != nil {
		return fail("Cannot re-read the CatalogSource: "+err.Error(), errorCodeFromK8sErr(err))
	} else if cs.Image != image {
		return fail("Update stopped: "+(&errCatalogImageChanged{app: app, image: cs.Image}).Error()+". The operator was not changed.", ErrorCodeGitOpsManaged)
	}
	// A managed Subscription keeps Git's startingCSV pin; an unmanaged one
	// is rewritten as in any Update (buildSubscription drops the pin).
	startingCSV := ""
	if g.subscriptionApp != "" {
		prevSpec, _ := recovery.subscriptionBase()["spec"].(map[string]interface{})
		startingCSV, _ = prevSpec["startingCSV"].(string)
		if startingCSV != "" && startingCSV != target.HeadCSV {
			return fail(fmt.Sprintf("The Subscription pins startingCSV %s in Git, but the catalog serves %s on channel %s. Change the pin in Git, or the catalog image, so they agree. Nothing was changed.", startingCSV, target.HeadCSV, target.Channel), ErrorCodeGitOpsManaged)
		}
	}

	// The policy holds for the Subscription as Git sets it now: nothing is
	// deleted that could not be written back.
	if policy != nil {
		live, found, err := readSubscriptionObject(c)
		if err != nil {
			return fail("Cannot re-read the Subscription: "+err.Error()+". Nothing was changed.", errorCodeFromK8sErr(err))
		}
		base := recovery.subscriptionBase()
		if found {
			base = live
		}
		preview := buildSubscription(base, CatalogName, target.Channel)
		if msg := policy(preview, recovery.subscription); msg != "" {
			return fail(msg+" Nothing was changed.", ErrorCodeProfileGitOps)
		}
		if found {
			if msg := policy(preview, live); msg != "" {
				return fail(msg+" Nothing was changed.", ErrorCodeProfileGitOps)
			}
		}
	}

	// --- Step 6: delete_csv ---
	emit(UpdateStepEvent{Step: "delete_csv", Status: "running", Message: "Deleting old CSV and InstallPlan..."})
	if ipName := recovery.installPlanName(); ipName != "" {
		ipPath := namespacedPath("operators.coreos.com/v1alpha1", "installplans", SubNS, ipName)
		if _, err := c.delete(ipPath); err != nil && !IsK8sError(err, 404) {
			return fail("Cannot delete old InstallPlan: "+err.Error(), errorCodeFromK8sErr(err))
		}
		*logs = append(*logs, fmt.Sprintf("OK: InstallPlan %s deleted", ipName))
	}
	if uid := recovery.preCSVs[currentCSV.Name]; currentCSV.Name != "" && uid != "" {
		recovery.noteCSVRemoved(currentCSV.Name)
		gone, err := deleteCSVByUIDAndWait(c, rhoaiCSV{Name: currentCSV.Name, UID: uid})
		switch {
		case errors.Is(err, errReplacedMeanwhile):
			// OLM installed it again for the existing Subscription; the
			// new Subscription adopts that CSV.
			*logs = append(*logs, fmt.Sprintf("Note: OLM already installed %s again from the catalog", currentCSV.Name))
		case err != nil && c.ctx.Err() != nil:
			return stopped("Operation stopped while waiting for the current CSV to be deleted")
		case err != nil:
			return fail("Cannot delete current CSV: "+err.Error(), errorCodeFromK8sErr(err))
		case gone:
			*logs = append(*logs, "OK: CSV "+currentCSV.Name+" deleted")
		default:
			*logs = append(*logs, fmt.Sprintf("Warning: CSV %s is still being deleted after %s; continuing", currentCSV.Name, CSVDeletionTimeout))
		}
	}
	emit(UpdateStepEvent{Step: "delete_csv", Status: "success", Message: "Old CSV and InstallPlan deleted"})

	// --- Step 7: apply_subscription ---
	emit(UpdateStepEvent{Step: "apply_subscription", Status: "running", Message: "Recreating the Subscription..."})
	// The live Subscription is Git's spec as Argo CD last synced it, which
	// may be newer than the one read at the start; the recreated one must
	// still install what was verified. When Git moved it elsewhere
	// meanwhile, the restore recreates it with Git's current settings (the
	// old CSV is gone, and OLM installs nothing for the existing one).
	base := recovery.subscriptionBase()
	live, found, err := readSubscriptionObject(c)
	if err != nil {
		return fail("Cannot re-read the Subscription: "+err.Error(), errorCodeFromK8sErr(err))
	}
	if found && policy != nil {
		// Git changed the Subscription after the check above to settings the
		// profile may not write: deleting it now could not be undone, and
		// the restore would delete it too, so both are skipped.
		if msg := policy(buildSubscription(live, CatalogName, target.Channel), live); msg != "" {
			recovery.keepNewInstall = true
			return fail("The Subscription changed while Update ran. "+msg+" The old CSV was already deleted and the Subscription was left as Git sets it, so OLM does not install the operator again until the Subscription is fixed in Git (Argo CD then syncs it) or recreated.", ErrorCodeProfileGitOps)
		}
	}
	if found {
		liveSpec, _ := live["spec"].(map[string]interface{})
		if g.subscriptionApp != "" {
			liveStartingCSV, _ := liveSpec["startingCSV"].(string)
			switch {
			case liveSpec["source"] != CatalogName || liveSpec["channel"] != target.Channel:
				recovery.subscription = live
				return fail(fmt.Sprintf("The Subscription changed while Update ran (source %v, channel %v; Argo CD may have synced a change from Git). It is recreated with Git's current settings, so OLM installs that channel's build; check the Status page, then run Update if needed.", liveSpec["source"], liveSpec["channel"]), "conflict")
			case liveStartingCSV != "" && liveStartingCSV != target.HeadCSV:
				recovery.subscription = live
				return fail(fmt.Sprintf("The Subscription now pins startingCSV %s in Git, but the catalog serves %s on channel %s. It is recreated with Git's current settings; make the pin and the catalog image agree in Git, then run Update again.", liveStartingCSV, target.HeadCSV, target.Channel), "conflict")
			}
			startingCSV = liveStartingCSV
		}
		base = live
	}
	newSub := buildSubscription(base, CatalogName, target.Channel)
	if startingCSV != "" {
		newSub["spec"].(map[string]interface{})["startingCSV"] = startingCSV
	}
	keepArgoTracking(base, newSub)
	recovery.subscriptionChanged = true
	if found {
		// Delete exactly the object just validated: a replacement with
		// another UID is left alone.
		liveMeta, _ := live["metadata"].(map[string]interface{})
		liveUID, _ := liveMeta["uid"].(string)
		if err := deleteSubscriptionWithUID(c, liveUID); errors.Is(err, errReplacedMeanwhile) {
			recovery.subscriptionReplaced = true
			return fail("The Subscription was replaced by someone else while Update ran, so it was left unchanged. Check it before running Update again.", "conflict")
		} else if err != nil {
			return fail("Cannot remove the current Subscription: "+err.Error(), errorCodeFromK8sErr(err))
		}
	}
	if r, err := applyUpdateSubscription(c, newSub, target.Channel, logs, emit, fail); r != nil || err != nil {
		return r, err
	}
	return verifyUpdateInstall(c, target, recovery, logs, emit)
}

// readSubscriptionObject reads the operator Subscription as a decoded object.
func readSubscriptionObject(c *Client) (obj map[string]interface{}, found bool, err error) {
	body, _, err := c.get(subscriptionPath())
	if IsK8sError(err, 404) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, false, fmt.Errorf("parse Subscription: %w", err)
	}
	return obj, true, nil
}
