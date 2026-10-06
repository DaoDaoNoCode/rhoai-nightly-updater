package cluster

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Certificates that cert-manager never issued.
//
// RHOAI 3.6 serves some webhooks with certificates from cert-manager: a
// cert-manager Certificate names the Secret (spec.secretName) that the
// webhook's pods mount, and the webhook configuration carries
// cert-manager.io/inject-ca-from: <namespace>/<certificate>. Without a
// running cert-manager the Secret never appears: live, odh-observability
// pods stayed in ContainerCreating with FailedMount `secret
// "odh-observability-webhook-cert" not found`, the Certificate had no Ready
// condition, cert-manager's CRDs were left from a manual install but no
// controller ran, and diagnostics showed the pods and a webhook without
// endpoints as two unrelated problems. This check reports the Certificate
// as the root cause and folds those symptoms into it (linkProblems).

// certificateGrace is how long cert-manager may take to issue a new
// Certificate before it is reported.
var certificateGrace = 2 * time.Minute

const injectCAAnnotation = "cert-manager.io/inject-ca-from"

type certificateItem struct {
	Metadata struct {
		Name              string `json:"name"`
		Namespace         string `json:"namespace"`
		CreationTimestamp string `json:"creationTimestamp"`
	} `json:"metadata"`
	Spec struct {
		SecretName string `json:"secretName"`
	} `json:"spec"`
	Status struct {
		Conditions []dscCondition `json:"conditions"`
	} `json:"status"`
}

func (ci certificateItem) ref() string { return ci.Metadata.Namespace + "/" + ci.Metadata.Name }

func (ci certificateItem) readyCondition() *dscCondition {
	for i := range ci.Status.Conditions {
		if ci.Status.Conditions[i].Type == "Ready" {
			return &ci.Status.Conditions[i]
		}
	}
	return nil
}

