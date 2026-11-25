package observability

import (
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/argo"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/dashdev"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/features"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/serving"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/storage"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Where the parts' Docs links go: RHOAI's monitoring module's own docs, on
// RHOAI's branch (red-hat-data-services/odh-observability rhoai-3.7-ea.1),
// at their headings.
const (
	docsUsageLogs = "https://github.com/red-hat-data-services/odh-observability/blob/rhoai-3.7-ea.1/docs/usage-logs.md"
	docsModes     = "https://github.com/red-hat-data-services/odh-observability/blob/rhoai-3.7-ea.1/docs/observability-modes.md"
)

// The Monitoring CR's conditions Set up waits for (docs/usage-logs.md
// L185-188 and the MaaS observability setup's checks), in the order the
// stack comes up.
var waitedConditions = []string{"MonitoringStackAvailable", "PersesAvailable", "UsageLogsLokiStackAvailable", "UsageLogsCollectorAvailable", "Ready"}

// Problem A (live on RHOAI 3.7.0-ea.1, 2026-11-25): the OpenTelemetry
// operator checks for cert-manager once, when it starts, and RHOAI's
// monitoring grants it cert-manager's permissions (ClusterRoleBinding
// odh-observability-opentelemetry-cert-manager) seconds later; the metrics
// collector's certificates are then never made, its pods wait for these
// Secrets forever, and the Monitoring CR still reports the collector
// available.
const (
	collectorPodPrefix = "data-science-collector-"
)

var collectorSecrets = []string{"data-science-collector-ta-client-cert", "data-science-collector-collector-headless-tls"}

// monitoringCR is the Monitoring CR as read.
type monitoringCR struct {
	Name       string
	Generation int64
	Conditions []monitoringCondition
}

type monitoringCondition struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	Reason             string `json:"reason"`
	Message            string `json:"message"`
	ObservedGeneration int64  `json:"observedGeneration"`
	// Severity Info on a False condition: not configured (odh-observability
	// MarkNotConfigured always sets it, whatever the reason: MetricsNotConfigured,
	// LokiNotRequired, ...).
	Severity string `json:"severity"`
}

// notConfigured: the condition says its part is not configured.
func (c monitoringCondition) notConfigured() bool {
	return c.Status == "False" && c.Severity == "Info"
}

func (m *monitoringCR) condition(typ string) (monitoringCondition, bool) {
	if m == nil {
		return monitoringCondition{}, false
	}
	for _, c := range m.Conditions {
		if c.Type == typ {
			return c, true
		}
	}
	return monitoringCondition{}, false
}

// current: the condition was written for the Monitoring CR's latest spec
// (a condition without observedGeneration counts as current).
func (m *monitoringCR) current(c monitoringCondition) bool {
	return c.ObservedGeneration == 0 || c.ObservedGeneration >= m.Generation
}

// readMonitoring reads the Monitoring CR RHOAI's operator makes from the
// DSCInitialization (cluster-scoped, one per cluster); nil when there is
// none or its API is not served.
func readMonitoring(c *kube.Client) (*monitoringCR, error) {
	var items []struct {
		Metadata kube.ObjectMeta `json:"metadata"`
		Status   struct {
			Conditions []monitoringCondition `json:"conditions"`
		} `json:"status"`
	}
	if err := c.ListInto(rhoai.ClusterPath(monitoringAPI(c), "monitorings", ""), &items); err != nil {
		if kube.IsK8sNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read the Monitoring CR: %w", err)
	}
	if len(items) == 0 {
		return nil, nil
	}
	it := items[0]
	return &monitoringCR{Name: it.Metadata.Name, Generation: it.Metadata.Generation, Conditions: it.Status.Conditions}, nil
}

// volume is a PVC of the monitoring namespace.
type volume struct {
	Meta         kube.ObjectMeta
	Size         string
	StorageClass string
}

func listVolumes(c *kube.Client, ns string) ([]volume, error) {
	var items []struct {
		Metadata kube.ObjectMeta `json:"metadata"`
		Spec     struct {
			StorageClassName string `json:"storageClassName"`
			Resources        struct {
				Requests map[string]string `json:"requests"`
			} `json:"resources"`
		} `json:"spec"`
		Status struct {
			Capacity map[string]string `json:"capacity"`
		} `json:"status"`
	}
	if err := c.ListInto(rhoai.NamespacedPath("v1", "persistentvolumeclaims", ns, ""), &items); err != nil {
		return nil, fmt.Errorf("list the PersistentVolumeClaims of %s: %w", ns, err)
	}
	out := make([]volume, 0, len(items))
	for _, it := range items {
		out = append(out, volume{Meta: it.Metadata, Size: kube.FirstNonEmpty(it.Status.Capacity["storage"], it.Spec.Resources.Requests["storage"]), StorageClass: it.Spec.StorageClassName})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Meta.Name < out[j].Meta.Name })
	return out, nil
}

