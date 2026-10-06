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
// available pod anywhere. Controllers are found by the labels of the
// upstream chart and of the Red Hat operator's operand
// (app.kubernetes.io/name=cert-manager or app=cert-manager; the cainjector
// and webhook Deployments carry other names or a component label other
// than controller). seen lists the controllers found, with their pods.
func certManagerRunning(c *Client) (running bool, seen string, err error) {
	var found []string
	byRef := map[string]bool{}
	for _, sel := range []string{"app.kubernetes.io/name=cert-manager", "app=cert-manager"} {
		body, _, err := c.do(http.MethodGet, "/apis/apps/v1/deployments", "", nil, url.Values{"labelSelector": {sel}})
		if err != nil {
			return false, "", err
		}
		var list struct {
			Items []deploymentJSON `json:"items"`
		}
		if err := json.Unmarshal(body, &list); err != nil {
			return false, "", fmt.Errorf("parse deployments: %w", err)
		}
		for _, j := range list.Items {
			ref := j.Metadata.Namespace + "/" + j.Metadata.Name
			if comp := j.Metadata.Labels["app.kubernetes.io/component"]; byRef[ref] || (comp != "" && comp != "controller") {
				continue
			}
			byRef[ref] = true
			replicas := 1
			if j.Spec.Replicas != nil {
				replicas = *j.Spec.Replicas
			}
			if j.Status.AvailableReplicas > 0 {
				running = true
			}
			found = append(found, fmt.Sprintf("%s (%d of %d available)", ref, j.Status.AvailableReplicas, replicas))
		}
	}
	sort.Strings(found)
	return running, strings.Join(found, ", "), nil
}

// certManagerOperatorState describes the installed cert-manager operator
// when its controller does not run: its own Deployments (from the CSV's
// install strategy) and its operand's managementState, with the commands
// that bring the controller back.
func certManagerOperatorState(c *Client, d *dependency) (evidence, cmds []string) {
	for _, name := range d.CSV.Deployments {
		od, err := readOperatorDeployment(c, d.CSV.Namespace, name, "")
		if err != nil {
			evidence = append(evidence, fmt.Sprintf("Operator Deployment %s/%s: %v", d.CSV.Namespace, name, err))
			continue
		}
		line := fmt.Sprintf("Operator Deployment %s: %d of %d available", od.ref(), od.Available, od.Replicas)
		if od.Replicas == 0 {
			line += " (scaled to 0, so nothing restores the controller)"
			cmds = append(cmds, shellCommand("oc", "scale", "deployment/"+od.Name, "-n", od.Namespace, "--replicas=1"))
		}
		evidence = append(evidence, line)
	}
	if d.Operand != nil && d.OperandSt == operandPresent {
		state, err := operandManagementState(c, *d.Operand)
		switch {
		case err != nil:
			evidence = append(evidence, fmt.Sprintf("%s: could not read managementState: %v%s", d.Operand.ref(), err, templateHint(err)))
		case state != "" && state != "Managed":
			evidence = append(evidence, fmt.Sprintf("%s has managementState %s, so the operator does not run cert-manager", d.Operand.ref(), state))
			cmds = append(cmds, shellCommand("oc", "patch", d.Operand.resourceArg(), d.Operand.objName(), "--type", "merge", "-p", `{"spec":{"managementState":"Managed"}}`))
		default:
			evidence = append(evidence, fmt.Sprintf("%s has managementState %s", d.Operand.ref(), nonEmpty(state, "unset")))
		}
	}
	return evidence, cmds
}

// certPod is the part of a pod the Certificates check reads.
type certPod struct {
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
		Phase                 string                `json:"phase"`
		InitContainerStatuses []diagContainerStatus `json:"initContainerStatuses"`
		ContainerStatuses     []diagContainerStatus `json:"containerStatuses"`
	} `json:"status"`
}

func (p certPod) mounts(secret string) bool {
	for _, v := range p.Spec.Volumes {
		if v.Secret != nil && v.Secret.SecretName == secret {
			return true
		}
	}
	return false
}

// podLister lists the pods of each namespace once per check.
type podLister struct {
	c    *Client
	pods map[string][]certPod
	errs map[string]error
}

