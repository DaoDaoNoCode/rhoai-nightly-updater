package storage

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/authz"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
)

// Project S3 storage (the non-admin design, section 4): SeaweedFS in one
// data science project, made with the token of an admin of that project,
// for that project's pipeline server only.
//
// It is the shared storage's pod (S3Resources) with these differences:
//   - every object lives in the project: no namespace and no Route of its
//     own (the shared storage's admin Route serves /metrics without
//     authentication); the admin UI is reached with oc port-forward;
//   - IAM writes stay off (no S3IAMWritesArg, no S3SelfServiceArg): the
//     one static key in Secret nightly-s3-credentials is the only key, and
//     nobody can mint another;
//   - the bucket is made inside the pod by -bucket, and the updater never
//     sends a request to the project's S3 endpoint, so its own namespace
//     needs no way in;
//   - NetworkPolicy nightly-s3-ingress lets in TCP 8333 only, from the
//     project's pods and from RHOAI's applications namespace (the
//     data-science-pipelines operator's object-store check), and is written
//     and read back before the Deployment, so the pod never starts unfenced;
//   - new names for every object and every Secret that holds the key: the
//     ServiceAccount may get four Secret names in any namespace
//     (deploy/template.yaml), and none of them is used here
//     (TestProjectStorageNamesMatchNoSARule).
//
// Ownership is the recorded UID of each object (the snapshot ConfigMap's
// key projectStorages, by "<project>/nightly-s3"), never a label: an
// editor of the project can set labels. A user's entry is admitted by the
// caps of users' records (record.AdmitUserEntryFor).
const (
	ProjectS3Deployment    = "nightly-s3"
	ProjectS3Service       = "nightly-s3"
	ProjectS3PVC           = "nightly-s3-data"
	ProjectS3Secret        = "nightly-s3-credentials"
	ProjectS3NetworkPolicy = "nightly-s3-ingress"
	// ProjectConnectionSecretName is the dashboard Connection of a
	// pipeline server on project storage (A1: not ConnectionSecretName,
	// which the ServiceAccount may read in any namespace).
	ProjectConnectionSecretName = "nightly-s3-connection"
	// projectConnectionDisplayName is how the dashboard lists it.
	projectConnectionDisplayName = "Project S3 storage (RHOAI Nightly Updater)"
	projectS3AppLabel            = "nightly-s3"
	// ProjectS3RecordKey is the snapshot ConfigMap's key of the record.
	ProjectS3RecordKey     = "projectStorages"
	projectS3RecordManager = "rhoai-nightly-updater-project-storages"
	// appsNamespaceLabel is the label every namespace carries with its own
	// name (Kubernetes 1.22 and later, NamespaceDefaultLabelName).
	appsNamespaceLabel = "kubernetes.io/metadata.name"
)

var (
	// ProjectS3ReadyTimeout bounds how long a setup waits for the pod; it
	// stays below the frontend's request timeout. Tests shorten it.
	ProjectS3ReadyTimeout = 90 * time.Second
	projectS3ReadyPoll    = 3 * time.Second
	// projectS3GoneTimeout bounds how long a delete waits for the
	// Deployment (and so its pod) to be gone before it deletes the
	// NetworkPolicy.
	projectS3GoneTimeout = 60 * time.Second
	projectS3GonePoll    = 2 * time.Second
)

// The kinds of a project storage, in the order a setup writes them: the
// NetworkPolicy before the Deployment.
const (
	kindPVC           = "PersistentVolumeClaim"
	kindSecret        = "Secret"
	kindNetworkPolicy = "NetworkPolicy"
	kindDeployment    = "Deployment"
	kindService       = "Service"
)

// ProjectS3Kinds are the kinds of a project storage's objects, in the
// order a setup writes them.
var ProjectS3Kinds = []string{kindPVC, kindSecret, kindNetworkPolicy, kindDeployment, kindService}

// ProjectS3ObjectName is the name of the project storage's object of kind.
func ProjectS3ObjectName(kind string) string {
	switch kind {
	case kindPVC:
		return ProjectS3PVC
	case kindSecret:
		return ProjectS3Secret
	case kindNetworkPolicy:
		return ProjectS3NetworkPolicy
	case kindDeployment:
		return ProjectS3Deployment
	case kindService:
		return ProjectS3Service
	}
	return ""
}

