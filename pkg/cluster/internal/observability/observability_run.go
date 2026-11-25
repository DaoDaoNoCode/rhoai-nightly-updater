package observability

import (
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/argo"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/serving"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/storage"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Step IDs (unique among the MaaS setup's, which runs after them when MaaS
// is set up with usage logs).
const (
	StepCheck    = "observability-check"
	StepStorage  = "usage-logs-storage"
	StepSettings = "monitoring-settings"
	StepReady    = "monitoring-ready"
	StepGC       = "monitoring-gc"
	StepLeft     = "monitoring-leftovers"
)

// secretAnnotation is on the usage-logs Secret: what it is for.
const secretAnnotation = "rhoai-nightly-updater/usage-logs"

// ValidateRequest returns why req cannot run, or "".
func ValidateRequest(req types.ObservabilityRequest) string {
	if req.Action != "setup" && req.Action != "teardown" {
		return "action must be setup or teardown"
	}
	return ""
}

// Run sets up ("observability-check", the operators' steps,
// "usage-logs-storage", "monitoring-settings", "monitoring-ready") or
// cleans up ("observability-check", "monitoring-settings", "monitoring-gc",
// "monitoring-leftovers", "usage-logs-storage") the usage logs and the
// dashboards, as one operation with streamed steps.
func Run(c *kube.Client, req types.ObservabilityRequest, onStep func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
	m := &serving.Run{Client: c, Emit: onStep, Activity: req.Action + "-observability", Target: "usage logs and dashboards"}
	if problem := ValidateRequest(req); problem != "" {
		return m.Fail(StepCheck, problem, "validation", "invalid request")
	}
	if req.Action == "teardown" {
		return teardown(m)
	}
	if r, err := SetUp(m); r != nil || err != nil {
		return r, err
	}
	slog.Info("observability set up", "user", kube.GetUser(c))
	return m.Done("Usage logs and the dashboards are set up. New calls through the MaaS gateway show in the usage dashboards (RHOAI dashboard, Observe & monitor) once they are made; older calls were not logged.")
}

// SetUp runs Set up's steps on m; it returns a response only when it
// stopped (MaaS's setup goes on after it otherwise).
func SetUp(m *serving.Run) (*types.OperationResponse, error) {
	c := m.Client
	m.Step(StepCheck, "running", "Checking what usage logs and the dashboards need on the cluster")
	v := read(c)
	if v.refusal != "" {
		return m.Fail(StepCheck, v.refusal, kube.FirstNonEmpty(v.code, "prerequisites"), "check failed")
	}
	storageClass := v.storageClass
	// Every write as a server-side dry run first.
	if err := v.ops.DryRun(c); err != nil {
		return m.Fail(StepCheck, fmt.Sprintf("%v. Nothing was changed.%s", err, kube.TemplateHint(err)), serving.MaaSWriteCode(err), "dry run failed")
	}
	var secretMeta kube.ObjectMeta
	var secretFound bool
	var current storage.S3Keys
	_, logsByTool := v.rec.Added[fieldUsageLogs]
	storageByTool := logsByTool || v.rec.Storage
	if v.addUsageLogs || storageByTool {
		var err error
		current, secretMeta, secretFound, err = storage.UsageLogsSecretKeys(c, v.ns)
		if err != nil {
			return m.Fail(StepCheck, fmt.Sprintf("Cannot read Secret %s/%s; nothing was changed: %v.%s", v.ns, storage.UsageLogsSecret, err, kube.TemplateHint(err)), kube.ErrorCodeFromK8sErr(err), "check failed")
		}
		if secretFound && !serving.ToolOwned(secretMeta) {
			return m.Fail(StepCheck, fmt.Sprintf("Secret %s/%s exists and was not created by this tool, so this tool does not change it. Nothing was changed.", v.ns, storage.UsageLogsSecret), "not_managed", "foreign secret")
		}
		if _, _, _, err := writeSecret(c, v.ns, storage.S3Keys{AccessKey: "dry-run", SecretKey: "dry-run"}, secretMeta, secretFound, true); err != nil {
			return m.Fail(StepCheck, fmt.Sprintf("The API server rejects Secret %s/%s: %v. Nothing was changed.", v.ns, storage.UsageLogsSecret, err), serving.MaaSWriteCode(err), "dry run failed")
		}
	}
	body, _ := settingsPatch(v, storageClass, nil)
	if body != nil {
		if _, err := patchDSCI(c, v.d, body, true); err != nil {
			return m.Fail(StepCheck, fmt.Sprintf("The API server rejects the monitoring fields in DSCInitialization %s: %v. Nothing was changed.%s", v.d.Meta.Name, err, kube.TemplateHint(err)), serving.MaaSWriteCode(err), "dry run failed")
		}
	}
	m.Step(StepCheck, "success", "Everything Set up changes is checked.")

	// 1. The operators.
	if r, err := v.ops.Install(m); r != nil || err != nil {
		return r, err
	}
	// 2. The usage-logs storage, after a record that it may exist (so Clean
	// up sees a setup that stops before the fields are added).
	if v.addUsageLogs || storageByTool {
		if !v.recOK || !v.rec.Storage {
			intent := v.rec
			intent.Storage = true
			if intent.At == "" {
				intent.At = kube.NowRFC3339()
			}
			if intent.Added == nil {
				intent.Added = map[string]AddedField{}
			}
			answer, err := patchDSCI(c, v.d, map[string]any{"metadata": map[string]any{"annotations": map[string]any{RecordAnnotation: recordJSON(intent)}}}, false)
			if err != nil {
				return m.Fail(StepStorage, fmt.Sprintf("Cannot record on DSCInitialization %s that this tool makes the usage-logs storage, so it made none of it: %v.%s", v.d.Meta.Name, err, kube.TemplateHint(err)), serving.MaaSWriteCode(err), "record failed")
			}
			v.d, v.rec, v.recOK = answer, intent, true
		}
		m.Step(StepStorage, "running", fmt.Sprintf("Making bucket %s and SeaweedFS user %s on this tool's S3 storage, and Secret %s/%s", storage.UsageLogsBucket, storage.UsageLogsUser, v.ns, storage.UsageLogsSecret))
		keys, fresh, err := storage.ProvisionUsageLogsKey(c, current)
		if err != nil {
			return m.Fail(StepStorage, fmt.Sprintf("%v. Set up again to continue.", err), "s3_keys", "usage-logs key failed")
		}
		if fresh || !secretFound {
			if _, _, _, err := writeSecret(c, v.ns, keys, secretMeta, secretFound, false); err != nil {
				return m.Fail(StepStorage, fmt.Sprintf("Failed to write Secret %s/%s: %v. Set up again to continue.", v.ns, storage.UsageLogsSecret, err), serving.MaaSWriteCode(err), "secret write failed")
			}
			m.Logs = append(m.Logs, fmt.Sprintf("OK: Secret %s/%s written", v.ns, storage.UsageLogsSecret))
		}
		m.Step(StepStorage, "success", fmt.Sprintf("Bucket %s, SeaweedFS user %s (its key reaches that bucket only) and Secret %s/%s are ready.", storage.UsageLogsBucket, storage.UsageLogsUser, v.ns, storage.UsageLogsSecret))
	}
	// 3. The DSCInitialization.
	d := v.d
	if body != nil {
		m.Step(StepSettings, "running", fmt.Sprintf("Adding the monitoring fields to DSCInitialization %s", d.Meta.Name))
		before, err := names(c, v.ns)
		if err != nil {
			return m.Fail(StepSettings, fmt.Sprintf("Cannot list what namespace %s has before the change; the DSCInitialization was not changed: %v.%s", v.ns, err, kube.TemplateHint(err)), kube.ErrorCodeFromK8sErr(err), "list failed")
		}
		body, rec := settingsPatch(v, storageClass, &before)
		answer, err := patchDSCI(c, d, body, false)
		if err != nil {
			return m.Fail(StepSettings, fmt.Sprintf("Failed to change DSCInitialization %s: %v. Set up again to continue.%s", d.Meta.Name, err, kube.TemplateHint(err)), serving.MaaSWriteCode(err), "dsci patch failed")
		}
		d, v.rec, v.recOK = answer, rec, true
		m.Logs = append(m.Logs, "OK: DSCInitialization "+d.Meta.Name+" spec.monitoring changed")
		m.Step(StepSettings, "success", fmt.Sprintf("DSCInitialization %s sets usage logs and metrics storage; RHOAI's operator deploys the stack.", d.Meta.Name))
	} else {
		m.Step(StepSettings, "success", fmt.Sprintf("DSCInitialization %s sets usage logs and metrics storage already.", d.Meta.Name))
	}
	// 4. Ready, then what the stack made is recorded for Clean up: only in
	// the run that added the fields (a later Set up records nothing: what
	// appears after it is not provably this change's).
	r, err := waitReady(m)
	if body != nil {
		if lerr := recordLeft(c, v.ns); lerr != nil {
			m.Logs = append(m.Logs, "Could not record the disks the stack made (Clean up then lists them instead of deleting them): "+lerr.Error())
		}
	}
	return r, err
}

// settingsPatch is the DSCInitialization merge patch that adds the missing
// fields with the record that names them (nil when nothing is missing).
func settingsPatch(v *view, storageClass string, before *Names) (map[string]any, Record) {
	if !v.addMetrics && !v.addUsageLogs {
		return nil, v.rec
	}
	rec := Record{At: kube.NowRFC3339(), Storage: v.rec.Storage || v.addUsageLogs, Added: map[string]AddedField{}, Before: before}
	for name, f := range v.rec.Added {
		rec.Added[name] = f
	}
	monitoring := map[string]any{}
	if v.addMetrics {
		set := metricsStorageValue()
		monitoring["metrics"] = map[string]any{"storage": set}
		rec.Added[fieldMetricsStorage] = AddedField{Previous: rawJSON(nil), Set: rawJSON(set)}
	}
	if v.addUsageLogs {
		set := usageLogsValue(storageClass)
		monitoring["usageLogs"] = set
		rec.Added[fieldUsageLogs] = AddedField{Previous: rawJSON(nil), Set: rawJSON(set)}
	}
	return map[string]any{
		"metadata": map[string]any{"annotations": map[string]any{RecordAnnotation: recordJSON(rec)}},
		"spec":     map[string]any{"monitoring": monitoring},
	}, rec
}

// defaultStorageClass is the cluster's default StorageClass ("" none).
// With several, Kubernetes uses the newest (Kubernetes docs, Storage
// Classes, "Default StorageClass"), and so does this.
func defaultStorageClass(c *kube.Client) (string, error) {
	var items []struct {
		Metadata kube.ObjectMeta `json:"metadata"`
	}
	if err := c.ListInto(rhoai.ClusterPath("storage.k8s.io/v1", "storageclasses", ""), &items); err != nil {
		return "", err
	}
	name, newest := "", ""
	for _, it := range items {
		a := it.Metadata.Annotations
		if a[defaultClassAnnotation] != "true" && a[betaDefaultClassAnnotation] != "true" {
			continue
		}
		// The newest; among equally new ones, the first by name
		// (Kubernetes' storage admission, pkg/volume/util GetDefaultClass).
		ts := it.Metadata.CreationTimestamp
		if name == "" || ts > newest || (ts == newest && it.Metadata.Name < name) {
			name, newest = it.Metadata.Name, ts
		}
	}
	return name, nil
}

// writeSecret creates or updates this tool's usage-logs Secret.
func writeSecret(c *kube.Client, ns string, keys storage.S3Keys, current kube.ObjectMeta, found, dryRun bool) (string, kube.ObjectMeta, bool, error) {
	data := map[string]any{}
	for k, val := range storage.UsageLogsSecretData(keys) {
		data[k] = val
	}
	obj := map[string]any{
		"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
		"metadata": map[string]any{"name": storage.UsageLogsSecret, "namespace": ns, "labels": envkit.ToolLabels(),
			"annotations": map[string]any{secretAnnotation: fmt.Sprintf("S3 key of SeaweedFS user %s for bucket %s, for RHOAI's usage logs (the Observability tab)", storage.UsageLogsUser, storage.UsageLogsBucket)}},
		"stringData": data,
	}
	path := rhoai.NamespacedPath("v1", "secrets", ns, storage.UsageLogsSecret)
	return envkit.EnsureToolObject(c, "Secret "+ns+"/"+storage.UsageLogsSecret, path, envkit.WithToolFieldManager(rhoai.NamespacedPath("v1", "secrets", ns, "")), obj, current, found,
		func() (kube.ObjectMeta, bool, error) { return kube.ReadObjectMeta(c, path) }, serving.ToolOwned, dryRun)
}

// waitReady waits until the Monitoring CR reports the stack available
// (waitedConditions, each for its latest spec). COOVersionCompatible False
// with VersionUnavailable is not waited for: it clears by itself.
func waitReady(m *serving.Run) (*types.OperationResponse, error) {
	c := m.Client
	m.Step(StepReady, "running", "Waiting for RHOAI's operator to report the monitoring stack, Perses and the usage logs available (about 3 minutes)")
	last := ""
	start := kube.Clock().Now()
	done, err := kube.WaitForGone(c, readyTimeout, pollInterval, func(cc *kube.Client) (bool, error) {
		mon, err := readMonitoring(cc)
		if err != nil {
			return false, err
		}
		waiting := notAvailable(mon, waitedConditions)
		if msg := strings.Join(waiting, "; "); msg != last && msg != "" {
			last = msg
			m.Step(StepReady, "running", "Waiting: "+msg)
		}
		return len(waiting) == 0, nil
	})
	switch {
	case err != nil:
		return m.Fail(StepReady, serving.StoppedWaitingText("the monitoring stack", kube.Clock().Since(start), err)+" What was set up stays; Set up again to wait again.", kube.FirstNonEmpty(kube.ErrorCodeFromK8sErr(err), "cancelled"), "ready wait failed")
	case !done:
		return m.Fail(StepReady, fmt.Sprintf("Not ready after %s (%s). What was set up stays; the tab shows each part, and Set up again waits again.", serving.ClockText(readyTimeout), last), "not_ready", "not ready")
	}
	m.Step(StepReady, "success", "RHOAI's operator reports the monitoring stack, Perses and the usage logs available.")
	return nil, nil
}

// notAvailable lists the conditions of mon among want that are not True
// for its latest spec, with what they say.
func notAvailable(mon *monitoringCR, want []string) []string {
	if mon == nil {
		return []string{"the Monitoring CR does not exist yet"}
	}
	var out []string
	for _, t := range want {
		cond, ok := mon.condition(t)
		switch {
		case !ok:
			out = append(out, t+": not reported yet")
		case !mon.current(cond):
			out = append(out, t+": not reconciled yet")
		case cond.Status != "True":
			out = append(out, fmt.Sprintf("%s: %s", t, kube.TruncateText(kube.FirstNonEmpty(cond.Message, cond.Reason, cond.Status), 200)))
		}
	}
	return out
}

// --- what the stack leaves ----------------------------------------------------------

// meta is an object's metadata (a metadata-only list: no Secret data).
type meta struct {
	Metadata kube.ObjectMeta `json:"metadata"`
}

func listMeta(c *kube.Client, path string) ([]meta, error) {
	var items []meta
	if err := c.MetadataOnly().ListInto(path, &items); err != nil {
		return nil, err
	}
	return items, nil
}

func listPVCMeta(c *kube.Client, ns string) ([]meta, error) {
	return listMeta(c, rhoai.NamespacedPath("v1", "persistentvolumeclaims", ns, ""))
}

func listSecretMeta(c *kube.Client, ns string) ([]meta, error) {
	return listMeta(c, rhoai.NamespacedPath("v1", "secrets", ns, ""))
}

// names lists the PVCs and Secrets of ns.
func names(c *kube.Client, ns string) (Names, error) {
	var out Names
	pvcs, err := listPVCMeta(c, ns)
	if err != nil {
		return out, err
	}
	secrets, err := listSecretMeta(c, ns)
	if err != nil {
		return out, err
	}
	for _, p := range pvcs {
		out.PVCs = append(out.PVCs, p.Metadata.Name)
	}
	for _, s := range secrets {
		out.Secrets = append(out.Secrets, s.Metadata.Name)
	}
	sort.Strings(out.PVCs)
	sort.Strings(out.Secrets)
	return out, nil
}

// leftover reports whether an object of the monitoring namespace is one the
// stack made after this tool's change that its garbage collection leaves:
// not there before, made after the record's time, with no owner, and none
// of OpenShift's own (a ServiceAccount's Secrets, a Service's serving
// certificate: those go with their owner or are made again).
func leftover(m kube.ObjectMeta, before []string, at string) bool {
	if slices.Contains(before, m.Name) || m.Name == storage.UsageLogsSecret || len(m.OwnerReferences) > 0 {
		return false
	}
	for _, a := range []string{"kubernetes.io/service-account.name", "service.beta.openshift.io/originating-service-name"} {
		if _, ok := m.Annotations[a]; ok {
			return false
		}
	}
	created, err1 := time.Parse(time.RFC3339, m.CreationTimestamp)
	mark, err2 := time.Parse(time.RFC3339, at)
	return err1 == nil && err2 == nil && !created.Before(mark)
}

// The stack's objects that its owners' garbage collection leaves, as RHOAI
// 3.7.0-ea.1 makes them (n8 spike, rendered/pvcs.txt and
// monitoring-ns-objects-after-gc.txt): the PVCs of the StatefulSets of the
// MonitoringStack's Prometheus, of Perses and of the usage-logs LokiStack,
// and the Secrets cert-manager issued for the collector (annotation
// cert-manager.io/certificate-name) plus the web CA odh-observability made.
// Only objects that match are recorded, and so deleted, whatever else
// appears in the namespace meanwhile.
var stackPVCNames = []*regexp.Regexp{
	regexp.MustCompile(`^prometheus-data-science-monitoringstack-db-prometheus-data-science-monitoringstack-\d+$`),
	regexp.MustCompile(`^storage-data-science-perses-\d+$`),
	regexp.MustCompile(`^(storage|wal)-data-science-usage-logs-lokistack-[a-z-]+-\d+$`),
}

const (
	certificateNameAnnotation = "cert-manager.io/certificate-name"
	webCASecret               = "prometheus-web-tls-ca"
)

func stackPVC(m kube.ObjectMeta) bool {
	for _, re := range stackPVCNames {
		if re.MatchString(m.Name) {
			return true
		}
	}
	return false
}

func stackSecret(m kube.ObjectMeta) bool {
	_, issued := m.Annotations[certificateNameAnnotation]
	return issued || m.Name == webCASecret
}

// recordLeft adds to the record what the stack made that its garbage
// collection leaves (leftover), by name and UID, so Clean up can delete
// exactly those. Without the record's list of what was there before, it
// records nothing.
func recordLeft(c *kube.Client, ns string) error {
	for attempt := 0; attempt < 3; attempt++ {
		d, found, err := readDSCI(c)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("the DSCInitialization is gone")
		}
		rec, ok, err := readRecord(d)
		if err != nil || !ok || rec.Before == nil || rec.Restored {
			return err
		}
		pvcs, err := listPVCMeta(c, ns)
		if err != nil {
			return err
		}
		secrets, err := listSecretMeta(c, ns)
		if err != nil {
			return err
		}
		left := Left{PVCs: map[string]string{}, Secrets: map[string]string{}}
		if rec.Left != nil {
			for k, uid := range rec.Left.PVCs {
				left.PVCs[k] = uid
			}
			for k, uid := range rec.Left.Secrets {
				left.Secrets[k] = uid
			}
		}
		for _, p := range pvcs {
			if leftover(p.Metadata, rec.Before.PVCs, rec.At) && stackPVC(p.Metadata) {
				left.PVCs[p.Metadata.Name] = p.Metadata.UID
			}
		}
		for _, s := range secrets {
			if leftover(s.Metadata, rec.Before.Secrets, rec.At) && stackSecret(s.Metadata) {
				left.Secrets[s.Metadata.Name] = s.Metadata.UID
			}
		}
		if rec.Left != nil && len(left.PVCs) == len(rec.Left.PVCs) && len(left.Secrets) == len(rec.Left.Secrets) {
			return nil
		}
		rec.Left = &left
		_, err = patchDSCI(c, d, map[string]any{"metadata": map[string]any{"annotations": map[string]any{RecordAnnotation: recordJSON(rec)}}}, false)
		if err == nil || !kube.IsK8sConflict(err) {
			return err
		}
	}
	return fmt.Errorf("the DSCInitialization kept changing")
}

