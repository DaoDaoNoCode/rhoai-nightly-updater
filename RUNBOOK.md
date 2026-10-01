# RHOAI Nightly Updater -- Runbook

Namespace: `rhoai-nightly-updater`
App label: `app=rhoai-nightly-updater`
Containers: `app` (Go backend, port 8080), `oauth-proxy` (port 8443)

---

## 1. App Is Down / Pod Not Starting

### Symptoms
- Route returns 502/503.
- `oc get pods -n rhoai-nightly-updater` shows `CrashLoopBackOff`, `ImagePullBackOff`, `OOMKilled`, or `0/2 Running`.

### Diagnosis

```bash
# Pod status
oc get pods -n rhoai-nightly-updater

# Recent events (image pull errors, OOM, scheduling failures)
oc describe pod -l app=rhoai-nightly-updater -n rhoai-nightly-updater

# App container logs
oc logs -l app=rhoai-nightly-updater -c app -n rhoai-nightly-updater --tail=100

# oauth-proxy container logs
oc logs -l app=rhoai-nightly-updater -c oauth-proxy -n rhoai-nightly-updater --tail=100

# Check the deployment rollout status
oc rollout status deployment/rhoai-nightly-updater -n rhoai-nightly-updater
```

### Common Causes and Fixes

**CrashLoopBackOff (app container)**
- The app exits if the static frontend directory is missing. Verify the container image was built with `./frontend/dist` included:
  ```bash
  oc get deployment rhoai-nightly-updater -n rhoai-nightly-updater -o jsonpath='{.spec.template.spec.containers[0].image}'
  ```
- Check if the ServiceAccount token is mounted (the app reads `/var/run/secrets/kubernetes.io/serviceaccount/token` at startup for health checks):
  ```bash
  oc get deployment rhoai-nightly-updater -n rhoai-nightly-updater -o jsonpath='{.spec.template.spec.automountServiceAccountToken}'
  ```
  Must return `true`.

**ImagePullBackOff**
- Image does not exist or registry credentials are missing:
  ```bash
  oc get events -n rhoai-nightly-updater --field-selector reason=Failed --sort-by='.lastTimestamp'
  ```
- If the image is on a private registry, ensure an `imagePullSecrets` entry exists on the ServiceAccount or Deployment.

**OOMKilled**
- The app container has a 128Mi memory limit. If the Build Explorer is loading many tags concurrently, it can spike. Bump the limit:
  ```bash
  oc patch deployment rhoai-nightly-updater -n rhoai-nightly-updater --type json \
    -p '[{"op":"replace","path":"/spec/template/spec/containers/0/resources/limits/memory","value":"256Mi"}]'
  ```

**readOnlyRootFilesystem**
- The container runs with `readOnlyRootFilesystem: true`. If a new code path tries to write to disk (temp files, caches), the pod will crash. Check logs for "read-only file system" errors.

---

## 2. Authentication Failing (403/401 Errors)

### Symptoms
- Users see a 403 Forbidden or 401 Unauthorized page after logging in.
- API calls return `{"error":"no auth token","errorCode":"unauthorized"}`.
- oauth-proxy logs show SAR check failures.

### Diagnosis

```bash
# oauth-proxy logs (authentication errors appear here)
oc logs -l app=rhoai-nightly-updater -c oauth-proxy -n rhoai-nightly-updater --tail=50

# Verify ServiceAccount exists and has the OAuth redirect annotation
oc get sa rhoai-nightly-updater -n rhoai-nightly-updater -o yaml

# Verify ClusterRoleBinding
oc get clusterrolebinding rhoai-nightly-updater -o yaml

# Check that the TLS serving cert secret exists
oc get secret rhoai-nightly-updater-tls -n rhoai-nightly-updater

# Test the SAR check manually (oauth-proxy checks: can user list pods in redhat-ods-operator?)
oc auth can-i list pods -n redhat-ods-operator --as=<username>
```

### Common Causes and Fixes

**Cookie secret changed / stale cookies**
- If the Deployment was redeployed with a new `COOKIE_SECRET`, existing browser cookies are invalid. Tell the user to clear cookies for the Route domain, or open an incognito window.

**SAR check failing for a user**
- The oauth-proxy is configured with `--openshift-sar={"resource":"pods","verb":"list","namespace":"redhat-ods-operator"}`. The user must have permission to list pods in `redhat-ods-operator`. Verify:
  ```bash
  oc auth can-i list pods -n redhat-ods-operator --as=<username>
  ```
  If `no`, grant access or change the SAR scope in the Deployment args.

**ServiceAccount token not mounted**
- The app container reads the SA token to make Kubernetes API calls. If it cannot find the token, all API endpoints return 500:
  ```bash
  oc exec deploy/rhoai-nightly-updater -c app -n rhoai-nightly-updater -- ls /var/run/secrets/kubernetes.io/serviceaccount/token
  ```

