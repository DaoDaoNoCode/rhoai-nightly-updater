package cluster

import (
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Storage from earlier versions, in namespace minio: MinIO (up to 2.0), or
// SeaweedFS behind the MinIO-era names (2.1 to 2.3). Its names are used only
// to detect it, to read its keys and address for Migrate storage, and to
// delete what the tool created there. Nothing new is ever created in it.
const (
	legacyNamespace   = "minio"
	legacyService     = "minio-service"
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
	legacySeaweedFSPVC = "seaweedfs-pvc"
	// MinIO's objects. 2.1 replaced the Deployment and kept the PVC (MinIO's
	// on-disk format, which only a MinIO server can read).
	legacyMinIODeployment    = "minio"
	legacyMinIOPVC           = "minio-pvc"
	legacyMinIONetworkPolicy = "minio-ingress"
)

// legacyS3Host is the in-cluster S3 endpoint of the earlier storage.
func legacyS3Host() string {
	return fmt.Sprintf("%s.%s.svc:%d", legacyService, legacyNamespace, legacyServicePort)
}

// legacyStorageObjects are the objects earlier versions created in
// namespace minio, in the order teardown deletes them: the Deployments
// first, so their pods release the PVCs (whose kubernetes.io/pvc-protection
// finalizer waits for that). Remove old storage, which must stay resumable
// after the copy, uses its own order (legacyRemovalPhases).
var legacyStorageObjects = []struct{ kind, name string }{
	{"Deployment", s3Deployment},
	{"Deployment", legacyMinIODeployment},
	{"NetworkPolicy", s3NetworkPolicy},
	{"NetworkPolicy", legacyMinIONetworkPolicy},
	{"Route", legacyUIRoute},
	{"Route", legacyAPIRoute},
	{"Service", legacyService},
	{"Secret", legacySecret},
	{"PersistentVolumeClaim", legacySeaweedFSPVC},
	{"PersistentVolumeClaim", legacyMinIOPVC},
}

// legacyGetPath returns the path a named object of the earlier storage is
// read at. The literal names keep the RBAC rules limited to them. PVCs are
// listed, not read by name.
func legacyGetPath(kind, name string) (string, bool) {
	switch {
	case kind == "Secret" && name == legacySecret:
		return namespacedPath("v1", "secrets", legacyNamespace, legacySecret), true
	case kind == "Deployment" && name == s3Deployment:
		return namespacedPath("apps/v1", "deployments", legacyNamespace, s3Deployment), true
	case kind == "Deployment" && name == legacyMinIODeployment:
		return namespacedPath("apps/v1", "deployments", legacyNamespace, legacyMinIODeployment), true
	case kind == "Service" && name == legacyService:
		return namespacedPath("v1", "services", legacyNamespace, legacyService), true
	case kind == "NetworkPolicy" && name == s3NetworkPolicy:
		return namespacedPath("networking.k8s.io/v1", "networkpolicies", legacyNamespace, s3NetworkPolicy), true
	case kind == "NetworkPolicy" && name == legacyMinIONetworkPolicy:
		return namespacedPath("networking.k8s.io/v1", "networkpolicies", legacyNamespace, legacyMinIONetworkPolicy), true
	case kind == "Route" && name == legacyUIRoute:
		return namespacedPath("route.openshift.io/v1", "routes", legacyNamespace, legacyUIRoute), true
	case kind == "Route" && name == legacyAPIRoute:
		return namespacedPath("route.openshift.io/v1", "routes", legacyNamespace, legacyAPIRoute), true
	}
	return "", false
}

// legacyDeletePath returns the path of an object of the earlier storage
// that teardown (or the end of Migrate storage) may delete.
func legacyDeletePath(kind, name string) (string, bool) {
	switch {
	case kind == "PersistentVolumeClaim" && name == legacySeaweedFSPVC:
		return namespacedPath("v1", "persistentvolumeclaims", legacyNamespace, legacySeaweedFSPVC), true
	case kind == "PersistentVolumeClaim" && name == legacyMinIOPVC:
		return namespacedPath("v1", "persistentvolumeclaims", legacyNamespace, legacyMinIOPVC), true
	case kind == "Secret" && name == legacySecret:
		return namespacedPath("v1", "secrets", legacyNamespace, legacySecret), true
	case kind == "Deployment" && name == s3Deployment:
		return namespacedPath("apps/v1", "deployments", legacyNamespace, s3Deployment), true
	case kind == "Deployment" && name == legacyMinIODeployment:
		return namespacedPath("apps/v1", "deployments", legacyNamespace, legacyMinIODeployment), true
	case kind == "Service" && name == legacyService:
		return namespacedPath("v1", "services", legacyNamespace, legacyService), true
	case kind == "NetworkPolicy" && name == s3NetworkPolicy:
		return namespacedPath("networking.k8s.io/v1", "networkpolicies", legacyNamespace, s3NetworkPolicy), true
	case kind == "NetworkPolicy" && name == legacyMinIONetworkPolicy:
		return namespacedPath("networking.k8s.io/v1", "networkpolicies", legacyNamespace, legacyMinIONetworkPolicy), true
	case kind == "Route" && name == legacyUIRoute:
		return namespacedPath("route.openshift.io/v1", "routes", legacyNamespace, legacyUIRoute), true
	case kind == "Route" && name == legacyAPIRoute:
		return namespacedPath("route.openshift.io/v1", "routes", legacyNamespace, legacyAPIRoute), true
	}
	return "", false
}

// readLegacyObject reads one named object of the earlier storage.
func readLegacyObject(c *Client, kind, name string) (objectMeta, bool, error) {
	if kind == "PersistentVolumeClaim" {
		p, found, err := findPVC(c, legacyNamespace, name)
		return p.Meta, found, err
	}
	path, ok := legacyGetPath(kind, name)
	if !ok {
		return objectMeta{}, false, fmt.Errorf("internal error: unexpected object %s %s of the earlier storage", kind, name)
	}
	return readObjectMeta(c, path)
}

// legacyObject is an object of the earlier storage that this tool created.
type legacyObject struct {
	kind, name string
	meta       objectMeta
}

// legacyStorage is the storage an earlier version left in namespace minio.
type legacyStorage struct {
	terminating bool       // the namespace is being deleted
	nsMeta      objectMeta // the namespace's metadata
	// objects are the objects this tool created there, in
	// legacyStorageObjects order.
	objects []legacyObject
	// pvcs are the data PVCs among them, with their sizes.
	pvcs []storagePVC
	// server is the server that runs there ("SeaweedFS" or "MinIO"), or ""
	// when neither Deployment is left; ready is true when it has a ready
	// replica.
	server string
	ready  bool
	// serviceFound is true while Service minio-service exists (whoever
	// created it): the copy Job reaches the server through it.
	serviceFound bool
	// others are the Deployments and PVCs in the namespace this tool did
	// not create ("Deployment name"): what deleting the namespace would
	// also delete, as far as the tool can see (it cannot list every kind).
	others []string
	// address is how pipeline servers may reach it; addressErr is set when
	// its Routes or Service could not be read.
	address    storageAddress
	addressErr error
}

// projectRequesterAnnotation is set on every namespace a ProjectRequest
// made (`oc new-project`), to the user who asked for it.
const projectRequesterAnnotation = "openshift.io/requester"

// legacyNamespaceOurs reports whether namespace minio is the one this tool
// made. Any user who may request projects can make a namespace minio and
// fill it with objects that carry the tool's label and field manager, so
// the objects' own markers prove nothing; the namespace's do:
//   - the tool label: 2.x created the namespace with it and labelled the
//     ones earlier versions made (v2.3.3 minio.go); only someone who may
//     patch namespaces (a cluster-admin, or this tool's ServiceAccount of
//     those versions) can set it, never a project's own admin
//     (SECURITY.md, inference gateway);
//   - else the earliest versions' plain POST (manager Go-http-client), but
//     never on a namespace a ProjectRequest made (openshift.io/requester).
func legacyNamespaceOurs(meta objectMeta) bool {
	if meta.hasToolLabel() {
		return true
	}
	if meta.Annotations[projectRequesterAnnotation] != "" {
		return false
	}
	return meta.hasManager([]string{legacyPostManager})
}

// readLegacyStorage reports the storage an earlier version left in
// namespace minio, or nil when there is none: no such namespace, a
// namespace this tool did not make (legacyNamespaceOurs), or none of the
// names in legacyStorageObjects belongs to an object this tool created
// (the ownership label, or an earlier version's server-side apply). Any
// read error is returned (fail closed), except for the addresses, which
// only the dependency checks need (addressErr).
func readLegacyStorage(c *Client) (*legacyStorage, error) {
	l, _, err := readLegacyStorageOwner(c)
	return l, err
}

// readLegacyStorageOwner is readLegacyStorage, and also reports a
// namespace minio this tool did not make (foreign), for the status.
func readLegacyStorageOwner(c *Client) (*legacyStorage, bool, error) {
	body, exists, err := c.getOptional("/api/v1/namespaces/" + legacyNamespace)
	if err != nil {
		return nil, false, fmt.Errorf("read namespace %s: %w", legacyNamespace, err)
	}
	if !exists {
		return nil, false, nil
	}
	var nsObj struct {
		Metadata objectMeta `json:"metadata"`
		Status   struct {
			Phase string `json:"phase"`
		} `json:"status"`
	}
	if err := json.Unmarshal(body, &nsObj); err != nil {
		return nil, false, fmt.Errorf("parse namespace %s: %w", legacyNamespace, err)
	}
	if !legacyNamespaceOurs(nsObj.Metadata) {
		return nil, true, nil
	}

	type result struct {
		meta  objectMeta
		found bool
		body  []byte
		err   error
	}
	results := make([]result, len(legacyStorageObjects))
	var (
		wg        sync.WaitGroup
		pvcs      []storagePVC
		pvcErr    error
		address   storageAddress
		addrErr   error
		deploys   []objectMeta
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
			b, present, err := c.getOptional(path)
			switch {
			case err != nil:
				results[i].err = err
			case !present:
			default:
				results[i].meta, results[i].err = parseObjectMeta(b)
				results[i].found, results[i].body = results[i].err == nil, b
			}
		}(i, o.kind, o.name)
	}
	wg.Add(3)
	go func() { defer wg.Done(); pvcs, pvcErr = listPVCs(c, legacyNamespace) }()
	go func() { defer wg.Done(); deploys, deployErr = listDeploymentMetas(c, legacyNamespace) }()
	go func() { defer wg.Done(); address, addrErr = readLegacyAddress(c) }()
	wg.Wait()

	if pvcErr != nil {
		return nil, false, fmt.Errorf("list PVCs in namespace %s: %w", legacyNamespace, pvcErr)
	}
	l := &legacyStorage{terminating: nsObj.Metadata.terminating() || nsObj.Status.Phase == "Terminating", nsMeta: nsObj.Metadata, address: address, addressErr: addrErr}
	deployments := map[string]s3DeploymentSpec{}
	for i, o := range legacyStorageObjects {
		r := results[i]
		if o.kind == "PersistentVolumeClaim" {
			for _, p := range pvcs {
				if p.Meta.Name == o.name && storageObjectOwned(p.Meta) {
					l.objects = append(l.objects, legacyObject{o.kind, o.name, p.Meta})
					l.pvcs = append(l.pvcs, p)
				}
			}
			continue
		}
		if r.err != nil {
			return nil, false, fmt.Errorf("read %s %s/%s: %w", o.kind, legacyNamespace, o.name, r.err)
		}
		if o.kind == "Service" && r.found {
			l.serviceFound = true
		}
		if !r.found || !storageObjectOwned(r.meta) {
			continue
		}
		l.objects = append(l.objects, legacyObject{o.kind, o.name, r.meta})
		if o.kind == "Deployment" {
			var d s3DeploymentSpec
			if json.Unmarshal(r.body, &d) == nil {
				deployments[o.name] = d
			}
		}
	}
	if len(l.objects) == 0 {
		return nil, false, nil
	}
	tool := map[string]bool{}
	for _, o := range l.objects {
		tool[o.kind+" "+o.name] = true
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
	case hasKey(deployments, s3Deployment):
		l.server, l.ready = "SeaweedFS", deployments[s3Deployment].Status.ReadyReplicas >= 1
	case hasKey(deployments, legacyMinIODeployment):
		l.server, l.ready = "MinIO", deployments[legacyMinIODeployment].Status.ReadyReplicas >= 1
	}
	return l, false, nil
}

