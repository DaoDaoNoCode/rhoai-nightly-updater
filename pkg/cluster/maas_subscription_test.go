package cluster

import (
	"reflect"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/serving"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

func TestParseMaaSLimit(t *testing.T) {
	rate := func(l int64, w string) types.MaaSTokenRate { return types.MaaSTokenRate{Limit: l, Window: w} }
	for _, tc := range []struct {
		name string
		in   *types.MaaSLimit
		want serving.MaaSLimit
		err  string
	}{
		{"none is the free sample, as before", nil, serving.MaaSLimit{Rates: []serving.MaaSRate{{Limit: 100, Window: "1m"}}}, ""},
		{"free", &types.MaaSLimit{Preset: "free"}, serving.MaaSLimit{Rates: []serving.MaaSRate{{Limit: 100, Window: "1m"}}}, ""},
		{"premium", &types.MaaSLimit{Preset: "premium"}, serving.MaaSLimit{Rates: []serving.MaaSRate{{Limit: 100000, Window: "24h"}}}, ""},
		{"unlimited", &types.MaaSLimit{Preset: "unlimited"}, serving.MaaSLimit{Unlimited: true}, ""},
		{"custom", &types.MaaSLimit{Preset: "custom", Rates: []types.MaaSTokenRate{rate(500, "1m"), rate(20000, "1h")}}, serving.MaaSLimit{Rates: []serving.MaaSRate{{Limit: 500, Window: "1m"}, {Limit: 20000, Window: "1h"}}}, ""},
		{"custom at the CRD's bounds", &types.MaaSLimit{Preset: "custom", Rates: []types.MaaSTokenRate{rate(1, "9999h"), rate(1000000000, "1s")}}, serving.MaaSLimit{Rates: []serving.MaaSRate{{Limit: 1, Window: "9999h"}, {Limit: 1000000000, Window: "1s"}}}, ""},
		{"a preset takes no rates", &types.MaaSLimit{Preset: "free", Rates: []types.MaaSTokenRate{rate(5, "1m")}}, serving.MaaSLimit{}, "takes no"},
		{"a preset is not unlimited", &types.MaaSLimit{Preset: "premium", Unlimited: true}, serving.MaaSLimit{}, "takes no"},
		{"custom needs a rate", &types.MaaSLimit{Preset: "custom"}, serving.MaaSLimit{}, "at least one"},
		{"custom is never unlimited", &types.MaaSLimit{Preset: "custom", Unlimited: true, Rates: []types.MaaSTokenRate{rate(5, "1m")}}, serving.MaaSLimit{}, "Unlimited"},
		{"zero tokens", &types.MaaSLimit{Preset: "custom", Rates: []types.MaaSTokenRate{rate(0, "1m")}}, serving.MaaSLimit{}, "1 to 1,000,000,000"},
		{"more than the CRD allows (no fake unlimited)", &types.MaaSLimit{Preset: "custom", Rates: []types.MaaSTokenRate{rate(1000000001, "1m")}}, serving.MaaSLimit{}, "1 to 1,000,000,000"},
		{"days are not a unit", &types.MaaSLimit{Preset: "custom", Rates: []types.MaaSTokenRate{rate(5, "1d")}}, serving.MaaSLimit{}, "window"},
		{"no leading zero", &types.MaaSLimit{Preset: "custom", Rates: []types.MaaSTokenRate{rate(5, "0m")}}, serving.MaaSLimit{}, "window"},
		{"at most four digits", &types.MaaSLimit{Preset: "custom", Rates: []types.MaaSTokenRate{rate(5, "10000s")}}, serving.MaaSLimit{}, "window"},
		{"the same window twice", &types.MaaSLimit{Preset: "custom", Rates: []types.MaaSTokenRate{rate(5, "1m"), rate(9, "1m")}}, serving.MaaSLimit{}, "twice"},
		{"too many rates", &types.MaaSLimit{Preset: "custom", Rates: []types.MaaSTokenRate{rate(1, "1s"), rate(1, "2s"), rate(1, "3s"), rate(1, "4s"), rate(1, "5s"), rate(1, "6s")}}, serving.MaaSLimit{}, "at most 5"},
		{"unknown preset", &types.MaaSLimit{Preset: "gold"}, serving.MaaSLimit{}, "preset"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, msg := serving.ParseMaaSLimit(tc.in)
			if tc.err != "" {
				if !strings.Contains(msg, tc.err) {
					t.Fatalf("message = %q, want it to contain %q", msg, tc.err)
				}
				return
			}
			if msg != "" || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %+v %q, want %+v", got, msg, tc.want)
			}
		})
	}
}