**TLS cert secret missing**
- The oauth-proxy cannot start without its TLS cert. The Service annotation `service.beta.openshift.io/serving-cert-secret-name: rhoai-nightly-updater-tls` should auto-generate it. If missing:
  ```bash
  oc delete secret rhoai-nightly-updater-tls -n rhoai-nightly-updater 2>/dev/null
  oc annotate service rhoai-nightly-updater -n rhoai-nightly-updater \
    service.beta.openshift.io/serving-cert-secret-name=rhoai-nightly-updater-tls --overwrite
  ```
  Then restart the pod:
  ```bash
  oc rollout restart deployment/rhoai-nightly-updater -n rhoai-nightly-updater
  ```

---

## 3. Quay API Errors (Tags Not Loading, Build Explorer Empty)

### Symptoms
- The "Nightly Tags" dropdown is empty or shows an error.
- Build Explorer page shows "Failed to fetch nightly tags."
- App logs show `quay auth returned 401` or `quay auth request failed`.

### Diagnosis

```bash
# App logs (Quay errors appear here)
oc logs -l app=rhoai-nightly-updater -c app -n rhoai-nightly-updater --tail=50 | grep -i quay

# Check that the pull secret exists and has quay.io/rhoai credentials
oc get secret additional-pull-secret -n kube-system -o jsonpath='{.data.\.dockerconfigjson}' | base64 -d | python3 -m json.tool

# Test Quay token exchange manually (from your workstation)
QUAY_AUTH=$(oc get secret additional-pull-secret -n kube-system -o jsonpath='{.data.\.dockerconfigjson}' | base64 -d | python3 -c "import sys,json; d=json.load(sys.stdin); print(d['auths'].get('quay.io/rhoai',{}).get('auth',''))")
curl -s -u "$(echo $QUAY_AUTH | base64 -d)" "https://quay.io/v2/auth?service=quay.io&scope=repository:rhoai/rhoai-fbc-fragment:pull"
```

### Common Causes and Fixes

**Pull secret expired or missing**
- The `additional-pull-secret` in `kube-system` contains the Quay robot account credentials. If the robot account token was rotated in Quay, update it:
  1. Get the new credentials from Bitwarden Enterprise ("Openshift AI devel" collection).
  2. Use the app's Setup page to update the pull secret, or manually:
     ```bash
     oc patch secret additional-pull-secret -n kube-system --type merge \
       -p '{"data":{".dockerconfigjson":"'$(echo -n '{"auths":{"quay.io/rhoai":{"auth":"<base64-user:pass>"}}}' | base64)'"}}'
     ```

**Quay rate limiting**
- Quay v2 API has rate limits. The app uses a 5-minute tag scan cache (`tagScanCacheTTL`). If many users hit Build Explorer simultaneously, the cache prevents hammering Quay. Wait a few minutes and retry.

**Network/firewall**
- The pod needs outbound HTTPS access to `quay.io`. Check if a NetworkPolicy or egress firewall is blocking it:
  ```bash
  oc get networkpolicy -n rhoai-nightly-updater
  oc exec deploy/rhoai-nightly-updater -c app -n rhoai-nightly-updater -- wget -q -O- --timeout=5 https://quay.io/v2/ 2>&1 || echo "Cannot reach quay.io"
  ```

---

## 4. GitHub API Rate Limit (Merged Column Showing "-")

### Symptoms
- The "Merged" date column in the Components page shows "-" for all deployments.
- App logs show `403` responses from `api.github.com`.

### Diagnosis

```bash
# Check rate limit from the pod's perspective
oc exec deploy/rhoai-nightly-updater -c app -n rhoai-nightly-updater -- \
  wget -q -O- https://api.github.com/rate_limit 2>/dev/null | python3 -m json.tool

# Check app logs for GitHub errors
oc logs -l app=rhoai-nightly-updater -c app -n rhoai-nightly-updater --tail=50 | grep -i github
```

### Details

- The app fetches commit dates from GitHub to populate the "Merged" column on the Components page.
- GitHub allows 60 unauthenticated requests per hour per IP. The pod shares its node's outbound IP with other workloads.
- There is no GitHub token configured -- all requests are unauthenticated.

### Fix

- This is expected behavior. The merged dates will populate as the rate limit resets (hourly).
- If you need the data immediately, you can look up the commit on GitHub manually using the Git Commit SHA shown in the Components page.
- To permanently fix this, add a `GITHUB_TOKEN` environment variable to the Deployment with a GitHub PAT (read-only, no scopes needed). This raises the limit to 5,000/hr. (Requires a code change to use the token in API calls.)

---

## 5. Components Page Showing Wrong DSC Components

