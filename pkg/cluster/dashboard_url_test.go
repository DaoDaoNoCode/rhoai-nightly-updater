package cluster

import (
	"testing"
)

const (
	dashboardsPath     = "/apis/components.platform.opendatahub.io/v1alpha1/dashboards"
	dashboardRoutePath = "/apis/route.openshift.io/v1/namespaces/redhat-ods-applications/routes/rhods-dashboard"
)

// The dashboard's address comes from the Dashboard component CR's
// status.url (current releases, behind the gateway), else from the
// rhods-dashboard Route (releases before it), else it is unknown and no
// link is built. The old Route on current releases only serves an
// "OpenShift AI has a new address" page, so the CR wins.
func TestDashboardBaseURL(t *testing.T) {
	cases := []struct {
		name  string
		setup func(f *resourceFake)
		want  string
	}{
		{"CR with status.url wins over the Route", func(f *resourceFake) {
			f.putJSON(dashboardsPath+"/any-name", `{"status":{"url":"https://rh-ai.apps.example.com/"}}`)
			f.putJSON(dashboardRoutePath, `{"spec":{"host":"rhods-dashboard-redhat-ods-applications.apps.example.com"}}`)
		}, "https://rh-ai.apps.example.com"},
		{"CR without a URL: the Route", func(f *resourceFake) {
			f.putJSON(dashboardsPath+"/default-dashboard", `{"status":{}}`)
			f.putJSON(dashboardRoutePath, `{"spec":{"host":"rhods-dashboard-redhat-ods-applications.apps.example.com"}}`)
		}, "https://rhods-dashboard-redhat-ods-applications.apps.example.com"},
		{"CR list refused (403): the Route", func(f *resourceFake) {
			f.fail["GET "+dashboardsPath] = 403
			f.putJSON(dashboardRoutePath, `{"spec":{"host":"rhods-dashboard-redhat-ods-applications.apps.example.com"}}`)
		}, "https://rhods-dashboard-redhat-ods-applications.apps.example.com"},
		{"a non-https status.url is not used", func(f *resourceFake) {
			f.putJSON(dashboardsPath+"/default-dashboard", `{"status":{"url":"http://rh-ai.apps.example.com/"}}`)
		}, ""},
		{"neither: unknown", func(f *resourceFake) {}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newResourceFake(t)
			tc.setup(f)
			if got := dashboardBaseURL(c); got != tc.want {
				t.Errorf("dashboardBaseURL = %q, want %q", got, tc.want)
			}
		})
	}
}

// The pipeline server's "Open pipelines" link is built on that address.
func TestGetResourcesStatus_PipelineLinkUsesTheDashboardAddress(t *testing.T) {
	cases := []struct {
		name  string
		setup func(f *resourceFake)
		want  string
	}{
		{"gateway address", func(f *resourceFake) {
			f.putJSON(dashboardsPath+"/default-dashboard", `{"status":{"url":"https://rh-ai.apps.example.com/"}}`)
			f.putJSON(dashboardRoutePath, `{"spec":{"host":"rhods-dashboard-redhat-ods-applications.apps.example.com"}}`)
		}, "https://rh-ai.apps.example.com/develop-train/pipelines/definitions/team-a"},
		{"Route fallback", func(f *resourceFake) {
			f.putJSON(dashboardRoutePath, `{"spec":{"host":"rhods-dashboard-redhat-ods-applications.apps.example.com"}}`)
		}, "https://rhods-dashboard-redhat-ods-applications.apps.example.com/develop-train/pipelines/definitions/team-a"},
		{"unknown: no link", func(f *resourceFake) {}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newResourceFake(t)
			putReadyManagedS3(f)
			putDSProject(f, "team-a")
			putDSPA(f, "team-a", dspaName, toolLabelJSON, toolFieldManager, "Apply", s3Host(), dspaSecretName, "", `{"conditions":[{"type":"Ready","status":"True"}]}`)
			tc.setup(f)
			st, err := GetResourcesStatus(c)
			if err != nil {
				t.Fatal(err)
			}
			if len(st.PipelineServers) != 1 || st.PipelineServers[0].UIRoute != tc.want {
				t.Errorf("pipeline servers = %+v, want UIRoute %q", st.PipelineServers, tc.want)
			}
		})
	}
}

// A transient read error is cached only briefly: once the read works, the
// link shows without waiting out the five minutes an answer is kept.
func TestCachedDashboardURL_ReadErrorIsCachedBriefly(t *testing.T) {
	cacheTime := fakeCacheClock(t, dashboardURLCache)
	f, c := newResourceFake(t)
	f.putJSON(dashboardsPath+"/default-dashboard", `{"status":{"url":"https://rh-ai.apps.example.com"}}`)
	f.fail["GET "+dashboardsPath] = 500
	if got := cachedDashboardURL(c); got != "" {
		t.Fatalf("cachedDashboardURL = %q during the error", got)
	}
	delete(f.fail, "GET "+dashboardsPath)
	cacheTime.Advance(dashboardURLErrorTTL)
	if got := cachedDashboardURL(c); got != "https://rh-ai.apps.example.com" {
		t.Errorf("cachedDashboardURL = %q after the error cleared", got)
	}
	// An answer is kept for the full lifetime.
	f.fail["GET "+dashboardsPath] = 500
	if got := cachedDashboardURL(c); got != "https://rh-ai.apps.example.com" {
		t.Errorf("a cached answer was not used: %q", got)
	}
}

// The status (both profiles) uses the CR only: the gitops profile cannot
// read the Route.
func TestCachedDashboardURL_UsesTheCROnly(t *testing.T) {
	dashboardURLCache.Purge()
	t.Cleanup(dashboardURLCache.Purge)
	f, c := newResourceFake(t)
	f.putJSON(dashboardRoutePath, `{"spec":{"host":"rhods-dashboard-redhat-ods-applications.apps.example.com"}}`)
	if got := cachedDashboardURL(c); got != "" {
		t.Errorf("without a CR URL the status must not fall back to the Route: %q", got)
	}
	dashboardURLCache.Purge()
	f.putJSON(dashboardsPath+"/default-dashboard", `{"status":{"url":"https://rh-ai.apps.example.com"}}`)
	if got := cachedDashboardURL(c); got != "https://rh-ai.apps.example.com" {
		t.Errorf("cachedDashboardURL = %q", got)
	}
}