// projectS3Path is the path of the project storage's object of kind in
// project. Only these literal names reach the API (the RBAC scan resolves
// them statically).
func projectS3Path(kind, project string) string {
	switch kind {
	case kindPVC:
		return rhoai.NamespacedPath("v1", "persistentvolumeclaims", project, ProjectS3PVC)
	case kindSecret:
		return rhoai.NamespacedPath("v1", "secrets", project, ProjectS3Secret)
	case kindNetworkPolicy:
		return rhoai.NamespacedPath("networking.k8s.io/v1", "networkpolicies", project, ProjectS3NetworkPolicy)
	case kindDeployment:
		return rhoai.NamespacedPath("apps/v1", "deployments", project, ProjectS3Deployment)
	}
	return rhoai.NamespacedPath("v1", "services", project, ProjectS3Service)
}

// projectS3CreatePath is the collection a project storage object of kind
// is created in (POST), with the tool's field manager.
func projectS3CreatePath(kind, project string) string {
	switch kind {
	case kindPVC:
		return envkit.WithToolFieldManager(rhoai.NamespacedPath("v1", "persistentvolumeclaims", project, ""))
	case kindSecret:
		return envkit.WithToolFieldManager(rhoai.NamespacedPath("v1", "secrets", project, ""))
	case kindNetworkPolicy:
		return envkit.WithToolFieldManager(rhoai.NamespacedPath("networking.k8s.io/v1", "networkpolicies", project, ""))
	case kindDeployment:
		return envkit.WithToolFieldManager(rhoai.NamespacedPath("apps/v1", "deployments", project, ""))
	}
	return envkit.WithToolFieldManager(rhoai.NamespacedPath("v1", "services", project, ""))
}

// ProjectS3Host is the in-cluster S3 address of project's storage, in the
// shared storage's form (S3Host).
func ProjectS3Host(project string) string {
	return fmt.Sprintf("%s.%s.svc:%d", ProjectS3Service, project, s3Port)
}

// ProjectS3Endpoint is ProjectS3Host as a URL.
func ProjectS3Endpoint(project string) string {
	return "http://" + ProjectS3Host(project)
}

// OnProjectStorage reports whether pipeline server d uses its own
// project's S3 storage (its object storage host is that storage's
// Service, in any DNS form).
func OnProjectStorage(d DSPA) bool {
	a := Address{Service: ProjectS3Service, Namespace: d.Meta.Namespace}
	return d.Meta.Namespace != "" && a.UsedBy(d.Meta.Namespace, d.Host)
}

// ConnectionNameOf is the dashboard Connection that goes with pipeline
// server d: ProjectConnectionSecretName on project storage, else the
// shared storage's ConnectionSecretName. Every reader of the tool's
// Connection asks this for a pipeline server it has
// (TestConnectionNameFollowsTheStorage).
func ConnectionNameOf(d DSPA) string {
	if OnProjectStorage(d) {
		return ProjectConnectionSecretName
	}
	return ConnectionSecretName
}

// ConnectionSecretPath is the path of the tool's Connection name in
// project; only the two names this tool writes reach the API (any other
// name gets the shared storage's).
func ConnectionSecretPath(project, name string) string {
	if name == ProjectConnectionSecretName {
		return rhoai.NamespacedPath("v1", "secrets", project, ProjectConnectionSecretName)
	}
	return rhoai.NamespacedPath("v1", "secrets", project, ConnectionSecretName)
}