### Symptoms
- The Components page shows unexpected component names, missing components, or "no DataScienceCluster found" error.
- Switching between RHOAI versions changes the component list.

### Diagnosis

```bash
# Check which DSC API versions are available
oc api-resources | grep datasciencecluster

# Get the DSC directly
oc get datascienceclusters -A -o yaml

# Check what the app sees (v2 first, then v1 fallback)
oc get datascienceclusters.v2.datasciencecluster.opendatahub.io -A 2>/dev/null || echo "v2 not available"
oc get datascienceclusters.v1.datasciencecluster.opendatahub.io -A 2>/dev/null || echo "v1 not available"
```

### Details

- The app queries the DSC API at `/apis/datasciencecluster.opendatahub.io/v2/datascienceclusters` first.
- If v2 returns an error (older RHOAI versions), it falls back to `/apis/datasciencecluster.opendatahub.io/v1/datascienceclusters`.
- The v2 schema may have different component names or structure than v1.

### Fix

- After an update or rollback, the DSC CRD version can change. The app handles this automatically on the next API call.
- If the DSC CRD has a conversion webhook that is broken (common during rollback), the API call will fail. Check for conversion webhook errors:
  ```bash
  oc get crd datascienceclusters.datasciencecluster.opendatahub.io -o jsonpath='{.spec.conversion}'
  ```
  If the webhook service is gone, patch the CRD:
  ```bash
  oc patch crd datascienceclusters.datasciencecluster.opendatahub.io --type merge \
    -p '{"spec":{"conversion":{"strategy":"None"}}}'
  ```

---

## 6. Update/Reinstall Stuck or Failed

### Symptoms
- The app reports "Update initiated" but the operator never finishes installing.
- Status page shows CatalogSource in `TRANSIENT_FAILURE` state.
- CSV stays in `Pending` or `InstallReady` phase for more than 10 minutes.

### Diagnosis

```bash
# CatalogSource status
oc get catalogsource rhoai-catalog-dev -n openshift-marketplace -o yaml

# Catalog pod (runs the FBC image)
oc get pods -n openshift-marketplace -l olm.catalogSource=rhoai-catalog-dev

# Catalog pod logs (FBC image extraction errors appear here)
oc logs -l olm.catalogSource=rhoai-catalog-dev -n openshift-marketplace --tail=50

# Subscription status
oc get subscription rhods-operator -n redhat-ods-operator -o yaml

# InstallPlans
oc get installplans -n redhat-ods-operator

# CSV status
oc get csv -n redhat-ods-operator | grep -i rhods

# Check for pending InstallPlans that need approval
oc get installplans -n redhat-ods-operator -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.approval}{"\t"}{.status.phase}{"\n"}{end}'
```

### Common Causes and Fixes

**CatalogSource in TRANSIENT_FAILURE**
- The FBC image cannot be pulled or is invalid. Check the catalog pod:
  ```bash
  oc describe pod -l olm.catalogSource=rhoai-catalog-dev -n openshift-marketplace
  ```
- If ImagePullBackOff: the pull secret or IDMS is not configured. Verify:
  ```bash
  oc get secret additional-pull-secret -n kube-system
  oc get imagedigestmirrorsets
  ```

**OLM not reconciling (same version rebuild)**
- When the nightly build has the same CSV version string but different image contents, OLM will not create a new InstallPlan. The app handles this by deleting the CSV and Subscription and recreating them. If this still does not work, use the "Reinstall" button in the UI, which does a full cleanup cycle (delete CatalogSource, Subscription, CSV, stale webhooks, patch CRD conversion webhooks, wait, then recreate).

**SSE streaming and the update modal**
- Updates, rollbacks, and refreshes now use Server-Sent Events (SSE) to stream pipeline progress in real time. The modal dialog closes immediately after the operation is initiated; progress is shown in the pipeline progress card on the Status page.
- If the SSE connection drops (network blip, browser tab backgrounded, proxy timeout), the operation continues running on the server. The frontend falls back to polling `/api/status` to track completion. No user action is required.

**"Assist Rollout" button**
- After an update is initiated, if the operator deployment appears stuck for more than 90 seconds, an "Assist Rollout" button appears on the Status page. Clicking it calls `POST /api/assist-rollout`, which inspects the rollout state and attempts to unblock it (e.g., by deleting a stuck ReplicaSet or restarting the deployment).

**Channel detection uses FBC image tag**
- Channel detection now parses the FBC (File-Based Catalog) image tag to determine the correct OLM channel for the Subscription. If the channel shown in the UI looks wrong after an update, verify the FBC image tag on the CatalogSource:
  ```bash
  oc get catalogsource rhoai-catalog-dev -n openshift-marketplace -o jsonpath='{.spec.image}'
  ```
  The tag encodes the version and channel information (e.g., `v2.19.0-...` maps to `stable-2.19`).

