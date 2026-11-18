package cluster

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Taking projects off this tool's inference gateway's list.
//
// Every project on the list lets its editors attach HTTPRoutes to the
// shared gateway, and one with an Exact path to another project's model
// takes that model's calls and their tokens (inference_gateway.go, "Why a
// list"). A name also stays admitted after its project is deleted: a new
// project of that name is admitted at once. Live (v3.0.0 E2E, 2026-11-18):
// after a model in e2e-v300 was set up, published, unpublished and
// deleted, the gateway still admitted e2e-v300.
//
// So this tool records each project it adds to the list (snapshot
// ConfigMap key "inferenceGatewayProjects", under a field manager of its
// own), before it writes the Gateway. When the last model routed through
// the gateway from a recorded project is deleted (the Models tab, Clean
// up), the project is taken off the list: the same guarded write as an add
// (a server-side dry run first, the read resourceVersion, planned again if
// someone else edits the list meanwhile), only when every model of the
// cluster can be listed and none in that project routes through the
// gateway (fail closed: on any doubt the name stays and the result says
// why). A name this tool did not add is never taken off by itself.
//
// The status names the projects on the list that no model routes through,
// and those that no longer exist. "Remove from the list" takes one off
// after the user confirms it, whoever added it (names added before this
// record existed, as e2e-v300, are not recorded), with the same checks:
// never the gateway's own namespace, never while a model in that project
// routes through the gateway. MaaS's gateway needs none of this: its list
// is computed from the published models (maasGatewayNamespaces), and
// Unpublish of a project's last model takes the project off it.
const (
	gatewayProjectsKey     = "inferenceGatewayProjects"
	gatewayProjectsManager = "rhoai-nightly-updater-gateway-projects"
)

func readGatewayProjects(c *Client) ([]string, error) {
	raw, err := readSnapshotKey(c, gatewayProjectsKey)
	if err != nil {
		return nil, fmt.Errorf("read this tool's record of the projects it added to the inference gateway: %w", err)
	}
	var rec struct {
		Projects []string `json:"projects"`
	}
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &rec); err != nil {
			return nil, fmt.Errorf("parse this tool's record of the projects it added to the inference gateway: %w", err)
		}
	}
	return rec.Projects, nil
}

func writeGatewayProjects(c *Client, projects []string) error {
	sort.Strings(projects)
	data, _ := json.Marshal(map[string]interface{}{"projects": projects})
	if err := writeSnapshotKey(c, gatewayProjectsKey, gatewayProjectsManager, string(data)); err != nil {
		return fmt.Errorf("record the projects this tool added to the inference gateway: %w", err)
	}
	return nil
}

// recordGatewayProject records that this tool adds project to the list,
// before the Gateway is written: a project that cannot be recorded is not
// added (it could never be taken off by this tool).
func recordGatewayProject(c *Client, project string) error {
	projects, err := readGatewayProjects(c)
	if err != nil {
		return err
	}
	if containsString(projects, project) {
		return nil
	}
	return writeGatewayProjects(c, append(projects, project))
}

func forgetGatewayProject(c *Client, project string) error {
	projects, err := readGatewayProjects(c)
	if err != nil {
		return err
	}
	var kept []string
	for _, p := range projects {
		if p != project {
			kept = append(kept, p)
		}
	}
	if len(kept) == len(projects) {
		return nil
	}
	return writeGatewayProjects(c, kept)
}

// gatewayRoutedIn lists the models ("namespace/name", anyone's) in project
// that route through the inference gateway. An error is returned when the
// models cannot be listed: the caller keeps the project (fail closed).
func gatewayRoutedIn(c *Client, gw gatewayObject, project string) ([]string, error) {
	models, err := listLLMInferenceServices(c)
	if errors.Is(err, errLLMISvcAPIMissing) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	def, err := readKServeGateway(c)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, m := range routedModels(models, def, gw) {
		if m.Metadata.Namespace == project && !m.Metadata.terminating() {
			out = append(out, m.Metadata.Namespace+"/"+m.Metadata.Name)
		}
	}
	return out, nil
}

