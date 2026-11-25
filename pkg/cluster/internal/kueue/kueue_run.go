package kueue

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// GoneWait limits how long delete paths wait on finalizers. Kueue keeps
// ClusterQueue and ResourceFlavor until dependents release them; KubeRay
// winds down RayCluster pods with comparable blocking. Tests tighten the cap.
var GoneWait = 2 * time.Minute

// Validate reports why req is rejected, or "" when the request passes.
func Validate(req types.KueueRequest) string {
	switch req.Action {
	case "setup", "teardown", "delete-sample", "optin", "optout":
	default:
		return "action must be setup, teardown, delete-sample, optin or optout"
	}
	if req.Action == "setup" || req.Project != "" {
		if len(req.Project) > 63 || !projectName.MatchString(req.Project) {
			return "a valid project is required"
		}
	}
	if req.Action == "delete-sample" && req.Sample != SampleRayCluster && req.Sample != SampleRayJob {
		return "sample must be raycluster or rayjob"
	}
	if req.Action != "setup" && (req.RayCluster || req.RayJob || req.RayImage != "") {
		return "rayCluster, rayJob and rayImage go with setup"
	}
	if req.RayImage != "" {
		if msg := ValidImage(req.RayImage); msg != "" {
			return msg
		}
	}
	return ""
}

type run struct {
	c        *kube.Client
	emit     func(types.UpdateStepEvent)
	logs     []string
	activity string
	target   string
	// Keys listed here are stamped onto cluster-scoped resources produced
	// during the run (see RunAnnotated).
	annotations map[string]string
}

func (r *run) step(id, status, msg string) {
	r.logs = append(r.logs, msg)
	if r.emit != nil {
		r.emit(types.UpdateStepEvent{Step: id, Status: status, Message: msg})
	}
}

func (r *run) fail(id, msg, code, why string) (*types.OperationResponse, error) {
	if r.emit != nil {
		r.emit(types.UpdateStepEvent{Step: id, Status: "failed", Message: msg, ErrorCode: code})
	}
	r.logs = append(r.logs, msg)
	slog.Warn("kueue operation stopped", "step", id, "reason", why, "user", kube.GetUser(r.c))
	record.Action(r.c, r.activity, fmt.Sprintf("%s (%s)", r.target, why), false)
	return &types.OperationResponse{Success: false, Message: msg, Logs: r.logs, ErrorCode: code}, nil
}

func (r *run) done(msg string) (*types.OperationResponse, error) {
	record.Action(r.c, r.activity, r.target, true)
	return &types.OperationResponse{Success: true, Message: msg, Logs: r.logs}, nil
}

func codeOf(err error) string { return kube.FirstNonEmpty(kube.ErrorCodeFromK8sErr(err), "internal") }

// Run drives setup, teardown, and the other Kueue actions in one streamed
// sequence on client c. Step IDs by action: setup — check, queues, project,
// samples; teardown — samples, queues, project, opt-in; delete-sample —
// samples; optin — check, dashboard, profile; optout — opt-in.
func Run(c *kube.Client, req types.KueueRequest, onStep func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
	return RunAnnotated(c, req, nil, onStep)
}

// RunAnnotated matches Run but merges annotations into cluster-scoped
// creates (setup: ResourceFlavor and ClusterQueue; optin: Queue hardware
// profile) so an environment recipe can tag what that invocation built.
// Pre-existing objects are left untouched.
func RunAnnotated(c *kube.Client, req types.KueueRequest, annotations map[string]string, onStep func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
	r := &run{c: c, emit: onStep, activity: req.Action + "-kueue", target: "distributed workloads", annotations: annotations}
	if req.Project != "" {
		r.target += " in " + req.Project
	}
	if msg := Validate(req); msg != "" {
		return r.fail("check", msg, "validation", "invalid request")
	}
	switch req.Action {
	case "setup":
		return r.setup(req)
	case "delete-sample":
		return r.deleteSampleOnly(req.Sample)
	case "optin":
		return r.optIn()
	case "optout":
		return r.optOut()
	}
	return r.teardown()
}

// create POSTs obj to collection under the tool field manager. With dryRun,
// the apiserver evaluates a server-side dry run and returns parsed metadata
// from that response.
func create(c *kube.Client, collection string, obj map[string]any, dryRun bool) (kube.ObjectMeta, error) {
	data, err := json.Marshal(obj)
	if err != nil {
		return kube.ObjectMeta{}, err
	}
	q := url.Values{"fieldManager": {envkit.ToolFieldManager}}
	if dryRun {
		q.Set("dryRun", "All")
	}
	body, _, err := c.Do(http.MethodPost, collection, "application/json", data, q)
	if err != nil {
		return kube.ObjectMeta{}, err
	}
	return kube.ParseObjectMeta(body)
}

// queuePath and queueCollection spell out object path and collection for a
// queue kind so static RBAC analysis can resolve both endpoints.
func queuePath(kind, project string) string {
	switch kind {
	case "ResourceFlavor":
		return rhoai.ClusterPath(kueueAPI, "resourceflavors", FlavorName)
	case "ClusterQueue":
		return rhoai.ClusterPath(kueueAPI, "clusterqueues", ClusterQueueName)
	}
	return rhoai.NamespacedPath(kueueAPI, "localqueues", project, LocalQueueName)
}

func queueCollection(kind, project string) string {
	switch kind {
	case "ResourceFlavor":
		return rhoai.ClusterPath(kueueAPI, "resourceflavors", "")
	case "ClusterQueue":
		return rhoai.ClusterPath(kueueAPI, "clusterqueues", "")
	}
	return rhoai.NamespacedPath(kueueAPI, "localqueues", project, "")
}

// toolObject bundles one queue resource in the order setup creates them.
type toolObject struct {
	kind, name, project string
	manifest            map[string]any
	read                func() (Queue, bool, error)
	uid                 func(r *Record) *string
}

func queueObjects(c *kube.Client, project string) []toolObject {
	return []toolObject{
		{"ResourceFlavor", FlavorName, project, FlavorManifest(),
			func() (Queue, bool, error) { return readFlavor(c) }, func(r *Record) *string { return &r.FlavorUID }},
		{"ClusterQueue", ClusterQueueName, project, ClusterQueueManifest(project),
			func() (Queue, bool, error) { return readClusterQueue(c) }, func(r *Record) *string { return &r.ClusterQueueUID }},
		{"LocalQueue", LocalQueueName, project, LocalQueueManifest(project),
			func() (Queue, bool, error) { return readLocalQueue(c, project) }, func(r *Record) *string { return &r.LocalQueueUID }},
	}
}

