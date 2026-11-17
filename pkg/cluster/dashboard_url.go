package cluster

import (
	"encoding/json"
	"net/url"
	"strings"
)

// dashboardBaseURL returns the RHOAI dashboard's address for links, as
// "https://host" (any path kept, no trailing slash), or "" when it is
// unknown.
//
// Current releases serve the dashboard behind the data-science gateway
// (https://rh-ai.<apps domain>/), and the old Route
// redhat-ods-applications/rhods-dashboard, now owned by the GatewayConfig,
// only serves an "OpenShift AI has a new address" page. The Dashboard
// component CR (components.platform.opendatahub.io, kind Dashboard) reports
// the address in status.url, which the operator's ConsoleLink uses too. So
// the CR comes first, at whichever API version is served and whatever its
// name; releases from before the gateway have no status.url (or no CR), and
// then the Route's host is used. No release is named: the CR either reports
// a URL or it does not. Only an https URL is used.
func dashboardBaseURL(c *Client) string {
	if u := dashboardURLFromComponent(c); u != "" {
		return u
	}
	if host := routeHost(c, dashboardNamespace, "rhods-dashboard"); host != "" {
		return "https://" + host
	}
	return ""
}

// dashboardURLFromComponent returns the first usable status.url of the
// Dashboard component CRs, or "" (no CRD, no CR, no URL, or the list is
// refused).
func dashboardURLFromComponent(c *Client) string {
	r, err := listServed(c, componentAPIGroup, "/apis/components.platform.opendatahub.io/%s/dashboards", nil, []string{"v1alpha1"})
	if err != nil || r.Version == "" {
		return ""
	}
	var list struct {
		Items []struct {
			Status struct {
				URL string `json:"url"`
			} `json:"status"`
		} `json:"items"`
	}
	if json.Unmarshal(r.Body, &list) != nil {
		return ""
	}
	for _, item := range list.Items {
		if u := httpsBaseURL(item.Status.URL); u != "" {
			return u
		}
	}
	return ""
}

// httpsBaseURL returns raw without its trailing slash when it is an
// absolute https URL with a host, else "".
func httpsBaseURL(raw string) string {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return ""
	}
	return strings.TrimSuffix(raw, "/")
}