func TestMaaSLimit_PresetTextAndModelRef(t *testing.T) {
	for _, tc := range []struct {
		l             serving.MaaSLimit
		preset, text  string
		unlimitedJSON bool
	}{
		{serving.MaaSLimit{Rates: []serving.MaaSRate{{Limit: 100, Window: "1m"}}}, "free", "100 tokens a minute", false},
		{serving.MaaSLimit{Rates: []serving.MaaSRate{{Limit: 100000, Window: "24h"}}}, "premium", "100,000 tokens a day", false},
		{serving.MaaSLimit{Unlimited: true}, "unlimited", "unlimited (usage still metered)", true},
		{serving.MaaSLimit{Rates: []serving.MaaSRate{{Limit: 500, Window: "1m"}, {Limit: 20000, Window: "1h"}}}, "custom", "500 tokens a minute and 20,000 tokens an hour", false},
		{serving.MaaSLimit{Rates: []serving.MaaSRate{{Limit: 7, Window: "30s"}, {Limit: 1, Window: "2h"}, {Limit: 3, Window: "1s"}}}, "custom", "7 tokens every 30 seconds, 1 token every 2 hours and 3 tokens a second", false},
	} {
		if got := tc.l.Preset(); got != tc.preset {
			t.Errorf("%+v: preset %q, want %q", tc.l, got, tc.preset)
		}
		if got := tc.l.Text(); got != tc.text {
			t.Errorf("%+v: text %q, want %q", tc.l, got, tc.text)
		}
		ref := map[string]any{"name": "m", "namespace": "p"}
		tc.l.AddTo(ref)
		_, hasRates := ref["tokenRateLimits"]
		if ref["unlimited"] == true != tc.unlimitedJSON || hasRates == tc.unlimitedJSON {
			t.Errorf("%+v: model reference %v: exactly one of unlimited and tokenRateLimits (the CRD's rule)", tc.l, ref)
		}
		back, ok := serving.MaaSLimitFromModelRef(ref)
		if !ok || !reflect.DeepEqual(back, tc.l) {
			t.Errorf("%+v: read back as %+v %v", tc.l, back, ok)
		}
	}
	if _, ok := serving.MaaSLimitFromModelRef(map[string]any{"name": "m"}); ok {
		t.Error("a model reference with neither is not a limit")
	}
}

func TestParseMaaSAudience(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   *types.MaaSAudience
		want serving.MaaSAudience
		err  string
	}{
		{"none is everyone, as before", nil, serving.MaaSAudience{Everyone: true}, ""},
		{"everyone", &types.MaaSAudience{Everyone: true}, serving.MaaSAudience{Everyone: true}, ""},
		{"groups and users, trimmed, without duplicates", &types.MaaSAudience{Groups: []string{" data-science ", "ml team", "data-science"}, Users: []string{"system:serviceaccount:ci:runner", "alice@example.com", "kube:admin", "b64:YWxpY2U6Ym9i"}},
			serving.MaaSAudience{Groups: []string{"data-science", "ml team"}, Users: []string{"system:serviceaccount:ci:runner", "alice@example.com", "kube:admin", "b64:YWxpY2U6Ym9i"}}, ""},
		{"everyone names nobody else", &types.MaaSAudience{Everyone: true, Groups: []string{"a"}}, serving.MaaSAudience{}, "Everyone"},
		{"specific needs a name", &types.MaaSAudience{Groups: []string{" "}}, serving.MaaSAudience{}, "at least one"},
		{"everyone's group is Everyone", &types.MaaSAudience{Groups: []string{"system:authenticated"}}, serving.MaaSAudience{}, "Everyone signed in"},
		// MaaS matches names exactly, as the identity provider sends them:
		// OIDC groups and user names are not OpenShift object names.
		{"identity-provider names as they are", &types.MaaSAudience{Groups: []string{"/admins", "oidc:ml", "a%b", "..", "~"}, Users: []string{"alice:bob", "https://idp.example.com#alice", "system:serviceaccount:ci"}},
			serving.MaaSAudience{Groups: []string{"/admins", "oidc:ml", "a%b", "..", "~"}, Users: []string{"alice:bob", "https://idp.example.com#alice", "system:serviceaccount:ci"}}, ""},
		{"a control character in a group", &types.MaaSAudience{Groups: []string{"a\tb"}}, serving.MaaSAudience{}, "control"},
		{"a control character in a user", &types.MaaSAudience{Users: []string{"a\x00b"}}, serving.MaaSAudience{}, "control"},
		{"a long group name", &types.MaaSAudience{Groups: []string{strings.Repeat("g", 254)}}, serving.MaaSAudience{}, "253"},
		{"a long user name", &types.MaaSAudience{Users: []string{strings.Repeat("u", 254)}}, serving.MaaSAudience{}, "253"},
		{"too many groups", &types.MaaSAudience{Groups: strings.Split("a b c d e f g h i j k l m n o p q r s t u", " ")}, serving.MaaSAudience{}, "at most 20"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, msg := serving.ParseMaaSAudience(tc.in)
			if tc.err != "" {
				if !strings.Contains(msg, tc.err) {
					t.Fatalf("message = %q, want it to contain %q", msg, tc.err)
				}
				return
			}
			if msg != "" || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %+v %q, want %+v", got, msg, tc.want)
			}
		})
	}
}

