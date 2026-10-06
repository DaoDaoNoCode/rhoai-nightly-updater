package cluster

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Module operators that have not retried yet.
//
// A module operator (controller-runtime) requeues a failing reconcile with
// a per-item exponential back-off that reaches about 16m40s, and it does
// not watch the operators its module depends on. After a missing
// prerequisite is installed, the module CR keeps its old "not installed"
// condition until the next retry (live on RHOAI 3.6: trainer-operator kept
// "JobSet Operator is not installed" until it was restarted; Ray and AIHub
// recovered only after their operators restarted, with Ray reporting
// "Module status is stale (observedGeneration < generation)"). A restart
// reconciles at once. The tool restarts only the way `oc rollout restart`
// does, by setting a pod-template annotation, and only when a pod of the
// Deployment is available and no failurePolicy Fail webhook would go
// unserved during the restart.

// moduleStaleAfter is how long scans must keep seeing a module CR lag
// behind the same generation before its operator is said to be stuck
// (staleSeen in diagnostics_dsc.go).
var moduleStaleAfter = 5 * time.Minute

const restartAnnotation = "kubectl.kubernetes.io/restartedAt"

// operatorDeployment is a module operator's Deployment.
type operatorDeployment struct {
	Namespace, Name, UID, ResourceVersion string
	Strategy                              string // RollingUpdate (default) or Recreate
	Replicas, Available                   int
	MaxUnavailable, MaxSurge              interface{}
	TemplateLabels                        map[string]string
	OwnerKinds                            []string
	PartOf                                string
	Via                                   string // how it was found
}

func (d operatorDeployment) ref() string { return d.Namespace + "/" + d.Name }

