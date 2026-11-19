package cluster

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Publications whose project is gone.
//
// maas-default-gateway admits routes from the projects of this tool's
// recorded publications (maasGatewayNamespaces). A project deleted while
// its model was published stayed on that list, and anyone who may create
// projects could create one of that name and attach routes to the MaaS
// gateway (another published model's exact path, or the MaaS API's, and
// receive what is sent there). So the gone publications are dropped
// (pruneGoneMaaSPublications) and the gateway's list is written without
// their projects: at the start of every MaaS, Models and Clean up
// operation, which hold the cluster lock, and in the background, at start
// and every few minutes under the same lock and cross-pod lease
// (pkg/api maas_prune_job.go). The MaaS status names them until then
// (goneMaaSPublications).
//
// A publication is gone when its project no longer exists (or is being
// deleted), or when the model it published is: absent, or another object
// under its name than the one whose UID the tool recorded (deleted and
// made again, as in a recreated project). Anything that cannot be read
// keeps the publication (fail closed: it stays as it is, and the next
// run looks again).

// maasGonePub is a publication that is gone, and why. projectGone: its
// project no longer exists or is being deleted (nothing of the tool's is
// left to delete there).
type maasGonePub struct {
	pub         maasPub
	why         string
	projectGone bool
}

// goneMaaSPublications reads, for each recorded publication, whether it is
// gone. A read that fails keeps it. The error is the models record that
// cannot be read: then a model made again under its name cannot be told
// from the published one, and only gone projects and absent models count.
func goneMaaSPublications(c *Client, rec maasRecord) ([]maasGonePub, error) {
	pubs := rec.pubs()
	if len(pubs) == 0 {
		return nil, nil
	}
	models, modelsErr := readModelsRecord(c)
	var out []maasGonePub
	for _, p := range pubs {
		meta, found, err := readObjectMeta(c, "/api/v1/namespaces/"+p.project)
		switch {
		case err != nil:
			continue
		case !found:
			out = append(out, maasGonePub{p, fmt.Sprintf("project %s no longer exists", p.project), true})
			continue
		case meta.terminating():
			out = append(out, maasGonePub{p, fmt.Sprintf("project %s is being deleted", p.project), true})
			continue
		}
		model, found, err := readToolModel(c, p.project, p.model)
		switch {
		case err != nil:
			continue
		case !found:
			out = append(out, maasGonePub{p, fmt.Sprintf("model %s in %s no longer exists", p.model, p.project), false})
		case modelsErr == nil:
			if e, ok := models[modelRecordKey(p.project, p.model)]; ok && e.UID != "" && e.UID != model.Metadata.UID {
				out = append(out, maasGonePub{p, fmt.Sprintf("model %s in %s is not the one this tool published (it was deleted and made again)", p.model, p.project), false})
			}
		}
	}
	return out, modelsErr
}

// goneMaaSText names the gone publications for people.
func goneMaaSText(gone []maasGonePub) string {
	var whys []string
	for _, g := range gone {
		whys = append(whys, g.why)
	}
	sort.Strings(whys)
	return strings.Join(whys, "; ")
}