**Manual recovery**
- If all else fails, do a clean reinstall to stable:
  ```bash
  # Delete nightly catalog
  oc delete catalogsource rhoai-catalog-dev -n openshift-marketplace --ignore-not-found

  # Delete subscription
  oc delete subscription rhods-operator -n redhat-ods-operator --ignore-not-found

  # Delete CSV
  oc delete csv -n redhat-ods-operator -l operators.coreos.com/rhods-operator.redhat-ods-operator=

  # Clean up stale webhooks
  oc delete validatingwebhookconfigurations -l olm.owner 2>/dev/null
  oc get validatingwebhookconfigurations -o name | grep -E 'opendatahub|rhods' | xargs oc delete --ignore-not-found
  oc get mutatingwebhookconfigurations -o name | grep -E 'opendatahub|rhods' | xargs oc delete --ignore-not-found

  # Patch CRD conversion webhooks
  oc patch crd datascienceclusters.datasciencecluster.opendatahub.io --type merge -p '{"spec":{"conversion":{"strategy":"None"}}}'
  oc patch crd dscinitializations.dscinitialization.opendatahub.io --type merge -p '{"spec":{"conversion":{"strategy":"None"}}}'

  # Wait for cleanup
  sleep 15

  # Recreate subscription to stable
  cat <<EOF | oc apply -f -
  apiVersion: operators.coreos.com/v1alpha1
  kind: Subscription
  metadata:
    name: rhods-operator
    namespace: redhat-ods-operator
  spec:
    channel: stable-3.4
    installPlanApproval: Automatic
    name: rhods-operator
    source: redhat-operators
    sourceNamespace: openshift-marketplace
  EOF
  ```

---

## 7. Metrics Not Showing in Prometheus

### Symptoms
- The `rhoai_nightly_updater_*` metrics do not appear in the Prometheus/Thanos UI.
- The `NightlyUpdaterDown` alert is firing even though the app is running.
- The Prometheus targets page shows the target as `DOWN`.

### Diagnosis

```bash
# Check that the ServiceMonitor exists
oc get servicemonitor rhoai-nightly-updater -n rhoai-nightly-updater

# Verify the Service has the correct label that the ServiceMonitor selects
oc get svc rhoai-nightly-updater -n rhoai-nightly-updater -o jsonpath='{.metadata.labels}'
# Must include: app.kubernetes.io/name=rhoai-nightly-updater

# Verify the metrics port (8080) is exposed on the Service
oc get svc rhoai-nightly-updater -n rhoai-nightly-updater -o jsonpath='{.spec.ports[*]}'

# Check if user-workload monitoring is enabled
oc get configmap cluster-monitoring-config -n openshift-monitoring -o yaml 2>/dev/null | grep enableUserWorkload

# Test the metrics endpoint directly from inside the cluster
oc exec deploy/rhoai-nightly-updater -c app -n rhoai-nightly-updater -- wget -q -O- http://localhost:8080/metrics
```

### Common Causes and Fixes

**NetworkPolicy blocking Prometheus scrape**
- The NetworkPolicy allows port 8080 only from namespaces `openshift-monitoring` and `openshift-user-workload-monitoring`. Verify the namespace labels match:
  ```bash
  oc get ns openshift-monitoring -o jsonpath='{.metadata.labels.kubernetes\.io/metadata\.name}'
  oc get ns openshift-user-workload-monitoring -o jsonpath='{.metadata.labels.kubernetes\.io/metadata\.name}'
  ```

**User workload monitoring not enabled**
- ServiceMonitor resources in user namespaces require user-workload monitoring. Enable it:
  ```bash
  oc apply -f - <<EOF
  apiVersion: v1
  kind: ConfigMap
  metadata:
    name: cluster-monitoring-config
    namespace: openshift-monitoring
  data:
    config.yaml: |
      enableUserWorkload: true
  EOF
  ```
  Wait 1-2 minutes for the user-workload-monitoring stack to deploy.

**ServiceMonitor not matching Service labels**
- The ServiceMonitor selects `app.kubernetes.io/name: rhoai-nightly-updater`. If the Service label is missing or different, Prometheus will not discover the target:
  ```bash
  oc label svc rhoai-nightly-updater -n rhoai-nightly-updater app.kubernetes.io/name=rhoai-nightly-updater --overwrite
  ```

---

## 8. ConfigMap Activity Log Full or Corrupted

### Symptoms
- Activity page shows no entries or an error.
- App logs show `warning: failed to parse activity entries` or `warning: failed to save activity configmap`.

### Diagnosis

