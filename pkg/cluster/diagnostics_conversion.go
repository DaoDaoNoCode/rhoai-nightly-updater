package cluster

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

// The "API versions" diagnostics check: a RHOAI CRD that serves several
// versions converts its objects between them through the operator's
// conversion webhook. When the webhook cannot convert to a version, every
// read at that version fails, `oc get` included when it is the preferred
// version. Live on 2026-10-07 (RHOAI 3.6 nightly): DataScienceCluster v3 and
// Platform v1alpha2 failed with "conversion webhook for
// datasciencecluster.opendatahub.io/v2, Kind=DataScienceCluster failed: no
// kind "DataScienceCluster" is registered for version
// "datasciencecluster.opendatahub.io/v3"", while v2 and v1alpha1 worked: the
// operator image was older than the CRDs its bundle installed.
//
// The check uses API discovery only (readable by every authenticated user)
// and one `?limit=1` list per resource and served version of every
// *.opendatahub.io group that serves more than one version. A list the app
// may not make (403) is skipped: its RBAC covers the RHOAI resources it uses.

const conversionCheckName = "API versions"

// conversionProbeParallelism bounds the concurrent requests of the check.
const conversionProbeParallelism = 4

// conversionProbe is one ?limit=1 list of a resource at one version.
type conversionProbe struct {
	Group, Version, Resource, Kind string
	Preferred                      bool
	Err                            error // nil when the list worked
}

func (p conversionProbe) path() string {
	return "/apis/" + p.Group + "/" + p.Version + "/" + p.Resource
}

// getDiscovered reads an API path found by discovery at run time. These
// reads are best effort: the app's RBAC grants the RHOAI resources it
// uses, other resources answer 403 and are skipped. The RBAC coverage test
// lists this function in rbacBestEffortCalls.
func getDiscovered(c *Client, path string, query url.Values) ([]byte, error) {
	body, _, err := c.do(http.MethodGet, path, "", nil, query)
	return body, err
}

// multiVersionRHOAIGroups returns the *.opendatahub.io API groups that serve
// more than one version.
func multiVersionRHOAIGroups(c *Client) ([]apiGroupDoc, error) {
	body, err := getDiscovered(c, "/apis", nil)
	if err != nil {
		return nil, err
	}
	var list struct {
		Groups []apiGroupDoc `json:"groups"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("parse API group list: %w", err)
	}
	var out []apiGroupDoc
	for _, g := range list.Groups {
		if strings.HasSuffix(g.Name, ".opendatahub.io") && len(g.orderedVersions()) > 1 {
			out = append(out, g)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// listableResources returns the resources of group/version that can be
// listed (no subresources).
func listableResources(c *Client, group, version string) ([]conversionProbe, error) {
	body, err := getDiscovered(c, "/apis/"+group+"/"+version, nil)
	if err != nil {
		return nil, err
	}
	var list struct {
		Resources []struct {
			Name  string   `json:"name"`
			Kind  string   `json:"kind"`
			Verbs []string `json:"verbs"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("parse %s/%s resources: %w", group, version, err)
	}
	var out []conversionProbe
	for _, r := range list.Resources {
		if strings.Contains(r.Name, "/") || !containsString(r.Verbs, "list") {
			continue
		}
		out = append(out, conversionProbe{Group: group, Version: version, Resource: r.Name, Kind: r.Kind})
	}
	return out, nil
}

// probeConversions lists every resource of every multi-version RHOAI group
// at each served version.
func probeConversions(c *Client) (probes []conversionProbe, groups int, errs []string, err error) {
	gs, err := multiVersionRHOAIGroups(c)
	if err != nil {
		return nil, 0, nil, err
	}
	type groupVersion struct {
		group, version string
		preferred      bool
	}
	var gvs []groupVersion
	for _, g := range gs {
		for i, v := range g.orderedVersions() {
			gvs = append(gvs, groupVersion{g.Name, v, i == 0 && g.PreferredVersion.Version != ""})
		}
	}
	resources := make([][]conversionProbe, len(gvs))
	discoveryErrs := make([]error, len(gvs))
	parallelFor(len(gvs), conversionProbeParallelism, func(i int) {
		resources[i], discoveryErrs[i] = listableResources(c, gvs[i].group, gvs[i].version)
	})
	for i, gv := range gvs {
		if discoveryErrs[i] != nil {
			errs = append(errs, fmt.Sprintf("discovery of %s/%s: %v", gv.group, gv.version, discoveryErrs[i]))
			continue
		}
		for _, p := range resources[i] {
			p.Preferred = gv.preferred
			probes = append(probes, p)
		}
	}
	parallelFor(len(probes), conversionProbeParallelism, func(i int) {
		_, probes[i].Err = getDiscovered(c, probes[i].path(), url.Values{"limit": {"1"}})
	})
	return probes, len(gs), errs, nil
}

