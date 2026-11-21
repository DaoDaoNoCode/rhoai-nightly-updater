package cluster

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// readCatalogServes reads the head of the nightly catalog's channel (the
// Subscription's) and whether the installed CSV is that build. It returns nil
// when the Subscription has no channel or the catalog does not list it; read
// errors are logged, not returned, because the rest of the status stands.
func readCatalogServes(c *Client, sub types.SubscriptionInfo, cs types.CatalogSourceInfo, csv types.CSVInfo) *types.CatalogServes {
	if sub.Channel == "" || sub.Source != rhoai.CatalogName || !cs.Exists {
		return nil
	}
	channels, err := catalogChannels(c, rhoai.CatalogName)
	if err != nil {
		slog.Warn("catalog head: reading the PackageManifest failed", "error", err)
		return nil
	}
	entry := findChannel(channels, sub.Channel)
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
		if ready, stale, ok := readCatalogPods(c, cs.Image); ok && (!ready || stale > 0) {
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
	headPrint := buildFingerprint(s.OperatorImage, s.Built)
	installedPrint := buildFingerprint(meta.Annotations["containerImage"], meta.Annotations["createdAt"])
	if found && headPrint != "" && installedPrint != "" {
		installed := headPrint == installedPrint
		s.Installed = &installed
	}
	return s
}

// errCatalogImageChanged: the CatalogSource image changed while the tool
// waited for the catalog to serve the build, which happens when Argo CD syncs
// a new image from Git, or an ACM policy enforces one, meanwhile. by is
// operatorGitOps.catalogChanger.
type errCatalogImageChanged struct{ by, image string }

func (e *errCatalogImageChanged) Error() string {
	return fmt.Sprintf("the CatalogSource image was changed to %s while Update waited, most likely by %s. "+
		"Run Update again to install the build the catalog serves now", e.image, e.by)
}

// catalogServing is what the nightly catalog serves, as one poll reads it.
type catalogServing struct {
	image       string // spec.image
	state       string // connectionState.lastObservedState
	podReady    bool   // a registry pod runs image and is Ready
	stalePods   int    // registry pods that run another image and are not terminating
	head        string // currentCSV of the target channel
	fingerprint string // headFingerprint of that channel
}

// servesTarget reports whether the catalog serves the verified build.
func (s catalogServing) servesTarget(image string, target catalogTarget) bool {
	return s.image == image && s.state == "READY" && s.podReady && s.stalePods == 0 &&
		s.head == target.HeadCSV && (target.Fingerprint == "" || s.fingerprint == target.Fingerprint)
}

func (s catalogServing) describe(target catalogTarget) string {
	switch {
	case !s.podReady || s.stalePods > 0:
		return "OLM is replacing the catalog pod"
	case s.state != "READY":
		return "catalog state " + kube.NonEmpty(s.state, "unknown")
	case s.head != target.HeadCSV:
		return fmt.Sprintf("channel %s lists %s, waiting for %s", target.Channel, kube.NonEmpty(s.head, "nothing"), target.HeadCSV)
	default:
		return "waiting for the catalog to list the new build"
	}
}

// readCatalogPods reads the nightly catalog's registry pods: whether one
// runs image and is Ready, and how many run another image and are not
// terminating. ok is false when the list could not be read.
func readCatalogPods(c *Client, image string) (ready bool, stale int, ok bool) {
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
func readCatalogServing(c *Client, image string, target catalogTarget) catalogServing {
	var s catalogServing
	if cs, err := rhoai.GetCatalogSource(c); err == nil {
		s.image, s.state = cs.Image, cs.State
	}
	s.podReady, s.stalePods, _ = readCatalogPods(c, image)
	if channels, err := catalogChannels(c, rhoai.CatalogName); err == nil {
		if entry := findChannel(channels, target.Channel); entry != nil {
			s.head, _ = entry["currentCSV"].(string)
			s.fingerprint = headFingerprint(entry)
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
// error, a *catalogImagePullError, or an *errCatalogImageChanged (by
// changedBy) when the image changed meanwhile. pullAdvice words the pull
// error (pullFailureWatch.advice).
func waitForCatalogServing(c *Client, image string, target catalogTarget, changedBy string, emit func(UpdateStepEvent), logs *[]string, pullAdvice func(*Client) string) (bool, error) {
	deadline := kube.Clock().Now().Add(CatalogReadyTimeout + PackageManifestPropagationWait)
	pull := pullFailureWatch{advice: pullAdvice}
	last := ""
	for kube.Clock().Now().Before(deadline) {
		if err := kube.Clock().Sleep(c.Context(), CatalogPollInterval); err != nil {
			return false, err
		}
		s := readCatalogServing(c, image, target)
		if s.image != "" && s.image != image {
			return false, &errCatalogImageChanged{by: changedBy, image: s.image}
		}
		if s.servesTarget(image, target) {
			return true, nil
		}
		if msg := s.describe(target); msg != last {
			last = msg
			*logs = append(*logs, "  "+msg)
			emit(UpdateStepEvent{Step: "wait_catalog_ready", Status: "running", Message: "CatalogSource: " + msg})
		}
		if err := pull.check(c, rhoai.CatalogName, image); err != nil {
			return false, err
		}
	}
	return false, nil
}
