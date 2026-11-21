package cluster

import (
	_ "embed"
	"fmt"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Where the nodes get their quay.io/rhoai credentials depends on the control
// plane. A hosted control plane (ROSA HCP, HyperShift) merges
// kube-system/additional-pull-secret into the nodes' pull secret. A
// self-managed cluster does not: its nodes pull with the cluster pull
// secret, openshift-config/pull-secret, only. The updater still writes
// additional-pull-secret everywhere: its own Quay lookups read it
// (getQuayAuth). Its ServiceAccount has no access to
// openshift-config/pull-secret, which holds every registry credential of
// the cluster; on a self-managed cluster the signed-in cluster-admin adds
// the entry with one click (node_setup.go, with their own token), or runs
// NodePullSecretCommand.
const (
	// ControlPlaneHosted: Infrastructure status.controlPlaneTopology
	// External, a hosted control plane.
	ControlPlaneHosted = "hosted"
	// ControlPlaneManaged: the control plane runs on the cluster (any
	// topology but External) and Red Hat runs the cluster: ROSA Classic or
	// OpenShift Dedicated, known by Red Hat SRE's admission webhooks
	// (isRedHatManagedWebhook).
	ControlPlaneManaged = "managed"
	// ControlPlaneSelfManaged: any other topology (HighlyAvailable,
	// SingleReplica, ...) without those webhooks.
	ControlPlaneSelfManaged = "self-managed"
)

// readControlPlane reads Infrastructure "cluster" (config.openshift.io/v1)
// and, unless the control plane is hosted, the validating webhook
// configurations, and returns ControlPlaneHosted, ControlPlaneManaged or
// ControlPlaneSelfManaged. A failed read, or a missing
// status.controlPlaneTopology, is an error: the platform is then unknown,
// and callers must not assume the nodes have the updater's credentials.
//
// The topology is read first on purpose: ROSA HCP has *.managed.openshift.io
// webhooks too (managed-cluster-validating-webhooks @eb1c03ee
// build/resources.go:1022-1078 packages every HypershiftEnabled webhook for
// hosted clusters), so the webhooks alone would call a hosted cluster
// managed (TestNodeSetupHostedCheckComesFirst).
func readControlPlane(c *Client) (string, error) {
	hosted, err := readTopologyHosted(c)
	if err != nil {
		return "", err
	}
	if hosted {
		return ControlPlaneHosted, nil
	}
	managed, err := readRedHatManaged(c)
	if err != nil {
		return "", err
	}
	if managed {
		return ControlPlaneManaged, nil
	}
	return ControlPlaneSelfManaged, nil
}

// readTopologyHosted reads Infrastructure "cluster": whether its
// status.controlPlaneTopology is External (a hosted control plane).
func readTopologyHosted(c *Client) (bool, error) {
	var infra struct {
		Status struct {
			ControlPlaneTopology string `json:"controlPlaneTopology"`
		} `json:"status"`
	}
	if err := c.getInto(clusterPath("config.openshift.io/v1", "infrastructures", "cluster"), &infra); err != nil {
		if isDecodeError(err) {
			return false, fmt.Errorf("parse Infrastructure cluster: %w", err)
		}
		return false, fmt.Errorf("read Infrastructure cluster: %w", err)
	}
	if infra.Status.ControlPlaneTopology == "" {
		return false, fmt.Errorf("infrastructure cluster has no status.controlPlaneTopology")
	}
	return infra.Status.ControlPlaneTopology == "External", nil
}

// redHatManagedWebhookSuffix ends the name of every webhook of Red Hat
// SRE's managed-cluster-validating-webhooks
// (github.com/openshift/managed-cluster-validating-webhooks build/resources.go:
// configuration "sre-<hook>", webhook "<hook>.managed.openshift.io"). Hive
// syncs them to ROSA Classic and OpenShift Dedicated clusters
// (ClassicEnabled); a package brings the HypershiftEnabled ones to ROSA
// HCP, which readControlPlane tells apart by its topology first; a
// self-managed cluster has none. Among them,
// imagecontentpolicies-validation refuses mirrors of registry.redhat.io
// itself, quay.io and registry.access.redhat.com, but not of
// registry.redhat.io/rhoai.
const redHatManagedWebhookSuffix = ".managed.openshift.io"

// readRedHatManaged lists the ValidatingWebhookConfigurations (the
// ServiceAccount's existing list rule, also used by the stale-webhook
// check): whether one holds a Red Hat SRE webhook.
func readRedHatManaged(c *Client) (bool, error) {
	var items []struct {
		Webhooks []struct {
			Name string `json:"name"`
		} `json:"webhooks"`
	}
	if err := c.listInto(clusterPath("admissionregistration.k8s.io/v1", "validatingwebhookconfigurations", ""), &items); err != nil {
		if isDecodeError(err) {
			return false, fmt.Errorf("parse validating webhook configurations: %w", err)
		}
		return false, fmt.Errorf("list validating webhook configurations: %w", err)
	}
	for _, item := range items {
		for _, wh := range item.Webhooks {
			if strings.HasSuffix(wh.Name, redHatManagedWebhookSuffix) {
				return true, nil
			}
		}
	}
	return false, nil
}

// The topology of a cluster does not change while the app runs; a read
// error is not cached, so the next status poll tries again.
var controlPlaneCache = newLRU[string, string](8)

// cachedControlPlane is readControlPlane, cached per cluster; "" when it
// cannot be read (unknown).
func cachedControlPlane(c *Client) string {
	if cp, ok := controlPlaneCache.Get(c.baseURL); ok {
		return cp
	}
	cp, err := readControlPlane(c)
	if err != nil {
		return ""
	}
	controlPlaneCache.Add(c.baseURL, cp, time.Hour)
	return cp
}

// node_pull_secret.sh adds the quay.io/rhoai entry to
// openshift-config/pull-secret. It is shown on the setup card, for a
// cluster-admin to run in bash or zsh. Why each part:
//   - printf, then `read -rs`: the token is typed at a prompt that does not
//     echo it, so it is not on the page, in the shell history or on the
//     screen. The prompt is printed by printf because zsh's read has no -p
//     (there it means a coprocess, and the read fails).
//   - `[ -n "$QUAY_AUTH" ] &&`: nothing runs without a token (an empty
//     line, Ctrl-D, a failed read), so an empty auth is never written.
//   - The token reaches jq through its environment ($ENV.QUAY_AUTH), not its
//     arguments, which `ps` shows to every user of the machine.
//   - jq changes only the quay.io/rhoai key of .dockerconfigjson; every other
//     registry login stays as it is.
//   - `oc get -o json` keeps metadata.resourceVersion in the object, so
//     `oc replace` fails with a conflict (409) if the Secret changed
//     meanwhile, instead of overwriting that change.
//   - A missing or garbled Secret makes jq fail (or print nothing), so
//     `oc replace` gets no object and changes nothing.
//   - @base64d and $ENV need jq 1.6 or later.
//   - `unset QUAY_AUTH` drops the token from the shell.
//
// The token is the value step 1 takes: the base64 "auth" of a Docker
// config, user:password. Step 1 also accepts it without its = padding (the
// updater pads it); CRI-O rejects an unpadded value, so the prompt asks for
// the padded one. scripts/test-node-pull-secret.sh runs this file against
// a stub oc.
//
//go:embed node_pull_secret.sh
var nodePullSecretScript string

// NodePullSecretCommand is node_pull_secret.sh without its final newline.
var NodePullSecretCommand = strings.TrimSpace(nodePullSecretScript)

// node_pull_secret_remove.sh removes the quay.io/rhoai entry from
// openshift-config/pull-secret, for a cluster-admin who prefers the CLI.
// Unlike Remove from the nodes' pull secret, which takes out only the entry
// this tool recorded adding (node_pull_record.go), the command removes
// whatever entry is there, also one the cluster was provisioned with: it
// says so and runs only after the user types yes (printf and read, as in
// node_pull_secret.sh; no comment line, which zsh does not take at an
// interactive prompt by default). The same safeguards as
// node_pull_secret.sh: only that key changes, a missing or garbled Secret
// gives oc replace no object, and the kept resourceVersion makes a
// concurrent change fail with a conflict. It needs no token.
// scripts/test-node-pull-secret.sh runs it against a stub oc.
//
//go:embed node_pull_secret_remove.sh
var nodePullSecretRemoveScript string

// NodePullSecretRemoveCommand is node_pull_secret_remove.sh without its
// final newline.
var NodePullSecretRemoveCommand = strings.TrimSpace(nodePullSecretRemoveScript)

// nodePullInfo is the nodes' side of the pull secret for a cluster whose
// nodes may not use additional-pull-secret (controlPlane is
// ControlPlaneSelfManaged, ControlPlaneManaged or "" for unknown); nil on
// a hosted control plane. cs is the nightly CatalogSource.
func nodePullInfo(controlPlane string, cs types.CatalogSourceInfo) *types.NodePullSecretInfo {
	if controlPlane == ControlPlaneHosted {
		return nil
	}
	return &types.NodePullSecretInfo{Command: NodePullSecretCommand, RemoveCommand: NodePullSecretRemoveCommand, Pulled: catalogPulledFromQuay(cs)}
}

// catalogPulledFromQuay: the nightly CatalogSource is READY on a
// quay.io/rhoai image, so a node has pulled from quay.io/rhoai with the
// nodes' own pull secret (its registry pod runs that image).
func catalogPulledFromQuay(cs types.CatalogSourceInfo) bool {
	return cs.Exists && cs.State == "READY" && strings.HasPrefix(cs.Image, "quay.io/rhoai/")
}

// nodePullAdvice is what a catalog image pull failure tells the user to
// check, for the cluster's control plane.
func nodePullAdvice(c *Client) string {
	return nodePullAdviceFor(cachedControlPlane(c))
}

// The cluster setup's pull secret item, where the command is.
const nodePullCommandPlace = "the command under Pull secret in the cluster setup (Status page)"

func nodePullAdviceFor(controlPlane string) string {
	switch controlPlane {
	case ControlPlaneHosted:
		return "Check that the pull secret has access to quay.io/rhoai (Setup step 1) and that the image exists; on a new cluster the credentials can take a few minutes to reach every node"
	case ControlPlaneSelfManaged:
		return "This cluster is self-managed: its nodes pull with the cluster pull secret, openshift-config/pull-secret, not with additional-pull-secret, which only the updater uses here. " +
			"A cluster-admin adds the quay.io/rhoai entry with Add to the nodes' pull secret under Pull secret in the cluster setup (Status page), or with the command there; the nodes pick it up within a few minutes. Also check that the image exists"
	case ControlPlaneManaged:
		return "This cluster is run by Red Hat (ROSA Classic or OpenShift Dedicated): its nodes pull with the cluster pull secret, openshift-config/pull-secret, not with additional-pull-secret. " +
			"Red Hat creates that secret and documents no procedure for changing it there, so the updater does not change it; " + nodePullCommandPlace + " adds the quay.io/rhoai entry if you choose to. Also check that the image exists"
	default:
		return "The updater could not read whether this cluster has a hosted control plane. If it is self-managed, its nodes pull with the cluster pull secret, openshift-config/pull-secret, not with additional-pull-secret: " +
			"a cluster-admin adds the quay.io/rhoai entry with " + nodePullCommandPlace + ". On a hosted control plane (ROSA HCP), check the pull secret in Setup step 1. Also check that the image exists"
	}
}

// gitOpsProfilePullAdvice is the gitops profile's: it does not set up or
// read pull secrets (gitOpsProfileNoPullSecret).
func gitOpsProfilePullAdvice(*Client) string {
	return "Check that the cluster's pull secrets, which Git or ACM manage here, have access to quay.io/rhoai (the nodes pull with openshift-config/pull-secret, and on a hosted control plane also with kube-system/additional-pull-secret) and that the image exists"
}
