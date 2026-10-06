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
| **Operator failed**, **No Subscription**, a failed Update or Reinstall step | [§6](#6-update--reinstall-failed) |
| A PR shows "Does not contain" in Build Explorer | [§8](#8-build-explorer-quay-and-github) |
| "Still MinIO: migration pending", S3 storage **Incomplete**, Tear down disabled | [§10](#10-test-resources) |
| DataScienceCluster **Not Ready**, a Diagnostics problem | [§11.7](#117-datasciencecluster-not-ready), [§11.6](#116-upgrade-leftovers-the-operator-cannot-fix-itself) |

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

- **"vX.Y.Z is available"**: a newer release exists. "Requires a full redeploy" means a new major version: run its `install.sh` (or check it out and `make upgrade`). Dismissing hides it until an even newer release.

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

**Restarts wait for operations.** If an Update/Reinstall is running, the old pod finishes it first (drain up to 980 s, `terminationGracePeriodSeconds: 1020`, `Recreate`). oauth-proxy exits at once, so the UI is unreachable for up to ~17 minutes. `make upgrade`/`rollback` wait up to `ROLLOUT_TIMEOUT=20m`. A direct `oc delete pod` or an eviction is different: the ReplicaSet starts a replacement at once, which shows the old pod's operation as running "on updater pod …" and refuses changes until it ends (§5). Check whether something is running before you restart (`operation` is the marker, `lock` the lease):

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
- **"*X* was interrupted":** the operation's marker is still recorded and the process that wrote it holds no live lease: the pod or its container stopped mid-operation (SIGKILL, node loss, OOM), or an updater version without leases left it. An operation that still runs in another pod is not reported as interrupted. Check the operator status, then run the same operation again. Every step is safe to repeat; Update and Reinstall recreate a missing Subscription with the settings recorded before the last operation. For Dashboard Dev, run **Revert**.
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

## 6. Update / Reinstall failed

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

**The operator has no Subscription**: usually an interrupted Update or Reinstall. Update or Reinstall recreates the Subscription with the settings recorded before the last operation.

<img src="docs/images/status-recovery.png" alt="The Recovery card: Re-deploy the operator and Reinstall the operator" width="760">

**Recovery** on the Status page: **Re-deploy operator...** installs the same version again; **Reinstall...** removes the operator and installs the target you choose (stable, a nightly, an exact build).

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
| "older than the installed" | Update never downgrades. Use Reinstall and tick the confirmation (see "Downgrades" below) |
| Succeeded, but "an upgrade gate holds it" | The operator waits for an admin acknowledgement (set its key to "true" in ConfigMap `odh-upgrade-acks` in `redhat-ods-operator`) or cannot resolve a gate because it is older than the resources it found. The note reads the Platform and every DataScienceCluster condition |
| Diagnostics: "Objects the operator cannot update" | Upgrade leftovers; see §11.6 |
| "cannot read the installed CSV version" | Use Reinstall and confirm |
| A second rhods-operator Subscription exists | Delete the extra one yourself; the tool refuses to guess which one is right |
| Namespace has 2+ OperatorGroups / a non-global one | Keep one global OperatorGroup in `redhat-ods-operator` |
| Re-deploy refuses: "channel head moved" (Automatic approval) | Use Update (it changes the version) |
| `BundleUnpackFailed` keeps recurring | See §11.5 |
| Webhook "connection refused / no endpoints" errors | Diagnostics → stale webhooks. For a CRD conversion webhook see §11.2 |

**Downgrades.** OLM cannot downgrade, so a confirmed Reinstall to an older version removes the operator and installs the older bundle. OLM applies every CRD in that bundle, so the CRDs it ships are **replaced by their older versions**:
- fields only the newer version knows can be pruned from stored objects such as the DataScienceCluster, and are not restored by going back;
- a same-named CRD that the newer version no longer ships stays at the older schema after you go back with Update, and the newer operator then fails on it (§11.6, schema mismatch);
- the older operator may refuse to manage, or stop at upgrade gates for, resources the newer version created (DSC `Ready: Error … upgrade gate`, `ModulesReady: AdminAckRequired`). The result of the Reinstall says so; check Components and Diagnostics. Go back to the newer version with **Update**.

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
- **Commit dates show "-", or PR search reports a rate limit:** GitHub allows 60 requests/hour per egress IP without a token. Set `GITHUB_TOKEN` ([README: Configuration](README.md#configuration)). Results are cached in memory per pod (cleared on restart).
- **A PR shows as not contained, but its change is in the build:**

  <img src="docs/images/builds-pr-search.png" alt="PR search for #5123: only rhoai-3.7-ea.1 contains it; the release branches do not" width="760">

  In this example only the build from `main` contains the PR. The release branches (`rhoai-3.6`, `rhoai-3.5`, ...) show **Does not contain** even if the change was cherry-picked there, because it arrived as a cherry-pick with a different SHA. PR search only follows merge commits.

## 9. Dashboard Dev

<img src="docs/images/dashboard-dev-session.png" alt="Dashboard Dev session active: PR #5123, Operator paused, with Revert to default" width="760">

1. Who deployed which build, and when.
2. **Revert to default** ends the session.

- **Banner "Dashboard Dev session active":** `dashboard-operator` is paused (0 replicas), so RHOAI updates don't reach the dashboard. Use **Revert** on Dashboard Dev when done. Operator operations offer "Revert Dashboard Dev and continue".
- **"A dashboard deletion is waiting for the paused dashboard-operator":** this is deadlock D1. Revert at once (§11.1).
- **Stuck rollout:** the panel reports the Deployment's `ProgressDeadlineExceeded`. Revert, or deploy again.
- **Revert fails, or the UI is down:** §11.1.

## 10. Test resources

The S3 storage is SeaweedFS (Deployment `seaweedfs`, PVC `seaweedfs-pvc`) behind Service `minio-service`, a name kept from the MinIO earlier versions deployed. Objects and ports: [CLUSTER_CHANGES §8](docs/CLUSTER_CHANGES.md#8-test-resources).

<img src="docs/images/s3-incomplete.png" alt="S3 storage (SeaweedFS) Incomplete: Deployment seaweedfs is scaled to 0 replicas, with Repair" width="760">

1. **Incomplete**: SeaweedFS does not serve through `minio-service` and `minio-ui`.
2. Why, in one line (here: scaled to 0, for example by a manual rollback).
3. **Repair** re-runs setup and keeps the stored data:

<img src="docs/images/s3-repair.gif" alt="Repair, confirm Repair S3 storage, the storage is Running again" width="760">

<img src="docs/images/s3-running.png" alt="S3 storage Running; Tear down is blocked by a pipeline server" width="760">

1. **Running**.
2. **Open admin UI** (user `admin`).
3. **Tear down** is disabled while a pipeline server uses the storage; the line names it.

| Problem | Fix |
|---|---|
| S3 storage setup: "not created by this tool" | The `minio` namespace holds foreign objects. Nothing was changed. Remove them or use another cluster |
| Setup or teardown: "not allowed ... run `make upgrade`" | The image is newer than the template (RBAC for the SeaweedFS names is missing; the banner says the template is outdated). An admin runs `make upgrade`, then retry. Nothing was changed by the refused step |
| "Still MinIO: migration pending" | MinIO from an earlier version still serves. **Migrate to SeaweedFS** (or re-run setup) replaces it and starts fresh (see below) |
| S3 storage "Incomplete" or "Needs repair" | SeaweedFS runs but `minio-service`/`minio-ui` do not point at it, it is scaled to 0, or the MinIO cleanup is unfinished. Press **Repair** (re-runs setup). During a manual rollback (§10.1) this is expected; Repair ends the rollback |
| SeaweedFS not ready / ImagePullBackOff | MinIO, if it was being replaced, keeps serving. Fix the cause (for example `SEAWEEDFS_IMAGE` to a mirror) and **Repair**/**Migrate** again. `oc logs -n minio deploy/seaweedfs` |
| S3 storage teardown refused | A pipeline server still uses the storage (the message names it). Tear that down first. After a partial teardown, run Tear down again |
| `minio` namespace left after teardown | Expected: `oc delete project minio` once `oc get all,pvc,secret -n minio` is empty |
| Reach the S3 API from your laptop | `oc port-forward -n minio svc/minio-service 9000`, endpoint `http://localhost:9000`, path-style, region `us-east-1`. Keys: `oc extract secret/minio-secret -n minio --to=-` |
| Admin UI login | **Open admin UI**; user `admin`, password `oc extract -n minio secret/minio-secret --keys=minio_root_password --to=-` |
| Pipeline server: "already has a pipeline server" | Projects get one DSPA; the existing one was left unchanged |
| Old pipeline run artifacts return 404 after the migration | Expected: the migration starts fresh. The files are still on `minio-pvc`; copy them (below) if you need them. If a cached step fails because its outputs are gone, re-run with caching disabled |
| DSPA or project stuck Terminating | §11.4 |
| MLflow teardown hangs / DSC change waits on MLflow | §11.4 |

### 10.1 MinIO to SeaweedFS: what the migration does, and how to roll back

<img src="docs/images/s3-migration-pending.png" alt="S3 storage still MinIO: migration pending, with Migrate to SeaweedFS" width="760">

1. **Still MinIO: migration pending**: the MinIO of an earlier version still serves the pipeline servers.
2. **Migrate to SeaweedFS** opens the confirmation, which lists every change:

<img src="docs/images/s3-migrate-dialog.png" alt="Dialog: Replace MinIO with SeaweedFS? with the changes and the Start fresh warning" width="760">

After **Migrate and start fresh**, the card shows SeaweedFS **Running** and the old volume that was kept:

<img src="docs/images/s3-migrate.gif" alt="Migrate to SeaweedFS, confirm, the storage runs SeaweedFS" width="760">

```mermaid
flowchart TD
  M0["MinIO serves minio-service<br>(Still MinIO: migration pending)"] --> M1["Migrate to SeaweedFS (or Set up / Repair)"]
  M1 --> M2["Start SeaweedFS on seaweedfs-pvc<br>same minio-secret, empty bucket pipelines"]
  M2 -->|"not ready in time"| MX["Nothing switched: MinIO keeps serving"]
  M2 -->|ready| M3["Switch minio-service and minio-ui to SeaweedFS"]
  M3 --> M4["Delete Deployment minio; then NetworkPolicy minio-ingress"]
  M4 --> M5["Running on SeaweedFS<br>minio-pvc kept, unused, until teardown"]
  M5 -.->|"rollback (manual, below)"| RB1["Temporary MinIO minio-old on minio-pvc"]
  RB1 -.-> RB2["Point minio-service at minio-old,<br>scale seaweedfs to 0 (shows Incomplete)"]
  RB2 -.->|"go forward: Repair"| M5
```

Setting up S3 storage on a cluster with the tool's MinIO (Deployment `minio`, shown as "Still MinIO: migration pending") replaces it:
1. SeaweedFS starts next to MinIO on a new PVC `seaweedfs-pvc`, with the same credentials (`minio-secret`) and an empty bucket `pipelines`. MinIO keeps serving meanwhile; if SeaweedFS cannot start, nothing is switched.
2. Once SeaweedFS is ready, `minio-service` and `minio-ui` switch to it. DSPAs keep `minio-service.minio.svc:9000` and their secrets; nothing in the projects changes. The data-science-pipelines operator's object-storage check may flap for a moment.
3. Deployment `minio` is deleted. NetworkPolicy `minio-ingress` is deleted once no MinIO pod is left (setup waits up to 20 seconds; otherwise it keeps the policy, reports the cleanup as incomplete, and **Repair** finishes it later). **PVC `minio-pvc` is kept**, unused, until teardown.

Nothing is copied: artifacts, logs and cached outputs of earlier runs return 404. Check the result with `oc get deploy,pvc,netpol -n minio` (expect `seaweedfs`, `seaweedfs-pvc`, `minio-pvc`, `seaweedfs-ingress`).

The page reports the storage as Running only when SeaweedFS is ready **and** `minio-service` and `minio-ui` point at it. A setup that stopped before the switch, a Service that points elsewhere, or SeaweedFS scaled to 0 shows **Incomplete** with the reason; **Repair** re-runs setup and fixes it.

#### Temporary MinIO on the kept volume

The copy and the rollback below both start a MinIO by hand on `minio-pvc`, with the image and container contract earlier versions used. The tool does not manage these objects (no tool label). Apply the NetworkPolicy first, so the pod is never reachable on other ports than the old `minio-ingress` allowed (S3 9000 from pods in any namespace, console 9090 only from the router):

```sh
oc -n minio apply -f - <<'EOF'
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: minio-old-ingress}
spec:
  podSelector: {matchLabels: {app: minio-old}}
  policyTypes: [Ingress]
  ingress:
  - from: [{namespaceSelector: {}}]
    ports: [{protocol: TCP, port: 9000}]
  - from:
    - namespaceSelector: {matchLabels: {policy-group.network.openshift.io/ingress: ""}}
    - namespaceSelector: {matchLabels: {policy-group.network.openshift.io/host-network: ""}}
    ports: [{protocol: TCP, port: 9090}]
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
        args: [server, /data, --console-address, ":9090"]
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
  ports: [{name: api, port: 9000, targetPort: 9000}, {name: ui, port: 9090, targetPort: 9090}]
EOF
oc -n minio rollout status deploy/minio-old --timeout=3m
```

To remove it, delete the Deployment and Service first and the policy only once its pod is gone:

```sh
oc -n minio delete deploy/minio-old svc/minio-old
oc -n minio wait --for=delete pod -l app=minio-old --timeout=2m
oc -n minio delete networkpolicy minio-old-ingress
```

#### Copy old objects into SeaweedFS (optional)

With the temporary MinIO running, run a one-off pod. The credentials come from `minio-secret` through `secretKeyRef`; no value appears on a command line or in the pod spec. The client image has no shell, so Kubernetes builds the `mc` endpoints from `$(AK)`/`$(SK)` (dependent environment variables):

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
    args: [mirror, --overwrite, old/pipelines, new/pipelines]
    env:
    - {name: MC_CONFIG_DIR, value: /tmp/.mc}
    - {name: AK, valueFrom: {secretKeyRef: {name: minio-secret, key: minio_root_user}}}
    - {name: SK, valueFrom: {secretKeyRef: {name: minio-secret, key: minio_root_password}}}
    - {name: MC_HOST_old, value: "http://$(AK):$(SK)@minio-old.minio.svc:9000"}
    - {name: MC_HOST_new, value: "http://$(AK):$(SK)@minio-service.minio.svc:9000"}
    securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}, runAsNonRoot: true, seccompProfile: {type: RuntimeDefault}}
EOF
oc -n minio wait pod/mc-copy --for=jsonpath='{.status.phase}'=Succeeded --timeout=30m
oc -n minio logs mc-copy | tail -5
oc -n minio delete pod mc-copy
```

Then remove the temporary MinIO (above). Old runs' artifacts resolve again, because KFP stores bucket and key only.

#### Roll back to MinIO (stop-gap)

Only while `minio-pvc` exists (teardown deletes it). Objects written to SeaweedFS since the migration are not carried back.

1. Start the temporary MinIO (policy, Deployment and Service above).
2. Point `minio-service` at it, then stop SeaweedFS:

```sh
oc -n minio patch svc minio-service --type=merge -p '{"spec":{"selector":{"app":"minio-old"},"ports":[{"name":"api","port":9000,"targetPort":9000},{"name":"ui","port":9090,"targetPort":9090}]}}'
oc -n minio scale deploy/seaweedfs --replicas=0
```

Pipeline servers reach MinIO again with no edits, and the `minio-ui` Route host serves the MinIO console. The Test resources page shows the storage as **Incomplete** ("scaled to 0 replicas"). **Do not press Repair while you rely on the rollback**: Repair switches back to SeaweedFS.

To go forward again, press **Repair** on Test resources (it scales SeaweedFS back to 1, waits for it, and points `minio-service` and `minio-ui` at it). Wait until the storage shows Running, then remove the temporary MinIO (the three commands above, policy last).

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
2. **DSC/DSCI CRDs** while the RHOAI operator is not installed: Reinstall from the UI does this safely (it switches conversion to `None` only when no RHOAI CSV exists and the Service is missing).
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

Fix: **Tear down** MLflow on Test resources. For an MLflow CR the tool didn't create (MANUAL ADMIN STEP): `oc delete mlflows.mlflow.opendatahub.io <name>`.

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

Stuck **module CRs** (`components.platform.opendatahub.io`): Diagnostics shows the owning operator and why it isn't finishing. Start that operator rather than stripping the finalizer. Reinstall strips one only after 10 minutes, and only when the operator Deployment is gone.

### 11.5 Recurring `BundleUnpackFailed`

OLM's documented recovery ("Refreshing failing subscriptions"), as a MANUAL ADMIN STEP:
1. Delete the Subscription and CSV. Reinstall in the UI does this.
2. Delete the failing bundle-unpack **Job and ConfigMap** in `openshift-marketplace`: `oc get job,cm -n openshift-marketplace | grep <bundle-hash>`. The tool never deletes these.
3. Reinstall.

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

## 12. Activity log

ConfigMap `rhoai-nightly-updater-activity` holds the audit log (`data.entries`, JSON). If it is corrupted, the app logs a parse warning and starts a fresh list on the next write. To reset it (MANUAL ADMIN STEP): `oc delete cm rhoai-nightly-updater-activity -n $NS`.

## Reference: objects of the updater itself

| Object | Name |
|---|---|
| Deployment / Service / Route / NetworkPolicy | `$APP` in `$NS` |
| Cookie secret | Secret `$APP-proxy` (`session_secret`) |
| Serving cert | Secret `$APP-tls` (service-ca) |
| ClusterRole / ClusterRoleBinding / ConsoleLink | `$APP-$NS` |
| Roles | `$APP` in `$NS`; `$APP-$NS` in `kube-system`, `openshift-marketplace` |
| State ConfigMaps | `rhoai-nightly-updater-{activity,snapshot,operation}` (`operation` holds the running-operation marker, `lastCompleted` and the cross-pod lease `lock`) |
