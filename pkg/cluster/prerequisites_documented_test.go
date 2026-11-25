package cluster

import (
	"strings"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/diagnostics"
)

// The JobSet Operator's install documentation requires the cert-manager
// Operator for Red Hat OpenShift first, which its bundle does not declare to
// OLM: the JobSet install commands install it first, wait for it, or only
// say it is installed, from what the cluster shows.
func TestDSCCheck_JobSetCommandsInstallCertManagerFirst(t *testing.T) {
	jobSetProblem := func(t *testing.T, w *dscWorld) Problem {
		t.Helper()
		out := diagnostics.CheckDataScienceCluster(w.c)
		p, ok := problemsByID(out)["prerequisite-missing-job-set"]
		if !ok {
			t.Fatalf("problems = %v", ids(out))
		}
		assertWrites(t, w.f)
		return p
	}

	t.Run("not installed: installed first", func(t *testing.T) {
		w := newDSCWorld(t, liveMissingPrereqs()[:2])
		p := jobSetProblem(t, w)
		cm := strings.Index(p.TechnicalCmd, "kind: Namespace\nmetadata:\n  name: cert-manager-operator")
		cmWait := strings.Index(p.TechnicalCmd, "oc wait subscription/openshift-cert-manager-operator -n cert-manager-operator")
		js := strings.Index(p.TechnicalCmd, "kind: Namespace\nmetadata:\n  name: openshift-jobset-operator")
		operand := strings.Index(p.TechnicalCmd, "oc get jobsetoperator.operator.openshift.io/cluster >/dev/null 2>&1 || oc create -f - <<'EOF'")
		if cm < 0 || cmWait < cm || js < cmWait || operand < js {
			t.Fatalf("order cert-manager %d, its wait %d, JobSet %d, JobSetOperator/cluster %d in:\n%s", cm, cmWait, js, operand, p.TechnicalCmd)
		}
		if !strings.Contains(p.TechnicalCmd, "  name: openshift-cert-manager-operator\n  source: redhat-operators") ||
			!strings.Contains(p.TechnicalCmd, `"managementState": "Managed"`) {
			t.Fatalf("cmd = %s", p.TechnicalCmd)
		}
		// Each operator is named in its own instructions.
		for _, want := range []string{
			"The Job Set Operator documentation requires cert-manager Operator for Red Hat OpenShift first, which is not installed, so the commands install it first",
			"cert-manager Operator for Red Hat OpenShift goes into its suggested namespace cert-manager-operator",
			"its two oc wait lines return once cert-manager Operator for Red Hat OpenShift is installed",
			"The last command of the cert-manager Operator for Red Hat OpenShift install creates CertManager/cluster",
			"the Job Set Operator block runs only when the first one succeeded",
			"Job Set Operator goes into its suggested namespace openshift-jobset-operator",
			"its two oc wait lines return once Job Set Operator is installed",
			"The last command of the Job Set Operator install creates JobSetOperator/cluster",
		} {
			if !strings.Contains(p.Fix, want) {
				t.Fatalf("fix lacks %q:\n%s", want, p.Fix)
			}
		}
		if strings.Contains(p.Fix, "It goes into") {
			t.Fatalf("fix = %s", p.Fix)
		}
		// Two subshell blocks with set -e, the JobSet one only when the
		// first exited 0 (not after &&, where set -e does not apply).
		blocks := strings.Split(p.TechnicalCmd, "\n)\nif [ $? -eq 0 ]; then\n(\n")
		if len(blocks) != 2 || !strings.HasPrefix(blocks[0], "(\nset -e\n") || !strings.HasPrefix(blocks[1], "set -e\n") ||
			!strings.HasSuffix(blocks[1], "\n)\nelse\n  echo 'A step of the commands before Job Set Operator failed, so its install did not run.' >&2\nfi") ||
			strings.Contains(blocks[0], "openshift-jobset-operator") || !strings.Contains(blocks[1], "openshift-jobset-operator") {
			t.Fatalf("blocks = %q", blocks)
		}
		if !strings.Contains(strings.Join(p.Evidence, "\n"), "Catalog: package openshift-cert-manager-operator in catalog redhat-operators") {
			t.Fatalf("evidence = %q", p.Evidence)
		}
	})

	t.Run("installed: only evidence", func(t *testing.T) {
		w := newDSCWorld(t, liveMissingPrereqs()[:2])
		w.installedCSVs(csvItem("cert-manager-operator.v1.20.1", "cert-manager-operator", "cert-manager Operator for Red Hat OpenShift", "Succeeded", time.Hour,
			"openshift-cert-manager-operator", certManagerExamples, nil))
		p := jobSetProblem(t, w)
		if strings.Contains(p.TechnicalCmd, "cert-manager") || strings.Contains(p.Fix, "requires") {
			t.Fatalf("cmd = %s\nfix = %s", p.TechnicalCmd, p.Fix)
		}
		if !strings.Contains(strings.Join(p.Evidence, "\n"), "cert-manager Operator for Red Hat OpenShift, which the Job Set Operator documentation requires first, is installed: CSV cert-manager-operator/cert-manager-operator.v1.20.1 is Succeeded") {
			t.Fatalf("evidence = %q", p.Evidence)
		}
	})

	t.Run("installing: waited for first", func(t *testing.T) {
		w := newDSCWorld(t, liveMissingPrereqs()[:2])
		w.installedCSVs(csvItem("cert-manager-operator.v1.20.1", "cert-manager-operator", "cert-manager Operator for Red Hat OpenShift", "Installing", time.Minute,
			"openshift-cert-manager-operator", certManagerExamples, nil))
		p := jobSetProblem(t, w)
		if !strings.HasPrefix(p.TechnicalCmd, "(\nset -e\noc wait csv/cert-manager-operator.v1.20.1 -n cert-manager-operator '--for=jsonpath={.status.phase}=Succeeded' --timeout=10m\n)\nif [ $? -eq 0 ]; then\n(\nset -e\noc apply -f - <<'EOF'") ||
			strings.Contains(p.TechnicalCmd, "name: cert-manager-operator\n") {
			t.Fatalf("cmd = %s", p.TechnicalCmd)
		}
		if !strings.Contains(p.Fix, "its install has not finished, so the first command waits for it") ||
			!strings.Contains(strings.Join(p.Evidence, "\n"), "CSV cert-manager-operator/cert-manager-operator.v1.20.1 is in phase Installing") {
			t.Fatalf("fix = %s\nevidence = %q", p.Fix, p.Evidence)
		}
	})

	t.Run("no catalog package: said, no command", func(t *testing.T) {
		w := newDSCWorld(t, liveMissingPrereqs()[:2])
		var catalog []map[string]any
		for _, m := range liveCatalog() {
			if name := m["metadata"].(map[string]any)["name"]; name != "openshift-cert-manager-operator" && name != "cert-manager" {
				catalog = append(catalog, m)
			}
		}
		w.f.obj(packageManifestsPath, map[string]any{"items": catalog})
		p := jobSetProblem(t, w)
		if strings.Contains(p.TechnicalCmd, "cert-manager") || !strings.HasPrefix(p.TechnicalCmd, "oc apply -f - <<'EOF'\napiVersion: v1\nkind: Namespace\nmetadata:\n  name: openshift-jobset-operator") ||
			!strings.Contains(p.Fix, "requires cert-manager Operator for Red Hat OpenShift first, which is not installed, and no package in this cluster's catalogs matches it: install it first") {
			t.Fatalf("cmd = %s\nfix = %s", p.TechnicalCmd, p.Fix)
		}
	})

	// Only documented prerequisites: LeaderWorkerSet has none.
	t.Run("other operators unchanged", func(t *testing.T) {
		w := newDSCWorld(t, []dcond{
			{"type": "Ready", "status": "True"},
			{"type": "KserveLLMInferenceServiceWideEPDependencies", "status": "False", "reason": "PreConditionFailed", "severity": "Info", "message": "LeaderWorkerSet not installed"},
		})
		p := problemsByID(diagnostics.CheckDataScienceCluster(w.c))["prerequisite-missing-leader-worker-set"]
		if !strings.HasPrefix(p.TechnicalCmd, "oc apply -f - <<'EOF'\napiVersion: v1\nkind: Namespace\nmetadata:\n  name: openshift-lws-operator") || strings.Contains(p.Fix, "requires") {
			t.Fatalf("lws = %+v", p)
		}
	})
}