```bash
# Check the ConfigMap
oc get configmap rhoai-nightly-updater-activity -n rhoai-nightly-updater -o yaml

# Check its size
oc get configmap rhoai-nightly-updater-activity -n rhoai-nightly-updater -o json | wc -c

# Verify the entries field is valid JSON
oc get configmap rhoai-nightly-updater-activity -n rhoai-nightly-updater -o jsonpath='{.data.entries}' | python3 -m json.tool
```

### Details

- The activity log is stored as a JSON array in the `entries` key of ConfigMap `rhoai-nightly-updater-activity`.
- The app auto-trims to the most recent 20 entries on every write.
- ConfigMaps have a 1MB size limit in Kubernetes. With 20 entries, this is never reached in practice (each entry is ~200 bytes).

### Fix

**Corrupted JSON**
- If the `entries` field contains invalid JSON, delete the ConfigMap. The app will recreate it on the next operation:
  ```bash
  oc delete configmap rhoai-nightly-updater-activity -n rhoai-nightly-updater
  ```

**ConfigMap missing**
- This is normal on first deployment. The app creates it automatically on the first update/rollback/reinstall operation.

---

## 9. Pod Restarts Frequently

### Symptoms
- `oc get pods` shows a high restart count.
- The `NightlyUpdaterPodRestarting` alert fires (>3 restarts in 1 hour).

### Diagnosis

```bash
# Check restart count and reason
oc get pods -n rhoai-nightly-updater -o wide

# Look at the previous container's logs
oc logs -l app=rhoai-nightly-updater -c app -n rhoai-nightly-updater --previous --tail=50

# Check for OOMKilled
oc get pods -n rhoai-nightly-updater -o jsonpath='{range .items[*].status.containerStatuses[*]}{.name}{"\t"}{.lastState.terminated.reason}{"\n"}{end}'

# Check events for the namespace
oc get events -n rhoai-nightly-updater --sort-by='.lastTimestamp' | tail -20
```

### Common Causes and Fixes

**OOMKilled (128Mi limit)**
- The app container has a 128Mi memory limit. Under heavy concurrent Quay API calls (Build Explorer loading all tags with labels), memory can spike. Increase the limit:
  ```bash
  oc set resources deployment/rhoai-nightly-updater -n rhoai-nightly-updater \
    -c app --limits=memory=256Mi
  ```

**Liveness probe failure**
- The liveness probe hits `GET /api/health` on port 8080 every 30 seconds with a 3-second timeout. If the app is busy (long-running Quay API calls), it might not respond in time. Check if the health endpoint works:
  ```bash
  oc exec deploy/rhoai-nightly-updater -c app -n rhoai-nightly-updater -- wget -q -O- --timeout=3 http://localhost:8080/api/health
  ```
  The health endpoint only checks that the SA token file exists -- it does not make API calls. If it is timing out, the app is likely stuck or out of memory.

**readOnlyRootFilesystem violations**
- If a code change introduced a write to the filesystem (e.g., temp files), the container will crash. Check logs for `read-only file system` errors and fix the code to use in-memory buffers instead.

**Startup probe failure**
- The startup probe allows 10 failures x 3 seconds = 30 seconds for the app to start. If the container image is large and pull takes >30s, the pod will be killed before it starts. Increase `failureThreshold`:
  ```bash
  oc patch deployment rhoai-nightly-updater -n rhoai-nightly-updater --type json \
    -p '[{"op":"replace","path":"/spec/template/spec/containers/0/startupProbe/failureThreshold","value":20}]'
  ```

---

## 10. ConsoleLink Showing Wrong URL

### Symptoms
- The "RHOAI Nightly Updater" link in the OpenShift console Application Menu points to the wrong URL or the placeholder URL.

### Diagnosis

```bash
# Check the current ConsoleLink
oc get consolelink rhoai-nightly-updater -o jsonpath='{.spec.href}'
echo

# Get the actual Route URL
oc get route rhoai-nightly-updater -n rhoai-nightly-updater -o jsonpath='https://{.spec.host}'
echo
```

### Fix

```bash
# Get the correct URL from the Route
ROUTE_URL=$(oc get route rhoai-nightly-updater -n rhoai-nightly-updater -o jsonpath='https://{.spec.host}')

# Patch the ConsoleLink
oc patch consolelink rhoai-nightly-updater --type merge -p "{\"spec\":{\"href\":\"${ROUTE_URL}\"}}"

# Verify
oc get consolelink rhoai-nightly-updater -o jsonpath='{.spec.href}'
echo
```

---

## 11. Dashboard Stuck on PR Image

On installations with `dashboard-operator`, use **Dashboard Dev → Revert to default**. This resumes the saved operator replica count and waits for all dashboard modules to return to their release images. A failed or interrupted deploy retains the recovery annotation on `dashboard-operator`, so the same action works after an updater restart. To inspect the recovery state:

```bash
oc get deployment dashboard-operator -n redhat-ods-applications -o json \
  | jq '{replicas:.spec.replicas,session:.metadata.annotations["rhoai-nightly-updater.opendatahub.io/dashboard-dev"]}'
```

The manual image restoration instructions below apply to older installations without dashboard-operator.

### Symptoms
- Dashboard shows an old or broken PR build instead of the operator-managed image.
- The Dashboard Dev page in the app shows "PR deployed" state but the revert button does not work or was never clicked.

### Diagnosis

```bash
# Check the current dashboard image
oc get deployment rhods-dashboard -n redhat-ods-applications -o jsonpath='{.spec.template.spec.containers[0].image}'
echo

# Check whether operator reconciliation is disabled
oc get deployment rhods-dashboard -n redhat-ods-applications -o jsonpath='{.metadata.annotations.opendatahub\.io/managed}'
echo
# "false" means operator reconciliation is disabled (PR image will persist)
# "true" or empty means operator is managing the deployment normally
```

### Fix

Get the original image from the operator's environment and patch both the image and annotation:

```bash
# Get the operator-managed image
ORIGINAL_IMAGE=$(oc get deployment rhods-operator -n redhat-ods-operator \
  -o jsonpath='{range .spec.template.spec.containers[*].env[*]}{.name}={.value}{"\n"}{end}' \
  | grep RELATED_IMAGE_ODH_DASHBOARD_IMAGE | cut -d= -f2)
echo "Original image: $ORIGINAL_IMAGE"

# Patch the deployment: restore image + re-enable operator management
oc patch deployment rhods-dashboard -n redhat-ods-applications --type=strategic \
  -p "{\"metadata\":{\"annotations\":{\"opendatahub.io/managed\":\"true\"}},\"spec\":{\"template\":{\"spec\":{\"containers\":[{\"name\":\"rhods-dashboard\",\"image\":\"$ORIGINAL_IMAGE\"}]}}}}"
```

Verify the image is restored:

```bash
oc get deployment rhods-dashboard -n redhat-ods-applications \
  -o jsonpath='{.spec.template.spec.containers[?(@.name=="rhods-dashboard")].image}'
echo
```

---

## 12. MLflow Not Deploying

### Symptoms
- The Resources tab shows MLflow as "Not deployed" even after clicking Setup.
- MLflow CR exists but status shows "Not ready" or "Provisioning" indefinitely.
- App logs show `Failed to create MLflow CR` errors.

### Diagnosis

```bash
# Check if the MLflow CRD exists (operator must support it)
oc api-resources | grep mlflow

# Check the MLflow CR
oc get mlflows -A

# Check MLflow CR status conditions
oc get mlflow mlflow -o yaml 2>/dev/null || echo "MLflow CR not found"

# Check MLflow pods in redhat-ods-applications
oc get pods -n redhat-ods-applications -l app=mlflow

# Check MLflow deployment
oc get deployment -n redhat-ods-applications -l app=mlflow

# Check MLflow route
oc get route mlflow -n redhat-ods-applications

# App logs for MLflow errors
oc logs -l app=rhoai-nightly-updater -c app -n rhoai-nightly-updater --tail=50 | grep -i mlflow
```

### Common Causes and Fixes

**MLflow CRD not installed**
- The MLflow CR requires the MLflow operator to be installed as part of RHOAI. If `oc api-resources | grep mlflow` returns nothing, the current RHOAI version does not include the MLflow operator. Upgrade to a nightly build that includes it.

**MLflow CR stuck in Provisioning**
- The operator may be waiting for resources (PVC, database). Check events:
  ```bash
  oc get events -n redhat-ods-applications --sort-by='.lastTimestamp' | grep -i mlflow | tail -10
  ```
- Check PVC status:
  ```bash
  oc get pvc -n redhat-ods-applications -l app=mlflow
  ```
  If PVC is `Pending`, the storage class may not support `ReadWriteOnce` or the cluster is out of storage.

**MLflow PR image not found**
- When deploying a PR image, the app verifies the image exists on Quay (`quay.io/opendatahub/mlflow:odh-pr-<N>`). If the PR CI has not published an image, the deploy will fail with "PR image not found". Wait for CI to complete and retry.

**Permission denied on MLflow CR**
- The SA needs `mlflow.opendatahub.io` permissions. Verify the ClusterRole:
  ```bash
  oc get clusterrole rhoai-nightly-updater -o yaml | grep -A3 mlflow
  ```
  Must include `mlflows` with verbs `get, list, create, update, patch, delete`.

---

## 13. Pipeline Server Setup Fails

### Symptoms
- Clicking "Add Pipeline Server" returns an error.
- DSPA CR exists but pipeline server never becomes ready.
- Error message says "MinIO is not deployed or not ready."

### Diagnosis

