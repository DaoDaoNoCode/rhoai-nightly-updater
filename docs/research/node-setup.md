# Research: one-click image mirror and nodes' pull secret (4.0.0)

Findings behind the one-click setup of the image mirror (ImageDigestMirrorSet
`registry.redhat.io/rhoai` to `quay.io/rhoai`) and of the `quay.io/rhoai` entry
in the cluster pull secret (`openshift-config/pull-secret`). Every platform
claim cites OpenShift documentation or source; sources were read on 2026-11-19.

## A. Platform detection

Three kinds of cluster need different setups:

1. **Hosted control plane** (ROSA HCP, HyperShift): Infrastructure `cluster`
   `status.controlPlaneTopology: External` (already read; `pkg/cluster/control_plane.go`).
   The mirror goes through OCM (`rosa create image-mirror`); the nodes get
   `kube-system/additional-pull-secret`, which HyperShift merges ("Hypershift
   manages the new credential propagation among the HostedCluster nodes",
   OCP 4.20 Images, 5.4.3 "Update the global cluster pull secret",
   https://docs.redhat.com/en/documentation/openshift_container_platform/4.20/html-single/images/index).
   Unchanged by this work.
2. **Red Hat-managed classic** (ROSA Classic, OpenShift Dedicated): topology is
   `HighlyAvailable` like a self-managed cluster. Signal chosen: a
   ValidatingWebhookConfiguration whose webhooks are named `*.managed.openshift.io`.
   Source: openshift/managed-cluster-validating-webhooks @eb1c03ee72ee9f3dcbc6eb4ba1bcab5c28f72308,
   `build/resources.go` names each configuration `sre-<hook>` and each webhook
   `<hook>.managed.openshift.io`, in namespace `openshift-validation-webhook`,
   and ships them to classic clusters by a Hive SelectorSyncSet
   (`ClassicEnabled()`); HyperShift gets them through a package instead. The
   ServiceAccount already `list`s validatingwebhookconfigurations (stale-webhook
   diagnostics), so no new SA read is needed. Rejected candidates: ClusterVersion
   `spec.upstream` (same graph URL as self-managed), AWS resource tags
   `red-hat-managed` (AWS only, OSD on GCP has none), Hive labels (on the hub,
   not the cluster).
3. **Self-managed**: anything else (IPI/UPI on any cloud, bare metal, SNO).
4. **Unknown**: the topology is not External but the webhook list cannot be
   read. The instructions stay first; the one-click is offered as an attempt,
   and a 403 or admission denial falls back to the instructions. When the
   topology itself cannot be read (even with the user's token), the action
   refuses: a hosted cluster cannot be ruled out.

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

## B. Node impact (Machine Config Operator)

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

Source (openshift/machine-config-operator @9dfb6bda9b5f66d9330faf157133cf4887be1aba):
- `pkg/daemon/update.go` `calculatePostConfigChangeActionFromMCDiffs`:
  `constants.KubeletAuthFile` (`/var/lib/kubelet/config.json`, the rendered
  pull secret) is in `filesPostConfigChangeActionNone`; `registries.conf` is
  "reload crio".
- `pkg/apihelpers/apihelpers.go` `defaultClusterPolicies`: KubeletAuthFile
  `None`; registries.conf `Special`.
- `pkg/daemon/drain.go` `isDrainRequired` / `isSafeContainerRegistryConfChanges`:
  a removed registry is unsafe (drain); a new registry whose mirrors are all
  `pull-from-mirror=digest-only` is safe (no drain). openshift/runtime-utils
  @7bdb5b9c `pkg/registries/registries.go` `EditRegistriesConfig` writes IDMS
  mirrors with `sysregistriesv2.MirrorByDigestOnly`.
- `pkg/daemon/drain.go` `drainRequired`: no drain on a single-node topology (SNO).

So, with the default node disruption policies:
- **Pull secret entry added, changed or removed:** written to every node, no
  drain, no reboot, CRI-O not reloaded; the pools roll one node at a time
  (`oc get mcp` UPDATED True).
- **IDMS created** (new source, digest-only mirror): no drain, no reboot; CRI-O
  is reloaded on each node.
- **IDMS deleted:** each node is drained (pods evicted and rescheduled) and
  uncordoned, CRI-O reloaded, no reboot; on SNO no drain.
- A paused MachineConfigPool takes neither until unpaused; an admin's
  NodeDisruptionPolicy can change the actions.

## C. Removal

- Removing the entry from the cluster pull secret: the nodes lose the
  credentials at the next rollout; images already on a node stay and running
  pods keep running; new pulls from quay.io/rhoai (a new nightly, a pod on a
  node that lacks the image) fail with unauthorized.
- Deleting the IDMS: registry.redhat.io/rhoai references are pulled from
  registry.redhat.io again, where nightly digests do not exist; the drain
  (not on SNO) evicts pods, which come back on nodes that may already have the
  image. A cluster-wide object: only the IDMS this setup creates is deleted
  (name `rhoai-quay-mirror`, exactly the one source and mirror, UID and
  resourceVersion preconditions).
