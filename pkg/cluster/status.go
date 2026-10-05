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

// GetStatus aggregates cluster, operator, and RHOAI status into a single response.
// All K8s API calls run in parallel since none depend on each other.
func GetStatus(c *Client) (*types.StatusResponse, error) {
	status := &types.StatusResponse{}
	var mu sync.Mutex
	var errs []string

	user, server, err := getClusterInfo(c)
	if err != nil {
		return nil, fmt.Errorf("cluster info: %w", err)
	}
	status.Cluster = types.ClusterInfo{Server: server, User: user}

	status.StableSource = getStableSource()

	var wg sync.WaitGroup
	var quayAuth string

	addErr := func(msg string) {
		mu.Lock()
		errs = append(errs, msg)
		mu.Unlock()
	}
	run := func(fn func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fn()
		}()
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
		if cs, err := getCatalogSource(c); err != nil {
			addErr(fmt.Sprintf("catalogsource: %v", err))
		} else {
			mu.Lock()
			status.CatalogSource = cs
			mu.Unlock()
		}
	})

	run(func() {
		if ps, auth, err := readPullSecret(c); err != nil {
			addErr(fmt.Sprintf("pull secret: %v", err))
		} else {
			mu.Lock()
			status.PullSecret = ps
			quayAuth = auth
			mu.Unlock()
		}
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
		exists, err := checkDSCExists(c)
		mu.Lock()
		if err != nil {
			// Transient API errors (429, 503) happen during operator reinstall.
			// Default to true to avoid falsely showing the "Create DSC" prompt.
			slog.Debug("dsc check failed, assuming exists", "error", err)
			status.DSCExists = true
		} else {
			status.DSCExists = exists
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

	wg.Wait()

	// Cached per installed catalog image; only the first poll after an image
	// change waits for Quay (bounded by nightlyStatusTimeout).
	status.Nightly = getNightlyStatus(c.ctx, quayAuth, status.Subscription, status.CatalogSource)

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

func getClusterInfo(c *Client) (string, string, error) {
	// Use the username set from OAuth headers (production)
	user := c.username
	if user == "" || user == "unknown" {
		// Fall back to k8s API user lookup (dev mode)
		body, _, err := c.get("/apis/user.openshift.io/v1/users/~")
		if err == nil {
			var result map[string]interface{}
			if json.Unmarshal(body, &result) == nil {
				if meta, ok := result["metadata"].(map[string]interface{}); ok {
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
	return user, c.baseURL, nil
}

func getClusterVersion(c *Client) (string, error) {
	body, _, err := c.get(clusterPath("config.openshift.io/v1", "clusterversions", "version"))
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("unmarshal: %w", err)
	}
	statusMap, _ := result["status"].(map[string]interface{})
	if statusMap == nil {
		return "", nil
	}
	desired, _ := statusMap["desired"].(map[string]interface{})
	if desired == nil {
		return "", nil
	}
	version, _ := desired["version"].(string)
	return version, nil
}

// subscriptionDetails is the RHOAI Subscription as read by status checks.
type subscriptionDetails struct {
	info         types.SubscriptionInfo
	installedCSV string
	found        bool
}

func readSubscription(c *Client) (subscriptionDetails, error) {
	path := namespacedPath("operators.coreos.com/v1alpha1", "subscriptions", SubNS, SubName)
	body, _, err := c.get(path)
	if err != nil {
		if IsK8sError(err, 404) {
			return subscriptionDetails{info: types.SubscriptionInfo{State: "Not Installed"}}, nil
		}
		return subscriptionDetails{}, fmt.Errorf("request failed: %w", err)
	}
	var sub struct {
		Spec struct {
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
			Name:    SubName,
			Source:  sub.Spec.Source,
			Channel: sub.Spec.Channel,
			State:   state,
		},
		installedCSV: installed,
		found:        true,
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
		} else if !IsK8sError(err, 404) {
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

func getCatalogSource(c *Client) (types.CatalogSourceInfo, error) {
	path := namespacedPath("operators.coreos.com/v1alpha1", "catalogsources", CatalogNS, CatalogName)
	body, _, err := c.get(path)
	if err != nil {
		// 404 is expected when the CatalogSource does not exist yet
		if IsK8sError(err, 404) {
			return types.CatalogSourceInfo{Exists: false, Name: CatalogName}, nil
		}
		return types.CatalogSourceInfo{Exists: false, Name: CatalogName}, fmt.Errorf("request failed: %w", err)
	}
	var result struct {
		Spec struct {
			Image string `json:"image"`
		} `json:"spec"`
		Status struct {
			ConnectionState struct {
				LastObservedState *string `json:"lastObservedState"`
			} `json:"connectionState"`
		} `json:"status"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return types.CatalogSourceInfo{Exists: false, Name: CatalogName}, fmt.Errorf("unmarshal: %w", err)
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
	}, nil
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
		if IsK8sError(err, 404) {
			return types.PullSecretInfo{Exists: false, Detail: "Secret not found"}, "", nil
		}
		return types.PullSecretInfo{Exists: false}, "", err
	}

	dockerConfig, problem := decodeDockerConfigSecret(body)
	if problem != "" {
		return types.PullSecretInfo{Exists: true, Detail: problem}, "", nil
	}

	auths, ok := dockerConfig["auths"].(map[string]interface{})
	if !ok {
		return types.PullSecretInfo{Exists: true, Detail: "Missing 'auths' key in docker config"}, "", nil
	}

	// Validate the same credential that registry calls use.
	basicAuth, _ := selectQuayAuth(auths)
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
		if IsK8sError(err, 404) {
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
				return types.ImageMirrorInfo{Exists: true, Name: item.Metadata.Name, Source: mirror.Source}, nil
			}
		}
	}
	return types.ImageMirrorInfo{Exists: false}, nil
}

// checkDSCExists returns true if at least one DataScienceCluster exists.
// Tries v2 API first, falls back to v1 for older RHOAI versions.
// 404 errors (CRD not installed) are not treated as errors — they return false.
func checkDSCExists(c *Client) (bool, error) {
	// Try v2 first. limit=1: only existence matters, not the (large) objects.
	dscBody, _, err := c.get("/apis/datasciencecluster.opendatahub.io/v2/datascienceclusters?limit=1")
	if err != nil {
		// Fall back to v1
		dscBody, _, err = c.get("/apis/datasciencecluster.opendatahub.io/v1/datascienceclusters?limit=1")
		if err != nil {
			// 404 means the CRD is not installed — this is not an error, just means no DSC
			if IsK8sError(err, 404) {
				return false, nil
			}
			return false, fmt.Errorf("request failed: %w", err)
		}
	}

	var dscList struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(dscBody, &dscList); err != nil {
		return false, fmt.Errorf("unmarshal: %w", err)
	}
	return len(dscList.Items) > 0, nil
}
