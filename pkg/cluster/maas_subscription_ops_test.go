package cluster

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

func subSpec(t *testing.T, mc *modelsCluster) (map[string]any, string) {
	t.Helper()
	sub := mc.f.get(tMaaSSub)
	if sub == nil {
		t.Fatal("no subscription")
	}
	desc, _ := ensureMap(ensureMap(sub, "metadata"), "annotations")["openshift.io/description"].(string)
	return ensureMap(sub, "spec"), desc
}

func asJSON(v any) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}

// Publish takes a preset and an audience; the subscription and the access
// policy name the same audience; its description says it.
func TestMaaSPublish_PresetAndAudience(t *testing.T) {
	mc := maasClusterWithModel(t)
	mc.f.putJSON("/apis/user.openshift.io/v1/groups/ml-team", `{}`)
	res, events := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "publish", Project: "proj",
		Limit: &types.MaaSLimit{Preset: "premium"}, Access: &types.MaaSAudience{Groups: []string{"ml-team", "nobody-yet"}, Users: []string{"system:serviceaccount:ci:runner"}}})
	if !res.Success {
		t.Fatalf("publish: %+v", res)
	}
	spec, desc := subSpec(t, mc)
	if got := asJSON(spec["modelRefs"]); got != `[{"name":"qwen25-05b-instruct","namespace":"proj","tokenRateLimits":[{"limit":100000,"window":"24h"}]}]` {
		t.Errorf("modelRefs %s", got)
	}
	owner := `{"groups":[{"name":"ml-team"},{"name":"nobody-yet"}],"users":["system:serviceaccount:ci:runner"]}`
	if got := asJSON(spec["owner"]); got != owner {
		t.Errorf("owner %s", got)
	}
	if got := asJSON(ensureMap(mc.f.get(tMaaSAuth), "spec")["subjects"]); got != owner {
		t.Errorf("subjects %s", got)
	}
	if desc != "100,000 tokens a day for groups ml-team, nobody-yet and user system:serviceaccount:ci:runner" {
		t.Errorf("description %q", desc)
	}
	warned := false
	for _, e := range events {
		warned = warned || strings.Contains(e.Message, "nobody-yet is not a group on this cluster. Nobody is in it unless your identity provider adds it at sign-in.")
	}
	if !warned {
		t.Error("no warning for a group without an OpenShift Group")
	}
	st, _ := GetMaaSStatus(mc.c)
	if st.Access == nil || st.Access.Everyone || len(st.Access.GroupProblems) != 1 {
		t.Fatalf("access %+v", st.Access)
	}
	if p := st.Access.GroupProblems[0]; p.Name != "nobody-yet" || !p.Missing || p.Commands != "oc adm groups new nobody-yet\noc adm groups add-users nobody-yet <user>" {
		t.Fatalf("group problem %+v", p)
	}
	for _, it := range st.Items {
		if it.Published && (it.Limit == nil || it.Limit.Preset != "premium") {
			t.Fatalf("item limit %+v", it.Limit)
		}
	}
}

func TestMaaSPublish_Unlimited(t *testing.T) {
	mc := maasClusterWithModel(t)
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "publish", Project: "proj", Limit: &types.MaaSLimit{Preset: "unlimited"}}); !res.Success {
		t.Fatalf("publish: %+v", res)
	}
	spec, desc := subSpec(t, mc)
	if got := asJSON(spec["modelRefs"]); got != `[{"name":"qwen25-05b-instruct","namespace":"proj","unlimited":true}]` {
		t.Errorf("modelRefs %s: unlimited alone, no huge numbers", got)
	}
	if desc != "unlimited (usage still metered) for every signed-in user" {
		t.Errorf("description %q", desc)
	}
	err := &K8sError{Status: 422, K8sMessage: `MaaSSubscription "x" is invalid: spec.modelRefs[0].tokenRateLimits: Required value`}
	if why := unlimitedRefusal(err, maasLimit{unlimited: true}); !strings.Contains(why, "has no unlimited option") || strings.Contains(why, "RHOAI") {
		t.Errorf("the refusal of an API without unlimited: %q (no release named)", why)
	}
}