type deploymentJSON struct {
	Metadata struct {
		Name            string            `json:"name"`
		Namespace       string            `json:"namespace"`
		UID             string            `json:"uid"`
		ResourceVersion string            `json:"resourceVersion"`
		Labels          map[string]string `json:"labels"`
		OwnerReferences []struct {
			Kind string `json:"kind"`
		} `json:"ownerReferences"`
	} `json:"metadata"`
	Spec struct {
		Replicas *int `json:"replicas"`
		Strategy struct {
			Type          string `json:"type"`
			RollingUpdate *struct {
				MaxUnavailable interface{} `json:"maxUnavailable"`
				MaxSurge       interface{} `json:"maxSurge"`
			} `json:"rollingUpdate"`
		} `json:"strategy"`
		Template struct {
			Metadata struct {
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
		} `json:"template"`
	} `json:"spec"`
	Status struct {
		AvailableReplicas int `json:"availableReplicas"`
	} `json:"status"`
}

func (j deploymentJSON) toOperatorDeployment(via string) operatorDeployment {
	d := operatorDeployment{
		Namespace: j.Metadata.Namespace, Name: j.Metadata.Name, UID: j.Metadata.UID, ResourceVersion: j.Metadata.ResourceVersion,
		Strategy: nonEmpty(j.Spec.Strategy.Type, "RollingUpdate"), Replicas: 1, Available: j.Status.AvailableReplicas,
		TemplateLabels: j.Spec.Template.Metadata.Labels, PartOf: j.Metadata.Labels["platform.opendatahub.io/part-of"], Via: via,
	}
	if j.Spec.Replicas != nil {
		d.Replicas = *j.Spec.Replicas
	}
	if j.Spec.Strategy.RollingUpdate != nil {
		d.MaxUnavailable = j.Spec.Strategy.RollingUpdate.MaxUnavailable
		d.MaxSurge = j.Spec.Strategy.RollingUpdate.MaxSurge
	}
	for _, o := range j.Metadata.OwnerReferences {
		d.OwnerKinds = append(d.OwnerKinds, o.Kind)
	}
	return d
}

func readOperatorDeployment(c *Client, namespace, name, via string) (*operatorDeployment, error) {
	body, _, err := c.get(namespacedPath("apps/v1", "deployments", namespace, name))
	if err != nil {
		return nil, err
	}
	var j deploymentJSON
	if err := json.Unmarshal(body, &j); err != nil {
		return nil, fmt.Errorf("parse Deployment %s/%s: %w", namespace, name, err)
	}
	if j.Metadata.Namespace == "" {
		j.Metadata.Namespace = namespace
	}
	d := j.toOperatorDeployment(via)
	return &d, nil
}

// maxUnavailablePods is the number of pods a rolling update may take down,
// as the Deployment controller computes it (pkg/controller/deployment/util
// ResolveFenceposts): maxSurge rounds up, maxUnavailable rounds down
// (defaults 25%), and when both resolve to 0, maxUnavailable becomes 1. ok
// is false when a value does not parse, so the caller assumes the worst.
func (d operatorDeployment) maxUnavailablePods() (int, bool) {
	surge, _, err1 := resolveIntOrPercent(d.MaxSurge, d.Replicas, true, "25%")
	unavailable, _, err2 := resolveIntOrPercent(d.MaxUnavailable, d.Replicas, false, "25%")
	if surge == 0 && unavailable == 0 {
		unavailable = 1
	}
	return unavailable, err1 == nil && err2 == nil
}

// findModuleOperator returns the Deployment that runs a module's operator:
// the known mapping first, else the Platform-owned Deployment in the
// applications namespace labelled for the module or named after it.
func findModuleOperator(c *Client, module, appNS string) (*operatorDeployment, string) {
	if op, ok := moduleOperators[module]; ok {
		d, err := readOperatorDeployment(c, op.namespace, op.name, "known operator of module "+module)
		switch {
		case err == nil:
			return d, ""
		case !IsK8sError(err, http.StatusNotFound):
			return nil, fmt.Sprintf("could not read Deployment %s/%s: %v", op.namespace, op.name, err)
		}
	}
	appNS = nonEmpty(appNS, "redhat-ods-applications")
	body, _, err := c.get(namespacedPath("apps/v1", "deployments", appNS, ""))
	if err != nil {
		return nil, fmt.Sprintf("could not list Deployments in %s: %v", appNS, err)
	}
	var list struct {
		Items []deploymentJSON `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Sprintf("could not parse Deployments in %s", appNS)
	}
	var labelled, named []operatorDeployment
	for _, j := range list.Items {
		if j.Metadata.Namespace == "" {
			j.Metadata.Namespace = appNS
		}
		d := j.toOperatorDeployment("")
		if !containsString(d.OwnerKinds, "Platform") {
			continue
		}
		switch {
		case d.PartOf == module:
			d.Via = fmt.Sprintf("Platform-owned Deployment labelled platform.opendatahub.io/part-of=%s", module)
			labelled = append(labelled, d)
		case strings.Contains(strings.ReplaceAll(d.Name, "-", ""), module):
			d.Via = "Platform-owned Deployment named after module " + module
			named = append(named, d)
		}
	}
	for _, set := range [][]operatorDeployment{labelled, named} {
		if len(set) == 1 {
			return &set[0], ""
		}
		if len(set) > 1 {
			var names []string
			for _, d := range set {
				names = append(names, d.Name)
			}
			sort.Strings(names)
			return nil, fmt.Sprintf("several Platform-owned Deployments in %s could be the %s operator: %s", appNS, module, strings.Join(names, ", "))
		}
	}
	return nil, fmt.Sprintf("no Platform-owned Deployment in %s belongs to module %s", appNS, module)
}

// restartAssessment says whether the tool may restart the Deployment and
// what the restart does.
type restartAssessment struct {
	OK     bool
	Note   string
	Hooks  []string // failurePolicy Fail webhooks its pods serve
	Reason string   // why not, when !OK
}

// assessRestart checks that a rolling restart keeps the module operator,
// and any failurePolicy Fail webhook it serves, available.
func assessRestart(c *Client, d operatorDeployment) restartAssessment {
	if d.Namespace == SubNS && d.Name == SubName {
		return restartAssessment{Reason: "it is the RHOAI operator, whose Deployment belongs to its CSV (OLM reverts template edits); delete its pod instead"}
	}
	if d.Replicas < 1 || d.Available < 1 {
		return restartAssessment{Reason: fmt.Sprintf("Deployment %s has no available pod (%d of %d), so a restart does not make it retry; see the RHOAI pods result for why it is down", d.ref(), d.Available, d.Replicas)}
	}
	hooks, err := webhooksServedBy(c, d)
	if err != nil {
		return restartAssessment{Reason: fmt.Sprintf("could not check which webhooks %s serves (%v), so the effect of a restart is unknown", d.ref(), err)}
	}
	a := restartAssessment{Hooks: hooks}
	unavailable, parsed := d.maxUnavailablePods()
	gap := d.Strategy == "Recreate" || !parsed || unavailable >= d.Available
	switch {
	case len(hooks) > 0 && gap:
		a.Reason = fmt.Sprintf("its pods serve the failurePolicy Fail webhooks %s, and its rollout strategy (%s) stops the running pod before a new one is ready, so matching requests would be rejected meanwhile; restart it yourself at a quiet moment", strings.Join(hooks, ", "), d.strategyText())
		return a
	case len(hooks) > 0:
		a.Note = fmt.Sprintf("Its pods also serve the failurePolicy Fail webhooks %s; the rolling restart keeps the old pod serving until the new one is ready.", strings.Join(hooks, ", "))
	case gap:
		a.Note = "Its rollout strategy stops the running pod first, so the operator pauses for the few seconds it takes to start; it serves no failurePolicy Fail webhook."
	default:
		a.Note = "Kubernetes starts a new pod and stops the old one once the new one is ready."
	}
	a.OK = true
	return a
}

func (d operatorDeployment) strategyText() string {
	if d.Strategy == "Recreate" {
		return "Recreate"
	}
	show := func(v interface{}) string {
		if v == nil {
			return "25%"
		}
		return fmt.Sprint(v)
	}
	unavailable, _ := d.maxUnavailablePods()
	return fmt.Sprintf("RollingUpdate with maxSurge %s and maxUnavailable %s for %s (up to %d unavailable)",
		show(d.MaxSurge), show(d.MaxUnavailable), countNoun(d.Replicas, "replica", "replicas"), unavailable)
}

// webhooksServedBy lists the failurePolicy Fail webhooks whose Service
// selects the Deployment's pods.
func webhooksServedBy(c *Client, d operatorDeployment) ([]string, error) {
	configs, errs := listAdmissionConfigs(c)
	if len(errs) > 0 {
		return nil, fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	selects := map[string]bool{}
	var out []string
	for _, cfg := range configs {
		for _, wh := range cfg.Webhooks {
			svc := wh.ClientConfig.Service
			if svc == nil || svc.Namespace != d.Namespace || (wh.FailurePolicy != nil && *wh.FailurePolicy != "Fail") {
				continue
			}
			sel, seen := selects[svc.Name]
			if !seen {
				var err error
				sel, err = serviceSelects(c, svc.Namespace, svc.Name, d.TemplateLabels)
				if err != nil {
					return nil, err
				}
				selects[svc.Name] = sel
			}
			if sel {
				ref := fmt.Sprintf("%s (%s)", wh.Name, cfg.ref())
				if !containsString(out, ref) {
					out = append(out, ref)
				}
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

func serviceSelects(c *Client, namespace, name string, podLabels map[string]string) (bool, error) {
	body, _, err := c.get(namespacedPath("v1", "services", namespace, name))
	if IsK8sError(err, http.StatusNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read Service %s/%s: %w", namespace, name, err)
	}
	var svc struct {
		Spec struct {
			Selector map[string]string `json:"selector"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(body, &svc); err != nil {
		return false, fmt.Errorf("parse Service %s/%s: %w", namespace, name, err)
	}
	if len(svc.Spec.Selector) == 0 {
		return false, nil
	}
	for k, v := range svc.Spec.Selector {
		if podLabels[k] != v {
			return false, nil
		}
	}
	return true, nil
}

// moduleBackoff is a module whose operator has not retried although what
// it reports has changed.
type moduleBackoff struct {
	Module     string
	Reasons    []string
	Blocking   bool
	Deployment *operatorDeployment
	FindErr    string
	Restart    restartAssessment
}

func (b *moduleBackoff) problemID() string { return "module-operator-backoff-" + b.Module }

func (b *moduleBackoff) problem() Problem {
	severity := "info"
	if b.Blocking {
		severity = "warning"
	}
	p := Problem{
		ID:       b.problemID(),
		Severity: severity,
		Title:    fmt.Sprintf("The %s module operator has not retried yet", b.Module),
		Description: "The module still reports a state that is no longer true. Module operators retry a failed reconcile with an exponential back-off " +
			"that reaches about 16m40s and do not watch the operators they depend on, so the condition can stay for many minutes. A restart makes the operator reconcile at once.",
		Evidence: append([]string{}, b.Reasons...),
	}
	if b.Deployment == nil {
		p.Evidence = append(p.Evidence, "Operator Deployment: "+b.FindErr)
		p.Fix = fmt.Sprintf("Restart the operator that reconciles the %s module (the tool could not tell which Deployment that is), or wait for its next retry.", b.Module)
		p.TechnicalCmd = "oc get deployments -n redhat-ods-applications -o custom-columns=NAME:.metadata.name,OWNER:.metadata.ownerReferences[0].kind,PART-OF:.metadata.labels.platform\\.opendatahub\\.io/part-of"
		return p
	}
	d := b.Deployment
	p.Evidence = append(p.Evidence, fmt.Sprintf("Operator Deployment %s (%s): %d of %d pods available, strategy %s", d.ref(), d.Via, d.Available, d.Replicas, d.strategyText()))
	p.AffectedObjects = []string{"Deployment " + d.ref()}
	if d.Namespace == SubNS && d.Name == SubName {
		p.Fix = "Delete the RHOAI operator pod; its Deployment recreates it and it reconciles at once. Or wait for its next retry."
		p.TechnicalCmd = shellCommand("oc", "delete", "pod", "-n", SubNS, "-l", "name="+SubName)
		return p
	}
	p.TechnicalCmd = shellCommand("oc", "rollout", "restart", "deployment/"+d.Name, "-n", d.Namespace)
	if !b.Restart.OK {
		p.Fix = fmt.Sprintf("Restart %s (command below) or wait for its next retry. The tool does not restart it: %s.", d.ref(), b.Restart.Reason)
		return p
	}
	p.Fix = fmt.Sprintf("Restart %s. %s", d.ref(), b.Restart.Note)
	p.AutoFixable = true
	p.AutoFixAction = "restart-module-operator:" + b.Module
	p.ConfirmMessage = fmt.Sprintf("This patches Deployment %s: it sets the pod-template annotation %s to the current time, which is what oc rollout restart does. %s\n\n"+
		"Right before patching, the tool checks again that the %s module still reports the problem, that a pod is available, and that the Deployment is unchanged since that check (UID and resourceVersion).",
		d.ref(), restartAnnotation, b.Restart.Note, b.Module)
	return p
}

// applyFixRestartModuleOperator restarts a module operator after checking
// again that the module still waits on a retry.
func applyFixRestartModuleOperator(c *Client, module string) (*types.OperationResponse, error) {
	if module == "" || !dns1123Label.MatchString(module) {
		return &types.OperationResponse{Success: false, Message: "restart-module-operator needs a module name", ErrorCode: "validation"}, nil
	}
	a, err := analyzeCurrentDSC(c, time.Now())
	if err != nil {
		return &types.OperationResponse{Success: false, Message: fmt.Sprintf("Cannot read the DataScienceCluster: %v. Nothing was restarted.%s", err, templateHint(err)), ErrorCode: errorCodeFromK8sErr(err)}, nil
	}
	var b *moduleBackoff
	for _, cand := range a.Backoffs {
		if cand.Module == module {
			b = cand
		}
	}
	if b == nil {
		return nothingToDo(fmt.Sprintf("the %s module no longer reports a state its operator has not retried. Nothing was restarted.", module), nil), nil
	}
	logs := append([]string{}, b.Reasons...)
	if b.Deployment == nil {
		return &types.OperationResponse{Success: false, Message: fmt.Sprintf("The tool does not know which Deployment runs the %s operator (%s). Nothing was restarted.", module, b.FindErr), Logs: logs, ErrorCode: "validation"}, nil
	}
	d := b.Deployment
	if !b.Restart.OK {
		return &types.OperationResponse{Success: false, Message: fmt.Sprintf("Not restarting %s: %s. Nothing was changed.", d.ref(), b.Restart.Reason), Logs: logs, ErrorCode: "prerequisites"}, nil
	}
	patch, _ := json.Marshal(map[string]interface{}{
		"metadata": map[string]interface{}{"uid": d.UID, "resourceVersion": d.ResourceVersion},
		"spec": map[string]interface{}{"template": map[string]interface{}{"metadata": map[string]interface{}{
			"annotations": map[string]string{restartAnnotation: time.Now().UTC().Format(time.RFC3339)},
		}}},
	})
	_, _, err = c.patch(namespacedPath("apps/v1", "deployments", d.Namespace, d.Name), patch)
	switch {
	case err == nil:
	case IsK8sError(err, http.StatusConflict) || IsK8sError(err, http.StatusUnprocessableEntity):
		return &types.OperationResponse{Success: false, Message: fmt.Sprintf("Deployment %s changed after it was checked, so it was not restarted. Run diagnostics again.", d.ref()), Logs: logs, ErrorCode: "conflict"}, nil
	case IsK8sError(err, http.StatusNotFound):
		return nothingToDo(fmt.Sprintf("Deployment %s no longer exists. Nothing was restarted.", d.ref()), logs), nil
	default:
		return &types.OperationResponse{Success: false, Message: fmt.Sprintf("Could not restart %s: %v.%s", d.ref(), err, templateHint(err)), Logs: logs, ErrorCode: errorCodeFromK8sErr(err)}, nil
	}
	RecordActivity(c, types.ActivityEntry{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		User:      getUser(c),
		Action:    "fix-restart-module-operator",
		Detail:    fmt.Sprintf("restarted %s (module %s)", d.ref(), module),
		Success:   true,
	})
	return &types.OperationResponse{
		Success: true,
		Message: fmt.Sprintf("Restarted %s. The %s operator reconciles when its new pod starts; re-scan in a minute or two.", d.ref(), module),
		Logs:    append(logs, "Patched the pod template annotation "+restartAnnotation),
	}, nil
}
