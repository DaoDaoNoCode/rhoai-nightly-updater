package cluster

import "testing"

// PersesDatasources may carry headers or tokens other teams put in them, so
// no code path reads them and neither the template nor the GitOps profile
// grants the ServiceAccount any verb on them (owner sign-off 2026-11-25,
// SECURITY.md "Observability"). The Observability tab reads PersesDashboards
// only, and tells maas-api's Loki proxy from its Deployment's variable names.
func TestNothingReadsPersesDatasources(t *testing.T) {
	all, _ := scanRequests(t, nil, false)
	for _, r := range all {
		if r.Resource == "persesdatasources" || (r.Group == "perses.dev" && r.Resource == "*") {
			t.Errorf("%s requests %s %s/%s", r.Site, r.Verb, r.Group, r.Resource)
		}
	}
	for _, path := range []string{templateRel, rbacGitOpsRel} {
		for _, g := range templateGrantsFrom(t, path) {
			for _, verb := range []string{"get", "list", "watch"} {
				r := rbacRequest{Verb: verb, Group: "perses.dev", Resource: "persesdatasources", Namespace: g.namespace, Name: "usage-logs-all"}
				if g.allows(r) {
					t.Errorf("%s (%s) grants %s on perses.dev/persesdatasources", path, g.source, verb)
				}
			}
		}
	}
}
