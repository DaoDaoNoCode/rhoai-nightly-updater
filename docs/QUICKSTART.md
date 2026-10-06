# Quick Start: RHOAI Nightly Updater on a fresh ROSA HCP cluster

About 15 minutes from an empty cluster to a running RHOAI nightly. Once the updater is deployed, day-to-day work happens in its web UI.

Before your first change, read **[How the tool keeps your cluster safe](../README.md#how-the-tool-keeps-your-cluster-safe)**.

## 0. Prerequisites

- A ROSA HCP (or OpenShift) cluster, **OCP 4.15 or newer**. The `ose-oauth-proxy-rhel9` image has no v4.14 tag.
- **cluster-admin** on that cluster. You need it to apply the template, and the UI allows changes only to cluster-admins (everyone else is read-only). On ROSA, the cluster owner can grant it to a user of the cluster's identity provider:
  ```bash
  rosa grant user cluster-admin --user=<username> --cluster=<cluster-name>
  ```
  For a quick break-glass login, `rosa create admin --cluster=<cluster-name>` creates a `cluster-admin` user and prints an `oc login` command. Syntax checked with `rosa` 1.2.57 (`rosa grant user --help`, `rosa create admin --help`).
- `oc` (logged in: `oc whoami` shows your cluster-admin user) and `curl`. `git` and `make` only for the clone path.
- A Quay.io pull token for `quay.io/rhoai`. Get it from [Bitwarden](https://vault.bitwarden.com/#/vault?collectionId=75f54536-fa36-4ef9-8f1a-b09701646cac&itemId=e6e1fdde-6601-4e8b-8154-b211005518a1) (Openshift AI devel collection). No access? Ask in [#rhoai-devtestops-requests](https://redhat.enterprise.slack.com/archives/C07TF3MBMMW).

## 1. Get the installer of a release

Pick the newest release on [GitHub](https://github.com/DaoDaoNoCode/rhoai-nightly-updater/releases) or [GitLab](https://gitlab.com/redhat/ai/rhoai-dashboard-team/rhoai-nightly-updater/-/releases) and download its `install.sh` (it contains that release's `deploy/template.yaml`). Check it against the SHA-256 in the release notes, and read it before you run it:

```bash
curl -fsSLO https://github.com/DaoDaoNoCode/rhoai-nightly-updater/releases/download/vX.Y.Z/install.sh
echo "<SHA-256 from the release notes>  install.sh" | sha256sum -c -   # macOS: shasum -a 256 -c
less install.sh
```

## 2. Deploy

```bash
bash install.sh --dry-run   # optional: resolve, check and validate with the API server only
bash install.sh             # namespace rhoai-nightly-updater; --namespace / --app-name to change
```

`install.sh` does the following:
- checks that you are logged in as cluster-admin;
- resolves `quay.io/juntao_wang/rhoai-nightly-updater:vX.Y.Z` to its digest and checks that the image was built from the release's commit;
- creates the namespace and the oauth-proxy cookie Secret;
- applies the template, picking the oauth-proxy tag that matches your cluster's OCP version;
- waits for the rollout, then adds the app to the console's application menu.

It ends by printing `App URL: https://...`. `bash install.sh --help` lists the options.

**From a clone** (contributors; also needed for `make rollback` and the smoke test):

```bash
git clone https://gitlab.com/redhat/ai/rhoai-dashboard-team/rhoai-nightly-updater.git
cd rhoai-nightly-updater
git checkout vX.Y.Z         # make deploy installs the release HEAD is on
make deploy                 # DRY_RUN=1 only validates
```

`make deploy` runs the same `scripts/install.sh`, with the template of your checkout.

If it stops with an `ERROR:` line:
- *"HEAD is not on a release tag"*: check out a release (`git checkout vX.Y.Z`), or pass `TAG=main` / `TAG=<commit>` to test a build.
- *"deploy/template.yaml in this checkout differs"*: your checkout is not the release you install. *"uncommitted changes"* means you edited the template locally.
- *"cannot resolve ... to a digest"*: the tag doesn't exist, or quay.io is unreachable.
- More in [UPGRADING.md](UPGRADING.md#6-faq).

Optional, from a checkout of the same release: `./scripts/smoke-test.sh rhoai-nightly-updater rhoai-nightly-updater` runs read-only checks of the live install.

## 3. Open the app

Open the printed URL, or use the grid icon in the OpenShift console (Red Hat Applications → RHOAI Nightly Updater). Sign in with OpenShift. To find the URL again:

```bash
oc get route rhoai-nightly-updater -n rhoai-nightly-updater -o jsonpath='https://{.spec.host}{"\n"}'
```

## 4. Pull secret

On the **Status** page, under **One-Time Cluster Setup**, find the **Pull secret** card. Paste the `quay.io/rhoai` token and click **Create secret**. The card links to Bitwarden and the Slack request channel. The tool stores it in `kube-system/additional-pull-secret` and replaces only the `quay.io/rhoai` entry.

## 5. Image mirror (IDMS)

RHOAI images reference `registry.redhat.io/rhoai`, and nightlies live in `quay.io/rhoai`. On ROSA HCP the mirror is created through OCM. You need the Red Hat VPN and ROSA CLI access (separate from `oc`):

```bash
kinit <your-username>@IPA.REDHAT.COM
rh-aws-saml-login                # select iaps-rhods-odh-dev
rosa login --use-auth-code
rosa whoami

rosa create image-mirror --cluster=<cluster-name> \
  --source=registry.redhat.io/rhoai \
  --mirrors=quay.io/rhoai
```

(Syntax checked with `rosa create image-mirror --help`, rosa 1.2.57. `digest` is the default and only type.) A 403 means you lack OCM write access to the cluster; ask its owner.

Self-managed OpenShift instead:

```bash
oc apply -f - <<'EOF'
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

Allow 2–3 minutes for nodes to pick up the credentials and the mirror.

## 6. First update (install RHOAI)

When both setup cards are green:
1. On the **Status** page, under **Update to latest**, the newest nightly is filled in. You can also pick a build under **Update to a specific build**, or paste one from [#rhoai-build-notifications](https://redhat.enterprise.slack.com/archives/C07ANR2U56C).
2. Optional: run a **Dry run**. It changes nothing.
3. Click **Install** (or **Update**) and confirm. Progress streams step by step. OLM gets up to 8 minutes to finish.
4. When the operator is `Succeeded`, the Status page shows **No DataScienceCluster yet**. Click **Preview and create DataScienceCluster**. The defaults come from the installed operator's own example.

Then check the **Components** page (deployments in `redhat-ods-applications`) and the **Diagnostics** page (it scans on load).

## Day to day

| Task | Where |
|---|---|
| Update to a new nightly | Status → Update to latest / Update to a specific build |
| Re-deploy the installed version (same version, fresh pods) | Status → Re-deploy the same version |
| Switch to stable, another nightly, or an exact older build | Status → Reinstall → choose the target (downgrades need a confirmation checkbox) |
| Test an odh-dashboard PR or main | Dashboard Dev → choose RHOAI (Konflux) or ODH (OpenShift CI) build → Deploy; **Revert** when done |
| S3 storage (SeaweedFS), pipeline servers, MLflow | Test resources |
| Which nightly contains PR #N? | Build Explorer → search `#123` (see [README](../README.md#build-explorer-pr-search)) |
| Something looks wrong | Diagnostics → read the guidance; automatic fixes ask for confirmation |

## Upgrading the updater itself

When the app says "vX.Y.Z is available", download that release's `install.sh` (section 1) and run it; it sees the existing install and upgrades it, keeping everyone signed in:

```bash
bash install.sh --dry-run   # optional
bash install.sh
```

From a clone: `git fetch --tags && git checkout vX.Y.Z && make upgrade`.

- A new MAJOR version changes the deployment template ("requires a full redeploy"); upgrading this way re-applies it. Never only change the image.
- If a cluster operation is running, the restart waits for it to finish. The updater (UI included) is then offline for up to ~17 minutes.
- Installed before releases (your Deployment references `:latest`)? Read [UPGRADING.md](UPGRADING.md) first.
- To test a build that is not a release: `make upgrade TAG=main` or `TAG=<8-character commit>` from a checkout of that commit.

## Rolling back the updater

From a clone:

```bash
git fetch --tags
make rollback TAG=v1.0.0 DRY_RUN=1   # optional check; any release or commit build
make rollback TAG=v1.0.0
```

This applies that release's own `deploy/template.yaml` with its image, keeps sessions, and waits for the rollout. Roll forward again with the newer `install.sh` or `make upgrade`. `oc rollout undo` does not work, because old ReplicaSets are pruned on purpose. Details: [RUNBOOK §2](../RUNBOOK.md#2-upgrade-roll-back-or-remove-the-updater).

## Uninstall

```bash
bash install.sh uninstall   # or, from a clone: make undeploy
```

This removes the updater, its cluster-wide RBAC and its ConsoleLink. It does **not** remove RHOAI, the nightly catalog, the pull secret, the IDMS, or anything created from Test resources. Tear those down in the UI first.

## Notes

- Metrics and alerts work only with user workload monitoring enabled (OpenShift docs: "Enabling monitoring for user-defined projects"). It is off by default; see [RUNBOOK §7](../RUNBOOK.md#7-metrics-and-alerts).
- Build Explorer PR search works better with a `GITHUB_TOKEN` (see [README: Configuration](../README.md#configuration)).
- Problems: [RUNBOOK.md](../RUNBOOK.md). Everything the tool changes: [CLUSTER_CHANGES.md](CLUSTER_CHANGES.md).
