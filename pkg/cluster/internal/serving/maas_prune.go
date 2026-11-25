package serving

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
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
// (PruneGoneMaaSPublications) and the gateway's list is written without
// their projects: at the start of every MaaS, Models and Clean up
// operation, which hold the cluster lock, and in the background, at start
// and every few minutes under the same lock and cross-pod lease
// (pkg/api maas_prune_job.go). The MaaS status names them until then
// (goneMaaSPublications).
//
// A publication is gone when its project no longer exists (or is being
// deleted), or when the model it published is: absent, or another object
// under its name than the one whose UID the tool recorded (deleted and
// made again, as in a recreated project), or when nothing of it is left
// while project and model are there: no MaaSModelRef of this tool's, and
// the model neither on the MaaS gateway nor routed through it (its
// reference deleted outside the tool, or a publish that stopped before
// it). Not gone either while the model of the tool still lacks the genai
// label or annotation publish took off (an Unpublish that failed before it
// put them back): its row offers Unpublish to finish. With no MaaSModelRef
// of this tool's while the tool's model (the recorded UID) still routes
// through the MaaS gateway, the reference is gone and the routing pending:
// the prune takes the model out of MaaS (subscription, access policy,
// gateway) like any gone publication, but the record keeps the values
// "Restore direct routing" writes back (its row, from the live model,
// offers it), until that restore is applied or the model is gone
// (settleRestoreValues). Anything that cannot be read keeps the
// publication (fail closed: it stays as it is, and the next run looks
// again).

// maasGonePub is a publication that is gone, and why. projectGone: its
// project no longer exists or is being deleted (nothing of the tool's is
// left to delete there). unpublished: the model is there and nothing of
// the publication is (its row is the model's live one, Not published).
// routingPending: the reference is gone and the model still routes through
// the MaaS gateway (the record keeps its restore values).
type maasGonePub struct {
	pub            MaaSPub
	why            string
	projectGone    bool
	unpublished    bool
	routingPending bool
}

// goneMaaSPublications reads, for each recorded publication, whether it is
// gone. A read that fails keeps it. The error is the models record that
// cannot be read: then a model made again under its name cannot be told
// from the published one, and only gone projects and absent models count.
func goneMaaSPublications(c *kube.Client, rec MaaSRecord) ([]maasGonePub, error) {
	pubs := rec.Pubs()
	if len(pubs) == 0 {
		return nil, nil
	}
	models, modelsErr := ReadModelsRecord(c)
	var out []maasGonePub
	for _, p := range pubs {
		meta, found, err := kube.ReadObjectMeta(c, "/api/v1/namespaces/"+p.Project)
		switch {
		case err != nil:
			continue
		case !found:
			out = append(out, maasGonePub{pub: p, why: fmt.Sprintf("project %s no longer exists", p.Project), projectGone: true})
			continue
		case meta.Terminating():
			out = append(out, maasGonePub{pub: p, why: fmt.Sprintf("project %s is being deleted", p.Project), projectGone: true})
			continue
		}
		model, found, err := ReadToolModel(c, p.Project, p.Model)
		switch {
		case err != nil:
			continue
		case !found:
			out = append(out, maasGonePub{pub: p, why: fmt.Sprintf("model %s in %s no longer exists", p.Model, p.Project)})
		case modelsErr == nil && models.notTheToolsModel(p, model.Metadata.UID):
			out = append(out, maasGonePub{pub: p, why: fmt.Sprintf("model %s in %s is not the one this tool published (it was deleted and made again)", p.Model, p.Project)})
		case rec.staleFor(p.Project, p.Model, model.Metadata.UID):
			// This tool made it again: its own, but not the one published.
			out = append(out, maasGonePub{pub: p, why: fmt.Sprintf("model %s in %s was deleted and made again since it was published", p.Model, p.Project)})
		default:
			// Restore values are kept for the tool's model only (fail
			// closed while the models record cannot tell).
			toolsModel := modelsErr != nil || models.Owns(model.Metadata)
			left, err := maasPublicationLeft(c, model)
			switch {
			case err != nil || left == maasRefLive || left == maasRouteLeft:
			case left == maasRouted && toolsModel:
				out = append(out, maasGonePub{pub: p, why: fmt.Sprintf("the MaaSModelRef of model %s in %s no longer exists (reference gone, routing pending)", p.Model, p.Project), routingPending: true})
			case rec.genAIPending(p, model.Metadata) && toolsModel:
				// An Unpublish that routed the model back but stopped
				// before it put back the genai label or annotation is not
				// finished: the record keeps what Unpublish again puts
				// back on this model.
			default:
				out = append(out, maasGonePub{pub: p, why: fmt.Sprintf("the MaaSModelRef of model %s in %s no longer exists", p.Model, p.Project), unpublished: true})
			}
		}
	}
	return out, modelsErr
}

