package cluster

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/diagnostics"
)

// The JobSet Command, pasted into a shell with an oc whose cert-manager
// step fails, never runs a JobSet command and does not end the shell;
// with every step working it runs both installs in order. The stub oc
// records its arguments and reads its here-document, and fails when
// FAIL_ON is in them.
func TestJobSetCommandStopsWhenCertManagerFails(t *testing.T) {
	w := newDSCWorld(t, liveMissingPrereqs()[:2])
	out := diagnostics.CheckDataScienceCluster(w.c)
	cmd := problemsByID(out)["prerequisite-missing-job-set"].TechnicalCmd
	if cmd == "" {
		t.Fatalf("problems = %v", ids(out))
	}
	bin := t.TempDir()
	stub := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$OC_LOG\"\ncat >> \"$OC_LOG\" 2>/dev/null </dev/stdin || true\ncase \"$*\" in *\"$FAIL_ON\"*) [ -n \"$FAIL_ON\" ] && exit 1;; esac\nexit 0\n"
	if err := os.WriteFile(filepath.Join(bin, "oc"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, shell := range []string{"bash", "zsh"} {
		path, err := exec.LookPath(shell)
		if err != nil {
			t.Logf("%s not installed: skipped", shell)
			continue
		}
		run := func(failOn string) (string, string) {
			log := filepath.Join(t.TempDir(), "oc.log")
			// The Command as pasted, then a line that shows the shell is
			// still there.
			c := exec.CommandContext(t.Context(), path, "-c", cmd+"\necho shell-still-open")
			c.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "OC_LOG="+log, "FAIL_ON="+failOn)
			c.Stdin = strings.NewReader("")
			outb, _ := c.CombinedOutput()
			b, _ := os.ReadFile(log)
			return string(b), string(outb)
		}
		t.Run(shell, func(t *testing.T) {
			// The wait for cert-manager's Subscription times out.
			ocLog, stdout := run("wait subscription/openshift-cert-manager-operator")
			if !strings.Contains(ocLog, "wait subscription/openshift-cert-manager-operator") || strings.Contains(ocLog, "jobset") ||
				!strings.Contains(stdout, "A step of the commands before Job Set Operator failed, so its install did not run.") || !strings.Contains(stdout, "shell-still-open") {
				t.Fatalf("oc calls:\n%s\noutput:\n%s", ocLog, stdout)
			}
			// Every install step works and JobSetOperator/cluster is missing:
			// cert-manager, then JobSet, then the operand is created.
			ocLog, stdout = run("get jobsetoperator.operator.openshift.io/cluster")
			cm, js := strings.Index(ocLog, "name: cert-manager-operator"), strings.Index(ocLog, "name: openshift-jobset-operator")
			create := strings.Index(ocLog, "create -f -\n{\n  \"apiVersion\": \"operator.openshift.io/v1\",\n  \"kind\": \"JobSetOperator\"")
			if cm < 0 || js < cm || create < js || strings.Contains(stdout, "did not run") || !strings.Contains(stdout, "shell-still-open") {
				t.Fatalf("oc calls:\n%s\noutput:\n%s", ocLog, stdout)
			}
		})
	}
}
