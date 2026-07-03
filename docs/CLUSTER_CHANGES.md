# Cluster Resource Changes Reference

This document lists every Kubernetes resource the rhoai-nightly-updater tool creates, modifies, or deletes on the cluster. All entries are sourced from actual code with file/function references for verification.

**Trust principle:** This tool never modifies resources it doesn't explicitly document here. All operations are traceable to source code.

---

## Resources Created by Deployment

These resources are created when you deploy the tool using `oc process -f deploy/template.yaml | oc apply -f -`.

| Kind | Name | Namespace | Purpose |
|------|------|-----------|---------|
| ServiceAccount | `rhoai-nightly-updater` | `rhoai-nightly-updater` | Service identity for API requests |
| ClusterRole | `rhoai-nightly-updater` | cluster-scoped | Permissions for operator/catalog/secret management |
| ClusterRoleBinding | `rhoai-nightly-updater` | cluster-scoped | Binds ServiceAccount to ClusterRole |
| Deployment | `rhoai-nightly-updater` | `rhoai-nightly-updater` | Main app (2 containers: app + oauth-proxy) |
| Service | `rhoai-nightly-updater` | `rhoai-nightly-updater` | Exposes ports 8443 (HTTPS) and 8080 (metrics) |
| NetworkPolicy | `rhoai-nightly-updater` | `rhoai-nightly-updater` | Allows ingress from OCP router + monitoring |
| Route | `rhoai-nightly-updater` | `rhoai-nightly-updater` | Public HTTPS endpoint (reencrypt TLS) |
| ServiceMonitor | `rhoai-nightly-updater` | `rhoai-nightly-updater` | Prometheus scrape config for /metrics |
| ConsoleLink | `rhoai-nightly-updater` | cluster-scoped | Adds app to OCP console Application Menu |
| PrometheusRule | `rhoai-nightly-updater` | `rhoai-nightly-updater` | Alerting rules (pod down, update failures, high error rate) |

**Source:** `deploy/template.yaml`

---

## Resources Created by Operations

### Nightly Update

Triggered by: `POST /api/update` (Dashboard "Update to Nightly" action)

| Kind | Name | Namespace | Operation | Source |
|------|------|-----------|-----------|--------|
| Namespace | `redhat-ods-operator` | — | create | `operations.go:UpdateStream()` line 495-509 |
| OperatorGroup | `rhods-operator` | `redhat-ods-operator` | create | `operations.go:UpdateStream()` line 530-549 |
| CatalogSource | `rhoai-catalog-dev` | `openshift-marketplace` | create/update | `operations.go:UpdateStream()` line 590-620 |
| Subscription | `rhods-operator` | `redhat-ods-operator` | create/update | `operations.go:UpdateStream()` line 758-804 |

**Notes:**
- Namespace and OperatorGroup are created only if they don't exist (fresh cluster setup)
- CatalogSource points to the nightly FBC image (e.g., `quay.io/rhoai/rhoai-fbc-fragment:rhoai-3.5@sha256:...`)
- Subscription is patched to use the nightly catalog + auto-detected channel

---

### Reinstall

Triggered by: `POST /api/reinstall` (Dashboard "Reinstall" action)

**Deletion phase (cleanup):**

| Kind | Name | Namespace | Operation | Source |
|------|------|-----------|-----------|--------|
| CatalogSource | `rhoai-catalog-dev` | `openshift-marketplace` | delete | `operations.go:ReinstallStream()` line 1158-1173 |
| Subscription | `rhods-operator` | `redhat-ods-operator` | delete | `operations.go:ReinstallStream()` line 1176-1192 |
| ClusterServiceVersion | (discovered) | `redhat-ods-operator` | delete | `operations.go:ReinstallStream()` line 1195-1218 |
| ValidatingWebhookConfiguration | (RHOAI webhooks) | cluster-scoped | delete | `operations.go:cleanupStaleWebhooks()` line 1694-1726 |
| MutatingWebhookConfiguration | (RHOAI webhooks) | cluster-scoped | delete | `operations.go:cleanupStaleWebhooks()` line 1728-1756 |

**Recreation phase (reinstall):**

