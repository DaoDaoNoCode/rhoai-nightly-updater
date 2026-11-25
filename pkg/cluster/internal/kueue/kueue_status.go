package kueue

import (
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/authz"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/dashdev"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Labels the dashboard applies to Workload states (odh-dashboard rhoai-3.6
// packages/k8s-core/src/kueue/types.ts KueueWorkloadStatus).
const (
	StateQueued       = "Queued"
	StateInadmissible = "Inadmissible"
	StateAdmitted     = "Admitted"
	StateRunning      = "Running"
	StateComplete     = "Complete"
	StateFailed       = "Failed"
	StateEvicted      = "Evicted"
	StatePreempted    = "Preempted"
)

var (
	failureText = regexp.MustCompile(`error|failed|rejected|timeout|timed out`)
	successText = regexp.MustCompile(`success|succeeded`)
)

// WorkloadState mirrors how the dashboard maps a Workload's conditions to a
// status (packages/k8s-core/src/kueue/workloadStatus.ts
// getKueueWorkloadStatusWithMessage), checking in this order: Failed,
// Evicted, Inadmissible, Preempted, Complete, Running, Admitted, Queued.
// Omitted here: permanent-inadmissible message patterns, preemption gates,
// and finer points around requeue and admission checks.
func WorkloadState(conds []Condition) (string, string) {
	msg := func(c Condition) string { return strings.TrimSpace(kube.FirstNonEmpty(c.Message, c.Reason)) }
	var failed, succeeded, fallback *Condition
	for i, c := range conds {
		if c.Status != "True" || c.Type != "Finished" {
			continue
		}
		text := strings.ToLower(c.Message + " " + c.Reason)
		if failureText.MatchString(text) {
			if failed == nil {
				failed = &conds[i]
			}
			continue
		}
		if fallback == nil {
			fallback = &conds[i]
		}
		if succeeded == nil && successText.MatchString(text) {
			succeeded = &conds[i]
		}
	}
	if succeeded == nil {
		succeeded = fallback
	}
	is := func(typ, status string) (Condition, bool) {
		for _, c := range conds {
			if c.Type == typ && c.Status == status {
				return c, true
			}
		}
		return Condition{}, false
	}
	if failed != nil {
		return StateFailed, msg(*failed)
	}
	if c, ok := is("Evicted", "True"); ok {
		if c.Reason == "Preempted" {
			return StatePreempted, msg(c)
		}
		return StateEvicted, msg(c)
	}
	for _, c := range conds {
		if c.Type == "QuotaReserved" && c.Status == "False" && c.Reason == "Inadmissible" {
			return StateInadmissible, msg(c)
		}
	}
	if c, ok := is("Preempted", "True"); ok {
		return StatePreempted, msg(c)
	}
	if succeeded != nil {
		return StateComplete, msg(*succeeded)
	}
	if c, ok := is("PodsReady", "True"); ok {
		return StateRunning, msg(c)
	}
	if c, ok := is("Admitted", "True"); ok {
		return StateAdmitted, msg(c)
	}
	if c, ok := is("QuotaReserved", "False"); ok {
		return StateQueued, msg(c)
	}
	return StateQueued, ""
}

// check captures what cluster Kueue permits, evaluated ahead of setup: the
// denial when setup must stop (fail closed) and how the status represents the
// instance.
type check struct {
	instance      types.KueueInstance
	refusal, code string
}

// checkCluster, using the user's token u, determines whether this tool may
// configure queues: kueue Unmanaged on the DataScienceCluster (dsc), Kueue's
// API at v1beta2, the Kueue CR cluster present and Available, labelPolicy
// other than None, and KueueReady True. Failed reads produce a refusal.
func checkCluster(u *kube.Client, dsc rhoai.DSCRead, block *types.ComponentBlock) check {
	var ck check
	refuse := func(msg, code string) {
		if ck.refusal == "" {
			ck.refusal, ck.code = msg, code
		}
	}
	if block != nil {
		// The prerequisites' button turns it on: one sentence, no fix to repeat.
		msg := "Set up its prerequisites first (above): Kueue is turned off on this cluster."
		if block.DSC == "" {
			msg = "Set up its prerequisites first (above): " + block.Reason
		}
		refuse(msg, "prerequisites")
	}
	ck.instance.KueueReady, ck.instance.Message = kueueReady(dsc)
	served, group, err := v1beta2Served(u)
	switch {
	case err != nil:
		refuse(fmt.Sprintf("Cannot read Kueue's API versions: %v.", err), kube.FirstNonEmpty(kube.ErrorCodeFromK8sErr(err), "internal"))
	case !group:
		refuse("Kueue's API (kueue.x-k8s.io) is not served yet: the Red Hat build of Kueue serves it once it runs.", "prerequisites")
	case !served:
		refuse("This cluster's Kueue does not serve kueue.x-k8s.io/v1beta2, the only version the RHOAI dashboard reads (the Red Hat build of Kueue 1.3 and later serve it). A cluster-admin upgrades the Red Hat build of Kueue.", "unsupported")
	}
	ck.instance.V1beta2 = served
	k, found, err := readInstance(u)
	switch {
	case err != nil:
		refuse(fmt.Sprintf("Cannot read the Kueue CR cluster: %v.", err), kube.FirstNonEmpty(kube.ErrorCodeFromK8sErr(err), "internal"))
	case !found:
		refuse("RHOAI has not created the Kueue CR cluster yet (it does once the DataScienceCluster's kueue is Unmanaged and the Red Hat build of Kueue runs).", "not_ready")
	default:
		ck.instance.Found = true
		ck.instance.LabelPolicy = k.labelPolicy()
		cond, ok := findCondition(k.Status.Conditions, "Available")
		ck.instance.Available = ok && cond.Status == "True"
		if !ck.instance.Available {
			ck.instance.Message = kube.TruncateText(kube.FirstNonEmpty(cond.Message, cond.Reason, "it reports no Available condition yet"), 300)
		}
		if ck.instance.LabelPolicy == "None" {
			refuse("The Kueue CR cluster sets workloadManagement.labelPolicy None: Kueue then takes over every job-like object in a labelled project, not only those sent to a queue. This tool does not label a project under that setting; a cluster-admin decides it.", "unsupported")
		}
		if !ck.instance.Available {
			refuse("The Kueue CR cluster is not Available: "+ck.instance.Message+".", "not_ready")
		}
	}
	// Only KueueReady True counts: absent (not reported yet), Unknown and
	// False all mean RHOAI does not confirm Kueue.
	switch ck.instance.KueueReady {
	case "True":
	case "":
		refuse("RHOAI does not report Kueue ready yet (no DataScienceCluster condition KueueReady).", "not_ready")
	default:
		why := ""
		if ck.instance.Message != "" {
			why = ": " + ck.instance.Message
		}
		refuse(fmt.Sprintf("RHOAI reports Kueue not ready (DataScienceCluster condition KueueReady is %s)%s.", ck.instance.KueueReady, why), "not_ready")
	}
	ck.instance.Ready = ck.instance.KueueReady == "True" && ck.instance.Available && ck.instance.V1beta2
	return ck
}

// kueueReady reads the DataScienceCluster KueueReady condition, which is
// True when kueue is Unmanaged and RHBoK is healthy (rhods-operator rhoai-3.6
// internal/controller/components/kueue kueue.go UpdateDSCStatus).
func kueueReady(dsc rhoai.DSCRead) (string, string) {
	status, _ := dsc.Object["status"].(map[string]any)
	conds, _ := status["conditions"].([]any)
	for _, c := range conds {
		m, _ := c.(map[string]any)
		if m["type"] == "KueueReady" {
			s, _ := m["status"].(string)
			msg, _ := m["message"].(string)
			reason, _ := m["reason"].(string)
			return s, kube.TruncateText(kube.FirstNonEmpty(msg, reason), 300)
		}
	}
	return "", ""
}

// objectState describes a single queue object as this tool has read it.
func objectState(kind, name, ns string, q Queue, found bool, uid string) types.KueueObject {
	o := types.KueueObject{Kind: kind, Name: name, Namespace: ns, Present: found}
	if !found {
		return o
	}
	o.ManagedByTool = owns(q.Metadata, uid)
	o.Foreign = !o.ManagedByTool
	o.Terminating = q.Metadata.Terminating()
	if kind == "ResourceFlavor" {
		o.Active = true
		return o
	}
	o.Active, o.Message = q.active()
	if o.Active {
		o.Message = ""
	}
	return o
}

// sampleState loads a sample plus the derived state of its Workload.
func sampleState(u *kube.Client, kind, project string, rec Record, workloads []Workload) (types.KueueSample, error) {
	name := RayClusterName
	if kind == SampleRayJob {
		name = RayJobName
	}
	s := types.KueueSample{Kind: kind, Name: name}
	r, found, err := readSample(u, kind, project)
	if err != nil || !found {
		return s, err
	}
	s.Present = true
	s.ManagedByTool = owns(r.Metadata, rec.SampleUID(kind))
	s.Foreign = !s.ManagedByTool
	s.Terminating = r.Metadata.Terminating()
	s.Image = r.image()
	if s.ManagedByTool {
		switch same, known := r.sizedAsManifest(); {
		case !known:
			// A size that cannot be read is not called outdated.
			slog.Debug("cannot compare a Kueue sample's resources with this version's", "sample", name, "project", project)
		case !same:
			s.Outdated = fmt.Sprintf("its head and worker have other CPU and memory than this version sets (head %s; worker %s)", headSize.text(), workerSize.text())
		}
	}
	// One state: a RayJob's job status when it has one (RUNNING,
	// SUCCEEDED...), else its deployment status; a RayCluster's state.
	s.RayState = stateWords(kube.FirstNonEmpty(r.Status.JobStatus, r.Status.State, r.Status.JobDeploymentStatus))
	for _, w := range workloads {
		if w.Metadata.OwnedBy(r.Metadata.UID) {
			s.State, s.Message = WorkloadState(w.Status.Conditions)
			s.Message = kube.TruncateText(s.Message, 300)
		}
	}
	return s, nil
}

// quotaDiffers says whether a live nominal quota is known to differ from
// the one this version sets, as amounts ("20480Mi" is "20Gi"); a quota
// that cannot be read is not called different.
func quotaDiffers(cpu, memory string) bool {
	wantCPU, wantMemory := Quota()
	sameCPU, knownCPU := kube.SameQuantity(cpu, wantCPU)
	sameMemory, knownMemory := kube.SameQuantity(memory, wantMemory)
	return (knownCPU && !sameCPU) || (knownMemory && !sameMemory)
}

// GetStatus powers the Distributed workloads tab: DataScienceCluster data via
// sa (same as other tabs), everything else via the user's token u.
func GetStatus(sa, u *kube.Client, project string) (*types.KueueStatus, error) {
	if !u.ActsAsUser() {
		return nil, authz.ErrNotUserClient
	}
	st := &types.KueueStatus{Project: project, DefaultRayImage: DefaultRayImage, Quota: describeQuota(), Objects: []types.KueueObject{}, Samples: []types.KueueSample{}}
	rec, err := ReadRecord(u)
	if err != nil {
		return nil, err
	}
	if rec.Project != "" {
		if project != "" && project != rec.Project {
			st.SetUpIn = rec.Project
		}
		st.Project = rec.Project
	}
	dsc, dscErr := rhoai.ReadFirstDSC(sa)
	var block *types.ComponentBlock
	if dscErr == nil {
		block = envkit.ComponentBlock(dsc, envkit.KueueComponent)
		st.Blocked = block
		if !envkit.ComponentOn(dsc, "ray") {
			st.RayBlocked = envkit.ComponentBlock(dsc, rayComponent)
		}
	}
	st.DashboardURL = dashdev.DashboardURL(sa)
	ck := checkCluster(u, dsc, block)
	if dscErr != nil {
		ck.refusal, ck.code = fmt.Sprintf("Cannot read the DataScienceCluster: %v.", dscErr), kube.ErrorCodeFromK8sErr(dscErr)
	}
	st.Instance, st.Refusal, st.RefusalCode = ck.instance, ck.refusal, ck.code
	var problems []string
	note := func(what string, err error) {
		problems = append(problems, fmt.Sprintf("Cannot read %s: %v.", what, err))
	}
	// Queue objects are fetched only after Kueue's API is served.
	if st.Instance.V1beta2 {
		rf, found, err := readFlavor(u)
		if err != nil {
			note("ResourceFlavor "+FlavorName, err)
		}
		st.Objects = append(st.Objects, objectState("ResourceFlavor", FlavorName, "", rf, found && err == nil, rec.FlavorUID))
		cq, found, err := readClusterQueue(u)
		if err != nil {
			note("ClusterQueue "+ClusterQueueName, err)
		}
		cqState := objectState("ClusterQueue", ClusterQueueName, "", cq, found && err == nil, rec.ClusterQueueUID)
		if found && err == nil {
			// The live quota, not the one Set up would create: Set up keeps
			// a ClusterQueue that is there.
			if cpu, memory := cq.nominalQuota(); cpu != "" || memory != "" {
				st.Quota = fmt.Sprintf("%s CPUs and %s of memory", kube.FirstNonEmpty(cpu, "no"), kube.FirstNonEmpty(memory, "no"))
				if cqState.ManagedByTool && quotaDiffers(cpu, memory) {
					cqState.Outdated = fmt.Sprintf("its quota is %s; this version sets %s", st.Quota, describeQuota())
				}
			}
		}
		st.Objects = append(st.Objects, cqState)
		if st.Project != "" {
			lq, found, err := readLocalQueue(u, st.Project)
			if err != nil {
				note("LocalQueue "+LocalQueueName, err)
			}
			st.Objects = append(st.Objects, objectState("LocalQueue", LocalQueueName, st.Project, lq, found && err == nil, rec.LocalQueueUID))
			if qs, err := listLocalQueues(u, st.Project); err != nil {
				note("the project's LocalQueues", err)
			} else {
				for _, q := range qs {
					st.DefaultQueue = st.DefaultQueue || q.Metadata.Name == "default"
				}
			}
		}
	}
	if st.Project != "" {
		ns, found, err := readNamespace(u, st.Project)
		switch {
		case err != nil:
			note("project "+st.Project, err)
		case found:
			st.Label = &types.KueueLabel{Present: ns.Metadata.Labels[ProjectLabel] == "true", AddedByTool: rec.LabelAdded && rec.Project == st.Project && ns.Metadata.UID == rec.ProjectUID}
		}
		var workloads []Workload
		if st.Instance.V1beta2 {
			if workloads, err = listWorkloads(u, st.Project); err != nil {
				note("the project's Workloads", err)
			}
		}
		for _, kind := range []string{SampleRayCluster, SampleRayJob} {
			s, err := sampleState(u, kind, st.Project, rec, workloads)
			if err != nil {
				note(sampleText(kind), err)
			}
			st.Samples = append(st.Samples, s)
		}
	}
	if apps, err := rhoai.AppsNamespace(u); err == nil {
		st.OptIn = optInState(u, apps, rec, note)
	}
	// The queues Clean up's turning kueue back counts (restoreCheck): the
	// tab offers that only while there are none.
	if users, err := Users(u); err != nil {
		st.QueuesUnknown = true
		note("the cluster's ClusterQueues and LocalQueues", err)
	} else {
		st.Queues = append([]string{}, users...)
	}
	st.ItemsProblem = strings.Join(problems, " ")
	return st, nil
}

// optInState inspects the dashboard toggle and this tool's hardware profile.
func optInState(u *kube.Client, apps string, rec Record, note func(string, error)) types.KueueOptIn {
	out := types.KueueOptIn{Profile: types.KueueObject{Kind: "HardwareProfile", Name: ProfileName, Namespace: apps}}
	cfg, found, err := readDashboardConfig(u, apps)
	if err != nil {
		note("the dashboard's settings (OdhDashboardConfig)", err)
	} else if found {
		out.DisableKueue = cfg.Spec.DashboardConfig.DisableKueue
	}
	o := rec.OptIn
	if o != nil {
		out.On = true
		out.Previous = o.Previous
		out.SetByTool = o.DisableKueueSet && found && configOwned(cfg)
	}
	meta, pFound, err := readProfile(u, apps)
	if err != nil {
		note("hardware profile "+ProfileName, err)
	} else if pFound {
		out.Profile.Present = true
		out.Profile.ManagedByTool = o != nil && owns(meta, o.ProfileUID)
		out.Profile.Foreign = !out.Profile.ManagedByTool
		out.Profile.Active = true
	}
	return out
}

// configOwned means disableKueue is false, this tool's field manager set it,
// and no other actor has changed it since (the API server assigns a field to
// whichever client last mutates its value).
func configOwned(cfg DashboardConfig) bool {
	v := cfg.Spec.DashboardConfig.DisableKueue
	return v != nil && !*v && cfg.Metadata.OwnsFieldPath([]string{dashboardManager}, "f:spec", "f:dashboardConfig", "f:disableKueue")
}

// rayComponent is the component metadata for samples (KubeRay).
var rayComponent = envkit.FeatureComponent{Feature: "Ray (distributed workloads)", Keys: []string{"ray"}}

// InstanceWaiting is what the prerequisites' ready step still waits for
// once RHOAI's kueue is turned on (user token): Kueue's API at v1beta2 and
// the Kueue CR cluster that RHOAI creates, Available. A refusal stops the
// wait: v1beta2 not served, or labelPolicy None.
func InstanceWaiting(c *kube.Client) (waiting, refusal string, err error) {
	served, group, err := v1beta2Served(c)
	switch {
	case err != nil:
		return "", "", err
	case !group:
		return "Kueue's API (kueue.x-k8s.io) is not served yet", "", nil
	case !served:
		return "", "This cluster's Kueue does not serve kueue.x-k8s.io/v1beta2, the only version the RHOAI dashboard reads. A cluster-admin upgrades the Red Hat build of Kueue.", nil
	}
	k, found, err := readInstance(c)
	switch {
	case err != nil:
		return "", "", err
	case !found:
		return "RHOAI has not created the Kueue CR cluster yet", "", nil
	case k.labelPolicy() == "None":
		return "", "The Kueue CR cluster sets workloadManagement.labelPolicy None; this tool does not set up queues under that setting.", nil
	}
	if cond, ok := findCondition(k.Status.Conditions, "Available"); !ok || cond.Status != "True" {
		return "the Kueue CR cluster is not Available yet: " + kube.TruncateText(kube.FirstNonEmpty(cond.Message, cond.Reason, "no condition yet"), 300), "", nil
	}
	return "", "", nil
}

// stateWords writes an upper-case state as a word ("RUNNING" -> "Running");
// others stay as they are.
func stateWords(s string) string {
	if s == "" || strings.ToUpper(s) != s {
		return s
	}
	return s[:1] + strings.ToLower(s[1:])
}