// maasLeft is what is on the cluster of a publication (maasPublicationLeft).
type maasLeft int

const (
	maasNothingLeft maasLeft = iota
	// maasRefLive: this tool's MaaSModelRef.
	maasRefLive
	// maasRouted: no such reference; the model's MaaS gateway ref.
	maasRouted
	// maasRouteLeft: neither; the model's route through the MaaS gateway
	// (KServe has not moved it yet).
	maasRouteLeft
)

// maasPublicationLeft says what of the publication of model is on the
// cluster, the first found of: this tool's MaaSModelRef, the model's MaaS
// gateway ref, its route through the MaaS gateway.
func maasPublicationLeft(c *kube.Client, model LLMISvcObject) (maasLeft, error) {
	ref, found, err := readMaaSModelRef(c, model.Metadata.Namespace, model.Metadata.Name)
	if err != nil {
		return maasNothingLeft, err
	}
	if found && ToolOwned(ref.Metadata) {
		return maasRefLive, nil
	}
	if ModelHasMaaSRef(model) {
		return maasRouted, nil
	}
	parents, _, err := readModelRoute(c, model.Metadata.Namespace, model.Metadata.Name)
	if err != nil {
		return maasNothingLeft, err
	}
	if slices.Contains(parents, inferenceGatewayNamespace+"/"+MaaSGatewayName) {
		return maasRouteLeft, nil
	}
	return maasNothingLeft, nil
}

// settleRestoreValues drops the restore values the record keeps without a
// publication (MaaSRecord.restorePending) once nothing is left to
// restore: the model is gone, another one (its UID), not the tool's, or it
// no longer routes through the MaaS gateway and has its Gen AI metadata
// back (restored outside the tool). What cannot be read is kept. It
// returns the keys it dropped.
func settleRestoreValues(c *kube.Client, rec *MaaSRecord) []string {
	pending := rec.restorePending()
	if len(pending) == 0 {
		return nil
	}
	models, modelsErr := ReadModelsRecord(c)
	var settled []string
	for _, p := range pending {
		model, found, err := ReadToolModel(c, p.Project, p.Model)
		if err != nil {
			continue
		}
		if found && !rec.staleFor(p.Project, p.Model, model.Metadata.UID) && (modelsErr != nil || models.Owns(model.Metadata)) &&
			(ModelHasMaaSRef(model) || rec.genAIPending(p, model.Metadata)) {
			continue
		}
		rec.SetPublished(p.Project, p.Model, false)
		settled = append(settled, p.Key())
	}
	return settled
}