func (r *run) setup(req types.KueueRequest) (*types.OperationResponse, error) {
	c, project := r.c, req.Project
	image := kube.FirstNonEmpty(req.RayImage, DefaultRayImage)
	r.step("check", "running", fmt.Sprintf("Checking that Kueue can run workloads in %s", project))
	rec, err := ReadRecord(c)
	if err != nil {
		return r.fail("check", err.Error()+" Nothing was changed.", codeOf(err), "record read failed")
	}
	if rec.Project != "" && rec.Project != project {
		return r.fail("check", fmt.Sprintf("This tool's distributed workloads are set up in project %s; it sets them up in one project only. Clean them up there first. Nothing was changed.", rec.Project), "validation", "other project")
	}
	dsc, err := rhoai.ReadFirstDSC(c)
	if err != nil {
		return r.fail("check", fmt.Sprintf("Cannot read the DataScienceCluster: %v. Nothing was changed.", err), codeOf(err), "dsc read failed")
	}
	ck := checkCluster(c, dsc, envkit.ComponentBlock(dsc, envkit.KueueComponent))
	if ck.refusal != "" {
		return r.fail("check", ck.refusal+" Nothing was changed.", ck.code, "cluster check")
	}
	samples := []string{}
	if req.RayCluster {
		samples = append(samples, SampleRayCluster)
	}
	if req.RayJob {
		samples = append(samples, SampleRayJob)
	}
	if len(samples) > 0 && !envkit.ComponentOn(dsc, "ray") {
		b := envkit.ComponentBlock(dsc, rayComponent)
		why := "The DataScienceCluster's ray component is off"
		if b != nil {
			why = strings.TrimSuffix(b.Reason, ".")
		}
		return r.fail("check", why+"; the samples need it (KubeRay runs them). Set up without samples, or turn ray on first. Nothing was changed.", "prerequisites", "ray off")
	}
	ns, found, err := readNamespace(c, project)
	switch {
	case err != nil:
		return r.fail("check", fmt.Sprintf("Cannot read project %s: %v. Nothing was changed.", project, err), codeOf(err), "project read failed")
	case !found:
		return r.fail("check", fmt.Sprintf("Project %s does not exist. Create it in the dashboard (for example %s), then set up again. Nothing was changed.", project, DefaultProject), "validation", "no project")
	case ns.Metadata.Terminating() || ns.Status.Phase == "Terminating":
		return r.fail("check", fmt.Sprintf("Project %s is being deleted. Nothing was changed.", project), "terminating", "terminating")
	case ns.Metadata.Labels[dashboardLabel] != "true":
		return r.fail("check", fmt.Sprintf("Namespace %s is not a data science project (it lacks the label %s=true), so the dashboard does not show its workloads. Choose a data science project. Nothing was changed.", project, dashboardLabel), "validation", "not a ds project")
	}
	// If the namespace was removed and recreated after we wrote the record,
	// its LocalQueue, samples, and label went with the old UID. Setup then
	// treats the name as a fresh project—objects that share names may belong
	// to others. We only rewrite the record during the queues step, so an
	// early refusal below still leaves the cluster unchanged.
	if rec.ProjectUID != "" && ns.Metadata.UID != rec.ProjectUID {
		rec.forgetProject()
	}
	// Project label policy: missing means we may add it; "true" is acceptable
	// as-is; any other value was chosen deliberately and we never stomp it.
	labelValue, labelled := ns.Metadata.Labels[ProjectLabel]
	if labelled && labelValue != "true" {
		return r.fail("check", fmt.Sprintf("Project %s has the label %s=%s; this tool does not change it. Choose another project, or set it to true in the dashboard. Nothing was changed.", project, ProjectLabel, labelValue), "validation", "label value")
	}
	// When a project already has a LocalQueue named default, Kueue routes new
	// pod-backed work there once labelled—so we reject setup outright, including
	// when the label predates later sample installs.
	qs, err := listLocalQueues(c, project)
	if err != nil {
		return r.fail("check", fmt.Sprintf("Cannot read the LocalQueues of %s: %v. Nothing was changed.", project, err), codeOf(err), "lq read failed")
	}
	for _, q := range qs {
		if q.Metadata.Name == "default" {
			return r.fail("check", fmt.Sprintf("Project %s has a LocalQueue named default: with the label %s=true, Kueue queues every new Deployment, StatefulSet and Pod in it there. Choose another project. Nothing was changed.", project, ProjectLabel), "validation", "default queue")
		}
	}
	// Adopt queues we already own (label and stored UID); block same-name
	// resources owned by something else.
	objs := queueObjects(c, project)
	for i := range objs {
		if objs[i].kind != "LocalQueue" {
			objs[i].manifest = withAnnotations(objs[i].manifest, r.annotations)
		}
	}
	present := map[string]bool{}
	for _, o := range objs {
		q, found, err := o.read()
		if err != nil {
			return r.fail("check", fmt.Sprintf("Cannot read %s %s: %v. Nothing was changed.", o.kind, o.name, err), codeOf(err), "read failed")
		}
		if !found {
			continue
		}
		if !owns(q.Metadata, *o.uid(&rec)) {
			return r.fail("check", fmt.Sprintf("%s %s exists and was not created by this tool, so this tool does not use or change it. Nothing was changed.", o.kind, o.name), "not_managed", "foreign queue")
		}
		if q.Metadata.Terminating() {
			return r.fail("check", fmt.Sprintf("%s %s is being deleted; set up again once it is gone. Nothing was changed.", o.kind, o.name), "terminating", "terminating")
		}
		present[o.kind] = true
	}
	samplePresent := map[string]bool{}
	for _, kind := range samples {
		s, found, err := readSample(c, kind, project)
		if err != nil {
			return r.fail("check", fmt.Sprintf("Cannot read %s: %v. Nothing was changed.", sampleText(kind), err), codeOf(err), "read failed")
		}
		if found && !owns(s.Metadata, rec.SampleUID(kind)) {
			return r.fail("check", fmt.Sprintf("%s exists in %s and was not created by this tool. Nothing was changed.", sampleText(kind), project), "not_managed", "foreign sample")
		}
		samplePresent[kind] = found
	}
	// Server-side dry-run each pending create first. Because LocalQueue
	// admission hinges on ClusterQueue that already exists or will, checking
	// all three in one pass is sound.
	for _, o := range objs {
		if present[o.kind] {
			continue
		}
		if _, err := create(c, queueCollection(o.kind, o.project), o.manifest, true); err != nil {
			return r.fail("check", fmt.Sprintf("The API server rejects %s %s: %v. Nothing was changed.", o.kind, o.name, err), codeOf(err), "dry run failed")
		}
	}
	if !labelled {
		if err := patchLabel(c, project, ns.Metadata.UID, ns.Metadata.ResourceVersion, true, true); err != nil {
			return r.fail("check", fmt.Sprintf("The API server rejects labelling project %s: %v. Nothing was changed.", project, err), codeOf(err), "dry run failed")
		}
	}
	for _, kind := range samples {
		if samplePresent[kind] {
			continue
		}
		if _, err := create(c, sampleCollection(kind, project), sampleManifest(kind, project, image), true); err != nil {
			return r.fail("check", fmt.Sprintf("The API server rejects %s: %v. Nothing was changed.", sampleText(kind), err), codeOf(err), "dry run failed")
		}
	}
	r.step("check", "success", fmt.Sprintf("Kueue is ready; queues for %s in %s.", describeQuota(), project))

	// First phase: materialize queues and write each UID to the record right
	// after a successful create.
	r.step("queues", "running", fmt.Sprintf("Creating ResourceFlavor %s, ClusterQueue %s and LocalQueue %s", FlavorName, ClusterQueueName, LocalQueueName))
	if _, err := updateRecord(c, func(e *Record) {
		if e.ProjectUID != "" && e.ProjectUID != ns.Metadata.UID {
			e.forgetProject()
		}
		e.Project = project
		e.ProjectUID = ns.Metadata.UID
	}); err != nil {
		return r.fail("queues", err.Error()+" Nothing was changed.", codeOf(err), "record write failed")
	}
	for _, o := range objs {
		if present[o.kind] {
			continue
		}
		meta, err := create(c, queueCollection(o.kind, o.project), o.manifest, false)
		if err != nil {
			return r.fail("queues", fmt.Sprintf("Cannot create %s %s: %v. What was created stays; set up again to continue.", o.kind, o.name, err), codeOf(err), "create failed")
		}
		if _, err := updateRecord(c, func(e *Record) { *o.uid(e) = meta.UID }); err != nil {
			if _, delErr := kube.DeleteWithUID(c, queuePath(o.kind, o.project), meta.UID); delErr != nil && !kube.IsK8sNotFound(delErr) {
				return r.fail("queues", fmt.Sprintf("%s %s was created, but this tool could not record it (%v) nor delete it again (%v). Delete it yourself, then set up again.", o.kind, o.name, err, delErr), codeOf(err), "record failed")
			}
			return r.fail("queues", fmt.Sprintf("%s %s was created, but this tool could not record it as its own (%v), so it was deleted again. Set up again to retry.", o.kind, o.name, err), codeOf(err), "record failed")
		}
		r.logs = append(r.logs, fmt.Sprintf("OK: %s %s created", o.kind, o.name))
	}
	r.step("queues", "success", fmt.Sprintf("ClusterQueue %s (%s, project %s only) and LocalQueue %s are there.", ClusterQueueName, describeQuota(), project, LocalQueueName))

	// Second phase: add the project label once ownership is recorded.
	if labelled {
		r.step("project", "success", fmt.Sprintf("Project %s already has the label %s=true.", project, ProjectLabel))
	} else {
		r.step("project", "running", fmt.Sprintf("Labelling project %s %s=true", project, ProjectLabel))
		if _, err := updateRecord(c, func(e *Record) { e.LabelAdded, e.LabelPrior = true, "absent" }); err != nil {
			return r.fail("project", err.Error()+" The project was not labelled.", codeOf(err), "record write failed")
		}
		added, msg, err := addLabel(c, project, ns.Metadata.UID, ns.Metadata.ResourceVersion)
		if err != nil || !added {
			_, _ = updateRecord(c, func(e *Record) { e.LabelAdded, e.LabelPrior = false, "" })
		}
		if err != nil {
			return r.fail("project", fmt.Sprintf("Cannot label project %s: %v. Set up again to retry.", project, err), codeOf(err), "label failed")
		}
		r.step("project", "success", msg)
	}

	// Third phase: optional Ray sample workloads.
	if len(samples) == 0 {
		slog.Info("kueue setup", "project", project, "user", kube.GetUser(c))
		return r.done(fmt.Sprintf("Distributed workloads are set up in %s: LocalQueue %s.", project, LocalQueueName))
	}
	r.step("samples", "running", "Creating the samples")
	var made []string
	for _, kind := range samples {
		if samplePresent[kind] {
			made = append(made, sampleText(kind)+" (already there)")
			continue
		}
		meta, err := create(c, sampleCollection(kind, project), sampleManifest(kind, project, image), false)
		if err != nil {
			return r.fail("samples", fmt.Sprintf("Cannot create %s: %v. The queues stay; set up again to retry.", sampleText(kind), err), codeOf(err), "sample create failed")
		}
		if _, err := updateRecord(c, func(e *Record) { e.setSampleUID(kind, meta.UID) }); err != nil {
			if _, delErr := kube.DeleteWithUID(c, samplePath(kind, project), meta.UID); delErr != nil && !kube.IsK8sNotFound(delErr) {
				return r.fail("samples", fmt.Sprintf("%s was created, but this tool could not record it (%v) nor delete it again (%v). Delete it yourself.", sampleText(kind), err, delErr), codeOf(err), "record failed")
			}
			return r.fail("samples", fmt.Sprintf("%s was created, but this tool could not record it (%v), so it was deleted again. Set up again to retry.", sampleText(kind), err), codeOf(err), "record failed")
		}
		made = append(made, sampleText(kind))
	}
	r.step("samples", "success", fmt.Sprintf("Created %s; Kueue admits %s when the quota allows (pulling the Ray image can take several minutes).", kube.JoinTitles(made), kube.Verb(len(made), "it", "them")))
	slog.Info("kueue setup", "project", project, "samples", len(made), "user", kube.GetUser(c))
	return r.done(fmt.Sprintf("Distributed workloads are set up in %s with %s.", project, kube.JoinTitles(made)))
}

