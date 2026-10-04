package cluster

import (
	"context"
	"encoding/json"
	"fmt"
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

// A fresh temporary CatalogSource verifies the supplied image (including old
// pinned builds) before uninstall. Its unique identity avoids stale package
// metadata from the previous nightly build. No unrelated tag is resolved.
func preflightReinstallCatalog(c *Client, image, override string) (string, error) {
	name := strings.TrimRight(CatalogName, "-")
	if len(name) > 35 {
		name = name[:35]
	}
	name += "-verify-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	path := namespacedPath("operators.coreos.com/v1alpha1", "catalogsources", CatalogNS, name)
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = c.WithContext(ctx).delete(path)
	}()
	spec := buildCatalogSourceSpec(image)
	spec["metadata"].(map[string]interface{})["name"] = name
	if _, _, err := c.apply(path, spec); err != nil {
		return "", fmt.Errorf("create verification catalog: %w", err)
	}
	deadline := time.Now().Add(CatalogReadyTimeout + PackageManifestPropagationWait)
	lastProblem := "the catalog has not reported its state yet"
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
			} else if channels, channelErr := catalogChannels(c, name); channelErr != nil {
				lastProblem = channelErr.Error()
			} else if len(channels) == 0 {
				lastProblem = "the catalog does not list the " + SubName + " package yet"
			} else if override != "" {
				for _, entry := range channels {
					ch, _ := entry.(map[string]interface{})
					if ch["name"] == override {
						return override, nil
					}
				}
				lastProblem = fmt.Sprintf("channel %q is not in the catalog (available: %s)", override, strings.Join(channelNames(channels), ", "))
			} else if ch, err := detectBestChannel(channels, image); err != nil {
				lastProblem = err.Error()
			} else if ch != "" {
				return ch, nil
			} else {
				lastProblem = fmt.Sprintf("no channel matches the image's release (available: %s)", strings.Join(channelNames(channels), ", "))
			}
		}
		select {
		case <-c.ctx.Done():
			return "", c.ctx.Err()
		case <-time.After(CatalogPollInterval):
		}
	}
	return "", fmt.Errorf("selected image did not publish a ready RHOAI catalog with a matching channel: %s", lastProblem)
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