// Whether unlimited is offered is read from the cluster's MaaSSubscription
// CRD, never from a release number; unknown is "offered" (the dry run
// refuses then).
func TestMaaSNoUnlimited_FromTheCRD(t *testing.T) {
	crd := func(refProps string) string {
		return `{"spec":{"versions":[{"name":"v1alpha1","schema":{"openAPIV3Schema":{"properties":{"spec":{"properties":{"modelRefs":{"items":{"properties":{` + refProps + `}}}}}}}}}]}}`
	}
	const path = "/apis/apiextensions.k8s.io/v1/customresourcedefinitions/maassubscriptions.maas.opendatahub.io"
	for _, tc := range []struct {
		name string
		crd  string
		want bool
	}{
		{"with unlimited", crd(`"name":{},"tokenRateLimits":{},"unlimited":{}`), false},
		{"without unlimited", crd(`"name":{},"tokenRateLimits":{}`), true},
		{"no schema to tell", `{"spec":{"versions":[{"name":"v1alpha1"}]}}`, false},
		{"no CRD", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newResourceFake(t)
			if tc.crd != "" {
				f.putJSON(path, tc.crd)
			}
			if got := maasNoUnlimited(c); got != tc.want {
				t.Errorf("noUnlimited = %v, want %v", got, tc.want)
			}
		})
	}
}

// Change limit edits the one entry in place, after a dry run, and keeps
// the others and the audience; a dry-run refusal changes nothing.
func TestMaaSChangeLimit(t *testing.T) {
	mc := maasClusterWithModel(t)
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "publish", Project: "proj"}); !res.Success {
		t.Fatalf("publish: %+v", res)
	}
	custom := &types.MaaSLimit{Preset: "custom", Rates: []types.MaaSTokenRate{{Limit: 500, Window: "1m"}, {Limit: 20000, Window: "1h"}}}
	req := types.MaaSRequest{Action: "change-limit", Project: "proj", Model: "qwen25-05b-instruct", Limit: custom}

	mc.f.failDryRun["PATCH "+tMaaSSub] = 422
	before, _ := subSpec(t, mc)
	beforeJSON := asJSON(before)
	if res, _ := runMaaSOp(t, mc.c, req); res.Success {
		t.Fatalf("change with a dry-run refusal: %+v", res)
	}
	// Only ensureToolObject's field-ownership upgrade may have run (it
	// changes managedFields, never the spec).
	if after, _ := subSpec(t, mc); asJSON(after) != beforeJSON {
		t.Fatalf("the spec changed: %s", asJSON(after))
	}
	delete(mc.f.failDryRun, "PATCH "+tMaaSSub)

	mc.f.requests = nil
	res, events := runMaaSOp(t, mc.c, req)
	if !res.Success {
		t.Fatalf("change: %+v", res)
	}
	if got := finalSteps(events); got != "check=success limit=success ready=success" {
		t.Fatalf("steps %s", got)
	}
	dry, write := -1, -1
	for i, r := range mc.f.requests {
		if strings.HasPrefix(r, "DRYRUN PATCH "+tMaaSSub) && dry < 0 {
			dry = i
		}
		if r == "PATCH "+tMaaSSub && write < 0 {
			write = i
		}
	}
	if dry < 0 || write < dry {
		t.Fatalf("no dry run before the write: %v", mc.f.requests)
	}
	spec, desc := subSpec(t, mc)
	if got := asJSON(spec["modelRefs"]); !strings.Contains(got, `"tokenRateLimits":[{"limit":500,"window":"1m"},{"limit":20000,"window":"1h"}]`) {
		t.Errorf("modelRefs %s", got)
	}
	if got := asJSON(spec["owner"]); got != `{"groups":[{"name":"system:authenticated"}],"users":[]}` {
		t.Errorf("the audience changed: %s", got)
	}
	if desc != "500 tokens a minute and 20,000 tokens an hour for every signed-in user" {
		t.Errorf("description %q", desc)
	}
	// A model that is not published has no limit to change.
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "change-limit", Project: "other", Model: "qwen25-05b-instruct", Limit: custom}); res.Success || res.ErrorCode != "prerequisites" {
		t.Fatalf("change of an unpublished model: %+v", res)
	}
}