func sampleManifest(kind, project, image string) map[string]any {
	if kind == SampleRayJob {
		return RayJobManifest(project, image)
	}
	return RayClusterManifest(project, image)
}

// patchLabel adds or removes ProjectLabel through JSON Patch. Every patch
// begins by testing metadata.uid so a same-named namespace that was recreated
// is never touched. Removal additionally asserts the label still equals "true".
func labelPointer() string {
	return "/metadata/labels/" + strings.ReplaceAll(strings.ReplaceAll(ProjectLabel, "~", "~0"), "/", "~1")
}

// patchLabel adds (add) or removes the project label with a JSON patch
// that tests the namespace's UID and sets its resourceVersion to the one
// read (the API server answers 409 when the namespace changed since: a
// compare-and-set), so a namespace made again, or changed by someone in
// between, is never changed. A removal also tests that the label still
// reads "true". An absent labels map is added whole.
func patchLabel(c *kube.Client, project, uid, resourceVersion string, add, dryRun bool) error {
	rawUID, _ := json.Marshal(uid)
	rawRV, _ := json.Marshal(resourceVersion)
	ptr := labelPointer()
	ops := []kube.JSONPatchOp{{Op: "test", Path: "/metadata/uid", Value: rawUID}, {Op: "replace", Path: "/metadata/resourceVersion", Value: rawRV}}
	if add {
		ops = append(ops, kube.JSONPatchOp{Op: "add", Path: ptr, Value: json.RawMessage(`"true"`)})
	} else {
		ops = append(ops, kube.JSONPatchOp{Op: "test", Path: ptr, Value: json.RawMessage(`"true"`)}, kube.JSONPatchOp{Op: "remove", Path: ptr})
	}
	body, err := json.Marshal(ops)
	if err != nil {
		return err
	}
	q := url.Values{"fieldManager": {envkit.ToolFieldManager}}
	if dryRun {
		q.Set("dryRun", "All")
	}
	_, _, err = c.Do(http.MethodPatch, "/api/v1/namespaces/"+project, "application/json-patch+json", body, q)
	return err
}

