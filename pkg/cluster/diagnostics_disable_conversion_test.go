package cluster

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The objects of a CRD whose conversion webhook a component serves are
// listed with the signed-in user's own client (never the ServiceAccount's),
// at the CRD's storage version, one page of conversionObjectsLimit.

const (
	mcpCRDName   = "mcpservers.mcp.x-k8s.io"
	mcpStorage   = "/apis/mcp.x-k8s.io/v1beta1/mcpservers"
	convUserAuth = "Bearer conversion-user-token"
)

// mcpCRDJSON is the live 3.6 shape of the MCPServer CRD: two served
// versions, v1beta1 the storage version, converted by the Service of the
// mcplifecycleoperator module (already gone, as live after removal).
func mcpCRDJSON(scope string, versions string) string {
	return `{"metadata":{"name":"` + mcpCRDName + `"},"spec":{"group":"mcp.x-k8s.io","names":{"plural":"mcpservers"},"scope":"` + scope + `",` +
		`"versions":` + versions + `,` +
		`"conversion":{"strategy":"Webhook","webhook":{"clientConfig":{"service":{"namespace":"redhat-ods-applications","name":"mcp-lifecycle-operator-webhook-service"}}}}},` +
		`"status":{"storedVersions":["v1alpha1","v1beta1"]}}`
}

const mcpVersions = `[{"name":"v1alpha1","served":true,"storage":false},{"name":"v1beta1","served":true,"storage":true}]`

// convObjectsWorld serves the RHOAI CRD list (crds, JSON objects) and a
// removalChecker whose client is the ServiceAccount's, with the signed-in
// user's client attached unless noUser. Every request's Authorization header
// is recorded in auth by "METHOD path?query".
type convObjectsWorld struct {
	f    *fakeAPI
	c    *Client
	mu   sync.Mutex
	auth map[string][]string
}

func newConvObjectsWorld(t *testing.T, noUser bool, crds ...string) *convObjectsWorld {
	t.Helper()
	f, c := newFakeAPI(t)
	w := &convObjectsWorld{f: f, c: c, auth: map[string][]string{}}
	f.json("GET", "/apis/apiextensions.k8s.io/v1/customresourcedefinitions", http.StatusOK, `{"items":[`+strings.Join(crds, ",")+`]}`)
	if !noUser {
		u := *c
		u.token, u.asUser = "conversion-user-token", true
		c.SetSignedInUser(&u)
	}
	return w
}

// list serves path, recording who asked.
func (w *convObjectsWorld) list(path string, h func(r *http.Request) (int, string)) {
	w.f.handle("GET", path, func(r *http.Request, _ []byte) (int, string) {
		w.mu.Lock()
		w.auth[path] = append(w.auth[path], r.Header.Get("Authorization")+" "+r.URL.RawQuery)
		w.mu.Unlock()
		return h(r)
	})
}

func (w *convObjectsWorld) calls(path string) []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string{}, w.auth[path]...)
}

func mcpItems(refs ...string) string {
	items := make([]string, 0, len(refs))
	for _, ref := range refs {
		ns, name, ok := strings.Cut(ref, "/")
		if !ok {
			items = append(items, fmt.Sprintf(`{"metadata":{"name":%q}}`, ref))
			continue
		}
		items = append(items, fmt.Sprintf(`{"metadata":{"namespace":%q,"name":%q}}`, ns, name))
	}
	return strings.Join(items, ",")
}

