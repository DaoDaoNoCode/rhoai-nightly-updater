# Cluster Resource Changes Reference

This document describes what Kubernetes resources the rhoai-nightly-updater tool manages on the cluster.

**Note:** This doc describes what the tool does. For implementation details, see the source code.

**Trust principle:** This tool never modifies resources it doesn't explicitly document here.

---

## Resources Created by Deployment

These resources are created when you deploy the tool.

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

---

## Operations

### Nightly Update

Triggered by the "Update to Nightly" action.

**Resources affected:**

| Kind | Name | Namespace | Operation |
|------|------|-----------|-----------|
| Namespace | `redhat-ods-operator` | — | create if missing |
| OperatorGroup | `rhods-operator` | `redhat-ods-operator` | create if missing |
| CatalogSource | `rhoai-catalog-dev` | `openshift-marketplace` | create/update |
| Subscription | `rhods-operator` | `redhat-ods-operator` | create/patch |

**What happens:**
- Namespace and OperatorGroup are created only if they don't exist (fresh cluster setup)
- CatalogSource points to the selected nightly FBC image
- Subscription is configured to use the nightly catalog with auto-detected channel

---

### Reinstall

Triggered by the "Reinstall" action.

**Stable target discovery:**
- Reads `rhods-operator` PackageManifest entries from the configured `STABLE_SOURCE` (default: `redhat-operators`) in `openshift-marketplace`, using a catalog label selector and verifying the returned catalog name and namespace. The nightly CatalogSource does not influence this result.
- Chooses the highest GA channel head among production `stable`, `fast`, and `eus` channels. EA and other prereleases are excluded. Equal versions prefer the catalog's default channel, then a stable channel.
- The UI shows the selected channel and GA version. `STABLE_CHANNEL` optionally pins a validated GA channel and is labeled as configured rather than latest.
- Rechecks catalog availability before cleanup; discovery failure stops reinstall without removing the operator. An older channel on the same Red Hat catalog can be reinstalled to the latest channel.

**Deletion phase:**

| Kind | Name | Namespace | Operation |
|------|------|-----------|-----------|
| CatalogSource | `rhoai-catalog-dev` | `openshift-marketplace` | delete |
| Subscription | `rhods-operator` | `redhat-ods-operator` | delete |
| ClusterServiceVersion | all RHOAI CSVs | `redhat-ods-operator` | delete |
| ValidatingWebhookConfiguration | RHOAI webhooks | cluster-scoped | delete |
| MutatingWebhookConfiguration | RHOAI webhooks | cluster-scoped | delete |

**Recreation phase:**

| Kind | Name | Namespace | Operation |
|------|------|-----------|-----------|
| CatalogSource | `rhoai-catalog-dev` | `openshift-marketplace` | create (nightly or custom image) |
| Subscription | `rhods-operator` | `redhat-ods-operator` | create |

**What happens:**
- All stale RHOAI webhooks are deleted to prevent blocking API calls
- For nightly or custom reinstalls, creates fresh CatalogSource + Subscription
- Custom version accepts a Quay FBC image, including older versions and builds pinned by SHA256 digest. The digest is used exactly, even if the tag now points to a different build. The channel is detected from the selected catalog or supplied as an override.
- For stable reinstalls, creates Subscription pointing to `redhat-operators` catalog

---

### Refresh

Triggered by the "Refresh Operator" action.

| Kind | Name | Namespace | Operation |
|------|------|-----------|-----------|
| ClusterServiceVersion | current CSV | `redhat-ods-operator` | delete |
| Subscription | `rhods-operator` | `redhat-ods-operator` | delete + recreate |

**What happens:**
- Preserves the catalog source and channel
- Forces OLM to reinstall with updated images from the same catalog
- No webhooks are cleaned up (operator is just restarted, not uninstalled)

---

### DSC Creation

Triggered by the "Create DSC" action.

| Kind | Name | Namespace | Operation |
|------|------|-----------|-----------|
| DataScienceCluster | `default-dsc` | cluster-scoped | create |

