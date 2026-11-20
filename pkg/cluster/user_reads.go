package cluster

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Reads made with the signed-in user's own token (pkg/api withUserRead,
// tier user-token): the API server, and the platform APIs the token is
// sent to, decide what the user sees. Each function here refuses a client
// that does not carry the user's token (errNotUserClient), so a read that
// is meant to be the user's never runs with the ServiceAccount's RBAC.
// pkg/cluster/rbac_coverage_test.go lists them in rbacUserTokenReads: the
// template does not grant what they read.

// errNotUserClient: a read that must be made with the user's own token got
// another client.
var errNotUserClient = errors.New("this read must be made with the signed-in user's own token")

// dsProjectSelector selects RHOAI's data science projects (the label the
// dashboard sets), for namespaces and projects alike.
const dsProjectSelector = "opendatahub.io/dashboard=true"

// ListUserDSProjects lists the data science projects the signed-in user
// may see, with their own token: the OpenShift projects API
// (project.openshift.io/v1 projects) with the label selector of
// GetDSProjects. The API lists the namespaces whose "get namespaces" the
// user's RBAC allows, applies the label selector, and keeps those the
// token's scopes allow (openshift/openshift-apiserver release-4.17
// 6ea968b: pkg/project/apiserver/registry/project/proxy/proxy.go:91-102,
// pkg/project/auth/cache.go:478-518, the review of pkg/project/auth/
// reviewer.go:49-56). Listing projects is granted to every authenticated
// user by OpenShift's basic-user ClusterRole (openshift-apiserver
// pkg/bootstrappolicy/policy.go:461, bound to system:authenticated at
// :766). A cluster-admin sees every project.
func ListUserDSProjects(u *Client) ([]string, error) {
	if !u.asUser {
		return nil, errNotUserClient
	}
	body, _, err := u.get("/apis/project.openshift.io/v1/projects?labelSelector=" + url.QueryEscape(dsProjectSelector))
	if err != nil {
		return nil, fmt.Errorf("list your data science projects: %w", err)
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Status struct {
				Phase string `json:"phase"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("parse the project list: %w", err)
	}
	projects := []string{}
	for _, p := range list.Items {
		if p.Status.Phase != "Terminating" {
			projects = append(projects, p.Metadata.Name)
		}
	}
	slices.Sort(projects)
	return projects, nil
}

// GetProjectStatus is the Environment status of one of the signed-in
// user's data science projects (GET /api/resources/status?project= for a
// user who is not a cluster-admin): its pipeline servers, models and
// InferenceServices, read with the user's own token (u) and only in that
// namespace. The ServiceAccount (sa) finds the dashboard's address; u's
// bookkeeping (the ServiceAccount) reads this tool's records of what it
// set up, used only for objects of this project. A part the user may not
// read says so in its Problem field; the others are still shown.
func GetProjectStatus(sa, u *Client, project string) (*types.ProjectStatus, error) {
	if !u.asUser {
		return nil, errNotUserClient
	}
	ok, err := isUserDSProject(u, project)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: '%s' is not one of your data science projects", ErrNotDSProject, project)
	}
	st := &types.ProjectStatus{Project: project, PipelineServers: []types.ResourceState{}, Models: []types.ModelState{}, InferenceServices: []types.ProjectInferenceService{}}
	var (
		wg                 sync.WaitGroup
		dspas              []dspaInfo
		dspaErr, modelsErr error
		models             []llmisvcObject
		isvcErr            error
	)
	wg.Go(func() { st.DashboardURL = dashboardBaseURL(sa) })
	wg.Go(func() { dspas, dspaErr = listDSPAs(u, project) })
	wg.Go(func() { models, modelsErr = listProjectLLMInferenceServices(u, project) })
	wg.Go(func() { st.InferenceServices, isvcErr = listProjectInferenceServices(u, project) })
	wg.Wait()

	switch {
	case errors.Is(dspaErr, errDSPACRDMissing):
		st.PipelineServersProblem = "Data Science Pipelines is not installed on this cluster."
	case dspaErr != nil:
		st.PipelineServers, st.PipelineServersProblem = nil, userReadProblem("the pipeline servers", project, dspaErr)
	default:
		rec, recErr := readPipelineRecord(u)
		if recErr != nil {
			rec = pipelineRecord{}
		}
		for _, d := range dspas {
			ps := pipelineServerState(d, st.DashboardURL)
			if !d.managedByTool() {
				ps.ManagedByTool, ps.DataPVCs = false, nil
				ps.TeardownBlockedReason = "This pipeline server was not created by this tool, so the tool will not delete it."
			} else {
				ps.Unrecorded = !rec.owns(d.Meta)
				ps.ConnectionState, ps.ConnectionMessage = connectionState(u, project, ps.Terminating)
			}
			st.PipelineServers = append(st.PipelineServers, ps)
		}
	}

	switch {
	case errors.Is(modelsErr, errLLMISvcAPIMissing):
		st.ModelsProblem = "Model serving is not installed on this cluster: it has no LLMInferenceService API."
	case modelsErr != nil:
		st.Models, st.ModelsProblem = nil, userReadProblem("the models", project, modelsErr)
	default:
		rec, recErr := readModelsRecord(u)
		if recErr != nil {
			rec = nil
		}
		for _, m := range models {
			st.Models = append(st.Models, modelState(u, m, true, rec))
		}
	}
	if isvcErr != nil {
		st.InferenceServices, st.InferenceServicesProblem = nil, userReadProblem("the InferenceServices", project, isvcErr)
	}
	return st, nil
}

// connectionState is what the tool's dashboard Connection Secret in
// project says (as GetResourcesStatusFor), read with the user's token: a
// project viewer may not read Secrets, which shows as unreadable.
func connectionState(u *Client, project string, terminating bool) (string, string) {
	if terminating {
		return "", ""
	}
	meta, found, err := readObjectMeta(u, namespacedPath("v1", "secrets", project, connectionSecretName))
	switch {
	case err != nil:
		return ConnectionUnreadable, userReadProblem("the connection", project, err)
	case !found:
		return ConnectionMissing, ""
	case !meta.hasToolLabel():
		return ConnectionForeign, ""
	}
	return ConnectionPresent, ""
}

// userReadProblem explains a part of a user's read that failed.
func userReadProblem(what, project string, err error) string {
	if _, code := HTTPStatusForError(err); code == ErrorCodeForbiddenForUser {
		return fmt.Sprintf("Your OpenShift account may not read %s of project %s.", what, project)
	}
	return fmt.Sprintf("Cannot read %s of project %s: %v", what, project, err)
}

// listProjectLLMInferenceServices lists the LLMInferenceServices of one
// project with the user's token.
func listProjectLLMInferenceServices(u *Client, project string) ([]llmisvcObject, error) {
	if !u.asUser {
		return nil, errNotUserClient
	}
	body, _, err := u.get(namespacedPath("serving.kserve.io/v1alpha2", "llminferenceservices", project, ""))
	if IsK8sError(err, 404) {
		return nil, errLLMISvcAPIMissing
	}
	if err != nil {
		return nil, fmt.Errorf("list LLMInferenceServices: %w", err)
	}
	var list struct {
		Items []llmisvcObject `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("parse LLMInferenceServices: %w", err)
	}
	slices.SortFunc(list.Items, func(a, b llmisvcObject) int { return strings.Compare(a.Metadata.Name, b.Metadata.Name) })
	return list.Items, nil
}

// listProjectInferenceServices lists the KServe InferenceServices of one
// project with the user's token. A cluster without the API has none.
func listProjectInferenceServices(u *Client, project string) ([]types.ProjectInferenceService, error) {
	if !u.asUser {
		return nil, errNotUserClient
	}
	out := []types.ProjectInferenceService{}
	body, _, err := u.get(namespacedPath("serving.kserve.io/v1beta1", "inferenceservices", project, ""))
	if IsK8sError(err, 404) {
		return out, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list InferenceServices: %w", err)
	}
	var list struct {
		Items []struct {
			Metadata objectMeta `json:"metadata"`
			Status   struct {
				URL        string         `json:"url"`
				Conditions []k8sCondition `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("parse InferenceServices: %w", err)
	}
	for _, it := range list.Items {
		s := types.ProjectInferenceService{Name: it.Metadata.Name, Terminating: it.Metadata.terminating()}
		if snippetURL(it.Status.URL) {
			s.URL = it.Status.URL
		}
		if cond, ok := findCondition(it.Status.Conditions, "Ready"); ok && cond.Status == "True" {
			s.Ready = true
		} else if ok {
			s.Message = firstNonEmpty(truncateText(cond.Message, 300), cond.Reason, "Not ready")
		} else {
			s.Message = "Reports no Ready condition"
		}
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b types.ProjectInferenceService) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// isUserDSProject reports whether project is one of the signed-in user's
// data science projects (ListUserDSProjects).
func isUserDSProject(u *Client, project string) (bool, error) {
	projects, err := ListUserDSProjects(u)
	if err != nil {
		return false, err
	}
	return slices.Contains(projects, project), nil
}
