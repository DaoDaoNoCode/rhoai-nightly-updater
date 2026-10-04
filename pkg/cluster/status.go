package cluster

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
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

	addErr := func(msg string) {
		mu.Lock()
		errs = append(errs, msg)
		mu.Unlock()
	}

	wg.Add(11)

	go func() {
		defer wg.Done()
		target, err := resolveStableTarget(c)
		mu.Lock()
		status.StableChannel = target.Channel
		status.StableVersion = target.Version
		status.StableChannelPinned = target.Pinned
		if err != nil {
			status.StableDiscoveryError = err.Error()
		}
		mu.Unlock()
	}()

	go func() {
		defer wg.Done()
		if v, err := getClusterVersion(c); err != nil {
			addErr(fmt.Sprintf("cluster version: %v", err))
		} else {
			mu.Lock()
			status.Cluster.Version = v
			mu.Unlock()
		}
	}()

	go func() {
		defer wg.Done()
		if sub, err := getSubscription(c); err != nil {
			addErr(fmt.Sprintf("subscription: %v", err))
		} else {
			mu.Lock()
			status.Subscription = sub
			mu.Unlock()
		}
	}()

	go func() {
		defer wg.Done()
		if csv, err := getCSV(c); err != nil {
			addErr(fmt.Sprintf("csv: %v", err))
		} else {
			mu.Lock()
			status.CSV = csv
			mu.Unlock()
		}
	}()

	go func() {
		defer wg.Done()
		if cs, err := getCatalogSource(c); err != nil {
			addErr(fmt.Sprintf("catalogsource: %v", err))
		} else {
			mu.Lock()
			status.CatalogSource = cs
			mu.Unlock()
		}
	}()

	go func() {
		defer wg.Done()
		if ps, err := getPullSecret(c); err != nil {
			addErr(fmt.Sprintf("pull secret: %v", err))
		} else {
			mu.Lock()
			status.PullSecret = ps
			mu.Unlock()
		}
	}()

	go func() {
		defer wg.Done()
		if idms, err := getIDMS(c); err != nil {
			addErr(fmt.Sprintf("image mirror: %v", err))
		} else {
			mu.Lock()
			status.ImageMirror = idms
			mu.Unlock()
		}
	}()

	go func() {
		defer wg.Done()
		if ip, err := getInstallPlan(c); err != nil {
			addErr(fmt.Sprintf("installplan: %v", err))
		} else {
			mu.Lock()
			status.InstallPlan = ip
			mu.Unlock()
		}
	}()

	go func() {
		defer wg.Done()
		if cp, err := getCatalogPod(c); err != nil {
			addErr(fmt.Sprintf("catalog pod: %v", err))
		} else {
			mu.Lock()
			status.CatalogPod = cp
			mu.Unlock()
		}
	}()

	go func() {
		defer wg.Done()
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
	}()

	go func() {
		defer wg.Done()
		if activity, err := GetActivity(c); err != nil {
			addErr(fmt.Sprintf("activity: %v", err))
		} else {
			mu.Lock()
			status.Activity = activity
			mu.Unlock()
		}
	}()

	wg.Wait()

	status.ConsoleURL = getConsoleURL(c)

	if len(errs) > 0 {
		status.Errors = errs
	}

	return status, nil
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

func getSubscription(c *Client) (types.SubscriptionInfo, error) {
	path := namespacedPath("operators.coreos.com/v1alpha1", "subscriptions", SubNS, SubName)
	body, _, err := c.get(path)
	if err != nil {
		if IsK8sError(err, 404) {
			return types.SubscriptionInfo{State: "Not Installed"}, nil
		}
		return types.SubscriptionInfo{}, fmt.Errorf("request failed: %w", err)
	}
	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return types.SubscriptionInfo{}, fmt.Errorf("unmarshal: %w", err)
	}
	spec, _ := result["spec"].(map[string]interface{})
	source, _ := spec["source"].(string)
	channel, _ := spec["channel"].(string)

	state := "Unknown"
	if status, ok := result["status"].(map[string]interface{}); ok {
		if s, ok := status["state"].(string); ok {
			state = s
		}
	}

	return types.SubscriptionInfo{
		Name:    SubName,
		Source:  source,
		Channel: channel,
		State:   state,
	}, nil
}

