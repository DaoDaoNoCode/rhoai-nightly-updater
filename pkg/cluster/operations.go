package cluster

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// parseCSVVersion extracts a parsedTag from a CSV name like "rhods-operator.3.5.0-ea.1".
// It handles three cases:
//  1. Full EA suffix:       "rhods-operator.3.5.0-ea.1" -> ea=1
//  2. GA (no suffix):       "rhods-operator.3.5.0"      -> ea=-1 (ranks above EA)
//  3. Non-EA pre-release:   "rhods-operator.3.5.0-rc.1" -> ea=-1000 (ranks below GA and EA)
//
// Returns the parsed version and true on success, or zero value and false if unparseable.
func parseCSVVersion(csv string) (parsedTag, bool) {
	// Normalize: strip optional "v" prefix from version portion.
	// CSV names may use "rhods-operator.v3.5.0" or "rhods-operator.3.5.0".
	normalized := csv
	dotIdx := strings.Index(csv, ".")
	if dotIdx >= 0 {
		after := csv[dotIdx+1:]
		if strings.HasPrefix(after, "v") {
			normalized = csv[:dotIdx+1] + after[1:]
		}
	}

	var major, minor, patch, eaNum int
	// Try full EA parse: "rhods-operator.M.m.p-ea.N"
	n, _ := fmt.Sscanf(normalized, "rhods-operator.%d.%d.%d-ea.%d", &major, &minor, &patch, &eaNum)
	if n == 4 {
		return parsedTag{raw: csv, major: major, minor: minor, patch: patch, ea: eaNum}, true
	}
	// Recognize unnumbered EA and build metadata using the same parser as release tags.
	version := strings.TrimPrefix(normalized, "rhods-operator.")
	if strings.HasPrefix(normalized, "rhods-operator.") {
		if parsed, ok := parseTag("rhoai-" + strings.SplitN(version, "+", 2)[0]); ok {
			parsed.raw = csv
			return parsed, true
		}
	}

	// Try GA parse (no suffix): "rhods-operator.M.m.p"
	n, _ = fmt.Sscanf(normalized, "rhods-operator.%d.%d.%d", &major, &minor, &patch)
	if n == 3 {
		// Check if the normalized CSV has any suffix after the patch number.
		// Build the canonical GA prefix and see if the normalized string is longer.
		gaPrefix := fmt.Sprintf("rhods-operator.%d.%d.%d", major, minor, patch)
		remainder := strings.TrimPrefix(normalized, gaPrefix)
		if strings.HasPrefix(remainder, "-") {
			// Has a non-EA suffix (e.g., "-rc.1", "-beta.2") — treat as pre-release,
			// ranking below both GA (ea=-1) and any EA build (ea>=0).
			slog.Debug("CSV has non-EA pre-release suffix, ranking below GA and EA",
				"csv", csv, "suffix", remainder)
			return parsedTag{raw: csv, major: major, minor: minor, patch: patch, ea: -1000}, true
		}
		// Genuine GA — no suffix at all.
		return parsedTag{raw: csv, major: major, minor: minor, patch: patch, ea: -1}, true
	}

	slog.Debug("failed to parse CSV version", "csv", csv)
	return parsedTag{}, false
}

// extractTargetVersion parses the target major.minor from an FBC image tag.
// e.g., "quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.5@sha256:..." → (3, 5, true)
func extractTargetVersion(imageRef string) (int, int, bool) {
	// Extract the tag portion: "rhoai-3.5" from the full image ref
	ref := imageRef
	if atIdx := strings.Index(ref, "@"); atIdx >= 0 {
		ref = ref[:atIdx]
	}
	colonIdx := strings.LastIndex(ref, ":")
	if colonIdx < 0 {
		return 0, 0, false
	}
	tag := ref[colonIdx+1:]

	var major, minor int
	n, _ := fmt.Sscanf(tag, "rhoai-%d.%d", &major, &minor)
	if n == 2 {
		return major, minor, true
	}
	return 0, 0, false
}

// detectNightlyChannel queries the nightly catalog's packagemanifest and picks
// the best channel for the given FBC image. It filters channels to the target
// major.minor (extracted from the image tag), excludes lifecycle channels
// (eus-*, support-required-upgrade-*), and picks the highest version with a
// deterministic tiebreaker: stable-M.x > stable-M.N > beta > fast > other.
func detectNightlyChannel(c *Client, imageRef string) (string, error) {
	channels, err := catalogChannels(c, CatalogName)
	if err != nil {
		return "", err
	}
	return detectBestChannel(channels, imageRef)
}

func detectBestChannel(channels []interface{}, imageRef string) (string, error) {
	if len(channels) == 0 {
		slog.Warn("detectNightlyChannel: no channels found in packagemanifest")
		return "", nil
	}

	targetMajor, targetMinor, hasTarget := extractTargetVersion(imageRef)
	targetRelease, hasRelease := parseTag(extractTagFromRef(imageRef))
	if hasTarget {
		slog.Info("detectNightlyChannel: filtering to target version", "major", targetMajor, "minor", targetMinor, "image", imageRef)
	}

	bestChannel := ""
	var bestParsed *parsedTag
	for _, ch := range channels {
		channel, _ := ch.(map[string]interface{})
		name, _ := channel["name"].(string)
		csv, _ := channel["currentCSV"].(string)

		// Skip lifecycle channels that aren't for nightly dev use
		if strings.HasPrefix(name, "eus-") || strings.HasPrefix(name, "support-required-upgrade") {
			continue
		}

		parsed, ok := parseCSVVersion(csv)
		if !ok {
			slog.Debug("skipping channel with unparseable CSV", "channel", name, "csv", csv)
			continue
		}

		// If we know the target version, only consider channels with matching major.minor
		if hasTarget && (parsed.major != targetMajor || parsed.minor != targetMinor) {
			continue
		}

		if hasRelease && parsed.ea != targetRelease.ea {
			continue
		}

		cmp := 0
		if bestParsed != nil {
			cmp = compareTags(parsed, *bestParsed)
		}

		if bestParsed == nil || cmp > 0 || (cmp == 0 && channelPriority(name, parsed.ea) > channelPriority(bestChannel, bestParsed.ea)) {
			bestParsed = &parsed
			bestChannel = name
		}
	}

	if bestChannel == "" {
		slog.Warn("detectNightlyChannel: no matching channels found", "targetMajor", targetMajor, "targetMinor", targetMinor)
	} else {
		slog.Info("detected nightly channel", "channel", bestChannel, "csv", bestParsed.raw)
	}

	return bestChannel, nil
}

// channelPriority returns a numeric priority for tiebreaking when two channels
// have the same version. Higher = preferred.
//
// For GA releases (ea < 0): stable-M.x > stable-M.N > beta > everything else.
// For EA releases (ea >= 0): beta > stable-M.x > stable-M.N > everything else.
func channelPriority(name string, ea int) int {
	isEA := ea >= 0
	switch {
	case name == "beta":
		if isEA {
			return 40
		}
		return 20
	case strings.HasPrefix(name, "stable-") && strings.HasSuffix(name, ".x"):
		if isEA {
			return 30
		}
		return 40
	case strings.HasPrefix(name, "stable-"):
		if isEA {
			return 25
		}
		return 35
	case strings.HasPrefix(name, "fast"):
		return 10
	default:
		return 0
	}
}

// channelExistsInCatalog checks whether the given channel name exists in the
// nightly catalog's packagemanifest. It returns (true, nil) if the channel is
// found, (false, nil) if the catalog has no such channel, and (false, err) on
// transient API / parse failures.
func channelExistsInCatalog(c *Client, channel string) (bool, error) {
	channels, err := catalogChannels(c, CatalogName)
	if err != nil {
		return false, err
	}
	for _, entry := range channels {
		ch, _ := entry.(map[string]interface{})
		if ch["name"] == channel {
			return true, nil
		}
	}
	return false, nil
}

// Timing constants used by multi-step operations. Package-level vars so tests
// can override them with shorter durations to avoid real-time waits.
var (
	CatalogReadyTimeout            = 120 * time.Second
	CatalogPollInterval            = 5 * time.Second
	InstallPlanPollTimeout         = 60 * time.Second
	InstallPlanPollInterval        = 5 * time.Second
	PropagationWait                = 10 * time.Second
	RefreshCleanupWait             = 5 * time.Second
	SubRetryBackoffs               = []time.Duration{2 * time.Second, 4 * time.Second}
	ChannelRetryDelay              = 8 * time.Second
	PackageManifestPropagationWait = 30 * time.Second
)

// buildCatalogSourceSpec returns the nightly CatalogSource for an FBC image.
func buildCatalogSourceSpec(image string) map[string]interface{} {
	return map[string]interface{}{
		"apiVersion": "operators.coreos.com/v1alpha1",
		"kind":       "CatalogSource",
		"metadata": map[string]interface{}{
			"name":      CatalogName,
			"namespace": CatalogNS,
		},
		"spec": map[string]interface{}{
			"sourceType":  "grpc",
			"image":       image,
			"displayName": "RHOAI Development Catalog",
			"publisher":   "RHOAI DevOps",
			"grpcPodConfig": map[string]interface{}{
				"securityContextConfig": "restricted",
			},
		},
	}
}

// UpdateStepEvent represents a single step in the update pipeline.
// Defined here so operations.go doesn't depend on the api package.
type UpdateStepEvent struct {
	Step      string `json:"step"`
	Status    string `json:"status"` // "running", "success", "failed", "skipped"
	Message   string `json:"message"`
	Detail    string `json:"detail,omitempty"`
	ErrorCode string `json:"errorCode,omitempty"`
}

// waitForNightlyCatalogReady polls the nightly CatalogSource until it reports
// READY, logging and emitting each observed state. The PackageManifest will not
// reflect new catalog content until the catalog pod serves it, so callers wait
// for READY before detecting channels. It returns false on timeout. The error
// is the context error if the operation is canceled, or a
// *catalogImagePullError when the catalog pod cannot pull image.
func waitForNightlyCatalogReady(c *Client, image string, emit func(UpdateStepEvent), logs *[]string) (bool, error) {
	deadline := time.Now().Add(CatalogReadyTimeout)
	var pull pullFailureWatch
	for time.Now().Before(deadline) {
		select {
		case <-c.ctx.Done():
			return false, c.ctx.Err()
		case <-time.After(CatalogPollInterval):
		}

		cs, err := getCatalogSource(c)
		if err != nil {
			slog.Warn("error polling CatalogSource state", "error", err)
			continue
		}
		*logs = append(*logs, fmt.Sprintf("  CatalogSource state: %s", cs.State))
		emit(UpdateStepEvent{Step: "wait_catalog_ready", Status: "running", Message: fmt.Sprintf("CatalogSource: %s", cs.State), Detail: cs.State})
		if cs.State == "READY" {
			return true, nil
		}
		if err := pull.check(c, CatalogName, image); err != nil {
			return false, err
		}
	}
	return false, nil
}

