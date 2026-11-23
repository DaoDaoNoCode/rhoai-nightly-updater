package serving

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
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
// why). Every plan of the write checks that again, so a write planned
// again after a conflict cannot take off a project a model started to use
// meanwhile. A recorded project whose namespace was deleted goes off the
// list after the next Models change that succeeds, and after the models'
// teardown (Clean up), with the same checks. A name this tool did not add
// is never taken off by itself.
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

// gatewayRecord is the record: the projects this tool added, and the UID
// of each one's namespace when it added it (none for an entry an earlier
// build wrote). A namespace of that name with another UID was made again:
// it is not the project this tool added (gatewayRecord.addedHere).
type gatewayRecord struct {
	Projects []string          `json:"projects"`
	UIDs     map[string]string `json:"uids,omitempty"`
}

func readGatewayRecord(c *kube.Client) (gatewayRecord, error) {
	raw, err := record.ReadSnapshotKey(c, gatewayProjectsKey)
	if err != nil {
		return gatewayRecord{}, fmt.Errorf("read this tool's record of the projects it added to the inference gateway: %w", err)
	}
	var rec gatewayRecord
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &rec); err != nil {
			return gatewayRecord{}, fmt.Errorf("parse this tool's record of the projects it added to the inference gateway: %w", err)
		}
	}
	return rec, nil
}

func writeGatewayRecord(c *kube.Client, rec gatewayRecord) error {
	sort.Strings(rec.Projects)
	for p := range rec.UIDs {
		if !slices.Contains(rec.Projects, p) {
			delete(rec.UIDs, p)
		}
	}
	if len(rec.UIDs) == 0 {
		rec.UIDs = nil
	}
	data, _ := json.Marshal(rec)
	if err := record.WriteSnapshotKey(c, gatewayProjectsKey, gatewayProjectsManager, string(data)); err != nil {
		return fmt.Errorf("record the projects this tool added to the inference gateway: %w", err)
	}
	return nil
}

func ReadGatewayProjects(c *kube.Client) ([]string, error) {
	rec, err := readGatewayRecord(c)
	return rec.Projects, err
}

func WriteGatewayProjects(c *kube.Client, projects []string) error {
	return writeGatewayRecord(c, gatewayRecord{Projects: projects})
}

// addedHere: project is recorded, and its namespace is the one this tool
// added (the recorded UID, when there is one, is the live namespace's).
// A namespace that cannot be read is not (fail closed: an entry that may
// be someone else's is not taken off).
func (r gatewayRecord) addedHere(c *kube.Client, project string) bool {
	if !slices.Contains(r.Projects, project) {
		return false
	}
	uid := r.UIDs[project]
	if uid == "" {
		return true
	}
	meta, found, err := kube.ReadObjectMeta(c, rhoai.ClusterPath("v1", "namespaces", project))
	return err == nil && found && meta.UID == uid
}

// recordGatewayProject records that this tool adds project to the list,
// before the Gateway is written: a project that cannot be recorded is not
// added (it could never be taken off by this tool).
func recordGatewayProject(c *kube.Client, project string) error {
	rec, err := readGatewayRecord(c)
	if err != nil {
		return err
	}
	uid := ""
	if meta, found, err := kube.ReadObjectMeta(c, rhoai.ClusterPath("v1", "namespaces", project)); err == nil && found {
		uid = meta.UID
	}
	if slices.Contains(rec.Projects, project) && rec.UIDs[project] == uid {
		return nil
	}
	if !slices.Contains(rec.Projects, project) {
		rec.Projects = append(rec.Projects, project)
	}
	if rec.UIDs == nil {
		rec.UIDs = map[string]string{}
	}
	rec.UIDs[project] = uid
	return writeGatewayRecord(c, rec)
}

func forgetGatewayProject(c *kube.Client, project string) error {
	rec, err := readGatewayRecord(c)
	if err != nil {
		return err
	}
	var kept []string
	for _, p := range rec.Projects {
		if p != project {
			kept = append(kept, p)
		}
	}
	if len(kept) == len(rec.Projects) {
		return nil
	}
	rec.Projects = kept
	return writeGatewayRecord(c, rec)
}