**What happens:**
- Only created if no DSC exists
- Default configuration is fetched from `red-hat-data-services/rhods-operator` using the installed CSV version: `3.6.0` maps to `rhoai-3.6`, and `3.6.0-ea.2` maps to `rhoai-3.6-ea.2`.
- Unnumbered EA releases work the same way: `3.7.0-ea` maps to `rhoai-3.7-ea`; GA maps to `rhoai-3.7`. Version numbers and prerelease names are derived, with no supported-version list. `DSC_SAMPLE_REF` can override the Git branch/tag if upstream naming changes.
- Fetches the v2 sample, with v1 fallback only when the v2 sample is absent in that same branch. Samples are cached by branch and API version for one hour.
- If the corresponding sample cannot be fetched, creation fails with an actionable error. An unrelated built-in spec is never substituted.

### DSC Field Repair

Triggered by **Remove invalid fields**, **Remove extra components**, or **Reset to version defaults** on the Components page, after confirmation.

- Checks field names against the installed DSC CRD schema, including nested keys. Management state differences are not compatibility errors. Valid optional settings and free-form configuration are retained.
- Compares component names in both directions against the version-matched upstream sample. Components absent from that sample are reported even if the installed CRD still accepts them for backward compatibility.
- **Remove invalid fields** patches the named DataScienceCluster, deleting unsupported keys while preserving valid values.
- **Remove extra components** deletes only the reviewed component entries absent from the version defaults. Remaining settings and management states are retained; missing components are not added. The operation refuses to proceed if the operator version or extra-component list changes after confirmation.
- **Reset to version defaults** previews the matching upstream sample, then replaces the DSC spec, including management states and custom settings. Removed keys are explicitly deleted; DSC metadata is retained.
- Repairs use a resource-version precondition to detect concurrent edits, share the cluster mutation lock, and are recorded in the activity log. Reset also checks that the installed operator version still matches the preview.

---

### MinIO Setup

Triggered by the Quick Resource Creator MinIO action.

| Kind | Name | Namespace | Operation |
|------|------|-----------|-----------|
| Namespace | `minio` | — | create |
| PersistentVolumeClaim | `minio-pvc` | `minio` | create |
| Secret | `minio-secret` | `minio` | create |
| Deployment | `minio` | `minio` | create |
| Service | `minio-service` | `minio` | create |
| Route | `minio-api` | `minio` | create |
| Route | `minio-ui` | `minio` | create |

**What happens:**
- Creates a complete MinIO deployment in the `minio` namespace
- S3 bucket `pipelines` is created via MinIO S3 API after deployment is ready

---

### MinIO Teardown

Triggered by the Quick Resource Creator MinIO teardown action.

| Kind | Name | Namespace | Operation |
|------|------|-----------|-----------|
| Namespace | `minio` | — | delete |

**What happens:**
- Deleting the namespace cascades deletion of all MinIO resources
- Blocked if any pipeline servers still depend on MinIO

---

### Pipeline Server Setup

Triggered by the Quick Resource Creator Pipeline Server action.

| Kind | Name | Namespace | Operation |
|------|------|-----------|-----------|
| Namespace | user-specified | — | create if missing |
| Secret | `dashboard-dspa-secret` | target namespace | create |
| DataSciencePipelinesApplication | `dspa` | target namespace | create |

**What happens:**
- Namespace is labeled with `opendatahub.io/dashboard=true` and `modelmesh-enabled=true`
- Secret contains MinIO S3 credentials
- DSPA points to MinIO S3 bucket for artifact storage

---

### Pipeline Server Teardown

Triggered by the Quick Resource Creator Pipeline Server teardown action.

| Kind | Name | Namespace | Operation |
|------|------|-----------|-----------|
| DataSciencePipelinesApplication | `dspa` | target namespace | delete |
| Secret | `dashboard-dspa-secret` | target namespace | delete |

**What happens:**
- Namespace is NOT deleted (preserves other workloads in the project)
- Operator cleans up DSPA-owned resources

---

### MLflow Setup

Triggered by the Quick Resource Creator MLflow action.

| Kind | Name | Namespace | Operation |
|------|------|-----------|-----------|
| MLflow | `mlflow` | cluster-scoped | create |

