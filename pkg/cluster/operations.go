package cluster

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
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
	// Query the packagemanifest from our nightly catalog specifically (not the
	// default redhat-operators catalog) by using a label selector.
	listPath := namespacedPath("packages.operators.coreos.com/v1", "packagemanifests", CatalogNS, "") +
		"?labelSelector=" + url.QueryEscape("catalog="+CatalogName)
	body, _, err := c.get(listPath)
	if err != nil {
		slog.Warn("detectNightlyChannel: API request failed, falling back to default", "error", err)
		// Fallback: query by name (may hit the wrong catalog)
		fallbackPath := namespacedPath("packages.operators.coreos.com/v1", "packagemanifests", CatalogNS, "rhods-operator")
		body, _, err = c.get(fallbackPath)
		if err != nil {
			return "", fmt.Errorf("packagemanifest API request failed: %w", err)
		}
		var result map[string]interface{}
		if err := json.Unmarshal(body, &result); err != nil {
			return "", fmt.Errorf("unmarshal packagemanifest: %w", err)
		}
		status, _ := result["status"].(map[string]interface{})
		channels, _ := status["channels"].([]interface{})
		return detectBestChannel(channels, imageRef)
	}

	// Parse the list response and find the rhods-operator entry
	var list struct {
		Items []struct {
			Metadata struct{ Name string `json:"name"` } `json:"metadata"`
			Status   struct {
				Channels []interface{} `json:"channels"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return "", fmt.Errorf("unmarshal packagemanifest list: %w", err)
	}

	for _, item := range list.Items {
		if item.Metadata.Name == "rhods-operator" {
			return detectBestChannel(item.Status.Channels, imageRef)
		}
	}

	slog.Warn("detectNightlyChannel: rhods-operator not found in nightly catalog")
	return "", nil
}

func detectBestChannel(channels []interface{}, imageRef string) (string, error) {
	if len(channels) == 0 {
		slog.Warn("detectNightlyChannel: no channels found in packagemanifest")
		return "", nil
	}

	targetMajor, targetMinor, hasTarget := extractTargetVersion(imageRef)
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
	listPath := namespacedPath("packages.operators.coreos.com/v1", "packagemanifests", CatalogNS, "") +
		"?labelSelector=" + url.QueryEscape("catalog="+CatalogName)
	body, _, err := c.get(listPath)
	if err != nil {
		// Fallback to direct query
		fallbackPath := namespacedPath("packages.operators.coreos.com/v1", "packagemanifests", CatalogNS, "rhods-operator")
		body, _, err = c.get(fallbackPath)
		if err != nil {
			return false, fmt.Errorf("packagemanifest API request failed: %w", err)
		}
		var result map[string]interface{}
		if err := json.Unmarshal(body, &result); err != nil {
			return false, fmt.Errorf("unmarshal packagemanifest: %w", err)
		}
		status, _ := result["status"].(map[string]interface{})
		channels, _ := status["channels"].([]interface{})
		for _, ch := range channels {
			chMap, _ := ch.(map[string]interface{})
			name, _ := chMap["name"].(string)
			if name == channel {
				return true, nil
			}
		}
		return false, nil
	}

	var list struct {
		Items []struct {
			Metadata struct{ Name string `json:"name"` } `json:"metadata"`
			Status   struct {
				Channels []struct {
					Name string `json:"name"`
				} `json:"channels"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return false, fmt.Errorf("unmarshal packagemanifest list: %w", err)
	}

	for _, item := range list.Items {
		if item.Metadata.Name == "rhods-operator" {
			for _, ch := range item.Status.Channels {
				if ch.Name == channel {
					return true, nil
				}
			}
		}
	}
	return false, nil
}

// Timing constants used by multi-step operations. Package-level vars so tests
// can override them with shorter durations to avoid real-time waits.
var (
	CatalogReadyTimeout    = 120 * time.Second
	CatalogPollInterval    = 5 * time.Second
	InstallPlanPollTimeout = 60 * time.Second
	InstallPlanPollInterval = 5 * time.Second
	PropagationWait        = 10 * time.Second
	RefreshCleanupWait     = 5 * time.Second
	SubRetryBackoffs       = []time.Duration{2 * time.Second, 4 * time.Second}
	ChannelRetryDelay              = 8 * time.Second
	PackageManifestPropagationWait = 30 * time.Second
)

// validateTagDigestMatch checks if the image reference contains both a tag and
// a digest, and verifies they match. This prevents accidentally installing the
// wrong version (e.g., tag says ea.2 but digest points to GA).
// Returns nil if: tag-only, digest-only, or tag+digest match.
// Returns error if: tag+digest mismatch.
func validateTagDigestMatch(ctx context.Context, c *Client, imageRef string) error {
	// Only validate when both tag and digest are present
	atIdx := strings.Index(imageRef, "@sha256:")
	if atIdx < 0 {
		return nil
	}
	providedDigest := imageRef[atIdx+1:]

	tag := extractTagFromRef(imageRef)
	if tag == "" || strings.HasPrefix(tag, "sha256:") {
		return nil
	}

	quayAuth := getQuayAuth(c)
	bearerToken, err := getQuayBearerToken(ctx, quayHTTPClient, quayAuth)
	if err != nil {
		slog.Warn("tag-digest validation: could not authenticate with Quay, skipping", "error", err)
		return nil
	}

	resolvedDigest, err := getTagDigest(ctx, quayHTTPClient, bearerToken, tag)
	if err != nil {
		slog.Warn("tag-digest validation: could not resolve tag digest, skipping", "tag", tag, "error", err)
		return nil
	}

	if resolvedDigest != providedDigest {
		return fmt.Errorf("tag-digest mismatch: tag %q resolves to %s but the provided digest is %s — this would install the wrong version", tag, resolvedDigest[:20]+"...", providedDigest[:20]+"...")
	}

	return nil
}

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

// waitForCatalogSourceReady polls the CatalogSource until its connectionState
// reports "READY" or the timeout expires. The packagemanifest will not reflect
// new catalog content until the CatalogSource grpc pod is running and OLM has
// connected to it, so callers should wait for READY before detecting channels.
func waitForCatalogSourceReady(c *Client, timeout time.Duration) error {
	deadline := time.After(timeout)
	ticker := time.NewTicker(CatalogPollInterval)
	defer ticker.Stop()

	for {
		cs, err := getCatalogSource(c)
		if err == nil && cs.Exists && cs.State == "READY" {
			return nil
		}

		select {
		case <-c.ctx.Done():
			return c.ctx.Err()
		case <-deadline:
			state := "unknown"
			if cs.Exists {
				state = cs.State
			}
			return fmt.Errorf("CatalogSource %s not READY after %s (current state: %s)", CatalogName, timeout, state)
		case <-ticker.C:
			// poll again
		}
	}
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

// UpdateStream executes the 8-step update pipeline, emitting progress events
// via the emit callback so callers can stream status to SSE clients.
// It returns the final OperationResponse with all collected logs.
func UpdateStream(c *Client, image string, emit func(UpdateStepEvent)) (*types.OperationResponse, error) {
	logs := []string{}

	// Warn if a dashboard PR image is currently deployed — the operator update will overwrite it
	if dashState, dashErr := GetDashboardState(c); dashErr == nil && dashState.IsCustomPR {
		warning := fmt.Sprintf("Warning: Dashboard PR #%d is currently deployed. The operator update will overwrite it.", dashState.PRNumber)
		slog.Warn("operator update will overwrite PR-deployed dashboard", "pr", dashState.PRNumber)
		logs = append(logs, warning)
	}

	logs = append(logs, fmt.Sprintf("Target image: %s", image))

	// --- Step 1: validate_prerequisites ---
	emit(UpdateStepEvent{Step: "validate_prerequisites", Status: "running", Message: "Checking prerequisites..."})

	ps, err := getPullSecret(c)
	if err != nil {
		msg := fmt.Sprintf("Failed to check pull secret: %v", err)
		logs = append(logs, msg)
		emit(UpdateStepEvent{Step: "validate_prerequisites", Status: "failed", Message: msg, ErrorCode: errorCodeFromK8sErr(err)})
		recordUpdateActivity(c, image, false)
		return &types.OperationResponse{Success: false, Message: msg, Logs: logs, ErrorCode: errorCodeFromK8sErr(err)}, nil
	}
	if !ps.Exists {
		msg := "additional-pull-secret not found in kube-system. Run the one-time setup first."
		logs = append(logs, msg)
		emit(UpdateStepEvent{Step: "validate_prerequisites", Status: "failed", Message: msg, ErrorCode: "prerequisites"})
		recordUpdateActivity(c, image, false)
		return &types.OperationResponse{Success: false, Message: msg, Logs: logs, ErrorCode: "prerequisites"}, nil
	}
	logs = append(logs, "OK: additional-pull-secret exists")

	idms, err := getIDMS(c)
	if err != nil {
		msg := fmt.Sprintf("Failed to check IDMS: %v", err)
		logs = append(logs, msg)
		emit(UpdateStepEvent{Step: "validate_prerequisites", Status: "failed", Message: msg, ErrorCode: errorCodeFromK8sErr(err)})
		recordUpdateActivity(c, image, false)
		return &types.OperationResponse{Success: false, Message: msg, Logs: logs, ErrorCode: errorCodeFromK8sErr(err)}, nil
	}
	if !idms.Exists {
		msg := fmt.Sprintf("No IDMS found mirroring %s. Run the one-time setup first.", IDMSSource)
		logs = append(logs, msg)
		emit(UpdateStepEvent{Step: "validate_prerequisites", Status: "failed", Message: msg, ErrorCode: "prerequisites"})
		recordUpdateActivity(c, image, false)
		return &types.OperationResponse{Success: false, Message: msg, Logs: logs, ErrorCode: "prerequisites"}, nil
	}
	logs = append(logs, fmt.Sprintf("OK: IDMS exists (%s)", idms.Name))

	// Validate tag-digest consistency (prevents installing wrong version)
	if mismatchErr := validateTagDigestMatch(c.ctx, c, image); mismatchErr != nil {
		msg := mismatchErr.Error()
		logs = append(logs, msg)
		emit(UpdateStepEvent{Step: "validate_prerequisites", Status: "failed", Message: msg, ErrorCode: "validation"})
		recordUpdateActivity(c, image, false)
		return &types.OperationResponse{Success: false, Message: msg, Logs: logs, ErrorCode: "validation"}, nil
	}

	// Ensure operator namespace and OperatorGroup exist (creates them for fresh clusters)
	nsPath := "/api/v1/namespaces/" + SubNS
	_, _, nsErr := c.get(nsPath)
	if nsErr != nil && IsK8sError(nsErr, 404) {
		slog.Info("creating operator namespace", "namespace", SubNS)
		nsResource := map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Namespace",
			"metadata":   map[string]interface{}{"name": SubNS},
		}
		_, _, nsCreateErr := c.apply(nsPath, nsResource)
		if nsCreateErr != nil {
			msg := fmt.Sprintf("Failed to create namespace %s: %v", SubNS, nsCreateErr)
			logs = append(logs, msg)
			emit(UpdateStepEvent{Step: "validate_prerequisites", Status: "failed", Message: msg, ErrorCode: errorCodeFromK8sErr(nsCreateErr)})
			recordUpdateActivity(c, image, false)
			return &types.OperationResponse{Success: false, Message: msg, Logs: logs, ErrorCode: errorCodeFromK8sErr(nsCreateErr)}, nil
		}
		logs = append(logs, fmt.Sprintf("OK: Created namespace %s", SubNS))
	} else if nsErr != nil {
		msg := fmt.Sprintf("Failed to check namespace %s: %v", SubNS, nsErr)
		logs = append(logs, msg)
		emit(UpdateStepEvent{Step: "validate_prerequisites", Status: "failed", Message: msg, ErrorCode: errorCodeFromK8sErr(nsErr)})
		recordUpdateActivity(c, image, false)
		return &types.OperationResponse{Success: false, Message: msg, Logs: logs, ErrorCode: errorCodeFromK8sErr(nsErr)}, nil
	}

	ogPath := namespacedPath("operators.coreos.com/v1", "operatorgroups", SubNS, "")
	ogBody, _, ogErr := c.get(ogPath)
	ogExists := false
	if ogErr == nil {
		var ogResult map[string]interface{}
		if json.Unmarshal(ogBody, &ogResult) == nil {
			ogItems, _ := ogResult["items"].([]interface{})
			ogExists = len(ogItems) > 0
		}
	}
	if !ogExists {
		slog.Info("creating OperatorGroup", "namespace", SubNS)
		ogResource := map[string]interface{}{
			"apiVersion": "operators.coreos.com/v1",
			"kind":       "OperatorGroup",
			"metadata": map[string]interface{}{
				"name":      "rhods-operator",
				"namespace": SubNS,
			},
			"spec": map[string]interface{}{},
		}
		ogApplyPath := namespacedPath("operators.coreos.com/v1", "operatorgroups", SubNS, "rhods-operator")
		_, _, ogCreateErr := c.apply(ogApplyPath, ogResource)
		if ogCreateErr != nil {
			msg := fmt.Sprintf("Failed to create OperatorGroup in %s: %v", SubNS, ogCreateErr)
			logs = append(logs, msg)
			emit(UpdateStepEvent{Step: "validate_prerequisites", Status: "failed", Message: msg, ErrorCode: errorCodeFromK8sErr(ogCreateErr)})
			recordUpdateActivity(c, image, false)
			return &types.OperationResponse{Success: false, Message: msg, Logs: logs, ErrorCode: errorCodeFromK8sErr(ogCreateErr)}, nil
		}
		logs = append(logs, fmt.Sprintf("OK: Created OperatorGroup in %s", SubNS))
	} else {
		logs = append(logs, fmt.Sprintf("OK: OperatorGroup exists in %s", SubNS))
	}

	emit(UpdateStepEvent{Step: "validate_prerequisites", Status: "success", Message: "Prerequisites validated"})

	// Downgrade check: compare the target image version against the currently
	// installed CSV version. OLM does not support downgrades, so we block the
	// operation and direct the user to use Reinstall instead.
	targetTag := extractTagFromRef(image)
	targetParsed, targetOK := parseTag(targetTag)
	currentCSV, csvCheckErr := getCSV(c)
	if csvCheckErr == nil && currentCSV.Name != "" && targetOK {
		currentParsed, currentOK := parseCSVVersion(currentCSV.Name)
		if currentOK && compareTags(targetParsed, currentParsed) < 0 {
			downgradeMsg := fmt.Sprintf("Downgrade detected: target %s < current %s. OLM does not support downgrades. Use Reinstall instead.", targetTag, currentCSV.Name)
			logs = append(logs, downgradeMsg)
			emit(UpdateStepEvent{Step: "validate_prerequisites", Status: "failed", Message: downgradeMsg, ErrorCode: "validation"})
			recordUpdateActivity(c, image, false)
			return &types.OperationResponse{
				Success:   false,
				Message:   downgradeMsg,
				Logs:      logs,
				ErrorCode: "validation",
			}, nil
		}
	}

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

	// --- Step 3: apply_catalog_source ---
	emit(UpdateStepEvent{Step: "apply_catalog_source", Status: "running", Message: "Applying CatalogSource..."})
	logs = append(logs, fmt.Sprintf("Applying CatalogSource %s...", CatalogName))
	catalogSource := buildCatalogSourceSpec(image)

	csPath := namespacedPath("operators.coreos.com/v1alpha1", "catalogsources", CatalogNS, CatalogName)
	_, _, err = c.apply(csPath, catalogSource)
	if err != nil {
		msg := fmt.Sprintf("Failed to apply CatalogSource: %v", err)
		logs = append(logs, msg)
		emit(UpdateStepEvent{Step: "apply_catalog_source", Status: "failed", Message: msg, ErrorCode: errorCodeFromK8sErr(err)})
		recordUpdateActivity(c, image, false)
		return &types.OperationResponse{Success: false, Message: msg, Logs: logs, ErrorCode: errorCodeFromK8sErr(err)}, nil
	}
	logs = append(logs, "OK: CatalogSource applied")
	emit(UpdateStepEvent{Step: "apply_catalog_source", Status: "success", Message: "CatalogSource applied", Detail: image})

	// --- Step 4: wait_catalog_ready (CRITICAL FIX — wait before detecting channel) ---
	emit(UpdateStepEvent{Step: "wait_catalog_ready", Status: "running", Message: "Waiting for CatalogSource to become READY..."})
	logs = append(logs, "Waiting for CatalogSource to become READY...")

	catalogReady := false
	catalogTimeout := CatalogReadyTimeout
	catalogPollInterval := CatalogPollInterval
	catalogDeadline := time.Now().Add(catalogTimeout)

	for time.Now().Before(catalogDeadline) {
		select {
		case <-c.ctx.Done():
			msg := "Operation cancelled while waiting for CatalogSource"
			logs = append(logs, msg)
			emit(UpdateStepEvent{Step: "wait_catalog_ready", Status: "failed", Message: msg})
			recordUpdateActivity(c, image, false)
			return &types.OperationResponse{Success: false, Message: msg, Logs: logs}, c.ctx.Err()
		case <-time.After(catalogPollInterval):
		}

		cs, csErr := getCatalogSource(c)
		if csErr != nil {
			slog.Warn("error polling CatalogSource state", "error", csErr)
			continue
		}
		state := cs.State
		logs = append(logs, fmt.Sprintf("  CatalogSource state: %s", state))
		emit(UpdateStepEvent{Step: "wait_catalog_ready", Status: "running", Message: fmt.Sprintf("CatalogSource: %s", state), Detail: state})

		if state == "READY" {
			catalogReady = true
			break
		}
	}

	if !catalogReady {
		msg := "CatalogSource did not reach READY state within 120s"
		logs = append(logs, msg)
		emit(UpdateStepEvent{Step: "wait_catalog_ready", Status: "failed", Message: msg})
		recordUpdateActivity(c, image, false)
		return &types.OperationResponse{Success: false, Message: msg, Logs: logs, ErrorCode: "timeout"}, nil
	}
	logs = append(logs, "OK: CatalogSource is READY")
	emit(UpdateStepEvent{Step: "wait_catalog_ready", Status: "success", Message: "CatalogSource is READY"})

	// Poll PackageManifest until the nightly catalog's channels appear.
	// The CatalogSource reports READY before OLM refreshes the PackageManifest.
	// Instead of a fixed wait, poll every 3s until we detect channels for our
	// target version, with a timeout.
	emit(UpdateStepEvent{Step: "detect_channel", Status: "running", Message: "Waiting for catalog channels to refresh..."})
	pmReady := false
	pmStart := time.Now()
	for time.Since(pmStart) < PackageManifestPropagationWait {
		ch, _ := detectNightlyChannel(c, image)
		if ch != "" {
			pmReady = true
			logs = append(logs, fmt.Sprintf("OK: PackageManifest ready in %s (channel: %s)", time.Since(pmStart).Round(time.Second), ch))
			break
		}
		select {
		case <-c.ctx.Done():
			return nil, c.ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	if !pmReady {
		logs = append(logs, fmt.Sprintf("Warning: PackageManifest not ready after %s, proceeding with detection", PackageManifestPropagationWait))
	}

	// --- Step 5: detect_channel ---
	emit(UpdateStepEvent{Step: "detect_channel", Status: "running", Message: "Detecting target channel..."})
	logs = append(logs, "Detecting target channel...")

	// Get the current subscription to use as fallback channel
	sub, subErr := getSubscription(c)
	if subErr != nil {
		msg := fmt.Sprintf("Failed to check Subscription: %v", subErr)
		logs = append(logs, msg)
		emit(UpdateStepEvent{Step: "detect_channel", Status: "failed", Message: msg, ErrorCode: errorCodeFromK8sErr(subErr)})
		recordUpdateActivity(c, image, false)
		return &types.OperationResponse{Success: false, Message: msg, Logs: logs, ErrorCode: errorCodeFromK8sErr(subErr)}, nil
	}
	targetChannel := sub.Channel

	// Retry up to 3 times with ChannelRetryDelay between each attempt.
	var nightlyChannel string
	var channelDetectFailed bool
	for attempt := 1; attempt <= 3; attempt++ {
		ch, chErr := detectNightlyChannel(c, image)
		if chErr != nil {
			slog.Warn("detectNightlyChannel failed", "attempt", attempt, "error", chErr)
			if attempt < 3 {
				select {
				case <-c.ctx.Done():
				case <-time.After(ChannelRetryDelay):
				}
				continue
			}
			// All retries exhausted — report failure
			channelDetectFailed = true
			logs = append(logs, fmt.Sprintf("Failed to detect nightly channel after %d attempts, fallback channel: %s", attempt, targetChannel))
			break
		}
		nightlyChannel = ch
		break
	}
	if channelDetectFailed {
		msg := fmt.Sprintf("Failed to detect nightly channel after 3 attempts. Fallback channel: %s", targetChannel)
		emit(UpdateStepEvent{Step: "detect_channel", Status: "failed", Message: msg, ErrorCode: "channel_detection"})
		recordUpdateActivity(c, image, false)
		return &types.OperationResponse{
			Success:   false,
			Message:   msg,
			Logs:      logs,
			ErrorCode: "channel_detection",
		}, nil
	}
	if nightlyChannel == "" {
		// Falling back to the channel from the current Subscription — validate it exists
		logs = append(logs, fmt.Sprintf("Warning: no channels found in catalog, using current: %s", targetChannel))
		exists, valErr := channelExistsInCatalog(c, targetChannel)
		if valErr != nil {
			slog.Warn("failed to validate fallback channel in catalog", "channel", targetChannel, "error", valErr)
		} else if !exists {
			slog.Error("fallback channel does not exist in catalog", "channel", targetChannel)
			logs = append(logs, fmt.Sprintf("Warning: fallback channel %q not found in catalog — subscription may fail", targetChannel))
		}
		emit(UpdateStepEvent{Step: "detect_channel", Status: "success", Message: fmt.Sprintf("No channels in catalog, using fallback: %s", targetChannel), Detail: targetChannel})
	} else {
		targetChannel = nightlyChannel
		logs = append(logs, fmt.Sprintf("Detected nightly channel: %s", nightlyChannel))
		emit(UpdateStepEvent{Step: "detect_channel", Status: "success", Message: fmt.Sprintf("Detected channel: %s", nightlyChannel), Detail: nightlyChannel})
	}

	// --- Step 6: apply_subscription ---
	emit(UpdateStepEvent{Step: "apply_subscription", Status: "running", Message: "Applying Subscription..."})
	logs = append(logs, "Applying Subscription...")

	newSub := map[string]interface{}{
		"apiVersion": "operators.coreos.com/v1alpha1",
		"kind":       "Subscription",
		"metadata": map[string]interface{}{
			"name":      SubName,
			"namespace": SubNS,
		},
		"spec": map[string]interface{}{
			"channel":             targetChannel,
			"installPlanApproval": "Automatic",
			"name":                SubName,
			"source":              CatalogName,
			"sourceNamespace":     CatalogNS,
		},
	}
	subApplyPath := namespacedPath("operators.coreos.com/v1alpha1", "subscriptions", SubNS, SubName)
	subRetryBackoffs := SubRetryBackoffs
	var subApplyErr error
	for attempt := 1; attempt <= 3; attempt++ {
		_, _, subApplyErr = c.apply(subApplyPath, newSub)
		if subApplyErr == nil {
			break
		}
		if attempt < 3 {
			slog.Warn("failed to apply Subscription, retrying", "attempt", attempt, "error", subApplyErr)
			logs = append(logs, fmt.Sprintf("  Warning: Subscription apply attempt %d failed: %v — retrying", attempt, subApplyErr))
			select {
			case <-c.ctx.Done():
				msg := "Operation cancelled during Subscription apply retry"
				emit(UpdateStepEvent{Step: "apply_subscription", Status: "failed", Message: msg})
				recordUpdateActivity(c, image, false)
				return &types.OperationResponse{Success: false, Message: msg, Logs: logs}, c.ctx.Err()
			case <-time.After(subRetryBackoffs[attempt-1]):
			}
		}
	}
	if subApplyErr != nil {
		msg := fmt.Sprintf("Failed to apply Subscription after 3 attempts: %v", subApplyErr)
		logs = append(logs, msg)
		emit(UpdateStepEvent{Step: "apply_subscription", Status: "failed", Message: msg, ErrorCode: errorCodeFromK8sErr(subApplyErr)})
		recordUpdateActivity(c, image, false)
		return &types.OperationResponse{Success: false, Message: msg, Logs: logs, ErrorCode: errorCodeFromK8sErr(subApplyErr)}, nil
	}
	logs = append(logs, fmt.Sprintf("OK: Subscription applied (channel=%s)", targetChannel))
	emit(UpdateStepEvent{Step: "apply_subscription", Status: "success", Message: "Subscription applied", Detail: targetChannel})

	// --- Step 7: delete_csv ---
	// Delete the CSV AND the old InstallPlan, then recreate the Subscription.
	// Just deleting the CSV is not enough for same-version refreshes — OLM sees
	// installedCSV == currentCSV and won't create a new InstallPlan. We must:
	// 1. Delete the CSV (removes operator pods)
	// 2. Delete the old InstallPlan (clears the "already installed" state)
	// 3. Delete and recreate the Subscription (clears installedCSV status)
	emit(UpdateStepEvent{Step: "delete_csv", Status: "running", Message: "Deleting old CSV and InstallPlan..."})
	logs = append(logs, "Deleting old CSV and InstallPlan...")

	csv, csvErr := getCSV(c)
	if csvErr != nil {
		slog.Warn("failed to look up CSV", "error", csvErr)
		logs = append(logs, fmt.Sprintf("Warning: could not look up CSV: %v", csvErr))
	} else if csv.Name != "" {
		csvPath := namespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", SubNS, csv.Name)
		_, csvDelErr := c.delete(csvPath)
		if csvDelErr != nil && !IsK8sError(csvDelErr, 404) {
			slog.Warn("failed to delete CSV", "name", csv.Name, "error", csvDelErr)
			logs = append(logs, fmt.Sprintf("Warning: CSV delete failed: %v", csvDelErr))
		} else {
			logs = append(logs, fmt.Sprintf("OK: CSV %s deleted", csv.Name))
		}
	}

	// Delete old InstallPlans
	ipListPath := namespacedPath("operators.coreos.com/v1alpha1", "installplans", SubNS, "")
	ipListBody, _, ipListErr := c.get(ipListPath)
	if ipListErr == nil {
		var ipList struct {
			Items []struct {
				Metadata struct {
					Name string `json:"name"`
				} `json:"metadata"`
			} `json:"items"`
		}
		if json.Unmarshal(ipListBody, &ipList) == nil {
			for _, ip := range ipList.Items {
				ipDelPath := namespacedPath("operators.coreos.com/v1alpha1", "installplans", SubNS, ip.Metadata.Name)
				c.delete(ipDelPath)
				logs = append(logs, fmt.Sprintf("OK: InstallPlan %s deleted", ip.Metadata.Name))
			}
		}
	}

	// Delete and recreate Subscription to clear installedCSV status
	subDelPath := namespacedPath("operators.coreos.com/v1alpha1", "subscriptions", SubNS, SubName)
	c.delete(subDelPath)
	logs = append(logs, "OK: Subscription deleted (clearing installedCSV)")

	select {
	case <-c.ctx.Done():
	case <-time.After(2 * time.Second):
	}

	// Recreate Subscription (fresh, no installedCSV)
	_, _, subRecreateErr := c.apply(subApplyPath, newSub)
	if subRecreateErr != nil {
		msg := fmt.Sprintf("Failed to recreate Subscription: %v", subRecreateErr)
		logs = append(logs, msg)
		emit(UpdateStepEvent{Step: "delete_csv", Status: "failed", Message: msg, ErrorCode: errorCodeFromK8sErr(subRecreateErr)})
		recordUpdateActivity(c, image, false)
		return &types.OperationResponse{Success: false, Message: msg, Logs: logs, ErrorCode: errorCodeFromK8sErr(subRecreateErr)}, nil
	}
	logs = append(logs, "OK: Subscription recreated — OLM will create fresh InstallPlan")
	emit(UpdateStepEvent{Step: "delete_csv", Status: "success", Message: "CSV, InstallPlan deleted, Subscription recreated"})

	// --- Step 8: verify_installplan ---
	emit(UpdateStepEvent{Step: "verify_installplan", Status: "running", Message: "Waiting for InstallPlan creation..."})
	logs = append(logs, "Waiting for InstallPlan creation...")

	ipFound := false
	ipName := ""
	ipTimeout := InstallPlanPollTimeout
	ipPollInterval := InstallPlanPollInterval
	ipDeadline := time.Now().Add(ipTimeout)

	for time.Now().Before(ipDeadline) {
		select {
		case <-c.ctx.Done():
			msg := "Operation cancelled while waiting for InstallPlan"
			logs = append(logs, msg)
			emit(UpdateStepEvent{Step: "verify_installplan", Status: "failed", Message: msg})
			recordUpdateActivity(c, image, false)
			return &types.OperationResponse{Success: false, Message: msg, Logs: logs}, c.ctx.Err()
		case <-time.After(ipPollInterval):
		}

		// Check Subscription status.installPlanRef for the InstallPlan name
		subBody, _, subGetErr := c.get(subApplyPath)
		if subGetErr != nil {
			slog.Debug("error polling Subscription for installPlanRef", "error", subGetErr)
			continue
		}
		var subResult map[string]interface{}
		if jsonErr := json.Unmarshal(subBody, &subResult); jsonErr != nil {
			continue
		}
		subStatus, _ := subResult["status"].(map[string]interface{})
		if subStatus == nil {
			continue
		}
		ipRef, _ := subStatus["installPlanRef"].(map[string]interface{})
		if ipRef != nil {
			if name, ok := ipRef["name"].(string); ok && name != "" {
				ipName = name
				ipFound = true
				break
			}
		}
		// Also check installplan field
		if ipField, ok := subStatus["installplan"].(map[string]interface{}); ok {
			if name, ok := ipField["name"].(string); ok && name != "" {
				ipName = name
				ipFound = true
				break
			}
		}
	}

	if !ipFound {
		msg := "No InstallPlan created within 60s — OLM may be stuck"
		logs = append(logs, msg)
		emit(UpdateStepEvent{Step: "verify_installplan", Status: "failed", Message: msg})
		// Non-fatal: the update was applied, OLM may still reconcile
		logs = append(logs, "Update applied but InstallPlan verification timed out. OLM may still be processing.")
		logs = append(logs, "Refresh the status to monitor progress.")
		recordUpdateActivity(c, image, true)
		return &types.OperationResponse{
			Success: true,
			Message: "RHOAI nightly update initiated (InstallPlan pending — OLM may still be processing).",
			Logs:    logs,
		}, nil
	}

	logs = append(logs, fmt.Sprintf("OK: InstallPlan created: %s", ipName))
	emit(UpdateStepEvent{Step: "verify_installplan", Status: "success", Message: fmt.Sprintf("InstallPlan created: %s", ipName)})

	logs = append(logs, "Update complete. OLM is installing the new operator version.")
	logs = append(logs, "Refresh the status to monitor progress.")

	recordUpdateActivity(c, image, true)

	return &types.OperationResponse{
		Success: true,
		Message: "RHOAI nightly update initiated successfully.",
		Logs:    logs,
	}, nil
}

// Update applies a nightly CatalogSource image and patches the Subscription accordingly.
// For non-dry-run calls it delegates to UpdateStream with a no-op emitter that collects logs.
func Update(c *Client, image string, dryRun bool) (*types.OperationResponse, error) {
	if !dryRun {
		// Delegate to UpdateStream with a silent emitter — all progress is
		// captured in the returned OperationResponse.Logs.
		return UpdateStream(c, image, func(UpdateStepEvent) {})
	}

	// --- Dry-run path (unchanged) ---
	logs := []string{}

	// Warn if a dashboard PR image is currently deployed — the operator update will overwrite it
	if dashState, dashErr := GetDashboardState(c); dashErr == nil && dashState.IsCustomPR {
		warning := fmt.Sprintf("Warning: Dashboard PR #%d is currently deployed. The operator update will overwrite it.", dashState.PRNumber)
		slog.Warn("operator update will overwrite PR-deployed dashboard", "pr", dashState.PRNumber)
		logs = append(logs, warning)
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
		logs = append(logs, "[DRY-RUN] WARNING: Pull secret exists but is invalid (missing quay.io/rhoai credentials)")
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

	// Check if namespace and OperatorGroup need to be created (fresh cluster)
	dryNsPath := "/api/v1/namespaces/" + SubNS
	_, _, nsCheckErr := c.get(dryNsPath)
	if nsCheckErr != nil && IsK8sError(nsCheckErr, 404) {
		logs = append(logs, fmt.Sprintf("[DRY-RUN] Would create namespace %s", SubNS))
		logs = append(logs, fmt.Sprintf("[DRY-RUN] Would create OperatorGroup in %s", SubNS))
	}

	// Downgrade check (dry-run): compare the target image version against the
	// currently installed CSV version.
	dryRunTag := extractTagFromRef(image)
	dryRunParsed, dryRunOK := parseTag(dryRunTag)
	dryRunCSV, dryRunCSVErr := getCSV(c)
	if dryRunCSVErr == nil && dryRunCSV.Name != "" && dryRunOK {
		dryRunCurrentParsed, dryRunCurrentOK := parseCSVVersion(dryRunCSV.Name)
		if dryRunCurrentOK && compareTags(dryRunParsed, dryRunCurrentParsed) < 0 {
			logs = append(logs, fmt.Sprintf("[DRY-RUN] WARNING: Downgrade detected: target %s < current %s. OLM does not support downgrades. Use Reinstall instead.", dryRunTag, dryRunCSV.Name))
		}
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
			ErrorCode: errorCodeFromK8sErr(csDryErr),
		}, nil
	}
	if cs.Exists && cs.Image == image {
		logs = append(logs, "OK: CatalogSource already points to this image — no change needed (validated)")
	} else if cs.Exists {
		logs = append(logs, fmt.Sprintf("OK: CatalogSource apply validated (would update image)"))
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
	} else if sub.Source == CatalogName {
		logs = append(logs, fmt.Sprintf("[DRY-RUN] Subscription already points to %s", CatalogName))
	} else {
		logs = append(logs, fmt.Sprintf("[DRY-RUN] Would patch Subscription source: %s -> %s", sub.Source, CatalogName))
	}

	return &types.OperationResponse{
		Success: true,
		Message: "Dry run complete. No changes were made.",
		Logs:    logs,
	}, nil
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

// ReinstallStream executes the full uninstall/cleanup/reinstall pipeline,
// emitting progress events via the emit callback so callers can stream
// status to SSE clients. It returns the final OperationResponse with all
// collected logs.
//
// targetType: "stable" reinstalls from the stable catalog; "nightly"
// reinstalls with the specified FBC image.
// channelOverride: if non-empty, forces the Subscription channel instead
// of auto-detecting from the catalog.
func ReinstallStream(c *Client, targetType, image, channelOverride string, emit func(UpdateStepEvent)) (*types.OperationResponse, error) {
	logs := []string{}

	stableSource := getStableSource()
	stableChannel := getStableChannel()
	isNightly := targetType == "nightly"

	// Warn if a dashboard PR image is currently deployed
	if dashState, dashErr := GetDashboardState(c); dashErr == nil && dashState.IsCustomPR {
		warning := fmt.Sprintf("Warning: Dashboard PR #%d is currently deployed. The operator reinstall will overwrite it.", dashState.PRNumber)
		slog.Warn("operator reinstall will overwrite PR-deployed dashboard", "pr", dashState.PRNumber)
		logs = append(logs, warning)
	}

	// --- Step 1: validate_target ---
	emit(UpdateStepEvent{Step: "validate_target", Status: "running", Message: "Validating reinstall target..."})

	sub, err := getSubscription(c)
	if err != nil {
		msg := fmt.Sprintf("Failed to get subscription: %v", err)
		logs = append(logs, msg)
		emit(UpdateStepEvent{Step: "validate_target", Status: "failed", Message: msg, ErrorCode: errorCodeFromK8sErr(err)})
		recordReinstallActivity(c, targetType, image, false)
		return &types.OperationResponse{Success: false, Message: msg, Logs: logs, ErrorCode: errorCodeFromK8sErr(err)}, nil
	}

	// For stable target: check if already on stable
	if !isNightly && sub.Source == stableSource {
		msg := "Already on stable. Nothing to reinstall."
		logs = append(logs, fmt.Sprintf("Subscription source is already %s / %s", stableSource, stableChannel))
		emit(UpdateStepEvent{Step: "validate_target", Status: "skipped", Message: msg})
		return &types.OperationResponse{
			Success: true,
			Message: msg,
			Logs:    logs,
		}, nil
	}

	if isNightly {
		logs = append(logs, fmt.Sprintf("Reinstalling to nightly: %s", image))
	} else {
		logs = append(logs, fmt.Sprintf("Reinstalling to stable: %s/%s -> %s/%s", sub.Source, sub.Channel, stableSource, stableChannel))
	}
	emit(UpdateStepEvent{Step: "validate_target", Status: "success", Message: "Reinstall target validated"})

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

	// --- Step 3: delete_catalog_source ---
	emit(UpdateStepEvent{Step: "delete_catalog_source", Status: "running", Message: "Removing nightly CatalogSource..."})
	csPath := namespacedPath("operators.coreos.com/v1alpha1", "catalogsources", CatalogNS, CatalogName)
	_, delErr := c.delete(csPath)
	if delErr != nil {
		if !IsK8sError(delErr, 404) {
			slog.Warn("failed to delete nightly CatalogSource", "name", CatalogName, "error", delErr)
			logs = append(logs, fmt.Sprintf("Warning: failed to delete nightly CatalogSource: %v", delErr))
			emit(UpdateStepEvent{Step: "delete_catalog_source", Status: "success", Message: "CatalogSource delete had warnings", Detail: delErr.Error()})
		} else {
			logs = append(logs, "OK: nightly CatalogSource already absent")
			emit(UpdateStepEvent{Step: "delete_catalog_source", Status: "skipped", Message: "Nightly CatalogSource already absent"})
		}
	} else {
		logs = append(logs, "OK: nightly CatalogSource deleted")
		emit(UpdateStepEvent{Step: "delete_catalog_source", Status: "success", Message: "Nightly CatalogSource deleted"})
	}

	// --- Step 4: delete_subscription ---
	emit(UpdateStepEvent{Step: "delete_subscription", Status: "running", Message: "Removing operator Subscription..."})
	subPath := namespacedPath("operators.coreos.com/v1alpha1", "subscriptions", SubNS, SubName)
	_, delErr = c.delete(subPath)
	if delErr != nil {
		if !IsK8sError(delErr, 404) {
			msg := fmt.Sprintf("Failed to delete Subscription: %v", delErr)
			logs = append(logs, msg)
			emit(UpdateStepEvent{Step: "delete_subscription", Status: "failed", Message: msg, ErrorCode: errorCodeFromK8sErr(delErr)})
			recordReinstallActivity(c, targetType, image, false)
			return &types.OperationResponse{Success: false, Message: msg, Logs: logs, ErrorCode: errorCodeFromK8sErr(delErr)}, nil
		}
		logs = append(logs, "OK: Subscription already absent")
		emit(UpdateStepEvent{Step: "delete_subscription", Status: "skipped", Message: "Subscription already absent"})
	} else {
		logs = append(logs, "OK: Subscription deleted")
		emit(UpdateStepEvent{Step: "delete_subscription", Status: "success", Message: "Subscription deleted"})
	}

	// --- Step 5: delete_csv ---
	emit(UpdateStepEvent{Step: "delete_csv", Status: "running", Message: "Removing current CSV..."})
	csv, csvErr := getCSV(c)
	if csvErr != nil {
		logs = append(logs, fmt.Sprintf("Warning: could not look up CSV: %v", csvErr))
		emit(UpdateStepEvent{Step: "delete_csv", Status: "skipped", Message: "Could not look up CSV", Detail: csvErr.Error()})
	} else if csv.Name != "" {
		csvPath := namespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", SubNS, csv.Name)
		_, delErr = c.delete(csvPath)
		if delErr != nil {
			if !IsK8sError(delErr, 404) {
				logs = append(logs, fmt.Sprintf("Warning: failed to delete CSV %s: %v", csv.Name, delErr))
				emit(UpdateStepEvent{Step: "delete_csv", Status: "success", Message: fmt.Sprintf("CSV delete had warnings: %v", delErr)})
			} else {
				logs = append(logs, fmt.Sprintf("OK: CSV %s already absent", csv.Name))
				emit(UpdateStepEvent{Step: "delete_csv", Status: "skipped", Message: fmt.Sprintf("CSV %s already absent", csv.Name)})
			}
		} else {
			logs = append(logs, fmt.Sprintf("OK: CSV %s deleted", csv.Name))
			emit(UpdateStepEvent{Step: "delete_csv", Status: "success", Message: fmt.Sprintf("CSV %s deleted", csv.Name)})
		}
	} else {
		logs = append(logs, "No RHOAI CSV found (already removed)")
		emit(UpdateStepEvent{Step: "delete_csv", Status: "skipped", Message: "No RHOAI CSV found"})
	}

	// --- Step 6: cleanup_webhooks ---
	emit(UpdateStepEvent{Step: "cleanup_webhooks", Status: "running", Message: "Cleaning up stale webhooks..."})
	cleanedCount, webhookWarnings := cleanupStaleWebhooks(c)
	logs = append(logs, fmt.Sprintf("Removed %d stale webhook configurations", cleanedCount))
	for _, w := range webhookWarnings {
		slog.Warn("stale webhook cleanup issue", "detail", w)
		logs = append(logs, fmt.Sprintf("Warning: %s", w))
	}
	emit(UpdateStepEvent{Step: "cleanup_webhooks", Status: "success", Message: fmt.Sprintf("Removed %d stale webhooks", cleanedCount)})

	// --- Step 6b: cleanup_stale_component_crs ---
	// After EA↔GA transitions, component CRs can get stuck with finalizers
	// during deletion. Remove finalizers to unblock (K8s finalizer docs pattern).
	unstuckCount := cleanupStuckComponentCRs(c)
	if unstuckCount > 0 {
		logs = append(logs, fmt.Sprintf("Unblocked %d stuck component CR(s) by removing finalizers", unstuckCount))
	}

	// --- Step 7: patch_crds ---
	emit(UpdateStepEvent{Step: "patch_crds", Status: "running", Message: "Patching CRD conversion webhooks..."})
	patchedCRDs := patchCRDConversionWebhooks(c)
	logs = append(logs, fmt.Sprintf("Patched %d CRDs to remove conversion webhooks", patchedCRDs))
	emit(UpdateStepEvent{Step: "patch_crds", Status: "success", Message: fmt.Sprintf("Patched %d CRDs", patchedCRDs)})

	// --- Step 8: wait_propagation ---
	emit(UpdateStepEvent{Step: "wait_propagation", Status: "running", Message: "Waiting for cleanup to propagate..."})
	select {
	case <-c.ctx.Done():
		msg := "Operation cancelled during cleanup wait."
		logs = append(logs, msg)
		emit(UpdateStepEvent{Step: "wait_propagation", Status: "failed", Message: msg})
		recordReinstallActivity(c, targetType, image, false)
		return &types.OperationResponse{Success: false, Message: msg, Logs: logs}, c.ctx.Err()
	case <-time.After(PropagationWait):
	}
	logs = append(logs, "OK: waited 10 seconds for cleanup propagation")
	emit(UpdateStepEvent{Step: "wait_propagation", Status: "success", Message: "Cleanup propagated (10s wait)"})

	// --- Steps 9+ diverge for stable vs nightly ---
	if isNightly {
		return reinstallNightlySteps(c, image, channelOverride, sub, logs, emit)
	}
	return reinstallStableSteps(c, stableSource, stableChannel, logs, emit)
}

// reinstallNightlySteps handles the nightly-specific portion of ReinstallStream:
// create CatalogSource, wait for READY, detect channel, create Subscription, verify InstallPlan.
func reinstallNightlySteps(c *Client, image, channelOverride string, sub types.SubscriptionInfo, logs []string, emit func(UpdateStepEvent)) (*types.OperationResponse, error) {
	// Validate tag-digest consistency before proceeding
	if mismatchErr := validateTagDigestMatch(c.ctx, c, image); mismatchErr != nil {
		msg := mismatchErr.Error()
		logs = append(logs, msg)
		emit(UpdateStepEvent{Step: "create_catalog_source", Status: "failed", Message: msg, ErrorCode: "validation"})
		return &types.OperationResponse{Success: false, Message: msg, Logs: logs, ErrorCode: "validation"}, nil
	}

	// --- Step 9: create_catalog_source ---
	emit(UpdateStepEvent{Step: "create_catalog_source", Status: "running", Message: "Creating CatalogSource with nightly image..."})
	catalogSource := buildCatalogSourceSpec(image)
	csApplyPath := namespacedPath("operators.coreos.com/v1alpha1", "catalogsources", CatalogNS, CatalogName)
	_, _, err := c.apply(csApplyPath, catalogSource)
	if err != nil {
		msg := fmt.Sprintf("Failed to create nightly CatalogSource: %v", err)
		logs = append(logs, msg)
		emit(UpdateStepEvent{Step: "create_catalog_source", Status: "failed", Message: msg, ErrorCode: errorCodeFromK8sErr(err)})
		recordReinstallActivity(c, "nightly", image, false)
		return &types.OperationResponse{Success: false, Message: msg, Logs: logs, ErrorCode: errorCodeFromK8sErr(err)}, nil
	}
	logs = append(logs, "OK: CatalogSource created with nightly image")
	emit(UpdateStepEvent{Step: "create_catalog_source", Status: "success", Message: "CatalogSource created", Detail: image})

	// --- Step 10: wait_catalog_ready ---
	emit(UpdateStepEvent{Step: "wait_catalog_ready", Status: "running", Message: "Waiting for CatalogSource to become READY..."})
	logs = append(logs, "Waiting for CatalogSource to become READY...")

	catalogReady := false
	catalogTimeout := CatalogReadyTimeout
	catalogPollInterval := CatalogPollInterval
	catalogDeadline := time.Now().Add(catalogTimeout)

	for time.Now().Before(catalogDeadline) {
		select {
		case <-c.ctx.Done():
			msg := "Operation cancelled while waiting for CatalogSource"
			logs = append(logs, msg)
			emit(UpdateStepEvent{Step: "wait_catalog_ready", Status: "failed", Message: msg})
			recordReinstallActivity(c, "nightly", image, false)
			return &types.OperationResponse{Success: false, Message: msg, Logs: logs}, c.ctx.Err()
		case <-time.After(catalogPollInterval):
		}

		cs, csErr := getCatalogSource(c)
		if csErr != nil {
			slog.Warn("error polling CatalogSource state", "error", csErr)
			continue
		}
		state := cs.State
		logs = append(logs, fmt.Sprintf("  CatalogSource state: %s", state))
		emit(UpdateStepEvent{Step: "wait_catalog_ready", Status: "running", Message: fmt.Sprintf("CatalogSource: %s", state), Detail: state})

		if state == "READY" {
			catalogReady = true
			break
		}
	}

	if !catalogReady {
		msg := "CatalogSource did not reach READY state within 120s"
		logs = append(logs, msg)
		emit(UpdateStepEvent{Step: "wait_catalog_ready", Status: "failed", Message: msg})
		recordReinstallActivity(c, "nightly", image, false)
		return &types.OperationResponse{Success: false, Message: msg, Logs: logs, ErrorCode: "timeout"}, nil
	}
	logs = append(logs, "OK: CatalogSource is READY")
	emit(UpdateStepEvent{Step: "wait_catalog_ready", Status: "success", Message: "CatalogSource is READY"})

	// Poll PackageManifest until the nightly catalog's channels appear (same as UpdateStream)
	emit(UpdateStepEvent{Step: "detect_channel", Status: "running", Message: "Waiting for catalog channels to refresh..."})
	pmReady := false
	pmStart := time.Now()
	for time.Since(pmStart) < PackageManifestPropagationWait {
		if c.ctx.Err() != nil {
			return nil, c.ctx.Err()
		}
		ch, _ := detectNightlyChannel(c, image)
		if ch != "" {
			pmReady = true
			logs = append(logs, fmt.Sprintf("OK: PackageManifest ready in %s (channel: %s)", time.Since(pmStart).Round(time.Second), ch))
			break
		}
		select {
		case <-c.ctx.Done():
			return nil, c.ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	if !pmReady {
		logs = append(logs, fmt.Sprintf("Warning: PackageManifest not ready after %s, proceeding with detection", PackageManifestPropagationWait))
	}

	// --- Step 11: detect_channel ---
	emit(UpdateStepEvent{Step: "detect_channel", Status: "running", Message: "Detecting target channel..."})

	var targetChannel string
	if channelOverride != "" {
		targetChannel = channelOverride
		logs = append(logs, fmt.Sprintf("Using channel override: %s", channelOverride))
		emit(UpdateStepEvent{Step: "detect_channel", Status: "success", Message: fmt.Sprintf("Using override channel: %s", channelOverride), Detail: channelOverride})
	} else {
		for attempt := 1; attempt <= 3; attempt++ {
			ch, chErr := detectNightlyChannel(c, image)
			if chErr != nil {
				slog.Warn("detectNightlyChannel failed in ReinstallStream, retrying", "attempt", attempt, "error", chErr)
				if attempt < 3 {
					select {
					case <-c.ctx.Done():
						msg := "Operation cancelled during channel detection."
						emit(UpdateStepEvent{Step: "detect_channel", Status: "failed", Message: msg})
						recordReinstallActivity(c, "nightly", image, false)
						return &types.OperationResponse{Success: false, Message: msg, Logs: logs}, c.ctx.Err()
					case <-time.After(ChannelRetryDelay):
					}
					continue
				}
				// All retries exhausted
				targetChannel = sub.Channel
				slog.Warn("detectNightlyChannel failed after retries, falling back",
					"fallbackChannel", targetChannel)
				logs = append(logs, fmt.Sprintf("WARNING: Could not detect nightly channel after %d attempts, falling back to '%s'", attempt, targetChannel))
				logs = append(logs, "This channel may be stale -- verify the catalog contains this channel if the install fails")
				emit(UpdateStepEvent{Step: "detect_channel", Status: "success", Message: fmt.Sprintf("Channel detection failed, using fallback: %s", targetChannel), Detail: targetChannel})
				break
			}
			if ch == "" {
				targetChannel = sub.Channel
				logs = append(logs, fmt.Sprintf("No channels found in catalog, using current: %s", targetChannel))
				// Validate fallback channel exists in the catalog before creating Subscription
				if exists, valErr := channelExistsInCatalog(c, targetChannel); valErr != nil {
					slog.Warn("failed to validate fallback channel in catalog during reinstall", "channel", targetChannel, "error", valErr)
				} else if !exists {
					slog.Error("fallback channel does not exist in catalog during reinstall", "channel", targetChannel)
					logs = append(logs, fmt.Sprintf("Warning: fallback channel %q not found in catalog — subscription may fail", targetChannel))
				}
				emit(UpdateStepEvent{Step: "detect_channel", Status: "success", Message: fmt.Sprintf("No channels in catalog, using fallback: %s", targetChannel), Detail: targetChannel})
			} else {
				targetChannel = ch
				logs = append(logs, fmt.Sprintf("Detected nightly channel: %s", targetChannel))
				emit(UpdateStepEvent{Step: "detect_channel", Status: "success", Message: fmt.Sprintf("Detected channel: %s", targetChannel), Detail: targetChannel})
			}
			break
		}
	}

	// --- Step 12: create_subscription ---
	emit(UpdateStepEvent{Step: "create_subscription", Status: "running", Message: "Creating Subscription to nightly catalog..."})
	newSub := map[string]interface{}{
		"apiVersion": "operators.coreos.com/v1alpha1",
		"kind":       "Subscription",
		"metadata": map[string]interface{}{
			"name":      SubName,
			"namespace": SubNS,
		},
		"spec": map[string]interface{}{
			"channel":             targetChannel,
			"installPlanApproval": "Automatic",
			"name":                SubName,
			"source":              CatalogName,
			"sourceNamespace":     CatalogNS,
		},
	}
	subApplyPath := namespacedPath("operators.coreos.com/v1alpha1", "subscriptions", SubNS, SubName)
	subRetryBackoffs := SubRetryBackoffs
	var subApplyErr error
	for attempt := 1; attempt <= 3; attempt++ {
		_, _, subApplyErr = c.apply(subApplyPath, newSub)
		if subApplyErr == nil {
			break
		}
		if attempt < 3 {
			slog.Warn("failed to create nightly Subscription, retrying",
				"attempt", attempt, "error", subApplyErr,
				"backoff", subRetryBackoffs[attempt-1])
			logs = append(logs, fmt.Sprintf("Warning: Subscription creation attempt %d failed: %v -- retrying in %s",
				attempt, subApplyErr, subRetryBackoffs[attempt-1]))
			emit(UpdateStepEvent{Step: "create_subscription", Status: "running",
				Message: fmt.Sprintf("Attempt %d failed, retrying...", attempt)})
			select {
			case <-c.ctx.Done():
				msg := "Operation cancelled during Subscription creation retry."
				emit(UpdateStepEvent{Step: "create_subscription", Status: "failed", Message: msg})
				recordReinstallActivity(c, "nightly", image, false)
				return &types.OperationResponse{Success: false, Message: msg, Logs: logs}, c.ctx.Err()
			case <-time.After(subRetryBackoffs[attempt-1]):
			}
		}
	}
	if subApplyErr != nil {
		msg := fmt.Sprintf("Failed to create nightly Subscription after 3 attempts: %v. Manual intervention required.", subApplyErr)
		slog.Error("all attempts to create nightly Subscription failed", "error", subApplyErr)
		logs = append(logs, fmt.Sprintf("CRITICAL: All 3 attempts to create Subscription failed: %v", subApplyErr))
		logs = append(logs, "Manual intervention required: the cluster has no operator Subscription.")
		emit(UpdateStepEvent{Step: "create_subscription", Status: "failed", Message: msg, ErrorCode: errorCodeFromK8sErr(subApplyErr)})
		recordReinstallActivity(c, "nightly", image, false)
		return &types.OperationResponse{Success: false, Message: msg, Logs: logs, ErrorCode: errorCodeFromK8sErr(subApplyErr)}, nil
	}
	logs = append(logs, "OK: Subscription created pointing to nightly catalog")
	emit(UpdateStepEvent{Step: "create_subscription", Status: "success", Message: "Subscription created", Detail: targetChannel})

	// --- Step 13: verify_installplan ---
	emit(UpdateStepEvent{Step: "verify_installplan", Status: "running", Message: "Verifying InstallPlan creation..."})
	logs = append(logs, "Verifying InstallPlan creation...")

	ipFound := false
	ipName := ""
	ipTimeout := InstallPlanPollTimeout
	ipPollInterval := InstallPlanPollInterval
	ipDeadline := time.Now().Add(ipTimeout)

	for time.Now().Before(ipDeadline) {
		select {
		case <-c.ctx.Done():
			msg := "Operation cancelled while waiting for InstallPlan"
			logs = append(logs, msg)
			emit(UpdateStepEvent{Step: "verify_installplan", Status: "failed", Message: msg})
			recordReinstallActivity(c, "nightly", image, false)
			return &types.OperationResponse{Success: false, Message: msg, Logs: logs}, c.ctx.Err()
		case <-time.After(ipPollInterval):
		}

		subBody, _, subGetErr := c.get(subApplyPath)
		if subGetErr != nil {
			slog.Debug("error polling Subscription for installPlanRef", "error", subGetErr)
			continue
		}
		var subResult map[string]interface{}
		if jsonErr := json.Unmarshal(subBody, &subResult); jsonErr != nil {
			continue
		}
		subStatus, _ := subResult["status"].(map[string]interface{})
		if subStatus == nil {
			continue
		}
		ipRef, _ := subStatus["installPlanRef"].(map[string]interface{})
		if ipRef != nil {
			if name, ok := ipRef["name"].(string); ok && name != "" {
				ipName = name
				ipFound = true
				break
			}
		}
		if ipField, ok := subStatus["installplan"].(map[string]interface{}); ok {
			if name, ok := ipField["name"].(string); ok && name != "" {
				ipName = name
				ipFound = true
				break
			}
		}
	}

	if !ipFound {
		msg := "No InstallPlan created within 60s -- OLM may still be processing"
		logs = append(logs, msg)
		emit(UpdateStepEvent{Step: "verify_installplan", Status: "failed", Message: msg})
		logs = append(logs, "Reinstall applied but InstallPlan verification timed out. OLM may still be processing.")
		logs = append(logs, "Refresh the status to monitor progress.")
		recordReinstallActivity(c, "nightly", image, true)
		return &types.OperationResponse{
			Success: true,
			Message: "Reinstall to nightly initiated (InstallPlan pending -- OLM may still be processing).",
			Logs:    logs,
		}, nil
	}

	logs = append(logs, fmt.Sprintf("OK: InstallPlan created: %s", ipName))
	emit(UpdateStepEvent{Step: "verify_installplan", Status: "success", Message: fmt.Sprintf("InstallPlan created: %s", ipName)})

	logs = append(logs, "Reinstall complete. OLM is installing the nightly operator. This may take several minutes.")
	logs = append(logs, "Refresh the status to monitor progress.")

	recordReinstallActivity(c, "nightly", image, true)

	return &types.OperationResponse{
		Success: true,
		Message: "Reinstall to nightly initiated. OLM is installing the operator. This may take several minutes.",
		Logs:    logs,
	}, nil
}

// reinstallStableSteps handles the stable-specific portion of ReinstallStream:
// create Subscription to stable catalog, verify InstallPlan.
func reinstallStableSteps(c *Client, stableSource, stableChannel string, logs []string, emit func(UpdateStepEvent)) (*types.OperationResponse, error) {
	// --- Step 9: create_subscription ---
	emit(UpdateStepEvent{Step: "create_subscription", Status: "running", Message: "Creating fresh Subscription to stable catalog..."})
	newSub := map[string]interface{}{
		"apiVersion": "operators.coreos.com/v1alpha1",
		"kind":       "Subscription",
		"metadata": map[string]interface{}{
			"name":      SubName,
			"namespace": SubNS,
		},
		"spec": map[string]interface{}{
			"channel":             stableChannel,
			"installPlanApproval": "Automatic",
			"name":                SubName,
			"source":              stableSource,
			"sourceNamespace":     CatalogNS,
		},
	}
	subApplyPath := namespacedPath("operators.coreos.com/v1alpha1", "subscriptions", SubNS, SubName)
	stableRetryBackoffs := SubRetryBackoffs
	var stableApplyErr error
	for attempt := 1; attempt <= 3; attempt++ {
		_, _, stableApplyErr = c.apply(subApplyPath, newSub)
		if stableApplyErr == nil {
			break
		}
		if attempt < 3 {
			slog.Warn("failed to create stable Subscription, retrying",
				"attempt", attempt, "error", stableApplyErr,
				"backoff", stableRetryBackoffs[attempt-1])
			logs = append(logs, fmt.Sprintf("Warning: Subscription creation attempt %d failed: %v -- retrying in %s",
				attempt, stableApplyErr, stableRetryBackoffs[attempt-1]))
			emit(UpdateStepEvent{Step: "create_subscription", Status: "running",
				Message: fmt.Sprintf("Attempt %d failed, retrying...", attempt)})
			select {
			case <-c.ctx.Done():
				msg := "Operation cancelled during Subscription creation retry."
				emit(UpdateStepEvent{Step: "create_subscription", Status: "failed", Message: msg})
				recordReinstallActivity(c, "stable", "", false)
				return &types.OperationResponse{Success: false, Message: msg, Logs: logs}, c.ctx.Err()
			case <-time.After(stableRetryBackoffs[attempt-1]):
			}
		}
	}
	if stableApplyErr != nil {
		msg := fmt.Sprintf("Failed to create stable Subscription after 3 attempts: %v. Manual intervention required.", stableApplyErr)
		slog.Error("all attempts to create stable Subscription failed", "error", stableApplyErr)
		logs = append(logs, fmt.Sprintf("CRITICAL: All 3 attempts to create Subscription failed: %v", stableApplyErr))
		logs = append(logs, "Manual intervention required: the cluster has no operator Subscription.")
		emit(UpdateStepEvent{Step: "create_subscription", Status: "failed", Message: msg, ErrorCode: errorCodeFromK8sErr(stableApplyErr)})
		recordReinstallActivity(c, "stable", "", false)
		return &types.OperationResponse{Success: false, Message: msg, Logs: logs, ErrorCode: errorCodeFromK8sErr(stableApplyErr)}, nil
	}
	logs = append(logs, "OK: Subscription created pointing to stable catalog")
	emit(UpdateStepEvent{Step: "create_subscription", Status: "success", Message: "Subscription created", Detail: stableChannel})

	// --- Step 10: verify_installplan ---
	emit(UpdateStepEvent{Step: "verify_installplan", Status: "running", Message: "Verifying InstallPlan creation..."})
	logs = append(logs, "Verifying InstallPlan creation...")

	ipFound := false
	ipName := ""
	ipTimeout := InstallPlanPollTimeout
	ipPollInterval := InstallPlanPollInterval
	ipDeadline := time.Now().Add(ipTimeout)

	for time.Now().Before(ipDeadline) {
		select {
		case <-c.ctx.Done():
			msg := "Operation cancelled while waiting for InstallPlan"
			logs = append(logs, msg)
			emit(UpdateStepEvent{Step: "verify_installplan", Status: "failed", Message: msg})
			recordReinstallActivity(c, "stable", "", false)
			return &types.OperationResponse{Success: false, Message: msg, Logs: logs}, c.ctx.Err()
		case <-time.After(ipPollInterval):
		}

		subBody, _, subGetErr := c.get(subApplyPath)
		if subGetErr != nil {
			slog.Debug("error polling Subscription for installPlanRef", "error", subGetErr)
			continue
		}
		var subResult map[string]interface{}
		if jsonErr := json.Unmarshal(subBody, &subResult); jsonErr != nil {
			continue
		}
		subStatus, _ := subResult["status"].(map[string]interface{})
		if subStatus == nil {
			continue
		}
		ipRef, _ := subStatus["installPlanRef"].(map[string]interface{})
		if ipRef != nil {
			if name, ok := ipRef["name"].(string); ok && name != "" {
				ipName = name
				ipFound = true
				break
			}
		}
		if ipField, ok := subStatus["installplan"].(map[string]interface{}); ok {
			if name, ok := ipField["name"].(string); ok && name != "" {
				ipName = name
				ipFound = true
				break
			}
		}
	}

	if !ipFound {
		msg := "No InstallPlan created within 60s -- OLM may still be processing"
		logs = append(logs, msg)
		emit(UpdateStepEvent{Step: "verify_installplan", Status: "failed", Message: msg})
		logs = append(logs, "Reinstall applied but InstallPlan verification timed out. OLM may still be processing.")
		logs = append(logs, "Refresh the status to monitor progress.")
		recordReinstallActivity(c, "stable", "", true)
		return &types.OperationResponse{
			Success: true,
			Message: "Reinstall to stable initiated (InstallPlan pending -- OLM may still be processing).",
			Logs:    logs,
		}, nil
	}

	logs = append(logs, fmt.Sprintf("OK: InstallPlan created: %s", ipName))
	emit(UpdateStepEvent{Step: "verify_installplan", Status: "success", Message: fmt.Sprintf("InstallPlan created: %s", ipName)})

	logs = append(logs, "Reinstall complete. OLM is installing the stable operator. This may take several minutes.")
	logs = append(logs, "Refresh the status to monitor progress.")

	recordReinstallActivity(c, "stable", "", true)

	return &types.OperationResponse{
		Success: true,
		Message: "Reinstall to stable initiated. OLM is installing the stable operator. This may take several minutes.",
		Logs:    logs,
	}, nil
}

func cleanupStaleWebhooks(c *Client) (int, []string) {
	count := 0
	var warnings []string

	// Clean validating webhook configurations
	vwhPath := clusterPath("admissionregistration.k8s.io/v1", "validatingwebhookconfigurations", "")
	body, _, err := c.get(vwhPath)
	if err != nil {
		warnings = append(warnings, fmt.Sprintf("failed to list validating webhooks: %v", err))
	} else {
		var result map[string]interface{}
		if unmarshalErr := json.Unmarshal(body, &result); unmarshalErr != nil {
			warnings = append(warnings, fmt.Sprintf("failed to parse validating webhooks response: %v", unmarshalErr))
		} else {
			items, _ := result["items"].([]interface{})
			for _, item := range items {
				obj, _ := item.(map[string]interface{})
				meta, _ := obj["metadata"].(map[string]interface{})
				name, _ := meta["name"].(string)
				labels, _ := meta["labels"].(map[string]interface{})

				if isRHOAIWebhook(name, labels) {
					delPath := vwhPath + "/" + name
					_, delErr := c.delete(delPath)
					if delErr != nil {
						warnings = append(warnings, fmt.Sprintf("failed to delete validating webhook %s: %v", name, delErr))
					} else {
						count++
					}
				}
			}
		}
	}

	// Clean mutating webhook configurations
	mwhPath := clusterPath("admissionregistration.k8s.io/v1", "mutatingwebhookconfigurations", "")
	body, _, err = c.get(mwhPath)
	if err != nil {
		warnings = append(warnings, fmt.Sprintf("failed to list mutating webhooks: %v", err))
	} else {
		var result map[string]interface{}
		if unmarshalErr := json.Unmarshal(body, &result); unmarshalErr != nil {
			warnings = append(warnings, fmt.Sprintf("failed to parse mutating webhooks response: %v", unmarshalErr))
		} else {
			items, _ := result["items"].([]interface{})
			for _, item := range items {
				obj, _ := item.(map[string]interface{})
				meta, _ := obj["metadata"].(map[string]interface{})
				name, _ := meta["name"].(string)
				labels, _ := meta["labels"].(map[string]interface{})

				if isRHOAIWebhook(name, labels) {
					delPath := mwhPath + "/" + name
					_, delErr := c.delete(delPath)
					if delErr != nil {
						warnings = append(warnings, fmt.Sprintf("failed to delete mutating webhook %s: %v", name, delErr))
					} else {
						count++
					}
				}
			}
		}
	}

	return count, warnings
}

// deleteStaleWebhooksOnly removes only RHOAI webhook configurations whose backing
// service no longer exists. Unlike cleanupStaleWebhooks (which removes all RHOAI
// webhooks for the Reinstall flow), this function is safe for the diagnostics
// auto-fix path because it preserves webhooks that are still actively serving.
func deleteStaleWebhooksOnly(c *Client) (int, []string) {
	count := 0
	var warnings []string

	// Clean validating webhook configurations
	vwhPath := clusterPath("admissionregistration.k8s.io/v1", "validatingwebhookconfigurations", "")
	body, _, err := c.get(vwhPath)
	if err != nil {
		warnings = append(warnings, fmt.Sprintf("failed to list validating webhooks: %v", err))
	} else {
		var result map[string]interface{}
		if unmarshalErr := json.Unmarshal(body, &result); unmarshalErr != nil {
			warnings = append(warnings, fmt.Sprintf("failed to parse validating webhooks response: %v", unmarshalErr))
		} else {
			items, _ := result["items"].([]interface{})
			for _, item := range items {
				obj, _ := item.(map[string]interface{})
				meta, _ := obj["metadata"].(map[string]interface{})
				name, _ := meta["name"].(string)
				labels, _ := meta["labels"].(map[string]interface{})

				if isRHOAIWebhook(name, labels) && isWebhookStale(c, obj) {
					delPath := vwhPath + "/" + name
					_, delErr := c.delete(delPath)
					if delErr != nil {
						warnings = append(warnings, fmt.Sprintf("failed to delete validating webhook %s: %v", name, delErr))
					} else {
						count++
					}
				}
			}
		}
	}

	// Clean mutating webhook configurations
	mwhPath := clusterPath("admissionregistration.k8s.io/v1", "mutatingwebhookconfigurations", "")
	body, _, err = c.get(mwhPath)
	if err != nil {
		warnings = append(warnings, fmt.Sprintf("failed to list mutating webhooks: %v", err))
	} else {
		var result map[string]interface{}
		if unmarshalErr := json.Unmarshal(body, &result); unmarshalErr != nil {
			warnings = append(warnings, fmt.Sprintf("failed to parse mutating webhooks response: %v", unmarshalErr))
		} else {
			items, _ := result["items"].([]interface{})
			for _, item := range items {
				obj, _ := item.(map[string]interface{})
				meta, _ := obj["metadata"].(map[string]interface{})
				name, _ := meta["name"].(string)
				labels, _ := meta["labels"].(map[string]interface{})

				if isRHOAIWebhook(name, labels) && isWebhookStale(c, obj) {
					delPath := mwhPath + "/" + name
					_, delErr := c.delete(delPath)
					if delErr != nil {
						warnings = append(warnings, fmt.Sprintf("failed to delete mutating webhook %s: %v", name, delErr))
					} else {
						count++
					}
				}
			}
		}
	}

	return count, warnings
}

// isRHOAIWebhook checks whether a webhook configuration belongs to the RHOAI operator
// by inspecting the olm.owner label and the resource name.
func isRHOAIWebhook(name string, labels map[string]interface{}) bool {
	if labels != nil {
		if ownerStr, ok := labels["olm.owner"].(string); ok {
			if strings.Contains(ownerStr, "rhods") || strings.Contains(ownerStr, "opendatahub") {
				return true
			}
		}
	}
	if strings.Contains(name, "opendatahub") || strings.Contains(name, "rhods") {
		return true
	}
	return false
}

// isWebhookStale checks whether a webhook's backing service exists.
// A webhook is stale if its service has been deleted (e.g., after an operator uninstall).
func isWebhookStale(c *Client, webhookObj map[string]interface{}) bool {
	webhooks, _ := webhookObj["webhooks"].([]interface{})
	for _, wh := range webhooks {
		whMap, _ := wh.(map[string]interface{})
		clientConfig, _ := whMap["clientConfig"].(map[string]interface{})
		svcRef, _ := clientConfig["service"].(map[string]interface{})
		if svcRef == nil {
			continue
		}
		svcName, _ := svcRef["name"].(string)
		svcNS, _ := svcRef["namespace"].(string)
		if svcName == "" || svcNS == "" {
			continue
		}
		svcPath := fmt.Sprintf("/api/v1/namespaces/%s/services/%s", svcNS, svcName)
		_, _, err := c.get(svcPath)
		if err != nil && IsK8sError(err, 404) {
			return true
		}
	}
	return false
}

// patchCRDConversionWebhooks patches RHOAI CRDs to remove conversion webhook configs.
// During the rollback window (operator down), CRD conversion webhooks would fail because
// the webhook service is gone. Patching to strategy=None prevents API failures.
// The new operator will re-add the conversion webhook when it starts.
// cleanupStuckComponentCRs finds component CRs that have a deletionTimestamp
// and finalizers (stuck deleting) and removes the finalizers to unblock deletion.
// This is the documented K8s pattern for unblocking stuck deletions:
// https://kubernetes.io/docs/concepts/overview/working-with-objects/finalizers/
func cleanupStuckComponentCRs(c *Client) int {
	componentResources := []string{
		"codeflares", "dashboards", "datasciencepipelines", "feastoperators",
		"kserves", "kueues", "llamastackoperators", "mlflowoperators",
		"modelcontrollers", "modelmeshservings", "modelregistries",
		"modelsasservices", "ogxs", "rays", "sparkoperators",
		"trainers", "trainingoperators", "trustyais", "workbenches",
	}

	unstuck := 0
	for _, resource := range componentResources {
		listPath := fmt.Sprintf("/apis/components.platform.opendatahub.io/v1alpha1/%s", resource)
		body, _, err := c.get(listPath)
		if err != nil {
			continue
		}

		var list struct {
			Items []struct {
				Metadata struct {
					Name              string   `json:"name"`
					Finalizers        []string `json:"finalizers"`
					DeletionTimestamp *string  `json:"deletionTimestamp"`
				} `json:"metadata"`
			} `json:"items"`
		}
		if err := json.Unmarshal(body, &list); err != nil {
			continue
		}

		for _, item := range list.Items {
			if item.Metadata.DeletionTimestamp != nil && len(item.Metadata.Finalizers) > 0 {
				crPath := fmt.Sprintf("%s/%s", listPath, item.Metadata.Name)
				_, _, patchErr := c.patch(crPath, []byte(`{"metadata":{"finalizers":[]}}`))
				if patchErr == nil {
					slog.Info("removed stuck finalizer from component CR",
						"resource", resource, "name", item.Metadata.Name,
						"finalizers", item.Metadata.Finalizers)
					unstuck++
				} else {
					slog.Warn("failed to remove finalizer from component CR",
						"resource", resource, "name", item.Metadata.Name, "error", patchErr)
				}
			}
		}
	}
	return unstuck
}

func patchCRDConversionWebhooks(c *Client) int {
	crds := []string{
		"datascienceclusters.datasciencecluster.opendatahub.io",
		"dscinitializations.dscinitialization.opendatahub.io",
	}
	count := 0
	for _, crd := range crds {
		patchData, _ := json.Marshal(map[string]interface{}{
			"spec": map[string]interface{}{
				"conversion": map[string]interface{}{
					"strategy": "None",
				},
			},
		})
		path := clusterPath("apiextensions.k8s.io/v1", "customresourcedefinitions", crd)
		_, _, err := c.patch(path, patchData)
		if err != nil {
			slog.Warn("failed to patch CRD conversion webhook", "crd", crd, "error", err)
		} else {
			count++
		}
	}
	return count
}

func recordRollbackActivity(c *Client, success bool) {
	RecordActivity(c, types.ActivityEntry{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		User:      getUser(c),
		Action:    "rollback",
		Detail:    fmt.Sprintf("to %s/%s", getStableSource(), getStableChannel()),
		Success:   success,
	})
}

func recordReinstallActivity(c *Client, targetType, image string, success bool) {
	detail := fmt.Sprintf("to %s/%s", getStableSource(), getStableChannel())
	if targetType == "nightly" {
		detail = fmt.Sprintf("to nightly %s", image)
	}
	RecordActivity(c, types.ActivityEntry{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		User:      getUser(c),
		Action:    "reinstall",
		Detail:    detail,
		Success:   success,
	})
}

// RefreshOperator deletes the current CSV to trigger OLM to reinstall
// from the updated catalog. This is safe and non-destructive — it keeps
// the Subscription, DSCI, DSC, and all user workloads.
// It delegates to RefreshOperatorStream with a no-op emitter.
func RefreshOperator(c *Client) (*types.OperationResponse, error) {
	return RefreshOperatorStream(c, func(UpdateStepEvent) {})
}

// RefreshOperatorStream executes the refresh pipeline, emitting progress events
// via the emit callback so callers can stream status to SSE clients.
// It deletes the current CSV and Subscription, waits for cleanup, recreates
// the Subscription to trigger a fresh InstallPlan, and verifies the InstallPlan.
func RefreshOperatorStream(c *Client, emit func(UpdateStepEvent)) (*types.OperationResponse, error) {
	logs := []string{}

	// --- Step 1: verify_csv ---
	emit(UpdateStepEvent{Step: "verify_csv", Status: "running", Message: "Verifying operator is installed..."})

	csv, err := getCSV(c)
	if err != nil {
		msg := fmt.Sprintf("Failed to look up CSV: %v", err)
		logs = append(logs, msg)
		emit(UpdateStepEvent{Step: "verify_csv", Status: "failed", Message: msg, ErrorCode: errorCodeFromK8sErr(err)})
		return &types.OperationResponse{Success: false, Message: msg, Logs: logs, ErrorCode: errorCodeFromK8sErr(err)}, nil
	}
	if csv.Name == "" {
		msg := "No RHOAI operator CSV found. The operator may still be installing -- check the status and try again once the CSV appears."
		logs = append(logs, "No CSV with displayName 'Red Hat OpenShift AI' found in the redhat-ods-operator namespace.")
		emit(UpdateStepEvent{Step: "verify_csv", Status: "success", Message: msg})
		return &types.OperationResponse{
			Success: true,
			Message: msg,
			Logs:    logs,
		}, nil
	}
	logs = append(logs, fmt.Sprintf("Current CSV: %s (%s)", csv.Name, csv.Phase))
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
		msg := fmt.Sprintf("Failed to get Subscription: %v", err)
		logs = append(logs, msg)
		emit(UpdateStepEvent{Step: "get_subscription", Status: "failed", Message: msg, ErrorCode: errorCodeFromK8sErr(err)})
		return &types.OperationResponse{Success: false, Message: msg, Logs: logs, ErrorCode: errorCodeFromK8sErr(err)}, nil
	}
	logs = append(logs, fmt.Sprintf("Subscription: source=%s, channel=%s", sub.Source, sub.Channel))
	emit(UpdateStepEvent{Step: "get_subscription", Status: "success", Message: fmt.Sprintf("Subscription: %s/%s", sub.Source, sub.Channel), Detail: sub.Channel})

	// --- Step 4: delete_csv ---
	emit(UpdateStepEvent{Step: "delete_csv", Status: "running", Message: "Deleting CSV to trigger fresh install..."})
	logs = append(logs, fmt.Sprintf("Deleting CSV %s...", csv.Name))

	csvPath := namespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", SubNS, csv.Name)
	_, csvDelErr := c.delete(csvPath)
	if csvDelErr != nil && !IsK8sError(csvDelErr, 404) {
		msg := fmt.Sprintf("Failed to delete CSV %s: %v", csv.Name, csvDelErr)
		logs = append(logs, msg)
		emit(UpdateStepEvent{Step: "delete_csv", Status: "failed", Message: msg, ErrorCode: errorCodeFromK8sErr(csvDelErr)})
		return &types.OperationResponse{Success: false, Message: msg, Logs: logs, ErrorCode: errorCodeFromK8sErr(csvDelErr)}, nil
	}
	logs = append(logs, "OK: CSV deleted")
	emit(UpdateStepEvent{Step: "delete_csv", Status: "success", Message: fmt.Sprintf("CSV %s deleted", csv.Name)})

	// --- Step 5: delete_subscription ---
	emit(UpdateStepEvent{Step: "delete_subscription", Status: "running", Message: "Deleting Subscription..."})
	logs = append(logs, "Deleting Subscription...")

	subPath := namespacedPath("operators.coreos.com/v1alpha1", "subscriptions", SubNS, SubName)
	_, subDelErr := c.delete(subPath)
	if subDelErr != nil && !IsK8sError(subDelErr, 404) {
		msg := fmt.Sprintf("Failed to delete Subscription: %v", subDelErr)
		logs = append(logs, msg)
		emit(UpdateStepEvent{Step: "delete_subscription", Status: "failed", Message: msg, ErrorCode: errorCodeFromK8sErr(subDelErr)})
		return &types.OperationResponse{Success: false, Message: msg, Logs: logs, ErrorCode: errorCodeFromK8sErr(subDelErr)}, nil
	}
	logs = append(logs, "OK: Subscription deleted")
	emit(UpdateStepEvent{Step: "delete_subscription", Status: "success", Message: "Subscription deleted"})

	// --- Step 6: wait_cleanup ---
	emit(UpdateStepEvent{Step: "wait_cleanup", Status: "running", Message: "Waiting for cleanup to propagate..."})
	logs = append(logs, "Waiting for cleanup to propagate...")

	select {
	case <-c.ctx.Done():
		msg := "Operation cancelled during cleanup wait."
		logs = append(logs, msg)
		emit(UpdateStepEvent{Step: "wait_cleanup", Status: "failed", Message: msg})
		RecordActivity(c, types.ActivityEntry{
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			User:      getUser(c),
			Action:    "refresh",
			Detail:    csv.Name,
			Success:   false,
		})
		return &types.OperationResponse{Success: false, Message: msg, Logs: logs}, c.ctx.Err()
	case <-time.After(RefreshCleanupWait):
	}
	logs = append(logs, "OK: waited 5 seconds")
	emit(UpdateStepEvent{Step: "wait_cleanup", Status: "success", Message: "Cleanup propagated"})

	// --- Step 7: recreate_subscription ---
	emit(UpdateStepEvent{Step: "recreate_subscription", Status: "running", Message: "Recreating Subscription..."})
	logs = append(logs, "Recreating Subscription...")

	newSub := map[string]interface{}{
		"apiVersion": "operators.coreos.com/v1alpha1",
		"kind":       "Subscription",
		"metadata": map[string]interface{}{
			"name":      SubName,
			"namespace": SubNS,
		},
		"spec": map[string]interface{}{
			"channel":             sub.Channel,
			"installPlanApproval": "Automatic",
			"name":                SubName,
			"source":              sub.Source,
			"sourceNamespace":     CatalogNS,
		},
	}
	subApplyPath := namespacedPath("operators.coreos.com/v1alpha1", "subscriptions", SubNS, SubName)

	retryBackoffs := SubRetryBackoffs
	var applyErr error
	for attempt := 1; attempt <= 3; attempt++ {
		_, _, applyErr = c.apply(subApplyPath, newSub)
		if applyErr == nil {
			break
		}
		slog.Warn("Subscription recreation failed, retrying", "attempt", attempt, "error", applyErr)
		logs = append(logs, fmt.Sprintf("  Attempt %d/3 failed: %v", attempt, applyErr))
		if attempt < 3 {
			select {
			case <-c.ctx.Done():
				msg := "Operation cancelled during Subscription recreation retry."
				emit(UpdateStepEvent{Step: "recreate_subscription", Status: "failed", Message: msg})
				RecordActivity(c, types.ActivityEntry{
					Timestamp: time.Now().UTC().Format(time.RFC3339),
					User:      getUser(c),
					Action:    "refresh",
					Detail:    csv.Name,
					Success:   false,
				})
				return &types.OperationResponse{Success: false, Message: msg, Logs: logs}, c.ctx.Err()
			case <-time.After(retryBackoffs[attempt-1]):
			}
		}
	}
	if applyErr != nil {
		msg := fmt.Sprintf("Failed to recreate Subscription after 3 attempts: %v", applyErr)
		logs = append(logs, msg)
		emit(UpdateStepEvent{Step: "recreate_subscription", Status: "failed", Message: msg, ErrorCode: errorCodeFromK8sErr(applyErr)})
		RecordActivity(c, types.ActivityEntry{
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			User:      getUser(c),
			Action:    "refresh",
			Detail:    csv.Name,
			Success:   false,
		})
		return &types.OperationResponse{
			Success:   false,
			Message:   msg,
			Logs:      logs,
			ErrorCode: errorCodeFromK8sErr(applyErr),
		}, nil
	}
	logs = append(logs, "OK: Subscription recreated")
	emit(UpdateStepEvent{Step: "recreate_subscription", Status: "success", Message: "Subscription recreated", Detail: sub.Channel})

	// --- Step 8: verify_installplan ---
	emit(UpdateStepEvent{Step: "verify_installplan", Status: "running", Message: "Verifying InstallPlan creation..."})
	logs = append(logs, "Waiting for InstallPlan creation...")

	ipFound := false
	ipName := ""
	ipTimeout := InstallPlanPollTimeout
	ipPollInterval := InstallPlanPollInterval
	ipDeadline := time.Now().Add(ipTimeout)

	for time.Now().Before(ipDeadline) {
		select {
		case <-c.ctx.Done():
			msg := "Operation cancelled while waiting for InstallPlan"
			logs = append(logs, msg)
			emit(UpdateStepEvent{Step: "verify_installplan", Status: "failed", Message: msg})
			RecordActivity(c, types.ActivityEntry{
				Timestamp: time.Now().UTC().Format(time.RFC3339),
				User:      getUser(c),
				Action:    "refresh",
				Detail:    csv.Name,
				Success:   false,
			})
			return &types.OperationResponse{Success: false, Message: msg, Logs: logs}, c.ctx.Err()
		case <-time.After(ipPollInterval):
		}

		subBody, _, subGetErr := c.get(subApplyPath)
		if subGetErr != nil {
			slog.Debug("error polling Subscription for installPlanRef", "error", subGetErr)
			continue
		}
		var subResult map[string]interface{}
		if jsonErr := json.Unmarshal(subBody, &subResult); jsonErr != nil {
			continue
		}
		subStatus, _ := subResult["status"].(map[string]interface{})
		if subStatus == nil {
			continue
		}
		ipRef, _ := subStatus["installPlanRef"].(map[string]interface{})
		if ipRef != nil {
			if name, ok := ipRef["name"].(string); ok && name != "" {
				ipName = name
				ipFound = true
				break
			}
		}
		if ipField, ok := subStatus["installplan"].(map[string]interface{}); ok {
			if name, ok := ipField["name"].(string); ok && name != "" {
				ipName = name
				ipFound = true
				break
			}
		}
	}

	if !ipFound {
		msg := "No InstallPlan created within 60s -- OLM may still be processing"
		logs = append(logs, msg)
		emit(UpdateStepEvent{Step: "verify_installplan", Status: "failed", Message: msg})
		logs = append(logs, "Refresh initiated but InstallPlan verification timed out. OLM may still be processing.")
		logs = append(logs, "This typically takes 1-3 minutes.")
		RecordActivity(c, types.ActivityEntry{
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			User:      getUser(c),
			Action:    "refresh",
			Detail:    csv.Name,
			Success:   true,
		})
		return &types.OperationResponse{
			Success: true,
			Message: "Operator refresh initiated (InstallPlan pending -- OLM may still be processing).",
			Logs:    logs,
		}, nil
	}

	logs = append(logs, fmt.Sprintf("OK: InstallPlan created: %s", ipName))
	emit(UpdateStepEvent{Step: "verify_installplan", Status: "success", Message: fmt.Sprintf("InstallPlan created: %s", ipName)})

	logs = append(logs, "OLM will create a new InstallPlan and reinstall with updated images.")
	logs = append(logs, "This typically takes 1-3 minutes.")

	RecordActivity(c, types.ActivityEntry{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		User:      getUser(c),
		Action:    "refresh",
		Detail:    csv.Name,
		Success:   true,
	})

	return &types.OperationResponse{
		Success: true,
		Message: "Operator refresh initiated. OLM will reinstall with updated images.",
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

	// Validate base64 encoding
	decoded, err := base64.StdEncoding.DecodeString(auth)
	if err != nil {
		// Try URL-safe or raw encoding as fallback
		decoded, err = base64.RawStdEncoding.DecodeString(auth)
		if err != nil {
			return &types.OperationResponse{
				Success:   false,
				Message:   "Auth value is not valid base64",
				Logs:      logs,
				ErrorCode: "validation",
			}, nil
		}
	}

	// Validate it looks like username:password
	if !strings.Contains(string(decoded), ":") {
		return &types.OperationResponse{
			Success:   false,
			Message:   "Auth value must decode to username:password format",
			Logs:      logs,
			ErrorCode: "validation",
		}, nil
	}

	logs = append(logs, "Auth value validated")

	// Check if secret already exists
	secretPath := namespacedPath("v1", "secrets", "kube-system", "additional-pull-secret")
	_, _, getErr := c.get(secretPath)
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

	// Construct the docker config using json.Marshal for safety (no string interpolation)
	dockerConfig := map[string]interface{}{
		"auths": map[string]interface{}{
			"quay.io/rhoai": map[string]interface{}{
				"auth": auth,
			},
		},
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
