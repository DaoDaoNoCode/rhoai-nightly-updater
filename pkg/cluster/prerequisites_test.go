package cluster

import (
	"encoding/json"
	"strings"
	"testing"
)

// Condition messages verbatim from a live RHOAI 3.6 nightly on OCP 4.22
// (2026-10-06).
const (
	msgTrainerJobSet = "dependency not met: JobSet Operator is not installed. Please install the JobSet Operator via OLM (OperatorHub) before deploying Trainer."
	msgCertManager   = "cert-manager operator not installed"
	msgWideEP        = "LeaderWorkerSet not installed; cert-manager operator (Wide EP) not installed"
	msgRayStale      = "Module status is stale (observedGeneration < generation)"
	msgGateAck       = "Waiting for upgrade gates to be acknowledged"
	msgGateResolve   = "failed to resolve upgrade gate version: unable to determine target release for upgrade gates"
)

func TestParseMissingDependencies_LiveMessages(t *testing.T) {
	tests := []struct {
		msg  string
		want []string
	}{
		{msgTrainerJobSet, []string{"JobSet Operator"}},
		{msgCertManager, []string{"cert-manager operator"}},
		{msgWideEP, []string{"LeaderWorkerSet", "cert-manager operator"}},
		{"Some modules are not ready: trainer", nil},
		{msgRayStale, nil},
		{"Kserve: dependency not met: Red Hat Connectivity Link is not installed", []string{"Red Hat Connectivity Link"}},
		{"Foo and Bar Operator are not installed", []string{"Foo", "Bar Operator"}},
	}
	for _, tt := range tests {
		if got := parseMissingDependencies(tt.msg); strings.Join(got, "|") != strings.Join(tt.want, "|") {
			t.Errorf("parseMissingDependencies(%q) = %q, want %q", tt.msg, got, tt.want)
		}
	}
}

