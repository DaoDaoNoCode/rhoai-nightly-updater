package cluster

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kueue"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/serving"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/workbench"

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
		{name: "a labelled project, the setting off: no profiles", mode: "existing", kueue: `{"enabled":true}`, labelled: true, order: unchanged, blocked: "offers no hardware profile"},
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
				putWorkbenchImage(f)
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

// DataScienceCluster kueue.autoCreateQueues on (K4): RHOAI would create a
// LocalQueue named default in the labelled project, which the Kueue step
// refuses; the plan refuses it up front. Another defaultLocalQueueName
// does not collide.
func TestRecipePlan_KueueAutoCreateQueuesRefuses(t *testing.T) {
	for _, tc := range []struct {
		name, kueue string
		refused     bool
	}{
		{"autoCreateQueues on", `"autoCreateQueues":true`, true},
		{"the tool's LocalQueue name", `"autoCreateQueues":true,"defaultLocalQueueName":"nightly-kueue"`, true},
		{"another queue name", `"autoCreateQueues":true,"defaultLocalQueueName":"team"`, false},
		{"off (the default)", `"autoCreateQueues":false`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := projectFake(t)
			installedRHOAI(f)
			f.putJSON(tRecipeDSC, `{"spec":{"components":{"kueue":{"managementState":"Unmanaged",`+tc.kueue+`}}},"status":{"conditions":[{"type":"Ready","status":"True"}]}}`)
			plan, _ := planRecipeFor(t, c.SignedInUser(), `{"rhoai":{"build":"latest","ifInstalled":"keep"},"dataScienceCluster":{"create":true},"project":{"mode":"create","name":"recipe-dev"},"features":{"kueue":{"enabled":true}}}`)
			got := planReason(plan, "kueue")
			if refused := strings.HasPrefix(got, "blocked") && strings.Contains(got, "kueue.autoCreateQueues: true"); refused != tc.refused {
				t.Fatalf("kueue step: %s", got)
			}
			if tc.refused && !strings.Contains(problemText(plan), "kueue.autoCreateQueues: true") {
				t.Errorf("problems %s", problemText(plan))
			}
		})
	}
}

// putWorkbenchImage gives the dashboard a workbench image, so the
// Workbench tab's preflight finds one.
func putWorkbenchImage(f *resourceFake) {
	f.putJSON(wbStreams+"s2i-minimal-notebook", `{"metadata":{"labels":{"opendatahub.io/notebook-image":"true"},"annotations":{"opendatahub.io/notebook-image-name":"Jupyter | Minimal | CPU | Python 3.12"}},
		"spec":{"tags":[{"name":"2025.2","annotations":{"opendatahub.io/default-image":"true"}}]},"status":{"dockerImageRepository":"reg/minimal","tags":[{"tag":"2025.2"}]}}`)
}

// A fresh cluster: the RHOAI step will install the dashboard and the
// HardwareProfile API, so the workloads' profiles are checked when the run
// gets there; the plan does not refuse the recipe that installs them.
func TestRecipePlan_KueueFreshClusterDefersTheProfileCheck(t *testing.T) {
	f, c := projectFake(t)
	plan, state := planRecipeFor(t, c.SignedInUser(), `{"rhoai":{"build":{"image":"`+tRecipeFBC+`"}},"dataScienceCluster":{"create":true},"project":{"mode":"create","name":"recipe-dev"},
"features":{"workbench":{"enabled":true},"models":{"enabled":true,"exposure":"internal"},"kueue":{"enabled":true,"dashboardOptIn":true}}}`)
	if !state.KueueFirst || strings.Contains(problemText(plan), "hardware profile") || strings.Contains(problemText(plan), "Cannot check which") {
		t.Fatalf("kueue first %v, problems %s", state.KueueFirst, problemText(plan))
	}
	for _, id := range []string{"workbench", "model"} {
		if got := planReason(plan, id); !strings.HasPrefix(got, "later") {
			t.Errorf("%s: %s", id, got)
		}
	}
	if m := f.mutations(); len(m) != 0 {
		t.Fatalf("the plan changed %v", m)
	}
}

