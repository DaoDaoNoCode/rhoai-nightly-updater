package cluster

import (
	"context"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// R5-F4/F8: the worst case of one operation (Dashboard Dev revert and every
// step inside OperationDeadline, then the automatic restore and the
// bookkeeping) fits in the shutdown drain, and the drain plus the HTTP
// server shutdown fits in terminationGracePeriodSeconds. The literals in
// main.go and pkg/api/handlers.go are read from source, so a change there
// that breaks the arithmetic fails here.
func TestShutdownBudgetFitsTerminationGracePeriod(t *testing.T) {
	read := func(path string) string {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	m := regexp.MustCompile(`terminationGracePeriodSeconds:\s*(\d+)`).FindStringSubmatch(read(templateRel))
	if m == nil {
		t.Fatal("terminationGracePeriodSeconds not found in the template")
	}
	secs, _ := strconv.Atoi(m[1])
	grace := time.Duration(secs) * time.Second

	if !strings.Contains(read("../api/handlers.go"), "context.WithTimeout(context.WithoutCancel(r.Context()), 15*time.Minute)") || OperationDeadline != 15*time.Minute {
		t.Fatal("the operation deadline in pkg/api/handlers.go no longer matches OperationDeadline")
	}
	mainSrc := read("../../main.go")
	if !strings.Contains(mainSrc, "drainTimeout := cluster.ShutdownDrainTimeout()") {
		t.Fatal("main.go no longer derives the drain from cluster.ShutdownDrainTimeout")
	}
	sm := regexp.MustCompile(`shutdownCtx, cancel := context.WithTimeout\(context.Background\(\), (\d+)\*time.Second\)`).FindStringSubmatch(mainSrc)
	if sm == nil {
		t.Fatal("HTTP shutdown timeout not found in main.go")
	}
	shutdownSecs, _ := strconv.Atoi(sm[1])
	shutdown := time.Duration(shutdownSecs) * time.Second

	if RecoveryTimeout != restoreCSVBudget+restoreApplyTimeout {
		t.Fatalf("RecoveryTimeout %s is not the sum of its phases", RecoveryTimeout)
	}
	worst := OperationDeadline + RecoveryTimeout + postOperationBookkeeping
	if ShutdownDrainTimeout() < worst {
		t.Fatalf("drain %s < worst case %s", ShutdownDrainTimeout(), worst)
	}
	const margin = 10 * time.Second // SIGKILL comes exactly at the grace period
	if ShutdownDrainTimeout()+shutdown+margin > grace {
		t.Fatalf("drain %s + shutdown %s + %s margin exceeds terminationGracePeriodSeconds %s", ShutdownDrainTimeout(), shutdown, margin, grace)
	}
	if installDeadlineReserve <= 0 || installDeadlineReserve >= OperationDeadline {
		t.Fatalf("installDeadlineReserve = %s", installDeadlineReserve)
	}
}

// R5-F8: the install wait ends on its own before the operation's deadline,
// whatever OperatorInstallTimeout says.
func TestInstallWaitEndsBeforeTheOperationDeadline(t *testing.T) {
	old := OperatorInstallTimeout
	OperatorInstallTimeout = time.Minute
	t.Cleanup(func() { OperatorInstallTimeout = old })
	f := newFakeOLM(t)
	f.sub = map[string]interface{}{"metadata": map[string]interface{}{"name": SubName}, "spec": map[string]interface{}{}, "status": map[string]interface{}{}}
	ctx, cancel := context.WithTimeout(context.Background(), installDeadlineReserve+300*time.Millisecond)
	defer cancel()
	var logs []string
	start := time.Now()
	out := waitForOperatorInstall(f.client(ctx), "verify_installplan", func(UpdateStepEvent) {}, &logs, &operatorRecovery{}, "")
	if out.errorCode != "install_timeout" || !strings.Contains(out.message, "time budget") || time.Since(start) > 5*time.Second {
		t.Fatalf("outcome = %+v after %s", out, time.Since(start))
	}
}