// pruneGoneMaaSPublications drops the publications that are gone, in
// independent steps, so one that fails does not keep the others from
// narrowing what is reachable:
//   - the tool's MaaSModelRef of a gone publication in a project that still
//     exists (a model made again), deleted by UID;
//   - the access policy and subscription, which no longer list them
//     (deleted when none is left);
//   - the gateway, which no longer admits their projects (unless another
//     publication is there).
//
// The record forgets them only when every step succeeded, so the next run
// tries again what failed. The caller holds the cluster lock. changed says
// whether anything was written; logs say what; an error says what is left.
func pruneGoneMaaSPublications(c *Client) (changed bool, logs []string, err error) {
	rec, err := readMaaSRecord(c)
	if err != nil {
		return false, nil, err
	}
	gone, _ := goneMaaSPublications(c, rec)
	if len(gone) == 0 {
		return false, nil, nil
	}
	for _, g := range gone {
		rec.setPublished(g.pub.project, g.pub.model, false)
	}
	logs = []string{"Publications that are gone, dropped from MaaS: " + goneMaaSText(gone) + "."}
	var failed []error

	// The tool's references in projects that still exist.
	for _, g := range gone {
		if g.projectGone {
			continue
		}
		ref, found, err := readMaaSModelRef(c, g.pub.project, g.pub.model)
		if err != nil {
			failed = append(failed, fmt.Errorf("read MaaSModelRef %s: %w", g.pub.key(), err))
			continue
		}
		if !found || !toolOwned(ref.Metadata) || ref.Metadata.terminating() {
			continue
		}
		path, _ := maasModelRefPath(c, g.pub.project, g.pub.model)
		if _, err := deleteWithUID(c, path, ref.Metadata.UID); err != nil && !IsK8sError(err, http.StatusNotFound) {
			failed = append(failed, fmt.Errorf("delete MaaSModelRef %s: %w", g.pub.key(), err))
			continue
		}
		changed = true
		logs = append(logs, "OK: MaaSModelRef "+g.pub.key()+" deleted")
	}

	// The access policy and the subscription.
	live, err := maasLivePubs(c, rec, maasPub{}, maasPub{})
	var cur maasToolSub
	if err == nil {
		// The remaining publications keep their limits and who can use
		// them, as publish rebuilds the subscription (maas_ops.go).
		cur, err = readMaaSToolSubscription(c)
	}
	if err != nil {
		failed = append(failed, fmt.Errorf("check the other publications: %w", err))
	} else {
		aud := maasAudience{everyone: true}
		if cur.found {
			aud = cur.audience
		}
		limits := map[string]maasLimit{}
		for _, p := range live {
			limits[p.key()] = cur.limitOf(p)
		}
		sub, auth := maasAccessObjects(c, live, limits, aud, maasOwnSubscriptionPriority(c))
		for _, w := range []struct {
			kind string
			obj  map[string]any
		}{{"MaaSAuthPolicy", auth}, {"MaaSSubscription", sub}} {
			accessPath := maasAccessPath(c, w.kind)
			meta, found, err := readObjectMeta(c, accessPath)
			if err != nil {
				failed = append(failed, fmt.Errorf("read %s: %w", w.kind, err))
				continue
			}
			if !found || !toolOwned(meta) {
				continue
			}
			if len(live) == 0 {
				if _, err := deleteWithUID(c, accessPath, meta.UID); err != nil && !IsK8sError(err, http.StatusNotFound) {
					failed = append(failed, fmt.Errorf("delete %s: %w", w.kind, err))
					continue
				}
				changed = true
				logs = append(logs, fmt.Sprintf("OK: %s deleted (no publication is left)", w.kind))
				continue
			}
			read := func() (objectMeta, bool, error) { return readObjectMeta(c, accessPath) }
			if err := updateToolObject(c, w.kind, accessPath, w.obj, meta, read, toolOwned); err != nil {
				failed = append(failed, fmt.Errorf("update %s: %w", w.kind, err))
				continue
			}
			changed = true
			logs = append(logs, fmt.Sprintf("OK: %s no longer lists them", w.kind))
		}
	}

	// The gateway.
	m := &maasRun{c: c}
	gwPlan, msg, code := planMaaSGateway(c, maasInfraNamespace(c), rec.projects())
	switch {
	case msg != "":
		failed = append(failed, fmt.Errorf("the MaaS gateway cannot be updated: %s", strings.TrimSuffix(msg, ".")))
	case code == "foreign":
		logs = append(logs, "The MaaS gateway someone else made is not changed; its owner removes the projects that are gone from its allowedRoutes.")
	case gwPlan.found["Gateway"]:
		if err := narrowMaaSGateway(c, gwPlan); err != nil {
			failed = append(failed, err)
		} else {
			changed = true
			logs = append(logs, "OK: the MaaS gateway admits routes only from the projects still published")
		}
	}
	logs = append(logs, m.logs...)

	if len(failed) > 0 {
		return changed, logs, errors.Join(failed...)
	}
	if err := writeMaaSRecord(c, rec); err != nil {
		return changed, logs, err
	}
	return true, logs, nil
}

