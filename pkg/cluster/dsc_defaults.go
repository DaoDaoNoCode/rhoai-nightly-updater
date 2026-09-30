package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

const dscSamplesBaseURL = "https://raw.githubusercontent.com/red-hat-data-services/rhods-operator/"

var dscVersionPattern = regexp.MustCompile(`(?i)^(?:rhoai-|v)?([0-9]+)\.([0-9]+)(?:\.[0-9]+)?(?:[-.]([a-z]+)(?:\.?([0-9]+))?)?(?:[+.-].*)?$`)

type dscDefaults struct {
	Spec      map[string]interface{}
	YAML      string
	Version   string
	Branch    string
	SourceURL string
}

// Store immutable YAML, keyed by branch and API version, so upgrades cannot reuse old defaults.
var dscDefaultsCache = struct {
	sync.RWMutex
	entries map[string]dscDefaultsCacheEntry
}{entries: make(map[string]dscDefaultsCacheEntry)}

type dscDefaultsCacheEntry struct {
	yaml string
	at   time.Time
}

func dscBranchForVersion(version string) (string, error) {
	m := dscVersionPattern.FindStringSubmatch(strings.TrimSpace(version))
	if m == nil {
		return "", fmt.Errorf("cannot determine DSC sample branch from operator version %q", version)
	}
	branch := "rhoai-" + m[1] + "." + m[2]
	if m[3] != "" && !strings.EqualFold(m[3], "ga") {
		branch += "-" + strings.ToLower(m[3])
		if m[4] != "" {
			branch += "." + m[4]
		}
	}
	return branch, nil
}

func installedOperatorVersion(c *Client) (string, error) {
	// Prefer the Subscription's installed CSV over old CSVs remaining during an upgrade.
	body, _, err := c.get(namespacedPath("operators.coreos.com/v1alpha1", "subscriptions", SubNS, SubName))
	if err == nil {
		var sub struct {
			Status struct {
				InstalledCSV string `json:"installedCSV"`
			} `json:"status"`
		}
		if err := json.Unmarshal(body, &sub); err != nil {
			return "", err
		}
		if sub.Status.InstalledCSV != "" {
			body, _, err = c.get(namespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", SubNS, sub.Status.InstalledCSV))
			if err != nil {
				return "", fmt.Errorf("reading installed CSV: %w", err)
			}
			var csv struct {
				Spec struct {
					Version string `json:"version"`
				} `json:"spec"`
			}
			if err := json.Unmarshal(body, &csv); err != nil {
				return "", err
			}
			if csv.Spec.Version == "" {
				return "", fmt.Errorf("installed CSV has no operator version")
			}
			return csv.Spec.Version, nil
		}
	} else if !IsK8sError(err, http.StatusNotFound) {
		return "", err
	}
	csv, err := getCSV(c)
	if err != nil {
		return "", err
	}
	if csv.Version == "" {
		return "", fmt.Errorf("install the RHOAI operator before fetching DSC defaults")
	}
	return csv.Version, nil
}

func fetchDefaultDSCSpec(c *Client) (*dscDefaults, error) {
	version, err := installedOperatorVersion(c)
	if err != nil {
		return nil, err
	}
	branch, err := dscBranchForVersion(version)
	// An explicit ref can accommodate future naming conventions without a code change.
	if override := strings.TrimSpace(os.Getenv("DSC_SAMPLE_REF")); override != "" {
		branch = override
	} else if err != nil {
		return nil, err
	}
	// Older releases may only ship the v1 sample. Only fall back within the same branch.
	for _, apiVersion := range []string{"v2", "v1"} {
		sourceURL := dscSamplesBaseURL + url.PathEscape(branch) + "/config/rhoai/samples/datasciencecluster_" + apiVersion + "_datasciencecluster.yaml"
		body, status, err := fetchDSCSample(c.ctx, sourceURL)
		if status == http.StatusNotFound && apiVersion == "v2" {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("fetching DSC defaults for %s (%s): %w", version, sourceURL, err)
		}
		var spec map[string]interface{}
		if err := yaml.Unmarshal([]byte(body), &spec); err != nil {
			return nil, fmt.Errorf("invalid DSC sample: %w", err)
		}
		meta, _ := spec["metadata"].(map[string]interface{})
		s, _ := spec["spec"].(map[string]interface{})
		components, _ := s["components"].(map[string]interface{})
		if spec["kind"] != "DataScienceCluster" || spec["apiVersion"] != "datasciencecluster.opendatahub.io/"+apiVersion || meta["name"] != "default-dsc" || len(components) == 0 {
			return nil, fmt.Errorf("upstream sample is not a valid default DataScienceCluster")
		}
		return &dscDefaults{Spec: spec, YAML: body, Version: version, Branch: branch, SourceURL: sourceURL}, nil
	}
	return nil, fmt.Errorf("no DSC sample found for %s", branch)
}

func fetchDSCSample(ctx context.Context, sourceURL string) (string, int, error) {
	dscDefaultsCache.RLock()
	entry, ok := dscDefaultsCache.entries[sourceURL]
	dscDefaultsCache.RUnlock()
	if ok && time.Since(entry.at) < time.Hour {
		return entry.yaml, http.StatusOK, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return "", 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", resp.StatusCode, fmt.Errorf("GitHub returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, (1<<16)+1))
	if err != nil {
		return "", resp.StatusCode, err
	}
	if len(body) > 1<<16 {
		return "", resp.StatusCode, fmt.Errorf("DSC sample is too large")
	}
	// Do not cache malformed responses.
	var sample map[string]interface{}
	if err := yaml.Unmarshal(body, &sample); err != nil {
		return "", resp.StatusCode, err
	}
	spec, _ := sample["spec"].(map[string]interface{})
	components, _ := spec["components"].(map[string]interface{})
	meta, _ := sample["metadata"].(map[string]interface{})
	expectedAPI := "datasciencecluster.opendatahub.io/v2"
	if strings.Contains(sourceURL, "datasciencecluster_v1_") {
		expectedAPI = "datasciencecluster.opendatahub.io/v1"
	}
	if sample["kind"] != "DataScienceCluster" || sample["apiVersion"] != expectedAPI || meta["name"] != "default-dsc" || len(components) == 0 {
		return "", resp.StatusCode, fmt.Errorf("invalid DSC sample")
	}
	dscDefaultsCache.Lock()
	dscDefaultsCache.entries[sourceURL] = dscDefaultsCacheEntry{yaml: string(body), at: time.Now()}
	dscDefaultsCache.Unlock()
	return string(body), resp.StatusCode, nil
}

func GetDefaultDSCYAML(c *Client) (*dscDefaults, error) {
	return fetchDefaultDSCSpec(c)
}