// The live case: 4 MCPServers in 2 projects block the removal, named, with
// how to delete them; the list is the user's, at the storage version, one
// page of 20.
func TestConversionBlockerNamesTheObjectsThatExist(t *testing.T) {
	w := newConvObjectsWorld(t, false, mcpCRDJSON("Namespaced", mcpVersions))
	w.list(mcpStorage, func(*http.Request) (int, string) {
		return http.StatusOK, `{"metadata":{"resourceVersion":"9"},"items":[` + mcpItems("team-b/search", "team-a/github", "team-a/fetch", "team-b/time") + `]}`
	})
	why := newRemovalChecker(w.c).conversionBlocker("mcplifecycleoperator")
	for _, want := range []string{
		"mcplifecycleoperator serves the conversion webhook of CRD " + mcpCRDName,
		"4 objects of " + mcpCRDName + " exist: team-a/fetch, team-a/github, team-b/search, team-b/time.",
		"Delete them first: oc delete " + mcpCRDName + " -n <namespace> <name>.",
		"The tool does not remove mcplifecycleoperator until none remain.",
	} {
		if !strings.Contains(why, want) {
			t.Fatalf("blocker %q lacks %q", why, want)
		}
	}
	if strings.Contains(why, "cannot list") || strings.Contains(why, "OpenShift console") {
		t.Fatalf("blocker still says the tool cannot list: %q", why)
	}
	if got := w.calls(mcpStorage); len(got) != 1 || got[0] != convUserAuth+" limit=20" {
		t.Fatalf("lists = %q, want one with the user's token and limit=20", got)
	}
}

// No objects: this check does not block (the other checks still run).
func TestConversionBlockerAllowsWhenNoObjectsExist(t *testing.T) {
	w := newConvObjectsWorld(t, false, mcpCRDJSON("Namespaced", mcpVersions))
	w.list(mcpStorage, func(*http.Request) (int, string) { return http.StatusOK, `{"metadata":{},"items":[]}` })
	rc := newRemovalChecker(w.c)
	if why := rc.conversionBlocker("mcplifecycleoperator"); why != "" {
		t.Fatalf("blocker = %q, want none", why)
	}
	if got := w.calls(mcpStorage); len(got) != 1 || !strings.HasPrefix(got[0], convUserAuth) {
		t.Fatalf("lists = %q", got)
	}
	// A component that serves no conversion webhook lists nothing.
	if why := rc.conversionBlocker("ray"); why != "" {
		t.Fatalf("ray: %q", why)
	}
	if got := w.calls(mcpStorage); len(got) != 1 {
		t.Fatalf("ray listed MCPServers: %q", got)
	}
}

// A list that fails refuses the removal (fail closed), with the error and
// the command that checks by hand.
func TestConversionBlockerFailsClosedWhenTheListFails(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
		status           int
	}{
		{"forbidden", `{"kind":"Status","status":"Failure","reason":"Forbidden","code":403,"message":"mcpservers.mcp.x-k8s.io is forbidden"}`,
			"k8s API error 403 (Forbidden): mcpservers.mcp.x-k8s.io is forbidden; " + UserForbiddenMessage, http.StatusForbidden},
		{"conversion webhook down", `{"kind":"Status","status":"Failure","reason":"InternalError","code":500,"message":"conversion webhook for mcp.x-k8s.io/v1alpha1, Kind=MCPServer failed: service \"mcp-lifecycle-operator-webhook-service\" not found"}`,
			`conversion webhook for mcp.x-k8s.io/v1alpha1, Kind=MCPServer failed: service "mcp-lifecycle-operator-webhook-service" not found`, http.StatusInternalServerError},
		{"timeout", `{"kind":"Status","status":"Failure","reason":"Timeout","code":504,"message":"the server was unable to return a response in the time allotted"}`,
			"the server was unable to return a response in the time allotted", http.StatusGatewayTimeout},
		{"not JSON", `<html>`, "could not parse the list", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newConvObjectsWorld(t, false, mcpCRDJSON("Namespaced", mcpVersions))
			w.list(mcpStorage, func(*http.Request) (int, string) { return tc.status, tc.body })
			why := newRemovalChecker(w.c).conversionBlocker("mcplifecycleoperator")
			for _, want := range []string{
				"The objects of " + mcpCRDName + " could not be listed with your sign-in: ",
				tc.want,
				"Check for them with: oc get " + mcpCRDName + " -A.",
				"The tool cannot tell whether any exist, so it does not remove mcplifecycleoperator.",
			} {
				if !strings.Contains(why, want) {
					t.Fatalf("blocker %q lacks %q", why, want)
				}
			}
		})
	}
}

