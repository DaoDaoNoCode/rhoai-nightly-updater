package rhoai

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
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
var ErrNoDSCInitialization = errors.New("no DSCInitialization exists, so RHOAI has no applications namespace on this cluster")

// Namespaces are the namespaces the DSCInitialization names.
type Namespaces struct {
	Applications string
	// Monitoring is "" when the DSCInitialization names none.
	Monitoring string
}

// rhoaiNamespacesTTL bounds how long a namespace read is reused. The
// operator does not let spec.applicationsNamespace change while the
// DSCInitialization exists; a reinstall with another namespace is picked
// up within this time. Only successful reads are cached.
const rhoaiNamespacesTTL = 30 * time.Second

var NamespacesCache = kube.NewLRU[string, Namespaces](16)

// DiscoverNamespaces returns the DSCInitialization's namespaces,
// cached per cluster for rhoaiNamespacesTTL. err is errNoDSCInitialization
// when none exists, or a read error; callers must not guess a name then.
//
// The DSCInitialization is read only with the cluster facts client
// (c.Facts(): the ServiceAccount's, which deploy/template.yaml lets list
// DSCInitializations), so the cache all users share is filled only by the
// ServiceAccount, and a change answers the same whoever runs it. A client
// that acts with the signed-in user's token and has no facts client is
// refused (kube.ErrNoFactsClient): it neither reads the cache nor fills
// it, so a project user's change never depends on whether an earlier read
// filled it.
func DiscoverNamespaces(c *kube.Client) (Namespaces, error) {
	if c.Facts().ActsAsUser() {
		return Namespaces{}, kube.ErrNoFactsClient
	}
	key := c.BaseURL()
	if ns, ok := NamespacesCache.Get(key); ok {
		return ns, nil
	}
	// Status reads ask from several goroutines at once: one read serves them.
	ctx := c.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	ns, err, _ := rhoaiNamespacesFlight.Do(ctx, key, func(context.Context) (Namespaces, error) {
		if ns, ok := NamespacesCache.Get(key); ok {
			return ns, nil
		}
		return readRHOAINamespaces(c.Facts(), key)
	})
	return ns, err
}

var rhoaiNamespacesFlight kube.FlightGroup[Namespaces]

func readRHOAINamespaces(c *kube.Client, key string) (Namespaces, error) {
	dscis, apiFound, err := ListFirstServed(c, DSCIGroup, "/apis/dscinitialization.opendatahub.io/%s/dscinitializations", "dscinitializations", DSCIFallbackVersions)
	switch {
	case err != nil:
		return Namespaces{}, fmt.Errorf("read the DSCInitialization for the RHOAI namespaces: %w", err)
	case !apiFound || len(dscis) == 0:
		return Namespaces{}, ErrNoDSCInitialization
	case len(dscis) > 1:
		return Namespaces{}, fmt.Errorf("%d DSCInitializations exist, so which namespaces RHOAI uses is ambiguous", len(dscis))
	case dscis[0].Spec.ApplicationsNamespace == "":
		return Namespaces{}, fmt.Errorf("DSCInitialization %s names no applications namespace", dscis[0].Metadata.Name)
	}
	ns := Namespaces{Applications: dscis[0].Spec.ApplicationsNamespace, Monitoring: dscis[0].Spec.Monitoring.Namespace}
	NamespacesCache.Add(key, ns, rhoaiNamespacesTTL)
	return ns, nil
}

// AppsNamespace is RHOAI's applications namespace (see
// discoverRHOAINamespaces for the errors).
func AppsNamespace(c *kube.Client) (string, error) {
	ns, err := DiscoverNamespaces(c)
	return ns.Applications, err
}

// ApplicationsNamespace is appsNamespace, "" when it is not known.
func ApplicationsNamespace(c *kube.Client) string {
	ns, _ := AppsNamespace(c)
	return ns
}

// appsNamespaceOrPlaceholder is the applications namespace for a command a
// person runs, or <applications-namespace> when it is not known.
func appsNamespaceOrPlaceholder(c *kube.Client) string {
	if ns, err := AppsNamespace(c); err == nil {
		return ns
	}
	return "<applications-namespace>"
}

// PodsCommand lists the pods of the operator and applications namespaces.
func PodsCommand(c *kube.Client) string {
	return kube.ShellCommand("oc", "get", "pods", "-n", SubNS) + "; " + kube.ShellCommand("oc", "get", "pods", "-n", appsNamespaceOrPlaceholder(c))
}
