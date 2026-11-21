package catalog

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
)

// Always scope and verify catalog identity: the stable and nightly catalogs
// both publish rhods-operator, so a by-name lookup is ambiguous.
func Channels(c *kube.Client, source string) ([]any, error) {
	path := rhoai.NamespacedPath("packages.operators.coreos.com/v1", "packagemanifests", rhoai.CatalogNS, "") +
		"?labelSelector=" + url.QueryEscape("catalog="+source) + "&fieldSelector=" + url.QueryEscape("metadata.name="+rhoai.SubName)
	var items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Status struct {
			PackageName string `json:"packageName"`
			Source      string `json:"catalogSource"`
			Namespace   string `json:"catalogSourceNamespace"`
			Channels    []any  `json:"channels"`
		} `json:"status"`
	}
	if err := c.ListInto(path, &items); err != nil {
		if kube.IsDecodeError(err) {
			return nil, fmt.Errorf("parse catalog packages: %w", err)
		}
		return nil, fmt.Errorf("catalog %s package lookup: %w", source, err)
	}
	for _, item := range items {
		if item.Metadata.Name == rhoai.SubName && item.Status.PackageName == rhoai.SubName && item.Status.Source == source && item.Status.Namespace == rhoai.CatalogNS {
			return item.Status.Channels, nil
		}
	}
	return nil, nil
}

// Labels on the temporary verification catalogs, so leftovers from a crashed
// or killed operation can be found and removed later.
const (
	ManagedByLabel   = "app.kubernetes.io/managed-by"
	ManagedByValue   = "rhoai-nightly-updater"
	PurposeLabel     = "rhoai-nightly-updater/purpose"
	VerificationRole = "catalog-verification"
)

var (
	// StaleVerificationAge is how old a verification catalog must be
	// before another operation removes it. A preflight takes at most
	// ReadyTimeout+PackageManifestPropagationWait (2.5 minutes).
	StaleVerificationAge = 15 * time.Minute
	// ImagePullGrace is how long a catalog pod may keep failing to
	// pull its image before the operation stops. Kubelet retries pulls with
	// a back-off, so a single ErrImagePull right after a pull secret change
	// is not fatal.
	ImagePullGrace = 30 * time.Second
)

// Target is what the verified catalog would install: the channel and
// that channel's head CSV, which OLM installs for a new Subscription without
// startingCSV.
type Target struct {
	Channel string
	HeadCSV string
	// OwnedCRDs maps each CRD the head CSV owns to the versions it lists
	// (PackageManifest currentCSVDesc.customresourcedefinitions.owned);
	// nil when the catalog does not say.
	OwnedCRDs map[string][]string
	// Fingerprint identifies the head bundle's build (headFingerprint); ""
	// when the catalog does not say.
	Fingerprint string
}

// HeadFingerprint identifies the build of a PackageManifest channel's head
// CSV: its operator image and creation time (currentCSVDesc annotations
// containerImage and createdAt). Two nightly builds of the same version have
// the same CSV name but differ here. "" when neither annotation is set.
func HeadFingerprint(entry map[string]any) string {
	desc, _ := entry["currentCSVDesc"].(map[string]any)
	ann, _ := desc["annotations"].(map[string]any)
	image, _ := ann["containerImage"].(string)
	created, _ := ann["createdAt"].(string)
	return BuildFingerprint(image, created)
}

// BuildFingerprint is headFingerprint for a CSV's containerImage and
// createdAt annotations; "" when neither is set.
func BuildFingerprint(image, created string) string {
	if image == "" && created == "" {
		return ""
	}
	return image + " " + created
}

// OwnedCRDVersions reads currentCSVDesc.customresourcedefinitions.owned of
// a PackageManifest channel entry.
func OwnedCRDVersions(entry map[string]any) map[string][]string {
	desc, _ := entry["currentCSVDesc"].(map[string]any)
	crds, _ := desc["customresourcedefinitions"].(map[string]any)
	owned, _ := crds["owned"].([]any)
	if len(owned) == 0 {
		return nil
	}
	out := map[string][]string{}
	for _, o := range owned {
		m, _ := o.(map[string]any)
		name, _ := m["name"].(string)
		version, _ := m["version"].(string)
		if name != "" && version != "" && !slices.Contains(out[name], version) {
			out[name] = append(out[name], version)
		}
	}
	return out
}

func VerificationPrefix() string {
	name := strings.TrimRight(rhoai.CatalogName, "-")
	if len(name) > 35 {
		name = name[:35]
	}
	return name + "-verify-"
}

