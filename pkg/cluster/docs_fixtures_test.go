package cluster

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/components"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/diagnostics"
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
	write := func(name string, outs ...diagnostics.CheckOutput) {
		t.Helper()
		var resp DiagnosticsResponse
		if err := json.Unmarshal(raw, &resp); err != nil {
			t.Fatal(err)
		}
		resp.Problems = []Problem{}
		for _, out := range outs {
			for i := range resp.Checks {
				if resp.Checks[i].Name == out.Check.Name {
					resp.Checks[i] = out.Check
				}
			}
			resp.Problems = append(resp.Problems, out.Problems...)
		}
		resp.Problems = diagnostics.LinkProblems(resp.Problems)
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
	dsc := func(w *dscWorld) diagnostics.CheckOutput {
		out := diagnostics.CheckDataScienceCluster(w.c)
		out.Check.Name = "DataScienceCluster"
		return out
	}

	// Missing prerequisite operators, with the commands that install them.
	write("prerequisite-missing", dsc(newDSCWorld(t, liveMissingPrereqs())))

	// JobSet installed, its JobSetOperator/cluster operand missing.
	w := newDSCWorld(t, liveMissingPrereqs()[:2])
	w.installedCSV(time.Hour)
	write("operand-missing", dsc(w))

	// Everything is in place, but the trainer module operator backs off.
	w = newDSCWorld(t, liveMissingPrereqs()[:2])
	w.installedCSV(time.Hour)
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
	cert := diagnostics.CheckOutput{Check: *findCheck(resp, "Certificates")}
	for _, p := range resp.Problems {
		if strings.HasPrefix(p.ID, "certificate-not-ready-") {
			cert.Problems = append(cert.Problems, p)
		}
	}
	if len(cert.Problems) != 1 {
		t.Fatalf("certificate problems: %d", len(cert.Problems))
	}
	write("certificate-stale", cert)

	// Objects the operator cannot update (without the made-up module of
	// the test, which no real cluster has).
	ac, _ := applyFailuresWorld(t)
	apply := diagnostics.CheckApplyFailures(ac)
	apply.Check.Name = components.ApplyFailuresCheckName
	kept := apply.Problems[:0]
	for _, p := range apply.Problems {
		if !strings.Contains(p.ID, "new-operator") {
			kept = append(kept, p)
		}
	}
	apply.Problems = kept
	write("apply-failures", apply)

	// An operator whose conversion webhook knows neither DataScienceCluster
	// v3 nor Platform v1alpha2 (a RHOAI 3.6 nightly, 2026-10-07).
	_, cc := conversionWorld(t, 500, liveDSCv3Conversion500)
	write("conversion-webhook", diagnostics.CheckConversionFailures(cc))

	// Left over from an earlier release (the live RHOAI 3.6.0 case): the
	// DSC reports the TrustyAI CR's stored booleans, the CR reports its
	// Deployment's old selector, and the module operator crash-loops.
	// The pod was created on a fixed date, so the checks measure its age
	// against a fixed now (age 10d) and the file is the same on any day.
	restore := diagnostics.SetWallClockForTest(func() time.Time { return time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC) })
	defer restore()
	lw := newLeftoverWorld(t, []string{msgStoredTypes}, []string{msgTrustySelector}, storedFalse())
	lw.f.json("GET", "/api/v1/namespaces/redhat-ods-applications/pods", 200, `{"items":[{"metadata":{"name":"trustyai-operator-module-controller-manager-6d9f7c8b5-x2x7q","namespace":"redhat-ods-applications","creationTimestamp":"2026-11-10T00:00:00Z","labels":{"pod-template-hash":"6d9f7c8b5"},
		"ownerReferences":[{"kind":"ReplicaSet","name":"trustyai-operator-module-controller-manager-6d9f7c8b5","controller":true}]},
		"status":{"phase":"Running","containerStatuses":[{"name":"manager","restartCount":1061,"state":{"waiting":{"reason":"CrashLoopBackOff"}},"lastState":{"terminated":{"reason":"Error","exitCode":1,"message":"`+msgInformer+`"}}}]}}]}`)
	lw.f.json("GET", "/api/v1/namespaces", 200, `{"items":[]}`)
	leftovers := diagnostics.CheckLeftovers(lw.c)
	leftovers.Check.Name = components.LeftoversCheckName
	pods := diagnostics.CheckRHOAIPods(lw.c)
	pods.Check.Name = "RHOAI pods"
	write("leftovers", leftovers, pods)
}
