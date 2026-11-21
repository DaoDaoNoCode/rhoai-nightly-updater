package cluster

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
)

// Which Deployment Dashboard Dev pauses, found at run time.
//
// On releases with a dashboard module (RHOAI 3.5 and later) a module
// operator reconciles the Dashboard CR and its workloads; Dashboard Dev
// scales it to 0 and keeps its recovery annotations on it. Its name is not
// assumed: the module operator finder (module_operators.go) identifies it
// by its RBAC (the ServiceAccount a ClusterRoleBinding lets update
// dashboards and dashboards/finalizers) and its Platform owner, in the
// applications namespace the DSCInitialization names.
//
// The legacy flow (patch rhods-dashboard directly) is chosen only when the
// cluster definitely has no dashboard module:
//   - components.platform.opendatahub.io is not served, or it serves no
//     Dashboard kind, or
//   - the RHOAI operator itself reconciles Dashboard: the installed CSV
//     owns CRD dashboards.components.platform.opendatahub.io and no
//     ServiceAccount of the applications namespace may update dashboards
//     and their finalizers (RHOAI 2.x and 3.4; rhods-operator
//     config/rhoai/manifests/bases CSV owns it there and not on 3.5/3.6).
//
// Everything else (a read error, several candidate Deployments, a
// ServiceAccount with the grant but no Deployment running as it, a CRD
// neither a module operator nor the CSV owns) is unknown, and Dashboard Dev
// refuses: following the legacy flow on a cluster with a dashboard module
// would patch workloads the module operator resets.

// dashboardTarget is where Dashboard Dev acts.
type dashboardTarget struct {
	// Operator is the dashboard module's operator; nil on the legacy flow.
	Operator *moduleOperator
	// Legacy says why the cluster has no dashboard module ("" when it has).
	Legacy string
	// Namespace is the applications namespace, where the dashboard runs; ""
	// when no DSCInitialization exists (legacy flow only).
	Namespace string
}

// dashboardOperatorError: the dashboard module's operator cannot be
// identified. Callers refuse to change anything.
type dashboardOperatorError struct {
	why string
	err error
}

func (e *dashboardOperatorError) Error() string {
	reason := e.why
	if e.err != nil {
		reason = e.err.Error()
	}
	return "cannot identify the dashboard module's operator Deployment: " + reason
}

func (e *dashboardOperatorError) Unwrap() error { return e.err }

// discoverDashboardTarget identifies the dashboard module's operator, or
// that the cluster has none (see above). It always reads the cluster.
func discoverDashboardTarget(c *Client) (dashboardTarget, error) {
	api, err := rhoai.DiscoverComponentAPI(c)
	if err != nil {
		return dashboardTarget{}, &dashboardOperatorError{err: err}
	}
	legacy := ""
	switch _, served := api.KindOf(rhoai.ComponentAPIGroup, "Dashboard"); {
	case api.Groups[rhoai.ComponentAPIGroup] == "":
		legacy = "the " + rhoai.ComponentAPIGroup + " API is not served"
	case !served:
		legacy = "no Dashboard kind is served in " + rhoai.ComponentAPIGroup
	}
	if legacy == "" {
		op, why, err := newModuleOperatorFinder(c, func() (rhoai.ComponentAPI, error) { return api, nil }).find("dashboard", "")
		switch {
		case err != nil:
			return dashboardTarget{}, &dashboardOperatorError{err: err}
		case op == nil:
			return dashboardTarget{}, &dashboardOperatorError{why: why}
		case !op.RHOAI:
			return dashboardTarget{Operator: op, Namespace: op.Namespace}, nil
		}
		legacy = "the RHOAI operator reconciles Dashboard itself (" + op.Via + ")"
	}
	ns, err := rhoai.AppsNamespace(c)
	if err != nil && !errors.Is(err, rhoai.ErrNoDSCInitialization) {
		return dashboardTarget{}, &dashboardOperatorError{err: err}
	}
	return dashboardTarget{Legacy: legacy, Namespace: ns}, nil
}

// dashboardTargetTTL bounds how long a status read reuses a discovery.
// Changes always discover again (loadDashboardOperator fresh).
const dashboardTargetTTL = 30 * time.Second

var dashboardTargetCache = kube.NewLRU[string, dashboardTarget](16)

// cachedDashboardTarget is discoverDashboardTarget for status reads, which
// the pages poll: the RBAC lists it reads are large. Errors are not cached.
func cachedDashboardTarget(c *Client) (dashboardTarget, error) {
	key := c.BaseURL()
	if t, ok := dashboardTargetCache.Get(key); ok {
		return t, nil
	}
	t, err := discoverDashboardTarget(c)
	if err == nil {
		dashboardTargetCache.Add(key, t, dashboardTargetTTL)
	}
	return t, err
}

// readDashboardOperatorAt reads the operator Deployment ns/name; nil when
// it does not exist.
func readDashboardOperatorAt(c *Client, ns, name string) (*dashboardDeployment, error) {
	body, status, err := c.Get(rhoai.NamespacedPath("apps/v1", "deployments", ns, name))
	if status == http.StatusNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var d dashboardDeployment
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, err
	}
	if d.Metadata.UID == "" {
		return nil, fmt.Errorf("deployment %s/%s has no UID", ns, name)
	}
	// The path's namespace and name are what was discovered.
	d.Metadata.Namespace, d.Metadata.Name = ns, name
	return &d, nil
}

// loadDashboardOperator discovers where Dashboard Dev acts and reads the
// operator Deployment. On the legacy flow operator is nil and err is nil.
// fresh: discover again (every change); otherwise a status read may reuse
// a recent discovery, and discovers again when its Deployment is gone.
func loadDashboardOperator(c *Client, fresh bool) (dashboardTarget, *dashboardDeployment, error) {
	discover := cachedDashboardTarget
	if fresh {
		discover = discoverDashboardTarget
	}
	t, err := discover(c)
	if err != nil || t.Operator == nil {
		return t, nil, err
	}
	d, err := readDashboardOperatorAt(c, t.Operator.Namespace, t.Operator.Name)
	if err != nil {
		return t, nil, err
	}
	if d == nil {
		dashboardTargetCache.Remove(c.BaseURL())
		if !fresh {
			return loadDashboardOperator(c, true)
		}
		return t, nil, &dashboardOperatorError{why: fmt.Sprintf("Deployment %s was identified but no longer exists; the RHOAI platform recreates it, retry then", t.Operator.ref())}
	}
	return t, d, nil
}
