package storage

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
)

// NotebookConnectionsAnnotation holds what the dashboard writes for
// Connections bound to a workbench: comma-separated "<namespace>/<name>"
// pairs (odh-dashboard rhoai-3.6 assembleNotebook). The workbenches
// connection webhook (workbenches-operator internal/webhook/notebook/
// mutating.go, connection-notebook.opendatahub.io) aligns the Notebook's
// envFrom with that value, and blocks any UPDATE on a Notebook when the
// annotation references a missing Secret, so Connections come off
// workbenches before removal.
const NotebookConnectionsAnnotation = "opendatahub.io/connections"

// ErrorCodeConfirmWorkbenches signals teardown rejection when workbenches
// still reference the tool's Connection and the caller never confirmed
// stripping it from each one.
const ErrorCodeConfirmWorkbenches = "confirm_connection_workbenches"

// ConnectionWorkbench describes a Notebook whose connections annotation
// lists the tool's Connection, as observed on read.
type ConnectionWorkbench struct {
	Name            string
	UID             string
	ResourceVersion string
	// Annotation stores the full annotation string from the read.
	Annotation string
}

// NotebookList carries Notebooks plus only the fields this tool consumes
// from them.
type NotebookList struct {
	Items []struct {
		Metadata kube.ObjectMeta `json:"metadata"`
	} `json:"items"`
}

// connectionEntry answers whether one annotation fragment refers to the
// given Secret name in namespace, using the same parsing as the webhook
// (parseConnectionsAnnotation: commas and "/" as delimiters, trim each
// piece).
func connectionEntry(part, namespace, name string) bool {
	ns, n, ok := strings.Cut(strings.TrimSpace(part), "/")
	return ok && !strings.Contains(n, "/") && strings.TrimSpace(ns) == namespace && strings.TrimSpace(n) == name
}

// withoutConnection strips entries that name the tool's Connection in
// project from value, preserves every other entry unchanged, and reports
// whether any match was removed.
func withoutConnection(value, project string) (string, bool) {
	parts := strings.Split(value, ",")
	kept := make([]string, 0, len(parts))
	found := false
	for _, p := range parts {
		if connectionEntry(p, project, ConnectionSecretName) {
			found = true
			continue
		}
		if strings.TrimSpace(p) != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, ","), found
}

