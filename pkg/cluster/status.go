package cluster

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// K8s resource names and namespaces used throughout RHOAI operations.
const (
	CatalogName = "rhoai-catalog-dev"
	CatalogNS   = "openshift-marketplace"
	SubName     = "rhods-operator"
	SubNS       = "redhat-ods-operator"
	IDMSSource  = "registry.redhat.io/rhoai"
)

func getStableSource() string {
	if v := os.Getenv("STABLE_SOURCE"); v != "" {
		return v
	}
	return "redhat-operators"
}

// statusRegistryReads are the reads of GetStatus that the gitops profile
// leaves out: the pull secret (a Secret, checked with Quay), the newest
// nightly of the installed stream (from Quay), and the control plane
// topology, which only the pull secret's node part uses. nil fields are
// skipped.
type statusRegistryReads struct {
	pullSecret   func(c *Client) (types.PullSecretInfo, string, error)
	nightly      func(ctx context.Context, basicAuth string, sub types.SubscriptionInfo, cs types.CatalogSourceInfo) *types.NightlyStatus
	controlPlane func(c *Client) string
}

// GetStatus aggregates cluster, operator, and RHOAI status into a single response.
// All K8s API calls run in parallel since none depend on each other.
func GetStatus(c *Client) (*types.StatusResponse, error) {
	return getStatus(c, statusRegistryReads{pullSecret: readPullSecret, nightly: getNightlyStatus, controlPlane: cachedControlPlane})
}

// GetStatusGitOps is GetStatus in the gitops profile, which reads no Secret
// and contacts no registry (docs/GITOPS.md, section 11): the pull secret is
// not read, and the nightly build is the catalog image's reference only;
// what the catalog serves comes from its PackageManifest (CatalogServes).
// pkg/cluster/gitops_offline_test.go checks that nothing it reaches sends a
// request to another host than the API server.
func GetStatusGitOps(c *Client) (*types.StatusResponse, error) {
	return getStatus(c, statusRegistryReads{})
}

// gitOpsNightlyStatus is the installed nightly build without Quay: the
// catalog image's tag and digest. Latest stays empty: in the gitops profile
// Git or an ACM policy picks the build.
func gitOpsNightlyStatus(sub types.SubscriptionInfo, cs types.CatalogSourceInfo) *types.NightlyStatus {
	if !cs.Exists || cs.Image == "" || sub.Source != CatalogName {
		return nil
	}
	return &types.NightlyStatus{Installed: nightlyBuildFromImage(cs.Image)}
}