// certManagerRunning reports whether a cert-manager controller has an
// available pod anywhere, by the labels of the upstream chart and of the
// Red Hat operator's deployment (app.kubernetes.io/name=cert-manager,
// app.kubernetes.io/component=controller).
func certManagerRunning(c *Client) (bool, string, error) {
	body, _, err := c.do(http.MethodGet, "/apis/apps/v1/deployments", "", nil,
		url.Values{"labelSelector": {"app.kubernetes.io/name=cert-manager,app.kubernetes.io/component=controller"}})
	if err != nil {
		return false, "", err
	}
	var list struct {
		Items []deploymentJSON `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return false, "", fmt.Errorf("parse deployments: %w", err)
	}
	var seen []string
	for _, d := range list.Items {
		ref := d.Metadata.Namespace + "/" + d.Metadata.Name
		if d.Status.AvailableReplicas > 0 {
			return true, ref, nil
		}
		seen = append(seen, ref+" (no available pod)")
	}
	return false, strings.Join(seen, ", "), nil
}

// podsMountingSecret returns "Pod <ns>/<name>" of the pods in namespace that
// are not running and mount the Secret.
func podsMountingSecret(c *Client, namespace, secret string) ([]string, error) {
	body, _, err := c.get(namespacedPath("v1", "pods", namespace, ""))
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				Volumes []struct {
					Secret *struct {
						SecretName string `json:"secretName"`
					} `json:"secret"`
				} `json:"volumes"`
			} `json:"spec"`
			Status struct {
				Phase string `json:"phase"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("parse pods in %s: %w", namespace, err)
	}
	var out []string
	for _, p := range list.Items {
		if p.Status.Phase != "Pending" {
			continue
		}
		for _, v := range p.Spec.Volumes {
			if v.Secret != nil && v.Secret.SecretName == secret {
				out = append(out, fmt.Sprintf("Pod %s/%s", namespace, p.Metadata.Name))
				break
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

func checkCertificates(c *Client) checkOutput {
	const name = "Certificates"
	out := checkOutput{check: CheckResult{Name: name, Status: "pass"}}
	namespaces := append([]string{SubNS}, rhoaiPodNamespaces...)
	now := time.Now()
	var certs []certificateItem
	var errs []string
	served := true
	for _, ns := range namespaces {
		body, _, err := c.get(namespacedPath("cert-manager.io/v1", "certificates", ns, ""))
		if IsK8sError(err, http.StatusNotFound) {
			served = false
			break
		}
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v%s", ns, err, templateHint(err)))
			continue
		}
		var list struct {
			Items []certificateItem `json:"items"`
		}
		if err := json.Unmarshal(body, &list); err != nil {
			errs = append(errs, fmt.Sprintf("parse certificates in %s: %v", ns, err))
			continue
		}
		for _, ci := range list.Items {
			if ci.Metadata.Namespace == "" {
				ci.Metadata.Namespace = ns
			}
			certs = append(certs, ci)
		}
	}
	if !served {
		out.check.Detail = "The cert-manager API is not installed, so no Certificates exist"
		return out
	}

	var pending []certificateItem
	ready := 0
	for _, ci := range certs {
		rc := ci.readyCondition()
		if rc != nil && rc.Status == "True" {
			ready++
			continue
		}
		if created, ok := parseK8sTime(ci.Metadata.CreationTimestamp); ok && now.Sub(created) < certificateGrace {
			continue
		}
		pending = append(pending, ci)
	}

	if len(pending) > 0 {
		running, controller, runErr := certManagerRunning(c)
		var configs []admissionConfig
		var cfgErrs []string
		configs, cfgErrs = listAdmissionConfigs(c)
		var dep *dependency
		if runErr == nil && !running {
			dep = &dependency{Mention: "cert-manager", Key: normalizeOperatorName("cert-manager")}
			resolveDependencies(c, []*dependency{dep})
		}
		for _, ci := range pending {
			p := certificateProblem(c, ci, certificateCause{running: running, controller: controller, runErr: runErr, dep: dep}, configs)
			out.problems = append(out.problems, p)
		}
		if dep != nil && dep.problemID() != "" {
			var refs []string
			for _, ci := range pending {
				refs = append(refs, "Certificate "+ci.ref())
			}
			dep.NeededBy = []string{"Not issued without a running cert-manager: " + strings.Join(refs, ", ")}
			dep.NeededImpact = "RHOAI Certificates are not issued without it, so the pods that mount their Secrets do not start and the webhooks they serve reject requests."
			out.problems = append(out.problems, prerequisiteProblem(c, dep))
		}
		if len(cfgErrs) > 0 {
			errs = append(errs, "webhook configurations: "+strings.Join(cfgErrs, "; "))
		}
		out.check.Status = "fail"
		out.check.Detail = fmt.Sprintf("%s not issued", countNoun(len(pending), "Certificate", "Certificates"))
	} else {
		out.check.Detail = fmt.Sprintf("%s Ready in %s", countNoun(ready, "Certificate", "Certificates"), strings.Join(namespaces, ", "))
	}
	if len(errs) > 0 {
		if out.check.Status == "pass" {
			out.check.Status = "warn"
		}
		out.check.Detail += "; could not read: " + strings.Join(errs, "; ")
	}
	return out
}

type certificateCause struct {
	running    bool
	controller string
	runErr     error
	dep        *dependency
}

func certificateProblem(c *Client, ci certificateItem, cause certificateCause, configs []admissionConfig) Problem {
	rc := ci.readyCondition()
	evidence := []string{}
	if rc == nil {
		evidence = append(evidence, fmt.Sprintf("Certificate %s (Secret %s) has no Ready condition", ci.ref(), nonEmpty(ci.Spec.SecretName, "unset")))
	} else {
		evidence = append(evidence, fmt.Sprintf("Certificate %s (Secret %s): %s", ci.ref(), nonEmpty(ci.Spec.SecretName, "unset"), conditionLine(*rc)))
	}
	p := Problem{
		ID:              "certificate-not-ready-" + ci.Metadata.Namespace + "-" + ci.Metadata.Name,
		Severity:        "warning",
		AffectedObjects: []string{"Certificate " + ci.ref()},
		Description: fmt.Sprintf("cert-manager issues Secret %s from this Certificate. Until it does, pods that mount the Secret stay in ContainerCreating, "+
			"and webhooks whose CA it injects (%s) have no serving certificate. Those symptoms are listed here instead of as separate problems.", nonEmpty(ci.Spec.SecretName, "(unset)"), injectCAAnnotation),
		TechnicalCmd: shellCommand("oc", "describe", "certificate.cert-manager.io", ci.Metadata.Name, "-n", ci.Metadata.Namespace),
	}
	switch {
	case rc != nil && rc.Status == "False" && rc.Message != "":
		p.Title = fmt.Sprintf("Certificate %s is not issued: %s", ci.ref(), truncate(rc.Message, 120))
		p.Fix = "Fix what the Certificate's Ready condition names (for example a missing Issuer); cert-manager retries on its own."
	case cause.runErr != nil:
		p.Title = fmt.Sprintf("Certificate %s is not issued", ci.ref())
		evidence = append(evidence, fmt.Sprintf("Could not check whether cert-manager runs: %v%s", cause.runErr, templateHint(cause.runErr)))
		p.Fix = "Check that cert-manager is installed and its pods run (oc get pods -A -l app.kubernetes.io/name=cert-manager), then read the Certificate's events."
	case !cause.running:
		p.Title = fmt.Sprintf("Certificate %s is not issued: cert-manager is not installed or not running", ci.ref())
		line := "No cert-manager controller Deployment (labels app.kubernetes.io/name=cert-manager, app.kubernetes.io/component=controller) has an available pod"
		if cause.controller != "" {
			line += ": " + cause.controller
		}
		evidence = append(evidence, line)
		d := cause.dep
		switch {
		case d == nil:
			p.Fix = "Install cert-manager (cert-manager Operator for Red Hat OpenShift); it then issues the Certificate and the pods start."
		case d.problemID() != "":
			p.RelatedProblems = []string{d.problemID()}
			p.Fix = fmt.Sprintf("Install or finish %s (see the related problem for the exact commands); cert-manager then issues the Certificate and the pods start on their own.", d.displayName())
		default:
			evidence = append(evidence, fmt.Sprintf("%s is installed (CSV %s/%s Succeeded)", d.displayName(), d.CSV.Namespace, d.CSV.Name))
			p.Fix = fmt.Sprintf("%s is installed but its controller does not run. Check the operator's pods and its operand (for the Red Hat operator, CertManager/cluster) and the namespace cert-manager.", d.displayName())
			p.TechnicalCmd = "oc get pods -A -l app.kubernetes.io/name=cert-manager; oc get certmanagers.operator.openshift.io cluster -o yaml"
		}
	default:
		p.Title = fmt.Sprintf("Certificate %s is not issued", ci.ref())
		evidence = append(evidence, "cert-manager runs ("+cause.controller+") but has not issued it")
		p.Fix = "Read the Certificate's events and its Issuer's status (command below); cert-manager logs name the cause."
	}

	if ci.Spec.SecretName != "" {
		pods, err := podsMountingSecret(c, ci.Metadata.Namespace, ci.Spec.SecretName)
		if err != nil {
			evidence = append(evidence, fmt.Sprintf("Could not list pods in %s: %v", ci.Metadata.Namespace, err))
		}
		if len(pods) > 0 {
			evidence = append(evidence, fmt.Sprintf("Waiting for Secret %s: %s", ci.Spec.SecretName, strings.Join(pods, ", ")))
			p.covers = append(p.covers, pods...)
		}
	}
	var hooks []string
	for _, cfg := range configs {
		if cfg.Metadata.Annotations[injectCAAnnotation] == ci.ref() {
			hooks = append(hooks, cfg.ref())
		}
	}
	sort.Strings(hooks)
	if len(hooks) > 0 {
		evidence = append(evidence, fmt.Sprintf("CA injected from it (%s): %s", injectCAAnnotation, strings.Join(hooks, ", ")))
		p.covers = append(p.covers, hooks...)
	}
	p.AffectedObjects = append(p.AffectedObjects, p.covers...)
	p.Evidence = evidence
	return p
}

// --- Report post-processing ---

var severityRank = map[string]int{"critical": 0, "warning": 1, "info": 2}

// mergeProblem adds src's findings to dst (same ID, reported by two
// checks).
func mergeProblem(dst *Problem, src Problem) {
	for _, e := range src.Evidence {
		if !containsString(dst.Evidence, e) {
			dst.Evidence = append(dst.Evidence, e)
		}
	}
	for _, o := range src.AffectedObjects {
		if !containsString(dst.AffectedObjects, o) {
			dst.AffectedObjects = append(dst.AffectedObjects, o)
		}
	}
	for _, id := range src.RelatedProblems {
		if !containsString(dst.RelatedProblems, id) {
			dst.RelatedProblems = append(dst.RelatedProblems, id)
		}
	}
	dst.covers = append(dst.covers, src.covers...)
	if r, ok := severityRank[src.Severity]; ok && r < severityRank[dst.Severity] {
		dst.Severity = src.Severity
	}
}

// linkProblems folds symptoms into the problem that explains them (pod
// problems whose pods it covers, and covered entries of the webhook
// guidance problem), and keeps only related IDs that are in the report.
func linkProblems(problems []Problem) []Problem {
	drop := map[int]bool{}
	for i := range problems {
		if len(problems[i].covers) == 0 {
			continue
		}
		covered := map[string]bool{}
		for _, o := range problems[i].covers {
			covered[o] = true
		}
		for j := range problems {
			if i == j || drop[j] || len(problems[j].AffectedObjects) == 0 {
				continue
			}
			pj := &problems[j]
			if !strings.HasPrefix(pj.ID, "pod-") && pj.ID != "webhook-service-missing" {
				continue
			}
			var rest []string
			for _, o := range pj.AffectedObjects {
				if !covered[o] {
					rest = append(rest, o)
				}
			}
			switch {
			case len(rest) == 0:
				drop[j] = true
				problems[i].Evidence = append(problems[i].Evidence, "Also explains: "+pj.Title)
			case len(rest) < len(pj.AffectedObjects) && pj.ID == "webhook-service-missing":
				// Keep the problem for the other configurations, and say
				// which ones the root cause explains.
				pj.Evidence = append(pj.Evidence, fmt.Sprintf("Explained by %q: %s", problems[i].Title, strings.Join(coveredOf(pj.AffectedObjects, covered), ", ")))
				pj.RelatedProblems = append(pj.RelatedProblems, problems[i].ID)
			}
		}
	}
	ids := map[string]bool{}
	out := make([]Problem, 0, len(problems))
	for i, p := range problems {
		if !drop[i] {
			out = append(out, p)
			ids[p.ID] = true
		}
	}
	for i := range out {
		var kept []string
		for _, id := range out[i].RelatedProblems {
			if ids[id] && id != out[i].ID && !containsString(kept, id) {
				kept = append(kept, id)
			}
		}
		out[i].RelatedProblems = kept
	}
	return out
}

func coveredOf(objects []string, covered map[string]bool) []string {
	var out []string
	for _, o := range objects {
		if covered[o] {
			out = append(out, o)
		}
	}
	return out
}
