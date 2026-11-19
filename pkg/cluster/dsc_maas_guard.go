package cluster

import (
	"fmt"
	"sort"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The MaaS in-use guard: a DataScienceCluster change that takes MaaS away
// (aigateway or aigateway.modelsAsAService stops being deployed: set to
// Removed, through the parent, by a reset or a repair) is refused while
// MaaS is in use. Without MaaS the operator removes maas-api and
// maas-controller, and the gateway policies maas-controller made go with
// them: every published model and every API key stops working, and a
// MaaSModelRef left behind keeps maas-controller's finalizer. The MaaS tab
// removes MaaS in order (Clean up MaaS: unpublish, then MaaS in the
// DataScienceCluster, then the gateway and the database), so that is the
// way to remove it. Reads that fail block (fail closed).
//
// Other parts of aigateway (aiGrid, batchGateway) have no in-use signal
// the tool can read, so they get only the module checks.

// dscMaaSPaths are the toggles whose removal takes MaaS away, parent
// first: the AI gateway, whose module deploys MaaS (rhods-operator 3.6
// modules/aigateway), and MaaS itself. The MaaS tab reads the same fields
// (readMaaSDSC).
var dscMaaSPaths = []string{"aigateway", "aigateway.modelsAsAService"}

func takesMaaSAway(path string) bool { return containsString(dscMaaSPaths, path) }

// maasUse is what uses MaaS on the cluster.
type maasUse struct {
	// models are the published models ("<project>/<model>"): every
	// MaaSModelRef, and every publication this tool recorded.
	models        []string
	subscriptions []string // MaaSSubscriptions, "<namespace>/<name>"
	policies      []string // MaaSAuthPolicies, "<namespace>/<name>"
	// setUp: this tool's MaaS record says it set MaaS up (the MaaS tab).
	setUp bool
}

func (u maasUse) inUse() bool {
	return u.setUp || len(u.models) > 0 || len(u.subscriptions) > 0 || len(u.policies) > 0
}

// text names what uses MaaS.
func (u maasUse) text() string {
	var parts []string
	if len(u.models) > 0 {
		parts = append(parts, verb(len(u.models), "published model ", "published models ")+strings.Join(u.models, ", "))
	}
	if len(u.subscriptions) > 0 {
		parts = append(parts, verb(len(u.subscriptions), "subscription ", "subscriptions ")+strings.Join(u.subscriptions, ", "))
	}
	if len(u.policies) > 0 {
		parts = append(parts, verb(len(u.policies), "access policy ", "access policies ")+strings.Join(u.policies, ", "))
	}
	if u.setUp {
		parts = append(parts, "this tool set MaaS up (MaaS tab)")
	}
	return strings.Join(parts, "; ")
}

// setUpByTool: the record says this tool set MaaS up, or published a model,
// and has not removed it (Clean up MaaS clears each of these as it goes).
func (r maasRecord) setUpByTool() bool {
	return r.DSCChanged || r.AIGatewayChanged || len(r.Published) > 0 || r.DBNamespace != "" || r.Database != "" || r.InfraNamespace != "" || r.POCCredsUID != ""
}

// readMaaSUse reads what uses MaaS with the MaaS tab's readers: this tool's
// record (snapshot ConfigMap, key maas) and, when the MaaS API is served,
// the MaaSModelRefs, MaaSSubscriptions and MaaSAuthPolicies at the version
// discovery reports (listMaaSObjects). Any read that fails is returned,
// a 404 on a list of a served group included.
func readMaaSUse(c *Client) (maasUse, error) {
	var u maasUse
	rec, err := readMaaSRecord(c)
	if err != nil {
		return u, err
	}
	u.setUp = rec.setUpByTool()
	models := map[string]bool{}
	for _, p := range rec.pubs() {
		models[p.key()] = true
	}
	objs, err := listMaaSObjects(c)
	if err != nil {
		return u, err
	}
	for _, o := range objs {
		name := o.meta.Namespace + "/" + o.meta.Name
		if o.meta.terminating() {
			name += " (being deleted)"
		}
		switch o.kind {
		case "MaaSModelRef":
			delete(models, o.meta.Namespace+"/"+o.meta.Name)
			u.models = append(u.models, name)
		case "MaaSSubscription":
			u.subscriptions = append(u.subscriptions, name)
		case "MaaSAuthPolicy":
			u.policies = append(u.policies, name)
		}
	}
	for k := range models {
		u.models = append(u.models, k)
	}
	sort.Strings(u.models)
	sort.Strings(u.subscriptions)
	sort.Strings(u.policies)
	return u, nil
}

// maasUse reads what uses MaaS once per checker.
func (rc *removalChecker) maasUse() (maasUse, error) {
	if !rc.maasLoaded {
		rc.maasLoaded = true
		rc.maas, rc.maasErr = readMaaSUse(rc.c)
	}
	return rc.maas, rc.maasErr
}

// maasBlocker says why path must not stop being deployed now: it takes
// MaaS away while MaaS is in use, or the tool cannot tell whether it is.
// "" for other paths and when MaaS is not in use.
func (rc *removalChecker) maasBlocker(path string) string {
	if !takesMaaSAway(path) {
		return ""
	}
	use, err := rc.maasUse()
	if err != nil {
		return fmt.Sprintf("cannot tell whether MaaS is in use (%v), and removing %s takes MaaS away; check the MaaS tab, remove MaaS there if nobody uses it (Clean up MaaS unpublishes and removes it in order), or keep %s Managed", err, path, path)
	}
	if !use.inUse() {
		return ""
	}
	return fmt.Sprintf("MaaS is in use (%s). Removing %s takes MaaS away: maas-api and the gateway's policies go, so every published model and every API key stops working. Remove MaaS on the MaaS tab first (Clean up MaaS unpublishes and removes it in order), or keep %s Managed", use.text(), path, path)
}

// maasBlocks checks a change from before to after against MaaS: the first
// MaaS path that stops being deployed blocks while MaaS is in use. In a
// hard reset (teardown) a MaaS path that is deployed again afterwards is
// down only meanwhile: a warning, while a read that fails still blocks.
func (v *dscView) maasBlocks(rc *removalChecker, before, after map[string]interface{}, teardown bool) (*types.DSCRemovalBlock, string) {
	gone, down := "", ""
	for _, p := range dscMaaSPaths {
		t, ok := v.byKey[p]
		if !ok || !t.Toggle || !v.deployed(before, t.Path) {
			continue
		}
		if !v.deployed(after, t.Path) {
			gone = p
			break
		}
		if teardown && down == "" {
			down = p
		}
	}
	if gone != "" {
		if why := rc.maasBlocker(gone); why != "" {
			return &types.DSCRemovalBlock{Component: gone, Reasons: []string{why}}, ""
		}
		return nil, ""
	}
	if down == "" {
		return nil, ""
	}
	use, err := rc.maasUse()
	switch {
	case err != nil:
		return &types.DSCRemovalBlock{Component: down, Reasons: []string{rc.maasBlocker(down)}}, ""
	case use.inUse():
		return nil, fmt.Sprintf("While %s is down, MaaS does not serve (%s): every published model and every API key stops working until the new DataScienceCluster brings it back.", down, use.text())
	}
	return nil, ""
}

// addBlock adds b to blocks, with the reasons of a block of the same
// component.
func addBlock(blocks []types.DSCRemovalBlock, b types.DSCRemovalBlock) []types.DSCRemovalBlock {
	for i := range blocks {
		if blocks[i].Component == b.Component {
			blocks[i].Reasons = uniqueInOrder(append(blocks[i].Reasons, b.Reasons...))
			return blocks
		}
	}
	return append(blocks, b)
}
