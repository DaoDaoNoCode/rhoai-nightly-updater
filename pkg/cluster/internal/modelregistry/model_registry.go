// The modelregistry package provisions one registry using the same shape as
// the RHOAI dashboard Create model registry dialog, reconciles that object
// from the cluster, and seeds a sample model through the registry REST
// endpoint.
//
// Reference (research note recipes-handoff/env-features/model-registry.md):
//   - Dashboard-created ModelRegistry: odh-dashboard rhoai-3.6
//     frontend/src/pages/modelRegistrySettings/CreateModal.tsx and utils.ts
//     (apiVersion modelregistry.opendatahub.io/v1beta1; spec kubeRBACProxy,
//     grpc and rest empty, postgres.generateDeployment true for the default
//     database; annotations openshift.io/display-name and
//     openshift.io/description; no labels), in the namespace the component
//     reports (backend/src/utils/aihub.ts).
//   - Operator-owned children and CR status: model-registry-operator
//     rhoai-3.6 internal/controller (the default database's Secret, PVC,
//     Deployment and Service carry controller owner references to the CR, so
//     the garbage collector deletes them with it; Available turns True once
//     the server's Deployment is up; the Service gets
//     routing.opendatahub.io/external-address-rest in gateway mode).
//   - Registry REST surface: kubeflow model-registry rhoai-3.6
//     api/openapi/model-registry.yaml (v1alpha3; no DELETE, only archive).
package modelregistry

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/authz"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/dashdev"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const (
	// Name identifies the registry this tool provisions (compare nightly-dspa
	// for the pipeline server).
	Name = "nightly-registry"
	// Group and Version match what the dashboard uses to write and list
	// registries (odh-dashboard rhoai-3.6 frontend/src/api/models/odh.ts).
	// Dashboard UI only shows registries at this version, so setup aborts when
	// the cluster does not expose it.
	Group      = "modelregistry.opendatahub.io"
	Version    = "v1beta1"
	APIVersion = "modelregistry.opendatahub.io/v1beta1"
	resource   = "modelregistries"

	// displayName and description mirror user input from the dashboard dialog;
	// those values land on the object as the annotations below.
	displayName = "Nightly registry"
	description = "A model registry with the default database, for development and testing. Set up by rhoai-nightly-updater."

	// AddressAnnotation is the Service key the operator sets to the gateway
	// REST URL for the registry
	// (<gateway domain>/model-registry/<name>); the dashboard BFF then calls
	// https://<it>/api/model_registry/v1alpha3 (model-registry-operator
	// rhoai-3.6 internal/controller/config/templates/service.yaml.tmpl).
	AddressAnnotation = "routing.opendatahub.io/external-address-rest"
)

// --- the namespace -----------------------------------------------------------------

// Namespace resolves where RHOAI places model registries from the
// DataScienceCluster document: status.components.aiHub.instancesNamespace
// (v3), else status.components.modelregistry.registriesNamespace (v2 and
// older), else the same fields of the spec. Status is checked before spec
// because the operator clears status when the component is Removed
// (rhods-operator rhoai-3.6 api/components/v1alpha1/aihub_types.go). The
// dashboard consumes the same value via the AIHub module CR, whose spec the
// operator copies from the cluster object.
func Namespace(dsc map[string]any) string {
	for _, part := range []string{"status", "spec"} {
		top, _ := dsc[part].(map[string]any)
		comps, _ := top["components"].(map[string]any)
		for _, f := range []struct{ key, field string }{{"aiHub", "instancesNamespace"}, {"modelregistry", "registriesNamespace"}} {
			comp, _ := comps[f.key].(map[string]any)
			if ns, _ := comp[f.field].(string); ns != "" {
				return ns
			}
		}
	}
	return ""
}

// --- the record (snapshot ConfigMap key "modelRegistries") -----------------------------

// Provisioned registries are tracked by UID in this tool's namespace (not the
// registries namespace, which outsiders may edit), analogous to pipeline
// servers in storage/pipeline_record.go: only the tool label on the registry
// is required from someone with edit rights there.
const (
	recordKey     = "modelRegistries"
	recordManager = "rhoai-nightly-updater-model-registries"
)

// Recorded holds metadata for one registry this tool created.
type Recorded struct {
	UID string `json:"uid"`
	At  string `json:"at,omitempty"`
	By  string `json:"by,omitempty"`
}

// Record maps "<namespace>/<name>" to recorded entries.
type Record map[string]Recorded

