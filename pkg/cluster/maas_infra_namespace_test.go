package cluster

import (
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const tMaaSTenantConfig = "/apis/maas.opendatahub.io/v1alpha1/namespaces/models-as-a-service/maastenantconfigs/default-tenant"

// The infrastructure namespace is derived from the applications namespace
// only when the default tenant is absent (404) or reports none yet. Any
// other answer is not a reason to guess (rhoai_namespaces.go: a read
// failure is never replaced by a default name): the read fails, and the
// applications namespace being known does not change that.
func TestMaaSInfraNamespace_TenantReadFailureIsNotGuessed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"forbidden", 403, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Forbidden","code":403}`},
		{"unauthorized", 401, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Unauthorized","code":401}`},
		{"server error", 500, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"InternalError","code":500}`},
		{"unparsable", 200, `{"status":`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newFakeAPI(t)
			serveDSCI(f, "opendatahub")
			f.json("GET", tMaaSTenantConfig, tc.status, tc.body)
			ns, err := maasInfraNamespace(c)
			if err == nil || ns != "" {
				t.Fatalf("ns=%q err=%v: derived a name although the tenant could not be read", ns, err)
			}
			if !strings.Contains(err.Error(), "default tenant") {
				t.Fatalf("err %q does not say what could not be read", err)
			}
		})
	}
}

func TestMaaSInfraNamespace_DerivedOnlyWithoutAReportedNamespace(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"tenant absent", 404, k8sNotFound, "odh-ai-gateway-infra"},
		{"no status yet", 200, `{"metadata":{"name":"default-tenant"}}`, "odh-ai-gateway-infra"},
		{"empty infraNamespace", 200, `{"status":{"infraNamespace":""}}`, "odh-ai-gateway-infra"},
		{"reported", 200, `{"status":{"infraNamespace":"custom-infra"}}`, "custom-infra"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newFakeAPI(t)
			serveDSCI(f, "opendatahub")
			f.json("GET", tMaaSTenantConfig, tc.status, tc.body)
			if ns, err := maasInfraNamespace(c); err != nil || ns != tc.want {
				t.Fatalf("ns=%q err=%v, want %q", ns, err, tc.want)
			}
		})
	}
}

// End to end: a tenant the user's token cannot read stops Set up before it
// changes anything, and the status says why instead of showing a guessed
// namespace.
func TestMaaSSetup_RefusesWhenTheTenantCannotBeRead(t *testing.T) {
	mc := newMaaSCluster(t)
	mc.f.fail["GET "+tMaaSTenantConfig] = 403
	res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup", Database: tCNPG})
	if res.Success {
		t.Fatalf("setup ran without knowing the infrastructure namespace: %+v", res)
	}
	if m := mc.f.mutations(); len(m) != 0 {
		t.Fatalf("setup changed %v before refusing", m)
	}
	st, err := GetMaaSStatus(mc.c)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(st.Platform.SetupBlockedReason, "default tenant") {
		t.Fatalf("SetupBlockedReason = %q", st.Platform.SetupBlockedReason)
	}
	for _, p := range st.Parts {
		if strings.Contains(p.Message, "redhat-ai-gateway-infra") {
			t.Fatalf("part %+v names a guessed namespace", p)
		}
	}

	// MaaS turned on by someone else: maas-api's state is unknown, with the
	// reason, not read from a guessed namespace.
	mc.f.putJSON(tMaaSDSC, `{"spec":{"components":{"kserve":{"managementState":"Managed"},"aigateway":{"managementState":"Managed","modelsAsAService":{"managementState":"Managed"}}}}}`)
	st, err = GetMaaSStatus(mc.c)
	if err != nil {
		t.Fatal(err)
	}
	var api *types.MaaSPart
	for i := range st.Parts {
		if st.Parts[i].ID == "maas-api" {
			api = &st.Parts[i]
		}
	}
	if api == nil || api.State != "unknown" || !strings.Contains(api.Message, "default tenant") {
		t.Fatalf("maas-api part = %+v", api)
	}
}