**What happens:**
- MLflow CR is cluster-scoped
- Operator creates Deployment, Service, Route, and PVC in `redhat-ods-applications` namespace

---

### MLflow Teardown

Triggered by the Quick Resource Creator MLflow teardown action.

| Kind | Name | Namespace | Operation |
|------|------|-----------|-----------|
| MLflow | `mlflow` | cluster-scoped | delete |

---

### Dashboard PR Deploy

Triggered by the Dashboard Dev PR deploy action.

| Kind | Name | Namespace | Operation |
|------|------|-----------|-----------|
| Deployment | `rhods-dashboard` | `redhat-ods-applications` | patch |

**What happens:**
- Disables operator reconciliation via annotation
- Patches container images for all containers with published PR images
- Verifies images exist on Quay before patching

---

### Dashboard PR Revert

Triggered by the Dashboard Dev revert action.

| Kind | Name | Namespace | Operation |
|------|------|-----------|-----------|
| Deployment | `rhods-dashboard` | `redhat-ods-applications` | patch |

**What happens:**
- Restores all container images to operator-managed defaults
- Re-enables operator management via annotation

---

### MLflow PR Deploy

Triggered by the Dashboard Dev MLflow PR deploy action.

| Kind | Name | Namespace | Operation |
|------|------|-----------|-----------|
| MLflow | `mlflow` | cluster-scoped | patch |

**What happens:**
- Updates MLflow CR to use the specified PR image

---

### MLflow PR Revert

Triggered by the Dashboard Dev MLflow revert action.

| Kind | Name | Namespace | Operation |
|------|------|-----------|-----------|
| MLflow | `mlflow` | cluster-scoped | patch |

**What happens:**
- Resets MLflow CR to use the default stable image

---

## Diagnostic Auto-Fixes

### Rollout Assist

Triggered by the diagnostics auto-fix for stuck deployments.

| Kind | Name | Namespace | Operation |
|------|------|-----------|-----------|
| Deployment | any stuck deployment | `redhat-ods-operator` or `redhat-ods-applications` | patch |
| Pod | pending pods | `redhat-ods-operator` or `redhat-ods-applications` | delete |

**What happens:**
- Adjusts deployment strategy to allow terminating one old pod
- Deletes pending pods to trigger scheduler re-evaluation
- Applied when new ReplicaSet has pending pods and old ReplicaSet holds resources

---

### Webhook Cleanup

Triggered by reinstall operation or diagnostics auto-fix.

| Kind | Name | Namespace | Operation |
|------|------|-----------|-----------|
| ValidatingWebhookConfiguration | RHOAI webhooks | cluster-scoped | delete |
| MutatingWebhookConfiguration | RHOAI webhooks | cluster-scoped | delete |

**What happens:**
- During reinstall: deletes all RHOAI webhooks
- During diagnostics auto-fix: deletes only webhooks whose backing service is missing
- Selection criteria: webhook name or label contains "opendatahub" or "rhods"

---

### CRD Conversion Webhook Cleanup

Triggered by reinstall operation.

| Kind | Name | Namespace | Operation |
|------|------|-----------|-----------|
| CustomResourceDefinition | DSC CRD | cluster-scoped | patch |
| CustomResourceDefinition | DSCI CRD | cluster-scoped | patch |

**What happens:**
- Disables conversion webhooks during reinstall
- Prevents API failures when operator is down

---

### Component CR Finalizer Cleanup

Triggered by reinstall operation during EA↔GA transitions.

| Kind | Name | Namespace | Operation |
|------|------|-----------|-----------|
| component CRs | any stuck CR | cluster-scoped | patch |

**What happens:**
- Checks all component CRs in `components.platform.opendatahub.io/v1alpha1`
- Removes finalizers from CRs with non-nil deletionTimestamp
- Unblocks deletion after EA↔GA operator transitions

---

### MaaS Gateway Annotation Fix

Triggered by diagnostics auto-fix.

| Kind | Name | Namespace | Operation |
|------|------|-----------|-----------|
| Gateway | `maas-default-gateway` | `openshift-ingress` | patch |

