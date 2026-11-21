package dashdev

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
)

// DashboardURL returns the RHOAI dashboard's address for links, as
// "https://host" (any path kept, no trailing slash), or "" when it is
// unknown.
//
// Current releases serve the dashboard behind the data-science gateway
// (https://rh-ai.<apps domain>/), and the old Route rhods-dashboard in the
// applications namespace, now owned by the GatewayConfig,
// only serves an "OpenShift AI has a new address" page. The Dashboard
// component CR (components.platform.opendatahub.io, kind Dashboard) reports
// the address in status.url, which the operator's ConsoleLink uses too. So
// the CR comes first, at whichever API version is served and whatever its
// name; releases from before the gateway have no status.url (or no CR), and
// then the Route's host is used. No release is named: the CR either reports
// a URL or it does not. Only an https URL is used.
func DashboardURL(c *kube.Client) string {
	if u := dashboardURLFromComponent(c); u != "" {
		return u
	}
	ns, err := rhoai.AppsNamespace(c)
	if err != nil {
		return "" // the Route's namespace is unknown, and so is the address
	}
	if host := rhoai.RouteHost(c, ns, "rhods-dashboard"); host != "" && kube.SnippetURL("https://"+host) {
		return "https://" + host
	}
	return ""
}

// dashboardURLFromComponent returns the first usable status.url of the
// Dashboard component CRs, or "" (no CRD, no CR, no URL, or the list is
// refused).
func dashboardURLFromComponent(c *kube.Client) string {
	u, _ := readComponentURL(c)
	return u
}

// readComponentURL is dashboardURLFromComponent with the read
// error: "" and nil mean the cluster has no such URL (no CRD served, no CR,
// or none reports an https status.url); an error means it could not be
// read.
func readComponentURL(c *kube.Client) (string, error) {
	r, err := rhoai.ListServed(c, rhoai.ComponentAPIGroup, "/apis/components.platform.opendatahub.io/%s/dashboards", nil, []string{"v1alpha1"})
	if err != nil || r.Version == "" {
		return "", err
	}
	var list struct {
		Items []struct {
			Status struct {
				URL string `json:"url"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(r.Body, &list); err != nil {
		return "", fmt.Errorf("parse dashboards: %w", err)
	}
	for _, item := range list.Items {
		if u := httpsBaseURL(item.Status.URL); u != "" {
			return u, nil
		}
	}
	return "", nil
}

// httpsBaseURL returns raw without its trailing slash when it is an
// absolute https URL with a host, else "".
func httpsBaseURL(raw string) string {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return ""
	}
	// It goes into the Playground tab's command too: a plain https
	// address only (kube.SnippetURL).
	if s := strings.TrimSuffix(raw, "/"); kube.SnippetURL(s) {
		return s
	}
	return ""
}