// ProjectS3Manifests are the objects of project's storage, by kind; apps
// is RHOAI's applications namespace (the NetworkPolicy's second source).
func ProjectS3Manifests(project, apps string, keys S3Keys) map[string]map[string]any {
	meta := func(name string) map[string]any {
		return map[string]any{"name": name, "namespace": project, "labels": envkit.ToolLabels()}
	}
	secretEnv := func(name, key string) map[string]any {
		return map[string]any{"name": name, "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": ProjectS3Secret, "key": key}}}
	}
	podLabels := map[string]any{"app": projectS3AppLabel}
	return map[string]map[string]any{
		kindPVC: {
			"apiVersion": "v1", "kind": kindPVC,
			"metadata": meta(ProjectS3PVC),
			"spec": map[string]any{
				"accessModes": []string{"ReadWriteOnce"},
				"resources":   map[string]any{"requests": map[string]any{"storage": s3Storage}},
				"volumeMode":  "Filesystem",
			},
		},
		kindSecret: {
			"apiVersion": "v1", "kind": kindSecret,
			"metadata": meta(ProjectS3Secret),
			"stringData": map[string]any{
				S3AccessKeyField:     keys.AccessKey,
				S3SecretKeyField:     keys.SecretKey,
				s3AdminPasswordField: keys.AdminPassword,
				s3ChecksumKeyField:   keys.ChecksumKey,
			},
		},
		kindNetworkPolicy: {
			"apiVersion": "networking.k8s.io/v1", "kind": kindNetworkPolicy,
			"metadata": meta(ProjectS3NetworkPolicy),
			"spec":     projectS3PolicySpec(apps),
		},
		kindDeployment: {
			"apiVersion": "apps/v1", "kind": kindDeployment,
			"metadata": meta(ProjectS3Deployment),
			"spec": map[string]any{
				"replicas": 1,
				"selector": map[string]any{"matchLabels": podLabels},
				"strategy": map[string]any{"type": "Recreate"},
				"template": map[string]any{
					"metadata": map[string]any{"labels": podLabels, "annotations": map[string]any{S3KeyChecksumAnnotation: keyChecksum(keys)}},
					"spec": map[string]any{
						"volumes": []map[string]any{
							{"name": "data", "persistentVolumeClaim": map[string]any{"claimName": ProjectS3PVC}},
						},
						"containers": []map[string]any{{
							"name":  s3Container,
							"image": s3Image(),
							// As the shared storage's, without the IAM
							// flags: IAM stays read-only (SeaweedFS's
							// default), so no key can be added.
							"args": ProjectS3Args(),
							"env": []map[string]any{
								secretEnv("AWS_ACCESS_KEY_ID", S3AccessKeyField),
								secretEnv("AWS_SECRET_ACCESS_KEY", S3SecretKeyField),
								{"name": "WEED_ADMIN_USER", "value": S3AdminUser},
								secretEnv("WEED_ADMIN_PASSWORD", s3AdminPasswordField),
							},
							"ports": []map[string]any{
								{"name": "s3", "containerPort": s3Port, "protocol": "TCP"},
							},
							"volumeMounts": []map[string]any{
								{"name": "data", "mountPath": "/data", "subPath": "seaweedfs"},
							},
							"resources": map[string]any{
								"limits":   map[string]any{"cpu": "500m", "memory": "1Gi"},
								"requests": map[string]any{"cpu": "64m", "memory": "256Mi"},
							},
							"securityContext": restrictedSecurityContext(),
							"readinessProbe":  map[string]any{"httpGet": map[string]any{"path": "/status", "port": s3Port}, "initialDelaySeconds": 5, "periodSeconds": 5},
							"livenessProbe":   map[string]any{"httpGet": map[string]any{"path": "/status", "port": s3Port}, "initialDelaySeconds": 30, "periodSeconds": 10, "failureThreshold": 6},
						}},
						"restartPolicy": "Always",
					},
				},
			},
		},
		kindService: {
			"apiVersion": "v1", "kind": kindService,
			"metadata": meta(ProjectS3Service),
			"spec": map[string]any{
				"selector": podLabels,
				"type":     "ClusterIP",
				"ports":    []map[string]any{{"name": "s3", "port": s3Port, "targetPort": s3Port}},
			},
		},
	}
}

// ProjectS3Args are the project storage's weed mini arguments: the shared
// storage's without the IAM flags (S3IAMWritesArg, S3SelfServiceArg).
func ProjectS3Args() []string {
	return []string{"mini", "-dir=/data", "-webdav=false", "-bucket=" + s3Bucket,
		"-admin.port=" + strconv.Itoa(s3AdminPort), "-admin.port.legacy=0"}
}

