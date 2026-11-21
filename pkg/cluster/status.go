package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/argo"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/catalog"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/dashdev"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/node"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/registry"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

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
	return getStatus(c, statusRegistryReads{pullSecret: registry.ReadPullSecret, nightly: registry.NightlyStatus, controlPlane: rhoai.CachedControlPlane})
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
	if !cs.Exists || cs.Image == "" || sub.Source != rhoai.CatalogName {
		return nil
	}
	return &types.NightlyStatus{Installed: registry.NightlyBuildFromImage(cs.Image)}
}

func getStatus(c *Client, reads statusRegistryReads) (*types.StatusResponse, error) {
	status := &types.StatusResponse{}
	var mu sync.Mutex
	var errs []string

	user, server := getClusterInfo(c)
	status.Cluster = types.ClusterInfo{Server: server, User: user}

	status.StableSource = catalog.StableSource()

	var wg sync.WaitGroup
	var quayAuth string
	// The metadata of the objects Argo CD may manage (gitOpsManagers).
	var subMeta, catalogMeta, dscMeta *kube.ObjectMeta

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
		sub, err := rhoai.ReadSubscription(c)
		if err != nil {
			addErr(fmt.Sprintf("subscription: %v", err))
			addErr(fmt.Sprintf("csv: %v", err))
			return
		}
		mu.Lock()
		status.Subscription = sub.Info
		if sub.Found {
			subMeta = &sub.Meta
		}
		mu.Unlock()
		if csv, err := rhoai.CSVForInstalledName(c, sub.InstalledCSV); err != nil {
			addErr(fmt.Sprintf("csv: %v", err))
		} else {
			mu.Lock()
			status.CSV = csv
			mu.Unlock()
		}
	})

	run(func() {
		if cs, meta, err := rhoai.ReadCatalogSource(c); err != nil {
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
	var nodeRecord *node.PullRecord
	run(func() {
		if reads.controlPlane == nil {
			return
		}
		rec, err := node.ReadPullRecord(c)
		if err != nil {
			slog.Debug("node pull secret record unreadable", "error", err)
			return
		}
		mu.Lock()
		nodeRecord = rec
		mu.Unlock()
	})

	run(func() {
		if idms, err := node.ImageMirror(c); err != nil {
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
		status.PullSecret.Nodes = rhoai.NodePullInfo(controlPlane, status.CatalogSource)
		if status.PullSecret.Nodes != nil {
			status.PullSecret.Nodes.AddedByTool = node.PullRecordInfo(nodeRecord, quayAuth)
		}
	}

	// Cached per installed catalog image; only the first poll after an image
	// change waits for Quay (bounded by nightlyStatusTimeout).
	if reads.nightly != nil {
		status.Nightly = reads.nightly(c.Context(), quayAuth, status.Subscription, status.CatalogSource)
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

var stableTargetCache = kube.NewLRU[string, catalog.StableTarget](8)

// cachedStableTarget is resolveStableTarget for status polling. Only
// successful lookups are cached, so a discovery error is retried next poll.
func cachedStableTarget(c *Client) (catalog.StableTarget, error) {
	key := c.BaseURL() + "|" + catalog.StableSource() + "|" + strings.TrimSpace(os.Getenv("STABLE_CHANNEL"))
	if target, ok := stableTargetCache.Get(key); ok {
		return target, nil
	}
	target, err := catalog.ResolveStableTarget(c)
	if err == nil {
		stableTargetCache.Add(key, target, stableTargetCacheTTL)
	}
	return target, err
}

// The console URL of a cluster does not change while the app runs.
var consoleURLCache = kube.NewLRU[string, string](8)

func cachedConsoleURL(c *Client) string {
	if u, ok := consoleURLCache.Get(c.BaseURL()); ok {
		return u
	}
	u := getConsoleURL(c)
	if u != "" {
		consoleURLCache.Add(c.BaseURL(), u, time.Hour)
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
var dashboardURLCache = kube.NewLRU[string, string](8)

// Dashboard URL cache lifetimes: an answer (a URL or none), and a read
// error.
var (
	dashboardURLTTL      = 5 * time.Minute
	dashboardURLErrorTTL = 15 * time.Second
)

func cachedDashboardURL(c *Client) string {
	if u, ok := dashboardURLCache.Get(c.BaseURL()); ok {
		return u
	}
	u, err := dashdev.ReadComponentURL(c)
	ttl := dashboardURLTTL
	if err != nil {
		slog.Debug("cannot read the dashboard URL", "error", err)
		ttl = dashboardURLErrorTTL
	}
	dashboardURLCache.Add(c.BaseURL(), u, ttl)
	return u
}

// getClusterInfo is the signed-in user's name ("unknown" when it cannot be
// read) and the API server's address.
func getClusterInfo(c *Client) (string, string) {
	// Use the username set from OAuth headers (production)
	user := c.Username()
	if user == "" || user == "unknown" {
		// Fall back to k8s API user lookup (dev mode)
		body, _, err := c.Get("/apis/user.openshift.io/v1/users/~")
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
	return user, c.BaseURL()
}

func getClusterVersion(c *Client) (string, error) {
	var result map[string]any
	if err := c.GetInto(rhoai.ClusterPath("config.openshift.io/v1", "clusterversions", "version"), &result); err != nil {
		if kube.IsDecodeError(err) {
			return "", fmt.Errorf("unmarshal: %w", err)
		}
		return "", fmt.Errorf("request failed: %w", err)
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

// gitOpsManagers sets on status who manages the operator Subscription, the
// nightly catalog and the DataScienceCluster: the Argo CD application that
// lists them, as Argo CD answers (argocd.go; one argoCheck, so each answer
// is read once per poll), and an ACM policy that
// enforces the catalog when no Argo CD application manages it. A nil
// metadata is an object the status did not find.
func gitOpsManagers(c *Client, status *types.StatusResponse, sub, catalogSrc, dsc *kube.ObjectMeta) {
	argoCD := argo.NewCheck(c)
	if sub != nil {
		t := argoCD.Object(sub.Annotations, sub.Labels, argo.SubscriptionObject)
		status.Subscription.ManagedBy, status.Subscription.ArgoCD = t.ManagedBy(), t.Info()
	}
	if catalogSrc != nil {
		t := argoCD.Object(catalogSrc.Annotations, catalogSrc.Labels, argo.CatalogObject)
		status.CatalogSource.ManagedBy, status.CatalogSource.ArgoCD = t.ManagedBy(), t.Info()
		status.CatalogSource.ManagedByACM = t.ManagedBy() == "" && argo.ACMEnforcesSpec(*catalogSrc)
	}
	if dsc != nil {
		t := argoCD.Object(dsc.Annotations, dsc.Labels, argo.DSCObject(dsc.Name))
		status.DSCManagedBy, status.DSCArgoCD = t.ManagedBy(), t.Info()
	}
}

func getConsoleURL(c *Client) string {
	var result struct {
		Status struct {
			ConsoleURL string `json:"consoleURL"`
		} `json:"status"`
	}
	if err := c.GetInto(rhoai.ClusterPath("config.openshift.io/v1", "consoles", "cluster"), &result); err != nil {
		return ""
	}
	return result.Status.ConsoleURL
}

// getInstallPlan lists InstallPlans in SubNS and returns the latest one
// whose clusterServiceVersionNames include "rhods-operator".
func getInstallPlan(c *Client) (*types.InstallPlanInfo, error) {
	path := rhoai.NamespacedPath("operators.coreos.com/v1alpha1", "installplans", rhoai.SubNS, "")
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
	found, err := c.GetIntoOptional(path, &result)
	if err != nil {
		if kube.IsDecodeError(err) {
			return nil, fmt.Errorf("unmarshal: %w", err)
		}
		return nil, fmt.Errorf("request failed: %w", err)
	}
	if !found {
		return nil, nil
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
	path := rhoai.NamespacedPath("v1", "pods", rhoai.CatalogNS, "") + "?labelSelector=olm.catalogSource%3D" + rhoai.CatalogName
	var items []struct {
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
	}
	if err := c.ListInto(path, &items); err != nil {
		if kube.IsDecodeError(err) {
			return nil, fmt.Errorf("unmarshal: %w", err)
		}
		return nil, fmt.Errorf("request failed: %w", err)
	}
	if len(items) == 0 {
		return nil, nil
	}

	// Take the first (usually only) catalog pod
	pod := items[0]
	info := &types.CatalogPodInfo{Name: pod.Metadata.Name, Phase: pod.Status.Phase}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Ready {
			info.Ready = true
		}
		info.RestartCount += cs.RestartCount
	}
	return info, nil
}

// checkDSCExists returns the metadata of the first DataScienceCluster,
// listed at the newest served version that works (listServed), or nil when
// there is none. No served version (CRD not installed) is not an error.
func checkDSCExists(c *Client) (*kube.ObjectMeta, error) {
	// limit=1: only existence matters, not the (large) objects.
	r, err := rhoai.ListServed(c, rhoai.DSCGroup, rhoai.DSCListFmt, url.Values{"limit": {"1"}}, rhoai.DSCFallbackVersions)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	if r.Version == "" {
		return nil, nil
	}
	var dscList struct {
		Items []struct {
			Metadata kube.ObjectMeta `json:"metadata"`
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