// ConnectionWorkbenches selects Notebooks in project whose connections
// annotation includes the tool's Connection, ordered by name.
func ConnectionWorkbenches(list NotebookList, project string) []ConnectionWorkbench {
	var out []ConnectionWorkbench
	for _, nb := range list.Items {
		m := nb.Metadata
		if m.Namespace != "" && m.Namespace != project {
			continue
		}
		value, ok := m.Annotations[NotebookConnectionsAnnotation]
		if !ok {
			continue
		}
		if _, named := withoutConnection(value, project); named {
			out = append(out, ConnectionWorkbench{Name: m.Name, UID: m.UID, ResourceVersion: m.ResourceVersion, Annotation: value})
		}
	}
	slices.SortFunc(out, func(a, b ConnectionWorkbench) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// ConnectionWorkbenchNames extracts names from wbs.
func ConnectionWorkbenchNames(wbs []ConnectionWorkbench) []string {
	names := make([]string, 0, len(wbs))
	for _, w := range wbs {
		names = append(names, w.Name)
	}
	return names
}

// listConnectionWorkbenches fetches Notebooks in project that reference
// the tool's Connection. Clusters lacking the Notebook API yield an empty list.
func listConnectionWorkbenches(c *kube.Client, project string) ([]ConnectionWorkbench, error) {
	var list NotebookList
	found, err := c.GetIntoOptional(rhoai.NamespacedPath("kubeflow.org/v1", "notebooks", project, ""), &list)
	if err != nil || !found {
		return nil, err
	}
	return ConnectionWorkbenches(list, project), nil
}

// connectionWorkbenchesToDetach is the pre-mutation guard for teardown:
// workbenches in project that depend on the tool's Connection (empty when
// no such Connection exists, or when the existing one is not the tool's
// and therefore stays), each requiring explicit user confirmation. The
// logic fails closed—unreadable Connection or workbench data, or any
// workbench left unconfirmed, yields a refusal message and code rather
// than proceeding.
func connectionWorkbenchesToDetach(c *kube.Client, project string, confirmed []string) ([]ConnectionWorkbench, string, string) {
	meta, found, err := kube.ReadObjectMeta(c, rhoai.NamespacedPath("v1", "secrets", project, ConnectionSecretName))
	switch {
	case err != nil:
		return nil, fmt.Sprintf("Cannot check the dashboard connection %s in %s: %v.", ConnectionSecretName, project, err), kube.ErrorCodeFromK8sErr(err)
	case !found || !meta.HasToolLabel():
		return nil, "", ""
	}
	wbs, err := listConnectionWorkbenches(c, project)
	if err != nil {
		return nil, fmt.Sprintf("Cannot check which workbenches in %s use the dashboard connection %s: %v.", project, ConnectionSecretName, err), kube.ErrorCodeFromK8sErr(err)
	}
	var unconfirmed []string
	for _, w := range wbs {
		if !slices.Contains(confirmed, w.Name) {
			unconfirmed = append(unconfirmed, w.Name)
		}
	}
	if len(unconfirmed) > 0 {
		n := len(unconfirmed)
		return nil, fmt.Sprintf("%s in %s %s the dashboard connection %s, and removing it from %s was not confirmed: %s. Review %s in the Delete dialog.",
			kube.CountNoun(n, "workbench", "workbenches"), project, kube.Verb(n, "uses", "use"), ConnectionSecretName,
			kube.Verb(n, "it", "them"), strings.Join(unconfirmed, ", "), kube.Verb(n, "it", "them")), ErrorCodeConfirmWorkbenches
	}
	return wbs, "", ""
}

// ConnectionWorkbenchesProblem runs connectionWorkbenchesToDetach in
// isolation (Clean up invokes it for each pipeline server ahead of any
// delete): it returns refusal message and code, plus the names of
// workbenches currently tied to the Connection.
func ConnectionWorkbenchesProblem(c *kube.Client, project string, confirmed []string) (string, string, []string) {
	wbs, problem, code := connectionWorkbenchesToDetach(c, project, confirmed)
	if code == ErrorCodeConfirmWorkbenches {
		if all, err := listConnectionWorkbenches(c, project); err == nil {
			wbs = all
		}
	}
	return problem, code, ConnectionWorkbenchNames(wbs)
}

// detachConnection clears the tool's Connection from every workbench in
// wbs using the snapshot from read: per Notebook, one JSON patch that
// asserts UID, resourceVersion, and annotation value, then removes only
// that entry (drop the annotation key when nothing remains). The webhook
// subsequently clears the Connection's envFrom on its own. Processing
// halts on the first error with a message; patches already applied stay
// applied, and a later teardown pass only sees what is left.
func detachConnection(c *kube.Client, project string, wbs []ConnectionWorkbench, logs *[]string) (string, string) {
	pointer := "/metadata/annotations/" + strings.ReplaceAll(NotebookConnectionsAnnotation, "/", "~1")
	for _, w := range wbs {
		rest, _ := withoutConnection(w.Annotation, project)
		uid, _ := json.Marshal(w.UID)
		rv, _ := json.Marshal(w.ResourceVersion)
		was, _ := json.Marshal(w.Annotation)
		ops := []kube.JSONPatchOp{
			{Op: "test", Path: "/metadata/uid", Value: uid},
			{Op: "test", Path: "/metadata/resourceVersion", Value: rv},
			{Op: "test", Path: pointer, Value: was},
		}
		if rest == "" {
			ops = append(ops, kube.JSONPatchOp{Op: "remove", Path: pointer})
		} else {
			next, _ := json.Marshal(rest)
			ops = append(ops, kube.JSONPatchOp{Op: "replace", Path: pointer, Value: next})
		}
		body, err := json.Marshal(ops)
		if err != nil {
			return err.Error(), "internal"
		}
		*logs = append(*logs, fmt.Sprintf("Removing connection %s from workbench %s...", ConnectionSecretName, w.Name))
		_, _, err = c.Do(http.MethodPatch, rhoai.NamespacedPath("kubeflow.org/v1", "notebooks", project, w.Name), "application/json-patch+json", body, url.Values{"fieldManager": {envkit.ToolFieldManager}})
		if err != nil {
			*logs = append(*logs, fmt.Sprintf("Warning: workbench %s was not changed: %v", w.Name, err))
			why, code := err.Error(), kube.ErrorCodeFromK8sErr(err)
			if kube.IsK8sConflict(err) || kube.IsK8sError(err, http.StatusUnprocessableEntity) || kube.IsK8sNotFound(err) {
				why, code = "it changed since it was read", "conflict"
			}
			return fmt.Sprintf("Cannot remove the dashboard connection %s from workbench %s in %s (%s). Nothing was deleted; run the deletion again to retry.", ConnectionSecretName, w.Name, project, why), code
		}
		*logs = append(*logs, fmt.Sprintf("OK: Workbench %s no longer uses connection %s", w.Name, ConnectionSecretName))
	}
	return "", ""
}