// labelAttempts bounds the compare-and-set retries when the namespace
// changes between the read and the patch.
const labelAttempts = 3

// addLabel applies the label via compare-and-set. After a conflict it re-reads
// the namespace: labels another actor set yield added=false without claiming
// them; non-"true" values error; otherwise it retries up to labelAttempts.
func addLabel(c *kube.Client, project, uid, resourceVersion string) (added bool, msg string, err error) {
	rv := resourceVersion
	for attempt := 1; ; attempt++ {
		err = patchLabel(c, project, uid, rv, true, false)
		if err == nil {
			return true, fmt.Sprintf("Labelled %s: Kueue admits its workloads sent to a queue.", project), nil
		}
		if !kube.IsK8sConflict(err) || attempt == labelAttempts {
			return false, "", err
		}
		ns, found, rerr := readNamespace(c, project)
		switch {
		case rerr != nil:
			return false, "", rerr
		case !found || ns.Metadata.UID != uid:
			return false, "", fmt.Errorf("project %s was deleted or made again meanwhile", project)
		}
		if v, ok := ns.Metadata.Labels[ProjectLabel]; ok {
			if v != "true" {
				return false, "", fmt.Errorf("someone set the label %s=%s meanwhile; this tool does not change it", ProjectLabel, v)
			}
			return false, fmt.Sprintf("Someone else labelled %s %s=true meanwhile; the label is theirs and stays after Clean up.", project, ProjectLabel), nil
		}
		rv = ns.Metadata.ResourceVersion
	}
}

// --- teardown sequence -----------------------------------------------------------

// deleteSample deletes an owned sample (label and stored UID) and waits until
// it disappears; samples we do not own are ignored.
func (r *run) deleteSample(rec Record, kind string) (string, string) {
	c := r.c
	s, found, err := readSample(c, kind, rec.Project)
	if err != nil {
		return fmt.Sprintf("Cannot read %s: %v.", sampleText(kind), err), codeOf(err)
	}
	if !found {
		return "", ""
	}
	if !owns(s.Metadata, rec.SampleUID(kind)) {
		if rec.SampleUID(kind) == "" {
			return "", "" // unowned sample and no UID recorded: leave it
		}
		return fmt.Sprintf("%s in %s is not the one this tool created (it was deleted and made again), so this tool does not delete it.", sampleText(kind), rec.Project), "not_managed"
	}
	if _, err := kube.DeleteWithUID(c, samplePath(kind, rec.Project), s.Metadata.UID); err != nil && !kube.IsK8sNotFound(err) {
		return fmt.Sprintf("Cannot delete %s: %v.", sampleText(kind), err), codeOf(err)
	}
	gone, err := kube.WaitForGone(c, GoneWait, 2*time.Second, func(cc *kube.Client) (bool, error) {
		now, found, err := readSample(cc, kind, rec.Project)
		return !found || now.Metadata.UID != s.Metadata.UID, err
	})
	if err != nil || !gone {
		return fmt.Sprintf("%s was deleted, but it is not gone yet; Clean up again later.", sampleText(kind)), "timeout"
	}
	return "", ""
}

func (r *run) deleteSampleOnly(kind string) (*types.OperationResponse, error) {
	r.step("samples", "running", fmt.Sprintf("Deleting %s", sampleText(kind)))
	rec, err := ReadRecord(r.c)
	if err != nil {
		return r.fail("samples", err.Error()+" Nothing was deleted.", codeOf(err), "record read failed")
	}
	if rec.Project == "" || rec.SampleUID(kind) == "" {
		return r.fail("samples", fmt.Sprintf("This tool has no %s to delete.", sampleText(kind)), "validation", "no sample")
	}
	if msg, code := r.deleteSample(rec, kind); msg != "" {
		return r.fail("samples", msg, code, "sample delete failed")
	}
	if _, err := updateRecord(r.c, func(e *Record) { e.setSampleUID(kind, "") }); err != nil {
		slog.Warn("cannot drop the sample from the record", "error", err)
	}
	r.step("samples", "success", fmt.Sprintf("Deleted %s.", sampleText(kind)))
	return r.done(fmt.Sprintf("Deleted %s in %s.", sampleText(kind), rec.Project))
}

