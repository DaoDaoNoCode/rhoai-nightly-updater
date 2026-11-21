package operator

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/catalog"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// ReadCatalogServes reads the head of the nightly catalog's channel (the
// Subscription's) and whether the installed CSV is that build. It returns nil
// when the Subscription has no channel or the catalog does not list it; read
// errors are logged, not returned, because the rest of the status stands.
func ReadCatalogServes(c *kube.Client, sub types.SubscriptionInfo, cs types.CatalogSourceInfo, csv types.CSVInfo) *types.CatalogServes {
	if sub.Channel == "" || sub.Source != rhoai.CatalogName || !cs.Exists {
		return nil
	}
	channels, err := catalog.Channels(c, rhoai.CatalogName)
	if err != nil {
		slog.Warn("catalog head: reading the PackageManifest failed", "error", err)
		return nil
	}
	entry := catalog.FindChannel(channels, sub.Channel)
	if entry == nil {
		return nil
	}
	head, _ := entry["currentCSV"].(string)
	if head == "" {
		return nil
	}
	desc, _ := entry["currentCSVDesc"].(map[string]any)
	ann, _ := desc["annotations"].(map[string]any)
	s := &types.CatalogServes{Channel: sub.Channel, CSV: head}
	s.OperatorImage, _ = ann["containerImage"].(string)
	s.Built, _ = ann["createdAt"].(string)
	// Right after Argo CD changed the catalog image, the PackageManifest
	// still lists the old build until OLM has replaced the registry pod; the
	// comparison below would then wrongly match the installed operator.
	if cs.Image != "" {
		if ready, stale, ok := ReadCatalogPods(c, cs.Image); ok && (!ready || stale > 0) {
			s.Updating = true
			return s
		}
	}
	if csv.Name == "" || csv.Phase == "Not Found" {
		return s
	}
	if csv.Name != head {
		s.Installed = new(bool)
		return s
	}
	meta, found, err := kube.ReadObjectMeta(c, rhoai.NamespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", rhoai.SubNS, csv.Name))
	if err != nil {
		slog.Warn("catalog head: reading the installed CSV failed", "csv", csv.Name, "error", err)
		return s
	}
	headPrint := catalog.BuildFingerprint(s.OperatorImage, s.Built)
	installedPrint := catalog.BuildFingerprint(meta.Annotations["containerImage"], meta.Annotations["createdAt"])
	if found && headPrint != "" && installedPrint != "" {
		installed := headPrint == installedPrint
		s.Installed = &installed
	}
	return s
}

// CatalogImageChangedError: the CatalogSource image changed while the tool
// waited for the catalog to serve the build, which happens when Argo CD syncs
// a new image from Git, or an ACM policy enforces one, meanwhile. by is
// operatorGitOps.catalogChanger.
type CatalogImageChangedError struct{ By, Image string }

func (e *CatalogImageChangedError) Error() string {
	return fmt.Sprintf("the CatalogSource image was changed to %s while Update waited, most likely by %s. "+
		"Run Update again to install the build the catalog serves now", e.Image, e.By)
}

// CatalogServing is what the nightly catalog serves, as one poll reads it.
type CatalogServing struct {
	Image       string // spec.image
	State       string // connectionState.lastObservedState
	PodReady    bool   // a registry pod runs image and is Ready
	StalePods   int    // registry pods that run another image and are not terminating
	Head        string // currentCSV of the target channel
	Fingerprint string // headFingerprint of that channel
}

// ServesTarget reports whether the catalog serves the verified build.
func (s CatalogServing) ServesTarget(image string, target catalog.Target) bool {
	return s.Image == image && s.State == "READY" && s.PodReady && s.StalePods == 0 &&
		s.Head == target.HeadCSV && (target.Fingerprint == "" || s.Fingerprint == target.Fingerprint)
}

func (s CatalogServing) describe(target catalog.Target) string {
	switch {
	case !s.PodReady || s.StalePods > 0:
		return "OLM is replacing the catalog pod"
	case s.State != "READY":
		return "catalog state " + kube.NonEmpty(s.State, "unknown")
	case s.Head != target.HeadCSV:
		return fmt.Sprintf("channel %s lists %s, waiting for %s", target.Channel, kube.NonEmpty(s.Head, "nothing"), target.HeadCSV)
	default:
		return "waiting for the catalog to list the new build"
	}
}

// ReadCatalogPods reads the nightly catalog's registry pods: whether one
// runs image and is Ready, and how many run another image and are not
// terminating. ok is false when the list could not be read.
func ReadCatalogPods(c *kube.Client, image string) (ready bool, stale int, ok bool) {
	path := rhoai.NamespacedPath("v1", "pods", rhoai.CatalogNS, "") + "?labelSelector=" + url.QueryEscape("olm.catalogSource="+rhoai.CatalogName)
	body, _, err := c.Get(path)
	if err != nil {
		return false, 0, false
	}
	{
		var pods struct {
			Items []struct {
				Metadata struct {
					DeletionTimestamp string `json:"deletionTimestamp"`
				} `json:"metadata"`
				Spec struct {
					InitContainers []struct {
						Image string `json:"image"`
					} `json:"initContainers"`
					Containers []struct {
						Image string `json:"image"`
					} `json:"containers"`
				} `json:"spec"`
				Status struct {
					Conditions []struct {
						Type   string `json:"type"`
						Status string `json:"status"`
					} `json:"conditions"`
				} `json:"status"`
			} `json:"items"`
		}
		if json.Unmarshal(body, &pods) != nil {
			return false, 0, false
		}
		for _, p := range pods.Items {
			if p.Metadata.DeletionTimestamp != "" {
				continue
			}
			// The catalog image is the main container, or the second
			// init container when OLM extracts its content.
			serves := false
			for _, ctr := range append(p.Spec.InitContainers, p.Spec.Containers...) {
				serves = serves || ctr.Image == image
			}
			if !serves {
				stale++
				continue
			}
			for _, cond := range p.Status.Conditions {
				if cond.Type == "Ready" && cond.Status == "True" {
					ready = true
				}
			}
		}
	}
	return ready, stale, true
}

// readCatalogServing reads the CatalogSource, its registry pods and its
// PackageManifest entry for target's channel. Read errors leave fields
// empty; the caller keeps polling.
func readCatalogServing(c *kube.Client, image string, target catalog.Target) CatalogServing {
	var s CatalogServing
	if cs, err := rhoai.GetCatalogSource(c); err == nil {
		s.Image, s.State = cs.Image, cs.State
	}
	s.PodReady, s.StalePods, _ = ReadCatalogPods(c, image)
	if channels, err := catalog.Channels(c, rhoai.CatalogName); err == nil {
		if entry := catalog.FindChannel(channels, target.Channel); entry != nil {
			s.Head, _ = entry["currentCSV"].(string)
			s.Fingerprint = catalog.HeadFingerprint(entry)
		}
	}
	return s
}

// waitForCatalogServing waits until the CatalogSource serves the verified
// build: its registry pod runs image and is Ready, the catalog is READY, and
// its PackageManifest lists target's head with the same fingerprint as the
// verification catalog. A catalog whose image Argo CD changed in place
// reports READY and its old channels until OLM has replaced the pod, so READY
// alone proves nothing. It returns false on timeout; the error is the context
// error, a *catalogImagePullError, or an *CatalogImageChangedError (by
// changedBy) when the image changed meanwhile. pullAdvice words the pull
// error (pullFailureWatch.advice).
func waitForCatalogServing(c *kube.Client, image string, target catalog.Target, changedBy string, emit func(types.UpdateStepEvent), logs *[]string, pullAdvice func(*kube.Client) string) (bool, error) {
	deadline := kube.Clock().Now().Add(catalog.ReadyTimeout + catalog.PackageManifestPropagationWait)
	pull := catalog.PullFailureWatch{Advice: pullAdvice}
	last := ""
	for kube.Clock().Now().Before(deadline) {
		if err := kube.Clock().Sleep(c.Context(), catalog.PollInterval); err != nil {
			return false, err
		}
		s := readCatalogServing(c, image, target)
		if s.Image != "" && s.Image != image {
			return false, &CatalogImageChangedError{By: changedBy, Image: s.Image}
		}
		if s.ServesTarget(image, target) {
			return true, nil
		}
		if msg := s.describe(target); msg != last {
			last = msg
			*logs = append(*logs, "  "+msg)
			emit(types.UpdateStepEvent{Step: "wait_catalog_ready", Status: "running", Message: "CatalogSource: " + msg})
		}
		if err := pull.Check(c, rhoai.CatalogName, image); err != nil {
			return false, err
		}
	}
	return false, nil
}