| Kind | Name | Namespace | Operation | Source |
|------|------|-----------|-----------|--------|
| CatalogSource | `rhoai-catalog-dev` | `openshift-marketplace` | create (nightly only) | `operations.go:reinstallNightlySteps()` line 1269-1297 |
| Subscription | `rhods-operator` | `redhat-ods-operator` | create | `operations.go:reinstallNightlySteps()` line 1422-1474<br>`operations.go:reinstallStableSteps()` line 1560-1612 |

**Notes:**
- Reinstall deletes ALL stale RHOAI webhooks to prevent blocking API calls
- For nightly reinstalls, creates fresh CatalogSource + Subscription
- For stable reinstalls, creates Subscription pointing to `redhat-operators` catalog

---

### Refresh (Same-version Reinstall)

Triggered by: `POST /api/refresh` (Dashboard "Refresh Operator" action)

| Kind | Name | Namespace | Operation | Source |
|------|------|-----------|-----------|--------|
| ClusterServiceVersion | (discovered) | `redhat-ods-operator` | delete | `operations.go:RefreshOperatorStream()` line 2042-2054 |
| Subscription | `rhods-operator` | `redhat-ods-operator` | delete + recreate | `operations.go:RefreshOperatorStream()` line 2057-2069<br>line 2094-2159 |

**Notes:**
- Refresh preserves the catalog source and channel
- Forces OLM to reinstall with updated images from the same catalog
- No webhooks are cleaned up (operator is just restarted, not uninstalled)

---

### DSC Creation

Triggered by: `POST /api/components/dsc/create` (Dashboard "Create DSC" action)

| Kind | Name | Namespace | Operation | Source |
|------|------|-----------|-----------|--------|
| DataScienceCluster | `default-dsc` | cluster-scoped | create | `components.go:CreateDefaultDSC()` line 533-541 |

**Notes:**
- Only created if no DSC exists
- Default component configuration fetched from upstream GitHub or falls back to built-in spec
- Spec source: `https://raw.githubusercontent.com/opendatahub-io/opendatahub-operator/main/config/rhoai/samples/datasciencecluster_v2_datasciencecluster.yaml`

---

### MinIO Setup

Triggered by: `POST /api/resources/minio/setup` (Dashboard "Quick Resource Creator" → MinIO)

| Kind | Name | Namespace | Operation | Source |
|------|------|-----------|-----------|--------|
| Namespace | `minio` | — | create | `minio.go:SetupMinIO()` line 117-149 |
| PersistentVolumeClaim | `minio-pvc` | `minio` | create | `minio.go:SetupMinIO()` line 157-165 |
| Secret | `minio-secret` | `minio` | create | `minio.go:SetupMinIO()` line 166-173 |
| Deployment | `minio` | `minio` | create | `minio.go:SetupMinIO()` line 174-213 |
| Service | `minio-service` | `minio` | create | `minio.go:SetupMinIO()` line 214-225 |
| Route | `minio-api` | `minio` | create | `minio.go:SetupMinIO()` line 226-234 |
| Route | `minio-ui` | `minio` | create | `minio.go:SetupMinIO()` line 235-243 |

**Additional operation:**
- S3 bucket `pipelines` is created via MinIO S3 API after deployment is ready (`minio.go:createMinioBucket()` line 372-394)

---

### MinIO Teardown

Triggered by: `POST /api/resources/minio/teardown`

| Kind | Name | Namespace | Operation | Source |
|------|------|-----------|-----------|--------|
| Namespace | `minio` | — | delete | `minio.go:TeardownMinIO()` line 409-423 |

**Notes:**
- Deleting the namespace cascades deletion of all MinIO resources (PVC, Deployment, Service, Routes)
- Blocked if any pipeline servers (DSPAs) still depend on MinIO

---

### Pipeline Server Setup

Triggered by: `POST /api/resources/pipeline-server/setup` (Dashboard "Quick Resource Creator" → Pipeline Server)

| Kind | Name | Namespace | Operation | Source |
|------|------|-----------|-----------|--------|
| Namespace | (user-specified or `test-pipelines`) | — | create | `pipeline_server.go:SetupPipelineServer()` line 118-167 |
| Secret | `dashboard-dspa-secret` | (pipeline project) | create | `pipeline_server.go:SetupPipelineServer()` line 202-227 |
| DataSciencePipelinesApplication | `dspa` | (pipeline project) | create | `pipeline_server.go:SetupPipelineServer()` line 230-274 |