// queueUsers names workloads or queues still referencing queue o; list errors
// bubble up (fail closed).
func queueUsers(c *kube.Client, o toolObject, rec Record) ([]string, error) {
	var users []string
	switch o.kind {
	case "LocalQueue":
		ws, err := listWorkloads(c, rec.Project)
		if err != nil {
			return nil, err
		}
		for _, w := range ws {
			sample := (rec.RayClusterUID != "" && w.Metadata.OwnedBy(rec.RayClusterUID)) || (rec.RayJobUID != "" && w.Metadata.OwnedBy(rec.RayJobUID))
			if w.Spec.QueueName == LocalQueueName && !sample {
				users = append(users, "Workload "+w.Metadata.Name)
			}
		}
	case "ClusterQueue":
		qs, err := listLocalQueues(c, "")
		if err != nil {
			return nil, err
		}
		for _, q := range qs {
			ours := q.Metadata.Namespace == rec.Project && q.Metadata.Name == LocalQueueName
			if q.Spec.ClusterQueue == ClusterQueueName && !ours {
				users = append(users, "LocalQueue "+q.Metadata.Namespace+"/"+q.Metadata.Name)
			}
		}
	case "ResourceFlavor":
		qs, err := listClusterQueues(c)
		if err != nil {
			return nil, err
		}
		for _, q := range qs {
			if q.Metadata.Name != ClusterQueueName && q.usesFlavor(FlavorName) {
				users = append(users, "ClusterQueue "+q.Metadata.Name)
			}
		}
	}
	return users, nil
}

// teardown tears down owned resources in order: samples, then LocalQueue,
// ClusterQueue, and ResourceFlavor (only ours and unused), then the project
// label when we added it and no Workloads remain, then dashboard opt-in
// state. Operator installs and the Kueue CR stay; DSC kueue disable runs in
// Clean up's components step.
func (r *run) teardown() (*types.OperationResponse, error) {
	c := r.c
	r.step("samples", "running", "Deleting the samples")
	rec, err := ReadRecord(c)
	if err != nil {
		return r.fail("samples", err.Error()+" Nothing was deleted.", codeOf(err), "record read failed")
	}
	if rec.Project != "" {
		r.target = "distributed workloads in " + rec.Project
	}
	if rec.Empty() {
		if rec.Project != "" {
			if err := writeRecord(c, Record{}); err != nil {
				return r.fail("samples", err.Error(), codeOf(err), "record write failed")
			}
		}
		r.step("samples", "success", "This tool has set up nothing.")
		return r.done("This tool has no distributed workloads set up; nothing was deleted.")
	}
	for _, kind := range []string{SampleRayCluster, SampleRayJob} {
		if rec.Project == "" || rec.SampleUID(kind) == "" {
			continue
		}
		if msg, code := r.deleteSample(rec, kind); msg != "" {
			return r.fail("samples", msg+" Nothing else was deleted.", code, "sample delete failed")
		}
		if rec, err = updateRecord(c, func(e *Record) { e.setSampleUID(kind, "") }); err != nil {
			return r.fail("samples", err.Error(), codeOf(err), "record write failed")
		}
	}
	r.step("samples", "success", "No sample of this tool is left.")

	r.step("queues", "running", fmt.Sprintf("Deleting LocalQueue %s, ClusterQueue %s and ResourceFlavor %s", LocalQueueName, ClusterQueueName, FlavorName))
	objs := queueObjects(c, rec.Project)
	for i := len(objs) - 1; i >= 0; i-- {
		o := objs[i]
		uid := *o.uid(&rec)
		if uid == "" || (o.kind == "LocalQueue" && rec.Project == "") {
			continue
		}
		q, found, err := o.read()
		if err != nil {
			return r.fail("queues", fmt.Sprintf("Cannot read %s %s: %v.", o.kind, o.name, err), codeOf(err), "read failed")
		}
		if found && !owns(q.Metadata, uid) {
			return r.fail("queues", fmt.Sprintf("%s %s is not the one this tool created (it was deleted and made again), so this tool does not delete it.", o.kind, o.name), "not_managed", "foreign queue")
		}
		if found {
			users, err := queueUsers(c, o, rec)
			if err != nil {
				return r.fail("queues", fmt.Sprintf("Cannot tell what uses %s %s, so it stays: %v.", o.kind, o.name, err), codeOf(err), "users read failed")
			}
			if len(users) > 0 {
				return r.fail("queues", fmt.Sprintf("%s %s is used by %s, so it stays; remove %s first.", o.kind, o.name, kube.JoinTitles(users), kube.Verb(len(users), "it", "them")), "in_use", "queue in use")
			}
			if _, err := kube.DeleteWithUID(c, queuePath(o.kind, o.project), uid); err != nil && !kube.IsK8sNotFound(err) {
				return r.fail("queues", fmt.Sprintf("Cannot delete %s %s: %v.", o.kind, o.name, err), codeOf(err), "delete failed")
			}
			// Kueue may keep finalizer kueue.x-k8s.io/resource-in-use on
			// ClusterQueue or ResourceFlavor while referenced; we block before
			// deleting the next queue until it drops.
			gone, err := kube.WaitForGone(c, GoneWait, 2*time.Second, func(cc *kube.Client) (bool, error) {
				now, found, err := o.read()
				return !found || now.Metadata.UID != uid, err
			})
			if err != nil || !gone {
				return r.fail("queues", fmt.Sprintf("%s %s was deleted, but it is not gone yet (Kueue keeps it while something uses it). Clean up again later.", o.kind, o.name), "timeout", "not gone")
			}
		}
		if rec, err = updateRecord(c, func(e *Record) { *o.uid(e) = "" }); err != nil {
			return r.fail("queues", err.Error(), codeOf(err), "record write failed")
		}
		r.logs = append(r.logs, fmt.Sprintf("OK: %s %s deleted", o.kind, o.name))
	}
	r.step("queues", "success", "No queue of this tool is left.")

	var kept []string
	r.step("project", "running", "Checking the project label")
	switch {
	case !rec.LabelAdded:
		r.step("project", "success", "This tool did not add the project's Kueue label; it stays as it is.")
	default:
		msg, err := r.removeLabel(rec)
		if err != nil {
			return r.fail("project", msg, codeOf(err), "label removal failed")
		}
		if strings.HasPrefix(msg, "Kept") {
			kept = append(kept, msg)
		} else if rec, err = updateRecord(c, func(e *Record) { e.LabelAdded = false }); err != nil {
			return r.fail("project", err.Error(), codeOf(err), "record write failed")
		}
		r.step("project", "success", msg)
	}

	r.step("opt-in", "running", "Turning back the dashboard opt-in")
	msg, code := r.restoreOptIn()
	if code != "" {
		return r.fail("opt-in", msg, code, "opt-in restore failed")
	}
	r.step("opt-in", "success", msg)
	if strings.HasPrefix(msg, "Kept") {
		kept = append(kept, msg)
	}
	// When no owned state remains, clear project fields from the record.
	if final, err := ReadRecord(c); err == nil && final.Empty() {
		if err := writeRecord(c, Record{}); err != nil {
			slog.Warn("cannot clear the Kueue record", "error", err)
		}
	}
	done := "Deleted this tool's distributed workloads setup."
	if len(kept) > 0 {
		done += " " + strings.Join(kept, " ")
	}
	slog.Info("kueue teardown", "user", kube.GetUser(c))
	return r.done(done)
}

