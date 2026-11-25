package rhoai

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
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
	DSCGroup      = "datasciencecluster.opendatahub.io"
	DSCIGroup     = "dscinitialization.opendatahub.io"
	PlatformGroup = "config.opendatahub.io"
	// The collections, %s for the version.
	DSCListFmt      = "/apis/datasciencecluster.opendatahub.io/%s/datascienceclusters"
	PlatformListFmt = "/apis/config.opendatahub.io/%s/platforms"
)

// The versions read before discovery, used when discovery fails or the
// group is not served yet, so older clusters behave as before.
var (
	DSCFallbackVersions      = []string{"v2", "v1"}
	DSCIFallbackVersions     = []string{"v2", "v1"}
	PlatformFallbackVersions = []string{"v1alpha1"}
)

// APIVersions is the order in which a group's versions are tried.
type APIVersions struct {
	Versions []string // preferred first, then newest first
	// Discovered is false when Versions is the fallback (discovery failed
	// or the group is not served).
	Discovered bool
}

// apiVersionsCacheTTL bounds how long a served-version list may lag a CRD
// change (an operator upgrade adds versions). Only discovered lists are
// cached, so a group that appears is picked up on the next read.
const apiVersionsCacheTTL = 30 * time.Second

var APIVersionsCache = kube.NewLRU[string, []string](64)

// ServedVersions reads the API discovery document of group (readable by
// every authenticated user through system:discovery) and returns its
// served versions, the preferred one first and the others newest first.
//
// The cache holds only what the ServiceAccount's client read: a client of
// the signed-in user's token (a change's, or a user's read) reads
// discovery afresh and caches nothing, so a per-cluster cache is never
// fed by a user's token.
func ServedVersions(c *kube.Client, group string, fallback []string) APIVersions {
	key := c.BaseURL() + "|" + group
	if v, ok := APIVersionsCache.Get(key); ok && !c.ActsAsUser() {
		return APIVersions{Versions: v, Discovered: true}
	}
	body, _, err := c.Get("/apis/" + group)
	if err != nil {
		return APIVersions{Versions: fallback}
	}
	versions := ParseGroupVersions(body)
	if len(versions) == 0 {
		return APIVersions{Versions: fallback}
	}
	if !c.ActsAsUser() {
		APIVersionsCache.Add(key, versions, apiVersionsCacheTTL)
	}
	return APIVersions{Versions: versions, Discovered: true}
}

// APIGroupDoc is an APIGroup discovery document (also the items of an
// APIGroupList).
type APIGroupDoc struct {
	Name     string `json:"name"`
	Versions []struct {
		Version string `json:"version"`
	} `json:"versions"`
	PreferredVersion struct {
		Version string `json:"version"`
	} `json:"preferredVersion"`
}

// OrderedVersions returns the group's versions, preferred first, then the
// others newest first.
func (g APIGroupDoc) OrderedVersions() []string {
	var rest []string
	for _, v := range g.Versions {
		if v.Version != "" && v.Version != g.PreferredVersion.Version && !slices.Contains(rest, v.Version) {
			rest = append(rest, v.Version)
		}
	}
	SortAPIVersions(rest)
	if g.PreferredVersion.Version == "" {
		return rest
	}
	return append([]string{g.PreferredVersion.Version}, rest...)
}

func ParseGroupVersions(body []byte) []string {
	var g APIGroupDoc
	if json.Unmarshal(body, &g) != nil {
		return nil
	}
	return g.OrderedVersions()
}

var KubeVersionPattern = regexp.MustCompile(`^v([1-9][0-9]*)(?:(alpha|beta)([1-9][0-9]*))?$`)