// waitForNightlyPackageManifest waits until the nightly catalog's
// PackageManifest lists a channel for image, because a CatalogSource reports
// READY before OLM refreshes its PackageManifest. A timeout is logged and the
// caller proceeds; the context error is returned if the operation is canceled.
func waitForNightlyPackageManifest(c *Client, image string, logs *[]string) error {
	start := time.Now()
	for time.Since(start) < PackageManifestPropagationWait {
		if err := c.ctx.Err(); err != nil {
			return err
		}
		if ch, _ := detectNightlyChannel(c, image); ch != "" {
			*logs = append(*logs, fmt.Sprintf("OK: PackageManifest ready in %s (channel: %s)", time.Since(start).Round(time.Second), ch))
			return nil
		}
		select {
		case <-c.ctx.Done():
			return c.ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	*logs = append(*logs, fmt.Sprintf("Warning: PackageManifest not ready after %s, proceeding with detection", PackageManifestPropagationWait))
	return nil
}

// applySubscriptionWithRetry server-side applies sub up to three times,
// waiting SubRetryBackoffs between attempts. onFailure is called after every
// failed attempt, with willRetry false for the last one. It returns the last
// apply error, or the context error if canceled while waiting to retry.
func applySubscriptionWithRetry(c *Client, path string, sub map[string]interface{}, onFailure func(attempt int, err error, willRetry bool, backoff time.Duration)) (applyErr, cancelErr error) {
	const attempts = 3
	for attempt := 1; attempt <= attempts; attempt++ {
		_, _, applyErr = c.apply(path, sub)
		if applyErr == nil {
			return nil, nil
		}
		willRetry := attempt < attempts
		var backoff time.Duration
		if willRetry {
			backoff = SubRetryBackoffs[attempt-1]
		}
		onFailure(attempt, applyErr, willRetry, backoff)
		if !willRetry {
			break
		}
		select {
		case <-c.ctx.Done():
			return applyErr, c.ctx.Err()
		case <-time.After(backoff):
		}
	}
	return applyErr, nil
}

// errorCodeFromK8sErr maps a K8s API error to a user-facing error code string.
func errorCodeFromK8sErr(err error) string {
	if IsNetworkError(err) {
		return "network"
	}
	if IsK8sError(err, 401) {
		return "unauthorized"
	}
	if IsK8sError(err, 403) {
		return "forbidden"
	}
	return ""
}

// VerifyNodeReadiness checks that the pull secret credentials can authenticate
// to quay.io/rhoai and that a known nightly image is accessible.
// This is a server-side registry API check, not a pod-level test.
func VerifyNodeReadiness(c *Client) (*types.OperationResponse, error) {
	logs := []string{}

	// Step 1: Check pull secret exists and is valid
	ps, err := getPullSecret(c)
	if err != nil || !ps.Exists {
		return &types.OperationResponse{
			Success:   false,
			Message:   "Pull secret is missing or invalid.",
			Logs:      []string{"additional-pull-secret not found in kube-system"},
			ErrorCode: "prerequisites",
		}, nil
	}
	if !ps.Valid {
		return &types.OperationResponse{
			Success:   false,
			Message:   fmt.Sprintf("Pull secret exists but is invalid: %s", ps.Detail),
			Logs:      []string{ps.Detail},
			ErrorCode: "validation",
		}, nil
	}
	logs = append(logs, "OK: Pull secret exists and contains quay.io/rhoai credentials")

	// Step 2: Check IDMS exists
	idms, err := getIDMS(c)
	if err != nil || !idms.Exists {
		return &types.OperationResponse{
			Success:   false,
			Message:   "Image mirror (IDMS) is not configured.",
			Logs:      logs,
			ErrorCode: "prerequisites",
		}, nil
	}
	logs = append(logs, fmt.Sprintf("OK: IDMS exists (%s -> quay.io/rhoai)", idms.Source))

	// Step 3: Test registry access by fetching a manifest from quay.io/rhoai
	logs = append(logs, "Testing registry access to quay.io/rhoai...")
	quayAuth := getQuayAuth(c)
	if quayAuth == "" {
		return &types.OperationResponse{
			Success:   false,
			Message:   "Could not extract quay.io/rhoai credentials from pull secret.",
			Logs:      logs,
			ErrorCode: "validation",
		}, nil
	}

	httpClient := &http.Client{Timeout: 15 * time.Second}
	bearerToken, err := getQuayBearerToken(c.ctx, httpClient, quayAuth)
	if err != nil {
		return &types.OperationResponse{
			Success:   false,
			Message:   "Failed to authenticate with quay.io/rhoai. Credentials may be expired.",
			Logs:      append(logs, fmt.Sprintf("Auth error: %v", err)),
			ErrorCode: "unauthorized",
		}, nil
	}
	logs = append(logs, "OK: Authenticated with quay.io/rhoai")

	// Step 4: Try listing tags to confirm full access
	req, _ := http.NewRequestWithContext(c.ctx, "GET", "https://quay.io/v2/rhoai/rhoai-fbc-fragment/tags/list?n=1", nil)
	req.Header.Set("Authorization", "Bearer "+bearerToken)
	resp, err := httpClient.Do(req)
	if err != nil {
		return &types.OperationResponse{
			Success:   false,
			Message:   "Cannot reach quay.io registry.",
			Logs:      append(logs, fmt.Sprintf("Network error: %v", err)),
			ErrorCode: "network",
		}, nil
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return &types.OperationResponse{
			Success:   false,
			Message:   fmt.Sprintf("Registry access check failed (HTTP %d). Credentials may not have pull access.", resp.StatusCode),
			Logs:      logs,
			ErrorCode: "unauthorized",
		}, nil
	}
	logs = append(logs, "OK: Registry access confirmed — can pull from quay.io/rhoai")
	logs = append(logs, "Note: After initial setup, allow 2-3 minutes for credentials to propagate to all cluster nodes.")

	return &types.OperationResponse{
		Success: true,
		Message: "Cluster readiness verified. Pull credentials and image mirror are configured correctly.",
		Logs:    logs,
	}, nil
}

// UpdateStream executes the update pipeline, emitting progress events via the
// emit callback so callers can stream status to SSE clients. It returns the
// final OperationResponse with all collected logs; it reports success only
// once OLM has installed the new CSV.
//
// Order of changes, each recoverable by re-running Update:
//  1. Verify the image in a temporary catalog and refuse downgrades.
//  2. Delete the Subscription. The operator keeps running (deleting a
//     Subscription never removes its CSV), and OLM cannot start an in-place
//     upgrade while the catalog is swapped underneath it.
//  3. Replace the nightly catalog and wait until it serves the target.
//  4. Delete the old CSV and InstallPlan, then create the new Subscription,
//     which carries over the previous spec.config and approval mode.
//  5. Wait for OLM's verdict; on failure the previous state is restored.
func UpdateStream(c *Client, image string, emit func(UpdateStepEvent)) (*types.OperationResponse, error) {
	return UpdateStreamWithOptions(c, image, OperationOptions{}, emit)
}

// UpdateStreamWithOptions is UpdateStream with caller confirmations; see
// OperationOptions.
func UpdateStreamWithOptions(c *Client, image string, opts OperationOptions, emit func(UpdateStepEvent)) (result *types.OperationResponse, opErr error) {
	logs := []string{}
	tracker := &stepTracker{emit: emit}
	emit = tracker.send
	var recovery *operatorRecovery
	defer func() {
		tracker.finish(c, &result, &opErr, logs, recovery, func(ok bool) { recordUpdateActivity(c, image, ok) })
	}()
	fail := func(msg, code string) (*types.OperationResponse, error) {
		logs = append(logs, msg)
		return &types.OperationResponse{Success: false, Message: msg, Logs: logs, ErrorCode: code}, nil
	}

	logs = append(logs, fmt.Sprintf("Target image: %s", image))

	// --- Step 1: validate_prerequisites ---
	emit(UpdateStepEvent{Step: "validate_prerequisites", Status: "running", Message: "Checking prerequisites..."})
	refusal, revertDashboard, guardErr := dashboardDevGuard(c, opts, "Update")
	if guardErr != nil {
		return fail(guardErr.Error(), errorCodeFromK8sErr(guardErr))
	}
	if refusal != "" {
		return fail(refusal, errorCodeDashboardDevActive)
	}
	if !revertDashboard {
		if note := dashboardPRNote(c); note != "" {
			logs = append(logs, note)
		}
	}

	ps, err := getPullSecret(c)
	if err != nil {
		return fail(fmt.Sprintf("Failed to check pull secret: %v", err), errorCodeFromK8sErr(err))
	}
	if !ps.Exists {
		return fail("additional-pull-secret not found in kube-system. Run the one-time setup first.", "prerequisites")
	}
	if !ps.Valid {
		if !strings.HasPrefix(ps.Detail, "Could not verify credentials with Quay") {
			return fail(fmt.Sprintf("The pull secret cannot pull from quay.io/rhoai: %s. Fix it in the one-time setup first. Nothing was changed.", ps.Detail), "prerequisites")
		}
		// Quay could not be reached; the catalog preflight below shows
		// whether the cluster itself can pull the image.
		logs = append(logs, "Warning: "+ps.Detail)
	} else {
		logs = append(logs, "OK: additional-pull-secret exists and Quay accepts its credentials")
	}

	idms, err := getIDMS(c)
	if err != nil {
		return fail(fmt.Sprintf("Failed to check IDMS: %v", err), errorCodeFromK8sErr(err))
	}
	if !idms.Exists {
		return fail(fmt.Sprintf("No IDMS found mirroring %s. Run the one-time setup first.", IDMSSource), "prerequisites")
	}
	logs = append(logs, fmt.Sprintf("OK: IDMS exists (%s)", idms.Name))

	ogLogs, ogProblem, ogCode := ensureOperatorNamespaceAndGroup(c, false)
	logs = append(logs, ogLogs...)
	if ogProblem != "" {
		return fail(ogProblem, ogCode)
	}

	currentCSV, csvCheckErr := getCSV(c)
	if csvCheckErr != nil {
		return fail("Cannot identify installed CSV: "+csvCheckErr.Error(), "prerequisites")
	}

	// Verify the exact image in a fresh catalog before switching the active
	// catalog or removing any installed resources. The channel head it
	// reports is the bundle OLM will install, so the downgrade check uses it.
	emit(UpdateStepEvent{Step: "validate_prerequisites", Status: "running", Message: "Verifying the selected catalog image..."})
	target, preflightErr := preflightReinstallCatalog(c, image, "")
	if preflightErr != nil {
		if c.ctx.Err() != nil {
			return &types.OperationResponse{Success: false, Message: "Operation stopped while verifying the catalog image", Logs: logs}, c.ctx.Err()
		}
		if isCatalogImagePullError(preflightErr) {
			return fail("Replacement catalog validation failed: "+preflightErr.Error()+". Nothing was changed.", "catalog_image_pull")
		}
		return fail("Replacement catalog validation failed: "+preflightErr.Error()+". Nothing was changed.", "validation")
	}
	logs = append(logs, fmt.Sprintf("OK: Catalog image verified (channel %s, head %s)", target.Channel, target.HeadCSV))
	blocked, note := downgradeCheck(currentCSV, extractTagFromRef(image), target.HeadCSV)
	if note != "" {
		logs = append(logs, note)
	}
	if blocked != "" {
		return fail(blocked, "validation")
	}
	if revertDashboard {
		emit(UpdateStepEvent{Step: "validate_prerequisites", Status: "running", Message: "Ending the Dashboard Dev session..."})
		if err := RevertDashboardDevForOperation(c); err != nil {
			return fail("Could not end the Dashboard Dev session, so the update was not started: "+err.Error(), errorCodeDashboardDevActive)
		}
		logs = append(logs, "OK: Dashboard Dev session ended; dashboard-operator is running again")
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

	captured, recoveryErr := captureOperatorRecovery(c)
	if recoveryErr != nil {
		return fail(recoveryErr.Error()+". Nothing was changed.", "prerequisites")
	}
	recovery = captured

	// --- Step 3: apply_catalog_source ---
	emit(UpdateStepEvent{Step: "apply_catalog_source", Status: "running", Message: "Applying CatalogSource..."})
	if recovery.subscription != nil {
		recovery.subscriptionChanged = true
		if _, err := c.delete(subscriptionPath()); err != nil && !IsK8sError(err, 404) {
			return fail("Cannot remove the current Subscription: "+err.Error(), errorCodeFromK8sErr(err))
		}
		logs = append(logs, "OK: Subscription removed while the catalog is replaced (the operator keeps running)")
	}

	logs = append(logs, fmt.Sprintf("Applying CatalogSource %s...", CatalogName))
	csPath := namespacedPath("operators.coreos.com/v1alpha1", "catalogsources", CatalogNS, CatalogName)
	recovery.catalogChanged = true
	// Recreate this source so READY and PackageManifest cannot describe its
	// previous image. The verified temporary catalog remains separate.
	if _, deleteErr := c.delete(csPath); deleteErr != nil && !IsK8sError(deleteErr, 404) {
		return fail("Cannot replace nightly catalog: "+deleteErr.Error(), errorCodeFromK8sErr(deleteErr))
	}
	if _, _, err := c.apply(csPath, buildCatalogSourceSpec(image)); err != nil {
		return fail(fmt.Sprintf("Failed to apply CatalogSource: %v", err), errorCodeFromK8sErr(err))
	}
	logs = append(logs, "OK: CatalogSource applied")
	emit(UpdateStepEvent{Step: "apply_catalog_source", Status: "success", Message: "CatalogSource applied", Detail: image})

	// --- Step 4: wait_catalog_ready (wait before detecting the channel) ---
	emit(UpdateStepEvent{Step: "wait_catalog_ready", Status: "running", Message: "Waiting for CatalogSource to become READY..."})
	logs = append(logs, "Waiting for CatalogSource to become READY...")
	catalogReady, catalogErr := waitForNightlyCatalogReady(c, image, emit, &logs)
	if msg, code, stopErr := catalogWaitFailure(catalogReady, catalogErr); msg != "" {
		r, _ := fail(msg, code)
		return r, stopErr
	}
	logs = append(logs, "OK: CatalogSource is READY")
	emit(UpdateStepEvent{Step: "wait_catalog_ready", Status: "success", Message: "CatalogSource is READY"})

	// Poll PackageManifest until the nightly catalog's channels appear.
	// The CatalogSource reports READY before OLM refreshes the PackageManifest.
	emit(UpdateStepEvent{Step: "detect_channel", Status: "running", Message: "Waiting for catalog channels to refresh..."})
	if err := waitForNightlyPackageManifest(c, image, &logs); err != nil {
		return &types.OperationResponse{Success: false, Message: "Operation stopped while waiting for the catalog channels", Logs: logs}, err
	}

	// --- Step 5: detect_channel ---
	emit(UpdateStepEvent{Step: "detect_channel", Status: "running", Message: "Detecting target channel..."})
	logs = append(logs, "Detecting target channel...")
	var nightlyChannel string
	var detectErr error
	for attempt := 1; attempt <= 3; attempt++ {
		nightlyChannel, detectErr = detectNightlyChannel(c, image)
		if detectErr == nil {
			break
		}
		slog.Warn("detectNightlyChannel failed", "attempt", attempt, "error", detectErr)
		if attempt < 3 {
			select {
			case <-c.ctx.Done():
				return &types.OperationResponse{Success: false, Message: "Operation stopped while detecting the channel", Logs: logs}, c.ctx.Err()
			case <-time.After(ChannelRetryDelay):
			}
		}
	}
	if detectErr != nil {
		return fail(fmt.Sprintf("Failed to detect nightly channel after 3 attempts: %v. The operator was not changed.", detectErr), "channel_detection")
	}
	if nightlyChannel == "" {
		return fail("No matching channel found in the selected nightly catalog. The operator was not changed.", "channel_detection")
	}
	logs = append(logs, fmt.Sprintf("Detected nightly channel: %s", nightlyChannel))
	emit(UpdateStepEvent{Step: "detect_channel", Status: "success", Message: fmt.Sprintf("Detected channel: %s", nightlyChannel), Detail: nightlyChannel})

	// --- Step 6: delete_csv ---
	// Delete the CSV and its InstallPlan so OLM installs the target as a
	// fresh CSV; a same-version build is not an upgrade OLM would perform.
	emit(UpdateStepEvent{Step: "delete_csv", Status: "running", Message: "Deleting old CSV and InstallPlan..."})
	if ipName := recovery.installPlanName(); ipName != "" {
		ipPath := namespacedPath("operators.coreos.com/v1alpha1", "installplans", SubNS, ipName)
		if _, err := c.delete(ipPath); err != nil && !IsK8sError(err, 404) {
			return fail("Cannot delete old InstallPlan: "+err.Error(), errorCodeFromK8sErr(err))
		}
		logs = append(logs, fmt.Sprintf("OK: InstallPlan %s deleted", ipName))
	}
	if currentCSV.Name != "" {
		if _, err := c.delete(csvPath(currentCSV.Name)); err != nil && !IsK8sError(err, 404) {
			return fail("Cannot delete current CSV: "+err.Error(), errorCodeFromK8sErr(err))
		}
		recovery.csvRemoved = true
		gone, err := waitForCSVGone(c, currentCSV.Name)
		if err != nil {
			return &types.OperationResponse{Success: false, Message: "Operation stopped while waiting for the current CSV to be deleted", Logs: logs}, err
		}
		if gone {
			logs = append(logs, "OK: CSV "+currentCSV.Name+" deleted")
		} else {
			logs = append(logs, fmt.Sprintf("Warning: CSV %s is still being deleted after %s; continuing", currentCSV.Name, CSVDeletionTimeout))
		}
	}
	emit(UpdateStepEvent{Step: "delete_csv", Status: "success", Message: "Old CSV and InstallPlan deleted"})

	// --- Step 7: apply_subscription ---
	emit(UpdateStepEvent{Step: "apply_subscription", Status: "running", Message: "Creating Subscription..."})
	newSub := buildSubscription(recovery.subscription, CatalogName, nightlyChannel)
	recovery.subscriptionChanged = true
	subApplyErr, cancelErr := applySubscriptionWithRetry(c, subscriptionPath(), newSub, func(attempt int, err error, willRetry bool, _ time.Duration) {
		if willRetry {
			slog.Warn("failed to apply Subscription, retrying", "attempt", attempt, "error", err)
			logs = append(logs, fmt.Sprintf("  Warning: Subscription apply attempt %d failed: %v — retrying", attempt, err))
		}
	})
	if cancelErr != nil {
		return &types.OperationResponse{Success: false, Message: "Operation stopped during Subscription apply retry", Logs: logs}, cancelErr
	}
	if subApplyErr != nil {
		return fail(fmt.Sprintf("Failed to apply Subscription after 3 attempts: %v", subApplyErr), errorCodeFromK8sErr(subApplyErr))
	}
	logs = append(logs, fmt.Sprintf("OK: Subscription applied (channel=%s, installPlanApproval=%v)", nightlyChannel, newSub["spec"].(map[string]interface{})["installPlanApproval"]))
	emit(UpdateStepEvent{Step: "apply_subscription", Status: "success", Message: "Subscription applied", Detail: nightlyChannel})

	// --- Step 8: verify_installplan (wait for OLM's result) ---
	emit(UpdateStepEvent{Step: "verify_installplan", Status: "running", Message: "Waiting for OLM to install the operator..."})
	logs = append(logs, "Waiting for OLM to install the operator...")
	outcome := waitForOperatorInstall(c, "verify_installplan", emit, &logs, recovery)
	if !outcome.succeeded {
		recovery.keepNewInstall = outcome.keepNewInstall
		return &types.OperationResponse{Success: false, Message: outcome.message, Logs: logs, ErrorCode: outcome.errorCode}, nil
	}
	emit(UpdateStepEvent{Step: "verify_installplan", Status: "success", Message: fmt.Sprintf("Operator installed: %s", outcome.csv), Detail: outcome.csv})
	logs = append(logs, "Update complete. The operator now reconciles the DataScienceCluster; component rollouts can take a few more minutes.")
	return &types.OperationResponse{
		Success: true,
		Message: strings.TrimSpace(fmt.Sprintf("RHOAI nightly update complete: %s is installed. %s", outcome.csv, outcome.note)),
		Logs:    logs,
	}, nil
}

// catalogWaitFailure describes why waitForNightlyCatalogReady did not reach
// READY. msg is empty when the catalog is ready; stopErr is set when the
// operation's context ended.
func catalogWaitFailure(ready bool, err error) (msg, code string, stopErr error) {
	switch {
	case err != nil && isCatalogImagePullError(err):
		return err.Error(), "catalog_image_pull", nil
	case err != nil:
		return "Operation stopped while waiting for the CatalogSource", "", err
	case !ready:
		return fmt.Sprintf("CatalogSource did not reach READY state within %s", CatalogReadyTimeout), "timeout", nil
	}
	return "", "", nil
}

// Update applies a nightly CatalogSource image and patches the Subscription accordingly.
// For non-dry-run calls it delegates to UpdateStream with a no-op emitter that collects logs.
func Update(c *Client, image string, dryRun bool) (*types.OperationResponse, error) {
	if !dryRun {
		// Delegate to UpdateStream with a silent emitter — all progress is
		// captured in the returned OperationResponse.Logs.
		return UpdateStream(c, image, func(UpdateStepEvent) {})
	}

	// --- Dry-run path: read-only checks plus server-side dry-run applies ---
	logs := []string{}

	if active, err := DashboardDevActive(c); err == nil && active {
		logs = append(logs, "[DRY-RUN] WARNING: A Dashboard Dev session is active. Update refuses to run unless it is asked to end the session first (revertDashboardDev).")
	} else if note := dashboardPRNote(c); note != "" {
		logs = append(logs, "[DRY-RUN] "+note)
	}

	logs = append(logs, fmt.Sprintf("Target image: %s", image))
	logs = append(logs, "[DRY-RUN] Checking prerequisites...")

	ps, err := getPullSecret(c)
	if err != nil {
		return &types.OperationResponse{
			Success:   false,
			Message:   fmt.Sprintf("Failed to check pull secret: %v", err),
			Logs:      logs,
			ErrorCode: errorCodeFromK8sErr(err),
		}, nil
	}
	if !ps.Exists {
		return &types.OperationResponse{
			Success:   false,
			Message:   "additional-pull-secret not found in kube-system. Run the one-time setup first.",
			Logs:      logs,
			ErrorCode: "prerequisites",
		}, nil
	}
	logs = append(logs, "OK: additional-pull-secret exists")

	idms, err := getIDMS(c)
	if err != nil {
		return &types.OperationResponse{
			Success:   false,
			Message:   fmt.Sprintf("Failed to check IDMS: %v", err),
			Logs:      logs,
			ErrorCode: errorCodeFromK8sErr(err),
		}, nil
	}
	if !idms.Exists {
		return &types.OperationResponse{
			Success:   false,
			Message:   fmt.Sprintf("No IDMS found mirroring %s. Run the one-time setup first.", IDMSSource),
			Logs:      logs,
			ErrorCode: "prerequisites",
		}, nil
	}
	logs = append(logs, fmt.Sprintf("OK: IDMS exists (%s)", idms.Name))

	// Validate pull secret has quay.io/rhoai auth
	if !ps.Valid {
		logs = append(logs, fmt.Sprintf("[DRY-RUN] WARNING: Pull secret is not usable: %s", ps.Detail))
	} else {
		logs = append(logs, "OK: Pull secret is valid")
	}

	// Validate image exists on Quay
	quayAuth := getQuayAuth(c)
	bearerToken, tokenErr := getQuayBearerToken(c.ctx, quayHTTPClient, quayAuth)
	if tokenErr != nil {
		logs = append(logs, fmt.Sprintf("[DRY-RUN] WARNING: Could not authenticate with Quay: %v", tokenErr))
	} else {
		tag := extractTagFromRef(image)
		_, digestErr := getTagDigest(c.ctx, quayHTTPClient, bearerToken, tag)
		if digestErr != nil {
			logs = append(logs, fmt.Sprintf("[DRY-RUN] WARNING: Image tag %q not found on Quay — the image may not exist", tag))
		} else {
			logs = append(logs, fmt.Sprintf("OK: Image tag %q exists on Quay", tag))
		}
	}

	ogLogs, ogProblem, ogCode := ensureOperatorNamespaceAndGroup(c, true)
	logs = append(logs, ogLogs...)
	if ogProblem != "" {
		return &types.OperationResponse{Success: false, Message: ogProblem, Logs: logs, ErrorCode: ogCode}, nil
	}

	// Downgrade check: the same rule as the real update. The bundle comes
	// from the FBC image itself because a dry run creates no catalog.
	dryRunCSV, dryRunCSVErr := getCSV(c)
	if dryRunCSVErr != nil {
		return &types.OperationResponse{Success: false, Message: "Cannot identify installed CSV: " + dryRunCSVErr.Error(), Logs: logs, ErrorCode: "prerequisites"}, nil
	}
	bundle, bundleErr := lookupTargetBundle(c, image)
	if bundleErr != nil {
		logs = append(logs, fmt.Sprintf("[DRY-RUN] WARNING: Could not read the operator bundle from the catalog image: %v", bundleErr))
	}
	blocked, note := downgradeCheck(dryRunCSV, extractTagFromRef(image), bundle)
	if note != "" {
		logs = append(logs, note)
	}
	if blocked != "" {
		logs = append(logs, "[DRY-RUN] FAILED: "+blocked)
		return &types.OperationResponse{Success: false, Message: blocked, Logs: logs, ErrorCode: "validation"}, nil
	}

	cs, err := getCatalogSource(c)
	if err != nil {
		return &types.OperationResponse{
			Success:   false,
			Message:   fmt.Sprintf("Failed to check CatalogSource: %v", err),
			Logs:      logs,
			ErrorCode: errorCodeFromK8sErr(err),
		}, nil
	}
	// Server-side dry run: attempt CatalogSource apply with ?dryRun=All
	catalogSpec := buildCatalogSourceSpec(image)
	csPath := namespacedPath("operators.coreos.com/v1alpha1", "catalogsources", CatalogNS, CatalogName)
	_, _, csDryErr := c.dryRunApply(csPath, catalogSpec)
	if csDryErr != nil {
		logs = append(logs, fmt.Sprintf("[DRY-RUN] FAILED: CatalogSource apply rejected by API server: %v", csDryErr))
		return &types.OperationResponse{
			Success:   false,
			Message:   fmt.Sprintf("Dry run failed: CatalogSource would be rejected: %v", csDryErr),
			Logs:      logs,
			ErrorCode: errorCodeOr(csDryErr, "validation"),
		}, nil
	}
	if cs.Exists && cs.Image == image {
		logs = append(logs, "OK: CatalogSource already points to this image — no change needed (validated)")
	} else if cs.Exists {
		logs = append(logs, "OK: CatalogSource apply validated (would update image)")
	} else {
		logs = append(logs, fmt.Sprintf("OK: CatalogSource apply validated (would create %s)", CatalogName))
	}

	sub, err := getSubscription(c)
	if err != nil {
		return &types.OperationResponse{
			Success:   false,
			Message:   fmt.Sprintf("Failed to check Subscription: %v", err),
			Logs:      logs,
			ErrorCode: errorCodeFromK8sErr(err),
		}, nil
	}
	if sub.State == "Not Installed" || sub.Source == "" {
		logs = append(logs, fmt.Sprintf("[DRY-RUN] Would create Subscription pointing to %s", CatalogName))
	} else {
		logs = append(logs, fmt.Sprintf("[DRY-RUN] Would delete and recreate Subscription (%s/%s -> %s), keeping its config and approval mode", sub.Source, sub.Channel, CatalogName))
	}
	if dryRunCSV.Name != "" {
		logs = append(logs, fmt.Sprintf("[DRY-RUN] Would delete CSV %s so OLM installs the selected build", dryRunCSV.Name))
	}

	return &types.OperationResponse{
		Success: true,
		Message: "Dry run complete. No changes were made.",
		Logs:    logs,
	}, nil
}

// lookupTargetBundle returns the operator bundle (CSV name) an FBC image
// ships for its release, read from the image's catalog content. Tests replace
// it to avoid registry calls.
var lookupTargetBundle = func(c *Client, image string) (string, error) {
	content, err := ExtractFBCContent(c.ctx, c, image)
	if err != nil {
		return "", err
	}
	return content.BundleName, nil
}

func recordUpdateActivity(c *Client, image string, success bool) {
	RecordActivity(c, types.ActivityEntry{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		User:      getUser(c),
		Action:    "update",
		Detail:    image,
		Success:   success,
	})
}

// Rollback removes the nightly catalog and restores the stable operator subscription.
// Deprecated: Use Reinstall instead. Kept for backward compatibility.
func Rollback(c *Client) (*types.OperationResponse, error) {
	return Reinstall(c, "stable", "", "")
}

// Reinstall performs the full uninstall/cleanup/reinstall flow.
// If targetType is "stable", it reinstalls from the stable catalog (same as the old Rollback).
// If targetType is "nightly", it reinstalls with the specified FBC image.
// Delegates to ReinstallStream with a no-op emitter.
func Reinstall(c *Client, targetType, image, channelOverride string) (*types.OperationResponse, error) {
	return ReinstallStream(c, targetType, image, channelOverride, func(UpdateStepEvent) {})
}

// ReinstallWithOptions is Reinstall with caller confirmations.
func ReinstallWithOptions(c *Client, targetType, image, channelOverride string, opts OperationOptions) (*types.OperationResponse, error) {
	return ReinstallStreamWithOptions(c, targetType, image, channelOverride, opts, func(UpdateStepEvent) {})
}

// ReinstallStream executes the full uninstall/cleanup/reinstall pipeline,
// emitting progress events via the emit callback so callers can stream
// status to SSE clients. It returns the final OperationResponse with all
// collected logs; it reports success only once OLM has installed the target.
//
// targetType: "stable" reinstalls from the stable catalog; "nightly" and
// "custom" reinstall with the specified FBC image.
// channelOverride: if non-empty, forces the Subscription channel instead
// of auto-detecting from the catalog.
//
// The DSC, DSCI, CRDs and component CRs are never deleted. Removing the CSV
// stops the operator; OLM's csv-cleanup finalizer removes the operator's own
// webhooks and OLM resets the DSC/DSCI conversion webhooks, so the APIs keep
// working until the new CSV is installed. Every step can be repeated by
// running Reinstall again.
func ReinstallStream(c *Client, targetType, image, channelOverride string, emit func(UpdateStepEvent)) (*types.OperationResponse, error) {
	return ReinstallStreamWithOptions(c, targetType, image, channelOverride, OperationOptions{}, emit)
}

// ReinstallStreamWithOptions is ReinstallStream with caller confirmations; a
// target older than the installed operator needs opts.AllowDowngrade.
func ReinstallStreamWithOptions(c *Client, targetType, image, channelOverride string, opts OperationOptions, emit func(UpdateStepEvent)) (result *types.OperationResponse, opErr error) {
	logs := []string{}
	tracker := &stepTracker{emit: emit}
	emit = tracker.send
	var recovery *operatorRecovery
	recordActivity := true
	stableSource := getStableSource()
	stableChannel := ""
	activityTarget := image
	isNightly := targetType == "nightly" || targetType == "custom"
	defer func() {
		tracker.finish(c, &result, &opErr, logs, recovery, func(ok bool) {
			if recordActivity {
				recordReinstallActivity(c, targetType, activityTarget, ok)
			}
		})
	}()
	fail := func(msg, code string) (*types.OperationResponse, error) {
		logs = append(logs, msg)
		return &types.OperationResponse{Success: false, Message: msg, Logs: logs, ErrorCode: code}, nil
	}

	// --- Step 1: validate_target ---
	emit(UpdateStepEvent{Step: "validate_target", Status: "running", Message: "Validating reinstall target..."})
	if targetType != "stable" && targetType != "nightly" && targetType != "custom" {
		recordActivity = false
		return fail("Invalid reinstall target", "validation")
	}
	if isNightly && image == "" {
		recordActivity = false
		return fail("An FBC image is required", "validation")
	}
	refusal, revertDashboard, guardErr := dashboardDevGuard(c, opts, "Reinstall")
	if guardErr != nil {
		return fail(guardErr.Error(), errorCodeFromK8sErr(guardErr))
	}
	if refusal != "" {
		recordActivity = false
		return fail(refusal, errorCodeDashboardDevActive)
	}
	if !revertDashboard {
		if note := dashboardPRNote(c); note != "" {
			logs = append(logs, note)
		}
	}

	if targetType == "custom" {
		logs = append(logs, "Using the supplied FBC image exactly; a digest pins the selected build even if its tag has moved")
	}
	targetCSV := ""
	if isNightly {
		emit(UpdateStepEvent{Step: "validate_target", Status: "running", Message: "Verifying the selected catalog image..."})
		target, err := preflightReinstallCatalog(c, image, channelOverride)
		if err != nil {
			if c.ctx.Err() != nil {
				return &types.OperationResponse{Success: false, Message: "Operation stopped while verifying the catalog image; the current operator was not removed", Logs: logs}, c.ctx.Err()
			}
			code := "validation"
			if isCatalogImagePullError(err) {
				code = "catalog_image_pull"
			}
			return fail("Replacement catalog validation failed; current operator was not removed: "+err.Error(), code)
		}
		channelOverride = target.Channel
		targetCSV = target.HeadCSV
		logs = append(logs, fmt.Sprintf("Replacement catalog verified before cleanup (channel: %s, head: %s)", target.Channel, target.HeadCSV))
	}

	sub, err := getSubscription(c)
	if err != nil {
		return fail(fmt.Sprintf("Failed to get subscription: %v", err), errorCodeFromK8sErr(err))
	}

	if !isNightly {
		if sub.State == "Not Installed" {
			return fail("No operator Subscription found. Install the operator before using reinstall.", "validation")
		}
		target, discoveryErr := resolveStableTarget(c)
		if discoveryErr != nil {
			return fail(fmt.Sprintf("Cannot determine the stable reinstall target: %v", discoveryErr), "validation")
		}
		stableSource, stableChannel = target.Source, target.Channel
		activityTarget = stableSource + "/" + stableChannel
		targetCSV = SubName + "." + target.Version
		logs = append(logs, fmt.Sprintf("Catalog target: %s / %s (GA %s)", stableSource, stableChannel, target.Version))
	}

	csv, csvErr := getCSV(c)
	if csvErr != nil {
		return fail("Cannot identify installed CSV: "+csvErr.Error(), "prerequisites")
	}

	// Already on the stable target: nothing to do when that install is
	// healthy and current. A Failed, Pending or outdated install on the same
	// channel is reinstalled; Refresh is not offered on stable.
	if !isNightly && sub.Source == stableSource && sub.Channel == stableChannel {
		if csv.Phase == "Succeeded" && (csv.Version == "" || csv.Version == strings.TrimPrefix(targetCSV, SubName+".")) {
			recordActivity = false
			msg := "Already on stable. Nothing to reinstall."
			logs = append(logs, fmt.Sprintf("Subscription is already %s / %s and %s is Succeeded", stableSource, stableChannel, csv.Name))
			emit(UpdateStepEvent{Step: "validate_target", Status: "skipped", Message: msg})
			return &types.OperationResponse{Success: true, Message: msg, Logs: logs}, nil
		}
		logs = append(logs, fmt.Sprintf("Already subscribed to %s / %s, but the operator is not a healthy %s (CSV %q, phase %s); reinstalling it",
			stableSource, stableChannel, strings.TrimPrefix(targetCSV, SubName+"."), csv.Name, csv.Phase))
	}

	ogLogs, ogProblem, ogCode := ensureOperatorNamespaceAndGroup(c, false)
	logs = append(logs, ogLogs...)
	if ogProblem != "" {
		return fail(ogProblem, ogCode)
	}

	if isNightly {
		logs = append(logs, fmt.Sprintf("Reinstalling to nightly: %s", image))
	} else {
		logs = append(logs, fmt.Sprintf("Reinstalling to stable: %s/%s -> %s/%s", sub.Source, sub.Channel, stableSource, stableChannel))
	}
	validated := "Reinstall target validated"
	if down, ok := isDowngrade(csv, targetCSV); ok && down {
		if !opts.AllowDowngrade {
			recordActivity = false
			return fail(fmt.Sprintf("The target %s is older than the installed %s. OLM cannot downgrade an operator: Reinstall would remove the operator and install the older version, while the CRDs keep the newer schema, which the older operator may reject. Nothing was changed. Confirm the downgrade to continue.", targetCSV, csv.Name), errorCodeDowngrade)
		}
		logs = append(logs, fmt.Sprintf("Warning: downgrade confirmed: %s -> %s. CRDs keep the newer schema.", csv.Name, targetCSV))
		validated += fmt.Sprintf(" (downgrade: %s -> %s)", csv.Name, targetCSV)
	}
	if revertDashboard {
		emit(UpdateStepEvent{Step: "validate_target", Status: "running", Message: "Ending the Dashboard Dev session..."})
		if err := RevertDashboardDevForOperation(c); err != nil {
			return fail("Could not end the Dashboard Dev session, so the reinstall was not started: "+err.Error(), errorCodeDashboardDevActive)
		}
		logs = append(logs, "OK: Dashboard Dev session ended; dashboard-operator is running again")
	}
	emit(UpdateStepEvent{Step: "validate_target", Status: "success", Message: validated})

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

	captured, recoveryErr := captureOperatorRecovery(c)
	if recoveryErr != nil {
		return fail(recoveryErr.Error()+". Nothing was changed.", "prerequisites")
	}
	recovery = captured

	// --- Step 3: delete_catalog_source ---
	emit(UpdateStepEvent{Step: "delete_catalog_source", Status: "running", Message: "Removing nightly CatalogSource..."})
	csPath := namespacedPath("operators.coreos.com/v1alpha1", "catalogsources", CatalogNS, CatalogName)
	if _, delErr := c.delete(csPath); delErr != nil {
		if !IsK8sError(delErr, 404) {
			return fail("Cannot remove nightly CatalogSource: "+delErr.Error(), errorCodeFromK8sErr(delErr))
		}
		logs = append(logs, "OK: nightly CatalogSource already absent")
		emit(UpdateStepEvent{Step: "delete_catalog_source", Status: "skipped", Message: "Nightly CatalogSource already absent"})
	} else {
		recovery.catalogChanged = true
		logs = append(logs, "OK: nightly CatalogSource deleted")
		emit(UpdateStepEvent{Step: "delete_catalog_source", Status: "success", Message: "Nightly CatalogSource deleted"})
	}

	// --- Step 4: delete_subscription ---
	emit(UpdateStepEvent{Step: "delete_subscription", Status: "running", Message: "Removing operator Subscription..."})
	if _, delErr := c.delete(subscriptionPath()); delErr != nil {
		if !IsK8sError(delErr, 404) {
			return fail(fmt.Sprintf("Failed to delete Subscription: %v", delErr), errorCodeFromK8sErr(delErr))
		}
		logs = append(logs, "OK: Subscription already absent")
		emit(UpdateStepEvent{Step: "delete_subscription", Status: "skipped", Message: "Subscription already absent"})
	} else {
		recovery.subscriptionChanged = true
		logs = append(logs, "OK: Subscription deleted")
		emit(UpdateStepEvent{Step: "delete_subscription", Status: "success", Message: "Subscription deleted"})
	}

	// --- Step 5: delete_csv ---
	emit(UpdateStepEvent{Step: "delete_csv", Status: "running", Message: "Removing current CSV..."})
	if csv.Name != "" {
		if _, err := c.delete(csvPath(csv.Name)); err != nil && !IsK8sError(err, 404) {
			return fail("Cannot delete current CSV: "+err.Error(), errorCodeFromK8sErr(err))
		}
		recovery.csvRemoved = true
		gone, err := waitForCSVGone(c, csv.Name)
		if err != nil {
			return &types.OperationResponse{Success: false, Message: "Operation stopped while waiting for the current CSV to be deleted", Logs: logs}, err
		}
		if !gone {
			logs = append(logs, fmt.Sprintf("Warning: CSV %s is still being deleted after %s; continuing", csv.Name, CSVDeletionTimeout))
		}
		logs = append(logs, "OK: CSV "+csv.Name+" deleted")
		emit(UpdateStepEvent{Step: "delete_csv", Status: "success", Message: "CSV removed: " + csv.Name})
	} else {
		emit(UpdateStepEvent{Step: "delete_csv", Status: "skipped", Message: "No current CSV"})
	}

	// --- Step 6: cleanup_webhooks ---
	// Only configurations that can no longer be served are removed; operand
	// webhooks such as KServe's keep serving while the operator is reinstalled.
	emit(UpdateStepEvent{Step: "cleanup_webhooks", Status: "running", Message: "Cleaning up stale webhooks..."})
	removedWebhooks, webhookWarnings := removeStaleWebhooks(c)
	logs = append(logs, fmt.Sprintf("Removed %d stale webhook configurations", len(removedWebhooks)))
	for _, r := range removedWebhooks {
		logs = append(logs, "  Removed "+r)
	}
	for _, w := range webhookWarnings {
		slog.Warn("stale webhook cleanup issue", "detail", w)
		logs = append(logs, fmt.Sprintf("Warning: %s", w))
	}
	emit(UpdateStepEvent{Step: "cleanup_webhooks", Status: "success", Message: fmt.Sprintf("Removed %d stale webhooks", len(removedWebhooks))})

	// --- Step 6b: cleanup_stale_component_crs ---
	// After EA↔GA transitions, component CRs can get stuck with finalizers
	// during deletion. Remove finalizers to unblock (K8s finalizer docs pattern).
	unstuckCount, componentWarnings := cleanupStuckComponentCRs(c)
	if unstuckCount > 0 {
		logs = append(logs, fmt.Sprintf("Unblocked %d stuck component CR(s) by removing finalizers", unstuckCount))
	}
	for _, w := range componentWarnings {
		slog.Warn("stuck component CR cleanup issue", "detail", w)
		logs = append(logs, "Warning: "+w)
	}

	// --- Step 7: patch_crds ---
	emit(UpdateStepEvent{Step: "patch_crds", Status: "running", Message: "Checking CRD conversion webhooks..."})
	patchedCRDs, crdWarnings := patchCRDConversionWebhooks(c)
	logs = append(logs, fmt.Sprintf("CRD conversion: %d CRD(s) with a missing conversion Service switched to strategy None (OLM normally does this when the CSV is deleted)", patchedCRDs))
	for _, w := range crdWarnings {
		slog.Warn("CRD conversion patch issue", "detail", w)
		logs = append(logs, "Warning: "+w)
	}
	emit(UpdateStepEvent{Step: "patch_crds", Status: "success", Message: fmt.Sprintf("Patched %d CRDs", patchedCRDs)})

	// --- Step 8: wait_propagation ---
	emit(UpdateStepEvent{Step: "wait_propagation", Status: "running", Message: "Waiting for cleanup to propagate..."})
	select {
	case <-c.ctx.Done():
		return &types.OperationResponse{Success: false, Message: "Operation cancelled during cleanup wait.", Logs: logs}, c.ctx.Err()
	case <-time.After(PropagationWait):
	}
	logs = append(logs, fmt.Sprintf("OK: waited %s for cleanup propagation", PropagationWait))
	emit(UpdateStepEvent{Step: "wait_propagation", Status: "success", Message: "Cleanup propagated"})

	// --- Steps 9+ diverge for stable vs nightly ---
	if isNightly {
		return reinstallNightlySteps(c, image, channelOverride, recovery, logs, emit)
	}
	return reinstallStableSteps(c, stableSource, stableChannel, recovery, logs, emit)
}

// isDowngrade reports whether targetCSV is older than the installed CSV; ok
// is false when either version is unknown.
func isDowngrade(installed types.CSVInfo, targetCSV string) (bool, bool) {
	if installed.Name == "" || targetCSV == "" {
		return false, false
	}
	current, ok := parseCSVVersion(installed.Name)
	if !ok && installed.Version != "" {
		current, ok = parseCSVVersion(SubName + "." + installed.Version)
	}
	target, tok := parseCSVVersion(targetCSV)
	if !ok || !tok {
		return false, false
	}
	return compareTags(target, current) < 0, true
}

// reinstallNightlySteps handles the nightly-specific portion of ReinstallStream:
// create CatalogSource, wait for READY, verify the channel, create the
// Subscription and wait for OLM to install it.
func reinstallNightlySteps(c *Client, image, channel string, recovery *operatorRecovery, logs []string, emit func(UpdateStepEvent)) (*types.OperationResponse, error) {
	fail := func(msg, code string) (*types.OperationResponse, error) {
		logs = append(logs, msg)
		return &types.OperationResponse{Success: false, Message: msg, Logs: logs, ErrorCode: code}, nil
	}

	// --- Step 9: create_catalog_source ---
	emit(UpdateStepEvent{Step: "create_catalog_source", Status: "running", Message: "Creating CatalogSource with nightly image..."})
	csApplyPath := namespacedPath("operators.coreos.com/v1alpha1", "catalogsources", CatalogNS, CatalogName)
	recovery.catalogChanged = true
	if _, _, err := c.apply(csApplyPath, buildCatalogSourceSpec(image)); err != nil {
		return fail(fmt.Sprintf("Failed to create nightly CatalogSource: %v", err), errorCodeFromK8sErr(err))
	}
	logs = append(logs, "OK: CatalogSource created with nightly image")
	emit(UpdateStepEvent{Step: "create_catalog_source", Status: "success", Message: "CatalogSource created", Detail: image})

	// --- Step 10: wait_catalog_ready ---
	emit(UpdateStepEvent{Step: "wait_catalog_ready", Status: "running", Message: "Waiting for CatalogSource to become READY..."})
	logs = append(logs, "Waiting for CatalogSource to become READY...")
	catalogReady, catalogErr := waitForNightlyCatalogReady(c, image, emit, &logs)
	if msg, code, stopErr := catalogWaitFailure(catalogReady, catalogErr); msg != "" {
		r, _ := fail(msg, code)
		return r, stopErr
	}
	logs = append(logs, "OK: CatalogSource is READY")
	emit(UpdateStepEvent{Step: "wait_catalog_ready", Status: "success", Message: "CatalogSource is READY"})

	// Poll PackageManifest until the nightly catalog's channels appear (same as UpdateStream)
	emit(UpdateStepEvent{Step: "detect_channel", Status: "running", Message: "Waiting for catalog channels to refresh..."})
	if err := waitForNightlyPackageManifest(c, image, &logs); err != nil {
		return &types.OperationResponse{Success: false, Message: "Operation stopped while waiting for the catalog channels", Logs: logs}, err
	}

	// --- Step 11: detect_channel ---
	emit(UpdateStepEvent{Step: "detect_channel", Status: "running", Message: "Detecting target channel..."})
	exists, channelErr := channelExistsInCatalog(c, channel)
	if channelErr != nil || !exists || channel == "" {
		msg := fmt.Sprintf("Selected channel %q is unavailable in the replacement catalog", channel)
		if channelErr != nil {
			msg = fmt.Sprintf("Cannot verify channel %q in the replacement catalog: %v", channel, channelErr)
		}
		return fail(msg, "channel_detection")
	}
	logs = append(logs, "Verified replacement channel: "+channel)
	emit(UpdateStepEvent{Step: "detect_channel", Status: "success", Message: "Verified replacement channel: " + channel, Detail: channel})

	// --- Step 12: create_subscription ---
	emit(UpdateStepEvent{Step: "create_subscription", Status: "running", Message: "Creating Subscription to nightly catalog..."})
	return createSubscriptionAndWait(c, CatalogName, channel, "nightly", recovery, logs, emit)
}

// reinstallStableSteps handles the stable-specific portion of ReinstallStream:
// create the Subscription to the stable catalog and wait for OLM to install it.
func reinstallStableSteps(c *Client, stableSource, stableChannel string, recovery *operatorRecovery, logs []string, emit func(UpdateStepEvent)) (*types.OperationResponse, error) {
	// --- Step 9: create_subscription ---
	emit(UpdateStepEvent{Step: "create_subscription", Status: "running", Message: "Creating fresh Subscription to stable catalog..."})
	return createSubscriptionAndWait(c, stableSource, stableChannel, "stable", recovery, logs, emit)
}

// createSubscriptionAndWait creates the Reinstall Subscription (keeping the
// previous spec.config and approval mode) and waits for OLM's verdict.
func createSubscriptionAndWait(c *Client, source, channel, kind string, recovery *operatorRecovery, logs []string, emit func(UpdateStepEvent)) (*types.OperationResponse, error) {
	newSub := buildSubscription(recovery.subscription, source, channel)
	recovery.subscriptionChanged = true
	applyErr, cancelErr := applySubscriptionWithRetry(c, subscriptionPath(), newSub, func(attempt int, err error, willRetry bool, backoff time.Duration) {
		if willRetry {
			slog.Warn("failed to create Subscription, retrying", "target", kind, "attempt", attempt, "error", err, "backoff", backoff)
			logs = append(logs, fmt.Sprintf("Warning: Subscription creation attempt %d failed: %v -- retrying in %s", attempt, err, backoff))
			emit(UpdateStepEvent{Step: "create_subscription", Status: "running", Message: fmt.Sprintf("Attempt %d failed, retrying...", attempt)})
		}
	})
	if cancelErr != nil {
		return &types.OperationResponse{Success: false, Message: "Operation cancelled during Subscription creation retry.", Logs: logs}, cancelErr
	}
	if applyErr != nil {
		msg := fmt.Sprintf("Failed to create %s Subscription after 3 attempts: %v", kind, applyErr)
		slog.Error("all attempts to create Subscription failed", "target", kind, "error", applyErr)
		logs = append(logs, msg)
		return &types.OperationResponse{Success: false, Message: msg, Logs: logs, ErrorCode: errorCodeFromK8sErr(applyErr)}, nil
	}
	logs = append(logs, fmt.Sprintf("OK: Subscription created (%s / %s, installPlanApproval=%v)", source, channel, newSub["spec"].(map[string]interface{})["installPlanApproval"]))
	emit(UpdateStepEvent{Step: "create_subscription", Status: "success", Message: "Subscription created", Detail: channel})

	// --- verify_installplan: wait for OLM's result ---
	emit(UpdateStepEvent{Step: "verify_installplan", Status: "running", Message: "Waiting for OLM to install the operator..."})
	logs = append(logs, "Waiting for OLM to install the operator...")
	outcome := waitForOperatorInstall(c, "verify_installplan", emit, &logs, recovery)
	if !outcome.succeeded {
		recovery.keepNewInstall = outcome.keepNewInstall
		return &types.OperationResponse{Success: false, Message: outcome.message, Logs: logs, ErrorCode: outcome.errorCode}, nil
	}
	emit(UpdateStepEvent{Step: "verify_installplan", Status: "success", Message: fmt.Sprintf("Operator installed: %s", outcome.csv), Detail: outcome.csv})
	logs = append(logs, "Reinstall complete. The operator now reconciles the DataScienceCluster; component rollouts can take a few more minutes.")
	return &types.OperationResponse{
		Success: true,
		Message: strings.TrimSpace(fmt.Sprintf("Reinstall to %s complete: %s is installed. %s", kind, outcome.csv, outcome.note)),
		Logs:    logs,
	}, nil
}

// patchCRDConversionWebhooks is a safety net for the DSC and DSCI CRDs after
// the CSV was deleted. OLM itself switches a deleted CSV's conversion CRDs to
// strategy None (operator-lifecycle-manager handleClusterServiceVersionDeletion)
// and the next CSV restores the webhook. A CRD is only patched when OLM did
// not do that: no RHOAI CSV exists, the CRD still uses Webhook conversion,
// and the conversion Service is NotFound (requests needing conversion would
// fail). While a CSV exists, OLM's install check would treat a patched CRD as
// unhealthy and reinstall the operator, so nothing is changed then. The
// webhook settings are cleared in the same patch because the API server
// rejects strategy None while webhook is still set (server-side dry run on
// OpenShift 4.22). A CRD that does not exist is skipped.
func patchCRDConversionWebhooks(c *Client) (int, []string) {
	crds := []string{
		"datascienceclusters.datasciencecluster.opendatahub.io",
		"dscinitializations.dscinitialization.opendatahub.io",
	}
	if exists, err := rhoaiCSVExists(c); err != nil || exists {
		if err != nil {
			return 0, []string{"cannot check for an RHOAI CSV, CRD conversion left unchanged: " + err.Error()}
		}
		return 0, nil
	}
	patchData := []byte(`{"spec":{"conversion":{"strategy":"None","webhook":null}}}`)
	count := 0
	var warnings []string
	for _, crd := range crds {
		path := clusterPath("apiextensions.k8s.io/v1", "customresourcedefinitions", crd)
		body, _, err := c.get(path)
		if IsK8sError(err, 404) {
			continue
		}
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("read CRD %s: %v", crd, err))
			continue
		}
		var obj struct {
			Spec struct {
				Conversion struct {
					Strategy string `json:"strategy"`
					Webhook  struct {
						ClientConfig struct {
							Service struct {
								Name      string `json:"name"`
								Namespace string `json:"namespace"`
							} `json:"service"`
						} `json:"clientConfig"`
					} `json:"webhook"`
				} `json:"conversion"`
			} `json:"spec"`
		}
		if err := json.Unmarshal(body, &obj); err != nil {
			warnings = append(warnings, fmt.Sprintf("parse CRD %s: %v", crd, err))
			continue
		}
		conv := obj.Spec.Conversion
		svc := conv.Webhook.ClientConfig.Service
		if conv.Strategy != "Webhook" || svc.Name == "" || svc.Namespace == "" {
			continue
		}
		if _, _, err := c.get(namespacedPath("v1", "services", svc.Namespace, svc.Name)); !IsK8sError(err, 404) {
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("check conversion Service of CRD %s: %v", crd, err))
			}
			continue
		}
		if _, _, err := c.patch(path, patchData); err != nil {
			warnings = append(warnings, fmt.Sprintf("patch conversion of CRD %s: %v", crd, err))
			continue
		}
		count++
	}
	return count, warnings
}