**What happens:**
- Adds required annotation to satisfy MaaS prerequisites check

---

### Component Disable

Triggered by diagnostics auto-fix for failing components.

| Kind | Name | Namespace | Operation |
|------|------|-----------|-----------|
| DataScienceCluster | `default-dsc` | cluster-scoped | patch |

**What happens:**
- Sets component managementState to "Removed"
- Available for experimental/optional components only

---

### Operator Restart

Triggered by diagnostics auto-fix.

| Kind | Name | Namespace | Operation |
|------|------|-----------|-----------|
| Deployment | `rhods-operator` | `redhat-ods-operator` | patch |

**What happens:**
- Triggers rolling restart via annotation update
- No downtime due to multiple replicas

---

## Read-Only Operations

The tool queries many resources for status display and diagnostics but never modifies them:

**Cluster Info:**
- ClusterVersion (OCP version)
- Console (console URL for links)
- ImageDigestMirrorSet (IDMS existence check)
- Node (capacity diagnostics)
- User (current user identity)

**Operator State:**
- Subscription, ClusterServiceVersion, CatalogSource, InstallPlan (operator status)
- PackageManifest (channel detection)
- Pods in `openshift-marketplace`, `redhat-ods-operator`, `redhat-ods-applications` (health checks)

**Component State:**
- DataScienceCluster (component status)
- Deployments, ReplicaSets, Pods in RHOAI namespaces (deployment status, rollout detection)

**Test Infrastructure:**
- Namespaces (Data Science project listing)
- MinIO Deployment, Routes (MinIO status)
- MLflow CR, Route (MLflow status)
- DataSciencePipelinesApplication (pipeline server status)

**Diagnostics:**
- ValidatingWebhookConfiguration, MutatingWebhookConfiguration (stale webhook detection)
- Pull secret in `kube-system` (credential validation)

---

## Namespaces Affected

| Namespace | Operations |
|-----------|------------|
| `rhoai-nightly-updater` | Tool deployment resources |
| `openshift-marketplace` | CatalogSource lifecycle |
| `redhat-ods-operator` | Subscription, CSV, OperatorGroup, operator restart |
| `redhat-ods-applications` | Dashboard PR deploy/revert |
| `kube-system` | Pull secret management |
| `minio` | MinIO deployment (all resources in namespace) |
| `openshift-ingress` | Gateway annotation fixes |
| User-specified Data Science projects | Pipeline server setup (namespace create, DSPA, secrets) |

**Cluster-scoped resources:**
- ClusterRole, ClusterRoleBinding, ConsoleLink
- CRD patches, webhook configurations
- DataScienceCluster, MLflow CRs

---

## Activity Logging

All operations are recorded in a ConfigMap for audit trail:

| Kind | Name | Namespace | Operation |
|------|------|-----------|-----------|
| ConfigMap | `rhoai-nightly-updater-activity` | `rhoai-nightly-updater` | create/update |

**Logged actions:** update, reinstall, refresh, rollback, pull secret management, resource lifecycle operations, PR deploys, diagnostic fixes

---

## Deployment Snapshots

Before each nightly update or reinstall, the tool captures current deployment images:

| Kind | Name | Namespace | Operation |
|------|------|-----------|-----------|
| ConfigMap | `rhoai-nightly-updater-snapshot` | `rhoai-nightly-updater` | create/update |

**Used for:** Detecting which deployments changed after an update (displayed in Components panel)

---

## Authorization Model

The ServiceAccount has broad RBAC permissions because pipeline project namespaces are user-specified at runtime. Operations include server-side validation that restricts actual access to known safe namespaces (`minio`, Data Science projects with `opendatahub.io/dashboard` label).

The tool checks the token owner's full RBAC before every mutation: `update` on `subscriptions.operators.coreos.com` in `redhat-ods-operator` grants access to app mutations. Users who can enter the app but lack that permission are read-only. Reviews use the user OAuth token and OpenShift self SAR with an explicitly empty scopes array; cluster mutations use the ServiceAccount token. Permission lookup errors block mutations.
