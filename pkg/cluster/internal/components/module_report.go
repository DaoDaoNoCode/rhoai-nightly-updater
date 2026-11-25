package components

import (
	"fmt"
	"sort"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
)

// What each component reports about itself, read from its own CR in
// components.platform.opendatahub.io (the kinds the API server lists, so no
// component is named here). RHOAI rolls the components up the same way: a
// component is ready when its CR's Ready condition is True, and one whose
// status is stale (observedGeneration < generation) or that has no Ready
// condition has not reported yet (rhods-operator rhoai-3.7-ea.1
// internal/controller/modules/modules_controller_actions.go,
// evaluateModulesStatus). RHOAI accepts a status.observedGeneration of 0;
// this does not, because the controllers' framework sets it only once the
// component is ready, and sets the generation on the ProvisioningSucceeded
// condition on every reconcile (odh-platform-utilities framework/v0.6.0,
// which rhods-operator 3.7-ea.1 uses: controller/reconciler/reconciler.go):
// a report counts once the status or its Ready or ProvisioningSucceeded
// condition says it is for the current generation. A component is ready
// only when Ready is True and ProvisioningSucceeded, when present, is True
// too. ProvisioningSucceeded=False is the operator's own
// verdict that it could not set the component up (a dependency it checks
// for, a manifest it could not apply), while Ready=False with provisioning
// succeeded is a component still rolling out (status.go
// ConditionDeploymentsNotAvailableReason): the first are listed first.
// Conditions are passed on as RHOAI wrote them; their text is not read.

// ModuleCondition is one component's own condition that is not True.
type ModuleCondition struct {
	Source string // "<Kind> <name>"
	Cond   rhoai.DSCCondition
	// ProvisioningFailed: the condition is ProvisioningSucceeded=False.
	ProvisioningFailed bool
}

// Line is "<Kind> <name>: <Type>=<Status> (<Reason>): <message>", the
// message in full.
func (m ModuleCondition) Line() string {
	line := fmt.Sprintf("%s: %s=%s", m.Source, m.Cond.Type, m.Cond.Status)
	if m.Cond.Reason != "" {
		line += fmt.Sprintf(" (%s)", m.Cond.Reason)
	}
	if m.Cond.Message != "" {
		line += ": " + m.Cond.Message
	}
	return line
}

// ModuleReport is what the component CRs report about themselves.
type ModuleReport struct {
	// NotReady: provisioning failures first, then the rest, each sorted
	// by Source.
	NotReady []ModuleCondition
	// NotReported names the CRs ("<Kind> <name>", sorted) with no
	// condition for their current generation yet.
	NotReported []string
	Ready       int
	Total       int
	// Served: the API server serves the component group (RHOAI installs
	// its CRDs with the operator, before any DataScienceCluster).
	Served bool
}

// Complete: every component CR has reported, and there is one; or the
// group is not served, so there is nothing to wait for.
func (r ModuleReport) Complete() bool {
	return !r.Served || (r.Total > 0 && len(r.NotReported) == 0)
}

// ReadModuleReport reads every component CR the API serves and sorts it
// into ready, not ready (with its own condition) and not reported yet.
func ReadModuleReport(c *kube.Client) (ModuleReport, error) {
	api, err := rhoai.DiscoverComponentAPI(c)
	if err != nil {
		return ModuleReport{}, err
	}
	comps := rhoai.ComponentAPI{Groups: map[string]string{}}
	for _, k := range api.Kinds {
		if k.Group == rhoai.ComponentAPIGroup {
			comps.Kinds = append(comps.Kinds, k)
			comps.Groups[k.Group] = k.Version
		}
	}
	if !comps.Served() {
		return ModuleReport{}, nil
	}
	objs, err := listModuleObjects(c, comps)
	if err != nil {
		return ModuleReport{}, err
	}
	rep := ModuleReport{Served: true}
	var failed, notReady []ModuleCondition
	for _, m := range objs {
		rep.Total++
		src := m.Kind + " " + m.Obj.Metadata.Name
		st := m.Obj.Status
		gen := m.Obj.Metadata.Generation
		var ready, provisioned *rhoai.DSCCondition
		current := gen <= 0 || st.ObservedGeneration >= gen
		for i, cond := range st.Conditions {
			switch cond.Type {
			case "Ready":
				ready = &st.Conditions[i]
			case "ProvisioningSucceeded":
				provisioned = &st.Conditions[i]
			default:
				continue
			}
			if cond.ObservedGeneration >= gen {
				current = true
			}
		}
		switch {
		case !current:
			rep.NotReported = append(rep.NotReported, src)
		case provisioned != nil && provisioned.Status == "False":
			failed = append(failed, ModuleCondition{Source: src, Cond: *provisioned, ProvisioningFailed: true})
		case ready == nil:
			rep.NotReported = append(rep.NotReported, src)
		case ready.Status != "True":
			notReady = append(notReady, ModuleCondition{Source: src, Cond: *ready})
		case provisioned != nil && provisioned.Status != "True":
			notReady = append(notReady, ModuleCondition{Source: src, Cond: *provisioned})
		default:
			rep.Ready++
		}
	}
	bySource := func(l []ModuleCondition) {
		sort.SliceStable(l, func(i, j int) bool { return l[i].Source < l[j].Source })
	}
	bySource(failed)
	bySource(notReady)
	sort.Strings(rep.NotReported)
	rep.NotReady = append(failed, notReady...)
	return rep, nil
}
