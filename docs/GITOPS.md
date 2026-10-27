# Deploying the updater with Argo CD (GitOps)

For clusters whose configuration lives in Git and is applied by Argo CD
(OpenShift GitOps). Argo CD cannot run `install.sh` or process an OpenShift
Template, so the repository has a Helm chart, `deploy/chart`, that Argo CD
renders from a release tag. Everything else (manual installs, upgrades,
rollbacks) stays with `install.sh` ([QUICKSTART](QUICKSTART.md),
[UPGRADING](UPGRADING.md)). Use one of the two per install, not both: Argo CD
undoes what `install.sh` changes, and `install.sh` would change what Git says.

The chart exists from v2.2.0 on.

## 1. What the chart is

`deploy/template.yaml`, the OpenShift Template `install.sh` applies, stays the
only definition of the app's objects. The chart carries a byte-identical copy
(`deploy/chart/files/template.yaml`; Helm reads only files inside the chart)
and one chart template, `templates/objects.yaml`, renders every object of it:
it takes the Template's parameter defaults, overlays `parameters` from the
values, and replaces each `${NAME}` the way `oc process` does. Nothing in the
chart names one of those objects, so a template change cannot be forgotten in
the chart. `make chart` writes the copy; `make check-chart`,
`scripts/test-chart.sh`, CI and the release guard fail when it differs.

The render fails, instead of applying something half-done, when:
- a key of `parameters` is not a parameter of the Template, or any other
  values key is misspelled (`values.schema.json` allows only known keys);
