package storage

import (
	"encoding/json"
	"errors"
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
	// Running: the Notebook has no kubeflow-resource-stopped annotation, so
	// its pod runs (or starts). Taking the Connection off changes its pod
	// template (the webhook drops the envFrom), which restarts that pod.
	Running bool
}

// NotebookStoppedAnnotation is set on a stopped workbench (kubeflow
// notebook-controller; the dashboard's Stop sets it).
const NotebookStoppedAnnotation = "kubeflow-resource-stopped"

// ErrorCodeWebhookRefused is a workbench change the workbenches connection
// webhook denied (for example because another Connection the workbench
// names no longer exists).
const ErrorCodeWebhookRefused = "webhook_refused"

// NotebookList carries Notebooks plus only the fields this tool consumes
// from them.
type NotebookList struct {
	Items []struct {
		Metadata kube.ObjectMeta `json:"metadata"`
		Spec     struct {
			Template struct {
				Spec struct {
					Containers []struct {
						EnvFrom []struct {
							SecretRef *struct {
								Name string `json:"name"`
							} `json:"secretRef"`
						} `json:"envFrom"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
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

// withoutConnection strips entries that name the tool's Connection conn
// in project from value, preserves every other entry unchanged, and
// reports whether any match was removed.
func withoutConnection(value, project, conn string) (string, bool) {
	parts := strings.Split(value, ",")
	kept := make([]string, 0, len(parts))
	found := false
	for _, p := range parts {
		if connectionEntry(p, project, conn) {
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
// annotation includes the tool's Connection conn (ConnectionNameOf its
// pipeline server), ordered by name.
func ConnectionWorkbenches(list NotebookList, project, conn string) []ConnectionWorkbench {
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
		if _, named := withoutConnection(value, project, conn); named {
			_, stopped := m.Annotations[NotebookStoppedAnnotation]
			out = append(out, ConnectionWorkbench{Name: m.Name, UID: m.UID, ResourceVersion: m.ResourceVersion, Annotation: value, Running: !stopped})
		}
	}
	slices.SortFunc(out, func(a, b ConnectionWorkbench) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// ConnectionEnvOnlyWorkbenches are the Notebooks of project whose
// containers load the tool's Connection conn through envFrom while their
// connections annotation does not name it (as workbenches of RHOAI 2.x
// were attached), sorted. Nothing takes the Connection off them: after it
// is deleted, their pods fail to start until it is removed in the
// dashboard.
func ConnectionEnvOnlyWorkbenches(list NotebookList, project, conn string) []string {
	var out []string
	for _, nb := range list.Items {
		m := nb.Metadata
		if m.Namespace != "" && m.Namespace != project {
			continue
		}
		if _, named := withoutConnection(m.Annotations[NotebookConnectionsAnnotation], project, conn); named {
			continue
		}
	containers:
		for _, ct := range nb.Spec.Template.Spec.Containers {
			for _, e := range ct.EnvFrom {
				if e.SecretRef != nil && e.SecretRef.Name == conn {
					out = append(out, m.Name)
					break containers
				}
			}
		}
	}
	slices.Sort(out)
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
// the tool's Connection conn. Clusters lacking the Notebook API yield an
// empty list.
func listConnectionWorkbenches(c *kube.Client, project, conn string) ([]ConnectionWorkbench, []string, error) {
	var list NotebookList
	found, err := c.GetIntoOptional(rhoai.NamespacedPath("kubeflow.org/v1", "notebooks", project, ""), &list)
	if err != nil || !found {
		return nil, nil, err
	}
	return ConnectionWorkbenches(list, project, conn), ConnectionEnvOnlyWorkbenches(list, project, conn), nil
}

// connectionUsers are the workbenches a teardown takes the tool's
// Connection off; nil when there is no such Connection (absent, or one that
// is not the tool's and stays).
type connectionUsers struct {
	// conn is the Connection (ConnectionNameOf the pipeline server).
	conn string
	wbs  []ConnectionWorkbench
	// envOnly load the Connection through envFrom only
	// (ConnectionEnvOnlyWorkbenches): reported, never changed.
	envOnly []string
}

// envOnlyNote is what a teardown says about envOnly workbenches.
func envOnlyNote(project string, users *connectionUsers) string {
	if users == nil || len(users.envOnly) == 0 {
		return ""
	}
	n := len(users.envOnly)
	return fmt.Sprintf(" %s in %s %s the dashboard connection %s without naming it in %s, so it was not taken off %s: %s. %s fail to start now that the connection is deleted; remove it from %s in the dashboard.",
		kube.CountNoun(n, "workbench", "workbenches"), project, kube.Verb(n, "loads", "load"), users.conn, NotebookConnectionsAnnotation,
		kube.Verb(n, "it", "them"), strings.Join(users.envOnly, ", "), kube.Verb(n, "It will", "They will"), kube.Verb(n, "it", "them"))
}

// unconfirmedRefusal is the refusal for workbenches that use the Connection
// and were not confirmed as they are now (by name and UID).
func unconfirmedRefusal(project, conn string, names []string, since string) string {
	n := len(names)
	return fmt.Sprintf("%s in %s %s the dashboard connection %s%s, and removing it from %s was not confirmed: %s. Nothing was deleted. Review %s in the Delete dialog.",
		kube.CountNoun(n, "workbench", "workbenches"), project, kube.Verb(n, "uses", "use"), conn, since,
		kube.Verb(n, "it", "them"), strings.Join(names, ", "), kube.Verb(n, "it", "them"))
}

// connectionWorkbenchesToDetach is the check a teardown makes before it
// changes anything: the workbenches of project that use the tool's
// Connection, each of which the user must have confirmed as the Notebook
// they reviewed (confirmed maps its name to its UID; one deleted and made
// again since under the same name is not confirmed). It fails closed: a
// Connection or workbench list that cannot be read, or a workbench that is
// not confirmed, gives a refusal message and code instead.
//
// connPath is the Connection's path (the caller's literal one).
func connectionWorkbenchesToDetach(c *kube.Client, project, conn, connPath string, confirmed map[string]string) (*connectionUsers, string, string) {
	meta, found, err := kube.ReadObjectMeta(c, connPath)
	switch {
	case err != nil:
		return nil, fmt.Sprintf("Cannot check the dashboard connection %s in %s: %v. Nothing was deleted.", conn, project, err), kube.ErrorCodeFromK8sErr(err)
	case !found || !meta.HasToolLabel():
		return nil, "", ""
	}
	wbs, envOnly, err := listConnectionWorkbenches(c, project, conn)
	if err != nil {
		return nil, fmt.Sprintf("Cannot check which workbenches in %s use the dashboard connection %s: %v. Nothing was deleted.", project, conn, err), kube.ErrorCodeFromK8sErr(err)
	}
	var unconfirmed []string
	for _, w := range wbs {
		if uid, ok := confirmed[w.Name]; !ok || uid == "" || uid != w.UID {
			unconfirmed = append(unconfirmed, w.Name)
		}
	}
	if len(unconfirmed) > 0 {
		return nil, unconfirmedRefusal(project, conn, unconfirmed, ""), ErrorCodeConfirmWorkbenches
	}
	return &connectionUsers{conn: conn, wbs: wbs, envOnly: envOnly}, "", ""
}

// ConnectionWorkbenchesProblem is the check of connectionWorkbenchesToDetach
// on its own (Clean up runs it for every pipeline server before it deletes
// anything, and again to list them when a teardown refused): a refusal
// message and code, and the workbenches that use the Connection now. The
// Connection is the one of the project's pipeline server
// (ToolConnectionOf).
func ConnectionWorkbenchesProblem(c *kube.Client, project string, confirmed map[string]string) (string, string, []ConnectionWorkbench) {
	conn, err := ToolConnectionOf(c, project)
	if err != nil {
		return fmt.Sprintf("Cannot check the pipeline server in %s, so which dashboard connection its workbenches use is unknown: %v. Nothing was deleted.", project, err), kube.ErrorCodeFromK8sErr(err), nil
	}
	users, problem, code := connectionWorkbenchesToDetach(c, project, conn, ConnectionSecretPath(project, conn), confirmed)
	if code == ErrorCodeConfirmWorkbenches {
		all, _, err := listConnectionWorkbenches(c, project, conn)
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
	// Every change as a server-side dry run first (the Notebook webhooks
	// declare sideEffects None, so the API server sends them dry runs):
	// a workbench the webhook refuses, or that changed, stops the teardown
	// before any is changed. The declaration is not a guarantee:
	// odh-notebook-controller's webhook can still create its ConfigMaps
	// and Secret ds-pipeline-config in the project on a dry run.
	conn := users.conn
	for _, w := range users.wbs {
		if err := patchOutConnection(c, project, conn, w, true); err != nil {
			*logs = append(*logs, fmt.Sprintf("Warning: workbench %s cannot be changed (dry run): %v", w.Name, err))
			return detachFailure(project, conn, w, err, "Nothing was changed or deleted.")
		}
	}
	for _, w := range users.wbs {
		*logs = append(*logs, fmt.Sprintf("Removing connection %s from workbench %s", conn, w.Name))
		if err := patchOutConnection(c, project, conn, w, false); err != nil {
			*logs = append(*logs, fmt.Sprintf("Warning: workbench %s was not changed: %v", w.Name, err))
			return detachFailure(project, conn, w, err, "Nothing was deleted; the workbenches before it no longer use the connection. Run the deletion again to retry.")
		}
		*logs = append(*logs, fmt.Sprintf("OK: Workbench %s no longer uses connection %s", w.Name, conn))
	}
	// A workbench may have attached the Connection (again) since the list
	// was read: read it once more right before the delete, and refuse while
	// any workbench names it, so none is left naming a deleted Secret.
	left, _, err := listConnectionWorkbenches(c, project, conn)
	if err != nil {
		return fmt.Sprintf("Cannot check again which workbenches in %s use the dashboard connection %s: %v. Nothing was deleted.", project, conn, err), kube.ErrorCodeFromK8sErr(err)
	}
	if len(left) > 0 {
		*logs = append(*logs, fmt.Sprintf("Workbenches use connection %s again: %s", conn, strings.Join(ConnectionWorkbenchNames(left), ", ")))
		return unconfirmedRefusal(project, conn, ConnectionWorkbenchNames(left), " (since the deletion started)"), ErrorCodeConfirmWorkbenches
	}
	return "", ""
}

// patchOutConnection takes the tool's Connection off workbench w as it was
// read: a JSON patch that tests its UID, resourceVersion and annotation
// value, then rewrites only that entry (the annotation goes when nothing is
// left); with dryRun, as a server-side dry run.
func patchOutConnection(c *kube.Client, project, conn string, w ConnectionWorkbench, dryRun bool) error {
	pointer := "/metadata/annotations/" + strings.ReplaceAll(NotebookConnectionsAnnotation, "/", "~1")
	rest, _ := withoutConnection(w.Annotation, project, conn)
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
		return err
	}
	q := url.Values{"fieldManager": {envkit.ToolFieldManager}}
	if dryRun {
		q.Set("dryRun", "All")
	}
	_, _, err = c.Do(http.MethodPatch, rhoai.NamespacedPath("kubeflow.org/v1", "notebooks", project, w.Name), "application/json-patch+json", body, q)
	return err
}

// webhookDenial is the reason an admission webhook gave for refusing a
// request (a 403 whose message starts with `admission webhook "`), or "".
func webhookDenial(err error) string {
	var k *kube.K8sError
	if !errors.As(err, &k) || k.Status != http.StatusForbidden || !strings.HasPrefix(k.K8sMessage, `admission webhook "`) {
		return ""
	}
	if _, reason, ok := strings.Cut(k.K8sMessage, "denied the request: "); ok && reason != "" {
		return reason
	}
	return k.K8sMessage
}

// detachFailure is the message and error code of a workbench change that
// failed; after says what was (not) done.
func detachFailure(project, conn string, w ConnectionWorkbench, err error, after string) (string, string) {
	if reason := webhookDenial(err); reason != "" {
		return fmt.Sprintf("The workbenches webhook refused to take the dashboard connection %s off workbench %s in %s: %s. Fix that workbench's other connections in the dashboard first (for example one whose Secret was deleted), then delete again. %s",
			conn, w.Name, project, strings.TrimSuffix(reason, "."), after), ErrorCodeWebhookRefused
	}
	why, code := err.Error(), kube.ErrorCodeFromK8sErr(err)
	if kube.IsK8sConflict(err) || kube.IsK8sError(err, http.StatusUnprocessableEntity) || kube.IsK8sNotFound(err) {
		why, code = "it changed since it was read", "conflict"
	}
	return fmt.Sprintf("Cannot take the dashboard connection %s off workbench %s in %s (%s). %s", conn, w.Name, project, why, after), code
}

// ToolConnectionOf is the Connection of the tool's pipeline server in
// project (ConnectionNameOf), read with c; the shared storage's
// ConnectionSecretName when the project has none of the tool's.
func ToolConnectionOf(c *kube.Client, project string) (string, error) {
	dspas, err := ListProjectDSPAs(c, project)
	if errors.Is(err, ErrDSPACRDMissing) {
		return ConnectionSecretName, nil
	}
	if err != nil {
		return "", err
	}
	for _, d := range dspas {
		if d.ManagedByTool() {
			return ConnectionNameOf(d), nil
		}
	}
	return ConnectionSecretName, nil
}
