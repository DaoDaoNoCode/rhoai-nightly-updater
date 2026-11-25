package features

import (
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
)

// A component two features of a set need is recorded once, for the first
// of them in the set's order; an operator two need is installed once.
// The record the set writes keeps what was recorded before, and the
// record it started from is not changed.
func TestUnionOf_TheFirstFeatureKeepsASharedComponent(t *testing.T) {
	dashboard := componentPlan{fc: dashboardComponent, key: "dashboard", state: "Removed", turnOn: true}
	cert := operatorPlan{op: certManagerOperator, install: &envkit.OperatorInstallPlan{Channel: "stable-v1"}}
	plans := []*featurePlan{
		{fs: featureSetup{id: "first"}, components: []componentPlan{dashboard, {key: "ogx", state: "Removed", turnOn: true}}},
		{fs: featureSetup{id: "second"}, components: []componentPlan{dashboard, {key: "kserve", state: "Managed"}}, operators: []operatorPlan{cert}},
		{fs: featureSetup{id: "third"}, operators: []operatorPlan{cert}},
	}
	u := unionOf(plans)
	if len(u.components) != 3 || len(u.operators) != 1 || u.kuadrant {
		t.Fatalf("union %+v", u)
	}
	base := prereqRecord{Components: map[string]prereqComponent{"aipipelines": {Previous: "Removed", Feature: "pipelines"}}}
	rec, changed := u.record(base)
	want := map[string]string{"dashboard": "first", "ogx": "first", "aipipelines": "pipelines"}
	if !changed || len(rec.Components) != len(want) {
		t.Fatalf("record %+v", rec)
	}
	for key, feature := range want {
		if rec.Components[key].Feature != feature {
			t.Errorf("%s: %+v", key, rec.Components[key])
		}
	}
	if rec.Shared["cert-manager"] != "Subscription cert-manager-operator/openshift-cert-manager-operator, channel stable-v1" {
		t.Errorf("shared %v", rec.Shared)
	}
	if len(base.Components) != 1 || base.Shared != nil {
		t.Errorf("the base record changed: %+v", base)
	}
	// Nothing to turn on or install: nothing to record.
	if _, changed := unionOf([]*featurePlan{{fs: featureSetup{id: "x"}, components: []componentPlan{{key: "kserve", state: "Managed"}}}}).record(base); changed {
		t.Error("an unchanged set changed the record")
	}
}