// A fresh temporary CatalogSource verifies the supplied image (including old
// pinned builds) before uninstall. Its unique identity avoids stale package
// metadata from the previous nightly build. No unrelated tag is resolved.
func PreflightReinstall(c *kube.Client, image, override string) (Target, error) {
	name, path, remove, err := CreateVerification(c, image)
	if err != nil {
		return Target{}, err
	}
	defer remove()
	deadline := kube.Clock().Now().Add(ReadyTimeout + PackageManifestPropagationWait)
	lastProblem := "the catalog has not reported its state yet"
	pull := PullFailureWatch{Advice: rhoai.NodePullAdvice}
	for kube.Clock().Now().Before(deadline) {
		state, err := VerificationState(c, path)
		if err != nil {
			lastProblem = err.Error()
		} else if state != "READY" {
			lastProblem = "catalog state is " + state
			if pullErr := pull.Check(c, name, image); pullErr != nil {
				return Target{}, pullErr
			}
		} else if channels, channelErr := Channels(c, name); channelErr != nil {
			lastProblem = channelErr.Error()
		} else if len(channels) == 0 {
			lastProblem = "the catalog does not list the " + rhoai.SubName + " package yet"
		} else if override != "" {
			if entry := FindChannel(channels, override); entry != nil {
				head, _ := entry["currentCSV"].(string)
				return Target{Channel: override, HeadCSV: head, OwnedCRDs: OwnedCRDVersions(entry), Fingerprint: HeadFingerprint(entry)}, nil
			}
			lastProblem = fmt.Sprintf("channel %q is not in the catalog (available: %s)", override, strings.Join(ChannelNames(channels), ", "))
		} else if ch, err := BestChannel(channels, image); err != nil {
			lastProblem = err.Error()
		} else if ch != "" {
			entry := FindChannel(channels, ch)
			head, _ := entry["currentCSV"].(string)
			return Target{Channel: ch, HeadCSV: head, OwnedCRDs: OwnedCRDVersions(entry), Fingerprint: HeadFingerprint(entry)}, nil
		} else {
			lastProblem = fmt.Sprintf("no channel matches the image's release (available: %s)", strings.Join(ChannelNames(channels), ", "))
		}
		if err := kube.Clock().Sleep(c.Context(), PollInterval); err != nil {
			return Target{}, err
		}
	}
	return Target{}, fmt.Errorf("selected image did not publish a ready RHOAI catalog with a matching channel: %s", lastProblem)
}

// CreateVerification creates a temporary CatalogSource for image,
// labelled so that a leftover is found and removed later, after removing
// stale ones. remove deletes it (also after the operation's context ended).
func CreateVerification(c *kube.Client, image string) (name, path string, remove func(), err error) {
	cleanupStaleVerificationCatalogs(c)
	name = VerificationPrefix() + strconv.FormatInt(time.Now().UnixNano(), 36)
	path = rhoai.NamespacedPath("operators.coreos.com/v1alpha1", "catalogsources", rhoai.CatalogNS, name)
	remove = func() {
		ctx, cancel := record.PostOperationContext(c, 30*time.Second)
		defer cancel()
		if _, err := c.WithContext(ctx).DeleteIfExists(path); err != nil {
			slog.Warn("failed to delete verification CatalogSource; the next operation removes it", "name", name, "error", err)
		}
	}
	spec := SourceSpec(image)
	meta := spec["metadata"].(map[string]any)
	meta["name"] = name
	meta["labels"] = map[string]any{ManagedByLabel: ManagedByValue, PurposeLabel: VerificationRole}
	if _, _, err := c.Apply(path, spec); err != nil {
		// The apply may have created it before failing.
		remove()
		return "", "", nil, fmt.Errorf("create verification catalog: %w", err)
	}
	return name, path, remove, nil
}

// VerificationState reads a verification catalog's
// status.connectionState.lastObservedState ("unknown" while it has none).
func VerificationState(c *kube.Client, path string) (string, error) {
	var cs struct {
		Status struct {
			Connection struct {
				State string `json:"lastObservedState"`
			} `json:"connectionState"`
		} `json:"status"`
	}
	if err := c.GetInto(path, &cs); err != nil {
		if kube.IsDecodeError(err) {
			return "", fmt.Errorf("parse verification catalog: %w", err)
		}
		return "", fmt.Errorf("read verification catalog: %w", err)
	}
	if cs.Status.Connection.State == "" {
		return "unknown", nil
	}
	return cs.Status.Connection.State, nil
}

