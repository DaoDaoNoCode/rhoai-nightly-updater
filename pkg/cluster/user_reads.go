package cluster

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/authz"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/dashdev"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/serving"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/storage"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Reads made with the signed-in user's own token (pkg/api withUserRead,
// tier user-token): the API server, and the platform APIs the token is
// sent to, decide what the user sees. Each function here refuses a client
// that does not carry the user's token (authz.ErrNotUserClient), so a read that
// is meant to be the user's never runs with the ServiceAccount's RBAC.
// pkg/cluster/rbac_coverage_test.go lists them in rbacUserTokenReads: the
// template does not grant what they read.

// GetProjectStatus is the Environment status of one of the signed-in
// user's data science projects (GET /api/resources/status?project= for a
// user who is not a cluster-admin): its pipeline servers, models and
// InferenceServices, read with the user's own token (u) and only in that
// namespace. The ServiceAccount (sa) finds the dashboard's address; u's
// bookkeeping (the ServiceAccount) reads this tool's records of what it
// set up, used only for objects of this project. A part the user may not
// read says so in its Problem field; the others are still shown.
func GetProjectStatus(sa, u *Client, project string) (*types.ProjectStatus, error) {
	if !u.ActsAsUser() {
		return nil, authz.ErrNotUserClient
	}
	ok, err := authz.IsUserDSProject(u, project)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: '%s' is not one of your data science projects", ErrNotDSProject, project)
	}
	st := &types.ProjectStatus{Project: project, PipelineServers: []types.ResourceState{}, Models: []types.ModelState{}, InferenceServices: []types.ProjectInferenceService{}}
	for _, m := range serving.ModelCatalog {
		st.ModelCatalog = append(st.ModelCatalog, m.CatalogModel)
	}
	var (
		wg                 sync.WaitGroup
		dspas              []storage.DSPA
		dspaErr, modelsErr error
		models             []serving.LLMISvcObject
		isvcErr            error
	)
	wg.Go(func() { st.DashboardURL = dashdev.DashboardURL(sa) })
	wg.Go(func() { dspas, dspaErr = storage.ListDSPAs(u, project) })
	wg.Go(func() { models, modelsErr = listProjectLLMInferenceServices(u, project) })
	wg.Go(func() { st.InferenceServices, isvcErr = listProjectInferenceServices(u, project) })
	wg.Wait()

	switch {
	case errors.Is(dspaErr, storage.ErrDSPACRDMissing):
		st.PipelineServersProblem, st.PipelineServersProblemCode = "Data Science Pipelines is not installed on this cluster.", types.ProblemNotInstalled
	case dspaErr != nil:
		st.PipelineServers = nil
		st.PipelineServersProblem, st.PipelineServersProblemCode = userReadProblem("the pipeline servers", project, dspaErr)
	default:
		rec, recErr := storage.ReadPipelineRecord(u)
		if recErr != nil {
			rec = storage.PipelineRecord{}
		}
		for _, d := range dspas {
			ps := storage.PipelineServerState(d, st.DashboardURL)
			if !d.ManagedByTool() {
				ps.ManagedByTool, ps.DataPVCs = false, nil
				ps.TeardownBlockedReason = "This pipeline server was not created by this tool, so the tool will not delete it."
			} else {
				storage.SetProjectStorageState(&ps, d, rec)
				ps.Unrecorded = !rec.OwnsAny(d.Meta)
				if ps.Unrecorded {
					ps.UID = d.Meta.UID
				}
				conn := storage.ConnectionNameOf(d)
				if ps.ProjectStorage {
					conn = storage.ProjectConnectionSecretName
				}
				ps.ConnectionState, ps.ConnectionMessage = connectionState(u, project, conn, ps.Terminating)
				if ps.ConnectionState == ConnectionPresent {
					addConnectionWorkbenches(u, project, conn, &ps)
				}
			}
			st.PipelineServers = append(st.PipelineServers, ps)
		}
	}

	switch {
	case errors.Is(modelsErr, serving.ErrLLMISvcAPIMissing):
		st.ModelsProblem, st.ModelsProblemCode = "Model serving is not installed on this cluster: it has no LLMInferenceService API.", types.ProblemNotInstalled
	case modelsErr != nil:
		st.Models = nil
		st.ModelsProblem, st.ModelsProblemCode = userReadProblem("the models", project, modelsErr)
	default:
		rec, recErr := serving.ReadModelsRecord(u)
		if recErr != nil {
			rec = nil
		}
		for _, m := range models {
			s := serving.ModelState(u, m, true, rec)
			// The address that connects: KServe may report the model's
			// Service without its port (serving.WithServicePort), read
			// with the user's token.
			s.InternalURL, _ = serving.ModelInClusterAddress(u, m)
			st.Models = append(st.Models, s)
		}
	}
	if isvcErr != nil {
		st.InferenceServices = nil
		st.InferenceServicesProblem, st.InferenceServicesProblemCode = userReadProblem("the InferenceServices", project, isvcErr)
	}
	return st, nil
}