// gatewayRoutedIn lists the models ("namespace/name", anyone's) in project
// that route through the inference gateway. An error is returned when the
// models cannot be listed: the caller keeps the project (fail closed).
func gatewayRoutedIn(c *kube.Client, gw GatewayObject, project string) ([]string, error) {
	models, err := ListLLMInferenceServices(c)
	if errors.Is(err, ErrLLMISvcAPIMissing) {
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
		if m.Metadata.Namespace == project && !m.Metadata.Terminating() {
			out = append(out, m.Metadata.Namespace+"/"+m.Metadata.Name)
		}
	}
	return out, nil
}

// planGatewayRemove makes every check of taking project off this tool's
// gateway's list, read-only, and a server-side dry run of the write. It
// returns the plan; nil when there is nothing to write (no gateway, or
// project is not on its list); or a refusal and a code.
func planGatewayRemove(c *kube.Client, project string) (*gatewayPlan, string, string) {
	if project == inferenceGatewayNamespace {
		return nil, fmt.Sprintf("%s is the gateway's own namespace and always stays on its list.", project), "validation"
	}
	gw, found, err := readInferenceGateway(c)
	switch {
	case err != nil:
		return nil, fmt.Sprintf("Cannot read the inference gateway, so %s stays on its list: %v.%s", project, err, kube.TemplateHint(err)), kube.ErrorCodeFromK8sErr(err)
	case !found:
		return nil, "", ""
	case !gatewayOwned(gw.Metadata):
		return nil, fmt.Sprintf("Gateway %s/%s was not created by this tool, so this tool does not change its list.", inferenceGatewayNamespace, inferenceGatewayName), "not_managed"
	}
	if listed, _ := gw.listedNamespaces(); !slices.Contains(listed, project) {
		return nil, "", ""
	}
	// The plan checks that no model in project routes through the gateway
	// (planInferenceGatewayList), as every plan of this write does.
	plan, msg, code := planInferenceGatewayList(c, nil, []string{project})
	if msg != "" {
		return nil, msg, code
	}
	if plan.foreign {
		return nil, fmt.Sprintf("Gateway %s/%s was not created by this tool, so this tool does not change its list.", inferenceGatewayNamespace, inferenceGatewayName), "not_managed"
	}
	if !plan.listChanges {
		return nil, "", ""
	}
	plan.gatewayOnly = true
	if msg, code, _ := applyInferenceGateway(c, plan, true, func(string) {}); msg != "" {
		return nil, msg, code
	}
	return plan, "", ""
}