// CompareAPIVersions orders API version names the way the API server
// orders a group's versions (k8s.io/apimachinery/pkg/version
// CompareKubeAwareVersionStrings): GA before beta before alpha, then the
// higher major, then the higher minor; names that are not Kubernetes
// versions come last, alphabetically. It returns a negative number when a
// comes first (is newer).
func CompareAPIVersions(a, b string) int {
	ma, mb := KubeVersionPattern.FindStringSubmatch(a), KubeVersionPattern.FindStringSubmatch(b)
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

func SortAPIVersions(versions []string) {
	sort.SliceStable(versions, func(i, j int) bool { return CompareAPIVersions(versions[i], versions[j]) < 0 })
}

// IsConversionWebhookError reports whether err is the API server failing to
// convert stored objects through a CRD's conversion webhook: HTTP 500
// "conversion webhook for <kind> failed: ..." on a direct read, or HTTP 429
// "storage is (re)initializing: ... conversion webhook ..." while the watch
// cache cannot list them (it never initializes, so retrying does not help).
func IsConversionWebhookError(err error) bool {
	var k *kube.K8sError
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

// ListServed lists a collection at each served version of group in turn.
// pathFmt is the collection path with %s for the version. A version that
// is not served (404) or whose objects cannot be converted is skipped; any
// other error is returned. With no version served the result has Version
// "" and no error. When every served version fails to convert, the first
// conversion error is returned.
func ListServed(c *kube.Client, group, pathFmt string, query url.Values, fallback []string) (servedRead, error) {
	return readServed(c, group, pathFmt, false, query, fallback)
}

// GetServed reads one object like listServed; pathFmt includes its name. A
// 404 at a discovered served version means the object does not exist and
// is returned.
func GetServed(c *kube.Client, group, pathFmt string, fallback []string) (servedRead, error) {
	return readServed(c, group, pathFmt, true, nil, fallback)
}

func readServed(c *kube.Client, group, pathFmt string, named bool, query url.Values, fallback []string) (servedRead, error) {
	// A conversion failure is answered with 429 and Retry-After 30 but does
	// not clear by itself: read the next version at once (NoRetryWhen).
	c = c.NoRetryWhen(IsConversionWebhookError)
	var out servedRead
	var conversionErr, notFound error
	versions := ServedVersions(c, group, fallback)
	for _, version := range versions.Versions {
		body, _, err := c.Do(http.MethodGet, fmt.Sprintf(pathFmt, version), "", nil, query)
		switch {
		case kube.IsK8sNotFound(err) && named && versions.Discovered:
			return out, err // the version is served, so the object does not exist
		case kube.IsK8sNotFound(err):
			notFound = err // the version is not served
			continue
		case IsConversionWebhookError(err):
			out.Skipped = append(out.Skipped, versionFailure{Version: version, Message: ConversionMessage(err)})
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

func ConversionMessage(err error) string {
	var k *kube.K8sError
	if errors.As(err, &k) && k.K8sMessage != "" {
		return k.K8sMessage
	}
	return err.Error()
}

// ListFirstServed lists a collection of group at the newest served version
// that works (listServed) and returns the items. pathFmt is the collection
// path with %s for the version. apiFound is false when no version is
// served (no CRD).
func ListFirstServed(c *kube.Client, group, pathFmt, resource string, fallback []string) (items []DSCObject, apiFound bool, err error) {
	r, err := ListServed(c, group, pathFmt, nil, fallback)
	if err != nil {
		return nil, true, err
	}
	if r.Version == "" {
		return nil, false, nil
	}
	var list struct {
		Items []DSCObject `json:"items"`
	}
	if err := json.Unmarshal(r.Body, &list); err != nil {
		return nil, true, fmt.Errorf("parse %s: %w", resource, err)
	}
	return list.Items, true, nil
}

type DSCObject struct {
	APIVersion string `json:"apiVersion"`
	Metadata   struct {
		Name       string `json:"name"`
		UID        string `json:"uid"`
		Generation int64  `json:"generation"`
	} `json:"metadata"`
	Spec struct {
		ApplicationsNamespace string `json:"applicationsNamespace"` // DSCInitialization
		Monitoring            struct {
			Namespace string `json:"namespace"` // DSCInitialization
		} `json:"monitoring"`
	} `json:"spec"`
	Status struct {
		Phase              string         `json:"phase"`
		ObservedGeneration int64          `json:"observedGeneration"`
		Conditions         []DSCCondition `json:"conditions"`
	} `json:"status"`
}

type DSCCondition struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	Reason             string `json:"reason"`
	Message            string `json:"message"`
	Severity           string `json:"severity"`
	LastTransitionTime string `json:"lastTransitionTime"`
}
