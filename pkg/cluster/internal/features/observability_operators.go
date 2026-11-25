package features

import (
	"fmt"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/serving"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The operators RHOAI's monitoring module needs for usage logs and the
// metrics dashboards (red-hat-data-services/odh-observability rhoai-3.7-ea.1
// docs/usage-logs.md L28-33: "Install the observability module and
// OpenTelemetry Operator. For local storage, also install Loki Operator";
// docs/observability-modes.md L474: Perses comes with the Cluster
// Observability Operator), each as RHOAI's own odh-gitops installs it
// (opendatahub-io/odh-gitops e1eb776bf1 components/operators/<op>/:
// namespace.yaml, operatorgroup.yaml, subscription.yaml) on the catalog's
// default channel (odh-gitops pins Loki to stable-6.5; RHOAI's Monitoring
// reconciler checks only that the operator is installed through OLM, and the
// catalog default, stable-6.7, was accepted live on RHOAI 3.7.0-ea.1). The
// Monitoring CR reports what it finds (MonitoringDependenciesReady,
// COOVersionCompatible). cert-manager comes first: the OpenTelemetry
// collector's target allocator uses its certificates.
//
// openshift-operators-redhat exists on ROSA without an OperatorGroup: the
// framework then creates the OperatorGroup only, and uses the namespace as
// it is (its cluster-monitoring label only lets the platform Prometheus
// scrape the operator's own metrics: Red Hat OpenShift Logging 6.x,
// "Installing Loki Operator by using the CLI"). These operators stay after
// Clean up, as other features and other admins share them.
var (
	COOOperator = envkit.OperatorSpec{ID: "cluster-observability-operator", Title: "Cluster Observability Operator", Pkg: "cluster-observability-operator",
		SubName: "cluster-observability-operator", SubNames: []string{"cluster-observability-operator"}, Namespace: "openshift-cluster-observability-operator",
		Source: "redhat-operators", Group: "openshift-cluster-observability-operator"}
	OpenTelemetryOperator = envkit.OperatorSpec{ID: "opentelemetry", Title: "Red Hat build of OpenTelemetry", Pkg: "opentelemetry-product",
		SubName: "opentelemetry-product", SubNames: []string{"opentelemetry-product"}, Namespace: "openshift-opentelemetry-operator",
		Source: "redhat-operators", Group: "openshift-opentelemetry-operator", NSLabels: map[string]string{"openshift.io/cluster-monitoring": "true"}}
	LokiOperator = envkit.OperatorSpec{ID: "loki-operator", Title: "Loki Operator", Pkg: "loki-operator",
		SubName: "loki-operator", SubNames: []string{"loki-operator"}, Namespace: "openshift-operators-redhat",
		Source: "redhat-operators", Group: "openshift-operators-redhat",
		NSLabels: map[string]string{"openshift.io/cluster-monitoring": "true"}, NSAnnotations: map[string]string{"openshift.io/node-selector": ""}}
)

// observabilitySetup is what the Observability tab's operators are: no
// DataScienceCluster component (RHOAI's monitoring is configured in the
// DSCInitialization), four operators, cert-manager first.
var observabilitySetup = featureSetup{id: "observability", title: "Observability",
	operators: []envkit.OperatorSpec{certManagerOperator, COOOperator, OpenTelemetryOperator, LokiOperator}}

// ObservabilityOperatorIDs are the step IDs of the Observability operators,
// in install order.
func ObservabilityOperatorIDs() []string {
	var out []string
	for _, op := range observabilitySetup.operators {
		out = append(out, op.ID)
	}
	return out
}

// OperatorsPlan is the Observability operators as read, and what setup does
// about each (a featurePlan without components).
type OperatorsPlan struct{ p *featurePlan }

// PlanObservabilityOperators reads the Observability operators. A read that
// fails refuses (Refusal).
func PlanObservabilityOperators(c *kube.Client) *OperatorsPlan {
	return &OperatorsPlan{p: planFeature(c, observabilitySetup, readClusterForPrerequisites(c))}
}

// Parts are the operators, one part each, in install order.
func (o *OperatorsPlan) Parts() []types.MaaSPart { return o.p.parts }

// Plan lists what setup installs.
func (o *OperatorsPlan) Plan() []string { return o.p.plan }

// Refusal is why setup cannot install them ("" when it can), and its code.
func (o *OperatorsPlan) Refusal() (string, string) { return o.p.refusal, o.p.code }

// Ready: every operator is installed and running.
func (o *OperatorsPlan) Ready() bool {
	for _, pt := range o.p.parts {
		if pt.State != "ready" {
			return false
		}
	}
	return true
}

// CSVNamespace is where the operator id runs ("" when it is not installed).
func (o *OperatorsPlan) CSVNamespace(id string) string {
	for _, op := range o.p.operators {
		if op.op.ID == id {
			return op.st.CSVNS
		}
	}
	return ""
}

// DryRun makes a server-side dry run of every object setup creates.
func (o *OperatorsPlan) DryRun(c *kube.Client) error {
	for _, op := range o.p.operators {
		if op.install == nil {
			continue
		}
		if _, err := envkit.CreateOperatorObjects(c, op.install, true); err != nil {
			return fmt.Errorf("the API server rejects %s's install: %w", op.op.Title, err)
		}
	}
	return nil
}

// Install installs the missing operators in order, each on its own step,
// and waits for OLM; an operator OLM is installing already is waited for.
// The prerequisites record keeps what this tool installed (shared: Clean up
// never removes them).
func (o *OperatorsPlan) Install(m *serving.Run) (*types.OperationResponse, error) {
	for _, op := range o.p.operators {
		if r, err := ensureOperator(m, &o.p.rec, op); r != nil || err != nil {
			return r, err
		}
	}
	return nil, nil
}

// Missing are the titles of the operators that are not ready.
func (o *OperatorsPlan) Missing() []string {
	var out []string
	for _, pt := range o.p.parts {
		if pt.State != "ready" {
			out = append(out, pt.Title)
		}
	}
	return out
}

// Summary is the operators' state in one sentence.
func (o *OperatorsPlan) Summary() string {
	if missing := o.Missing(); len(missing) > 0 {
		return "Not ready yet: " + strings.Join(missing, ", ") + "."
	}
	return "Every operator is installed."
}