// --- Clean up -----------------------------------------------------------------------

func teardown(m *serving.Run) (*types.OperationResponse, error) {
	c := m.Client
	m.Step(StepCheck, "running", "Checking what this tool's setup changed")
	d, found, err := readDSCI(c)
	if err != nil {
		return m.Fail(StepCheck, fmt.Sprintf("Cannot read the DSCInitialization; nothing was changed: %v.%s", err, kube.TemplateHint(err)), kube.ErrorCodeFromK8sErr(err), "check failed")
	}
	if !found {
		m.Step(StepCheck, "success", "No DSCInitialization: nothing of this tool's setup is left.")
		return m.Done("Nothing to clean up: this cluster has no DSCInitialization.")
	}
	rec, ok, err := readRecord(d)
	if err != nil {
		return m.Fail(StepCheck, err.Error()+". Nothing was changed.", "prerequisites", "record unreadable")
	}
	if !ok {
		m.Step(StepCheck, "success", "This tool's setup changed nothing on this cluster's monitoring.")
		return m.Done("Nothing to clean up: DSCInitialization " + d.Meta.Name + " has no record of this tool's setup (annotation " + RecordAnnotation + "), so its monitoring was not set up by this tool and stays as it is.")
	}
	ns := d.Namespace()
	if !rec.Restored {
		v := &view{d: d, rec: rec, recOK: true}
		if _, _, blocked := v.cleanupPlan(nil); blocked != "" {
			return m.Fail(StepCheck, blocked+" Nothing was changed.", "conflict", "changed since setup")
		}
		r := readArgo(c, d)
		if r != "" {
			return m.Fail(StepCheck, r+" Nothing was changed.", kube.ErrorCodeGitOpsManaged, "gitops")
		}
	}
	m.Step(StepCheck, "success", "Clean up removes only what this tool's setup added.")

	// 1. The DSCInitialization fields, as they were.
	if !rec.Restored {
		m.Step(StepSettings, "running", fmt.Sprintf("Removing the monitoring fields this tool added to DSCInitialization %s", d.Meta.Name))
		monitoring := map[string]any{}
		for name, f := range rec.Added {
			var prev any
			if len(f.Previous) > 0 && string(f.Previous) != "null" {
				prev = f.Previous
			}
			switch name {
			case fieldMetricsStorage:
				monitoring["metrics"] = map[string]any{"storage": prev}
			case fieldUsageLogs:
				monitoring["usageLogs"] = prev
			}
		}
		rec.Restored = true
		answer, err := patchDSCI(c, d, map[string]any{
			"metadata": map[string]any{"annotations": map[string]any{RecordAnnotation: recordJSON(rec)}},
			"spec":     map[string]any{"monitoring": monitoring},
		}, false)
		if err != nil {
			code := serving.MaaSWriteCode(err)
			msg := fmt.Sprintf("Failed to change DSCInitialization %s: %v. Nothing was changed.%s", d.Meta.Name, err, kube.TemplateHint(err))
			if kube.IsK8sConflict(err) {
				msg = fmt.Sprintf("DSCInitialization %s changed while Clean up read it; nothing was changed. Clean up again.", d.Meta.Name)
			}
			return m.Fail(StepSettings, msg, code, "dsci patch failed")
		}
		d = answer
		m.Logs = append(m.Logs, "OK: DSCInitialization "+d.Meta.Name+" spec.monitoring as before")
		m.Step(StepSettings, "success", fmt.Sprintf("DSCInitialization %s is as before this tool's setup; RHOAI's operator deletes the stack.", d.Meta.Name))
	} else {
		m.Step(StepSettings, "success", fmt.Sprintf("DSCInitialization %s is as before this tool's setup already.", d.Meta.Name))
	}

	// 2. RHOAI's operator deletes the stack.
	if r, err := waitGone(m, ns, rec); r != nil || err != nil {
		return r, err
	}

	// 3. What the stack left.
	notes, r, err := deleteLeft(m, ns, rec)
	if r != nil || err != nil {
		return r, err
	}

	// 4. The usage-logs storage (also of a setup that stopped before the
	// fields: its intent record).
	if _, ok := rec.Added[fieldUsageLogs]; ok || rec.Storage {
		m.Step(StepStorage, "running", fmt.Sprintf("Deleting Secret %s/%s, SeaweedFS user %s and bucket %s", ns, storage.UsageLogsSecret, storage.UsageLogsUser, storage.UsageLogsBucket))
		path := rhoai.NamespacedPath("v1", "secrets", ns, storage.UsageLogsSecret)
		sm, found, err := kube.ReadObjectMeta(c, path)
		if err != nil {
			return m.Fail(StepStorage, fmt.Sprintf("Cannot read Secret %s/%s: %v. Clean up again to continue.", ns, storage.UsageLogsSecret, err), kube.ErrorCodeFromK8sErr(err), "secret read failed")
		}
		switch {
		case found && !serving.ToolOwned(sm):
			notes = append(notes, fmt.Sprintf("Secret %s/%s was not created by this tool, so it was kept.", ns, storage.UsageLogsSecret))
		case found:
			if _, err := kube.DeleteWithUID(c, path, sm.UID); err != nil && !kube.IsK8sNotFound(err) {
				return m.Fail(StepStorage, fmt.Sprintf("Failed to delete Secret %s/%s: %v. Clean up again to continue.", ns, storage.UsageLogsSecret, err), kube.ErrorCodeFromK8sErr(err), "secret delete failed")
			}
			m.Logs = append(m.Logs, fmt.Sprintf("OK: Secret %s/%s deleted", ns, storage.UsageLogsSecret))
		}
		st := storage.S3Status(c)
		if st.Deployed {
			logs, err := storage.DeleteUsageLogsStorage(c)
			m.Logs = append(m.Logs, logs...)
			if err != nil {
				return m.Fail(StepStorage, fmt.Sprintf("%v. Clean up again to continue.", err), "s3_keys", "usage-logs storage delete failed")
			}
		} else {
			notes = append(notes, fmt.Sprintf("This tool's S3 storage is not deployed, so bucket %s and user %s went with it.", storage.UsageLogsBucket, storage.UsageLogsUser))
		}
		m.Step(StepStorage, "success", fmt.Sprintf("Secret %s, SeaweedFS user %s and bucket %s are deleted.", storage.UsageLogsSecret, storage.UsageLogsUser, storage.UsageLogsBucket))
	}

	// 5. The record goes last: until then Clean up can continue.
	if err := dropRecord(c); err != nil {
		notes = append(notes, "Could not remove this tool's annotation from the DSCInitialization: "+err.Error()+"; Clean up again removes it.")
	}
	slog.Info("observability cleaned up", "user", kube.GetUser(c))
	msg := "Usage logs and the dashboards this tool set up are removed, with their data. The Cluster Observability Operator, OpenTelemetry, the Loki Operator and cert-manager stay: other features share them."
	if len(notes) > 0 {
		msg += " " + strings.Join(notes, " ")
	}
	return m.Done(msg)
}