// notTheToolsModel: the tool recorded another UID for p's model than uid.
func (r ModelsRecord) notTheToolsModel(p MaaSPub, uid string) bool {
	e, ok := r[ModelRecordKey(p.Project, p.Model)]
	return ok && e.UID != "" && e.UID != uid
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

// PruneGoneMaaSPublications drops the publications that are gone, in
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
func PruneGoneMaaSPublications(c *kube.Client) (changed bool, logs []string, err error) {
	rec, err := ReadMaaSRecord(c)
	if err != nil {
		return false, nil, err
	}
	settled := settleRestoreValues(c, &rec)
	var settledLog []string
	if len(settled) > 0 {
		settledLog = []string{"Nothing is left to restore for " + strings.Join(settled, ", ") + "; the record no longer keeps its values."}
	}
	gone, _ := goneMaaSPublications(c, rec)
	if len(gone) == 0 {
		setLastPruneFailure(nil)
		if len(settled) == 0 {
			return false, nil, nil
		}
		if err := WriteMaaSRecord(c, rec); err != nil {
			return false, settledLog, err
		}
		return true, settledLog, nil
	}
	defer func() { setLastPruneFailure(err) }()
	for _, g := range gone {
		if g.routingPending {
			rec.dropPublishedKeepRestore(g.pub.Project, g.pub.Model)
			continue
		}
		rec.SetPublished(g.pub.Project, g.pub.Model, false)
	}
	logs = append(settledLog, "Publications that are gone, dropped from MaaS: "+goneMaaSText(gone)+".")
	var failed []error

	// The tool's references in projects that still exist.
	for _, g := range gone {
		if g.projectGone {
			continue
		}
		ref, found, err := readMaaSModelRef(c, g.pub.Project, g.pub.Model)
		if err != nil {
			failed = append(failed, fmt.Errorf("read MaaSModelRef %s: %w", g.pub.Key(), err))
			continue
		}
		if !found || !ToolOwned(ref.Metadata) || ref.Metadata.Terminating() {
			continue
		}
		path, _ := maasModelRefPath(c, g.pub.Project, g.pub.Model)
		if _, err := kube.DeleteWithUID(c, path, ref.Metadata.UID); err != nil && !kube.IsK8sNotFound(err) {
			failed = append(failed, fmt.Errorf("delete MaaSModelRef %s: %w", g.pub.Key(), err))
			continue
		}
		changed = true
		logs = append(logs, "OK: MaaSModelRef "+g.pub.Key()+" deleted")
	}

	// The access policy and the subscription.
	live, liveErr := maasLivePubs(c, rec, MaaSPub{}, MaaSPub{})
	err = liveErr
	var cur maasToolSub
	if err == nil {
		// The remaining publications keep their limits and who can use
		// them, as publish rebuilds the subscription (maas_ops.go).
		cur, err = readMaaSToolSubscription(c)
	}
	if err != nil {
		failed = append(failed, fmt.Errorf("check the other publications: %w", err))
	} else {
		aud := MaaSAudience{Everyone: true}
		if cur.found {
			aud = cur.audience
		}
		limits := map[string]MaaSLimit{}
		for _, p := range live {
			limits[p.Key()] = cur.limitOf(p)
		}
		sub, auth := maasAccessObjects(c, live, limits, aud)
		for _, w := range []struct {
			kind string
			obj  map[string]any
		}{{"MaaSAuthPolicy", auth}, {"MaaSSubscription", sub}} {
			accessPath := maasAccessPath(c, w.kind)
			meta, found, err := kube.ReadObjectMeta(c, accessPath)
			if err != nil {
				failed = append(failed, fmt.Errorf("read %s: %w", w.kind, err))
				continue
			}
			if !found || !ToolOwned(meta) {
				continue
			}
			if len(live) == 0 {
				if _, err := kube.DeleteWithUID(c, accessPath, meta.UID); err != nil && !kube.IsK8sNotFound(err) {
					failed = append(failed, fmt.Errorf("delete %s: %w", w.kind, err))
					continue
				}
				changed = true
				logs = append(logs, fmt.Sprintf("OK: %s deleted (no publication is left)", w.kind))
				continue
			}
			read := func() (kube.ObjectMeta, bool, error) { return kube.ReadObjectMeta(c, accessPath) }
			if err := updateToolObject(c, w.kind, accessPath, w.obj, meta, read, ToolOwned); err != nil {
				failed = append(failed, fmt.Errorf("update %s: %w", w.kind, err))
				continue
			}
			changed = true
			logs = append(logs, fmt.Sprintf("OK: %s no longer lists them", w.kind))
		}
	}

	// The gateway.
	m := &Run{Client: c}
	var gwPlan *maasGatewayPlan
	var msg, code string
	if infra, err := MaaSInfraNamespace(c); err != nil {
		msg = err.Error()
	} else {
		// The projects of the publications left on the cluster; while
		// they cannot be read, those the record still lists.
		projects := rec.Projects()
		if liveErr == nil {
			projects = pubProjects(live)
		}
		gwPlan, msg, code = planMaaSGateway(c, infra, projects)
	}
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
	logs = append(logs, m.Logs...)

	if len(failed) > 0 {
		return changed, logs, errors.Join(failed...)
	}
	if err := WriteMaaSRecord(c, rec); err != nil {
		return changed, logs, err
	}
	return true, logs, nil
}

// narrowMaaSGateway applies the gateway plan to the tool's existing MaaS
// gateway and its options ConfigMap (when there is one), so the gateway
// admits routes only from the projects still published. It never creates
// either: pruning only takes away.
func narrowMaaSGateway(c *kube.Client, plan *maasGatewayPlan) error {
	for _, kind := range []string{"ConfigMap", "Gateway"} {
		if !plan.found[kind] {
			continue
		}
		path := maasGatewayObjectPath(kind)
		owned := gatewayOwned
		if kind == "ConfigMap" {
			owned = ToolOwned
		}
		read := func() (kube.ObjectMeta, bool, error) { return kube.ReadObjectMeta(c, path) }
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
		return rhoai.NamespacedPath("gateway.networking.k8s.io/v1", "gateways", inferenceGatewayNamespace, MaaSGatewayName)
	}
	return rhoai.NamespacedPath("v1", "configmaps", inferenceGatewayNamespace, maasGatewayOptions)
}

// updateToolObject is the update half of envkit.EnsureToolObject, for an object
// that must exist: the same ownership check, field-ownership upgrade and
// resourceVersion guard, and a server-side apply sent as a plain PATCH. It
// never creates the object: on one that is gone the API server would check
// create too, which the ServiceAccount does not have. The MaaS prune job
// runs with the ServiceAccount, so the ServiceAccount needs only patch on
// the tool's named MaaS objects (deploy/template.yaml).
func updateToolObject(c *kube.Client, desc, path string, obj map[string]any, current kube.ObjectMeta, read envkit.ToolObjectReader, owned func(kube.ObjectMeta) bool) error {
	for range 3 {
		if !owned(current) {
			return &envkit.ForeignObjectError{Desc: desc}
		}
		if current.Terminating() {
			return fmt.Errorf("%s is being deleted", desc)
		}
		upgraded, err := envkit.UpgradeToolFieldOwnership(c, path, current)
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
			_, _, err = c.Do(http.MethodPatch, path, "application/apply-patch+yaml", data, url.Values{"fieldManager": {envkit.ToolFieldManager}, "force": {"true"}})
			if err == nil {
				return nil
			}
		}
		if !kube.IsK8sConflict(err) {
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
	return fmt.Errorf("%s: %w", desc, envkit.ErrObjectChanged)
}

// lastPrune is the error of the last PruneGoneMaaSPublications that found
// gone publications ("" when it succeeded), for the status: a removal that
// is on its way needs no attention, one that failed does. It is this
// process's (the updater runs one replica, deploy/template.yaml); after a
// restart the background job runs at once.
var lastPrune struct {
	sync.Mutex
	failure string
}

func setLastPruneFailure(err error) {
	lastPrune.Lock()
	defer lastPrune.Unlock()
	lastPrune.failure = ""
	if err != nil {
		lastPrune.failure = err.Error()
	}
}

func lastPruneFailure() string {
	lastPrune.Lock()
	defer lastPrune.Unlock()
	return lastPrune.failure
}

// ReleaseGoneMaaSProjects runs PruneGoneMaaSPublications for an operation
// and returns its log lines; a failure is a warning in them (the operation
// itself goes on, and the next run tries again).
func ReleaseGoneMaaSProjects(c *kube.Client) []string {
	_, logs, err := PruneGoneMaaSPublications(c)
	if err != nil {
		logs = append(logs, fmt.Sprintf("Warning: the publications that are gone could not all be dropped from MaaS (%v). The MaaS tab names them; the next MaaS, Models or Clean up operation, or the background check, tries again.", err))
	}
	return logs
}

// recordPruneActivity records the background run's entry; tests replace it.
var recordPruneActivity = record.Activity

// PruneGoneMaaSPublicationsInBackground is the background run (pkg/api
// holds the cluster lock and lease around it): it records an activity
// entry only when it changed something.
func PruneGoneMaaSPublicationsInBackground(c *kube.Client) {
	changed, logs, err := PruneGoneMaaSPublications(c)
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
		Timestamp: kube.NowRFC3339(),
		User:      kube.GetUser(c),
		Action:    "prune-maas-publications",
		Detail:    kube.TruncateText(detail, 300),
		Success:   err == nil,
	})
}