// projectS3PolicySpec is NetworkPolicy nightly-s3-ingress's spec: TCP 8333
// from the project's pods and from RHOAI's applications namespace apps,
// matched by the label every namespace carries with its name; nothing
// else (no admin UI, master, volume or filer port, and not the updater's
// namespace).
func projectS3PolicySpec(apps string) map[string]any {
	return map[string]any{
		"podSelector": map[string]any{"matchLabels": map[string]any{"app": projectS3AppLabel}},
		"policyTypes": []string{"Ingress"},
		"ingress": []map[string]any{{
			"from": []map[string]any{
				{"podSelector": map[string]any{}},
				{"namespaceSelector": map[string]any{"matchLabels": map[string]any{appsNamespaceLabel: apps}}},
			},
			"ports": []map[string]any{{"protocol": "TCP", "port": s3Port}},
		}},
	}
}

// projectS3PolicyProblem says how a NetworkPolicy's spec (as read) differs
// from what setup applies for apps, "" when it does not.
func projectS3PolicyProblem(body []byte, apps string) string {
	var live struct {
		Spec any `json:"spec"`
	}
	var want any
	b, _ := json.Marshal(projectS3PolicySpec(apps))
	_ = json.Unmarshal(b, &want)
	if json.Unmarshal(body, &live) != nil || !reflect.DeepEqual(live.Spec, want) {
		return fmt.Sprintf("NetworkPolicy %s was changed: only port %d from the project's pods and from RHOAI's applications namespace %s may reach the storage.", ProjectS3NetworkPolicy, s3Port, apps)
	}
	return ""
}

// ProjectStorageRecorded is the record of one project storage: the UID of
// each object it made, who and when.
type ProjectStorageRecorded struct {
	PVCUID           string `json:"pvcUID,omitempty"`
	SecretUID        string `json:"secretUID,omitempty"`
	NetworkPolicyUID string `json:"networkPolicyUID,omitempty"`
	DeploymentUID    string `json:"deploymentUID,omitempty"`
	ServiceUID       string `json:"serviceUID,omitempty"`
	At               string `json:"at,omitempty"`
	By               string `json:"by,omitempty"`
	// ActorRole is record.ActorRoleUser for an admin of the project who is
	// not a cluster-admin (the caps of users' records count it).
	ActorRole string `json:"actorRole,omitempty"`
	// ProjectUID is the project's namespace UID, for a user's entry.
	ProjectUID string `json:"projectUID,omitempty"`
}

// UID is the recorded UID of the object of kind.
func (e ProjectStorageRecorded) UID(kind string) string {
	switch kind {
	case kindPVC:
		return e.PVCUID
	case kindSecret:
		return e.SecretUID
	case kindNetworkPolicy:
		return e.NetworkPolicyUID
	case kindDeployment:
		return e.DeploymentUID
	case kindService:
		return e.ServiceUID
	}
	return ""
}

func (e *ProjectStorageRecorded) setUID(kind, uid string) {
	switch kind {
	case kindPVC:
		e.PVCUID = uid
	case kindSecret:
		e.SecretUID = uid
	case kindNetworkPolicy:
		e.NetworkPolicyUID = uid
	case kindDeployment:
		e.DeploymentUID = uid
	case kindService:
		e.ServiceUID = uid
	}
}

// Empty reports whether no object is recorded.
func (e ProjectStorageRecorded) Empty() bool {
	return e.PVCUID == "" && e.SecretUID == "" && e.NetworkPolicyUID == "" && e.DeploymentUID == "" && e.ServiceUID == ""
}

// ProjectStorageRecord is the record, by "<project>/nightly-s3".
type ProjectStorageRecord map[string]ProjectStorageRecorded

func projectS3RecordKey(project string) string { return project + "/" + ProjectS3Deployment }

// Get is project's entry.
func (r ProjectStorageRecord) Get(project string) (ProjectStorageRecorded, bool) {
	e, ok := r[projectS3RecordKey(project)]
	return e, ok
}

// Projects are the projects with an entry.
func (r ProjectStorageRecord) Projects() []string {
	var out []string
	for k := range r {
		if p, ok := strings.CutSuffix(k, "/"+ProjectS3Deployment); ok {
			out = append(out, p)
		}
	}
	return out
}

// Owns reports whether meta (an object of kind in its namespace) is the
// one the tool recorded: its UID, and the tool's label.
func (r ProjectStorageRecord) Owns(kind string, meta kube.ObjectMeta) bool {
	e, ok := r.Get(meta.Namespace)
	uid := e.UID(kind)
	return ok && uid != "" && uid == meta.UID && meta.HasToolLabel()
}