func (l *podLister) list(namespace string) ([]certPod, error) {
	if pods, ok := l.pods[namespace]; ok || l.errs[namespace] != nil {
		return pods, l.errs[namespace]
	}
	body, _, err := l.c.get(namespacedPath("v1", "pods", namespace, ""))
	var list struct {
		Items []certPod `json:"items"`
	}
	if err == nil {
		if perr := json.Unmarshal(body, &list); perr != nil {
			err = fmt.Errorf("parse pods in %s: %w", namespace, perr)
		}
	}
	if err != nil {
		l.errs[namespace] = err
		return nil, err
	}
	l.pods[namespace] = list.Items
	return list.Items, nil
}

// podsBlockedOnSecret returns "Pod <ns>/<name>" of the Pending pods that
// show they cannot start because THIS Secret is missing: a container
// waiting with a message that names it (CreateContainerConfigError for an
// env reference), or a FailedMount Warning event that names it. Pods that
// mount the Secret but fail for another reason are returned in other, so
// their own problems stay.
func podsBlockedOnSecret(c *Client, l *podLister, namespace, secret string) (blocked, other []string, err error) {
	pods, err := l.list(namespace)
	if err != nil {
		return nil, nil, err
	}
	needle := fmt.Sprintf("secret %q not found", secret)
	for _, p := range pods {
		if p.Status.Phase != "Pending" || !p.mounts(secret) {
			continue
		}
		ref := fmt.Sprintf("Pod %s/%s", namespace, p.Metadata.Name)
		named := false
		for _, cs := range append(append([]diagContainerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...) {
			if w := cs.State.Waiting; w != nil && strings.Contains(w.Message, needle) {
				named = true
			}
		}
		if !named {
			ev, evErr := latestWarningEvent(c, namespace, p.Metadata.Name)
			named = evErr == nil && strings.HasPrefix(ev, "FailedMount:") && strings.Contains(ev, needle)
		}
		if named {
			blocked = append(blocked, ref)
		} else {
			other = append(other, ref)
		}
	}
	sort.Strings(blocked)
	sort.Strings(other)
	return blocked, other, nil
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

	// A Certificate is effectively ready only if its Secret exists too: with
	// no cert-manager controller running, nothing updates a Ready=True
	// condition after the Secret is gone. The ServiceAccount may not read
	// Secrets (only a few by name), so a missing Secret is known from the
	// pods: a FailedMount event or a container waiting on it by name.
	pods := &podLister{c: c, pods: map[string][]certPod{}, errs: map[string]error{}}
	mounts := map[string]secretMounts{}
	var pending []certificateItem
	ready, stale := 0, 0
	for _, ci := range certs {
		if ci.Spec.SecretName != "" {
			var m secretMounts
			m.blocked, m.other, m.err = podsBlockedOnSecret(c, pods, ci.Metadata.Namespace, ci.Spec.SecretName)
			mounts[ci.ref()] = m
		}
		rc := ci.readyCondition()
		if rc != nil && rc.Status == "True" {
			if len(mounts[ci.ref()].blocked) == 0 {
				ready++
				continue
			}
			stale++
			pending = append(pending, ci)
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
			p := certificateProblem(c, ci, certificateCause{running: running, controller: controller, runErr: runErr, dep: dep}, configs, mounts[ci.ref()])
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
		if stale > 0 {
			out.check.Detail += fmt.Sprintf(" (%d %s Ready, but %s Secret is missing: stale)", stale, verb(stale, "reports", "report"), verb(stale, "its", "their"))
		}
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

// secretMounts are the pods that mount a Certificate's Secret.
type secretMounts struct {
	blocked, other []string
	err            error
}

type certificateCause struct {
	running    bool
	controller string
	runErr     error
	dep        *dependency
}

func certificateProblem(c *Client, ci certificateItem, cause certificateCause, configs []admissionConfig, mounts secretMounts) Problem {
	rc := ci.readyCondition()
	staleReady := rc != nil && rc.Status == "True" && len(mounts.blocked) > 0
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
	subject := fmt.Sprintf("Certificate %s is not issued", ci.ref())
	if staleReady {
		subject = fmt.Sprintf("Certificate %s reports Ready, but its Secret %s is missing", ci.ref(), ci.Spec.SecretName)
		evidence = append(evidence, fmt.Sprintf("Ready=True is stale: Secret %s does not exist (pods fail to mount it), and only a running cert-manager would update the condition", ci.Spec.SecretName))
		p.Description = fmt.Sprintf("cert-manager keeps Secret %s from this Certificate. The Secret is gone but the Certificate still says Ready, because no cert-manager controller runs to notice. "+
			"Until it is issued again, pods that mount the Secret stay in ContainerCreating, and webhooks whose CA it injects (%s) have no serving certificate. Those symptoms are listed here instead of as separate problems.", ci.Spec.SecretName, injectCAAnnotation)
	}
	switch {
	case rc != nil && rc.Status == "False" && rc.Message != "":
		p.Title = fmt.Sprintf("Certificate %s is not issued: %s", ci.ref(), truncate(rc.Message, 120))
		p.Fix = "Fix what the Certificate's Ready condition names (for example a missing Issuer); cert-manager retries on its own."
	case cause.runErr != nil:
		p.Title = subject
		evidence = append(evidence, fmt.Sprintf("Could not check whether cert-manager runs: %v%s", cause.runErr, templateHint(cause.runErr)))
		p.Fix = "Check that cert-manager is installed and its pods run (oc get pods -A -l app.kubernetes.io/name=cert-manager), then read the Certificate's events."
	case !cause.running:
		p.Title = subject + ": cert-manager is not installed or not running"
		line := "No cert-manager controller Deployment (labels app.kubernetes.io/name=cert-manager or app=cert-manager) has an available pod"
		if cause.controller != "" {
			line += ": " + cause.controller
		}
		evidence = append(evidence, line)
		d := cause.dep
		switch {
		case d != nil && d.problemID() == "":
			p.Title = subject + ": cert-manager is installed but its controller is not running"
			evidence = append(evidence, fmt.Sprintf("%s is installed (CSV %s/%s Succeeded)", d.displayName(), d.CSV.Namespace, d.CSV.Name))
			opEvidence, cmds := certManagerOperatorState(c, d)
			evidence = append(evidence, opEvidence...)
			if len(cmds) > 0 {
				p.Fix = fmt.Sprintf("Bring the %s back (commands below); it then restarts the cert-manager controller, which issues the Secret again, and the pods start on their own.", d.displayName())
				p.TechnicalCmd = strings.Join(cmds, "\n")
			} else {
				p.Fix = fmt.Sprintf("%s runs and manages cert-manager, but the controller has no available pod: read the controller's pods and events (command below).", d.displayName())
				p.TechnicalCmd = "oc get deployments,pods -A -l app.kubernetes.io/name=cert-manager"
			}
		case d == nil:
			p.Fix = "Install cert-manager (cert-manager Operator for Red Hat OpenShift); it then issues the Certificate and the pods start."
		case d.problemID() != "":
			p.RelatedProblems = []string{d.problemID()}
			p.Fix = fmt.Sprintf("Install or finish %s (see the related problem for the exact commands); cert-manager then issues the Certificate and the pods start on their own.", d.displayName())
		}
	default:
		p.Title = subject
		evidence = append(evidence, "cert-manager runs ("+cause.controller+") but has not issued it")
		p.Fix = "Read the Certificate's events and its Issuer's status (command below); cert-manager logs name the cause."
	}

	if ci.Spec.SecretName != "" {
		blocked, other, err := mounts.blocked, mounts.other, mounts.err
		if err != nil {
			evidence = append(evidence, fmt.Sprintf("Could not list pods in %s: %v", ci.Metadata.Namespace, err))
		}
		if len(blocked) > 0 {
			evidence = append(evidence, fmt.Sprintf("Cannot start without Secret %s: %s", ci.Spec.SecretName, strings.Join(blocked, ", ")))
			p.covers = append(p.covers, blocked...)
		}
		if len(other) > 0 {
			evidence = append(evidence, fmt.Sprintf("Also mount Secret %s but show no error about it (their own problems are kept): %s", ci.Spec.SecretName, strings.Join(other, ", ")))
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
			// Only the pod problems a missing Secret causes are folded: pods
			// stuck creating or unable to create a container.
			podSymptom := strings.HasPrefix(pj.ID, "pod-"+string(issueStuckCreating)+"-") || strings.HasPrefix(pj.ID, "pod-"+string(issueContainerConfig)+"-")
			if !podSymptom && pj.ID != "webhook-service-missing" {
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
