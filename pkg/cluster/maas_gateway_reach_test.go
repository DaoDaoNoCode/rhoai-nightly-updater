package cluster

import (
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// maasDSCState is spec.components.aigateway.modelsAsAService.managementState.
func maasDSCState(mc *modelsCluster) any {
	spec := mc.f.get(tMaaSDSC)["spec"].(map[string]any)
	return spec["components"].(map[string]any)["aigateway"].(map[string]any)["modelsAsAService"].(map[string]any)["managementState"]
}

// The MaaS gateway gets no load balancer address within the wait (bare
// metal without MetalLB, for example): setup fails at the gateway, names
// the controller's reason and the documented Route alternative this tool
// does not build, and stops before the DataScienceCluster.
func TestMaaSSetup_GatewayWithoutAddressFails(t *testing.T) {
	mc := newMaaSCluster(t)
	prev := mc.f.onGet
	mc.f.onGet = func(k fakeKey, obj map[string]any) {
		prev(k, obj)
		if k.plural == "gateways" && k.name == "maas-default-gateway" {
			obj["status"] = map[string]any{"conditions": []any{
				map[string]any{"type": "Accepted", "status": "True"},
				map[string]any{"type": "Programmed", "status": "False", "reason": "AddressNotAssigned", "message": "No addresses have been assigned to the Gateway"},
			}}
		}
	}
	res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup", Database: tCNPG})
	if res.Success || res.ErrorCode != "not_ready" {
		t.Fatalf("setup = %+v", res)
	}
	for _, want := range []string{
		"Gateway openshift-ingress/maas-default-gateway has no load balancer address after 2:00",
		"The gateway controller says: No addresses have been assigned to the Gateway.",
		"INGRESS_MODE=ocproute and odh-gitops' openshiftRoute do) is not supported by this tool yet",
		"Clean up MaaS removes what it made",
	} {
		if !strings.Contains(res.Message, want) {
			t.Errorf("message lacks %q: %s", want, res.Message)
		}
	}
	if got := maasDSCState(mc); got != "Removed" {
		t.Errorf("modelsAsAService = %v after a failed gateway", got)
	}
}

// With an address but no DNS zone the hostname never resolves: setup fails
// there too. Zones still being published are the normal wait.
func TestMaaSSetup_GatewayDNS(t *testing.T) {
	for _, tc := range []struct {
		name, zones string
		ok          bool
	}{
		{"no zone", `[]`, false},
		{"not published yet", `[{"conditions":[{"type":"Published","status":"False"}]}]`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mc := newMaaSCluster(t)
			mc.f.putJSON("/apis/ingress.operator.openshift.io/v1/namespaces/openshift-ingress/dnsrecords/maas",
				`{"spec":{"dnsName":"maas.apps.example.com."},"status":{"zones":`+tc.zones+`}}`)
			res, events := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup", Database: tCNPG})
			if res.Success != tc.ok {
				t.Fatalf("setup = %+v", res)
			}
			if !tc.ok {
				if res.ErrorCode != "not_ready" || !strings.Contains(res.Message, "OpenShift publishes no DNS for maas.apps.example.com: its DNSRecord in openshift-ingress lists no DNS zone") ||
					!strings.Contains(res.Message, "is not supported by this tool yet") {
					t.Errorf("failure: %+v", res)
				}
				if got := maasDSCState(mc); got != "Removed" {
					t.Errorf("modelsAsAService = %v after a failed gateway", got)
				}
				return
			}
			note := false
			for _, e := range events {
				note = note || e.Step == "gateway" && e.Status == "success" && strings.Contains(e.Message, "is not in DNS yet")
			}
			if !note {
				t.Error("no note that the hostname is not in DNS yet")
			}
		})
	}
}
