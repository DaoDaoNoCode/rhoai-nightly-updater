package cluster

import (
	"errors"
	"fmt"
	"time"
)

// Where RHOAI runs its applications and its monitoring stack, read from the
// cluster instead of assumed. The DSCInitialization names both
// (spec.applicationsNamespace, spec.monitoring.namespace); the operator
// creates them from it, and an install may choose others than the RHOAI
// defaults (redhat-ods-applications, redhat-ods-monitoring) or the ODH ones
// (opendatahub, opendatahub-odh-monitoring).
//
// The operator's own namespace is SubNS: the namespace of the Subscription
// this tool installs and reads RHOAI through. OLM runs a CSV's Deployments
// in the CSV's namespace, which is the Subscription's.
//
// A read failure is never replaced by a default name: a change refuses
// (fail closed), and a status read shows that the namespace is unknown.

// errNoDSCInitialization: no DSCInitialization exists (its API is not
// served, or none was created). This is definite, not a read failure:
// RHOAI has no applications namespace on this cluster yet (not installed,
// still installing, or uninstalled).
var errNoDSCInitialization = errors.New("no DSCInitialization exists, so RHOAI has no applications namespace on this cluster")

// rhoaiNamespaces are the namespaces the DSCInitialization names.
type rhoaiNamespaces struct {
	Applications string
	// Monitoring is "" when the DSCInitialization names none.
	Monitoring string
}

// rhoaiNamespacesTTL bounds how long a namespace read is reused. The
// operator does not let spec.applicationsNamespace change while the
// DSCInitialization exists; a reinstall with another namespace is picked
// up within this time. Only successful reads are cached.
const rhoaiNamespacesTTL = 30 * time.Second

var rhoaiNamespacesCache = newLRU[string, rhoaiNamespaces](16)

// discoverRHOAINamespaces returns the DSCInitialization's namespaces,
// cached per cluster for rhoaiNamespacesTTL. err is errNoDSCInitialization
// when none exists, or a read error; callers must not guess a name then.
func discoverRHOAINamespaces(c *Client) (rhoaiNamespaces, error) {
	key := c.baseURL
	if ns, ok := rhoaiNamespacesCache.Get(key); ok {
		return ns, nil
	}
	dscis, apiFound, err := listFirstServed(c, dsciGroup, "/apis/dscinitialization.opendatahub.io/%s/dscinitializations", "dscinitializations", dsciFallbackVersions)
	switch {
	case err != nil:
		return rhoaiNamespaces{}, fmt.Errorf("read the DSCInitialization for the RHOAI namespaces: %w", err)
	case !apiFound || len(dscis) == 0:
		return rhoaiNamespaces{}, errNoDSCInitialization
	case len(dscis) > 1:
		return rhoaiNamespaces{}, fmt.Errorf("%d DSCInitializations exist, so which namespaces RHOAI uses is ambiguous", len(dscis))
	case dscis[0].Spec.ApplicationsNamespace == "":
		return rhoaiNamespaces{}, fmt.Errorf("DSCInitialization %s names no applications namespace", dscis[0].Metadata.Name)
	}
	ns := rhoaiNamespaces{Applications: dscis[0].Spec.ApplicationsNamespace, Monitoring: dscis[0].Spec.Monitoring.Namespace}
	rhoaiNamespacesCache.Add(key, ns, rhoaiNamespacesTTL)
	return ns, nil
}

// appsNamespace is RHOAI's applications namespace (see
// discoverRHOAINamespaces for the errors).
func appsNamespace(c *Client) (string, error) {
	ns, err := discoverRHOAINamespaces(c)
	return ns.Applications, err
}

// applicationsNamespace is appsNamespace, "" when it is not known.
func applicationsNamespace(c *Client) string {
	ns, _ := appsNamespace(c)
	return ns
}

// appsNamespaceProblem is the sentence a change refuses with when the
// applications namespace is not known.
func appsNamespaceProblem(err error) string {
	return fmt.Sprintf("Cannot tell which namespace RHOAI's applications run in (%v). Nothing was changed.", err)
}
