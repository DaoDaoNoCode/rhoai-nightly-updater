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

// connectionUsers are the workbenches a teardown takes the tool's
// Connection off; nil when there is no such Connection (absent, or one that
// is not the tool's and stays).
type connectionUsers struct {
	wbs []ConnectionWorkbench
}

// unconfirmedRefusal is the refusal for workbenches that use the Connection
// and were not confirmed as they are now (by name and UID).
func unconfirmedRefusal(project string, names []string, since string) string {
	n := len(names)
	return fmt.Sprintf("%s in %s %s the dashboard connection %s%s, and removing it from %s was not confirmed: %s. Nothing was deleted. Review %s in the Delete dialog.",
		kube.CountNoun(n, "workbench", "workbenches"), project, kube.Verb(n, "uses", "use"), ConnectionSecretName, since,
		kube.Verb(n, "it", "them"), strings.Join(names, ", "), kube.Verb(n, "it", "them"))
}

// connectionWorkbenchesToDetach is the check a teardown makes before it
// changes anything: the workbenches of project that use the tool's
// Connection, each of which the user must have confirmed as the Notebook
// they reviewed (confirmed maps its name to its UID; one deleted and made
// again since under the same name is not confirmed). It fails closed: a
// Connection or workbench list that cannot be read, or a workbench that is
// not confirmed, gives a refusal message and code instead.
func connectionWorkbenchesToDetach(c *kube.Client, project string, confirmed map[string]string) (*connectionUsers, string, string) {
	meta, found, err := kube.ReadObjectMeta(c, rhoai.NamespacedPath("v1", "secrets", project, ConnectionSecretName))
	switch {
	case err != nil:
		return nil, fmt.Sprintf("Cannot check the dashboard connection %s in %s: %v. Nothing was deleted.", ConnectionSecretName, project, err), kube.ErrorCodeFromK8sErr(err)
	case !found || !meta.HasToolLabel():
		return nil, "", ""
	}
	wbs, err := listConnectionWorkbenches(c, project)
	if err != nil {
		return nil, fmt.Sprintf("Cannot check which workbenches in %s use the dashboard connection %s: %v. Nothing was deleted.", project, ConnectionSecretName, err), kube.ErrorCodeFromK8sErr(err)
	}
	var unconfirmed []string
	for _, w := range wbs {
		if uid, ok := confirmed[w.Name]; !ok || uid == "" || uid != w.UID {
			unconfirmed = append(unconfirmed, w.Name)
		}
	}
	if len(unconfirmed) > 0 {
		return nil, unconfirmedRefusal(project, unconfirmed, ""), ErrorCodeConfirmWorkbenches
	}
	return &connectionUsers{wbs: wbs}, "", ""
}

// ConnectionWorkbenchesProblem is the check of connectionWorkbenchesToDetach
// on its own (Clean up runs it for every pipeline server before it deletes
// anything, and again to list them when a teardown refused): a refusal
// message and code, and the workbenches that use the Connection now.
func ConnectionWorkbenchesProblem(c *kube.Client, project string, confirmed map[string]string) (string, string, []ConnectionWorkbench) {
	users, problem, code := connectionWorkbenchesToDetach(c, project, confirmed)
	if code == ErrorCodeConfirmWorkbenches {
		all, err := listConnectionWorkbenches(c, project)
		if err != nil {
			return problem, code, nil
		}
		return problem, code, all
	}
	if users == nil {
		return problem, code, nil
	}
	return problem, code, users.wbs
}

// detachConnection clears the tool's Connection from every workbench in
// wbs using the snapshot from read: per Notebook, one JSON patch that
// asserts UID, resourceVersion, and annotation value, then removes only
// that entry (drop the annotation key when nothing remains). The webhook
// subsequently clears the Connection's envFrom on its own. Processing
// halts on the first error with a message; patches already applied stay
// applied, and a later teardown pass only sees what is left.
func detachConnection(c *kube.Client, project string, users *connectionUsers, logs *[]string) (string, string) {
	if users == nil {
		return "", ""
	}
	pointer := "/metadata/annotations/" + strings.ReplaceAll(NotebookConnectionsAnnotation, "/", "~1")
	for _, w := range users.wbs {
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
	// A workbench may have attached the Connection (again) since the list
	// was read: read it once more right before the delete, and refuse while
	// any workbench names it, so none is left naming a deleted Secret.
	left, err := listConnectionWorkbenches(c, project)
	if err != nil {
		return fmt.Sprintf("Cannot check again which workbenches in %s use the dashboard connection %s: %v. Nothing was deleted.", project, ConnectionSecretName, err), kube.ErrorCodeFromK8sErr(err)
	}
	if len(left) > 0 {
		*logs = append(*logs, fmt.Sprintf("Workbenches use connection %s again: %s", ConnectionSecretName, strings.Join(ConnectionWorkbenchNames(left), ", ")))
		return unconfirmedRefusal(project, ConnectionWorkbenchNames(left), " (since the deletion started)"), ErrorCodeConfirmWorkbenches
	}
	return "", ""
}