// Without the signed-in user's client (the Components page's repair
// preview, the gitops profile) it refuses, and the ServiceAccount does not
// list the objects instead.
func TestConversionBlockerNeedsTheUsersSignIn(t *testing.T) {
	w := newConvObjectsWorld(t, true, mcpCRDJSON("Namespaced", mcpVersions))
	w.list(mcpStorage, func(*http.Request) (int, string) { return http.StatusOK, `{"items":[]}` })
	why := newRemovalChecker(w.c).conversionBlocker("mcplifecycleoperator")
	for _, want := range []string{
		"The tool lists those objects with your own sign-in, which this check does not have, so it cannot tell whether any exist and does not remove mcplifecycleoperator here.",
		"Check for them with: oc get " + mcpCRDName + " -A.",
	} {
		if !strings.Contains(why, want) {
			t.Fatalf("blocker %q lacks %q", why, want)
		}
	}
	if got := w.calls(mcpStorage); len(got) != 0 {
		t.Fatalf("listed without the user's client: %q", got)
	}
}

// The list is at the storage version (objects stored at it need no
// conversion); a storage version that is not served falls back to a served
// one (listPath).
func TestConversionBlockerListsAtTheStorageVersion(t *testing.T) {
	for _, tc := range []struct {
		name, versions, path string
	}{
		{"storage listed last", mcpVersions, mcpStorage},
		{"storage listed first", `[{"name":"v1beta1","served":true,"storage":true},{"name":"v1alpha1","served":true,"storage":false}]`, mcpStorage},
		{"storage not served", `[{"name":"v1alpha1","served":true,"storage":false},{"name":"v1beta1","served":false,"storage":true}]`, "/apis/mcp.x-k8s.io/v1alpha1/mcpservers"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newConvObjectsWorld(t, false, mcpCRDJSON("Namespaced", tc.versions))
			w.list(tc.path, func(*http.Request) (int, string) { return http.StatusOK, `{"items":[]}` })
			if why := newRemovalChecker(w.c).conversionBlocker("mcplifecycleoperator"); why != "" {
				t.Fatalf("blocker %q", why)
			}
			if got := w.calls(tc.path); len(got) != 1 {
				t.Fatalf("lists of %s = %q", tc.path, got)
			}
		})
	}
	// No served version: nothing to list it at, so it refuses.
	w := newConvObjectsWorld(t, false, mcpCRDJSON("Namespaced", `[{"name":"v1beta1","served":false,"storage":true}]`))
	if why := newRemovalChecker(w.c).conversionBlocker("mcplifecycleoperator"); !strings.Contains(why, "could not be listed with your sign-in: the CRD serves no version") {
		t.Fatalf("blocker %q", why)
	}
}