func TestMaaSAudience_OwnerAndText(t *testing.T) {
	everyone := serving.MaaSAudience{Everyone: true}
	if got := everyone.Owner(); !reflect.DeepEqual(got, map[string]any{"groups": []any{map[string]any{"name": "system:authenticated"}}, "users": []any{}}) {
		t.Errorf("everyone's owner = %v (the docs' sample: system:authenticated)", got)
	}
	back := serving.MaaSAudienceFromOwner(everyone.Owner())
	if !back.Equal(everyone) {
		t.Errorf("read back as %+v", back)
	}
	some := serving.MaaSAudience{Groups: []string{"a", "b"}, Users: []string{"system:serviceaccount:ci:runner"}}
	if !serving.MaaSAudienceFromOwner(some.Owner()).Equal(some) {
		t.Error("groups and users do not read back")
	}
	for _, tc := range []struct {
		a    serving.MaaSAudience
		want string
	}{
		{everyone, "every signed-in user"},
		{serving.MaaSAudience{Groups: []string{"data-science"}}, "group data-science"},
		{serving.MaaSAudience{Groups: []string{"a", "b"}, Users: []string{"x"}}, "groups a, b and user x"},
		{serving.MaaSAudience{Users: []string{"x", "y"}}, "users x, y"},
		{serving.MaaSAudience{Groups: []string{"a", "b", "c", "d"}, Users: []string{"x"}}, "groups a, b, c and 2 more"},
	} {
		if got := tc.a.Text(); got != tc.want {
			t.Errorf("%+v: %q, want %q", tc.a, got, tc.want)
		}
	}
}

func TestMaaSSubscriptionDescription(t *testing.T) {
	free := serving.MaaSLimit{Rates: []serving.MaaSRate{{Limit: 100, Window: "1m"}}}
	pubs := []serving.MaaSPub{{Project: "proj", Model: "qwen25-05b-instruct"}, {Project: "proj", Model: "smollm2-135m-instruct"}}
	everyone := serving.MaaSAudience{Everyone: true}
	if got := serving.MaaSSubscriptionDescription(pubs, map[string]serving.MaaSLimit{"proj/qwen25-05b-instruct": free, "proj/smollm2-135m-instruct": free}, everyone); got != "100 tokens a minute for every signed-in user" {
		t.Errorf("one limit: %q (the text the dashboard's API-key dialog showed before)", got)
	}
	mixed := serving.MaaSSubscriptionDescription(pubs, map[string]serving.MaaSLimit{"proj/qwen25-05b-instruct": free, "proj/smollm2-135m-instruct": {Unlimited: true}}, serving.MaaSAudience{Groups: []string{"ml"}})
	for _, want := range []string{"Qwen2.5 0.5B Instruct (CPU): 100 tokens a minute", "SmolLM2 135M Instruct (CPU): unlimited (usage still metered)", "for group ml"} {
		if !strings.Contains(mixed, want) {
			t.Errorf("mixed: %q lacks %q", mixed, want)
		}
	}
	twoProjects := serving.MaaSSubscriptionDescription([]serving.MaaSPub{{Project: "a", Model: "qwen25-05b-instruct"}, {Project: "b", Model: "qwen25-05b-instruct"}}, map[string]serving.MaaSLimit{"a/qwen25-05b-instruct": free, "b/qwen25-05b-instruct": {Unlimited: true}}, everyone)
	if !strings.Contains(twoProjects, "Qwen2.5 0.5B Instruct (CPU) in a: 100 tokens a minute") || !strings.Contains(twoProjects, "Qwen2.5 0.5B Instruct (CPU) in b: unlimited") {
		t.Errorf("the same model in two projects: %q", twoProjects)
	}
}

