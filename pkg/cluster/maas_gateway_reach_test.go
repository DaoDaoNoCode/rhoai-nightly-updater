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

// What the DNS wait finds is only reported: no zone, a record still being
// published, or an unreadable list is a success with what to check.
func TestMaaSSetup_GatewayDNS(t *testing.T) {
	for _, tc := range []struct {
		name, zones string
		unreadable  bool
		want        string
	}{
		{"no zone", `[]`, false, "Programmed; Warning: OpenShift has not published https://maas.apps.example.com in DNS: its DNSRecord in openshift-ingress lists no DNS zone"},
		{"not published yet", `[{"conditions":[{"type":"Published","status":"False"}]}]`, false, "Programmed; https://maas.apps.example.com is not published in DNS yet"},
		{"unreadable", `[]`, true, "Programmed; Warning: whether https://maas.apps.example.com is published in DNS could not be read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mc := newMaaSCluster(t)
			mc.f.putJSON("/apis/ingress.operator.openshift.io/v1/namespaces/openshift-ingress/dnsrecords/maas",
				`{"spec":{"dnsName":"maas.apps.example.com."},"status":{"zones":`+tc.zones+`}}`)
			if tc.unreadable {
				mc.f.fail["GET /apis/ingress.operator.openshift.io/v1/namespaces/openshift-ingress/dnsrecords"] = 500
			}
			res, events := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup", Database: tCNPG})
			if !res.Success {
				t.Fatalf("setup = %+v", res)
			}
			found := false
			for _, e := range events {
				found = found || e.Step == "gateway" && e.Status == "success" && strings.Contains(e.Message, tc.want)
			}
			if !found {
				t.Errorf("no gateway step with %q", tc.want)
			}
		})
	}
}