// view is everything the tab, Set up and Clean up decide on, read once.
type view struct {
	d        dsci
	dsciOK   bool
	dsciErr  error
	rec      Record
	recOK    bool
	recErr   error
	ns       string
	ops      *features.OperatorsPlan
	s3       types.ResourceState
	secured  bool
	s3Err    error
	mon      *monitoringCR
	monErr   error
	argoApp  string
	argoNote string
	// external: usage logs are configured, not by this tool.
	external bool
	// addMetrics, addUsageLogs: Set up adds the field.
	addMetrics, addUsageLogs bool
	// storageClass is the cluster's default StorageClass (read only when
	// Set up adds usageLogs).
	storageClass  string
	block         *types.ComponentBlock
	refusal, code string
}

func (v *view) refuse(msg, code string) {
	if v.refusal == "" {
		v.refusal, v.code = msg, code
	}
}

// read reads what the tab shows and Set up decides on. Nothing it reads
// is only Set up's (those reads use the user's token: run.go).
func read(c *kube.Client) *view {
	v := &view{}
	v.d, v.dsciOK, v.dsciErr = readDSCI(c)
	v.ops = features.PlanObservabilityOperators(c)
	switch {
	case v.dsciErr != nil:
		v.refuse(fmt.Sprintf("Cannot read the DSCInitialization; nothing was changed: %v.%s", v.dsciErr, kube.TemplateHint(v.dsciErr)), kube.ErrorCodeFromK8sErr(v.dsciErr))
		return v
	case !v.dsciOK:
		v.block = &types.ComponentBlock{Component: "monitoring", Reason: "RHOAI is not installed on this cluster, or it has no DSCInitialization: RHOAI's monitoring is configured there. A cluster-admin installs RHOAI on the Status page."}
		v.refuse(v.block.Reason+" Nothing was changed.", "prerequisites")
		return v
	}
	v.ns = v.d.Namespace()
	if state := v.d.ManagementState(); state != "Managed" {
		v.block = &types.ComponentBlock{Component: "monitoring", DSC: v.d.Meta.Name, State: state,
			Reason: fmt.Sprintf("RHOAI's monitoring is turned off: spec.monitoring.managementState is %s in DSCInitialization %s, and the usage logs and dashboards are part of it. A cluster-admin sets it to Managed (RHOAI's default).", kube.FirstNonEmpty(state, "not set"), v.d.Meta.Name)}
		v.refuse(v.block.Reason+" Nothing was changed.", "prerequisites")
	}
	if v.ns == "" && v.block == nil {
		v.block = &types.ComponentBlock{Component: "monitoring", DSC: v.d.Meta.Name, Reason: fmt.Sprintf("DSCInitialization %s names no monitoring namespace (spec.monitoring.namespace), so this tool cannot tell where RHOAI's monitoring runs.", v.d.Meta.Name)}
		v.refuse(v.block.Reason+" Nothing was changed.", "prerequisites")
	}
	v.rec, v.recOK, v.recErr = readRecord(v.d)
	if v.recErr != nil {
		v.refuse(v.recErr.Error()+". A cluster-admin removes the annotation once spec.monitoring is as wanted. Nothing was changed.", "prerequisites")
	}
	_, logsByTool := v.rec.Added[fieldUsageLogs]
	v.addMetrics = v.d.field(fieldMetricsStorage) == nil
	v.addUsageLogs = v.d.field(fieldUsageLogs) == nil
	v.external = !v.addUsageLogs && (!v.recOK || !logsByTool)
	if v.external {
		v.refuse(fmt.Sprintf("Usage logs are configured in DSCInitialization %s already, not by this tool (storage Secret %s), so this tool leaves them as they are. Nothing was changed.", v.d.Meta.Name, kube.FirstNonEmpty(v.d.usageLogsSecret(), "none: forwarding only")), "not_managed")
	}
	v.argoApp, v.argoNote = argoOf(c, v.d)
	if v.argoApp != "" && (v.addMetrics || v.addUsageLogs) {
		v.refuse(argo.GitOpsRefusal("The DSCInitialization", v.argoApp, "adding the monitoring fields")+v.argoNote, kube.ErrorCodeGitOpsManaged)
	}
	if msg, code := v.ops.Refusal(); msg != "" {
		v.refuse(msg, code)
	}
	if v.addUsageLogs {
		sc, err := defaultStorageClass(c)
		switch {
		case err != nil:
			v.refuse(fmt.Sprintf("Cannot read the StorageClasses; nothing was changed: %v.%s", err, kube.TemplateHint(err)), kube.ErrorCodeFromK8sErr(err))
		case sc == "":
			v.refuse("This cluster has no default StorageClass (annotation "+defaultClassAnnotation+"), and the LokiStack's disks need one: RHOAI's default for usageLogs.storage.storageClassName is gp3-csi, which exists on AWS only. A cluster-admin marks a StorageClass as the default. Nothing was changed.", "prerequisites")
		}
		v.storageClass = sc
	}
	v.s3, _, v.s3Err = storage.S3StatusAndEndpoints(c)
	if v.s3.Deployed {
		v.secured, v.s3Err = storage.StorageSecured(c)
	}
	if v.addUsageLogs {
		if v.s3Err != nil {
			v.refuse(fmt.Sprintf("Cannot read the S3 storage, where usage logs would be kept; nothing was changed: %v.%s", v.s3Err, kube.TemplateHint(v.s3Err)), kube.ErrorCodeFromK8sErr(v.s3Err))
		} else if why := storage.UsageLogsStorageBlock(v.s3, v.secured); why != "" {
			v.refuse(why+" Nothing was changed.", "prerequisites")
		}
	}
	v.mon, v.monErr = readMonitoring(c)
	if v.addUsageLogs && v.monErr == nil && v.block == nil {
		v.usageLogsSupported()
	}
	return v
}