// One page of 20: "N" when the API server says that was all, "N or more"
// when it returns metadata.continue, with remainingItemCount as an
// estimate; at most 5 names per CRD.
func TestConversionBlockerCountsHonestly(t *testing.T) {
	var twenty []string
	for i := range 20 {
		twenty = append(twenty, fmt.Sprintf("p%02d/s", 19-i))
	}
	for _, tc := range []struct {
		name, meta, items string
		want, not         []string
	}{
		{"more, with an estimate", `{"continue":"tok","remainingItemCount":17}`, mcpItems(twenty...),
			[]string{"20 or more objects of " + mcpCRDName + " exist (about 37, the API server's estimate): p00/s, p01/s, p02/s, p03/s, p04/s and more. List them all with: oc get " + mcpCRDName + " -A."}, nil},
		{"more, without an estimate", `{"continue":"tok"}`, mcpItems(twenty...),
			[]string{"20 or more objects of " + mcpCRDName + " exist: p00/s, p01/s, p02/s, p03/s, p04/s and more."}, []string{"estimate"}},
		{"exactly 7", `{}`, mcpItems("a/1", "a/2", "a/3", "a/4", "a/5", "a/6", "a/7"),
			[]string{"7 objects of " + mcpCRDName + " exist: a/1, a/2, a/3, a/4, a/5 and 2 more. List them all with: oc get " + mcpCRDName + " -A."}, []string{"or more"}},
		{"one", `{}`, mcpItems("a/1"),
			[]string{"1 object of " + mcpCRDName + " exists: a/1. Delete it first: oc delete " + mcpCRDName + " -n <namespace> <name>."}, []string{"List them all"}},
		// A server that ignores limit returns everything without continue:
		// the count is exact.
		{"limit ignored", `{}`, mcpItems(append(twenty, "q/1", "q/2")...),
			[]string{"22 objects of " + mcpCRDName + " exist: "}, []string{"or more"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newConvObjectsWorld(t, false, mcpCRDJSON("Namespaced", mcpVersions))
			w.list(mcpStorage, func(*http.Request) (int, string) {
				return http.StatusOK, `{"metadata":` + tc.meta + `,"items":[` + tc.items + `]}`
			})
			why := newRemovalChecker(w.c).conversionBlocker("mcplifecycleoperator")
			for _, want := range tc.want {
				if !strings.Contains(why, want) {
					t.Fatalf("blocker %q lacks %q", why, want)
				}
			}
			for _, not := range tc.not {
				if strings.Contains(why, not) {
					t.Fatalf("blocker %q has %q", why, not)
				}
			}
		})
	}
	// No items on the first page, but continue set: the API server says
	// more exist (limit doc: "clients should only use the presence of the
	// continue field"), so it is not "none".
	w := newConvObjectsWorld(t, false, mcpCRDJSON("Namespaced", mcpVersions))
	w.list(mcpStorage, func(*http.Request) (int, string) { return http.StatusOK, `{"metadata":{"continue":"tok"},"items":[]}` })
	if why := newRemovalChecker(w.c).conversionBlocker("mcplifecycleoperator"); !strings.Contains(why, "could not be listed with your sign-in: the API server returned no objects but says more exist") {
		t.Fatalf("blocker %q", why)
	}
}

// A cluster-scoped CRD's objects have no namespace, and neither does the
// delete command; a second CRD without objects is not named.
func TestConversionBlockerClusterScopedAndSeveralCRDs(t *testing.T) {
	other := strings.ReplaceAll(strings.ReplaceAll(mcpCRDJSON("Namespaced", mcpVersions), mcpCRDName, "mcpcatalogs.mcp.x-k8s.io"), `"plural":"mcpservers"`, `"plural":"mcpcatalogs"`)
	w := newConvObjectsWorld(t, false, mcpCRDJSON("Cluster", mcpVersions), other)
	w.list(mcpStorage, func(*http.Request) (int, string) {
		return http.StatusOK, `{"items":[` + mcpItems("shared", "global") + `]}`
	})
	w.list("/apis/mcp.x-k8s.io/v1beta1/mcpcatalogs", func(*http.Request) (int, string) { return http.StatusOK, `{"items":[]}` })
	why := newRemovalChecker(w.c).conversionBlocker("mcplifecycleoperator")
	for _, want := range []string{
		"of CRDs mcpcatalogs.mcp.x-k8s.io, " + mcpCRDName,
		"2 objects of " + mcpCRDName + " exist: global, shared. Delete them first: oc delete " + mcpCRDName + " <name>.",
	} {
		if !strings.Contains(why, want) {
			t.Fatalf("blocker %q lacks %q", why, want)
		}
	}
	if strings.Contains(why, "objects of mcpcatalogs") {
		t.Fatalf("named the CRD without objects: %q", why)
	}
}

