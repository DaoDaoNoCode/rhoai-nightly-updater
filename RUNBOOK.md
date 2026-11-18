# RHOAI Nightly Updater: Runbook

Troubleshooting and manual recovery. Commands assume the defaults `NS=rhoai-nightly-updater` and `APP=rhoai-nightly-updater`:

```bash
NS=rhoai-nightly-updater; APP=rhoai-nightly-updater
```

Steps marked **MANUAL ADMIN STEP** change the cluster outside the tool. They need cluster-admin. Run them only after the read-only checks before them confirm the situation, and only on a cluster you own or are allowed to change.

What the tool itself changes: [docs/CLUSTER_CHANGES.md](docs/CLUSTER_CHANGES.md). How it protects the cluster: [README](README.md#how-the-tool-keeps-your-cluster-safe).

**Find your symptom.** Each section starts with what the app shows, then the fix.

| What you see in the app | Section |
|---|---|
| "vX.Y.Z is available", "this updater's deployment is out of date" | [§2](#2-upgrade-roll-back-or-remove-the-updater) |
| "... is running ... on updater pod ...", "... was interrupted", 409 `cluster_busy` | [§5](#5-operations-busy-stuck-interrupted) |
| **Operator failed**, **No Subscription**, a failed Update or Install a different build step | [§6](#6-update--install-a-different-build-failed) |
| A PR shows "Does not contain" in Build Explorer | [§8](#8-build-explorer-quay-and-github) |
| Storage **Needs attention** (Migrate storage or Repair), **Error**, Delete disabled | [§10](#10-environment-page) |
| DataScienceCluster **Not Ready**, a Diagnostics problem | [§11.7](#117-datasciencecluster-not-ready), [§11.6](#116-upgrade-leftovers-the-operator-cannot-fix-itself) |
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
| 403 from oauth-proxy right after login | The user can't `list pods` in `redhat-ods-operator` (the login gate). On a fresh cluster only cluster-admins pass |
| UI is read-only ("Read-only access") | Changes need cluster-admin. Check with `oc auth can-i '*' '*' --all-namespaces`. On ROSA: `rosa grant user cluster-admin --user=<u> --cluster=<c>` |
| "Sign in again" banner | The oauth-proxy session (23 h) or the OAuth token expired. Follow the link |
| 401 "Bearer tokens are not accepted" | Something called the API with a bearer token. Use the Route in a browser |
| 503 "Cannot verify ..." | The API server didn't answer the identity/SAR check. Nothing was changed; retry |
| 429 | One accepted request per user and endpoint per 30 s. Dry runs and rejected requests don't count |

## 5. Operations: busy, stuck, interrupted

An operation that another updater pod runs (here, after the pod was evicted mid-update):

<img src="docs/images/banner-remote-operation.png" alt="Banner: qa-user is running Update to nightly on updater pod rhoai-nightly-updater-..., step 8 of 8, with the latest progress" width="760">

An operation whose pod stopped before it finished:

<img src="docs/images/banner-interrupted.png" alt="Banner: Update to nightly was interrupted, with who started it, the pod that stopped and what to do" width="760">

- **409 `cluster_busy` / "Another operation is changing this cluster":** one operation at a time. The banner names who started it and the current step. Wait, or follow it on the Status page.
- **Lost the progress view:** reload. The page reattaches to a running operation. If the connection was lost, the final card may say "outcome unknown, see the activity log".
- **"… on updater pod *P*" (a remote operation):** another updater pod runs it. This happens when the pod is deleted or evicted (node drain, cluster upgrade, autoscaler): the ReplicaSet starts a replacement at once while the old pod still drains its operation (up to 980 s). The replacement reads the old pod's lease, shows the operation with its latest step, refuses new changes with 409 `cluster_busy`, and shows the result (`lastCompleted`) when it ends. There is no live step stream for it; the page polls.
- **The lease:** every operation records a lease (key `lock` in ConfigMap `rhoai-nightly-updater-operation`) and renews its heartbeat every 10 s, also while the pod drains at shutdown. A lease whose heartbeat is older than **45 s** is free: that is how long a crashed pod's operation blocks a new one. A lease of an earlier container of the same pod (kubelet restart) is free at once. The lock fails closed: a lease of another pod refuses with 409 `cluster_busy`, and a lease that cannot be read or written (API errors, or write conflicts through every retry) refuses with **503 `lock_unavailable`** ("Cannot verify that no other updater pod is running an operation"). Nothing was changed; retry after a few seconds (`Retry-After`). If it persists, check that the updater can reach the API and patch ConfigMap `rhoai-nightly-updater-operation`.
- **"Stopped: another updater pod took over" (`lock_lost`):** while an operation runs, a renewal found another pod's lease, or renewals failed for 35 s (the TTL minus one heartbeat) so the lease may have expired. The operation stops at once and makes no further change, **not even the automatic restore**, because the other pod may be changing the same Subscription and catalog. Check the cluster state, wait for the other operation, then re-run.
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
  Note over A: If A's renewals fail for 35 s or find another pod's lease,<br>A stops at once (lock_lost) and does not restore.
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
| "Replacement catalog validation failed", `catalog_image_pull` | Pull secret or IDMS problem, or a bad image. Nothing was changed. Test the pull secret on the Status page |
| "older than the installed" | Update never downgrades. Use Install a different build and tick the confirmation (see "Downgrades" below) |
| Succeeded, but "an upgrade gate holds it" | The operator waits for an admin acknowledgement (set its key to "true" in ConfigMap `odh-upgrade-acks` in `redhat-ods-operator`) or cannot resolve a gate because it is older than the resources it found. The note reads the Platform and every DataScienceCluster condition |
| Diagnostics: "Objects the operator cannot update" | Upgrade leftovers; see §11.6 |
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

- **Banner "Dashboard Dev session active":** `dashboard-operator` is paused (0 replicas), so RHOAI updates don't reach the dashboard. Use **Revert** on Dashboard Dev when done. Operator operations offer "Revert Dashboard Dev and continue".
- **"A dashboard deletion is waiting for the paused dashboard-operator":** this is deadlock D1. Revert at once (§11.1).
- **Stuck rollout:** the panel reports the Deployment's `ProgressDeadlineExceeded`. Revert, or deploy again.
- **"Distance from main unknown" on a container:** the page could not ask GitHub how far the build's commit is from upstream `main` (or from the PR's latest commit). Usually the rate limit: GitHub allows 60 anonymous requests per hour per egress IP, shared by every user of the pod; the tooltip names the retry time. Set `GITHUB_TOKEN` on the updater Deployment ([README: Configuration](README.md#configuration)) to raise it to 5000/hour. Other reasons in the tooltip: GitHub does not know the commit (a force-pushed PR), the PR was not found, or GitHub answered an error (retried on the next refresh). The commit itself comes from the image labels and is shown either way.
- **"A newer ... build exists" or "Newer build available" under a container:** the tag the build was deployed from (`odh-stable`, `main`, `odh-pr-N`, `pr-N`) now points to another image. Nothing changed on the cluster: the container keeps its digest-pinned image. **Deploy latest main** or **Deploy PR** again to switch; main builds are then written under their commit tag ([CLUSTER_CHANGES §7](docs/CLUSTER_CHANGES.md#7-dashboard-dev)).
- **Revert fails, or the UI is down:** §11.1.

## 10. Environment page

The S3 storage is SeaweedFS in namespace `seaweedfs` (Deployment `seaweedfs`, PVC `seaweedfs-data`, Service `seaweedfs-s3`, Route `seaweedfs-admin`). Storage an earlier version set up in namespace `minio` is moved with **Migrate storage** (§10.1). Objects and ports: [CLUSTER_CHANGES §8](docs/CLUSTER_CHANGES.md#8-environment-page).

The page has an **Overview** (each feature's state and next step) and one tab per feature: **Pipelines** (S3 storage and a project's pipeline server), **MLflow** and **Models** (the inference gateway and a project's CPU model, §10.2). The tab and the project are in the address. Each part shows its health, its actions and its details inline (endpoint, login, the reason for an error or a repair, the pod's reason under **Technical details**); its menu (⋮) has **Delete** and **Repair**. **Clean up** (each tab, and **Clean up everything** on the Overview) deletes several parts as one operation (`POST /api/resources/cleanup/stream`): the pipeline servers, then the storage, then MLflow, then the models, then the inference gateway, stopping at the first part that fails and saying what was and was not deleted. Health and ownership are separate: something this tool did not create can be **Ready**; it only gets no Delete or Repair. Every change shows its steps and outcome in one dialog; closed early, a notification brings it back.

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
5. **Open pipelines**. In the storage's menu, **Delete** stays disabled while a pipeline server uses it, and says which.

| Problem | Fix |
|---|---|
| S3 storage setup: "not created by this tool" | Namespace `seaweedfs` holds foreign objects (or was not created by this tool). Nothing was changed. Remove them or use another cluster |
| Setup or teardown: "not allowed ... run `make upgrade`" | The image is newer than the template (RBAC for the SeaweedFS names is missing; the banner says the template is outdated). An admin runs `make upgrade`, then retry. Nothing was changed by the refused step |
| Storage "Needs attention" with **Migrate storage** | Storage from an earlier version is in namespace `minio`. **Migrate storage** moves it (§10.1); Set up, and a new pipeline server, wait until it has copied the data |
| Migrate storage stopped: "The copy is not complete" | Nothing else changed. The message says what differs and whether a retry can help. Objects or buckets missing, or unreadable: run it again (the copy never deletes). Before the first verified copy, while nothing uses the new storage, changed objects are copied again. Objects "different in the new storage" after that (a migration finished, pipeline servers write there, or Delete old storage's final copy): the copy never overwrites them, so a retry gives the same result; for each path named, copy the old version into `seaweedfs` yourself or delete it from `minio`, then run it again. Extra objects in `seaweedfs` are only reported |
| Migrate storage: "The copy is still running" | A large storage: the Job keeps running (up to an hour). Run Migrate storage again to wait for it |
| Delete old storage: "The final copy and check are still running" | Nothing was deleted and the copy keeps running (Job `seaweedfs-migrate`, marked as a final copy). Run Delete old storage again: it waits for that copy instead of starting over, and deletes the old storage only once it passed |
| Migrate storage: "Not enough time is left in this run" | The run stopped before moving the next pipeline server, so none is left moved but unchecked. Run Migrate storage again to continue |
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
| "Project ... no longer exists" | The address names a project that was deleted (or is no longer a data science project). Choose another; Set up is not offered for it, since it would create the project again |
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
| Sample data: "lacks the rights for this call" | The updater's ServiceAccount misses the samples rules: re-apply the template (`make upgrade`). MLflow caches access decisions for up to 5 minutes, so retry after that |
| Sample **Partly added** | An earlier Add stopped part-way; the row names what is missing. **Add** (or **Regenerate** for the agent and prompt demo) completes it |
| Sample **Not managed by this tool** | Something of the sample's name exists without the `rhoai-nightly-updater.sample` tag; the tool never changes or deletes it. Rename or delete it in MLflow if it is yours to remove |
| Agent and prompt demo has no Remove | By design: the updater holds no right to delete MLflow data it did not name. Remove it in the MLflow UI; Clean up leaves it |
| Agent skill **Not available** | This MLflow has no Skill Registry (older RHOAI releases). The dashboard's AI hub > Skills also needs the `skillRegistry` flag |
| Sample pipeline run Failed: an image cannot be pulled | Disconnected or mirrored cluster: mirror the image the section shows, or set `SAMPLE_PIPELINE_IMAGE` on the updater to a pullable copy, download the file again and import it as a new pipeline (or a new version) |
| Sample pipeline commands: `curl: (22) ... 401` or `403` | The commands use your own token (`oc whoami -t`): log in again with `oc login`, and check you can edit the project. The updater does not run the sample for you |
| Sample pipeline commands: upload fails with "already exists" | A pipeline `rhoai-nightly-sample` is in the project already. Start another run with the last command (in the same shell), or delete the pipeline in the dashboard first |

### 10.2 Models: the inference gateway and a CPU model

The **Models** tab sets up two parts with one **Set up**: the cluster's **inference gateway** (Gateway `openshift-ingress/openshift-ai-inference`, which every model is reached through) and **Qwen2.5 0.5B Instruct** on CPU in the chosen project (LLMInferenceService `qwen25-05b-instruct`). Objects: [CLUSTER_CHANGES §8](docs/CLUSTER_CHANGES.md#models-inference-gateway-cluster-and-a-cpu-model-a-project-you-choose).

Before Set up the tab says what is missing: model serving turned off (DataScienceCluster `kserve` not Managed, or `KserveLLMInferenceServiceDependencies=False`), no CPU preset on this release, no data science project, or too little room on the workers (a warning only: the model needs 4 CPUs and 12 GiB requested, 16 GiB limit, on one amd64 worker). A gateway someone else made (RHOAI's install, an admin) reads **Not managed by this tool** and is used as it is.

Call a ready model with your own token (no account or key is made):

```bash
TOKEN=$(oc whoami -t)
URL=$(oc get llminferenceservice qwen25-05b-instruct -n <project> -o jsonpath='{.status.url}')
curl -s -H "Authorization: Bearer $TOKEN" $URL/v1/models
curl -s -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' $URL/v1/chat/completions \
  -d '{"model":"qwen25-05b-instruct","messages":[{"role":"user","content":"Say hello"}],"max_tokens":64}'
```

`401`: no or expired token. `403`: your user cannot `get` the model (no access to the project). The `model` field is the resource name, as the dashboard's playground expects.

| Symptom | Cause and fix |
|---|---|
| Model **Error**: "The cluster preset's image is not published yet" (`ErrImagePull ... manifest unknown`) | A nightly build's CPU preset names a runtime image Red Hat has not published. Use **Use the newest released Red Hat image** on the model: it sets the newest released `registry.redhat.io/rhaii/vllm-cpu-rhel9` image, pinned by digest. Or mirror `registry.redhat.io/rhaii` to `registry.stage.redhat.io/rhaii` (ImageDigestMirrorSet) with a pull credential for that registry, then set the model up again. On disconnected clusters set `VLLM_CPU_IMAGE` on the updater |
| Model stays **Setting up**, "Waiting for a worker with room" | `FailedScheduling: Insufficient cpu/memory`. Free 4 CPUs / 12 GiB on one amd64 worker, or add a node; it starts by itself |
| Model **Error**: "keeps crashing ... OOMKilled" | The 16 GiB limit was too small for the model's KV cache; check `oc logs deploy/qwen25-05b-instruct-kserve -n <project> -c main` |
| Model **Error**: "KServe cannot resolve its preset" | The preset named in `spec.baseRefs` is gone (RHOAI was updated and its prefix changed). Set the model up again: it re-reads the prefix |
| Gateway **Needs attention**: "The gateway's authentication is missing" | odh-model-controller adds AuthPolicy `openshift-ai-inference-authn` once a model uses the gateway; check that odh-model-controller and Connectivity Link (Kuadrant) run: `oc get authpolicy -n openshift-ingress` |
| Gateway **Setting up** for minutes | `oc get gateway openshift-ai-inference -n openshift-ingress -o yaml`: `Programmed=False` names the reason (no load balancer quota, for example). DNS for a new load balancer can take a few minutes |
| Gateway **Unknown · not managed by this tool** | Someone else's gateway that reports no `Programmed` condition: the tool cannot tell; ask its owner |
| **Delete inference gateway** unavailable | Models route through it (named), or routes are attached to it (`oc get httproute -A`). Delete those first; models of other tools are never deleted here |

Rollback: **Delete model**, then **Delete inference gateway** (or **Clean up Models**, which runs both in that order). By hand, only for what the tool made (`app.kubernetes.io/managed-by=rhoai-nightly-updater`): `oc delete llminferenceservice qwen25-05b-instruct -n <project>`, then `oc delete gateway openshift-ai-inference -n openshift-ingress`, `oc delete configmap openshift-ai-inference-options -n openshift-ingress`, `oc delete gatewayclass openshift-ai-inference`.

### 10.3 Playground: the dashboard's Gen AI playground

The **Playground** tab asks the RHOAI dashboard's own gen-ai API to create or delete its Gen AI playground in a project. Calls and objects: [CLUSTER_CHANGES §8](docs/CLUSTER_CHANGES.md#playground-the-dashboards-gen-ai-playground-a-project-you-choose).

Chat with a ready playground from a terminal (the dashboard checks that your user may use the project):

```bash
TOKEN=$(oc whoami -t)
curl -s -X POST "https://rh-ai.<apps domain>/gen-ai/api/v1/lsd/responses?namespace=<project>" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"model":"qwen25-05b-instruct","model_source_type":"namespace","input":"Say hello","stream":false}'
```

| Symptom | Cause and fix |
|---|---|
| Set up unavailable: "Needs: Models" | The project has no ready chat model. Set one up on the Models tab, or wait until it is ready |
| A playground this tool set up now reads "not managed by this tool" | Its OGXServer has another UID than the one recorded: it was deleted and made again in Gen AI studio, or the project was recreated. The new one is someone else's; the next Set up or Delete in the project drops the old record. Delete it in Gen AI studio if you no longer need it |
| "This RHOAI release's dashboard gen-ai API differs" | A gen-ai path answered 404 on this release. Use the playground from the dashboard's Gen AI studio; report the call the message names |
| "The dashboard's gen-ai API refused the request (HTTP 403)" | The updater's ServiceAccount lacks a rule the dashboard needs: re-apply the template (`deploy/template.yaml`). `oc auth can-i list ogxservers.ogx.io -n <project> --as=system:serviceaccount:<ns>:<sa>` must say yes |
| Install fails: "An earlier playground ... left objects behind" | ConfigMaps of a playground whose server is gone (400 "already exists"). Someone with edit rights deletes the one named, for example `oc delete configmap -n <project> llama-stack-config` (check `oc get configmap -n <project> -l opendatahub.io/dashboard=true`), then Set up again |
| "still being removed after 3:00" | The previous playground's database (`genai-pgvector*`) is still terminating: `oc get deploy,pvc -n <project> -l gen-ai.opendatahub.io/pgvector=true`. Set up again once it is gone |
| Playground **Error** (phase Failed) | `oc get ogxserver lsd-genai-playground -n <project> -o yaml` and `oc get events -n <project>`; the OGX operator's webhook and `default-ogx` must be ready. Delete and set up again |
| Delete unavailable, **Not managed by this tool** | Someone set it up in the dashboard; delete it there |

Rollback: **Delete** in the playground's menu (or **Clean up Playground**). By hand, the dashboard's way: `oc delete ogxserver lsd-genai-playground -n <project>`, then the `genai-pgvector*` objects (`oc delete deploy,svc,networkpolicy,pvc,secret,configmap -n <project> -l gen-ai.opendatahub.io/pgvector=true`), then remove `<project>` from the `playgrounds` key of ConfigMap `rhoai-nightly-updater-snapshot` in the updater's namespace.

### 10.4 MaaS: Models as a Service

The **MaaS** tab sets up the MaaS platform as the MaaS install docs do and publishes this tool's model. Objects and order: [CLUSTER_CHANGES](docs/CLUSTER_CHANGES.md#maas-models-as-a-service-cluster-and-this-tools-published-model).

```bash
INFRA=redhat-ai-gateway-infra   # odh-ai-gateway-infra on ODH
oc get dsc -o jsonpath='{.items[0].spec.components.aigateway}'; echo
oc get clusters.postgresql.cnpg.io maas-db -n $INFRA
oc get gateway maas-default-gateway -n openshift-ingress
oc get deploy maas-api -n $INFRA
oc get aitenant -n ai-tenants; oc get maastenantconfig -n models-as-a-service
oc get authpolicy -n openshift-ingress
oc get maasmodelref -A; oc get maassubscription,maasauthpolicy -n models-as-a-service
```

| Symptom | Cause and fix |
|---|---|
| Set up refused: no Kuadrant CR | A cluster-admin installs Red Hat Connectivity Link and creates the Kuadrant CR (MaaS `platform-setup.md`) |
| Set up refused: CloudNativePG not installed | Install the CloudNativePG operator from OperatorHub, or create Secret `maas-db-config` for your own PostgreSQL |
| **Authorino TLS** needs attention | Run the commands the part shows (the docs' `setup-authorino-tls.sh`) as a cluster-admin; until then API keys are refused |
| maas-api restarts | It restarts until the database answers: `oc get cluster maas-db -n $INFRA`, `oc logs deploy/maas-api -n $INFRA --previous` |
| Gateway not programmed, or `maas.<apps domain>` does not resolve | OpenShift is making the load balancer; `oc get dnsrecords -n openshift-ingress` shows the record |
| Connectivity Link version warning | The docs ask for RHCL 1.5 (channel `stable-1.5`); 1.4.x can hang callers with two or more subscriptions |
| Model delete refused: published | Unpublish it on the MaaS tab first |
| A published model's direct address (`inference.<apps domain>/<project>/qwen25-05b-instruct`) answers 404 | Expected: while published, the model routes through `maas-default-gateway` only. Use it through MaaS with an API key (the MaaS tab's commands) or the dashboard's playground; Unpublish brings the direct address back |
| A published model's direct address answers 500 (Envoy log: `kuadrant_wasm_shim: Failed to apply predicates ... selected_subscription_id`) | A publication by an earlier build, which kept the model on the inference gateway too; the MaaS tab shows it as "Partly published". Unpublish, then Publish again. `oc get llminferenceservice qwen25-05b-instruct -n <project> -o jsonpath='{.spec.router.gateway.refs}'` then names `maas-default-gateway` alone |
| Publish or Unpublish stops with "Partly published: the model's route ..." | KServe did not move the model's HTTPRoute within 2 minutes: `oc get httproute qwen25-05b-instruct-kserve-route -n <project> -o jsonpath='{.spec.parentRefs}'`, and the model's `HTTPRoutesReady` condition. Unpublish (again) once KServe runs |
| Unpublish refused: someone added a gateway | Someone added a gateway to the published model; writing back the refs from before publishing would drop it. Set the refs the model should use without `maas-default-gateway` (the message has the `oc patch` for KServe's default), then Unpublish again |

Rollback: **Remove MaaS** (or Clean up everything). By hand, in order: `oc delete maasauthpolicy rhoai-nightly-updater-access maassubscription rhoai-nightly-updater-subscription -n models-as-a-service`, `oc delete maasmodelref qwen25-05b-instruct -n <project>`, set `aigateway.modelsAsAService` back to the value in data key `maas` of ConfigMap `rhoai-nightly-updater-snapshot`, then `oc delete gateway maas-default-gateway -n openshift-ingress`, `oc delete secret maas-db-config -n $INFRA`, `oc delete cluster.postgresql.cnpg.io maas-db -n $INFRA`, and remove the `maas` key.

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
  A["Migrate storage"] --> B["Set up seaweedfs with the old keys"]
  B --> C["Job seaweedfs-migrate: copy, check every byte"]
  C -->|"any difference"| X["Stop: nothing else changed"]
  C --> D["Move this tool's pipeline servers, one at a time"]
  D -->|"not ObjectStoreAvailable in 2 minutes"| Y["Stop: the old storage is kept"]
  D --> E["Summary and checklist"]
  E --> F["Later: Delete old storage (confirm, optionally namespace minio)"]
  F --> G["Copy and check once more, then delete the old objects by UID"]
```

What each step does, and what is never touched: [CLUSTER_CHANGES §8](docs/CLUSTER_CHANGES.md#s3-storage-of-an-earlier-version-namespace-minio). Every run starts with a dry run and does only what is still missing, so after any stop you run **Migrate storage** again.

Before you remove the old storage, update what this tool did not create (data connections, workbenches, other pipeline servers): the endpoint becomes `http://seaweedfs-s3.seaweedfs.svc:8333`; keys, bucket names and object paths stay. Anything still on `minio-service.minio.svc` stops working after the removal. A pipeline server the tool did not create is listed in the dialog and blocks the removal until it moves.

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

Then copy with a one-off pod. The keys come from `minio-secret` through `secretKeyRef` (the new storage has the same keys); the client image has no shell, so Kubernetes builds the endpoints from `$(AK)`/`$(SK)`. `--overwrite` is left out, so nothing already in the new storage is replaced:

```sh
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
    - {name: MC_HOST_new, value: "http://$(AK):$(SK)@seaweedfs-s3.seaweedfs.svc:8333"}
    securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}, runAsNonRoot: true, seccompProfile: {type: RuntimeDefault}}
EOF
oc -n minio wait pod/mc-copy --for=jsonpath='{.status.phase}'=Succeeded --timeout=30m
oc -n minio logs mc-copy | tail -5
oc -n minio delete pod mc-copy
oc -n minio delete deploy/minio-old svc/minio-old
oc -n minio wait --for=delete pod -l app=minio-old --timeout=2m
oc -n minio delete networkpolicy minio-old-ingress
```

Old runs' artifacts resolve again, because KFP stores bucket and key only.

## 11. Manual recovery for operator deadlocks

These follow the deadlock map in the RHOAI 3.6 operator notes (D1–D8). The tool refuses the flows that cause them, but a cluster can reach them by other means. All recovery steps are **MANUAL ADMIN STEPs**.

### 11.1 D1: dashboard-operator scaled to 0 (Dashboard CR deletion hangs)

Only dashboard-operator removes the Dashboard CR finalizer `components.platform.opendatahub.io/cleanup`. If it stays at 0 while the Dashboard is being removed, the DSC change or uninstall waits forever.

```bash
oc get deploy dashboard-operator -n redhat-ods-applications -o jsonpath='{.spec.replicas}{"\n"}'
oc get dashboards.components.platform.opendatahub.io -o jsonpath='{range .items[*]}{.metadata.name} {.metadata.deletionTimestamp}{"\n"}{end}'
```

Fix: **Revert** on Dashboard Dev. If the UI is unavailable (MANUAL ADMIN STEP):

```bash
oc scale deploy/dashboard-operator -n redhat-ods-applications --replicas=1
oc annotate deploy/dashboard-operator -n redhat-ods-applications rhoai-nightly-updater.opendatahub.io/dashboard-dev-
```

The operator then restores the release images and finishes the deletion.

### 11.2 D3: stale CRD conversion webhook

All reads and writes that need conversion fail with `conversion webhook ... service "<x>" not found`, and garbage collection and namespace deletion stall. List the CRDs that use a webhook, then check each Service:

```bash
oc get crd -o jsonpath='{range .items[?(@.spec.conversion.strategy=="Webhook")]}{.metadata.name}{"  "}{.spec.conversion.webhook.clientConfig.service.namespace}/{.spec.conversion.webhook.clientConfig.service.name}{"\n"}{end}'
oc get svc -n <namespace> <service>          # NotFound = stale
```

Recovery (MANUAL ADMIN STEP), in order of preference:
1. **Bring the producer back.** If the module was set to `Removed` in the DSC, set it to `Managed` again, wait for its operator and Service, migrate or delete the affected CRs, then remove the module again.
2. **DSC/DSCI CRDs** while the RHOAI operator is not installed: Install a different build from the UI does this safely (it switches conversion to `None` only when no RHOAI CSV exists and the Service is missing).
3. **Last resort, lossy:** `None` converts no fields; reads at another version return the stored shape. Only for CRDs whose objects you can lose, and the owning operator may revert it:
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

Stuck **module CRs** (`components.platform.opendatahub.io`): Diagnostics shows the owning operator and why it isn't finishing. Start that operator rather than stripping the finalizer. Install a different build strips one only after 10 minutes, and only when the operator Deployment is gone.

### 11.5 Recurring `BundleUnpackFailed`

OLM's documented recovery ("Refreshing failing subscriptions"), as a MANUAL ADMIN STEP:
1. Delete the Subscription and CSV. Install a different build in the UI does this.
2. Delete the failing bundle-unpack **Job and ConfigMap** in `openshift-marketplace`: `oc get job,cm -n openshift-marketplace | grep <bundle-hash>`. The tool never deletes these.
3. Install a different build (Recovery on the Status page).

### 11.6 Upgrade leftovers the operator cannot fix itself

<img src="docs/images/diag-apply-failures.png" alt="Diagnostics: Deployment redhat-ods-applications/kuberay-operator must be recreated: spec.selector cannot be changed, with the commands" width="760">

The DataScienceCluster, the Platform and the module CRs report objects the operator failed to apply as `failure deploying resource <ns>/<name>: apply failed <group/version>, Kind=<Kind>: …`. Diagnostics ("Objects the operator cannot update") lists each object once, with the CRs that report it and exact commands. It changes nothing. Find them by hand with:

```bash
oc get datasciencecluster -o jsonpath='{range .items[*].status.conditions[*]}{.message}{"\n"}{end}' | grep 'failure deploying'
oc get platforms.config.opendatahub.io -o jsonpath='{range .items[*].status.conditions[*]}{.message}{"\n"}{end}' | grep 'failure deploying'
```

MANUAL ADMIN STEPs, by error:

1. **`field is immutable`** (for example `spec.selector` of a Deployment an older version created). The object must be recreated: delete it, then restart the operator that reconciles the reporting CR so it reconciles at once. Module operators back off a failing object exponentially (up to about 16m40s) and may wait that long otherwise.
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

## 12. Activity log

ConfigMap `rhoai-nightly-updater-activity` holds the audit log (`data.entries`, JSON). If it is corrupted, the app logs a parse warning and starts a fresh list on the next write. To reset it (MANUAL ADMIN STEP): `oc delete cm rhoai-nightly-updater-activity -n $NS`.

## 13. Clusters managed by Argo CD

On a cluster where Argo CD (OpenShift GitOps) installs RHOAI from Git, for example with the [odh-gitops](https://github.com/opendatahub-io/odh-gitops) chart, Argo CD keeps the CatalogSource, the Subscription and the DataScienceCluster as Git describes them. With `selfHeal`, it [syncs as soon as the cluster drifts from Git](https://argo-cd.readthedocs.io/en/stable/user-guide/auto_sync/), and it recreates what is deleted. Git is the source of truth, and the tool works with that: it notices Argo CD by its tracking annotation `argocd.argoproj.io/tracking-id` ([resource tracking](https://argo-cd.readthedocs.io/en/stable/user-guide/resource_tracking/)), shows a notice on the Status page, and only does what Argo CD keeps. **No change to the Argo CD Application is needed**: the tool keeps every field Git sets.

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

**Checking what Argo CD tracks.** Which Application renders the catalog and the Subscription, and its sync policy:

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
| Serving cert | Secret `$APP-tls` (service-ca) |
| ClusterRole / ClusterRoleBinding / ConsoleLink | `$APP-$NS` |
| Roles | `$APP` in `$NS`; `$APP-$NS` in `kube-system`, `openshift-marketplace` |
| State ConfigMaps | `rhoai-nightly-updater-{activity,snapshot,operation}` (`operation` holds the running-operation marker, `lastCompleted` and the cross-pod lease `lock`) |