// connectionState is what the tool's dashboard Connection Secret conn
// (storage.ConnectionNameOf its pipeline server) in project says (as
// GetResourcesStatusFor), read with the user's token: a project viewer may
// not read Secrets, which shows as unreadable.
func connectionState(u *Client, project, conn string, terminating bool) (string, string) {
	if terminating {
		return "", ""
	}
	if !u.ActsAsUser() {
		return ConnectionUnreadable, authz.ErrNotUserClient.Error()
	}
	var sec struct {
		Metadata kube.ObjectMeta `json:"metadata"`
	}
	found, err := u.GetIntoOptional(storage.ConnectionSecretPath(project, conn), &sec)
	meta := sec.Metadata
	switch {
	case err != nil:
		msg, _ := userReadProblem("the connection", project, err)
		return ConnectionUnreadable, msg
	case !found:
		return ConnectionMissing, ""
	case !meta.HasToolLabel():
		return ConnectionForeign, ""
	}
	return ConnectionPresent, ""
}

// listConnectionWorkbenches lists, with the user's token, the workbenches
// of project whose opendatahub.io/connections annotation names the tool's
// dashboard Connection conn (storage.ConnectionWorkbenches). A cluster
// without the Notebook API has none.
func listConnectionWorkbenches(u *Client, project, conn string) ([]types.ConnectionWorkbench, []string, error) {
	if !u.ActsAsUser() {
		return nil, nil, authz.ErrNotUserClient
	}
	var list storage.NotebookList
	found, err := u.GetIntoOptional(rhoai.NamespacedPath("kubeflow.org/v1", "notebooks", project, ""), &list)
	if err != nil || !found {
		return nil, nil, err
	}
	return connectionWorkbenchRefs(storage.ConnectionWorkbenches(list, project, conn)), storage.ConnectionEnvOnlyWorkbenches(list, project, conn), nil
}

// connectionWorkbenchRefs are wbs by name and UID, as the API returns them.
func connectionWorkbenchRefs(wbs []storage.ConnectionWorkbench) []types.ConnectionWorkbench {
	out := make([]types.ConnectionWorkbench, 0, len(wbs))
	for _, w := range wbs {
		out = append(out, types.ConnectionWorkbench{Name: w.Name, UID: w.UID, Running: w.Running})
	}
	return out
}

// addConnectionWorkbenches sets ps.ConnectionWorkbenches (or why they could
// not be read) for the tool's present Connection conn in project.
func addConnectionWorkbenches(u *Client, project, conn string, ps *types.ResourceState) {
	names, envOnly, err := listConnectionWorkbenches(u, project, conn)
	if err != nil {
		ps.ConnectionWorkbenchesMessage, _ = userReadProblem("the workbenches", project, err)
		return
	}
	ps.ConnectionWorkbenches, ps.ConnectionEnvOnlyWorkbenches = names, envOnly
}

// userReadProblem explains a part of a user's read that failed, with its
// fixed code (types.Problem*).
func userReadProblem(what, project string, err error) (string, string) {
	if _, code := kube.HTTPStatusForError(err); code == kube.ErrorCodeForbiddenForUser {
		return fmt.Sprintf("Your OpenShift account may not read %s of project %s.", what, project), types.ProblemForbiddenForUser
	}
	return fmt.Sprintf("Cannot read %s of project %s: %v", what, project, err), types.ProblemError
}