// Owns reports whether meta is a registry whose UID appears in the tool record
// and whose metadata carries the tool label.
func (r Record) Owns(meta kube.ObjectMeta) bool {
	e, ok := r[meta.Namespace+"/"+meta.Name]
	return ok && e.UID != "" && e.UID == meta.UID && meta.HasToolLabel()
}

// ReadRecord loads the snapshot record, or an empty map when none exists.
func ReadRecord(c *kube.Client) (Record, error) {
	raw, err := record.ReadSnapshotKey(c, recordKey)
	if err != nil {
		return nil, fmt.Errorf("read this tool's record of its model registry: %w", err)
	}
	rec := Record{}
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &rec); err != nil {
			return nil, fmt.Errorf("parse this tool's record of its model registry: %w", err)
		}
	}
	return rec, nil
}

func writeRecord(c *kube.Client, rec Record) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if err := record.WriteSnapshotKey(c, recordKey, recordManager, string(data)); err != nil {
		return fmt.Errorf("record this tool's model registry: %w", err)
	}
	return nil
}

// --- the ModelRegistry -------------------------------------------------------------

// crPath is the registry's path; collectionPath the one it is created at.
func crPath(ns string) string { return rhoai.NamespacedPath(APIVersion, resource, ns, Name) }

func collectionPath(ns string) string { return rhoai.NamespacedPath(APIVersion, resource, ns, "") }

