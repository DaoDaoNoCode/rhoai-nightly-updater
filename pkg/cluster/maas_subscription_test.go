package cluster

import (
	"reflect"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

func TestParseMaaSLimit(t *testing.T) {
	rate := func(l int64, w string) types.MaaSTokenRate { return types.MaaSTokenRate{Limit: l, Window: w} }
	for _, tc := range []struct {
		name string
		in   *types.MaaSLimit
		want maasLimit
		err  string
	}{
		{"none is the free sample, as before", nil, maasLimit{rates: []maasRate{{100, "1m"}}}, ""},
		{"free", &types.MaaSLimit{Preset: "free"}, maasLimit{rates: []maasRate{{100, "1m"}}}, ""},
		{"premium", &types.MaaSLimit{Preset: "premium"}, maasLimit{rates: []maasRate{{100000, "24h"}}}, ""},
		{"unlimited", &types.MaaSLimit{Preset: "unlimited"}, maasLimit{unlimited: true}, ""},
		{"custom", &types.MaaSLimit{Preset: "custom", Rates: []types.MaaSTokenRate{rate(500, "1m"), rate(20000, "1h")}}, maasLimit{rates: []maasRate{{500, "1m"}, {20000, "1h"}}}, ""},
		{"custom at the CRD's bounds", &types.MaaSLimit{Preset: "custom", Rates: []types.MaaSTokenRate{rate(1, "9999h"), rate(1000000000, "1s")}}, maasLimit{rates: []maasRate{{1, "9999h"}, {1000000000, "1s"}}}, ""},
		{"a preset takes no rates", &types.MaaSLimit{Preset: "free", Rates: []types.MaaSTokenRate{rate(5, "1m")}}, maasLimit{}, "takes no"},
		{"a preset is not unlimited", &types.MaaSLimit{Preset: "premium", Unlimited: true}, maasLimit{}, "takes no"},
		{"custom needs a rate", &types.MaaSLimit{Preset: "custom"}, maasLimit{}, "at least one"},
		{"custom is never unlimited", &types.MaaSLimit{Preset: "custom", Unlimited: true, Rates: []types.MaaSTokenRate{rate(5, "1m")}}, maasLimit{}, "Unlimited"},
		{"zero tokens", &types.MaaSLimit{Preset: "custom", Rates: []types.MaaSTokenRate{rate(0, "1m")}}, maasLimit{}, "1 to 1,000,000,000"},
		{"more than the CRD allows (no fake unlimited)", &types.MaaSLimit{Preset: "custom", Rates: []types.MaaSTokenRate{rate(1000000001, "1m")}}, maasLimit{}, "1 to 1,000,000,000"},
		{"days are not a unit", &types.MaaSLimit{Preset: "custom", Rates: []types.MaaSTokenRate{rate(5, "1d")}}, maasLimit{}, "window"},
		{"no leading zero", &types.MaaSLimit{Preset: "custom", Rates: []types.MaaSTokenRate{rate(5, "0m")}}, maasLimit{}, "window"},
		{"at most four digits", &types.MaaSLimit{Preset: "custom", Rates: []types.MaaSTokenRate{rate(5, "10000s")}}, maasLimit{}, "window"},
		{"the same window twice", &types.MaaSLimit{Preset: "custom", Rates: []types.MaaSTokenRate{rate(5, "1m"), rate(9, "1m")}}, maasLimit{}, "twice"},
		{"too many rates", &types.MaaSLimit{Preset: "custom", Rates: []types.MaaSTokenRate{rate(1, "1s"), rate(1, "2s"), rate(1, "3s"), rate(1, "4s"), rate(1, "5s"), rate(1, "6s")}}, maasLimit{}, "at most 5"},
		{"unknown preset", &types.MaaSLimit{Preset: "gold"}, maasLimit{}, "preset"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, msg := parseMaaSLimit(tc.in)
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
		l             maasLimit
		preset, text  string
		unlimitedJSON bool
	}{
		{maasLimit{rates: []maasRate{{100, "1m"}}}, "free", "100 tokens a minute", false},
		{maasLimit{rates: []maasRate{{100000, "24h"}}}, "premium", "100,000 tokens a day", false},
		{maasLimit{unlimited: true}, "unlimited", "unlimited (usage still metered)", true},
		{maasLimit{rates: []maasRate{{500, "1m"}, {20000, "1h"}}}, "custom", "500 tokens a minute and 20,000 tokens an hour", false},
		{maasLimit{rates: []maasRate{{7, "30s"}, {1, "2h"}, {3, "1s"}}}, "custom", "7 tokens every 30 seconds, 1 token every 2 hours and 3 tokens a second", false},
	} {
		if got := tc.l.preset(); got != tc.preset {
			t.Errorf("%+v: preset %q, want %q", tc.l, got, tc.preset)
		}
		if got := tc.l.text(); got != tc.text {
			t.Errorf("%+v: text %q, want %q", tc.l, got, tc.text)
		}
		ref := map[string]interface{}{"name": "m", "namespace": "p"}
		tc.l.addTo(ref)
		_, hasRates := ref["tokenRateLimits"]
		if ref["unlimited"] == true != tc.unlimitedJSON || hasRates == tc.unlimitedJSON {
			t.Errorf("%+v: model reference %v: exactly one of unlimited and tokenRateLimits (the CRD's rule)", tc.l, ref)
		}
		back, ok := maasLimitFromModelRef(ref)
		if !ok || !reflect.DeepEqual(back, tc.l) {
			t.Errorf("%+v: read back as %+v %v", tc.l, back, ok)
		}
	}
	if _, ok := maasLimitFromModelRef(map[string]interface{}{"name": "m"}); ok {
		t.Error("a model reference with neither is not a limit")
	}
}

func TestParseMaaSAudience(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   *types.MaaSAudience
		want maasAudience
		err  string
	}{
		{"none is everyone, as before", nil, maasAudience{everyone: true}, ""},
		{"everyone", &types.MaaSAudience{Everyone: true}, maasAudience{everyone: true}, ""},
		{"groups and users, trimmed, without duplicates", &types.MaaSAudience{Groups: []string{" data-science ", "ml team", "data-science"}, Users: []string{"system:serviceaccount:ci:runner", "alice@example.com", "kube:admin", "b64:YWxpY2U6Ym9i"}},
			maasAudience{groups: []string{"data-science", "ml team"}, users: []string{"system:serviceaccount:ci:runner", "alice@example.com", "kube:admin", "b64:YWxpY2U6Ym9i"}}, ""},
		{"everyone names nobody else", &types.MaaSAudience{Everyone: true, Groups: []string{"a"}}, maasAudience{}, "Everyone"},
		{"specific needs a name", &types.MaaSAudience{Groups: []string{" "}}, maasAudience{}, "at least one"},
		{"everyone's group is Everyone", &types.MaaSAudience{Groups: []string{"system:authenticated"}}, maasAudience{}, "Everyone signed in"},
		{"a group name with a colon", &types.MaaSAudience{Groups: []string{"system:masters"}}, maasAudience{}, `":"`},
		{"a group name with a slash", &types.MaaSAudience{Groups: []string{"a/b"}}, maasAudience{}, `"/"`},
		{"a group name with a percent", &types.MaaSAudience{Groups: []string{"a%b"}}, maasAudience{}, `"%"`},
		{"a group named ..", &types.MaaSAudience{Groups: []string{".."}}, maasAudience{}, "not a valid"},
		{"a group named ~", &types.MaaSAudience{Groups: []string{"~"}}, maasAudience{}, "not a valid"},
		{"a control character", &types.MaaSAudience{Groups: []string{"a\tb"}}, maasAudience{}, "control"},
		{"a long group name", &types.MaaSAudience{Groups: []string{strings.Repeat("g", 254)}}, maasAudience{}, "253"},
		{"a user name with a colon", &types.MaaSAudience{Users: []string{"alice:bob"}}, maasAudience{}, "b64:"},
		{"a malformed service account", &types.MaaSAudience{Users: []string{"system:serviceaccount:Bad_NS:runner"}}, maasAudience{}, "service account"},
		{"a service account without a name", &types.MaaSAudience{Users: []string{"system:serviceaccount:ci"}}, maasAudience{}, "service account"},
		{"too many groups", &types.MaaSAudience{Groups: strings.Split("a b c d e f g h i j k l m n o p q r s t u", " ")}, maasAudience{}, "at most 20"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, msg := parseMaaSAudience(tc.in)
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
	everyone := maasAudience{everyone: true}
	if got := everyone.owner(); !reflect.DeepEqual(got, map[string]interface{}{"groups": []interface{}{map[string]interface{}{"name": "system:authenticated"}}, "users": []interface{}{}}) {
		t.Errorf("everyone's owner = %v (the docs' sample: system:authenticated)", got)
	}
	back := maasAudienceFromOwner(everyone.owner())
	if !back.equal(everyone) {
		t.Errorf("read back as %+v", back)
	}
	some := maasAudience{groups: []string{"a", "b"}, users: []string{"system:serviceaccount:ci:runner"}}
	if !maasAudienceFromOwner(some.owner()).equal(some) {
		t.Error("groups and users do not read back")
	}
	for _, tc := range []struct {
		a    maasAudience
		want string
	}{
		{everyone, "every signed-in user"},
		{maasAudience{groups: []string{"data-science"}}, "group data-science"},
		{maasAudience{groups: []string{"a", "b"}, users: []string{"x"}}, "groups a, b and user x"},
		{maasAudience{users: []string{"x", "y"}}, "users x, y"},
		{maasAudience{groups: []string{"a", "b", "c", "d"}, users: []string{"x"}}, "groups a, b, c and 2 more"},
	} {
		if got := tc.a.text(); got != tc.want {
			t.Errorf("%+v: %q, want %q", tc.a, got, tc.want)
		}
	}
}

func TestMaaSSubscriptionDescription(t *testing.T) {
	free := maasLimit{rates: []maasRate{{100, "1m"}}}
	pubs := []maasPub{{"proj", "qwen25-05b-instruct"}, {"proj", "smollm2-135m-instruct"}}
	everyone := maasAudience{everyone: true}
	if got := maasSubscriptionDescription(pubs, map[string]maasLimit{"proj/qwen25-05b-instruct": free, "proj/smollm2-135m-instruct": free}, everyone); got != "100 tokens a minute for every signed-in user" {
		t.Errorf("one limit: %q (the text the dashboard's API-key dialog showed before)", got)
	}
	mixed := maasSubscriptionDescription(pubs, map[string]maasLimit{"proj/qwen25-05b-instruct": free, "proj/smollm2-135m-instruct": {unlimited: true}}, maasAudience{groups: []string{"ml"}})
	for _, want := range []string{"Qwen2.5 0.5B Instruct (CPU): 100 tokens a minute", "SmolLM2 135M Instruct (CPU): unlimited (usage still metered)", "for group ml"} {
		if !strings.Contains(mixed, want) {
			t.Errorf("mixed: %q lacks %q", mixed, want)
		}
	}
	twoProjects := maasSubscriptionDescription([]maasPub{{"a", "qwen25-05b-instruct"}, {"b", "qwen25-05b-instruct"}}, map[string]maasLimit{"a/qwen25-05b-instruct": free, "b/qwen25-05b-instruct": {unlimited: true}}, everyone)
	if !strings.Contains(twoProjects, "Qwen2.5 0.5B Instruct (CPU) in a: 100 tokens a minute") || !strings.Contains(twoProjects, "Qwen2.5 0.5B Instruct (CPU) in b: unlimited") {
		t.Errorf("the same model in two projects: %q", twoProjects)
	}
}