// usageLogsSupported refuses Set up when this RHOAI cannot store usage
// logs as Set up would configure them: its Monitoring CR reports no usage
// logs conditions (RHOAI's operator reports UsageLogs* in the releases
// whose DSCInitialization has spec.monitoring.usageLogs), or the cluster
// has no default StorageClass (the CRD's default for storageClassName is
// gp3-csi, an AWS class).
func (v *view) usageLogsSupported() {
	switch {
	case v.mon == nil:
		v.refuse("RHOAI's operator has not made the Monitoring CR yet, so this tool cannot tell whether this RHOAI stores usage logs. Nothing was changed.", "prerequisites")
		return
	case !v.mon.reports("UsageLogs"):
		v.refuse("This RHOAI's monitoring reports no usage logs (its Monitoring CR has no UsageLogs conditions), so it has none to set up. Nothing was changed.", "unsupported")
		return
	}
}

// reports: the Monitoring CR has a condition whose type starts with prefix.
func (m *monitoringCR) reports(prefix string) bool {
	for _, c := range m.Conditions {
		if strings.HasPrefix(c.Type, prefix) {
			return true
		}
	}
	return false
}

// argoOf is the Argo CD application that manages the DSCInitialization
// ("" none), and the note when that cannot be confirmed.
func argoOf(c *kube.Client, d dsci) (string, string) {
	tr := argo.TrackingOf(c, map[string]any{"metadata": map[string]any{"name": d.Meta.Name, "annotations": anyMap(d.Meta.Annotations), "labels": anyMap(d.Meta.Labels)}}, rhoai.DSCIGroup, "DSCInitialization")
	if tr.ManagedBy() == "" {
		return "", ""
	}
	return tr.ManagedBy(), tr.UnconfirmedNote()
}

func anyMap(m map[string]string) map[string]any {
	out := map[string]any{}
	for k, val := range m {
		out[k] = val
	}
	return out
}

// GetStatus reads the Observability tab: every part, what Set up and Clean
// up would do, the disks and the Monitoring CR's conditions. Reads that fail
// show in their part.
func GetStatus(c *kube.Client) (*types.ObservabilityStatus, error) {
	v := read(c)
	out := &types.ObservabilityStatus{Blocked: v.block, Namespace: v.ns, ManagedByTool: v.recOK, External: v.external && v.dsciOK,
		DashboardURL: dashdev.CachedURL(c)}
	out.Configured = v.dsciOK && v.d.field(fieldUsageLogs) != nil
	out.Parts = append(out.Parts, v.ops.Parts()...)
	out.Parts = append(out.Parts, v.storagePart(), v.dsciPart())
	var volumes []volume
	var volErr error
	if v.ns != "" {
		volumes, volErr = listVolumes(c, v.ns)
	}
	if v.dsciOK && v.ns != "" {
		out.Parts = append(out.Parts, v.stackPart(), v.usageLogsPart(c), v.collectorPart(c), v.dashboardsPart(c))
	}
	out.Ready = len(out.Parts) > 0
	for _, p := range out.Parts {
		if p.State != "ready" {
			out.Ready = false
		}
	}
	if v.mon != nil {
		for _, cond := range v.mon.Conditions {
			out.Conditions = append(out.Conditions, types.ObservabilityCondition{Type: cond.Type, Status: cond.Status, Reason: cond.Reason, Message: cond.Message})
		}
	}
	var total float64
	for _, vol := range volumes {
		uid, recorded := v.rec.Left.pvcs()[vol.Meta.Name]
		out.Volumes = append(out.Volumes, types.ObservabilityVolume{Name: vol.Meta.Name, Size: vol.Size, StorageClass: vol.StorageClass, SetUpByTool: recorded && uid == vol.Meta.UID})
		if q, ok := serving.ParseQuantity(vol.Size); ok {
			total += q
		}
	}
	if len(volumes) > 0 {
		out.VolumesTotal = fmt.Sprintf("%.0fGi", total/(1<<30))
	}
	if volErr != nil {
		out.Parts = append(out.Parts, types.MaaSPart{ID: "volumes", Title: "Disks", State: "unknown", Message: volErr.Error()})
		out.Ready = false
	}
	out.SetupPlan = v.setupPlan()
	if v.refusal != "" {
		out.SetupBlockedReason = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(v.refusal), "Nothing was changed."))
	}
	if v.recOK {
		out.CleanupPlan, out.CleanupKept, out.CleanupBlockedReason = v.cleanupPlan(volumes)
	}
	return out, nil
}

