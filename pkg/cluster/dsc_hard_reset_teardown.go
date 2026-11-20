package cluster

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// What deleting the DataScienceCluster sets off, read from the source of
// rhods-operator rhoai-3.6 @0ea7206c (R), odh-platform-utilities
// framework/v0.3.1 @d7856ba3 (FW) and kubernetes v1.34 @f28b4c9e (K):
//
//  1. The DSC finalizer platform.opendatahub.io/finalizer runs
//     disableDSCModulesOnDelete (R internal/controller/datasciencecluster/
//     datasciencecluster_controller_actions.go:97-113): it server-side
//     applies Removed for the DSC's modules (ConfigFromDSC; monitoring is
//     the DSCInitialization's and stays) on the Platform "default", and
//     nothing else. The framework removes the finalizer in the same
//     reconcile once the finalizer actions return (FW controller/reconciler/
//     reconciler.go:362-374). So the DSC is gone within a reconcile; it does
//     not wait for any module.
//  2. The DSC owns the module CRs and the Kueue component CR (its deploy
//     action applies them with dynamic ownership, R datasciencecluster
//     controller WithDynamicOwnership and provisionModuleCRs/
//     provisionComponents). Once the DSC is gone, the garbage collector
//     deletes them. With propagationPolicy Background (what the operator's
//     own deletion test uses, R tests/e2e/deletion_test.go TestDSCDeletion)
//     they are deleted as any owner-less object: each module CR keeps its
//     own finalizer until its module operator has cleaned up. With
//     Foreground the collector would delete every dependent that has
//     dependents of its own in the foreground too (K pkg/controller/
//     garbagecollector/garbagecollector.go:591-623): the workloads a module
//     CR owns would go while its module operator may still need them for
//     its finalizer. So the hard reset deletes in the background.
//  3. The Platform controller cleans up the modules marked Removed
//     (cleanupDisabledModules, R internal/controller/modules/
//     modules_controller_actions.go:255-341): while a module CR is
//     deleting it keeps the module operator; once the CR is gone ("phase
//     2", :301-304) it deletes the operator's resources (its Deployment,
//     RBAC, chart objects). It reports the modules on the Platform's
//     ModulesReady condition (writeAggregateCondition, :763-806): a
//     disabled module whose CR still exists is "pending deletion" (reason
//     Removed while nothing else is wrong, otherwise "; pending deletion:
//     ..." in the message). The condition is written in the same status
//     update as ProvisioningSucceeded, which carries the generation the
//     reconcile saw (FW reconciler.go:534-545, one ApplyStatus at :577); a
//     reconcile whose actions stopped before updateModuleStatus leaves
//     ModulesReady "ConditionNotSet" (FW conditions_manager.go:198-225).
//
// So the new DSC is created only when (a) the old DSC is gone, (b) no
// object of a module kind (both module API groups) is owned by the old DSC
// any more, (c) the module operator Deployments of the module CRs it owned
// are gone or replaced (phase 2 for each), and (d) the Platform "default"
// reports, for the generation it is at, no module pending deletion. A
// read error is retried until the deadline; the reset then stops and says
// what is still there.

// hardResetOperator is the operator Deployment of a module CR the old DSC
// owned, as it was before the delete.
type hardResetOperator struct {
	Module, Namespace, Name, UID string
}

func (o hardResetOperator) record() string { return o.Namespace + "/" + o.Name + "/" + o.UID }

func parseHardResetOperator(s string) (hardResetOperator, bool) {
	parts := strings.Split(s, "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return hardResetOperator{}, false
	}
	return hardResetOperator{Namespace: parts[0], Name: parts[1], UID: parts[2]}, true
}

// ownedModuleCR is an object of a module kind owned by the old DSC.
type ownedModuleCR struct {
	Kind       componentKind
	Name       string
	Finalizers []string
	Deleting   string
}

