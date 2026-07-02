package cluster

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"

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

func getStableChannel() string {
	if v := os.Getenv("STABLE_CHANNEL"); v != "" {
		return v
	}
	return "stable-3.4"
}

// GetStatus aggregates cluster, operator, and RHOAI status into a single response.
func GetStatus(c *Client) (*types.StatusResponse, error) {
	status := &types.StatusResponse{}
	var errs []string

	user, server, err := getClusterInfo(c)
	if err != nil {
		return nil, fmt.Errorf("cluster info: %w", err)
	}
	status.Cluster = types.ClusterInfo{Server: server, User: user}

	version, err := getClusterVersion(c)
	if err != nil {
		errs = append(errs, fmt.Sprintf("cluster version: %v", err))
	} else {
		status.Cluster.Version = version
	}

	sub, err := getSubscription(c)
	if err != nil {
		errs = append(errs, fmt.Sprintf("subscription: %v", err))
	} else {
		status.Subscription = sub
	}

	csv, err := getCSV(c)
	if err != nil {
		errs = append(errs, fmt.Sprintf("csv: %v", err))
	} else {
		status.CSV = csv
	}

	cs, err := getCatalogSource(c)
	if err != nil {
		errs = append(errs, fmt.Sprintf("catalogsource: %v", err))
	} else {
		status.CatalogSource = cs
	}

	ps, err := getPullSecret(c)
	if err != nil {
		errs = append(errs, fmt.Sprintf("pull secret: %v", err))
	} else {
		status.PullSecret = ps
	}

	idms, err := getIDMS(c)
	if err != nil {
		errs = append(errs, fmt.Sprintf("image mirror: %v", err))
	} else {
		status.ImageMirror = idms
	}

	ip, err := getInstallPlan(c)
	if err != nil {
		errs = append(errs, fmt.Sprintf("installplan: %v", err))
	} else {
		status.InstallPlan = ip
	}

	cp, err := getCatalogPod(c)
	if err != nil {
		errs = append(errs, fmt.Sprintf("catalog pod: %v", err))
	} else {
		status.CatalogPod = cp
	}

	status.ConsoleURL = getConsoleURL(c)
	status.StableSource = getStableSource()
	status.StableChannel = getStableChannel()

	activity, err := GetActivity(c)
	if err != nil {
		errs = append(errs, fmt.Sprintf("activity: %v", err))
	} else {
		status.Activity = activity
	}

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
		if displayName == "Red Hat OpenShift AI" {
			return types.CSVInfo{Name: name, Version: version, Phase: phase}, nil
		}
		if fallback == nil && strings.HasPrefix(name, "rhods-operator.") {
			fallback = &types.CSVInfo{Name: name, Version: version, Phase: phase}
		}
	}
	if fallback != nil {
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

func getPullSecret(c *Client) (types.PullSecretInfo, error) {
	path := namespacedPath("v1", "secrets", "kube-system", "additional-pull-secret")
	body, _, err := c.get(path)
	if err != nil {
		if IsK8sError(err, 404) {
			return types.PullSecretInfo{Exists: false, Detail: "Secret not found"}, nil
		}
		return types.PullSecretInfo{Exists: false}, err
	}

	var secret map[string]interface{}
	if err := json.Unmarshal(body, &secret); err != nil {
		return types.PullSecretInfo{Exists: true, Detail: "Failed to parse secret response"}, nil
	}

	data, _ := secret["data"].(map[string]interface{})
	if data == nil {
		return types.PullSecretInfo{Exists: true, Detail: "Secret has no data"}, nil
	}

	dockerConfigB64, ok := data[".dockerconfigjson"].(string)
	if !ok || dockerConfigB64 == "" {
		return types.PullSecretInfo{Exists: true, Detail: "Secret is missing .dockerconfigjson key"}, nil
	}

	dockerConfigBytes, err := base64.StdEncoding.DecodeString(dockerConfigB64)
	if err != nil {
		return types.PullSecretInfo{Exists: true, Detail: "Failed to decode .dockerconfigjson"}, nil
	}

	var dockerConfig map[string]interface{}
	if err := json.Unmarshal(dockerConfigBytes, &dockerConfig); err != nil {
		return types.PullSecretInfo{Exists: true, Detail: "Invalid JSON in .dockerconfigjson"}, nil
	}

	auths, ok := dockerConfig["auths"].(map[string]interface{})
	if !ok {
		return types.PullSecretInfo{Exists: true, Detail: "Missing 'auths' key in docker config"}, nil
	}

	for key := range auths {
		if strings.Contains(key, "quay.io/rhoai") {
			return types.PullSecretInfo{Exists: true, Valid: true}, nil
		}
	}

	return types.PullSecretInfo{Exists: true, Detail: "No quay.io/rhoai entry found in auths"}, nil
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
