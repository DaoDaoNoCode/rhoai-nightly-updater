package cluster

import (
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const (
	tDashboardConfig = "/apis/opendatahub.io/v1alpha/namespaces/redhat-ods-applications/odhdashboardconfigs/odh-dashboard-config"
	tAppsProfiles    = "/apis/infrastructure.opendatahub.io/v1/namespaces/redhat-ods-applications/hardwareprofiles"
)

// kueueRecipeSpec is a recipe with a workbench, a model and kueue (its
// JSON object) in project recipe-dev (mode create, or existing).
func kueueRecipeSpec(mode, kueue string) string {
	features := `"workbench":{"enabled":true},"models":{"enabled":true,"exposure":"internal"}`
	if kueue != "" {
		features += `,"kueue":` + kueue
	}
	return `{"rhoai":{"build":"latest","ifInstalled":"keep"},"dataScienceCluster":{"create":true},"project":{"mode":"` + mode + `","name":"recipe-dev"},"features":{` + features + `}}`
}

// planOrder is the plan's step IDs in order.
func planOrder(p *types.RecipePlan) string {
	var out []string
	for _, s := range p.Steps {
		out = append(out, s.ID)
	}
	return strings.Join(out, " ")
}

// planReason is the plan's reason for step id ("" when none).
func planReason(p *types.RecipePlan, id string) string {
	for _, s := range p.Steps {
		if s.ID == id {
			return s.Action + ": " + s.Reason
		}
	}
	return ""
}

// The run order follows the dashboard's Kueue rule (KueueFirst), decided
// from the cluster at plan time; the plan refuses up front a workbench or
// model the dashboard would offer no usable hardware profile (read-only).
func TestRecipePlan_KueueOrderFollowsTheDashboardRule(t *testing.T) {
	const (
		first     = "rhoai dsc project prerequisites kueue-prereq kueue model workbench"
		unchanged = "rhoai dsc project prerequisites model workbench kueue-prereq kueue"
		queueBig  = `{"metadata":{"name":"admin-queue","resourceVersion":"7"},"spec":{"identifiers":[
			{"identifier":"cpu","displayName":"CPU","resourceType":"CPU","defaultCount":1,"minCount":1,"maxCount":8},
			{"identifier":"memory","displayName":"Memory","resourceType":"Memory","defaultCount":"2Gi","minCount":"1Gi","maxCount":"32Gi"}],
			"scheduling":{"type":"Queue","kueue":{"localQueueName":"nightly-kueue"}}}}`
	)
	for _, tc := range []struct {
		name, mode, kueue string
		settingOn         bool
		labelled          bool
		kueueOn           bool // the DataScienceCluster's kueue Unmanaged
		profiles          []string
		order             string
		blocked           string // in both the workbench's and the model's reason ("" none)
	}{
		{name: "opt-in, the setting off: Kueue first, the opt-in makes the profile", mode: "create", kueue: `{"enabled":true,"dashboardOptIn":true}`, order: first},
		{name: "opt-in, the setting on", mode: "create", kueue: `{"enabled":true,"dashboardOptIn":true}`, settingOn: true, order: first},
		{name: "the setting on without the opt-in, no Queue profile", mode: "create", kueue: `{"enabled":true}`, settingOn: true, order: first, blocked: "With spec.features.kueue.dashboardOptIn the run also makes the hardware profile Kueue CPU (RHOAI Nightly Updater)"},
		{name: "the setting on without the opt-in, a Queue profile for nightly-kueue", mode: "create", kueue: `{"enabled":true}`, settingOn: true, profiles: []string{queueBig}, order: first},
		{name: "the setting off, no opt-in: unchanged", mode: "create", kueue: `{"enabled":true}`, order: unchanged},
		{name: "the setting on, no distributed workloads: no LocalQueue", mode: "create", settingOn: true, kueueOn: true, order: "rhoai dsc project prerequisites model workbench", blocked: "has no LocalQueue"},
		{name: "the setting on, no distributed workloads, Kueue off: no profiles", mode: "create", settingOn: true, order: "rhoai dsc project prerequisites model workbench", blocked: "Turn the dashboard's Kueue setting on"},
		{name: "a labelled project, the setting off: no profiles", mode: "existing", kueue: `{"enabled":true}`, labelled: true, order: unchanged, blocked: "while Kueue is off in the dashboard"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := projectFake(t)
			installedRHOAI(f)
			if tc.kueueOn {
				f.putJSON(tRecipeDSC, `{"spec":{"components":{"kueue":{"managementState":"Unmanaged"}}},"status":{"conditions":[{"type":"Ready","status":"True"}]}}`)
			}
			if tc.settingOn {
				f.putJSON(tDashboardConfig, `{"spec":{"dashboardConfig":{"disableKueue":false}}}`)
			}
			if tc.mode == "existing" {
				labels := `"opendatahub.io/dashboard":"true"`
				if tc.labelled {
					labels += `,"kueue.openshift.io/managed":"true"`
				}
				f.putJSON(tRecipeNS, `{"metadata":{"name":"recipe-dev","uid":"ns-1","labels":{`+labels+`}}}`)
			}
			for i, p := range tc.profiles {
				f.putJSON(tAppsProfiles+"/p"+string(rune('0'+i)), p)
			}
			plan, state := planRecipeFor(t, c.SignedInUser(), kueueRecipeSpec(tc.mode, tc.kueue))
			if got := planOrder(plan); got != tc.order {
				t.Fatalf("order\n got %s\nwant %s", got, tc.order)
			}
			if state.KueueFirst != (tc.order == first) {
				t.Errorf("state KueueFirst %v", state.KueueFirst)
			}
			for _, id := range []string{"workbench", "model"} {
				got := planReason(plan, id)
				if tc.blocked == "" && strings.HasPrefix(got, "blocked") || tc.blocked != "" && (!strings.HasPrefix(got, "blocked") || !strings.Contains(got, tc.blocked)) {
					t.Errorf("%s: %s", id, got)
				}
				if tc.blocked != "" && !strings.Contains(problemText(plan), got[len("blocked: "):]) {
					t.Errorf("%s not in the problems: %s", id, problemText(plan))
				}
			}
			if m := f.mutations(); len(m) != 0 {
				t.Fatalf("the plan changed %v", m)
			}
		})
	}
}

// A dashboard setting that cannot be read refuses the plan (fail closed):
// it decides the order and the profiles.
func TestRecipePlan_KueueSettingUnreadableRefuses(t *testing.T) {
	f, c := projectFake(t)
	installedRHOAI(f)
	f.fail["GET "+tDashboardConfig] = 403
	plan, _ := planRecipeFor(t, c.SignedInUser(), kueueRecipeSpec("create", `{"enabled":true}`))
	if !strings.Contains(problemText(plan), "Cannot read the dashboard's Kueue setting") {
		t.Fatalf("problems %s", problemText(plan))
	}
}
