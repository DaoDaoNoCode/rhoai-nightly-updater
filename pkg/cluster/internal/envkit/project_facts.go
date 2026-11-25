package envkit

import (
	"errors"
	"log/slog"
	"sync"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/components"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// ReadProjectFacts reads the cluster facts of a project action
// (types.ProjectFacts) with the facts client (c.Facts(), the
// ServiceAccount's, through the rules deploy/template.yaml already grants
// it: DSCInitializations list, DataScienceClusters list, Infrastructure
// get and ValidatingWebhookConfigurations list). It refuses a client that
// acts with the signed-in user's token and has no facts client
// (kube.ErrNoFactsClient): the facts are the ServiceAccount's answer or
// none. A fact that cannot be read is left empty and named in Problems;
// the read errors are logged, not returned.
func ReadProjectFacts(c *kube.Client) (types.ProjectFacts, error) {
	if c.Facts().ActsAsUser() {
		return types.ProjectFacts{}, kube.ErrNoFactsClient
	}
	var (
		wg     sync.WaitGroup
		ns     rhoai.Namespaces
		nsErr  error
		dsc    rhoai.DSCRead
		dscErr error
		cp     string
	)
	wg.Go(func() { ns, nsErr = rhoai.DiscoverNamespaces(c) })
	wg.Go(func() { dsc, dscErr = rhoai.ReadFirstDSC(c.Facts()) })
	wg.Go(func() { cp = rhoai.CachedControlPlane(c) })
	wg.Wait()

	out := types.ProjectFacts{Components: []types.SummaryComponent{}, Problems: []string{}}
	switch {
	case nsErr == nil:
		out.ApplicationsNamespace = ns.Applications
	case !errors.Is(nsErr, rhoai.ErrNoDSCInitialization):
		slog.Warn("cannot read RHOAI's applications namespace for a project action", "error", nsErr)
		out.Problems = append(out.Problems, types.ProjectFactsProblemApplicationsNamespace)
	}
	switch {
	case dscErr != nil:
		slog.Warn("cannot read the DataScienceCluster for a project action", "error", dscErr)
		out.Problems = append(out.Problems, types.ProjectFactsProblemDSC)
	case dsc.State == rhoai.DSCStateNoCRD:
		out.DSC = types.ProjectFactsDSCNotInstalled
	case dsc.State == rhoai.DSCStateNoDSC:
		out.DSC = types.ProjectFactsDSCNone
	default:
		out.DSC = types.ProjectFactsDSCPresent
		var comps types.ComponentsResponse
		components.AddDSCComponents(&comps, dsc.Object)
		for _, comp := range comps.Components {
			out.Components = append(out.Components, types.SummaryComponent{
				Name: comp.Name, ManagementState: comp.ManagementState, Ready: comp.Status == "Available",
			})
		}
	}
	switch cp {
	case rhoai.ControlPlaneHosted:
		out.ControlPlane = types.ProjectFactsControlPlaneHosted
	case rhoai.ControlPlaneManaged, rhoai.ControlPlaneSelfManaged:
		out.ControlPlane = types.ProjectFactsControlPlaneNotHosted
	default:
		out.Problems = append(out.Problems, types.ProjectFactsProblemControlPlane)
	}
	return out, nil
}
