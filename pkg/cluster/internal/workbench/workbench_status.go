package workbench

import (
	"fmt"
	"slices"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/authz"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/dashdev"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Workbenches are served by this DataScienceCluster component
// (rhods-operator api/datasciencecluster v1-v3: "workbenches").
var Component = envkit.FeatureComponent{Feature: "Workbenches", Keys: []string{"workbenches"}}

// Workbench status labels as rendered in the dashboard (pages/projects/notebook
// utils.ts, concepts/notebooks/NotebookStatusLabel.tsx).
const (
	StateStarting = "starting"
	StateRunning  = "running"
	StateStopping = "stopping"
	StateStopped  = "stopped"
	StateFailed   = "failed"
)

// Unsupported returns text when workbenches cannot be configured in the
// RHOAI 3 style, or "" when they can: a v1-only DataScienceCluster implies
// RHOAI 2.x, where workbenches rely on per-Route oauth-proxy (inject-oauth)
// rather than kube-rbac-proxy on the shared gateway (the dashboard 3.x form
// sets inject-auth). Setup dry-run validates the controller as well
// (kubeRBACProxy).
func Unsupported(dsc rhoai.DSCRead) string {
	if dsc.State == rhoai.DSCStatePresent && dsc.Version == "v1" {
		return "This cluster runs RHOAI 2.x (its DataScienceCluster API is v1 only). This tool sets up workbenches the RHOAI 3 way (kube-rbac-proxy on the shared gateway), which 2.x does not have. Create a workbench in the dashboard instead."
	}
	return ""
}

// componentOn implements dashboard isAreaAvailable for one component—true when
// status.components.<key>.managementState is Managed or Unmanaged, with spec
// used as fallback if status lacks the entry.
func componentOn(dsc rhoai.DSCRead, key string) bool {
	status, _ := dsc.Object["status"].(map[string]any)
	comps, _ := status["components"].(map[string]any)
	if comp, ok := comps[key].(map[string]any); ok {
		if s, _ := comp["managementState"].(string); s != "" {
			return s == "Managed" || s == "Unmanaged"
		}
	}
	s, _ := envkit.ComponentManagement(dsc.Object, key)
	return s == "Managed" || s == "Unmanaged"
}

// stopped follows dashboard isStopped: the stop annotation is present and
// differs from the controller create-time lock token.
func stopped(meta kube.ObjectMeta) bool {
	v, ok := meta.Annotations[StoppedAnnotation]
	return ok && v != controllerLock
}

// terminal holds Waiting reasons that stay until something fixes them—the
// workbench counts as failed.
var terminal = []string{"ImagePullBackOff", "ErrImagePull", "InvalidImageName", "CrashLoopBackOff", "CreateContainerConfigError", "CreateContainerError", "RunContainerError"}

// State mirrors how the dashboard classifies a workbench
// (pages/projects/notebook/service.ts, utils.ts). podsReady means some pod has
// every container ready and running, kube-rbac-proxy included; pair that with
// isStopped for Starting, Running, Stopping, or Stopped. While still Starting,
// a container stuck in a non-recovering Waiting reason (bad image pull, crash
// loop, absent Secret, and similar) yields Failed plus that reason.
func State(nb Notebook, pods []Pod) (state, reason, message string) {
	ready := false
	for _, p := range pods {
		if p.Metadata.Terminating() || len(p.Status.ContainerStatuses) == 0 {
			continue
		}
		all := true
		for _, cs := range p.Status.ContainerStatuses {
			all = all && cs.Ready && cs.State.Running != nil
		}
		ready = ready || all
	}
	isStopped := stopped(nb.Metadata)
	switch {
	case !isStopped && ready:
		return StateRunning, "", ""
	case isStopped && ready:
		return StateStopping, "", ""
	case isStopped:
		return StateStopped, "", ""
	}
	for _, p := range pods {
		if p.Metadata.Terminating() {
			continue
		}
		for _, cs := range p.Status.ContainerStatuses {
			if w := cs.State.Waiting; w != nil && slices.Contains(terminal, w.Reason) {
				return StateFailed, w.Reason, kube.FirstNonEmpty(w.Message, w.Reason)
			}
		}
		for _, cond := range p.Status.Conditions {
			if cond.Type == "PodScheduled" && cond.Status == "False" {
				return StateStarting, kube.FirstNonEmpty(cond.Reason, "Unschedulable"), cond.Message
			}
		}
	}
	return StateStarting, "", ""
}

// itemState builds a WorkbenchState from the project's nightly-workbench and
// PVC; found is false only if both resources are missing.
func itemState(c *kube.Client, project, dashboardURL string, rec Record) (types.WorkbenchState, bool, error) {
	st := types.WorkbenchState{ResourceState: types.ResourceState{Namespace: project, Name: Name}}
	nb, found, err := readNotebook(c, project)
	if err != nil {
		return st, false, err
	}
	pvc, pvcFound, err := readStorage(c, project)
	if err != nil {
		return st, false, err
	}
	if pvcFound {
		st.Storage = &types.WorkbenchStorage{Name: StorageName, Size: pvc.Spec.Resources.Requests["storage"], StorageClass: pvc.Spec.StorageClassName, ManagedByTool: rec.OwnsStorage(pvc.Metadata)}
	}
	if !found {
		st.ManagedByTool = true
		return st, pvcFound, nil
	}
	st.Deployed = true
	st.UID = nb.Metadata.UID
	st.ManagedByTool = rec.OwnsNotebook(nb.Metadata)
	st.Terminating = nb.Metadata.Terminating()
	st.URL = OpenURL(dashboardURL, project)
	st.Image = nb.Metadata.Annotations[imageDisplayAnnotation]
	if sel := nb.Metadata.Annotations[imageSelectionAnnotation]; sel != "" {
		st.Image = kube.FirstNonEmpty(st.Image, sel)
		if _, tag, ok := cutLast(sel, ":"); ok && st.Image != sel {
			st.Image += " (" + tag + ")"
		}
	}
	st.HardwareProfile = profileLabel(c, nb.Metadata.Annotations[profileNamespaceAnnotation], nb.Metadata.Annotations[profileNameAnnotation])
	for _, ct := range nb.Spec.Template.Spec.Containers {
		if ct.Name == Name {
			st.CPU, st.Memory = ResourceText(ct.Resources.Requests["cpu"]), ResourceText(ct.Resources.Requests["memory"])
		}
	}
	_, st.ConnectionAttached = withoutEntry(nb.Metadata.Annotations[ConnectionsAnnotation], project)
	if st.Terminating {
		st.State = StateStopping
		return st, true, nil
	}
	pods, err := listPods(c, project)
	if err != nil {
		return st, true, err
	}
	state, reason, msg := State(nb, pods)
	st.State, st.WaitingReason = state, reason
	st.Ready = state == StateRunning
	st.TerminalError = state == StateFailed
	if msg != "" {
		st.Message = kube.TruncateText(msg, 300)
	}
	return st, true, nil
}

func cutLast(s, sep string) (string, string, bool) {
	for i := len(s) - len(sep); i >= 0; i-- {
		if s[i:i+len(sep)] == sep {
			return s[:i], s[i+len(sep):], true
		}
	}
	return s, "", false
}

// GetStatus produces admin-facing Workbench feature status: DSC/cluster facts
// via sa, user-scoped inventory through u, the target project's
// nightly-workbench, and the resources Set up would apply there.
func GetStatus(sa, u *kube.Client, project string) (*types.WorkbenchStatus, error) {
	if !u.ActsAsUser() {
		return nil, authz.ErrNotUserClient
	}
	st := &types.WorkbenchStatus{Project: project}
	dsc, dscErr := rhoai.ReadFirstDSC(sa)
	if dscErr == nil {
		st.Blocked = envkit.ComponentBlock(dsc, Component)
		st.Unsupported = Unsupported(dsc)
	}
	dashURL := dashdev.DashboardURL(sa)
	rec, err := ReadRecord(u)
	if err != nil {
		return nil, err
	}
	projects := rec.Projects()
	if project != "" && !slices.Contains(projects, project) {
		projects = append(projects, project)
	}
	for _, p := range projects {
		item, found, err := itemState(u, p, dashURL, rec)
		if err != nil {
			if p == project {
				st.PlanProblem, st.PlanProblemCode = fmt.Sprintf("Cannot read the workbench in %s: %v.", p, err), kube.ErrorCodeFromK8sErr(err)
			}
			st.ItemsProblem = fmt.Sprintf("Cannot read the workbench in %s: %v.", p, err)
			st.ItemsUnreadable = append(st.ItemsUnreadable, p)
			continue
		}
		if found {
			st.Items = append(st.Items, item)
		}
	}
	if project == "" || st.PlanProblem != "" || st.Blocked != nil || st.Unsupported != "" {
		return st, nil
	}
	if dscErr != nil {
		st.PlanProblem, st.PlanProblemCode = fmt.Sprintf("Cannot read the DataScienceCluster: %v.", dscErr), kube.ErrorCodeFromK8sErr(dscErr)
		return st, nil
	}
	plan, msg, code := BuildPlan(u, project, dsc)
	if msg != "" {
		st.PlanProblem, st.PlanProblemCode = msg, code
		return st, nil
	}
	st.Plan = PlanView(plan)
	return st, nil
}

// PlanView converts an internal Plan into the struct the UI exposes.
func PlanView(p Plan) *types.WorkbenchPlan {
	v := &types.WorkbenchPlan{
		Image: p.Image.DisplayName, ImageStream: p.Image.Stream, ImageTag: p.Image.Tag,
		HardwareProfile: p.Profile.DisplayName, HardwareProfileName: p.Profile.Namespace + "/" + p.Profile.Name,
		CPU: ResourceText(p.Profile.Resources["cpu"]), Memory: ResourceText(p.Profile.Resources["memory"]),
		Queue:       p.Profile.Queue,
		StorageSize: p.StorageSize, StorageClass: p.StorageClass,
		Connection: p.Connection, Pipelines: p.Pipelines, MLflow: p.MLflow,
	}
	return v
}

// BuildPlan, acting as c, resolves the payload Set up would create in project—
// aligned with the dashboard Create workbench form—or a failure message
// (e.g. "no_image", "no_profile", "validation", or the underlying API code).
func BuildPlan(c *kube.Client, project string, dsc rhoai.DSCRead) (Plan, string, string) {
	labels, msg, code := projectLabels(c, project)
	if msg != "" {
		return Plan{}, msg, code
	}
	apps, err := rhoai.AppsNamespace(c)
	if err != nil {
		return Plan{}, fmt.Sprintf("Cannot find RHOAI's applications namespace, where the workbench images are: %v.", err), "prerequisites"
	}
	fail := func(what string, err error) (Plan, string, string) {
		return Plan{}, fmt.Sprintf("Cannot read %s: %v.", what, err), kube.FirstNonEmpty(kube.ErrorCodeFromK8sErr(err), "internal")
	}
	cfg, err := readDashboardConfig(c, apps)
	if err != nil {
		return fail("the dashboard's settings (OdhDashboardConfig)", err)
	}
	streams, err := listImageStreams(c, apps)
	if err != nil {
		return fail("the workbench images", err)
	}
	image, ok := ChooseImage(streams)
	if !ok {
		return Plan{}, fmt.Sprintf("The dashboard offers no workbench image: no ImageStream in %s labelled %s=true has a tag that is not outdated and has been imported.", apps, notebookImageLabel), "no_image"
	}
	global, err := listProfiles(c, apps)
	if err != nil {
		return fail("the hardware profiles", err)
	}
	local, err := listProfiles(c, project)
	if err != nil {
		return fail("the project's hardware profiles", err)
	}
	disable := cfg.Spec.DashboardConfig.DisableKueue
	kueueOn := componentOn(dsc, "kueue") && disable != nil && !*disable
	mode := ModeFor(labels[KueueProjectLabel] == "true", kueueOn)
	var queues []string
	if mode == OnlyKueue {
		if queues, err = listLocalQueues(c, project); err != nil {
			return fail("the project's LocalQueues", err)
		}
	}
	profile, why := ChooseProfile(global, local, mode, queues)
	if why != "" {
		// "no_profile": the cluster offers none for workbenches;
		// "no_profile_project": none fits this project (its Kueue setting).
		code := "no_profile_project"
		if strings.HasPrefix(why, "No enabled") {
			code = "no_profile"
		}
		return Plan{}, why, code
	}
	classes, err := listStorageClasses(c)
	if err != nil {
		return fail("the storage classes", err)
	}
	connection, err := toolConnection(c, project)
	if err != nil {
		return fail("the dashboard connection "+ConnectionSecret, err)
	}
	pipelines, err := pipelineConfigExists(c, project)
	if err != nil {
		return fail("Secret "+PipelineSecret, err)
	}
	return Plan{
		Project: project, Username: kube.GetUser(c), Image: image, Profile: profile,
		StorageSize:  kube.FirstNonEmpty(cfg.Spec.NotebookController.PVCSize, DefaultStorageSize),
		StorageClass: ChooseStorageClass(classes, cfg.Spec.NotebookController.StorageClassName),
		Connection:   connection, Pipelines: pipelines, MLflow: componentOn(dsc, "mlflowoperator"),
	}, "", ""
}

// profileLabel is the hardware profile's display name as the
// dashboard shows it, else its name ("" without one). A profile that
// cannot be read or is gone shows by name.
func profileLabel(c *kube.Client, namespace, name string) string {
	if name == "" || namespace == "" {
		return name
	}
	profiles, err := listProfiles(c, namespace)
	if err != nil {
		return name
	}
	for _, p := range profiles {
		if p.Metadata.Name == name {
			return kube.FirstNonEmpty(p.Metadata.Annotations[profileDisplayName], name)
		}
	}
	return name
}