// object decodes a ModelRegistry returned from the API.
type object struct {
	Metadata kube.ObjectMeta `json:"metadata"`
	Status   struct {
		Conditions []struct {
			Type    string `json:"type"`
			Status  string `json:"status"`
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"conditions"`
	} `json:"status"`
}

// available returns the CR Available condition status and message (empty
// strings when the condition has not been published yet).
func (o object) available() (string, string) {
	for _, c := range o.Status.Conditions {
		if c.Type == "Available" {
			return c.Status, kube.FirstNonEmpty(c.Message, c.Reason)
		}
	}
	return "", ""
}

// readCR fetches the registry; found is false when the object does not exist.
func readCR(c *kube.Client, ns string) (object, bool, error) {
	var o object
	found, err := c.GetIntoOptional(crPath(ns), &o)
	if err != nil && kube.IsDecodeError(err) {
		return o, true, fmt.Errorf("parse ModelRegistry %s/%s: %w", ns, Name, err)
	}
	return o, found, err
}

// Body is the ModelRegistry manifest setup applies: the payload from the
// dashboard Create model registry dialog with Default database selected
// (odh-dashboard rhoai-3.6 CreateModal.tsx:323-344, utils.ts:132-154), with
// the tool label added. spec.postgres.image is omitted so the operator can
// inject RHOAI's PostgreSQL image.
func Body(ns string) map[string]any {
	return map[string]any{
		"apiVersion": APIVersion,
		"kind":       "ModelRegistry",
		"metadata": map[string]any{
			"name":      Name,
			"namespace": ns,
			"labels":    envkit.ToolLabels(),
			"annotations": map[string]any{
				"openshift.io/display-name": displayName,
				"openshift.io/description":  description,
			},
		},
		"spec": map[string]any{
			"kubeRBACProxy": map[string]any{},
			"grpc":          map[string]any{},
			"rest":          map[string]any{},
			"postgres":      map[string]any{"generateDeployment": true},
		},
	}
}

// ErrNoDefaultDatabase means the cluster ModelRegistry API lacks built-in
// database support (spec.postgres.generateDeployment), available in RHOAI
// from 3.4 onward. Setup stops: RHOAI documents no alternate database path
// this tool could configure.
var ErrNoDefaultDatabase = errors.New("this cluster's ModelRegistry API has no default database (spec.postgres.generateDeployment), which RHOAI 3.4 and later provide. A registry here needs an external MySQL or PostgreSQL database, which a cluster-admin sets up in the dashboard (Settings, Model registry settings)")

// unknownFieldsError inspects a create rejected under fieldValidation=Strict
// for schema fields the CRD does not define (KEP-2885 server-side field
// validation), e.g. "unknown field \"spec.postgres.generateDeployment\"".
func unknownFieldsError(err error) error {
	var k *kube.K8sError
	if !errors.As(err, &k) || !strings.Contains(k.K8sMessage, "unknown field") {
		return nil
	}
	if strings.Contains(k.K8sMessage, "generateDeployment") {
		return ErrNoDefaultDatabase
	}
	return fmt.Errorf("this cluster's ModelRegistry API does not have every field the dashboard writes: %s", kube.TruncateText(k.K8sMessage, 300))
}

// --- the Service --------------------------------------------------------------------

// service mirrors the operator-created Service for the registry.
type service struct {
	Metadata kube.ObjectMeta `json:"metadata"`
	Spec     struct {
		Ports []struct {
			Name string `json:"name"`
		} `json:"ports"`
	} `json:"spec"`
}

// shown checks Service metadata against what the dashboard registry list
// expects: label component=model-registry and a port named https-api or
// http-api (odh-dashboard rhoai-3.6 packages/model-registry/upstream/bff
// internal/integrations/kubernetes/shared_k8s_client.go).
func (s service) shown() bool {
	if s.Metadata.Labels["component"] != "model-registry" {
		return false
	}
	return slices.ContainsFunc(s.Spec.Ports, func(p struct {
		Name string `json:"name"`
	}) bool {
		return p.Name == "https-api" || p.Name == "http-api"
	})
}

// address extracts the gateway REST URL from the Service, or "" when the
// annotation is missing (no gateway), or when the value is not a bare https
// URL (the user token is only sent to proper https endpoints).
func (s service) address() string {
	v := strings.TrimSpace(s.Metadata.Annotations[AddressAnnotation])
	if v == "" {
		return ""
	}
	if !strings.Contains(v, "://") {
		v = "https://" + v
	}
	v = strings.TrimRight(v, "/")
	if !kube.SnippetURL(v) {
		return ""
	}
	return v
}

func readService(c *kube.Client, ns string) (service, bool, error) {
	var s service
	found, err := c.GetIntoOptional(rhoai.NamespacedPath("v1", "services", ns, Name), &s)
	return s, found, err
}

// userReadCR and userReadService are the status's reads, made with the
// signed-in user's own token only: they refuse another client
// (authz.ErrNotUserClient), so the ServiceAccount is granted nothing for
// them (pkg/cluster rbac_coverage_test.go rbacUserTokenReads).
func userReadCR(u *kube.Client, ns string) (object, bool, error) {
	if !u.ActsAsUser() {
		return object{}, false, authz.ErrNotUserClient
	}
	var o object
	found, err := u.GetIntoOptional(crPath(ns), &o)
	return o, found, err
}

func userReadService(u *kube.Client, ns string) (service, bool, error) {
	if !u.ActsAsUser() {
		return service{}, false, authz.ErrNotUserClient
	}
	var s service
	found, err := u.GetIntoOptional(rhoai.NamespacedPath("v1", "services", ns, Name), &s)
	return s, found, err
}

// trustedAddress is the registry's REST address the user's token may be
// sent to, or why there is none to use ("" and "": the Service names no
// address). The Service must be the one the operator made for the
// registry (owned by its UID, with the operator's labels app=<name> and
// component=model-registry), and the address must be on the cluster's own
// gateway (the host of the dashboard's address, which the data-science
// gateway serves, as it serves the registry: model-registry-operator
// rhoai-3.6 service.yaml.tmpl writes <gateway domain>/model-registry/<name>)
// at the registry's own path. Anything else gets no request: whoever may
// edit a Service there could otherwise point the user's token anywhere.
func trustedAddress(svc service, registryUID, dashboardURL string) (string, string) {
	where := fmt.Sprintf("Service %s/%s", svc.Metadata.Namespace, svc.Metadata.Name)
	if registryUID == "" || !svc.Metadata.OwnedBy(registryUID) {
		return "", where + " is not owned by the model registry this tool created"
	}
	if svc.Metadata.Labels["app"] != Name || svc.Metadata.Labels["component"] != "model-registry" {
		return "", where + " does not carry the operator's labels (app=" + Name + ", component=model-registry)"
	}
	raw := strings.TrimSpace(svc.Metadata.Annotations[AddressAnnotation])
	if raw == "" {
		return "", ""
	}
	address := svc.address()
	if address == "" {
		return "", fmt.Sprintf("the address %s names is not a plain https address", where)
	}
	u, err := url.Parse(address)
	if err != nil {
		return "", fmt.Sprintf("the address %s names cannot be read", where)
	}
	gw, err := url.Parse(dashboardURL)
	if dashboardURL == "" || err != nil || gw.Host == "" {
		return "", "the cluster's gateway address is not known (the dashboard reports no address), so the registry's address cannot be checked"
	}
	if !strings.EqualFold(u.Host, gw.Host) {
		return "", fmt.Sprintf("the address %s names (%s) is not the cluster's gateway (%s)", where, u.Host, gw.Host)
	}
	if u.Path != "/model-registry/"+Name || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Sprintf("the address %s names (%s) is not the registry's own path (/model-registry/%s) on the cluster's gateway", where, u.Path, Name)
	}
	return address, ""
}

// --- status -----------------------------------------------------------------------------

// placement captures which component and namespace host registries on this
// cluster, derived from the DataScienceCluster.
type placement struct {
	component string
	namespace string
	block     *types.ComponentBlock
}

func placementOf(r rhoai.DSCRead) placement {
	p := placement{block: envkit.ComponentBlock(r, envkit.ModelRegistryComponent)}
	if r.Object != nil {
		p.component = envkit.ModelRegistryComponent.KeyIn(r)
		p.namespace = Namespace(r.Object)
	}
	return p
}

// Status assembles registry health: dsc is the DataScienceCluster as seen by
// the ServiceAccount (registry placement is not end-user content); u loads
// the CR and Service with the signed-in user's token plus the tool bookkeeping
// record. Failed reads become state "unknown" with a reason; expired sign-in
// alone is returned as an error.
func Status(u *kube.Client, dsc rhoai.DSCRead, dashboardURL string) (*types.ModelRegistryStatus, error) {
	if !u.ActsAsUser() {
		return nil, authz.ErrNotUserClient
	}
	p := placementOf(dsc)
	st := &types.ModelRegistryStatus{Name: Name, Namespace: p.namespace, Component: p.component, Blocked: p.block, DashboardURL: dashboardURL}
	if p.namespace == "" {
		st.State = "absent"
		if p.block == nil {
			st.Message = "RHOAI does not report where model registries go yet (the DataScienceCluster's component status names no namespace)."
		}
		return st, nil
	}
	o, found, err := userReadCR(u, p.namespace)
	switch {
	case kube.IsK8sError(err, 401):
		return nil, err
	case err != nil && kube.IsK8sNotFound(err):
		// CRD or API unavailable: the component is not active.
		st.State = "absent"
		return st, nil
	case err != nil:
		st.State, st.Message = "unknown", fmt.Sprintf("Cannot read ModelRegistry %s/%s: %v", p.namespace, Name, err)
		return st, nil
	case !found:
		st.State = "absent"
		return st, nil
	}
	rec, err := ReadRecord(u)
	if err != nil {
		st.State, st.Message = "unknown", err.Error()
		return st, nil
	}
	st.ManagedByTool = rec.Owns(o.Metadata)
	// One of that name that this tool did not create keeps its own
	// lifecycle state; Foreign says whose it is.
	st.Foreign = !st.ManagedByTool
	if o.Metadata.Terminating() {
		st.State, st.Message = "terminating", "Being deleted."
		return st, nil
	}
	avail, msg := o.available()
	if avail != "True" {
		st.State, st.Message = "starting", kube.FirstNonEmpty(kube.TruncateText(msg, 300), "Starting: the operator has not reported the registry available yet.")
		return st, nil
	}
	svc, svcFound, err := userReadService(u, p.namespace)
	switch {
	case kube.IsK8sError(err, 401):
		return nil, err
	case err != nil:
		st.State, st.Message = "unknown", fmt.Sprintf("Cannot read Service %s/%s: %v", p.namespace, Name, err)
		return st, nil
	case !svcFound || !svc.shown():
		st.State, st.Message = "starting", fmt.Sprintf("Available, but Service %s/%s does not have what the dashboard lists registries by yet (label component=model-registry and a port named https-api).", p.namespace, Name)
		return st, nil
	}
	st.State = "ready"
	st.Address, st.AddressProblem = trustedAddress(svc, o.Metadata.UID, dashboardURL)
	if st.Address == "" && st.AddressProblem == "" {
		st.Message = "Ready. The operator reports no address through the cluster's gateway, so sample data cannot be added."
	}
	return st, nil
}

// ReadStatus is the Model registry tab's status (GET
// /api/resources/model-registry): sa finds where registries live (the
// DataScienceCluster) and the dashboard's address, which are no user data;
// everything about the registry is read with u, the signed-in user's own
// token, and so is the sample model (through the registry's gateway
// address, only for the registry this tool created).
func ReadStatus(sa, u *kube.Client) (*types.ModelRegistryStatus, error) {
	if !u.ActsAsUser() {
		return nil, authz.ErrNotUserClient
	}
	dsc, err := rhoai.ReadFirstDSC(sa)
	if err != nil {
		return nil, fmt.Errorf("read the DataScienceCluster: %w", err)
	}
	st, err := Status(u, dsc, dashdev.DashboardURL(sa))
	if err != nil {
		return nil, err
	}
	if st.State == "ready" && st.ManagedByTool && st.Address != "" {
		st.Sample = SampleStatus(u, st.Address)
	}
	return st, nil
}