func getStatus(c *Client, reads statusRegistryReads) (*types.StatusResponse, error) {
	status := &types.StatusResponse{}
	var mu sync.Mutex
	var errs []string

	user, server := getClusterInfo(c)
	status.Cluster = types.ClusterInfo{Server: server, User: user}

	status.StableSource = getStableSource()

	var wg sync.WaitGroup
	var quayAuth string
	// The metadata of the objects Argo CD may manage (gitOpsManagers).
	var subMeta, catalogMeta, dscMeta *objectMeta

	addErr := func(msg string) {
		mu.Lock()
		errs = append(errs, msg)
		mu.Unlock()
	}
	run := func(fn func()) {
		wg.Go(func() {
			fn()
		})
	}

	run(func() {
		target, err := cachedStableTarget(c)
		mu.Lock()
		status.StableChannel = target.Channel
		status.StableVersion = target.Version
		status.StableChannelPinned = target.Pinned
		if err != nil {
			status.StableDiscoveryError = err.Error()
		}
		mu.Unlock()
	})

	run(func() {
		if v, err := getClusterVersion(c); err != nil {
			addErr(fmt.Sprintf("cluster version: %v", err))
		} else {
			mu.Lock()
			status.Cluster.Version = v
			mu.Unlock()
		}
	})

	// The Subscription names the installed CSV, so the CSV is read by name
	// after it instead of listing every CSV in the namespace.
	run(func() {
		sub, err := readSubscription(c)
		if err != nil {
			addErr(fmt.Sprintf("subscription: %v", err))
			addErr(fmt.Sprintf("csv: %v", err))
			return
		}
		mu.Lock()
		status.Subscription = sub.info
		if sub.found {
			subMeta = &sub.meta
		}
		mu.Unlock()
		if csv, err := csvForInstalledName(c, sub.installedCSV); err != nil {
			addErr(fmt.Sprintf("csv: %v", err))
		} else {
			mu.Lock()
			status.CSV = csv
			mu.Unlock()
		}
	})

	run(func() {
		if cs, meta, err := readCatalogSource(c); err != nil {
			addErr(fmt.Sprintf("catalogsource: %v", err))
		} else {
			mu.Lock()
			status.CatalogSource, catalogMeta = cs, meta
			mu.Unlock()
		}
	})

	run(func() {
		if reads.pullSecret == nil {
			return
		}
		if ps, auth, err := reads.pullSecret(c); err != nil {
			addErr(fmt.Sprintf("pull secret: %v", err))
		} else {
			mu.Lock()
			status.PullSecret = ps
			quayAuth = auth
			mu.Unlock()
		}
	})

	controlPlane := ""
	run(func() {
		if reads.controlPlane == nil {
			return
		}
		cp := reads.controlPlane(c)
		mu.Lock()
		controlPlane = cp
		mu.Unlock()
	})

	// The tool's record of the entry it added to the nodes' pull secret
	// (node_pull_record.go), for the nodes' part below. An unreadable
	// record shows none, so the removal is not offered.
	var nodeRecord *nodePullRecord
	run(func() {
		if reads.controlPlane == nil {
			return
		}
		rec, err := readNodePullRecord(c)
		if err != nil {
			slog.Debug("node pull secret record unreadable", "error", err)
			return
		}
		mu.Lock()
		nodeRecord = rec
		mu.Unlock()
	})

	run(func() {
		if idms, err := getIDMS(c); err != nil {
			addErr(fmt.Sprintf("image mirror: %v", err))
		} else {
			mu.Lock()
			status.ImageMirror = idms
			mu.Unlock()
		}
	})

	run(func() {
		if ip, err := getInstallPlan(c); err != nil {
			addErr(fmt.Sprintf("installplan: %v", err))
		} else {
			mu.Lock()
			status.InstallPlan = ip
			mu.Unlock()
		}
	})

	run(func() {
		if cp, err := getCatalogPod(c); err != nil {
			addErr(fmt.Sprintf("catalog pod: %v", err))
		} else {
			mu.Lock()
			status.CatalogPod = cp
			mu.Unlock()
		}
	})

	run(func() {
		meta, err := checkDSCExists(c)
		mu.Lock()
		if err != nil {
			// Transient API errors (429, 503) happen during operator reinstall.
			// Default to true to avoid falsely showing the "Create DSC" prompt.
			slog.Debug("dsc check failed, assuming exists", "error", err)
			status.DSCExists = true
		} else {
			status.DSCExists = meta != nil
			dscMeta = meta
		}
		mu.Unlock()
	})

	run(func() {
		if activity, err := GetActivity(c); err != nil {
			addErr(fmt.Sprintf("activity: %v", err))
		} else {
			mu.Lock()
			status.Activity = activity
			mu.Unlock()
		}
	})

	run(func() {
		consoleURL := cachedConsoleURL(c)
		mu.Lock()
		status.ConsoleURL = consoleURL
		mu.Unlock()
	})

	run(func() {
		dashboardURL := cachedDashboardURL(c)
		mu.Lock()
		status.DashboardURL = dashboardURL
		mu.Unlock()
	})

	wg.Wait()

	gitOpsManagers(c, status, subMeta, catalogMeta, dscMeta)

	// The nodes' side of the pull secret: shown, with the command, unless
	// the control plane is known to be hosted. An unreadable topology
	// counts as possibly self-managed: the status never claims that the
	// nodes have the credentials.
	if reads.controlPlane != nil {
		status.Cluster.ControlPlane = controlPlane
		status.PullSecret.Nodes = nodePullInfo(controlPlane, status.CatalogSource)
		if status.PullSecret.Nodes != nil {
			status.PullSecret.Nodes.AddedByTool = nodePullRecordInfo(nodeRecord, quayAuth)
		}
	}

	// Cached per installed catalog image; only the first poll after an image
	// change waits for Quay (bounded by nightlyStatusTimeout).
	if reads.nightly != nil {
		status.Nightly = reads.nightly(c.ctx, quayAuth, status.Subscription, status.CatalogSource)
	} else {
		status.Nightly = gitOpsNightlyStatus(status.Subscription, status.CatalogSource)
	}
	// Under Argo CD (or an ACM policy) the catalog image is Git's choice and
	// can run ahead of the installed operator (a same-version build OLM does
	// not install by itself), so say what the catalog serves.
	if status.CatalogSource.ManagedBy != "" || status.CatalogSource.ManagedByACM || status.Subscription.ManagedBy != "" {
		status.CatalogServes = readCatalogServes(c, status.Subscription, status.CatalogSource, status.CSV)
	}

	if len(errs) > 0 {
		status.Errors = errs
	}

	return status, nil
}

