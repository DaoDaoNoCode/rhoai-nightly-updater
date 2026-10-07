package cluster

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestWriteDocsFixtures writes the Diagnostics answers that the docs mock
// backend (docs/tools/mock) serves for its screenshots. They come from the
// fake clusters of the tests in this package, so the docs show what the
// backend really reports: titles, evidence and the generated commands.
// It runs only with DOCS_FIXTURES_DIR set: `make docs-fixtures`.
//
// Each file is healthy.json (the checks of a healthy cluster, kept by hand)
// with the checks named here replaced and their problems added, linked and
// ordered as DiagnoseCluster does.
func TestWriteDocsFixtures(t *testing.T) {
	dir := os.Getenv("DOCS_FIXTURES_DIR")
	if dir == "" {
		t.Skip("DOCS_FIXTURES_DIR is not set")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "healthy.json"))
	if err != nil {
		t.Fatal(err)
	}
	write := func(name string, outs ...checkOutput) {
		t.Helper()
		var resp DiagnosticsResponse
		if err := json.Unmarshal(raw, &resp); err != nil {
			t.Fatal(err)
		}
		resp.Problems = []Problem{}
		for _, out := range outs {
			for i := range resp.Checks {
				if resp.Checks[i].Name == out.check.Name {
					resp.Checks[i] = out.check
				}
			}
			resp.Problems = append(resp.Problems, out.problems...)
		}
		resp.Problems = linkProblems(resp.Problems)
		order := map[string]int{"critical": 0, "warning": 1, "info": 2}
		sort.SliceStable(resp.Problems, func(i, j int) bool { return order[resp.Problems[i].Severity] < order[resp.Problems[j].Severity] })
		b, err := json.MarshalIndent(resp, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name+".json"), append(b, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dsc := func(w *dscWorld) checkOutput {
		out := checkDataScienceCluster(w.c)
		out.check.Name = "DataScienceCluster"
		return out
	}

	// Missing prerequisite operators, with the commands that install them.
	write("prerequisite-missing", dsc(newDSCWorld(t, liveMissingPrereqs())))

	// JobSet installed, its JobSetOperator/cluster operand missing.
	w := newDSCWorld(t, liveMissingPrereqs()[:2])
	w.installedCSV("jobset-operator.v1.0.1", "openshift-jobset-operator", "Job Set Operator", "Succeeded", time.Hour, "job-set")
	write("operand-missing", dsc(w))

	// Everything is in place, but the trainer module operator backs off.
	w = newDSCWorld(t, liveMissingPrereqs()[:2])
	w.installedCSV("jobset-operator.v1.0.1", "openshift-jobset-operator", "Job Set Operator", "Succeeded", time.Hour, "job-set")
	w.f.json("GET", jobSetOperandPath, 200, `{"metadata":{"name":"cluster"}}`)
	w.trainerCR([]dcond{{"type": "Ready", "status": "False", "reason": "Error", "message": msgTrainerJobSet, "lastTransitionTime": ago(time.Hour)}}, 3, 3)
	write("module-backoff", dsc(w))

	// An upgrade gate waits for an acknowledgement.
	w = newDSCWorld(t, []dcond{
		{"type": "Ready", "status": "False", "reason": "Error", "message": msgGateResolve},
		{"type": "ModulesReady", "status": "False", "reason": "AdminAckRequired", "message": msgGateAck},
	})
	w.f.json("GET", "/api/v1/namespaces/redhat-ods-operator/configmaps/odh-upgrade-acks", 200, `{"data":{"kserve-v2":"false","old":"true"}}`)
	write("upgrade-gates", dsc(w))

	// A Certificate that says Ready while cert-manager is stopped. The full
	// report folds the stuck pod into it; keep the Certificates part.
	_, c := staleCertWorld(t, "Managed")
	resp, err := RunDiagnostics(c)
	if err != nil {
		t.Fatal(err)
	}
	cert := checkOutput{check: *findCheck(resp, "Certificates")}
	for _, p := range resp.Problems {
		if strings.HasPrefix(p.ID, "certificate-not-ready-") {
			cert.problems = append(cert.problems, p)
		}
	}
	if len(cert.problems) != 1 {
		t.Fatalf("certificate problems: %d", len(cert.problems))
	}
	write("certificate-stale", cert)

	// Objects the operator cannot update (without the made-up module of
	// the test, which no real cluster has).
	ac, _ := applyFailuresWorld(t)
	apply := checkApplyFailures(ac)
	apply.check.Name = applyFailuresCheckName
	kept := apply.problems[:0]
	for _, p := range apply.problems {
		if !strings.Contains(p.ID, "new-operator") {
			kept = append(kept, p)
		}
	}
	apply.problems = kept
	write("apply-failures", apply)

	// An operator whose conversion webhook knows neither DataScienceCluster
	// v3 nor Platform v1alpha2 (a RHOAI 3.6 nightly, 2026-10-07).
	_, cc := conversionWorld(t, 429, liveDSCv3Conversion429)
	write("conversion-webhook", checkConversionFailures(cc))
}
