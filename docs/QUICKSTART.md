# Quick Start

From an empty ROSA HCP (or OpenShift) cluster to a running RHOAI nightly in about 15 minutes, then a test environment on the Environment page. After step 1, everything happens in the app, except the image mirror (step 3) and, on self-managed OpenShift, the cluster pull secret.

Before your first change, read **[How the tool keeps your cluster safe](../README.md#how-the-tool-keeps-your-cluster-safe)**.

| Step | You do | Time |
|---|---|---|
| [0](#0-before-you-start) | Check the prerequisites | 2 min |
| [1](#1-install-the-updater) | Install the updater with the release's `install.sh` | 3 min |
| [2](#2-open-the-app) | Open the app | 1 min |
| [3](#3-one-time-setup-pull-secret-and-image-mirror) | One-time setup: pull secret and image mirror | 5 min |
| [4](#4-install-rhoai) | Install the latest RHOAI nightly | 5–8 min |
| [5](#5-create-the-datasciencecluster) | Create the DataScienceCluster | 1 min |
| [6](#6-check-that-rhoai-is-healthy) | Check that RHOAI is healthy | 2 min |
| [7](#7-environment-pipelines-and-s3-storage) | Optional: pipelines and S3 storage | 3 min |
| [8](#8-environment-mlflow-and-sample-data) | Optional: MLflow and sample data | 3 min |
| [9](#9-environment-a-model-on-cpu) | Optional: a model on CPU | 5 min |
| [10](#10-environment-the-gen-ai-playground) | Optional: the dashboard's Gen AI playground | 2 min |
| [11](#11-environment-maas) | Optional: Models as a Service | 10 min |

## 0. Before you start

- A ROSA HCP or OpenShift cluster, **OCP 4.15 or newer**. The `ose-oauth-proxy-rhel9` image has no v4.14 tag.
- **cluster-admin** on that cluster. You need it to install the updater, and the app allows changes only to cluster-admins (everyone else is read-only). On ROSA, the cluster owner can grant it to a user of the cluster's identity provider:
  ```bash
  rosa grant user cluster-admin --user=<username> --cluster=<cluster-name>
  ```
  For a quick break-glass login, `rosa create admin --cluster=<cluster-name>` creates a `cluster-admin` user and prints an `oc login` command. Syntax checked with `rosa` 1.2.57 (`rosa grant user --help`, `rosa create admin --help`).
- `oc`, logged in (`oc whoami` shows your cluster-admin user), and `curl`. `git` and `make` only for the clone path.
- A Quay.io pull token for `quay.io/rhoai`. Get it from [Bitwarden](https://vault.bitwarden.com/#/vault?collectionId=75f54536-fa36-4ef9-8f1a-b09701646cac&itemId=e6e1fdde-6601-4e8b-8154-b211005518a1) (Openshift AI devel collection). No access? Ask in [#rhoai-devtestops-requests](https://redhat.enterprise.slack.com/archives/C07TF3MBMMW).

## 1. Install the updater

Is the cluster's configuration applied by Argo CD from Git? Deploy the Helm chart instead and continue at step 2: [GITOPS.md](GITOPS.md).

Download the latest release's `install.sh` (it contains that release's `deploy/template.yaml`) and read it before you run it:

```bash
curl -fsSLO https://github.com/DaoDaoNoCode/rhoai-nightly-updater/releases/latest/download/install.sh
less install.sh
```

- **Check the download:** compare `sha256sum install.sh` (macOS: `shasum -a 256 install.sh`) with the SHA-256 in the release notes ([GitHub](https://github.com/DaoDaoNoCode/rhoai-nightly-updater/releases/latest), [GitLab](https://gitlab.com/redhat/ai/rhoai-dashboard-team/rhoai-nightly-updater/-/releases)).
- **A specific version:** download `https://github.com/DaoDaoNoCode/rhoai-nightly-updater/releases/download/vX.Y.Z/install.sh` instead.

Then run it:

```bash
bash install.sh --dry-run   # optional: resolve, check and validate with the API server only
bash install.sh             # namespace rhoai-nightly-updater; --namespace / --app-name to change
```

`install.sh`:
- checks that you are logged in as cluster-admin;
- resolves `quay.io/juntao_wang/rhoai-nightly-updater:vX.Y.Z` to its digest and checks that the image was built from the release's commit;
- creates the namespace and the oauth-proxy cookie Secret;
- applies the template, with the oauth-proxy tag that matches your cluster's OCP version;
- waits for the rollout, then adds the app to the console's application menu.

**What you should see:** the last line is `App URL: https://...`. `bash install.sh --help` lists the options.

<details>
<summary>From a clone instead (contributors; also needed for <code>make rollback</code> and the smoke test)</summary>

```bash
git clone https://gitlab.com/redhat/ai/rhoai-dashboard-team/rhoai-nightly-updater.git
cd rhoai-nightly-updater
git checkout vX.Y.Z         # make deploy installs the release HEAD is on
make deploy                 # DRY_RUN=1 only validates
```

`make deploy` runs the same `scripts/install.sh`, with the template of your checkout. Optional, from a checkout of the same release: `./scripts/smoke-test.sh rhoai-nightly-updater rhoai-nightly-updater` runs read-only checks of the live install.

</details>

If it stops with an `ERROR:` line:
- *"HEAD is not on a release tag"*: check out a release (`git checkout vX.Y.Z`), or pass `TAG=main` / `TAG=<commit>` to test a build.
- *"deploy/template.yaml in this checkout differs"*: your checkout is not the release you install. *"uncommitted changes"* means you edited the template locally.
- *"cannot resolve ... to a digest"*: the tag doesn't exist, or quay.io is unreachable.
- More in [UPGRADING.md](UPGRADING.md#7-faq).

## 2. Open the app

Open the printed URL, or use the grid icon in the OpenShift console (Red Hat Applications → RHOAI Nightly Updater). Sign in with OpenShift. To find the URL again:

```bash
oc get route rhoai-nightly-updater -n rhoai-nightly-updater -o jsonpath='https://{.spec.host}{"\n"}'
```

<img src="images/masthead-version.png" alt="The masthead: the release v3.0.1, the appearance button, Help, Applications and the user menu" width="380">

**What you should see:** the **Status** page. The masthead shows (1) the release you installed (`v3.0.1` in the picture), then the appearance button (color scheme and contrast), **Help** (`?`), the application menu and your user name. A fresh cluster shows **One-time cluster setup** first.

## 3. One-time setup: pull secret and image mirror

Nightly builds come from `quay.io/rhoai`. The cluster needs a pull secret for it and an image mirror, once.

<img src="images/status-cluster-setup.png" alt="One-time cluster setup: Pull secret and Image mirror (IDMS), both Missing" width="760">

1. **Pull secret:** paste the `quay.io/rhoai` token (step 0) and click **Create secret**. The tool stores it in `kube-system/additional-pull-secret` and replaces only the `quay.io/rhoai` entry. On self-managed OpenShift, also add it to the cluster pull secret (below).
2. **Image mirror (IDMS):** click **How to create the image mirror** for the instructions. RHOAI images reference `registry.redhat.io/rhoai`; the mirror redirects them to `quay.io/rhoai`.

### Self-managed OpenShift: the cluster pull secret

Only hosted control planes (ROSA HCP) pass `kube-system/additional-pull-secret` to the nodes. On a self-managed cluster the nodes pull with the cluster pull secret, `openshift-config/pull-secret`, so the `quay.io/rhoai` entry has to go there too. The app keeps writing `additional-pull-secret` on every cluster: its own Quay lookups (the newest nightly, Build Explorer) read it. On a self-managed cluster that copy is the app's only; the nodes never see it.

The app reads the cluster's control plane topology (Infrastructure `cluster`, `status.controlPlaneTopology`; `External` is a hosted control plane). It has no access to `openshift-config/pull-secret`, which holds every registry credential of the cluster. So once the token is saved, the **Pull secret** item of a self-managed cluster (or one whose topology it could not read) shows **Nodes not checked** and this command for a cluster-admin (bash or zsh, `jq` 1.6 or later):

```bash
printf 'quay.io/rhoai token (the one you saved under Pull secret; base64 user:password, with its = padding): ' && read -rs QUAY_AUTH && echo
[ -n "$QUAY_AUTH" ] && oc get secret pull-secret -n openshift-config -o json \
  | QUAY_AUTH="$QUAY_AUTH" jq '.data[".dockerconfigjson"] |= (@base64d | fromjson | .auths["quay.io/rhoai"] = {auth: $ENV.QUAY_AUTH} | tojson | @base64)' \
  | oc replace -f -
unset QUAY_AUTH
```

- It asks for the token you saved under **Pull secret** (the `quay.io/rhoai` token of step 0, the value you pasted into the app) and does not echo it, so it stays off the screen and out of the shell history. The prompt is printed by `printf` because zsh's `read` has no `-p`.
- The token reaches `jq` through its environment, not its arguments, which `ps` shows to every user of the machine. Without a token (an empty line, Ctrl-D) nothing runs.
- `jq` changes only the `quay.io/rhoai` key; every other registry login stays as it is.
- `oc get -o json` keeps the Secret's `resourceVersion`, so `oc replace` fails with a conflict if someone changed the Secret meanwhile, instead of overwriting their change. A missing or garbled Secret makes `jq` fail, so `oc replace` gets nothing and changes nothing. Nothing is written to disk.
- The token must keep its `=` padding: CRI-O rejects an unpadded value. (The app pads it for its own copy; the token Bitwarden gives is padded.)
- The nodes pick it up within a few minutes, without a drain or a reboot; `oc get mcp` shows `UPDATED True` for every pool when done.

<img src="images/status-cluster-setup-self-managed.png" alt="Self-managed cluster setup: the Pull secret item with the command for openshift-config/pull-secret, Check that the nodes can pull, and its result" width="760">

(1) the command, with its copy button; (2) **Check that the nodes can pull**; (3) its result.

Then click **Check that the nodes can pull**. It runs Update's own catalog check: a temporary CatalogSource with the newest nightly catalog image, which tells whether a node can pull it, and removes it again (up to two minutes). Until the check has passed the item reads **Nodes not checked**; if a node cannot pull the image it reads **Nodes can't pull**, with the reason under the button (run the command again, wait for `oc get mcp`, then check again). The item says **Ready** once the check passes, and from then on while the nightly catalog runs its `quay.io/rhoai` image. Until then the setup card stays at the top of the page; **Install latest nightly** is not held back, and its own catalog check says the same if the nodes cannot pull.

### The image mirror

On ROSA HCP the mirror is created through OCM. You need the Red Hat VPN and ROSA CLI access (separate from `oc`):

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

On self-managed OpenShift, create the ImageDigestMirrorSet with `oc` (cluster-admin); **How to create the image mirror** shows this command there (both, when the app could not read the topology). Creating it does not drain or reboot the nodes; deleting it later does drain them.

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

**What you should see:** both items stop saying **Missing**. Once both are done, the setup card moves to the bottom of the page, collapsed, as **Cluster setup** with a green **Ready** label, and **Install latest nightly** is enabled. Allow 2–3 minutes for the nodes to pick up the credentials and the mirror.

## 4. Install RHOAI

On the **Status** page, the **RHOAI on this cluster** card shows the newest nightly. Click **Install latest nightly**, read the confirmation, and click **Install**:

<img src="images/install.gif" alt="Install latest nightly, confirm, the eight steps run, then Create the DataScienceCluster appears" width="760">

To install another build, use **Choose another build**, or paste an image from [#rhoai-build-notifications](https://redhat.enterprise.slack.com/archives/C07ANR2U56C) under **Update to a specific build**. **Dry run** checks a build and changes nothing.

**What you should see:** eight steps, from **Check prerequisites** to **Wait for the operator install**, each with a green check. OLM gets up to 8 minutes to install the operator. You can leave the page: the operation keeps running, and every page shows a banner until it ends. Then **Update complete** appears, the card says **Up to date**, and a **Create the DataScienceCluster** card appears.

If a step fails, the card says which one and why. A failed install restores the previous state; see [RUNBOOK §6](../RUNBOOK.md#6-update--install-a-different-build-failed).

## 5. Create the DataScienceCluster

<img src="images/status-create-dsc.png" alt="The Create the DataScienceCluster card" width="760">

1. Click **Create DataScienceCluster...**. The dialog shows the YAML of `default-dsc`, with the defaults of the installed operator. Click **Create**.

**What you should see:** a success message. The operator now deploys the components (dashboard, pipelines, model serving, ...); this takes a few minutes. You can change components later on the **Components** page.

## 6. Check that RHOAI is healthy

Open **Components**, then **Diagnostics**.

<img src="images/diag-all-passing.png" alt="Diagnostics: All 16 checks passed, no issues detected" width="760">

**What you should see:**
- **Components:** DataScienceCluster `default-dsc` is **Ready** and every enabled component is ready. Right after step 5, some deployments are still rolling out; refresh after a few minutes.
- **Diagnostics:** **All 16 checks passed, no issues detected**. If a problem is listed, open it: it shows what was observed and the fix. [RUNBOOK §11.7](../RUNBOOK.md#117-datasciencecluster-not-ready) explains each one.

## 7. Environment: pipelines and S3 storage

Steps 7 to 11 are optional: each sets up one feature to develop against or reproduce an issue with. Open **Environment**. The **Overview** has one row per feature (Pipelines, MLflow, Models, Playground, MaaS) with its state and next step; each feature has its own tab.

<img src="images/environment-overview.png" alt="Overview: each feature with its state and next step" width="760">

Before **Set up**, each tab says what it still needs, and Set up stays disabled with the reason under it: a DataScienceCluster component that is not Managed (with **Open Components**), or no data science project. The tool never creates a project: create one in the RHOAI dashboard (Projects, then Create project) first. Every Set up asks first, names what it creates, and shows its steps.

1. On the **Overview**, **Pipelines** reads **Not set up in any project**: click **Choose a project**, or open the **Pipelines** tab.
2. Pick a data science project, click **Set up** and confirm. The tool deploys SeaweedFS in namespace `seaweedfs` (bucket `pipelines`, Service `seaweedfs-s3`), then the pipeline server `dspa` in the project, as the dashboard sets one up, with a dashboard S3 Connection that workbenches can use. (If S3 storage reads **Needs attention** with **Migrate storage**, an earlier version's storage is still in namespace `minio`: see [UPGRADING §5](UPGRADING.md#5-upgrade-to-v300).)

<img src="images/environment-pipelines.png" alt="Pipelines tab: Ready in the project, with the storage's endpoint and admin login, and Open pipelines" width="760">

**What you should see:** the tab reads **Ready in** your project, with **Open pipelines** (after 1–3 minutes) and **Open admin UI** (user `admin`; the eye beside the password shows it, cluster-admins only). **Run a sample pipeline** on the same tab offers a two-step pipeline to import in the dashboard, or commands that make a token that may only use the pipeline API (it expires in 15 minutes), then upload and run it. To remove it all, use **Clean up Pipelines** at the bottom of the tab (or **Delete** in a part's menu: the storage's stays disabled while a pipeline server uses it).

## 8. Environment: MLflow and sample data

1. Open the **MLflow** tab, click **Set up** and confirm. MLflow runs in `redhat-ods-applications`, with the MLflow operator's own resource defaults. One someone set up in the dashboard reads **Not managed by this tool** and works the same; the tool does not delete it.
2. Under **Sample data**, choose a project (its MLflow workspace) and click **Add all**: MLflow's agent and prompt demo, a training experiment with a registered model, an MCP server entry and an agent skill. Each row says where it shows in the dashboard.

<img src="images/environment-mlflow-samples.png" alt="MLflow tab, Sample data for dev-project: Add all and Remove all, and each sample with its state" width="760">

**What you should see:** MLflow **Ready**, with **Open MLflow experiments** (MLflow's pages in the RHOAI dashboard) and the client settings (`MLFLOW_TRACKING_URI`, `MLFLOW_TRACKING_TOKEN`, `MLFLOW_WORKSPACE`) under **Use it**. **Remove all** removes the samples the tool added (the agent and prompt demo is removed in the MLflow UI).

## 9. Environment: a model on CPU

Open the **Models** tab and pick a data science project.

1. **Choose a model** in the picker. Each option says what it is for, what it reserves and how much it downloads; one no worker has room for is disabled, with the reason.

   | Model | For | Reserves (CPUs, memory) |
   |---|---|---|
   | SmolLM2 135M Instruct | chat, a quick smoke test | 2, 4 GiB |
   | Qwen2.5 0.5B Instruct (the default) | chat | 4, 6 GiB |
   | Qwen3 1.7B | chat, tool calling, reasoning | 4, 12 GiB |
   | Granite Embedding 125M English | embeddings (768 dimensions) | 2, 3 GiB |

2. If **On the cluster first** lists missing parts (KServe, cert-manager, and Connectivity Link when the model goes through a gateway), click **Set up what Models needs** and confirm.
3. Under **How it is reached**, keep the recommended choice or pick another: the cluster's default inference gateway, MaaS only, another gateway that admits the project, or only inside the cluster. When no gateway fits, the tab recommends **Set up the inference gateway** (once per cluster; it runs a cloud load balancer, which costs money while it exists).
4. Click **Set up** and confirm. The first time takes 3 to 5 minutes. Once the model runs, the tool calls it once, as a user would; the setup succeeds only when it answers.

<img src="images/environment-models-picker.png" alt="The model picker open: each model with its use case, the CPUs and memory it reserves and its download; Qwen3 is disabled because no worker has room for it now" width="760">

**The runtime image.** A model runs on the vLLM CPU image that the installed RHOAI's CPU preset names. On a nightly build that image is not published yet, so the cluster cannot pull it, and the tool uses the newest released Red Hat vLLM CPU image instead (`registry.redhat.io/rhaii/vllm-cpu-rhel9`, found in Red Hat's container catalog and pinned by digest); the model's details say which image it runs. A released RHOAI, or a cluster with a mirror for the preset's image, uses the preset's own image. `VLLM_CPU_IMAGE` sets an image of your own, for example a mirror on a disconnected cluster.

**What you should see:** the model **Ready**, **Can it be called?** reading **Answers requests**, its address and the exact name to send as `"model"`, and under **Use it** the `curl` commands that call it with its own token. The dashboard lists it with the project's deployments. Troubleshooting: [RUNBOOK §10.2](../RUNBOOK.md#102-models-the-inference-gateway-and-cpu-models).

## 10. Environment: the Gen AI playground

Needs a ready chat model in the project (step 9) and the DataScienceCluster component that runs the playground Managed (the tab says which).

1. Open the **Playground** tab and pick the project. Before Set up, the project's ready chat models are chosen, and this tool's embedding model is added for searching your documents.
2. Click **Set up** and confirm. The tool asks the RHOAI dashboard to create its playground, as a click in its Gen AI studio does; this takes 1 to 2 minutes.

<img src="images/environment-playground.png" alt="Playground tab: Ready in dev-project; the project's models, and the playground Ready with Open the playground" width="760">

**What you should see:** the playground **Ready**, with **Open the playground** (Gen AI studio, then Playground, then the project). Troubleshooting: [RUNBOOK §10.3](../RUNBOOK.md#103-playground-the-dashboards-gen-ai-playground).

## 11. Environment: MaaS

Models as a Service lets everyone signed in to the cluster call a published model with a MaaS API key. The **MaaS** tab sets up the platform as the MaaS install docs do and publishes this tool's chat models, one at a time.

Nothing has to be installed first: the tab lists every part MaaS needs and what Set up will do about each.

1. Click **Set up** and choose where MaaS keeps its API keys: the preselected **Development database** (one pod with no storage, as the MaaS docs' `setup-database.sh` sets it up; API keys are lost whenever it restarts: fine for trying MaaS out), **Persistent development database (CloudNativePG)** (not in the MaaS docs: this tool's own option; installs the CloudNativePG operator), or **Your PostgreSQL** (enter its connection URL; for keys that must last). Read the changes it lists, and confirm (about 10 minutes on a cluster without any of it). It installs what is missing as the MaaS docs do: Red Hat Connectivity Link and its Kuadrant CR, the AI gateway's namespace, the database you chose, Gateway `maas-default-gateway` at `maas.<apps domain>` (another cloud load balancer), Authorino TLS, and `aigateway` with MaaS turned on in the DataScienceCluster. If a step fails, **Set up** again continues from there.
2. If a part offers a fix of its own (**Upgrade Connectivity Link** on a version below the docs' 1.5, **Remove the old tier policies** after a tier-based MaaS), run it; each lists its changes first. **Do it from the CLI instead** under a part shows the same change as commands.
3. **Publish** a chat model: choose who can use it (everyone signed in, or groups and users as your identity provider names them; a group with no OpenShift Group is flagged as you type) and its token limit for each user (the docs' premium example of 100,000 a day, preselected; their free example of 100 a minute; unlimited where the cluster's MaaS offers it; or your own token rate limits). While it is published, it is reached through MaaS only: its direct address answers 404 until you unpublish it. **Change limit** in the table and **Change who can use them** above it change these later.

<img src="images/environment-maas.png" alt="MaaS tab: Ready, 1 model published; the platform's parts with their health, and the published model with Unpublish" width="760">

**What you should see:** MaaS **Ready**, and under **Use it** the commands to get an API key and call the model. **Remove MaaS** unpublishes and removes what the tool set up for MaaS alone (your own PostgreSQL is never touched); Connectivity Link, Kuadrant, Authorino TLS and CloudNativePG stay, since other RHOAI features use them. More: [README: MaaS](../README.md#maas-models-as-a-service), troubleshooting: [RUNBOOK §10.4](../RUNBOOK.md#104-maas-models-as-a-service).

Each tab has **Clean up** at the bottom, and the Overview has **Clean up everything**: one confirmed operation with a checkbox per part.

## Day to day

| Task | Where |
|---|---|
| Update to a new nightly | Status → **Update to latest**, or **Update to a specific build** ([what it looks like](images/update.gif)) |
| Re-deploy the installed version (same version, fresh pods) | Status → Recovery → **Re-deploy operator...** |
| Switch to stable, another nightly, or an exact older build | Status → Recovery → **Choose a build...** (downgrades need a confirmation checkbox) |
| Test an odh-dashboard PR or main | Dashboard Dev → choose the RHOAI (Konflux) or ODH (OpenShift CI) build → **Deploy PR** or **Deploy latest main**; **Revert to default** when done. The containers card shows each container's commit, how far behind `main` (or the PR) it is, and when a newer build exists |
| S3 storage (SeaweedFS), pipeline servers, MLflow, models, the playground, MaaS | Environment |
| Which nightly contains PR #N? | Build Explorer → search `#123` ([Build Explorer search](../README.md#build-explorer-search)) |
| Something looks wrong | Diagnostics → open the problem; automatic fixes ask for confirmation. [RUNBOOK](../RUNBOOK.md) |

## Upgrade, roll back or remove the updater

- **Upgrade:** when the app says "vX.Y.Z is available", download that release's `install.sh` (step 1) and run it; it sees the existing install and upgrades it, keeping everyone signed in. From a clone: `git fetch --tags && git checkout vX.Y.Z && make upgrade`. Details, and installs from before releases: [UPGRADING.md](UPGRADING.md); from 2.x to 3.0.0: [UPGRADING §5](UPGRADING.md#5-upgrade-to-v300).
- **Roll back:** from a clone, `git fetch --tags && make rollback TAG=v1.0.0` (`DRY_RUN=1` validates). It applies that release's own template with its image and keeps sessions. `oc rollout undo` does not work, because old ReplicaSets are pruned on purpose. [UPGRADING §6](UPGRADING.md#6-roll-back).
- If a cluster operation is running, an upgrade or rollback waits for it: the updater (UI included) is offline for up to ~17 minutes.
- **Remove:** `bash install.sh uninstall` (or `make undeploy` from a clone) removes the updater, its cluster-wide RBAC and its ConsoleLink. It does **not** remove RHOAI, the nightly catalog, the pull secret, the IDMS, or anything set up on the Environment page. Remove those in the app first (Environment, Overview, **Clean up everything**).

## Notes

- Metrics and alerts work only with user workload monitoring enabled (OpenShift docs: "Enabling monitoring for user-defined projects"). It is off by default; see [RUNBOOK §7](../RUNBOOK.md#7-metrics-and-alerts).
- Build Explorer PR and commit searches work better with a `GITHUB_TOKEN` (see [README: Configuration](../README.md#configuration)).
- Problems: [RUNBOOK.md](../RUNBOOK.md). Everything the tool changes: [CLUSTER_CHANGES.md](CLUSTER_CHANGES.md).
