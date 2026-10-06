package api

import (
	"log/slog"
	"net/http"
	"os"
	"runtime"
)

// Build information, set at link time:
//
//	go build -ldflags "-X github.com/juntwang/rhoai-nightly-updater/pkg/api.Commit=$(git rev-parse HEAD) ..."
var (
	Version   = "dev"
	Commit    = "unknown"
	BuildDate = "unknown"
)

// ExpectedTemplateRevision is the deploy/template.yaml revision this binary
// was written for. The template passes its own revision in the
// TEMPLATE_REVISION env var; bump both together whenever a template change
// matters to the running code (probes, listeners, strategy, timeouts, RBAC).
// An install whose template predates the variable reports revision "".
const ExpectedTemplateRevision = "4"

// templateRevision reads the revision of the template that deployed us.
func templateRevision() string {
	return os.Getenv("TEMPLATE_REVISION")
}

// VersionInfo describes the running build and whether the Deployment that
// runs it was created from the template this build expects.
type VersionInfo struct {
	Version                  string `json:"version"`
	Commit                   string `json:"commit"`
	BuildDate                string `json:"buildDate"`
	GoVersion                string `json:"goVersion"`
	TemplateRevision         string `json:"templateRevision"`
	ExpectedTemplateRevision string `json:"expectedTemplateRevision"`
	// TemplateOutdated is true for in-cluster installs whose Deployment was
	// not re-applied from the current template (run `make upgrade`).
	TemplateOutdated bool `json:"templateOutdated"`
}

func currentVersion() VersionInfo {
	rev := templateRevision()
	return VersionInfo{
		Version:                  Version,
		Commit:                   Commit,
		BuildDate:                BuildDate,
		GoVersion:                runtime.Version(),
		TemplateRevision:         rev,
		ExpectedTemplateRevision: ExpectedTemplateRevision,
		TemplateOutdated:         runningInCluster() && rev != ExpectedTemplateRevision,
	}
}

// runningInCluster reports whether a ServiceAccount token is mounted.
func runningInCluster() bool {
	_, err := os.Stat(serviceAccountTokenPath)
	return err == nil
}

// HandleVersion returns the build and deployment template information.
func HandleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, currentVersion(), "version")
}

// LogStartupVersion logs the build and warns when the Deployment template is
// older than this build expects.
func LogStartupVersion() {
	v := currentVersion()
	slog.Info("build", "version", v.Version, "commit", v.Commit, "buildDate", v.BuildDate, "go", v.GoVersion, "templateRevision", v.TemplateRevision)
	if v.TemplateOutdated {
		slog.Warn("the Deployment was created from an older deploy/template.yaml; re-apply it with `make upgrade`",
			"templateRevision", v.TemplateRevision, "expected", v.ExpectedTemplateRevision)
	}
}
