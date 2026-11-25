// Package observability sets up RHOAI's monitoring for the MaaS usage logs
// and the metrics dashboards, the documented way (the Observability tab):
// the operators the monitoring module needs (features.PlanObservabilityOperators),
// a bucket with a key of its own on this tool's S3 storage and the Secret
// RHOAI's docs describe, and the DSCInitialization's spec.monitoring fields
// the docs write. RHOAI's operator then deploys the stack (the Monitoring
// CR, a MonitoringStack with Prometheus and Thanos, Perses, a LokiStack and
// the usage-logs collector).
//
// Sources (red-hat-data-services/odh-observability at rhoai-3.7-ea.1,
// c5029e8c): docs/usage-logs.md L28-53 (prerequisites, the usageLogs
// fragment), docs/observability-modes.md L259-276 (metrics.storage: Perses
// is deployed only with it, internal/controller/actions.go deployPerses
// L439-445), L300-328 (the S3 Secret's seven keys), L342-343 (1x.demo for
// development clusters); red-hat-data-services/rhods-operator d6782ebf
// api/services/v1alpha1/monitoring_types.go L152-230 (UsageLogs and
// LokiStorageConfig: storageClassName defaults to gp3-csi, an AWS class).
//
// Ownership is marked on the cluster objects: this tool's label on its
// Secret, and an annotation on the DSCInitialization (RecordAnnotation)
// that says which spec.monitoring fields it added, what they were before,
// what it set, and the UIDs of what the stack then made that its garbage
// collection leaves (PVCs, certificates' Secrets). Clean up acts only on
// what that record names and the cluster still matches; anything else is
// left as it is (fail closed). Every write uses the signed-in user's token.
package observability

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/storage"
)

const (
	// RecordAnnotation is this tool's record on the DSCInitialization.
	RecordAnnotation = "rhoai-nightly-updater/monitoring"
	// FieldManager owns the spec.monitoring fields this tool sets.
	FieldManager = "rhoai-nightly-updater-monitoring"

	dsciListFmt = "/apis/dscinitialization.opendatahub.io/%s/dscinitializations"

	monitoringGroup = "services.platform.opendatahub.io"
	lokiGroup       = "loki.grafana.com"
	persesGroup     = "perses.dev"
	// The Cluster Observability Operator's MonitoringStack and the
	// OpenTelemetry operator's collectors (Clean up's wait, user token).
	monitoringStackGroup = "monitoring.rhobs"
	otelGroup            = "opentelemetry.io"

	// The fields this tool adds, as the docs write them.
	fieldMetricsStorage = "metrics.storage"
	fieldUsageLogs      = "usageLogs"
	// metricsSize and metricsRetention are the docs' metrics.storage
	// (docs/observability-modes.md L262-269).
	metricsSize      = "5Gi"
	metricsRetention = "90d"
	// UsageLogsSize is the smallest LokiStack size the CRD allows
	// (rhods-operator d6782ebf api/services/v1alpha1/monitoring_types.go
	// L162-170: the default, 1x.extra-small, "reserves 2 CPU and 8Gi per
	// ingester, which does not schedule on smaller clusters").
	UsageLogsSize = "1x.demo"

	defaultClassAnnotation     = "storageclass.kubernetes.io/is-default-class"
	betaDefaultClassAnnotation = "storageclass.beta.kubernetes.io/is-default-class"
)

// The versions read when discovery cannot tell.
var (
	monitoringFallback = []string{"v1alpha1"}
	lokiFallback       = []string{"v1"}
	persesFallback     = []string{"v1alpha1"}
)

// Timings; tests shorten them (UseTestTimings).
var (
	// readyTimeout bounds the wait for the Monitoring CR to report the
	// stack available (live on RHOAI 3.7.0-ea.1: about 3 minutes).
	readyTimeout = 8 * time.Minute
	// gcTimeout bounds the wait for RHOAI's operator to delete the stack
	// after Clean up (live: within 11 seconds).
	gcTimeout    = 3 * time.Minute
	pollInterval = 5 * time.Second
	// stuckAfter is how long a collector pod may wait for its certificate
	// Secrets before the status calls it stuck (problem A).
	stuckAfter = 5 * time.Minute
)

func monitoringAPI(c *kube.Client) string {
	return monitoringGroup + "/" + rhoai.ServedVersions(c, monitoringGroup, monitoringFallback).Versions[0]
}

func lokiAPI(c *kube.Client) string {
	return lokiGroup + "/" + rhoai.ServedVersions(c, lokiGroup, lokiFallback).Versions[0]
}

