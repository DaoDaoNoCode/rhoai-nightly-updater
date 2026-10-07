package cluster

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// API versions of the RHOAI CRDs are discovered, not assumed. RHOAI 3.6
// introduced DataScienceCluster v3 and Platform v1alpha2 (opendatahub-operator
// PR #4137, "Introduce DataScienceCluster v3 for the RHOAI 3.6 component
// reorganization"), with other component names than v2. A CRD can serve a
// version that the operator's conversion webhook cannot convert to: a
// nightly whose operator image is older than the CRDs its bundle installed
// answers every read of the new version with "conversion webhook for ...
// failed: no kind ... is registered for version ..." (seen live on
// 2026-10-07). Reads then fall back to the next served version and report
// that they did.

const (
	dscGroup      = "datasciencecluster.opendatahub.io"
	dsciGroup     = "dscinitialization.opendatahub.io"
	platformGroup = "config.opendatahub.io"
	// The collections, %s for the version.
	dscListFmt      = "/apis/datasciencecluster.opendatahub.io/%s/datascienceclusters"
	platformListFmt = "/apis/config.opendatahub.io/%s/platforms"
)

// The versions read before discovery, used when discovery fails or the
// group is not served yet, so older clusters behave as before.
var (
	dscFallbackVersions      = []string{"v2", "v1"}
	dsciFallbackVersions     = []string{"v2", "v1"}
	platformFallbackVersions = []string{"v1alpha1"}
)

// apiVersions is the order in which a group's versions are tried.
type apiVersions struct {
	Versions []string // preferred first, then newest first
	// Discovered is false when Versions is the fallback (discovery failed
	// or the group is not served).
	Discovered bool
}

// apiVersionsCacheTTL bounds how long a served-version list may lag a CRD
// change (an operator upgrade adds versions). Only discovered lists are
// cached, so a group that appears is picked up on the next read.
const apiVersionsCacheTTL = 30 * time.Second

var apiVersionsCache = newLRU[string, []string](64)

// servedVersions reads the API discovery document of group (readable by
// every authenticated user through system:discovery) and returns its
// served versions, the preferred one first and the others newest first.
func servedVersions(c *Client, group string, fallback []string) apiVersions {
	key := c.baseURL + "|" + group
	if v, ok := apiVersionsCache.Get(key); ok {
		return apiVersions{Versions: v, Discovered: true}
	}
	body, _, err := c.get("/apis/" + group)
	if err != nil {
		return apiVersions{Versions: fallback}
	}
	versions := parseGroupVersions(body)
	if len(versions) == 0 {
		return apiVersions{Versions: fallback}
	}
	apiVersionsCache.Add(key, versions, apiVersionsCacheTTL)
	return apiVersions{Versions: versions, Discovered: true}
}

// apiGroupDoc is an APIGroup discovery document (also the items of an
// APIGroupList).
type apiGroupDoc struct {
	Name     string `json:"name"`
	Versions []struct {
		Version string `json:"version"`
	} `json:"versions"`
	PreferredVersion struct {
		Version string `json:"version"`
	} `json:"preferredVersion"`
}

// orderedVersions returns the group's versions, preferred first, then the
// others newest first.
func (g apiGroupDoc) orderedVersions() []string {
	var rest []string
	for _, v := range g.Versions {
		if v.Version != "" && v.Version != g.PreferredVersion.Version && !containsString(rest, v.Version) {
			rest = append(rest, v.Version)
		}
	}
	sortAPIVersions(rest)
	if g.PreferredVersion.Version == "" {
		return rest
	}
	return append([]string{g.PreferredVersion.Version}, rest...)
}

func parseGroupVersions(body []byte) []string {
	var g apiGroupDoc
	if json.Unmarshal(body, &g) != nil {
		return nil
	}
	return g.orderedVersions()
}

var kubeVersionPattern = regexp.MustCompile(`^v([1-9][0-9]*)(?:(alpha|beta)([1-9][0-9]*))?$`)

