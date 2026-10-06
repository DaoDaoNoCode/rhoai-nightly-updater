# What the tool changes on your cluster

This is every object the updater creates, patches or deletes, with the conditions. It is derived from the write calls in `pkg/cluster` (`post`, `put`, `patch`, `strategicPatch`, `apply`, `delete`, `deleteWithUID`, `deleteExact`) and from the Makefile. RBAC that allows exactly these calls is in `deploy/template.yaml` ([SECURITY.md](../SECURITY.md#serviceaccount-rbac)).

Conventions:
- **Tool label:** objects the tool creates carry `app.kubernetes.io/managed-by=rhoai-nightly-updater`. Teardowns delete only labelled objects (or the exact objects earlier versions created), with a UID precondition. An object that isn't the tool's is reported as *Not managed by this tool* and left alone.
- **SSA:** server-side apply with field manager `rhoai-nightly-updater`.
- Every action below needs cluster-admin, a confirmation in the UI, and the cluster lock (one operation at a time).

## 1. Installing the updater (`make` targets, run by you)

| Target | Objects |
|---|---|
| `make deploy` | Project `<NAMESPACE>` (if missing); Secret `<APP>-proxy` (cookie secret, created once, never replaced); everything in `deploy/template.yaml`: ServiceAccount, ClusterRole + ClusterRoleBinding `<APP>-<NS>`, Roles/RoleBindings in `<NS>`, `kube-system`, `openshift-marketplace`, Deployment, Service, NetworkPolicy, Route, ServiceMonitor, PrometheusRule; ConsoleLink `<APP>-<NS>`. Then `cleanup-legacy` and `prune-replicasets` (below) |
| `make upgrade` | Same as deploy, minus the project. The image is applied by digest |
| `make rollback TAG=` | The template objects of that commit. Old templates may recreate legacy objects; nothing is cleaned up |
| `cleanup-legacy` | Deletes ClusterRoleBinding/ClusterRole `<APP>` only if bound to this namespace's SA, and ConsoleLink `<APP>` only if it points at this Route (or the old placeholder) |
| `prune-replicasets` | Deletes the app's ReplicaSets that are owned by the Deployment (UID), scaled to 0 and not the current revision |
| `make undeploy` | Deletes the ConsoleLink, ClusterRole and ClusterRoleBinding labelled `app.kubernetes.io/instance=<APP>-<NS>`, the labelled Roles/RoleBindings in `kube-system`, `openshift-marketplace` (and `openshift-ingress`, from old installs), the legacy objects, and project `<NAMESPACE>` |

## 2. The updater's own state (namespace `<NAMESPACE>`)

| Object | When |
|---|---|
| ConfigMap `rhoai-nightly-updater-activity` | create, then update, on every recorded action (audit log) |
| ConfigMap `rhoai-nightly-updater-snapshot` | SSA before each operator operation (deployment snapshot). Key `operator-subscription` records the live Subscription after each operator operation |
| ConfigMap `rhoai-nightly-updater-operation` | SSA/patch when an operation starts and ends (the marker used to report an interrupted operation) |

## 3. One-time cluster setup

| Action | Object | Change |
|---|---|---|
| Pull secret card | Secret `kube-system/additional-pull-secret` | Created if missing; otherwise SSA. Only the `quay.io/rhoai` entry of `.dockerconfigjson` is replaced (equivalent spellings removed); other registries are kept. Unpadded base64 is stored padded |
| — | IDMS | **Never created by the tool.** You create it (`rosa create image-mirror`) |

## 4. Operator lifecycle (Status page)

Common to Update, Re-deploy and Reinstall:
- They refuse, with *Nothing was changed*, when any of these holds: a prerequisite fails, the image or version check fails, a second rhods-operator Subscription exists, an OperatorGroup is unusable, or a Dashboard Dev session is active (unless you chose *Revert Dashboard Dev and continue*, which first runs the [Dashboard Dev revert](#7-dashboard-dev)).
- Namespace `redhat-ods-operator` (SSA) and OperatorGroup `rhods-operator` (SSA, global) are **created only if missing**. With 2+ OperatorGroups, or a non-global one, the operation refuses instead.
- **Verification catalog:** before any change, a temporary CatalogSource `rhoai-catalog-dev-verify-<id>` in `openshift-marketplace` (labelled `managed-by` + `rhoai-nightly-updater/purpose=catalog-verification`) checks the exact image. It is deleted afterwards. Leftovers older than 15 minutes (from a killed pod) are deleted by the next operation.
- **Manual approval:** the tool patches `spec.approved=true` only on the InstallPlan it caused, and only when it installs exactly the confirmed rhods-operator CSV.
- **On failure (restore):**
  - the CSVs this attempt installed are deleted by UID;
  - the previous CatalogSource and Subscription are re-applied, or the catalog is deleted if there was none;
  - a Subscription someone else created meanwhile is left alone.

  A timeout while OLM is still installing keeps the new state.

### Update (to a nightly)
| # | Object (ns) | Change |
|---|---|---|
| 1 | Subscription `rhods-operator` (`redhat-ods-operator`) | delete (the operator keeps running) |
| 2 | CatalogSource `rhoai-catalog-dev` (`openshift-marketplace`) | delete, then SSA with the new image (`grpc`, `securityContextConfig: restricted`) |
| 3 | InstallPlan of the current CSV, then the current CSV | delete |
| 4 | Subscription `rhods-operator` | create once: channel detected from the new catalog. `installPlanApproval`, `config` and other fields are kept; `startingCSV` is dropped |
| 5 | — | wait up to 8 min for the CSV to reach `Succeeded` |

### Re-deploy the same version
Delete the Subscription, delete the CSV, wait for it to go, recreate the Subscription with the same source and channel. With Manual approval `startingCSV` is pinned to the installed CSV. With Automatic approval it refuses if the channel head is no longer the installed CSV.

### Reinstall (stable, nightly or custom FBC image)
| # | Object | Change | Condition |
|---|---|---|---|
| 1 | CatalogSource `rhoai-catalog-dev` | delete | |
| 2 | Subscription `rhods-operator` | delete | |
| 3 | rhods-operator CSV(s) | delete | |
| 4 | Validating/Mutating webhook configurations | delete (uid/resourceVersion precondition) | Only stale ones: every backing Service is NotFound, or has had no ready endpoints for 5 min; **and** the owning CSV (`olm.owner`) or module is gone; **and** no CSV is installing. Platform and unknown-owner configs are never deleted |
| 5 | `components.platform.opendatahub.io` CRs | merge-patch finalizers away (resourceVersion guard) | Only a CR that has been deleting for over 10 min and whose operator Deployment no longer exists |
| 6 | CRDs `datascienceclusters…`, `dscinitializations…` | patch `spec.conversion` to `None` | Only when no RHOAI CSV exists and the conversion Service is missing. OLM normally does this itself |
| 7 | Nightly/custom: CatalogSource `rhoai-catalog-dev` | SSA | |
| 8 | Subscription `rhods-operator` | create (stable: `STABLE_SOURCE`/channel; nightly: `rhoai-catalog-dev`) | |

Downgrades need the confirmation checkbox. The DSC, DSCI and operands are not deleted.

## 5. DataScienceCluster (Status and Components pages)

| Action | Change |
|---|---|
| Preview and create DataScienceCluster | `POST` a DSC (defaults from the installed CSV's `alm-examples`, or `DSC_SAMPLE_REF`). Only when none exists |
| Remove invalid fields / remove extra components / reset to version defaults | Merge-patch of the DSC spec, after a preview. A removal that fails the disable-component preconditions is refused |

## 6. Diagnostics automatic fixes

| Fix | Change | Preconditions (re-checked right before acting) |
|---|---|---|
| `delete-stale-webhooks` | delete webhook configurations (uid/resourceVersion precondition) | the stale rules in §4 step 4; never during an operator install |
| `delete-stale-installplans` | delete InstallPlans in `redhat-ods-operator` | phase `Failed` only. A plan the Subscription references is deleted only while the Subscription is `UpgradePending` |
| `assist-rollout` | strategic-merge-patch `spec.strategy.rollingUpdate.maxUnavailable: 1` on one Deployment in `redhat-ods-operator`/`redhat-ods-applications`; the original value is saved in annotation `rhoai-nightly-updater.opendatahub.io/assist-rollout` | RollingUpdate with effective maxUnavailable 0; new pods Unschedulable only for cpu/memory/pod count; ≥2 replicas and 2 ready pods; the field is not owned by an operator's server-side apply. No pods are deleted |
| `restore-rollout-strategy` | puts the saved maxUnavailable back and removes the annotation | the assist annotation is present |
| `disable-component:<name>` | DSC `spec.components.<name>.managementState: Removed` | only `llamastackoperator`, `feastoperator`, `trustyai`, `ray`, `kueue`, `sparkoperator`, `trainer`, `trainingoperator`; present and not already Removed; no blockers that would leave it stuck in deletion |

Removed fixes (refused if an old page sends them): `recreate-subscription`, `fix-maas-gateway-annotation`, `restart-operator`. Every other finding is guidance only.

## 7. Dashboard Dev

All objects are in `redhat-ods-applications`.

| Action | Object | Change |
|---|---|---|
| Deploy PR / main (installs with `dashboard-operator`, RHOAI 3.5+) | Deployment `dashboard-operator` | merge-patch (uid + resourceVersion): `replicas: 0` and annotations `rhoai-nightly-updater.opendatahub.io/dashboard-dev` (saved replicas, mode, image bindings) and `…/dashboard-dev-last-action` |
| | Dashboard-owned Deployments (the host and installed modules) | strategic-merge-patch of the selected containers' images, pinned by digest, after the operator pods are gone (ownership and resourceVersion re-checked). Components without a build keep their release image. A new deploy replaces the whole session |
| Revert | Deployment `dashboard-operator` | restore the saved replica count (1 if unknown) and remove the session annotation; dashboard-operator then puts the release images back |
| | Dashboard-owned Deployments | remove any `opendatahub.io/managed` annotation |
| Legacy deploy (no `dashboard-operator`, RHOAI ≤3.4) | Deployment `rhods-dashboard` | image patch plus `opendatahub.io/managed: "false"`; the prior value is saved in `rhoai-nightly-updater.opendatahub.io/original-managed` |
| Legacy revert | Deployment `rhods-dashboard` | restore the images and restore or remove `opendatahub.io/managed` |

Revert is safe to repeat and works without a saved session.

## 8. Test resources

### S3 storage: SeaweedFS (namespace `minio`)

The namespace, Service, Secret and bucket keep the names earlier versions used for MinIO, so pipeline servers pointed at `minio-service.minio.svc:9000` keep working with no edits. Setup creates or updates the following, in this order. Updates are guarded by resourceVersion; a foreign object refuses.

| Object | Notes |
|---|---|
| Namespace `minio` | created with the tool label. A namespace from an earlier version gets the label patched on |
| PVC `seaweedfs-pvc` | 20Gi, RWO. A new name: SeaweedFS cannot read MinIO's on-disk format |
| Secret `minio-secret` | keys `minio_root_user` (S3 access key) and `minio_root_password` (S3 secret key and admin UI password), random on first setup, reused on re-run |
| NetworkPolicy `seaweedfs-ingress` | pods `app=seaweedfs`: 8333 (S3) from pods in all namespaces; 23646 (admin UI) only from the router namespaces; nothing else |
| Deployment `seaweedfs` | `SEAWEEDFS_IMAGE` or the pinned `ghcr.io/chrislusf/seaweedfs` 4.48 digest; `weed mini -dir=/data -webdav=false -bucket=pipelines`; env `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`/`WEED_ADMIN_PASSWORD` from the Secret, `WEED_ADMIN_USER=admin`; probes `GET /status` on 8333; 64m/256Mi requests, 500m/1Gi limits; no fixed UID (restricted-v2) |
| Service `minio-service` | applied once SeaweedFS is ready: selector `app=seaweedfs`, port `api` 9000 → 8333 and `ui` 9090 → 23646 |
| Route `minio-ui` | applied once SeaweedFS is ready: edge TLS to the admin UI (port `ui`). There is no S3 API Route |
| Route `minio-api` (legacy) | deleted on setup unless a pipeline server uses its host |
| Bucket `pipelines` | created by SeaweedFS on start and again through the S3 API |

**Migration from MinIO.** When setup finds the tool's MinIO Deployment `minio` (earlier versions), it deploys SeaweedFS next to it as above; MinIO keeps serving until SeaweedFS is ready, and if SeaweedFS cannot start nothing is switched. Once it is ready, setup switches `minio-service` and `minio-ui`, then deletes Deployment `minio` and, once no MinIO pod is left (up to 20 seconds; otherwise it is kept and **Repair** removes it later), NetworkPolicy `minio-ingress` (labelled, by UID). Test resources reports the storage as Running only when SeaweedFS is ready and `minio-service`/`minio-ui` point at it; otherwise it is Incomplete and **Repair** re-runs setup. **PVC `minio-pvc` is kept** and shown on the Test resources page with its size. Objects stored in MinIO are **not copied**: artifacts of earlier pipeline runs return 404. Re-running setup is safe at any point. Rollback: RUNBOOK §10.

**Teardown** deletes, in order: Deployments `seaweedfs` and `minio`, NetworkPolicies `seaweedfs-ingress` and `minio-ingress`, Routes `minio-ui`/`minio-api`, Service, Secret, and PVCs `seaweedfs-pvc` and `minio-pvc` (**the data is lost**). Each is deleted only if labelled, by UID. The **namespace is kept**; delete it with `oc delete project minio`. Teardown refuses in these cases:
- a pipeline server still uses the storage;
- the storage's Route/Service can't be read while pipeline servers exist;
- a CRD conversion webhook is down (namespace deletion would hang).

### Pipeline server (a project you choose)
| Action | Objects |
|---|---|
| Setup | Namespace `<project>` only if missing (labels `opendatahub.io/dashboard=true`, `modelmesh-enabled=false`); Secret `nightly-dspa-s3` (the S3 credentials from `minio-secret`, labelled); DSPA `nightly-dspa` → `minio-service.minio.svc:9000`, bucket `pipelines`. It refuses if the project already has a DSPA |
| Teardown | DSPA `nightly-dspa`, or legacy `dspa` if this tool created it, then its Secret (legacy `dashboard-dspa-secret`), by UID. The project is kept |

### MLflow (cluster-scoped CR `mlflow`, workloads in `redhat-ods-applications`)
| Action | Change |
|---|---|
| Setup | create `mlflow` (labelled, no `spec.image`: the operator default RHOAI image) |
| Teardown | delete `mlflow` by UID, only if the tool created it. The operator's `mlflow-pvc` (tracking data) goes with it |
| Deploy MLflow PR | merge-patch (resourceVersion) `spec.image.image` to the PR image digest; the first deploy saves the previous image in annotation `rhoai-nightly-updater/original-image` |
| Revert | restore the saved image, or remove `spec.image.image` |

## 9. What the tool never does

- Delete a namespace (other than `make undeploy` deleting its own).
- Read or list Secrets cluster-wide.
- Create an IDMS, or touch `openshift-config/pull-secret`.
- Delete webhook configurations of a live CSV or operand.
- Pause any operator other than `dashboard-operator`.
- Set `opendatahub.io/managed=false` where `dashboard-operator` exists (it would freeze the resource across upgrades).
- Delete DSC, DSCI or operator CRDs.