// stableTargetCacheTTL bounds how long the stable channel shown on the status
// page may lag the redhat-operators catalog. The lookup downloads the whole
// PackageManifest (~800 KB), which only changes when that catalog updates.
// Operations resolve the target uncached.
const stableTargetCacheTTL = 5 * time.Minute

var stableTargetCache = newLRU[string, stableTarget](8)

// cachedStableTarget is resolveStableTarget for status polling. Only
// successful lookups are cached, so a discovery error is retried next poll.
func cachedStableTarget(c *Client) (stableTarget, error) {
	key := c.baseURL + "|" + getStableSource() + "|" + strings.TrimSpace(os.Getenv("STABLE_CHANNEL"))
	if target, ok := stableTargetCache.Get(key); ok {
		return target, nil
	}
	target, err := resolveStableTarget(c)
	if err == nil {
		stableTargetCache.Add(key, target, stableTargetCacheTTL)
	}
	return target, err
}

// The console URL of a cluster does not change while the app runs.
var consoleURLCache = newLRU[string, string](8)

func cachedConsoleURL(c *Client) string {
	if u, ok := consoleURLCache.Get(c.baseURL); ok {
		return u
	}
	u := getConsoleURL(c)
	if u != "" {
		consoleURLCache.Add(c.baseURL, u, time.Hour)
	}
	return u
}

// The dashboard's address changes only when RHOAI does (an upgrade can move
// it behind the gateway), so the status, which is polled, reads it at most
// every few minutes. The status serves both profiles, and the gitops
// profile's RBAC (deploy/rbac-gitops.yaml) has no Route read, so it uses
// the Dashboard CR only (dashboardURLFromComponent): on a release without
// status.url the app launcher shows no dashboard link rather than a guess.
// A read error is cached only briefly, so a transient failure does not hide
// the link for the full five minutes.
var dashboardURLCache = newLRU[string, string](8)

// Dashboard URL cache lifetimes: an answer (a URL or none), and a read
// error.
var (
	dashboardURLTTL      = 5 * time.Minute
	dashboardURLErrorTTL = 15 * time.Second
)

func cachedDashboardURL(c *Client) string {
	if u, ok := dashboardURLCache.Get(c.baseURL); ok {
		return u
	}
	u, err := readDashboardURLFromComponent(c)
	ttl := dashboardURLTTL
	if err != nil {
		slog.Debug("cannot read the dashboard URL", "error", err)
		ttl = dashboardURLErrorTTL
	}
	dashboardURLCache.Add(c.baseURL, u, ttl)
	return u
}

// getClusterInfo is the signed-in user's name ("unknown" when it cannot be
// read) and the API server's address.
func getClusterInfo(c *Client) (string, string) {
	// Use the username set from OAuth headers (production)
	user := c.username
	if user == "" || user == "unknown" {
		// Fall back to k8s API user lookup (dev mode)
		body, _, err := c.get("/apis/user.openshift.io/v1/users/~")
		if err == nil {
			var result map[string]any
			if json.Unmarshal(body, &result) == nil {
				if meta, ok := result["metadata"].(map[string]any); ok {
					if name, ok := meta["name"].(string); ok && name != "" {
						user = name
					}
				}
			}
		}
	}
	if user == "" {
		user = "unknown"
	}
	return user, c.baseURL
}

func getClusterVersion(c *Client) (string, error) {
	body, _, err := c.get(clusterPath("config.openshift.io/v1", "clusterversions", "version"))
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	var result map[string]any
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("unmarshal: %w", err)
	}
	statusMap, _ := result["status"].(map[string]any)
	if statusMap == nil {
		return "", nil
	}
	desired, _ := statusMap["desired"].(map[string]any)
	if desired == nil {
		return "", nil
	}
	version, _ := desired["version"].(string)
	return version, nil
}

