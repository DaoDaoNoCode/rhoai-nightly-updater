package cluster

import (
	"strings"
	"testing"
	"time"
)

// Live, after `oc delete jobsetoperator cluster` (RHOAI 3.6, 2026-10-06).
const msgTrainerOperand = "dependency not met: JobSetOperator CR with name 'cluster' not found. Please create the JobSetOperator CR to enable the JobSet controller."

func TestParseMissingOperands(t *testing.T) {
	for _, tt := range []struct {
		msg  string
		want []operandMention
	}{
		{msgTrainerOperand, []operandMention{{"JobSetOperator", "cluster"}}},
		{"Please create the LeaderWorkerSetOperator CR", []operandMention{{"LeaderWorkerSetOperator", ""}}},
		{`Foo custom resource "main" does not exist`, []operandMention{{"Foo", "main"}}},
		{msgTrainerJobSet, nil},
		{"the CR was not found", nil},
	} {
		got := parseMissingOperands(tt.msg)
		if len(got) != len(tt.want) {
			t.Errorf("parseMissingOperands(%q) = %v, want %v", tt.msg, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("parseMissingOperands(%q) = %v, want %v", tt.msg, got, tt.want)
			}
		}
	}
	if deps := parseMissingDependencies(msgTrainerOperand); len(deps) != 0 {
		t.Fatalf("the operand message is not an operator name: %v", deps)
	}
}

func liveOperandConds() []dcond {
	return []dcond{
		{"type": "Ready", "status": "False", "reason": "Error", "message": "Some modules are not ready: trainer"},
		{"type": "TrainerReady", "status": "False", "reason": "Error", "message": msgTrainerOperand, "lastTransitionTime": ago(time.Hour)},
	}
}

func TestDSCCheck_OperandMessage_InstalledOperatorMissingOperand(t *testing.T) {
	w := newDSCWorld(t, liveOperandConds())
	w.installedCSV("jobset-operator.v1.0.1", "openshift-jobset-operator", "Job Set Operator", "Succeeded", time.Hour, "job-set")
	out := checkDataScienceCluster(w.c)
	byID := problemsByID(out)
	p, ok := byID["prerequisite-operand-job-set"]
	if !ok || p.AutoFixable || p.Title != "Job Set Operator is installed, but its JobSetOperator/cluster does not exist" ||
		!strings.HasPrefix(p.TechnicalCmd, "oc get jobsetoperator.operator.openshift.io/cluster >/dev/null 2>&1 || oc create -f - <<'EOF'") ||
		!strings.Contains(p.TechnicalCmd, `"managementState": "Managed"`) {
		t.Fatalf("problems = %v, operand = %+v", ids(out), p)
	}
	if _, ok := byID["module-operator-backoff-trainer"]; ok {
		t.Fatal("restart offered while the operand is missing")
	}
	dsc := byID["dsc-not-ready"]
	ev := strings.Join(dsc.Evidence, "\n")
	if strings.Contains(ev, "cause not classified") || !strings.Contains(ev, "→ cause: Job Set Operator is installed, but JobSetOperator/cluster does not exist") ||
		!containsString(dsc.RelatedProblems, "prerequisite-operand-job-set") {
		t.Fatalf("dsc-not-ready = %+v", dsc)
	}
	// The kind is resolved through the installed CSV's owned CRDs, not the catalog.
	if n := len(w.f.requests("GET", packageManifestsPath)); n != 1 {
		t.Fatalf("package manifest reads = %d", n)
	}
}

// Live: "ModulesReady=False (NotReady): Some modules are not ready: trainer"
// was left unclassified; it now carries the trainer's own cause.
func TestDSCCheck_ModulesReadyRollupLinksModuleCauses(t *testing.T) {
	conds := append(liveOperandConds(), dcond{"type": "ModulesReady", "status": "False", "reason": "NotReady", "message": "Some modules are not ready: trainer"})
	w := newDSCWorld(t, conds)
	w.installedCSV("jobset-operator.v1.0.1", "openshift-jobset-operator", "Job Set Operator", "Succeeded", time.Hour, "job-set")
	dsc := problemsByID(checkDataScienceCluster(w.c))["dsc-not-ready"]
	ev := strings.Join(dsc.Evidence, "\n")
	if !strings.Contains(ev, "ModulesReady=False (NotReady): Some modules are not ready: trainer → cause: module trainer: Job Set Operator is installed, but JobSetOperator/cluster does not exist") ||
		strings.Contains(ev, "cause not classified") || !containsString(dsc.RelatedProblems, "prerequisite-operand-job-set") {
		t.Fatalf("dsc-not-ready = %s / %v", ev, dsc.RelatedProblems)
	}

	if got := parseNotReadyModules("Some components are not ready: kserve, Ray and trainer"); strings.Join(got, ",") != "kserve,ray,trainer" {
		t.Fatalf("parseNotReadyModules = %v", got)
	}
}

func TestDSCCheck_OperandMessage_OperandBackOffersRestart(t *testing.T) {
	w := newDSCWorld(t, liveOperandConds())
	w.installedCSV("jobset-operator.v1.0.1", "openshift-jobset-operator", "Job Set Operator", "Succeeded", time.Hour, "job-set")
	w.f.json("GET", jobSetOperandPath, 200, `{"metadata":{"name":"cluster"}}`)
	out := checkDataScienceCluster(w.c)
	b, ok := problemsByID(out)["module-operator-backoff-trainer"]
	if !ok || !b.AutoFixable || !strings.Contains(strings.Join(b.Evidence, "\n"), "still reports JobSetOperator/cluster as missing") {
		t.Fatalf("problems = %v, backoff = %+v", ids(out), b)
	}
}

func TestDSCCheck_OperandMessage_OperatorNotInstalled(t *testing.T) {
	w := newDSCWorld(t, liveOperandConds())
	cat := liveCatalog()
	ch := cat[0]["status"].(map[string]interface{})["channels"].([]interface{})[1].(map[string]interface{})
	ch["currentCSVDesc"].(map[string]interface{})["customresourcedefinitions"] = map[string]interface{}{
		"owned": []interface{}{map[string]string{"name": "jobsetoperators.operator.openshift.io", "kind": "JobSetOperator", "version": "v1"}},
	}
	w.f.obj("GET", packageManifestsPath, map[string]interface{}{"items": cat})
	out := checkDataScienceCluster(w.c)
	p, ok := problemsByID(out)["prerequisite-missing-job-set"]
	if !ok || !strings.Contains(p.TechnicalCmd, "name: job-set") || !strings.Contains(p.TechnicalCmd, "oc get jobsetoperator.operator.openshift.io/cluster") {
		t.Fatalf("problems = %v, p = %+v", ids(out), p)
	}

	// Nothing provides the kind: guidance names it.
	w2 := newDSCWorld(t, []dcond{
		{"type": "Ready", "status": "False", "reason": "Error", "message": "Some modules are not ready: trainer"},
		{"type": "TrainerReady", "status": "False", "reason": "Error", "message": "dependency not met: NoSuchThing CR with name 'cluster' not found."},
	})
	out = checkDataScienceCluster(w2.c)
	p, ok = problemsByID(out)["prerequisite-missing-operand-nosuchthing"]
	if !ok || !strings.Contains(p.Title, "No installed operator provides NoSuchThing") || !strings.Contains(p.TechnicalCmd, "grep -w NoSuchThing") {
		t.Fatalf("problems = %v, p = %+v", ids(out), p)
	}
}
