# Research: one-click image mirror and nodes' pull secret (4.0.0)

Findings behind the one-click setup of the image mirror (ImageDigestMirrorSet
`registry.redhat.io/rhoai` to `quay.io/rhoai`) and of the `quay.io/rhoai` entry
in the cluster pull secret (`openshift-config/pull-secret`). Every platform
claim cites OpenShift documentation or source at a fixed commit; sources were
read on 2026-11-19 and verified again the same day.

Sources (commit, then what it is cited for):

| Source | Commit | Cited for |
|---|---|---|
| openshift/openshift-docs `enterprise-4.18` / `enterprise-4.20` / `enterprise-4.22` | `981b3ade` / `59cc908c` / `e363cad7` | OCP and ROSA books (procedures, node impact, release notes) |
| openshift/machine-config-operator | `4dabc4b5` | drain decisions, drain timeout, registries config validation and rendering |
| openshift/runtime-utils | `7bdb5b9c` | how IDMS, ICSP and ITMS become `/etc/containers/registries.conf` |
| openshift/managed-cluster-validating-webhooks | `eb1c03ee` | Red Hat SRE webhooks (managed-cluster detection) |
| openshift/api | `41d93514` | IDMS/ICSP semantics, MachineConfigPool `maxUnavailable` |
| openshift/hypershift | `0f43b296` | the hosted control plane's global pull secret |
| openshift/operator-framework-olm | `9822880e` | the image pull policy of a catalog's registry pod |

## A. Platform detection

Three kinds of cluster need different setups:

1. **Hosted control plane** (ROSA HCP, HyperShift): Infrastructure `cluster`
   `status.controlPlaneTopology: External` (`pkg/cluster/internal/rhoai/control_plane.go`).
   The mirror goes through OCM (`rosa create image-mirror`); the nodes get
   `kube-system/additional-pull-secret` through HyperShift's global pull
   secret (section D). Unchanged by this work.
2. **Red Hat-managed classic** (ROSA Classic, OpenShift Dedicated): topology is
   `HighlyAvailable` like a self-managed cluster. Signal chosen: a
   ValidatingWebhookConfiguration whose webhooks are named `*.managed.openshift.io`.
   Source: managed-cluster-validating-webhooks `build/resources.go` names each
   configuration `sre-<hook>` and each webhook `<hook>.managed.openshift.io`, in
   namespace `openshift-validation-webhook`, and ships them to classic clusters
   by a Hive SelectorSyncSet (`ClassicEnabled()`, :937).
   **ROSA HCP has them too**: the same file builds a package of every
   `HypershiftEnabled()` webhook for hosted clusters (:1022-1078). So the
   webhooks alone do not tell classic from hosted; detection is correct only
   because the topology is read first and an External topology decides before
   the webhooks are listed (`readControlPlane`, `nodeSetupPlatform`;
   `TestNodeSetupHostedCheckComesFirst` pins the order and that the webhooks
   are not even listed). The ServiceAccount already `list`s
   validatingwebhookconfigurations (stale-webhook diagnostics). Rejected
   candidates: ClusterVersion `spec.upstream` (same graph URL as
   self-managed), AWS resource tags `red-hat-managed` (AWS only, OSD on GCP has
   none), Hive labels (on the hub, not the cluster).
