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
	SourceURL string // GitHub sample URL; empty when Source is "csv"
	// Source is "csv" (alm-examples of the installed CSV) or "github".
	Source            string
	SourceDescription string
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

// installedOperator is the installed RHOAI CSV as needed for DSC defaults.
type installedOperator struct {
	Name        string
	Version     string
	Phase       string
	ALMExamples string // metadata.annotations["alm-examples"], may be empty
}

// csvForDefaults is the part of a CSV the DSC defaults read.
type csvForDefaults struct {
	Metadata struct {
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
	Spec struct {
		Version string `json:"version"`
	} `json:"spec"`
	Status struct {
		Phase string `json:"phase"`
	} `json:"status"`
}

func readCSVForDefaults(c *Client, name string) (*installedOperator, error) {
	body, _, err := c.get(namespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", SubNS, name))
	if err != nil {
		return nil, err
	}
	var csv csvForDefaults
	if err := json.Unmarshal(body, &csv); err != nil {
		return nil, err
	}
	return &installedOperator{Name: name, Version: csv.Spec.Version, Phase: csv.Status.Phase, ALMExamples: csv.Metadata.Annotations["alm-examples"]}, nil
}

// getInstalledOperator reads the installed RHOAI CSV. The Subscription's
// installedCSV is preferred over old CSVs remaining during an upgrade.
func getInstalledOperator(c *Client) (*installedOperator, error) {
	body, _, err := c.get(namespacedPath("operators.coreos.com/v1alpha1", "subscriptions", SubNS, SubName))
	if err == nil {
		var sub struct {
			Status struct {
				InstalledCSV string `json:"installedCSV"`
			} `json:"status"`
		}
		if err := json.Unmarshal(body, &sub); err != nil {
			return nil, err
		}
		if sub.Status.InstalledCSV != "" {
			op, err := readCSVForDefaults(c, sub.Status.InstalledCSV)
			if err != nil {
				return nil, fmt.Errorf("reading installed CSV: %w", err)
			}
			if op.Version == "" {
				return nil, fmt.Errorf("installed CSV has no operator version")
			}
			return op, nil
		}
	} else if !IsK8sError(err, http.StatusNotFound) {
		return nil, err
	}
	csv, err := getCSV(c)
	if err != nil {
		return nil, err
	}
	if csv.Version == "" {
		return nil, fmt.Errorf("install the RHOAI operator before fetching DSC defaults")
	}
	op := &installedOperator{Name: csv.Name, Version: csv.Version, Phase: csv.Phase}
	// Read the CSV itself for alm-examples; without it the GitHub sample is used.
	if full, err := readCSVForDefaults(c, csv.Name); err == nil && full.Version == csv.Version {
		op.ALMExamples = full.ALMExamples
	}
	return op, nil
}

func installedOperatorVersion(c *Client) (string, error) {
	op, err := getInstalledOperator(c)
	if err != nil {
		return "", err
	}
	return op.Version, nil
}

// dscFromALMExamples returns the DataScienceCluster example that the
// installed operator bundle ships in its CSV's alm-examples annotation. It is
// the exact sample of the installed build (the GitHub branch can move ahead
// of it) and needs no network access. ok is false when the CSV carries no
// usable DataScienceCluster example.
func dscFromALMExamples(almExamples string) (map[string]interface{}, string, bool) {
	if strings.TrimSpace(almExamples) == "" {
		return nil, "", false
	}
	var examples []map[string]interface{}
	if err := json.Unmarshal([]byte(almExamples), &examples); err != nil {
		return nil, "", false
	}
	for _, example := range examples {
		if example["kind"] != "DataScienceCluster" {
			continue
		}
		apiVersion, _ := example["apiVersion"].(string)
		meta, _ := example["metadata"].(map[string]interface{})
		spec, _ := example["spec"].(map[string]interface{})
		components, _ := spec["components"].(map[string]interface{})
		if (apiVersion != "datasciencecluster.opendatahub.io/v2" && apiVersion != "datasciencecluster.opendatahub.io/v1") ||
			meta["name"] != "default-dsc" || len(components) == 0 {
			continue
		}
		// Keep only what a user would write; never carry status or server fields.
		cleanMeta := map[string]interface{}{"name": "default-dsc"}
		if labels, ok := meta["labels"].(map[string]interface{}); ok && len(labels) > 0 {
			cleanMeta["labels"] = labels
		}
		clean := map[string]interface{}{
			"apiVersion": apiVersion,
			"kind":       "DataScienceCluster",
			"metadata":   cleanMeta,
			"spec":       spec,
		}
		out, err := yaml.Marshal(clean)
		if err != nil {
			return nil, "", false
		}
		// Return a map decoded from the YAML so callers get their own copy.
		var fresh map[string]interface{}
		if err := yaml.Unmarshal(out, &fresh); err != nil {
			return nil, "", false
		}
		return fresh, string(out), true
	}
	return nil, "", false
}

func fetchDefaultDSCSpec(c *Client) (*dscDefaults, error) {
	op, err := getInstalledOperator(c)
	if err != nil {
		return nil, err
	}
	return defaultDSCSpecFor(c.ctx, op)
}

// defaultDSCSpecFor prefers the installed CSV's alm-examples and falls back
// to the sample on the matching rhods-operator branch. DSC_SAMPLE_REF forces
// the GitHub sample from that ref.
func defaultDSCSpecFor(ctx context.Context, op *installedOperator) (*dscDefaults, error) {
	version := op.Version
	branch, err := dscBranchForVersion(version)
	override := strings.TrimSpace(os.Getenv("DSC_SAMPLE_REF"))
	if override == "" {
		if spec, text, ok := dscFromALMExamples(op.ALMExamples); ok {
			return &dscDefaults{Spec: spec, YAML: text, Version: version, Branch: branch, Source: "csv",
				SourceDescription: "alm-examples of the installed CSV " + op.Name}, nil
		}
	}
	// An explicit ref can accommodate future naming conventions without a code change.
	if override != "" {
		branch = override
	} else if err != nil {
		return nil, err
	}
	// Older releases may only ship the v1 sample. Only fall back within the same branch.
	for _, apiVersion := range []string{"v2", "v1"} {
		sourceURL := dscSamplesBaseURL + url.PathEscape(branch) + "/config/rhoai/samples/datasciencecluster_" + apiVersion + "_datasciencecluster.yaml"
		body, status, err := fetchDSCSample(ctx, sourceURL)
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
		return &dscDefaults{Spec: spec, YAML: body, Version: version, Branch: branch, SourceURL: sourceURL, Source: "github", SourceDescription: sourceURL}, nil
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
