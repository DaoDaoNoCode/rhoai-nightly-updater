package cluster

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// DataScienceCluster / DSCInitialization readiness.
//
// The operator rolls component and module health up into the DSC "Ready"
// condition. Conditions with severity "Info" are informational (for example
// a component set to Removed, or an optional dependency that is not
// installed) and do not participate in readiness: "Only dependents with
// ConditionSeverityError (or empty severity, which defaults to Error)
// participate in happiness computation. Conditions with
// ConditionSeverityInfo are ignored." (odh-platform-utilities
// pkg/controller/conditions/conditions.go). The CRD schema says severity
// "defaults to Error" when unset. DSCInitialization reports status.phase
// Ready, Progressing or Error (rhods-operator rhoai-3.6
// internal/controller/status/status.go).

type dscCondition struct {
	Type     string `json:"type"`
	Status   string `json:"status"`
	Reason   string `json:"reason"`
	Message  string `json:"message"`
	Severity string `json:"severity"`
}

type dscObject struct {
	APIVersion string `json:"apiVersion"`
	Metadata   struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Status struct {
		Phase      string         `json:"phase"`
		Conditions []dscCondition `json:"conditions"`
	} `json:"status"`
}

// listFirstServed lists a collection with the newest served version and
// returns the items. pathFmt is the collection path with %s for the
// version. apiFound is false when no version is served (no CRD).
func listFirstServed(c *Client, pathFmt, resource string) (items []dscObject, apiFound bool, err error) {
	for _, version := range []string{"v2", "v1"} {
		body, _, err := c.get(fmt.Sprintf(pathFmt, version))
		if IsK8sError(err, http.StatusNotFound) {
			continue
		}
		if err != nil {
			return nil, true, err
		}
		var list struct {
			Items []dscObject `json:"items"`
		}
		if err := json.Unmarshal(body, &list); err != nil {
			return nil, true, fmt.Errorf("parse %s: %w", resource, err)
		}
		return list.Items, true, nil
	}
	return nil, false, nil
}

// failingConditions returns the non-Info conditions that are not True,
// excluding the Ready roll-up itself. Duplicate messages are reported once.
func failingConditions(conds []dscCondition) []string {
	seen := map[string]bool{}
	var out []string
	for _, cond := range conds {
		if cond.Type == "Ready" || cond.Type == "Progressing" || strings.EqualFold(cond.Severity, "Info") {
			continue
		}
		failing := cond.Status != "True"
		if cond.Type == "Degraded" {
			// The one negative-polarity condition (DSCI): True is bad.
			failing = cond.Status == "True"
		}
		if !failing {
			continue
		}
		msg := truncate(cond.Message, 300)
		if seen[msg] && msg != "" {
			continue
		}
		seen[msg] = true
		line := fmt.Sprintf("%s=%s", cond.Type, cond.Status)
		if cond.Reason != "" {
			line += fmt.Sprintf(" (%s)", cond.Reason)
		}
		if msg != "" {
			line += ": " + msg
		}
		out = append(out, line)
	}
	sort.Strings(out)
	return out
}

