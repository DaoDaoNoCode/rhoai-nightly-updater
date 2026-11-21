package cluster

import (
	"fmt"
	"sync"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// GetSummary reads the cluster summary every signed-in user sees (GET
// /api/summary), with the ServiceAccount's client c. It reuses the reads
// of GetStatus and of the Components page (the Subscription and its CSV,
// the nightly CatalogSource and the Quay lookup of its newest build, the
// DataScienceCluster's components) and copies only the reviewed fields
// into types.Summary: the pull secret is read for its quay.io/rhoai
// credential only, which the Quay lookup uses, as /api/status does; no
// name of a node, pod, project or user is read into it. UpdaterVersion is
// set by the handler.
func GetSummary(c *Client) (*types.Summary, error) {
	var (
		wg       sync.WaitGroup
		sub      subscriptionDetails
		subErr   error
		csv      types.CSVInfo
		csvErr   error
		cs       types.CatalogSourceInfo
		csErr    error
		quayAuth string
		dsc      dscRead
		dscErr   error
	)
	wg.Go(func() {
		if sub, subErr = readSubscription(c); subErr == nil && sub.found {
			csv, csvErr = csvForInstalledName(c, sub.installedCSV)
		}
	})
	wg.Go(func() { cs, _, csErr = readCatalogSource(c) })
	// The credential only: whether the Secret is valid is not reported.
	wg.Go(func() { _, quayAuth, _ = readPullSecret(c) })
	wg.Go(func() { dsc, dscErr = readFirstDSC(c) })
	wg.Wait()
	switch {
	case subErr != nil:
		return nil, fmt.Errorf("read the RHOAI Subscription: %w", subErr)
	case csvErr != nil:
		return nil, fmt.Errorf("read the RHOAI operator's CSV: %w", csvErr)
	case dscErr != nil:
		return nil, dscErr
	}

	s := &types.Summary{DSC: types.SummaryDSC{Components: []types.SummaryComponent{}}}
	s.RHOAI.Installed = sub.found
	if sub.found {
		s.RHOAI.Channel, s.RHOAI.Version, s.RHOAI.Phase = sub.info.Channel, csv.Version, csv.Phase
	}
	s.Nightly = summaryNightly(csErr, func() *types.NightlyStatus { return getNightlyStatus(c.Context(), quayAuth, sub.info, cs) })
	if dsc.Object != nil {
		s.DSC.Exists = true
		s.DSC.Ready = dscReady(dsc.Object)
		var comps types.ComponentsResponse
		addDSCComponents(&comps, dsc.Object)
		for _, comp := range comps.Components {
			s.DSC.Components = append(s.DSC.Components, types.SummaryComponent{
				Name: comp.Name, ManagementState: comp.ManagementState, Ready: comp.Status == "Available",
			})
		}
	}
	return s, nil
}

// summaryNightly is the summary's nightly part: the comparison status
// returns, or why it is not known (a fixed code, never the read's text).
// An unreadable CatalogSource is "catalog_unreadable", not "not a nightly
// install"; a lookup that failed or has no digest is "lookup_failed", so
// the page never says "the newest build" when it could not check.
func summaryNightly(csErr error, status func() *types.NightlyStatus) types.SummaryNightly {
	if csErr != nil {
		return types.SummaryNightly{Problem: types.NightlyProblemCatalogUnreadable}
	}
	n := status()
	if n == nil {
		return types.SummaryNightly{}
	}
	out := types.SummaryNightly{InstalledBuild: summaryBuild(n.Installed), LatestBuild: summaryBuild(n.Latest)}
	switch {
	case n.Installed != nil && !isNightlyStreamImage(n.Installed.Image):
		out.Problem = types.NightlyProblemNoStream
	case n.Latest == nil || n.UpdateAvailable == nil || n.Error != "":
		out.Problem = types.NightlyProblemLookupFailed
	default:
		out.NewerAvailable = *n.UpdateAvailable
	}
	return out
}

// summaryBuild is a nightly build's tag and build date, or nil.
func summaryBuild(b *types.NightlyBuild) *types.SummaryBuild {
	if b == nil {
		return nil
	}
	return &types.SummaryBuild{Tag: b.Tag, BuildDate: b.BuildDate}
}

// dscReady reports whether the DataScienceCluster's Ready condition is
// True.
func dscReady(dsc map[string]any) bool {
	status, _ := dsc["status"].(map[string]any)
	conds, _ := status["conditions"].([]any)
	for _, raw := range conds {
		cond, _ := raw.(map[string]any)
		if t, _ := cond["type"].(string); t == "Ready" {
			s, _ := cond["status"].(string)
			return s == "True"
		}
	}
	return false
}