// The Workbench tab's preflight for a project that is there: no workbench
// image refuses a recipe without Kueue, as the tab's Set up would. With
// Kueue first, the workbench waits for the Kueue steps (they make its
// LocalQueue and Queue profile; internal/recipe TestDependsOn_WorkbenchWaitsForKueueFirst).
func TestRecipePlan_WorkbenchPreflight(t *testing.T) {
	f, c := projectFake(t)
	installedRHOAI(f)
	f.putJSON(tRecipeNS, `{"metadata":{"name":"recipe-dev","uid":"ns-1","labels":{"opendatahub.io/dashboard":"true"}}}`)
	plan, _ := planRecipeFor(t, c.SignedInUser(), `{"rhoai":{"build":"latest","ifInstalled":"keep"},"dataScienceCluster":{"create":true},"project":{"mode":"existing","name":"recipe-dev"},"features":{"workbench":{"enabled":true}}}`)
	if got := planReason(plan, "workbench"); !strings.HasPrefix(got, "blocked") || !strings.Contains(got, "offers no workbench image") {
		t.Fatalf("workbench: %s", got)
	}
}

// A recipe project deleted and made again (a project Clean up) with the
// dashboard's setting still on and the tool's Queue profile from the first
// run: Kueue first, nothing refused; when the run gets there, the workbench
// and the model both choose the tool's Queue profile for nightly-kueue (the
// steps' own choosers, on the profile as this version makes it).
func TestRecipe_ProjectMadeAgainWithTheSettingOn(t *testing.T) {
	f, c := projectFake(t)
	installedRHOAI(f)
	f.putJSON(tDashboardConfig, `{"spec":{"dashboardConfig":{"disableKueue":false}}}`)
	profile, _ := json.Marshal(kueue.ProfileManifest("redhat-ods-applications", "now"))
	f.putJSON(tAppsProfiles+"/"+kueue.ProfileName, string(profile))
	for _, kueueSpec := range []string{`{"enabled":true,"dashboardOptIn":true}`, `{"enabled":true}`} {
		plan, state := planRecipeFor(t, c.SignedInUser(), kueueRecipeSpec("create", kueueSpec))
		if !state.KueueFirst || !strings.HasPrefix(planOrder(plan), "rhoai dsc project prerequisites kueue-prereq kueue") {
			t.Fatalf("%s: kueue first %v, order %s", kueueSpec, state.KueueFirst, planOrder(plan))
		}
		for _, id := range []string{"workbench", "model"} {
			if got := planReason(plan, id); strings.HasPrefix(got, "blocked") || strings.Contains(problemText(plan), "hardware profile") {
				t.Errorf("%s: %s: %s", kueueSpec, id, got)
			}
		}
	}
	// When the run gets there: the project labelled, LocalQueue nightly-kueue.
	queues := []string{kueue.LocalQueueName}
	var wp workbench.HardwareProfile
	var mp serving.HardwareProfileObject
	if json.Unmarshal(profile, &wp) != nil || json.Unmarshal(profile, &mp) != nil {
		t.Fatal("profile")
	}
	if got, why := workbench.ChooseProfile([]workbench.HardwareProfile{wp}, nil, envkit.OnlyKueue, queues); why != "" || got.Name != kueue.ProfileName || got.Queue != kueue.LocalQueueName {
		t.Fatalf("workbench profile %+v %s", got, why)
	}
	for _, e := range serving.ModelCatalog {
		if got, why := serving.ChooseModelProfile([]serving.HardwareProfileObject{mp}, envkit.OnlyKueue, queues, e); why != "" || got.Metadata.Name != kueue.ProfileName {
			t.Fatalf("model %s profile %+v %s", e.Name, got, why)
		}
	}
}