// readArgo is the refusal when Argo CD manages the DSCInitialization ("" none).
func readArgo(c *kube.Client, d dsci) string {
	app, note := argoOf(c, d)
	if app == "" {
		return ""
	}
	return argo.GitOpsRefusal("The DSCInitialization", app, "removing the monitoring fields") + note
}

// waitGone waits until RHOAI's operator deleted what the removed fields
// deployed: the Monitoring CR no longer reports them available, and no pod
// uses a disk the record names.
func waitGone(m *serving.Run, ns string, rec Record) (*types.OperationResponse, error) {
	c := m.Client
	var conds []string
	if _, ok := rec.Added[fieldMetricsStorage]; ok {
		conds = append(conds, "MonitoringStackAvailable")
	}
	if _, ok := rec.Added[fieldUsageLogs]; ok {
		conds = append(conds, "UsageLogsLokiStackAvailable")
	}
	m.Step(StepGC, "running", "Waiting for RHOAI's operator to delete the stack (usually seconds)")
	last := ""
	done, err := kube.WaitForGone(c, gcTimeout, pollInterval, func(cc *kube.Client) (bool, error) {
		mon, err := readMonitoring(cc)
		if err != nil {
			return false, err
		}
		var waiting []string
		for _, t := range conds {
			if cond, ok := mon.condition(t); ok && (cond.Status == "True" || !mon.current(cond)) {
				waiting = append(waiting, t+" is still True")
			}
		}
		left, err := stackLeft(cc, ns, rec)
		if err != nil {
			return false, err
		}
		waiting = append(waiting, left...)
		users, err := diskUsers(cc, ns, rec.Left.pvcs())
		if err != nil {
			return false, err
		}
		if len(users) > 0 {
			waiting = append(waiting, "pods still use the disks: "+strings.Join(users, ", "))
		}
		if msg := strings.Join(waiting, "; "); msg != last && msg != "" {
			last = msg
			m.Step(StepGC, "running", "Waiting: "+msg)
		}
		return len(waiting) == 0, nil
	})
	switch {
	case err != nil:
		return m.Fail(StepGC, fmt.Sprintf("Stopped waiting for the stack to go: %v. The DSCInitialization is as before; Clean up again to finish.", err), kube.FirstNonEmpty(kube.ErrorCodeFromK8sErr(err), "cancelled"), "gc wait failed")
	case !done:
		return m.Fail(StepGC, fmt.Sprintf("The stack is not gone after %s (%s). The DSCInitialization is as before; Clean up again to finish.", serving.ClockText(gcTimeout), last), "not_ready", "gc wait timed out")
	}
	m.Step(StepGC, "success", "RHOAI's operator deleted the stack.")
	return nil, nil
}