// removeLabel takes off the Kueue project label we added when the namespace
// UID still matches the record and no active Workloads remain. A "Kept"
// prefix in the return means the label remains on the cluster.
func (r *run) removeLabel(rec Record) (string, error) {
	c := r.c
	ns, found, err := readNamespace(c, rec.Project)
	if err != nil {
		return fmt.Sprintf("Cannot read project %s: %v.", rec.Project, err), err
	}
	if !found || ns.Metadata.UID != rec.ProjectUID || ns.Metadata.Labels[ProjectLabel] != "true" {
		return fmt.Sprintf("Project %s no longer has the label this tool added.", rec.Project), nil
	}
	ws, err := listWorkloads(c, rec.Project)
	if err != nil && !kube.IsK8sNotFound(err) {
		return fmt.Sprintf("Cannot tell whether Kueue still manages workloads in %s, so its label stays: %v.", rec.Project, err), err
	}
	var live []string
	// A Workload being deleted still counts until it is gone.
	for _, w := range ws {
		live = append(live, w.Metadata.Name)
	}
	if len(live) > 0 {
		return fmt.Sprintf("Kept the label %s=true on %s: Kueue still manages %s there (Workload %s).", ProjectLabel, rec.Project, kube.Verb(len(live), "a workload", "workloads"), strings.Join(live, ", ")), nil
	}
	rv := ns.Metadata.ResourceVersion
	for attempt := 1; ; attempt++ {
		err := patchLabel(c, rec.Project, rec.ProjectUID, rv, false, false)
		if err == nil {
			break
		}
		if !kube.IsK8sConflict(err) || attempt == labelAttempts {
			return fmt.Sprintf("Cannot remove the label %s from %s: %v.", ProjectLabel, rec.Project, err), err
		}
		// Changed meanwhile: read again; a label no longer the tool's
		// value stays.
		now, found, rerr := readNamespace(c, rec.Project)
		if rerr != nil {
			return fmt.Sprintf("Cannot read project %s: %v.", rec.Project, rerr), rerr
		}
		if !found || now.Metadata.UID != rec.ProjectUID || now.Metadata.Labels[ProjectLabel] != "true" {
			return fmt.Sprintf("Project %s no longer has the label this tool added.", rec.Project), nil
		}
		rv = now.Metadata.ResourceVersion
	}
	return fmt.Sprintf("Removed the label %s from %s.", ProjectLabel, rec.Project), nil
}

// --- dashboard opt-in ------------------------------------------------------------

func configPath(apps string) string {
	return rhoai.NamespacedPath("opendatahub.io/v1alpha", "odhdashboardconfigs", apps, dashboardConfig)
}

// patchDisableKueue updates disableKueue through merge patch (nil clears the
// field) using the dashboard field manager and the given resourceVersion.
func patchDisableKueue(c *kube.Client, apps, resourceVersion string, value *bool, dryRun bool) error {
	var v any
	if value != nil {
		v = *value
	}
	body, _ := json.Marshal(map[string]any{
		"metadata": map[string]any{"resourceVersion": resourceVersion},
		"spec":     map[string]any{"dashboardConfig": map[string]any{"disableKueue": v}},
	})
	q := url.Values{"fieldManager": {dashboardManager}}
	if dryRun {
		q.Set("dryRun", "All")
	}
	_, _, err := c.Do(http.MethodPatch, configPath(apps), "application/merge-patch+json", body, q)
	return err
}

func previousText(v *bool) string {
	if v == nil {
		return "unset"
	}
	return fmt.Sprint(*v)
}