```bash
# Check MinIO is running (prerequisite for pipeline server)
oc get pods -n minio -l app=minio
oc get deployment minio -n minio

# Check the DSPA in the target project
oc get datasciencepipelinesapplications -n <project-name>

# Check DSPA status conditions
oc get dspa dspa -n <project-name> -o yaml 2>/dev/null

# Check pipeline server pods
oc get pods -n <project-name> -l app=ds-pipeline-dspa

# Check the DSPA secret
oc get secret dashboard-dspa-secret -n <project-name> -o yaml

# App logs for pipeline server errors
oc logs -l app=rhoai-nightly-updater -c app -n rhoai-nightly-updater --tail=50 | grep -i pipeline
```

### Common Causes and Fixes

**MinIO not ready**
- Pipeline servers require MinIO for artifact storage. Set up MinIO first via the Resources tab. Verify MinIO is running:
  ```bash
  oc get deployment minio -n minio -o jsonpath='{.status.readyReplicas}'
  ```
  Must return `1`.

**DSPA stuck in not-ready state**
- The DSPA operator may be waiting for the S3 endpoint to become reachable. Verify the MinIO service is accessible from the project namespace:
  ```bash
  oc exec -n <project-name> deploy/ds-pipeline-dspa -- wget -q -O- --timeout=5 http://minio-service.minio.svc:9000/minio/health/live 2>&1 || echo "Cannot reach MinIO"
  ```

**DSPA secret missing or wrong credentials**
- The pipeline server secret (`dashboard-dspa-secret`) must contain `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY` matching the MinIO credentials. If MinIO was torn down and recreated with different credentials, delete the stale secret and re-run setup:
  ```bash
  oc delete secret dashboard-dspa-secret -n <project-name> --ignore-not-found
  ```
  Then click "Add Pipeline Server" again in the UI.

**DSPA CRD not installed**
- The `datasciencepipelinesapplications.opendatahub.io` CRD must exist. If `oc api-resources | grep datasciencepipelinesapplication` returns nothing, the Data Science Pipelines component is not enabled in the DSC. Check:
  ```bash
  oc get datascienceclusters -A -o yaml | grep -A2 datasciencepipelines
  ```

**Cannot delete MinIO while pipeline servers exist**
- The app prevents MinIO teardown while pipeline servers are still running. Tear down all pipeline servers first, then tear down MinIO.

---

## 14. Rate Limited After Dry Run

Dry runs and real updates share the same server-side rate limit window (30 seconds). If you run a dry run and then immediately click "Update", the request may be rejected with a rate limit error. Wait for the 30-second cooldown to expire before retrying. The error message includes a "Retry-After" hint with the remaining seconds.

In addition to the time-based rate limit, all cluster-mutating operations (update, rollback, refresh, assist-rollout, deploy-pr, revert, setup, teardown) are protected by a **cluster mutation lock**. Only one mutation can run at a time. If a second mutation is attempted while one is already in progress, the server returns **HTTP 409 Conflict** with the message `"another cluster mutation is already in progress"`. Wait for the current operation to finish (watch the pipeline progress card or poll `/api/status`) and retry.

---

## 15. SSE Stream Connection Issues

### Symptoms
- The pipeline progress card freezes mid-step (e.g., "Deleting CSV..." with no further updates).
- Browser console shows `EventSource` connection timeout or error.
- The SSE stream endpoint (`/api/update/stream`, `/api/rollback/stream`, or `/api/refresh/stream`) returns no data or closes prematurely.

### Diagnosis

1. **Check the browser Network tab.** Open DevTools > Network and filter by "EventStream" or the `/stream` path. Look for:
   - HTTP status (should be 200 with `Content-Type: text/event-stream`).
   - Whether events were received before the connection dropped.
   - Whether the browser shows a `net::ERR_INCOMPLETE_CHUNKED_ENCODING` or similar network error.

2. **Check pod logs for the operation.**
   ```bash
   oc logs -l app=rhoai-nightly-updater -c app -n rhoai-nightly-updater --tail=100 | grep -E 'mutation|pipeline|stream'
   ```
   If the operation started successfully, you will see log lines for each pipeline step even if the SSE connection was lost.

3. **Check if the Route has a timeout annotation.** Long-running SSE connections may be terminated by the OpenShift Router if the timeout is too short:
   ```bash
   oc get route rhoai-nightly-updater -n rhoai-nightly-updater -o jsonpath='{.metadata.annotations.haproxy\.router\.openshift\.io/timeout}'
   ```
   If empty or less than `300s`, the router may close the connection during lengthy operations (reinstall can take several minutes).

### Fix

