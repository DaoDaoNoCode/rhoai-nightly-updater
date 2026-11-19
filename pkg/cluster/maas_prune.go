package cluster

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

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
	if live, err := maasLivePubs(c, rec, maasPub{}, maasPub{}); err != nil {
		failed = append(failed, fmt.Errorf("check the other publications: %w", err))
	} else {
		sub, auth := maasAccessObjects(c, live, maasOwnSubscriptionPriority(c))
		for _, w := range []struct {
			kind string
			obj  map[string]interface{}
		}{{"MaaSAuthPolicy", auth}, {"MaaSSubscription", sub}} {
			path := maasAccessPath(c, w.kind)
			meta, found, err := readObjectMeta(c, path)
			if err != nil {
				failed = append(failed, fmt.Errorf("read %s: %w", w.kind, err))
				continue
			}
			if !found || !toolOwned(meta) {
				continue
			}
			if len(live) == 0 {
				if _, err := deleteWithUID(c, path, meta.UID); err != nil && !IsK8sError(err, http.StatusNotFound) {
					failed = append(failed, fmt.Errorf("delete %s: %w", w.kind, err))
					continue
				}
				changed = true
				logs = append(logs, fmt.Sprintf("OK: %s deleted (no publication is left)", w.kind))
				continue
			}
			if err := applyMaaSObject(c, w.kind, path, maasAccessCreatePath(c, w.kind), w.obj, false); err != nil {
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
		gwPlan.classMissing = false // only the gateway changes
		if msg, _ := m.applyGateway(gwPlan, false); msg != "" {
			failed = append(failed, errors.New(strings.TrimSuffix(msg, ".")))
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
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		User:      getUser(c),
		Action:    "prune-maas-publications",
		Detail:    truncateMessage(detail),
		Success:   err == nil,
	})
}