// compareAPIVersions orders API version names the way the API server
// orders a group's versions (k8s.io/apimachinery/pkg/version
// CompareKubeAwareVersionStrings): GA before beta before alpha, then the
// higher major, then the higher minor; names that are not Kubernetes
// versions come last, alphabetically. It returns a negative number when a
// comes first (is newer).
func compareAPIVersions(a, b string) int {
	ma, mb := kubeVersionPattern.FindStringSubmatch(a), kubeVersionPattern.FindStringSubmatch(b)
	switch {
	case ma == nil && mb == nil:
		return strings.Compare(a, b)
	case ma == nil:
		return 1
	case mb == nil:
		return -1
	}
	level := map[string]int{"": 0, "beta": 1, "alpha": 2}
	if d := level[ma[2]] - level[mb[2]]; d != 0 {
		return d
	}
	if d := atoiOr0(mb[1]) - atoiOr0(ma[1]); d != 0 {
		return d
	}
	return atoiOr0(mb[3]) - atoiOr0(ma[3])
}

func atoiOr0(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

func sortAPIVersions(versions []string) {
	sort.SliceStable(versions, func(i, j int) bool { return compareAPIVersions(versions[i], versions[j]) < 0 })
}

// isConversionWebhookError reports whether err is the API server failing to
// convert stored objects through a CRD's conversion webhook: HTTP 500
// "conversion webhook for <kind> failed: ..." on a direct read, or HTTP 429
// "storage is (re)initializing: ... conversion webhook ..." while the watch
// cache cannot list them (it never initializes, so retrying does not help).
func isConversionWebhookError(err error) bool {
	var k *K8sError
	if !errors.As(err, &k) || (k.Status != http.StatusInternalServerError && k.Status != http.StatusTooManyRequests) {
		return false
	}
	return strings.Contains(strings.ToLower(k.K8sMessage), "conversion webhook")
}

// versionFailure is a served version that a read skipped because the API
// server could not convert the objects to it.
type versionFailure struct {
	Version string
	Message string // the API server's message
}

// servedRead is a read at the first served version that worked.
type servedRead struct {
	Body    []byte
	Version string // "" when no version is served (no CRD)
	// Skipped are the versions tried first that failed with a conversion
	// webhook error, in the order tried.
	Skipped []versionFailure
}

// listServed lists a collection at each served version of group in turn.
// pathFmt is the collection path with %s for the version. A version that
// is not served (404) or whose objects cannot be converted is skipped; any
// other error is returned. With no version served the result has Version
// "" and no error. When every served version fails to convert, the first
// conversion error is returned.
func listServed(c *Client, group, pathFmt string, query url.Values, fallback []string) (servedRead, error) {
	return readServed(c, group, pathFmt, false, query, fallback)
}

// getServed reads one object like listServed; pathFmt includes its name. A
// 404 at a discovered served version means the object does not exist and
// is returned.
func getServed(c *Client, group, pathFmt string, fallback []string) (servedRead, error) {
	return readServed(c, group, pathFmt, true, nil, fallback)
}

func readServed(c *Client, group, pathFmt string, named bool, query url.Values, fallback []string) (servedRead, error) {
	var out servedRead
	var conversionErr, notFound error
	versions := servedVersions(c, group, fallback)
	for _, version := range versions.Versions {
		body, _, err := c.do(http.MethodGet, fmt.Sprintf(pathFmt, version), "", nil, query)
		switch {
		case IsK8sError(err, http.StatusNotFound) && named && versions.Discovered:
			return out, err // the version is served, so the object does not exist
		case IsK8sError(err, http.StatusNotFound):
			notFound = err // the version is not served
			continue
		case isConversionWebhookError(err):
			out.Skipped = append(out.Skipped, versionFailure{Version: version, Message: conversionMessage(err)})
			if conversionErr == nil {
				conversionErr = err
			}
			continue
		case err != nil:
			return out, err
		}
		out.Body, out.Version = body, version
		return out, nil
	}
	if conversionErr != nil {
		return out, conversionErr
	}
	if named && notFound != nil {
		return out, notFound
	}
	return out, nil
}

func conversionMessage(err error) string {
	var k *K8sError
	if errors.As(err, &k) && k.K8sMessage != "" {
		return k.K8sMessage
	}
	return err.Error()
}