// listDeploymentMetas lists the metadata of a namespace's Deployments.
func listDeploymentMetas(c *Client, namespace string) ([]objectMeta, error) {
	var items []struct {
		Metadata objectMeta `json:"metadata"`
	}
	if err := c.listInto(namespacedPath("apps/v1", "deployments", namespace, ""), &items); err != nil {
		return nil, err
	}
	out := make([]objectMeta, 0, len(items))
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
func readLegacyAddress(c *Client) (storageAddress, error) {
	a := storageAddress{service: legacyService, namespace: legacyNamespace}
	api, err := readRoute(c, legacyNamespace, legacyAPIRoute)
	if err != nil {
		return a, err
	}
	ui, err := readRoute(c, legacyNamespace, legacyUIRoute)
	if err != nil {
		return a, err
	}
	a.routeHosts = []string{api.host, ui.host}
	svc, err := readService(c, legacyNamespace, legacyService)
	if err != nil {
		return a, err
	}
	a.clusterIP = svc.clusterIP
	return a, nil
}

// hasPVC reports whether the earlier storage holds the named data PVC.
func (l *legacyStorage) hasPVC(name string) bool {
	for _, p := range l.pvcs {
		if p.Meta.Name == name {
			return true
		}
	}
	return false
}

// pvcsNotDeleting returns the names of the data PVCs that are not being
// deleted (no deletionTimestamp).
func (l *legacyStorage) pvcsNotDeleting() []string {
	var names []string
	for _, p := range l.pvcs {
		if !p.Meta.terminating() {
			names = append(names, p.Meta.Name)
		}
	}
	return names
}

// copiablePVCsNotDeleting is pvcsNotDeleting without the uncopiable PVCs:
// their objects were never reachable by a copy, and the removal plan says
// they are deleted, so they do not stop a removal that got past the copy.
func (l *legacyStorage) copiablePVCsNotDeleting() []string {
	skip := map[string]bool{}
	for _, n := range l.uncopiable() {
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
func (l *legacyStorage) anyPVCTerminating() bool {
	for _, p := range l.pvcs {
		if p.Meta.terminating() {
			return true
		}
	}
	return false
}

// uncopiable returns the data PVCs whose objects no server of the earlier
// storage serves, so a copy through its Service cannot reach them: MinIO's
// volume once SeaweedFS replaced MinIO (2.1 to 2.3 kept it, unused).
func (l *legacyStorage) uncopiable() []string {
	if l.server != "MinIO" && l.hasPVC(legacyMinIOPVC) {
		return []string{legacyMinIOPVC}
	}
	return nil
}

// legacyStorageState converts the earlier storage into its status entry;
// dspas are the pipeline servers, to name those that still use it, and
// newReady tells whether the new storage is ready (see MigrateS3Storage).
//
// The stage is "remove" only on proof that the data moved: Migrate storage
// recorded a verified copy (copied, migrationCopiedAnnotation), nothing is
// left to move, and every pipeline server of this tool on the new storage
// reaches it (DSPO's ObjectStoreAvailable). Until then it stays "migrate",
// so Migrate storage finishes the job.
func legacyStorageState(l *legacyStorage, dspas []dspaInfo, rec pipelineRecord, newReady, copied bool) *types.LegacyStorageState {
	if l == nil {
		return nil
	}
	state := &types.LegacyStorageState{
		Namespace:    legacyNamespace,
		Server:       l.server,
		Ready:        l.ready,
		Endpoint:     legacyS3Host(),
		Terminating:  l.terminating,
		UncopiedPVCs: l.uncopiable(),
	}
	for _, o := range l.objects {
		state.Objects = append(state.Objects, o.kind+" "+o.name)
	}
	for _, p := range l.pvcs {
		state.DataPVCs = append(state.DataPVCs, types.KeptPVC{Name: p.Meta.Name, Size: p.Size})
	}
	for _, d := range dspas {
		if !l.address.usedBy(d.Meta.Namespace, d.Host) {
			continue
		}
		name := d.Meta.Namespace + "/" + d.Meta.Name
		if !d.Meta.terminating() {
			state.UsedBy = append(state.UsedBy, name)
		}
		if why, ok := repointable(d); ok {
			state.Repoint = append(state.Repoint, name)
			if !rec.owns(d.Meta) {
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
		if d.managedByTool() && !d.Meta.terminating() && !l.address.usedBy(d.Meta.Namespace, d.Host) && !d.objectStoreReady() {
			movedReady = false
		}
	}
	state.Stage = "migrate"
	if newReady && copied && movedReady && len(state.Repoint) == 0 {
		state.Stage = "remove"
		state.RemoveBlockedReason = s3TeardownBlocker(dspas, l.address)
	}
	state.NamespaceDeletable, state.NamespaceKeptReason = legacyNamespaceDeletable(l.nsMeta)
	state.OtherObjects = l.others
	return state
}
