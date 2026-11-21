package cluster

import (
	"errors"
	"sync"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/components"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// GetComponents returns DSC component statuses and deployment information.
// A cluster without a DSC (or without the DSC CRD) is a normal state, reported
// through DSCExists/DSCState rather than an error. Labels already in the cache
// are always added; includeLabels also fetches missing ones from Quay (slower).
func GetComponents(c *Client, includeLabels bool) (*types.ComponentsResponse, error) {
	return getComponents(c, componentRegistryReads{labels: components.FetchImageLabels, dscSample: components.FetchDSCSample}, includeLabels)
}

// componentRegistryReads are the reads of GetComponents that the gitops
// profile leaves out: the images' OCI labels (from Quay, and commit dates
// from GitHub) and the GitHub DSC sample. nil fields are skipped.
type componentRegistryReads struct {
	labels    func(c *Client, deployments []types.DeploymentInfo, fetch bool)
	dscSample components.DSCSampleFetcher
}

// GetComponentsGitOps is GetComponents in the gitops profile, which
// contacts no host but the API server (docs/GITOPS.md, section 11): no
// image labels (ImageLabelsOff), and the DSC defaults from the installed
// CSV's alm-examples only. The operator build comes from the installed CSV
// as in both profiles (OperatorImage, OperatorBuilt).
func GetComponentsGitOps(c *Client) (*types.ComponentsResponse, error) {
	return getComponents(c, componentRegistryReads{}, false)
}

func getComponents(c *Client, reads componentRegistryReads, includeLabels bool) (*types.ComponentsResponse, error) {
	resp := &types.ComponentsResponse{Components: []types.ComponentInfo{}, ImageLabelsOff: reads.labels == nil}

	// Every read below is independent of the DSC, so they run while it is read.
	var wg sync.WaitGroup
	run := func(fn func()) {
		wg.Go(func() {
			fn()
		})
	}
	var (
		appDeps, opDeps    []types.DeploymentInfo
		appErr, opDepsErr  error
		appPods, opPods    []types.PodInfo
		snapshot           map[string]string
		snapshotTime       string
		installedOp        *components.InstalledOperator
		installedOpErr     error
		installedOpReadyCh = make(chan struct{})
	)
	// The applications namespace is the DSCInitialization's; unknown, its
	// Deployments are left out and the response says why.
	resp.OperatorNamespace = rhoai.SubNS
	var appNS string
	var appNSErr error
	run(func() {
		if appNS, appNSErr = rhoai.AppsNamespace(c); appNSErr != nil {
			appErr = appNSErr
			return
		}
		var inner sync.WaitGroup
		inner.Go(func() { appDeps, appErr = components.Deployments(c, appNS) })
		inner.Go(func() { appPods, _ = components.PodsInNamespace(c, appNS) })
		inner.Wait()
	})
	run(func() { opDeps, opDepsErr = components.Deployments(c, rhoai.SubNS) })
	run(func() { opPods, _ = components.PodsInNamespace(c, rhoai.SubNS) })
	run(func() { resp.ConsoleURL = cachedConsoleURL(c) })
	run(func() { snapshot, snapshotTime = components.ReadDeploymentSnapshot(c) })
	run(func() {
		defer close(installedOpReadyCh)
		installedOp, installedOpErr = components.ReadInstalledOperator(c)
	})

	read, err := rhoai.ReadFirstDSC(c)
	if err != nil {
		wg.Wait()
		return nil, err
	}
	resp.DSCState = read.State
	resp.DSCExists = read.Object != nil
	if read.Object != nil {
		resp.DSCAPIVersion, _ = read.Object["apiVersion"].(string)
		resp.DSCVersionFallback = read.VersionFallback()
		<-installedOpReadyCh
		resp.DSCCompatibility = checkDSCCompatibilityFor(c, read, installedOp, installedOpErr, reads.dscSample)
		components.AddDSCComponents(resp, read.Object)
	}
	wg.Wait()

	if installedOp != nil {
		resp.OperatorVersion = installedOp.Version
		resp.OperatorPhase = installedOp.Phase
		resp.OperatorImage = installedOp.ContainerImage
		resp.OperatorBuilt = installedOp.CreatedAt
	}
	if appErr == nil {
		resp.Deployments = append(resp.Deployments, appDeps...)
	}
	if opDepsErr == nil {
		resp.Deployments = append(resp.Deployments, opDeps...)
	}
	if resp.Deployments == nil {
		resp.Deployments = []types.DeploymentInfo{}
	}
	switch {
	case appNSErr == nil:
		resp.ApplicationsNamespace = appNS
		components.MatchPodsToDeployments(c, appPods, appNS, resp.Deployments)
	case !errors.Is(appNSErr, rhoai.ErrNoDSCInitialization): // none yet is not an error
		resp.ApplicationsNamespaceError = appNSErr.Error()
	}
	components.MatchPodsToDeployments(c, opPods, rhoai.SubNS, resp.Deployments)

	// Compare against last snapshot to detect changes
	if snapshot != nil {
		resp.SnapshotTime = snapshotTime
		resp.ChangedCount = components.CompareDeployments(resp.Deployments, snapshot)
	}

	if reads.labels != nil {
		reads.labels(c, resp.Deployments, includeLabels)
	}
	return resp, nil
}
