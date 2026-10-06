package cluster

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Always scope and verify catalog identity: the stable and nightly catalogs
// both publish rhods-operator, so a by-name lookup is ambiguous.
func catalogChannels(c *Client, source string) ([]interface{}, error) {
	path := namespacedPath("packages.operators.coreos.com/v1", "packagemanifests", CatalogNS, "") +
		"?labelSelector=" + url.QueryEscape("catalog="+source) + "&fieldSelector=" + url.QueryEscape("metadata.name="+SubName)
	body, _, err := c.get(path)
	if err != nil {
		return nil, fmt.Errorf("catalog %s package lookup: %w", source, err)
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Status struct {
				PackageName string        `json:"packageName"`
				Source      string        `json:"catalogSource"`
				Namespace   string        `json:"catalogSourceNamespace"`
				Channels    []interface{} `json:"channels"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("parse catalog packages: %w", err)
	}
	for _, item := range list.Items {
		if item.Metadata.Name == SubName && item.Status.PackageName == SubName && item.Status.Source == source && item.Status.Namespace == CatalogNS {
			return item.Status.Channels, nil
		}
	}
	return nil, nil
}

// Labels on the temporary verification catalogs, so leftovers from a crashed
// or killed operation can be found and removed later.
const (
	managedByLabel          = "app.kubernetes.io/managed-by"
	managedByValue          = "rhoai-nightly-updater"
	catalogPurposeLabel     = "rhoai-nightly-updater/purpose"
	verificationCatalogRole = "catalog-verification"
)

var (
	// StaleVerificationCatalogAge is how old a verification catalog must be
	// before another operation removes it. A preflight takes at most
	// CatalogReadyTimeout+PackageManifestPropagationWait (2.5 minutes).
	StaleVerificationCatalogAge = 15 * time.Minute
	// CatalogImagePullGrace is how long a catalog pod may keep failing to
	// pull its image before the operation stops. Kubelet retries pulls with
	// a back-off, so a single ErrImagePull right after a pull secret change
	// is not fatal.
	CatalogImagePullGrace = 30 * time.Second
)

// catalogTarget is what the verified catalog would install: the channel and
// that channel's head CSV, which OLM installs for a new Subscription without
// startingCSV.
type catalogTarget struct {
	Channel string
	HeadCSV string
	// OwnedCRDs maps each CRD the head CSV owns to the versions it lists
	// (PackageManifest currentCSVDesc.customresourcedefinitions.owned);
	// nil when the catalog does not say.
	OwnedCRDs map[string][]string
}

// ownedCRDVersions reads currentCSVDesc.customresourcedefinitions.owned of
// a PackageManifest channel entry.
func ownedCRDVersions(entry map[string]interface{}) map[string][]string {
	desc, _ := entry["currentCSVDesc"].(map[string]interface{})
	crds, _ := desc["customresourcedefinitions"].(map[string]interface{})
	owned, _ := crds["owned"].([]interface{})
	if len(owned) == 0 {
		return nil
	}
	out := map[string][]string{}
	for _, o := range owned {
		m, _ := o.(map[string]interface{})
		name, _ := m["name"].(string)
		version, _ := m["version"].(string)
		if name != "" && version != "" && !containsString(out[name], version) {
			out[name] = append(out[name], version)
		}
	}
	return out
}

func verificationCatalogPrefix() string {
	name := strings.TrimRight(CatalogName, "-")
	if len(name) > 35 {
		name = name[:35]
	}
	return name + "-verify-"
}

// A fresh temporary CatalogSource verifies the supplied image (including old
// pinned builds) before uninstall. Its unique identity avoids stale package
// metadata from the previous nightly build. No unrelated tag is resolved.
func preflightReinstallCatalog(c *Client, image, override string) (catalogTarget, error) {
	cleanupStaleVerificationCatalogs(c)
	name := verificationCatalogPrefix() + strconv.FormatInt(time.Now().UnixNano(), 36)
	path := namespacedPath("operators.coreos.com/v1alpha1", "catalogsources", CatalogNS, name)
	defer func() {
		ctx, cancel := postOperationContext(c, 30*time.Second)
		defer cancel()
		if _, err := c.WithContext(ctx).delete(path); err != nil && !IsK8sError(err, 404) {
			slog.Warn("failed to delete verification CatalogSource; the next operation removes it", "name", name, "error", err)
		}
	}()
	spec := buildCatalogSourceSpec(image)
	meta := spec["metadata"].(map[string]interface{})
	meta["name"] = name
	meta["labels"] = map[string]interface{}{managedByLabel: managedByValue, catalogPurposeLabel: verificationCatalogRole}
	if _, _, err := c.apply(path, spec); err != nil {
		return catalogTarget{}, fmt.Errorf("create verification catalog: %w", err)
	}
	deadline := time.Now().Add(CatalogReadyTimeout + PackageManifestPropagationWait)
	lastProblem := "the catalog has not reported its state yet"
	var pull pullFailureWatch
	for time.Now().Before(deadline) {
		body, _, err := c.get(path)
		if err != nil {
			lastProblem = "read verification catalog: " + err.Error()
		} else {
			var cs struct {
				Status struct {
					Connection struct {
						State string `json:"lastObservedState"`
					} `json:"connectionState"`
				} `json:"status"`
			}
			if err := json.Unmarshal(body, &cs); err != nil {
				lastProblem = "parse verification catalog: " + err.Error()
			} else if state := cs.Status.Connection.State; state != "READY" {
				if state == "" {
					state = "unknown"
				}
				lastProblem = "catalog state is " + state
				if pullErr := pull.check(c, name, image); pullErr != nil {
					return catalogTarget{}, pullErr
				}
			} else if channels, channelErr := catalogChannels(c, name); channelErr != nil {
				lastProblem = channelErr.Error()
			} else if len(channels) == 0 {
				lastProblem = "the catalog does not list the " + SubName + " package yet"
			} else if override != "" {
				if entry := findChannel(channels, override); entry != nil {
					head, _ := entry["currentCSV"].(string)
					return catalogTarget{Channel: override, HeadCSV: head, OwnedCRDs: ownedCRDVersions(entry)}, nil
				}
				lastProblem = fmt.Sprintf("channel %q is not in the catalog (available: %s)", override, strings.Join(channelNames(channels), ", "))
			} else if ch, err := detectBestChannel(channels, image); err != nil {
				lastProblem = err.Error()
			} else if ch != "" {
				entry := findChannel(channels, ch)
				head, _ := entry["currentCSV"].(string)
				return catalogTarget{Channel: ch, HeadCSV: head, OwnedCRDs: ownedCRDVersions(entry)}, nil
			} else {
				lastProblem = fmt.Sprintf("no channel matches the image's release (available: %s)", strings.Join(channelNames(channels), ", "))
			}
		}
		select {
		case <-c.ctx.Done():
			return catalogTarget{}, c.ctx.Err()
		case <-time.After(CatalogPollInterval):
		}
	}
	return catalogTarget{}, fmt.Errorf("selected image did not publish a ready RHOAI catalog with a matching channel: %s", lastProblem)
}

// findChannel returns the PackageManifest channel entry with the given name.
func findChannel(channels []interface{}, name string) map[string]interface{} {
	for _, entry := range channels {
		ch, _ := entry.(map[string]interface{})
		if ch["name"] == name {
			return ch
		}
	}
	return nil
}

// cleanupStaleVerificationCatalogs deletes verification catalogs left behind
// by an operation that was killed before its deferred cleanup ran. Unlabelled
// catalogs from older versions are recognised by their generated name. Young
// catalogs may belong to a running preflight and are kept.
func cleanupStaleVerificationCatalogs(c *Client) {
	body, _, err := c.get(namespacedPath("operators.coreos.com/v1alpha1", "catalogsources", CatalogNS, ""))
	if err != nil {
		slog.Warn("cannot list CatalogSources to remove stale verification catalogs", "error", err)
		return
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name              string            `json:"name"`
				Labels            map[string]string `json:"labels"`
				CreationTimestamp string            `json:"creationTimestamp"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		slog.Warn("cannot parse CatalogSources to remove stale verification catalogs", "error", err)
		return
	}
	for _, item := range list.Items {
		m := item.Metadata
		if m.Labels[catalogPurposeLabel] != verificationCatalogRole && !strings.HasPrefix(m.Name, verificationCatalogPrefix()) {
			continue
		}
		created, err := time.Parse(time.RFC3339, m.CreationTimestamp)
		if err != nil || time.Since(created) < StaleVerificationCatalogAge {
			continue
		}
		path := namespacedPath("operators.coreos.com/v1alpha1", "catalogsources", CatalogNS, m.Name)
		if _, err := c.delete(path); err != nil && !IsK8sError(err, 404) {
			slog.Warn("failed to delete stale verification CatalogSource", "name", m.Name, "error", err)
			continue
		}
		slog.Info("deleted stale verification CatalogSource", "name", m.Name, "created", m.CreationTimestamp)
	}
}

// catalogImagePullError reports that a catalog pod cannot pull its image.
type catalogImagePullError struct {
	image  string
	detail string
}

func (e *catalogImagePullError) Error() string {
	return fmt.Sprintf("the catalog image %s cannot be pulled (%s). Check that the pull secret has access to quay.io/rhoai (Setup step 1) and that the image exists; on a new cluster the credentials can take a few minutes to reach every node", e.image, e.detail)
}

func isCatalogImagePullError(err error) bool {
	var pullErr *catalogImagePullError
	return errors.As(err, &pullErr)
}

// pullFailureWatch tracks how long a catalog pod has been failing to pull.
type pullFailureWatch struct {
	since time.Time
}

// check reads the pod OLM runs for the CatalogSource (label
// olm.catalogSource=<name>) and returns an error once its image pull has
// failed for CatalogImagePullGrace, or at once for an invalid image name.
func (w *pullFailureWatch) check(c *Client, source, image string) error {
	reason, detail := catalogPodPullFailure(c, source)
	if reason == "" {
		w.since = time.Time{}
		return nil
	}
	if w.since.IsZero() {
		w.since = time.Now()
	}
	if reason == "InvalidImageName" || time.Since(w.since) >= CatalogImagePullGrace {
		return &catalogImagePullError{image: image, detail: detail}
	}
	return nil
}

var imagePullFailureReasons = map[string]bool{
	"ErrImagePull":      true,
	"ImagePullBackOff":  true,
	"InvalidImageName":  true,
	"ErrImageNeverPull": true,
}

// catalogPodPullFailure returns the waiting reason and kubelet message of a
// catalog pod container that cannot pull its image. Lookup errors are
// ignored: the caller keeps polling the CatalogSource state.
func catalogPodPullFailure(c *Client, source string) (string, string) {
	path := namespacedPath("v1", "pods", CatalogNS, "") + "?labelSelector=" + url.QueryEscape("olm.catalogSource="+source)
	body, _, err := c.get(path)
	if err != nil {
		return "", ""
	}
	var pods struct {
		Items []struct {
			Status struct {
				InitContainerStatuses []containerWaitStatus `json:"initContainerStatuses"`
				ContainerStatuses     []containerWaitStatus `json:"containerStatuses"`
			} `json:"status"`
		} `json:"items"`
	}
	if json.Unmarshal(body, &pods) != nil {
		return "", ""
	}
	for _, pod := range pods.Items {
		for _, cs := range append(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses...) {
			if waiting := cs.State.Waiting; waiting != nil && imagePullFailureReasons[waiting.Reason] {
				detail := waiting.Reason
				if waiting.Message != "" {
					detail += ": " + waiting.Message
				}
				return waiting.Reason, detail
			}
		}
	}
	return "", ""
}

type containerWaitStatus struct {
	State struct {
		Waiting *struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"waiting"`
	} `json:"state"`
}

// channelNames lists the channel names of a PackageManifest, sorted.
func channelNames(channels []interface{}) []string {
	var names []string
	for _, entry := range channels {
		ch, _ := entry.(map[string]interface{})
		if name, _ := ch["name"].(string); name != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// storedVersionConflicts compares the CRD versions the target bundle lists
// with the live CRDs' status.storedVersions. OLM fails an InstallPlan whose
// CRD drops a version still listed in storedVersions ("risk of data loss",
// operator-lifecycle-manager lib/crd/storage.go; RHOAI operator notes §1.4),
// which a downgrade can hit (live: DSC and DSCI store v2, which 2.x bundles
// lack). Only the operator's own CRDs (OLM package label) are read; CRDs
// the target does not list are skipped.
func storedVersionConflicts(c *Client, owned map[string][]string) ([]string, error) {
	body, _, err := c.do(http.MethodGet, "/apis/apiextensions.k8s.io/v1/customresourcedefinitions", "", nil, url.Values{"labelSelector": {rhoaiCRDSelectors[0]}})
	if err != nil {
		return nil, fmt.Errorf("list the operator's CRDs: %w", err)
	}
	var list struct {
		Items []conversionCRD `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("parse CRDs: %w", err)
	}
	var out []string
	for _, crd := range list.Items {
		versions, listed := owned[crd.Metadata.Name]
		if !listed {
			continue
		}
		for _, stored := range crd.Status.StoredVersions {
			if !containsString(versions, stored) {
				out = append(out, fmt.Sprintf("CRD %s stores objects as %s, which the target lists only as %s", crd.Metadata.Name, stored, strings.Join(versions, ", ")))
			}
		}
	}
	sort.Strings(out)
	return out, nil
}