**Notes:**
- Namespace is labeled with `opendatahub.io/dashboard=true` and `modelmesh-enabled=true`
- Secret contains MinIO S3 credentials (`AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`)
- DSPA points to MinIO S3 bucket for artifact storage

---

### Pipeline Server Teardown

Triggered by: `POST /api/resources/pipeline-server/teardown`

| Kind | Name | Namespace | Operation | Source |
|------|------|-----------|-----------|--------|
| DataSciencePipelinesApplication | `dspa` | (pipeline project) | delete | `pipeline_server.go:TeardownPipelineServer()` line 297-312 |
| Secret | `dashboard-dspa-secret` | (pipeline project) | delete | `pipeline_server.go:TeardownPipelineServer()` line 315-323 |

**Notes:**
- Namespace is NOT deleted (preserves other workloads in the project)
- Operator cleans up DSPA-owned resources (MariaDB, pipeline pods, etc.)

---

### MLflow Setup

Triggered by: `POST /api/resources/mlflow/setup`

| Kind | Name | Namespace | Operation | Source |
|------|------|-----------|-----------|--------|
| MLflow | `mlflow` | cluster-scoped | create | `mlflow.go:SetupMLflow()` line 99-150 |

**Notes:**
- MLflow CR is cluster-scoped (no namespace)
- Operator creates Deployment, Service, Route, and PVC in `redhat-ods-applications` namespace

---

### MLflow Teardown

Triggered by: `POST /api/resources/mlflow/teardown`

| Kind | Name | Namespace | Operation | Source |
|------|------|-----------|-----------|--------|
| MLflow | `mlflow` | cluster-scoped | delete | `mlflow.go:TeardownMLflow()` line 172-187 |

---

### Dashboard PR Deploy

Triggered by: `POST /api/dashboard/deploy-pr` (Dashboard "Developer Tools" → Deploy PR)

| Kind | Name | Namespace | Operation | Source |
|------|------|-----------|-----------|--------|
| Deployment | `rhods-dashboard` | `redhat-ods-applications` | strategic merge patch | `dashboard.go:DeployPRImage()` line 318-349 |

**Patch content:**
- Sets `metadata.annotations["opendatahub.io/managed"]="false"` to disable operator reconciliation
- Patches container images for all containers with published PR images (up to 8 containers):
  - `rhods-dashboard`, `model-registry-ui`, `gen-ai-ui`, `maas-ui`, `mlflow-ui`, `eval-hub-ui`, `automl-ui`, `autorag-ui`

**Image sources:** `quay.io/{repo}:pr-{number}` (verified via Quay manifest API before patching)

---

### Dashboard PR Revert

Triggered by: `POST /api/dashboard/revert`

| Kind | Name | Namespace | Operation | Source |
|------|------|-----------|-----------|--------|
| Deployment | `rhods-dashboard` | `redhat-ods-applications` | strategic merge patch | `dashboard.go:RevertDashboardImage()` line 403-433 |

**Patch content:**
- Restores ALL container images to operator-managed defaults (read from operator env vars)
- Sets `metadata.annotations["opendatahub.io/managed"]="true"` to re-enable operator management

**Original images source:** `redhat-ods-operator/rhods-operator` deployment env vars (`RELATED_IMAGE_*`)

---

### MLflow PR Deploy

Triggered by: `POST /api/resources/mlflow/deploy-pr`

| Kind | Name | Namespace | Operation | Source |
|------|------|-----------|-----------|--------|
| MLflow | `mlflow` | cluster-scoped | patch | `mlflow.go:DeployMLflowPR()` line 244-264 |

**Patch content:**
- Sets `spec.image.image` to `quay.io/opendatahub/mlflow:odh-pr-{number}`

---

### MLflow PR Revert

Triggered by: `POST /api/resources/mlflow/revert`

| Kind | Name | Namespace | Operation | Source |
|------|------|-----------|-----------|--------|
| MLflow | `mlflow` | cluster-scoped | patch | `mlflow.go:RevertMLflowImage()` line 289-308 |

