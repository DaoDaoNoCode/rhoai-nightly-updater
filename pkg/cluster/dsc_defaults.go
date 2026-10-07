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
	Spec       map[string]interface{}
	YAML       string
	Version    string // operator version
	APIVersion string // DataScienceCluster API version of Spec, e.g. "v3"
	Branch     string
	SourceURL  string // GitHub sample URL; empty when Source is "csv"
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

// installedOperator is the installed RHOAI CSV as needed for DSC defaults
// and diagnostics.
type installedOperator struct {
	Name        string
	Version     string
	Phase       string
	ALMExamples string // metadata.annotations["alm-examples"], may be empty
	// Deployments are the operator Deployments the CSV installs (in SubNS).
	Deployments []string
}

// csvForDefaults is the part of a CSV the DSC defaults read.
type csvForDefaults struct {
	Metadata struct {
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
	Spec struct {
		Version string `json:"version"`
		Install struct {
			Spec struct {
				Deployments []struct {
					Name string `json:"name"`
				} `json:"deployments"`
			} `json:"spec"`
		} `json:"install"`
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
	op := &installedOperator{Name: name, Version: csv.Spec.Version, Phase: csv.Status.Phase, ALMExamples: csv.Metadata.Annotations["alm-examples"]}
	for _, d := range csv.Spec.Install.Spec.Deployments {
		if d.Name != "" {
			op.Deployments = append(op.Deployments, d.Name)
		}
	}
	return op, nil
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
		op.Deployments = full.Deployments
	}
	return op, nil
}

// dscExample is a usable DataScienceCluster example of alm-examples.
type dscExample struct {
	Spec map[string]interface{}
	YAML string
}

// dscExamplesFromALM returns the DataScienceCluster examples that the
// installed operator bundle ships in its CSV's alm-examples annotation, by
// API version ("v3"). They are the exact samples of the installed build
// (the GitHub branch can move ahead of it) and need no network access.
// Examples not named default-dsc or without components are left out.
func dscExamplesFromALM(almExamples string) map[string]dscExample {
	out := map[string]dscExample{}
	if strings.TrimSpace(almExamples) == "" {
		return out
	}
	var examples []map[string]interface{}
	if err := json.Unmarshal([]byte(almExamples), &examples); err != nil {
		return out
	}
	for _, example := range examples {
		if example["kind"] != "DataScienceCluster" {
			continue
		}
		apiVersion, _ := example["apiVersion"].(string)
		version, ok := dscAPIVersion(apiVersion)
		if !ok || !isDefaultDSC(example, version) {
			continue
		}
		if _, dup := out[version]; dup {
			continue
		}
		meta, _ := example["metadata"].(map[string]interface{})
		// Keep only what a user would write; never carry status or server fields.
		cleanMeta := map[string]interface{}{"name": "default-dsc"}
		if labels, ok := meta["labels"].(map[string]interface{}); ok && len(labels) > 0 {
			cleanMeta["labels"] = labels
		}
		clean := map[string]interface{}{
			"apiVersion": apiVersion,
			"kind":       "DataScienceCluster",
			"metadata":   cleanMeta,
			"spec":       example["spec"],
		}
		text, err := yaml.Marshal(clean)
		if err != nil {
			continue
		}
		// Return a map decoded from the YAML so callers get their own copy.
		var fresh map[string]interface{}
		if err := yaml.Unmarshal(text, &fresh); err != nil {
			continue
		}
		out[version] = dscExample{Spec: fresh, YAML: string(text)}
	}
	return out
}

// dscAPIVersion returns the version of a DataScienceCluster apiVersion
// ("datasciencecluster.opendatahub.io/v3" gives "v3").
func dscAPIVersion(apiVersion string) (string, bool) {
	group, version, ok := strings.Cut(apiVersion, "/")
	if !ok || group != dscGroup || !kubeVersionPattern.MatchString(version) {
		return "", false
	}
	return version, true
}

// isDefaultDSC reports whether a parsed sample is the default
// DataScienceCluster (default-dsc, with components) of the given version.
func isDefaultDSC(sample map[string]interface{}, version string) bool {
	meta, _ := sample["metadata"].(map[string]interface{})
	spec, _ := sample["spec"].(map[string]interface{})
	components, _ := spec["components"].(map[string]interface{})
	return sample["kind"] == "DataScienceCluster" && sample["apiVersion"] == dscGroup+"/"+version &&
		meta["name"] == "default-dsc" && len(components) > 0
}

// missingDefaultsError: the installed operator has no defaults in the API
// version the DataScienceCluster was read at. Defaults of another version
// are never used instead: component names differ between versions.
type missingDefaultsError struct {
	Want            string   // "v2"
	Available       []string // the versions alm-examples has defaults for
	OperatorVersion string
	CSV             string // "" when alm-examples were not read (DSC_SAMPLE_REF)
	Branch          string
}

func (e *missingDefaultsError) Error() string {
	var where []string
	if e.CSV != "" {
		where = append(where, "the alm-examples of "+e.CSV)
	}
	where = append(where, "branch "+e.Branch+" of rhods-operator")
	msg := fmt.Sprintf("operator %s has no DataScienceCluster %s defaults (none in %s)", e.OperatorVersion, e.Want, strings.Join(where, " or "))
	if len(e.Available) > 0 {
		msg += fmt.Sprintf("; it ships them as %s only", strings.Join(e.Available, ", "))
	}
	return msg
}

// fetchDefaultDSCSpec returns the installed operator's defaults in API
// version want ("v3"), or with want "" in the newest served version that
// has them.
func fetchDefaultDSCSpec(c *Client, want string) (*dscDefaults, error) {
	op, err := getInstalledOperator(c)
	if err != nil {
		return nil, err
	}
	return defaultDSCSpecFor(c.ctx, op, servedVersions(c, dscGroup, dscFallbackVersions), want)
}

// defaultsOrder is the API versions to look for defaults in: want alone,
// else the served versions in order. When those are only the fallback
// (discovery failed), every version alm-examples has is added and all are
// tried newest first.
func defaultsOrder(versions apiVersions, examples map[string]dscExample, want string) []string {
	if want != "" {
		return []string{want}
	}
	order := append([]string{}, versions.Versions...)
	if versions.Discovered {
		return order
	}
	for v := range examples {
		if !containsString(order, v) {
			order = append(order, v)
		}
	}
	sortAPIVersions(order)
	return order
}

// defaultDSCSpecFor prefers the installed CSV's alm-examples and falls back
// to the sample on the matching rhods-operator branch, in the API versions
// of defaultsOrder. DSC_SAMPLE_REF forces the GitHub sample from that ref.
func defaultDSCSpecFor(ctx context.Context, op *installedOperator, versions apiVersions, want string) (*dscDefaults, error) {
	version := op.Version
	branch, err := dscBranchForVersion(version)
	override := strings.TrimSpace(os.Getenv("DSC_SAMPLE_REF"))
	examples := map[string]dscExample{}
	if override == "" {
		examples = dscExamplesFromALM(op.ALMExamples)
	}
	order := defaultsOrder(versions, examples, want)
	for _, v := range order {
		if ex, ok := examples[v]; ok {
			return &dscDefaults{Spec: ex.Spec, YAML: ex.YAML, Version: version, APIVersion: v, Branch: branch, Source: "csv",
				SourceDescription: "alm-examples of the installed CSV " + op.Name}, nil
		}
	}
	// An explicit ref can accommodate future naming conventions without a code change.
	if override != "" {
		branch = override
	} else if err != nil {
		return nil, err
	}
	// Only fall back within the same branch: a missing sample (404) moves
	// on to the next version, any other failure stops.
	for _, apiVersion := range order {
		sourceURL := dscSamplesBaseURL + url.PathEscape(branch) + "/config/rhoai/samples/datasciencecluster_" + apiVersion + "_datasciencecluster.yaml"
		body, status, err := fetchDSCSample(ctx, sourceURL, apiVersion)
		if status == http.StatusNotFound {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("fetching DSC defaults for %s (%s): %w", version, sourceURL, err)
		}
		var spec map[string]interface{}
		if err := yaml.Unmarshal([]byte(body), &spec); err != nil {
			return nil, fmt.Errorf("invalid DSC sample: %w", err)
		}
		if !isDefaultDSC(spec, apiVersion) {
			return nil, fmt.Errorf("upstream sample is not a valid default DataScienceCluster")
		}
		return &dscDefaults{Spec: spec, YAML: body, Version: version, APIVersion: apiVersion, Branch: branch, SourceURL: sourceURL, Source: "github", SourceDescription: sourceURL}, nil
	}
	if want != "" {
		e := &missingDefaultsError{Want: want, OperatorVersion: version, Branch: branch}
		if override == "" {
			e.CSV = op.Name
		}
		for v := range examples {
			e.Available = append(e.Available, v)
		}
		sortAPIVersions(e.Available)
		return nil, e
	}
	return nil, fmt.Errorf("no DSC sample found for %s (DataScienceCluster %s)", branch, strings.Join(order, ", "))
}

// fetchDSCSample downloads a GitHub sample of DataScienceCluster apiVersion
// version. Only valid samples are cached.
func fetchDSCSample(ctx context.Context, sourceURL, version string) (string, int, error) {
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
	if !isDefaultDSC(sample, version) {
		return "", resp.StatusCode, fmt.Errorf("invalid DSC sample")
	}
	dscDefaultsCache.Lock()
	dscDefaultsCache.entries[sourceURL] = dscDefaultsCacheEntry{yaml: string(body), at: time.Now()}
	dscDefaultsCache.Unlock()
	return string(body), resp.StatusCode, nil
}

// GetDefaultDSCYAML returns the defaults a "create" or "reset to defaults"
// would use: in the API version the existing DataScienceCluster is read
// at, or, without one, in the newest served version that has them.
func GetDefaultDSCYAML(c *Client) (*dscDefaults, error) {
	read, err := readFirstDSC(c)
	if err != nil {
		return nil, err
	}
	want := ""
	if read.Object != nil {
		want = read.Version
	}
	return fetchDefaultDSCSpec(c, want)
}