// removeGatewayProject takes project off this tool's gateway's list, with
// every check first (step logf). It returns a refusal and a code, or "".
func removeGatewayProject(c *Client, project string, logf func(string)) (string, string) {
	if project == inferenceGatewayNamespace {
		return fmt.Sprintf("%s is the gateway's own namespace and always stays on its list.", project), "validation"
	}
	gw, found, err := readInferenceGateway(c)
	switch {
	case err != nil:
		return fmt.Sprintf("Cannot read the inference gateway, so '%s' stays on its list: %v.%s", project, err, templateHint(err)), errorCodeFromK8sErr(err)
	case !found:
		return "", ""
	case !gatewayOwned(gw.Metadata):
		return fmt.Sprintf("Gateway %s/%s was not created by this tool, so this tool does not change its list.", inferenceGatewayNamespace, inferenceGatewayName), "not_managed"
	}
	if listed, _ := gw.listedNamespaces(); !containsString(listed, project) {
		return "", ""
	}
	users, err := gatewayRoutedIn(c, gw, project)
	if err != nil {
		return fmt.Sprintf("Cannot check which models in '%s' use the inference gateway, so it stays on its list: %v.%s", project, err, templateHint(err)), errorCodeFromK8sErr(err)
	}
	if len(users) > 0 {
		return fmt.Sprintf("'%s' stays on the inference gateway's list: %s %s through it.", project, strings.Join(users, ", "), verb(len(users), "routes", "route")), "in_use"
	}
	plan, msg, code := planInferenceGatewayList(c, nil, []string{project})
	if msg != "" {
		return msg, code
	}
	if plan.foreign {
		return fmt.Sprintf("Gateway %s/%s was not created by this tool, so this tool does not change its list.", inferenceGatewayNamespace, inferenceGatewayName), "not_managed"
	}
	if !plan.listChanges {
		return "", ""
	}
	plan.gatewayOnly = true
	if msg, code, _ := applyInferenceGateway(c, plan, true, func(string) {}); msg != "" {
		return msg, code
	}
	if _, msg, code := writeInferenceGateway(c, plan, logf); msg != "" {
		return msg, code
	}
	return "", ""
}

// releaseGatewayProject takes a recorded project off the list once no
// model routes through the gateway from it (after its models were deleted).
// It returns a sentence for the result: what was done, or why the project
// stays ("" when there is nothing to say).
func releaseGatewayProject(c *Client, project string) string {
	recorded, err := readGatewayProjects(c)
	if err != nil {
		return fmt.Sprintf(" '%s' stays on the inference gateway's list: %v. Remove it from the list on the Models tab.", project, err)
	}
	if !containsString(recorded, project) {
		return ""
	}
	gw, found, err := readInferenceGateway(c)
	if err == nil && (!found || !gatewayOwned(gw.Metadata)) {
		_ = forgetGatewayProject(c, project)
		return ""
	}
	if err == nil {
		if listed, _ := gw.listedNamespaces(); !containsString(listed, project) {
			_ = forgetGatewayProject(c, project)
			return ""
		}
		if users, uerr := gatewayRoutedIn(c, gw, project); uerr == nil && len(users) > 0 {
			return ""
		}
	}
	if msg, code := removeGatewayProject(c, project, func(string) {}); msg != "" {
		if code == "in_use" {
			return ""
		}
		slog.Warn("project stays on the inference gateway's list", "project", project, "reason", msg)
		return " " + msg + " Remove it from the list on the Models tab once that is fixed."
	}
	if err := forgetGatewayProject(c, project); err != nil {
		slog.Warn("cannot update the gateway projects record", "error", err)
	}
	slog.Info("project taken off the inference gateway's list", "project", project, "user", getUser(c))
	return fmt.Sprintf(" '%s' was taken off the projects the inference gateway allows (this tool had added it, and no model there uses the gateway any more).", project)
}

// disallow is "Remove from the list" (steps "check", "gateway"): after the
// user confirmed it, project goes off this tool's gateway's list.
func (m *modelsRun) disallow(project string) (*types.OperationResponse, error) {
	c := m.c
	m.step("check", "running", fmt.Sprintf("Checking that no model in '%s' uses the inference gateway...", project))
	m.step("check", "success", "Checked.")
	m.step("gateway", "running", fmt.Sprintf("Taking '%s' off the projects the inference gateway allows...", project))
	if msg, code := removeGatewayProject(c, project, func(line string) { m.step("gateway", "running", line) }); msg != "" {
		return m.fail("gateway", msg+" Nothing was changed.", code, "gateway list failed")
	}
	if err := forgetGatewayProject(c, project); err != nil {
		slog.Warn("cannot update the gateway projects record", "error", err)
	}
	done := fmt.Sprintf("The inference gateway no longer allows '%s': routes from it no longer attach.", project)
	m.step("gateway", "success", done)
	recordModelsActivity(c, m.activity, m.target, true)
	return &types.OperationResponse{Success: true, Message: done, Logs: m.logs}, nil
}

// listedProjectsState fills the status's UnusedProjects (on this tool's
// gateway's list, no model routes through it from there) and GoneProjects
// (of those, the ones that no longer exist).
func listedProjectsState(c *Client, state *types.InferenceGatewayState, gw gatewayObject, models []llmisvcObject, def kserveGateway) {
	listed, shaped := gw.listedNamespaces()
	if !shaped {
		return
	}
	used := map[string]bool{}
	for _, ns := range routeNamespaces(models, def, gw) {
		used[ns] = true
	}
	seen := map[string]bool{}
	for _, ns := range listed {
		if ns == inferenceGatewayNamespace || used[ns] || seen[ns] {
			continue
		}
		seen[ns] = true
		state.UnusedProjects = append(state.UnusedProjects, ns)
		if _, found, err := readObjectMeta(c, clusterPath("v1", "namespaces", ns)); err == nil && !found {
			state.GoneProjects = append(state.GoneProjects, ns)
		}
	}
	sort.Strings(state.UnusedProjects)
	sort.Strings(state.GoneProjects)
}
