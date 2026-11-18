# What the tool changes on your cluster

This is every object the updater creates, patches or deletes, with the conditions. It is derived from the write calls in `pkg/cluster` (`post`, `put`, `patch`, `strategicPatch`, `apply`, `delete`, `deleteWithUID`, `deleteExact`), from the Makefile and `scripts/install.sh`, and from the Helm chart `deploy/chart`. RBAC that allows exactly these calls is in `deploy/template.yaml` ([SECURITY.md](../SECURITY.md#serviceaccount-rbac)).

**The gitops profile** (`PROFILE=gitops`, the Helm chart's default; [GITOPS.md §11](GITOPS.md#11-profiles)) changes only what [Update when Argo CD manages the operator](#update-when-argo-cd-manages-the-operator) lists, plus the updater's own state (§2): its old InstallPlan and CSV, Subscription `rhods-operator` (deleted and at once recreated), the InstallPlan it caused under Manual approval (approved) and, after a failure, the restore of the previous Subscription. It **never writes a CatalogSource**: there is no verification catalog; it reads the build from the catalog Git manages. It refuses unless Argo CD or an ACM policy manages CatalogSource `rhoai-catalog-dev` (GITOPS.md §11), reads no Secret and contacts no registry, refuses before any change when the Subscription it would create or restore breaks the admission policy, never creates the operator namespace or OperatorGroup, never ends a Dashboard Dev session, runs only while its admission policy is in place, and §3 and §5 to §8 are off. Its RBAC is `deploy/rbac-gitops.yaml`; a ValidatingAdmissionPolicy lets its ServiceAccount write only Subscription `rhods-operator` from `rhoai-catalog-dev` with at most allowlisted `spec.config.env`, approve or delete only rhods-operator InstallPlans, delete only rhods-operator CSVs, and no CatalogSource or OperatorGroup.

Conventions:
- **Tool label:** objects the tool creates carry `app.kubernetes.io/managed-by=rhoai-nightly-updater`. Teardowns delete only labelled objects (or the exact objects earlier versions created), with a UID precondition. An object that isn't the tool's is reported as *Not managed by this tool* and left alone.
- **SSA:** server-side apply with field manager `rhoai-nightly-updater`.
- Every action below needs cluster-admin, a confirmation in the UI, and the cluster lock (one operation at a time).

## 1. Installing the updater (`install.sh`, the `make` targets that run it, or Argo CD with the Helm chart)

| Target | Objects |
|---|---|
| `make deploy`, `install.sh deploy` (or `install.sh` on a cluster without it) | Project `<NAMESPACE>` (if missing); Secret `<APP>-proxy` (cookie secret, created once, never replaced); everything in `deploy/template.yaml`: ServiceAccount, ClusterRole + ClusterRoleBinding `<APP>-<NS>`, Roles/RoleBindings in `<NS>`, `kube-system`, `openshift-marketplace`, Deployment, Service, NetworkPolicy, Route, ServiceMonitor, PrometheusRule; ConsoleLink `<APP>-<NS>`. Then `cleanup-legacy` and `prune-replicasets` (below) |
| `make upgrade`, `install.sh upgrade` (or `install.sh` on an installed cluster) | Same as deploy, minus the project. The image is applied by digest |
| `make rollback TAG=` | The template objects of that commit. Old templates may recreate legacy objects; nothing is cleaned up |
| Legacy cleanup (after deploy and upgrade) | Deletes ClusterRoleBinding/ClusterRole `<APP>` only if bound to this namespace's SA, and ConsoleLink `<APP>` only if it points at this Route (or the old placeholder) |
| ReplicaSet pruning (after deploy and upgrade) | Deletes the app's ReplicaSets that are owned by the Deployment (UID), scaled to 0 and not the current revision |
| `make undeploy`, `install.sh uninstall` | Deletes the ConsoleLink, ClusterRole and ClusterRoleBinding labelled `app.kubernetes.io/instance=<APP>-<NS>`, the labelled Roles/RoleBindings in `kube-system`, `openshift-marketplace` (and `openshift-ingress`, from old installs), the legacy objects, and project `<NAMESPACE>` |
| Argo CD sync of the Helm chart `deploy/chart` ([GITOPS.md](GITOPS.md)) | With `profile: gitops` (the default): everything in `deploy/template.yaml` except its ClusterRole, ClusterRoleBinding, Roles and RoleBindings, and instead the objects of `deploy/rbac-gitops.yaml`: ClusterRole + ClusterRoleBinding `<APP>-<NS>` (reads only), Roles/RoleBindings in `<NS>`, `kube-system`, `openshift-marketplace` and `redhat-ods-operator`, ValidatingAdmissionPolicy + Binding `<APP>-<NS>-gitops` (sync wave -1). With `profile: full`: everything in `deploy/template.yaml`, as above (the namespace comes from the Application's `CreateNamespace=true`). Sync hooks, before those objects: ServiceAccount, Role and RoleBinding `<APP>-session-secret` (Role: `get` Secret `<APP>-proxy`, `create` Secrets) and Job `<APP>-session-secret`, which creates Secret `<APP>-proxy` only if missing and never replaces it. PostSync hooks, once the app is healthy: ServiceAccount, Role and RoleBinding `<APP>-consolelink` (Role: `get` Route `<APP>`), ClusterRole and ClusterRoleBinding `<APP>-<NS>-consolelink` (`get`/`patch` ConsoleLink `<APP>-<NS>`, `create` ConsoleLinks; instance label) and Job `<APP>-consolelink`, which server-side-applies ConsoleLink `<APP>-<NS>` (field manager `rhoai-nightly-updater-gitops`), the same object `install.sh` adds. Argo CD deletes the hook objects after a successful sync and keeps them after a failed one. No legacy cleanup or ReplicaSet pruning |
| Deleting that Argo CD Application | Deletes the template's objects. Leaves the namespace (with Secret `<APP>-proxy` and the app's ConfigMaps), ConsoleLink `<APP>-<NS>` and the hook objects of a failed run; `install.sh uninstall` removes them |

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

Common to Update, Re-deploy and Install a different build:
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

### Update when Argo CD manages the operator
When CatalogSource `rhoai-catalog-dev` or Subscription `rhods-operator` has Argo CD's tracking annotation (`argocd.argoproj.io/tracking-id`), Git is the source of truth: Argo CD puts back what differs from Git and recreates them as soon as they are deleted. Update then installs the build the catalog serves, writes nothing Argo CD manages, and deletes nothing it manages for longer than one request (see [RUNBOOK §13](../RUNBOOK.md#13-clusters-managed-by-argo-cd)):

| # | Object (ns) | Change |
|---|---|---|
| — | — | refuses, with *Nothing was changed*, when the image is not the catalog's current `spec.image` (Git picks the build), the catalog does not exist, the managed Subscription's source is not `rhoai-catalog-dev`, the catalog's image has no channel of the Subscription's name, or nothing is installed yet and OLM is still working on the Subscription's InstallPlan (the first install Argo CD started) |
| 1 | CatalogSource `rhoai-catalog-dev` (`openshift-marketplace`) | nothing: left as Git sets it |
| 2 | — | wait until a registry pod runs the image and is Ready, the catalog is READY, and its PackageManifest lists the verified build (head CSV, operator image and creation time); OLM may still be replacing the pod after a change in Git. Then re-read the catalog: if the image changed meanwhile (Argo CD syncing a new one from Git), or Git pins `startingCSV` to another build, Update stops before step 3 |
| 3 | InstallPlan of the current CSV, then the current CSV | delete (the CSV by UID) |
| 4 | Subscription `rhods-operator` | re-read; if Argo CD manages it and its source, channel or `startingCSV` pin changed meanwhile, stop (`conflict`): the restore below recreates it from Git's current spec. Else delete (by the UID just read), then at once SSA from the live spec with the **same channel** and settings and `startingCSV` set to the verified head CSV (a pin Git sets must already be that head; an unmanaged Subscription takes the build's channel, as in any Update), so OLM installs exactly that CSV or reports `ResolutionFailed` when the catalog does not have it, plus its annotations (Argo CD's tracking and last-applied among them) and labels (a self-heal sync that follows finds it unchanged) |
| 5 | — | wait up to 8 min for the CSV to reach `Succeeded`, then check that its `containerImage` and `createdAt` annotations are the verified build's (`install_mismatch` otherwise, also when it has none; the running operator is kept). When the CSV cannot be read, the result says the build was not confirmed |

The restore after a failure deletes only the Subscription Update created (by UID; one that replaced it is left alone and reported) and re-applies the previous Subscription with those annotations and `startingCSV` set to the CSV Update removed: OLM installs that CSV from Git's catalog again, or nothing when the catalog no longer has it (never an older build). Re-deploy and Install a different build refuse while Argo CD manages the catalog or the Subscription, and so does *Create the DataScienceCluster* (Git creates it).

### Re-deploy the same version
Delete the Subscription, delete the CSV, wait for it to go, recreate the Subscription with the same source and channel. With Manual approval `startingCSV` is pinned to the installed CSV. With Automatic approval it refuses if the channel head is no longer the installed CSV.

### Install a different build (GA release, nightly or custom FBC image)
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
| Remove invalid fields / remove extra components / reset to version defaults | Merge-patch of the DSC spec, after a preview. A removal that fails the disable-component preconditions is refused, and so is any repair of a DSC Argo CD manages |

## 6. Diagnostics automatic fixes

| Fix | Change | Preconditions (re-checked right before acting) |
|---|---|---|
| `delete-stale-webhooks` | delete webhook configurations (uid/resourceVersion precondition) | the stale rules in §4 step 4; never during an operator install |
| `delete-stale-installplans` | delete InstallPlans in `redhat-ods-operator` | phase `Failed` only. A plan the Subscription references is deleted only while the Subscription is `UpgradePending` |
| `assist-rollout` | strategic-merge-patch `spec.strategy.rollingUpdate.maxUnavailable: 1` on one Deployment in `redhat-ods-operator`/`redhat-ods-applications`; the original value is saved in annotation `rhoai-nightly-updater.opendatahub.io/assist-rollout` | RollingUpdate with effective maxUnavailable 0; new pods Unschedulable only for cpu/memory/pod count; ≥2 replicas and 2 ready pods; the field is not owned by an operator's server-side apply. No pods are deleted |
| `restore-rollout-strategy` | puts the saved maxUnavailable back and removes the annotation | the assist annotation is present |
| `disable-component:<name>` | DSC `spec.components.<name>.managementState: Removed` | only `llamastackoperator`, `feastoperator`, `trustyai`, `ray`, `kueue`, `sparkoperator`, `trainer`, `trainingoperator`; present and not already Removed; no blockers that would leave it stuck in deletion; the DSC is not managed by Argo CD |
| `restart-module-operator:<module>` | merge-patch (uid + resourceVersion) of the module operator Deployment's pod-template annotation `kubectl.kubernetes.io/restartedAt` (what `oc rollout restart` does); Kubernetes rolls the pods | the module still reports a prerequisite that is now installed (and whose singleton operand was read and exists), or scans at least 5 minutes apart keep seeing its module CR lag the same generation; the Deployment is the known operator of that module or the only Platform-owned one for it; ≥1 available pod; not the RHOAI operator (OLM owns it); no `failurePolicy: Fail` webhook served by its pods would go unserved (Recreate, or maxUnavailable ≥ available pods) |

Removed fixes (refused if an old page sends them): `recreate-subscription`, `fix-maas-gateway-annotation`, `restart-operator`. Every other finding is guidance only. Prerequisite operators (JobSet, cert-manager, LeaderWorkerSet, …) are never installed by the tool: Diagnostics prints the commands.

## 7. Dashboard Dev

All objects are in `redhat-ods-applications`.

| Action | Object | Change |
|---|---|---|
| Deploy PR / main (installs with `dashboard-operator`, RHOAI 3.5+) | Deployment `dashboard-operator` | merge-patch (uid + resourceVersion): `replicas: 0` and annotations `rhoai-nightly-updater.opendatahub.io/dashboard-dev` (saved replicas, mode, image bindings) and `…/dashboard-dev-last-action` |
| | Dashboard-owned Deployments (the host and installed modules) | strategic-merge-patch of the selected containers' images, pinned by digest, after the operator pods are gone (ownership and resourceVersion re-checked). A main build is written under its commit tag when the registry publishes one for the same digest: `quay.io/opendatahub/<repo>:<40-hex commit>@sha256:...` for the RHOAI (Konflux) build, `quay.io/opendatahub/<repo>:main-<7-hex commit>@sha256:...` for the ODH (OpenShift CI) build; otherwise the floating tag (`odh-stable`, `main`) stays in the reference. PR builds keep their `odh-pr-<N>`/`pr-<N>` tag. Components without a build keep their release image. A new deploy replaces the whole session |
| Revert | Deployment `dashboard-operator` | restore the saved replica count (1 if unknown) and remove the session annotation; dashboard-operator then puts the release images back |
| | Dashboard-owned Deployments | remove any `opendatahub.io/managed` annotation |
| Legacy deploy (no `dashboard-operator`, RHOAI ≤3.4) | Deployment `rhods-dashboard` | image patch plus `opendatahub.io/managed: "false"`; the prior value is saved in `rhoai-nightly-updater.opendatahub.io/original-managed` |
| Legacy revert | Deployment `rhods-dashboard` | restore the images and restore or remove `opendatahub.io/managed` |

Revert is safe to repeat and works without a saved session.

## 8. Environment page

### S3 storage: SeaweedFS (namespace `seaweedfs`)

Setup creates or updates the following, in this order. Updates are guarded by resourceVersion; a foreign object refuses. Every create and update is first sent as a server-side dry run (`?dryRun=All`), and none is made unless all pass. Before updating an object it created, setup moves its own field ownership from client-side to server-side apply in `metadata.managedFields` (as `kubectl apply --server-side` does), so a field it no longer sets is removed; other managers' entries are not touched. Setup refuses while storage from an earlier version is in namespace `minio`: Migrate storage (below) sets the new storage up instead.

| Object | Notes |
|---|---|
| Namespace `seaweedfs` | created with the tool label; a namespace of that name the tool did not create refuses |
| PVC `seaweedfs-data` | 20Gi, RWO |
| Secret `seaweedfs-s3-credentials` | keys `AWS_ACCESS_KEY_ID` (S3 access key) and `AWS_SECRET_ACCESS_KEY` (S3 secret key), random on first setup (or `SEAWEEDFS_ACCESS_KEY_ID`/`SEAWEEDFS_SECRET_ACCESS_KEY`), reused on re-run; Migrate storage sets the old storage's keys. Key `WEED_ADMIN_PASSWORD` (admin UI password): random, its own, only in this Secret; setup or Repair adds it to storage an earlier version set up |
| NetworkPolicy `seaweedfs-ingress` | pods `app=seaweedfs`: 8333 (S3) from pods in all namespaces; 12535 (admin UI) only from the router namespaces; nothing else |
| Deployment `seaweedfs` | `SEAWEEDFS_IMAGE` or the pinned `ghcr.io/chrislusf/seaweedfs` 4.54 digest; `weed mini -dir=/data -webdav=false -bucket=pipelines -admin.port=12535 -admin.port.legacy=0`; env `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`/`WEED_ADMIN_PASSWORD` from the Secret's keys of the same names, `WEED_ADMIN_USER=admin`; probes `GET /status` on 8333; 64m/256Mi requests, 500m/1Gi limits; no fixed UID (restricted-v2) |
| Service `seaweedfs-s3` | applied once SeaweedFS is ready: selector `app=seaweedfs`, ports `s3` 8333 and `ui` 12535. Pipeline servers use `seaweedfs-s3.seaweedfs.svc:8333` |
| Route `seaweedfs-admin` | applied once SeaweedFS is ready: edge TLS to the admin UI (port `ui`). There is no S3 API Route |
| Bucket `pipelines` | created by SeaweedFS on start and again through the S3 API |

**Teardown** deletes, in order: Deployment `seaweedfs`, NetworkPolicy `seaweedfs-ingress`, Route `seaweedfs-admin`, Service `seaweedfs-s3`, Secret `seaweedfs-s3-credentials` and PVC `seaweedfs-data` (**the data is lost**), and, when storage from an earlier version is left in namespace `minio`, the tool's objects there too (below). Each is deleted only if the tool created it, by UID. The **namespaces are kept**, unless you tick **Also delete namespace seaweedfs (created by this tool)**: offered only when `seaweedfs` carries the tool label and the tool wrote it, unticked, it deletes the namespace with everything in it once the storage's objects are gone (UID precondition). Namespace `minio` is deleted only by Delete old storage, the same way. **Clean up** runs these same deletions as one operation (`POST /api/resources/cleanup/stream`): the pipeline servers, then the storage, then MLflow. Otherwise delete them with `oc delete project`. Teardown refuses in these cases:
- a pipeline server still uses either storage;
- a storage's Route/Service can't be read while pipeline servers exist;
- a CRD conversion webhook is down (namespace deletion would hang).

### S3 storage of an earlier version (namespace `minio`)

Versions before 3.0.0 set up the storage in namespace `minio`: MinIO (Deployment `minio`, PVC `minio-pvc`, NetworkPolicy `minio-ingress`, Route `minio-api`), later SeaweedFS behind the same names (Deployment `seaweedfs`, PVC `seaweedfs-pvc`, NetworkPolicy `seaweedfs-ingress`), with Secret `minio-secret` (keys `minio_root_user`, `minio_root_password`), Service `minio-service` (port 9000) and Route `minio-ui`. 3.0.0 creates nothing there. It recognises these objects by the tool label or, for an earlier version's unlabelled object, by the tool's field manager owning its spec or data in `metadata.managedFields`, reads the keys and the address, and deletes them only in Migrate storage's last step or in teardown.

**Migrate storage** (streamed; each run does only what is missing, so it resumes after any step):

| Step | Changes |
|---|---|
| 1. New storage | the setup above, with the keys of `minio-secret`. A storage in `seaweedfs` with other keys stops the run |
| 2. Copy | Job `seaweedfs-migrate` in `seaweedfs`: rclone 1.76.0 (`RCLONE_IMAGE` or the pinned `docker.io/rclone/rclone` digest), `copy` (never deletes; while no pipeline server uses the new storage it copies again every object that is not identical, once one does it adds `--ignore-existing` and never overwrites there), `check --one-way --download` (every object of the old storage must be in the new one, byte for byte; extra objects in the new storage are reported, not a failure) and `size --json` on both sides. Keys only through `secretKeyRef`; restricted security context, no service account token, 100m/128Mi requests, 500m/512Mi limits, `backoffLimit: 0`, `activeDeadlineSeconds: 3600`, `ttlSecondsAfterFinished: 86400`. The result is the pod's termination message; the Job is deleted by UID once it is read. Any difference stops the run here |
| 3. Pipeline servers | for each DSPA this tool created on `minio-service.minio.svc:9000` (labelled, or a `dspa` an earlier version created: its name, address and secret match and the tool's field managers own its object storage host and secret name, which tells it apart from a `dspa` odh-dashboard created): Secret `nightly-dspa-s3` with the same keys, then a merge patch (resourceVersion) of `spec.objectStorage.externalStorage.host` to `seaweedfs-s3.seaweedfs.svc:8333`, its secret to `nightly-dspa-s3`, and the tool label. The run waits up to 2 minutes for `ObjectStoreAvailable=True` at the new generation; otherwise it stops. Other DSPAs are listed, never changed |

**Remove the old storage** (Migrate storage's last part, confirmed separately): refused while any pipeline server uses the old storage. It copies and checks once more with a new copy Job (one an earlier run left running is stopped first; the wait may use the rest of the operation's 15 minutes but 2, and a copy that does not finish is stopped and nothing is deleted), then deletes by UID the tool's objects in `minio` (the list above, `minio-pvc` included: MinIO's files from before SeaweedFS, which no running server reads and which cannot be copied) and the `dashboard-dspa-secret` of moved `dspa` servers, in this order, stopping at the first failure: the `dashboard-dspa-secret`s, Routes `minio-ui` and `minio-api`, the PVCs (a PVC a pod uses is only marked for deletion until the pod is gone), Secret `minio-secret`, Service `minio-service`; then the server's Deployments; then, once the PVCs are gone, the NetworkPolicies (deleting them while the server runs would open all its ports). Run again after a stop, it continues: while the server, its Service and its Secret are there it copies and checks again first; once the old storage cannot be copied from and every data PVC left is being deleted, the copy was checked before the first PVC was marked, so it finishes without a copy. A data PVC that is not being deleted while the old storage cannot be copied from stops it, and nothing is deleted. Namespace `minio` is deleted only when you tick it (unticked by default), which deletes everything in it, including what the tool did not create; the choice is offered only when the namespace carries the tool label and the tool's managers wrote it, and the dialog lists the Deployments and PVCs there that the tool did not create.

### Pipeline server (a project you choose)
| Action | Objects |
|---|---|
| Setup | Namespace `<project>` only if missing (labels `opendatahub.io/dashboard=true`, `modelmesh-enabled=false`); Secret `nightly-dspa-s3` (the S3 credentials from `seaweedfs-s3-credentials`, labelled); DSPA `nightly-dspa` → `seaweedfs-s3.seaweedfs.svc:8333` (http, region `us-east-1`), bucket `pipelines`. It refuses if the project already has a DSPA |
| Teardown | DSPA `nightly-dspa`, or legacy `dspa` if this tool created it, then its Secret (legacy `dashboard-dspa-secret`), by UID. The project is kept |

### MLflow (cluster-scoped CR `mlflow`, workloads in `redhat-ods-applications`)
| Action | Change |
|---|---|
| Setup | create `mlflow` (labelled, no `spec.image`: the operator default RHOAI image) |
| Teardown | delete `mlflow` by UID, only if the tool created it. The operator's `mlflow-pvc` (tracking data) goes with it |
| Deploy MLflow PR | merge-patch (resourceVersion) `spec.image.image` to the PR image digest; the first deploy saves the previous image in annotation `rhoai-nightly-updater/original-image` |
| Revert | restore the saved image, or remove `spec.image.image` |

### Models: inference gateway (cluster) and a CPU model (a project you choose)
One streamed operation, `POST /api/resources/models/stream`. Setup reads every object it writes and dry-runs every write before the first one; a refusal changes nothing. Every object carries `app.kubernetes.io/managed-by=rhoai-nightly-updater` and is written with field manager `rhoai-nightly-updater`; an object of the same name without both is someone else's and is never changed or deleted.

| Action | Objects |
|---|---|
| Set up the inference gateway | GatewayClass `openshift-ai-inference` (`controllerName: openshift.io/gateway-controller/v1`; a class of that name someone else made is used if it is OpenShift's controller's). ConfigMap `openshift-ingress/openshift-ai-inference-options` (`data.service`: the Service annotation `service.beta.openshift.io/serving-cert-secret-name: openshift-ai-inference-service-tls`). Gateway `openshift-ingress/openshift-ai-inference` (label `istio.io/rev: openshift-gateway`, `spec.infrastructure.parametersRef` to that ConfigMap) with two HTTPS 443 listeners, `allowedRoutes: All`: `https-external`, hostname `inference.<ingresses.config/cluster spec.domain>`, certificate = the Secret IngressController `openshift-ingress-operator/default` names in `spec.defaultCertificate` (else `router-certs-default`), only referenced, never read; and `https`, hostname `openshift-ai-inference-openshift-ai-inference.openshift-ingress.svc.cluster.local` (the Gateway's Service), certificate `openshift-ai-inference-service-tls` (issued by service-ca). Then it waits up to 2 minutes for `Programmed` and an address, and up to 2 minutes for the external hostname's DNSRecord to be published. **Made by OpenShift, not the tool:** the gateway's Deployment and LoadBalancer Service (a cloud load balancer, billed while it exists), a DNSRecord per listener (the `.svc` one stays `UnmanagedDNS`: expected), the serving-cert Secret; and by odh-model-controller, once a model uses the gateway, AuthPolicy `openshift-ai-inference-authn` (TokenReview, then `get` on the model) and EnvoyFilter `openshift-ai-inference-authn-ssl`. A Gateway `openshift-ai-inference` someone else made (RHOAI's chart, an admin) is used as it is |
| Set up a model | LLMInferenceService `<project>/qwen25-05b-instruct` (`serving.kserve.io/v1alpha2`): labels `opendatahub.io/dashboard=true`, `opendatahub.io/genai-asset=true`; annotations `openshift.io/display-name: Qwen2.5 0.5B Instruct (CPU)`, `opendatahub.io/model-type: generative`, `opendatahub.io/genai-use-case: chat`; `baseRefs: [<LLM_INFERENCE_SERVICE_CONFIG_PREFIX>config-llm-template-cpu]` (prefix read from Deployment `llmisvc-controller-manager` in the applications namespace); `model: {uri: hf://Qwen/Qwen2.5-0.5B-Instruct, name: qwen25-05b-instruct}`; `replicas: 1`; `router: {route: {}, gateway: {}}` (the inference gateway, no scheduler); container `main` with `OMP_NUM_THREADS=4`, `VLLM_CPU_KVCACHE_SPACE=2` (the preset reserves 4 GiB; 2 GiB still holds over 100k tokens for this model), `VLLM_ADDITIONAL_ARGS=--max-model-len 4096`, requests 4 CPU / 6Gi, limits 4 CPU / 8Gi, annotations `opendatahub.io/hardware-profile-name`, `-namespace` and `-resource-version` naming the first enabled CPU-only hardware profile without scheduling rules (in the applications namespace) whose ranges hold those requests and limits, as the dashboard's deploy wizard records one (none when no profile fits), and `image` only when it is not the preset's (then annotations `rhoai-nightly-updater/runtime-image-source: released` or `override` and `rhoai-nightly-updater/runtime-image-version`). Then it waits up to 10 minutes for `Ready`. The project must exist and be a data science project; it is never created. **Made by KServe:** Deployment `qwen25-05b-instruct-kserve`, Service `qwen25-05b-instruct-kserve-workload-svc`, HTTPRoute `qwen25-05b-instruct-kserve-route`, Secret `qwen25-05b-instruct-kserve-self-signed-certs`. **No** ServiceAccount, Role, RoleBinding or token is created |
| Use the released image | the same LLMInferenceService, its `main` image set to the newest released `registry.redhat.io/rhaii/vllm-cpu-rhel9@<digest>` (the highest version tag in Red Hat's public container catalog) |
| Delete the model | LLMInferenceService `qwen25-05b-instruct` in the project, by UID, then a wait (up to 2 minutes) until KServe's finalizer has removed its Deployment, Service and route. The project is kept |
| Delete the inference gateway | refused while any LLMInferenceService in any namespace routes through it (`spec.router.gateway {}` or a ref to it) or the Gateway reports attached routes; then Gateway, ConfigMap and GatewayClass, those the tool created, by UID. OpenShift removes the load balancer and the ingress operator the DNS records |

Nothing else is touched: other gateways (`maas-default-gateway`, `data-science-gateway`, MCP gateways), RHOAI's presets, `inferenceservice-config`, and models or gateways someone else made.

### Playground: the dashboard's Gen AI playground (a project you choose)
One streamed operation, `POST /api/resources/playground/stream`. The tool writes no playground object itself: it calls the installed RHOAI dashboard's own gen-ai API on the dashboard's address (`https://rh-ai.<apps domain>`, from the Dashboard component's `status.url`) with the updater's ServiceAccount token, and the dashboard creates or deletes the playground exactly as a click in its Gen AI studio does. What the dashboard creates depends on the RHOAI release; on 3.6 it is the list below.

| Action | Calls and objects |
|---|---|
| Set up | Checks: the project is a data science project; DataScienceCluster component `ogx` (or `llamastackoperator`) Managed and its `<component>Ready` condition not False (not reported is only a note); Deployment `gen-ai-ui` in the applications namespace available; `GET /gen-ai/api/v1/lsd/status?namespace=<project>` (an existing playground the tool did not request stops the setup); `GET .../aaa/models` and `GET .../maas/models` (404 = no MaaS): every model deployed for Gen AI in the project ready, at least one ready chat model. Then the tool records the request (below) and calls `POST .../lsd/install?namespace=<project>` with `{"models":[{"model_name","model_source_type":"namespace|maas","model_type":"llm"}],"enable_guardrails":false,"enable_tracing":false,"vector_stores":[]}`; a 409 (the previous playground's database is still being removed) is retried every 10 s for up to 3 minutes. Then it polls `lsd/status` every 5 s for up to 6 minutes until the phase is `Ready`. **Made by the dashboard (RHOAI 3.6):** OGXServer `lsd-genai-playground` (label `opendatahub.io/dashboard=true`); ConfigMaps `llama-stack-config`, `ogx-office-mime-types`, `ogx-router-ca-bundle` (and `ogx-trusted-ca-bundle` when the project has no `odh-trusted-ca-bundle`), owned by the OGXServer; the label `ogx.io/watch=true` on `odh-trusted-ca-bundle`; and, with the dashboard's own ServiceAccount, the pgvector database: Secret `genai-pgvector-credentials`, ConfigMap `genai-pgvector-init`, PVC `genai-pgvector-storage` (5Gi), Deployment, Service and NetworkPolicy `genai-pgvector`. **Made by the OGX operator:** the server's Deployment, Service, ServiceAccount, RoleBinding, NetworkPolicy, ConfigMap `lsd-genai-playground-ca-bundle` |
| Delete | Only where the tool recorded its own request: `DELETE .../lsd/delete?namespace=<project>` with `{"name":"<server name lsd/status reports>"}`, then `lsd/status` every 5 s for up to 3 minutes until it reports none, then the record is cleared. The dashboard deletes the OGXServer (its ConfigMaps and the operator's objects follow by ownership) and the pgvector objects, with their data. The `ogx.io/watch` label on `odh-trusted-ca-bundle` stays (the dashboard never removes it) |
| Record | In the tool's own namespace, ConfigMap `rhoai-nightly-updater-snapshot`, data key `playgrounds`: `{"<project>":{"requestedAt","by"}}`, written by server-side apply under field manager `rhoai-nightly-updater-playgrounds` |

A playground the tool did not request (made in the dashboard) is shown as **Not managed by this tool** and never deleted. ConfigMaps an earlier playground left without its server make the dashboard's install answer 400 "already exists": the tool explains which and how to remove them, and does not delete them.

### MaaS: Models as a Service (cluster) and this tool's published model

One streamed operation, `POST /api/resources/maas/stream` (`setup`, `teardown`, `publish`, `unpublish`), in the order of the MaaS install docs (`opendatahub-io/models-as-a-service` `docs/content/install`, and `scripts/setup-gateway.sh`, `setup-database.sh`, `setup-authorino-tls.sh`). The infrastructure namespace is the default tenant's `status.infraNamespace`, else derived from the applications namespace as the scripts do (`redhat-ods-applications` → `redhat-ai-gateway-infra`, `opendatahub` → `odh-ai-gateway-infra`). Every object below carries `app.kubernetes.io/managed-by=rhoai-nightly-updater`, is written with field manager `rhoai-nightly-updater` and deleted with a UID precondition; one of the same name the tool did not create is used as it is (or refused) and never changed or deleted.

| Step | What the tool does |
|---|---|
| Checks (nothing written on a refusal) | DataScienceCluster `aigateway` Managed; a Kuadrant CR exists (Connectivity Link; never created or changed); the infrastructure namespace exists; CRD group `postgresql.cnpg.io/v1` served (unless a `maas-db-config` someone else made exists); then a server-side dry run of every write. The Connectivity Link CSV version is read for a warning below 1.5 |
| 1. Database (`maas-setup.md` "Database Setup") | `postgresql.cnpg.io/v1` Cluster `<infra>/maas-db`: `instances: 1`, `storage.size: 1Gi`, `bootstrap.initdb: {database: maas, owner: maas}`; waits up to 5 minutes for a ready instance. Then Secret `<infra>/maas-db-config` (labels also `app=maas-api`), key `DB_CONNECTION_URL` = `postgresql://<user>:<password>@maas-db-rw.<infra>.svc.cluster.local:5432/maas?sslmode=require`, built from CloudNativePG's Secret `maas-db-app` (read by name; never logged or returned) |
| 2. Gateway (`setup-gateway.sh`, loadbalancer mode) | GatewayClass `openshift-default` (`controllerName: openshift.io/gateway-controller/v1`) only when the cluster has none; never changed or deleted. Gateway `openshift-ingress/maas-default-gateway`: labels `app.kubernetes.io/name=maas`, `instance=maas-default-gateway`, `component=gateway`, `opendatahub.io/managed=false`; annotations `opendatahub.io/managed: "false"`, `security.opendatahub.io/authorino-tls-bootstrap: "true"`; one listener `https`, port 443, hostname `maas.<apps domain>`, TLS Terminate with the default ingress certificate (the IngressController's `spec.defaultCertificate`, else `router-certs-default`; only named), `allowedRoutes` Selector `kubernetes.io/metadata.name In [<infra>, models-as-a-service, <published projects>]` (the script's `ALLOWED_ROUTE_NAMESPACES`). Waits up to 2 minutes for Programmed, then for the DNS record. Plus, beyond the docs: ConfigMap `openshift-ingress/maas-gateway-options` (`data.service` asks service-ca for Secret `maas-gw-service-tls`; the Gateway points at it with `infrastructure.parametersRef`) and a second listener `https-internal` on `maas-default-gateway-openshift-default.openshift-ingress.svc.cluster.local`, as RHOAI's inference gateway has: in-cluster callers such as the dashboard's playground proxy use that name for a published model. Removal deletes the ConfigMap after the Gateway |
| 3. Authorino TLS (`setup-authorino-tls.sh`) | Read only: the Service `authorino-authorino-authorization` annotation, the Authorino CR's `spec.listener.tls.enabled`, the Deployment's `SSL_CERT_FILE`, in the Kuadrant CR's namespace. When one is missing, the tab shows the script's commands for a cluster-admin |
| 4. DataScienceCluster | Records the current `aigateway.modelsAsAService.managementState` (snapshot ConfigMap, data key `maas`, field manager `rhoai-nightly-updater-maas-record`), then sets it to `Managed` |
| 5. Validation | Waits up to 5 minutes for Deployment `<infra>/maas-api` Available, AITenant `ai-tenants/models-as-a-service` Ready and the AuthPolicy on the gateway (found by its target) Enforced; still starting at the bound is not a failure |
| Publish (`model-setup.md` "Update Existing Models") | Adds the project to the gateway's `allowedRoutes`; sets `spec.router.gateway.refs: [{name: maas-default-gateway, namespace: openshift-ingress}, {name: openshift-ai-inference, namespace: openshift-ingress}]` (the model keeps the inference gateway it used by default, as a dashboard-deployed model does; MaaS first because KServe's route update misses a parent removed from the end of the list) to LLMInferenceService `<project>/qwen25-05b-instruct` as field manager `rhoai-nightly-updater-maas` (only that field); MaaSModelRef `<project>/qwen25-05b-instruct` (`modelRef: {kind: LLMInferenceService, name: qwen25-05b-instruct}`); in `models-as-a-service`, MaaSSubscription `rhoai-nightly-updater-subscription` (owner group `system:authenticated`, `tokenRateLimits: [{limit: 100, window: 1m}]` per model, priority 10) and MaaSAuthPolicy `rhoai-nightly-updater-access` (subjects group `system:authenticated`), listing every published project. Waits for the reference Ready and both Active |
| Unpublish | Reverse: the project leaves the access policy and subscription (deleted when empty), the MaaSModelRef is deleted (waits for its finalizer), the model's gateway ref is removed (field manager `rhoai-nightly-updater-maas` applies nothing), the project leaves the gateway's `allowedRoutes`. The Models teardown refuses while the model is published |
| Remove | Unpublish every published model; set `modelsAsAService` back to the recorded value (only if this tool changed it; refused when someone else turned MaaS on and it uses the tool's gateway or database); delete the Gateway, then Secret `maas-db-config`, then the CloudNativePG Cluster (with its volume) |

Kept by Remove: GatewayClass `openshift-default`, Connectivity Link and Authorino, the CloudNativePG operator, the namespaces RHOAI's operator generated (`<infra>`, `ai-tenants`, `models-as-a-service`) and what the operator made in them (`Config/default`, the AITenant, MaasTenantConfig, maas-api), and every MaaS object someone else made.

## 9. What the tool never does

- Delete a namespace (other than `make undeploy` deleting its own).
- Read or list Secrets cluster-wide.
- Create an IDMS, or touch `openshift-config/pull-secret`.
- Delete webhook configurations of a live CSV or operand.
- Pause any operator other than `dashboard-operator`.
- Set `opendatahub.io/managed=false` where `dashboard-operator` exists (it would freeze the resource across upgrades).
- Delete DSC, DSCI or operator CRDs.