// removeGatewayProject takes project off this tool's gateway's list, with
// every check first (step logf). It returns a refusal and a code, or "".
func removeGatewayProject(c *kube.Client, project string, logf func(string)) (string, string) {
	plan, msg, code := planGatewayRemove(c, project)
	if msg != "" || plan == nil {
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
func releaseGatewayProject(c *kube.Client, project string) string {
	recorded, err := readGatewayRecord(c)
	if err != nil {
		return fmt.Sprintf(" %s stays on the inference gateway's list: %v. Remove it from the list on the Models tab.", project, err)
	}
	if !slices.Contains(recorded.Projects, project) {
		return ""
	}
	// Made again since this tool added it: the entry is not this tool's.
	if !recorded.addedHere(c, project) {
		if meta, found, err := kube.ReadObjectMeta(c, rhoai.ClusterPath("v1", "namespaces", project)); err == nil && found && meta.UID != recorded.UIDs[project] {
			_ = forgetGatewayProject(c, project)
		}
		return ""
	}
	gw, found, err := readInferenceGateway(c)
	if err == nil && (!found || !gatewayOwned(gw.Metadata)) {
		_ = forgetGatewayProject(c, project)
		return ""
	}
	if err == nil {
		if listed, _ := gw.listedNamespaces(); !slices.Contains(listed, project) {
			_ = forgetGatewayProject(c, project)
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
	slog.Info("project taken off the inference gateway's list", "project", project, "user", kube.GetUser(c))
	return fmt.Sprintf(" %s was taken off the projects the inference gateway allows (this tool had added it, and no model there uses the gateway any more).", project)
}

// releaseGoneGatewayProjects takes off the gateway's list every project
// this tool recorded whose namespace no longer exists (a new project of
// that name would be admitted at once), with removeGatewayProject's
// checks. It runs at the start of each Models change and after the
// models' teardown (Models tab, Clean up), which hold the operation lock.
// A namespace that cannot be read counts as existing. It returns a
// sentence for the result ("" when there is nothing to say).
func releaseGoneGatewayProjects(c *kube.Client) string {
	recorded, err := ReadGatewayProjects(c)
	if err != nil || len(recorded) == 0 {
		return ""
	}
	var released, kept []string
	for _, project := range recorded {
		if _, exists, err := kube.ReadObjectMeta(c, rhoai.ClusterPath("v1", "namespaces", project)); err != nil || exists {
			continue
		}
		if msg, _ := removeGatewayProject(c, project, func(string) {}); msg != "" {
			slog.Warn("deleted project stays on the inference gateway's list", "project", project, "reason", msg)
			kept = append(kept, project)
			continue
		}
		if err := forgetGatewayProject(c, project); err != nil {
			slog.Warn("cannot update the gateway projects record", "error", err)
		}
		slog.Info("deleted project taken off the inference gateway's list", "project", project, "user", kube.GetUser(c))
		released = append(released, project)
	}
	out := ""
	if len(released) > 0 {
		out += fmt.Sprintf(" Project %s no longer %s, so %s taken off the projects the inference gateway allows.", strings.Join(released, ", "), kube.Verb(len(released), "exists", "exist"), kube.Verb(len(released), "it was", "they were"))
	}
	if len(kept) > 0 {
		out += fmt.Sprintf(" Project %s no longer %s but %s on the inference gateway's list; remove %s on the Models tab.", strings.Join(kept, ", "), kube.Verb(len(kept), "exists", "exist"), kube.Verb(len(kept), "stays", "stay"), kube.Verb(len(kept), "it", "them"))
	}
	return out
}

// disallow is "Remove from the list" (steps "check", "gateway"): after the
// user confirmed it, project goes off this tool's gateway's list. Every
// check and the server-side dry run run on step "check", before the write.
func (m *modelsRun) disallow(project string) (*types.OperationResponse, error) {
	c := m.c
	m.step("check", "running", fmt.Sprintf("Checking that no model in %s uses the inference gateway...", project))
	plan, msg, code := planGatewayRemove(c, project)
	if msg != "" {
		return m.fail("check", msg+" Nothing was changed.", code, "gateway check failed")
	}
	if plan == nil {
		m.step("check", "success", "Checked.")
		if err := forgetGatewayProject(c, project); err != nil {
			slog.Warn("cannot update the gateway projects record", "error", err)
		}
		done := fmt.Sprintf("%s is not on the inference gateway's list; nothing was changed.", project)
		m.step("gateway", "success", done)
		return &types.OperationResponse{Success: true, Message: done, Logs: m.logs}, nil
	}
	m.step("check", "success", fmt.Sprintf("No model in %s uses the inference gateway, and the API server accepts the change.", project))
	m.step("gateway", "running", fmt.Sprintf("Taking %s off the projects the inference gateway allows...", project))
	if _, msg, code := writeInferenceGateway(c, plan, func(line string) { m.step("gateway", "running", line) }); msg != "" {
		return m.fail("gateway", msg+" Nothing was changed.", code, "gateway list failed")
	}
	if err := forgetGatewayProject(c, project); err != nil {
		slog.Warn("cannot update the gateway projects record", "error", err)
	}
	done := fmt.Sprintf("The inference gateway no longer allows %s: routes from it no longer attach.", project)
	m.step("gateway", "success", done)
	record.Action(c, m.activity, m.target, true)
	return &types.OperationResponse{Success: true, Message: done, Logs: m.logs}, nil
}

// listedProjectsState fills the status's UnusedProjects (on this tool's
// gateway's list, no model routes through it from there) and GoneProjects
// (of those, the ones that no longer exist).
func listedProjectsState(c *kube.Client, state *types.InferenceGatewayState, gw GatewayObject, models []LLMISvcObject, def KServeGateway) {
	listed, shaped := gw.listedNamespaces()
	if !shaped {
		return
	}
	if recorded, err := readGatewayRecord(c); err == nil {
		for _, p := range recorded.Projects {
			if slices.Contains(listed, p) && recorded.addedHere(c, p) {
				state.AddedProjects = append(state.AddedProjects, p)
			}
		}
		sort.Strings(state.AddedProjects)
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
		if _, found, err := kube.ReadObjectMeta(c, rhoai.ClusterPath("v1", "namespaces", ns)); err == nil && !found {
			state.GoneProjects = append(state.GoneProjects, ns)
		}
	}
	sort.Strings(state.UnusedProjects)
	sort.Strings(state.GoneProjects)
}