func getCSV(c *Client) (types.CSVInfo, error) {
	// Prefer the Subscription's installed CSV when multiple versions coexist
	// during OLM reconciliation. List order is not installation identity.
	installedName := ""
	subBody, _, subErr := c.get(namespacedPath("operators.coreos.com/v1alpha1", "subscriptions", SubNS, SubName))
	if subErr != nil && !IsK8sError(subErr, 404) {
		return types.CSVInfo{}, subErr
	}
	if subErr == nil {
		var sub struct {
			Status struct {
				InstalledCSV string `json:"installedCSV"`
			} `json:"status"`
		}
		if err := json.Unmarshal(subBody, &sub); err != nil {
			return types.CSVInfo{}, err
		}
		installedName = sub.Status.InstalledCSV
	}
	path := namespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", SubNS, "")
	body, _, err := c.get(path)
	if err != nil {
		return types.CSVInfo{}, fmt.Errorf("request failed: %w", err)
	}
	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return types.CSVInfo{}, fmt.Errorf("unmarshal: %w", err)
	}
	items, _ := result["items"].([]interface{})

	// Primary: match by displayName (most specific)
	// Fallback: match by name prefix "rhods-operator." (resilient to branding changes)
	var fallback *types.CSVInfo
	for _, item := range items {
		obj, _ := item.(map[string]interface{})
		meta, _ := obj["metadata"].(map[string]interface{})
		name, _ := meta["name"].(string)
		spec, _ := obj["spec"].(map[string]interface{})
		version, _ := spec["version"].(string)
		phase := "Unknown"
		if status, ok := obj["status"].(map[string]interface{}); ok {
			if p, ok := status["phase"].(string); ok {
				phase = p
			}
		}

		displayName, _ := spec["displayName"].(string)
		if installedName != "" {
			if name == installedName {
				return types.CSVInfo{Name: name, Version: version, Phase: phase}, nil
			}
			if fallback == nil && (displayName == "Red Hat OpenShift AI" || strings.HasPrefix(name, "rhods-operator.")) {
				fallback = &types.CSVInfo{Name: name, Version: version, Phase: phase}
			}
			continue
		}
		if displayName == "Red Hat OpenShift AI" {
			return types.CSVInfo{Name: name, Version: version, Phase: phase}, nil
		}
		if fallback == nil && strings.HasPrefix(name, "rhods-operator.") {
			fallback = &types.CSVInfo{Name: name, Version: version, Phase: phase}
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
	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return types.CatalogSourceInfo{Exists: false, Name: CatalogName}, fmt.Errorf("unmarshal: %w", err)
	}
	spec, _ := result["spec"].(map[string]interface{})
	image, _ := spec["image"].(string)

	state := "Unknown"
	if status, ok := result["status"].(map[string]interface{}); ok {
		if conn, ok := status["connectionState"].(map[string]interface{}); ok {
			if s, ok := conn["lastObservedState"].(string); ok {
				state = s
			}
		}
	}

	return types.CatalogSourceInfo{
		Exists: true,
		Name:   CatalogName,
		Image:  image,
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
	path := namespacedPath("v1", "secrets", "kube-system", "additional-pull-secret")
	body, _, err := c.get(path)
	if err != nil {
		if IsK8sError(err, 404) {
			return types.PullSecretInfo{Exists: false, Detail: "Secret not found"}, nil
		}
		return types.PullSecretInfo{Exists: false}, err
	}

	dockerConfig, problem := decodeDockerConfigSecret(body)
	if problem != "" {
		return types.PullSecretInfo{Exists: true, Detail: problem}, nil
	}

	auths, ok := dockerConfig["auths"].(map[string]interface{})
	if !ok {
		return types.PullSecretInfo{Exists: true, Detail: "Missing 'auths' key in docker config"}, nil
	}

	// Validate the same credential that registry calls use.
	basicAuth, _ := selectQuayAuth(auths)
	if basicAuth == "" {
		return types.PullSecretInfo{Exists: true, Detail: "No quay.io/rhoai entry found in auths"}, nil
	}

	if err := verifyQuayCredentials(basicAuth); err != nil {
		if isQuayCredentialRejection(err) {
			return types.PullSecretInfo{Exists: true, Detail: fmt.Sprintf("Credentials rejected by Quay: %v", err)}, nil
		}
		return types.PullSecretInfo{Exists: true, Detail: fmt.Sprintf("Could not verify credentials with Quay: %v", err)}, nil
	}

	return types.PullSecretInfo{Exists: true, Valid: true}, nil
}

func getConsoleURL(c *Client) string {
	body, _, err := c.get(clusterPath("config.openshift.io/v1", "consoles", "cluster"))
	if err != nil {
		return ""
	}
	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return ""
	}
	status, _ := result["status"].(map[string]interface{})
	consoleURL, _ := status["consoleURL"].(string)
	return consoleURL
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
	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	items, _ := result["items"].([]interface{})

	var latest *types.InstallPlanInfo
	var latestTime string

	for _, item := range items {
		obj, _ := item.(map[string]interface{})
		spec, _ := obj["spec"].(map[string]interface{})
		csvNames, _ := spec["clusterServiceVersionNames"].([]interface{})

		isRhods := false
		for _, name := range csvNames {
			nameStr, _ := name.(string)
			if strings.Contains(nameStr, "rhods-operator") {
				isRhods = true
				break
			}
		}
		if !isRhods {
			continue
		}

		meta, _ := obj["metadata"].(map[string]interface{})
		name, _ := meta["name"].(string)
		creationTimestamp, _ := meta["creationTimestamp"].(string)
		approved, _ := spec["approved"].(bool)

		phase := "Unknown"
		if status, ok := obj["status"].(map[string]interface{}); ok {
			if p, ok := status["phase"].(string); ok {
				phase = p
			}
		}

		if latestTime == "" || creationTimestamp > latestTime {
			latestTime = creationTimestamp
			latest = &types.InstallPlanInfo{
				Name:     name,
				Phase:    phase,
				Approved: approved,
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
	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("unmarshal: %w", err)
	}
	items, _ := result["items"].([]interface{})
	if len(items) == 0 {
		return nil, nil
	}

	// Take the first (usually only) catalog pod
	obj, _ := items[0].(map[string]interface{})
	meta, _ := obj["metadata"].(map[string]interface{})
	name, _ := meta["name"].(string)

	status, _ := obj["status"].(map[string]interface{})
	phase, _ := status["phase"].(string)

	ready := false
	restartCount := 0

	containerStatuses, _ := status["containerStatuses"].([]interface{})
	for _, cs := range containerStatuses {
		csMap, _ := cs.(map[string]interface{})
		if r, ok := csMap["ready"].(bool); ok && r {
			ready = true
		}
		if rc, ok := csMap["restartCount"].(float64); ok {
			restartCount += int(rc)
		}
	}

	return &types.CatalogPodInfo{
		Name:         name,
		Phase:        phase,
		Ready:        ready,
		RestartCount: restartCount,
	}, nil
}

func getIDMS(c *Client) (types.ImageMirrorInfo, error) {
	path := clusterPath("config.openshift.io/v1", "imagedigestmirrorsets", "")
	body, _, err := c.get(path)
	if err != nil {
		return types.ImageMirrorInfo{Exists: false}, fmt.Errorf("request failed: %w", err)
	}
	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return types.ImageMirrorInfo{Exists: false}, fmt.Errorf("unmarshal: %w", err)
	}
	items, _ := result["items"].([]interface{})
	for _, item := range items {
		obj, _ := item.(map[string]interface{})
		spec, _ := obj["spec"].(map[string]interface{})
		mirrors, _ := spec["imageDigestMirrors"].([]interface{})
		for _, m := range mirrors {
			mirror, _ := m.(map[string]interface{})
			source, _ := mirror["source"].(string)
			if source == IDMSSource {
				meta, _ := obj["metadata"].(map[string]interface{})
				name, _ := meta["name"].(string)
				return types.ImageMirrorInfo{Exists: true, Name: name, Source: source}, nil
			}
		}
	}
	return types.ImageMirrorInfo{Exists: false}, nil
}

// checkDSCExists returns true if at least one DataScienceCluster exists.
// Tries v2 API first, falls back to v1 for older RHOAI versions.
// 404 errors (CRD not installed) are not treated as errors — they return false.
func checkDSCExists(c *Client) (bool, error) {
	// Try v2 first
	dscBody, _, err := c.get("/apis/datasciencecluster.opendatahub.io/v2/datascienceclusters")
	if err != nil {
		// Fall back to v1
		dscBody, _, err = c.get("/apis/datasciencecluster.opendatahub.io/v1/datascienceclusters")
		if err != nil {
			// 404 means the CRD is not installed — this is not an error, just means no DSC
			if IsK8sError(err, 404) {
				return false, nil
			}
			return false, fmt.Errorf("request failed: %w", err)
		}
	}

	var dscList map[string]interface{}
	if err := json.Unmarshal(dscBody, &dscList); err != nil {
		return false, fmt.Errorf("unmarshal: %w", err)
	}

	items, _ := dscList["items"].([]interface{})
	return len(items) > 0, nil
}