**Patch content:**
- Resets `spec.image.image` to `quay.io/opendatahub/mlflow:odh-stable`

---

## Resources Modified (Patches)

### Rollout Assist (Unblock Stuck Deployments)

Triggered by: `POST /api/diagnostics/fix/assist-rollout` (Auto-fix for stuck rollouts)

| Kind | Name | Namespace | Operation | Source |
|------|------|-----------|-----------|--------|
| Deployment | (any stuck deployment) | `redhat-ods-operator` or `redhat-ods-applications` | strategic merge patch | `rollout.go:assistDeploymentRollout()` line 233-250 |
| Pod | (Pending pod) | `redhat-ods-operator` or `redhat-ods-applications` | delete | `rollout.go:assistDeploymentRollout()` line 252-259 |

**Patch content:**
- Sets `spec.strategy.rollingUpdate.maxUnavailable=1` to allow K8s to terminate one old pod
- Deletes the Pending pod to trigger scheduler re-evaluation

**When applied:**
- Deployment has new ReplicaSet with desired > ready
- New ReplicaSet has Pending pods (scheduling failure)
- Old ReplicaSets still have ready pods holding resources

---

### Webhook Cleanup (Stale Webhooks)

Triggered by: Reinstall operation, or `POST /api/diagnostics/fix/delete-stale-webhooks`

| Kind | Name | Namespace | Operation | Source |
|------|------|-----------|-----------|--------|
| ValidatingWebhookConfiguration | (RHOAI webhooks) | cluster-scoped | delete | `operations.go:cleanupStaleWebhooks()` line 1694-1726 |
| MutatingWebhookConfiguration | (RHOAI webhooks) | cluster-scoped | delete | `operations.go:cleanupStaleWebhooks()` line 1728-1756 |

**Selection criteria:** Webhook name contains "opendatahub" or "rhods", OR has label `olm.owner` containing "rhods"/"opendatahub"

**Notes:**
- `cleanupStaleWebhooks()` deletes ALL RHOAI webhooks (used during Reinstall)
- `deleteStaleWebhooksOnly()` deletes only webhooks whose backing service is missing (used by diagnostics auto-fix)

---

### CRD Conversion Webhook Cleanup

Triggered by: Reinstall operation

| Kind | Name | Namespace | Operation | Source |
|------|------|-----------|-----------|--------|
| CustomResourceDefinition | `datascienceclusters.datasciencecluster.opendatahub.io` | cluster-scoped | patch | `operations.go:patchCRDConversionWebhooks()` line 1930-1953 |
| CustomResourceDefinition | `dscinitializations.dscinitialization.opendatahub.io` | cluster-scoped | patch | `operations.go:patchCRDConversionWebhooks()` line 1930-1953 |

**Patch content:**
- Sets `spec.conversion.strategy="None"` to disable conversion webhooks during reinstall

**Why:** Prevents API failures when the operator is down and cannot serve conversion webhook requests

---

### Component CR Finalizer Cleanup (Unstuck Deletions)

Triggered by: Reinstall operation (EA↔GA transitions)

| Kind | Name | Namespace | Operation | Source |
|------|------|-----------|-----------|--------|
| (Any component CR) | (name from discovery) | cluster-scoped | patch | `operations.go:cleanupStuckComponentCRs()` line 1881-1927 |

**Scope:** Component CRs in `components.platform.opendatahub.io/v1alpha1` with `deletionTimestamp != nil` and `finalizers != []`

**Resources checked:**
- `codeflares`, `dashboards`, `datasciencepipelines`, `feastoperators`, `kserves`, `kueues`, `llamastackoperators`, `mlflowoperators`, `modelcontrollers`, `modelmeshservings`, `modelregistries`, `modelsasservices`, `ogxs`, `rays`, `sparkoperators`, `trainers`, `trainingoperators`, `trustyais`, `workbenches`

**Patch content:**
- Sets `metadata.finalizers=[]` to unblock deletion

**When applied:** After EA↔GA operator transitions when component CRs get stuck with finalizers during deletion

---

### MaaS Gateway Annotation Fix

Triggered by: `POST /api/diagnostics/fix/fix-maas-gateway-annotation`

