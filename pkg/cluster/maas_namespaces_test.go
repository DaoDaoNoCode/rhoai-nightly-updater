package cluster

import (
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const tUserProject = `{"metadata":{"annotations":{"openshift.io/requester":"mallory"}}}`

// A user who may request projects makes models-as-a-service (or the infra
// namespace) before MaaS runs: maas-controller adopts a namespace of that
// name that exists, so that user would be an admin of the namespace the
// MaaS gateway admits routes from and where this tool writes its
// subscription and authorization policy. Setup refuses before any write.
func TestMaaSSetup_RefusesANamespaceAUserRequested(t *testing.T) {
	for _, ns := range []string{maasTenantNamespace, "redhat-ai-gateway-infra"} {
		t.Run(ns, func(t *testing.T) {
			mc := newMaaSCluster(t)
			mc.f.putJSON("/api/v1/namespaces/"+ns, tUserProject)
			before := len(mc.f.mutations())
			res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "setup", Database: tCNPG})
			if res.Success || res.ErrorCode != "not_managed" || !strings.Contains(res.Message, ns) || !strings.Contains(res.Message, "mallory") || !strings.Contains(res.Message, "Nothing was changed") {
				t.Fatalf("setup = %+v", res)
			}
			if muts := mc.f.mutations()[before:]; len(muts) != 0 {
				t.Fatalf("wrote %v", muts)
			}
		})
	}
}

// The same check guards publish, which writes into models-as-a-service.
func TestMaaSPublish_RefusesANamespaceAUserRequested(t *testing.T) {
	mc := modelReadyForMaaS(t)
	mc.f.putJSON("/api/v1/namespaces/"+maasTenantNamespace, tUserProject)
	before := len(mc.f.mutations())
	res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "publish", Project: "proj"})
	if res.Success || res.ErrorCode != "not_managed" || !strings.Contains(res.Message, maasTenantNamespace) {
		t.Fatalf("publish = %+v", res)
	}
	if muts := mc.f.mutations()[before:]; len(muts) != 0 {
		t.Fatalf("wrote %v", muts)
	}
}

// Fail closed: a namespace that cannot be read stops setup too; one that
// does not exist yet (RHOAI creates it) or that RHOAI made does not.
func TestMaaSNamespaceRefusal(t *testing.T) {
	mc := newMaaSCluster(t)
	if msg, _ := maasNamespaceRefusal(mc.c, "redhat-ai-gateway-infra"); msg != "" {
		t.Fatalf("RHOAI's namespaces: %s", msg)
	}
	mc.f.putJSON("/api/v1/namespaces/"+maasTenantNamespace, `{"metadata":{"labels":{"app.kubernetes.io/managed-by":"maas-controller"}}}`)
	if msg, _ := maasNamespaceRefusal(mc.c, "redhat-ai-gateway-infra"); msg != "" {
		t.Fatalf("maas-controller's namespace: %s", msg)
	}
	mc.f.fail["GET /api/v1/namespaces/"+maasTenantNamespace] = 500
	if msg, code := maasNamespaceRefusal(mc.c, "redhat-ai-gateway-infra"); msg == "" || code == "" {
		t.Fatalf("a read error must refuse: %q %q", msg, code)
	}
}