func (o ownedModuleCR) text() string {
	s := o.Kind.Kind + " " + o.Name
	if o.Deleting != "" {
		s += fmt.Sprintf(" (deleting since %s, finalizers %s)", o.Deleting, nonEmpty(strings.Join(o.Finalizers, ", "), "none"))
	}
	return s
}

// moduleCRsOwnedBy lists the objects of every module kind (both module API
// groups) that have an ownerReference to uid.
func moduleCRsOwnedBy(c *Client, uid string) ([]ownedModuleCR, error) {
	api, err := discoverComponentAPI(c)
	if err != nil {
		return nil, err
	}
	var out []ownedModuleCR
	for _, k := range api.Kinds {
		if k.Namespaced {
			continue // module CRs are cluster-scoped; a DSC cannot own namespaced objects
		}
		items, err := listKindCRs(c, k)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			for _, ref := range it.Metadata.OwnerReferences {
				if ref.UID == uid {
					out = append(out, ownedModuleCR{Kind: k, Name: it.Metadata.Name, Finalizers: it.Metadata.Finalizers, Deleting: it.Metadata.DeletionTimestamp})
					break
				}
			}
		}
	}
	return out, nil
}

// hardResetOperators returns the operator Deployments of the module CRs the
// DSC owns. A module CR whose operator the tool cannot identify, or whose
// operator the DSC itself owns (the collector would delete it with the DSC
// while the CR still needs it), is a refusal. Kinds the RHOAI operator
// reconciles itself (Kueue) have no Deployment to wait for.
func hardResetOperators(c *Client, dscUID string) ([]hardResetOperator, string, error) {
	owned, err := moduleCRsOwnedBy(c, dscUID)
	if err != nil {
		return nil, "", err
	}
	finder := newModuleOperatorFinder(c, nil)
	var out []hardResetOperator
	seen := map[string]bool{}
	var refusals []string
	for _, cr := range owned {
		op, why, err := finder.find(cr.Kind.module(), cr.Name)
		switch {
		case err != nil:
			return nil, "", fmt.Errorf("identify the operator of %s %s: %w", cr.Kind.Kind, cr.Name, err)
		case op == nil:
			refusals = append(refusals, fmt.Sprintf("%s %s: the tool cannot identify its module operator (%s), so it cannot tell when the platform has removed it", cr.Kind.Kind, cr.Name, why))
			continue
		case op.RHOAI:
			continue
		case strings.HasPrefix(op.Owner, "DataScienceCluster "):
			refusals = append(refusals, fmt.Sprintf("%s %s: its operator %s is owned by the DataScienceCluster, so deleting the DSC would garbage-collect it while %s still needs it to remove its finalizer", cr.Kind.Kind, cr.Name, op.ref(), cr.Name))
			continue
		}
		if seen[op.ref()] {
			continue
		}
		seen[op.ref()] = true
		body, _, err := c.get(namespacedPath("apps/v1", "deployments", op.Namespace, op.Name))
		if err != nil {
			return nil, "", fmt.Errorf("read Deployment %s: %w", op.ref(), err)
		}
		var d struct {
			Metadata objectMeta `json:"metadata"`
		}
		if err := json.Unmarshal(body, &d); err != nil || d.Metadata.UID == "" {
			return nil, "", fmt.Errorf("read Deployment %s: no uid", op.ref())
		}
		out = append(out, hardResetOperator{Module: cr.Kind.module(), Namespace: op.Namespace, Name: op.Name, UID: d.Metadata.UID})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].record() < out[j].record() })
	return out, strings.Join(refusals, "; "), nil
}

// operatorsLeft returns the recorded operator Deployments that still exist
// with the recorded UID.
func operatorsLeft(c *Client, ops []hardResetOperator) ([]hardResetOperator, error) {
	var left []hardResetOperator
	for _, o := range ops {
		body, _, err := c.get(namespacedPath("apps/v1", "deployments", o.Namespace, o.Name))
		if IsK8sError(err, http.StatusNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read Deployment %s/%s: %w", o.Namespace, o.Name, err)
		}
		var d struct {
			Metadata objectMeta `json:"metadata"`
		}
		if err := json.Unmarshal(body, &d); err != nil {
			return nil, fmt.Errorf("parse Deployment %s/%s: %w", o.Namespace, o.Name, err)
		}
		if d.Metadata.UID == o.UID {
			left = append(left, o)
		}
	}
	return left, nil
}