// subscriptionDetails is the RHOAI Subscription as read by status checks.
// info.ManagedBy is not set: gitOpsManagers sets it from meta.
type subscriptionDetails struct {
	info         types.SubscriptionInfo
	installedCSV string
	found        bool
	meta         objectMeta
}

func readSubscription(c *Client) (subscriptionDetails, error) {
	path := namespacedPath("operators.coreos.com/v1alpha1", "subscriptions", SubNS, SubName)
	body, _, err := c.get(path)
	if err != nil {
		if isK8sNotFound(err) {
			return subscriptionDetails{info: types.SubscriptionInfo{State: "Not Installed", Namespace: SubNS}}, nil
		}
		return subscriptionDetails{}, fmt.Errorf("request failed: %w", err)
	}
	var sub struct {
		Metadata objectMeta `json:"metadata"`
		Spec     struct {
			Source  string `json:"source"`
			Channel string `json:"channel"`
		} `json:"spec"`
		Status *struct {
			State        *string `json:"state"`
			InstalledCSV string  `json:"installedCSV"`
		} `json:"status"`
	}
	if err := json.Unmarshal(body, &sub); err != nil {
		return subscriptionDetails{}, fmt.Errorf("unmarshal: %w", err)
	}
	state := "Unknown"
	installed := ""
	if sub.Status != nil {
		if sub.Status.State != nil {
			state = *sub.Status.State
		}
		installed = sub.Status.InstalledCSV
	}
	return subscriptionDetails{
		info: types.SubscriptionInfo{
			Namespace: SubNS,
			Name:      SubName,
			Source:    sub.Spec.Source,
			Channel:   sub.Spec.Channel,
			State:     state,
		},
		installedCSV: installed,
		found:        true,
		meta:         sub.Metadata,
	}, nil
}

func getSubscription(c *Client) (types.SubscriptionInfo, error) {
	sub, err := readSubscription(c)
	return sub.info, err
}

// csvSummary is the part of a ClusterServiceVersion the status checks read.
type csvSummary struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Version     string `json:"version"`
		DisplayName string `json:"displayName"`
	} `json:"spec"`
	Status struct {
		Phase *string `json:"phase"`
	} `json:"status"`
}

func (s csvSummary) info() types.CSVInfo {
	phase := "Unknown"
	if s.Status.Phase != nil {
		phase = *s.Status.Phase
	}
	return types.CSVInfo{Name: s.Metadata.Name, Version: s.Spec.Version, Phase: phase}
}

func getCSV(c *Client) (types.CSVInfo, error) {
	// Prefer the Subscription's installed CSV when multiple versions coexist
	// during OLM reconciliation. List order is not installation identity.
	sub, err := readSubscription(c)
	if err != nil {
		return types.CSVInfo{}, err
	}
	return csvForInstalledName(c, sub.installedCSV)
}