// The same names in another order are the same audience: MaaS matches
// names, not positions. Publish of a second model with the audience in
// another order is then not a different audience, and Change who can use
// them has nothing to change.
func TestMaaSAudience_EqualIgnoresOrder(t *testing.T) {
	a := serving.MaaSAudience{Groups: []string{"a", "b"}, Users: []string{"x", "y"}}
	b := serving.MaaSAudience{Groups: []string{"b", "a"}, Users: []string{"y", "x"}}
	if !a.Equal(b) || !b.Equal(a) {
		t.Error("order changes the audience")
	}
	if a.Groups[0] != "a" || b.Groups[0] != "b" {
		t.Error("equal sorted the audience it compared")
	}
	for _, o := range []serving.MaaSAudience{{Groups: []string{"a"}, Users: []string{"x", "y"}}, {Groups: []string{"a", "b"}, Users: []string{"x"}}, {Everyone: true}, {Groups: []string{"a", "b", "x", "y"}}} {
		if a.Equal(o) {
			t.Errorf("%+v equals %+v", a, o)
		}
	}
}

// A group without an OpenShift Group is a warning with the commands that
// make it; a name that cannot be an OpenShift Group's is not looked up and
// has no commands; one that cannot be read says so.
func TestMaaSGroupProblems(t *testing.T) {
	f, c := newResourceFake(t)
	f.putJSON("/apis/user.openshift.io/v1/groups/ml-team", `{}`)
	f.fail["GET /apis/user.openshift.io/v1/groups/secret-team"] = 403
	got := serving.MaaSGroupProblems(c, []string{"ml-team", "nobody-yet", "/admins", "oidc:ml", "secret-team"})
	if len(got) != 4 {
		t.Fatalf("problems %+v", got)
	}
	want := []types.MaaSGroupProblem{
		{Name: "nobody-yet", Missing: true, Message: "nobody-yet is not a group on this cluster. Nobody is in it unless your identity provider adds it at sign-in.", Commands: "oc adm groups new nobody-yet\noc adm groups add-users nobody-yet <user>"},
		{Name: "/admins", Missing: true, Message: "/admins is not a group on this cluster. Nobody is in it unless your identity provider adds it at sign-in."},
		{Name: "oidc:ml", Missing: true, Message: "oidc:ml is not a group on this cluster. Nobody is in it unless your identity provider adds it at sign-in."},
	}
	if !reflect.DeepEqual(got[:3], want) {
		t.Errorf("got %+v\nwant %+v", got[:3], want)
	}
	if got[3].Name != "secret-team" || got[3].Missing || !strings.Contains(got[3].Message, "Cannot check whether group secret-team exists") {
		t.Errorf("unreadable: %+v", got[3])
	}
	for _, r := range f.requests {
		if strings.Contains(r, "admins") || strings.Contains(r, "oidc") {
			t.Errorf("looked up a name that cannot be an OpenShift Group: %s", r)
		}
	}
	if _, problem := CheckMaaSGroups(c, []string{"system:authenticated"}); !strings.Contains(problem, "Everyone signed in") {
		t.Errorf("dialog check of system:authenticated: %q", problem)
	}
	if out, problem := CheckMaaSGroups(c, []string{"ml-team"}); problem != "" || out == nil || len(out) != 0 {
		t.Errorf("dialog check of an existing group: %+v %q", out, problem)
	}
}