func TestNormalizeOperatorName(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"JobSet Operator", "jobset"},
		{"Job Set Operator", "jobset"},
		{"job-set", "jobset"},
		{"jobset-operator", "jobset"},
		{"cert-manager operator (Wide EP)", "certmanager"},
		{"cert-manager Operator for Red Hat OpenShift", "certmanager"},
		{"openshift-cert-manager-operator", "certmanager"},
		{"LeaderWorkerSet", "leaderworkerset"},
		{"leader-worker-set", "leaderworkerset"},
		{"Leader Worker Set Operator", "leaderworkerset"},
		{"LWS", "leaderworkerset"},
		{"Red Hat Connectivity Link", "connectivitylink"},
	} {
		if got := normalizeOperatorName(tt.in); got != tt.want {
			t.Errorf("normalizeOperatorName(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// Catalog facts from the live cluster's packagemanifests (redhat-operators
// and community-operators).
const (
	jobSetExamples      = `[{"apiVersion":"operator.openshift.io/v1","kind":"JobSetOperator","metadata":{"name":"cluster"},"spec":{"logLevel":"Normal","managementState":"Managed","operatorLogLevel":"Normal"}}]`
	certManagerExamples = `[{"apiVersion":"cert-manager.io/v1","kind":"Certificate","metadata":{"name":"tls-cert","namespace":"default"}},{"apiVersion":"operator.openshift.io/v1alpha1","kind":"CertManager","metadata":{"name":"cluster"},"spec":{"managementState":"Managed"}},{"apiVersion":"operator.openshift.io/v1alpha1","kind":"TrustManager","metadata":{"name":"cluster"},"spec":{}}]`
	lwsExamples         = `[{"apiVersion":"operator.openshift.io/v1","kind":"LeaderWorkerSetOperator","metadata":{"name":"cluster"},"spec":{"managementState":"Managed"}}]`
)

func pkgManifest(name, catalog, provider, channel, csv, display, suggestedNS string, modes []string, examples string, monitoring bool) map[string]interface{} {
	var installModes []map[string]interface{}
	for _, m := range []string{"OwnNamespace", "SingleNamespace", "MultiNamespace", "AllNamespaces"} {
		installModes = append(installModes, map[string]interface{}{"type": m, "supported": containsString(modes, m)})
	}
	ann := map[string]string{"alm-examples": examples}
	if suggestedNS != "" {
		ann["operatorframework.io/suggested-namespace"] = suggestedNS
	}
	if monitoring {
		ann["operatorframework.io/cluster-monitoring"] = "true"
	}
	return map[string]interface{}{
		"metadata": map[string]interface{}{"name": name, "labels": map[string]string{"catalog": catalog, "provider": provider}},
		"status": map[string]interface{}{
			"catalogSource": catalog, "catalogSourceNamespace": "openshift-marketplace", "defaultChannel": channel,
			"provider": map[string]string{"name": provider},
			"channels": []interface{}{
				map[string]interface{}{"name": "older", "currentCSV": "old.v0", "currentCSVDesc": map[string]interface{}{"displayName": "Old"}},
				map[string]interface{}{"name": channel, "currentCSV": csv, "currentCSVDesc": map[string]interface{}{
					"displayName": display, "installModes": installModes, "annotations": ann,
				}},
			},
		},
	}
}

func liveCatalog() []map[string]interface{} {
	return []map[string]interface{}{
		pkgManifest("job-set", "redhat-operators", "Red Hat, Inc.", "stable-v1.0", "jobset-operator.v1.0.1", "Job Set Operator", "openshift-jobset-operator",
			[]string{"OwnNamespace", "SingleNamespace"}, jobSetExamples, true),
		pkgManifest("openshift-cert-manager-operator", "redhat-operators", "Red Hat", "stable-v1", "cert-manager-operator.v1.20.1", "cert-manager Operator for Red Hat OpenShift", "cert-manager-operator",
			[]string{"OwnNamespace", "SingleNamespace", "AllNamespaces"}, certManagerExamples, false),
		pkgManifest("cert-manager", "community-operators", "The cert-manager maintainers", "stable", "cert-manager.v1.16.5", "cert-manager", "",
			[]string{"AllNamespaces"}, `[]`, false),
		pkgManifest("leader-worker-set", "redhat-operators", "Red Hat", "stable-v1.0", "leader-worker-set.v1.0.1", "Leader Worker Set Operator", "openshift-lws-operator",
			[]string{"OwnNamespace"}, lwsExamples, false),
		pkgManifest("unrelated", "certified-operators", "Someone", "stable", "unrelated.v1", "Unrelated", "", []string{"AllNamespaces"}, `[]`, false),
	}
}

func catalogPackages(t *testing.T, items []map[string]interface{}) []catalogPackage {
	t.Helper()
	body, _ := json.Marshal(map[string]interface{}{"items": items})
	pkgs, err := parsePackageManifests(body)
	if err != nil {
		t.Fatal(err)
	}
	return pkgs
}

func TestMatchPackages_PrefersRedHatOverCommunity(t *testing.T) {
	pkgs := catalogPackages(t, liveCatalog())
	got := matchPackages(pkgs, normalizeOperatorName("cert-manager operator"))
	if len(got) != 2 || got[0].Name != "openshift-cert-manager-operator" || got[1].Name != "cert-manager" {
		t.Fatalf("matches = %+v", got)
	}
	if m := matchPackages(pkgs, normalizeOperatorName("JobSet Operator")); len(m) != 1 || m[0].Name != "job-set" || m[0].DefaultChannel != "stable-v1.0" || m[0].CurrentCSV != "jobset-operator.v1.0.1" {
		t.Fatalf("job-set = %+v", m)
	}
	if m := matchPackages(pkgs, "nosuchoperator"); len(m) != 0 {
		t.Fatalf("unexpected match %+v", m)
	}
}

func TestSingletonFor(t *testing.T) {
	pkgs := catalogPackages(t, liveCatalog())
	byName := map[string]catalogPackage{}
	for _, p := range pkgs {
		byName[p.Name] = p
	}
	// cert-manager ships two singletons; the one named like the operator is
	// its operand (TrustManager is a separate feature).
	if s := byName["openshift-cert-manager-operator"].singletonFor("certmanager"); s == nil || s.Kind != "CertManager" {
		t.Fatalf("cert-manager singleton = %+v", s)
	}
	if s := byName["job-set"].singletonFor("jobset"); s == nil || s.Kind != "JobSetOperator" || s.APIVersion != "operator.openshift.io/v1" {
		t.Fatalf("job-set singleton = %+v", s)
	}
	if s := byName["cert-manager"].singletonFor("certmanager"); s != nil {
		t.Fatalf("community cert-manager has no singleton, got %+v", s)
	}
}

func TestPlanInstall(t *testing.T) {
	pkgs := catalogPackages(t, liveCatalog())
	byName := map[string]catalogPackage{}
	for _, p := range pkgs {
		byName[p.Name] = p
	}
	t.Run("OwnNamespace with operand (job-set)", func(t *testing.T) {
		_, c := newFakeAPI(t)
		p := byName["job-set"]
		plan, err := planInstall(c, p, p.singletonFor("jobset"))
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{
			"oc apply -f - <<'EOF'\napiVersion: v1\nkind: Namespace\nmetadata:\n  name: openshift-jobset-operator\n  labels:\n    openshift.io/cluster-monitoring: \"true\"\n---\n",
			"kind: OperatorGroup\nmetadata:\n  name: openshift-jobset-operator\n  namespace: openshift-jobset-operator\nspec:\n  targetNamespaces:\n  - openshift-jobset-operator\n---\n",
			"kind: Subscription\nmetadata:\n  name: job-set\n  namespace: openshift-jobset-operator\nspec:\n  channel: stable-v1.0\n  name: job-set\n  source: redhat-operators\n  sourceNamespace: openshift-marketplace\n  installPlanApproval: Automatic\nEOF\n",
			"oc wait subscription/job-set -n openshift-jobset-operator '--for=jsonpath={.status.state}=AtLatestKnown' --timeout=10m",
			"--for=jsonpath='{.status.phase}'=Succeeded",
			"oc get jobsetoperator.operator.openshift.io/cluster >/dev/null 2>&1 || oc create -f - <<'EOF'\n{\n  \"apiVersion\": \"operator.openshift.io/v1\",\n  \"kind\": \"JobSetOperator\"",
			`"managementState": "Managed"`,
		} {
			if !strings.Contains(plan.script, want) {
				t.Errorf("script lacks %q:\n%s", want, plan.script)
			}
		}
		if !strings.HasSuffix(plan.script, "\nEOF") {
			t.Errorf("script does not end its here-document:\n%s", plan.script)
		}
	})
	t.Run("OwnNamespace preferred over AllNamespaces (cert-manager)", func(t *testing.T) {
		_, c := newFakeAPI(t)
		p := byName["openshift-cert-manager-operator"]
		plan, err := planInstall(c, p, p.singletonFor("certmanager"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(plan.script, "  name: cert-manager-operator\n---") || !strings.Contains(plan.script, "targetNamespaces:\n  - cert-manager-operator") ||
			!strings.Contains(plan.script, "oc get certmanager.operator.openshift.io/cluster") || strings.Contains(plan.script, "TrustManager") ||
			strings.Contains(plan.script, "cluster-monitoring") {
			t.Fatalf("script:\n%s", plan.script)
		}
		if !strings.Contains(strings.Join(plan.notes, " "), "some operators create it themselves") {
			t.Fatalf("notes = %q", plan.notes)
		}
	})
	t.Run("AllNamespaces only goes to openshift-operators without an OperatorGroup", func(t *testing.T) {
		f, c := newFakeAPI(t)
		f.json("GET", "/apis/operators.coreos.com/v1/namespaces/openshift-operators/operatorgroups", 200, `{"items":[{"metadata":{"name":"global-operators"},"spec":{}}]}`)
		plan, err := planInstall(c, byName["cert-manager"], nil)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(plan.script, "OperatorGroup") || strings.Contains(plan.script, "kind: Namespace") || !strings.Contains(plan.script, "namespace: openshift-operators") ||
			!strings.Contains(plan.script, "source: community-operators") {
			t.Fatalf("script:\n%s", plan.script)
		}
	})
	t.Run("an existing OperatorGroup is reused", func(t *testing.T) {
		f, c := newFakeAPI(t)
		f.json("GET", "/apis/operators.coreos.com/v1/namespaces/openshift-lws-operator/operatorgroups", 200, `{"items":[{"metadata":{"name":"lws-og"},"spec":{"targetNamespaces":["openshift-lws-operator"]}}]}`)
		p := byName["leader-worker-set"]
		plan, err := planInstall(c, p, p.singletonFor("leaderworkerset"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(plan.script, "kind: OperatorGroup") || !strings.Contains(strings.Join(plan.notes, " "), "already has OperatorGroup lws-og (install mode OwnNamespace") {
			t.Fatalf("script:\n%s\nnotes: %q", plan.script, plan.notes)
		}
	})
	t.Run("an existing OperatorGroup with an unsupported mode is refused", func(t *testing.T) {
		for name, ogs := range map[string]string{
			"AllNamespaces for an OwnNamespace-only CSV": `[{"metadata":{"name":"all"},"spec":{}}]`,
			"another namespace (SingleNamespace)":        `[{"metadata":{"name":"single"},"spec":{"targetNamespaces":["other"]}}]`,
			"two OperatorGroups":                         `[{"metadata":{"name":"a"},"spec":{}},{"metadata":{"name":"b"},"spec":{}}]`,
			"a label selector":                           `[{"metadata":{"name":"sel"},"spec":{"selector":{"matchLabels":{"x":"y"}}}}]`,
		} {
			f, c := newFakeAPI(t)
			f.json("GET", "/apis/operators.coreos.com/v1/namespaces/openshift-lws-operator/operatorgroups", 200, `{"items":`+ogs+`}`)
			p := byName["leader-worker-set"]
			_, err := planInstall(c, p, p.singletonFor("leaderworkerset"))
			if err == nil {
				t.Errorf("%s: expected a refusal", name)
				continue
			}
			t.Logf("%s: %v", name, err)
		}
		// openshift-operators whose OperatorGroup does not watch all namespaces.
		f, c := newFakeAPI(t)
		f.json("GET", "/apis/operators.coreos.com/v1/namespaces/openshift-operators/operatorgroups", 200, `{"items":[{"metadata":{"name":"own"},"spec":{"targetNamespaces":["openshift-operators"]}}]}`)
		if _, err := planInstall(c, byName["cert-manager"], nil); err == nil || !strings.Contains(err.Error(), "install mode OwnNamespace") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("openshift-operators without an OperatorGroup gets one for all namespaces", func(t *testing.T) {
		_, c := newFakeAPI(t)
		plan, err := planInstall(c, byName["cert-manager"], nil)
		if err != nil || !strings.Contains(plan.script, "kind: OperatorGroup\nmetadata:\n  name: global-operators\n  namespace: openshift-operators\nspec: {}") {
			t.Fatalf("plan = %+v, %v", plan, err)
		}
	})
	t.Run("SingleNamespace only has no commands", func(t *testing.T) {
		_, c := newFakeAPI(t)
		p := byName["job-set"]
		p.InstallModes = []string{"SingleNamespace"}
		if _, err := planInstall(c, p, nil); err == nil || !strings.Contains(err.Error(), "SingleNamespace") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("names that are not Kubernetes names are refused", func(t *testing.T) {
		_, c := newFakeAPI(t)
		p := byName["job-set"]
		p.DefaultChannel = "stable; rm -rf /"
		if _, err := planInstall(c, p, nil); err == nil {
			t.Fatal("expected an error")
		}
	})
}

func TestOperandCommand_RefusesUnexpectedKinds(t *testing.T) {
	if _, err := operandCommand(almExample{APIVersion: "operator.openshift.io/v1", Kind: "Bad Kind"}); err == nil {
		t.Fatal("expected an error for a kind with a space")
	}
	cmd, err := operandCommand(almExample{APIVersion: "operator.openshift.io/v1", Kind: "JobSetOperator", Spec: json.RawMessage(`{"a":"EOF"}`)})
	if err != nil {
		t.Fatal(err)
	}
	// No line of the here-document may be exactly EOF before its end.
	lines := strings.Split(cmd, "\n")
	for _, l := range lines[1 : len(lines)-1] {
		if l == "EOF" {
			t.Fatalf("here-document ends early:\n%s", cmd)
		}
	}
}

func TestFindInstalledCSV(t *testing.T) {
	pkgs := catalogPackages(t, liveCatalog())
	jobset := matchPackages(pkgs, "jobset")[0]
	csvs := []clusterCSV{
		{Name: "jobset-operator.v1.0.0", Namespace: "openshift-jobset-operator", Phase: "Failed"},
		{Name: "jobset-operator.v1.0.1", Namespace: "openshift-jobset-operator", Phase: "Succeeded", Labels: map[string]string{"operators.coreos.com/job-set.openshift-jobset-operator": ""}},
		{Name: "other.v1", Namespace: "x", Phase: "Succeeded"},
	}
	if got := findInstalledCSV(csvs, &jobset, "jobset"); got == nil || got.Name != "jobset-operator.v1.0.1" {
		t.Fatalf("got %+v", got)
	}
	// Without a catalog match, the CSV's display name decides.
	csvs = []clusterCSV{{Name: "lws.v1", Namespace: "ns", DisplayName: "Leader Worker Set Operator", Phase: "Succeeded"}}
	if got := findInstalledCSV(csvs, nil, "leaderworkerset"); got == nil {
		t.Fatal("not found by display name")
	}
}
