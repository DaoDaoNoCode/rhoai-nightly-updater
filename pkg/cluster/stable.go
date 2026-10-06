package cluster

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
)

type stableTarget struct {
	Source  string
	Channel string
	Version string
	// HeadCSV is the channel head's CSV name (what OLM installs).
	HeadCSV string
	// OwnedCRDs: see catalogTarget.OwnedCRDs.
	OwnedCRDs map[string][]string
	Pinned    bool
}

type stablePackageChannel struct {
	Name           string `json:"name"`
	CurrentCSV     string `json:"currentCSV"`
	CurrentCSVDesc struct {
		Version                   string `json:"version"`
		CustomResourceDefinitions struct {
			Owned []struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"owned"`
		} `json:"customresourcedefinitions"`
	} `json:"currentCSVDesc"`
}

// ownedCRDs returns the CRD versions the channel head lists, or nil.
func (ch stablePackageChannel) ownedCRDs() map[string][]string {
	if len(ch.CurrentCSVDesc.CustomResourceDefinitions.Owned) == 0 {
		return nil
	}
	out := map[string][]string{}
	for _, o := range ch.CurrentCSVDesc.CustomResourceDefinitions.Owned {
		if o.Name != "" && o.Version != "" && !containsString(out[o.Name], o.Version) {
			out[o.Name] = append(out[o.Name], o.Version)
		}
	}
	return out
}

func productionChannel(name string) bool {
	for _, family := range []string{"stable", "fast", "eus"} {
		if name == family || strings.HasPrefix(name, family+"-") {
			return true
		}
	}
	return false
}

func stableChannelVersion(channel stablePackageChannel) (parsedTag, string, bool) {
	version := channel.CurrentCSVDesc.Version
	if version == "" {
		version = strings.TrimPrefix(channel.CurrentCSV, SubName+".")
	}
	version = strings.TrimPrefix(version, "v")
	parsed, ok := parseTag("rhoai-" + strings.SplitN(version, "+", 2)[0])
	// Only GA heads qualify. EA, RC, beta, and other prereleases are excluded.
	return parsed, version, ok && parsed.ea == -1 && channel.CurrentCSV != ""
}

// resolveStableTarget reads the same catalog metadata used by OperatorHub.
// Never query a PackageManifest by name alone: several catalogs can publish
// rhods-operator, and a nightly catalog must not determine the stable target.
func resolveStableTarget(c *Client) (stableTarget, error) {
	target := stableTarget{Source: getStableSource()}
	path := namespacedPath("packages.operators.coreos.com/v1", "packagemanifests", CatalogNS, "") +
		"?labelSelector=" + url.QueryEscape("catalog="+target.Source) +
		"&fieldSelector=" + url.QueryEscape("metadata.name="+SubName)
	body, _, err := c.get(path)
	if err != nil {
		return target, fmt.Errorf("read RHOAI releases from catalog %q: %w", target.Source, err)
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Status struct {
				PackageName            string                 `json:"packageName"`
				CatalogSource          string                 `json:"catalogSource"`
				CatalogSourceNamespace string                 `json:"catalogSourceNamespace"`
				DefaultChannel         string                 `json:"defaultChannel"`
				Channels               []stablePackageChannel `json:"channels"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return target, fmt.Errorf("parse RHOAI catalog releases: %w", err)
	}
	override := strings.TrimSpace(os.Getenv("STABLE_CHANNEL"))
	for _, item := range list.Items {
		if item.Metadata.Name != SubName || item.Status.PackageName != SubName ||
			item.Status.CatalogSource != target.Source || item.Status.CatalogSourceNamespace != CatalogNS {
			continue
		}
		var best parsedTag
		bestPreference := -1
		for _, channel := range item.Status.Channels {
			version, displayVersion, ga := stableChannelVersion(channel)
			if !ga {
				continue
			}
			if override != "" {
				if channel.Name == override {
					target.Channel, target.Version, target.HeadCSV, target.Pinned = channel.Name, displayVersion, channel.CurrentCSV, true
					target.OwnedCRDs = channel.ownedCRDs()
					return target, nil
				}
				continue
			}
			if !productionChannel(channel.Name) {
				continue
			}
			// Prefer the catalog default on equal versions, then a stable channel.
			preference := 0
			if channel.Name == "stable" || strings.HasPrefix(channel.Name, "stable-") {
				preference = 1
			}
			if channel.Name == item.Status.DefaultChannel {
				preference = 2
			}
			comparison := compareTags(version, best)
			if target.Channel == "" || comparison > 0 || (comparison == 0 &&
				(preference > bestPreference || (preference == bestPreference && channel.Name < target.Channel))) {
				target.Channel, target.Version, target.HeadCSV = channel.Name, displayVersion, channel.CurrentCSV
				target.OwnedCRDs = channel.ownedCRDs()
				best, bestPreference = version, preference
			}
		}
	}
	if target.Channel != "" {
		return target, nil
	}
	if override != "" {
		return target, fmt.Errorf("configured STABLE_CHANNEL %q is missing or has no GA release in catalog %q", override, target.Source)
	}
	return target, fmt.Errorf("no RHOAI GA release found in catalog %q; check that the catalog is ready and contains rhods-operator", target.Source)
}