// csvForInstalledName returns the RHOAI CSV. When the Subscription names its
// installed CSV, that CSV is read by name (~225 KB instead of a ~475 KB list on
// a cluster with copied CSVs). The list is only read when there is no name or
// the named CSV is missing, so a stale Subscription reference is still
// detected below.
func csvForInstalledName(c *Client, installedName string) (types.CSVInfo, error) {
	if installedName != "" {
		body, _, err := c.get(namespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", SubNS, installedName))
		if err == nil {
			var csv csvSummary
			if err := json.Unmarshal(body, &csv); err != nil {
				return types.CSVInfo{}, fmt.Errorf("unmarshal: %w", err)
			}
			// Lifecycle cleanup acts on this identity, so only trust an
			// object that really is the named CSV; otherwise use the list.
			if csv.Metadata.Name == installedName {
				return csv.info(), nil
			}
		} else if !isK8sNotFound(err) {
			return types.CSVInfo{}, fmt.Errorf("request failed: %w", err)
		}
	}
	// OLM labels the CSVs it copies into this namespace from operators
	// installed elsewhere with olm.copiedFrom; they are never the RHOAI CSV.
	path := namespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", SubNS, "") +
		"?labelSelector=" + url.QueryEscape("!olm.copiedFrom")
	body, _, err := c.get(path)
	if err != nil {
		return types.CSVInfo{}, fmt.Errorf("request failed: %w", err)
	}
	var result struct {
		Items []csvSummary `json:"items"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return types.CSVInfo{}, fmt.Errorf("unmarshal: %w", err)
	}

	// Primary: match by displayName (most specific)
	// Fallback: match by name prefix "rhods-operator." (resilient to branding changes)
	var fallback *types.CSVInfo
	for _, item := range result.Items {
		info := item.info()
		name, displayName := item.Metadata.Name, item.Spec.DisplayName
		if installedName != "" {
			if name == installedName {
				return info, nil
			}
			if fallback == nil && (displayName == "Red Hat OpenShift AI" || strings.HasPrefix(name, "rhods-operator.")) {
				fallback = &info
			}
			continue
		}
		if displayName == "Red Hat OpenShift AI" {
			return info, nil
		}
		if fallback == nil && strings.HasPrefix(name, "rhods-operator.") {
			fallback = &info
		}
	}
	if fallback != nil {
		if installedName != "" {
			// A stale Subscription reference is not proof that the operator is
			// absent. Stop lifecycle cleanup rather than guessing which CSV to delete.
			return types.CSVInfo{}, fmt.Errorf("subscription references missing CSV %q, but RHOAI CSV %q still exists; wait for OLM reconciliation or resolve the stale Subscription reference before retrying", installedName, fallback.Name)
		}
		return *fallback, nil
	}
	return types.CSVInfo{Phase: "Not Found"}, nil
}

// getCatalogSource reads the nightly CatalogSource. Who manages it
// (ManagedBy, ManagedByACM) is not set: gitOpsManagers sets it from the
// metadata readCatalogSource returns.
func getCatalogSource(c *Client) (types.CatalogSourceInfo, error) {
	cs, _, err := readCatalogSource(c)
	return cs, err
}

func readCatalogSource(c *Client) (types.CatalogSourceInfo, *objectMeta, error) {
	path := namespacedPath("operators.coreos.com/v1alpha1", "catalogsources", CatalogNS, CatalogName)
	body, _, err := c.get(path)
	if err != nil {
		// 404 is expected when the CatalogSource does not exist yet
		if isK8sNotFound(err) {
			return types.CatalogSourceInfo{Exists: false, Name: CatalogName}, nil, nil
		}
		return types.CatalogSourceInfo{Exists: false, Name: CatalogName}, nil, fmt.Errorf("request failed: %w", err)
	}
	var result struct {
		Metadata objectMeta `json:"metadata"`
		Spec     struct {
			Image string `json:"image"`
		} `json:"spec"`
		Status struct {
			ConnectionState struct {
				LastObservedState *string `json:"lastObservedState"`
			} `json:"connectionState"`
		} `json:"status"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return types.CatalogSourceInfo{Exists: false, Name: CatalogName}, nil, fmt.Errorf("unmarshal: %w", err)
	}

	state := "Unknown"
	if s := result.Status.ConnectionState.LastObservedState; s != nil {
		state = *s
	}

	return types.CatalogSourceInfo{
		Exists: true,
		Name:   CatalogName,
		Image:  result.Spec.Image,
		State:  state,
	}, &result.Metadata, nil
}

// gitOpsManagers sets on status who manages the operator Subscription, the
// nightly catalog and the DataScienceCluster: the Argo CD application that
// lists them, as Argo CD answers (argocd.go; one argoCheck, so each answer
// is read once per poll), and an ACM policy that
// enforces the catalog when no Argo CD application manages it. A nil
// metadata is an object the status did not find.
func gitOpsManagers(c *Client, status *types.StatusResponse, sub, catalog, dsc *objectMeta) {
	argo := newArgoCheck(c)
	if sub != nil {
		t := argo.object(sub.Annotations, sub.Labels, argoSubscriptionObject)
		status.Subscription.ManagedBy, status.Subscription.ArgoCD = t.managedBy(), t.info()
	}
	if catalog != nil {
		t := argo.object(catalog.Annotations, catalog.Labels, argoCatalogObject)
		status.CatalogSource.ManagedBy, status.CatalogSource.ArgoCD = t.managedBy(), t.info()
		status.CatalogSource.ManagedByACM = t.managedBy() == "" && acmEnforcesSpec(*catalog)
	}
	if dsc != nil {
		t := argo.object(dsc.Annotations, dsc.Labels, argoDSCObject(dsc.Name))
		status.DSCManagedBy, status.DSCArgoCD = t.managedBy(), t.info()
	}
}

// quayCredentialCacheTTL bounds how long a Quay verdict on a credential is
// reused. Status is polled every few seconds; without a cache every poll calls
// Quay's token endpoint.
const quayCredentialCacheTTL = 5 * time.Minute

type quayCredentialVerdict struct {
	err error // nil when accepted; a rejection error otherwise
	at  time.Time
}

var (
	quayCredentialCacheMu sync.Mutex
	quayCredentialCache   = map[[sha256.Size]byte]quayCredentialVerdict{}
)

// verifyQuayCredentials confirms Quay accepts the credentials. Tests override
// it to skip the real network call.
var verifyQuayCredentials = verifyQuayCredentialsCached

// verifyQuayCredentialsCached calls the Quay token endpoint to confirm the
// credentials are accepted. Definitive answers (accepted or rejected) are cached
// per credential; transient failures are not.
func verifyQuayCredentialsCached(basicAuth string) error {
	key := sha256.Sum256([]byte(basicAuth))
	quayCredentialCacheMu.Lock()
	cached, ok := quayCredentialCache[key]
	quayCredentialCacheMu.Unlock()
	if ok && time.Since(cached.at) < quayCredentialCacheTTL {
		return cached.err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := getQuayBearerToken(ctx, quayHTTPClient, basicAuth)
	if err == nil || isQuayCredentialRejection(err) {
		quayCredentialCacheMu.Lock()
		for k, v := range quayCredentialCache {
			if time.Since(v.at) >= quayCredentialCacheTTL {
				delete(quayCredentialCache, k)
			}
		}
		quayCredentialCache[key] = quayCredentialVerdict{err: err, at: time.Now()}
		quayCredentialCacheMu.Unlock()
	}
	return err
}

func getPullSecret(c *Client) (types.PullSecretInfo, error) {
	info, _, err := readPullSecret(c)
	return info, err
}

// readPullSecret validates the pull secret and also returns the quay.io/rhoai
// credential it holds, so status polling does not read the Secret twice.
func readPullSecret(c *Client) (types.PullSecretInfo, string, error) {
	path := namespacedPath("v1", "secrets", "kube-system", "additional-pull-secret")
	body, _, err := c.get(path)
	if err != nil {
		if isK8sNotFound(err) {
			return types.PullSecretInfo{Exists: false, Detail: "Secret not found"}, "", nil
		}
		return types.PullSecretInfo{Exists: false}, "", err
	}

	dockerConfig, problem := decodeDockerConfigSecret(body)
	if problem != "" {
		return types.PullSecretInfo{Exists: true, Detail: problem}, "", nil
	}

	auths, ok := dockerConfig["auths"].(map[string]any)
	if !ok {
		return types.PullSecretInfo{Exists: true, Detail: "Missing 'auths' key in docker config"}, "", nil
	}

	// Validate the same credential that registry calls use.
	basicAuth := selectQuayAuth(auths)
	if basicAuth == "" {
		return types.PullSecretInfo{Exists: true, Detail: "No quay.io/rhoai entry found in auths"}, "", nil
	}

	if err := verifyQuayCredentials(basicAuth); err != nil {
		if isQuayCredentialRejection(err) {
			return types.PullSecretInfo{Exists: true, Detail: fmt.Sprintf("Credentials rejected by Quay: %v", err)}, basicAuth, nil
		}
		return types.PullSecretInfo{Exists: true, Detail: fmt.Sprintf("Could not verify credentials with Quay: %v", err)}, basicAuth, nil
	}

	return types.PullSecretInfo{Exists: true, Valid: true}, basicAuth, nil
}

func getConsoleURL(c *Client) string {
	body, _, err := c.get(clusterPath("config.openshift.io/v1", "consoles", "cluster"))
	if err != nil {
		return ""
	}
	var result struct {
		Status struct {
			ConsoleURL string `json:"consoleURL"`
		} `json:"status"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return ""
	}
	return result.Status.ConsoleURL
}

// getInstallPlan lists InstallPlans in SubNS and returns the latest one
// whose clusterServiceVersionNames include "rhods-operator".
func getInstallPlan(c *Client) (*types.InstallPlanInfo, error) {
	path := namespacedPath("operators.coreos.com/v1alpha1", "installplans", SubNS, "")
	body, _, err := c.get(path)
	if err != nil {
		if isK8sNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("request failed: %w", err)
	}
	var result struct {
		Items []struct {
			Metadata struct {
				Name              string `json:"name"`
				CreationTimestamp string `json:"creationTimestamp"`
			} `json:"metadata"`
			Spec struct {
				Approved                   bool     `json:"approved"`
				ClusterServiceVersionNames []string `json:"clusterServiceVersionNames"`
			} `json:"spec"`
			Status struct {
				Phase *string `json:"phase"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}

	var latest *types.InstallPlanInfo
	var latestTime string

	for _, item := range result.Items {
		isRhods := false
		for _, name := range item.Spec.ClusterServiceVersionNames {
			if strings.Contains(name, "rhods-operator") {
				isRhods = true
				break
			}
		}
		if !isRhods {
			continue
		}

		phase := "Unknown"
		if item.Status.Phase != nil {
			phase = *item.Status.Phase
		}

		if latestTime == "" || item.Metadata.CreationTimestamp > latestTime {
			latestTime = item.Metadata.CreationTimestamp
			latest = &types.InstallPlanInfo{
				Name:     item.Metadata.Name,
				Phase:    phase,
				Approved: item.Spec.Approved,
			}
		}
	}

	return latest, nil
}

// getCatalogPod finds the pod backing the rhoai-catalog-dev CatalogSource.
func getCatalogPod(c *Client) (*types.CatalogPodInfo, error) {
	path := namespacedPath("v1", "pods", CatalogNS, "") + "?labelSelector=olm.catalogSource%3D" + CatalogName
	body, _, err := c.get(path)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	var result struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Status struct {
				Phase             string `json:"phase"`
				ContainerStatuses []struct {
					Ready        bool `json:"ready"`
					RestartCount int  `json:"restartCount"`
				} `json:"containerStatuses"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	if len(result.Items) == 0 {
		return nil, nil
	}

	// Take the first (usually only) catalog pod
	pod := result.Items[0]
	info := &types.CatalogPodInfo{Name: pod.Metadata.Name, Phase: pod.Status.Phase}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Ready {
			info.Ready = true
		}
		info.RestartCount += cs.RestartCount
	}
	return info, nil
}

func getIDMS(c *Client) (types.ImageMirrorInfo, error) {
	path := clusterPath("config.openshift.io/v1", "imagedigestmirrorsets", "")
	body, _, err := c.get(path)
	if err != nil {
		return types.ImageMirrorInfo{Exists: false}, fmt.Errorf("request failed: %w", err)
	}
	var result struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				ImageDigestMirrors []struct {
					Source string `json:"source"`
				} `json:"imageDigestMirrors"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return types.ImageMirrorInfo{Exists: false}, fmt.Errorf("unmarshal: %w", err)
	}
	for _, item := range result.Items {
		for _, mirror := range item.Spec.ImageDigestMirrors {
			if mirror.Source == IDMSSource {
				return types.ImageMirrorInfo{Exists: true, Kind: mirrorKindIDMS, Name: item.Metadata.Name, Source: mirror.Source}, nil
			}
		}
	}
	// An ImageContentSourcePolicy mirrors the source just as well: the
	// Machine Config Operator merges its mirrors with the IDMSs' (runtime-utils
	// @7bdb5b9c pkg/registries/registries.go mergedDigestMirrorSets,
	// :165-183).
	icsp, err := listICSPRules(c)
	if err != nil {
		return types.ImageMirrorInfo{Exists: false}, fmt.Errorf("list ImageContentSourcePolicies: %w", err)
	}
	for _, r := range icsp {
		if r.Source == IDMSSource {
			return types.ImageMirrorInfo{Exists: true, Kind: mirrorKindICSP, Name: r.Name, Source: r.Source}, nil
		}
	}
	return types.ImageMirrorInfo{Exists: false}, nil
}

// checkDSCExists returns the metadata of the first DataScienceCluster,
// listed at the newest served version that works (listServed), or nil when
// there is none. No served version (CRD not installed) is not an error.
func checkDSCExists(c *Client) (*objectMeta, error) {
	// limit=1: only existence matters, not the (large) objects.
	r, err := listServed(c, dscGroup, dscListFmt, url.Values{"limit": {"1"}}, dscFallbackVersions)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	if r.Version == "" {
		return nil, nil
	}
	var dscList struct {
		Items []struct {
			Metadata objectMeta `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(r.Body, &dscList); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	if len(dscList.Items) == 0 {
		return nil, nil
	}
	return &dscList.Items[0].Metadata, nil
}