- `IMAGE` or `OAUTH_PROXY_IMAGE` is missing, or an image is not pinned by
  digest ([section 3](#3-images-by-digest));
- `IMAGE` is a release (`:vX.Y.Z@sha256:...`) other than the chart's own;
- a `${...}` placeholder is left after substitution, or the Template uses the
  non-string `${{NAME}}` form (it does not today).

What `install.sh` does, and what replaces it here:

| `install.sh` | With the chart |
|---|---|
| Checks that you are cluster-admin | Argo CD's credentials for the destination cluster need the same ([section 8](#8-what-argo-cd-needs)) |
| Detects the oauth-proxy tag from the cluster version | `parameters.OAUTH_PROXY_IMAGE`, set per cluster, by digest |
| Resolves the image tag to a digest and checks the image's commit against the template | `parameters.IMAGE` by digest in Git; the chart refuses a tag, and a release tag that is not the chart's release |
| `oc new-project` | The Application's `CreateNamespace=true` sync option |
| Creates the cookie Secret `<APP_NAME>-proxy` once | The session Secret hook ([section 4](#4-the-two-hook-jobs)) |
| `oc process \| oc apply`, then waits for the rollout | Argo CD syncs the rendered objects and waits for them to be healthy |
| Adds ConsoleLink `<APP_NAME>-<NAMESPACE>` | The ConsoleLink hook ([section 4](#4-the-two-hook-jobs)) |
| Removes objects of installs from before v1.0.0 (`cleanup-legacy`) and prunes old ReplicaSets | Not done: they matter only when upgrading such an install. Upgrade it with `install.sh` first ([section 10](#10-moving-an-install-from-installsh-to-argo-cd)) |
| `make rollback` | Revert the commit in Git ([section 6](#6-upgrading-and-rolling-back)) |
| `install.sh uninstall` | Delete the Application, then the few objects it leaves ([section 7](#7-removing-it)) |

The chart sets the Template parameter `INSTALL_METHOD=gitops`. The app then
words its notices for Git: "vX.Y.Z is available" says to change the chart's
`targetRevision` and `parameters.IMAGE` instead of showing an `install.sh`
command, and so do the "deployment is out of date" notice and the help
dialog.

## 2. Values

`values.schema.json` refuses any key not listed here, at every level.

| Value | Default | Meaning |
|---|---|---|
| `parameters.IMAGE` | none, required | The updater image by digest: `quay.io/juntao_wang/rhoai-nightly-updater:vX.Y.Z@sha256:<digest>` (or without the tag). A release tag must be the chart's release |
| `parameters.OAUTH_PROXY_IMAGE` | none, required | The oauth-proxy image by digest; its tag is the cluster's OCP minor version: `registry.redhat.io/openshift4/ose-oauth-proxy-rhel9:v4.<minor>@sha256:<digest>`. The Template's own default (a tag) is for `install.sh` and is not used |
| `parameters.NAMESPACE` | the release namespace | Must be the Application's destination namespace ([section 5](#5-the-namespace)) |
| `parameters.APP_NAME` | `rhoai-nightly-updater` | The name of the app's objects; cluster-scoped names are `<APP_NAME>-<NAMESPACE>` |
| `parameters.PULL_SECRET_NAMESPACE`, `parameters.MARKETPLACE_NAMESPACE` | `kube-system`, `openshift-marketplace` | Where the app gets a Role for the pull secret and the CatalogSources |
| `parameters.IMAGE_REPOSITORY`, `parameters.RELEASES_URL` | the Template's | The update check and its release-notes link ([README: Configuration](../README.md#configuration)) |
| `parameters.INSTALL_METHOD` | `gitops` | Leave it: it words the app's notices for Git |
| `allowTemplateMismatch` | `false` | Deploy an `IMAGE` whose release tag is not the chart's (`install.sh`'s `ALLOW_TEMPLATE_MISMATCH=1`) |
| `gitops.cliImage` | `registry.redhat.io/openshift4/ose-cli-rhel9:v4.17@sha256:b39fdb5a...` | The image of both hook Jobs, by digest. They only get, create and apply single objects, so any recent `oc` works; the tag follows the oauth-proxy convention. The rhel9 repository starts at v4.16; on OCP 4.15 use `registry.redhat.io/openshift4/ose-cli:v4.15@sha256:<digest>` |
| `gitops.sessionSecret.enabled` | `true` | The session Secret hook. With `false`, create Secret `<APP_NAME>-proxy` (key `session_secret`) before the first sync: the Deployment mounts it and does not start without it |
| `gitops.consoleLink.enabled` | `true` | The ConsoleLink hook |

The Template has no parameter for other container settings, such as
`GITHUB_TOKEN` ([README: Configuration](../README.md#configuration)), so the
chart has none either. Set with `oc set env`, such a setting is not in Git,
and Argo CD may report the Deployment as OutOfSync or, with `selfHeal`,
remove it.

## 3. Images by digest

`install.sh` resolves a tag to a digest right before it applies, so the image
it checked is the image that runs. Argo CD applies what Git says, so the
digest has to be in Git: the chart refuses `parameters.IMAGE`,
`parameters.OAUTH_PROXY_IMAGE` and `gitops.cliImage` unless each matches
`<repository>[:<tag>]@sha256:<64 hex digits>`, both in the schema and in the
chart itself (when schema validation is skipped). Keep the tag next to the
digest: it says what the digest is, and Renovate uses it.

Finding a digest:

```bash
# The updater (public on quay.io). install.sh reads it the same way.
oc image info quay.io/juntao_wang/rhoai-nightly-updater:v2.2.0 --filter-by-os=linux/amd64 -o json | jq -r '.listDigest // .digest'

# The oauth-proxy and CLI images (registry.redhat.io needs a login: podman
# login registry.redhat.io, or the cluster's pull secret in
# ~/.docker/config.json). For an image built for several architectures this
# is the manifest list's digest, which every node can pull.
oc image info registry.redhat.io/openshift4/ose-oauth-proxy-rhel9:v4.17 --filter-by-os=linux/amd64 -o json | jq -r '.listDigest // .digest'
skopeo inspect --raw docker://registry.redhat.io/openshift4/ose-cli-rhel9:v4.17 | sha256sum   # the same, with skopeo
```

The Red Hat Ecosystem Catalog (catalog.redhat.com, the image's "Get this
image" tab) also shows the manifest list digest of each tag.

**Renovate keeps the digests current.** Its `helm-values` manager does not
see these values (they are single strings under `parameters`), so add a
regex manager. A sketch for a repository laid out as in
[section 9](#9-examples); check the dependency names in Renovate's log before
relying on the grouping:

```json
{
  "customManagers": [
    {
      "customType": "regex",
      "managerFilePatterns": ["/(^|/)apps/rhoai-nightly-updater/values[^/]*\\.ya?ml$/"],
      "matchStrings": [
        "(?:IMAGE|OAUTH_PROXY_IMAGE|cliImage): *(?<depName>[^\\s:@]+):(?<currentValue>[^\\s@]+)@(?<currentDigest>sha256:[0-9a-f]{64})"
      ],
      "datasourceTemplate": "docker"
    }
  ],
  "argocd": {
    "managerFilePatterns": ["/(^|/)apps/rhoai-nightly-updater/.+\\.ya?ml$/"]
  },
  "packageRules": [
    {
      "description": "The chart and its image are one release: update them together",
      "matchDepNames": [
        "quay.io/juntao_wang/rhoai-nightly-updater",
        "https://github.com/DaoDaoNoCode/rhoai-nightly-updater.git"
      ],
      "groupName": "rhoai-nightly-updater"
    },
    {
      "description": "The oauth-proxy and CLI tags follow the cluster's OCP version: digests only",
      "matchDepNames": [
        "registry.redhat.io/openshift4/ose-oauth-proxy-rhel9",
        "registry.redhat.io/openshift4/ose-cli-rhel9"
      ],
      "matchUpdateTypes": ["major", "minor", "patch"],
      "enabled": false
    }
  ]
}
```

Renovate needs credentials for `registry.redhat.io` (a `hostRules` entry) to
read its digests. The chart and the image must move together: a chart of one
release with the image of another fails to render (unless
`allowTemplateMismatch`), and if only the digest of the same release changes,
nothing else needs to.

## 4. The two hook Jobs

Both are Argo CD hooks, so Argo CD does not track them as part of the
application. Each has its own ServiceAccount and RBAC, created as hooks one
wave earlier, with `hook-delete-policy: BeforeHookCreation,HookSucceeded`:
Argo CD deletes them once the whole sync has succeeded, and keeps them after
a failure so you can read the Job's log; the next sync replaces them. The
Jobs run `gitops.cliImage` with a non-root user (assigned by the
`restricted-v2` SCC), no privilege escalation, all capabilities dropped, the
`RuntimeDefault` seccomp profile, a read-only root filesystem, small
requests and limits, `backoffLimit: 2` and `activeDeadlineSeconds: 300`.
Their pods do not carry the app's `app` label, so the app's Service and
NetworkPolicy do not select them.

**Session Secret** (`Sync` hook; RBAC in wave -2, Job `<APP_NAME>-session-secret`
in wave -1, before the Template's objects in wave 0). The oauth-proxy cookie
secret must stay the same across upgrades, or everyone is signed out; Helm
cannot keep a random value under Argo CD (`helm template` has no `lookup`,
and a random value would change on every render). The Job creates Secret
`<APP_NAME>-proxy` with key `session_secret`, 32 random letters and digits
as `install.sh` makes them, only when the Secret does not exist; an existing
Secret is never replaced, and a Secret created meanwhile by another run is
kept. The value is written to a memory-backed volume and never printed.
Its Role allows `get` on that one Secret and `create` on Secrets (Kubernetes
cannot limit `create` to a name).

**ConsoleLink** (`PostSync` hook: it runs once the sync succeeded and the
app is healthy). The console's application-menu entry needs the Route's
host, which the router assigns. The Job reads `spec.host` of Route
`<APP_NAME>` and server-side-applies ConsoleLink `<APP_NAME>-<NAMESPACE>`,
the same object as `install.sh`'s (same name, label
`app.kubernetes.io/instance=<APP_NAME>-<NAMESPACE>`, text, section, icon and
`href: https://<host>`; `scripts/test-chart.sh` compares the two). It takes
over a link that `install.sh` created (`--force-conflicts`). RBAC: `get` on
that one Route (a Role), and `get`/`patch` on that one ConsoleLink plus
`create` on ConsoleLinks (a ClusterRole `<APP_NAME>-<NAMESPACE>-consolelink`
with the same instance label, so `install.sh uninstall` also removes it if a
failed run left it). Argo CD does not prune the ConsoleLink, because a Job
created it ([section 7](#7-removing-it)).

## 5. The namespace

Every namespaced object of the app goes to `parameters.NAMESPACE`, which
defaults to the release namespace: the Application's destination namespace.
Leave `NAMESPACE` unset, or set it to the destination namespace; any other
value would put the app in one namespace while Argo CD creates and watches
another. The chart does not create the namespace (`install.sh` used
`oc new-project`): set `CreateNamespace=true` in the Application's
`syncOptions`, as in [section 9](#9-examples).

The Roles in `kube-system` and `openshift-marketplace` (the pull secret and
the CatalogSources) are in those namespaces, as with `install.sh`.

## 6. Upgrading and rolling back

An upgrade is one commit: the chart's `targetRevision` to the new release
tag and `parameters.IMAGE` to that release's image by digest. Argo CD
re-applies the release's template with its image, so a new MAJOR version
(a template change, [CHANGELOG](../CHANGELOG.md)) needs nothing more than a
minor one. The cookie Secret is kept, so everyone stays signed in. As with
`install.sh`, a running cluster operation delays the restart until it
finishes (`Recreate`, up to ~17 minutes); the ConsoleLink hook runs once the
new pod is ready.

To roll back, revert that commit. Only releases from v2.2.0 on have the
chart; to go further back, use `make rollback` from a clone
([UPGRADING §5](UPGRADING.md#5-roll-back)), after removing the Application
without deleting its resources.

`oc rollout undo` does not work with Argo CD either: the next sync puts back
what Git says.

## 7. Removing it

Deleting the Application (with Argo CD's resources finalizer, the default
for a cascading delete) removes everything the Template creates: the
Deployment, Service, Route, NetworkPolicy, ServiceMonitor, PrometheusRule,
the ServiceAccount, the ClusterRole and ClusterRoleBinding
`<APP_NAME>-<NAMESPACE>`, and the Roles in `<NAMESPACE>`, `kube-system` and
`openshift-marketplace`.

What stays:
- the namespace, when `CreateNamespace=true` created it (Argo CD does not
  delete namespaces it created), and with it Secret `<APP_NAME>-proxy` and the
  app's ConfigMaps (activity log, snapshot, operation marker);
- ConsoleLink `<APP_NAME>-<NAMESPACE>`, created by a Job;
- the hook objects of a failed last run.

Remove those after the Application is gone (with it still there, Argo CD
would recreate the app), with `install.sh uninstall --namespace <NAMESPACE>
--app-name <APP_NAME>`, which deletes by the same instance label and then
the namespace, or by hand:

```bash
oc delete consolelink,clusterrole,clusterrolebinding -l app.kubernetes.io/instance=<APP_NAME>-<NAMESPACE>
oc delete project <NAMESPACE>
```

RHOAI and everything the app created on the cluster (catalog, pull secret,
test resources) stay, as with `install.sh uninstall`.

## 8. What Argo CD needs

- **Cluster-admin on the destination**, or the equivalent. The Template
  creates a ClusterRole, Roles in `kube-system` and `openshift-marketplace`,
  and their bindings, and Kubernetes lets a client grant only permissions it
  holds itself (or with `escalate`/`bind`). The hooks add a ClusterRole for
  ConsoleLinks. This is the same requirement as `install.sh`'s
  cluster-admin check. When Argo CD reaches a managed cluster through a
  proxy identity, check that admission on that cluster allows a
  NetworkPolicy and Roles in those namespaces.
- **Annotation-based resource tracking**
  (`application.resourceTrackingMethod: annotation`, the default from Argo
  CD 3.0). With `label` or `annotation+label` tracking, Argo CD writes its
  application name into the `app.kubernetes.io/instance` label, which the
  Template uses for `<APP_NAME>-<NAMESPACE>`: `install.sh uninstall` would no
  longer find the cluster-scoped objects by that label, and if the
  application name happened to equal `<APP_NAME>-<NAMESPACE>`, Argo CD would
  treat the hook-created ConsoleLink as its own and prune it.
- **The chart's repository.** The GitLab project is not public, so point
  Argo CD at the public GitHub mirror,
  `https://github.com/DaoDaoNoCode/rhoai-nightly-updater.git`, which has the
  same release tags.
- **Images.** The nodes pull `registry.redhat.io` images (oauth-proxy, the
  CLI) with the cluster's global pull secret, as with `install.sh`.

**When Argo CD also manages RHOAI.** Deploying the updater with Argo CD does
not change how it works with an RHOAI that Argo CD manages: it notices Argo
CD's tracking annotation on the catalog, Subscription and DataScienceCluster
and treats Git as the source of truth there
([CLUSTER_CHANGES: Update when Argo CD manages the operator](CLUSTER_CHANGES.md#update-when-argo-cd-manages-the-operator),
[RUNBOOK §13](../RUNBOOK.md#13-clusters-managed-by-argo-cd)). Keep the
updater in its own Application, apart from the one that manages RHOAI.

## 9. Examples

### One Application

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: rhoai-nightly-updater
  namespace: openshift-gitops
spec:
  project: default
  sources:
    # The chart, at a release tag.
    - repoURL: https://github.com/DaoDaoNoCode/rhoai-nightly-updater.git
      targetRevision: v2.2.0
      path: deploy/chart
      helm:
        releaseName: rhoai-nightly-updater
        valueFiles:
          - $values/apps/rhoai-nightly-updater/values.yaml
    # The values, from your GitOps repository.
    - repoURL: https://github.com/<org>/<gitops-repo>.git
      targetRevision: HEAD
      ref: values
  destination:
    server: https://kubernetes.default.svc
    namespace: rhoai-nightly-updater   # = parameters.NAMESPACE (the default)
  syncPolicy:
    automated:
      prune: true
      selfHeal: true
    syncOptions:
      - CreateNamespace=true
```

`apps/rhoai-nightly-updater/values.yaml` in the GitOps repository:

```yaml
parameters:
  IMAGE: quay.io/juntao_wang/rhoai-nightly-updater:v2.2.0@sha256:<digest>
  OAUTH_PROXY_IMAGE: registry.redhat.io/openshift4/ose-oauth-proxy-rhel9:v4.17@sha256:<digest>
```

### An ApplicationSet for every cluster

In a GitOps repository that adds each app as `apps/<name>/application.yaml`
(an Application that syncs `apps/<name>/hub`), the ApplicationSet goes in
`apps/rhoai-nightly-updater/hub/`. The `clusters` generator makes one
Application per Argo CD cluster secret that matches the selector; each
renders the chart at the release tag with the shared values, then the
cluster's own file (for example its OCP minor version's oauth-proxy image):

```yaml
apiVersion: argoproj.io/v1alpha1
kind: ApplicationSet
metadata:
  name: rhoai-nightly-updater
  namespace: openshift-gitops
spec:
  goTemplate: true
  goTemplateOptions: ["missingkey=error"]
  generators:
    - clusters:
        selector:
          matchLabels:
            rhoai-nightly-updater: "true"
  template:
    metadata:
      name: 'rhoai-nightly-updater-{{ .name }}'
      namespace: openshift-gitops
    spec:
      project: default
      sources:
        - repoURL: https://github.com/DaoDaoNoCode/rhoai-nightly-updater.git
          targetRevision: v2.2.0
          path: deploy/chart
          helm:
            releaseName: rhoai-nightly-updater
            # values-<cluster>.yaml is optional.
            ignoreMissingValueFiles: true
            valueFiles:
              - $values/apps/rhoai-nightly-updater/values.yaml
              - $values/apps/rhoai-nightly-updater/values-{{ .name }}.yaml
        - repoURL: https://github.com/<org>/<gitops-repo>.git
          targetRevision: HEAD
          ref: values
      destination:
        server: '{{ .server }}'
        namespace: rhoai-nightly-updater
      syncPolicy:
        automated:
          prune: true
          selfHeal: true
        syncOptions:
          - CreateNamespace=true
```

`apps/rhoai-nightly-updater/values-<cluster>.yaml`, for a cluster on another
OCP minor version:

```yaml
parameters:
  OAUTH_PROXY_IMAGE: registry.redhat.io/openshift4/ose-oauth-proxy-rhel9:v4.19@sha256:<digest>
```

To check what a values file renders, without a cluster:

```bash
helm template rhoai-nightly-updater deploy/chart -n rhoai-nightly-updater -f values.yaml
```

## 10. Moving an install from `install.sh` to Argo CD

Use the same `NAMESPACE` and `APP_NAME` and the release the install runs
(or a newer one). Argo CD takes over the existing objects on its first sync
(it applies over them and adds its tracking annotation), the session hook
finds Secret `<APP_NAME>-proxy` and keeps it, so nobody is signed out, and
the ConsoleLink hook takes over the link. From then on, change the install
only in Git; do not run `install.sh` or `make upgrade` on it.

An install from before v1.0.0 (an image on `:latest`, a `COOKIE_SECRET`
environment value, objects named `<APP_NAME>`) needs the cleanup only
`install.sh` does: upgrade it with `install.sh` first
([UPGRADING](UPGRADING.md)), then move it.
