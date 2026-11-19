/**
 * The commands of the one-time cluster setup for people who prefer the CLI
 * ("Do it yourself instead"), on a cluster whose control plane runs on the
 * cluster. They make exactly the objects the one-click setup makes
 * (pkg/cluster/node_setup.go): `oc create`, not `oc apply`, so an
 * ImageDigestMirrorSet of that name that someone else made is not changed.
 * The pull secret's commands come from the backend
 * (pullSecret.nodes.command and removeCommand).
 */
export const MIRROR_NAME = "rhoai-quay-mirror";

export const MIRROR_CREATE_COMMAND = `oc create -f - <<'EOF'
apiVersion: config.openshift.io/v1
kind: ImageDigestMirrorSet
metadata:
  name: ${MIRROR_NAME}
spec:
  imageDigestMirrors:
    - source: registry.redhat.io/rhoai
      mirrors:
        - quay.io/rhoai
EOF`;

export const MIRROR_REMOVE_COMMAND = `oc delete imagedigestmirrorset ${MIRROR_NAME}`;

/** On ROSA HCP the mirror is created through OpenShift Cluster Manager. */
export const ROSA_MIRROR_COMMAND = `rosa create image-mirror --cluster=<your-cluster-name> \\
  --source=registry.redhat.io/rhoai --mirrors=quay.io/rhoai`;
