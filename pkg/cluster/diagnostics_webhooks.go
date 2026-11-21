package cluster

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The "Stale webhooks" diagnostics check and the delete-stale-webhooks fix.
// Detection lives in stale_webhooks.go (RHOAI operator notes §3.4); this
// file only turns its verdicts into problems and fix results.

type staleConversion struct {
	deadConversion
	Module      string // DSC component the Service name points to, if any
	ModuleState string
	Objects     crdObjects
	// ObjectsUnread says why Objects is crdObjectsUnknown.
	ObjectsUnread string
}

// crdObjects is whether a CRD holds objects.
type crdObjects int

const (
	crdObjectsUnknown crdObjects = iota
	crdObjectsNone
	crdObjectsPresent
)

// probeCRDObjects lists at most one object of the CRD in every namespace.
// An empty list needs no conversion, so it answers even with the
// conversion webhook down; a list that fails on the conversion webhook
// proves an object exists (only objects are converted). This app may lack
// the right to list the kind (403): then it cannot tell.
func probeCRDObjects(c *Client, listPath string) (crdObjects, string) {
	if listPath == "" {
		return crdObjectsUnknown, "the CRD serves no version"
	}
	body, _, err := c.do(http.MethodGet, listPath, "", nil, url.Values{"limit": {"1"}})
	switch {
	case IsK8sError(err, http.StatusForbidden):
		return crdObjectsUnknown, "this app may not list them"
	case err != nil && strings.Contains(err.Error(), "conversion webhook"):
		return crdObjectsPresent, ""
	case err != nil:
		return crdObjectsUnknown, err.Error()
	}
	var list struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return crdObjectsUnknown, "parse: " + err.Error()
	}
	if len(list.Items) == 0 {
		return crdObjectsNone, ""
	}
	return crdObjectsPresent, ""
}