- **The operation continues on the server regardless of the SSE connection state.** If the stream drops, the frontend automatically falls back to polling `/api/status`. Simply refresh the page and check the Status page for current progress.
- If you suspect the Route timeout is causing premature disconnects, increase it:
  ```bash
  oc annotate route rhoai-nightly-updater -n rhoai-nightly-updater \
    haproxy.router.openshift.io/timeout=300s --overwrite
  ```
- If the operation itself is stuck (not just the stream), see Section 6 (Update/Reinstall Stuck or Failed).

---

## 16. Dashboard PR Deploy Shows Wrong Status

### Symptoms
- The Dashboard Dev page shows "Running" status but the dashboard still serves the old build.
- The PR image tag in the UI does not match what is actually running in the pod.
- After deploying a PR image, the dashboard behaves as if it is still on the operator-managed image.

### Diagnosis

```bash
# Check what image the Deployment spec requests
oc get deployment rhods-dashboard -n redhat-ods-applications \
  -o jsonpath='{.spec.template.spec.containers[?(@.name=="rhods-dashboard")].image}'
echo

# Check what image the running pod is actually using
oc get pods -n redhat-ods-applications -l app.kubernetes.io/part-of=rhods-dashboard \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.status.containerStatuses[?(@.name=="rhods-dashboard")].image}{"\n"}{end}'

# Check rollout status
oc rollout status deployment/rhods-dashboard -n redhat-ods-applications --timeout=10s
```

If the Deployment spec shows the new PR image but the running pod still uses the old image, the rollout is in progress or stuck.

### Fix

- **Wait for the rollout to complete.** Kubernetes replaces pods gradually. Give it 1-2 minutes for the new ReplicaSet to scale up and the old one to scale down.
- **If stuck (rollout does not progress for >90 seconds),** use the "Assist Rollout" button on the Status page, or call the endpoint directly:
  ```bash
  # From your workstation (through the Route, requires auth cookie):
  curl -X POST https://<route-host>/api/assist-rollout -H "Cookie: <auth-cookie>"

  # From inside the cluster (bypasses oauth-proxy, port 8080):
  oc exec deploy/rhoai-nightly-updater -c app -n rhoai-nightly-updater -- \
    wget -q -O- --post-data='' http://localhost:8080/api/assist-rollout
  ```
- **If the operator keeps reverting the image,** check that the `opendatahub.io/managed` annotation is set to `"false"` on the dashboard Deployment. The app sets this automatically when deploying a PR image, but if the operator was restarted it may have reconciled before the annotation took effect:
  ```bash
  oc get deployment rhods-dashboard -n redhat-ods-applications \
    -o jsonpath='{.metadata.annotations.opendatahub\.io/managed}'
  ```
  Must return `false`. If not, re-deploy the PR image through the UI or see Section 11 for manual patching.

---

## Quick Reference: Key Resources

| Resource | Namespace | Name |
|----------|-----------|------|
| Deployment | `rhoai-nightly-updater` | `rhoai-nightly-updater` |
| ServiceAccount | `rhoai-nightly-updater` | `rhoai-nightly-updater` |
| ClusterRole | (cluster-scoped) | `rhoai-nightly-updater` |
| ClusterRoleBinding | (cluster-scoped) | `rhoai-nightly-updater` |
| Service | `rhoai-nightly-updater` | `rhoai-nightly-updater` (ports: 8443 https, 8080 metrics) |
| Route | `rhoai-nightly-updater` | `rhoai-nightly-updater` |
| NetworkPolicy | `rhoai-nightly-updater` | `rhoai-nightly-updater` |
| TLS Secret | `rhoai-nightly-updater` | `rhoai-nightly-updater-tls` (auto-generated) |
| ServiceMonitor | `rhoai-nightly-updater` | `rhoai-nightly-updater` |
| PrometheusRule | `rhoai-nightly-updater` | `rhoai-nightly-updater` |
| ConsoleLink | (cluster-scoped) | `rhoai-nightly-updater` |
| ConfigMap (activity) | `rhoai-nightly-updater` | `rhoai-nightly-updater-activity` |
| Pull Secret | `kube-system` | `additional-pull-secret` |
| CatalogSource (nightly) | `openshift-marketplace` | `rhoai-catalog-dev` |
| Subscription | `redhat-ods-operator` | `rhods-operator` |

## Quick Reference: Prometheus Alerts

| Alert | Condition | Severity |
|-------|-----------|----------|
| `NightlyUpdaterDown` | `up{job="rhoai-nightly-updater"} == 0` for 5m | warning |
| `NightlyUpdateFailed` | Any failed update in the last hour | warning |
| `NightlyUpdaterHighErrorRate` | >50% update failure rate over 10m | critical |
| `NightlyUpdaterPodRestarting` | >3 container restarts in 1 hour | warning |

## Quick Reference: App Endpoints

For the full list of API endpoints, see the API Endpoints section in README.md.
