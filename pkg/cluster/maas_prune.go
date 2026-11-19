package cluster

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// Publications whose project is gone.
//
// maas-default-gateway admits routes from the projects of this tool's
// recorded publications (maasGatewayNamespaces). A project deleted while
// its model was published stayed on that list, and anyone who may create
// projects could create one of that name and attach routes to the MaaS
// gateway (another published model's exact path, or the MaaS API's, and
// receive what is sent there). So every MaaS, Models and Clean up
// operation, which hold the cluster lock, first drops the publications
// that are gone (pruneGoneMaaSPublications) and writes the gateway's list
// without their projects; the MaaS status names them until then
// (goneMaaSPublications).
//
// A publication is gone when its project no longer exists (or is being
// deleted), or when the model it published is: absent, or another object
// under its name than the one whose UID the tool recorded (deleted and
// made again, as in a recreated project). Anything that cannot be read
// keeps the publication (fail closed: it stays as it is, and the next
// operation looks again).

// maasGonePub is a publication that is gone, and why.
type maasGonePub struct {
	pub maasPub
	why string
}

// goneMaaSPublications reads, for each recorded publication, whether it is
// gone. A read that fails keeps it.
func goneMaaSPublications(c *Client, rec maasRecord) []maasGonePub {
	pubs := rec.pubs()
	if len(pubs) == 0 {
		return nil
	}
	models, modelsErr := readModelsRecord(c)
	var out []maasGonePub
	for _, p := range pubs {
		meta, found, err := readObjectMeta(c, "/api/v1/namespaces/"+p.project)
		switch {
		case err != nil:
			continue
		case !found:
			out = append(out, maasGonePub{p, fmt.Sprintf("project %s no longer exists", p.project)})
			continue
		case meta.terminating():
			out = append(out, maasGonePub{p, fmt.Sprintf("project %s is being deleted", p.project)})
			continue
		}
		model, found, err := readToolModel(c, p.project, p.model)
		switch {
		case err != nil:
			continue
		case !found:
			out = append(out, maasGonePub{p, fmt.Sprintf("model %s in %s no longer exists", p.model, p.project)})
		case modelsErr == nil:
			if e, ok := models[modelRecordKey(p.project, p.model)]; ok && e.UID != "" && e.UID != model.Metadata.UID {
				out = append(out, maasGonePub{p, fmt.Sprintf("model %s in %s is not the one this tool published (it was deleted and made again)", p.model, p.project)})
			}
		}
	}
	return out
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

// pruneGoneMaaSPublications drops the publications that are gone: the
// access policy and subscription no longer list them (deleted when none is
// left), the gateway no longer admits their projects (unless another
// publication is there), and then the record forgets them, so a step that
// fails leaves the record as it was and the next operation tries again.
// The caller holds the cluster lock. It returns what it did, for the
// operation's log; an error says what is left.
func pruneGoneMaaSPublications(c *Client) ([]string, error) {
	rec, err := readMaaSRecord(c)
	if err != nil {
		return nil, err
	}
	gone := goneMaaSPublications(c, rec)
	if len(gone) == 0 {
		return nil, nil
	}
	m := &maasRun{c: c}
	for _, g := range gone {
		rec.setPublished(g.pub.project, g.pub.model, false)
	}
	logs := []string{"Publications that are gone, dropped from MaaS: " + goneMaaSText(gone) + "."}
	live, err := maasLivePubs(c, rec, maasPub{}, maasPub{})
	if err != nil {
		return logs, fmt.Errorf("check the other publications: %w", err)
	}
	sub, auth := maasAccessObjects(c, live, maasOwnSubscriptionPriority(c))
	for _, w := range []struct {
		kind string
		obj  map[string]interface{}
	}{{"MaaSAuthPolicy", auth}, {"MaaSSubscription", sub}} {
		path := maasAccessPath(c, w.kind)
		meta, found, err := readObjectMeta(c, path)
		if err != nil {
			return logs, fmt.Errorf("read %s: %w", w.kind, err)
		}
		if !found || !toolOwned(meta) {
			continue
		}
		if len(live) == 0 {
			if _, err := deleteWithUID(c, path, meta.UID); err != nil && !IsK8sError(err, http.StatusNotFound) {
				return logs, fmt.Errorf("delete %s: %w", w.kind, err)
			}
			logs = append(logs, fmt.Sprintf("OK: %s deleted (no publication is left)", w.kind))
			continue
		}
		if err := applyMaaSObject(c, w.kind, path, maasAccessCreatePath(c, w.kind), w.obj, false); err != nil {
			return logs, fmt.Errorf("update %s: %w", w.kind, err)
		}
		logs = append(logs, fmt.Sprintf("OK: %s no longer lists them", w.kind))
	}
	gwPlan, msg, code := planMaaSGateway(c, maasInfraNamespace(c), rec.projects())
	switch {
	case msg != "":
		return logs, fmt.Errorf("the MaaS gateway cannot be updated: %s", strings.TrimSuffix(msg, "."))
	case code == "foreign":
		logs = append(logs, "The MaaS gateway someone else made is not changed; its owner removes the projects that are gone from its allowedRoutes.")
	case gwPlan.found["Gateway"]:
		gwPlan.classMissing = false // only the gateway changes
		if msg, _ := m.applyGateway(gwPlan, false); msg != "" {
			return logs, fmt.Errorf("%s", strings.TrimSuffix(msg, "."))
		}
		logs = append(logs, "OK: the MaaS gateway admits routes only from the projects still published")
	}
	if err := writeMaaSRecord(c, rec); err != nil {
		return logs, err
	}
	return append(logs, m.logs...), nil
}

// releaseGoneMaaSProjects runs pruneGoneMaaSPublications for an operation
// and returns its log lines; a failure is a warning in them (the operation
// itself goes on, and the next one tries again).
func releaseGoneMaaSProjects(c *Client) []string {
	logs, err := pruneGoneMaaSPublications(c)
	if err != nil {
		logs = append(logs, fmt.Sprintf("Warning: the publications that are gone could not all be dropped from MaaS (%v). The MaaS tab names them; the next MaaS, Models or Clean up operation tries again.", err))
	}
	return logs
}
