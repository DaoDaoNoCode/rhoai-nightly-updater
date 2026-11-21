# RHOAI Nightly Updater: Runbook

Troubleshooting and manual recovery. Commands assume the defaults `NS=rhoai-nightly-updater` and `APP=rhoai-nightly-updater`:

```bash
NS=rhoai-nightly-updater; APP=rhoai-nightly-updater
```

Steps marked **MANUAL ADMIN STEP** change the cluster outside the tool. They need cluster-admin. Run them only after the read-only checks before them confirm the situation, and only on a cluster you own or are allowed to change.

What the tool itself changes: [docs/CLUSTER_CHANGES.md](docs/CLUSTER_CHANGES.md). How it protects the cluster: [README](README.md#how-the-tool-keeps-your-cluster-safe).

**Contents, by task.** The section numbers are stable; other docs cite them.

- **Install, upgrade, roll back, remove the updater:** [§2](#2-upgrade-roll-back-or-remove-the-updater); next to Argo CD: [§13](#13-clusters-managed-by-argo-cd); its own objects: [Reference](#reference-objects-of-the-updater-itself).
- **Check the updater:** [§1 health](#1-quick-health-checks), [§7 metrics and alerts](#7-metrics-and-alerts), [§12 activity log](#12-activity-log).
- **Each feature's operations:** RHOAI updates and installs [§6](#6-update--install-a-different-build-failed); Build Explorer [§8](#8-build-explorer-quay-and-github); Dashboard Dev [§9](#9-dashboard-dev); Environment page [§10](#10-environment-page): storage migration [§10.1](#101-migrate-storage-from-an-earlier-version), Models [§10.2](#102-models-the-inference-gateway-and-cpu-models), Playground [§10.3](#103-playground-the-dashboards-gen-ai-playground), MaaS [§10.4](#104-maas-models-as-a-service); Components and the DataScienceCluster [§11.9](#119-change-or-reset-the-datasciencecluster).
- **Troubleshooting by symptom:** the table below; operator deadlocks and upgrade leftovers: [§11](#11-manual-recovery-for-operator-deadlocks).

**Find your symptom.** Each section starts with what the app shows, then the fix.

| What you see in the app | Section |
|---|---|
| The app does not load, the pod does not start, Route 503 | [§3](#3-app-down-or-pod-not-starting) |
| 403 after login, "Read-only access", "Only cluster-admins can see this page", "Sign in again", "Log out and log in again", a change stopped because your sign-in ended or your account was refused | [§4](#4-sign-in-and-permissions) |
| An OAuth error page (`unauthorized_client`, `invalid_request`, `scope denied`), an OAuthClient left after deleting the namespace | [§4, Sign-in and the OAuthClient](#sign-in-and-the-oauthclient) |
| "vX.Y.Z is available", "this updater's deployment is out of date" | [§2](#2-upgrade-roll-back-or-remove-the-updater) |
| "... is running ... on updater pod ...", "... was interrupted", 409 `cluster_busy`, 503 `lock_unavailable` | [§5](#5-operations-busy-stuck-interrupted) |
| **Operator failed**, **No Subscription**, a failed Update or Install a different build step | [§6](#6-update--install-a-different-build-failed) |
| A PR shows "Does not contain" in Build Explorer, tags or commit dates do not load | [§8](#8-build-explorer-quay-and-github) |
| "Dashboard Dev session active", "Distance from main unknown" | [§9](#9-dashboard-dev) |
| Storage **Needs attention** (Migrate storage or Repair), **Error**, Delete disabled, a pipeline server or sample problem | [§10](#10-environment-page), [§10.1](#101-migrate-storage-from-an-earlier-version) |
| A model **Error** or **Needs attention**, "Can it be called?" fails, the gateway's list of projects | [§10.2](#102-models-the-inference-gateway-and-cpu-models) |
| A playground that cannot be set up or deleted | [§10.3](#103-playground-the-dashboards-gen-ai-playground) |
| MaaS setup, Publish or Unpublish refused, API keys that stop working, 429 | [§10.4](#104-maas-models-as-a-service) |
| DataScienceCluster **Not Ready**, a Diagnostics problem | [§11.7](#117-datasciencecluster-not-ready), [§11.6](#116-upgrade-leftovers-the-operator-cannot-fix-itself) |
| A component missing or "Not set" after an upgrade, a change refused on Components, a hard reset that stopped | [§11.9](#119-change-or-reset-the-datasciencecluster) |
| "The operator cannot convert DataScienceCluster to v3", "The DataScienceCluster is shown as v2" | [§11.8](#118-the-operator-cannot-convert-a-crd-version) |
| "Argo CD manages ...", **Install pending**, **OLM installing**, `gitops_managed`, `install_mismatch`, "sets the image of CatalogSource ... in Git" | [§13](#13-clusters-managed-by-argo-cd) |

---

## 1. Quick health checks

The API listens on `127.0.0.1:8080` inside the pod and needs a signed-in user, so it is reachable only through the Route. Port 9090 serves `/api/health`, `/api/health/ready`, `/api/version` and `/metrics` without auth. The image is UBI 9 minimal: it has `curl` and `bash`, but no `wget`.

```bash
oc get pods -n $NS -l app=$APP
oc exec -n $NS deploy/$APP -c app -- curl -s http://127.0.0.1:9090/api/health/ready
oc exec -n $NS deploy/$APP -c app -- curl -s http://127.0.0.1:9090/api/version   # version, commit, templateOutdated
```

Without `pods/exec`, use a port-forward (it bypasses the NetworkPolicy, like any port-forward):

```bash
oc port-forward -n $NS deploy/$APP 9090:9090 &
curl -s localhost:9090/api/version; curl -s localhost:9090/metrics | grep '^rhoai_'
kill %1
```

Read-only verification of the whole install: `./scripts/smoke-test.sh $NS $APP`.

Logs: `oc logs -n $NS deploy/$APP -c app` (JSON, one line per request and action) and `-c oauth-proxy`.

## 2. Upgrade, roll back or remove the updater

Install and upgrade **releases** (`vX.Y.Z`). Each release attaches an `install.sh` with its own template; a new MAJOR version changes the template and needs this full redeploy, never just a new image. Installs from before releases (a Deployment on `:latest`): [docs/UPGRADING.md](docs/UPGRADING.md).

| Goal | Without a clone (the release's `install.sh`) | From a clone |
|---|---|---|
| Upgrade to a release | `bash install.sh` (`--dry-run` only validates) | `git checkout vX.Y.Z && make upgrade` (`DRY_RUN=1`) |
| Test a non-release build | n/a | `make upgrade TAG=main` or `TAG=<8-char commit>`; the build's template must equal your checkout |
| Roll back | the older release's `install.sh` (v2.0.0 and later) | `git fetch --tags && make rollback TAG=v1.0.0` (or any commit build) |
| See what would be deployed | `bash install.sh resolve-image` | `make resolve-image [TAG=...]` |
| Remove | `bash install.sh uninstall` | `make undeploy` |

Download `install.sh` from the release page ([GitHub](https://github.com/DaoDaoNoCode/rhoai-nightly-updater/releases), [GitLab](https://gitlab.com/redhat/ai/rhoai-dashboard-team/rhoai-nightly-updater/-/releases)), check its SHA-256 against the release notes and read it; do not pipe it into a shell. `make deploy`/`upgrade`/`rollback`/`undeploy` run the same script (`scripts/install.sh`).

Image tags: `:vX.Y.Z` (immutable release), `:vN` (newest release of major N), `:latest` (newest release of the major line it is on; it moves to a new major only through a manual CI job), `:main` (newest `main` build, for testing) and `:<8-char commit>`.

What the app's notices mean:

<img src="docs/images/banner-major-update.png" alt="Notice: v2.0.0 is available; a new major version requires a full redeploy" width="760">

- **"vX.Y.Z is available"**: a newer release exists. The notice shows that release's installer command (download, read, run) to copy; [UPGRADING](docs/UPGRADING.md#patch-and-minor-releases) explains it. "Requires a full redeploy" means a new major version: run its `install.sh` (or check it out and `make upgrade`). Dismissing hides it until an even newer release.

<img src="docs/images/banner-template-outdated.png" alt="Notice for admins: this updater's deployment is out of date" width="760">

- **"This updater's deployment is out of date"**: the Deployment was created from an older template than the running image expects, typically an install on `:latest` that pulled a newer image. Re-apply the template of the release you run (its `install.sh`, or `make upgrade` from its checkout). Which version runs: [UPGRADING §1](docs/UPGRADING.md#1-which-version-am-i-running).

What the image checks do:
- `deploy` and `upgrade` apply `IMAGE@sha256:<digest of TAG>`. A release's `install.sh` resolves its own `:vX.Y.Z` and refuses an image built from another commit.
- They refuse when:
  - the digest lookup fails (override `ALLOW_MUTABLE_TAG=1`);
  - the image's `org.opencontainers.image.revision` label is unknown or contradicts the tag;
  - the template differs from your working tree, uncommitted edits included.
  `ALLOW_TEMPLATE_MISMATCH=1` overrides the last two.
- **Images published before the revision label** existed need `ALLOW_TEMPLATE_MISMATCH=1` once.

What `make rollback` does:
1. Applies `git show <commit>:deploy/template.yaml` with that commit's image, pinned by digest like `upgrade` (it stops when no digest can be read, unless `ALLOW_MUTABLE_TAG=1`). It looks for the image under the given tag, then the 8-character tag (GitLab `CI_COMMIT_SHORT_SHA`), then the 7-character tag of older manual builds.
2. Refuses if the image's revision label names a different commit of this repo (override `ALLOW_TEMPLATE_MISMATCH=1`).
3. Keeps sessions: a template with a `COOKIE_SECRET` parameter gets the current Secret value.
4. Skips the legacy cleanup, because the old build may need its legacy objects. Rolling back to a template from before namespaced names recreates a ConsoleLink with a placeholder URL until the next `make upgrade`.

**`oc rollout undo` does not work.** `install.sh` (and `make deploy`/`upgrade`) delete old ReplicaSets after a successful rollout, because their pod templates may hold the old plaintext cookie secret. Use `make rollback`.

**Restarts wait for operations.** If an Update/Install a different build is running, the old pod finishes it first (drain up to 980 s, `terminationGracePeriodSeconds: 1020`, `Recreate`). oauth-proxy exits at once, so the UI is unreachable for up to ~17 minutes. `make upgrade`/`rollback` wait up to `ROLLOUT_TIMEOUT=20m`. A direct `oc delete pod` or an eviction is different: the ReplicaSet starts a replacement at once, which shows the old pod's operation as running "on updater pod …" and refuses changes until it ends (§5). Check whether something is running before you restart (`operation` is the marker, `lock` the lease):

```bash
oc get cm rhoai-nightly-updater-operation -n $NS -o jsonpath='{.data}'; echo
```

## 3. App down or pod not starting

```bash
oc get pods -n $NS -l app=$APP
oc describe pod -n $NS -l app=$APP | sed -n '/Events/,$p'
oc logs -n $NS deploy/$APP -c app --previous
```

| Cause | Fix |
|---|---|
| `ImagePullBackOff` on oauth-proxy | The tag must match the OCP minor version, and v4.14 doesn't exist. Re-run `make upgrade`, which detects it, or pass `OAUTH_PROXY_IMAGE=registry.redhat.io/openshift4/ose-oauth-proxy-rhel9:v4.<minor>` |
| `ImagePullBackOff` on app | The digest or tag is gone from quay.io. Run `make resolve-image` (or `bash install.sh resolve-image`), then upgrade |
| App never Ready, probes fail on 9090 | The image is older than the template (no metrics listener). Use `make rollback TAG=<its commit>` or `make upgrade` |
| `Pending` | Node capacity (`oc describe pod`). Requests are 60m CPU / 96Mi |
| `Terminating` for minutes | Expected while an operation drains (§2) |
| Route 503 | Pods not Ready, or the NetworkPolicy no longer admits the router: `oc get netpol $APP -n $NS -o yaml` |

## 4. Sign-in and permissions

| Symptom | Meaning / fix |
|---|---|
| 403 from oauth-proxy right after login | The user may not `get` `auths.services.platform.opendatahub.io` (the login gate since 5.0.0): they are not in RHOAI's allowed groups (`oc get auths.services.platform.opendatahub.io auth -o jsonpath='{.spec.allowedGroups}'`), or RHOAI is not installed, where only cluster-admins pass. Check with `oc auth can-i get auths.services.platform.opendatahub.io --as=<user>` |
| UI is read-only ("Read-only access") | Changes need cluster-admin. Check with `oc auth can-i '*' '*' --all-namespaces`. On ROSA: `rosa grant user cluster-admin --user=<u> --cluster=<c>` |
| Only Status, Build Explorer and Environment in the navigation; other pages say "Only cluster-admins can see this page"; Environment shows only your projects | You are signed in but not a cluster-admin: since 5.0.0 that is the view of everyone RHOAI lets in ([README](README.md#not-a-cluster-admin-what-you-see)). No projects listed: you are in none (`oc get projects -l opendatahub.io/dashboard=true` as that user). For every page and for changes, cluster-admin, as above |
| "Sign in again" banner | The oauth-proxy session (23 h) or the OAuth token expired. Follow the link |
| "Your sign-in expires in N minutes ... Log out and log in again" (401 `sign_in_again`), or the banner "Sign in again to make changes" with that reason | Since 4.0.0 changes run with your own token, which must outlive the change (the 15-minute deadline, the restore and a margin). Nothing was changed. Use **Log out** in the banner or the user menu, then log in again |
| "Your sign-in cannot make changes ... Log out and log in again" (401 `sign_in_again`) | A session from before 4.0.0 (the cookie survives the upgrade; its token was made for the ServiceAccount OAuth client and can only identify you). Nothing was changed. Log out and log in again, once |
| An operation stopped with "your OpenShift sign-in is no longer accepted (it expired or was revoked)" (outcome "Stopped: your sign-in ended", `sign_in_ended`) | Your token was rejected mid-operation (expired, revoked with `oc delete oauthaccesstokens`, you logged out elsewhere, or your user changed). The steps before it were made; the restore runs with the same token and may not have run. Log out and log in again, look at the Status and Components pages, then run it again if it is not complete, or recover by hand (sections 6 and 11). A sign-in that ends before anything runs is refused up front ("Your session expired", `session_expired`; nothing changed) |
| An operation stopped with "your OpenShift account is not allowed to do this" (403) | The API server refused your own RBAC for one step (for example a cluster-admin binding removed mid-operation, or a namespace-scoped admin). Ask for cluster-admin, then retry |
| 401 "Bearer tokens are not accepted" | Something called the API with a bearer token. Use the Route in a browser |
| 503 "Cannot verify ..." | The API server didn't answer the identity/SAR check. Nothing was changed; retry |
| 429 | One accepted request per user and endpoint per 30 s. Dry runs and rejected requests don't count |

### Sign-in and the OAuthClient

Since 4.0.0 users sign in through the install's own OAuthClient `$APP-$NS` (`install.sh` creates it after the Route; the chart's PostSync hook in Argo CD with `profile: full`). The chart's gitops profile has no OAuthClient: users sign in through the ServiceAccount as with 3.x ([GITOPS §11](docs/GITOPS.md#sign-in-in-the-gitops-profile)). Check it:

```bash
oc get oauthclient $APP-$NS -o jsonpath='{.grantMethod} {.redirectURIs}{"\n"}'   # auto ["https://<route host>/oauth/callback"]
oc get route $APP -n $NS -o jsonpath='{.spec.host}{"\n"}'                       # must be that host
oc get secret $APP-oauth-client -n $NS -o jsonpath='{.data.client-secret}' | base64 -d | wc -c   # 32 (no newline)
oc get oauthaccesstokens --field-selector clientName=$APP-$NS                   # the sign-ins it issued
```

| Symptom | Meaning / fix |
|---|---|
| The OAuth server shows JSON `{"error":"unauthorized_client"}` | OAuthClient `$APP-$NS` is missing (an install interrupted before it, or a chart with `gitops.oauthClient.enabled=false`). Re-run `install.sh upgrade` (or sync Argo CD) |
| JSON `{"error":"invalid_request"}` about the redirect URI | The Route's host changed after the OAuthClient was made. Re-run `install.sh upgrade`; it applies the client with the current host |
| oauth-proxy's "403 Permission Denied" with body `access_denied`, and `?error_description=scope+denied` in the address bar | The OAuthClient's `scopeRestrictions` do not allow `user:full` (someone changed them; the tool sets exactly `user:full`), or the pod still runs a template from before 4.0.0 with the ServiceAccount as the client. Re-run `install.sh upgrade`, or upgrade ([OPENSHIFT_INTEGRATION §2](docs/OPENSHIFT_INTEGRATION.md#2-oauth-proxy-token-scopes)) |
| oauth-proxy logs `invalid_client` after login | The OAuthClient's secret differs from Secret `$APP-oauth-client` (one was replaced by hand). Re-run `install.sh upgrade`: it applies the Secret's value |
| Rotate the client secret | `oc delete secret $APP-oauth-client -n $NS`, then `install.sh upgrade`: a new secret, the client updated, the pod restarted; everyone signs in again |
| End every session of this install now | `oc delete oauthaccesstokens --field-selector clientName=$APP-$NS` (their cookies then fail and users sign in again). Not instant: live, the revoked tokens kept working for about a minute, then the app showed "Your session expired" ([SECURITY.md](SECURITY.md#revoking-a-sign-in)) |
| OAuthClient `$APP-$NS` still there after `oc delete project $NS` | Expected: deleting the namespace does not delete the client (live-checked) or its sign-ins. Run `install.sh uninstall --app-name $APP --namespace $NS`, which works after the namespace is gone, or `oc delete oauthclient $APP-$NS` and `oc delete oauthaccesstokens --field-selector clientName=$APP-$NS` |

## 5. Operations: busy, stuck, interrupted

An operation that another updater pod runs (here, after the pod was evicted mid-update):

<img src="docs/images/banner-remote-operation.png" alt="Banner: qa-user is running Update to nightly on updater pod rhoai-nightly-updater-..., step 8 of 8, with the latest progress" width="760">

An operation whose pod stopped before it finished:

<img src="docs/images/banner-interrupted.png" alt="Banner: Update to nightly was interrupted, with who started it, the pod that stopped and what to do" width="760">

- **409 `cluster_busy` / "Another operation is changing this cluster":** one operation at a time. The banner names who started it and the current step. Wait, or follow it on the Status page.
- **Lost the progress view:** click **View progress** on the alert. For an RHOAI update, install or re-deploy it opens the Status page's progress card; for a run of the Environment page whose dialog you closed, the same dialog with its steps; for anything else (another tab, another user), a read-only view of what the server records: who started it, when, its latest progress and, once it ends, how it ended. The server keeps only the latest step, not the list. Reloading also works: the Status page reattaches to a running operation. If the connection was lost, the final card may say "outcome unknown, see the activity log".
- **"… on updater pod *P*" (a remote operation):** another updater pod runs it. This happens when the pod is deleted or evicted (node drain, cluster upgrade, autoscaler): the ReplicaSet starts a replacement at once while the old pod still drains its operation (up to 980 s). The replacement reads the old pod's lease, shows the operation with its latest step, refuses new changes with 409 `cluster_busy`, and shows the result (`lastCompleted`) when it ends. There is no live step stream for it; the page polls.
- **The lease:** every operation records a lease (key `lock` in ConfigMap `rhoai-nightly-updater-operation`) and renews its heartbeat every 10 s, also while the pod drains at shutdown. A lease whose heartbeat is older than **45 s** is free: that is how long a crashed pod's operation blocks a new one. A lease of an earlier container of the same pod (kubelet restart) is free at once. The lock fails closed: a lease of another pod refuses with 409 `cluster_busy`, and a lease that cannot be read or written (API errors, or write conflicts through every retry) refuses with **503 `lock_unavailable`** ("Cannot verify that no other updater pod is running an operation"). Nothing was changed; retry after a few seconds (`Retry-After`). If it persists, check that the updater can reach the API and patch ConfigMap `rhoai-nightly-updater-operation`.
- **"Stopped: another updater pod took over" (`lock_lost`):** while an operation runs, a renewal found another pod's lease, or no renewal succeeded for 35 s (the TTL minus one heartbeat) so the lease may soon expire. The 35 s count from the heartbeat time written by the last successful renewal, the time other pods compare with the 45 s TTL. The stop comes at that moment even if a renewal is still in flight, 10 s before another pod may take the lease over. The lock compares heartbeat times written by different pods, so it assumes their clocks agree to well within those 10 s; OpenShift nodes keep them synchronised with chronyd, so this only matters for a `DEV_MODE` process on a machine whose clock is off. The operation stops at once and makes no further change, **not even the automatic restore**, because the other pod may be changing the same Subscription and catalog. Check the cluster state, wait for the other operation, then re-run.
- **"Stopped: your sign-in ended" (`sign_in_ended`):** the API server rejected your own token while the change ran; part of it may have run. See [§4](#4-sign-in-and-permissions).
- **"*X* was interrupted":** the operation's marker is still recorded and the process that wrote it holds no live lease: the pod or its container stopped mid-operation (SIGKILL, node loss, OOM), or an updater version without leases left it. An operation that still runs in another pod is not reported as interrupted. Check the operator status, then run the same operation again. Every step is safe to repeat; Update and Install a different build recreate a missing Subscription with the settings recorded before the last operation. For Dashboard Dev, run **Revert**.
- **Operation deadline:** 15 minutes. OLM gets 8 minutes to reach `Succeeded`, and the CSV must then stay `Succeeded` for 20 seconds: right after an install OLM can briefly move it back to `Pending` (NeedsReinstall) while its webhooks come up. A failure restores the previous catalog and Subscription within about 1 minute.

The cross-pod lock, when a pod is evicted during an update:

```mermaid
sequenceDiagram
  participant A as Old updater pod
  participant L as ConfigMap rhoai-nightly-updater-operation (key lock)
  participant B as Replacement pod
  participant U as Browser
  A->>L: Take the lease (pod, boot ID, operation)
  loop Every 10 s, also while draining
    A->>L: Renew the heartbeat, latest step
  end
  Note over A: SIGTERM (eviction): no new changes, drains up to 980 s
  B->>L: Read the lease: heartbeat is recent
  B-->>U: Banner "... on updater pod A", step from the lease
  U->>B: A new change
  B-->>U: 409 cluster_busy, nothing changed
  A->>L: Done: lastCompleted, release the lease
  B-->>U: The result of A's operation
  Note over A,B: If A dies instead (SIGKILL, node loss), its heartbeat stops.<br>After 45 s the lease is free, and B shows "X was interrupted".
  Note over A: If no renewal of A succeeds for 35 s after its last heartbeat,<br>or one finds another pod's lease, A stops at once (lock_lost),<br>10 s before the lease expires, and does not restore.
```

## 6. Update / Install a different build failed

A successful update, for reference: each step turns green, then **Update complete**.

<img src="docs/images/update.gif" alt="Update to latest, confirm, the eight steps run, Update complete" width="760">

The update pipeline, and what happens when a step fails:

```mermaid
flowchart TD
  S1["1 Check prerequisites<br>pull secret, IDMS, OperatorGroup,<br>image in a temporary catalog, version"] -->|refused| R0["Nothing was changed"]
  S1 --> S2["2 Save snapshot"]
  S2 --> C
  subgraph C ["Steps that change the cluster"]
    S3["3 Replace the nightly catalog<br>(Subscription removed; the operator keeps running)"] --> S4["4 Wait for the catalog: READY"]
    S4 --> S5["5 Detect the channel"]
    S5 --> S6["6 Remove the old operator version (CSV, InstallPlan)"]
    S6 --> S7["7 Create the Subscription (same settings)"]
    S7 --> S8["8 Wait for the operator install<br>(up to 8 min; Succeeded for 20 s)"]
  end
  S8 -->|Succeeded| OK["Update complete"]
  S8 -->|"still installing after 8 min"| KEEP["Keep the new state; the page keeps watching"]
  C -->|"a step fails"| RS["Restore: the previous catalog and Subscription,<br>remove the half-installed CSV (about 1 min)"]
```

What the Status page shows when the operator is broken:

<img src="docs/images/status-operator-failed.png" alt="RHOAI on this cluster: Operator failed, with the recommended fix" width="760">

1. **Operator failed**: the CSV phase is `Failed`.
2. The recommendation, with **Re-deploy the same version**, **Open Diagnostics** and **View in console**.
3. **Update to latest**: a new build is the usual fix; Update removes the failed version first.

<img src="docs/images/status-no-subscription.png" alt="Alert: The operator has no Subscription" width="760">

**The operator has no Subscription**: usually an interrupted Update or Install a different build. Update or Install a different build recreates the Subscription with the settings recorded before the last operation.

<img src="docs/images/status-recovery.png" alt="The Recovery card: Re-deploy the operator and Install a different build" width="760">

**Recovery** on the Status page: **Re-deploy operator...** installs the same version again; **Choose a build...** removes the operator and installs the target you choose (stable, a nightly, an exact build).

Read the step that failed and its log in the UI first. Then:

```bash
oc get catalogsource -n openshift-marketplace | grep rhoai
oc get pods -n openshift-marketplace | grep rhoai-catalog
oc get subscription rhods-operator -n redhat-ods-operator -o jsonpath='{.status.conditions}' ; echo
oc get installplan,csv -n redhat-ods-operator
```

| Message / state | Fix |
|---|---|
| "Replacement catalog validation failed", `catalog_image_pull` | The nodes cannot pull the catalog image (the kubelet's reason, often `ErrImagePull ... unauthorized`), or the image does not exist. Nothing was changed. The message names the pull secret the nodes use: on a hosted control plane (ROSA HCP) `kube-system/additional-pull-secret` (test it on the Status page); on a self-managed cluster, or one whose topology the app could not read, `openshift-config/pull-secret` |
| "This cluster is self-managed: its nodes pull with the cluster pull secret, openshift-config/pull-secret" | The nodes do not use the app's `additional-pull-secret`. A cluster-admin clicks **Add to the nodes' pull secret** under **Pull secret** in the cluster setup (or runs the command under **Do it yourself instead**; also in Diagnostics and [QUICKSTART](docs/QUICKSTART.md#self-managed-openshift-the-cluster-pull-secret)), waits until `oc get mcp` shows `UPDATED True` for every pool, then clicks **Check that the nodes can pull** and runs Update again. A "conflict" (button or `oc replace`) means the Secret changed meanwhile: review or run it again |
| One-click setup review: "Not on a hosted control plane" | ROSA HCP: the mirror goes through OCM (`rosa create image-mirror`), the nodes' credentials through `additional-pull-secret`. Nothing was changed. HyperShift merges that Secret with the cluster's own pull secret ([global pull secret](https://github.com/openshift/hypershift/blob/0f43b296/docs/content/how-to/common/global-pull-secret.md)): where the cluster's own already has a `quay.io/rhoai` entry, that one wins; only nodes of AWS and Azure node pools that replace their nodes on an update get it (not in-place node pools); its sync restarts the kubelet on each node. If a node still gets `unauthorized` from `quay.io/rhoai`, check those three first |
| One-click setup review: "Not changed: this may be a Red Hat-managed cluster" (pull secret) | The validating webhook configurations could not be listed with your token, so ROSA Classic or OSD, where the app does not change the cluster pull secret, cannot be ruled out. Nothing was changed. Use the command under **Do it yourself instead** if you choose to |
| One-click setup review: "Another mirror serves these images now" (Create the image mirror) | An ImageDigestMirrorSet, ImageContentSourcePolicy or ImageTagMirrorSet maps a parent of `registry.redhat.io/rhoai` (`registry.redhat.io`, `*.redhat.io`) to a mirror, and the nodes pull `registry.redhat.io/rhoai` images through it today. A mirror for exactly `registry.redhat.io/rhoai` would take those pulls away from it (the most specific source wins), so the app does not create one. The message names the object and the entry the cluster's administrator can add to it instead: source `registry.redhat.io/rhoai`, that mirror's path first, then `quay.io/rhoai`. `oc get imagedigestmirrorset,imagecontentsourcepolicy,imagetagmirrorset -o yaml` shows them |
| One-click setup review: "Another mirror sets a different source policy" | An entry for `registry.redhat.io/rhoai` sets `mirrorSourcePolicy: NeverContactSource`; the Machine Config Operator refuses the whole mirror configuration when two entries for one source set different policies, so the app does not add its default-policy mirror. Add `quay.io/rhoai` to the mirrors of an ImageDigestMirrorSet entry with that policy instead |
| One-click setup review: "Not changed on a Red Hat-managed cluster" (pull secret) | ROSA Classic or OSD, recognised by Red Hat SRE's `*.managed.openshift.io` admission webhooks: Red Hat creates the cluster pull secret and documents no procedure for changing it, so the app does not. The commands are shown for whoever chooses to; the mirror button works there |
| One-click setup review: "The cluster does not allow this change here" | The server-side dry run was refused: your RBAC, or an admission webhook (its message is quoted, credentials redacted). Nothing was changed; use **Do it yourself instead** or ask the cluster's administrator |
| One-click setup review: "Someone else's object is in the way" | An ImageDigestMirrorSet named `rhoai-quay-mirror` that does not redirect `registry.redhat.io/rhoai`, or holds more than this setup's mirror, or the mirror of that source has another name: the app does not change or delete it. `oc get imagedigestmirrorset -o yaml` shows it |
| One-click setup review: "An entry this tool did not add is already there", "Not added by this tool", or the button reads **Already set up (not by this tool)** | `openshift-config/pull-secret` has a `quay.io/rhoai` entry the app did not add (a provisioner's, say), or one changed since it added it (another token, or the Secret made again). The app never removes or overwrites it. Run **Check that the nodes can pull**: if it passes, nothing is needed. If that entry no longer works, tick the replace box in the Add review (its token is not kept and the app cannot restore it; ask whoever set the cluster up first), or fix it by hand. To remove it by hand use the command under **Do it yourself instead**, which removes whatever entry is there. The app's record is key `nodePullSecretEntry` of ConfigMap `rhoai-nightly-updater-snapshot` (UID and hash only); "Cannot read this tool's own record": nothing was changed, check that ConfigMap |
| One-click setup review: "The saved token cannot be used", "no readable list of registry logins", "kept in sync by External Secrets" | Replace the token under **Pull secret** (Quay refused it, or there is none); or the cluster pull secret is unreadable or empty (not touched: writing one entry would drop the cluster's own logins); or External Secrets would undo the change: add the `quay.io/rhoai` login to its source instead (`oc get externalsecrets -A`) |
| After **Remove the image mirror**: nodes drain, one at a time by default (the pool's `maxUnavailable`); installs fail with `manifest unknown` on `registry.redhat.io/rhoai` | Expected: removing a mirror drains each node (not on a single node) and nightly digests exist only on `quay.io/rhoai`. **Create the image mirror** again (no drain). A pool stuck on one node: a PodDisruptionBudget that allows no eviction holds the drain; the machine config daemon waits up to an hour, then fails that node's update (event `FailedToDrain`; `oc get pdb -A`). When another ImageDigestMirrorSet or ImageContentSourcePolicy still lists `quay.io/rhoai` for the source, the review says so and nothing rolls out. After **Remove from the nodes' pull secret**, new pulls fail with `unauthorized`; add it again |
| **Check that the nodes can pull** refused: "The check needs the image by tag, not by digest" | OLM pulls a digest only when the node lacks it, so the check would pass on a cached image without the nodes' credentials. The app sends the build's tag; this shows only for a build without one |
| "older than the installed" | Update never downgrades. Use Install a different build and tick the confirmation (see "Downgrades" below) |
| Succeeded, but "an upgrade gate holds it" | The operator waits for an admin acknowledgement (set its key to "true" in ConfigMap `odh-upgrade-acks` in `redhat-ods-operator`) or cannot resolve a gate because it is older than the resources it found. The note reads the Platform and every DataScienceCluster condition |
| Diagnostics: "Objects the operator cannot update" | Upgrade leftovers; see §11.6 |
| Diagnostics: "Left over from an earlier release" | Stored field types or a changed selector; Fix, see §11.6 |
| "cannot read the installed CSV version" | Use Install a different build and confirm |
| A second rhods-operator Subscription exists | Delete the extra one yourself; the tool refuses to guess which one is right |
| Namespace has 2+ OperatorGroups / a non-global one | Keep one global OperatorGroup in `redhat-ods-operator` |
| Re-deploy refuses: "channel head moved" (Automatic approval) | Use Update (it changes the version) |
| `BundleUnpackFailed` keeps recurring | See §11.5 |
| Webhook "connection refused / no endpoints" errors | Diagnostics → stale webhooks. For a CRD conversion webhook see §11.2 |

**Downgrades.** OLM cannot downgrade, so a confirmed install of an older version removes the operator and installs the older bundle. OLM applies every CRD in that bundle, so the CRDs it ships are **replaced by their older versions**:
- fields only the newer version knows can be pruned from stored objects such as the DataScienceCluster, and are not restored by going back;
- a same-named CRD that the newer version no longer ships stays at the older schema after you go back with Update, and the newer operator then fails on it (§11.6, schema mismatch);
- the older operator may refuse to manage, or stop at upgrade gates for, resources the newer version created (DSC `Ready: Error … upgrade gate`, `ModulesReady: AdminAckRequired`). The result of the install says so; check Components and Diagnostics. Go back to the newer version with **Update**.

## 7. Metrics and alerts

The ServiceMonitor and PrometheusRule work only with **user workload monitoring** enabled (off by default; OpenShift docs "Enabling monitoring for user-defined projects"). Without it nothing scrapes port 9090 and no alert fires. The smoke test warns about this.

```bash
oc get pods -n openshift-user-workload-monitoring        # empty = UWM off
oc get servicemonitor,prometheusrule -n $NS
```

Metrics: `rhoai_nightly_updater_{uptime_seconds,requests_total,updates_total,updates_failed_total,rollbacks_total,reinstalls_total,page_views_total,feature_usage_total}`.

Alerts: `NightlyUpdaterDown` (target down or absent for 5 m), `NightlyUpdateFailed`, `NightlyUpdaterHighErrorRate`, `NightlyUpdaterPodRestarting`.

## 8. Build Explorer, Quay and GitHub

- **Tags don't load / Quay errors:** the Quay token is derived from the cluster pull secret. Test it on the Status page, then check `oc logs ... -c app | grep -i quay`.
- **Commit dates show "-", or the Build Explorer search reports a rate limit:** GitHub allows 60 requests/hour per egress IP without a token. Set `GITHUB_TOKEN` ([README: Configuration](README.md#configuration)). Results are cached in memory per pod (cleared on restart).
- **A PR shows as not contained, but its change is in the build:**

  <img src="docs/images/builds-pr-search.png" alt="PR search for #5123: only rhoai-3.7-ea.1 contains it; the release branches do not" width="760">

  In this example only the build from `main` contains the PR. The release branches (`rhoai-3.6`, `rhoai-3.5`, ...) show **Does not contain** even if the change was cherry-picked there, because it arrived as a cherry-pick with a different SHA. A build contains the PR when the PR's merge commit is an ancestor of the component's commit in the fork (`red-hat-data-services/<name>`). The odh-dashboard fork merges upstream `main`, so this is reliable for dashboard PRs; a fork that cherry-picks or rebases upstream changes has different commit SHAs, so a PR it took that way shows as **Does not contain**. Look at the build's component commit (the link in the row) on GitHub instead. Builds built more than an hour before the merge say **Built before the merge**: they were not read.
- **Unknown (commit not on GitHub):** GitHub does not know the searched commit (or the PR's merge commit) in the repository or its fork, so it cannot compare. Check the SHA.

## 9. Dashboard Dev

<img src="docs/images/dashboard-dev-session.png" alt="Dashboard Dev session active: PR #5123, Operator paused, with Revert to default" width="760">

1. Who deployed which build, and when.
2. **Revert to default** ends the session.

- **Banner "Dashboard Dev session active":** the dashboard module's operator (`dashboard-operator` on RHOAI 3.6; the banner names the Deployment the tool discovered) is paused (0 replicas), so RHOAI updates don't reach the dashboard. Use **Revert** on Dashboard Dev when done. Operator operations offer "Revert Dashboard Dev and continue".
- **"A dashboard deletion is waiting for the paused <operator>":** this is deadlock D1. Revert at once (§11.1).
- **Stuck rollout:** the panel reports the Deployment's `ProgressDeadlineExceeded`. Revert, or deploy again.
- **"Distance from main unknown" on a container:** the page could not ask GitHub how far the build's commit is from upstream `main` (or from the PR's latest commit). Usually the rate limit: GitHub allows 60 anonymous requests per hour per egress IP, shared by every user of the pod; the tooltip names the retry time. Set `GITHUB_TOKEN` on the updater Deployment ([README: Configuration](README.md#configuration)) to raise it to 5000/hour. Other reasons in the tooltip: GitHub does not know the commit (a force-pushed PR), the PR was not found, or GitHub answered an error (retried on the next refresh). The commit itself comes from the image labels and is shown either way.
- **"A newer ... build exists" or "Newer build available" under a container:** the tag the build was deployed from (`odh-stable`, `main`, `odh-pr-N`, `pr-N`) now points to another image. Nothing changed on the cluster: the container keeps its digest-pinned image. **Deploy latest main** or **Deploy PR** again to switch; main builds are then written under their commit tag ([CLUSTER_CHANGES §7](docs/CLUSTER_CHANGES.md#7-dashboard-dev)).
- **Revert fails, or the UI is down:** §11.1.

## 10. Environment page

The S3 storage is SeaweedFS in namespace `seaweedfs` (Deployment `seaweedfs`, PVC `seaweedfs-data`, Service `seaweedfs-s3`, Route `seaweedfs-admin`). Storage an earlier version set up in namespace `minio` is moved with **Migrate storage** (§10.1). Objects and ports: [CLUSTER_CHANGES §8](docs/CLUSTER_CHANGES.md#8-environment-page).

The page has an **Overview** (each feature's state and next step) and one tab per feature: **Pipelines** (S3 storage and a project's pipeline server), **MLflow**, **Models** (the inference gateway and a project's CPU models, §10.2), **Playground** (§10.3) and **MaaS** (§10.4). The tab and the project are in the address. Each part shows its health, its actions and its details inline (endpoint, login, the reason for an error or a repair, the pod's reason under **Technical details**); its menu (⋮) has **Delete** and **Repair**. **Clean up** (each tab, and **Clean up everything** on the Overview) deletes several parts as one operation (`POST /api/resources/cleanup/stream`): first the MaaS publications (Unpublish), then the pipeline servers, the storage, MLflow, the playgrounds, the models, the inference gateway, and the MaaS platform last, stopping at the first part that fails and saying what was and was not deleted. Health and ownership are separate: something this tool did not create can be **Ready**; it only gets no Delete or Repair. Every change shows its steps and outcome in one dialog; closed early, the alert at the top names the run and its step with **View progress** to reopen it, and a notification brings the outcome back.

While the page reads the cluster, each Overview row and each tab shows loading placeholders where its state and parts will be: nothing reads **Not set up** or empty until the status that says so has been read. A status that cannot be read (the playground's or MaaS's) reads **Unknown: cannot be read** on the Overview, with the reason and **Try again** above the table. **Refresh** reads everything again and keeps what is shown until the new answers are in; if that read fails, the last data stays with a warning that it may be out of date, and **Retry**.

<img src="docs/images/s3-incomplete.png" alt="Storage Needs attention: Deployment seaweedfs is scaled to 0 replicas, with Repair, and the reason in the details" width="760">

1. **Needs attention**: SeaweedFS does not serve through `seaweedfs-s3` and `seaweedfs-admin`.
2. Why, in the details (here: scaled to 0), and what Repair does.
3. **Repair** re-runs setup and keeps the stored data:

<img src="docs/images/s3-repair.gif" alt="Repair, confirm Repair S3 storage, the storage is Running again" width="760">

<img src="docs/images/environment-pipelines.png" alt="Pipelines tab: Ready in dev-project, S3 storage with endpoint and masked admin password, pipeline server with Open pipelines" width="760">

1. The state in the selected project.
2. The project.
3. The S3 endpoint and the bucket.
4. The admin UI password, masked: show or copy it (cluster-admins only; it is read from the server only then).
5. The dashboard S3 Connection `rhoai-nightly-updater-s3` set up with the pipeline server (owned by it; in-cluster address).
6. **Open pipelines**. In the storage's menu, **Delete** stays disabled while a pipeline server uses it, and says which.

| Problem | Fix |
|---|---|
| S3 storage setup: "not created by this tool" | Namespace `seaweedfs` holds foreign objects (or was not created by this tool). Nothing was changed. Remove them or use another cluster |
| "The updater's ServiceAccount is not allowed to do this ... run `make upgrade`" | A read the updater makes with its ServiceAccount (a status, an ownership check) was refused: the image is newer than the template (the banner says the template is outdated). An admin runs `make upgrade`, then retry. A refusal of a setup or teardown step itself says "your OpenShift account is not allowed to do this" instead, since changes run with your own token ([§4](#4-sign-in-and-permissions)) |
| Storage "Needs attention" with **Migrate storage** | Storage from an earlier version is in namespace `minio`. **Migrate storage** moves it (§10.1); Set up, and a new pipeline server, wait until it has copied the data |
| Migrate storage stopped: "The copy is not complete" | Nothing else changed. The message says what differs and whether a retry can help. Objects or buckets missing, or unreadable: run it again (the copy never deletes). Before the first verified copy, while nothing uses the new storage, changed objects are copied again. Objects "different in the new storage" after that (a migration finished, pipeline servers write there, or Delete old storage's final copy): the copy never overwrites them, so a retry gives the same result; for each path named, copy the old version into `seaweedfs` yourself or delete it from `minio`, then run it again. Extra objects in `seaweedfs` are only reported |
| Migrate storage: "The copy is still running" | A large storage: the Job keeps running (up to an hour). Run Migrate storage again to wait for it |
| Delete old storage: "The final copy and check are still running" | Nothing was deleted and the copy keeps running (Job `seaweedfs-migrate`, marked as a final copy). Run Delete old storage again: it waits for that copy instead of starting over, and deletes the old storage only once it passed |
| Migrate storage: "Not enough time is left in this run" | The run stopped before moving the next pipeline server, so none is left moved but unchecked. Run Migrate storage again to continue |
| Migrate storage lists a pipeline server under **Confirm the pipeline servers this tool set up**, or the result says "not recorded as this tool's" | Since 3.0.1 the storage's keys go only to pipeline servers this tool recorded (`pipelineServers` in ConfigMap `rhoai-nightly-updater-snapshot`). Ones set up before 3.0.1 have no record. Tick a project only when you know this tool set its pipeline server up (`oc get dspa -n <project> -o yaml`: who made it, when); a project's editors can add the tool's label themselves, and moving it writes keys that open every project's pipeline artifacts. Unticked ones stay on the old storage; point them at the new one yourself or delete them. "Cannot read this tool's record": nothing was changed, check the ConfigMap and run it again |
| Migrate storage: "does not reach the new storage" | The pipeline server was moved but DSPO reports `ObjectStoreAvailable` False. The old storage is kept. `oc get dspa -n <project> <name> -o jsonpath='{.status.conditions}'`, fix the cause, run Migrate storage again |
| Migrate storage: "other keys" | Namespace `seaweedfs` already has a storage with other keys; the tool never changes the keys of a storage that may hold data. Not Delete: it also deletes the old storage in `minio`. If the new storage holds nothing you need, delete it by hand (`oc delete -n seaweedfs deployment/seaweedfs service/seaweedfs-s3 route/seaweedfs-admin networkpolicy/seaweedfs-ingress secret/seaweedfs-s3-credentials pvc/seaweedfs-data`), then migrate |
| Delete old storage stopped part-way | Run **Delete old storage** again: it continues where it stopped. Until the server's Deployment is deleted it copies and checks again first; after that the old storage's data volumes are already being deleted and it finishes without a copy |
| Delete old storage: "cannot be copied from ... not being deleted" | The old server is gone (or not running) but its data volume was never deleted by a removal, so it may hold objects that were never copied: nothing is deleted. Start the server again, or once you have checked `seaweedfs` holds what you need, delete the volume as the message says and run Delete old storage again |
| Copy Job cannot start (ImagePullBackOff) | Set `RCLONE_IMAGE` to a pullable rclone 1.x image (a mirror), then run Migrate storage again |
| Storage "Needs attention" with **Repair** ("Needs repair" in the details) | SeaweedFS runs but `seaweedfs-s3`/`seaweedfs-admin` do not point at it, or it is scaled to 0. Press **Repair** (re-runs setup) |
| "Needs repair: SeaweedFS runs ..., not the image this version deploys" | Expected after `SEAWEEDFS_IMAGE` changed. **Repair** updates the Deployment in place; the data PVC and the credentials are kept. S3 is unavailable while the pod restarts (`Recreate`) |
| SeaweedFS not ready / ImagePullBackOff | Fix the cause (for example `SEAWEEDFS_IMAGE` to a mirror) and **Repair** again. `oc logs -n seaweedfs deploy/seaweedfs` |
| S3 storage deletion refused | A pipeline server still uses the storage (the message names it). Delete that first, or select it in **Clean up**. After a partial deletion, run Delete again |
| "Data Science Pipelines (or MLflow) is turned off on this cluster" | The DataScienceCluster's component (`aipipelines`, `datasciencepipelines` on DSC v1, `mlflowoperator`) is not Managed: the operator keeps the CRD, so a pipeline server or MLflow would be created and never start. Set up refuses before changing anything. A cluster-admin sets the component to Managed (`oc edit datasciencecluster <name>`), waits for it to be ready on the Components page, then sets up |
| Clean up stopped | The message names the part that failed and what was not done. Fix that part's cause (the same as for its own Delete) and run Clean up again: parts already deleted are no longer offered |
| "Workbench pipelines don't work with this pipeline server" | The pipeline server was set up by an earlier version as `nightly-dspa`; odh-notebook-controller gives workbenches the pipeline config of the DSPA named `dspa` only. **Recreate with the standard name** (it checks everything first, then deletes `nightly-dspa` with its run history; S3 artifacts stay) or keep it: the dashboard's pipelines pages and the KFP SDK work either way. If the old server stays Terminating, Recreate stops there; use **Set up** once it is gone |
| Set up: "already has pipeline server 'nightly-dspa'" | Same cause: use **Recreate with the standard name** instead of Set up |
| Pipelines tab: "Connection in the dashboard: Missing" | Secret `rhoai-nightly-updater-s3` is gone from the project (for example deleted in the dashboard). **Set up again** recreates it and keeps runs and data. "not created by this tool": someone else made a Secret of that name; the tool leaves it alone. The tab reads the Secret itself for the selected project only |
| Set up: "is not a Data Science project" | The pipeline server goes only into an existing data science project; the tool never creates one. Create the project in the RHOAI dashboard |
| Set up or Delete: "Secret 'rhoai-nightly-updater-s3' already exists ... not created by this tool" | Someone else made a Secret with the Connection's name in the project. Nothing was changed; rename or delete theirs |
| Pipeline server reads Error after 5 minutes | Its API server is not ready 5 minutes after creation (`APIServerReady`, the dashboard's own timeout). `Ready=False` alone (another component restarting) does not count |
| "Project ... no longer exists" | The address names a project that was deleted (or is no longer a data science project). Choose another; Set up is not offered for it: a pipeline server goes only into an existing data science project, and setup refuses any other namespace |
| Namespace left after deletion | Delete keeps namespace `seaweedfs` unless you tick **Also delete namespace seaweedfs (created by this tool)**; it is offered only when the namespace carries the tool label and the tool wrote it, and deletes everything in it. Otherwise `oc delete project seaweedfs` (or `minio`) once `oc get all,pvc,secret -n <namespace>` is empty |
| Pipelines tab: the project picker is empty after clearing it | Expected: a clear is kept in the address (`project=` with no value), so the default project does not come back; choose a project, or open the tab without `project=` for the default |
| Reach the S3 API from your laptop | `oc port-forward -n seaweedfs svc/seaweedfs-s3 8333`, endpoint `http://localhost:8333`, path-style, region `us-east-1`. Keys: `oc extract secret/seaweedfs-s3-credentials -n seaweedfs --to=-` |
| Admin UI login | **Open admin UI**; user `admin`. The password is on the Pipelines tab under S3 storage: the eye shows it, the copy button copies it (cluster-admins only; a password of its own, not the S3 secret key). "There is no admin UI password yet": the storage was set up by an earlier version, whose admin UI used the S3 secret key; **Repair** writes one and keeps the S3 keys and the data. Disabled with "Read-only access": you are not a cluster-admin |
| **Set up** disabled: "Available once ..." | The storage is not ready, or Migrate storage has not copied the old storage yet, or namespace `seaweedfs` is not this tool's; the reason is under the button |
| The Pipelines tab opens another project | Without `?project=` it opens the first project with a ready pipeline server (else the first with one). Pick the project; the address keeps it |
| Pipeline server: "already has a pipeline server" | Projects get one DSPA; the existing one was left unchanged |
| Old pipeline run artifacts return 404 after Migrate storage | They were written to MinIO before an earlier version replaced it with SeaweedFS, so they are on `minio-pvc`, which cannot be copied automatically. Copy them by hand before you remove the old storage (§10.1). If a cached step fails because its outputs are gone, re-run with caching disabled |
| DSPA or project stuck Terminating | §11.4 |
| MLflow teardown hangs / DSC change waits on MLflow | §11.4 |
| Sample data: "Too many requests. Please wait 30 seconds" | One samples operation per user and route every 30 seconds. Use **Add all** / **Remove all** (one request) instead of one row after another |
| Sample data: "The updater's ServiceAccount lacks the rights for this call" | The samples status: the ServiceAccount misses the samples rules. Re-apply the template (`make upgrade`). MLflow caches access decisions for up to 5 minutes, so retry after that |
| Sample data: "MLflow refused your own OpenShift permissions for this call" | Add or Remove: they run with your own token, and MLflow's access review refused it in this project, or your sign-in expired. Log out and log in again, or ask for the rights in the project; MLflow caches decisions for up to 5 minutes |
| Sample **Partly added** | An earlier Add stopped part-way; the row names what is missing. **Add** (or **Regenerate** for the agent and prompt demo) completes it |
| Sample **Not managed by this tool** | Something of the sample's name exists without the `rhoai-nightly-updater.sample` tag; the tool never changes or deletes it. Rename or delete it in MLflow if it is yours to remove |
| Agent and prompt demo has no Remove | By design: the tool offers Add and Regenerate only, since MLflow's own `demo/delete` deletes without names (SECURITY.md, [MLflow samples](SECURITY.md#mlflow-samples)). Remove it in the MLflow UI; Clean up leaves it |
| Agent skill **Not available** | This MLflow has no Skill Registry (older RHOAI releases). The dashboard's AI hub > Skills also needs the `skillRegistry` flag |
| Sample pipeline run Failed: an image cannot be pulled | Disconnected or mirrored cluster: mirror the image the section shows, or set `SAMPLE_PIPELINE_IMAGE` on the updater to a pullable copy, download the file again and import it as a new pipeline (or a new version) |
| Sample pipeline commands: `curl: (22) ... 401` or `403` | The commands use a token of ServiceAccount `pipeline-api` that expires in 15 minutes: `401`, it expired (run the `oc create token` line again); `403`, its Role does not allow the call (`oc get role pipeline-api -n <project> -o yaml` must name `datasciencepipelinesapplications/api`, the pipeline server's name, and `get` `create`). Creating the Role needs an admin of the project. The updater does not run the sample for you |
| Sample pipeline commands: upload fails with "already exists" | A pipeline `rhoai-nightly-sample` is in the project already. Start another run with the last command (in the same shell), or delete the pipeline in the dashboard first |

### 10.1 Migrate storage from an earlier version

Versions before 3.0.0 kept the S3 storage in namespace `minio`, behind MinIO-era names. 3.0.0 moves it, with its data, to namespace `seaweedfs`:

<img src="docs/images/s3-migration-needed.png" alt="S3 storage Needs attention, with Migrate storage, and the old storage's objects in its section" width="760">

1. **Needs attention**: the old storage still holds the data and serves its pipeline servers.
2. **Migrate storage** opens the plan; nothing changes until you confirm.
3. **Storage of an earlier version** names the old storage's objects and volumes (the only place with MinIO-era names).

<img src="docs/images/s3-migrate-dialog.png" alt="Dialog: Migrate storage, with the three steps, the pipeline server it moves and the volume that cannot be copied" width="760">

The run streams each step; closing the dialog does not stop it. After it, the old storage is still there until you remove it:

<img src="docs/images/s3-migrate.gif" alt="Migrate storage, confirm, the steps complete, then the checklist before removing the old storage" width="760">

```mermaid
flowchart TD
  A["Migrate storage"] --> B["Set up seaweedfs with new keys"]
  B --> C["Job seaweedfs-migrate: copy, check every byte"]
  C -->|"any difference"| X["Stop: nothing else changed"]
  C --> D["Move this tool's pipeline servers, one at a time"]
  D -->|"not ObjectStoreAvailable in 2 minutes"| Y["Stop: the old storage is kept"]
  D --> E["Summary and checklist"]
  E --> F["Later: Delete old storage (confirm, optionally namespace minio)"]
  F --> G["Copy and check once more, then delete the old objects by UID"]
```

What each step does, and what is never touched: [CLUSTER_CHANGES §8](docs/CLUSTER_CHANGES.md#s3-storage-of-an-earlier-version-namespace-minio). Every run starts with a dry run and does only what is still missing, so after any stop you run **Migrate storage** again.

Before you remove the old storage, update what this tool did not create (data connections, workbenches, other pipeline servers): the endpoint becomes `http://seaweedfs-s3.seaweedfs.svc:8333` and the keys are the new storage's (the Pipelines tab shows them); bucket names and object paths stay. Anything still on `minio-service.minio.svc` stops working after the removal. A pipeline server the tool did not create is listed in the dialog and blocks the removal until it moves.

<img src="docs/images/s3-remove-dialog.png" alt="Dialog: Remove the old storage?, with the checklist, what is deleted, and the choice to delete namespace minio" width="760">

After a migration the S3 storage's menu offers **Delete old storage**, a separate dialog (the migration's own dialog ends with its outcome). It deletes the old objects. **Also delete namespace minio** (unticked) deletes the namespace and everything in it, including what the tool did not create; it is offered only when the namespace carries the tool's label and the tool wrote it, and the dialog lists the other Deployments and PVCs it sees there. Check the rest with `oc get all,pvc,secret,configmap -n minio` first.

Check the result:

```sh
oc get deploy,svc,pvc,route -n seaweedfs
oc get dspa -A -o custom-columns=NS:.metadata.namespace,NAME:.metadata.name,HOST:.spec.objectStorage.externalStorage.host
oc get all,pvc,secret -n minio      # after Delete old storage
```

#### Recover MinIO's files from `minio-pvc` (optional, before Delete old storage)

`minio-pvc` holds what MinIO stored before an earlier version replaced it with SeaweedFS (artifacts of the oldest pipeline runs). Only a MinIO server reads that format, so Migrate storage cannot copy it. To keep those objects, start a temporary MinIO on the volume and copy them into the new storage before the removal. The tool does not manage these objects (no tool label).

```sh
oc -n minio apply -f - <<'EOF'
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: minio-old-ingress}
spec:
  podSelector: {matchLabels: {app: minio-old}}
  policyTypes: [Ingress]
  ingress:
  - ports: [{protocol: TCP, port: 9000}]
EOF
oc -n minio apply -f - <<'EOF'
apiVersion: apps/v1
kind: Deployment
metadata: {name: minio-old}
spec:
  replicas: 1
  strategy: {type: Recreate}
  selector: {matchLabels: {app: minio-old}}
  template:
    metadata: {labels: {app: minio-old}}
    spec:
      containers:
      - name: minio
        image: quay.io/hummingbird-community/minio@sha256:25268b5a6539d9ffc7d23b89a2ba846d12a49aac4e81172336700222818d5f45
        args: [server, /data]
        env:
        - {name: MINIO_ROOT_USER, valueFrom: {secretKeyRef: {name: minio-secret, key: minio_root_user}}}
        - {name: MINIO_ROOT_PASSWORD, valueFrom: {secretKeyRef: {name: minio-secret, key: minio_root_password}}}
        volumeMounts: [{name: data, mountPath: /data, subPath: minio}]
        securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}, runAsNonRoot: true, seccompProfile: {type: RuntimeDefault}}
      volumes: [{name: data, persistentVolumeClaim: {claimName: minio-pvc}}]
---
apiVersion: v1
kind: Service
metadata: {name: minio-old}
spec:
  selector: {app: minio-old}
  ports: [{port: 9000, targetPort: 9000}]
EOF
oc -n minio rollout status deploy/minio-old --timeout=3m
```

Then copy with a one-off pod. The keys come through `secretKeyRef`: the old storage's from `minio-secret`, the new storage's from a copy of `seaweedfs-s3-credentials` (since 3.0.0 the new storage has keys of its own), made in `minio` first and deleted with the pod; the client image has no shell, so Kubernetes builds the endpoints from `$(AK)`/`$(SK)`. `--overwrite` is left out, so nothing already in the new storage is replaced:

```sh
new_key() { oc -n seaweedfs get secret seaweedfs-s3-credentials -o jsonpath="{.data.$1}" | base64 -d; }
oc -n minio create secret generic seaweedfs-keys \
  --from-literal=AWS_ACCESS_KEY_ID="$(new_key AWS_ACCESS_KEY_ID)" --from-literal=AWS_SECRET_ACCESS_KEY="$(new_key AWS_SECRET_ACCESS_KEY)"
oc -n minio apply -f - <<'EOF'
apiVersion: v1
kind: Pod
metadata: {name: mc-copy}
spec:
  restartPolicy: Never
  containers:
  - name: mc
    image: quay.io/hummingbird-community/minio-client:latest
    args: [mirror, old/pipelines, new/pipelines]
    env:
    - {name: MC_CONFIG_DIR, value: /tmp/.mc}
    - {name: AK, valueFrom: {secretKeyRef: {name: minio-secret, key: minio_root_user}}}
    - {name: SK, valueFrom: {secretKeyRef: {name: minio-secret, key: minio_root_password}}}
    - {name: MC_HOST_old, value: "http://$(AK):$(SK)@minio-old.minio.svc:9000"}
    - {name: NAK, valueFrom: {secretKeyRef: {name: seaweedfs-keys, key: AWS_ACCESS_KEY_ID}}}
    - {name: NSK, valueFrom: {secretKeyRef: {name: seaweedfs-keys, key: AWS_SECRET_ACCESS_KEY}}}
    - {name: MC_HOST_new, value: "http://$(NAK):$(NSK)@seaweedfs-s3.seaweedfs.svc:8333"}
    securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}, runAsNonRoot: true, seccompProfile: {type: RuntimeDefault}}
EOF
oc -n minio wait pod/mc-copy --for=jsonpath='{.status.phase}'=Succeeded --timeout=30m
oc -n minio logs mc-copy | tail -5
oc -n minio delete pod mc-copy secret/seaweedfs-keys
oc -n minio delete deploy/minio-old svc/minio-old
oc -n minio wait --for=delete pod -l app=minio-old --timeout=2m
oc -n minio delete networkpolicy minio-old-ingress
```

Old runs' artifacts resolve again, because KFP stores bucket and key only.

### 10.2 Models: the inference gateway and CPU models

The **Models** tab has two kinds of parts: the cluster's **inference gateway** (Gateway `openshift-ingress/openshift-ai-inference`, with its own **Set up the inference gateway** and Delete; a model setup never creates it) and the models, each on CPU in the chosen project (an LLMInferenceService of the model's name: `smollm2-135m-instruct`, `qwen25-05b-instruct` (the default), `qwen3-1-7b` or `granite-embedding-125m-english`), reached the way you choose under **Model access** ([README](README.md#models-small-models-on-cpu)): the inference gateway, MaaS only, another gateway that admits the project, or only inside the cluster. A project can hold several; Set up and Delete act on the chosen one. Objects: [CLUSTER_CHANGES §8](docs/CLUSTER_CHANGES.md#models-inference-gateway-cluster-and-cpu-models-a-project-you-choose).

Before Set up the tab says what is missing: model serving turned off (DataScienceCluster `kserve` not Managed, or `KserveLLMInferenceServiceDependencies=False`), no CPU preset on this release, no data science project, or too little room on the workers: the picker disables a model no amd64 worker has room for now, and says what it needs (SmolLM2 2 CPUs / 4 GiB, Qwen2.5 4 / 6 GiB, Qwen3 4 / 12 GiB, Granite 2 / 3 GiB requested). The check is a snapshot: a cluster that adds nodes on demand may still fit it; the API's setup only warns. A gateway someone else made (RHOAI's install, an admin) reads **Not managed by this tool** and is used as it is.

Each model also gets the dashboard's token authentication (ServiceAccount `<model>-sa` that may only `get` the model, Role `<model>-view-role`, RoleBinding `<model>-view`, token Secret `default-name-<model>-sa`, all owned by the model): the dashboard lists that token for the model, and its **Edit** keeps authentication on. Call a ready model with that token. The Models tab shows these commands only for a model this tool set up with its token: send a model only its own token, never your OpenShift token, because whoever can edit the project controls what answers at the model's address (SECURITY.md, [Which token is used where](SECURITY.md#which-token-is-used-where)). For a model deployed in the dashboard, use the token its Token authentication shows.

```bash
TOKEN=$(oc get secret default-name-qwen25-05b-instruct-sa -n <project> -o jsonpath='{.data.token}' | base64 -d)
URL=$(oc get llminferenceservice qwen25-05b-instruct -n <project> -o jsonpath='{.status.url}')
curl -s -H "Authorization: Bearer $TOKEN" $URL/v1/models
curl -s -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' $URL/v1/chat/completions \
  -d '{"model":"qwen25-05b-instruct","messages":[{"role":"user","content":"Say hello"}],"max_tokens":64}'
# Qwen3: thinking is off by default; turn it on for one request (streamed: it can take over a minute)
URL=$(oc get llminferenceservice qwen3-1-7b -n <project> -o jsonpath='{.status.url}')
curl -N -s -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' $URL/v1/chat/completions \
  -d '{"model":"qwen3-1-7b","messages":[{"role":"user","content":"Is 1001 prime?"}],"max_tokens":1024,"stream":true,"chat_template_kwargs":{"enable_thinking":true}}'
# The embedding model: one 768-number vector per input
URL=$(oc get llminferenceservice granite-embedding-125m-english -n <project> -o jsonpath='{.status.url}')
curl -s -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' $URL/v1/embeddings \
  -d '{"model":"granite-embedding-125m-english","input":["Red Hat OpenShift AI serves models."]}'
```

`401`: no or expired token. `403`: the token cannot `get` the model (each model's token is only for its own model). The `model` field is the resource name, as the dashboard's playground expects. The Qwen3 token above is in `default-name-qwen3-1-7b-sa`, the embedding model's in `default-name-granite-embedding-125m-english-sa`. Requests that take longer than about a minute must stream (`"stream": true`, and `curl -N`), because the cloud load balancer in front of the gateway closes a connection that sends nothing for 60 seconds (the AWS default): a non-streamed one gets no answer and ends after about 60 seconds (HTTP code `000` in curl).

| Symptom | Cause and fix |
|---|---|
| Model **Error**: "The cluster preset's image is not published yet" (`ErrImagePull ... manifest unknown`) | A nightly build's CPU preset names a runtime image Red Hat has not published. Use **Use the newest released Red Hat image** on the model: it sets the newest released `registry.redhat.io/rhaii/vllm-cpu-rhel9` image, pinned by digest. Or mirror `registry.redhat.io/rhaii` to `registry.stage.redhat.io/rhaii` (ImageDigestMirrorSet) with a pull credential for that registry, then set the model up again. On disconnected clusters set `VLLM_CPU_IMAGE` on the updater |
| Model **Error**: "The cluster preset's image may not be published" (a mirrored pull: one source `manifest unknown`, another `unauthorized`) | The pull failed for longer than a setup waits (8 minutes). Either the image is not published (as above: **Use the newest released Red Hat image**) or the node lacks the cluster's pull secret for the mirror (on ROSA, check the additional pull secret; a node that just joined gets it a few minutes later) |
| Setup fails at "Check that it can be called": "The model's server answered 404: it does not serve a model named ..." | The request reached the model, but its `"model"` was not the served name. Send exactly the name under **Model name in requests** (`spec.model.name`, the catalog name); the display name and the Hugging Face id get `{"error":{"message":"The model ... does not exist","type":"NotFoundError","code":404}}` |
| Setup fails at "Check that it can be called": "No answer at ... (404, empty body)" | The gateway did not route the request to the model: it has not loaded the route yet (the check retries for 90 s), or the route is not accepted. `oc get httproute -n <project>` and the route's status; **Check again** once it is accepted |
| "Can it be called?": **Answers without a token** | The model answered a request without a token through its gateway: the gateway does not check who calls. `oc get authpolicy -n openshift-ingress` (RHOAI's `<gateway>-authn` must be enforced), check that odh-model-controller and Connectivity Link run, and that the gateway is not labelled `opendatahub.io/managed=false`. Until then anyone who reaches the address can use the model |
| Setup fails: "Created, but not ready after 10:00: its image pull is still failing" | The ready wait ended while a failing pull was still being waited on (a node that joined minutes ago). Same checks as the pull failure below; the model starts once the pull succeeds |
| "Can it be called?": 401 or 403 | 401: the gateway refused the model's own token; check that odh-model-controller and Connectivity Link run (`oc get authpolicy -n openshift-ingress`). 403: the token's ServiceAccount may not get the model; set the model up again (it restores Role `<model>-view-role`) |
| "Can it be called?": "The address's hostname does not resolve" | A new gateway's DNS record is not published yet; **Check again** in a few minutes |
| "Can it be called?": Not checked, in-cluster address | The model is reached only inside the cluster and this app cannot reach it (a NetworkPolicy) or does not trust KServe's own certificate. Call it from a pod on the cluster with the command under **Use it** |
| Model setup refuses: "authenticated by Red Hat Connectivity Link, which is not set up" | The chosen way goes through a gateway, and KServe does not start a model with token authentication without Connectivity Link's AuthPolicy API. **Set up what Models needs** (with that choice) installs it and its Kuadrant instance; or choose **Only inside the cluster** |
| Setup fails: "the image of container main cannot be pulled ..., for 2:00" (or 8:00 on a new node) | The pull kept failing past the wait. Check that the cluster's pull secret can read the image (on ROSA, the additional pull secret; a node that just joined gets it a few minutes later), that a mirror covers its registry, and that the image exists |
| "The gateway accepts projects that don't use it": a project "deleted" or "no model uses the gateway" | Its editors (or whoever creates a project of that name later) can attach routes to the shared gateway. **Remove** takes it off after confirmation; every check runs before the write and again if the write is retried, and it is refused while a model there routes through the gateway or when the models, the Gateway or KServe's configuration cannot be read. Projects this tool added are taken off by themselves when their last model on the gateway is deleted, and once deleted, at the next Models change or Clean up |
| No way to reach a model is recommended, though a gateway exists | The tool recommends only gateways whose token checks it can read: in `openshift-ingress` and not labelled `opendatahub.io/managed=false`. Choose another gateway explicitly if you trust it. When KServe's `inferenceservice-config` names no default gateway (or cannot be read), the default route is not offered: choose the inference gateway by name |
| Model stays **Setting up**, "Waiting for a worker with room" | `FailedScheduling: Insufficient cpu/memory`. Free what the picker says the model reserves on one amd64 worker, or add a node; it starts by itself |
| Model **Error**: "keeps crashing ... OOMKilled" | The memory limit was too small (each limit is the model's memory measured idle on RHOAI 3.6 with vLLM CPU 0.24, plus 1.7 to 2.7 GiB for load): `oc logs deploy/<model>-kserve -n <project> -c main --previous`, and report the model and its peak |
| Qwen3 answers without `tool_calls`, or a tool request is refused | The model is served with `--enable-auto-tool-choice --tool-call-parser hermes`; check them in `oc get deploy qwen3-1-7b-kserve -n <project> -o yaml` (env `VLLM_ADDITIONAL_ARGS`) and the vLLM log for an unknown option |
| The embedding model refuses an input | Its context is 512 tokens; split longer text |
| A model reads "not created by this tool" although this tool set it up | A model is the tool's only when its UID is in the tool's record (snapshot ConfigMap, key `models`). A model a pre-release 3.0.0 build set up has none, nor one made again elsewhere under the name. Delete it yourself (`oc delete llminferenceservice <model> -n <project>`, after unpublishing it on the MaaS tab), then set it up again |
| Model details: "has no token of its own yet", and Use it shows no commands | Set up before the token objects existed: **Set up again to create its token** (only the missing objects are made); the commands appear once the token Secret is recorded. "Off: ... enable-auth is false": someone turned token authentication off in the dashboard; turn it back on there |
| Set up refuses: "already has a ServiceAccount / Role / RoleBinding / Secret ... that this tool did not make for this model" | An object of the dashboard's token name for this model exists and is not owned by the model (left over, or someone else's). Delete it (`oc delete <kind> <name> -n <project>`) or use another project |
| Set up warns "no room in its quota" | A ResourceQuota of the project leaves less than the model requests (`oc describe resourcequota -n <project>`). Raise it or stop other workloads; KServe cannot create the pod until then |
| Model stays **Setting up**, `Insufficient ephemeral-storage` | The model requests node disk for its download (about 1.5 times it; KServe's default model volume is an emptyDir). Free disk on a worker or add one |
| Hardware profile "None fits" (Qwen3) | No profile allows 4 CPUs and 12-14 GiB (the default allows 8 GiB): the dashboard's Deployments table shows "No hardware profile" and the details "Hardware profile: Unknown". The model runs as set up |
| A model deployed in the dashboard never becomes ready; its HTTPRoute says `NotAllowedByListeners` | The inference gateway does not list its project (`oc get gateway openshift-ai-inference -n openshift-ingress -o jsonpath='{.spec.listeners[*].allowedRoutes}'`). The Models tab and the Overview name the model: **Allow <project>** adds the project. For a gateway this tool did not create, a cluster admin adds the project to the values array of its listeners' `allowedRoutes` |
| After Unpublish, the direct address does not answer | The inference gateway does not list the project (someone took it off while the model was published): **Allow <project>** on the Models tab adds it back. For a gateway this tool did not create, a cluster admin adds the project to the values array of its listeners' `allowedRoutes`. A model set up **Through MaaS only** has no direct address after Unpublish, by design ([§10.4](#104-maas-models-as-a-service)) |
| **Delete model** refuses: "is published to Models as a Service. Unpublish it on the MaaS tab first; nothing was deleted." | The model was published on the MaaS tab: unpublish it there, then delete it. A model with no route of its own before it was published (set up **Through MaaS only**, or set up **Only inside the cluster** and then published) is not refused: Delete unpublishes it first (step "Unpublish ... from MaaS"), then deletes it. When that unpublish stops (MaaS cannot be read, or someone added a gateway to the model), the model is not deleted, and the message says why |
| Model **Error**: "KServe cannot resolve its preset" | The preset named in `spec.baseRefs` is gone (RHOAI was updated and its prefix changed). Set the model up again: it re-reads the prefix |
| Gateway **Needs attention**: "The gateway's authentication is missing" | odh-model-controller adds AuthPolicy `openshift-ai-inference-authn` once a model uses the gateway; check that odh-model-controller and Connectivity Link (Kuadrant) run: `oc get authpolicy -n openshift-ingress` |
| Gateway **Setting up** for minutes | `oc get gateway openshift-ai-inference -n openshift-ingress -o yaml`: `Programmed=False` names the reason (no load balancer quota, for example). DNS for a new load balancer can take a few minutes |
| Gateway **Unknown · not managed by this tool** | Someone else's gateway that reports no `Programmed` condition: the tool cannot tell; ask its owner |
| **Delete inference gateway** unavailable | Models route through it (named), or routes are attached to it (`oc get httproute -A`). Delete those first; models of other tools are never deleted here |

Rollback: **Delete model**, then **Delete inference gateway** (or **Clean up Models**, which runs both in that order). By hand, only for what the tool made (`app.kubernetes.io/managed-by=rhoai-nightly-updater`): `oc delete llminferenceservice <model> -n <project>` for each model, then `oc delete gateway openshift-ai-inference -n openshift-ingress`, `oc delete configmap openshift-ai-inference-options -n openshift-ingress`, `oc delete gatewayclass openshift-ai-inference`.

### 10.3 Playground: the dashboard's Gen AI playground

The **Playground** tab asks the RHOAI dashboard's own gen-ai API to create or delete its Gen AI playground in a project. Calls and objects: [CLUSTER_CHANGES §8](docs/CLUSTER_CHANGES.md#playground-the-dashboards-gen-ai-playground-a-project-you-choose).

Chat with a ready playground from a terminal (the dashboard checks that your user may use the project):

```bash
TOKEN=$(oc whoami -t)
curl -s -X POST "https://rh-ai.<apps domain>/gen-ai/api/v1/lsd/responses?namespace=<project>" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"model":"qwen25-05b-instruct","model_source_type":"namespace","input":"Say hello","stream":false}'
```

For a Models as a Service model, the body names the source and a subscription to the model, `"model_source_type":"maas","subscription":"<name>"` (this tool's is `rhoai-nightly-updater-subscription`): the dashboard mints the MaaS key for that subscription, and without one the playground answers 500. The Playground tab's **Use it** builds the command for the chosen model.

| Symptom | Cause and fix |
|---|---|
| Set up unavailable: "Needs: Models" | The project has no ready chat model. Set one up on the Models tab, or wait until it is ready |
| Set up offers no Models as a Service model | Since 4.0.0 Set up lists the MaaS models the dashboard offers you (it runs with your own token): you need a subscription that includes the model. The status lists every published model, read from their MaaSModelRefs; choosing one you are not subscribed to stops Set up with "The dashboard does not offer model ..." before anything changes |
| A playground this tool set up now reads "not managed by this tool" | Its OGXServer has another UID than the one recorded: it was deleted and made again in Gen AI studio, or the project was recreated. The new one is someone else's; the next Set up or Delete in the project drops the old record. Delete it in Gen AI studio if you no longer need it |
| "This RHOAI release's dashboard gen-ai API differs" | A gen-ai path answered 404 on this release. Use the playground from the dashboard's Gen AI studio; report the call the message names |
| "The dashboard's gen-ai API refused the request (HTTP 401 or 403)" | Set up, Delete and Clean up call it with your own token: your sign-in expired (log out and log in again), or you may not use the project (the dashboard checks `list ogxservers` in it: `oc auth can-i list ogxservers.ogx.io -n <project>`). For the status, which uses the updater's ServiceAccount: re-apply the template (`make upgrade`); `oc auth can-i list ogxservers.ogx.io -n <project> --as=system:serviceaccount:<ns>:<sa>` must say yes |
| Install fails: "An earlier playground ... left objects behind" | ConfigMaps of a playground whose server is gone (400 "already exists"). Someone with edit rights deletes the one named, for example `oc delete configmap -n <project> llama-stack-config` (check `oc get configmap -n <project> -l opendatahub.io/dashboard=true`), then Set up again |
| "still being removed after 3:00" | The previous playground's database (`genai-pgvector*`) is still terminating: `oc get deploy,pvc -n <project> -l gen-ai.opendatahub.io/pgvector=true`. Set up again once it is gone |
| Playground **Error** (phase Failed) | `oc get ogxserver lsd-genai-playground -n <project> -o yaml` and `oc get events -n <project>`; the OGX operator's webhook and `default-ogx` must be ready. Delete and set up again |
| Delete unavailable, **Not managed by this tool** | Someone set it up in the dashboard; delete it there |

Rollback: **Delete** in the playground's menu (or **Clean up Playground**). By hand, the dashboard's way: `oc delete ogxserver lsd-genai-playground -n <project>`, then the `genai-pgvector*` objects (`oc delete deploy,svc,networkpolicy,pvc,secret,configmap -n <project> -l gen-ai.opendatahub.io/pgvector=true`), then remove `<project>` from the `playgrounds` key of ConfigMap `rhoai-nightly-updater-snapshot` in the updater's namespace.

### 10.4 MaaS: Models as a Service

The **MaaS** tab sets up the MaaS platform as the MaaS install docs do and publishes this tool's model. Objects and order: [CLUSTER_CHANGES](docs/CLUSTER_CHANGES.md#maas-models-as-a-service-cluster-and-this-tools-published-models).

```bash
INFRA=redhat-ai-gateway-infra   # odh-ai-gateway-infra on ODH
oc get dsc -o jsonpath='{.items[0].spec.components.aigateway}'; echo
oc get secret maas-db-config -n $INFRA --show-labels                                  # which database (never print its data)
oc get deploy,svc,secret -l app=postgres -n $INFRA                                     # the docs' development database
oc get clusters.postgresql.cnpg.io maas-db -n $INFRA                                   # the CloudNativePG one
oc get configmap rhoai-nightly-updater-snapshot -n rhoai-nightly-updater -o jsonpath='{.data.maas}'; echo   # "database": the kind chosen
oc get gateway maas-default-gateway -n openshift-ingress
oc get deploy maas-api -n $INFRA
oc get aitenant -n ai-tenants; oc get maastenantconfig -n models-as-a-service
oc get authpolicy -n openshift-ingress
oc get maasmodelref -A; oc get maassubscription,maasauthpolicy -n models-as-a-service
```

| Symptom | Cause and fix |
|---|---|
| Set up or Publish refuses: "Namespace models-as-a-service (or redhat-ai-gateway-infra) was made through a project request by ..." | Someone made that namespace with a project request before MaaS ran, and MaaS takes over a namespace of that name, which would make them an admin of a namespace the MaaS gateway admits routes from. `oc get namespace <name> -o yaml`: check `openshift.io/requester` and what runs there. If it is not RHOAI's, delete it (`oc delete namespace <name>`); RHOAI makes it again, then retry |
| Set up stops at Connectivity Link or CloudNativePG | Set up installs them from `redhat-operators` and `certified-operators`; the message says why OLM did not (`oc describe subscription kuadrant-operator -n kuadrant-system`, `oc describe subscription cloudnative-pg -n cnpg-system`). Fix that, then **Set up** again: it continues where it stopped. A catalog that does not offer the package, more than one OperatorGroup in the namespace, or one that targets only some namespaces refuses before anything changes |
| Set up stops: "Namespace ... does not exist, and this tool creates only the AI gateway's own infrastructure namespaces" | A custom applications namespace without an infrastructure namespace: create the namespace maas-api uses yourself, then Set up again |
| **Authorino TLS** needs attention | **Configure Authorino TLS** on the part (or **Set up**) applies the docs' `setup-authorino-tls.sh` steps; **Do it from the CLI instead** has the same commands. Until then API keys are refused |
| maas-api restarts | It restarts until the database answers: `oc logs deploy/maas-api -n $INFRA --previous` names the connection error. CloudNativePG: `oc get cluster maas-db -n $INFRA`; development: `oc get deploy postgres -n $INFRA`; your PostgreSQL: maas-api must reach it from the cluster (host, port, credentials, `sslmode`); fix it with **Change database** (Your PostgreSQL again, with the right URL) |
| Set up refuses: "Choose where MaaS keeps its API keys" | A page from before 4.0.0 sent Set up without the database choice: reload the page; the dialog asks |
| Set up refuses: "Deployment ... /postgres exists and was not created by this tool" | Someone ran the docs' `setup-database.sh` (same names) without finishing `maas-db-config`. Its owner deletes the objects, or choose another database |
| Development database: API keys stopped working; the Database part warns "pod was replaced" | Its pod has no volume: a restart loses every key, and maas-api makes its tables only when it starts. `oc rollout restart deployment/maas-api -n $INFRA`, then create keys again. For keys that last, **Change database** to your PostgreSQL or the CloudNativePG one |
| **Change database** is not offered | Only for a database this tool set up that is ready, and while nobody else uses MaaS (the part says who); a `maas-db-config` someone else wrote is never changed |
| Publish refuses: "Who can use the published models is set for all of them" | The audience is the one subscription's: change it with **Change who can use them**, then Publish |
| "Group ... does not exist" under Published models, or "... is not a group on this cluster" in the dialog | The group has no OpenShift Group: copy the commands the warning shows (`oc adm groups new <group>`, `oc adm groups add-users <group> <user>`). Groups your identity provider adds at sign-in (OIDC names such as `/admins`) have none and still work; a name that cannot be an OpenShift Group's has no commands |
| Unlimited is disabled: "This cluster's MaaS has no unlimited option", or Publish refuses with it | The cluster's MaaSSubscription API (its CRD's schema) has no `unlimited`: choose a limit |
| Set up refuses: "Your PostgreSQL is set up already, and Set up does not change its connection URL" | Use **Change database** on the Database part with **Your PostgreSQL** and the new URL (keys stay only if it is the same database, for example with a new password) |
| Change database refuses: "... is not available" / "is not ready" | It runs only from a database that runs: **Set up** first finishes the current one, then change |
| Set up refuses: "Secret .../postgres-creds ... not created by this tool" | A Secret of that name exists that this tool did not record creating (it never reads that Secret, so it cannot tell whose it is): its owner deletes it, or choose another database. Remove likewise keeps a `postgres-creds` it has no record of, and the log says so |
| 429 sooner or later than expected | `oc get maassubscription rhoai-nightly-updater-subscription -n models-as-a-service -o jsonpath='{.spec.modelRefs}'` shows each model's limit; tokens are counted from responses that report usage |
| Gateway not programmed, or `maas.<apps domain>` does not resolve | OpenShift is making the load balancer; `oc get dnsrecords -n openshift-ingress` shows the record |
| Connectivity Link version warning | The MaaS docs ask for RHCL 1.5 or later; 1.4.x can hang callers with two or more subscriptions (Publish then refuses while other MaaSSubscriptions exist), and 1.4.0 bypasses gateway authentication (Publish refuses). **Upgrade Connectivity Link** switches the Subscription to the channel the catalog shows meets 1.5; when OLM stays at the old version (`AtLatestKnown`: seen live, 1.5.0 skips only `>=1.4.4 <1.5.0`), it goes through a channel that lists the installed version and has a newer one (`stable-1.4`), then to the target, and sets Authorino TLS up again (the 1.4.4 step recreates Authorino's Service, which removes its certificate). It refuses a Subscription with Manual approval, one managed by Argo CD, or one not named `kuadrant-operator` or `rhcl-operator`. 1.5 replaces the separate Authorino, Limitador and DNS operators with one (`oc get kuadrantcontrolplane default`); a hand-installed upstream MCP gateway controller then fights its MCP controller |
| Connectivity Link part recommends denying traffic when rate limiting fails | The MaaS docs set `RATELIMIT_CHECK_SERVICE_FAILURE_MODE` and `RATELIMIT_REPORT_SERVICE_FAILURE_MODE` to `deny` on the Connectivity Link Subscription, so requests are refused instead of let through unlimited while Limitador is unavailable. A Subscription this tool creates has them; one installed otherwise is not changed by Set up: **Deny traffic when rate limiting fails** sets them and keeps the other env entries (the part also shows the docs' `oc patch`) |
| Kuadrant operator OOMKilled (the Connectivity Link part says so) | Known issue of RHCL 1.5.0 (CONNLINK-1878; its CSV limits it to 500Mi). **Raise the memory limit** sets the Subscription's `spec.config.resources` with a 1Gi memory limit and the CSV's other values (a patch to the Deployment is reverted by OLM). Offered only after an OOM kill is seen: `oc get pods -n kuadrant-system -o jsonpath='{range .items[*]}{.metadata.name} {.status.containerStatuses[*].lastState.terminated.reason}{"\n"}{end}'` |
| **Old gateway policies** needs attention | A tier-based MaaS (RHOAI 3.2) left `gateway-auth-policy` and `gateway-tier-rate-limits` on `maas-default-gateway`; RHOAI does not remove them, and with old and new gateway policies Kuadrant's behaviour is undefined. **Remove the old tier policies** deletes those two by name; any other policy not managed by maas-controller is only listed, with its `oc delete` |
| Publish refuses: Gateway maas-default-gateway admits routes from every namespace | Someone else's gateway with `allowedRoutes` `from: All`: apply the `oc patch` the gateway part shows (the docs' namespace list). On this tool's own gateway, **Set up** writes the list back |
| Model delete refused: published | Unpublish it on the MaaS tab first; a model with no route of its own is unpublished by Delete itself ([§10.2](#102-models-the-inference-gateway-and-cpu-models), **Delete model** refuses) |
| A published model's direct address (`inference.<apps domain>/<project>/<model>`) answers 404 | Expected: while published, the model routes through `maas-default-gateway` only. Use it through MaaS with an API key (the MaaS tab's commands) or the dashboard's playground; Unpublish brings the direct address back |
| After Unpublish, a model set up **Through MaaS only** has no outside address | Expected: it never had a route of its own, so Unpublish removes its route (`spec.router`) and it answers only at its in-cluster address, as **Only inside the cluster**. It is never put on the inference gateway. `oc get llminferenceservice <model> -n <project> -o jsonpath='{.spec.router}'` is empty. To reach it from outside, delete it and set it up again through a gateway |
| A published model's direct address answers 500 (Envoy log: `kuadrant_wasm_shim: Failed to apply predicates ... selected_subscription_id`) | A publication by an earlier build, which kept the model on the inference gateway too; the MaaS tab shows it as "Partly published". Unpublish, then Publish again. `oc get llminferenceservice <model> -n <project> -o jsonpath='{.spec.router.gateway.refs}'` then names `maas-default-gateway` alone |
| Publish refuses: "The spec of <model> ... was changed since this tool set it up: <fields>" | Someone (a project editor, `oc edit`, the dashboard's Edit) changed what the model runs (the fields named: image, model URI, preset, command, environment, volumes, ...), so it may no longer be the catalog's model, and it is not offered to every signed-in user. `oc get llminferenceservice <model> -n <project> --show-managed-fields -o yaml` shows who wrote what. Set it up again on the Models tab, then publish; if a field someone added stays, delete the model there and set it up again |
| Publish or Unpublish stops with "Partly published: the model's route ..." | KServe did not move the model's HTTPRoute within 2 minutes: `oc get httproute <model>-kserve-route -n <project> -o jsonpath='{.spec.parentRefs}'`, and the model's `HTTPRoutesReady` condition. Unpublish (again) once KServe runs |
| Unpublish refused: someone added a gateway | Someone added a gateway to the published model; writing back the refs from before publishing would drop it. Set the refs the model should use without `maas-default-gateway` (the message has the `oc patch` for KServe's default), then Unpublish again |
| The embedding model has no Publish | This tool publishes chat models only; use the embedding model at its own address, or in the playground |
| MaaS gateway: "still admits routes from published projects that are gone" | A published project was deleted (or its model deleted or made again). The background check (at start and every 5 minutes, when no other operation runs) or the next MaaS, Models or Clean up operation drops the publication and the project from the gateway; the Activity log shows "prune-maas-publications" when it changed something. "record of its models cannot be read": check the `models` key of ConfigMap `rhoai-nightly-updater-snapshot`; until it reads, a remade model's publication stays. A project or model that cannot be read is kept until it can. For a gateway this tool did not create, its owner removes the names from its `allowedRoutes` |

Rollback: **Clean up MaaS** (or Clean up everything). It keeps Connectivity Link, its Kuadrant instance and Authorino TLS, and CloudNativePG, also when this tool installed them (data key `maas`, field `shared`, of ConfigMap `rhoai-nightly-updater-snapshot` lists what it installed): RHOAI's inference gateway authentication uses Kuadrant. Remove them from the OpenShift console only if nothing needs them. By hand, in order: `oc delete maasauthpolicy rhoai-nightly-updater-access maassubscription rhoai-nightly-updater-subscription -n models-as-a-service`, `oc delete maasmodelref <model> -n <project>` for each published model, set `aigateway.modelsAsAService` and `aigateway.managementState` back to the values in data key `maas` (`previousModelsAsAService`, `previousAIGateway`), then `oc delete gateway maas-default-gateway -n openshift-ingress`, `oc delete secret maas-db-config -n $INFRA`, `oc delete cluster.postgresql.cnpg.io maas-db -n $INFRA`, `oc delete namespace $INFRA` if the record's `infraNamespace` names it and nothing else is in it, and remove the `maas` key.

## 11. Manual recovery for operator deadlocks

These follow the deadlock map in the RHOAI 3.6 operator notes (D1–D8). The tool refuses the flows that cause them, but a cluster can reach them by other means. All recovery steps are **MANUAL ADMIN STEPs**.

### 11.1 D1: dashboard-operator scaled to 0 (Dashboard CR deletion hangs)

Only the dashboard module's operator removes the Dashboard CR finalizer `components.platform.opendatahub.io/cleanup`. If it stays at 0 while the Dashboard is being removed, the DSC change or uninstall waits forever.

The tool does not assume its name or namespace: the namespace is the DSCInitialization's `spec.applicationsNamespace`, and the operator is the Deployment there that carries Dashboard Dev's annotation (or, without a session, the one whose ServiceAccount a ClusterRoleBinding lets update `dashboards` and `dashboards/finalizers`; on RHOAI 3.6 it is `dashboard-operator`). The Dashboard Dev page and its banner name it.

```bash
NS=$(oc get dscinitializations -o jsonpath='{.items[0].spec.applicationsNamespace}')
OP=$(oc get deploy -n "$NS" -o json | jq -r '.items[] | select(.metadata.annotations["rhoai-nightly-updater.opendatahub.io/dashboard-dev"]) | .metadata.name')
echo "$NS/$OP"   # empty OP: no session annotation; use the name the Dashboard Dev page shows
oc get deploy "$OP" -n "$NS" -o jsonpath='{.spec.replicas}{"\n"}'
oc get dashboards.components.platform.opendatahub.io -o jsonpath='{range .items[*]}{.metadata.name} {.metadata.deletionTimestamp}{"\n"}{end}'
```

Fix: **Revert** on Dashboard Dev. If the UI is unavailable (MANUAL ADMIN STEP):

```bash
oc scale deploy/"$OP" -n "$NS" --replicas=1
oc annotate deploy/"$OP" -n "$NS" rhoai-nightly-updater.opendatahub.io/dashboard-dev-
```

The operator then restores the release images and finishes the deletion.

### 11.2 D3: stale CRD conversion webhook

All reads and writes that need conversion fail with `conversion webhook ... service "<x>" not found`, and garbage collection and namespace deletion stall. List the CRDs that use a webhook, then check each Service:

```bash
oc get crd -o jsonpath='{range .items[?(@.spec.conversion.strategy=="Webhook")]}{.metadata.name}{"  "}{.spec.conversion.webhook.clientConfig.service.namespace}/{.spec.conversion.webhook.clientConfig.service.name}{"\n"}{end}'
oc get svc -n <namespace> <service>          # NotFound = stale
```

Recovery (MANUAL ADMIN STEP), in order of preference:
1. **A CRD that holds no objects: delete it.** Removing a module leaves its CRDs (they get no owner) with the conversion webhook its operator served, so setting the module to `Managed` and back to `Removed` ends in the same state. Check with `oc get <crd-name> -A` (Diagnostics checks this itself when this app may list the kind and says so in the problem); when it lists nothing, `oc delete crd <crd-name>`. The operator applies the CRD again when the module is set to `Managed`.
2. **A CRD with objects: bring the producer back.** If the module was set to `Removed` in the DSC, set it to `Managed` again, wait for its operator and Service, migrate or delete the affected CRs, delete the CRDs, then remove the module again.
3. **DSC/DSCI CRDs** while the RHOAI operator is not installed: Install a different build from the UI does this safely (it switches conversion to `None` only when no RHOAI CSV exists and the Service is missing).
4. **Last resort, lossy:** `None` converts no fields; reads at another version return the stored shape. Only for CRDs whose objects you can lose, and the owning operator may revert it:
   ```bash
   oc patch crd <name> --type=merge -p '{"spec":{"conversion":{"strategy":"None","webhook":null}}}'
   ```

### 11.3 D5: `opendatahub.io/managed: "false"` on an operator-applied resource

The operator skips that resource and drops its owner reference, so it is frozen across upgrades. On RHOAI 3.5+ the tool never sets it (only the legacy Dashboard Dev flow on installs without dashboard-operator did).

```bash
oc get deploy -A -o jsonpath='{range .items[?(@.metadata.annotations.opendatahub\.io/managed=="false")]}{.metadata.namespace}/{.metadata.name}{"\n"}{end}'
```

Fix (MANUAL ADMIN STEP): `oc annotate deploy/<name> -n <ns> opendatahub.io/managed-`. Then check that the operator re-applies it: its `platform.opendatahub.io/version` annotation should match the installed version.

### 11.4 D7 / D8: MLflow or pipeline server finalizers

**D7.** The MLflowOperator finalizer blocks while any MLflow CR exists. Disabling `mlflowoperator` or deleting the DSC then waits.

```bash
oc get mlflows.mlflow.opendatahub.io
```

Fix: **Delete MLflow** (the MLflow server's menu) on the Environment page. For an MLflow CR the tool didn't create (MANUAL ADMIN STEP): `oc delete mlflows.mlflow.opendatahub.io <name>`.

**D8.** Only the pipelines operator (DSPO) removes the DSPA finalizer. If pipelines were disabled, or DSPO is gone, a DSPA and its project hang in Terminating.

```bash
oc get datasciencepipelinesapplications -A
oc get deploy -n redhat-ods-applications | grep -i pipeline
```

Fix (MANUAL ADMIN STEP):
1. Re-enable `aipipelines` (DSC `Managed`) so DSPO runs.
2. Delete the DSPAs.
3. Disable it again.

Removing the finalizer by hand (`oc patch dspa <name> -n <ns> --type=merge -p '{"metadata":{"finalizers":null}}'`) skips DSPO's cleanup and leaves its resources behind. Use it only if the project is being deleted anyway.

Stuck **module CRs** (`components.platform.opendatahub.io`, and `services.platform.opendatahub.io` for `databaseservice` on 3.6): Diagnostics shows the owning operator and why it isn't finishing. Start that operator rather than stripping the finalizer. Stripping a module CR's finalizer, by hand or by the tool, skips that operator's cleanup: what the CR owns is garbage-collected once the CR is gone, but anything the finalizer would have cleaned up that the CR does not own (objects in other namespaces, cluster-scoped objects without an owner reference to the CR) can be left behind. Install a different build strips one only after 10 minutes of deletion, and only when its operator is gone. The operator is identified as in [§11.9](#119-change-or-reset-the-datasciencecluster) ("How the module operator is identified"), by the ServiceAccounts a ClusterRoleBinding lets update the CR and its `<plural>/finalizers` (or `<plural>/status`): the platform deletes that binding only after the CR is gone, so it still names the operator when its Deployment is gone. "Gone" means nothing in the applications namespace runs as one of those ServiceAccounts, or as any other that may update the CR and its finalizers or status: no Deployment (whatever owns it and however many replicas it has) and no Pod that is not Succeeded or Failed, whatever controls it (`oc get pods -n <ns> -o custom-columns=NAME:.metadata.name,SA:.spec.serviceAccountName,PHASE:.status.phase`). Everything else leaves the finalizer and logs a warning in the install's logs: a Deployment that still exists ("its operator ... still exists"); an operator the tool cannot identify ("is unknown (...)": no such ServiceAccount, for example Kueue, which the RHOAI operator reconciles itself, or a module whose binding is gone too); a read error ("cannot identify the operator ..."). The ServiceAccounts and Deployments the tool reads are listed by the commands in §11.9 (use `<plural>/finalizers` of the stuck kind).

### 11.5 Recurring `BundleUnpackFailed`

OLM's documented recovery ("Refreshing failing subscriptions"), as a MANUAL ADMIN STEP:
1. Delete the Subscription and CSV. Install a different build in the UI does this.
2. Delete the failing bundle-unpack **Job and ConfigMap** in `openshift-marketplace`: `oc get job,cm -n openshift-marketplace | grep <bundle-hash>`. The tool never deletes these.
3. Install a different build (Recovery on the Status page).

### 11.6 Upgrade leftovers the operator cannot fix itself

The DataScienceCluster, the Platform and the module CRs report objects the operator failed to apply as `failure deploying resource <ns>/<name>: apply failed <group/version>, Kind=<Kind>: …`. Diagnostics lists each object once, with the CRs that report it and exact commands, under one of two checks: **Left over from an earlier release** (the two kinds the tool can fix, below, with a **Fix** button) or **Objects the operator cannot update** (everything else, guidance only; it changes nothing). Find them by hand with:

```bash
oc get datasciencecluster -o jsonpath='{range .items[*].status.conditions[*]}{.message}{"\n"}{end}' | grep 'failure deploying'
oc get platforms.config.opendatahub.io -o jsonpath='{range .items[*].status.conditions[*]}{.message}{"\n"}{end}' | grep 'failure deploying'
```

**Left over from an earlier release** (live on RHOAI 3.6.0 after upgrades over several releases: the DSC was Not Ready while every component said Ready). Every fix runs with your token, reads everything again first, and changes nothing unless each check passes.

<img src="docs/images/diag-leftover-fields.png" alt="Diagnostics: TrustyAI default-trustyai has field values from an earlier release, with the operator's error, a table of the fields (stored false, after deny, the CRD default) and a Fix button" width="760">

1. **Stored field types**: `... to smd typed: errors: .spec.<path>: expected <type>, got ...`. A module CR still holds a value of the type an earlier release wrote (here `permitOnline: false` where the CRD now has a string enum `allow`/`deny` with default `deny`); Kubernetes never rewrites a stored object. The API server raises this error only for an object without `managedFields` (server-side apply then first records the live object's fields, and a value of the old type fails that conversion; kubernetes v1.34 `managedfields/internal/skipnonapplied.go:75-91`; a non-apply update that cannot track the fields removes them, `fieldmanager.go:145-164`). The module operator often crash-loops too ("failed waiting for *v1alpha1.<Kind> Informer to sync"); Diagnostics links that pod problem here when the pod's last exit says so.
   1. **Observed**: the operator's error, each field's stored value and what the installed CRD says (type, values, default), and who last wrote the CRD's schema.
   2. **Fields**: per field, the stored value and the value after: the CRD's **default**, or **Removed** for an optional field without one. A required field without a default has no automatic fix. The tool cannot know what a value of the older type meant, so every change (`false` included) shows old -> new and needs your acknowledgement that the new value is right; nothing relates the old value to the default.
   3. **Fix** patches only those fields: a JSON Patch that tests the object's UID and resourceVersion and each stored value, after a server-side dry run (`fieldValidation=Strict`). The tool fixes only when it can tell that the CRD, not the object, is the current shape: the installed CSV owns the CRD (`spec.customresourcedefinitions.owned`), or the CRD's `spec.versions` was last written by the RHOAI operator's own field manager `platform.opendatahub.io` (its deploy action applies CRDs with that owner, odh-platform-utilities framework v0.3.1 `deploy/action_deploy.go:389-396`). Anything else (OLM for a bundle the installed CSV does not own, a person, another controller, two writers at the same second) is refused: "can't tell that CRD ... is the newer side" (the downgrade case of step 3 below).
   4. The acknowledgement binds to what you reviewed: the problem carries the object's UID and resourceVersion and a hash of the statement, the Fix dialog sends them back, and the server refuses ("changed since you reviewed it", `conflict`) when the object or the statement is different now. Run diagnostics again and review it.
   The DataScienceCluster controller watches module CRs, so it applies the object again by itself (rhods-operator rhoai-3.6). If a re-scan still shows the error with every field valid, the problem becomes "still reports an error ... that is already fixed" with **Retry now**: it adds the annotation `rhoai-nightly-updater.opendatahub.io/retry` to the DataScienceCluster and removes it again, which makes the operator reconcile at once. By hand: `oc annotate datasciencecluster default-dsc rhoai-nightly-updater.opendatahub.io/retry=now`, then the same with `rhoai-nightly-updater.opendatahub.io/retry-`.
2. **Changed selector**: `spec.selector: Invalid value: ...: field is immutable` on a Deployment, StatefulSet, DaemonSet or Job (the only failure for that object). An older release created it with another selector. **Fix** deletes it (UID and resourceVersion preconditions, propagation Background, after a dry run) so the operator creates it again, then waits up to 2 minutes for it to be back and ready, and says so if it is not. It refuses when the object is not in the applications namespace (read from the DSCInitialization), its controller owner is not a module CR or the DataScienceCluster that reports the failure, or that owner's operator has no available pod.

<img src="docs/images/diag-leftover-recreate-dialog.png" alt="The Fix dialog for recreating Deployment trustyai-service-operator-controller-manager: the acknowledgement checkbox, what stops meanwhile, and the Fix button" width="760">

   1. The workload is down until the operator creates it again; changes to that component's resources wait meanwhile. When the tool cannot tell whether the component is in use (it has no reader for most components' resources), or the pods serve failurePolicy Fail webhooks, you tick an acknowledgement first. The server refuses the fix without it, and with it when the Deployment's UID or resourceVersion, or the reasons, changed since the scan (`conflict`). For a StatefulSet, DaemonSet or Job the scan cannot read, the acknowledgement covers only the reasons the scan showed; if reading it with your token finds more (whether its component is in use, webhooks its pods serve), the fix refuses and gives the command to delete it yourself.
   2. **Fix** runs the delete and the wait.

The Diagnostics scan reads Deployments only (the ServiceAccount reads no other workload kind): for a StatefulSet, DaemonSet or Job, Fix reads it with your token and checks it before deleting.

**Objects the operator cannot update** (guidance only):

<img src="docs/images/diag-apply-failures.png" alt="Diagnostics: Deployment redhat-ods-applications/kuberay-operator must be recreated: spec.template.metadata.labels cannot be changed, with the commands" width="760">

MANUAL ADMIN STEPs, by error:

1. **`field is immutable`** on any other field (for example `spec.template.metadata.labels`), or together with another error. The object must be recreated: delete it, then restart the operator that reconciles the reporting CR so it reconciles at once. Module operators back off a failing object exponentially (up to about 16m40s) and may wait that long otherwise.
   ```bash
   oc delete deployment.apps <name> -n <ns>
   oc rollout restart deployment/<module-operator> -n <ns>        # a module operator (Diagnostics names it when known)
   oc delete pod -n redhat-ods-operator -l name=rhods-operator     # the DSC or Platform reported it (OLM owns that Deployment)
   ```
   Deleting a controller Deployment stops its pods until the operator recreates it with the current spec; that is safe when its operator recreates it.
2. **`Only one reference can have Controller set to true`** (ownership moved between versions, for example from the Platform to a module CR). Same steps: delete the object; its new owner recreates it.
3. **`failed to create typed live|patch object … expected <type>, got …`**: the live CRD and the object disagree on a field's type. It usually follows a downgrade or a round trip, when an older bundle replaced a same-named CRD (see "Downgrades" in §6). Diagnostics shows the fields, the CRD, and who last wrote its `spec.versions` (managedFields; `catalog` is OLM installing a bundle). Fix it by hand:
   1. re-apply that CRD from the **installed** operator version's bundle (`oc get crd <crd> -o yaml` shows the current schema);
   2. correct the listed fields of the object to the type the CRD expects (`oc edit <kind>.<group> <name>`), keeping the intended meaning;
   3. restart the operator as in step 1.
   There is no automatic fix: the right value needs judgement.

### 11.7 DataScienceCluster not ready

On **Components**, the DataScienceCluster card names the failing components:

<img src="docs/images/components-cause.png" alt="Components: DataScienceCluster default-dsc Not Ready; trainer Error with the cause Prerequisite operator not installed: JobSet Operator" width="760">

1. **Not Ready**, with the operator's summary.
2. The **Cause** of each failing component, and a link to Diagnostics, which shows the fix.

On **Diagnostics**, open the problem:

<img src="docs/images/diag-prerequisite-missing.png" alt="Diagnostics: Prerequisite operator Job Set Operator is not installed, with the commands that install it" width="760">

1. **Observed**: the conditions and catalog facts the tool read.
2. **Fix**: what to do, in plain words. The tool does not install operators.
3. **Command**: the exact commands (here: Namespace, OperatorGroup, Subscription, `oc wait`, then the operand).
4. Copy them, read them, and run them as cluster-admin.

The other causes in the table below look the same. Open each example to see it:

<details>
<summary><b>Installed, but its operand is missing</b></summary>

<img src="docs/images/diag-operand-missing.png" alt="Job Set Operator is installed, but its JobSetOperator/cluster does not exist, with the command that creates it" width="760">

</details>

<details>
<summary><b>Module operator in back-off</b> (the only one with a Fix button)</summary>

<img src="docs/images/diag-module-backoff.png" alt="The trainer module operator has not retried yet, with the restart command and a Fix button" width="760">

1. **Fix** restarts the module operator, after a confirmation. The **Command** does the same by hand.

</details>

<details>
<summary><b>Certificate not issued</b> (cert-manager stopped)</summary>

<img src="docs/images/diag-certificate-stale.png" alt="Certificate reports Ready, but its Secret is missing: cert-manager is installed but its controller is not running" width="760">

</details>

<details>
<summary><b>Upgrade gate</b></summary>

<img src="docs/images/diag-upgrade-gates.png" alt="The DataScienceCluster waits on an upgrade gate, with the oc patch command" width="760">

</details>

Diagnostics ("DataScienceCluster is not ready") lists each failing DSC condition, followed by its classified cause and a **Related** link to the problem that fixes it. Conditions with severity `Info` do not block `Ready`. A prerequisite named only by Info conditions (for example `KserveLLMInferenceServiceWideEPDependencies: LeaderWorkerSet not installed`) is reported as informational: it gates an optional feature. The Components page shows the same cause in a short line. Read the conditions by hand with:

```bash
oc get datasciencecluster -o jsonpath='{range .items[0].status.conditions[*]}{.type}={.status} [{.severity}] {.reason}: {.message}{"\n"}{end}'
```

| Cause (condition message) | What Diagnostics shows | Fix (MANUAL ADMIN STEP unless noted) |
|---|---|---|
| **Missing prerequisite operator** (`dependency not met: JobSet Operator is not installed`, `cert-manager operator not installed`, `LeaderWorkerSet not installed`) | `Prerequisite operator <name> is not installed`, one problem per operator, with the catalog package it resolved to (Red Hat catalog first; a community-only match is flagged). It also says whether the operator blocks Ready or is optional | Run the Command: a quoted here-document with the Namespace (the CSV's suggested namespace), the OperatorGroup (OwnNamespace when supported, else openshift-operators, which already has `global-operators`; an existing OperatorGroup is reused), and the Subscription (default channel, Automatic), then `oc wait` lines. When the CSV ships a singleton operand (`JobSetOperator/cluster`), a last command creates it if it is missing. The tool never installs operators |
| **Installed, but its operand is missing** (still "not installed" after the install, or `JobSetOperator CR with name 'cluster' not found. Please create the JobSetOperator CR`; Trainer also needs `JobSetOperator/cluster`) | `<operator> is installed, but its <Kind>/cluster does not exist` | Run the Command (`oc get <kind>/cluster \|\| oc create -f - <<'EOF' …`). Some operators create their own operand (cert-manager creates `CertManager/cluster`) |
| **Installed but not ready** (CSV not Succeeded) | `<operator> is installed but not ready (CSV phase …)` | Read the CSV status (`oc get csv <name> -n <ns> -o jsonpath='{.status.message}'`) |
| **Module operator in back-off**: the module still reports a prerequisite that is now installed (with its operand), or its module CR lags its generation (`Module status is stale (observedGeneration < generation)`) in Diagnostics scans at least 5 minutes apart; a new spec change restarts that wait. Module operators back off up to about 16m40s and do not watch their dependencies | `The <module> module operator has not retried yet` | Confirm-gated **Fix**: a rolling restart of the module operator Deployment (the `kubectl.kubernetes.io/restartedAt` pod-template annotation, as `oc rollout restart` does), with UID/resourceVersion preconditions, after re-checking the condition. It is offered only when a pod is available and the operand, if any, could be read. If the pods serve a `failurePolicy: Fail` webhook and the strategy would stop the old pod first (Recreate), it becomes guidance: run `oc rollout restart deployment/<name> -n <ns>` at a quiet moment |
| **Certificates never issued, or their Secret deleted** (pods stuck on `secret "<x>-cert" not found`; the `Certificate` has no `Ready`, or still says Ready=True because no cert-manager controller runs to update it) | `Certificate <ns>/<name> is not issued: cert-manager is not installed or not running` (or "reports Ready, but its Secret … is missing: cert-manager is installed but its controller is not running", with the operator Deployment (scaled to 0?) and `CertManager/cluster` managementState, or the Certificate's own failing condition). The stuck pods and the webhook whose CA it injects (`cert-manager.io/inject-ca-from`) are folded into it | Install cert-manager (Related: the prerequisite problem), or run the printed `oc scale deployment/<operator> --replicas=1` / `oc patch certmanager.operator.openshift.io cluster … Managed`. The pods start once the Secret exists |
| **Upgrade gate** (`ModulesReady=False (AdminAckRequired)`, `failed to resolve upgrade gate version`) | `The DataScienceCluster waits on an upgrade gate`, plus `platform-admin-ack-required` for the Platform | Read what each gate is about, then run the per-key `oc patch configmap odh-upgrade-acks -n redhat-ods-operator --type merge -p '{"data":{"<key>":"true"}}'` commands. "Unable to determine target release" means the operator is older than what it found: install that version or newer (§6) |
| **Upgrade leftovers** (`failure deploying resource …: field is immutable`, two controller owners, schema mismatch) | "Objects the operator cannot update" (§11.6) | §11.6 |
| **Left over from an earlier release** (`to smd typed: errors: <path>: expected <type>`, or a changed `spec.selector` alone) | "Left over from an earlier release" (§11.6), with a Fix button | §11.6 |
| **Dead CRD conversion webhook** (`conversion webhook …`) | `stale-crd-conversion` in "Stale webhooks" | §11.2 |
| **`opendatahub.io/managed: "false"`** on an operator-applied object (the object never updates) | `<name> is excluded from operator management` | §11.3 |
| **DSCInitialization in Error** | `DSCInitialization <name> is in phase Error`; linked from the DSC problem when other causes are not classified | Fix the cause its conditions name; the operator retries |
| Anything else | The condition with "cause not classified" and the operator's message as evidence | Fix what the message names, or set an unused component to Removed |

Reproduce the cases safely on a test cluster: delete `JobSetOperator/cluster` (Trainer reports JobSet missing; the operand problem appears; re-create it from the Command), then restart nothing and watch the back-off problem appear once the operand is back.

### 11.8 The operator cannot convert a CRD version

Diagnostics → "API versions" reports `The operator cannot convert <Kind> to <version>` (for example DataScienceCluster v3, Platform v1alpha2). Unlike §11.2 the webhook's Service is up: the operator answers, but its image does not know the version, so every read at that version fails, `oc get` included when it is the preferred one. Seen on a RHOAI 3.6 nightly (2026-10-07) whose operator image was older than the CRDs its bundle installed: `conversion webhook for datasciencecluster.opendatahub.io/v2, Kind=DataScienceCluster failed: no kind "DataScienceCluster" is registered for version "datasciencecluster.opendatahub.io/v3"`. A list without `limit` answers HTTP 429 `storage is (re)initializing` with `Retry-After`, and `oc` retries that forever, so pass `--request-timeout`.

What the tool does: it reads the next served version that works (here v2) and says so on the Components page; it never compares a DSC with defaults of another version, because v3 renamed components. When the bundle ships v3 defaults only, the Components page explains that instead of comparing.

Check (read-only):

```bash
oc get --raw '/apis/datasciencecluster.opendatahub.io/v3/datascienceclusters?limit=1' --request-timeout=20s   # fails
oc get --raw '/apis/datasciencecluster.opendatahub.io/v2/datascienceclusters?limit=1' --request-timeout=20s   # works
oc logs -n redhat-ods-operator deploy/rhods-operator | grep conversion-webhook
```

Fix: update RHOAI to a newer nightly (Update on the Status page, or a build from Build Explorer), then run Diagnostics again. Do not switch the CRD to conversion strategy `None` (§11.2 step 3): the stored objects would be read in the wrong shape.

### 11.9 Change or reset the DataScienceCluster

The Components page's **Components and sub-components** card ([README](README.md#components-and-sub-components-change-them-without-the-openshift-console)) changes the DSC. What it writes: [CLUSTER_CHANGES §5](docs/CLUSTER_CHANGES.md#5-datasciencecluster-status-and-components-pages).

**Read the comparison first.** The top of the card says what the DSC was compared with (the DataScienceCluster example in the installed CSV's `alm-examples`, with the operator version) and lists, for each kind of difference, the count and the paths or **None**: Missing, Not in the installed CRD, Deploys nothing, Changed from the default. **None** means it was checked; **Not checked** means it could not be: without a release default (the CSV ships no DataScienceCluster example in the version the DSC is read at, or its example sets fields the installed CRD does not define, which the message names) Missing and Changed are not checked, and without the CRD's schema neither are the other two. Each row carries the same labels; click one for what it means and what Fix does, and turn on **Show only differences** to see only those rows (with their parents).

<img src="docs/images/components-summary-ok.png" alt="Nothing to fix compared with the release default: Missing, Not in the installed CRD and Deploys nothing read None; Changed from the default lists aigateway and aigateway.modelsAsAService, whose rows are labelled Changed from the default" width="760">

With differences, the summary names each kind and offers **Fix**:

<img src="docs/images/components-differences.png" alt="6 differences from the release default to fix: 3 Missing, 2 Not in the installed CRD, 1 Deploys nothing, 1 Changed from the default, with Fix; Show only differences on, the rows that differ with their labels" width="760">

1. The comparison: each kind of difference with its count and paths, or **None**.
2. **Fix**: sets what is missing to the release default and removes the fields the CRD does not define.
3. **Show only differences**: only the rows that differ, with their parents.
4. A row's label (here **Deploys nothing — parent Removed**); click it for what it means.

**Deprecated fields.** The DataScienceCluster CRD marks only whole API versions as deprecated (`deprecated: true` on a served version), never single fields, so the page cannot list deprecated fields and does not guess. It does not need to: the API server prunes fields a CRD does not define, so a field the new CRD dropped cannot stay in the DSC, and components renamed between API versions (v2 to v3) are converted by the operator's conversion webhook. After an upgrade, what matters is what the new release default sets that the DSC lacks: those rows read **Missing**.

**Not in the installed CRD.** A field the installed CRD does not define (for example a component of an older API version) is a row under the component it is in, labelled **Not in the installed CRD**. The operator never sees it. **Fix** removes it.

**A component is "Not set" after an upgrade.** A release that adds a sub-component (`dashboard.maasPortal`, `workbenches.workbenchesV2`, `kserve.modelExpress`, `aigateway.aiGrid`, `databaseservice` in 3.6) does not add it to an existing DSC. The operator treats a value that is not set as the CRD's default for it where the CRD has one (`aipipelines.argoWorkflowsControllers` starts Managed; the tree shows "Not set (Managed)" and, as the release default, "Not set (CRD default Managed)"), else as Removed. When the release default is Removed too, nothing differs in practice; when it is Managed (for example `dashboard.standard` lost by a client that wrote the old `dashboard.managementState` shape against v3, which the API server prunes), the component is gone. Such a row reads **Missing — default Managed** (orange); one whose default is what the operator already does reads **Missing — default Removed** (grey: only worth setting explicitly). **Fix** sets each missing value to the release default.

**Managed under a parent that is not Managed.** A sub-component is deployed through its parent's module (`aigateway.modelsAsAService` needs `aigateway` Managed); its row reads **Deploys nothing — parent Removed**, and Fix does not change it. Set the parent to Managed (the confirmation offers both in one change) or set the sub-component to Removed.

**A change is refused.** Every change shows its exact list first:

<img src="docs/images/components-toggle.png" alt="Set ray to Removed? The confirmation lists ray: Managed to Removed (removes it)" width="640">

- "changed since you looked" or "changed since you reviewed this change": someone else changed the DSC (the JSON Patch's `test` failed, or the plan computed again differs from the one the dialog showed). Nothing was changed; reload.
- "The API server refused the change in its dry run: ...": a validation rule or the operator's webhook refused it, quoted as is (for example Kueue `Managed`, which the 3.6 webhook denies; use `Unmanaged`; or `kserve.modelCache` Managed without `cacheSize`, which needs settings this page does not edit: use the console).
- "X must not be removed now: MaaS is in use (...)": X is `aigateway` or `aigateway.modelsAsAService`, and removing it takes Models as a Service away (maas-api, maas-controller and the gateway policies it made), so every published model and every API key would stop working, and a MaaSModelRef left behind keeps maas-controller's finalizer. The message lists what uses MaaS: every MaaSModelRef (cluster-wide), the MaaSSubscriptions and MaaSAuthPolicies in `models-as-a-service`, and this tool's MaaS record (ConfigMap `rhoai-nightly-updater-snapshot`, key `maas`: a setup, a database or a publication it has not removed). Remove MaaS on the Environment page's MaaS tab: **Clean up MaaS** unpublishes the tool's models and removes MaaS in order (it refuses while others' MaaS objects exist and names them; they remove theirs first). Or keep `aigateway` Managed: for a reset, keep **Keep the components you turned on** checked. "cannot tell whether MaaS is in use" is the same refusal when a read failed (the record, the `maas.opendatahub.io` API discovery, or one of the lists): the preview reads with the updater's ServiceAccount (`oc auth can-i list maasmodelrefs.maas.opendatahub.io -A --as=system:serviceaccount:<ns>:<sa>` must say yes; re-apply the template otherwise), the change itself with your own token (`oc auth can-i list maasmodelrefs.maas.opendatahub.io -A`); fix the one that fails and try again. The same check runs for Diagnostics' disable fix and the version-matched repair (when it runs; its preview does not list it, as the gitops profile cannot read MaaS objects). Other parts of `aigateway` (`aiGrid`, `batchGateway`) have no in-use signal the tool can read, so they get only the module checks below.

  <img src="docs/images/components-toggle-maas-in-use.png" alt="Set aigateway to Removed? refused: MaaS is in use, with the published model, the subscription and the access policy; Set to Removed is disabled" width="640">

- "X must not be removed now" with other reasons: the removal checks of [§11.1](#111-d1-dashboard-operator-scaled-to-0-dashboard-cr-deletion-hangs) and [§11.4](#114-d7--d8-mlflow-or-pipeline-server-finalizers) (the RHOAI operator, found from the installed CSV's Deployments, and the module operator must run; the module CR must not be deleting; nothing must still use it). The dialog shows them before you confirm: the change runs the same checks. The dashboard, MLflow operator and pipelines are never removed by the tool. A module the tool cannot check (no kind named after it in `components.platform.opendatahub.io` or `services.platform.opendatahub.io`, or a module CR whose operator it cannot identify) is refused rather than removed unchecked: check its operator yourself and change it in the console.

  **How the module operator is identified.** Not from a list of names: it is the Deployment in the applications namespace (the DSCInitialization's), owned by the Platform (or the DataScienceCluster), whose ServiceAccount a ClusterRoleBinding lets update the module CR and its `<plural>/finalizers` (every module operator's ClusterRole grants that; `<plural>/status` when no ServiceAccount there holds it). A rule that names the kind beats one that only matches through a `*` verb or API group; `*` resources (cluster-admin) and bindings to groups of ServiceAccounts identify nothing. A kind no ServiceAccount there may update is the RHOAI operator's own when its installed CSV owns the CRD (Kueue). No such Deployment, several equally specific ones, or a read error: "can't confirm its operator is running" (or "could not identify its operator (...)") and the change is refused. To see what the tool sees for a module (here `trustyai`):

  ```bash
  NS=$(oc get dscinitializations -o jsonpath='{.items[0].spec.applicationsNamespace}')
  oc get clusterrolebindings -o json | jq -r --arg ns "$NS" '.items[] | select(any(.subjects[]?; .kind=="ServiceAccount" and .namespace==$ns)) | "\(.roleRef.name) \([.subjects[] | select(.namespace==$ns) | .name] | join(","))"' |
    while read -r role sas; do oc get clusterrole "$role" -o json | jq -e '.rules[] | select(.resources[]? == "trustyais/finalizers")' >/dev/null && echo "$role -> $sas"; done
  oc get deployments -n "$NS" -o custom-columns=NAME:.metadata.name,SA:.spec.template.spec.serviceAccountName,OWNER:.metadata.ownerReferences[0].kind
  ```
- "X serves the conversion webhook of CRD ...": removing X deletes the webhook's Service but not the CRDs, so their objects could no longer be converted, garbage-collected or deleted with their namespace. The check lists each such CRD's objects in every namespace with your own token (the ServiceAccount may not read them), at the CRD's storage version, one page of 20. When none exist it does not block (the other checks still apply). When some do, the message says how many ("4 objects", or "20 or more" when the API server has more, with its estimate when it gives one), names up to five as `namespace/name`, and refuses until none remain; delete them first, for example:

  ```bash
  oc get mcpservers.mcp.x-k8s.io -A                            # all of them
  oc delete mcpservers.mcp.x-k8s.io -n <namespace> <name>
  ```

  "The objects of ... could not be listed with your sign-in: ..." refuses too: the list failed (your account may not list that kind, the conversion webhook failed for an object stored at an older version, a timeout, an answer that is not a list: no `items`, or a kind that does not end in `List`), so the tool cannot tell whether any exist; the message quotes the error. Check with `oc get <crd> -A` and try again. The repair preview at the top of the card (Reset to defaults, Remove extra components) runs the same check with your sign-in, so it names the same objects; once none remain it no longer blocks a repair that removes the component. "... which this request does not carry" means the check had no sign-in of yours: in the gitops profile always (it reads only with the ServiceAccount, and has no repair or component changes; change the DSC in Git), in the full profile only if the request reached the app without your token (reload the page, or log out and in again).
- Argo CD, an ACM policy or Helm (`meta.helm.sh/release-name` annotation, `app.kubernetes.io/managed-by: Helm` label) manages the DSC: change it in Git, in the policy or in the chart's values.

**Keep the components you turned on.** Both resets offer it, checked by default, and name the components: every toggle that is Managed (or Unmanaged, where the CRD allows it) in the DSC and deployed, while the release default leaves it Removed or unset, with the parents it needs (for example `aigateway` and `aigateway.modelsAsAService` after MaaS setup, or `kueue` Unmanaged). Checked, the reset keeps their value and resets everything else; the soft reset lists them as "Kept: ... (you turned these on)", the hard reset creates them in the new DSC. Unchecked, the reset is the pure release default, and turning them off gets the removal checks (the MaaS guard above). Changing it plans the reset again; the option is part of what you review (`keepEnabled` in the plan and the request; the API requires it for both resets). A soft reset that would keep everything and change nothing else says so ("Nothing to change").

<img src="docs/images/components-soft-reset.png" alt="Soft reset with Keep the components you turned on (2): aigateway, aigateway.modelsAsAService checked; it sets ray to Managed and workbenchesV2 to Removed, and lists Kept: aigateway, aigateway.modelsAsAService" width="640">

Unchecked while a model is published, the soft reset is refused before you confirm:

<img src="docs/images/components-soft-reset-maas-in-use.png" alt="The same soft reset unchecked: it would remove aigateway, so This change is not possible now comes first, names the published model dev-project/qwen25-05b-instruct and says to remove MaaS on the MaaS tab or keep aigateway Managed; What a soft reset would do is collapsed; Reset is disabled" width="640">

**Hard reset.** Use it when components are stuck and a soft reset does not help.

<img src="docs/images/components-hard-reset.png" alt="The hard reset dialog with Keep the components you turned on checked: What a hard reset would do is open; it deletes default-dsc and tears down every enabled component (12), which come back as the release default, and stops without creating anything if the old DataScienceCluster is not gone within 10 minutes; aigateway and modelsAsAService are kept; collapsed sections list what stops working (5), the CRDs that cannot be read meanwhile (2), the changes in the new DataScienceCluster (2) and the object it creates; the DSC name typed to confirm" width="640">

It runs as one operation with steps: check, delete, wait until the DSC is gone and the platform has cleaned up its modules, create the release default with the old name and the old workbench and AI hub namespaces, wait for Ready. The dialog lists how the new DSC differs from the current one, both as the API server stores them: a field the installed CRD gives a default (for example `kserve.rawDeploymentServiceConfig: Headless` or the `kueue` queue names) comes back by itself in the new DSC, so it is no change, and the soft reset does not remove it either. When the reset is not possible, the reasons come first, one line per component, and what it would do is collapsed; otherwise it is open, with a short summary (what is deleted, how many components are torn down, how long it waits for the old DSC) and collapsed sections with counts. Before anything is deleted, the check step runs a dry run of the delete and a dry run of the new spec against the live DSC (`fieldValidation=Strict`, so the CRD's rules and the operator's webhook see it), and records the object it will create in ConfigMap `rhoai-nightly-updater-operation` (key `dscHardReset`). The dry run of the new spec is an update: validating-webhook rules that check only creates (the operator's `denyMultipleDsc`, one DSC per cluster) do not see it, and run only at the create after the delete; if one refuses then, the reset stops with nothing created and **Finish the hard reset...** applies (below).

What the delete sets off on RHOAI 3.6 (read from rhods-operator rhoai-3.6 and odh-platform-utilities framework v0.3.1): the DSC finalizer `platform.opendatahub.io/finalizer` only marks the DSC's modules `Removed` on the Platform `default` and is removed in the same reconcile (`datasciencecluster_controller_actions.go:97-113`, framework `reconciler.go:362-374`), so the DSC is gone within a reconcile. The module CRs and the Kueue CR it owned are then garbage-collected; each keeps its finalizer until its module operator has cleaned up. The Platform controller keeps a module's operator while its CR is deleting and deletes the operator's resources once the CR is gone (phase 2, `modules_controller_actions.go:255-341`). The delete has the UID precondition and `propagationPolicy: Background`: with `Foreground` the garbage collector would also delete, in the foreground, the workloads a module CR owns while its operator may still need them for its finalizer (kubernetes `garbagecollector.go:591-623`). So after the delete the reset waits, up to 10 minutes in all, until (a) the DSC is gone, (b) no object of a module kind (both module API groups) has an owner reference to the old DSC, (c) the module operator Deployments of those CRs, recorded before the delete, are deleted or replaced (phase 2), and (d) the Platform `default` reports, for the generation it is at (`ProvisioningSucceeded` `observedGeneration`), `ModulesReady` without a module pending deletion. Before the create, the DSCs are listed: if one exists by then, nothing is created. Before the delete it is refused when a module CR the DSC owns has an operator the tool cannot identify, or one the DSC itself owns (it would be garbage-collected with the DSC), and when the garbage collector would delete a namespace, with everything in it, together with the DSC. That is decided as the collector decides (kubernetes v1.34 `garbagecollector.go` `attemptToDeleteItem` and `classifyReferences`): it deletes an object with owner references only when none of its owners is "solid", that is, exists with the referenced UID and is not being deleted in the foreground (`deletionTimestamp` with finalizer `foregroundDeletion`). So a namespace is refused when no owner of it is solid and stays so (is not being deleted at all, as a deletion ends in the owner's removal or, with orphaning, in the collector removing its reference, and does not lead, directly or through its own owners, to an `opendatahub.io` object: the DSC, a module CR, the Platform, ...), and one of its owners leads to such an object or cannot be read. One such owner that leads elsewhere is enough to keep it; a namespace whose owners are all gone or being deleted, none of them RHOAI's, is the collector's already, reset or not. The tool reads the namespaces and their owners, which can be of any kind (on an ACM spoke the `open-cluster-management-*` namespaces and the cluster's own namespace are owned by ACM's AppliedManifestWorks), with your own sign-in, in the plan and in the reset; the ServiceAccount is not allowed to read them. The message names each namespace with its owner chain and suggests the soft reset or Fix: do not remove owner references other controllers set (removing ACM's would break ACM). For an owner it cannot read it says which, with your sign-in, and gives the command that shows that owner's own owners, for example `oc get appliedmanifestwork.work.open-cluster-management.io <name> -o jsonpath='{.metadata.ownerReferences}'`. List the namespaces that have owners with `oc get namespaces -o json | jq -r '.items[] | select(.metadata.ownerReferences) | "\(.metadata.name) \(.metadata.ownerReferences)"'`. Resources that need a component (pipeline servers, models, playground servers, ...) are kept, but stop working until it is back: the dialog lists those it found under **Stops working until its component is back**, and under **CRDs that cannot be read at another version meanwhile** the CRDs whose conversion webhook a torn-down component serves. Components you turned on that the release default leaves Removed come back as they are with **Keep the components you turned on** (below); unchecked, they stay Removed and get the same removal checks as setting them to Removed (a conversion webhook, something that still uses them, MaaS in use). Kept components are torn down and come back too: while `aigateway` is down, MaaS does not serve (the dialog lists the published models and the MaaS API keys among what stops working). It is refused on Managed RHOAI (the operator recreates `default-dsc` itself), when the operator is not running, when the installed CSV ships no DataScienceCluster example, when the DSC is already being deleted, when there is not exactly one DSC, under Argo CD, ACM or Helm, and while a module cannot be cleaned up (for example an MLflow CR exists).

If it stops after the delete, every message prints the object it planned, and nothing else is created. Once no DataScienceCluster exists, the Components page shows **A hard reset did not finish** with **Finish the hard reset…**: it creates exactly the recorded object (old name and namespaces), after checking that no DSC exists, that the CRD still stores the recorded version, that the installed operator is still the one whose release default it is, that the platform is self-managed and the operator runs, and a dry run. Use it rather than **Create DataScienceCluster**, which creates the release default named `default-dsc` without the old namespaces. This works after a restart of the tool too.

- **"still being deleted after 10m0s"**: the DSC still has `platform.opendatahub.io/finalizer`. The operator removes it in the reconcile that marks the modules `Removed`, so the RHOAI operator has not processed the deletion: check that it runs and read its logs. Do **not** remove the finalizer by hand: the modules would not be marked `Removed`.
- **"is gone, but after 10m0s these module CRs it owned are still there"**: each named CR waits for its module operator to remove its finalizer; check Diagnostics → Platform modules for the module that is stuck and fix it (§11.1, §11.4). Do **not** remove the finalizers by hand: that skips the module's cleanup.
- **"the platform has not removed these module operators yet"** or **"the Platform does not report its modules removed yet"**: phase 2 has not run, or the Platform's last reconcile failed; read `oc get platforms.config.opendatahub.io default -o yaml` (`ModulesReady`, `ProvisioningSucceeded`) and the RHOAI operator's logs.
- In each case **Finish the hard reset…** checks the same (no module CR owned by the old DSC, the recorded operators gone, the Platform settled) and refuses, saying what is still there, until it is done; once it is, use it.
- **"could not be read for the last poll"**: reads of the DSC kept failing until the deadline (they are retried while waiting). Check it with `oc get datasciencecluster`, then finish as above.
- **"The reset was stopped"**: the tool lost its lock or was stopping while the DSC was deleted. Finish as above once it is gone.
- **"creating the release default failed"** or **"refused the release default in its dry run"**: the old DSC is gone. Fix what the message quotes, then use **Finish the hard reset…**, or create the printed object yourself:

  ```bash
  oc get datasciencecluster                 # expect: No resources found
  oc create -f dsc.yaml                     # the object from the message
  ```

- **"A new DataScienceCluster ... appeared"** or **"DataScienceCluster X now exists"**: someone, or the operator, created one meanwhile; the reset created nothing and Finish is not offered while it exists. Review it on the Components page.

## 12. Activity log

ConfigMap `rhoai-nightly-updater-activity` holds the audit log (`data.entries`, JSON). If it is corrupted, the app logs a parse warning and starts a fresh list on the next write. To reset it (MANUAL ADMIN STEP): `oc delete cm rhoai-nightly-updater-activity -n $NS`.

## 13. Clusters managed by Argo CD

On a cluster where Argo CD (OpenShift GitOps) installs RHOAI from Git, for example with the [odh-gitops](https://github.com/opendatahub-io/odh-gitops) chart, Argo CD keeps the CatalogSource, the Subscription and the DataScienceCluster as Git describes them. With `selfHeal`, it [syncs as soon as the cluster drifts from Git](https://argo-cd.readthedocs.io/en/stable/user-guide/auto_sync/), and it recreates what is deleted. Git is the source of truth, and the tool works with that: it asks Argo CD which Application manages the object (below), shows a notice on the Status page, and only does what Argo CD keeps. **No change to the Argo CD Application is needed**: the tool keeps every field Git sets.

**How the tool knows Argo CD manages an object (4.0.0).** Argo CD marks what it syncs by its [resource tracking](https://argo-cd.readthedocs.io/en/stable/user-guide/resource_tracking/) method: the annotation `argocd.argoproj.io/tracking-id` (`annotation`, `annotation+label`), or only the label `app.kubernetes.io/instance` (`label`). OpenShift GitOps defaults to `label` up to 1.18 and to `annotation` from 1.19 ([gitops-operator docs/upgrading.md](https://github.com/redhat-developer/gitops-operator/blob/master/docs/upgrading.md)), so objects synced by OpenShift GitOps 1.12 to 1.18 carry no tracking annotation; up to 3.x the tool looked only for the annotation and treated them as not managed. Neither mark proves management anyway: both outlive the application that wrote them (an Application deleted without cascade, OpenShift GitOps uninstalled, a cluster set up from Git once and handed over, objects copied from another cluster), Helm sets the label too, and an object in Git is Argo CD's before Argo CD marks it. So on every status read and before every refused action the tool lists Argo CD's Applications and looks for the object (group, kind, namespace, name; a cluster-scoped object has no namespace there) in each one's `status.resources`, Argo CD's own list of what the application syncs or would prune. The marks only name the application in messages and pick which Applications' state counts:

| What the tool finds | Verdict | What you see |
|---|---|---|
| An Application lists the object in `status.resources`, with or without a mark on the object | managed | The notice, naming the Application and its namespace; the actions in the table below are off |
| No Application lists it, and the object carries no mark | not managed | Nothing |
| Argo CD's Application API is not served on this cluster, and the object carries the tracking annotation | left over | A small note: "... carry Argo CD tracking from application X, but Argo CD is not installed on this cluster; the tool treats them as not managed". Every action is on |
| No Application lists it and none has the name the annotation gives | left over | A note: "... left over from application X, which no longer exists" |
| No Application lists it; the Application the annotation or label names exists and Argo CD compared it with Git | left over | A note: "... from application X (namespace N), which no longer lists them" |
| Only the instance label, and no Application of that name (or no Argo CD API here) | not managed | Nothing: the label need not be Argo CD's |
| A read error or a 403; the Application the mark names has not been compared yet (sync status `Unknown`) or reports `ComparisonError`; or, for a marked object no Application here lists, signs that an Argo CD on **another cluster** may manage it (below) | not confirmed | The notice as for managed, plus "Couldn't confirm with Argo CD ... (the reason), so the tool treats them as managed"; refusals end with the same reason. Without any mark the application is named `(unknown)` |

One exception follows Argo CD: an object whose tracking annotation names *another* object (copied from it; Argo CD compares namespace, name, group and kind, `isSelfReferencedObj` in `controller/state.go`) is listed without a sync status, and Argo CD neither syncs nor prunes it, so the tool does not count that entry.

**How current the notice is.** The pages reuse Argo CD's list of Applications for up to 30 seconds, so a notice (or its absence) can trail a change in Argo CD by that long. Every action reads the list again before it changes anything, so a page that has not caught up never lets an action fight Argo CD: the action is refused with the current reason.

**Signs of an Argo CD on another cluster.** Argo CD reaches another cluster with credentials made there; the tool looks for the marks two ways of making them leave:

- This cluster serves ACM's AppliedManifestWork API: it is (or was) an ACM managed cluster, and a hub's GitOpsCluster can add it to the hub's Argo CD.
- `argocd cluster add` (without the flags below) creates ServiceAccount `kube-system/argocd-manager` and binds it ([`InstallClusterManagerRBAC`, util/clusterauth/clusterauth.go](https://github.com/argoproj/argo-cd/blob/v2.10.18/util/clusterauth/clusterauth.go)): ClusterRoleBinding `argocd-manager-role-binding`, or, with `--namespace`, a RoleBinding of that name in each namespace given. The tool reads the ClusterRoleBinding and the RoleBinding in `redhat-ods-operator`, `openshift-marketplace`, RHOAI's applications namespace (from the DSCInitialization) and the object's own namespace.

The tool never treats an object as unmanaged on doubt: it must not fight an Argo CD. **What leaves no sign** the tool can read, so that with no Argo CD API here, or no Application here listing the object, the tool treats it as not managed:

- `argocd cluster add --service-account <sa>`: Argo CD uses an existing ServiceAccount's token and creates no binding ([cmd/argocd/commands/cluster.go](https://github.com/argoproj/argo-cd/blob/v2.10.18/cmd/argocd/commands/cluster.go) `NewClusterAddCommand`);
- `argocd cluster add --aws-cluster-name` or `--exec-command`: Argo CD authenticates with AWS or an exec plugin and installs nothing on the cluster (same function);
- a cluster Secret made by hand (any token in an Argo CD cluster Secret on the other cluster);
- `argocd cluster add --namespace` for namespaces other than the four above;
- label tracking on a remote Argo CD with `application.instanceLabelKey` set to another key: the object carries neither the tracking annotation nor `app.kubernetes.io/instance`, and the remote signs are only consulted for marked objects.

In the other direction, a sign can outlive its Argo CD: a cluster detached from ACM keeps the AppliedManifestWork CRD (the klusterlet no longer removes CRDs that several klusterlets may share: open-cluster-management-io/ocm `klusterlet_crd_reconcile.go`), and `argocd cluster rm` removes ClusterRoleBinding `argocd-manager-role-binding` but not the RoleBindings `--namespace` made (`UninstallClusterManagerRBAC` deletes only the cluster-scoped binding, its ClusterRole and the ServiceAccount). The tool then says it could not confirm. If the mark is left over in such a case, remove it; an Argo CD that does manage the object puts it back on its next sync:

```bash
oc annotate subscription rhods-operator -n redhat-ods-operator argocd.argoproj.io/tracking-id-
oc annotate catalogsource rhoai-catalog-dev -n openshift-marketplace argocd.argoproj.io/tracking-id-
oc annotate datasciencecluster default-dsc argocd.argoproj.io/tracking-id-
# label tracking: check first that the label is Argo CD's (Helm sets it too)
oc label subscription rhods-operator -n redhat-ods-operator app.kubernetes.io/instance-
```

**Git picks the build; the tool installs it.** The catalog image in Git decides which nightly the cluster runs. OLM installs a new catalog's head CSV by itself only when that CSV *replaces* the installed one ([update graph](https://olm.operatorframework.io/docs/concepts/olm-architecture/operator-catalog/creating-an-update-graph/)). Nightly builds of the same version share the CSV name (for example `rhods-operator.3.6.0-ea.1`) with no `replaces` or `skipRange` between them, so after Git moves the catalog to a newer build of the same version the operator stays on the old one. The Status page then shows **Install pending**, the build Git set (*In Git*) and the installed CSV; *Install the build the catalog serves* removes the installed CSV and recreates the Subscription, so OLM installs the catalog's head.

| Action | With Argo CD |
|---|---|
| Update | Installs the build the catalog serves: the catalog stays as Git sets it, the channel the Subscription has in Git is kept, and the Subscription is recreated with Argo CD's tracking ([what it changes](docs/CLUSTER_CHANGES.md#update-when-argo-cd-manages-the-operator)). Any other image is refused: change the image in Git. The catalog's image must have the Subscription's channel |
| A newer nightly in Quay | The Status page shows it with the image reference to put in Git; nothing happens on the cluster until Git changes. The gitops profile does not look it up (it contacts no registry): its *Latest* row says that a newer nightly arrives as a change in Git ([GITOPS.md §11](docs/GITOPS.md#no-registry-access)) |
| Re-deploy, Install a different build | Off: they delete the Subscription for minutes. Update installs the catalog's build again instead |
| DSC repairs, Diagnostics *disable component* | Off when Argo CD manages the DSC: change the DSC in Git |
| Dashboard Dev, the Environment setups, other Diagnostics fixes | Unchanged: they touch objects Argo CD does not manage |

The tool copies the annotations (Argo CD's tracking and `kubectl.kubernetes.io/last-applied-configuration` among them) and the labels onto the recreated Subscription. Argo CD may notice the recreation and run one automated self-heal sync of the Subscription; it finds it unchanged.

**"Argo CD application ... sets the image of CatalogSource ... in Git".** Update was asked for another image (an old page, a script). Only the build the catalog serves can be installed; change the image in Git, wait for Argo CD to sync, then run Update.

**"The CatalogSource image was changed to ... while Update waited".** Argo CD synced a new image from Git between the start of Update and the catalog serving the build. Update stopped before it touched the operator. Run it again once the catalog serves the new build (the Status page shows *Install pending*).

**First install.** When Argo CD has just created the catalog and the Subscription, OLM installs the head by itself; the Status page shows *OLM installing* and the install button waits (Update refuses while the Subscription's InstallPlan is not Complete or Failed). Once OLM is done, or if its InstallPlan failed, *Install the build the catalog serves* recreates the Subscription. *Create the DataScienceCluster* is off: the Application renders it from Git on its next sync.

**Failed install.** If the build the catalog serves does not come up, the restore puts the previous Subscription back (with Argo CD's tracking, and `startingCSV` pinned to the CSV Update removed) and OLM installs the catalog's build again if it has that CSV, which is the one that failed: change the catalog image in Git to go back. If the catalog no longer has that CSV (Git moved to another version), the Subscription reports `ResolutionFailed` and nothing is installed; run Update, or change the catalog image in Git.

**"no operators found with name ..."** (`resolution_failed`) right after Git changed the catalog image: the catalog's PackageManifest still listed the previous head when Update checked it, and the pin on the new Subscription kept OLM from installing another version (for example an older one after a rollback in Git). Wait for the Status page to show what the catalog serves now, then run Update again. If the tool dies between deleting and recreating the Subscription, Argo CD's self-heal recreates it from Git within seconds and OLM installs the head, so nothing is lost.

**"The Subscription changed while Update ran ..."** (`conflict`): Argo CD synced a new source, channel or `startingCSV` pin from Git after the old CSV was deleted. The Subscription is recreated with Git's current settings and OLM installs that channel's build; check the Status page, then run Update if it shows *Install pending*.

**"OLM installed ..., but it is not the build that was verified"** (`install_mismatch`): the catalog switched to another build (a Git change) between the check and the install. The running operator is kept; run Update again to install what the catalog serves now.

**"The catalog did not serve ... within ..."** after a change in Git: OLM replaces the catalog pod when the image changes; check the pod in `openshift-marketplace` (`oc get pods -n openshift-marketplace -l olm.catalogSource=rhoai-catalog-dev`) and [§6](#6-update--install-a-different-build-failed) (`catalog_image_pull`) for pull problems.

**Checking what Argo CD tracks.** Which Application renders the catalog and the Subscription, and its sync policy (the same `status.resources` the tool reads):

```bash
oc get applications.argoproj.io -A -o json | jq -r '.items[] | select(any(.status.resources[]?; .kind=="CatalogSource" and .name=="rhoai-catalog-dev")) | "\(.metadata.namespace)/\(.metadata.name): syncPolicy=\(.spec.syncPolicy // {} | tostring)"'
```

**A catalog an ACM policy enforces.** On some clusters an ACM ConfigurationPolicy (`remediationAction: enforce`), not Argo CD, applies CatalogSource `rhoai-catalog-dev`; a hub Argo CD application may apply the Policy, but the catalog on the cluster has no Argo CD annotation. The tool recognises it by its `managedFields`: manager `config-policy-controller` owns `f:spec`. It then treats the catalog as above: the image stays as the policy sets it, Update installs the build the catalog serves, Re-deploy and Install a different build are off, and messages name `an ACM policy (config-policy-controller)` instead of an Argo CD application. To move to another build, change the image in the policy. The Subscription can still be Argo CD's; messages about it name its application. Check the managers with:

```bash
oc get catalogsource rhoai-catalog-dev -n openshift-marketplace --show-managed-fields -o json | jq -r '.metadata.managedFields[] | "\(.manager) \(.operation) \(.fieldsV1 | keys)"'
```

The entry stays after the policy is removed (with `pruneObjectBehavior: None`), so the tool still treats the catalog as managed until another manager takes over its spec.

**Installing the updater next to Argo CD.** Install it with the release's `install.sh` as usual. Its objects carry no tracking annotation, so Argo CD [never prunes them](https://argo-cd.readthedocs.io/en/stable/user-guide/resource_tracking/).

## Reference: objects of the updater itself

| Object | Name |
|---|---|
| Deployment / Service / Route / NetworkPolicy | `$APP` in `$NS` |
| Cookie secret | Secret `$APP-proxy` (`session_secret`) |
| OAuth client (4.0.0) | OAuthClient `$APP-$NS` (cluster-scoped), its secret in Secret `$APP-oauth-client` (`client-secret`) |
| Serving cert | Secret `$APP-tls` (service-ca) |
| ClusterRole / ClusterRoleBinding / ConsoleLink | `$APP-$NS` |
| Roles | `$APP` in `$NS`; `$APP-$NS` in `kube-system`, `openshift-marketplace`, `openshift-ingress`, `openshift-ingress-operator` |
| State ConfigMaps | `rhoai-nightly-updater-{activity,snapshot,operation}` (`operation` holds the running-operation marker, `lastCompleted` and the cross-pod lease `lock`) |