// rhoaiCSVExists reports whether any (non-copied) RHOAI CSV is in the
// operator namespace.
func rhoaiCSVExists(c *Client) (bool, error) {
	body, _, err := c.get(namespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", SubNS, ""))
	if IsK8sError(err, 404) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name   string            `json:"name"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return false, err
	}
	for _, item := range list.Items {
		if strings.HasPrefix(item.Metadata.Name, SubName+".") && item.Metadata.Labels["olm.copiedFrom"] == "" {
			return true, nil
		}
	}
	return false, nil
}

func recordRefreshActivity(c *Client, csvName string, success bool) {
	RecordActivity(c, types.ActivityEntry{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		User:      getUser(c),
		Action:    "refresh",
		Detail:    csvName,
		Success:   success,
	})
}

func recordReinstallActivity(c *Client, targetType, image string, success bool) {
	detail := fmt.Sprintf("to latest GA from %s", getStableSource())
	if targetType == "stable" && image != "" {
		detail = "to " + image
	} else if targetType == "nightly" || targetType == "custom" {
		detail = fmt.Sprintf("to %s %s", targetType, image)
	}
	RecordActivity(c, types.ActivityEntry{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		User:      getUser(c),
		Action:    "reinstall",
		Detail:    detail,
		Success:   success,
	})
}

// RefreshOperator re-deploys the installed operator version: it deletes the
// CSV and Subscription and recreates the Subscription from the same catalog
// and channel. It cannot pick up newer images: the nightly catalog image and
// every image in the CSV are pinned by digest, so OLM reinstalls the same
// bundle. Use Update for a newer build. The DSCI, DSC and user workloads are
// kept. It delegates to RefreshOperatorStream with a no-op emitter.
func RefreshOperator(c *Client) (*types.OperationResponse, error) {
	return RefreshOperatorStream(c, func(UpdateStepEvent) {})
}

// RefreshOperatorWithOptions is RefreshOperator with caller confirmations.
func RefreshOperatorWithOptions(c *Client, opts OperationOptions) (*types.OperationResponse, error) {
	return RefreshOperatorStreamWithOptions(c, opts, func(UpdateStepEvent) {})
}

// RefreshOperatorStream executes the refresh pipeline, emitting progress events
// via the emit callback so callers can stream status to SSE clients.
// It deletes the Subscription and then the CSV, recreates the Subscription
// with its previous spec, and waits for OLM to install the CSV again.
func RefreshOperatorStream(c *Client, emit func(UpdateStepEvent)) (*types.OperationResponse, error) {
	return RefreshOperatorStreamWithOptions(c, OperationOptions{}, emit)
}

// RefreshOperatorStreamWithOptions is RefreshOperatorStream with caller
// confirmations; see OperationOptions.
func RefreshOperatorStreamWithOptions(c *Client, opts OperationOptions, emit func(UpdateStepEvent)) (result *types.OperationResponse, opErr error) {
	logs := []string{}
	tracker := &stepTracker{emit: emit}
	emit = tracker.send
	var recovery *operatorRecovery
	csvName := ""
	recordActivity := true
	defer func() {
		tracker.finish(c, &result, &opErr, logs, recovery, func(ok bool) {
			if recordActivity {
				recordRefreshActivity(c, csvName, ok)
			}
		})
	}()
	fail := func(msg, code string) (*types.OperationResponse, error) {
		logs = append(logs, msg)
		return &types.OperationResponse{Success: false, Message: msg, Logs: logs, ErrorCode: code}, nil
	}

	// --- Step 1: verify_csv ---
	emit(UpdateStepEvent{Step: "verify_csv", Status: "running", Message: "Verifying operator is installed..."})

	csv, err := getCSV(c)
	if err != nil {
		return fail(fmt.Sprintf("Failed to look up CSV: %v", err), errorCodeOr(err, "prerequisites"))
	}
	if csv.Name == "" {
		recordActivity = false
		msg := "No RHOAI operator CSV found. The operator may still be installing -- check the status and try again once the CSV appears."
		logs = append(logs, "No CSV with displayName 'Red Hat OpenShift AI' found in the redhat-ods-operator namespace.")
		emit(UpdateStepEvent{Step: "verify_csv", Status: "success", Message: msg})
		return &types.OperationResponse{Success: true, Message: msg, Logs: logs}, nil
	}
	csvName = csv.Name
	logs = append(logs, fmt.Sprintf("Current CSV: %s (%s)", csv.Name, csv.Phase))
	refusal, revertDashboard, guardErr := dashboardDevGuard(c, opts, "Refresh")
	if guardErr != nil {
		return fail(guardErr.Error(), errorCodeFromK8sErr(guardErr))
	}
	if refusal != "" {
		recordActivity = false
		return fail(refusal, errorCodeDashboardDevActive)
	}
	emit(UpdateStepEvent{Step: "verify_csv", Status: "success", Message: fmt.Sprintf("CSV found: %s", csv.Name), Detail: csv.Name})

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

	// --- Step 3: get_subscription ---
	emit(UpdateStepEvent{Step: "get_subscription", Status: "running", Message: "Reading current Subscription..."})

	sub, err := getSubscription(c)
	if err != nil {
		return fail(fmt.Sprintf("Failed to get Subscription: %v", err), errorCodeFromK8sErr(err))
	}
	if sub.State == "Not Installed" || sub.Source == "" || sub.Channel == "" {
		// Refresh recreates the Subscription from its own source and channel;
		// without them it would leave the operator uninstalled.
		return fail("The operator Subscription is missing or has no source/channel, so it cannot be recreated. Nothing was changed; use Reinstall instead.", "prerequisites")
	}
	logs = append(logs, fmt.Sprintf("Subscription: source=%s, channel=%s", sub.Source, sub.Channel))

	// If anything below fails, restore the previous Subscription so OLM
	// reinstalls the operator, as Update and Reinstall do.
	captured, recoveryErr := captureOperatorRecovery(c)
	if recoveryErr != nil {
		return fail(recoveryErr.Error()+". Nothing was changed.", "prerequisites")
	}
	if revertDashboard {
		if err := RevertDashboardDevForOperation(c); err != nil {
			return fail("Could not end the Dashboard Dev session, so the refresh was not started: "+err.Error(), errorCodeDashboardDevActive)
		}
		logs = append(logs, "OK: Dashboard Dev session ended; dashboard-operator is running again")
	}
	recovery = captured
	emit(UpdateStepEvent{Step: "get_subscription", Status: "success", Message: fmt.Sprintf("Subscription: %s/%s", sub.Source, sub.Channel), Detail: sub.Channel})

	// --- Step 4: delete_subscription ---
	// The Subscription goes first: with it still in place, OLM would start a
	// new InstallPlan as soon as the CSV disappears, and deleting the
	// Subscription afterwards would garbage-collect that plan mid-install.
	emit(UpdateStepEvent{Step: "delete_subscription", Status: "running", Message: "Deleting Subscription..."})
	if _, subDelErr := c.delete(subscriptionPath()); subDelErr != nil && !IsK8sError(subDelErr, 404) {
		return fail(fmt.Sprintf("Failed to delete Subscription: %v", subDelErr), errorCodeFromK8sErr(subDelErr))
	}
	recovery.subscriptionChanged = true
	logs = append(logs, "OK: Subscription deleted")
	emit(UpdateStepEvent{Step: "delete_subscription", Status: "success", Message: "Subscription deleted"})

	// --- Step 5: delete_csv ---
	emit(UpdateStepEvent{Step: "delete_csv", Status: "running", Message: "Deleting CSV to trigger a fresh install..."})
	logs = append(logs, fmt.Sprintf("Deleting CSV %s...", csv.Name))
	if _, csvDelErr := c.delete(csvPath(csv.Name)); csvDelErr != nil && !IsK8sError(csvDelErr, 404) {
		return fail(fmt.Sprintf("Failed to delete CSV %s: %v", csv.Name, csvDelErr), errorCodeFromK8sErr(csvDelErr))
	}
	recovery.csvRemoved = true
	logs = append(logs, "OK: CSV deleted")
	emit(UpdateStepEvent{Step: "delete_csv", Status: "success", Message: fmt.Sprintf("CSV %s deleted", csv.Name)})

	// --- Step 6: wait_cleanup ---
	emit(UpdateStepEvent{Step: "wait_cleanup", Status: "running", Message: "Waiting for the CSV to be removed..."})
	gone, err := waitForCSVGone(c, csv.Name)
	if err != nil {
		return &types.OperationResponse{Success: false, Message: "Operation cancelled while waiting for the CSV to be removed.", Logs: logs}, err
	}
	if !gone {
		logs = append(logs, fmt.Sprintf("Warning: CSV %s is still being deleted after %s; continuing", csv.Name, CSVDeletionTimeout))
	}
	select {
	case <-c.ctx.Done():
		return &types.OperationResponse{Success: false, Message: "Operation cancelled during cleanup wait.", Logs: logs}, c.ctx.Err()
	case <-time.After(RefreshCleanupWait):
	}
	emit(UpdateStepEvent{Step: "wait_cleanup", Status: "success", Message: "Cleanup propagated"})

	// --- Step 7: recreate_subscription ---
	emit(UpdateStepEvent{Step: "recreate_subscription", Status: "running", Message: "Recreating Subscription..."})
	logs = append(logs, "Recreating Subscription...")
	newSub := buildSubscription(recovery.subscription, sub.Source, sub.Channel)
	applyErr, cancelErr := applySubscriptionWithRetry(c, subscriptionPath(), newSub, func(attempt int, err error, _ bool, _ time.Duration) {
		slog.Warn("Subscription recreation failed", "attempt", attempt, "error", err)
		logs = append(logs, fmt.Sprintf("  Attempt %d/3 failed: %v", attempt, err))
	})
	if cancelErr != nil {
		return &types.OperationResponse{Success: false, Message: "Operation cancelled during Subscription recreation retry.", Logs: logs}, cancelErr
	}
	if applyErr != nil {
		return fail(fmt.Sprintf("Failed to recreate Subscription after 3 attempts: %v", applyErr), errorCodeFromK8sErr(applyErr))
	}
	logs = append(logs, "OK: Subscription recreated")
	emit(UpdateStepEvent{Step: "recreate_subscription", Status: "success", Message: "Subscription recreated", Detail: sub.Channel})

	// --- Step 8: verify_installplan (wait for OLM's result) ---
	emit(UpdateStepEvent{Step: "verify_installplan", Status: "running", Message: "Waiting for OLM to reinstall the operator..."})
	logs = append(logs, "Waiting for OLM to reinstall the operator...")
	outcome := waitForOperatorInstall(c, "verify_installplan", emit, &logs, recovery)
	if !outcome.succeeded {
		recovery.keepNewInstall = outcome.keepNewInstall
		return &types.OperationResponse{Success: false, Message: outcome.message, Logs: logs, ErrorCode: outcome.errorCode}, nil
	}
	emit(UpdateStepEvent{Step: "verify_installplan", Status: "success", Message: fmt.Sprintf("Operator re-deployed: %s", outcome.csv), Detail: outcome.csv})
	logs = append(logs, "The same operator version was re-deployed from the current catalog. To get a newer nightly build, use Update.")
	return &types.OperationResponse{
		Success: true,
		Message: strings.TrimSpace(fmt.Sprintf("Operator re-deployed: %s is installed again (same version; use Update for a newer build). %s", outcome.csv, outcome.note)),
		Logs:    logs,
	}, nil
}

// CreatePullSecret creates or updates the quay.io/rhoai pull secret in kube-system.
func CreatePullSecret(c *Client, auth string) (*types.OperationResponse, error) {
	logs := []string{}

	// Validate auth value
	if auth == "" {
		return &types.OperationResponse{
			Success:   false,
			Message:   "Auth value is required",
			Logs:      logs,
			ErrorCode: "validation",
		}, nil
	}

	// Check for newlines or control characters
	for _, r := range auth {
		if unicode.IsControl(r) {
			return &types.OperationResponse{
				Success:   false,
				Message:   "Auth value contains invalid characters",
				Logs:      logs,
				ErrorCode: "validation",
			}, nil
		}
	}

	// Validate base64 encoding. CRI-O reads the secret through
	// containers/image, which decodes "auth" with base64.StdEncoding only
	// (pkg/docker/config/config.go decodeDockerAuth) and rejects unpadded
	// input. Accept an unpadded value but store its padded form.
	decoded, err := base64.StdEncoding.DecodeString(auth)
	if err != nil {
		decoded, err = base64.RawStdEncoding.DecodeString(auth)
		if err != nil {
			return &types.OperationResponse{
				Success:   false,
				Message:   "Auth value is not valid base64",
				Logs:      logs,
				ErrorCode: "validation",
			}, nil
		}
		auth = base64.StdEncoding.EncodeToString(decoded)
		logs = append(logs, "Auth value was missing base64 padding; stored the padded form that CRI-O can decode")
	}

	// Validate it looks like username:password with both parts present
	if user, pass, ok := strings.Cut(string(decoded), ":"); !ok || user == "" || pass == "" {
		return &types.OperationResponse{
			Success:   false,
			Message:   "Auth value must decode to username:password format",
			Logs:      logs,
			ErrorCode: "validation",
		}, nil
	}

	logs = append(logs, "Auth value validated")

	// Check if secret already exists. Its other registry credentials must be
	// preserved: only the quay.io/rhoai entry is replaced.
	secretPath := namespacedPath("v1", "secrets", "kube-system", "additional-pull-secret")
	existingBody, _, getErr := c.get(secretPath)
	secretExists := true
	if getErr != nil {
		if IsK8sError(getErr, 404) {
			secretExists = false
		} else {
			return &types.OperationResponse{
				Success:   false,
				Message:   fmt.Sprintf("Failed to check existing secret: %v", getErr),
				Logs:      logs,
				ErrorCode: errorCodeFromK8sErr(getErr),
			}, nil
		}
	}

	dockerConfig := map[string]interface{}{}
	if secretExists {
		existing, problem := decodeDockerConfigSecret(existingBody)
		switch {
		case problem == "":
			dockerConfig = existing
		case problem == "Secret has no data" || problem == "Secret is missing .dockerconfigjson key":
			// Nothing to preserve.
		default:
			// The kubelet cannot use an unreadable config either, so nothing
			// usable is lost by replacing it.
			logs = append(logs, fmt.Sprintf("Warning: existing .dockerconfigjson was unreadable (%s) and will be replaced", problem))
		}
	}
	auths, ok := dockerConfig["auths"].(map[string]interface{})
	if !ok {
		auths = map[string]interface{}{}
	}
	preserved := 0
	for key := range auths {
		if normalizeRegistryKey(key) == quayRHOAIRegistry {
			// Remove equivalent spellings so the new credential is the one used.
			delete(auths, key)
			continue
		}
		preserved++
	}
	auths[quayRHOAIRegistry] = map[string]interface{}{"auth": auth}
	dockerConfig["auths"] = auths
	if preserved > 0 {
		logs = append(logs, fmt.Sprintf("Preserving %d other registry credential(s) in the pull secret", preserved))
	}

	dockerConfigBytes, err := json.Marshal(dockerConfig)
	if err != nil {
		return &types.OperationResponse{
			Success:   false,
			Message:   "Failed to construct docker config",
			Logs:      logs,
			ErrorCode: "validation",
		}, nil
	}
	dockerConfigB64 := base64.StdEncoding.EncodeToString(dockerConfigBytes)

	secret := map[string]interface{}{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]interface{}{
			"name":      "additional-pull-secret",
			"namespace": "kube-system",
		},
		"type": "kubernetes.io/dockerconfigjson",
		"data": map[string]interface{}{
			".dockerconfigjson": dockerConfigB64,
		},
	}

	if secretExists {
		logs = append(logs, "Secret already exists, updating...")
		_, _, err = c.apply(secretPath, secret)
		if err != nil {
			return &types.OperationResponse{
				Success:   false,
				Message:   fmt.Sprintf("Failed to update pull secret: %v", err),
				Logs:      logs,
				ErrorCode: errorCodeFromK8sErr(err),
			}, nil
		}
		logs = append(logs, "OK: Pull secret updated")
	} else {
		logs = append(logs, "Creating new pull secret...")
		secretsPath := namespacedPath("v1", "secrets", "kube-system", "")
		data, marshalErr := json.Marshal(secret)
		if marshalErr != nil {
			return nil, fmt.Errorf("marshal secret: %w", marshalErr)
		}
		_, _, err = c.post(secretsPath, data)
		if err != nil {
			return &types.OperationResponse{
				Success:   false,
				Message:   fmt.Sprintf("Failed to create pull secret: %v", err),
				Logs:      logs,
				ErrorCode: errorCodeFromK8sErr(err),
			}, nil
		}
		logs = append(logs, "OK: Pull secret created")
	}

	// Validate by reading it back
	ps, err := getPullSecret(c)
	if err != nil {
		logs = append(logs, fmt.Sprintf("Warning: failed to verify secret after creation: %v", err))
		return &types.OperationResponse{
			Success: true,
			Message: "Pull secret was created but verification failed. Try refreshing the status.",
			Logs:    logs,
		}, nil
	}

	if !ps.Exists {
		return &types.OperationResponse{
			Success:   false,
			Message:   "Pull secret was not found after creation",
			Logs:      logs,
			ErrorCode: "validation",
		}, nil
	}

	if !ps.Valid {
		logs = append(logs, fmt.Sprintf("Warning: secret exists but validation issue: %s", ps.Detail))
	} else {
		logs = append(logs, "OK: Pull secret verified — contains quay.io/rhoai credentials")
	}

	RecordActivity(c, types.ActivityEntry{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		User:      getUser(c),
		Action:    "create-pull-secret",
		Detail:    "",
		Success:   true,
	})

	return &types.OperationResponse{
		Success: true,
		Message: "Pull secret configured successfully.",
		Logs:    logs,
	}, nil
}

// TestPullSecret validates that the pull secret exists and contains quay.io/rhoai credentials.
func TestPullSecret(c *Client) (*types.OperationResponse, error) {
	logs := []string{}

	ps, err := getPullSecret(c)
	if err != nil {
		return &types.OperationResponse{
			Success:   false,
			Message:   fmt.Sprintf("Failed to check pull secret: %v", err),
			Logs:      logs,
			ErrorCode: errorCodeFromK8sErr(err),
		}, nil
	}

	if !ps.Exists {
		return &types.OperationResponse{
			Success:   false,
			Message:   "additional-pull-secret does not exist in kube-system.",
			Logs:      []string{"Secret not found. Create it using the setup instructions."},
			ErrorCode: "prerequisites",
		}, nil
	}

	logs = append(logs, "OK: additional-pull-secret exists")

	if !ps.Valid {
		return &types.OperationResponse{
			Success:   false,
			Message:   fmt.Sprintf("Pull secret exists but is invalid: %s", ps.Detail),
			Logs:      logs,
			ErrorCode: "validation",
		}, nil
	}

	logs = append(logs, "OK: credentials accepted by quay.io/rhoai")
	logs = append(logs, "Pull secret is correctly configured.")

	return &types.OperationResponse{
		Success: true,
		Message: "Pull secret is valid — credentials verified against Quay.",
		Logs:    logs,
	}, nil
}