func monitoringStackAPI(c *kube.Client) string {
	return monitoringStackGroup + "/" + rhoai.ServedVersions(c, monitoringStackGroup, []string{"v1alpha1"}).Versions[0]
}

func otelAPI(c *kube.Client) string {
	return otelGroup + "/" + rhoai.ServedVersions(c, otelGroup, []string{"v1beta1"}).Versions[0]
}

func persesAPI(c *kube.Client) string {
	return persesGroup + "/" + rhoai.ServedVersions(c, persesGroup, persesFallback).Versions[0]
}

// --- the DSCInitialization ---------------------------------------------------------

// dsci is the DSCInitialization as read.
type dsci struct {
	Version string
	Meta    kube.ObjectMeta
	// Monitoring is spec.monitoring, as read.
	Monitoring map[string]any
	Conditions []kube.Condition
}

// readDSCI reads the cluster's one DSCInitialization at the newest served
// version that works. found is false without one (or without its API).
func readDSCI(c *kube.Client) (d dsci, found bool, err error) {
	r, err := rhoai.ListServed(c, rhoai.DSCIGroup, dsciListFmt, nil, rhoai.DSCIFallbackVersions)
	if err != nil {
		return d, false, fmt.Errorf("read the DSCInitialization: %w", err)
	}
	if r.Version == "" {
		return d, false, nil
	}
	var list struct {
		Items []struct {
			Metadata kube.ObjectMeta `json:"metadata"`
			Spec     struct {
				Monitoring map[string]any `json:"monitoring"`
			} `json:"spec"`
			Status struct {
				Conditions []kube.Condition `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(r.Body, &list); err != nil {
		return d, false, fmt.Errorf("parse the DSCInitialization: %w", err)
	}
	switch len(list.Items) {
	case 0:
		return d, false, nil
	case 1:
	default:
		return d, false, fmt.Errorf("%d DSCInitializations exist, so which one RHOAI uses is ambiguous", len(list.Items))
	}
	it := list.Items[0]
	return dsci{Version: r.Version, Meta: it.Metadata, Monitoring: it.Spec.Monitoring, Conditions: it.Status.Conditions}, true, nil
}

// Namespace is RHOAI's monitoring namespace (spec.monitoring.namespace).
func (d dsci) Namespace() string {
	ns, _ := d.Monitoring["namespace"].(string)
	return ns
}

// ManagementState is spec.monitoring.managementState.
func (d dsci) ManagementState() string {
	s, _ := d.Monitoring["managementState"].(string)
	return s
}

// field is the value of a spec.monitoring field this tool adds (nil when
// it is not set).
func (d dsci) field(name string) any {
	switch name {
	case fieldMetricsStorage:
		metrics, _ := d.Monitoring["metrics"].(map[string]any)
		return metrics["storage"]
	case fieldUsageLogs:
		return d.Monitoring["usageLogs"]
	}
	return nil
}

// usageLogsSecret is spec.monitoring.usageLogs.storage.secretName ("" none).
func (d dsci) usageLogsSecret() string {
	u, _ := d.Monitoring["usageLogs"].(map[string]any)
	st, _ := u["storage"].(map[string]any)
	name, _ := st["secretName"].(string)
	return name
}

// --- the record ----------------------------------------------------------------------

// Record is this tool's annotation on the DSCInitialization.
type Record struct {
	At string `json:"at"`
	// Storage: this tool may have made the usage-logs storage (bucket, user,
	// key, Secret); written before it makes any of it, so Clean up sees a
	// setup that stopped before the fields were added.
	Storage bool `json:"storage,omitempty"`
	// Bucket and User are the names this setup drew for the usage-logs
	// storage (written before it makes them), and Secret the Secret's name:
	// Clean up deletes exactly these.
	Bucket string `json:"bucket,omitempty"`
	User   string `json:"user,omitempty"`
	Secret string `json:"secret,omitempty"`
	// Added are the spec.monitoring fields this tool added ("metrics.storage",
	// "usageLogs"), each with its value before (null: not set) and the value
	// it set.
	Added map[string]AddedField `json:"added"`
	// Before are the PVCs and Secrets the monitoring namespace had right
	// before the change: what the stack makes afterwards is told from them.
	Before *Names `json:"before,omitempty"`
	// Left are the UIDs, by name, of what the stack made that its owners'
	// garbage collection does not delete (no ownerReferences): recorded once
	// the stack reports ready.
	Left *Left `json:"left,omitempty"`
	// Restored: Clean up put the fields back; what Left names and the
	// usage-logs storage remain to be deleted.
	Restored bool `json:"restored,omitempty"`
}

// AddedField is one field this tool added.
type AddedField struct {
	Previous json.RawMessage `json:"previous"`
	Set      json.RawMessage `json:"set"`
}

// Names are object names by kind.
type Names struct {
	PVCs    []string `json:"pvcs,omitempty"`
	Secrets []string `json:"secrets,omitempty"`
}

// Left are object UIDs by name. Certificates are the cert-manager
// Certificates (of the RHOAI stack) whose Secrets Secrets holds.
type Left struct {
	PVCs         map[string]string `json:"pvcs,omitempty"`
	Secrets      map[string]string `json:"secrets,omitempty"`
	Certificates map[string]string `json:"certificates,omitempty"`
}

// names are the recorded usage-logs bucket and user.
func (r Record) names() storage.UsageLogsNames {
	return storage.UsageLogsNames{Bucket: r.Bucket, User: r.User}
}

// readRecord parses the record on d. ok is false without one; an
// annotation that does not parse is an error (Clean up then refuses).
func readRecord(d dsci) (rec Record, ok bool, err error) {
	raw := d.Meta.Annotations[RecordAnnotation]
	if raw == "" {
		return rec, false, nil
	}
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		return rec, false, fmt.Errorf("annotation %s on DSCInitialization %s does not parse: %w", RecordAnnotation, d.Meta.Name, err)
	}
	if len(rec.Added) == 0 && !rec.Storage {
		return rec, false, fmt.Errorf("annotation %s on DSCInitialization %s names no field", RecordAnnotation, d.Meta.Name)
	}
	return rec, true, nil
}

// sameJSON compares a value as read with one as recorded.
func sameJSON(read any, recorded json.RawMessage) bool {
	var want any
	if len(recorded) == 0 || string(recorded) == "null" {
		return read == nil
	}
	if err := json.Unmarshal(recorded, &want); err != nil {
		return false
	}
	raw, err := json.Marshal(read)
	if err != nil {
		return false
	}
	var got any
	if err := json.Unmarshal(raw, &got); err != nil {
		return false
	}
	return reflect.DeepEqual(got, want)
}

// changedSince lists the fields of rec whose value on d is no longer what
// this tool set.
func changedSince(rec Record, d dsci) []string {
	var out []string
	for _, name := range []string{fieldMetricsStorage, fieldUsageLogs} {
		f, ok := rec.Added[name]
		if ok && !sameJSON(d.field(name), f.Set) {
			out = append(out, name)
		}
	}
	return out
}

// --- what Set up writes ----------------------------------------------------------------

// metricsStorageValue is the docs' metrics.storage.
func metricsStorageValue() map[string]any {
	return map[string]any{"size": metricsSize, "retention": metricsRetention}
}

// usageLogsValue is the docs' usageLogs, on this tool's Secret and the
// given StorageClass.
func usageLogsValue(storageClass string) map[string]any {
	return map[string]any{
		"size": UsageLogsSize,
		"storage": map[string]any{
			"type": "s3", "secretName": storage.UsageLogsSecret, "credentialMode": "static", "storageClassName": storageClass,
		},
	}
}

// patchDSCI merge-patches the DSCInitialization as this tool's field
// manager, guarded by resourceVersion (409 when it changed since read),
// with strict field validation: a field the served schema does not know is
// refused instead of dropped (an older RHOAI without usageLogs). It returns
// the object the API server answered with.
func patchDSCI(c *kube.Client, d dsci, body map[string]any, dryRun bool) (dsci, error) {
	meta, _ := body["metadata"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
		body["metadata"] = meta
	}
	meta["resourceVersion"] = d.Meta.ResourceVersion
	data, err := json.Marshal(body)
	if err != nil {
		return d, err
	}
	query := url.Values{"fieldManager": {FieldManager}, "fieldValidation": {"Strict"}}
	if dryRun {
		query.Set("dryRun", "All")
	}
	answer, _, err := c.Do(http.MethodPatch, fmt.Sprintf(dsciListFmt, d.Version)+"/"+d.Meta.Name, "application/merge-patch+json", data, query)
	if err != nil {
		return d, err
	}
	var obj struct {
		Metadata kube.ObjectMeta `json:"metadata"`
		Spec     struct {
			Monitoring map[string]any `json:"monitoring"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(answer, &obj); err != nil {
		return d, fmt.Errorf("parse the DSCInitialization the API server answered with: %w", err)
	}
	out := d
	out.Meta, out.Monitoring = obj.Metadata, obj.Spec.Monitoring
	return out, nil
}

// recordJSON is rec as the annotation's value.
func recordJSON(rec Record) string {
	data, _ := json.Marshal(rec)
	return string(data)
}

// rawJSON marshals v (nil is null).
func rawJSON(v any) json.RawMessage {
	data, _ := json.Marshal(v)
	return data
}