// listProjectLLMInferenceServices lists the LLMInferenceServices of one
// project with the user's token.
func listProjectLLMInferenceServices(u *Client, project string) ([]serving.LLMISvcObject, error) {
	if !u.ActsAsUser() {
		return nil, authz.ErrNotUserClient
	}
	var list struct {
		Items []serving.LLMISvcObject `json:"items"`
	}
	found, err := u.GetIntoOptional(rhoai.NamespacedPath("serving.kserve.io/v1alpha2", "llminferenceservices", project, ""), &list)
	if err != nil {
		if kube.IsDecodeError(err) {
			return nil, fmt.Errorf("parse LLMInferenceServices: %w", err)
		}
		return nil, fmt.Errorf("list LLMInferenceServices: %w", err)
	}
	if !found {
		return nil, serving.ErrLLMISvcAPIMissing
	}
	slices.SortFunc(list.Items, func(a, b serving.LLMISvcObject) int { return strings.Compare(a.Metadata.Name, b.Metadata.Name) })
	return list.Items, nil
}

// listProjectInferenceServices lists the KServe InferenceServices of one
// project with the user's token. A cluster without the API has none.
func listProjectInferenceServices(u *Client, project string) ([]types.ProjectInferenceService, error) {
	if !u.ActsAsUser() {
		return nil, authz.ErrNotUserClient
	}
	out := []types.ProjectInferenceService{}
	var list struct {
		Items []struct {
			Metadata kube.ObjectMeta `json:"metadata"`
			Status   struct {
				URL        string           `json:"url"`
				Conditions []kube.Condition `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	found, err := u.GetIntoOptional(rhoai.NamespacedPath("serving.kserve.io/v1beta1", "inferenceservices", project, ""), &list)
	if err != nil {
		if kube.IsDecodeError(err) {
			return nil, fmt.Errorf("parse InferenceServices: %w", err)
		}
		return nil, fmt.Errorf("list InferenceServices: %w", err)
	}
	if !found {
		return out, nil
	}
	for _, it := range list.Items {
		s := types.ProjectInferenceService{Name: it.Metadata.Name, Terminating: it.Metadata.Terminating()}
		if kube.SnippetURL(it.Status.URL) {
			s.URL = it.Status.URL
		}
		if cond, ok := kube.FindCondition(it.Status.Conditions, "Ready"); ok && cond.Status == "True" {
			s.Ready = true
		} else if ok {
			s.Message = kube.FirstNonEmpty(kube.TruncateText(cond.Message, 300), cond.Reason, "Not ready")
		} else {
			s.Message = "Reports no Ready condition"
		}
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b types.ProjectInferenceService) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// knownManagementStates are the DataScienceCluster managementState values
// (its CRD's enum); anything else is not shown to a user.
var knownManagementStates = map[string]bool{"Managed": true, "Removed": true, "Unmanaged": true}

// userComponentBlock is b as a user who is not a cluster-admin sees it:
// only fixed text. b's Reason names the DataScienceCluster (an SA read);
// the user gets the component key (this tool's own constant), its state
// when it is one of the CRD's values, and a fixed reason.
func userComponentBlock(b *types.ComponentBlock) *types.ComponentBlock {
	if b == nil {
		return nil
	}
	out := &types.ComponentBlock{Component: b.Component,
		Reason: "This feature is not turned on on this cluster. A cluster-admin turns it on."}
	if knownManagementStates[b.State] {
		out.State = b.State
	}
	return out
}

// PlaygroundStatusForUser is st as a user who is not a cluster-admin sees
// it: what the ServiceAccount read is reduced to fixed values (the
// operator's readiness, the gen-ai backend's state, whether the feature is
// blocked), never text from the objects it read (the DataScienceCluster's
// condition message, its name). What the user's own token read (their
// projects' playgrounds and models, the gen-ai API's answers to them) is
// kept. st itself (cached) is not changed.
func PlaygroundStatusForUser(st *types.PlaygroundStatus) *types.PlaygroundStatus {
	if st == nil {
		return nil
	}
	out := *st
	out.OperatorMessage = ""
	out.Blocked = userComponentBlock(st.Blocked)
	switch out.OperatorReady {
	case "True", "False", "":
	default:
		out.OperatorReady = ""
	}
	switch out.GenAIUI {
	case "available", "unavailable", "missing", "":
	default:
		out.GenAIUI = ""
	}
	return &out
}