// stackLeft lists what of the stack the removed fields deployed is still
// there, read with the user's token: the LokiStack on this tool's Secret,
// the MonitoringStack and Perses (metrics), and the OpenTelemetry
// collectors (both fields). A kind whose API is not served is gone.
func stackLeft(c *kube.Client, ns string, rec Record) ([]string, error) {
	_, metrics := rec.Added[fieldMetricsStorage]
	_, logs := rec.Added[fieldUsageLogs]
	var out []string
	if logs {
		var stacks []struct {
			Metadata kube.ObjectMeta `json:"metadata"`
			Spec     struct {
				Storage struct {
					Secret struct {
						Name string `json:"name"`
					} `json:"secret"`
				} `json:"storage"`
			} `json:"spec"`
		}
		if err := c.ListInto(rhoai.NamespacedPath(lokiAPI(c), "lokistacks", ns, ""), &stacks); err != nil && !kube.IsK8sNotFound(err) {
			return nil, fmt.Errorf("list the LokiStacks of %s: %w", ns, err)
		}
		for _, s := range stacks {
			if s.Spec.Storage.Secret.Name == storage.UsageLogsSecret {
				out = append(out, "LokiStack "+s.Metadata.Name)
			}
		}
	}
	if metrics {
		names, err := listNames(c, rhoai.NamespacedPath(monitoringStackAPI(c), "monitoringstacks", ns, ""))
		if err != nil {
			return nil, err
		}
		out = append(out, prefixed("MonitoringStack ", names)...)
		if names, err = listNames(c, rhoai.NamespacedPath(persesAPI(c), "perses", ns, "")); err != nil {
			return nil, err
		}
		out = append(out, prefixed("Perses ", names)...)
	}
	if metrics && logs {
		names, err := listNames(c, rhoai.NamespacedPath(otelAPI(c), "opentelemetrycollectors", ns, ""))
		if err != nil {
			return nil, err
		}
		out = append(out, prefixed("OpenTelemetryCollector ", names)...)
	}
	return out, nil
}

