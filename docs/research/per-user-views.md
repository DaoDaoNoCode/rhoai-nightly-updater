# Design: per-user views (5.0.0)

Status: agreed with the owner on 2026-11-20; built on `feat/per-user-views`,
released as 5.0.0 through pre-releases (`v5.0.0-alpha.N`). The inventory it
rests on (every GET route, the client it reads with, the page that calls it,
what a non-admin's own RBAC allows) was taken from `main` at e729bf5; the
facts it cites are repeated here where a decision depends on them.

## Why

Up to 4.x only cluster-admins can open the updater: oauth-proxy lets in only
users who may `list pods` in `redhat-ods-operator`
(`deploy/template.yaml`, `--openshift-sar`), and RHOAI binds nothing there,
so neither project users nor `rhods-admins` members pass. Reads after the
gate use the ServiceAccount, so anyone let in would see the SA's view of the
whole cluster. The goal: every RHOAI user can open the updater and sees what
is useful to them, read with their own token where the data is theirs, and
nothing they should not see. Changing things stays cluster-admin only.

## Decisions (owner, 2026-11-20)

1. **Who signs in: whoever RHOAI lets in.** The gate becomes "may `get`
   `auths.services.platform.opendatahub.io`". RHOAI binds exactly that to its
   Auth CR's `allowedGroups` (ClusterRole
   `data-science-allowedgroupcluster-role`; default group
   `system:authenticated`), so by default every OpenShift user signs in, and
   an admin who narrows RHOAI's allowed groups narrows the updater too.
   Cluster-admins always pass. Without RHOAI installed only cluster-admins
   pass (no binding exists), which is fine: installing RHOAI is an admin's
   job.
2. **Two tiers.** `admin` = today's mutation check (SAR `*`/`*`/`*`
   cluster-wide with the user's token): everything, as today. `user` =
   everyone else, `rhods-admins` members who are not cluster-admins included
   (an intermediate tier is not built now).
3. **Non-admins see a short read-only cluster summary** (below), read by
   the ServiceAccount with a reviewed field list.
4. **Non-admins never see other users' names** (activity log, operation
   banner).
5. **Fail closed.** Every GET route is classified; a route missing from the
   classification is admin-only, and a test fails when a registered route is
   not classified.

## Pages for a non-admin

| Page | Non-admin view |
|---|---|
| Status (`/`) | The summary only: updater version; RHOAI installed or not, its version, channel and CSV phase; the installed nightly build and whether a newer one exists; whether a DataScienceCluster exists, is Ready, and each component's managementState and Ready. A running operation shows as "An update is running since HH:MM" with no name. No activity log, pull secret card, update or reinstall actions, Argo CD details. |
| Build Explorer | As today (registry metadata read by the SA's registry proxy; it returns build data only, never the credential). |
| Environment | The user's own projects only (the projects API with their token). Per project, read with their token: pipeline servers, models (InferenceServices / LLMInferenceServices), playground status (gen-ai API), MLflow samples, the pipeline sample spec. Platform parts (S3 storage, inference gateway, MaaS platform, prerequisites) and every create/remove action: a short "a cluster-admin sets this up" note. |
| Components, Dashboard Dev, Diagnostics | Not in the navigation; a direct link shows "Only cluster-admins can see this page". |

The gitops profile stays cluster-admin only.

## API

- `GET /api/user/permissions` adds `role: "admin" | "user"`.
- New `GET /api/summary` (all signed-in users; SA reads; a fixed response
  type, so a new field is a reviewed change): `updaterVersion`,
  `rhoai {installed, version, channel, phase}`,
  `nightly {installedBuild, latestBuild, newerAvailable}`,
  `dsc {exists, ready, components: [{name, managementState, ready}]}`.
  No node, pod, project or user names; no pull-secret detail.
- `GET /api/operation` for a user: `{running, label, startedAt}` only (no
  `user`, `pod`, `interrupted`). For an admin, unchanged.
- `GET /api/resources/projects`: the projects API
  (`project.openshift.io/v1/projects`, same label selector) with the user's
  token, for both tiers.
- Project-scoped Environment reads (`/api/resources/playground`,
  `/api/resources/samples/mlflow`, `/api/resources/samples/pipeline/spec`,
  and a project-scoped status for users) read with the user's token;
  the playground fan-out covers only the user's projects.
- Admin-only GETs (403 `admin_only` for users): `/api/status`,
  `/api/activity`, `/api/components*`, `/api/debug`, `/api/dashboard/*`,
  `/api/diagnostics`, `/api/setup/dsc/preview`, `/api/test-pull-secret`,
  `/api/verify-nodes`, `/api/latest-nightly`, `/api/nightly-tags`,
  `/api/resources/maas*`, `/api/resources/prerequisites`,
  `/api/resources/models/exposures`, `/api/resources/s3/admin-password`, and
  the full (cluster-wide) `/api/resources/status`.
- Unchanged for all: `/api/update-check`, `/api/version`, health, metrics,
  Build Explorer routes.

## Hazards to fix in the same change

- **Caches**: a read made with a user's token is cached per user (by token
  hash, as the playground caches already are); SA caches keep serving only
  SA-read data. `mlflowSamplesCache` is keyed by project only today.
- **RBAC tests** assume every `withAuth` read is an SA read; user-token
  reads become their own category.
- **Errors**: a 403 on a user-token read means "you cannot see this", not
  "the template is out of date"; the UI needs a "not yours to see" state
  instead of the generic "Access denied".
- The sign-in change is a template change: TEMPLATE_REVISION bump, so 5.0.0.
  SECURITY.md gets the audit: who signs in now, what each tier reads, the
  summary's field list, and that the app now receives `user:full` tokens of
  every RHOAI user who signs in (it already did for admins).

## Verification

Live, on a test instance of the branch, with four signed-in personas made
for the test and removed after: a cluster-admin; a `rhods-admins` member who
is not cluster-admin; a project admin of one test project; a user with no
project. Each page and each GET route is checked for each persona (what is
shown, what is refused, that no other user's names or projects appear),
light and dark screenshots of the non-admin pages, and the cleanup is
verified against snapshots.