// optIn clears disableKueue so new projects get kueue.openshift.io/managed=true
// and dashboard Queue hardware profiles for workbenches/models in Kueue
// projects, and it creates the profile targeting this tool's LocalQueue; both
// changes are stored on the record.
func (r *run) optIn() (*types.OperationResponse, error) {
	c := r.c
	r.step("check", "running", "Checking the dashboard's settings")
	rec, err := ReadRecord(c)
	if err != nil {
		return r.fail("check", err.Error()+" Nothing was changed.", codeOf(err), "record read failed")
	}
	if rec.LocalQueueUID == "" || rec.Project == "" {
		return r.fail("check", "Set up the queues first: the hardware profile sends workloads to the tool's LocalQueue. Nothing was changed.", "validation", "not set up")
	}
	r.target = "distributed workloads in " + rec.Project
	if msg, code := r.liveSetupProblem(rec); msg != "" {
		return r.fail("check", msg+" The dashboard's settings were not changed.", code, "setup check")
	}
	apps, err := rhoai.AppsNamespace(c)
	if err != nil {
		return r.fail("check", fmt.Sprintf("Cannot find RHOAI's applications namespace: %v. Nothing was changed.", err), "prerequisites", "no apps ns")
	}
	cfg, found, err := readDashboardConfig(c, apps)
	if err != nil {
		return r.fail("check", fmt.Sprintf("Cannot read OdhDashboardConfig %s/%s: %v. Nothing was changed.", apps, dashboardConfig, err), codeOf(err), "config read failed")
	}
	if !found {
		return r.fail("check", fmt.Sprintf("OdhDashboardConfig %s/%s does not exist, so there is no dashboard switch to turn on. Nothing was changed.", apps, dashboardConfig), "prerequisites", "no config")
	}
	meta, pFound, err := readProfile(c, apps)
	if err != nil {
		return r.fail("check", fmt.Sprintf("Cannot read hardware profile %s: %v. Nothing was changed.", ProfileName, err), codeOf(err), "profile read failed")
	}
	o := rec.OptIn
	if o == nil {
		o = &OptIn{}
	}
	if pFound && !owns(meta, o.ProfileUID) {
		return r.fail("check", fmt.Sprintf("Hardware profile %s/%s exists and was not created by this tool. Nothing was changed.", apps, ProfileName), "not_managed", "foreign profile")
	}
	turnOn := cfg.Spec.DashboardConfig.DisableKueue == nil || *cfg.Spec.DashboardConfig.DisableKueue
	off := false
	if turnOn {
		if err := patchDisableKueue(c, apps, cfg.Metadata.ResourceVersion, &off, true); err != nil {
			return r.fail("check", fmt.Sprintf("The API server rejects setting disableKueue to false: %v. Nothing was changed.", err), codeOf(err), "dry run failed")
		}
	}
	profile := withAnnotations(ProfileManifest(apps, kube.NowRFC3339()), r.annotations)
	if !pFound {
		if _, err := create(c, rhoai.NamespacedPath(profileAPI, "hardwareprofiles", apps, ""), profile, true); err != nil {
			return r.fail("check", fmt.Sprintf("The API server rejects hardware profile %s: %v. Nothing was changed.", ProfileName, err), codeOf(err), "dry run failed")
		}
	}
	r.step("check", "success", "Both changes are checked.")

	if !turnOn {
		r.step("dashboard", "success", "The dashboard's Kueue switch is on already (disableKueue: false); it stays as it is.")
	} else {
		r.step("dashboard", "running", "Setting disableKueue to false in the dashboard's settings")
		if _, err := updateRecord(c, func(e *Record) {
			if e.OptIn == nil {
				e.OptIn = &OptIn{}
			}
			e.OptIn.DisableKueueSet, e.OptIn.Previous, e.OptIn.ConfigNamespace = true, previousText(cfg.Spec.DashboardConfig.DisableKueue), apps
		}); err != nil {
			return r.fail("dashboard", err.Error()+" Nothing was changed.", codeOf(err), "record write failed")
		}
		if err := patchDisableKueue(c, apps, cfg.Metadata.ResourceVersion, &off, false); err != nil {
			_, _ = updateRecord(c, func(e *Record) { e.OptIn.DisableKueueSet, e.OptIn.Previous = false, "" })
			return r.fail("dashboard", fmt.Sprintf("Cannot set disableKueue to false: %v. Nothing was changed.", err), codeOf(err), "config patch failed")
		}
		r.step("dashboard", "success", fmt.Sprintf("disableKueue is false (it was %s): new dashboard projects are Kueue projects.", previousText(cfg.Spec.DashboardConfig.DisableKueue)))
	}
	if pFound {
		r.step("profile", "success", fmt.Sprintf("Hardware profile %s is there.", ProfileName))
	} else {
		r.step("profile", "running", fmt.Sprintf("Creating hardware profile %s", ProfileName))
		pm, err := create(c, rhoai.NamespacedPath(profileAPI, "hardwareprofiles", apps, ""), profile, false)
		if err != nil {
			return r.fail("profile", fmt.Sprintf("Cannot create hardware profile %s: %v. The dashboard switch stays; turn the opt-in off to restore it.", ProfileName, err), codeOf(err), "profile create failed")
		}
		if _, err := updateRecord(c, func(e *Record) {
			if e.OptIn == nil {
				e.OptIn = &OptIn{}
			}
			e.OptIn.ProfileUID, e.OptIn.ProfileNamespace, e.OptIn.ProfileResourceVersion = pm.UID, apps, pm.ResourceVersion
		}); err != nil {
			if _, delErr := kube.DeleteWithUID(c, profilePath(apps), pm.UID); delErr != nil && !kube.IsK8sNotFound(delErr) {
				return r.fail("profile", fmt.Sprintf("Hardware profile %s was created, but this tool could not record it (%v) nor delete it again (%v). Delete it in the dashboard.", ProfileName, err, delErr), codeOf(err), "record failed")
			}
			return r.fail("profile", fmt.Sprintf("Hardware profile %s was created, but this tool could not record it (%v), so it was deleted again.", ProfileName, err), codeOf(err), "record failed")
		}
		r.step("profile", "success", fmt.Sprintf("Created hardware profile %s for LocalQueue %s.", ProfileName, LocalQueueName))
	}
	return r.done("The dashboard's Kueue opt-in is on.")
}

// liveSetupProblem checks, right before the opt-in changes the dashboard,
// that the recorded setup is still there as recorded: Kueue ready, the
// project the recorded one (its UID) with the label, the ClusterQueue and
// the LocalQueue the tool's (label and recorded UID) and not being deleted.
// It returns why not, and an error code, or "".
func (r *run) liveSetupProblem(rec Record) (string, string) {
	c := r.c
	dsc, err := rhoai.ReadFirstDSC(c)
	if err != nil {
		return fmt.Sprintf("Cannot read the DataScienceCluster: %v.", err), codeOf(err)
	}
	if ck := checkCluster(c, dsc, envkit.ComponentBlock(dsc, envkit.KueueComponent)); ck.refusal != "" {
		return ck.refusal, ck.code
	}
	ns, found, err := readNamespace(c, rec.Project)
	switch {
	case err != nil:
		return fmt.Sprintf("Cannot read project %s: %v.", rec.Project, err), codeOf(err)
	case !found || ns.Metadata.UID != rec.ProjectUID || ns.Metadata.Terminating():
		return fmt.Sprintf("Project %s is gone, being deleted or was made again since this tool's setup. Set up again.", rec.Project), "validation"
	case ns.Metadata.Labels[ProjectLabel] != "true":
		return fmt.Sprintf("Project %s no longer has the label %s=true. Set up again first.", rec.Project, ProjectLabel), "validation"
	}
	for _, o := range queueObjects(c, rec.Project)[1:] {
		q, found, err := o.read()
		switch {
		case err != nil:
			return fmt.Sprintf("Cannot read %s %s: %v.", o.kind, o.name, err), codeOf(err)
		case !found || !owns(q.Metadata, *o.uid(&rec)) || q.Metadata.Terminating():
			return fmt.Sprintf("%s %s is not the one this tool set up (gone, made again or being deleted). Set up again first.", o.kind, o.name), "not_managed"
		}
	}
	return "", ""
}

