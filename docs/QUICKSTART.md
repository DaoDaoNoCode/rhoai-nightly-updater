# RHOAI Nightly Updater — Quick Start Guide

Deploy the RHOAI Nightly Updater on your ROSA HCP (or OpenShift) cluster in about 10 minutes. Once running, you can install, update, and manage RHOAI nightly builds from a web UI — no `oc` commands needed for day-to-day use.

## Prerequisites

- A ROSA HCP or OpenShift cluster (OCP 4.14+)
- `oc` CLI logged into the cluster (`oc login ...`)
- A Quay.io pull token for `quay.io/rhoai` (get it from Bitwarden — "Openshift AI devel" collection)

## Step 1: Deploy the App

```bash
# Auto-detect OCP version for the oauth-proxy image
OCP_VERSION=$(oc version -o json | sed -n 's/.*"openshiftVersion": "\([0-9]*\.[0-9]*\).*/\1/p')
echo "Detected OCP version: $OCP_VERSION"

oc new-project rhoai-nightly-updater

oc process -f deploy/template.yaml \
  -p IMAGE=quay.io/juntao_wang/rhoai-nightly-updater:latest \
  -p OAUTH_PROXY_IMAGE=registry.redhat.io/openshift4/ose-oauth-proxy-rhel9:v${OCP_VERSION} \
  -p NAMESPACE=rhoai-nightly-updater | oc apply -f -

oc rollout status deployment/rhoai-nightly-updater -n rhoai-nightly-updater --timeout=120s
```

## Step 2: Get the App URL

```bash
ROUTE_URL=$(oc get route rhoai-nightly-updater -n rhoai-nightly-updater -o jsonpath='https://{.spec.host}')
echo "Open: $ROUTE_URL"

# Update the console launcher link so the app appears in the OpenShift app launcher
oc patch consolelink rhoai-nightly-updater --type merge -p "{\"spec\":{\"href\":\"$ROUTE_URL\"}}"
```

Open the URL in your browser. You'll be prompted to log in via OpenShift SSO.

## Step 3: Run the Smoke Test (optional)

```bash
./scripts/smoke-test.sh
```

This verifies pods, routes, RBAC, health probes, observability, and oauth-proxy config. All checks should pass.

## Step 4: Configure Pull Secret

In the app UI:

1. Go to the **Status** page (home)
2. Find the **Pull Secret** card (shows "Missing" in red)
3. Paste your `quay.io/rhoai` base64 auth token into the input field
4. Click **Create Secret**

The app creates the `additional-pull-secret` in `kube-system` for you.

**Where to get the token:** [Bitwarden — Openshift AI devel collection](https://vault.bitwarden.com/#/vault?collectionId=75f54536-fa36-4ef9-8f1a-b09701646cac&itemId=e6e1fdde-6601-4e8b-8154-b211005518a1). If you don't have access, request it in [#rhoai-devtestops-requests](https://redhat.enterprise.slack.com/archives/C07TF3MBMMW).

## Step 5: Create the Image Digest Mirror Set (IDMS)

This is needed so the cluster can pull nightly images from `quay.io/rhoai` when the operator expects `registry.redhat.io/rhoai`.

**For ROSA HCP clusters:**

You need to be on the **Red Hat VPN** and have ROSA CLI access (separate from `oc`). If you're not already logged in:

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
  --mirror=quay.io/rhoai
```

> If you get a 403, you need OCM write access. Find the cluster owner and ask them to assign you access in OCM.

**For self-managed OpenShift:**

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

> **Important:** After creating the pull secret and IDMS, wait **2–3 minutes** for credentials to propagate to all cluster nodes before proceeding.

## Step 6: Install a RHOAI Nightly Build

1. Go to the **Status** page
2. The **Update to Nightly** card should now show a green "Ready" state
3. Click **Fetch latest** to auto-fill the latest nightly image, or paste an image from [#rhoai-build-notifications](https://redhat.enterprise.slack.com/archives/C07ANR2U56C)
4. (Optional) Click **Dry Run** to see what would change without modifying anything
5. Click **Update** to apply the nightly build
6. Watch the live pipeline progress — each step streams in real time

The update creates a CatalogSource, detects the best OLM channel, creates a Subscription, and triggers an InstallPlan. This typically takes 2–5 minutes.

## Step 7: Verify the Installation

1. Go to the **Components** page to see all RHOAI deployments
2. Go to the **Diagnostics** page to run health checks
3. Check the OpenShift Console under **Operators → Installed Operators** for the RHOAI operator

## Day-to-Day Usage

Once installed, the app supports these workflows:

| Task | How |
|------|-----|
| **Update to a new nightly** | Status page → enter image or click "Fetch latest" → Update |
| **Refresh current nightly** | Status page → "Refresh operator" (re-pulls same version with latest images) |
| **Rollback to stable** | Status page → Reinstall → select "Stable" → Reinstall Operator |
| **Switch to a different nightly** | Status page → Reinstall → select "Nightly" → pick version → Reinstall |
| **Deploy a Dashboard PR** | Dashboard Dev page → enter PR number → Deploy |
| **Set up MinIO + Pipelines** | Status page → Quick Resources → Set up MinIO → Add Pipeline Server |
| **Set up MLflow** | Status page → Quick Resources → Set up MLflow |
| **Troubleshoot issues** | Diagnostics page → Run Scan → use auto-fix buttons |
| **Explore available builds** | Build Explorer page → browse tags, inspect FBC contents |

## Uninstall

```bash
oc delete consolelink rhoai-nightly-updater
oc delete project rhoai-nightly-updater
```

This removes only the nightly updater app. It does **not** remove the RHOAI operator or any resources it created.

## Troubleshooting

| Symptom | Fix |
|---------|-----|
| Pod stuck in `Pending` | Check node resources: `oc describe pod -n rhoai-nightly-updater -l app=rhoai-nightly-updater` |
| 403 after login | Your user needs permission to list pods in `redhat-ods-operator` namespace |
| "Cluster token not available" | ServiceAccount token not mounted — check `oc get sa rhoai-nightly-updater -n rhoai-nightly-updater` |
| Update stuck on "Waiting for catalog" | The catalog pod is starting — wait 2 minutes, check Diagnostics page |
| Images fail to pull after IDMS | Credential propagation takes 2–3 minutes — wait and retry |
| ConsoleLink shows placeholder | Run: `oc patch consolelink rhoai-nightly-updater --type merge -p "{\"spec\":{\"href\":\"$(oc get route rhoai-nightly-updater -n rhoai-nightly-updater -o jsonpath='https://{.spec.host}')\"}}"` |

For more detailed troubleshooting, see [RUNBOOK.md](../RUNBOOK.md).