// ReadProjectStorageRecord reads the record (empty when nothing is
// recorded), with c's bookkeeping client.
func ReadProjectStorageRecord(c *kube.Client) (ProjectStorageRecord, error) {
	raw, err := record.ReadSnapshotKey(c, ProjectS3RecordKey)
	if err != nil {
		return nil, fmt.Errorf("read this tool's record of its project storages: %w", err)
	}
	rec := ProjectStorageRecord{}
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &rec); err != nil {
			return nil, fmt.Errorf("parse this tool's record of its project storages: %w", err)
		}
	}
	return rec, nil
}

func writeProjectStorageRecord(c *kube.Client, rec ProjectStorageRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if err := record.WriteSnapshotKey(c, ProjectS3RecordKey, projectS3RecordManager, string(data)); err != nil {
		return fmt.Errorf("record this tool's project storages: %w", err)
	}
	return nil
}

// updateProjectStorageRecord changes project's entry in one
// read-change-write (record.UpdateSnapshot); an entry left with no UID is
// removed. A user's new entry, or one taken over, is admitted by the caps
// of users' records in the same step.
func updateProjectStorageRecord(c *kube.Client, project string, change func(e *ProjectStorageRecorded)) error {
	return record.UpdateSnapshot(c, func() error {
		rec, err := ReadProjectStorageRecord(c)
		if err != nil {
			return err
		}
		e, had := rec[projectS3RecordKey(project)]
		before := e
		change(&e)
		if e == before {
			return nil
		}
		if e.Empty() {
			delete(rec, projectS3RecordKey(project))
			return writeProjectStorageRecord(c, rec)
		}
		if e.ActorRole == record.ActorRoleUser {
			var prior *record.RecordOwner
			if had {
				prior = &record.RecordOwner{Key: ProjectS3RecordKey, Project: project, ProjectUID: before.ProjectUID, By: before.By, ActorRole: before.ActorRole}
			}
			data, err := json.Marshal(e)
			if err != nil {
				return err
			}
			if err := record.AdmitUserEntryFor(c, ProjectS3RecordKey, project, prior, data); err != nil {
				return err
			}
		}
		rec[projectS3RecordKey(project)] = e
		return writeProjectStorageRecord(c, rec)
	})
}

// ForgetGoneProjectStorages drops the entries whose project is gone (gone
// answers for each), in one read-change-write, and returns their
// projects. An entry gone cannot tell about stays (fail closed).
func ForgetGoneProjectStorages(c *kube.Client, gone func(project string, e ProjectStorageRecorded) bool) ([]string, error) {
	var dropped []string
	err := record.UpdateSnapshot(c, func() error {
		rec, err := ReadProjectStorageRecord(c)
		if err != nil {
			return err
		}
		for key, e := range rec {
			project, ok := strings.CutSuffix(key, "/"+ProjectS3Deployment)
			if ok && gone(project, e) {
				delete(rec, key)
				dropped = append(dropped, project)
			}
		}
		if len(dropped) == 0 {
			return nil
		}
		return writeProjectStorageRecord(c, rec)
	})
	return dropped, err
}

// newProjectS3Keys makes a project storage's key, admin UI password and
// checksum key (random, as the shared storage's).
func newProjectS3Keys() (S3Keys, error) {
	var k S3Keys
	for _, f := range []struct {
		dst *string
		n   int
	}{{&k.AccessKey, 10}, {&k.SecretKey, 20}, {&k.AdminPassword, 16}, {&k.ChecksumKey, 16}} {
		v, err := randomHex(f.n)
		if err != nil {
			return S3Keys{}, err
		}
		*f.dst = v
	}
	return k, nil
}

// ReadProjectS3Keys reads the key of project's storage from its Secret
// with c (the user's token: an editor or admin of the project reads
// Secrets there). found is false when the Secret does not exist.
func ReadProjectS3Keys(c *kube.Client, project string) (keys S3Keys, meta kube.ObjectMeta, found bool, err error) {
	if !c.ActsAsUser() {
		return S3Keys{}, kube.ObjectMeta{}, false, authz.ErrNotUserClient
	}
	var secret struct {
		Metadata kube.ObjectMeta   `json:"metadata"`
		Data     map[string]string `json:"data"`
	}
	exists, err := c.GetIntoOptional(projectS3Path(kindSecret, project), &secret)
	if err != nil || !exists {
		return S3Keys{}, kube.ObjectMeta{}, false, err
	}
	return S3Keys{
		AccessKey: DecodeBase64Field(secret.Data[S3AccessKeyField]), SecretKey: DecodeBase64Field(secret.Data[S3SecretKeyField]),
		AdminPassword: DecodeBase64Field(secret.Data[s3AdminPasswordField]),
		ChecksumKey:   DecodeBase64Field(secret.Data[s3ChecksumKeyField]),
	}, secret.Metadata, true, nil
}