func TestMaaSChangeAccess(t *testing.T) {
	mc := maasClusterWithModel(t)
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "change-access", Access: &types.MaaSAudience{Groups: []string{"ml"}}}); res.Success || res.ErrorCode != "prerequisites" {
		t.Fatalf("change before anything is published: %+v", res)
	}
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "publish", Project: "proj", Limit: &types.MaaSLimit{Preset: "unlimited"}}); !res.Success {
		t.Fatalf("publish: %+v", res)
	}
	mc.f.fail["GET /apis/user.openshift.io/v1/groups/ml"] = 403
	res, events := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "change-access", Access: &types.MaaSAudience{Groups: []string{"ml"}}})
	if !res.Success {
		t.Fatalf("change: %+v", res)
	}
	if got := finalSteps(events); got != "check=success access=success ready=success" {
		t.Fatalf("steps %s", got)
	}
	cannot := false
	for _, e := range events {
		cannot = cannot || strings.Contains(e.Message, "Cannot check whether group ml exists")
	}
	if !cannot {
		t.Error("a group that cannot be read is not said so")
	}
	spec, _ := subSpec(t, mc)
	if got := asJSON(spec["owner"]); got != `{"groups":[{"name":"ml"}],"users":[]}` {
		t.Errorf("owner %s", got)
	}
	if got := asJSON(spec["modelRefs"]); !strings.Contains(got, `"unlimited":true`) {
		t.Errorf("the limit changed: %s", got)
	}
	if got := asJSON(ensureMap(mc.f.get(tMaaSAuth), "spec")["subjects"]); got != `{"groups":[{"name":"ml"}],"users":[]}` {
		t.Errorf("subjects %s", got)
	}
	// Publishing again with another audience while it is the only model is
	// allowed; the audience is the subscription's.
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "publish", Project: "proj", Access: &types.MaaSAudience{Everyone: true}}); !res.Success {
		t.Fatalf("publish again: %+v", res)
	}
}

// Upgrade: a subscription an earlier build wrote (100 tokens a minute,
// everyone) reads as the free sample; Unpublish of another model keeps the
// entries left as they are.
func TestMaaSUpgrade_EarlierSubscriptionIsTheFreeSample(t *testing.T) {
	mc := maasClusterWithModel(t)
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "publish", Project: "proj"}); !res.Success {
		t.Fatalf("publish: %+v", res)
	}
	sub := mc.f.get(tMaaSSub)
	ensureMap(sub, "metadata")["annotations"] = map[string]any{"openshift.io/display-name": "rhoai-nightly-updater models", "openshift.io/description": "100 tokens a minute for every signed-in user"}
	mc.f.put(tMaaSSub, sub)
	st, _ := GetMaaSStatus(mc.c)
	for _, it := range st.Items {
		if it.Published && (it.Limit == nil || it.Limit.Preset != "free") {
			t.Fatalf("limit %+v", it.Limit)
		}
	}
	if st.Access == nil || !st.Access.Everyone {
		t.Fatalf("access %+v", st.Access)
	}
}

func TestValidateMaaSRequest_NewFields(t *testing.T) {
	url := &types.MaaSDatabaseRequest{Kind: "external", ConnectionURL: tUserDBURL}
	for _, tc := range []struct {
		req  types.MaaSRequest
		want string
	}{
		{types.MaaSRequest{Action: "setup", Database: url}, ""},
		{types.MaaSRequest{Action: "setup", Database: &types.MaaSDatabaseRequest{Kind: "external", ConnectionURL: "mysql://" + tUserDBPass + "@x/y"}}, "must begin with postgresql://"},
		{types.MaaSRequest{Action: "teardown", Database: tCNPG}, "teardown takes no database"},
		{types.MaaSRequest{Action: "change-database", Database: tCNPG}, "confirm it"},
		{types.MaaSRequest{Action: "change-database", Database: tCNPG, ConfirmKeysLost: true}, ""},
		{types.MaaSRequest{Action: "publish", Project: "p", ConfirmKeysLost: true}, "takes no confirmation"},
		{types.MaaSRequest{Action: "unpublish", Project: "p", Limit: &types.MaaSLimit{Preset: "free"}}, "unpublish takes no limit"},
		{types.MaaSRequest{Action: "change-limit", Project: "p"}, "give the model's new limit"},
		{types.MaaSRequest{Action: "change-limit", Project: "p", Limit: &types.MaaSLimit{Preset: "custom", Rates: []types.MaaSTokenRate{{Limit: 1, Window: "1d"}}}}, "window"},
		{types.MaaSRequest{Action: "change-access"}, "say who can use"},
		{types.MaaSRequest{Action: "change-access", Access: &types.MaaSAudience{Groups: []string{"oidc:ml", "/admins"}}}, ""},
		{types.MaaSRequest{Action: "change-access", Access: &types.MaaSAudience{Groups: []string{"a\nb"}}}, "control character"},
		{types.MaaSRequest{Action: "publish", Project: "p", Access: &types.MaaSAudience{Everyone: true}, Limit: &types.MaaSLimit{Preset: "premium"}}, ""},
	} {
		got := ValidateMaaSRequest(tc.req)
		if tc.want == "" && got != "" || tc.want != "" && !strings.Contains(got, tc.want) {
			t.Errorf("%s: %q, want %q", tc.req.Action, got, tc.want)
		}
		if strings.Contains(got, tUserDBPass) {
			t.Errorf("the message quotes the URL: %q", got)
		}
	}
}
