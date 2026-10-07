package cluster

import (
	"container/list"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Thin wrappers and accessors that only tests use; production code calls
// the underlying functions directly.

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

// RefreshOperator re-deploys the installed operator version: it deletes the
// CSV and Subscription and recreates the Subscription from the same catalog
// and channel. It cannot pick up newer images: the nightly catalog image and
// every image in the CSV are pinned by digest, so OLM reinstalls the same
// bundle. Use Update for a newer build. The DSCI, DSC and user workloads are
// kept. It delegates to RefreshOperatorStream with a no-op emitter.
func RefreshOperator(c *Client) (*types.OperationResponse, error) {
	return RefreshOperatorStream(c, func(UpdateStepEvent) {})
}

// RefreshOperatorStream executes the refresh pipeline, emitting progress events
// via the emit callback so callers can stream status to SSE clients.
// It deletes the Subscription and then the CSV, recreates the Subscription
// with its previous spec, and waits for OLM to install the CSV again.
func RefreshOperatorStream(c *Client, emit func(UpdateStepEvent)) (*types.OperationResponse, error) {
	return RefreshOperatorStreamWithOptions(c, OperationOptions{}, emit)
}

// DeployPRImage patches the rhods-dashboard deployment with PR images.
// It checks all 8 dashboard container repos on Quay for pr-N tags and
// patches every container that has a published image.
func DeployPRImage(c *Client, prNumber int) (*types.OperationResponse, error) {
	return DeployPRImageWithFlavor(c, prNumber, "")
}

func DeployDashboardMain(c *Client) (*types.OperationResponse, error) {
	return deployDashboardBuild(c, "main", 0, "")
}

func populateDashboardDevState(c *Client, state *types.DashboardState) {
	operator, err := readDashboardOperator(c)
	populateDashboardDevStateWithOperator(c, state, operator, err)
}

func patchControlledDashboardImage(c *Client, operator *dashboardDeployment, image types.DashboardDevImage, target string) error {
	return patchControlledDashboardImages(c, operator, []dashboardImagePatch{{Image: image, Target: target}})
}

func checkDSCCompatibility(c *Client, dsc map[string]interface{}) *types.DSCCompatibility {
	op, err := getInstalledOperator(c)
	apiVersion, _ := dsc["apiVersion"].(string)
	version, _ := dscAPIVersion(apiVersion)
	return checkDSCCompatibilityFor(c, dscRead{Object: dsc, State: DSCStatePresent, Version: version}, op, err)
}

// Len returns the number of stored entries, including expired ones not yet removed.
func (c *lruCache[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}

// Purge removes every entry.
func (c *lruCache[K, V]) Purge() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ll.Init()
	c.items = make(map[K]*list.Element)
}