| Kind | Name | Namespace | Operation | Source |
|------|------|-----------|-----------|--------|
| Gateway | `maas-default-gateway` | `openshift-ingress` | patch | `diagnostics.go:applyFixMaaSGatewayAnnotation()` line 1124-1133 |

**Patch content:**
- Adds `metadata.annotations["opendatahub.io/managed"]="false"` to satisfy MaaS prerequisites check

---

### Component Disable (DSC Patch)

Triggered by: `POST /api/diagnostics/fix/disable-component:{name}`

| Kind | Name | Namespace | Operation | Source |
|------|------|-----------|-----------|--------|
| DataScienceCluster | `default-dsc` | cluster-scoped | patch | `diagnostics.go:applyFixDisableComponent()` line 1093-1100 |

**Patch content:**
- Sets `spec.components.{componentName}.managementState="Removed"`

**Allowed components:**
- `llamastackoperator`, `feastoperator`, `trustyai`, `ray`, `kueue`, `sparkoperator`, `trainer`, `trainingoperator`, `modelsasservice`

---

### Operator Restart

Triggered by: `POST /api/diagnostics/fix/restart-operator`

| Kind | Name | Namespace | Operation | Source |
|------|------|-----------|-----------|--------|
| Deployment | `rhods-operator` | `redhat-ods-operator` | strategic merge patch | `diagnostics.go:applyFixRestartOperator()` line 1228-1240 |

**Patch content:**
- Sets `spec.template.metadata.annotations["kubectl.kubernetes.io/restartedAt"]` to current timestamp

**Effect:** Rolling restart of operator pods (no downtime due to multiple replicas)

---

## Resources Read Only

These resources are queried for status but never modified.

**Source:** `status.go:GetStatus()`

| Kind | Name | Namespace | Purpose |
|------|------|-----------|---------|
| ClusterVersion | `version` | cluster-scoped | OCP version display |
| Console | `cluster` | cluster-scoped | Console URL for log links |
| ImageDigestMirrorSet | (all) | cluster-scoped | IDMS existence check |
| Subscription | `rhods-operator` | `redhat-ods-operator` | Current channel/state |
| ClusterServiceVersion | (RHOAI CSV) | `redhat-ods-operator` | Operator version/phase |
| CatalogSource | `rhoai-catalog-dev` | `openshift-marketplace` | Catalog health |
| InstallPlan | (latest) | `redhat-ods-operator` | OLM install progress |
| Pod | (catalog pod) | `openshift-marketplace` | Catalog pod health |
| Pod | (operator pods) | `redhat-ods-operator` | Operator pod health |
| Pod | (app pods) | `redhat-ods-applications` | Component pod health |
| DataScienceCluster | `default-dsc` | cluster-scoped | DSC existence check |
| Secret | `additional-pull-secret` | `kube-system` | Pull secret validation |
| User | `~` | cluster-scoped | Current user identity |

**Source:** `components.go:GetComponents()`

| Kind | Name | Namespace | Purpose |
|------|------|-----------|---------|
| DataScienceCluster | (all) | cluster-scoped | Component status + deployments |
| Deployment | (all) | `redhat-ods-applications`, `redhat-ods-operator` | Replica counts + images |
| ReplicaSet | (all) | `redhat-ods-applications`, `redhat-ods-operator` | Pod ownership (rollout assist) |
| Pod | (all) | `redhat-ods-applications`, `redhat-ods-operator` | Container status + restarts |

**Source:** `resources.go:GetResourcesStatus()`

| Kind | Name | Namespace | Purpose |
|------|------|-----------|---------|
| Namespace | (DS projects with `opendatahub.io/dashboard=true`) | — | Pipeline server listing |
| Deployment | `minio` | `minio` | MinIO readiness |
| Route | `minio-api`, `minio-ui` | `minio` | MinIO URLs |
| MLflow | `mlflow` | cluster-scoped | MLflow status |
| Route | `mlflow` | `redhat-ods-applications` | MLflow URL |
| DataSciencePipelinesApplication | `dspa` | (DS projects) | Pipeline server status |

**Source:** `diagnostics.go:DiagnoseCluster()`

