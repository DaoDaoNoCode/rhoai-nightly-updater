package storage

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The S3 storage: SeaweedFS ("weed mini", a single-process master, volume,
// filer, S3 gateway and admin UI) in its own namespace. Storage that earlier
// versions set up in namespace minio is handled in s3_legacy.go.
const (
	S3Namespace     = "seaweedfs"
	s3Bucket        = "pipelines"
	s3Service       = "seaweedfs-s3"
	S3Secret        = "seaweedfs-s3-credentials"
	s3AdminRoute    = "seaweedfs-admin"
	S3Deployment    = "seaweedfs"
	s3PVC           = "seaweedfs-data"
	s3NetworkPolicy = "seaweedfs-ingress"
	s3Container     = "seaweedfs"
	s3AppLabel      = "seaweedfs"
	s3AppSelector   = "app=seaweedfs"
	s3Storage       = "20Gi"
	// S3Region is the region clients sign requests for. SeaweedFS accepts
	// any; Data Science Pipelines and boto3 need one set.
	S3Region = "us-east-1"
	// The Secret's keys are the standard AWS variable names, so the Secret
	// can be used as is (envFrom) and pipeline servers copy them unchanged.
	S3AccessKeyField = "AWS_ACCESS_KEY_ID"
	S3SecretKeyField = "AWS_SECRET_ACCESS_KEY"
	// s3AdminPasswordField holds the admin UI password, a random value of
	// its own. Only Secret seaweedfs/seaweedfs-s3-credentials holds it: the
	// copies in pipeline projects (nightly-dspa-s3) carry only the S3 keys.
	s3AdminPasswordField = "WEED_ADMIN_PASSWORD"
	// s3Port is SeaweedFS's S3 port (weed/command/mini.go "s3.port") and
	// s3AdminPort its admin UI ("admin.port"). SeaweedFS 4.49 moved the
	// admin default from 23646 to 12535, because the admin's gRPC port
	// (HTTP port + 10000) then sat in Linux's ephemeral port range and could
	// be taken at startup (seaweedfs/seaweedfs#11816, fixing #10838). The
	// port is passed explicitly, so a taken port stops weed mini instead of
	// moving the UI away from the Service. The Service uses the same port
	// numbers, named s3 and ui.
	s3Port      = 8333
	s3AdminPort = 12535
	// S3AdminUser is the admin UI login: the default of -admin.user
	// (weed/command/mini.go:603 at 4.54), set explicitly below.
	S3AdminUser = "admin"
	// SeaweedFSDefaultImage is SeaweedFS 4.54
	// (ghcr.io/chrislusf/seaweedfs:4.54, Apache-2.0), pinned by its
	// multi-arch index digest (amd64, arm64, arm/v7, 386, ppc64le, s390x).
	// It is anonymously pullable. Upstream Kubeflow Pipelines made SeaweedFS
	// its default object store in 2.15.0 (kubeflow/pipelines#11965); its
	// OpenShift overlay lets restricted-v2 assign the UID (#12314), so no
	// runAsUser is set here either. The image's entrypoint.sh drops to user
	// "seaweed" only when started as root; under an assigned UID it runs
	// weed directly, and /data must be writable, which the fsGroup
	// restricted-v2 sets on the PVC provides. The image's
	// /etc/seaweedfs/filer.toml keeps the filer metadata in /data/filerldb2
	// whatever -dir says (weed/command/mini.go:392-397 at 4.54); /data is
	// the PVC here, so the metadata survives a restart.
	SeaweedFSDefaultImage = "ghcr.io/chrislusf/seaweedfs@sha256:4b42c212543a0acd4c0e675215ecdcaf62a186ef98def76c7e12701871222d15"
)

var (
	// S3ReadyTimeout bounds how long setup waits for the SeaweedFS pod. It
	// stays below the frontend's 120s request timeout. Tests shorten it.
	S3ReadyTimeout = 90 * time.Second
	s3ReadyPoll    = 3 * time.Second
	// s3DeleteTimeout bounds how long teardown waits for the data PVCs to
	// disappear before reporting that deletion is still in progress.
	s3DeleteTimeout = 60 * time.Second
	s3DeletePoll    = 2 * time.Second

	// s3BucketCreator creates the pipelines bucket; tests replace it
	// because the in-cluster S3 endpoint is unreachable from unit tests.
	s3BucketCreator    = createS3Bucket
	s3BucketRetryDelay = 3 * time.Second
)

// s3Image returns the SeaweedFS image, overridable with SEAWEEDFS_IMAGE
// (for example a mirror on a disconnected cluster). An override must be a
// SeaweedFS 4.x release whose "weed mini" reads the admin credentials from
// WEED_ADMIN_* (see s3Resources).
func s3Image() string {
	if img := strings.TrimSpace(os.Getenv("SEAWEEDFS_IMAGE")); img != "" {
		return img
	}
	return SeaweedFSDefaultImage
}

// S3Host is the in-cluster S3 endpoint pipeline servers use.
func S3Host() string {
	return fmt.Sprintf("%s.%s.svc:%d", s3Service, S3Namespace, s3Port)
}

// S3Endpoint is s3Host as a URL.
func S3Endpoint() string {
	return "http://" + S3Host()
}

// S3BucketEndpoint is where the app itself sends its one S3 request (creating
// the bucket): the Service, s3Endpoint. A dev-mode backend runs on a
// workstation that cannot resolve cluster DNS, so dev.sh keeps a port-forward
// to the Service open and points DEV_S3_ENDPOINT at it. The override is used
// only with DEV_MODE=true and only for a loopback http URL, so the storage
// keys are never sent to any other address. Pipeline servers and the copy Job
// always get s3Endpoint.
func S3BucketEndpoint(getenv func(string) string) string {
	raw := getenv("DEV_S3_ENDPOINT")
	if getenv("DEV_MODE") != "true" || raw == "" {
		return S3Endpoint()
	}
	u, err := url.Parse(raw)
	if err == nil && u.Scheme == "http" && (u.Path == "" || u.Path == "/") && u.RawQuery == "" && u.User == nil && u.Port() != "" {
		if ip := net.ParseIP(u.Hostname()); (ip != nil && ip.IsLoopback()) || u.Hostname() == "localhost" {
			return "http://" + u.Host
		}
	}
	slog.Warn("DEV_S3_ENDPOINT ignored: it must be a loopback http URL with a port, such as http://127.0.0.1:18333", "value", raw)
	return S3Endpoint()
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// S3Keys are an S3 access key and secret key, and the admin UI password
// (set only for the storage's own Secret).
type S3Keys struct {
	AccessKey     string
	SecretKey     string
	AdminPassword string
}

// ReadS3Keys reads the keys stored in the storage's Secret. found is false
// when the Secret does not exist.
func ReadS3Keys(c *kube.Client) (keys S3Keys, found bool, err error) {
	var secret struct {
		Data map[string]string `json:"data"`
	}
	exists, err := c.GetIntoOptional(rhoai.NamespacedPath("v1", "secrets", S3Namespace, S3Secret), &secret)
	if err != nil {
		if kube.IsDecodeError(err) {
			return S3Keys{}, false, fmt.Errorf("parse Secret %s/%s: %w", S3Namespace, S3Secret, err)
		}
		return S3Keys{}, false, fmt.Errorf("read Secret %s/%s: %w", S3Namespace, S3Secret, err)
	}
	if !exists {
		return S3Keys{}, false, nil
	}
	return S3Keys{
		AccessKey: DecodeBase64Field(secret.Data[S3AccessKeyField]), SecretKey: DecodeBase64Field(secret.Data[S3SecretKeyField]),
		AdminPassword: DecodeBase64Field(secret.Data[s3AdminPasswordField]),
	}, true, nil
}

// S3AdminLoginUnavailable is why S3AdminLogin offers no password: the
// storage is not set up, or this tool did not create it.
type S3AdminLoginUnavailable struct{ Reason string }

func (e *S3AdminLoginUnavailable) Error() string { return e.Reason }

// S3Login is the S3 storage's credentials: the admin UI user and password,
// and the S3 access key and secret key.
type S3Login struct {
	User      string `json:"user"`
	Password  string `json:"password"`
	AccessKey string `json:"accessKey"`
	SecretKey string `json:"secretKey"`
}

// S3AdminLogin returns the S3 storage's credentials: the admin UI user
// s3AdminUser and its own password (WEED_ADMIN_PASSWORD, see s3Resources),
// and the S3 keys. It reads the storage's Secret on each call, so they are
// never part of the status. It answers only for storage this tool set up:
// the namespace carries the tool label and the tool wrote the Secret. The
// caller must keep them out of logs and caches.
func S3AdminLogin(c *kube.Client) (login S3Login, err error) {
	unavailable := func(reason string) (S3Login, error) { return S3Login{}, &S3AdminLoginUnavailable{reason} }
	ns, err := lookupS3Namespace(c)
	if err != nil {
		return S3Login{}, err
	}
	switch {
	case !ns.Exists:
		return unavailable("The S3 storage is not set up.")
	case !ns.Managed:
		return unavailable(fmt.Sprintf("Namespace %s was not created by this tool, so it does not read the password there.", S3Namespace))
	}
	var secret struct {
		Metadata kube.ObjectMeta   `json:"metadata"`
		Data     map[string]string `json:"data"`
	}
	found, err := c.GetIntoOptional(rhoai.NamespacedPath("v1", "secrets", S3Namespace, S3Secret), &secret)
	if err != nil {
		if kube.IsDecodeError(err) {
			return S3Login{}, fmt.Errorf("parse Secret %s/%s: %w", S3Namespace, S3Secret, err)
		}
		return S3Login{}, fmt.Errorf("read Secret %s/%s: %w", S3Namespace, S3Secret, err)
	}
	if !found {
		return unavailable(fmt.Sprintf("Secret %s does not exist in namespace %s. Repair re-creates it.", S3Secret, S3Namespace))
	}
	if !storageObjectOwned(secret.Metadata) {
		return unavailable(fmt.Sprintf("Secret %s in namespace %s was not created by this tool, so it does not read it.", S3Secret, S3Namespace))
	}
	login = S3Login{
		User: S3AdminUser, Password: DecodeBase64Field(secret.Data[s3AdminPasswordField]),
		AccessKey: DecodeBase64Field(secret.Data[S3AccessKeyField]), SecretKey: DecodeBase64Field(secret.Data[S3SecretKeyField]),
	}
	if login.SecretKey == "" {
		return unavailable(fmt.Sprintf("Secret %s has no %s. Repair writes it again.", S3Secret, S3SecretKeyField))
	}
	// Password is "" on storage an earlier version set up (its admin UI
	// used the S3 secret key) until its next Repair; the UI says so.
	return login, nil
}

// S3Credentials returns the keys a setup writes to the storage's Secret.
// Keys already stored there are kept, because pipeline servers hold copies
// of them. On a first setup, SEAWEEDFS_ACCESS_KEY_ID and
// SEAWEEDFS_SECRET_ACCESS_KEY set them; otherwise both are random, so no
// credential is baked into the binary or guessable. The admin UI password
// is kept too, or generated (withAdminPassword).
func S3Credentials(c *kube.Client) (S3Keys, error) {
	stored, found, err := ReadS3Keys(c)
	if err != nil {
		return S3Keys{}, err
	}
	keys := S3Keys{AccessKey: os.Getenv("SEAWEEDFS_ACCESS_KEY_ID"), SecretKey: os.Getenv("SEAWEEDFS_SECRET_ACCESS_KEY")}
	if found {
		keys.AccessKey = kube.FirstNonEmpty(stored.AccessKey, keys.AccessKey)
		keys.SecretKey = kube.FirstNonEmpty(stored.SecretKey, keys.SecretKey)
		keys.AdminPassword = stored.AdminPassword
	}
	if keys.AccessKey == "" {
		suffix, err := randomHex(6)
		if err != nil {
			return S3Keys{}, fmt.Errorf("generate S3 access key: %w", err)
		}
		keys.AccessKey = "seaweedfs-" + suffix
	}
	if keys.SecretKey == "" {
		if keys.SecretKey, err = randomHex(16); err != nil {
			return S3Keys{}, fmt.Errorf("generate S3 secret key: %w", err)
		}
	}
	return withAdminPassword(c, keys)
}

// withAdminPassword gives keys the admin UI password: the one stored in the
// storage's Secret, else a new random one. Storage an earlier version set
// up used the S3 secret key there, which every pipeline project's copy of
// the keys holds; its next setup or Repair writes a password of its own.
func withAdminPassword(c *kube.Client, keys S3Keys) (S3Keys, error) {
	if keys.AdminPassword != "" {
		return keys, nil
	}
	stored, found, err := ReadS3Keys(c)
	if err != nil {
		return S3Keys{}, err
	}
	if found && stored.AdminPassword != "" && stored.AdminPassword != keys.SecretKey {
		keys.AdminPassword = stored.AdminPassword
		return keys, nil
	}
	if keys.AdminPassword, err = randomHex(16); err != nil {
		return S3Keys{}, fmt.Errorf("generate the admin UI password: %w", err)
	}
	return keys, nil
}

// s3NamespaceInfo describes the storage namespace and whether this tool
// created it (it carries the tool label).
type s3NamespaceInfo struct {
	Exists  bool
	Meta    kube.ObjectMeta
	Phase   string
	Managed bool
}

func (n s3NamespaceInfo) terminating() bool {
	return n.Meta.Terminating() || n.Phase == "Terminating"
}

// lookupS3Namespace reads the storage namespace.
func lookupS3Namespace(c *kube.Client) (s3NamespaceInfo, error) {
	var nsObj struct {
		Metadata kube.ObjectMeta `json:"metadata"`
		Status   struct {
			Phase string `json:"phase"`
		} `json:"status"`
	}
	found, err := c.GetIntoOptional("/api/v1/namespaces/"+S3Namespace, &nsObj)
	if err != nil {
		if kube.IsDecodeError(err) {
			return s3NamespaceInfo{}, fmt.Errorf("parse namespace %s: %w", S3Namespace, err)
		}
		return s3NamespaceInfo{}, fmt.Errorf("read namespace %s: %w", S3Namespace, err)
	}
	if !found {
		return s3NamespaceInfo{}, nil
	}
	return s3NamespaceInfo{Exists: true, Meta: nsObj.Metadata, Phase: nsObj.Status.Phase, Managed: nsObj.Metadata.HasToolLabel()}, nil
}

// s3Leftovers names this tool's storage objects (besides the Deployment,
// the PVC and the Route, which the status reads anyway) still in namespace
// seaweedfs: what a teardown that did not finish left. One that cannot be
// read is not named (the next read looks again).
func s3Leftovers(c *kube.Client) []string {
	var out []string
	for _, o := range []struct{ kind, name, path string }{
		{"NetworkPolicy", s3NetworkPolicy, rhoai.NamespacedPath("networking.k8s.io/v1", "networkpolicies", S3Namespace, s3NetworkPolicy)},
		{"Service", s3Service, rhoai.NamespacedPath("v1", "services", S3Namespace, s3Service)},
		{"Secret", S3Secret, rhoai.NamespacedPath("v1", "secrets", S3Namespace, S3Secret)},
	} {
		meta, found, err := kube.ReadObjectMeta(c, o.path)
		if err == nil && found && storageObjectOwned(meta) {
			out = append(out, o.kind+" "+o.name)
		}
	}
	sort.Strings(out)
	return out
}

// storageObjectOwned reports whether the tool created a storage object: it
// carries the ownership label, or (an earlier version's unlabelled object)
// the tool's field manager owns its content, spec or data (see
// ownsFieldPath).
func storageObjectOwned(m kube.ObjectMeta) bool {
	tool := []string{envkit.ToolFieldManager}
	return m.HasToolLabel() || m.OwnsFieldPath(tool, "f:spec") || m.OwnsFieldPath(tool, "f:data") || m.OwnsFieldPath(tool, "f:stringData")
}

// PVC is a PVC of a storage namespace and the size of its volume.
type PVC struct {
	Meta kube.ObjectMeta
	// Size is the bound capacity, or the requested size while unbound.
	Size string
}

// listPVCs lists the PVCs of a namespace. PVCs are listed rather than read
// by name, so the RBAC rules need no "get" on them.
func listPVCs(c *kube.Client, namespace string) ([]PVC, error) {
	var items []struct {
		Metadata kube.ObjectMeta `json:"metadata"`
		Spec     struct {
			Resources struct {
				Requests map[string]string `json:"requests"`
			} `json:"resources"`
		} `json:"spec"`
		Status struct {
			Capacity map[string]string `json:"capacity"`
		} `json:"status"`
	}
	if err := c.ListInto(rhoai.NamespacedPath("v1", "persistentvolumeclaims", namespace, ""), &items); err != nil {
		if kube.IsDecodeError(err) {
			return nil, fmt.Errorf("parse PVC list: %w", err)
		}
		return nil, err
	}
	out := make([]PVC, 0, len(items))
	for _, item := range items {
		out = append(out, PVC{Meta: item.Metadata, Size: kube.FirstNonEmpty(item.Status.Capacity["storage"], item.Spec.Resources.Requests["storage"])})
	}
	return out, nil
}

// findPVC returns the named PVC of a namespace, and whether it exists.
func findPVC(c *kube.Client, namespace, name string) (PVC, bool, error) {
	pvcs, err := listPVCs(c, namespace)
	if err != nil {
		return PVC{}, false, err
	}
	for _, p := range pvcs {
		if p.Meta.Name == name {
			return p, true, nil
		}
	}
	return PVC{}, false, nil
}

// S3DeploymentSpec is the part of a storage Deployment the tool reads.
type S3DeploymentSpec struct {
	Metadata struct {
		Generation int64 `json:"generation"`
	} `json:"metadata"`
	Spec struct {
		Replicas *int `json:"replicas"`
		Template struct {
			Spec struct {
				Containers []struct {
					Name  string   `json:"name"`
					Image string   `json:"image"`
					Args  []string `json:"args"`
					Env   []struct {
						Name      string `json:"name"`
						ValueFrom *struct {
							SecretKeyRef *struct {
								Key string `json:"key"`
							} `json:"secretKeyRef"`
						} `json:"valueFrom"`
					} `json:"env"`
				} `json:"containers"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
	Status struct {
		ObservedGeneration int64 `json:"observedGeneration"`
		Replicas           int   `json:"replicas"`
		ReadyReplicas      int   `json:"readyReplicas"`
		UpdatedReplicas    int   `json:"updatedReplicas"`
	} `json:"status"`
}

// RolledOut reports whether the current template has a ready replica.
func (d S3DeploymentSpec) RolledOut() bool {
	return d.Status.ObservedGeneration >= d.Metadata.Generation &&
		d.Status.UpdatedReplicas >= 1 && d.Status.ReadyReplicas >= 1
}

// AdminPasswordIsS3Key reports whether the server's admin UI password is
// the S3 secret key, as storage set up by an earlier version has it.
func (d S3DeploymentSpec) AdminPasswordIsS3Key() bool {
	for _, c := range d.Spec.Template.Spec.Containers {
		for _, e := range c.Env {
			if e.Name == "WEED_ADMIN_PASSWORD" && e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil && e.ValueFrom.SecretKeyRef.Key == S3SecretKeyField {
				return true
			}
		}
	}
	return false
}

// Secured reports whether the server runs with IAM writes on and
// self-service off (ArgsSecured): storage a first setup or Secure S3
// storage made.
func (d S3DeploymentSpec) Secured() bool {
	for _, c := range d.Spec.Template.Spec.Containers {
		if c.Name == s3Container {
			return ArgsSecured(c.Args)
		}
	}
	return false
}

// image returns the image of the named container.
func (d S3DeploymentSpec) image(container string) string {
	for _, ctr := range d.Spec.Template.Spec.Containers {
		if ctr.Name == container {
			return ctr.Image
		}
	}
	return ""
}

// Address is how a pipeline server may reach an S3 storage: the DNS
// name of its Service in any form, the Service's ClusterIP, or the host of
// one of its Routes.
type Address struct {
	Service    string
	Namespace  string
	ClusterIP  string
	RouteHosts []string
}

// UsedBy reports whether a pipeline server in namespace dspaNamespace with
// object-storage host host talks to this storage, whatever form the
// address takes: the service DNS name in any of its forms
// (https://kubernetes.io/docs/concepts/services-networking/dns-pod-service/#namespaces-of-services),
// with or without scheme and port, the ClusterIP, or a Route host.
func (a Address) UsedBy(dspaNamespace, host string) bool {
	h := endpointHostname(host)
	if h == "" {
		return false
	}
	svc := a.Service + "." + a.Namespace
	switch h {
	case svc, svc + ".svc", svc + ".svc.cluster.local":
		return true
	case a.Service:
		// A bare service name resolves in the client's own namespace: the
		// pod DNS search path starts with <namespace>.svc.cluster.local.
		return dspaNamespace == a.Namespace
	}
	if a.ClusterIP != "" && h == a.ClusterIP {
		return true
	}
	for _, r := range a.RouteHosts {
		if r != "" && h == strings.TrimSuffix(strings.ToLower(r), ".") {
			return true
		}
	}
	return false
}

// serviceSpec is the part of a Service the tool reads.
type serviceSpec struct {
	found     bool
	clusterIP string
	selector  string            // "key=value" pairs, sorted, comma-separated
	targets   map[string]string // targetPort by port name
}

// readService reads a Service; found is false when it does not exist. Any
// other read or parse error is returned.
func readService(c *kube.Client, namespace, name string) (serviceSpec, error) {
	body, exists, err := c.GetOptional(rhoai.NamespacedPath("v1", "services", namespace, name))
	switch {
	case err != nil:
		return serviceSpec{}, fmt.Errorf("read Service %s: %w", name, err)
	case !exists:
		return serviceSpec{}, nil
	}
	var svc struct {
		Spec struct {
			ClusterIP string            `json:"clusterIP"`
			Selector  map[string]string `json:"selector"`
			Ports     []struct {
				Name       string          `json:"name"`
				Port       int             `json:"port"`
				TargetPort json.RawMessage `json:"targetPort"`
			} `json:"ports"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(body, &svc); err != nil {
		return serviceSpec{}, fmt.Errorf("parse Service %s: %w", name, err)
	}
	s := serviceSpec{found: true, targets: map[string]string{}}
	if svc.Spec.ClusterIP != "None" {
		s.clusterIP = svc.Spec.ClusterIP
	}
	pairs := make([]string, 0, len(svc.Spec.Selector))
	for k, v := range svc.Spec.Selector {
		pairs = append(pairs, k+"="+v)
	}
	sort.Strings(pairs)
	s.selector = strings.Join(pairs, ",")
	for _, p := range svc.Spec.Ports {
		s.targets[kube.FirstNonEmpty(p.Name, strconv.Itoa(p.Port))] = intOrString(p.TargetPort)
	}
	return s, nil
}

// S3Endpoints are where the storage's Service and admin UI Route send
// traffic, and the addresses pipeline servers may use to reach it.
type S3Endpoints struct {
	Address Address
	// Secured is set by S3StatusAndEndpoints from the Deployment: the
	// server runs with the secured args (S3DeploymentSpec.Secured).
	Secured bool
	svc     serviceSpec
	ui      routeSpec
}

// readS3Endpoints reads the Service and the admin UI Route. A missing object
// (404) is not an error. Any other read or parse error is returned, because
// a caller that deletes the storage must not miss a pipeline server that
// uses one of its addresses; it refuses instead (fail closed).
func readS3Endpoints(c *kube.Client) (S3Endpoints, error) {
	ui, err := readRoute(c, S3Namespace, s3AdminRoute)
	if err != nil {
		return S3Endpoints{}, err
	}
	svc, err := readService(c, S3Namespace, s3Service)
	if err != nil {
		return S3Endpoints{}, err
	}
	return S3Endpoints{
		Address: Address{Service: s3Service, Namespace: S3Namespace, ClusterIP: svc.clusterIP, RouteHosts: []string{ui.host}},
		svc:     svc,
		ui:      ui,
	}, nil
}

// servingProblem explains why the Service or the admin UI Route does not
// send traffic to SeaweedFS, or returns "".
func (e S3Endpoints) servingProblem() string {
	switch {
	case !e.svc.found:
		return fmt.Sprintf("Service %s is missing, so pipeline servers cannot reach SeaweedFS.", s3Service)
	case e.svc.selector != s3AppSelector:
		return fmt.Sprintf("Service %s selects %s instead of SeaweedFS (%s), so pipeline servers do not reach SeaweedFS.", s3Service, kube.FirstNonEmpty(e.svc.selector, "no pods"), s3AppSelector)
	case e.svc.targets["s3"] != strconv.Itoa(s3Port):
		return fmt.Sprintf("Service %s port s3 forwards to %s instead of SeaweedFS's S3 port %d.", s3Service, kube.FirstNonEmpty(e.svc.targets["s3"], "nothing"), s3Port)
	case e.svc.targets["ui"] != strconv.Itoa(s3AdminPort):
		return fmt.Sprintf("Service %s port ui forwards to %s instead of SeaweedFS's admin UI port %d.", s3Service, kube.FirstNonEmpty(e.svc.targets["ui"], "nothing"), s3AdminPort)
	case !e.ui.found:
		return fmt.Sprintf("Route %s (the admin UI) is missing.", s3AdminRoute)
	case e.ui.toService != s3Service || e.ui.targetPort != "ui":
		return fmt.Sprintf("Route %s does not point at port ui of Service %s.", s3AdminRoute, s3Service)
	}
	return ""
}

func S3Status(c *kube.Client) types.ResourceState {
	state, _, _ := S3StatusAndEndpoints(c)
	return state
}

// S3StatusAndEndpoints returns the status of the S3 storage and its
// endpoints (for the teardown dependency check). The error is non-nil when
// the endpoints could not be read. It does not look at storage an earlier
// version set up in namespace minio (see readLegacyStorage).
func S3StatusAndEndpoints(c *kube.Client) (types.ResourceState, S3Endpoints, error) {
	state := types.ResourceState{Namespace: S3Namespace}

	ns, err := lookupS3Namespace(c)
	if err != nil {
		state.Message = "Cannot read the S3 storage status: " + err.Error()
		return state, S3Endpoints{}, nil
	}
	if !ns.Exists {
		state.Message = "Not deployed"
		return state, S3Endpoints{}, nil
	}
	state.ManagedByTool = ns.Managed
	if ns.terminating() {
		state.Terminating = true
		state.Message = "Terminating"
		state.SetupBlockedReason = fmt.Sprintf("Namespace %s is still being deleted.", S3Namespace)
		return state, S3Endpoints{}, nil
	}
	if !ns.Managed {
		state.SetupBlockedReason = fmt.Sprintf("Namespace %s exists but was not created by this tool, so setup will not modify it.", S3Namespace)
		state.TeardownBlockedReason = fmt.Sprintf("Namespace %s was not created by this tool.", S3Namespace)
	}

	var (
		wg           sync.WaitGroup
		deployBody   []byte
		deployErr    error
		endpoints    S3Endpoints
		endpointsErr error
		pvcs         []PVC
		pvcErr       error
		deploys      []kube.ObjectMeta
		deploysErr   error
	)
	wg.Add(4)
	go func() {
		defer wg.Done()
		deployBody, _, deployErr = c.Get(rhoai.NamespacedPath("apps/v1", "deployments", S3Namespace, S3Deployment))
	}()
	go func() {
		defer wg.Done()
		endpoints, endpointsErr = readS3Endpoints(c)
	}()
	go func() {
		defer wg.Done()
		if ns.Managed {
			pvcs, pvcErr = listPVCs(c, S3Namespace)
		}
	}()
	go func() {
		defer wg.Done()
		if ns.Managed {
			deploys, deploysErr = listDeploymentMetas(c, S3Namespace)
		}
	}()
	wg.Wait()
	if ns.Managed {
		state.NamespaceDeletable, state.NamespaceKeptReason = toolNamespaceDeletable(S3Namespace, ns.Meta)
		state.OtherObjects = s3OtherObjects(pvcs, pvcErr, deploys, deploysErr)
	}

	if endpoints.ui.host != "" {
		state.UIRoute = "https://" + endpoints.ui.host
	}
	if ns.Managed && pvcErr == nil {
		for _, p := range pvcs {
			if p.Meta.Name == s3PVC && storageObjectOwned(p.Meta) {
				state.DataPVCs = append(state.DataPVCs, p.Meta.Name)
			}
		}
	}

	if deployErr != nil {
		if kube.IsK8sNotFound(deployErr) {
			state.Message = "Namespace exists but the S3 storage is not deployed"
			if ns.Managed {
				state.Leftovers = s3Leftovers(c)
			}
		} else {
			state.Message = fmt.Sprintf("Cannot read the SeaweedFS deployment: %v", deployErr)
		}
		if !ns.Managed {
			state.Message = state.SetupBlockedReason
		}
		return state, endpoints, endpointsErr
	}
	state.Deployed = true

	var deploy S3DeploymentSpec
	if err := json.Unmarshal(deployBody, &deploy); err != nil {
		state.Message = "Cannot parse the SeaweedFS deployment"
		return state, endpoints, endpointsErr
	}
	state.CurrentImage = deploy.image(s3Container)
	state.UIUser = S3AdminUser
	endpoints.Secured = deploy.Secured()
	scaledDown := deploy.Spec.Replicas != nil && *deploy.Spec.Replicas == 0
	serving := ""
	if ns.Managed {
		switch {
		case scaledDown:
			state.RepairNeeded = fmt.Sprintf("Deployment %s is scaled to 0 replicas.", S3Deployment)
		case endpointsErr == nil:
			serving = endpoints.servingProblem()
			state.RepairNeeded = serving
		}
		// Another image, for example after SEAWEEDFS_IMAGE changed: re-running
		// setup (Repair) updates it in place. A more serious repair reason
		// wins; the image is then a warning.
		if state.CurrentImage != "" && state.CurrentImage != s3Image() {
			drift := fmt.Sprintf("SeaweedFS runs %s, not the image this version deploys (%s). Re-run setup to update it; the data PVC is kept.", state.CurrentImage, s3Image())
			if state.RepairNeeded == "" {
				state.RepairNeeded = drift
			} else {
				state.Warning = drift
			}
		}
		if deploy.AdminPasswordIsS3Key() && state.RepairNeeded == "" {
			state.RepairNeeded = "The admin UI's password is the S3 secret key, which every pipeline project's copy of the keys holds. Repair gives the admin UI a password of its own; the S3 keys and the data stay."
		}
		if state.RepairNeeded == "" {
			state.RepairNeeded = networkPolicyProblem(c)
		}
	}
	switch {
	case ns.Managed && scaledDown:
		// Nothing will start on its own: say so instead of "starting".
		state.Message = state.RepairNeeded
		state.TerminalError = true
		return state, endpoints, endpointsErr
	case deploy.Status.ReadyReplicas < 1:
	case serving != "":
		state.Message = serving
		state.TerminalError = true
		return state, endpoints, endpointsErr
	case ns.Managed && endpointsErr != nil:
		state.Message = fmt.Sprintf("SeaweedFS is running, but whether Service %s and Route %s point at it cannot be checked: %v", s3Service, s3AdminRoute, endpointsErr)
		return state, endpoints, endpointsErr
	default:
		state.Ready = true
		state.Message = "Running"
		return state, endpoints, endpointsErr
	}
	state.Message = fmt.Sprintf("%d/%d ready", deploy.Status.ReadyReplicas, deploy.Status.Replicas)
	if issue, err := envkit.InspectPods(c, S3Namespace, s3AppSelector, ""); err == nil {
		envkit.ApplyPodIssue(&state, issue)
	}
	return state, endpoints, endpointsErr
}

// s3ApplyPath returns the API path of one object from s3Resources. Only the
// fixed objects the tool creates are accepted; the literal names keep the
// RBAC rules limited to them (pkg/cluster/rbac_coverage_test.go).
func s3ApplyPath(obj map[string]any) (string, bool) {
	meta, _ := obj["metadata"].(map[string]any)
	name, _ := meta["name"].(string)
	switch kind, _ := obj["kind"].(string); {
	case kind == "PersistentVolumeClaim" && name == s3PVC:
		return rhoai.NamespacedPath("v1", "persistentvolumeclaims", S3Namespace, s3PVC), true
	case kind == "Secret" && name == S3Secret:
		return rhoai.NamespacedPath("v1", "secrets", S3Namespace, S3Secret), true
	case kind == "Deployment" && name == S3Deployment:
		return rhoai.NamespacedPath("apps/v1", "deployments", S3Namespace, S3Deployment), true
	case kind == "Service" && name == s3Service:
		return rhoai.NamespacedPath("v1", "services", S3Namespace, s3Service), true
	case kind == "NetworkPolicy" && name == s3NetworkPolicy:
		return rhoai.NamespacedPath("networking.k8s.io/v1", "networkpolicies", S3Namespace, s3NetworkPolicy), true
	case kind == "Route" && name == s3AdminRoute:
		return rhoai.NamespacedPath("route.openshift.io/v1", "routes", S3Namespace, s3AdminRoute), true
	}
	return "", false
}

// s3CreatePath returns the collection path an s3Resources object is created
// at with POST.
func s3CreatePath(kind string) (string, bool) {
	switch kind {
	case "PersistentVolumeClaim":
		return envkit.WithToolFieldManager(rhoai.NamespacedPath("v1", "persistentvolumeclaims", S3Namespace, "")), true
	case "Secret":
		return envkit.WithToolFieldManager(rhoai.NamespacedPath("v1", "secrets", S3Namespace, "")), true
	case "Deployment":
		return envkit.WithToolFieldManager(rhoai.NamespacedPath("apps/v1", "deployments", S3Namespace, "")), true
	case "Service":
		return envkit.WithToolFieldManager(rhoai.NamespacedPath("v1", "services", S3Namespace, "")), true
	case "NetworkPolicy":
		return envkit.WithToolFieldManager(rhoai.NamespacedPath("networking.k8s.io/v1", "networkpolicies", S3Namespace, "")), true
	case "Route":
		return envkit.WithToolFieldManager(rhoai.NamespacedPath("route.openshift.io/v1", "routes", S3Namespace, "")), true
	}
	return "", false
}

// s3GetPath returns the path a named storage object is read at. PVCs are
// not read by name (see listPVCs).
func s3GetPath(kind, name string) (string, bool) {
	switch {
	case kind == "Job" && name == MigrateJobName:
		return rhoai.NamespacedPath("batch/v1", "jobs", S3Namespace, MigrateJobName), true
	case kind == "Secret" && name == S3Secret:
		return rhoai.NamespacedPath("v1", "secrets", S3Namespace, S3Secret), true
	case kind == "Deployment" && name == S3Deployment:
		return rhoai.NamespacedPath("apps/v1", "deployments", S3Namespace, S3Deployment), true
	case kind == "Service" && name == s3Service:
		return rhoai.NamespacedPath("v1", "services", S3Namespace, s3Service), true
	case kind == "NetworkPolicy" && name == s3NetworkPolicy:
		return rhoai.NamespacedPath("networking.k8s.io/v1", "networkpolicies", S3Namespace, s3NetworkPolicy), true
	case kind == "Route" && name == s3AdminRoute:
		return rhoai.NamespacedPath("route.openshift.io/v1", "routes", S3Namespace, s3AdminRoute), true
	}
	return "", false
}

// s3DeletePath returns the path of a storage object teardown may delete.
func s3DeletePath(kind, name string) (string, bool) {
	switch {
	case kind == "Job" && name == MigrateJobName:
		return rhoai.NamespacedPath("batch/v1", "jobs", S3Namespace, MigrateJobName), true
	case kind == "PersistentVolumeClaim" && name == s3PVC:
		return rhoai.NamespacedPath("v1", "persistentvolumeclaims", S3Namespace, s3PVC), true
	case kind == "Secret" && name == S3Secret:
		return rhoai.NamespacedPath("v1", "secrets", S3Namespace, S3Secret), true
	case kind == "Deployment" && name == S3Deployment:
		return rhoai.NamespacedPath("apps/v1", "deployments", S3Namespace, S3Deployment), true
	case kind == "Service" && name == s3Service:
		return rhoai.NamespacedPath("v1", "services", S3Namespace, s3Service), true
	case kind == "NetworkPolicy" && name == s3NetworkPolicy:
		return rhoai.NamespacedPath("networking.k8s.io/v1", "networkpolicies", S3Namespace, s3NetworkPolicy), true
	case kind == "Route" && name == s3AdminRoute:
		return rhoai.NamespacedPath("route.openshift.io/v1", "routes", S3Namespace, s3AdminRoute), true
	}
	return "", false
}

// readS3Object reads one named object of the storage namespace.
func readS3Object(c *kube.Client, kind, name string) (kube.ObjectMeta, bool, error) {
	if kind == "PersistentVolumeClaim" {
		p, found, err := findPVC(c, S3Namespace, name)
		return p.Meta, found, err
	}
	path, ok := s3GetPath(kind, name)
	if !ok {
		return kube.ObjectMeta{}, false, fmt.Errorf("internal error: unexpected S3 storage object %s %s", kind, name)
	}
	return kube.ReadObjectMeta(c, path)
}

type s3Resource struct {
	name string // shown in logs, for example "PVC"
	Kind string
	Obj  map[string]any
	// frontDoor marks the objects that route traffic to the server: they are
	// applied only once the server is ready.
	frontDoor bool
}

// S3Resources returns the objects setup creates or updates, in order. Only
// the admin UI gets a Route: the S3 API stays inside the cluster, where
// pipeline servers reach it through the Service. secured adds the IAM
// flags of the secured storage (ArgsSecured, see s3SetupSecured).
func S3Resources(keys S3Keys, secured bool) []s3Resource {
	meta := func(name string) map[string]any {
		return map[string]any{"name": name, "namespace": S3Namespace, "labels": envkit.ToolLabels()}
	}
	secretEnv := func(name, key string) map[string]any {
		return map[string]any{"name": name, "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": S3Secret, "key": key}}}
	}
	podLabels := map[string]any{"app": s3AppLabel}
	args := []string{"mini", "-dir=/data", "-webdav=false", "-bucket=" + s3Bucket,
		"-admin.port=" + strconv.Itoa(s3AdminPort), "-admin.port.legacy=0"}
	if secured {
		args = append(args, S3IAMWritesArg, S3SelfServiceArg)
	}
	return []s3Resource{
		{"PVC", "PersistentVolumeClaim", map[string]any{
			"apiVersion": "v1", "kind": "PersistentVolumeClaim",
			"metadata": meta(s3PVC),
			"spec": map[string]any{
				"accessModes": []string{"ReadWriteOnce"},
				"resources":   map[string]any{"requests": map[string]any{"storage": s3Storage}},
				"volumeMode":  "Filesystem",
			},
		}, false},
		{"Secret", "Secret", map[string]any{
			"apiVersion": "v1", "kind": "Secret",
			"metadata": meta(S3Secret),
			"stringData": map[string]any{
				S3AccessKeyField:     keys.AccessKey,
				S3SecretKeyField:     keys.SecretKey,
				s3AdminPasswordField: keys.AdminPassword,
			},
		}, false},
		// "weed mini" listens on many ports besides S3 (master 9333/19333,
		// volume 9340/19340, filer 8888/18888, Iceberg 8181, Lance 9101; the
		// admin's worker gRPC 22535 on loopback only), and the master and
		// filer HTTP APIs need no authentication. The pod is therefore
		// fenced in before it starts:
		//   - S3 (8333) only from pods: the pipeline servers in their
		//     projects, the data-science-pipelines operator's object-storage
		//     health check (redhat-ods-applications), this tool's bucket
		//     creation and the storage migration Job. Any namespace is
		//     allowed, because a pipeline server may live in any project;
		//     there is no S3 Route.
		//   - The admin UI (12535) only from the OpenShift router, through
		//     Route seaweedfs-admin: namespaces labelled
		//     policy-group.network.openshift.io/ingress (router pods) or
		//     .../host-network (a HostNetwork router), as in the OpenShift
		//     docs "Allowing ingress from the Ingress Controller"
		//     (checked live: openshift-ingress carries the ingress label).
		//   - Nothing else. This is a security control, not hygiene: the
		//     filer's gRPC port (18888) serves the IAM store without
		//     authentication, so a pod reaching it could make itself an
		//     admin (seen locally with SeaweedFS 4.54). A Go test pins the
		//     two ports (TestS3NetworkPolicyAllowsOnlyS3AndAdminUI).
		// The kubelet's probes always pass (traffic from the pod's node is
		// allowed by NetworkPolicy semantics), and `oc port-forward` does
		// not go through the pod network.
		{"NetworkPolicy", "NetworkPolicy", map[string]any{
			"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy",
			"metadata": meta(s3NetworkPolicy),
			"spec": map[string]any{
				"podSelector": map[string]any{"matchLabels": podLabels},
				"policyTypes": []string{"Ingress"},
				"ingress": []map[string]any{
					{
						"from":  []map[string]any{{"namespaceSelector": map[string]any{}}},
						"ports": []map[string]any{{"protocol": "TCP", "port": s3Port}},
					},
					{
						"from": []map[string]any{
							{"namespaceSelector": map[string]any{"matchLabels": map[string]any{"policy-group.network.openshift.io/ingress": ""}}},
							{"namespaceSelector": map[string]any{"matchLabels": map[string]any{"policy-group.network.openshift.io/host-network": ""}}},
						},
						"ports": []map[string]any{{"protocol": "TCP", "port": s3AdminPort}},
					},
				},
			},
		}, false},
		{"Deployment", "Deployment", map[string]any{
			"apiVersion": "apps/v1", "kind": "Deployment",
			"metadata": meta(S3Deployment),
			"spec": map[string]any{
				"replicas": 1,
				"selector": map[string]any{"matchLabels": podLabels},
				"strategy": map[string]any{"type": "Recreate"},
				"template": map[string]any{
					// The checksum of the admin keys: a new key in the
					// Secret changes the template, so the pod restarts with
					// it (env from a Secret is read at start only).
					"metadata": map[string]any{"labels": podLabels, "annotations": map[string]any{S3KeyChecksumAnnotation: keyChecksum(keys)}},
					"spec": map[string]any{
						"volumes": []map[string]any{
							{"name": "data", "persistentVolumeClaim": map[string]any{"claimName": s3PVC}},
						},
						"containers": []map[string]any{{
							"name":  s3Container,
							"image": s3Image(),
							// -bucket creates the bucket before the server
							// reports ready (weed/command/mini.go:1672-1679 at
							// 4.54); setup also creates it through the Service.
							// -admin.port.legacy=0 turns off the second admin
							// listener SeaweedFS keeps on the old port 23646
							// for compatibility (weed/command/mini.go:598).
							"args": args,
							"env": []map[string]any{
								// The S3 server adds a static admin identity
								// from these (weed/s3api/auth_credentials.go:555-611
								// at 4.54), so the S3 keys are the ones in the
								// Secret.
								secretEnv("AWS_ACCESS_KEY_ID", S3AccessKeyField),
								secretEnv("AWS_SECRET_ACCESS_KEY", S3SecretKeyField),
								// The admin UI login, from the environment rather
								// than -admin.password, so the password is not on
								// the process command line: "weed mini" fills the
								// unset admin flags from viper key admin.user /
								// admin.password (weed/command/mini.go:1907-1909
								// and 1985, weed/command/admin.go:874-881 at
								// 4.54), and viper reads WEED_-prefixed variables
								// with "." replaced by "_"
								// (weed/util/config.go:204-212). With a password
								// set, the UI requires a login and weed mini
								// binds it to the pod's address; without one it
								// would bind it to loopback only
								// (weed/command/mini.go:1915-1928).
								{"name": "WEED_ADMIN_USER", "value": S3AdminUser},
								secretEnv("WEED_ADMIN_PASSWORD", s3AdminPasswordField),
							},
							"ports": []map[string]any{
								{"name": "s3", "containerPort": s3Port, "protocol": "TCP"},
								{"name": "admin", "containerPort": s3AdminPort, "protocol": "TCP"},
							},
							"volumeMounts": []map[string]any{
								{"name": "data", "mountPath": "/data", "subPath": "seaweedfs"},
							},
							// Kubeflow Pipelines requests 64m/256Mi; the limit
							// leaves room for the admin UI and multipart
							// uploads (kubeflow/pipelines#13686 raised the CPU).
							"resources": map[string]any{
								"limits":   map[string]any{"cpu": "500m", "memory": "1Gi"},
								"requests": map[string]any{"cpu": "64m", "memory": "256Mi"},
							},
							// Restricted pod security without a fixed UID, so
							// OpenShift's restricted-v2 SCC assigns one.
							"securityContext": restrictedSecurityContext(),
							// GET /status on the S3 port
							// (weed/s3api/s3api_server.go:980 at 4.54).
							"readinessProbe": map[string]any{"httpGet": map[string]any{"path": "/status", "port": s3Port}, "initialDelaySeconds": 5, "periodSeconds": 5},
							"livenessProbe":  map[string]any{"httpGet": map[string]any{"path": "/status", "port": s3Port}, "initialDelaySeconds": 30, "periodSeconds": 10, "failureThreshold": 6},
						}},
						"restartPolicy": "Always",
					},
				},
			},
		}, false},
		{"Service", "Service", map[string]any{
			"apiVersion": "v1", "kind": "Service",
			"metadata": meta(s3Service),
			"spec": map[string]any{
				"selector": podLabels,
				"type":     "ClusterIP",
				"ports": []map[string]any{
					{"name": "s3", "port": s3Port, "targetPort": s3Port},
					{"name": "ui", "port": s3AdminPort, "targetPort": s3AdminPort},
				},
			},
		}, true},
		{"Admin UI Route", "Route", map[string]any{
			"apiVersion": "route.openshift.io/v1", "kind": "Route",
			"metadata": meta(s3AdminRoute),
			"spec": map[string]any{
				"to":   map[string]any{"kind": "Service", "name": s3Service, "weight": 100},
				"port": map[string]any{"targetPort": "ui"},
				"tls":  map[string]any{"termination": "edge", "insecureEdgeTerminationPolicy": "Redirect"},
			},
		}, true},
	}
}

// networkPolicyProblem explains how NetworkPolicy seaweedfs-ingress differs
// from what setup applies, or returns "" (also when it cannot be read: the
// status then says nothing). It is a security control: only the S3 port
// and the admin UI port may be reachable (see S3Resources).
func networkPolicyProblem(c *kube.Client) string {
	body, found, err := c.GetOptional(rhoai.NamespacedPath("networking.k8s.io/v1", "networkpolicies", S3Namespace, s3NetworkPolicy))
	if err != nil {
		return ""
	}
	repair := fmt.Sprintf(" Only the S3 port %d and the admin UI port %d may be reachable; Repair applies it again.", s3Port, s3AdminPort)
	if !found {
		return fmt.Sprintf("NetworkPolicy %s is missing, so every port of SeaweedFS is reachable from the pod network.", s3NetworkPolicy) + repair
	}
	var np struct {
		Spec struct {
			PodSelector struct {
				MatchLabels map[string]string `json:"matchLabels"`
			} `json:"podSelector"`
			PolicyTypes []string `json:"policyTypes"`
			Ingress     []struct {
				From  []json.RawMessage            `json:"from"`
				Ports []map[string]json.RawMessage `json:"ports"`
			} `json:"ingress"`
		} `json:"spec"`
	}
	changed := fmt.Sprintf("NetworkPolicy %s was changed.", s3NetworkPolicy) + repair
	if json.Unmarshal(body, &np) != nil || np.Spec.PodSelector.MatchLabels["app"] != s3AppLabel || len(np.Spec.PodSelector.MatchLabels) != 1 ||
		!slices.Equal(np.Spec.PolicyTypes, []string{"Ingress"}) {
		return changed
	}
	var ports []string
	for _, rule := range np.Spec.Ingress {
		if len(rule.From) == 0 || len(rule.Ports) == 0 {
			return changed
		}
		for _, p := range rule.Ports {
			if len(p) != 2 || string(p["protocol"]) != `"TCP"` || p["port"] == nil {
				return changed
			}
			ports = append(ports, string(p["port"]))
		}
	}
	sort.Strings(ports)
	// Sorted as text: "12535" before "8333".
	if !slices.Equal(ports, []string{strconv.Itoa(s3AdminPort), strconv.Itoa(s3Port)}) {
		return changed
	}
	return ""
}

// S3KeyChecksumAnnotation on the storage's pod template is keyChecksum of
// the admin keys it was started with.
const S3KeyChecksumAnnotation = "rhoai-nightly-updater/admin-key-checksum"

// keyChecksum is a short SHA-256 of the admin key pair (not reversible).
func keyChecksum(keys S3Keys) string {
	sum := sha256.Sum256([]byte(keys.AccessKey + "\n" + keys.SecretKey))
	return hex.EncodeToString(sum[:8])
}

// restrictedSecurityContext is a container securityContext that passes
// Pod Security "restricted" and OpenShift's restricted-v2 SCC, without a
// fixed UID so the SCC assigns one.
func restrictedSecurityContext() map[string]any {
	return map[string]any{
		"allowPrivilegeEscalation": false,
		"capabilities":             map[string]any{"drop": []string{"ALL"}},
		"runAsNonRoot":             true,
		"seccompProfile":           map[string]any{"type": "RuntimeDefault"},
	}
}

// s3Retry names the action that retries a storage setup that stopped: the
// Storage card's Set up (or Repair), or Migrate storage, whose first step
// is this setup and which continues from where it stopped. A message must
// name the action the user is in: inside Migrate storage "re-run setup"
// would send them to the wrong place.
type s3Retry struct {
	// again continues a sentence ("..., then re-run setup"), Again starts one.
	again, Again string
}

var (
	s3RetrySetup   = s3Retry{"re-run setup", "Re-run setup"}
	s3RetryMigrate = s3Retry{"run Migrate storage again", "Run Migrate storage again"}
	s3RetrySecure  = s3Retry{"run Secure S3 storage again", "Run Secure S3 storage again"}
)

// foreignHint tells the user how to resolve a refusal over an object that
// has a name the tool uses but no ownership label.
func (r s3Retry) foreignHint() string {
	return fmt.Sprintf("Delete or rename it, or label it with `oc label -n %s <kind>/<name> app.kubernetes.io/managed-by=rhoai-nightly-updater` if this tool created it, then %s.", S3Namespace, r.again)
}

// legacyStorageSetupBlock explains why setup does not run while storage
// from an earlier version exists, or returns "".
func legacyStorageSetupBlock(l *Legacy) string {
	if l == nil {
		return ""
	}
	return fmt.Sprintf("Storage from an earlier version of this tool is still in namespace %s. Use Migrate storage: it sets up the storage in namespace %s with new keys, copies every object and moves the pipeline servers this tool set up to the new storage and its keys. Anything else that uses the old keys, such as your own S3 clients, then needs the new keys from the Pipelines tab.", LegacyNamespace, S3Namespace)
}

// SetupS3 deploys the S3 storage (SeaweedFS) with a bucket for
// pipeline artifacts in namespace seaweedfs. While storage from an earlier
// version exists in namespace minio it refuses: Migrate storage sets up the
// new storage as its first step, then moves the data and the pipeline
// servers (see readLegacyStorage).
func SetupS3(c *kube.Client) (*types.OperationResponse, error) {
	legacy, err := ReadLegacy(c)
	if err != nil {
		record.Action(c, "setup-s3", fmt.Sprintf("namespace=%s (earlier storage check failed)", S3Namespace), false)
		return &types.OperationResponse{Success: false, Message: fmt.Sprintf("Cannot check for storage from an earlier version in namespace %s; nothing was changed: %v.%s", LegacyNamespace, err, kube.TemplateHint(err)), ErrorCode: kube.ErrorCodeFromK8sErr(err)}, nil
	}
	if reason := legacyStorageSetupBlock(legacy); reason != "" {
		record.Action(c, "setup-s3", fmt.Sprintf("namespace=%s (earlier storage present)", S3Namespace), false)
		return &types.OperationResponse{Success: false, Message: reason + " Nothing was changed.", ErrorCode: "prerequisites"}, nil
	}
	keys, secured, rotated, err := s3SetupKeys(c)
	if err != nil {
		record.Action(c, "setup-s3", fmt.Sprintf("namespace=%s (credentials)", S3Namespace), false)
		return &types.OperationResponse{Success: false, Message: fmt.Sprintf("Cannot determine the S3 credentials; nothing was changed: %v.%s", err, kube.TemplateHint(err)), ErrorCode: kube.ErrorCodeFromK8sErr(err)}, nil
	}
	// Who held the stored key, when this setup replaces it (read before).
	var holders []string
	if rotated {
		if dspas, err := ListDSPAs(c, ""); err == nil {
			if ep, err := readS3Endpoints(c); err == nil {
				live, _ := Users(dspas, ep.Address)
				holders = live
			}
		}
	}
	finish := func(res *types.OperationResponse) (*types.OperationResponse, error) {
		if !res.Success || !secured {
			return res, nil
		}
		if rotated {
			res.Message += " The storage's Deployment was missing, so it was set up again with a new admin key and per-server keys turned on: the old admin key no longer works."
			if len(holders) > 0 {
				res.Message += fmt.Sprintf(" %s used it and lost S3 access: %s. Run Secure S3 storage to give each pipeline server this tool set up a key of its own.", kube.Verb(len(holders), "This pipeline server", "These pipeline servers"), strings.Join(holders, ", "))
			}
		}
		// Users of pipeline servers that are gone: their keys still work.
		if left, err := removeLeftoverUsers(c); err != nil {
			res.Message += fmt.Sprintf(" The keys of deleted pipeline servers could not be checked: %v.", err)
		} else if len(left) > 0 {
			res.Message += fmt.Sprintf(" Deleted the keys of pipeline servers that are gone: %s.", strings.Join(left, ", "))
			res.Logs = append(res.Logs, "OK: Deleted SeaweedFS users "+strings.Join(left, ", "))
		}
		return res, nil
	}
	return finish(setupS3Storage(c, keys, &opLog{}, s3RetrySetup, secured))
}

// opLog collects an operation's log lines for its response, and passes each
// to observe when set (a streamed operation shows them as step progress).
type opLog struct {
	lines   []string
	observe func(string)
}

func (l *opLog) add(line string) {
	l.lines = append(l.lines, line)
	if l.observe != nil {
		l.observe(line)
	}
}

// setupS3Storage applies the storage with the given keys, logging each step
// to log. It reports success only once the SeaweedFS pod is ready and
// the bucket exists. Re-running it updates the objects it created, so it
// also repairs a partial setup. It never changes an object it did not
// create: every object is checked first, absent ones are created with POST,
// and updates are guarded by the resourceVersion that was checked (see
// envkit.EnsureToolObject). Every create and apply is sent as a server-side dry
// run before any is made, so an object the API server rejects changes
// nothing (the namespace, which the dry runs need, is created before them).
// retry names the action that continues a setup that stopped. secured
// applies the IAM flags of the secured storage (see s3SetupSecured).
func setupS3Storage(c *kube.Client, keys S3Keys, log *opLog, retry s3Retry, secured bool) *types.OperationResponse {
	fail := func(msg, code, activity string) *types.OperationResponse {
		slog.Warn("s3 storage setup failed", "reason", activity, "user", kube.GetUser(c))
		record.Action(c, "setup-s3", fmt.Sprintf("namespace=%s (%s)", S3Namespace, activity), false)
		return &types.OperationResponse{Success: false, Message: msg, Logs: log.lines, ErrorCode: code}
	}
	// Migrate storage passes the old storage's keys, which have no admin
	// UI password: keep the stored one, or make one.
	keys, err := withAdminPassword(c, keys)
	if err != nil {
		return fail(fmt.Sprintf("Cannot determine the admin UI password; nothing was changed: %v.%s", err, kube.TemplateHint(err)), kube.ErrorCodeFromK8sErr(err), "credentials")
	}

	// Step 1: Namespace. Never adopt a namespace this tool did not create.
	log.add(fmt.Sprintf("Creating namespace %s...", S3Namespace))
	ns, err := lookupS3Namespace(c)
	if err != nil {
		return fail(fmt.Sprintf("Cannot check namespace %s: %v.%s", S3Namespace, err, kube.TemplateHint(err)), kube.ErrorCodeFromK8sErr(err), "namespace check failed")
	}
	if !ns.Exists {
		nsData, _ := json.Marshal(map[string]any{
			"apiVersion": "v1",
			"kind":       "Namespace",
			"metadata":   map[string]any{"name": S3Namespace, "labels": envkit.ToolLabels()},
		})
		_, _, err := c.Post(envkit.WithToolFieldManager("/api/v1/namespaces"), nsData)
		switch {
		case err == nil:
			log.add("OK: Namespace created")
			ns = s3NamespaceInfo{Exists: true, Managed: true}
		case kube.IsK8sConflict(err):
			// Created concurrently: decide ownership again.
			if ns, err = lookupS3Namespace(c); err != nil {
				return fail(fmt.Sprintf("Cannot check namespace %s: %v.%s", S3Namespace, err, kube.TemplateHint(err)), kube.ErrorCodeFromK8sErr(err), "namespace check failed")
			}
		default:
			return fail(fmt.Sprintf("Failed to create namespace %s: %v.%s", S3Namespace, err, kube.TemplateHint(err)), kube.ErrorCodeFromK8sErr(err), "namespace creation failed")
		}
	} else if ns.Managed && !ns.terminating() {
		log.add("Namespace already exists (created by this tool) — reusing")
	}
	if ns.terminating() {
		return fail(fmt.Sprintf("Namespace %s is still being deleted. Wait until it is gone, then %s.", S3Namespace, retry.again), "terminating", "namespace terminating")
	}
	if !ns.Managed {
		return fail(fmt.Sprintf("Namespace %s already exists and was not created by this tool. Nothing was changed. Remove it yourself, or use a cluster without it.", S3Namespace), "not_managed", "namespace not managed")
	}

	// Step 2: Check every object the tool writes before writing any, so a
	// refusal leaves everything unchanged.
	type current struct {
		meta  kube.ObjectMeta
		found bool
	}
	resources := S3Resources(keys, secured)
	existing := map[string]current{}
	var foreign, deleting []string
	for _, r := range resources {
		name, _ := r.Obj["metadata"].(map[string]any)["name"].(string)
		meta, found, err := readS3Object(c, r.Kind, name)
		if err != nil {
			return fail(fmt.Sprintf("Cannot read %s %s/%s; nothing was changed: %v.%s", r.Kind, S3Namespace, name, err, kube.TemplateHint(err)), kube.ErrorCodeFromK8sErr(err), "read failed")
		}
		existing[r.Kind+"/"+name] = current{meta, found}
		switch {
		case !found:
		case !storageObjectOwned(meta):
			foreign = append(foreign, r.Kind+" "+name)
		case meta.Terminating():
			deleting = append(deleting, r.Kind+" "+name)
		}
	}
	if len(foreign) > 0 {
		return fail(fmt.Sprintf("Namespace %s already contains %s, not created by this tool. Nothing was changed. %s", S3Namespace, strings.Join(foreign, ", "), retry.foreignHint()), "not_managed", "foreign objects: "+strings.Join(foreign, ", "))
	}
	if len(deleting) > 0 {
		return fail(fmt.Sprintf("%s in namespace %s is still being deleted. Wait until it is gone, then %s.", strings.Join(deleting, ", "), S3Namespace, retry.again), "terminating", "objects terminating")
	}

	// Step 3: Create or update the server objects. The Service and the
	// Route follow once the server is ready (step 5).
	var applied []string
	write := func(r s3Resource, dryRun bool) *types.OperationResponse {
		if !dryRun {
			log.add(fmt.Sprintf("Applying %s...", r.name))
		}
		path, ok := s3ApplyPath(r.Obj)
		createPath, createOK := s3CreatePath(r.Kind)
		if !ok || !createOK {
			return fail(fmt.Sprintf("Internal error: unexpected S3 storage object %s %v", r.Kind, r.Obj["metadata"]), "internal", "unknown object")
		}
		name, _ := r.Obj["metadata"].(map[string]any)["name"].(string)
		key := r.Kind + "/" + name
		cur := existing[key]
		kind := r.Kind
		read := func() (kube.ObjectMeta, bool, error) { return readS3Object(c, kind, name) }
		action, meta, found, err := envkit.EnsureToolObject(c, fmt.Sprintf("%s %s/%s", r.Kind, S3Namespace, name), path, createPath, r.Obj, cur.meta, cur.found, read, storageObjectOwned, dryRun)
		existing[key] = current{meta, found}
		if err != nil {
			done := "nothing"
			if len(applied) > 0 {
				done = strings.Join(applied, ", ")
			}
			if foreignErr, ok := errors.AsType[*envkit.ForeignObjectError](err); ok {
				return fail(fmt.Sprintf("%s was created by someone else while setup was running, so it was left unchanged. Already applied: %s. %s", foreignErr.Desc, done, retry.foreignHint()), "not_managed", r.name+" became foreign")
			}
			code := kube.ErrorCodeFromK8sErr(err)
			if errors.Is(err, envkit.ErrObjectChanged) || kube.IsK8sConflict(err) {
				code = "conflict"
			}
			if dryRun {
				return fail(fmt.Sprintf("The API server rejects %s: %v. Nothing was changed.%s", r.name, err, kube.TemplateHint(err)), code, "dry run "+r.name+" failed")
			}
			return fail(fmt.Sprintf("Failed to apply %s: %v. Already applied: %s. %s to retry, or delete the storage to remove it.%s", r.name, err, done, retry.Again, kube.TemplateHint(err)), code, "apply "+r.name+" failed")
		}
		if !dryRun {
			applied = append(applied, r.name)
			log.add(fmt.Sprintf("OK: %s %s", r.name, action))
		}
		return nil
	}

	// Every write is first sent as a server-side dry run, so an object the
	// API server rejects (validation, admission, the apply merge) stops
	// setup before anything changed: the check-before-write rule above,
	// extended to what only the API server can check.
	log.add("Checking every change with the API server (dry run)...")
	for _, r := range resources {
		if resp := write(r, true); resp != nil {
			return resp
		}
	}
	log.add("OK: The API server accepts every change")
	for _, r := range resources {
		if r.frontDoor {
			continue
		}
		if resp := write(r, false); resp != nil {
			return resp
		}
	}

	// Step 4: Wait for the pod. Stop early when it cannot start.
	log.add(fmt.Sprintf("Waiting up to %s for SeaweedFS to become ready...", S3ReadyTimeout))
	issue, err := WaitS3Ready(c, s3Image())
	if err != nil {
		if errors.Is(err, ErrS3NotReady) {
			reason := "the pod is still starting"
			if issue.Reason != "" {
				reason = issue.Reason
				if issue.Message != "" {
					reason += ": " + kube.TruncateText(issue.Message, 300)
				}
			}
			log.add("SeaweedFS is not ready: " + reason)
			return fail(fmt.Sprintf("SeaweedFS was deployed but is not ready after %s (%s). The status shows the current state; %s once it is ready.", S3ReadyTimeout, reason, retry.again), "timeout", "not ready")
		}
		if errors.Is(err, errS3CannotStart) {
			msg := issue.Reason
			if issue.Message != "" {
				msg += ": " + kube.TruncateText(issue.Message, 300)
			}
			log.add("SeaweedFS cannot start: " + msg)
			return fail(fmt.Sprintf("SeaweedFS was deployed but its pod cannot start (%s). Fix the cause (for example set SEAWEEDFS_IMAGE to a pullable image) and %s, or delete the storage.", msg, retry.again), "not_ready", issue.Reason)
		}
		return fail(fmt.Sprintf("Stopped waiting for SeaweedFS: %v.%s", err, kube.TemplateHint(err)), kube.ErrorCodeFromK8sErr(err), "wait failed")
	}
	log.add("OK: SeaweedFS is running")

	// Step 5: Route traffic to it: the Service (the address pipeline servers
	// use) and the admin UI Route.
	for _, r := range resources {
		if !r.frontDoor {
			continue
		}
		if resp := write(r, false); resp != nil {
			return resp
		}
	}

	// Step 6: Create the bucket through the Service, as pipeline servers
	// reach it (try once, retry once if it fails). SeaweedFS also creates
	// it on start (-bucket).
	log.add(fmt.Sprintf("Creating bucket %s...", s3Bucket))
	bucketErr := s3BucketCreator(c, s3Bucket)
	if bucketErr != nil {
		if kube.Clock().Sleep(c.Context(), s3BucketRetryDelay) == nil {
			bucketErr = s3BucketCreator(c, s3Bucket)
		}
	}
	if bucketErr != nil {
		log.add(fmt.Sprintf("Bucket creation failed: %v — you can create it from the SeaweedFS admin UI.", bucketErr))
		return fail(fmt.Sprintf("SeaweedFS deployed but bucket creation failed: %v. %s to retry, or create bucket %s from the admin UI.", bucketErr, retry.Again, s3Bucket), kube.ErrorCodeFromK8sErr(bucketErr), "bucket failed")
	}
	log.add(fmt.Sprintf("OK: Bucket %s created", s3Bucket))

	slog.Info("s3 storage setup complete", "user", kube.GetUser(c))
	record.Action(c, "setup-s3", fmt.Sprintf("namespace=%s bucket=%s", S3Namespace, s3Bucket), true)
	return &types.OperationResponse{
		Success: true,
		Message: fmt.Sprintf("S3 storage (SeaweedFS) deployed with bucket %s; pipeline servers reach it at %s. Sign in to the admin UI as %s; its password (a value of its own) and the admin S3 keys are in Secret %s in namespace %s.", s3Bucket, S3Endpoint(), S3AdminUser, S3Secret, S3Namespace),
		Logs:    log.lines,
	}
}

var (
	ErrS3NotReady    = errors.New("seaweedfs not ready before timeout")
	errS3CannotStart = errors.New("seaweedfs pod cannot start")
)

// WaitS3Ready polls the SeaweedFS Deployment until its current template has
// a ready replica. It returns errS3CannotStart as soon as a pod of image
// reports a terminal waiting reason (ImagePullBackOff, CrashLoopBackOff, ...),
// and errS3NotReady on timeout, with the last observed pod issue. Every
// request runs under one context bounded by S3ReadyTimeout, so a request
// that starts just before the deadline cannot overrun it.
func WaitS3Ready(c *kube.Client, image string) (envkit.PodIssue, error) {
	ctx, cancel := kube.Clock().WithTimeout(c.Context(), S3ReadyTimeout)
	defer cancel()
	cc := c.WithContext(ctx)
	var last envkit.PodIssue
	expired := func() (envkit.PodIssue, error) {
		if c.Context().Err() != nil {
			return last, c.Context().Err()
		}
		return last, ErrS3NotReady
	}
	for {
		body, _, err := cc.Get(rhoai.NamespacedPath("apps/v1", "deployments", S3Namespace, S3Deployment))
		if ctx.Err() != nil {
			return expired()
		}
		if err == nil {
			var deploy S3DeploymentSpec
			if json.Unmarshal(body, &deploy) == nil {
				if deploy.RolledOut() {
					return envkit.PodIssue{}, nil
				}
				// Only judge pods once the controller has seen the new
				// template, and only pods running the image just applied, so
				// an old pod from an earlier setup is not mistaken for this one.
				if deploy.Status.ObservedGeneration >= deploy.Metadata.Generation {
					if issue, perr := envkit.InspectPods(cc, S3Namespace, s3AppSelector, image); perr == nil && issue.Reason != "" {
						last = issue
						if issue.Terminal {
							return issue, errS3CannotStart
						}
					}
				}
			}
		} else if !kube.IsK8sNotFound(err) && !kube.IsNetworkError(err) {
			return last, err
		}
		if kube.Clock().Sleep(ctx, s3ReadyPoll) != nil {
			return expired()
		}
	}
}

// createS3Bucket creates a bucket through the storage's Service with the
// keys in its Secret.
func createS3Bucket(c *kube.Client, bucket string) error {
	keys, found, err := ReadS3Keys(c)
	if err != nil {
		return err
	}
	if !found || keys.AccessKey == "" || keys.SecretKey == "" {
		return fmt.Errorf("no S3 keys in Secret %s/%s", S3Namespace, S3Secret)
	}
	return s3PutBucket(c.Context(), S3BucketEndpoint(os.Getenv), keys.AccessKey, keys.SecretKey, bucket)
}

// s3KeptNamespaceHint ends every teardown message: namespaces are never
// deleted, because the tool cannot see everything in them without reading
// every Secret in the cluster (a cluster-wide list grant).
func s3KeptNamespaceHint(namespaces []string) string {
	if len(namespaces) == 0 {
		return ""
	}
	cmds := make([]string, len(namespaces))
	for i, n := range namespaces {
		cmds[i] = "`oc delete project " + n + "`"
	}
	return fmt.Sprintf(" %s %s kept: %s may hold objects this tool did not create. Delete %s with %s once you've checked %s empty.",
		kube.Verb(len(namespaces), "Namespace", "Namespaces"), strings.Join(namespaces, " and ")+" "+kube.Verb(len(namespaces), "was", "were"),
		kube.Verb(len(namespaces), "it", "they"), kube.Verb(len(namespaces), "it", "them"), strings.Join(cmds, " and "), kube.Verb(len(namespaces), "it's", "they're"))
}

// s3TeardownObjects are the objects teardown removes from the storage
// namespace, in order. The Deployment goes first, so its pod releases the
// PVC (whose kubernetes.io/pvc-protection finalizer waits for that).
var s3TeardownObjects = []struct{ kind, name string }{
	// Migrate storage's copy Job first: it must not keep copying into a
	// storage that is being deleted.
	{"Job", MigrateJobName},
	{"Deployment", S3Deployment},
	{"NetworkPolicy", s3NetworkPolicy},
	{"Route", s3AdminRoute},
	{"Service", s3Service},
	{"Secret", S3Secret},
	{"PersistentVolumeClaim", s3PVC},
}

// storageDeletion collects the result of deleting a storage's objects.
type storageDeletion struct {
	deleted, kept, failed, pvcNames []string
	pvcUIDs                         map[string]string // UID -> namespace
	forbidden                       error
}

// storageObjectRead reads a named object of either storage namespace.
func storageObjectRead(c *kube.Client, namespace, kind, name string) (kube.ObjectMeta, bool, error) {
	if namespace == LegacyNamespace {
		return readLegacyObject(c, kind, name)
	}
	return readS3Object(c, kind, name)
}

// storageDeletePath returns the delete path of a named object of either
// storage namespace (see s3DeletePath and legacyDeletePath).
func storageDeletePath(namespace, kind, name string) (string, bool) {
	if namespace == LegacyNamespace {
		return legacyDeletePath(kind, name)
	}
	return s3DeletePath(kind, name)
}

// deleteOwnedObject deletes one object of a storage namespace when the
// tool created it, with a UID precondition, and records the outcome.
func (d *storageDeletion) deleteOwnedObject(c *kube.Client, namespace, kind, name string, logs *[]string) {
	if d.pvcUIDs == nil {
		d.pvcUIDs = map[string]string{}
	}
	desc := fmt.Sprintf("%s %s/%s", kind, namespace, name)
	meta, found, err := storageObjectRead(c, namespace, kind, name)
	switch {
	case err != nil:
		d.failed = append(d.failed, fmt.Sprintf("%s: cannot read it: %v", desc, err))
		if kube.IsK8sError(err, http.StatusForbidden) {
			d.forbidden = err
		}
		return
	case !found:
		return
	case !storageObjectOwned(meta):
		d.kept = append(d.kept, desc)
		*logs = append(*logs, fmt.Sprintf("Kept %s: it was not created by this tool", desc))
		return
	case meta.Terminating():
		*logs = append(*logs, desc+" is already being deleted")
	default:
		path, ok := storageDeletePath(namespace, kind, name)
		if !ok {
			return
		}
		*logs = append(*logs, fmt.Sprintf("Deleting %s...", desc))
		if _, err := kube.DeleteWithUID(c, path, meta.UID); err != nil && !kube.IsK8sNotFound(err) {
			d.failed = append(d.failed, fmt.Sprintf("%s: %v", desc, err))
			*logs = append(*logs, fmt.Sprintf("Failed to delete %s: %v", desc, err))
			if kube.IsK8sError(err, http.StatusForbidden) {
				d.forbidden = err
			}
			return
		}
		d.deleted = append(d.deleted, desc)
		*logs = append(*logs, fmt.Sprintf("OK: %s deleted", desc))
	}
	if kind == "PersistentVolumeClaim" {
		d.pvcUIDs[meta.UID] = namespace
		d.pvcNames = append(d.pvcNames, namespace+"/"+name)
	}
}

// WaitPVCsGone waits, bounded by s3DeleteTimeout, until none of the PVCs is
// left. It returns the names still present on timeout: those of the last
// listing that succeeded (the one at the deadline fails with the expired
// context, and must not read as "none left").
func WaitPVCsGone(c *kube.Client, uids map[string]string) ([]string, error) {
	namespaces := map[string]bool{}
	for _, ns := range uids {
		namespaces[ns] = true
	}
	var remaining []string
	listed := false
	gone, err := kube.WaitForGone(c, s3DeleteTimeout, s3DeletePoll, func(cc *kube.Client) (bool, error) {
		var left []string
		for _, ns := range []string{S3Namespace, LegacyNamespace} {
			if !namespaces[ns] {
				continue
			}
			pvcs, err := listPVCs(cc, ns)
			if err != nil {
				return false, err
			}
			for _, p := range pvcs {
				if _, ok := uids[p.Meta.UID]; ok {
					left = append(left, ns+"/"+p.Meta.Name)
				}
			}
		}
		remaining, listed = left, true
		return len(left) == 0, nil
	})
	switch {
	case gone || err != nil:
		return nil, err
	case !listed:
		return nil, fmt.Errorf("the PVCs could not be listed within %s", s3DeleteTimeout)
	}
	return remaining, nil
}

// S3TeardownBlocker explains why the S3 storage cannot be torn down because
// of pipeline servers, or returns "". Any DSPA that stores artifacts in one
// of the storages (see storageAddress.usedBy), or lives in a storage
// namespace, blocks it, whoever created it. Terminating ones block too:
// only a running data-science-pipelines operator removes their finalizer
// (DSPO dspipeline_controller.go), so the storage waits until they are
// really gone (teardown order DSPA, then S3 storage).
func S3TeardownBlocker(dspas []DSPA, addresses ...Address) string {
	live, deleting := Users(dspas, addresses...)
	var parts []string
	if len(live) > 0 {
		parts = append(parts, fmt.Sprintf("%s %s this S3 storage: %s. Delete %s first.", kube.CountNoun(len(live), "pipeline server", "pipeline servers"), kube.Verb(len(live), "uses", "use"), strings.Join(live, ", "), kube.Verb(len(live), "it", "them")))
	}
	if len(deleting) > 0 {
		parts = append(parts, fmt.Sprintf("%s %s still being deleted: %s. Wait until %s gone; the data-science-pipelines operator must be running to finish the cleanup.", kube.CountNoun(len(deleting), "pipeline server", "pipeline servers"), kube.Verb(len(deleting), "is", "are"), strings.Join(deleting, ", "), kube.Verb(len(deleting), "it is", "they are")))
	}
	return strings.Join(parts, " ")
}

// Users lists the pipeline servers ("namespace/name") that store
// their artifacts in a storage at one of the addresses, sorted: those that
// are live and those being deleted.
func Users(dspas []DSPA, addresses ...Address) (live, deleting []string) {
	for _, d := range dspas {
		uses := false
		for _, a := range addresses {
			if a.UsedBy(d.Meta.Namespace, d.Host) || d.Meta.Namespace == a.Namespace {
				uses = true
			}
		}
		if !uses {
			continue
		}
		name := d.Meta.Namespace + "/" + d.Meta.Name
		if d.Meta.Terminating() {
			deleting = append(deleting, name)
		} else {
			live = append(live, name)
		}
	}
	sort.Strings(live)
	sort.Strings(deleting)
	return live, deleting
}

// TeardownS3 removes the S3 storage objects this tool created, with
// their data PVCs, when no pipeline server uses the storage: those in
// namespace seaweedfs, and those an earlier version left in namespace minio
// (see legacyStorageObjects). Each object is read by name, deleted only
// when it carries the tool's label (or an earlier version's server-side
// apply marker), and deleted with a UID precondition, so an object someone
// else created or recreated is never removed. Namespace seaweedfs is
// deleted only with opts.DeleteNamespace, once every object is gone, and
// only when this tool created it (toolNamespaceDeletable, UID
// precondition); otherwise the namespaces are kept (s3KeptNamespaceHint).
// Re-running it is safe.
func TeardownS3(c *kube.Client, opts types.S3TeardownRequest) (*types.OperationResponse, error) {
	logs := []string{}
	fail := func(msg, code, activity string) (*types.OperationResponse, error) {
		record.Action(c, "teardown-s3", fmt.Sprintf("%s (%s)", S3Namespace, activity), false)
		return &types.OperationResponse{Success: false, Message: msg, Logs: logs, ErrorCode: code}, nil
	}

	ns, err := lookupS3Namespace(c)
	if err != nil {
		return fail(fmt.Sprintf("Cannot check namespace %s; nothing was deleted: %v.%s", S3Namespace, err, kube.TemplateHint(err)), kube.ErrorCodeFromK8sErr(err), "namespace check failed")
	}
	legacy, err := ReadLegacy(c)
	if err != nil {
		return fail(fmt.Sprintf("Cannot check namespace %s for storage from an earlier version; nothing was deleted: %v.%s", LegacyNamespace, err, kube.TemplateHint(err)), kube.ErrorCodeFromK8sErr(err), "earlier storage check failed")
	}
	current := ns.Exists && ns.Managed && !ns.terminating()
	if !current && legacy == nil {
		switch {
		case !ns.Exists:
			logs = append(logs, "Namespace already absent")
			return &types.OperationResponse{Success: true, Message: "The S3 storage is not deployed; nothing to remove.", Logs: logs}, nil
		case !ns.Managed:
			return fail(fmt.Sprintf("Namespace %s was not created by this tool, so nothing in it was deleted.", S3Namespace), "not_managed", "not managed")
		default:
			logs = append(logs, "Namespace is being deleted")
			return &types.OperationResponse{Success: false, Message: fmt.Sprintf("Namespace %s is being deleted.", S3Namespace) + namespaceDeletionDetail(c, S3Namespace), Logs: logs, ErrorCode: "in_progress"}, nil
		}
	}

	// Teardown order is MLflow, pipeline servers, then the S3 storage.
	// Pipeline servers that store artifacts here would break, and a DSPA
	// that is still terminating must finish first. A missing DSPA CRD means
	// there are none.
	dspas, err := ListDSPAs(c, "")
	if err != nil && !errors.Is(err, ErrDSPACRDMissing) {
		return fail(fmt.Sprintf("Cannot verify pipeline dependencies; the S3 storage was not deleted: %v.%s", err, kube.TemplateHint(err)), kube.ErrorCodeFromK8sErr(err), "dependency check failed")
	}
	// Pipeline servers may reach a storage through a Route host or the
	// ClusterIP. When those cannot be read and any pipeline server exists, a
	// dependency cannot be ruled out: refuse rather than delete the PVC a
	// pipeline server may still store its artifacts in.
	var addresses []Address
	if current {
		endpoints, err := readS3Endpoints(c)
		if err != nil && len(dspas) > 0 {
			return fail(fmt.Sprintf("Cannot verify which pipeline servers use the S3 storage (its Route host or service address could not be read); nothing was deleted: %v.%s", err, kube.TemplateHint(err)), kube.ErrorCodeFromK8sErr(err), "dependency check failed")
		}
		addresses = append(addresses, endpoints.Address)
	}
	if legacy != nil {
		if legacy.AddressErr != nil && len(dspas) > 0 {
			return fail(fmt.Sprintf("Cannot verify which pipeline servers use the storage in namespace %s (its Route hosts or service address could not be read); nothing was deleted: %v.%s", LegacyNamespace, legacy.AddressErr, kube.TemplateHint(legacy.AddressErr)), kube.ErrorCodeFromK8sErr(legacy.AddressErr), "dependency check failed")
		}
		addresses = append(addresses, legacy.Address)
	}
	if reason := S3TeardownBlocker(dspas, addresses...); reason != "" {
		return fail("Cannot delete the S3 storage yet. "+reason, "prerequisites", "pipeline servers depend on it")
	}

	var d storageDeletion
	var keptNamespaces []string
	if current {
		// The SeaweedFS users of deleted pipeline servers (one deleted in the
		// RHOAI dashboard keeps its user): keys first, then the user. The
		// data volume holds them and goes too, so a storage that cannot be
		// reached only logs it.
		if secured, err := storageSecured(c); err == nil && secured {
			sw, err := adminSeaweed(c)
			var users []string
			if err == nil {
				users, err = deleteScopedUsers(sw)
			}
			if err != nil {
				logs = append(logs, fmt.Sprintf("Note: the SeaweedFS users of pipeline servers could not be deleted first (%v); they go with the data volume", err))
			} else if len(users) > 0 {
				logs = append(logs, fmt.Sprintf("OK: SeaweedFS users deleted with their keys: %s", strings.Join(users, ", ")))
			}
		}
		for _, o := range s3TeardownObjects {
			d.deleteOwnedObject(c, S3Namespace, o.kind, o.name, &logs)
		}
		keptNamespaces = append(keptNamespaces, S3Namespace)
	}
	if legacy != nil {
		for _, o := range legacyStorageObjects {
			d.deleteOwnedObject(c, LegacyNamespace, o.kind, o.name, &logs)
		}
		keptNamespaces = append(keptNamespaces, LegacyNamespace)
	}

	keptNote := ""
	if len(d.kept) > 0 {
		keptNote = fmt.Sprintf(" Left in place because this tool did not create them: %s.", strings.Join(d.kept, ", "))
	}
	dataNote := ""
	if len(d.pvcNames) > 0 {
		dataNote = fmt.Sprintf(" %s %s and all stored objects (pipeline artifacts, models, test files) are deleted with %s.", kube.Verb(len(d.pvcNames), "PVC", "PVCs"), strings.Join(d.pvcNames, " and "), kube.Verb(len(d.pvcNames), "it", "them"))
	}
	nsNote := ""
	if current && opts.DeleteNamespace && len(d.failed) == 0 {
		// Every object is gone: the namespace goes with whatever else is in
		// it, as the user chose. A namespace someone else created or labelled
		// is kept, with why.
		if ok, reason := toolNamespaceDeletable(S3Namespace, ns.Meta); !ok {
			nsNote = " " + reason
			logs = append(logs, reason)
		} else if _, err := kube.DeleteWithUID(c, "/api/v1/namespaces/"+S3Namespace, ns.Meta.UID); err != nil && !kube.IsK8sNotFound(err) {
			d.failed = append(d.failed, fmt.Sprintf("namespace %s: %v", S3Namespace, err))
			d.forbidden = err
		} else {
			keptNamespaces = keptNamespaces[1:]
			nsNote = fmt.Sprintf(" Namespace %s is being deleted, with everything else in it.", S3Namespace)
			logs = append(logs, "OK: Namespace "+S3Namespace+" is being deleted")
		}
	}
	nsHint := nsNote + s3KeptNamespaceHint(keptNamespaces)
	if len(d.failed) > 0 {
		record.Action(c, "teardown-s3", fmt.Sprintf("%s (failed: %s)", S3Namespace, strings.Join(d.failed, "; ")), false)
		code := "partial_failure"
		if len(d.deleted) == 0 {
			code = "delete_failed"
		}
		return &types.OperationResponse{
			Success: false, Message: fmt.Sprintf("Some S3 storage objects could not be removed: %s. Re-run teardown to retry.%s%s%s", strings.Join(d.failed, "; "), kube.TemplateHint(d.forbidden), keptNote, nsHint),
			Logs: logs, ErrorCode: code,
		}, nil
	}
	if len(d.deleted) == 0 && len(d.pvcUIDs) == 0 {
		record.Action(c, "teardown-s3", S3Namespace+" (nothing to remove)", true)
		return &types.OperationResponse{Success: true, Message: "No S3 storage objects created by this tool remain." + keptNote + nsHint, Logs: logs}, nil
	}
	slog.Info("s3 storage teardown", "user", kube.GetUser(c), "deleted", strings.Join(d.deleted, ", "))
	record.Action(c, "teardown-s3", S3Namespace+" (objects deleted, namespace kept)", true)

	if len(d.pvcUIDs) > 0 {
		remaining, err := WaitPVCsGone(c, d.pvcUIDs)
		if err != nil {
			return &types.OperationResponse{
				Success: false, Message: fmt.Sprintf("The S3 storage objects were deleted, but whether its PVCs are gone cannot be checked: %v.%s%s%s%s", err, kube.TemplateHint(err), dataNote, keptNote, nsHint),
				Logs: logs, ErrorCode: kube.FirstNonEmpty(kube.ErrorCodeFromK8sErr(err), "in_progress"),
			}, nil
		}
		if len(remaining) > 0 {
			logs = append(logs, fmt.Sprintf("PVC %s still terminating", strings.Join(remaining, ", ")))
			return &types.OperationResponse{
				Success: false, Message: fmt.Sprintf("The S3 storage objects were deleted, but PVC %s %s still terminating after %s; re-run teardown to check again.%s%s%s", strings.Join(remaining, ", "), kube.Verb(len(remaining), "is", "are"), s3DeleteTimeout, dataNote, keptNote, nsHint),
				Logs: logs, ErrorCode: "in_progress",
			}, nil
		}
	}
	logs = append(logs, "OK: S3 storage removed")
	return &types.OperationResponse{Success: true, Message: "S3 storage removed." + dataNote + keptNote + nsHint, Logs: logs}, nil
}

// s3OtherObjects names the Deployments and PVCs in namespace seaweedfs
// that are not the storage's own, for Tear down's "Also delete namespace"
// choice. The tool cannot list every kind (no cluster-wide Secret list), so
// the dialog says so.
func s3OtherObjects(pvcs []PVC, pvcErr error, deploys []kube.ObjectMeta, deploysErr error) []string {
	var others []string
	for _, p := range pvcs {
		if p.Meta.Name != s3PVC {
			others = append(others, "PersistentVolumeClaim "+p.Meta.Name)
		}
	}
	if pvcErr != nil {
		others = append(others, "(the PersistentVolumeClaims could not be listed: "+pvcErr.Error()+")")
	}
	for _, d := range deploys {
		if d.Name != S3Deployment {
			others = append(others, "Deployment "+d.Name)
		}
	}
	if deploysErr != nil {
		others = append(others, "(the Deployments could not be listed: "+deploysErr.Error()+")")
	}
	sort.Strings(others)
	return others
}

// namespaceDeletionDetail returns the namespace controller's reported
// reasons (the NamespaceDeletion*Failure, NamespaceContentRemaining and
// NamespaceFinalizersRemaining conditions) for a namespace that is
// terminating (deleted by someone else), or "".
func namespaceDeletionDetail(c *kube.Client, namespace string) string {
	var ns struct {
		Status struct {
			Conditions []struct {
				Type    string `json:"type"`
				Status  string `json:"status"`
				Message string `json:"message"`
			} `json:"conditions"`
		} `json:"status"`
	}
	if c.GetInto("/api/v1/namespaces/"+namespace, &ns) != nil {
		return ""
	}
	var details []string
	for _, cond := range ns.Status.Conditions {
		if cond.Status == "True" && cond.Message != "" {
			details = append(details, cond.Type+": "+kube.TruncateText(cond.Message, 300))
		}
	}
	if len(details) == 0 {
		return ""
	}
	return " Namespace status: " + strings.Join(details, "; ") + "."
}