// projectS3Deployment is the part of the Deployment a status reads.
type projectS3Deployment struct {
	Metadata kube.ObjectMeta `json:"metadata"`
	Status   struct {
		ObservedGeneration int64 `json:"observedGeneration"`
		ReadyReplicas      int   `json:"readyReplicas"`
		Conditions         []struct {
			Type    string `json:"type"`
			Status  string `json:"status"`
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"conditions"`
	} `json:"status"`
}

// ready reports whether the Deployment's pod is ready for its current
// generation.
func (d projectS3Deployment) ready() bool {
	return d.Status.ReadyReplicas > 0 && d.Status.ObservedGeneration >= d.Metadata.Generation
}

// replicaFailure is the Deployment controller's ReplicaFailure condition
// (FailedCreate: the pod could not be made, for example by quota or the
// SCC), "" when there is none.
func (d projectS3Deployment) replicaFailure() string {
	for _, c := range d.Status.Conditions {
		if c.Type == "ReplicaFailure" && c.Status == "True" {
			return kube.FirstNonEmpty(kube.TruncateText(c.Message, 300), c.Reason)
		}
	}
	return ""
}

// definiteRefusal reports whether a pod creation failure is a refusal that
// will not pass by itself (forbidden, quota): the setup rolls back then.
func definiteRefusal(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "forbidden") || strings.Contains(m, "exceeded quota") || strings.Contains(m, "unable to validate against any security context constraint")
}

func readProjectS3Deployment(c *kube.Client, project string) (projectS3Deployment, bool, error) {
	if !c.ActsAsUser() {
		return projectS3Deployment{}, false, authz.ErrNotUserClient
	}
	var d projectS3Deployment
	found, err := c.GetIntoOptional(projectS3Path(kindDeployment, project), &d)
	return d, found, err
}

// errProjectAppsUnknown: RHOAI's applications namespace, the
// NetworkPolicy's second source, cannot be read (fail closed: no guessed
// name).
var errProjectAppsUnknown = errors.New("RHOAI's applications namespace could not be read")

// projectAppsNamespace is RHOAI's applications namespace, read with c's
// facts client (its DSCInitialization). The error names only how the read
// failed.
func projectAppsNamespace(c *kube.Client) (string, error) {
	ns, err := rhoai.DiscoverNamespaces(c)
	if err != nil {
		if errors.Is(err, rhoai.ErrNoDSCInitialization) {
			return "", fmt.Errorf("%w: RHOAI has no DSCInitialization on this cluster", errProjectAppsUnknown)
		}
		kube.LogFactReadFailure("cannot read RHOAI's applications namespace for project storage", "DSCInitialization", err)
		return "", fmt.Errorf("%w (%s)", errProjectAppsUnknown, kube.FactReadProblem(err))
	}
	if ns.Applications == "" {
		return "", errProjectAppsUnknown
	}
	return ns.Applications, nil
}

// readProjectS3Object reads the metadata of project's storage object of
// kind with c, the signed-in user's own token: never the ServiceAccount's,
// which may not read these objects (Secret nightly-s3-credentials holds
// the project's root key).
func readProjectS3Object(c *kube.Client, kind, project string) (kube.ObjectMeta, bool, error) {
	if !c.ActsAsUser() {
		return kube.ObjectMeta{}, false, authz.ErrNotUserClient
	}
	var obj struct {
		Metadata kube.ObjectMeta `json:"metadata"`
	}
	found, err := c.GetIntoOptional(projectS3Path(kind, project), &obj)
	return obj.Metadata, found, err
}

// fieldManagerQuery is the query of a write by the tool.
func fieldManagerQuery() url.Values {
	return url.Values{"fieldManager": {envkit.ToolFieldManager}}
}