// narrowMaaSGateway applies the gateway plan to the tool's existing MaaS
// gateway and its options ConfigMap (when there is one), so the gateway
// admits routes only from the projects still published. It never creates
// either: pruning only takes away.
func narrowMaaSGateway(c *Client, plan *maasGatewayPlan) error {
	for _, kind := range []string{"ConfigMap", "Gateway"} {
		if !plan.found[kind] {
			continue
		}
		path := maasGatewayObjectPath(kind)
		owned := gatewayOwned
		if kind == "ConfigMap" {
			owned = toolOwned
		}
		read := func() (objectMeta, bool, error) { return readObjectMeta(c, path) }
		if err := updateToolObject(c, maasDesc(kind, ""), path, plan.objects[kind], plan.current[kind], read, owned); err != nil {
			return fmt.Errorf("update %s: %w", maasDesc(kind, ""), err)
		}
	}
	return nil
}

// maasGatewayObjectPath is the path of the MaaS gateway or its options
// ConfigMap (maasObjectPath, for these two kinds only).
func maasGatewayObjectPath(kind string) string {
	if kind == "Gateway" {
		return namespacedPath("gateway.networking.k8s.io/v1", "gateways", inferenceGatewayNamespace, maasGatewayName)
	}
	return namespacedPath("v1", "configmaps", inferenceGatewayNamespace, maasGatewayOptions)
}

// updateToolObject is the update half of ensureToolObject, for an object
// that must exist: the same ownership check, field-ownership upgrade and
// resourceVersion guard, and a server-side apply sent as a plain PATCH. It
// never creates the object: on one that is gone the API server would check
// create too, which the ServiceAccount does not have. The MaaS prune job
// runs with the ServiceAccount, so the ServiceAccount needs only patch on
// the tool's named MaaS objects (deploy/template.yaml).
func updateToolObject(c *Client, desc, path string, obj map[string]any, current objectMeta, read toolObjectReader, owned func(objectMeta) bool) error {
	for range 3 {
		if !owned(current) {
			return &foreignObjectError{Desc: desc}
		}
		if current.terminating() {
			return fmt.Errorf("%s is being deleted", desc)
		}
		upgraded, err := upgradeToolFieldOwnership(c, path, current)
		if err == nil {
			current = upgraded
			guarded := make(map[string]any, len(obj))
			maps.Copy(guarded, obj)
			meta := map[string]any{}
			if m, ok := obj["metadata"].(map[string]any); ok {
				maps.Copy(meta, m)
			}
			meta["resourceVersion"] = current.ResourceVersion
			guarded["metadata"] = meta
			data, mErr := json.Marshal(guarded)
			if mErr != nil {
				return mErr
			}
			_, _, err = c.do(http.MethodPatch, path, "application/apply-patch+yaml", data, url.Values{"fieldManager": {toolFieldManager}, "force": {"true"}})
			if err == nil {
				return nil
			}
		}
		if !IsK8sError(err, http.StatusConflict) {
			return err
		}
		var found bool
		if current, found, err = read(); err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("%s is gone", desc)
		}
	}
	return fmt.Errorf("%s: %w", desc, errObjectChanged)
}

// releaseGoneMaaSProjects runs pruneGoneMaaSPublications for an operation
// and returns its log lines; a failure is a warning in them (the operation
// itself goes on, and the next run tries again).
func releaseGoneMaaSProjects(c *Client) []string {
	_, logs, err := pruneGoneMaaSPublications(c)
	if err != nil {
		logs = append(logs, fmt.Sprintf("Warning: the publications that are gone could not all be dropped from MaaS (%v). The MaaS tab names them; the next MaaS, Models or Clean up operation, or the background check, tries again.", err))
	}
	return logs
}

// recordPruneActivity records the background run's entry; tests replace it.
var recordPruneActivity = RecordActivity

// PruneGoneMaaSPublicationsInBackground is the background run (pkg/api
// holds the cluster lock and lease around it): it records an activity
// entry only when it changed something.
func PruneGoneMaaSPublicationsInBackground(c *Client) {
	changed, logs, err := pruneGoneMaaSPublications(c)
	if err != nil {
		slog.Warn("background MaaS check: gone publications not all dropped", "error", err)
	}
	if !changed {
		return
	}
	detail := strings.Join(logs, " ")
	if err != nil {
		detail += " Not finished: " + err.Error()
	}
	recordPruneActivity(c, types.ActivityEntry{
		Timestamp: nowRFC3339(),
		User:      getUser(c),
		Action:    "prune-maas-publications",
		Detail:    truncateText(detail, 300),
		Success:   err == nil,
	})
}
