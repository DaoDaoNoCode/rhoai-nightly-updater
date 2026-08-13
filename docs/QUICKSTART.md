# RHOAI Nightly Updater — Quick Start Guide

Deploy the RHOAI Nightly Updater on your ROSA HCP (or OpenShift) cluster in about 10 minutes. Once running, you can install, update, and manage RHOAI nightly builds from a web UI — no `oc` commands needed for day-to-day use.

## Prerequisites

- A ROSA HCP or OpenShift cluster (OCP 4.14+)
- `oc` CLI logged into the cluster (`oc login ...`)
- A Quay.io pull token for `quay.io/rhoai` — get it from [Bitwarden](https://vault.bitwarden.com/#/vault?collectionId=75f54536-fa36-4ef9-8f1a-b09701646cac&itemId=e6e1fdde-6601-4e8b-8154-b211005518a1) (Openshift AI devel collection). No access? Request in [#rhoai-devtestops-requests](https://redhat.enterprise.slack.com/archives/C07TF3MBMMW).

---

## Fresh Cluster Setup (First Time Only)

Follow Steps 1–8 below. After that, see [Day-to-Day Usage](#day-to-day-usage).

### Step 1: Deploy the App

```bash
OCP_VERSION=$(oc version -o json | sed -n 's/.*"openshiftVersion": "\([0-9]*\.[0-9]*\).*/\1/p')
echo "Detected OCP version: $OCP_VERSION"

oc new-project rhoai-nightly-updater

oc process -f deploy/template.yaml \
  -p IMAGE=quay.io/juntao_wang/rhoai-nightly-updater:latest \
  -p OAUTH_PROXY_IMAGE=registry.redhat.io/openshift4/ose-oauth-proxy-rhel9:v${OCP_VERSION} \
  -p NAMESPACE=rhoai-nightly-updater | oc apply -f -

oc rollout status deployment/rhoai-nightly-updater -n rhoai-nightly-updater --timeout=120s
```

### Step 2: Get the App URL

```bash
ROUTE_URL=$(oc get route rhoai-nightly-updater -n rhoai-nightly-updater -o jsonpath='https://{.spec.host}')
echo "Open: $ROUTE_URL"

oc patch consolelink rhoai-nightly-updater --type merge -p "{\"spec\":{\"href\":\"$ROUTE_URL\"}}"
```

Open the URL in your browser. You'll be prompted to log in via OpenShift SSO. The app also appears in the OpenShift Console app launcher (grid icon).

### Step 3: Run the Smoke Test (optional)

```bash
./scripts/smoke-test.sh
```

Verifies pods, routes, RBAC, health probes, observability, and oauth-proxy config.

### Step 4: Configure Pull Secret

In the app UI:

1. You'll see a **One-Time Cluster Setup** section at the top
2. Find the **Pull Secret** card (shows "Missing")
3. Paste your `quay.io/rhoai` base64 auth token into the input field
4. Click **Create Secret**

The Bitwarden and Slack request links are directly in the card.

### Step 5: Create the Image Digest Mirror Set (IDMS)

Click **View setup instructions** in the Image Mirror card, or run manually:

You need to be on the **Red Hat VPN** and have ROSA CLI access (separate from `oc`):

```bash
kinit <your-username>@IPA.REDHAT.COM
rh-aws-saml-login                # select iaps-rhods-odh-dev
rosa login --use-auth-code
rosa whoami                       # verify access
```

Then create the image mirror:

```bash
rosa create image-mirror --cluster=<your-cluster-name> \
  --source=registry.redhat.io/rhoai \
  --mirrors=quay.io/rhoai
```

> If you get a 403, you need OCM write access. Find the cluster owner and ask them to assign you access in OCM.

**For self-managed OpenShift** (not ROSA), apply the IDMS directly:

```bash
cat <<EOF | oc apply -f -
apiVersion: config.openshift.io/v1
kind: ImageDigestMirrorSet
metadata:
  name: rhoai-quay-mirror
spec:
  imageDigestMirrors:
    - source: registry.redhat.io/rhoai
      mirrors:
        - quay.io/rhoai
EOF
```

> Wait **2–3 minutes** after this step for credentials to propagate to all cluster nodes.

### Step 6: Install the Nightly Build

After the setup section disappears (both prerequisites green):

1. The **Install Nightly Build** card appears with all preflight checks green
2. The latest nightly image is auto-filled, or paste one from [#rhoai-build-notifications](https://redhat.enterprise.slack.com/archives/C07ANR2U56C)
3. (Optional) Click **Dry Run** to preview changes without modifying anything
4. Click **Update** to install
5. Watch the live pipeline progress — each step streams in real time

The tool automatically creates the `redhat-ods-operator` namespace, OperatorGroup, CatalogSource, Subscription, and waits for the InstallPlan. This typically takes 2–5 minutes.

### Step 7: Create the DataScienceCluster

After the operator installs, a **DataScienceCluster Required** card appears:

1. Click **Create DataScienceCluster**
2. The tool creates a `default-dsc` with all standard components enabled

The DSC tells the operator which components to deploy (dashboard, pipelines, model serving, etc.). Components can be toggled later from the **Components** page.

### Step 8: Verify

1. Go to the **Components** page — you should see deployments appearing in `redhat-ods-applications`
2. Go to the **Diagnostics** page — run a scan, all checks should pass
3. The operator card on the Status page should show a green "Succeeded" label

Your cluster is now ready.

---

## Existing Cluster (RHOAI Already Installed)

If your cluster already has RHOAI installed (stable or nightly) and you want to use the Nightly Updater to manage updates:

1. **Deploy the app** — follow Steps 1–3 above (same for all clusters)
2. **Check prerequisites** — open the app and look at the status cards:
   - **Pull Secret**: If you already have credentials for `quay.io/rhoai`, it should show "Ready". If not, configure it (Step 4).
   - **Image Mirror (IDMS)**: If you already have an IDMS for `registry.redhat.io/rhoai`, it should show "Ready". If not, create it (Step 5).
3. **Start using it** — the app detects your existing operator:
   - The card says **"Upgrade to Nightly Build"** (not "Install")
   - Your current operator version, source, and channel are displayed
   - The **Reinstall Operator** card is available for switching between stable and nightly
   - The **Refresh operator** button re-pulls images from the current catalog
4. **Skip DSC creation** — your existing DataScienceCluster is preserved. The DSC prompt only appears if no DSC exists.

The app never modifies your existing DSC or DSCI. It only manages the operator lifecycle (CatalogSource, Subscription, CSV).

---

## Day-to-Day Usage

Once RHOAI is installed, the Status page changes to show the operator status and daily workflows. The one-time setup section is replaced by a collapsible "Cluster setup" link for checking/updating prerequisites.

| Task | How |
|------|-----|
| **Update to a new nightly** | Status page → enter image or click "Fetch latest" → Update |
| **Refresh current nightly** | Status page → "Refresh operator" (re-pulls same version with latest images) |
| **Rollback to stable** | Status page → Reinstall → select "Stable" → Reinstall Operator |
| **Switch to a different nightly** | Status page → Reinstall → select "Nightly" → pick version → Reinstall |
| **Deploy a Dashboard PR** | Dashboard Dev page → enter PR number → Deploy |
| **Deploy an MLflow PR** | Dashboard Dev page → Resources tab → enter PR number → Deploy |
| **Set up MinIO + Pipelines** | Dashboard Dev page → Resources tab → Set up MinIO → Add Pipeline Server |
| **Set up MLflow** | Dashboard Dev page → Resources tab → Set up MLflow |
| **Troubleshoot issues** | Diagnostics page → Run Scan → use auto-fix buttons |
| **Explore available builds** | Build Explorer page → browse tags, inspect FBC contents |
| **Check/update pull secret** | Status page → expand "Cluster setup" → Test / Update |

---

## Uninstall

```bash
oc delete consolelink rhoai-nightly-updater
oc delete project rhoai-nightly-updater
```

This removes only the nightly updater app. It does **not** remove the RHOAI operator or any resources it created.

---

## Troubleshooting

| Symptom | Fix |
|---------|-----|
| Pod stuck in `ImagePullBackOff` | Check the oauth-proxy image tag matches your OCP version: `oc get deployment rhoai-nightly-updater -n rhoai-nightly-updater -o jsonpath='{.spec.template.spec.containers[?(@.name=="oauth-proxy")].image}'` |
| Pod stuck in `Pending` | Check node resources: `oc describe pod -n rhoai-nightly-updater -l app=rhoai-nightly-updater` |
| 403 after login | Your user needs permission to list pods in `redhat-ods-operator` namespace |
| "Cluster token not available" | ServiceAccount token not mounted — check `oc get sa rhoai-nightly-updater -n rhoai-nightly-updater` |
| Update stuck on "Waiting for catalog" | The catalog pod is starting — wait 2 minutes, check Diagnostics page |
| Images fail to pull after IDMS | Credential propagation takes 2–3 minutes — wait and retry |
| Title still says "Upgrade" after deploy | Hard-refresh the browser (Cmd+Shift+R) to clear cached frontend |
| ConsoleLink shows placeholder | Run: `oc patch consolelink rhoai-nightly-updater --type merge -p "{\"spec\":{\"href\":\"$(oc get route rhoai-nightly-updater -n rhoai-nightly-updater -o jsonpath='https://{.spec.host}')\"}}"` |

For more detailed troubleshooting, see [RUNBOOK.md](../RUNBOOK.md).
