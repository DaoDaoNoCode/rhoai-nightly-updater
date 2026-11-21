package cluster

import (
	"context"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/operator"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
)

// R5-F4/F8: the worst case of one operation (Dashboard Dev revert and every
// step inside OperationDeadline, then the automatic restore and the
// bookkeeping) fits in the shutdown drain, and the drain plus the HTTP
// server shutdown fits in terminationGracePeriodSeconds. The literals in
// main.go and pkg/api/auth_handlers.go are read from source, so a change there
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

	if !strings.Contains(read("../api/auth_handlers.go"), "context.WithTimeout(context.WithoutCancel(r.Context()), 15*time.Minute)") || OperationDeadline != 15*time.Minute {
		t.Fatal("the operation deadline in pkg/api/auth_handlers.go no longer matches OperationDeadline")
	}
	// A recipe run gives each step the same budget as one operation, and
	// its last record write runs beside the marker clear and is shorter,
	// so a draining pod's running step still fits the drain.
	runner := read("../api/recipe_runner.go")
	if !strings.Contains(runner, "context.WithTimeout(x.runCtx, cluster.OperationDeadline)") {
		t.Fatal("pkg/api/recipe_runner.go no longer gives each recipe step cluster.OperationDeadline")
	}
	if !strings.Contains(runner, "const recipeRecordTimeout = 5 * time.Second") || 5*time.Second > MarkerClearTimeout {
		t.Fatal("pkg/api/recipe_runner.go's record write is not bounded within the marker clear")
	}
	mainSrc := read("../../main.go")
	for _, want := range []string{
		"drainTimeout := cluster.ShutdownDrainTimeout()",
		"context.WithTimeout(context.Background(), cluster.MarkerFlushTimeout)",
		"context.WithTimeout(context.Background(), cluster.HTTPShutdownTimeout)",
	} {
		if !strings.Contains(mainSrc, want) {
			t.Fatalf("main.go no longer contains %q", want)
		}
	}
	apiMarker := read("../api/operation_marker.go")
	if !strings.Contains(apiMarker, "const markerWriteTimeout = 5 * time.Second") ||
		!strings.Contains(apiMarker, "context.WithTimeout(context.Background(), cluster.MarkerClearTimeout)") {
		t.Fatal("pkg/api/operation_marker.go no longer bounds the marker clear by cluster.MarkerClearTimeout with 5s writes")
	}
	// The clear may first wait for a background retry's 5s write.
	if MarkerClearTimeout < 2*5*time.Second || MarkerFlushTimeout < 2*5*time.Second {
		t.Fatalf("marker clear %s / flush %s cannot cover a pending retry write plus its own write", MarkerClearTimeout, MarkerFlushTimeout)
	}

	if RecoveryTimeout != record.RestoreCSVBudget+record.RestoreApplyTimeout {
		t.Fatalf("RecoveryTimeout %s is not the sum of its phases", RecoveryTimeout)
	}
	// After the deadline: the restore, then the activity entry and the
	// Subscription record (all capped together by postOperationContext),
	// then the marker clear.
	worst := OperationDeadline + record.PostDeadlineWork() + MarkerClearTimeout
	if ShutdownDrainTimeout() < worst {
		t.Fatalf("drain %s < worst case %s", ShutdownDrainTimeout(), worst)
	}
	total := ShutdownDrainTimeout() + MarkerFlushTimeout + HTTPShutdownTimeout
	if record.ShutdownMargin < 10*time.Second || total+record.ShutdownMargin > grace {
		t.Fatalf("drain %s + marker flush %s + HTTP shutdown %s = %s, plus a %s margin, exceeds terminationGracePeriodSeconds %s",
			ShutdownDrainTimeout(), MarkerFlushTimeout, HTTPShutdownTimeout, total, record.ShutdownMargin, grace)
	}
	if operator.InstallDeadlineReserve <= 0 || operator.InstallDeadlineReserve >= OperationDeadline {
		t.Fatalf("installDeadlineReserve = %s", operator.InstallDeadlineReserve)
	}
}

// R5-F8: the install wait ends on its own before the operation's deadline,
// whatever OperatorInstallTimeout says.
func TestInstallWaitEndsBeforeTheOperationDeadline(t *testing.T) {
	t.Cleanup(operator.SetInstallTimeoutForTest(time.Minute))
	f := newFakeOLM(t)
	f.sub = map[string]any{"metadata": map[string]any{"name": rhoai.SubName}, "spec": map[string]any{}, "status": map[string]any{}}
	ctx, cancel := kube.Clock().WithTimeout(context.Background(), operator.InstallDeadlineReserve+300*time.Millisecond)
	defer cancel()
	var logs []string
	start := kube.Clock().Now()
	out := operator.WaitForInstall(f.client(ctx), func(UpdateStepEvent) {}, &logs, &operator.Recovery{}, "")
	if out.ErrorCode != "install_timeout" || !strings.Contains(out.Message, "time budget") || kube.Clock().Since(start) > 300*time.Millisecond+operator.InstallPlanPollInterval {
		t.Fatalf("outcome = %+v after %s", out, kube.Clock().Since(start))
	}
}