func checkDataScienceCluster(c *Client) checkOutput {
	const name = "DataScienceCluster"
	out := checkOutput{check: CheckResult{Name: name, Status: "pass"}}
	details := []string{}
	// warn lowers a pass to warn; fail is kept.
	warn := func(detail string) {
		if out.check.Status == "pass" {
			out.check.Status = "warn"
		}
		details = append(details, detail)
	}

	// The DSC and the DSCI are evaluated independently: a DSCI in Error
	// often explains why no DSC components deploy, so it is reported
	// whatever the DSC state is.
	dscs, apiFound, err := listFirstServed(c, "/apis/datasciencecluster.opendatahub.io/%s/datascienceclusters", "datascienceclusters")
	switch {
	case err != nil:
		warn(fmt.Sprintf("could not verify the DataScienceCluster: %v", err))
	case !apiFound:
		details = append(details, "No DataScienceCluster API (operator not installed yet)")
	case len(dscs) == 0:
		warn("No DataScienceCluster exists")
		out.problems = append(out.problems, Problem{
			ID:          "dsc-missing",
			Severity:    "info",
			Title:       "No DataScienceCluster exists",
			Description: "The operator is installed, but no DataScienceCluster exists, so no RHOAI components are deployed.",
			Fix:         "Create the DataScienceCluster from the Status page.",
		})
	default:
		dsc := dscs[0]
		var ready *dscCondition
		for i := range dsc.Status.Conditions {
			if dsc.Status.Conditions[i].Type == "Ready" {
				ready = &dsc.Status.Conditions[i]
			}
		}
		switch {
		case ready == nil:
			warn(fmt.Sprintf("%s has no Ready condition yet (the operator has not reconciled it)", dsc.Metadata.Name))
		case ready.Status == "True":
			details = append(details, fmt.Sprintf("%s is Ready", dsc.Metadata.Name))
		default:
			failing := failingConditions(dsc.Status.Conditions)
			evidence := []string{fmt.Sprintf("DataScienceCluster %s: Ready=%s (%s): %s", dsc.Metadata.Name, ready.Status, ready.Reason, truncate(ready.Message, 400))}
			evidence = append(evidence, failing...)
			out.check.Status = "fail"
			details = append(details, fmt.Sprintf("%s is not ready (%d failing condition(s))", dsc.Metadata.Name, len(failing)))
			out.problems = append(out.problems, Problem{
				ID:       "dsc-not-ready",
				Severity: "warning",
				Title:    fmt.Sprintf("DataScienceCluster %s is not ready", dsc.Metadata.Name),
				Description: "The operator reports that some components or modules failed to deploy or are unhealthy. " +
					"The conditions below are the operator's own messages. Conditions with severity Info (for example components set to Removed) are not listed.",
				Evidence:        evidence,
				AffectedObjects: []string{"DataScienceCluster " + dsc.Metadata.Name},
				Fix:             "Fix the cause named in each condition (for example install a missing dependency operator, or set an unused component to Removed). The Components page shows the per-component status.",
				TechnicalCmd:    fmt.Sprintf("oc get datasciencecluster %s -o jsonpath='{range .status.conditions[*]}{.type}={.status} {.reason}: {.message}{\"\\n\"}{end}'", dsc.Metadata.Name),
			})
		}
	}

	dscis, dsciAPI, dsciErr := listFirstServed(c, "/apis/dscinitialization.opendatahub.io/%s/dscinitializations", "dscinitializations")
	switch {
	case dsciErr != nil:
		warn(fmt.Sprintf("could not verify the DSCInitialization: %v", dsciErr))
	case !dsciAPI || len(dscis) == 0:
		details = append(details, "no DSCInitialization")
	default:
		dsci := dscis[0]
		if dsci.Status.Phase == "Error" {
			failing := failingConditions(dsci.Status.Conditions)
			out.check.Status = "fail"
			out.problems = append(out.problems, Problem{
				ID:              "dsci-error",
				Severity:        "warning",
				Title:           fmt.Sprintf("DSCInitialization %s is in phase Error", dsci.Metadata.Name),
				Description:     "The operator could not finish platform initialization (namespaces, monitoring, trusted CA bundle). Components may not deploy until this is fixed.",
				Evidence:        append([]string{fmt.Sprintf("DSCInitialization %s: phase=Error", dsci.Metadata.Name)}, failing...),
				AffectedObjects: []string{"DSCInitialization " + dsci.Metadata.Name},
				Fix:             "Fix the cause named in the conditions; the operator retries automatically.",
				TechnicalCmd:    fmt.Sprintf("oc get dscinitialization %s -o jsonpath='{.status.conditions}'", dsci.Metadata.Name),
			})
		}
		details = append(details, fmt.Sprintf("DSCInitialization %s phase %s", dsci.Metadata.Name, nonEmpty(dsci.Status.Phase, "unknown")))
	}

	out.check.Detail = strings.Join(details, "; ")
	return out
}