func checkConversionFailures(c *Client) checkOutput {
	out := checkOutput{check: CheckResult{Name: conversionCheckName, Status: "pass"}}
	probes, groups, errs, err := probeConversions(c)
	if err != nil {
		out.check.Status = "warn"
		out.check.Detail = fmt.Sprintf("could not read API discovery: %v", err)
		return out
	}

	// One entry per kind (group and resource), its versions in the order tried.
	type kindProbes struct {
		group, resource, kind string
		probes                []conversionProbe
	}
	var kinds []*kindProbes
	byKey := map[string]*kindProbes{}
	forbidden := 0
	for _, p := range probes {
		key := p.Resource + "." + p.Group
		k := byKey[key]
		if k == nil {
			k = &kindProbes{group: p.Group, resource: p.Resource, kind: p.Kind}
			byKey[key] = k
			kinds = append(kinds, k)
		}
		k.probes = append(k.probes, p)
		switch {
		case p.Err == nil, isConversionWebhookError(p.Err), IsK8sError(p.Err, http.StatusNotFound):
		case IsK8sError(p.Err, http.StatusForbidden):
			forbidden++
		default:
			errs = append(errs, fmt.Sprintf("%s?limit=1: %v", p.path(), p.Err))
		}
	}

	var operator *installedOperator
	failedKinds := []string{}
	for _, k := range kinds {
		var failed, working []conversionProbe
		for _, p := range k.probes {
			switch {
			case p.Err == nil:
				working = append(working, p)
			case isConversionWebhookError(p.Err):
				failed = append(failed, p)
			}
		}
		if len(failed) == 0 {
			continue
		}
		if operator == nil {
			operator, _ = getInstalledOperator(c)
			if operator == nil {
				operator = &installedOperator{}
			}
		}
		failedKinds = append(failedKinds, fmt.Sprintf("%s %s", k.kind, joinVersions(failed)))
		out.problems = append(out.problems, conversionProblem(k.group, k.resource, k.kind, failed, working, operator))
	}

	var details []string
	switch {
	case len(out.problems) > 0:
		out.check.Status = "fail"
		details = append(details, fmt.Sprintf("the operator's conversion webhook fails for %s", strings.Join(failedKinds, ", ")))
	case groups == 0:
		details = append(details, "No RHOAI API group serves more than one version")
	default:
		details = append(details, fmt.Sprintf("Every served version of %s in %s can be read",
			countNoun(len(kinds), "resource", "resources"), countNoun(groups, "multi-version RHOAI API group", "multi-version RHOAI API groups")))
	}
	if forbidden > 0 {
		details = append(details, fmt.Sprintf("%s not checked (this app may not list %s)", countNoun(forbidden, "read", "reads"), verb(forbidden, "it", "them")))
	}
	if len(errs) > 0 {
		details = append(details, "could not verify: "+strings.Join(errs, "; "))
		if out.check.Status == "pass" {
			out.check.Status = "warn"
		}
	}
	out.check.Detail = strings.Join(details, "; ")
	return out
}

func joinVersions(probes []conversionProbe) string {
	var vs []string
	for _, p := range probes {
		vs = append(vs, p.Version)
	}
	return strings.Join(vs, " and ")
}

// conversionProblem reports one kind whose objects the operator's
// conversion webhook cannot convert to the failed versions.
func conversionProblem(group, resource, kind string, failed, working []conversionProbe, op *installedOperator) Problem {
	crd := resource + "." + group
	failedVersions := joinVersions(failed)
	evidence := []string{}
	for _, p := range failed {
		evidence = append(evidence, fmt.Sprintf("GET %s?limit=1: HTTP %d: %s", p.path(), k8sStatus(p.Err), truncate(conversionMessage(p.Err), 400)))
	}
	for _, p := range working {
		evidence = append(evidence, fmt.Sprintf("GET %s?limit=1: OK", p.path()))
	}

	description := fmt.Sprintf("Reading %s objects as %s fails: the API server asks the operator's conversion webhook to convert them, and the webhook returns an error.", kind, failedVersions)
	if len(working) > 0 {
		description += fmt.Sprintf(" Reading them as %s works, so clients of that version keep working.", joinVersions(working))
	}
	for _, p := range failed {
		if p.Preferred {
			description += fmt.Sprintf(" %s is the version the API server prefers, so `oc get %s` and every client that follows discovery fail too.", p.Version, strings.ToLower(kind))
			break
		}
	}
	unregistered := false
	for _, p := range failed {
		if strings.Contains(conversionMessage(p.Err), "is registered for version") {
			unregistered = true
		}
	}
	if unregistered {
		description += fmt.Sprintf(" Likely cause (an inference from the message): the conversion webhook served by the running operator does not know %s. That happens when a nightly's operator image is older than the CRDs its bundle installed.", failedVersions)
	} else {
		description += " Likely cause (an inference): the operator's conversion webhook does not support this conversion or is failing."
	}

	ns := SubNS
	deployment := "<operator deployment>"
	if len(op.Deployments) > 0 {
		deployment = op.Deployments[0]
	}
	if op.Name != "" {
		evidence = append(evidence, fmt.Sprintf("Installed operator: %s, Deployment %s/%s", op.Name, ns, deployment))
	}
	cmds := []string{}
	for _, p := range failed {
		cmds = append(cmds, fmt.Sprintf("oc get --raw '%s?limit=1' --request-timeout=20s", p.path()))
	}
	cmds = append(cmds, fmt.Sprintf("oc logs -n %s deploy/%s | grep conversion-webhook", ns, deployment))

	return Problem{
		ID:              "conversion-failed-" + crd,
		Severity:        "critical",
		Title:           fmt.Sprintf("The operator cannot convert %s to %s", kind, failedVersions),
		Description:     description,
		Evidence:        evidence,
		AffectedObjects: []string{"CustomResourceDefinition " + crd},
		Fix: "Update RHOAI to a newer nightly (Update on the Status page, or a build from Build Explorer), then run diagnostics again. " +
			"Nothing needs to be changed by hand: this tool keeps reading the version that works, and does not change the CRD.",
		LearnMore: "A CRD that serves several versions stores each object in one of them and converts on every read in another, through the conversion webhook of the operator. " +
			"The webhook can only convert to the versions compiled into the operator image. A newer operator build that knows the version fixes the reads; " +
			"switching the CRD to conversion strategy None would change how the stored objects are read, so the tool does not do it.",
		TechnicalCmd: strings.Join(cmds, "\n"),
	}
}

func k8sStatus(err error) int {
	var k *K8sError
	if errors.As(err, &k) {
		return k.Status
	}
	return 0
}
