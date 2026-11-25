package storage

import (
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Storage from earlier versions, in namespace minio: MinIO (up to 2.0), or
// SeaweedFS behind the MinIO-era names (2.1 to 2.3). Its names are used only
// to detect it, to read its keys and address for Migrate storage, and to
// delete what the tool created there. Nothing new is ever created in it.
const (
	LegacyNamespace   = "minio"
	LegacyService     = "minio-service"
	legacyServicePort = 9000
	legacySecret      = "minio-secret"
	// The keys of minio-secret: MinIO's root user and password, which were
	// the S3 access key and secret key.
	legacyAccessKeyField = "minio_root_user"
	legacySecretKeyField = "minio_root_password"
	legacyUIRoute        = "minio-ui"
	// legacyAPIRoute exposed the S3 API outside the cluster (before 2.0).
	legacyAPIRoute = "minio-api"
	// The SeaweedFS of 2.1 to 2.3 used the names s3Deployment ("seaweedfs")
	// and s3NetworkPolicy ("seaweedfs-ingress") in namespace minio, and
	// legacySeaweedFSPVC for its data.
	LegacySeaweedFSPVC = "seaweedfs-pvc"
	// MinIO's objects. 2.1 replaced the Deployment and kept the PVC (MinIO's
	// on-disk format, which only a MinIO server can read).
	legacyMinIODeployment    = "minio"
	LegacyMinIOPVC           = "minio-pvc"
	legacyMinIONetworkPolicy = "minio-ingress"
)

// LegacyS3Host is the in-cluster S3 endpoint of the earlier storage.
func LegacyS3Host() string {
	return fmt.Sprintf("%s.%s.svc:%d", LegacyService, LegacyNamespace, legacyServicePort)
}

// legacyStorageObjects are the objects earlier versions created in
// namespace minio, in the order teardown deletes them: the Deployments
// first, so their pods release the PVCs (whose kubernetes.io/pvc-protection
// finalizer waits for that). Remove old storage, which must stay resumable
// after the copy, uses its own order (legacyRemovalPhases).
var legacyStorageObjects = []struct{ kind, name string }{
	{"Deployment", S3Deployment},
	{"Deployment", legacyMinIODeployment},
	{"NetworkPolicy", s3NetworkPolicy},
	{"NetworkPolicy", legacyMinIONetworkPolicy},
	{"Route", legacyUIRoute},
	{"Route", legacyAPIRoute},
	{"Service", LegacyService},
	{"Secret", legacySecret},
	{"PersistentVolumeClaim", LegacySeaweedFSPVC},
	{"PersistentVolumeClaim", LegacyMinIOPVC},
}

// legacyGetPath returns the path a named object of the earlier storage is
// read at. The literal names keep the RBAC rules limited to them. PVCs are
// listed, not read by name.
func legacyGetPath(kind, name string) (string, bool) {
	switch {
	case kind == "Secret" && name == legacySecret:
		return rhoai.NamespacedPath("v1", "secrets", LegacyNamespace, legacySecret), true
	case kind == "Deployment" && name == S3Deployment:
		return rhoai.NamespacedPath("apps/v1", "deployments", LegacyNamespace, S3Deployment), true
	case kind == "Deployment" && name == legacyMinIODeployment:
		return rhoai.NamespacedPath("apps/v1", "deployments", LegacyNamespace, legacyMinIODeployment), true
	case kind == "Service" && name == LegacyService:
		return rhoai.NamespacedPath("v1", "services", LegacyNamespace, LegacyService), true
	case kind == "NetworkPolicy" && name == s3NetworkPolicy:
		return rhoai.NamespacedPath("networking.k8s.io/v1", "networkpolicies", LegacyNamespace, s3NetworkPolicy), true
	case kind == "NetworkPolicy" && name == legacyMinIONetworkPolicy:
		return rhoai.NamespacedPath("networking.k8s.io/v1", "networkpolicies", LegacyNamespace, legacyMinIONetworkPolicy), true
	case kind == "Route" && name == legacyUIRoute:
		return rhoai.NamespacedPath("route.openshift.io/v1", "routes", LegacyNamespace, legacyUIRoute), true
	case kind == "Route" && name == legacyAPIRoute:
		return rhoai.NamespacedPath("route.openshift.io/v1", "routes", LegacyNamespace, legacyAPIRoute), true
	}
	return "", false
}

// legacyDeletePath returns the path of an object of the earlier storage
// that teardown (or the end of Migrate storage) may delete.
func legacyDeletePath(kind, name string) (string, bool) {
	switch {
	case kind == "PersistentVolumeClaim" && name == LegacySeaweedFSPVC:
		return rhoai.NamespacedPath("v1", "persistentvolumeclaims", LegacyNamespace, LegacySeaweedFSPVC), true
	case kind == "PersistentVolumeClaim" && name == LegacyMinIOPVC:
		return rhoai.NamespacedPath("v1", "persistentvolumeclaims", LegacyNamespace, LegacyMinIOPVC), true
	case kind == "Secret" && name == legacySecret:
		return rhoai.NamespacedPath("v1", "secrets", LegacyNamespace, legacySecret), true
	case kind == "Deployment" && name == S3Deployment:
		return rhoai.NamespacedPath("apps/v1", "deployments", LegacyNamespace, S3Deployment), true
	case kind == "Deployment" && name == legacyMinIODeployment:
		return rhoai.NamespacedPath("apps/v1", "deployments", LegacyNamespace, legacyMinIODeployment), true
	case kind == "Service" && name == LegacyService:
		return rhoai.NamespacedPath("v1", "services", LegacyNamespace, LegacyService), true
	case kind == "NetworkPolicy" && name == s3NetworkPolicy:
		return rhoai.NamespacedPath("networking.k8s.io/v1", "networkpolicies", LegacyNamespace, s3NetworkPolicy), true
	case kind == "NetworkPolicy" && name == legacyMinIONetworkPolicy:
		return rhoai.NamespacedPath("networking.k8s.io/v1", "networkpolicies", LegacyNamespace, legacyMinIONetworkPolicy), true
	case kind == "Route" && name == legacyUIRoute:
		return rhoai.NamespacedPath("route.openshift.io/v1", "routes", LegacyNamespace, legacyUIRoute), true
	case kind == "Route" && name == legacyAPIRoute:
		return rhoai.NamespacedPath("route.openshift.io/v1", "routes", LegacyNamespace, legacyAPIRoute), true
	}
	return "", false
}

// readLegacyObject reads one named object of the earlier storage.
func readLegacyObject(c *kube.Client, kind, name string) (kube.ObjectMeta, bool, error) {
	if kind == "PersistentVolumeClaim" {
		p, found, err := findPVC(c, LegacyNamespace, name)
		return p.Meta, found, err
	}
	path, ok := legacyGetPath(kind, name)
	if !ok {
		return kube.ObjectMeta{}, false, fmt.Errorf("internal error: unexpected object %s %s of the earlier storage", kind, name)
	}
	return kube.ReadObjectMeta(c, path)
}

// legacyObject is an object of the earlier storage that this tool created.
type legacyObject struct {
	Kind, Name string
	meta       kube.ObjectMeta
}

// Legacy is the storage an earlier version left in namespace minio.
type Legacy struct {
	terminating bool            // the namespace is being deleted
	nsMeta      kube.ObjectMeta // the namespace's metadata
	// Objects are the objects this tool created there, in
	// legacyStorageObjects order.
	Objects []legacyObject
	// PVCs are the data PVCs among them, with their sizes.
	PVCs []PVC
	// Server is the server that runs there ("SeaweedFS" or "MinIO"), or ""
	// when neither Deployment is left; ready is true when it has a ready
	// replica.
	Server string
	Ready  bool
	// serviceFound is true while Service minio-service exists (whoever
	// created it): the copy Job reaches the server through it.
	serviceFound bool
	// others are the Deployments and PVCs in the namespace this tool did
	// not create ("Deployment name"): what deleting the namespace would
	// also delete, as far as the tool can see (it cannot list every kind).
	others []string
	// Address is how pipeline servers may reach it; addressErr is set when
	// its Routes or Service could not be read.
	Address    Address
	AddressErr error
}

// ProjectRequesterAnnotation is set on every namespace a ProjectRequest
// made (`oc new-project`), to the user who asked for it.
const ProjectRequesterAnnotation = "openshift.io/requester"

// legacyNamespaceOurs reports whether namespace minio is the one this tool
// made. Any user who may request projects can make a namespace minio and
// fill it with objects that carry the tool's label and field manager, so
// the objects' own markers prove nothing; the namespace's do:
//   - the tool label: 2.x created the namespace with it and labelled the
//     ones earlier versions made (v2.3.3 minio.go); only someone who may
//     patch namespaces (a cluster-admin, or this tool's ServiceAccount of
//     those versions) can set it, never a project's own admin
//     (SECURITY.md, inference gateway);
//   - else the earliest versions' plain POST (manager Go-http-client, which
//     only versions before the tool's own User-Agent wrote with:
//     envkit.LegacyPostManager), but never on a namespace a ProjectRequest
//     made (openshift.io/requester).
func legacyNamespaceOurs(meta kube.ObjectMeta) bool {
	if meta.HasToolLabel() {
		return true
	}
	if meta.Annotations[ProjectRequesterAnnotation] != "" {
		return false
	}
	return meta.HasManager([]string{envkit.LegacyPostManager})
}

// ReadLegacy reports the storage an earlier version left in
// namespace minio, or nil when there is none: no such namespace, a
// namespace this tool did not make (legacyNamespaceOurs), or none of the
// names in legacyStorageObjects belongs to an object this tool created
// (the ownership label, or an earlier version's server-side apply). Any
// read error is returned (fail closed), except for the addresses, which
// only the dependency checks need (addressErr).
func ReadLegacy(c *kube.Client) (*Legacy, error) {
	l, _, err := ReadLegacyOwner(c)
	return l, err
}

// ReadLegacyOwner is readLegacyStorage, and also reports a
// namespace minio this tool did not make (foreign), for the status.
func ReadLegacyOwner(c *kube.Client) (*Legacy, bool, error) {
	body, exists, err := c.GetOptional("/api/v1/namespaces/" + LegacyNamespace)
	if err != nil {
		return nil, false, fmt.Errorf("read namespace %s: %w", LegacyNamespace, err)
	}
	if !exists {
		return nil, false, nil
	}
	var nsObj struct {
		Metadata kube.ObjectMeta `json:"metadata"`
		Status   struct {
			Phase string `json:"phase"`
		} `json:"status"`
	}
	if err := json.Unmarshal(body, &nsObj); err != nil {
		return nil, false, fmt.Errorf("parse namespace %s: %w", LegacyNamespace, err)
	}
	if !legacyNamespaceOurs(nsObj.Metadata) {
		return nil, true, nil
	}

	type result struct {
		meta  kube.ObjectMeta
		found bool
		body  []byte
		err   error
	}
	results := make([]result, len(legacyStorageObjects))
	var (
		wg        sync.WaitGroup
		pvcs      []PVC
		pvcErr    error
		address   Address
		addrErr   error
		deploys   []kube.ObjectMeta
		deployErr error
	)
	for i, o := range legacyStorageObjects {
		if o.kind == "PersistentVolumeClaim" {
			continue
		}
		wg.Add(1)
		go func(i int, kind, name string) {
			defer wg.Done()
			path, _ := legacyGetPath(kind, name)
			b, present, err := c.GetOptional(path)
			switch {
			case err != nil:
				results[i].err = err
			case !present:
			default:
				results[i].meta, results[i].err = kube.ParseObjectMeta(b)
				results[i].found, results[i].body = results[i].err == nil, b
			}
		}(i, o.kind, o.name)
	}
	wg.Add(3)
	go func() { defer wg.Done(); pvcs, pvcErr = listPVCs(c, LegacyNamespace) }()
	go func() { defer wg.Done(); deploys, deployErr = listDeploymentMetas(c, LegacyNamespace) }()
	go func() { defer wg.Done(); address, addrErr = readLegacyAddress(c) }()
	wg.Wait()

	if pvcErr != nil {
		return nil, false, fmt.Errorf("list PVCs in namespace %s: %w", LegacyNamespace, pvcErr)
	}
	l := &Legacy{terminating: nsObj.Metadata.Terminating() || nsObj.Status.Phase == "Terminating", nsMeta: nsObj.Metadata, Address: address, AddressErr: addrErr}
	deployments := map[string]S3DeploymentSpec{}
	for i, o := range legacyStorageObjects {
		r := results[i]
		if o.kind == "PersistentVolumeClaim" {
			for _, p := range pvcs {
				if p.Meta.Name == o.name && storageObjectOwned(p.Meta) {
					l.Objects = append(l.Objects, legacyObject{o.kind, o.name, p.Meta})
					l.PVCs = append(l.PVCs, p)
				}
			}
			continue
		}
		if r.err != nil {
			return nil, false, fmt.Errorf("read %s %s/%s: %w", o.kind, LegacyNamespace, o.name, r.err)
		}
		if o.kind == "Service" && r.found {
			l.serviceFound = true
		}
		if !r.found || !storageObjectOwned(r.meta) {
			continue
		}
		l.Objects = append(l.Objects, legacyObject{o.kind, o.name, r.meta})
		if o.kind == "Deployment" {
			var d S3DeploymentSpec
			if json.Unmarshal(r.body, &d) == nil {
				deployments[o.name] = d
			}
		}
	}
	if len(l.Objects) == 0 {
		return nil, false, nil
	}
	tool := map[string]bool{}
	for _, o := range l.Objects {
		tool[o.Kind+" "+o.Name] = true
	}
	for _, p := range pvcs {
		if name := "PersistentVolumeClaim " + p.Meta.Name; !tool[name] {
			l.others = append(l.others, name)
		}
	}
	if deployErr != nil {
		l.others = append(l.others, "(the Deployments could not be listed: "+deployErr.Error()+")")
	}
	for _, d := range deploys {
		if name := "Deployment " + d.Name; !tool[name] {
			l.others = append(l.others, name)
		}
	}
	sort.Strings(l.others)
	switch {
	case hasKey(deployments, S3Deployment):
		l.Server, l.Ready = "SeaweedFS", deployments[S3Deployment].Status.ReadyReplicas >= 1
	case hasKey(deployments, legacyMinIODeployment):
		l.Server, l.Ready = "MinIO", deployments[legacyMinIODeployment].Status.ReadyReplicas >= 1
	}
	return l, false, nil
}

// listDeploymentMetas lists the metadata of a namespace's Deployments.
func listDeploymentMetas(c *kube.Client, namespace string) ([]kube.ObjectMeta, error) {
	var items []struct {
		Metadata kube.ObjectMeta `json:"metadata"`
	}
	if err := c.ListInto(rhoai.NamespacedPath("apps/v1", "deployments", namespace, ""), &items); err != nil {
		return nil, err
	}
	out := make([]kube.ObjectMeta, 0, len(items))
	for _, item := range items {
		out = append(out, item.Metadata)
	}
	return out, nil
}

func hasKey[V any](m map[string]V, k string) bool {
	_, ok := m[k]
	return ok
}

// readLegacyAddress reads the addresses of the earlier storage besides its
// service DNS names: the hosts of its Routes and the ClusterIP of its
// Service. Missing objects are not errors.
func readLegacyAddress(c *kube.Client) (Address, error) {
	a := Address{Service: LegacyService, Namespace: LegacyNamespace}
	api, err := readRoute(c, LegacyNamespace, legacyAPIRoute)
	if err != nil {
		return a, err
	}
	ui, err := readRoute(c, LegacyNamespace, legacyUIRoute)
	if err != nil {
		return a, err
	}
	a.RouteHosts = []string{api.host, ui.host}
	svc, err := readService(c, LegacyNamespace, LegacyService)
	if err != nil {
		return a, err
	}
	a.ClusterIP = svc.clusterIP
	return a, nil
}

// hasPVC reports whether the earlier storage holds the named data PVC.
func (l *Legacy) hasPVC(name string) bool {
	for _, p := range l.PVCs {
		if p.Meta.Name == name {
			return true
		}
	}
	return false
}

// pvcsNotDeleting returns the names of the data PVCs that are not being
// deleted (no deletionTimestamp).
func (l *Legacy) pvcsNotDeleting() []string {
	var names []string
	for _, p := range l.PVCs {
		if !p.Meta.Terminating() {
			names = append(names, p.Meta.Name)
		}
	}
	return names
}

// CopiablePVCsNotDeleting is pvcsNotDeleting without the uncopiable PVCs:
// their objects were never reachable by a copy, and the removal plan says
// they are deleted, so they do not stop a removal that got past the copy.
func (l *Legacy) CopiablePVCsNotDeleting() []string {
	skip := map[string]bool{}
	for _, n := range l.Uncopiable() {
		skip[n] = true
	}
	var names []string
	for _, n := range l.pvcsNotDeleting() {
		if !skip[n] {
			names = append(names, n)
		}
	}
	return names
}

// anyPVCTerminating reports whether a data PVC is being deleted.
func (l *Legacy) anyPVCTerminating() bool {
	for _, p := range l.PVCs {
		if p.Meta.Terminating() {
			return true
		}
	}
	return false
}

// Uncopiable returns the data PVCs whose objects no server of the earlier
// storage serves, so a copy through its Service cannot reach them: MinIO's
// volume once SeaweedFS replaced MinIO (2.1 to 2.3 kept it, unused).
func (l *Legacy) Uncopiable() []string {
	if l.Server != "MinIO" && l.hasPVC(LegacyMinIOPVC) {
		return []string{LegacyMinIOPVC}
	}
	return nil
}

// LegacyState converts the earlier storage into its status entry;
// dspas are the pipeline servers, to name those that still use it, and
// newReady tells whether the new storage is ready (see MigrateS3Storage).
//
// The stage is "remove" only on proof that the data moved: Migrate storage
// recorded a verified copy (copied, migrationCopiedAnnotation), nothing is
// left to move, and every pipeline server of this tool on the new storage
// reaches it (DSPO's ObjectStoreAvailable). Until then it stays "migrate",
// so Migrate storage finishes the job.
func LegacyState(l *Legacy, dspas []DSPA, rec PipelineRecord, newReady, copied bool) *types.LegacyStorageState {
	if l == nil {
		return nil
	}
	state := &types.LegacyStorageState{
		Namespace:    LegacyNamespace,
		Server:       l.Server,
		Ready:        l.Ready,
		Endpoint:     LegacyS3Host(),
		Terminating:  l.terminating,
		UncopiedPVCs: l.Uncopiable(),
	}
	for _, o := range l.Objects {
		state.Objects = append(state.Objects, o.Kind+" "+o.Name)
	}
	for _, p := range l.PVCs {
		state.DataPVCs = append(state.DataPVCs, types.KeptPVC{Name: p.Meta.Name, Size: p.Size})
	}
	for _, d := range dspas {
		if !l.Address.UsedBy(d.Meta.Namespace, d.Host) {
			continue
		}
		name := d.Meta.Namespace + "/" + d.Meta.Name
		if !d.Meta.Terminating() {
			state.UsedBy = append(state.UsedBy, name)
		}
		if why, ok := Repointable(d); ok {
			state.Repoint = append(state.Repoint, name)
			if !rec.Owns(d.Meta) {
				state.Unrecorded = append(state.Unrecorded, name)
			}
		} else {
			state.NotMoved = append(state.NotMoved, name+" ("+why+")")
		}
	}
	sort.Strings(state.UsedBy)
	sort.Strings(state.Repoint)
	sort.Strings(state.NotMoved)
	sort.Strings(state.Unrecorded)
	movedReady := true
	for _, d := range dspas {
		if d.ManagedByTool() && !d.Meta.Terminating() && !l.Address.UsedBy(d.Meta.Namespace, d.Host) && !d.objectStoreReady() {
			movedReady = false
		}
	}
	state.Stage = "migrate"
	if newReady && copied && movedReady && len(state.Repoint) == 0 {
		state.Stage = "remove"
		state.RemoveBlockedReason = S3TeardownBlocker(dspas, l.Address)
	}
	state.NamespaceDeletable, state.NamespaceKeptReason = LegacyNamespaceDeletable(l.nsMeta)
	state.OtherObjects = l.others
	return state
}
