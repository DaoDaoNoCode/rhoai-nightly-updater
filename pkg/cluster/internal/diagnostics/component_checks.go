package diagnostics

import (
	"fmt"
	"sort"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/components"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/report"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
)

func CheckDataScienceCluster(c *kube.Client) CheckOutput {
	const name = "DataScienceCluster"
	out := CheckOutput{Check: report.CheckResult{Name: name, Status: "pass"}}
	details := []string{}
	// warn lowers a pass to warn; fail is kept.
	warn := func(detail string) {
		if out.Check.Status == "pass" {
			out.Check.Status = "warn"
		}
		details = append(details, detail)
	}

	// The DSCI is read first: a DSCI in Error often explains why no DSC
	// components deploy, so it is reported whatever the DSC state is.
	dscis, dsciAPI, dsciErr := rhoai.ListFirstServed(c, rhoai.DSCIGroup, "/apis/dscinitialization.opendatahub.io/%s/dscinitializations", "dscinitializations", rhoai.DSCIFallbackVersions)
	var dsciProblem *report.Problem
	if dsciErr == nil && dsciAPI && len(dscis) > 0 {
		dsci := dscis[0]
		if dsci.Status.Phase == "Error" {
			dsciProblem = &report.Problem{
				ID:              "dsci-error",
				Severity:        "warning",
				Title:           fmt.Sprintf("DSCInitialization %s is in phase Error", dsci.Metadata.Name),
				Description:     "The operator could not finish platform initialization (namespaces, monitoring, trusted CA bundle). Components may not deploy until this is fixed.",
				Evidence:        append([]string{fmt.Sprintf("DSCInitialization %s: phase=Error", dsci.Metadata.Name)}, components.FailingConditions(dsci.Status.Conditions)...),
				AffectedObjects: []string{"DSCInitialization " + dsci.Metadata.Name},
				Fix:             "Fix the cause named in the conditions; the operator retries automatically.",
				TechnicalCmd:    fmt.Sprintf("oc get dscinitialization %s -o jsonpath='{.status.conditions}'", dsci.Metadata.Name),
			}
		}
	}

	dscs, apiFound, err := rhoai.ListFirstServed(c, rhoai.DSCGroup, rhoai.DSCListFmt, "datascienceclusters", rhoai.DSCFallbackVersions)
	switch {
	case err != nil:
		warn(fmt.Sprintf("could not verify the DataScienceCluster: %v", err))
	case !apiFound:
		details = append(details, "No DataScienceCluster API (operator not installed yet)")
	case len(dscs) == 0:
		warn("No DataScienceCluster exists")
		out.Problems = append(out.Problems, report.Problem{
			ID:          "dsc-missing",
			Severity:    "info",
			Title:       "No DataScienceCluster exists",
			Description: "The operator is installed, but no DataScienceCluster exists, so no RHOAI components are deployed.",
			Fix:         "Create the DataScienceCluster from the Status page.",
		})
	default:
		dsc := dscs[0]
		a := components.AnalyzeDSC(c, dsc, wallNow())
		titles := map[string]string{}
		var causeProblems []report.Problem
		for _, d := range a.Deps {
			if d.ProblemID() == "" {
				continue
			}
			p := components.PrerequisiteProblem(c, d)
			causeProblems = append(causeProblems, p)
		}
		for _, b := range a.Backoffs {
			causeProblems = append(causeProblems, b.Problem())
		}
		if gates := a.Gates(); len(gates) > 0 {
			causeProblems = append(causeProblems, components.GateProblem(c, gates))
		}
		for _, p := range causeProblems {
			titles[p.ID] = p.Title
		}
		if dsciProblem != nil {
			titles[dsciProblem.ID] = dsciProblem.Title
		}

		ready := a.Ready
		switch {
		case ready == nil:
			warn(fmt.Sprintf("%s has no Ready condition yet (the operator has not reconciled it)", dsc.Metadata.Name))
		case ready.Status == "True":
			details = append(details, fmt.Sprintf("%s is Ready", dsc.Metadata.Name))
			var optional []string
			for _, d := range a.Deps {
				if !d.Satisfied() && !d.Blocking() {
					optional = append(optional, d.DisplayName())
				}
			}
			if len(optional) > 0 {
				details = append(details, "optional prerequisites not installed: "+strings.Join(optional, ", "))
			}
		default:
			out.Check.Status = "fail"
			p, failing := a.NotReadyProblem(dsc, titles, dsciProblem)
			details = append(details, fmt.Sprintf("%s is not ready (%s)", dsc.Metadata.Name, kube.CountNoun(failing, "failing condition", "failing conditions")))
			out.Problems = append(out.Problems, p)
		}
		for _, p := range causeProblems {
			if p.Severity != "info" && out.Check.Status == "pass" {
				out.Check.Status = "warn"
			}
		}
		out.Problems = append(out.Problems, causeProblems...)
		if a.ModulesErr != nil {
			warn(fmt.Sprintf("module CRs not fully checked: %v", a.ModulesErr))
		}
	}

	switch {
	case dsciErr != nil:
		warn(fmt.Sprintf("could not verify the DSCInitialization: %v", dsciErr))
	case !dsciAPI || len(dscis) == 0:
		details = append(details, "no DSCInitialization")
	default:
		if dsciProblem != nil {
			out.Check.Status = "fail"
			out.Problems = append(out.Problems, *dsciProblem)
		}
		details = append(details, fmt.Sprintf("DSCInitialization %s phase %s", dscis[0].Metadata.Name, kube.NonEmpty(dscis[0].Status.Phase, "unknown")))
	}

	out.Check.Detail = strings.Join(details, "; ")
	return out
}

func CheckApplyFailures(c *kube.Client) CheckOutput {
	out := CheckOutput{Check: report.CheckResult{Name: components.ApplyFailuresCheckName, Status: "pass"}}
	s := components.ScanApplyReporters(c, components.AllApplyFailures)
	found, api, errs, scanned := s.Found, s.API, s.Errs, s.Scanned

	keys := make([]string, 0, len(found))
	for k, rf := range found {
		if !rf.Leftover() {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	operators := components.NewModuleOperatorFinder(c, nil)
	for _, k := range keys {
		out.Problems = append(out.Problems, components.ApplyFailureProblem(c, api, operators, found[k]))
	}

	switch {
	case len(out.Problems) > 0:
		out.Check.Status = "fail"
		out.Check.Detail = fmt.Sprintf("%s the operator cannot update", kube.CountNoun(len(out.Problems), "object", "objects"))
	case len(errs) > 0:
		out.Check.Status = "warn"
		out.Check.Detail = "No apply failures in the CRs that could be read"
	default:
		out.Check.Detail = fmt.Sprintf("No apply failures reported (%s checked)", kube.CountNoun(scanned, "CR", "CRs"))
	}
	if len(errs) > 0 {
		out.Check.Detail += "; could not read: " + strings.Join(errs, "; ")
	}
	return out
}