// listNames lists the names at path (none when the API is not served).
func listNames(c *kube.Client, path string) ([]string, error) {
	var items []meta
	if err := c.ListInto(path, &items); err != nil {
		if kube.IsK8sNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, it := range items {
		out = append(out, it.Metadata.Name)
	}
	return out, nil
}

func prefixed(prefix string, names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = prefix + n
	}
	return out
}

// diskUsers lists the pods of ns that mount one of pvcs (by name).
func diskUsers(c *kube.Client, ns string, pvcs map[string]string) ([]string, error) {
	if len(pvcs) == 0 {
		return nil, nil
	}
	var pods []struct {
		Metadata kube.ObjectMeta `json:"metadata"`
		Spec     struct {
			Volumes []struct {
				PersistentVolumeClaim *struct {
					ClaimName string `json:"claimName"`
				} `json:"persistentVolumeClaim"`
			} `json:"volumes"`
		} `json:"spec"`
	}
	if err := c.ListInto(rhoai.NamespacedPath("v1", "pods", ns, ""), &pods); err != nil {
		return nil, fmt.Errorf("list the pods of %s: %w", ns, err)
	}
	var out []string
	for _, p := range pods {
		for _, vol := range p.Spec.Volumes {
			if vol.PersistentVolumeClaim != nil {
				if _, ok := pvcs[vol.PersistentVolumeClaim.ClaimName]; ok {
					out = append(out, p.Metadata.Name)
					break
				}
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// deleteLeft deletes what the record names that the stack left: each only
// with its recorded UID, made after the record's time and without an owner
// now. Without a record of them nothing is deleted: the disks made after
// this tool's change are listed with the commands that delete them.
func deleteLeft(m *serving.Run, ns string, rec Record) ([]string, *types.OperationResponse, error) {
	c := m.Client
	m.Step(StepLeft, "running", "Deleting the disks and certificates the stack left")
	pvcs, err := listPVCMeta(c, ns)
	if err != nil {
		r, err := m.Fail(StepLeft, fmt.Sprintf("Cannot list the PersistentVolumeClaims of %s: %v. Clean up again to continue.", ns, err), kube.ErrorCodeFromK8sErr(err), "list failed")
		return nil, r, err
	}
	var notes []string
	// The stack's disks made after this tool's change that it did not
	// record are listed, never deleted.
	var unrecorded []string
	for _, p := range pvcs {
		if _, ok := rec.Left.pvcs()[p.Metadata.Name]; !ok && rec.Before != nil && leftover(p.Metadata, rec.Before.PVCs, rec.At) && stackPVC(p.Metadata) {
			unrecorded = append(unrecorded, p.Metadata.Name)
		}
	}
	if len(unrecorded) > 0 {
		notes = append(notes, fmt.Sprintf("These disks of the stack were made after this tool's change but not recorded by it, so they were kept (they hold metrics and usage logs; a cluster-admin deletes them when nothing needs them): oc delete pvc -n %s %s", kube.ShellQuote(ns), strings.Join(unrecorded, " ")))
	}
	if rec.Left == nil {
		m.Step(StepLeft, "success", "Nothing recorded to delete.")
		return notes, nil, nil
	}
	secrets, err := listSecretMeta(c, ns)
	if err != nil {
		r, err := m.Fail(StepLeft, fmt.Sprintf("Cannot list the Secrets of %s: %v. Clean up again to continue.", ns, err), kube.ErrorCodeFromK8sErr(err), "list failed")
		return nil, r, err
	}
	deleted := 0
	for _, pvc := range []bool{true, false} {
		title, recorded, live := "Secret", rec.Left.Secrets, secrets
		if pvc {
			title, recorded, live = "PersistentVolumeClaim", rec.Left.PVCs, pvcs
		}
		for _, name := range sortedKeys(recorded) {
			i := slices.IndexFunc(live, func(o meta) bool { return o.Metadata.Name == name })
			if i < 0 {
				continue
			}
			o := live[i].Metadata
			stack := stackSecret(o)
			if pvc {
				stack = stackPVC(o)
			}
			if o.UID != recorded[name] || len(o.OwnerReferences) > 0 || !leftover(o, nil, rec.At) || !stack {
				notes = append(notes, fmt.Sprintf("%s %s/%s was made again or changed owner since this tool's setup, so it was kept.", title, ns, name))
				continue
			}
			path := rhoai.NamespacedPath("v1", "secrets", ns, name)
			if pvc {
				path = rhoai.NamespacedPath("v1", "persistentvolumeclaims", ns, name)
			}
			if _, err := kube.DeleteWithUID(c, path, o.UID); err != nil && !kube.IsK8sNotFound(err) {
				r, err := m.Fail(StepLeft, fmt.Sprintf("Failed to delete %s %s/%s: %v. Clean up again to continue.", title, ns, name, err), kube.ErrorCodeFromK8sErr(err), "delete failed")
				return nil, r, err
			}
			deleted++
			m.Logs = append(m.Logs, fmt.Sprintf("OK: %s %s/%s deleted", title, ns, name))
		}
	}
	m.Step(StepLeft, "success", fmt.Sprintf("Deleted %d %s the stack left.", deleted, kube.Verb(deleted, "object", "objects")))
	return notes, nil, nil
}

// dropRecord removes this tool's annotation from the DSCInitialization.
func dropRecord(c *kube.Client) error {
	for attempt := 0; attempt < 3; attempt++ {
		d, found, err := readDSCI(c)
		if err != nil || !found {
			return err
		}
		if _, ok := d.Meta.Annotations[RecordAnnotation]; !ok {
			return nil
		}
		_, err = patchDSCI(c, d, map[string]any{"metadata": map[string]any{"annotations": map[string]any{RecordAnnotation: nil}}}, false)
		if err == nil || !kube.IsK8sConflict(err) {
			return err
		}
	}
	return fmt.Errorf("the DSCInitialization kept changing")
}
