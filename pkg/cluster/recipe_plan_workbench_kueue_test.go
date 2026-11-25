package cluster

import (
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
)

// N12 follow-up, end to end on the fake API (the real project check, the
// Workbench tab's status and the profile reads): a resumed run after an
// interrupted project setup (the project created and recorded as the
// recipe's, its labels not set yet). With the dashboard's Kueue setting on
// the project step labels it kueue.openshift.io/managed=true, and the
// project has no LocalQueue, so the review refuses the workbench before
// anything runs; with the setting off it is checked when the run gets there.
func TestRecipePlan_ResumeAfterAnInterruptedProjectSetup(t *testing.T) {
	for _, settingOn := range []bool{true, false} {
		f, c := projectFake(t)
		installedRHOAI(f)
		f.putJSON(tRecipeDSC, `{"spec":{"components":{"workbenches":{"managementState":"Managed"},"kueue":{"managementState":"Unmanaged"}}},`+
			`"status":{"conditions":[{"type":"Ready","status":"True"}],"components":{"workbenches":{"managementState":"Managed"},"kueue":{"managementState":"Unmanaged"}}}}`)
		disable := "true"
		if settingOn {
			disable = "false"
		}
		f.putJSON(kqConfig, `{"spec":{"dashboardConfig":{"disableKueue":`+disable+`}}}`)
		f.putJSON(wbProfiles+"default-profile", `{"metadata":{"resourceVersion":"9"},"spec":{`+cpuMem+`}}`)
		f.putJSON(tRecipeNS, `{"metadata":{"name":"recipe-dev","uid":"rec-uid"}}`)
		if err := record.WriteSnapshotKey(c, "recipeProject", "test", `{"projects":[{"name":"recipe-dev","uid":"rec-uid","recipe":"dev"}]}`); err != nil {
			t.Fatal(err)
		}
		u := userClientOf(c, fakeUserToken)
		plan, _ := planRecipeFor(t, u, `{"rhoai":{"build":"latest","ifInstalled":"keep"},"dataScienceCluster":{"create":true},"project":{"mode":"create","name":"recipe-dev"},"features":{"workbench":{"enabled":true}}}`)
		actions, problems := planActions(plan), problemText(plan)
		if settingOn {
			if !strings.Contains(actions, "project=run") || !strings.Contains(actions, "workbench=blocked") ||
				!strings.Contains(problems, "the project step labels project recipe-dev kueue.openshift.io/managed=true") || !strings.Contains(problems, "no enabled workbench hardware profile") {
				t.Errorf("the setting on: actions %s, problems %s", actions, problems)
			}
		} else if !strings.Contains(actions, "project=run") || !strings.Contains(actions, "workbench=later") || problems != "" {
			t.Errorf("the setting off: actions %s, problems %s", actions, problems)
		}
		if m := f.mutations(); len(m) != 0 {
			t.Errorf("the plan changed %v", m)
		}
	}
}