func (l *Left) pvcs() map[string]string {
	if l == nil {
		return nil
	}
	return l.PVCs
}

func (l *Left) secrets() map[string]string {
	if l == nil {
		return nil
	}
	return l.Secrets
}

// setupPlan lists every change Set up would make now.
func (v *view) setupPlan() []string {
	if !v.dsciOK || v.external {
		return nil
	}
	plan := append([]string(nil), v.ops.Plan()...)
	if v.addUsageLogs {
		plan = append(plan,
			fmt.Sprintf("On this tool's S3 storage: bucket %s, and SeaweedFS user %s with a key that reaches that bucket only", storage.UsageLogsBucket, storage.UsageLogsUser),
			fmt.Sprintf("Secret %s/%s with the seven keys RHOAI's docs list (the key, the bucket, the in-cluster S3 address)", v.ns, storage.UsageLogsSecret))
	}
	var fields []string
	if v.addMetrics {
		fields = append(fields, fmt.Sprintf("metrics.storage (size %s, retention %s)", metricsSize, metricsRetention))
	}
	if v.addUsageLogs {
		fields = append(fields, fmt.Sprintf("usageLogs (size %s, S3 storage in Secret %s, credentialMode static, the cluster's default StorageClass)", UsageLogsSize, storage.UsageLogsSecret))
	}
	if len(fields) > 0 {
		plan = append(plan, fmt.Sprintf("DSCInitialization %s: add spec.monitoring.%s; this tool records what it added in annotation %s, and Clean up puts it back", v.d.Meta.Name, strings.Join(fields, " and spec.monitoring."), RecordAnnotation))
	}
	if len(plan) > 0 {
		plan = append(plan, "Wait until RHOAI's operator reports the monitoring stack, Perses, the usage-logs LokiStack and collector available (about 3 minutes)")
	} else if v.recOK && !v.rec.Restored && v.monErr == nil && len(notAvailable(v.mon, waitedConditions)) > 0 {
		// A setup of this tool that stopped waiting (or before the fields):
		// Set up again checks the storage and waits again.
		plan = append(plan, "Check the usage-logs storage, then wait again until RHOAI's operator reports the monitoring stack, Perses, the usage-logs LokiStack and collector available (the last Set up stopped before they were)")
	}
	return plan
}

