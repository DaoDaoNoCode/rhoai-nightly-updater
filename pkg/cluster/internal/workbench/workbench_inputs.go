package workbench

import (
	"fmt"
	"slices"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
)

// ProfileInputs are what a new workbench's hardware profile is chosen from
// (BuildPlan, ChooseProfile), read so that a caller can say which of them a
// step before the workbench changes: the hardware profiles of the
// applications namespace and of the project, the project's
// kueue.openshift.io/managed label and LocalQueues, the dashboard's Kueue
// setting, the DataScienceCluster's kueue component, and the LocalQueue
// RHOAI creates in every Kueue project when the DataScienceCluster's
// kueue.autoCreateQueues is on (AutoQueue, "" when off).
type ProfileInputs struct {
	Global, Local          []HardwareProfile
	ProjectFound, Labelled bool
	Queues                 []string
	SettingOn              bool
	KueueComponent         bool
	AutoQueue              string
}

// ReadProfileInputs reads ProfileInputs for project, acting as c (the
// signed-in user, as BuildPlan reads them). A project that is not there has
// no label, LocalQueue or profile of its own. A read that fails is an error.
func ReadProfileInputs(c *kube.Client, project string, dsc rhoai.DSCRead) (ProfileInputs, error) {
	var in ProfileInputs
	if err := userOnly(c); err != nil {
		return in, err
	}
	apps, err := rhoai.AppsNamespace(c)
	if err != nil {
		return in, fmt.Errorf("find RHOAI's applications namespace: %w", err)
	}
	cfg, err := readDashboardConfig(c, apps)
	if err != nil {
		return in, fmt.Errorf("read the dashboard's settings (OdhDashboardConfig): %w", err)
	}
	d := cfg.Spec.DashboardConfig.DisableKueue
	in.SettingOn = d != nil && !*d
	in.KueueComponent = envkit.ComponentOn(dsc, "kueue")
	in.AutoQueue = AutoQueue(dsc)
	if in.Global, err = listProfiles(c, apps); err != nil {
		return in, fmt.Errorf("read the hardware profiles: %w", err)
	}
	meta, found, err := kube.ReadObjectMeta(c, "/api/v1/namespaces/"+project)
	if err != nil {
		return in, fmt.Errorf("read project %s: %w", project, err)
	}
	if !found {
		return in, nil
	}
	in.ProjectFound, in.Labelled = true, meta.Labels[KueueProjectLabel] == "true"
	if in.Local, err = listProfiles(c, project); err != nil {
		return in, fmt.Errorf("read the project's hardware profiles: %w", err)
	}
	if in.Queues, err = listLocalQueues(c, project); err != nil {
		return in, fmt.Errorf("read the project's LocalQueues: %w", err)
	}
	return in, nil
}

// AutoQueue is the LocalQueue RHOAI creates in every namespace labelled
// kueue.openshift.io/managed=true when the DataScienceCluster's
// spec.components.kueue.autoCreateQueues is true: its
// defaultLocalQueueName, "default" when unset (the API server's default).
// "" when autoCreateQueues is off.
func AutoQueue(dsc rhoai.DSCRead) string {
	spec, _ := dsc.Object["spec"].(map[string]any)
	comps, _ := spec["components"].(map[string]any)
	k, _ := comps["kueue"].(map[string]any)
	if on, _ := k["autoCreateQueues"].(bool); !on {
		return ""
	}
	name, _ := k["defaultLocalQueueName"].(string)
	return kube.FirstNonEmpty(name, "default")
}

// Choose is ChooseProfile with in: the dashboard's Kueue rule from the
// label, the setting and the component, and the project's LocalQueues,
// with AutoQueue once the project is Kueue's.
func (in ProfileInputs) Choose() (ProfileChoice, string) {
	queues := in.Queues
	if in.Labelled && in.AutoQueue != "" && !slices.Contains(queues, in.AutoQueue) {
		queues = append(slices.Clone(queues), in.AutoQueue)
	}
	return ChooseProfile(in.Global, in.Local, ModeFor(in.Labelled, in.SettingOn && in.KueueComponent), queues)
}
