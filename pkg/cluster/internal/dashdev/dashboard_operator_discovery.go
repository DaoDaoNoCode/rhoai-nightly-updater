package dashdev

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/components"
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

// Target is where Dashboard Dev acts.
type Target struct {
	// Operator is the dashboard module's operator; nil on the legacy flow.
	Operator *components.ModuleOperator
	// Legacy says why the cluster has no dashboard module ("" when it has).
	Legacy string
	// Namespace is the applications namespace, where the dashboard runs; ""
	// when no DSCInitialization exists (legacy flow only).
	Namespace string
}

// OperatorError: the dashboard module's operator cannot be
// identified. Callers refuse to change anything.
type OperatorError struct {
	why string
	err error
}

func (e *OperatorError) Error() string {
	reason := e.why
	if e.err != nil {
		reason = e.err.Error()
	}
	return "cannot identify the dashboard module's operator Deployment: " + reason
}

func (e *OperatorError) Unwrap() error { return e.err }

// DiscoverTarget identifies the dashboard module's operator, or
// that the cluster has none (see above). It always reads the cluster.
func DiscoverTarget(c *kube.Client) (Target, error) {
	api, err := rhoai.DiscoverComponentAPI(c)
	if err != nil {
		return Target{}, &OperatorError{err: err}
	}
	legacy := ""
	switch _, served := api.KindOf(rhoai.ComponentAPIGroup, "Dashboard"); {
	case api.Groups[rhoai.ComponentAPIGroup] == "":
		legacy = "the " + rhoai.ComponentAPIGroup + " API is not served"
	case !served:
		legacy = "no Dashboard kind is served in " + rhoai.ComponentAPIGroup
	}
	if legacy == "" {
		op, why, err := components.NewModuleOperatorFinder(c, func() (rhoai.ComponentAPI, error) { return api, nil }).Find("dashboard", "")
		switch {
		case err != nil:
			return Target{}, &OperatorError{err: err}
		case op == nil:
			return Target{}, &OperatorError{why: why}
		case !op.RHOAI:
			return Target{Operator: op, Namespace: op.Namespace}, nil
		}
		legacy = "the RHOAI operator reconciles Dashboard itself (" + op.Via + ")"
	}
	ns, err := rhoai.AppsNamespace(c)
	if err != nil && !errors.Is(err, rhoai.ErrNoDSCInitialization) {
		return Target{}, &OperatorError{err: err}
	}
	return Target{Legacy: legacy, Namespace: ns}, nil
}

// TargetTTL bounds how long a status read reuses a discovery.
// Changes always discover again (LoadOperator fresh).
const TargetTTL = 30 * time.Second

var TargetCache = kube.NewLRU[string, Target](16)

// cachedDashboardTarget is DiscoverTarget for status reads, which
// the pages poll: the RBAC lists it reads are large. Errors are not cached.
func cachedDashboardTarget(c *kube.Client) (Target, error) {
	key := c.BaseURL()
	if t, ok := TargetCache.Get(key); ok {
		return t, nil
	}
	t, err := DiscoverTarget(c)
	if err == nil {
		TargetCache.Add(key, t, TargetTTL)
	}
	return t, err
}

// readDashboardOperatorAt reads the operator Deployment ns/name; nil when
// it does not exist.
func readDashboardOperatorAt(c *kube.Client, ns, name string) (*Deployment, error) {
	body, status, err := c.Get(rhoai.NamespacedPath("apps/v1", "deployments", ns, name))
	if status == http.StatusNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var d Deployment
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

// LoadOperator discovers where Dashboard Dev acts and reads the
// operator Deployment. On the legacy flow operator is nil and err is nil.
// fresh: discover again (every change); otherwise a status read may reuse
// a recent discovery, and discovers again when its Deployment is gone.
func LoadOperator(c *kube.Client, fresh bool) (Target, *Deployment, error) {
	discover := cachedDashboardTarget
	if fresh {
		discover = DiscoverTarget
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
		TargetCache.Remove(c.BaseURL())
		if !fresh {
			return LoadOperator(c, true)
		}
		return t, nil, &OperatorError{why: fmt.Sprintf("Deployment %s was identified but no longer exists; the RHOAI platform recreates it, retry then", t.Operator.Ref())}
	}
	return t, d, nil
}