// findStaleConversions reports RHOAI CRDs whose conversion webhook Service is
// missing, or has had no ready endpoint for longer than the grace period.
func findStaleConversions(c *Client) ([]staleConversion, []string, error) {
	crds, err := listRHOAIConversionCRDs(c)
	if isK8sNotFound(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	// RHOAI's: a CRD of an opendatahub.io API group, or one whose Service
	// runs in the operator, applications or monitoring namespace.
	rhoaiNS, nsErr := rhoaiPodNamespaces(c)
	rhoaiNS = append(rhoaiNS, SubNS)
	var candidates []conversionCRD
	for _, crd := range crds {
		ref := crd.conversionService()
		if ref == "" {
			continue
		}
		if ns, _, _ := strings.Cut(ref, "/"); !slices.Contains(rhoaiNS, ns) && !strings.Contains(crd.Metadata.Name, "opendatahub") {
			continue
		}
		candidates = append(candidates, crd)
	}
	dead, unknown := deadConversions(candidates, newServiceHealthCache(c, staleServiceGrace))
	var errs []string
	if nsErr != nil {
		errs = append(errs, fmt.Sprintf("the applications and monitoring namespaces are not known, so CRDs whose conversion Service runs there may be missing: %v", nsErr))
	}
	for _, u := range unknown {
		errs = append(errs, fmt.Sprintf("conversion Service of CRD %s: %v", u.CRD, u.Health.err))
	}
	if len(dead) == 0 {
		return nil, errs, nil
	}
	components := readDSCComponents(c)
	out := make([]staleConversion, 0, len(dead))
	for _, d := range dead {
		sc := staleConversion{deadConversion: d}
		_, svcName, _ := strings.Cut(d.Service, "/")
		normalized := strings.ReplaceAll(svcName, "-", "")
		for name, state := range components {
			if len(name) > len(sc.Module) && strings.Contains(normalized, name) {
				sc.Module, sc.ModuleState = name, state
			}
		}
		out = append(out, sc)
	}
	parallelFor(len(out), 6, func(i int) {
		out[i].Objects, out[i].ObjectsUnread = probeCRDObjects(c, out[i].ListPath)
	})
	return out, errs, nil
}

// readDSCComponents returns spec.components managementState by component
// name, or nil when there is no DataScienceCluster.
func readDSCComponents(c *Client) map[string]string {
	path, err := dataScienceClusterPath(c)
	if err != nil {
		return nil
	}
	body, _, err := c.get(path)
	if err != nil {
		return nil
	}
	var dsc struct {
		Spec struct {
			Components map[string]struct {
				ManagementState string `json:"managementState"`
			} `json:"components"`
		} `json:"spec"`
	}
	if json.Unmarshal(body, &dsc) != nil {
		return nil
	}
	out := map[string]string{}
	for k, v := range dsc.Spec.Components {
		out[k] = v.ManagementState
	}
	return out
}

func checkStaleWebhooks(c *Client) checkOutput {
	const name = "Stale webhooks"
	out := checkOutput{check: CheckResult{Name: name, Status: "pass"}}
	var conversions []staleConversion
	var convErrs []string
	var convErr error
	convDone := make(chan struct{})
	go func() {
		defer close(convDone)
		conversions, convErrs, convErr = findStaleConversions(c)
	}()
	scan := scanStaleWebhooks(c)
	<-convDone

	var deletable, guidance []webhookVerdict
	for _, v := range scan.Verdicts {
		if v.Deletable {
			deletable = append(deletable, v)
		} else {
			guidance = append(guidance, v)
		}
	}

	if len(deletable) > 0 {
		severity := "warning"
		var evidence, affected, lines []string
		for _, v := range deletable {
			if v.Config.blocking() {
				severity = "critical"
			}
			evidence = append(evidence, fmt.Sprintf("%s: Service %s; %s", v.Config.ref(), v.downText(), v.Reason))
			affected = append(affected, v.Config.ref())
			lines = append(lines, fmt.Sprintf("- %s (Service %s; %s)", v.Config.ref(), v.downText(), v.Reason))
		}
		out.problems = append(out.problems, Problem{
			ID:       "stale-webhooks",
			Severity: severity,
			Title:    fmt.Sprintf("%s %s to a Service that cannot serve %s", countNoun(len(deletable), "leftover RHOAI webhook configuration", "leftover RHOAI webhook configurations"), verb(len(deletable), "points", "point"), verb(len(deletable), "it", "them")),
			Description: "The API server calls these webhooks, but nothing serves them and their owner is gone, so nothing will recreate them. " +
				"With failurePolicy Fail (the default), every matching request is rejected until the configuration is deleted.",
			Evidence: evidence,
			Fix:      "Delete these leftover webhook configurations. Only configurations whose Services are all missing or without ready endpoints, and whose owner (CSV or module) no longer exists, are deleted.",
			LearnMore: "OLM deletes the webhooks of a CSV when the CSV is deleted, and recreates them while the CSV exists. " +
				"rhods-operator re-applies a module's webhooks while the module exists. A configuration is a leftover only when both are gone.",
			AutoFixable:     true,
			AutoFixAction:   "delete-stale-webhooks",
			AffectedObjects: affected,
			ConfirmMessage: "This deletes these webhook configurations:\n" + strings.Join(lines, "\n") +
				"\n\nRight before deleting, each one is checked again (Services, owner, no operator upgrade in progress) and deleted only if it is unchanged since that check.",
			TechnicalCmd: "oc get validatingwebhookconfigurations,mutatingwebhookconfigurations -o custom-columns=NAME:.metadata.name,SERVICE:.webhooks[*].clientConfig.service.name",
		})
	}

	if len(guidance) > 0 {
		// failurePolicy Ignore webhooks are skipped by the API server when
		// they cannot be called, so they only lose their effect.
		severity := "info"
		var evidence, affected []string
		for _, v := range guidance {
			if v.Config.blocking() {
				severity = "critical"
			}
			policy := "failurePolicy Fail: matching requests are rejected"
			if !v.Config.blocking() {
				policy = "failurePolicy Ignore: requests are not blocked, but the webhook has no effect"
			}
			evidence = append(evidence, fmt.Sprintf("%s: Service %s (%s); not deleted automatically because %s", v.Config.ref(), v.downText(), policy, v.Reason))
			affected = append(affected, v.Config.ref())
		}
		out.problems = append(out.problems, Problem{
			ID:       "webhook-service-missing",
			Severity: severity,
			Title:    fmt.Sprintf("%s %s a Service that cannot serve %s", countNoun(len(guidance), "RHOAI webhook configuration", "RHOAI webhook configurations"), verb(len(guidance), "calls", "call"), verb(len(guidance), "it", "them")),
			Description: "Requests that these webhooks intercept are rejected while their Service is missing or has no ready pods. Their owner still exists (or is unknown), " +
				"so deleting them would not help: the owner recreates them, or they may still be needed.",
			Evidence:        evidence,
			AffectedObjects: affected,
			Fix:             "Get the owner running again: check the Operator pods and RHOAI pods results and the operator logs. During an operator upgrade, wait for it to finish.",
			TechnicalCmd:    podsCommand(c),
		})
	}

	if len(conversions) > 0 {
		var evidence, affected []string
		for _, sc := range conversions {
			line := fmt.Sprintf("CRD %s: conversion Service %s (storedVersions %s)", sc.CRD, sc.Health.describe(sc.Service), strings.Join(sc.StoredVersions, ", "))
			if sc.Module != "" {
				line += fmt.Sprintf("; the Service belongs to DSC component %s, which is %s", sc.Module, nonEmpty(sc.ModuleState, "not set"))
			}
			switch sc.Objects {
			case crdObjectsNone:
				line += "; it holds no objects"
			case crdObjectsPresent:
				line += "; it holds objects"
			default:
				line += "; could not check whether it holds objects: " + sc.ObjectsUnread
			}
			evidence = append(evidence, line)
			affected = append(affected, "CustomResourceDefinition "+sc.CRD)
		}
		fix, technicalCmd := staleConversionFix(conversions)
		out.problems = append(out.problems, Problem{
			ID:       "stale-crd-conversion",
			Severity: "critical",
			Title:    fmt.Sprintf("%s %s a conversion webhook whose Service cannot serve", countNoun(len(conversions), "CRD", "CRDs"), verb(len(conversions), "uses", "use")),
			Description: "Reading or writing these objects at a version other than the one they are stored in fails, which also breaks garbage collection " +
				"and makes namespace deletion hang in Terminating. The tool does not change this automatically: switching the CRD to conversion strategy None changes how stored objects are read.",
			Evidence:        evidence,
			AffectedObjects: affected,
			Fix:             fix,
			LearnMore:       "OLM's own source notes that a conversion webhook without its Service makes all requests for the CRD's objects fail and \"ultimately breaks kubernetes garbage collection\" (operator-lifecycle-manager olm/operator.go).",
			TechnicalCmd:    technicalCmd,
		})
	}

	var details []string
	switch {
	case len(scan.Verdicts) == 0 && len(conversions) == 0:
		details = append(details, "No RHOAI webhook or CRD conversion points to a Service that cannot serve it")
	default:
		out.check.Status = "fail"
		details = append(details, fmt.Sprintf("%s and %s point to a Service that cannot serve them", countNoun(len(scan.Verdicts), "webhook configuration", "webhook configurations"), countNoun(len(conversions), "CRD conversion", "CRD conversions")))
	}
	if scan.Upgrading != "" {
		details = append(details, "webhook configurations not checked while an operator install is in progress ("+scan.Upgrading+")")
	}
	if errs := append(append([]string{}, scan.Errors...), convErrs...); len(errs) > 0 {
		details = append(details, "could not verify: "+strings.Join(errs, "; "))
		if out.check.Status == "pass" {
			out.check.Status = "warn"
		}
	}
	if convErr != nil {
		msg := fmt.Sprintf("CRD conversions not checked: %v", convErr)
		if IsK8sError(convErr, http.StatusForbidden) {
			msg = "CRD conversions not checked: this app cannot list CRDs (RBAC)"
		}
		details = append(details, msg)
		if out.check.Status == "pass" {
			out.check.Status = "warn"
		}
	}
	out.check.Detail = strings.Join(details, "; ")
	return out
}

// staleConversionFix is the stale-crd-conversion problem's advice and its
// command. Removing a component leaves its CRDs (they get no owner) with
// the conversion webhook its operator served, so setting the component to
// Managed and back to Removed ends in the same state. A CRD that holds no
// objects can be deleted instead: nothing uses it, and the operator
// applies it again when its component is set to Managed. A CRD with
// objects, whose component is on, or with no known component (the
// operator's own CRDs, which OLM applies) needs its Service back.
func staleConversionFix(conversions []staleConversion) (fix, technicalCmd string) {
	var empty, unknown, held []string
	var emptyOwners, heldOwners []string
	for _, sc := range conversions {
		on := sc.ModuleState == "Managed" || sc.ModuleState == "Unmanaged"
		switch {
		case on || sc.Module == "" || sc.Objects == crdObjectsPresent:
			held = append(held, sc.CRD)
			if sc.ModuleState == "Removed" && !slices.Contains(heldOwners, sc.Module) {
				heldOwners = append(heldOwners, sc.Module)
			}
		case sc.Objects == crdObjectsNone:
			empty = append(empty, sc.CRD)
			if !slices.Contains(emptyOwners, sc.Module) {
				emptyOwners = append(emptyOwners, sc.Module)
			}
		default:
			unknown = append(unknown, sc.CRD)
			if sc.ModuleState == "Removed" && !slices.Contains(heldOwners, sc.Module) {
				heldOwners = append(heldOwners, sc.Module)
			}
		}
	}
	here := func(owners []string) string {
		if len(owners) == 0 {
			return ""
		}
		return " (here: " + strings.Join(owners, ", ") + ")"
	}
	restore := "bring back the Service that serves the conversion, usually by setting the component that owns it to Managed again" + here(heldOwners) +
		". Then, if you do not need the objects, delete them, then the CRDs, before you set the component back to Removed: removing a component leaves its CRDs with this conversion webhook. " +
		"Do not delete namespaces that contain these objects before this is fixed."
	var parts []string
	if len(empty) > 0 {
		parts = append(parts, fmt.Sprintf("%s %s no objects, so %s can be deleted: `oc delete crd %s`. The operator applies %s again when the component%s is set to Managed again.",
			verb(len(empty), "CRD", "CRDs")+" "+strings.Join(empty, ", "), verb(len(empty), "holds", "hold"), verb(len(empty), "it", "they"),
			strings.Join(empty, " "), verb(len(empty), "it", "them"), here(emptyOwners)))
	}
	if len(unknown) > 0 {
		parts = append(parts, fmt.Sprintf("This app could not check whether %s %s objects. Check with `oc get %s -A`: if it lists none, delete %s (`oc delete crd %s`); otherwise %s",
			verb(len(unknown), "CRD", "CRDs")+" "+strings.Join(unknown, ", "), verb(len(unknown), "holds", "hold"), strings.Join(unknown, ","),
			verb(len(unknown), "it", "them"), strings.Join(unknown, " "), restore))
	}
	if len(held) > 0 {
		parts = append(parts, fmt.Sprintf("For %s: %s", verb(len(held), "CRD", "CRDs")+" "+strings.Join(held, ", "), restore))
	}
	switch {
	case len(held) > 0:
		technicalCmd = "oc get crd -o custom-columns=NAME:.metadata.name,STRATEGY:.spec.conversion.strategy,SERVICE:.spec.conversion.webhook.clientConfig.service.name"
	case len(unknown) > 0:
		technicalCmd = "oc get " + strings.Join(unknown, ",") + " -A"
	default:
		technicalCmd = "oc delete crd " + strings.Join(empty, " ")
	}
	return strings.Join(parts, " "), technicalCmd
}

// deleteExact deletes an object only if it still has the UID and
// resourceVersion that were inspected (DeleteOptions preconditions).
func deleteExact(c *Client, path, uid, resourceVersion string) error {
	opts := map[string]any{"apiVersion": "v1", "kind": "DeleteOptions"}
	pre := map[string]string{}
	if uid != "" {
		pre["uid"] = uid
	}
	if resourceVersion != "" {
		pre["resourceVersion"] = resourceVersion
	}
	if len(pre) > 0 {
		opts["preconditions"] = pre
	}
	body, _ := json.Marshal(opts)
	_, _, err := c.do(http.MethodDelete, path, "application/json", body, nil)
	return err
}

// applyFixDeleteStaleWebhooks re-evaluates every RHOAI webhook configuration
// and deletes only the deletable stale ones, each guarded by its UID and
// resourceVersion, so a configuration that changed since the check is kept.
func applyFixDeleteStaleWebhooks(c *Client) (*types.OperationResponse, error) {
	scan := scanStaleWebhooks(c)
	if scan.Upgrading != "" {
		return nothingToDo("an operator install is in progress ("+scan.Upgrading+"); OLM and the operator recreate webhooks when it finishes. Nothing was deleted.", nil), nil
	}
	d := deleteStaleWebhookConfigs(c, scan.Verdicts)
	logs := d.Logs
	for _, e := range scan.Errors {
		logs = append(logs, "Warning: "+e)
	}

	if len(d.Deleted) > 0 {
		RecordActivity(c, types.ActivityEntry{
			Timestamp: nowRFC3339(),
			User:      getUser(c),
			Action:    "fix-delete-stale-webhooks",
			Detail:    "deleted leftover webhook configurations: " + strings.Join(d.Deleted, ", "),
			Success:   len(d.Failed) == 0,
		})
	}

	switch {
	case len(d.Deleted) == 0 && len(d.Failed) == 0 && len(d.Changed) > 0:
		return &types.OperationResponse{Success: false, Message: fmt.Sprintf("%s changed after the check, so nothing was deleted. Run diagnostics again.", strings.Join(d.Changed, ", ")), Logs: logs, ErrorCode: "conflict"}, nil
	case len(d.Deleted) == 0 && len(d.Failed) == 0:
		if len(scan.Errors) > 0 {
			return &types.OperationResponse{Success: false, Message: "Could not check webhooks: " + strings.Join(scan.Errors, "; "), Logs: logs}, nil
		}
		return nothingToDo("no leftover webhook configuration was found (a Service is back, its owner still exists, or an upgrade is in progress). Nothing was deleted.", logs), nil
	case len(d.Deleted) == 0:
		return &types.OperationResponse{Success: false, Message: "Could not delete " + strings.Join(d.Failed, ", "), Logs: logs}, nil
	case len(d.Failed) > 0:
		return &types.OperationResponse{
			Success:   false,
			Message:   fmt.Sprintf("Deleted %s; could not delete %s.", strings.Join(d.Deleted, ", "), strings.Join(d.Failed, ", ")),
			Logs:      logs,
			ErrorCode: "partial_failure",
		}, nil
	}
	return &types.OperationResponse{
		Success: true,
		Message: fmt.Sprintf("Deleted %s: %s.", countNoun(len(d.Deleted), "leftover webhook configuration", "leftover webhook configurations"), strings.Join(d.Deleted, ", ")),
		Logs:    logs,
	}, nil
}

// operatorCSV is a CSV of the rhods-operator package in SubNS.
type operatorCSV struct {
	Name    string
	Version string
	Phase   string
	Reason  string
	Message string
}

func listOperatorCSVs(c *Client) ([]operatorCSV, error) {
	var items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Spec struct {
			Version string `json:"version"`
		} `json:"spec"`
		Status struct {
			Phase   string `json:"phase"`
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"status"`
	}
	if err := c.listInto(namespacedPath("operators.coreos.com/v1alpha1", "clusterserviceversions", SubNS, ""), &items); err != nil {
		return nil, err
	}
	var out []operatorCSV
	for _, it := range items {
		if !strings.HasPrefix(it.Metadata.Name, SubName+".") {
			continue
		}
		out = append(out, operatorCSV{Name: it.Metadata.Name, Version: it.Spec.Version, Phase: it.Status.Phase, Reason: it.Status.Reason, Message: it.Status.Message})
	}
	return out, nil
}

// parallelFor runs fn(0..n-1) with at most limit calls at a time.
func parallelFor(n, limit int, fn func(i int)) {
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			fn(i)
		}(i)
	}
	wg.Wait()
}