// FindChannel returns the PackageManifest channel entry with the given name.
func FindChannel(channels []any, name string) map[string]any {
	for _, entry := range channels {
		ch, _ := entry.(map[string]any)
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
func cleanupStaleVerificationCatalogs(c *kube.Client) {
	var items []struct {
		Metadata struct {
			Name              string            `json:"name"`
			Labels            map[string]string `json:"labels"`
			CreationTimestamp string            `json:"creationTimestamp"`
		} `json:"metadata"`
	}
	if err := c.ListInto(rhoai.NamespacedPath("operators.coreos.com/v1alpha1", "catalogsources", rhoai.CatalogNS, ""), &items); err != nil {
		if kube.IsDecodeError(err) {
			slog.Warn("cannot parse CatalogSources to remove stale verification catalogs", "error", err)
			return
		}
		slog.Warn("cannot list CatalogSources to remove stale verification catalogs", "error", err)
		return
	}
	for _, item := range items {
		m := item.Metadata
		if m.Labels[PurposeLabel] != VerificationRole && !strings.HasPrefix(m.Name, VerificationPrefix()) {
			continue
		}
		created, err := time.Parse(time.RFC3339, m.CreationTimestamp)
		if err != nil || time.Since(created) < StaleVerificationAge {
			continue
		}
		path := rhoai.NamespacedPath("operators.coreos.com/v1alpha1", "catalogsources", rhoai.CatalogNS, m.Name)
		if _, err := c.DeleteIfExists(path); err != nil {
			slog.Warn("failed to delete stale verification CatalogSource", "name", m.Name, "error", err)
			continue
		}
		slog.Info("deleted stale verification CatalogSource", "name", m.Name, "created", m.CreationTimestamp)
	}
}

// ImagePullError reports that a catalog pod cannot pull its image.
type ImagePullError struct {
	image  string
	detail string
	// advice says which pull secret to check (pullFailureWatch.advice).
	advice string
}

func (e *ImagePullError) Error() string {
	advice := e.advice
	if advice == "" {
		advice = "Check that the nodes' pull secret has access to quay.io/rhoai and that the image exists"
	}
	return fmt.Sprintf("the catalog image %s cannot be pulled (%s). %s", e.image, e.detail, advice)
}

func IsImagePullError(err error) bool {
	var pullErr *ImagePullError
	return errors.As(err, &pullErr)
}

// PullFailureWatch tracks how long a catalog pod has been failing to pull.
type PullFailureWatch struct {
	Since time.Time
	// advice, when set, words what the error tells the user to check:
	// nodePullAdvice in the full profile (it depends on the control plane),
	// gitOpsProfilePullAdvice in the gitops profile. A function value, so
	// the gitops profile, which passes its own, never reaches the control
	// plane read (deploy/rbac-gitops.yaml does not grant it).
	Advice func(*kube.Client) string
}

// Check reads the pod OLM runs for the CatalogSource (label
// olm.catalogSource=<name>) and returns an error once its image pull has
// failed for ImagePullGrace, or at once for an invalid image name.
func (w *PullFailureWatch) Check(c *kube.Client, source, image string) error {
	reason, detail := catalogPodPullFailure(c, source)
	if reason == "" {
		w.Since = time.Time{}
		return nil
	}
	if w.Since.IsZero() {
		w.Since = kube.Clock().Now()
	}
	if reason == "InvalidImageName" || kube.Clock().Since(w.Since) >= ImagePullGrace {
		err := &ImagePullError{image: image, detail: detail}
		if w.Advice != nil {
			err.advice = w.Advice(c)
		}
		return err
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
func catalogPodPullFailure(c *kube.Client, source string) (string, string) {
	path := rhoai.NamespacedPath("v1", "pods", rhoai.CatalogNS, "") + "?labelSelector=" + url.QueryEscape("olm.catalogSource="+source)
	var items []struct {
		Status struct {
			InitContainerStatuses []containerWaitStatus `json:"initContainerStatuses"`
			ContainerStatuses     []containerWaitStatus `json:"containerStatuses"`
		} `json:"status"`
	}
	if c.ListInto(path, &items) != nil {
		return "", ""
	}
	for _, pod := range items {
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

// ChannelNames lists the channel names of a PackageManifest, sorted.
func ChannelNames(channels []any) []string {
	var names []string
	for _, entry := range channels {
		ch, _ := entry.(map[string]any)
		if name, _ := ch["name"].(string); name != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}
