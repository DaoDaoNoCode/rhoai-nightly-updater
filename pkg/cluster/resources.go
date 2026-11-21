package cluster

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sort"
	"strings"
	"sync"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/dashdev"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// workloadPodIssue is the most relevant reason a workload's pods are not ready.
type workloadPodIssue struct {
	Reason   string
	Message  string
	Terminal bool
}

// terminalWaitingReasons are kubelet container waiting reasons that do not
// clear without a change (image, config or crash fix). ErrImagePull is not
// included: kubelet retries it and moves to ImagePullBackOff when it persists.
var terminalWaitingReasons = map[string]bool{
	"ImagePullBackOff":           true,
	"InvalidImageName":           true,
	"ErrImageNeverPull":          true,
	"CrashLoopBackOff":           true,
	"CreateContainerConfigError": true,
	"CreateContainerError":       true,
	"RunContainerError":          true,
}

// inspectPods returns why the pods matching selector are not ready, preferring
// terminal reasons. An empty Reason means nothing specific was found. When
// wantImage is set, only pods with a container running that image count.
func inspectPods(c *Client, namespace, selector, wantImage string) (workloadPodIssue, error) {
	var items []struct {
		Metadata kube.ObjectMeta `json:"metadata"`
		Spec     struct {
			Containers []struct {
				Image string `json:"image"`
			} `json:"containers"`
		} `json:"spec"`
		Status struct {
			Conditions []struct {
				Type    string `json:"type"`
				Status  string `json:"status"`
				Reason  string `json:"reason"`
				Message string `json:"message"`
			} `json:"conditions"`
			InitContainerStatuses []containerStatus `json:"initContainerStatuses"`
			ContainerStatuses     []containerStatus `json:"containerStatuses"`
		} `json:"status"`
	}
	if err := c.ListInto(rhoai.NamespacedPath("v1", "pods", namespace, "")+"?labelSelector="+url.QueryEscape(selector), &items); err != nil {
		if kube.IsDecodeError(err) {
			return workloadPodIssue{}, fmt.Errorf("parse pod list: %w", err)
		}
		return workloadPodIssue{}, err
	}
	var best workloadPodIssue
	consider := func(p workloadPodIssue) {
		if p.Reason == "" {
			return
		}
		if best.Reason == "" || (p.Terminal && !best.Terminal) {
			best = p
		}
	}
	for _, pod := range items {
		if pod.Metadata.Terminating() {
			continue
		}
		if wantImage != "" {
			match := false
			for _, ctr := range pod.Spec.Containers {
				match = match || ctr.Image == wantImage
			}
			if !match {
				continue
			}
		}
		for _, cond := range pod.Status.Conditions {
			if cond.Type == "PodScheduled" && cond.Status == "False" {
				consider(workloadPodIssue{Reason: kube.FirstNonEmpty(cond.Reason, "Unschedulable"), Message: cond.Message})
			}
		}
		statuses := append(append([]containerStatus{}, pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...)
		for _, cs := range statuses {
			if w := cs.State.Waiting; w != nil && w.Reason != "" {
				msg := w.Message
				if t := cs.LastState.Terminated; t != nil && w.Reason == "CrashLoopBackOff" {
					msg = strings.TrimSpace(fmt.Sprintf("%s (last exit: %s, code %d)", msg, t.Reason, t.ExitCode))
				}
				consider(workloadPodIssue{Reason: w.Reason, Message: msg, Terminal: terminalWaitingReasons[w.Reason]})
			}
		}
	}
	return best, nil
}

type containerStatus struct {
	State struct {
		Waiting *struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"waiting"`
	} `json:"state"`
	LastState struct {
		Terminated *struct {
			Reason   string `json:"reason"`
			ExitCode int    `json:"exitCode"`
		} `json:"terminated"`
	} `json:"lastState"`
}

// applyPodIssue copies a pod issue into a resource state.
func applyPodIssue(state *types.ResourceState, issue workloadPodIssue) {
	if issue.Reason == "" {
		return
	}
	state.WaitingReason = issue.Reason
	state.TerminalError = issue.Terminal
	state.Message = issue.Reason
	if issue.Message != "" {
		state.Message = issue.Reason + ": " + kube.TruncateText(issue.Message, 300)
	}
}

// GetResourcesStatus checks the state of test infrastructure resources. It
// lists DS projects, pipeline servers (one cluster-wide LIST), S3 storage, MLflow
// and the dashboard route concurrently, so the cost no longer grows with the
// number of projects. Pipeline servers are reported only for projects the
// user can see.
func GetResourcesStatus(c *Client) (*types.ResourcesStatus, error) {
	var (
		wg          sync.WaitGroup
		projects    []string
		projectsErr error
		dspas       []dspaInfo
		dspasErr    error
		dashURL     string
		s3EP        s3Endpoints
		s3EPErr     error
		legacy      *legacyStorage
		legacyErr   error
		foreignNS   bool
		dsc         rhoai.DSCRead
		dscErr      error
		models      []llmisvcObject
		modelsErr   error
		status      = &types.ResourcesStatus{}
	)
	wg.Add(8)
	go func() { defer wg.Done(); models, modelsErr = listLLMInferenceServices(c) }()
	go func() { defer wg.Done(); projects, projectsErr = envkit.DSProjects(c) }()
	go func() { defer wg.Done(); dspas, dspasErr = listDSPAs(c, "") }()
	go func() { defer wg.Done(); status.MinIO, s3EP, s3EPErr = s3StatusAndEndpoints(c) }()
	go func() { defer wg.Done(); legacy, foreignNS, legacyErr = readLegacyStorageOwner(c) }()
	go func() { defer wg.Done(); status.MLflow = getMLflowStatus(c) }()
	go func() { defer wg.Done(); dashURL = dashdev.DashboardURL(c) }()
	go func() { defer wg.Done(); dsc, dscErr = rhoai.ReadFirstDSC(c) }()
	wg.Wait()
	if projectsErr != nil {
		return nil, projectsErr
	}
	if dspasErr != nil && !errors.Is(dspasErr, errDSPACRDMissing) {
		return nil, dspasErr
	}

	status.DashboardURL = dashURL
	status.ForeignLegacyNamespace = foreignNS
	// What the features need from the DataScienceCluster. One that cannot
	// be read blocks nothing; setup still checks the CRD.
	if dscErr != nil {
		slog.Debug("cannot read the DataScienceCluster for the Environment page", "error", dscErr)
	} else {
		status.PipelinesBlocked = envkit.ComponentBlock(dsc, envkit.PipelinesComponent)
		status.MLflowBlocked = envkit.ComponentBlock(dsc, envkit.MLflowComponent)
	}
	if status.PipelinesBlocked == nil && errors.Is(dspasErr, errDSPACRDMissing) {
		status.PipelinesBlocked = &types.ComponentBlock{Component: envkit.PipelinesComponent.Keys[0],
			Reason: "Data Science Pipelines is not installed yet: this cluster has no DataSciencePipelinesApplication API. If the component was just turned on, wait for the operator; otherwise check the Components page."}
	}

	visible := map[string]bool{}
	for _, p := range projects {
		visible[p] = true
	}
	// Models: the inference gateway (which models use it) and the
	// catalog's models in the visible projects.
	var mwg sync.WaitGroup
	mwg.Add(2)
	go func() { defer mwg.Done(); status.InferenceGateway = getInferenceGatewayStatus(c, models, modelsErr) }()
	go func() { defer mwg.Done(); status.Models = getModelsStatus(c, dsc, dscErr, models, modelsErr, visible) }()
	mwg.Wait()

	// A record that cannot be read leaves every pipeline server unrecorded
	// (fail closed: Migrate storage then asks before moving one).
	pipelineRec, recErr := readPipelineRecord(c)
	if recErr != nil {
		slog.Debug("cannot read the pipeline server record", "error", recErr)
		pipelineRec = pipelineRecord{}
	}
	unmanaged := map[string]bool{}
	for _, d := range dspas {
		if !visible[d.Meta.Namespace] {
			continue
		}
		if !d.managedByTool() {
			unmanaged[d.Meta.Namespace] = true
			foreign := pipelineServerState(d, dashURL)
			foreign.ManagedByTool = false
			foreign.DataPVCs = nil
			foreign.TeardownBlockedReason = "This pipeline server was not created by this tool, so the tool will not delete it."
			status.UnmanagedPipelineServers = append(status.UnmanagedPipelineServers, foreign)
			continue
		}
		ps := pipelineServerState(d, dashURL)
		ps.Unrecorded = !pipelineRec.owns(d.Meta)
		status.PipelineServers = append(status.PipelineServers, ps)
	}
	for ns := range unmanaged {
		status.UnmanagedPipelineProjects = append(status.UnmanagedPipelineProjects, ns)
	}
	sort.Strings(status.UnmanagedPipelineProjects)
	sort.Slice(status.UnmanagedPipelineServers, func(i, j int) bool {
		a, b := status.UnmanagedPipelineServers[i], status.UnmanagedPipelineServers[j]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Name < b.Name
	})
	sort.Slice(status.PipelineServers, func(i, j int) bool {
		return status.PipelineServers[i].Namespace < status.PipelineServers[j].Namespace
	})

	// Storage an earlier version left in namespace minio: Migrate storage
	// replaces Set up and Repair while it is there (SetupS3Storage refuses),
	// and teardown removes it with the current storage.
	switch {
	case legacyErr != nil:
		status.MinIO.Warning = strings.TrimSpace(fmt.Sprintf("Cannot check namespace %s for storage from an earlier version: %v. %s", legacyNamespace, legacyErr, status.MinIO.Warning))
	case legacy != nil:
		newReady := status.MinIO.Ready && status.MinIO.ManagedByTool && status.MinIO.RepairNeeded == ""
		// Unreadable counts as not copied: the stage stays "migrate".
		copied, _ := migrationCopied(c)
		status.LegacyStorage = legacyStorageState(legacy, dspas, pipelineRec, newReady, copied)
		status.MinIO.MigrationPending = true
	}

	// Also for a storage that is gone but left its data PVC: teardown
	// deletes it under the same guard.
	current := status.MinIO.ManagedByTool && (status.MinIO.Deployed || len(status.MinIO.DataPVCs) > 0)
	if current || legacy != nil {
		var addresses []storageAddress
		var addrErr error
		if current {
			addresses, addrErr = append(addresses, s3EP.address), s3EPErr
		}
		if legacy != nil {
			addresses = append(addresses, legacy.address)
			if addrErr == nil {
				addrErr = legacy.addressErr
			}
		}
		if addrErr != nil && len(dspas) > 0 {
			status.MinIO.TeardownBlockedReason = "Cannot verify which pipeline servers use the S3 storage: " + addrErr.Error()
		} else if reason := s3TeardownBlocker(dspas, addresses...); reason != "" {
			status.MinIO.TeardownBlockedReason = reason
		}
		if addrErr == nil {
			live, deleting := storageUsers(dspas, addresses...)
			status.MinIO.UsedBy = append(live, deleting...)
		}
	}
	return status, nil
}