| Kind | Name | Namespace | Purpose |
|------|------|-----------|---------|
| Node | (all) | cluster-scoped | Capacity diagnostics |
| ValidatingWebhookConfiguration | (all) | cluster-scoped | Stale webhook detection |
| MutatingWebhookConfiguration | (all) | cluster-scoped | Stale webhook detection |
| PackageManifest | `rhods-operator` | `openshift-marketplace` | Channel detection |

---

## Namespaces Affected

### Namespaces the tool operates in:

| Namespace | Operations | Notes |
|-----------|------------|-------|
| `rhoai-nightly-updater` | **Tool deployment** | All tool resources (Deployment, Service, Route, ConfigMaps) |
| `openshift-marketplace` | **CatalogSource create/update/delete** | Nightly catalog management |
| `redhat-ods-operator` | **Subscription, CSV, InstallPlan, OperatorGroup create/delete**<br>**Deployment patch** (operator restart) | Operator namespace (created if missing) |
| `redhat-ods-applications` | **Deployment patch** (dashboard PR images)<br>**MLflow CR create/delete** (operator-managed) | Operator-managed components |
| `kube-system` | **Secret create/update** (`additional-pull-secret`) | Pull secret for nightly images |
| `minio` | **All MinIO resources** (Namespace, PVC, Deployment, Service, Routes) | Test infrastructure (user-requested) |
| `openshift-ingress` | **Gateway patch** (MaaS annotation fix) | MaaS component prerequisites |
| (DS projects) | **Namespace create**<br>**DSPA create/delete**<br>**Secret create/delete** | Pipeline server setup (user-specified namespace) |

**Cluster-scoped resources:**
- ClusterRole, ClusterRoleBinding, ConsoleLink
- CRD patches, Webhook configurations
- DataScienceCluster, MLflow CRs

---

## Activity Logging

All operations are recorded in a ConfigMap for audit trail:

| Kind | Name | Namespace | Operation | Source |
|------|------|-----------|-----------|--------|
| ConfigMap | `rhoai-nightly-updater-activity` | `rhoai-nightly-updater` | create/update | `activity.go:RecordActivity()` |

**Logged actions:**
- `update`, `reinstall`, `refresh`, `rollback`, `create-pull-secret`, `setup-minio`, `teardown-minio`, `setup-pipeline-server`, `teardown-pipeline-server`, `setup-mlflow`, `teardown-mlflow`, `deploy-pr`, `revert-dashboard`, `deploy-mlflow-pr`, `revert-mlflow`, `assist-rollout`, `create-dsc`, `fix-delete-stale-webhooks`, `fix-recreate-subscription`, `disable-component`, `fix-maas-gateway`, `restart-operator`

---

## Deployment Snapshots

Before each nightly update or reinstall, the tool captures current deployment images:

| Kind | Name | Namespace | Operation | Source |
|------|------|-----------|-----------|--------|
| ConfigMap | `rhoai-nightly-updater-snapshot` | `rhoai-nightly-updater` | create/update | `snapshot.go:SaveDeploymentSnapshot()` |

**Content:** JSON snapshot of deployment name → image digest mappings

**Used for:** Detecting which deployments changed after an update (displayed in Components panel)

---

## Authorization Checks

The tool performs self-access reviews before mutations:

| Kind | Name | Namespace | Operation | Source |
|------|------|-----------|-----------|--------|
| SelfSubjectAccessReview | (ephemeral) | cluster-scoped | create | `authz.go:CheckAccess()` |

**Checked permissions:**
- Namespace create/delete (for MinIO + pipeline servers)
- Secret create/update (for pull secrets + DSPA secrets)
- DSPA create/delete (for pipeline servers)
- MLflow create/delete (for MLflow setup)

**Authorization model:** Operations that require elevated permissions (namespace create, secret management) include server-side validation that restricts actual access to known safe namespaces (`minio`, DS projects with `opendatahub.io/dashboard` label). Broad RBAC permissions are required because pipeline project namespaces are user-specified at runtime.

---

## Summary

**Resources created:** 10 (deployment) + 27 (operations) + 2 (audit/snapshots) = **39 resource types**

**Resources modified:** 12 resource types (patches, strategic merges)

**Resources deleted:** 9 resource types (cleanup operations)

**Read-only queries:** 20+ resource types (status/diagnostics)

**Trust verification:** Every entry in this document includes source code references (file + function name) so operations can be independently verified by reading the code.