// cleanupPlan is what Clean up changes and deletes, what it leaves, and why
// it cannot run now ("").
func (v *view) cleanupPlan(volumes []volume) (plan, kept []string, blocked string) {
	var fields []string
	for _, name := range []string{fieldMetricsStorage, fieldUsageLogs} {
		if _, ok := v.rec.Added[name]; ok {
			fields = append(fields, "spec.monitoring."+name)
		}
	}
	if !v.rec.Restored && len(fields) > 0 {
		plan = append(plan, fmt.Sprintf("DSCInitialization %s: remove %s, as before this tool's setup; RHOAI's operator then deletes the stack", v.d.Meta.Name, strings.Join(fields, " and ")))
	}
	var pvcs []string
	var total float64
	for _, vol := range volumes {
		if uid, ok := v.rec.Left.pvcs()[vol.Meta.Name]; ok && uid == vol.Meta.UID {
			pvcs = append(pvcs, fmt.Sprintf("%s (%s)", vol.Meta.Name, vol.Size))
			if q, ok := serving.ParseQuantity(vol.Size); ok {
				total += q
			}
		}
	}
	if len(pvcs) > 0 {
		plan = append(plan, fmt.Sprintf("Delete the %d PersistentVolumeClaims the stack left in %s, %.0fGi in total, with their data (metrics and usage logs): %s", len(pvcs), v.ns, total/(1<<30), strings.Join(pvcs, ", ")))
	}
	if names := sortedKeys(v.rec.Left.secrets()); len(names) > 0 {
		plan = append(plan, fmt.Sprintf("Delete the Secrets the stack left in %s (certificates): %s", v.ns, strings.Join(names, ", ")))
	}
	if _, ok := v.rec.Added[fieldUsageLogs]; ok || v.rec.Storage {
		plan = append(plan, fmt.Sprintf("Delete Secret %s/%s, SeaweedFS user %s with its key, and bucket %s with the usage logs in it", v.ns, storage.UsageLogsSecret, storage.UsageLogsUser, storage.UsageLogsBucket))
	}
	kept = append(kept, "The Cluster Observability Operator, Red Hat build of OpenTelemetry, Loki Operator and cert-manager: left in place, as other features and admins share them")
	if v.rec.Left == nil {
		kept = append(kept, "Disks and Secrets the stack made that this tool did not record (its setup did not reach the end): listed with the commands that delete them")
	}
	if changed := changedSince(v.rec, v.d); len(changed) > 0 && !v.rec.Restored {
		blocked = fmt.Sprintf("spec.monitoring.%s in DSCInitialization %s changed since this tool's setup, so Clean up leaves it: put back what this tool set, or remove the fields and annotation %s yourself.", strings.Join(changed, " and spec.monitoring."), v.d.Meta.Name, RecordAnnotation)
	}
	if v.argoApp != "" && !v.rec.Restored {
		blocked = argo.GitOpsRefusal("The DSCInitialization", v.argoApp, "removing the monitoring fields") + v.argoNote
	}
	return plan, kept, blocked
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// storagePart is the usage-logs storage: bucket, key and Secret.
func (v *view) storagePart() types.MaaSPart {
	pt := types.MaaSPart{ID: "usage-logs-storage", Title: "Usage-logs storage", DocURL: docsModes + "#enabling-built-in-logs-loki"}
	_, byTool := v.rec.Added[fieldUsageLogs]
	switch {
	case !v.dsciOK:
		pt.State, pt.Message = "unknown", "Depends on the DSCInitialization, which cannot be read."
	case v.external:
		pt.State, pt.NotManaged = "ready", true
		pt.Message = fmt.Sprintf("Configured, not by this tool: usage logs use Secret %s/%s. This tool leaves it as it is.", v.ns, kube.FirstNonEmpty(v.d.usageLogsSecret(), "(none: forwarding only)"))
	case v.recOK && byTool:
		pt.State, pt.ManagedByTool = "ready", true
		pt.Message = fmt.Sprintf("Bucket %s on this tool's S3 storage, SeaweedFS user %s whose key reaches that bucket only, and Secret %s/%s with that key (set up by this tool).", storage.UsageLogsBucket, storage.UsageLogsUser, v.ns, storage.UsageLogsSecret)
		if !v.s3.Deployed {
			pt.State, pt.Message = "problem", fmt.Sprintf("This tool's S3 storage, which holds bucket %s, is not deployed: Loki cannot store usage logs. Set up the S3 storage again, or clean up Observability.", storage.UsageLogsBucket)
		}
	case v.s3Err != nil:
		pt.State, pt.Message = "unknown", "Cannot read the S3 storage: "+v.s3Err.Error()
	default:
		if why := storage.UsageLogsStorageBlock(v.s3, v.secured); why != "" {
			pt.State, pt.Message = "problem", why
			return pt
		}
		pt.State, pt.Message = "missing", fmt.Sprintf("Not set up. Set up makes bucket %s on this tool's S3 storage, SeaweedFS user %s with a key that reaches that bucket only, and Secret %s/%s with that key.", storage.UsageLogsBucket, storage.UsageLogsUser, v.ns, storage.UsageLogsSecret)
	}
	return pt
}

// dsciPart is the DSCInitialization's spec.monitoring fields.
func (v *view) dsciPart() types.MaaSPart {
	pt := types.MaaSPart{ID: "monitoring-settings", Title: "Monitoring settings (DSCInitialization)", DocURL: docsUsageLogs + "#configure-the-managed-collector-and-local-storage"}
	if !v.dsciOK {
		pt.State, pt.Message = "unknown", "Cannot read the DSCInitialization."
		if v.dsciErr == nil {
			pt.State, pt.Message = "manual", "No DSCInitialization: RHOAI is not installed."
		}
		return pt
	}
	if v.block != nil {
		pt.State, pt.Message = "manual", v.block.Reason
		return pt
	}
	var set, missing []string
	for _, f := range []struct {
		name string
		add  bool
	}{{fieldMetricsStorage, v.addMetrics}, {fieldUsageLogs, v.addUsageLogs}} {
		if f.add {
			missing = append(missing, "spec.monitoring."+f.name)
			continue
		}
		who := "not by this tool"
		if _, ok := v.rec.Added[f.name]; ok {
			who = "by this tool"
		}
		set = append(set, fmt.Sprintf("spec.monitoring.%s (%s)", f.name, who))
	}
	switch {
	case len(missing) > 0:
		pt.State = "missing"
		pt.Message = fmt.Sprintf("DSCInitialization %s has no %s. Set up adds %s as RHOAI's docs write %s: metrics with %s of disk kept %s (this also deploys Perses, the dashboards), usage logs on a %s LokiStack, a test size (not for production).", v.d.Meta.Name, strings.Join(missing, " or "), kube.Verb(len(missing), "it", "them"), kube.Verb(len(missing), "it", "them"), metricsSize, metricsRetention, UsageLogsSize)
		if len(set) > 0 {
			pt.Message += " Already set: " + strings.Join(set, ", ") + "; used as it is."
		}
	default:
		pt.State, pt.Message = "ready", fmt.Sprintf("DSCInitialization %s sets %s.", v.d.Meta.Name, strings.Join(set, " and "))
	}
	pt.ManagedByTool = v.recOK
	pt.NotManaged = !v.recOK && len(missing) == 0
	if changed := changedSince(v.rec, v.d); v.recOK && len(changed) > 0 && !v.rec.Restored {
		pt.Warning = fmt.Sprintf("spec.monitoring.%s changed since this tool set it; Clean up leaves the fields as they are now.", strings.Join(changed, " and spec.monitoring."))
	}
	return pt
}

// conditionPart is a part from Monitoring CR conditions: ready when all are
// True; missing while one is not configured; waiting otherwise.
func (v *view) conditionPart(pt types.MaaSPart, conds []string, what string) types.MaaSPart {
	if v.monErr != nil {
		pt.State, pt.Message = "unknown", v.monErr.Error()
		return pt
	}
	if v.mon == nil {
		pt.State, pt.Message = "missing", "RHOAI's operator has not made the Monitoring CR yet."
		return pt
	}
	var waiting, notConfigured []string
	for _, t := range conds {
		cond, ok := v.mon.condition(t)
		switch {
		case !ok:
			waiting = append(waiting, t+": not reported yet")
		case !v.mon.current(cond):
			waiting = append(waiting, t+": RHOAI's operator has not reconciled the latest change yet")
		case cond.Status == "True":
		case cond.notConfigured():
			notConfigured = append(notConfigured, t)
		default:
			waiting = append(waiting, fmt.Sprintf("%s: %s", t, kube.TruncateText(kube.FirstNonEmpty(cond.Message, cond.Reason, "not available"), 300)))
		}
	}
	switch {
	case len(notConfigured) > 0 && len(waiting) == 0:
		pt.State, pt.Message = "missing", fmt.Sprintf("Not deployed yet (%s report not configured). Once the DSCInitialization's monitoring fields are set, RHOAI's operator deploys %s.", strings.Join(notConfigured, ", "), what)
	case len(waiting) > 0 || len(notConfigured) > 0:
		pt.State, pt.Message = "waiting", "RHOAI's operator reports: "+strings.Join(append(waiting, notConfigured...), "; ")+"."
	default:
		pt.State, pt.Message = "ready", fmt.Sprintf("%s: deployed (RHOAI's operator reports %s True).", kube.SentenceStart(what), strings.Join(conds, " and "))
	}
	return pt
}

// stackPart is the metrics stack and Perses.
func (v *view) stackPart() types.MaaSPart {
	pt := v.conditionPart(types.MaaSPart{ID: "monitoring-stack", Title: "Metrics stack and Perses", DocURL: docsModes + "#3-all-built-in"},
		[]string{"MonitoringStackAvailable", "PersesAvailable"}, "the metrics stack (Prometheus, Thanos) and Perses, the dashboards' server")
	if cond, ok := v.mon.condition("MonitoringDependenciesReady"); ok && cond.Status == "False" {
		pt.Warning = "RHOAI's operator reports missing operators (MonitoringDependenciesReady False): " + kube.TruncateText(kube.FirstNonEmpty(cond.Message, cond.Reason), 300)
	}
	if cond, ok := v.mon.condition("COOVersionCompatible"); ok && cond.Status == "False" {
		note := fmt.Sprintf("COOVersionCompatible is False (%s): %s", cond.Reason, kube.TruncateText(cond.Message, 300))
		if cond.Reason == "VersionUnavailable" {
			note += " RHOAI's operator could not read the operator's version yet and reads it again by itself; it does not stop the stack."
		}
		if pt.Warning == "" {
			pt.Warning = note
		} else {
			pt.Message += " " + note
		}
	}
	return pt
}

// usageLogsPart is the usage-logs LokiStack and collector.
func (v *view) usageLogsPart(c *kube.Client) types.MaaSPart {
	pt := v.conditionPart(types.MaaSPart{ID: "usage-logs", Title: "Usage logs (LokiStack and collector)", DocURL: docsUsageLogs + "#configure-the-managed-collector-and-local-storage"},
		[]string{"UsageLogsLokiStackAvailable", "UsageLogsCollectorAvailable"}, "the usage-logs LokiStack and its collector")
	if pt.State == "waiting" || pt.State == "unknown" {
		if detail := lokiStacksDetail(c, v.ns); detail != "" {
			pt.Message += " " + detail
		}
	}
	return pt
}

// lokiStacksDetail says what the LokiStacks of ns report when one is not
// Ready ("" when there is nothing to add).
func lokiStacksDetail(c *kube.Client, ns string) string {
	var items []struct {
		Metadata kube.ObjectMeta `json:"metadata"`
		Status   struct {
			Conditions []kube.Condition `json:"conditions"`
		} `json:"status"`
	}
	if err := c.ListInto(rhoai.NamespacedPath(lokiAPI(c), "lokistacks", ns, ""), &items); err != nil {
		if kube.IsK8sNotFound(err) {
			return ""
		}
		return "Cannot read the LokiStacks: " + err.Error() + "."
	}
	var out []string
	for _, it := range items {
		if ready, ok := kube.FindCondition(it.Status.Conditions, "Ready"); ok && ready.Status == "True" {
			continue
		}
		for _, cond := range it.Status.Conditions {
			if cond.Status == "True" && cond.Type != "Ready" {
				out = append(out, fmt.Sprintf("LokiStack %s: %s (%s)", it.Metadata.Name, kube.TruncateText(kube.FirstNonEmpty(cond.Message, cond.Reason), 200), cond.Type))
			}
		}
	}
	return strings.Join(out, "; ")
}

// collectorPart is the metrics collector, with problem A when its pods
// wait for certificates that were never made.
func (v *view) collectorPart(c *kube.Client) types.MaaSPart {
	pt := v.conditionPart(types.MaaSPart{ID: "metrics-collector", Title: "Metrics collector (OpenTelemetry)", DocURL: docsModes + "#3-all-built-in"},
		[]string{"OpenTelemetryCollectorAvailable"}, "the metrics collector")
	if pt.State != "ready" {
		return pt
	}
	pods, missing, err := stuckCollector(c, v.ns)
	if err != nil {
		pt.Warning = "Cannot check the collector's pods: " + err.Error()
		return pt
	}
	if len(pods) == 0 {
		return pt
	}
	opNS := kube.FirstNonEmpty(v.ops.CSVNamespace(features.OpenTelemetryOperator.ID), features.OpenTelemetryOperator.Namespace)
	pt.State = "problem"
	pt.Message = fmt.Sprintf("RHOAI reports the collector available, but its pods %s wait for Secret %s, which was never made: the built-in metrics are not collected (usage logs are not affected). This is a product bug in RHOAI's monitoring and the OpenTelemetry operator: the operator checks for cert-manager once when it starts, before RHOAI's monitoring gives it cert-manager's permissions, and then never makes the collector's certificates.",
		strings.Join(pods, ", "), strings.Join(missing, " and "))
	pt.Commands = fmt.Sprintf("# Not in RHOAI docs; worked in our test: restart the OpenTelemetry operator once (OLM makes its pod again).\noc delete pod -n %s -l app.kubernetes.io/name=opentelemetry-operator,control-plane=controller-manager", kube.ShellQuote(opNS))
	return pt
}

// stuckCollector lists the metrics collector's pods that have waited longer
// than stuckAfter to start because a certificate Secret it mounts is
// missing (the kubelet's FailedMount events say "secret ... not found"), and
// those Secrets. Pods and events only: no Secret is read.
func stuckCollector(c *kube.Client, ns string) (pods, missing []string, err error) {
	var items []struct {
		Metadata kube.ObjectMeta `json:"metadata"`
		Spec     struct {
			Volumes []struct {
				Secret *struct {
					SecretName string `json:"secretName"`
				} `json:"secret"`
			} `json:"volumes"`
		} `json:"spec"`
		Status struct {
			Phase             string `json:"phase"`
			ContainerStatuses []struct {
				State struct {
					Waiting *struct {
						Reason string `json:"reason"`
					} `json:"waiting"`
				} `json:"state"`
			} `json:"containerStatuses"`
		} `json:"status"`
	}
	if err := c.ListInto(rhoai.NamespacedPath("v1", "pods", ns, ""), &items); err != nil {
		return nil, nil, fmt.Errorf("list the pods of %s: %w", ns, err)
	}
	candidates := map[string][]string{}
	for _, it := range items {
		if !strings.HasPrefix(it.Metadata.Name, collectorPodPrefix) || it.Status.Phase != "Pending" {
			continue
		}
		created, perr := time.Parse(time.RFC3339, it.Metadata.CreationTimestamp)
		if perr != nil || kube.Clock().Since(created) < stuckAfter {
			continue
		}
		creating := false
		for _, cs := range it.Status.ContainerStatuses {
			if cs.State.Waiting != nil && cs.State.Waiting.Reason == "ContainerCreating" {
				creating = true
			}
		}
		if !creating {
			continue
		}
		for _, vol := range it.Spec.Volumes {
			if vol.Secret != nil && slices.Contains(collectorSecrets, vol.Secret.SecretName) {
				candidates[it.Metadata.Name] = append(candidates[it.Metadata.Name], vol.Secret.SecretName)
			}
		}
	}
	if len(candidates) == 0 {
		return nil, nil, nil
	}
	var events []struct {
		InvolvedObject struct {
			Name string `json:"name"`
		} `json:"involvedObject"`
		Message string `json:"message"`
	}
	if err := c.ListInto(rhoai.NamespacedPath("v1", "events", ns, "")+"?fieldSelector="+url.QueryEscape("reason=FailedMount"), &events); err != nil {
		return nil, nil, fmt.Errorf("list the FailedMount events of %s: %w", ns, err)
	}
	seen := map[string]bool{}
	for pod, secrets := range candidates {
		for _, e := range events {
			if e.InvolvedObject.Name != pod {
				continue
			}
			for _, s := range secrets {
				if strings.Contains(e.Message, fmt.Sprintf("secret %q not found", s)) {
					if !slices.Contains(pods, pod) {
						pods = append(pods, pod)
					}
					if !seen[s] {
						seen[s] = true
						missing = append(missing, s)
					}
				}
			}
		}
	}
	sort.Strings(pods)
	sort.Strings(missing)
	return pods, missing, nil
}

// dashboardsPart is Perses and the dashboards and datasources MaaS and
// KServe made for it (problem B: made before Perses ran, they fail until
// the Perses operator retries them).
func (v *view) dashboardsPart(c *kube.Client) types.MaaSPart {
	pt := v.conditionPart(types.MaaSPart{ID: "dashboards", Title: "Dashboards (Perses)", DocURL: docsUsageLogs + "#configure-reads-and-verify-new-traffic"},
		[]string{"PersesAvailable"}, "Perses, which serves the usage and metrics dashboards")
	if pt.State != "ready" {
		return pt
	}
	notSynced, total, err := persesNotSynced(c, v.ns)
	if err != nil {
		pt.Warning = "Cannot read the Perses dashboards and datasources: " + err.Error()
		return pt
	}
	if len(notSynced) == 0 {
		pt.Message = fmt.Sprintf("Perses runs, with the %d dashboards and datasources RHOAI and MaaS made for it: in the RHOAI dashboard, Observe & monitor, Dashboard.", total)
		return pt
	}
	pt.State = "waiting"
	pt.Message = fmt.Sprintf("Not in Perses yet: %s. They were made before Perses ran; the Perses operator retries them by itself, with growing pauses. This tool does not change them.", strings.Join(notSynced, "; "))
	return pt
}

// persesNotSynced lists the PersesDatasources and PersesDashboards of ns
// whose conditions report a problem, and how many there are in all.
func persesNotSynced(c *kube.Client, ns string) ([]string, int, error) {
	datasources, err := listPerses(c, rhoai.NamespacedPath(persesAPI(c), "persesdatasources", ns, ""))
	if err != nil {
		return nil, 0, err
	}
	dashboards, err := listPerses(c, rhoai.NamespacedPath(persesAPI(c), "persesdashboards", ns, ""))
	if err != nil {
		return nil, 0, err
	}
	var out []string
	for _, kind := range []struct {
		title string
		items []persesObject
	}{{"datasource", datasources}, {"dashboard", dashboards}} {
		for _, it := range kind.items {
			for _, cond := range it.Status.Conditions {
				if (cond.Type == "Available" && cond.Status == "False") || (cond.Type == "Degraded" && cond.Status == "True") || strings.Contains(cond.Reason, "Error") {
					out = append(out, fmt.Sprintf("%s %s (%s)", kind.title, it.Metadata.Name, kube.TruncateText(strings.TrimSuffix(cond.Reason+": "+cond.Message, ": "), 200)))
					break
				}
			}
		}
	}
	sort.Strings(out)
	return out, len(datasources) + len(dashboards), nil
}

type persesObject struct {
	Metadata kube.ObjectMeta `json:"metadata"`
	Status   struct {
		Conditions []kube.Condition `json:"conditions"`
	} `json:"status"`
}

// listPerses lists Perses objects at path; none when the API is not served.
func listPerses(c *kube.Client, path string) ([]persesObject, error) {
	var items []persesObject
	if err := c.ListInto(path, &items); err != nil {
		if kube.IsK8sNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return items, nil
}