// platformModulesSettled reports whether the Platform "default" says, for
// the generation it is at, that no module is pending deletion (see the top
// of this file). Without the Platform API (operators before 3.6) or without
// a Platform "default" there is nothing to wait for: the modules
// controller reconciles only "default" (R modules_controller.go:79), and
// module CRs are checked separately. A Platform being deleted is not
// settled (its finalizer waitForModuleCRDeletion holds it while module CRs
// exist, R modules_controller_actions.go:343-391).
func platformModulesSettled(c *Client) (bool, string, error) {
	r, err := listServed(c, platformGroup, platformListFmt, nil, platformFallbackVersions)
	switch {
	case IsK8sError(err, http.StatusNotFound), err == nil && r.Version == "":
		return true, "no Platform API is served", nil
	case err != nil:
		return false, "", fmt.Errorf("read the Platform: %w", err)
	}
	var list struct {
		Items []struct {
			Metadata objectMeta `json:"metadata"`
			Status   struct {
				Conditions []struct {
					dscCondition
					ObservedGeneration int64 `json:"observedGeneration"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(r.Body, &list); err != nil {
		return false, "", fmt.Errorf("parse the Platform: %w", err)
	}
	for _, p := range list.Items {
		if p.Metadata.Name != "default" {
			continue
		}
		if p.Metadata.DeletionTimestamp != "" {
			return false, fmt.Sprintf("Platform default is being deleted (finalizers %s)", nonEmpty(strings.Join(p.Metadata.Finalizers, ", "), "none")), nil
		}
		var provisioned, modules *dscCondition
		var observed int64
		for i := range p.Status.Conditions {
			switch cd := &p.Status.Conditions[i]; cd.Type {
			case "ProvisioningSucceeded":
				provisioned, observed = &cd.dscCondition, cd.ObservedGeneration
			case "ModulesReady":
				modules = &cd.dscCondition
			}
		}
		switch {
		case provisioned == nil || observed != p.Metadata.Generation:
			return false, fmt.Sprintf("Platform default has not reported a reconcile of its generation %d yet (ProvisioningSucceeded observedGeneration %d)", p.Metadata.Generation, observed), nil
		case modules == nil:
			return false, "Platform default reports no ModulesReady condition", nil
		case modules.Reason == "ConditionNotSet":
			return false, "Platform default's last reconcile stopped before it checked the modules (ModulesReady: " + truncateText(modules.Message, 300) + "; ProvisioningSucceeded: " + truncateText(provisioned.Message, 300) + ")", nil
		case modules.Reason == "Removed" || strings.Contains(modules.Message, "pending deletion"):
			return false, "Platform default: ModulesReady " + modules.Status + " (" + modules.Reason + "): " + truncateText(modules.Message, 300), nil
		}
		return true, "Platform default reports no module pending deletion (ModulesReady " + modules.Status + ", " + nonEmpty(modules.Reason, "no reason") + ")", nil
	}
	return true, "no Platform default exists", nil
}

// teardownState is what still stands between the deleted DSC and the
// create: "" when nothing.
type teardownState struct {
	ModuleCRs []ownedModuleCR
	Operators []hardResetOperator
	Platform  string // why the Platform is not settled
}

func (s teardownState) done() bool {
	return len(s.ModuleCRs) == 0 && len(s.Operators) == 0 && s.Platform == ""
}

// progress words the state for a step message.
func (s teardownState) progress() string {
	switch {
	case len(s.ModuleCRs) > 0:
		var names []string
		for _, cr := range s.ModuleCRs {
			names = append(names, cr.text())
		}
		return "Waiting for the module CRs the DataScienceCluster owned to be deleted: " + strings.Join(names, "; ")
	case len(s.Operators) > 0:
		var names []string
		for _, o := range s.Operators {
			names = append(names, o.Namespace+"/"+o.Name)
		}
		return "Waiting for the platform to remove the module operators of the deleted module CRs: Deployment " + strings.Join(names, ", ")
	case s.Platform != "":
		return "Waiting for the Platform to report the modules removed: " + s.Platform
	}
	return "Every module the DataScienceCluster owned is cleaned up"
}

// stuck words, for the timeout, what is still there and what to check.
func (s teardownState) stuck() string {
	switch {
	case len(s.ModuleCRs) > 0:
		var names []string
		for _, cr := range s.ModuleCRs {
			names = append(names, cr.text())
		}
		return "these module CRs it owned are still there: " + strings.Join(names, "; ") + ". Each one's module operator removes its finalizer; Diagnostics (Platform modules) names the operator and why it does not finish. Do not remove the finalizers by hand: the module's cleanup would be skipped"
	case len(s.Operators) > 0:
		var names []string
		for _, o := range s.Operators {
			names = append(names, "Deployment "+o.Namespace+"/"+o.Name+" ("+o.Module+")")
		}
		return "their module CRs are gone, but the platform has not removed these module operators yet: " + strings.Join(names, ", ") + ". The Platform controller deletes them once their CR is gone; check its conditions with: oc get platforms.config.opendatahub.io default -o yaml"
	case s.Platform != "":
		return "the Platform does not report its modules removed yet: " + s.Platform + ". Check it with: oc get platforms.config.opendatahub.io default -o yaml"
	}
	return ""
}

// readTeardown reads what is still there after the DSC uid is gone.
func readTeardown(c *Client, uid string, ops []hardResetOperator) (teardownState, error) {
	var st teardownState
	var err error
	if st.ModuleCRs, err = moduleCRsOwnedBy(c, uid); err != nil || len(st.ModuleCRs) > 0 {
		return st, err
	}
	if st.Operators, err = operatorsLeft(c, ops); err != nil || len(st.Operators) > 0 {
		return st, err
	}
	settled, why, err := platformModulesSettled(c)
	if err != nil {
		return st, err
	}
	if !settled {
		st.Platform = why
	}
	return st, nil
}

// --- namespaces owned through the DataScienceCluster -------------------------

// maxOwnerDepth bounds how far namespaceOwnerBlocker follows owners.
const maxOwnerDepth = 8

// namespaceOwnerBlocker refuses the hard reset when a namespace could be
// garbage-collected with the DSC: the collector deletes an object whose
// owners are all gone (K garbagecollector.go attemptToDeleteItem), and a
// namespace takes everything in it. A namespace is refused when the chain
// of its ownerReferences reaches an object of an *.opendatahub.io (or
// opendatahub.io) API group: the DataScienceCluster, a module CR or the
// Platform, or anything else of RHOAI's, which the tool does not try to
// tell apart. Owners of other groups are read and their owners followed
// (cluster-scoped: a cluster-scoped object can only have cluster-scoped
// owners). Anything that cannot be read or resolved refuses (fail closed).
// It returns "" when no namespace is owned that way.
func namespaceOwnerBlocker(c *Client) string {
	body, _, err := c.get("/api/v1/namespaces")
	if err != nil {
		return "the namespaces could not be listed to check that none would be deleted with the DataScienceCluster: " + err.Error()
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name            string     `json:"name"`
				OwnerReferences []ownerRef `json:"ownerReferences"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return "the namespaces could not be read to check that none would be deleted with the DataScienceCluster: " + err.Error()
	}
	resolver := &ownerResolver{c: c, docs: map[string][]apiResource{}}
	var why []string
	for _, ns := range list.Items {
		for _, ref := range ns.Metadata.OwnerReferences {
			if reason := resolver.leadsToRHOAI(ref, 1, map[string]bool{}); reason != "" {
				why = append(why, fmt.Sprintf("namespace %s: %s", ns.Metadata.Name, reason))
			}
		}
	}
	if len(why) == 0 {
		return ""
	}
	return "deleting the DataScienceCluster could garbage-collect namespaces with everything in them: " + strings.Join(why, "; ")
}

// isRHOAIGroup: opendatahub.io or one of its subdomains.
func isRHOAIGroup(group string) bool {
	return group == "opendatahub.io" || strings.HasSuffix(group, ".opendatahub.io")
}

type apiResource struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Namespaced bool   `json:"namespaced"`
}

// ownerResolver reads owners of any cluster-scoped kind, finding their
// resource from API discovery.
type ownerResolver struct {
	c    *Client
	docs map[string][]apiResource
}

// leadsToRHOAI says how ref leads to an *.opendatahub.io object ("" when it
// does not), or why the tool cannot tell.
func (r *ownerResolver) leadsToRHOAI(ref ownerRef, depth int, seen map[string]bool) string {
	group, version, found := strings.Cut(ref.APIVersion, "/")
	if !found {
		group, version = "", ref.APIVersion
	}
	what := fmt.Sprintf("%s %s (%s)", ref.Kind, ref.Name, nonEmpty(ref.APIVersion, "no apiVersion"))
	switch {
	case isRHOAIGroup(group):
		return "owned through " + what
	case depth > maxOwnerDepth:
		return "its owner chain is longer than the tool follows, at " + what
	case seen[ref.UID]:
		return ""
	}
	seen[ref.UID] = true
	path, why := r.path(group, version, ref.Kind, ref.Name)
	if why != "" {
		return "the tool cannot tell whether its owner " + what + " leads to the DataScienceCluster: " + why
	}
	body, _, err := r.c.get(path)
	if IsK8sError(err, http.StatusNotFound) {
		return "" // gone: it leads nowhere
	}
	if err != nil {
		return "the tool cannot tell whether its owner " + what + " leads to the DataScienceCluster: " + err.Error()
	}
	var obj struct {
		Metadata struct {
			UID             string     `json:"uid"`
			OwnerReferences []ownerRef `json:"ownerReferences"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		return "the tool cannot tell whether its owner " + what + " leads to the DataScienceCluster: " + err.Error()
	}
	if ref.UID != "" && obj.Metadata.UID != ref.UID {
		return "" // another object of that name: the referenced owner is gone
	}
	for _, next := range obj.Metadata.OwnerReferences {
		if reason := r.leadsToRHOAI(next, depth+1, seen); reason != "" {
			return what + " is " + reason
		}
	}
	return ""
}

// path is the API path of a cluster-scoped object of kind in group/version.
func (r *ownerResolver) path(group, version, kind, name string) (string, string) {
	if version == "" || kind == "" || name == "" {
		return "", "the owner reference is incomplete"
	}
	gv := "/apis/" + group + "/" + version
	if group == "" {
		gv = "/api/" + version
	}
	resources, ok := r.docs[gv]
	if !ok {
		body, _, err := r.c.get(gv)
		if err != nil {
			return "", "API discovery of " + strings.TrimPrefix(gv, "/") + ": " + err.Error()
		}
		var doc struct {
			Resources []apiResource `json:"resources"`
		}
		if err := json.Unmarshal(body, &doc); err != nil {
			return "", "API discovery of " + strings.TrimPrefix(gv, "/") + ": " + err.Error()
		}
		resources = doc.Resources
		r.docs[gv] = resources
	}
	for _, res := range resources {
		if res.Kind == kind && !strings.Contains(res.Name, "/") {
			if res.Namespaced {
				return "", kind + " is namespaced, and a namespace cannot have a namespaced owner"
			}
			return gv + "/" + res.Name + "/" + name, ""
		}
	}
	return "", "no served resource has kind " + kind
}