func (r *run) optOut() (*types.OperationResponse, error) {
	r.step("opt-in", "running", "Turning back the dashboard opt-in")
	msg, code := r.restoreOptIn()
	if code != "" {
		return r.fail("opt-in", msg, code, "opt-in restore failed")
	}
	r.step("opt-in", "success", msg)
	return r.done(msg)
}

// restoreOptIn removes the recorded hardware profile when UID and
// resourceVersion still match, and reverts disableKueue to the pre–opt-in
// value while the tool still owns the false field. Externally modified
// resources are left as-is (reported with "Kept"). Non-empty code signals error.
func (r *run) restoreOptIn() (string, string) {
	c := r.c
	rec, err := ReadRecord(c)
	if err != nil {
		return err.Error(), codeOf(err)
	}
	o := rec.OptIn
	if o.empty() {
		return "The dashboard opt-in was not made by this tool; nothing to turn back.", ""
	}
	var kept, done []string
	if o.ProfileUID != "" {
		meta, found, err := readProfile(c, o.ProfileNamespace)
		switch {
		case err != nil:
			return fmt.Sprintf("Cannot read hardware profile %s: %v.", ProfileName, err), codeOf(err)
		case !found || meta.UID != o.ProfileUID:
			done = append(done, "hardware profile "+ProfileName+" is gone already")
		default:
			if _, err := kube.DeleteWithPreconditions(c, profilePath(o.ProfileNamespace), o.ProfileUID, o.ProfileResourceVersion); err != nil {
				if !kube.IsK8sConflict(err) {
					return fmt.Sprintf("Cannot delete hardware profile %s: %v.", ProfileName, err), codeOf(err)
				}
				kept = append(kept, fmt.Sprintf("hardware profile %s (someone changed it since this tool created it)", ProfileName))
			} else {
				done = append(done, "deleted hardware profile "+ProfileName)
			}
		}
		if rec, err = updateRecord(c, func(e *Record) {
			e.OptIn.ProfileUID, e.OptIn.ProfileNamespace, e.OptIn.ProfileResourceVersion = "", "", ""
		}); err != nil {
			return err.Error(), codeOf(err)
		}
		o = rec.OptIn
	}
	if o != nil && o.DisableKueueSet {
		cfg, found, err := readDashboardConfig(c, o.ConfigNamespace)
		switch {
		case err != nil:
			return fmt.Sprintf("Cannot read OdhDashboardConfig: %v.", err), codeOf(err)
		case !found || !configOwned(cfg):
			kept = append(kept, "the dashboard's disableKueue (someone changed it since this tool's opt-in)")
		default:
			var prev *bool
			if o.Previous == "true" {
				t := true
				prev = &t
			}
			if err := patchDisableKueue(c, o.ConfigNamespace, cfg.Metadata.ResourceVersion, prev, false); err != nil {
				if kube.IsK8sConflict(err) {
					return "The dashboard's settings changed while the opt-in was turned back; Clean up again.", "conflict"
				}
				return fmt.Sprintf("Cannot turn disableKueue back: %v.", err), codeOf(err)
			}
			done = append(done, "disableKueue is "+o.Previous+" again")
		}
		if _, err := updateRecord(c, func(e *Record) { e.OptIn = nil }); err != nil {
			return err.Error(), codeOf(err)
		}
	}
	msg := ""
	if len(done) > 0 {
		msg = strings.ToUpper(done[0][:1]) + strings.Join(done, "; ")[1:] + "."
	}
	if len(kept) > 0 {
		msg = strings.TrimSpace(msg + " Kept " + strings.Join(kept, " and ") + ".")
		if len(done) == 0 {
			msg = "Kept " + strings.Join(kept, " and ") + "."
		}
	}
	return msg, ""
}

// Teardown is the Kueue slice of Clean up — same path as Run with action teardown.
func Teardown(c *kube.Client) (*types.OperationResponse, error) {
	return Run(c, types.KueueRequest{Action: "teardown"}, nil)
}

// Users returns human-readable names of remaining ClusterQueues and LocalQueues
// (any owner). Non-empty output keeps DSC kueue enabled during Clean up
// (components). List failures error out and the caller treats kueue as still on.
func Users(c *kube.Client) ([]string, error) {
	if _, group, err := v1beta2Served(c); err != nil || !group {
		return nil, err
	}
	var out []string
	cqs, err := listClusterQueues(c)
	if err != nil {
		return nil, fmt.Errorf("list ClusterQueues: %w", err)
	}
	// Queues being deleted count until they are gone.
	for _, q := range cqs {
		out = append(out, "ClusterQueue "+q.Metadata.Name)
	}
	lqs, err := listLocalQueues(c, "")
	if err != nil {
		return nil, fmt.Errorf("list LocalQueues: %w", err)
	}
	for _, q := range lqs {
		out = append(out, "LocalQueue "+q.Metadata.Namespace+"/"+q.Metadata.Name)
	}
	return out, nil
}

// withAnnotations adds annotations to manifest's metadata (a copy of the
// metadata map; the manifest is the caller's own).
func withAnnotations(manifest map[string]any, annotations map[string]string) map[string]any {
	if len(annotations) == 0 {
		return manifest
	}
	meta, _ := manifest["metadata"].(map[string]any)
	m := map[string]any{}
	for k, v := range meta {
		m[k] = v
	}
	ann := map[string]any{}
	if old, ok := m["annotations"].(map[string]any); ok {
		for k, v := range old {
			ann[k] = v
		}
	}
	for k, v := range annotations {
		ann[k] = v
	}
	m["annotations"] = ann
	manifest["metadata"] = m
	return manifest
}

// ReadClusterQueueMeta reads the metadata of this tool's ClusterQueue
// with u, the signed-in user's own client.
func ReadClusterQueueMeta(u *kube.Client) (kube.ObjectMeta, bool, error) {
	q, found, err := readClusterQueue(u)
	return q.Metadata, found, err
}

// ReadProfileMeta reads the metadata of this tool's Queue hardware profile
// in ns with u, the signed-in user's own client.
func ReadProfileMeta(u *kube.Client, ns string) (kube.ObjectMeta, bool, error) {
	return readProfile(u, ns)
}

// Owns reports whether meta is the object of uid this tool created (its
// label and that UID).
func Owns(meta kube.ObjectMeta, uid string) bool { return owns(meta, uid) }