3. **Self-managed**: anything else (IPI/UPI on any cloud, bare metal, SNO).
4. **Unknown**: the topology is not External but the webhook list cannot be
   read (with the user's token, in the one-click setup). The cluster may be
   ROSA Classic or OSD, where the cluster pull secret is not changed (below),
   and no API check would stop that write, so **the pull secret actions
   refuse** (`unknown_platform`). The mirror actions are attempted: they are
   allowed on those clusters too, and RBAC or an admission webhook refuses
   them in the server-side dry run otherwise. When the topology itself cannot
   be read, every action refuses: a hosted cluster cannot be ruled out.

### What managed classic allows

- **IDMS: allowed.** Webhook `imagecontentpolicies-validation`
  (`pkg/webhooks/imagecontentpolicies/imagecontentpolicies.go`, same commit)
  denies CREATE/UPDATE of IDMS/ITMS/ICSP whose *source* matches
  `(^registry\.redhat\.io$|^quay\.io(/.*)?$|^registry\.access\.redhat\.com(/.*)?)`.
  `registry.redhat.io/rhoai` does not match (only exactly `registry.redhat.io`
  is blocked). ROSA Classic documents creating an ImageDigestMirrorSet as a
  `dedicated-admin` (ROSA Classic Images, 9.5.2 "Configure image registry
  repository mirroring",
  https://docs.redhat.com/en/documentation/red_hat_openshift_service_on_aws_classic_architecture/4/html-single/images/index;
  OSD Images has the same section).
- **Cluster pull secret: not offered.** No webhook covers Secrets
  (`pkg/webhooks/regularuser/common/regularuser.go` covers configmaps,
  machineconfigs, clusterversions and other groups, not secrets), but the ROSA
  Classic and OSD Images books have no "Update the global cluster pull secret"
  procedure (the OCP book has it, 5.4.3), and the service definition lists
  "Create clusters with image pull secrets" as Red Hat's responsibility. So the
  tool does not change it there: product standard, nothing undocumented. The
  command stays available under "Do it yourself instead".

## B. Which mirror objects count, and which would be overridden

How the Machine Config Operator renders `/etc/containers/registries.conf`
(runtime-utils `pkg/registries/registries.go` `EditRegistriesConfig`, :235-350):

- IDMS and ICSP entries are merged into one digest-only mirror set per source
  (`mergedDigestMirrorSets`, :165-183); ITMS entries give tag-only mirrors in
  the same registries.conf entry of their source (:285-289).
- An entry whose mirrors only repeat its source is ignored
  (`mirrorsContainsARealMirror`, :39-47).
- A mirror set is copied into a nested scope only while that scope's entry has
  **no mirror of its own** (:331-345), and the nodes use only the most specific
  match (openshift/api `config/v1/types_image_digest_mirror_set.go:45-49`:
  "only the objects which define the most specific namespace match will be
  used"). Scopes nest as in `ScopeIsNestedInsideScope` (:13-36): a namespace
  inside a registry, or a host matching a wildcard (`*.redhat.io`).
- The MCO refuses the whole registries configuration when two IDMS, ITMS or
  ICSP entries for one source set different `mirrorSourcePolicy` values
  (machine-config-operator
  `pkg/controller/container-runtime-config/helpers.go`
  `validateRegistriesConfScopes`, :961-975, called before
  `EditRegistriesConfig`, :590-596; an ICSP counts as the default,
  `convertICSPToIDMS`).

Verdicts for **Create the image mirror**:

| What is on the cluster | Verdict |
|---|---|
| An IDMS or ICSP with source `registry.redhat.io/rhoai` and `quay.io/rhoai` among its mirrors | Done, nothing to change. (Before this fix any IDMS with that source counted, whatever it mirrored to.) |
| Same source, other mirrors only | Not done: the new IDMS is created; both objects' mirrors are merged, the plan says so |
| Same source in an ITMS only | Not done: an ITMS serves no pull by digest |
| An IDMS/ITMS entry for exactly `registry.redhat.io/rhoai` with `mirrorSourcePolicy: NeverContactSource` | **Refused** (`policy_conflict`): the new IDMS has the default policy, and the MCO would refuse the whole mirror configuration |
| An IDMS, ICSP or ITMS with a real mirror for a parent scope (`registry.redhat.io`, `*.redhat.io`, ...), and no entry with a real mirror for exactly `registry.redhat.io/rhoai` | **Refused** (`parent_scope`): today the nodes pull `registry.redhat.io/rhoai/*` through that mirror (at its path plus `/rhoai`); a mirror for exactly the source would take those pulls away from it (by digest to `quay.io/rhoai`, then the source; for an ITMS parent, tag pulls to the source). The refusal names the object, what it maps, and the entry an administrator can add to it instead (its mirror's path first, then `quay.io/rhoai`) |
| A parent scope, but the source has an entry with a real mirror of its own (any kind) | Not refused: the parent is not used for it now either, nothing is taken away |
| An IDMS named `rhoai-quay-mirror` that is not done | Refused (`foreign`) |

The ICSP and ITMS lists fail closed: an unreadable list refuses; a 404 for the
collection means the API is not served, so no such object exists. The status
(ServiceAccount) also counts an ICSP for the source, so the ServiceAccount
lists `imagecontentsourcepolicies` (read-only; SECURITY.md).

## C. Node impact (Machine Config Operator)

Sources: OCP 4.20 Machine configuration
(https://docs.redhat.com/en/documentation/openshift_container_platform/4.20/html-single/machine_configuration/index):
"When the MCO detects any of the following changes, it applies the update
without draining or rebooting the node: ... Changes to the global pull secret
or pull secret in the openshift-config namespace." and "When the MCO detects
changes to the /etc/containers/registries.conf file, such as editing an
ImageDigestMirrorSet ... it drains the corresponding nodes, applies the
changes, and uncordons the nodes. The node drain does not happen for ... The
addition of a registry with the pull-from-mirror = "digest-only" parameter set
for each mirror." OCP 4.20 Disconnected environments: "After the object is
created, the MCO drains the nodes for ImageTagMirrorSet objects only. The MCO
does not drain the nodes for ImageDigestMirrorSet and ImageContentSourcePolicy
objects." ROSA Classic Images Table 9.3: IDMS Create: drain No, reboot No;
Modify: drain Yes, reboot No; Delete: drain Yes, reboot No.

Source (machine-config-operator):
- `pkg/daemon/update.go` `calculatePostConfigChangeActionFromMCDiffs`:
  `constants.KubeletAuthFile` (`/var/lib/kubelet/config.json`, the rendered
  pull secret) is in `filesPostConfigChangeActionNone`; `registries.conf` is
  "reload crio".
- `pkg/apihelpers/apihelpers.go` `defaultClusterPolicies`: KubeletAuthFile
  `None`; registries.conf `Special`.
- `pkg/daemon/drain.go` `isDrainRequired` / `isSafeContainerRegistryConfChanges`
  (:149-260): a removed registry is unsafe (drain); a new registry whose mirrors
  are all `pull-from-mirror=digest-only`, or a new digest-only mirror, is safe
  (no drain). runtime-utils `EditRegistriesConfig` writes IDMS mirrors with
  `sysregistriesv2.MirrorByDigestOnly`.
- `pkg/daemon/drain.go` `drainRequired`: no drain on a single-node topology (SNO).
- `pkg/daemon/drain.go` `performDrain` (:96-117): the daemon waits up to one
  hour for the drain, then fails the node's update ("failed to drain node ...
  after 1 hour", event `FailedToDrain`). The drain respects
  PodDisruptionBudgets (openshift/api `machineconfiguration/v1/types.go:427-439`),
  so a PDB that allows no eviction holds it.
- openshift/api `machineconfiguration/v1/types.go:427-439`: a pool's
  `maxUnavailable` defaults to 1, so its nodes update **one at a time by
  default**; a larger value updates more at once.
- `pkg/controller/container-runtime-config/container_runtime_config_controller.go`
  `syncIgnitionConfig` (:1185-1201): a MachineConfig whose rendered content is
  unchanged is not updated, so no node is updated.

So, with the default node disruption policies:
- **Pull secret entry added, changed or removed:** written to every node, no
  drain, no reboot, CRI-O not reloaded; one node at a time by default (the
  pool's `maxUnavailable`); `oc get mcp` UPDATED True when done.
- **IDMS created** (new source, digest-only mirror): no drain, no reboot; CRI-O
  is reloaded on each node.
- **IDMS deleted:** each node is drained (pods evicted and rescheduled) and
  uncordoned, CRI-O reloaded, no reboot; one node at a time by default; on SNO
  no drain. A PDB can hold a node's drain for up to an hour, then that node's
  update fails.
- **IDMS deleted while another IDMS or ICSP still lists `quay.io/rhoai` for
  `registry.redhat.io/rhoai`:** the merged mirror list keeps `quay.io/rhoai`;
  the deleted object only added an edge `quay.io/rhoai` -> source that the
  others imply, which changes the sorted order only when those objects list
  their mirrors in conflicting orders (registries.go `mergedMirrors`, topo.go
  `Sorted`). Otherwise registries.conf is the same, the MachineConfig is not
  updated, and no node is drained or updated.
- A paused MachineConfigPool takes neither until unpaused; an admin's
  NodeDisruptionPolicy can change the actions.

## D. Hosted control plane: the global pull secret

Source: hypershift `docs/content/how-to/common/global-pull-secret.md`, and the
ROSA HCP release notes (openshift-docs `e363cad7`
`modules/rosa-release-notes-q1-2026.adoc:30-32`, "Updating the global pull
secret is available for Red Hat OpenShift Service on AWS clusters", HCP only).
An earlier version of this file cited an OCP 4.20 Images sentence ("Hypershift
manages the new credential propagation among the HostedCluster nodes"), which
is in the OCP book, not a HyperShift source; it is replaced by these.

- `kube-system/additional-pull-secret`, a `kubernetes.io/dockerconfigjson`
  Secret, is merged with the HostedCluster pull secret and written to
  `/var/lib/kubelet/config.json` on eligible nodes (lines 5-9, 40-47).
- **Conflicting entries:** "If there are conflicting registry entries, the
  original pull secret takes precedence" (line 117). An existing
  `quay.io/rhoai` entry in the cluster's own pull secret wins over the
  updater's.
- **Which nodes:** the sync DaemonSet runs only on nodes of AWS and Azure
  **Replace** node pools (and Karpenter nodes), not on **InPlace** node pools
  or other platforms (lines 21-28).
- **Node impact:** when the content differs, the sync "restarts the kubelet
  service via DBus" on each node (line 139), rolling the file back after three
  failed restarts (line 140).

These caveats are on the Pull secret item for a hosted control plane, in the
hosted refusal of the one-click setup, and in RUNBOOK.

## E. Removal

- Removing the entry from the cluster pull secret: the nodes lose the
  credentials at the next rollout; images already on a node stay and running
  pods keep running; new pulls from quay.io/rhoai (a new nightly, a pod on a
  node that lacks the image) fail with unauthorized.
- Deleting the IDMS: registry.redhat.io/rhoai references are pulled from
  registry.redhat.io again, where nightly digests do not exist, unless other
  mirrors stay for the source (the plan names them) or a parent scope's mirror
  serves it again (the plan names it; Create then refuses, section B). The
  drain (not on SNO) evicts pods, which come back on nodes that may already
  have the image. A cluster-wide object: only the IDMS this setup creates is
  deleted (name `rhoai-quay-mirror`, exactly the one source and mirror, UID and
  resourceVersion preconditions).

## F. Check that the nodes can pull

OLM gives a catalog's registry pod `imagePullPolicy` from the image reference:
an `@` (digest) gives `IfNotPresent`, anything else `Always`
(operator-framework-olm `staging/operator-lifecycle-manager/pkg/lib/image/image.go:9-17`,
`InferImagePullPolicy`). A node that already has a digest's image would not
pull it again, so the check would pass without the nodes' credentials. The
check therefore takes the catalog image **by tag** only: the UI sends the
build's tag, and the API refuses a digest reference before anything is created.