// The plan (withAdminRead: the ServiceAccount's client carrying the user's)
// and the change (the user's own client) list the objects with the user's
// token and agree: objects block both; with none the removal goes through.
func TestConversionWebhookComponentRemovalFollowsItsObjects(t *testing.T) {
	f, c := managerWorld(t, func(dsc map[string]any) {
		setPath(t, dsc, "spec.components.sparkoperator.managementState", "Managed")
	})
	putSparkCRD(f, "sparkapplications")
	putSparkCRD(f, "scheduledsparkapplications")
	f.put("/apis/sparkoperator.k8s.io/v1beta2/namespaces/team-a/sparkapplications/pi", map[string]any{})
	plan, err := PlanDSCChange(c, "toggle", "sparkoperator", "Removed", false, false)
	if err != nil {
		t.Fatal(err)
	}
	why := blockReasons(plan, "sparkoperator")
	if !strings.Contains(why, "1 object of sparkapplications.sparkoperator.k8s.io exists: team-a/pi.") || strings.Contains(why, "objects of scheduledsparkapplications") {
		t.Fatalf("plan blocker %q", why)
	}
	user := c.SignedInUser()
	user.signedInUser = nil
	res, _ := SetDSCManagementState(user, types.DSCToggleRequest{Name: "default-dsc", Path: "sparkoperator", From: "Managed", To: "Removed", UID: plan.UID, Token: plan.Token})
	if res.Success || res.ErrorCode != "prerequisites" || !strings.Contains(strings.Join(res.Logs, " "), "team-a/pi") {
		t.Fatalf("change = %+v", res)
	}
	for _, call := range append(f.callsTo(http.MethodGet, "/apis/sparkoperator.k8s.io/v1beta2/sparkapplications"), f.callsTo(http.MethodGet, "/apis/sparkoperator.k8s.io/v1beta2/scheduledsparkapplications")...) {
		if call.Token != fakeUserToken || call.Query.Get("limit") != "20" {
			t.Fatalf("listed with token %q, query %v", call.Token, call.Query)
		}
	}
	if dry, real := patchesTo(f); len(dry)+len(real) != 0 {
		t.Fatal("patched a blocked change")
	}

	// The object is deleted: the plan has no conversion blocker, and the
	// change goes through.
	f.mu.Lock()
	delete(f.objects, fakeKey{gv: "sparkoperator.k8s.io/v1beta2", ns: "team-a", plural: "sparkapplications", name: "pi"})
	f.mu.Unlock()
	plan, err = PlanDSCChange(c, "toggle", "sparkoperator", "Removed", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if why := blockReasons(plan, "sparkoperator"); strings.Contains(why, "conversion webhook") {
		t.Fatalf("plan still blocked by the conversion check: %q", why)
	}
	if len(plan.Blocked) != 0 {
		t.Fatalf("plan blocked = %+v", plan.Blocked)
	}
	res, _ = SetDSCManagementState(user, types.DSCToggleRequest{Name: "default-dsc", Path: "sparkoperator", From: "Managed", To: "Removed", UID: plan.UID, Token: plan.Token})
	if !res.Success {
		t.Fatalf("change = %+v", res)
	}
	if _, real := patchesTo(f); len(real) != 1 || real[0].Token != fakeUserToken {
		t.Fatalf("patches = %+v", real)
	}
}

// putSparkCRD stores <plural>.sparkoperator.k8s.io, converted by the
// sparkoperator module's Service (putConversionCRD), with the group,
// plural, scope and versions (v1beta2 the storage version) the object list
// needs.
func putSparkCRD(f *resourceFake, plural string) {
	name := plural + ".sparkoperator.k8s.io"
	putConversionCRD(f, name, "spark-operator-webhook-service")
	crd := f.get("/apis/apiextensions.k8s.io/v1/customresourcedefinitions/" + name)
	f.mu.Lock()
	defer f.mu.Unlock()
	spec := ensureMap(crd, "spec")
	spec["group"], spec["scope"] = "sparkoperator.k8s.io", "Namespaced"
	spec["names"] = map[string]any{"plural": plural}
	spec["versions"] = []any{
		map[string]any{"name": "v1beta1", "served": true, "storage": false},
		map[string]any{"name": "v1beta2", "served": true, "storage": true},
	}
}
