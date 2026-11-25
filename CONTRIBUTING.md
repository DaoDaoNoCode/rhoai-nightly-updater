# Contributing to RHOAI Nightly Updater

This guide covers how to develop, test, and deploy changes to the RHOAI Nightly Updater.

## Prerequisites

### Required Versions

- **Go**: 1.27; `go.mod` names toolchain go1.27.4, the version CI and the Containerfile use, so an older local Go 1.21+ downloads and runs it (`GOTOOLCHAIN=auto`, the default)
- **Node.js**: 26.x, the Active LTS line (CI and the image use `node:26-alpine`); Node 22.22.2+ and 24.15+ also run the tooling (jsdom 31)
- **TypeScript**: 6.0 (installed by `npm ci`)

### Required Tools

- **Container Runtime**: Podman or Docker (auto-detected by `make build`)
- **OpenShift CLI**: `oc` (required for deployment and local dev)
- **golangci-lint v2**: install a release binary (https://golangci-lint.run/docs/welcome/install/local/); CI pins v2.14.0
- **Helm 3.16 or newer**: for `make test-chart` (the Helm chart for Argo CD); CI pins v3.22.1. Without it the chart tests are skipped locally

### Cluster Access

- You must be logged into an OpenShift cluster with `oc login` before running local dev or deploying.
- The local dev server (`dev.sh`) uses your current `oc` token to authenticate with the cluster.

## Local Development

### Quick Start

```bash
./dev.sh
```

This starts two servers:

1. **Go backend** on `127.0.0.1:8080` — connects to your current OpenShift cluster using your `oc` token
2. **Webpack dev server** on `127.0.0.1:9000` — proxies `/api` requests to the backend on `:8080`

Open **http://127.0.0.1:9000** in your browser.

### What `dev.sh` Does

1. Verifies Node.js is installed
2. Verifies you're logged into an OpenShift cluster (`oc whoami -t`)
3. Takes a lock for this checkout (one `dev.sh` per checkout: a second would fight over ports 8080, 9000 and the S3 port-forward). A second copy stops with `ERROR: dev.sh already runs for this checkout (PID <pid>). Stop it first: kill <pid>`; a lock left by a `dev.sh` that was killed hard is taken over
4. Installs frontend dependencies (`npm install`) if `node_modules` is missing
5. Keeps a port-forward to the S3 storage open (`oc port-forward -n seaweedfs svc/seaweedfs-s3 127.0.0.1:${S3_LOCAL_PORT:-18333}`): the backend's one direct S3 request (creating the bucket) goes to the storage Service, whose cluster DNS name does not resolve on your machine. The loop retries until the Service exists and after the port-forward ends, and the backend gets `DEV_S3_ENDPOINT` pointing at it (honoured only in dev mode, loopback only)
6. Starts the Go backend with these environment variables:
   - `KUBERNETES_SERVICE_HOST` — cluster API host (extracted from `oc whoami --show-server`)
   - `KUBERNETES_SERVICE_PORT` — cluster API port
   - `DEV_TOKEN` — your current `oc` token
   - `DEV_USER` — your OpenShift username
   - `DEV_MODE=true` — enables dev mode (no oauth-proxy and no cluster-admin gate: every request uses your token; listens on `127.0.0.1` only; Host/Origin must be this machine). API server TLS is still verified (see [Environment Variables](#environment-variables))
7. Starts the frontend dev server (`npm run dev`) which runs webpack-dev-server with:
   - Hot reload enabled
   - Proxy: `/api` → `http://127.0.0.1:8080` (override with `API_TARGET`)
8. Stops everything it started, with their child processes (the compiled server `go run` leaves, webpack under `npm run dev`, the port-forward), on Ctrl-C, `kill`, a closed terminal (HUP) or its own exit, and removes the lock. After `kill -9`, which skips that, a watchdog that waits for the script's PID stops the backend and the dev server, and the port-forward loop ends by itself

### Frontend Development

**Changing frontend dependencies:** after editing `frontend/package.json`, run `make frontend-lock`. It updates `package-lock.json` with the npm of CI's pinned `node:26-alpine` image (npm 11), so CI's `npm ci` accepts it. npm majors write the lock differently: when CI ran npm 10, it refused locks from a local npm 11 as out of sync (npm 11 leaves out optional peer entries npm 10 expects). Renovate's merge requests: [Dependency updates](#dependency-updates).

The frontend uses:

- **React 19** with TypeScript 6.0
- **PatternFly 6.x** for UI components (`@patternfly/react-core`, `@patternfly/react-table`)
- **React Router 7** (`react-router-dom` ^7.18.6) for client-side routing
- **Webpack 5** for bundling (configured in `frontend/webpack.config.js`)

#### TypeScript Configuration

- **Target**: ES2020
- **Module**: ESNext with `bundler` resolution
- **JSX**: `react-jsx` (automatic runtime, no manual React imports needed)
- **Source maps**: Enabled for debugging

#### Build Commands

```bash
cd frontend
npm run build       # Production build → frontend/dist/
npm run dev         # Dev server on :9000 with hot reload
```

#### Dev Server Proxy

The webpack dev server proxies `/api` requests to the Go backend:

```javascript
// frontend/webpack.config.js
devServer: {
  host: '127.0.0.1',
  proxy: [{ context: ['/api'], target: 'http://127.0.0.1:8080' }],
}
```

## Project Structure

```
rhoai-nightly-updater/
├── main.go                  # HTTP server entry point
├── go.mod                   # Go module definition
├── Makefile                 # Build/test/deploy automation
├── Containerfile            # Multi-stage Docker build
├── dev.sh                   # Local development launcher
├── .env.example             # Environment variable reference
├── pkg/                     # Go backend packages
│   ├── api/                 # HTTP handlers (one <feature>_handlers.go per feature), auth in auth_handlers.go
│   ├── cluster/             # Facade over cluster/internal/: the Kubernetes API client and operations (see Backend layout)
│   ├── middleware/          # HTTP middleware (security, request ID)
│   └── types/               # Shared type definitions
├── frontend/                # React/TypeScript frontend
│   ├── src/                 # Source files
│   │   ├── index.tsx        # React app entry point
│   │   ├── App.tsx          # Root component with routing
│   │   ├── pages/           # Page components
│   │   ├── components/      # Reusable UI components
│   │   └── services/        # API client
│   ├── public/              # Static assets
│   ├── dist/                # Production build output (generated)
│   ├── package.json         # Node dependencies and scripts
│   ├── tsconfig.json        # TypeScript compiler config
│   └── webpack.config.js    # Webpack bundler config
├── deploy/                  # template.yaml (the OpenShift Template) and chart/ (the Helm chart for Argo CD)
├── scripts/                 # Automation scripts
└── docs/                    # Documentation
```

## Backend layout

`pkg/api` (the HTTP handlers) and `main.go` call only `pkg/cluster`, which is a facade over the packages in `pkg/cluster/internal/` plus the orchestrators that combine several of them (status, summary, resources, the components page, the user-token project reads, clean-up). Shared request and response types are in `pkg/types`, the clock interface in `pkg/clock`.

- **Layers.** Each internal package imports only packages below it; none imports `pkg/cluster`. From the bottom: `kube` (the API client, its errors, REST helpers, object and text helpers, the clock, the cache registry) and `report` (the Diagnostics report types); `rhoai` (the product's namespaces, API versions, path builders) and `authz` (user-token checks); `record`, `argo`, `registry`; `catalog`, `components`; `dashdev`, `envkit`, `node`; `mlflow`, `storage`, `operator`; `serving`, `gitops`, `workbench`; `modelregistry`, `kueue`; `dsc`; `features`, `diagnostics`; `observability`; `recipe`. The package doc of `pkg/cluster` (`doc.go`) has the same list with what each package holds. New code goes into the lowest package whose imports it needs; if it needs a higher one, it belongs higher. `TestInternalPackagesImportOnlyLowerLayers` (`pkg/cluster/layers_test.go`) fails on an import of the same or a higher layer, an import of `pkg/cluster`, or a package in no layer, and when this list or `doc.go` differs from its own; a new package goes into all three.
- **The facade rule.** `pkg/cluster/facade_<package>.go` re-exports `internal/<package>` under the names `pkg/api` uses: type aliases, constants, error values (the same values, so `errors.Is` matches either name) and functions. A facade function is a pure forward, `return pkg.F(its parameters in order)`, nothing else: the RBAC scan follows a parameter through such a forward, and a wrapper that computes, reorders or drops an argument is an ordinary caller to it.
- **Names.** Inside a package, names do not repeat the package name (`record.Action`, not `record.RecordAction`); a name nothing outside its package uses stays unexported. File base names are unique across `pkg/cluster/**`: the RBAC scan keys call sites by `file:Function`.
- **Tests.** Most tests stay in `pkg/cluster` (they share its fake clusters) and call the internal packages directly. A test changes an internal package's timing or hook through its `UseTestTimings`/`Set...ForTest` functions, which panic outside a test binary; `TestMain` installs the fake clock (`kube.SetClockForTest`). These hooks set package variables that every test in the binary reads, until the test puts them back, so a test that uses one must not run in parallel with other tests: no `t.Parallel()` in it or its subtests. No Go test calls `t.Parallel()`; `scripts/test-go.sh` runs the packages, and the parts of a large one, as separate processes instead.

**The RBAC scan and its golden file.** `pkg/cluster/rbac_coverage_test.go` reads the source of every package under `pkg/cluster` and derives each API request the code can make (verb, API group, resource, namespace, name); the RBAC tests check them against `deploy/template.yaml` and `deploy/rbac-gitops.yaml`. `TestRBACRequestsGolden` freezes that list in `pkg/cluster/testdata/rbac-requests.golden`. A change that only moves or rewrites code (a refactor, a new helper) must leave the golden byte for byte the same; if it differs, the move changed a request or the scanner lost track of one, so find out which before anything else. A change that is meant to request something new or different regenerates it (`go test ./pkg/cluster -run TestRBACRequestsGolden -update`); review the golden's diff like the code (every added line is something the ServiceAccount or the user's token now asks for, and it needs its rule in the template and in [SECURITY.md](SECURITY.md)) and commit it with the change.

**Where `*` comes from in the scan.** A namespace or name the scan cannot pin down becomes `*` (any). A parameter is resolved from the function's call sites in the scanned source. When the function also has a caller the scan cannot list, the result keeps the listed callers' values and adds `*`. Such callers are: a call from `pkg/api` or `main.go` (not scanned); a use of the function as a value (stored, returned, passed along, or put in a package-level table or initializer); and, for a function behind a facade forward, any of those on the facade. So handing a function around as a value, or calling a `pkg/cluster` function from a new handler, can add `*` to its requests. Usually the golden then only shows higher counts on rows it already has. A new `*` row is a new request like any other: grant it or narrow the code.

**Refactors keep behaviour.** The 5.0.0 restructuring of `pkg/cluster` into these packages was made in steps, each checked before it was merged: lint clean, `pkg/api` and `main.go` unchanged, the RBAC golden and the frontend contract fixtures byte-identical, the same test names, the race tests no slower, per-function coverage not lower, no error text gone. A later refactor of the backend should hold to the same: the golden, `TestFrontendContract` and the test list are the quick evidence that nothing a user or the cluster sees changed.

## Running Tests

### All Tests

```bash
make test              # Go, frontend, release/install script and chart tests
```

### Go Tests

```bash
make test-go           # Runs: scripts/test-go.sh (go test -race -count=1 ./..., the test binaries side by side, one more than the CPUs at a time (a race test binary idles for a second before it exits), and a package with 200+ tests split into parts; GO_TEST_JOBS, GO_TEST_SHARDS=1 for no parts, GO_TEST_PART_TESTS; test flags such as -shuffle=on as arguments)
```

**Test patterns:**

- Tests use fast timing constants instead of production timeouts
- `TestMain` overrides package-level timing constants to run quickly without changing production behavior
- The shared fake API servers of `pkg/cluster` (`newResourceFake`, `newFakeAPI`, `newMockClient`, ...) are `newFakeServer`/`newFakePlainServer` (`fake_server_test.go`): real net/http servers and clients over an in-memory connection, without TLS or loopback sockets. `CLUSTER_FAKES_LOOPBACK=1 go test ./pkg/cluster` puts them back on `httptest`'s TLS and loopback servers, to compare. Tests of TLS itself keep `httptest.NewTLSServer`

### Frontend Tests

```bash
make test-frontend     # Runs: cd frontend && npm test
```

Runs Vitest over `frontend/src/**/*.test.ts[x]` in jsdom. No browser needed.

### Script and chart tests

```bash
make test-scripts      # scripts/test-release.sh, test-install.sh, test-node-pull-secret.sh and test-test-go.sh in parallel (offline, fake oc/curl/crane; test-node-pull-secret.sh needs jq, test-test-go.sh go; the summary names what was skipped)
make test-chart        # scripts/test-chart.sh: helm lint and helm template of deploy/chart (offline)
make check-chart       # only: the chart's copies of deploy/template.yaml and deploy/rbac-gitops.yaml are current
```

What a GitLab test job runs, locally (with your tools; the jobs' versions are in `.gitlab-ci.yml`): `make ci-go`, `make -k ci-checks`, `make -k ci-script-tests` (needs jq and helm), `make lint-go` (golangci-lint), `make test-frontend`, `make build-frontend`, and `make release-check TAG=vX.Y.Z` for a tag's release guard.

The Helm chart (`deploy/chart`, [docs/GITOPS.md](docs/GITOPS.md)) renders copies of `deploy/template.yaml` and `deploy/rbac-gitops.yaml` (the gitops profile's RBAC and admission policy), because Helm reads only files inside the chart. After any change to either run `make chart` and commit the copies with it; `make check-chart`, `make test-chart`, CI and the release guard fail otherwise. A new API route is off in the gitops profile until it is added to `gitOpsRoutes` in `pkg/api/routes.go` (or listed in `gitOpsRefused`; `TestEveryRouteHasAGitOpsProfileDecision` fails for a route in neither); adding it there can widen what the profile reaches, and `pkg/cluster/rbac_gitops_test.go` then names the rules `deploy/rbac-gitops.yaml` must gain. `scripts/test-chart.sh` checks that every Template object renders exactly once with its parameters replaced, that the schema lists exactly the Template's parameters, that unknown keys, missing or tag-only images fail, and that the ConsoleLink hook applies the same object as `install.sh`.

## Documentation images

Screenshots and GIFs in `docs/images` come from a mock backend with made-up
data, never from a cluster (the GitHub mirror is public). After a UI change,
regenerate them and check the docs:

```bash
make docs-screenshots   # every image and GIF (playwright-cli, ffmpeg, ImageMagick)
make docs-check         # links, anchors, image paths; every image used
make docs-fixtures      # after a Diagnostics change: the mock's answers, from the test fakes
```

How it works and the image conventions: [docs/tools/README.md](docs/tools/README.md).

## Linting

### All Linters

```bash
make lint              # Run both Go and frontend linters
```

### Go Linter

```bash
make lint-go           # Runs: gofmt -l (must be empty), go vet ./..., golangci-lint run ./...
```

Requires golangci-lint v2 (configuration in `.golangci.yml`), installed from the release binaries: https://golangci-lint.run/docs/welcome/install/local/. `make vuln` runs govulncheck and `npm audit --omit=dev --audit-level=high`.

### Frontend Linter

```bash
make lint-frontend     # Runs: cd frontend && npm run typecheck && npm run lint (ESLint)
```

ESLint reads the TypeScript types for `src/` (`frontend/eslint.config.mjs`): a promise must be awaited, caught or marked `void`. The async handlers catch their own errors and show them, so a button calls one as `onClick={() => void handleSave()}`; a new async handler must do the same (catch, and show or log what fails) before it is called that way.

## Building

### Local Build (Containerfile)

```bash
make build             # Build using podman or docker (auto-detected)
```

**What happens**:

1. Auto-detects container runtime (`podman` or `docker`)
2. Builds a multi-stage image from `Containerfile`:
   - **Stage 1 (frontend)**: `node:26-alpine`
     - Installs dependencies: `npm ci`
     - Type-checks, lints and tests: `npm run typecheck && npm run lint && npm test` (skipped with `--build-arg RUN_CHECKS=false`, which only GitLab's image jobs pass: their pipeline's test jobs have run these checks on the same commit)
     - Builds production bundle: `npm run build` → `frontend/dist/`
     - Uses `NODE_OPTIONS="--max-old-space-size=2048"` to handle large bundles
   - **Stage 2 (backend)**: `golang:1.27.4-alpine`
     - Downloads Go modules: `go mod download`
     - Vets and tests: `go vet ./... && go test ./...` (also skipped with `RUN_CHECKS=false`)
     - Builds a static binary with the version, commit and build date linked in
   - **Stage 3 (runtime)**: `registry.access.redhat.com/ubi9/ubi-minimal` 9.8 (all base images pinned by digest)
     - Sets the `org.opencontainers.image.revision` label to the commit (`make deploy`/`upgrade` check it)
     - Copies compiled Go server from backend stage
     - Copies frontend static files from frontend stage
     - Sets `STATIC_DIR=/app/static`, `PORT=8080`
     - Runs as non-root user `1001`
3. Tags the image as `IMAGE:dev-<GIT_SHA>` (`dev-<GIT_SHA>-dirty` when the tree has uncommitted changes), plus `IMAGE:TAG` when `TAG` is a test tag of your own (never `latest`, `main`, `vN`, `vX.Y.Z` or a commit-like 7 to 40 hex characters: CI publishes those). A local build never takes the bare commit tag that CI writes once

**Build variables** (override via environment, command line, or `.env` file):

- `IMAGE` — Registry and image name (default: `quay.io/juntao_wang/rhoai-nightly-updater`)
- `TAG` — Image tag (default: the release tag HEAD is on, else none). `deploy`/`upgrade` install this tag
- `BUILD_VERSION` — The version the binary reports (default: the release tag HEAD is on, else `GIT_SHA`)
- `GIT_SHA` — the commit in local build tags (default: the first 8 characters of the HEAD commit, the same as GitLab's `CI_COMMIT_SHORT_SHA`); local builds are tagged `dev-<GIT_SHA>`
- `RUNTIME` — Container runtime (default: auto-detected `podman` or `docker`)
- `PLATFORM` — Target platform (default: `linux/amd64`)
- `NO_CACHE` — Set to `1` to skip cache and force rebuild

**Examples**:

```bash
make build IMAGE=quay.io/myorg/myapp        # Custom registry
make build RUNTIME=docker                   # Force docker instead of podman
make build NO_CACHE=1                       # Force full rebuild
```

### Push to Registry

```bash
make push              # Push IMAGE:dev-<GIT_SHA> (and IMAGE:TAG for a test tag of your own); refuses a tree with uncommitted changes
```

Requires prior `docker login` or `podman login` to the registry. To test your build on a cluster: commit, `make build push`, then `make upgrade TAG=dev-<GIT_SHA>` (or `make all`, which deploys `:dev-<GIT_SHA>`). `make push` refuses a tree with uncommitted changes (untracked files too): the image's revision label names HEAD, and `make deploy`/`upgrade` pick the template by it. For Docker, set `RUNTIME=docker`.

### Automatic Image Publishing

GitLab CI (`.gitlab-ci.yml`) runs on merge requests, branch pushes and release tags (`vX.Y.Z`, and pre-releases `vX.Y.Z-alpha.N`, `-beta.N`, `-rc.N`; other tags run nothing):

- **test** stage, all must pass, no lint exceptions:
  - `go` (`make ci-go`): gofmt, `go mod verify`, `go vet`, the race tests (`scripts/test-go.sh`: `go test -race -count=1` over every package, the test binaries run side by side, one more than the CPUs at a time, and a package with 200 or more tests split into parts of about 200, at most one per CPU; the parts start while the other test binaries build), govulncheck. GitHub's `go` job runs the same target;
  - `golangci-lint`;
  - `checks` (`make -k ci-checks`): the frontend's typecheck, ESLint and `npm audit`; `make docs-check`; shellcheck 0.11.0 on `scripts/*.sh`, `dev.sh`, `docs/tools/*.sh` and `pkg/cluster/internal/rhoai/node_pull_secret.sh` (as bash: it is a pasted command, so it carries no shell directive; `make shellcheck`). GitHub runs the same in its `frontend-checks` and `docs` jobs, with the shellcheck release tarball checked against `SHELLCHECK_SHA256`;
  - `frontend-test` (tests) and `frontend-build` (production build), in parallel with `checks`, each after its own `npm ci` from one npm cache;
  - `script-tests` (`make -k ci-script-tests`): `scripts/test-release.sh`, `scripts/test-install.sh`, `scripts/test-node-pull-secret.sh` and `scripts/test-test-go.sh` (`scripts/test-go.sh` on a small module of its own) at the same time, then `scripts/test-chart.sh`. The job installs Debian's `jq` 1.7.1 into the Go image and Helm `HELM_VERSION` from get.helm.sh, checked against `HELM_SHA256` (both in `.gitlab-ci.yml` and `.github/workflows/ci.yml`; change them together); the target sets `REQUIRE_JQ=1` and `REQUIRE_HELM=1`, so a missing tool fails instead of skipping tests;
  - `release-guard` (tags only, `make release-check TAG=vX.Y.Z`): `scripts/release.sh check`, see [Releases](#releases);
  - the docs, chart and script checks share the `checks` and `script-tests` jobs because each job waits about two minutes for its runner VM, and those checks take seconds; `make -k` runs every part and names each one that failed;
  - the Go jobs keep Go's module and build caches (and golangci-lint's) in `.cache/`, one GitLab cache per job keyed on `go.sum`; tag pipelines only read them (main's pipeline of the commit wrote them).
  - Job timeouts: 20 minutes for the test jobs, 30 for the image builds, 10 for the crane, installer and release-notes jobs (GitHub: 20 minutes per job, 45 for the release job).
- **Image jobs**: `build-main` and `release-build` start with the test jobs and only build a tarball; the publish jobs (`publish-main`, `release-publish`) `need` every test job of the pipeline (on a tag also `release-guard`) and the build, so nothing is pushed unless all of them passed. A failed test still leaves that pipeline's tarball as a one-day artifact, never pushed. `build-branch`, which pushes from its own job, still waits for the test jobs. They build with `RUN_CHECKS=false`, so the Containerfile does not run those checks a second time.
- **main**: `build-main` builds the image to a tarball; `publish-main` pushes it as the write-once `:$CI_COMMIT_SHORT_SHA` (8 characters) and as `:main`, the latter only while the commit is still the tip of `main`. `:main` is always the pipeline's own image. An existing commit tag is kept only when it is that same image, or the release image of the same commit (a tag pipeline that published first: its `:vX.Y.Z` has the same digest); anything else (another commit, an earlier build, a workstation push) stops the job before `:main` moves. Delete such a tag in Quay after checking it, then retry the job. It never touches `:latest`.
- **Other branches**: the manual `build-branch` job builds the image. It pushes it as `$BRANCH_IMAGE:$CI_COMMIT_REF_SLUG` only when the project has the variables `BRANCH_IMAGE`, `QUAY_BRANCH_USER` and `QUAY_BRANCH_TOKEN` (below); without them it builds and pushes nothing. It never uses `QUAY_USER`/`QUAY_TOKEN`.
- **Release tags**: `release-build` builds the image (`VERSION=<tag>`) to a tarball; `release-installer` generates `install.sh`; `release-publish` pushes `:vX.Y.Z` and moves `:vN` and `:latest` (every tag-writing job, main or release, holds the one resource group `publish-latest`); `release-notes` creates the GitLab Release. `promote-latest` is manual. A pre-release tag runs the same jobs, but `release-publish` pushes only `:vX.Y.Z-rc.N` (and the commit tag when new), never `:vN` or `:latest`, and there is no `promote-latest` job ([Pre-releases](#pre-releases)).
- The registry comes from the `IMAGE` variable (a project CI/CD variable overrides the default) with `QUAY_USER`/`QUAY_TOKEN` as its credentials. Only the crane jobs (`publish-main`, `release-publish`, `promote-latest`) read them, and those run only on `main` and on release tags. The image jobs never log in with them: `build-main` and `release-build` build a tarball with no registry login (except the Dependency Proxy, when `DOCKER_HUB` points at it) and the crane job pushes it.
- The image jobs build with buildah (`scripts/ci-build-image.sh`; `BUILDAH_IMAGE` pins `quay.io/buildah/stable` by digest): chroot isolation, vfs storage, `linux/amd64`, Docker image format, no layer cache. The Containerfile's `RUN` steps (`npm ci` with its packages' install scripts, `go mod download`) run chrooted with buildah's default capabilities: they do not see the job's registry logins, and reading the environments of the job's processes is refused; on a privileged runner they can read the host's block devices (what they can and cannot see: [SECURITY.md](SECURITY.md#where-the-updaters-image-comes-from-trust-root)).
- **Which runner builds the image.** The image jobs have no tags. GitLab.com's hosted runners are turned off for this project (project API: `shared_runners_enabled: false`), and the job logs show the `redhat/ai` group runner (`on pe-runners …`, executor `docker+machine`; it has the tag `aipcc` and takes untagged jobs). Whether it runs jobs privileged is set by its owners and is not visible to this project; `ci-build-image.sh` prints the job's effective capabilities (`Job capabilities: CapEff: …`) and the block devices in `/dev`, so the build log tells (in Podman: privileged `000001ffffffffff` and the host's disks; not privileged `00000000800405fb` and `none`). buildah needs a privileged container, or `seccomp=unconfined` plus `label=disable` (tested with buildah v1.46.1 in Podman 5.2.5, SELinux enforcing, 2026-11-19). With Docker's default seccomp profile (moby 29.10.0's) it stops as it starts, `Error during unshare(CLONE_NEWUSER): Operation not permitted`; with SELinux labels it stops at the first `RUN`, `masking non-directory ".../proc/interrupts" in mount namespace: permission denied`. Either way nothing is pushed: if the runner changes to one that allows neither, the image jobs fail, safely. A host with AppArmor was not tested. kaniko, used before 4.0.0, ran without these settings but is archived upstream.

#### GitLab and Quay settings

The image tags are only as safe as the credentials that write them. Set these by hand (a Maintainer of the GitLab project, and the owner of the Quay repository):

| Where | Setting | Why |
|---|---|---|
| GitLab, Settings > CI/CD > Variables | `QUAY_USER` and `QUAY_TOKEN`: **Protect variable** and **Mask variable** on (Visibility: Masked) | Protected variables reach only pipelines of protected branches and tags, so a pipeline of any other branch (whoever can push one can change `.gitlab-ci.yml`) cannot read the token that pushes `:main`, `:latest` and the release tags |
| GitLab, Settings > CI/CD > Variables | `QUAY_USER` and `QUAY_TOKEN`: **Environment scope** `quay-publish` (and `RENOVATE_TOKEN`, `GITHUB_COM_TOKEN`: `renovate`) | Only the jobs that declare that environment (the crane jobs: `publish-main`, `release-publish`, `promote-latest`; the `renovate` job) receive the tokens; the test and image jobs, whose `npm ci` runs packages' install scripts, do not. **Order:** merge 4.0.0 first; let the first `main` pipeline's `publish-main` succeed; then set the scope. Before 4.0.0 the crane jobs declare no environment (`main`'s `.crane`), so with the scope set they get no `QUAY_*` and fail. After setting it, check that the next `publish-main` **pushes** (`Pushed …:<commit> (sha256:…)` in its log), not only that it logs in: `crane auth login` only writes the credentials to a file (go-containerregistry `cmd/crane/cmd/auth.go`). From then on, do not release from a commit before 4.0.0: its tag pipeline runs its own `.gitlab-ci.yml`, without the environment |
| GitLab, Settings > Repository > Protected branches | `main` protected (allowed to push and merge: Maintainers) | `publish-main` runs on every `main` pipeline with the push token |
| GitLab, Settings > Repository > Protected tags | `v*`: allowed to create **Maintainers** only | A release tag pipeline gets the protected variables and publishes `:vX.Y.Z`, `:vN` and `:latest`; without the protection anyone who can push a tag could publish a release |
| Quay, the repository's robot accounts | `QUAY_USER`/`QUAY_TOKEN` a robot account with **Write** on the `IMAGE` repository only (not the user's own password or an organisation-wide token) | Limits what a leaked token can overwrite |
| Quay, the repository's Settings | **Not available today.** Tag immutability for `v[0-9]+\.[0-9]+\.[0-9]+` (an immutability policy) needs a Quay organisation: "Immutable tags are not available for personal user namespaces" (quay-docs `modules/immutable-tag-overview.adoc`), and `IMAGE` is in the personal namespace `juntao_wang`. After [moving the repository](#moving-the-image-repository) to an organisation, set it there | `:vX.Y.Z` then could not be overwritten even with the push token; until then only `release.sh publish` refuses to |
| Optional: `build-branch` | `BRANCH_IMAGE` (a repository of its own, for example `quay.io/<you>/rhoai-nightly-updater-dev`), `QUAY_BRANCH_USER`, `QUAY_BRANCH_TOKEN` (masked, not protected: a robot with Write on that repository only) | Branch builds then push to that repository; the job refuses a `BRANCH_IMAGE` equal to `IMAGE`. Unset, it only builds |

GitHub Actions (`.github/workflows/ci.yml`) runs the same test-stage checks on pushes to `main` and on pull requests; `release.yml` runs them on release tags and creates the GitHub Release. Nothing is published to a registry from GitHub.

Teammates install releases, so a broken `main` only reaches people who test `:main` on purpose.

### Dependency updates

`renovate.json5` configures [Renovate](https://docs.renovatebot.com/): merge requests every Monday before 6am (New York time) for Go modules (`go.mod`'s toolchain line moves, its `go` line stays), npm (`frontend/`), the container images of the Containerfile and of `.gitlab-ci.yml`'s `*_IMAGE` variables (tag and digest; digest-only references get digest updates), and the GitHub Actions (by commit SHA). Minor and patch updates are grouped (Go modules, npm dependencies, container image digests, GitHub Actions); the Go toolchain (go.mod, the golang images, the workflows' `go-version`) and golangci-lint (image and workflow version) each move in one merge request; major updates come one by one. Nothing is merged automatically; every merge request runs the normal pipeline. npm versions wait 3 days before a merge request. Labels: `dependencies`, plus `go`, `frontend` or `ci`.

**The frontend lock.** Renovate updates `frontend/package-lock.json` with npm 11 (`constraints` in `renovate.json5`), the npm major of CI's `node:26-alpine` image, which is what `make frontend-lock` uses. If CI's `npm ci` still refuses a Renovate lock, check out the branch, run `make frontend-lock` and push. (Renovate's `postUpgradeTasks` could run that npm command itself, but only on a self-hosted Renovate whose admin allows the command, never on the Mend app; the version constraint covers both.) A Node major update (for example `node:28-alpine`) brings another npm: change the constraint in the same merge request.

Not covered: the images the backend deploys on clusters (`pkg/cluster/*.go` constants such as SeaweedFS and rclone), Helm and shellcheck versions with their SHA-256s, and the chart's example `ose-cli` image (its tag follows the cluster's OCP version). Update those by hand.

Renovate needs a host. Pick one:

| | Mend Renovate app | Scheduled self-hosted job (`renovate` in `.gitlab-ci.yml`) |
|---|---|---|
| Setup | The hosted app for GitLab.com, through the [Mend Developer Platform](https://developer.mend.io) ([Renovate: Hosted GitLab.com App](https://docs.renovatebot.com/getting-started/installing-onboarding/#hosted-gitlabcom-app)). It installs on a **group** only, not a single project: someone with that right on the group (and the organisation's approval for a third-party app with write access) installs it and selects this project | A bot token, two variables and a pipeline schedule (below), by a Maintainer of this project |
| Runs as | Mend's bot, on Mend's infrastructure | This project's pipeline, with this project's bot token |
| `postUpgradeTasks` | not available | possible (not used) |
| Note | Create no pipeline schedule with `RENOVATE_RUN`, so the `renovate` job never runs: never run both | Nothing outside this project |

The self-hosted job, step by step (a Maintainer):

1. **Bot token.** Settings > Access tokens: a project access token, role **Developer** (it pushes `renovate/*` branches and opens merge requests; it cannot push to the protected `main`), scopes `api` and `write_repository`, with an expiry date (a year at most; put a reminder in the calendar).
2. **Variables.** Settings > CI/CD > Variables: `RENOVATE_TOKEN` = that token, **Protect variable** and **Mask variable** on (Visibility: Masked and hidden), **Environment scope** `renovate`. Optional: `GITHUB_COM_TOKEN` (Renovate reads it also as `RENOVATE_GITHUB_COM_TOKEN`: `lib/workers/global/config/parse/env.ts:333` at 44.254.1), a GitHub personal access token with no scopes (read-only public data), protected, masked, scope `renovate`: without it Renovate skips the GitHub Actions, the workflows' Go and Node versions and golangci-lint's workflow version (GitHub's API refuses anonymous lookups at this rate), and its merge requests carry no release notes.
3. **Schedule.** Build > Pipeline schedules > New schedule: target branch `main` (protected, so the pipeline gets the protected token), for example `0 9 * * 1` (Mondays), with the variable `RENOVATE_RUN` = `1` in the schedule. The job's rule tests `RENOVATE_RUN`, not the token: GitLab's docs say "You should not use environment-scoped variables with `rules` … The variables might not be defined when GitLab validates the pipeline configuration at pipeline creation" (Environments, "Limit the environment scope of a CI/CD variable"). The job stops with a message when `RENOVATE_TOKEN` does not reach it. A schedule pipeline runs the `renovate` job only; the test, image and publish jobs skip schedules. Renovate's own `schedule` in `renovate.json5` still decides which updates it opens, so a more frequent pipeline only rebases and refreshes.
4. **First run.** Run the schedule once by hand (Play). The job log lists the dependencies found and the branches it would create; a dependency dashboard issue appears in the project's issues.

Protected variables reach every job of a `main` pipeline, not just the job that reads them: in a push pipeline on `main`, `RENOVATE_TOKEN` is in the environment of the test jobs too (their `npm ci` runs the packages' install scripts), as `QUAY_TOKEN` is. A token with Developer role on this project only, with an expiry, limits what a leak can do.

## Releases

Version numbers (`vMAJOR.MINOR.PATCH`):

- **MAJOR**: the deploy contract changed: `TEMPLATE_REVISION` in `deploy/template.yaml` (with `api.ExpectedTemplateRevision`), RBAC, or anything else that needs the template re-applied. Upgrading needs a full redeploy.
- **MINOR**: features with the same template. **PATCH**: fixes.
- Pre-releases (`vX.Y.Z-rc.N`, ...) iterate a version before its release: [Pre-releases](#pre-releases).

Image tags:

| Tag | Moves | Set by |
|---|---|---|
| `:vX.Y.Z` | never (refused if it exists from another commit) | the tag pipeline |
| `:vX.Y.Z-rc.N` (`-alpha.N`, `-beta.N`) | never, as `:vX.Y.Z` | the pre-release's tag pipeline ([Pre-releases](#pre-releases)) |
| `:vN` | to the newest release of major N | the tag pipeline |
| `:latest` | to a newer release of the **same** major only; an unlabelled `:latest` (from before releases) counts as v1 | the tag pipeline; across majors only the manual `promote-latest` job |
| `:main` | to every new tip of `main` | `publish-main` |
| `:<8-char commit>` | never: written once, by whichever pipeline publishes the commit first | `publish-main` or the tag pipeline (`make push` writes `:dev-<commit>`, never this) |

How to cut a release:

1. On `main`, change the top `CHANGELOG.md` section from `## [X.Y.Z] - unreleased` to `## [X.Y.Z] - YYYY-MM-DD` (today) in a commit; merge it.
2. With `main` checked out, clean and up to date:
   ```bash
   make release VERSION=vX.Y.Z
   ```
   It checks the branch, the tree, `origin/main`, the CHANGELOG section and the template guard, creates an annotated tag and prints the push commands. Nothing is pushed.
3. Push the tag to both remotes, as printed: `git push origin vX.Y.Z` (GitLab: images, `install.sh`, GitLab Release) and `git push github vX.Y.Z` (GitHub Release).
4. Watch the tag pipeline. For a new MAJOR, `:latest` stays on the old major; when teammates have been told, run the manual `promote-latest` job of that pipeline.
5. Start the next section: `## [X.Y.Z+1] - unreleased` (or the next planned version), and set `version` and `appVersion` in `deploy/chart/Chart.yaml` to that version (`X.Y.Z+1`, `"vX.Y.Z+1"`) in the same commit. If the plan changes, change both again.

The guard (`scripts/release.sh check`, the same in `make release` and CI) fails when the tag is not the top dated CHANGELOG section, when `deploy/chart/Chart.yaml` does not name the release or its copy of the template is not `deploy/template.yaml` (`make test-chart` warns about both earlier), or when `TEMPLATE_REVISION` differs from the previous release tag's (the highest lower `vX.Y.Z` tag; pre-release tags do not count) and the MAJOR is not higher. A MAJOR bump without a template change is allowed. Retrying a release pipeline is safe: an image already published from the same commit is kept; one from another commit stops the job.

### Pre-releases

Every template change needs a new MAJOR, so releasing each iteration of a template change burns a major each time (1.0.0 on 2026-10-06 to 4.0.0 on 2026-11-20). Iterate with pre-releases instead and release the version once: `vX.Y.Z-alpha.N`, `vX.Y.Z-beta.N` or `vX.Y.Z-rc.N` (N from 1, no leading zeros; no other identifiers), ordered alpha < beta < rc < `vX.Y.Z` ([SemVer 2.0.0](https://semver.org/spec/v2.0.0.html), items 9 and 11). Use them to test a version on a cluster before telling teammates about it, typically while a new major is worked on in an `integ/*` branch.

1. On `main` or the `integ/*` branch, the top `CHANGELOG.md` section is the version's, still undated: `## [X.Y.Z] - unreleased`. Pre-releases get no section of their own; their notes are that section so far.
2. In a commit, set `deploy/chart/Chart.yaml` to the pre-release itself: `version: X.Y.Z-rc.1`, `appVersion: "vX.Y.Z-rc.1"` (Helm takes SemVer 2 versions). The chart's tag check then takes only that pre-release's image: the templates of two pre-releases may differ.
3. With that branch checked out, clean and pushed: `make release VERSION=vX.Y.Z-rc.1`, then push the tag to both remotes as printed.
4. For the release: date the CHANGELOG section and set `Chart.yaml` back to `X.Y.Z` / `"vX.Y.Z"` in one commit on `main`, then `make release VERSION=vX.Y.Z` as above.

The guard checks a pre-release as a release, except that its section must be undated, X.Y.Z must be higher than every release tag (no pre-release of a version that is out, or of an older line), and the template rule compares with the last release only: the pre-releases of one version may change the template among themselves, and the release is checked against the previous release as always.

What a pre-release publishes, and what never moves:

- `:vX.Y.Z-rc.N`, written once, and the 8-character commit tag when no pipeline wrote it yet. **Never** `:vN` or `:latest`; `promote-latest` refuses a pre-release, and its pipeline has no such job.
- `install.sh`, which installs exactly that pre-release (its template and its image), and from a clone `git checkout vX.Y.Z-rc.N && make upgrade`; `make rollback TAG=vX.Y.Z-rc.N` works as for a release.
- A GitHub Release marked as a pre-release and not as "Latest" (`gh release create --prerelease --latest=false`): `releases/latest`, which the README's install command and other tools download, is "the most recent non-prerelease, non-draft release" (GitHub REST API, "Get the latest release"). GitLab Releases have no pre-release flag, so it is named `vX.Y.Z-rc.N (pre-release)` and its notes start by saying so; GitLab's `/-/releases/permalink/latest` follows the release date and can point at it (nothing here uses that link).
- The app's update check never offers a pre-release. An updater that runs one is offered the newest release once it is X.Y.Z or higher ("requires a full redeploy" when the MAJOR differs).

### Moving the image repository

The default repository, `quay.io/juntao_wang/rhoai-nightly-updater`, is a personal Quay namespace, kept because the published images live there. Everything that uses it at run time takes another one:

| Where | Setting |
|---|---|
| GitLab CI (builds, releases, the embedded repo of `install.sh`) | project CI/CD variable `IMAGE`, with `QUAY_USER`/`QUAY_TOKEN` for its registry (the registry host comes from `IMAGE`) |
| GitHub release (notes, `install.sh`, the image wait) | repository variable `IMAGE` |
| `make` | `IMAGE=...` (or `.env`) |
| `install.sh` | `--image-repo` or `IMAGE` |
| The app's update check | template parameter `IMAGE_REPOSITORY` (`install.sh` and `make` pass the repository they install from); `RELEASES_URL` for the notes link; the container variable `INSTALLER_RELEASES_URL` for the installer download ([README: Configuration](README.md#configuration)) |

To move for good:
1. Copy the release tags to the new repository (`:vX.Y.Z`, `:vN`, `:latest`) so existing references keep resolving.
2. Set the CI variables.
3. Change the defaults in `Makefile`, `scripts/install.sh` (`DEFAULT_IMAGE`), `deploy/template.yaml` (`IMAGE_REPOSITORY`, then `make chart`), `.gitlab-ci.yml` and `.github/workflows/release.yml` in one commit.

Installs of older releases keep checking the old repository for updates, so keep publishing there until they upgrade, or leave it in place as a mirror.

## Code Patterns

### Backend Patterns

#### 1. HTTP Handlers with Authentication

All API handlers use one of two wrapper functions:

**`withAuth`** — For read-only endpoints (GET requests):

```go
// From pkg/api/update_handlers.go
var HandleStatus = withAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
    // Handler receives an authenticated cluster.Client
    // Use c.get(), c.post(), etc. to call Kubernetes APIs
})
```

**`withMutationAuth`** — For mutation endpoints (POST/PUT/DELETE):

```go
var HandleUpdate = withMutationAuth(func(c *cluster.Client, w http.ResponseWriter, r *http.Request) {
    // The wrapper has checked cluster-admin and taken the per-user rate-limit slot (30s window).
    // A cluster-changing handler then calls lockCluster: this pod's lock, then the cross-pod
    // lease in the operation ConfigMap (pkg/api/operation_lease.go); 409 cluster_busy if either is taken.
})
```

**What the wrappers do**:

1. Take the user token from `X-Forwarded-Access-Token` (set by oauth-proxy). A `Bearer` header is refused outside `DEV_MODE`
2. Resolve the user from that token (`users/~`); `X-Forwarded-User` is not trusted
3. Create the `cluster.Client` and call the handler: `withAuth` (reads) with the ServiceAccount token (or `DEV_TOKEN` in dev mode); `withMutationAuth` (changes) with the **user's own token** (`cluster.NewUserClient`), plus a ServiceAccount client for the app's own records (`Client.bookkeeping()`; every request to the updater's ConfigMaps must go through it, `TestOwnConfigMapsAreReachedOnlyThroughBookkeeping`). A change must never use the ServiceAccount's token (`getClusterToken`, `genAIToken`, `cluster.NewClientWithContext`): `TestMutationPathsActWithTheUsersToken` fails. A new read that needs a rule goes into `deploy/template.yaml` (the RBAC tests list it); a new change needs none, but the user must have the right
4. (`withMutationAuth` only) Run a cluster-admin SubjectAccessReview with the user's token, check that the sign-in can carry the change (`sessionProblem`: scope `user:full`, not expiring within the deadline), take the per-user/endpoint 30 s rate-limit slot (given back when the change did not go through and its client sent nothing that may have changed the cluster: `cluster.TrackChanges`; an HTTP call to a service other than the API server that may write calls `kube.NoteRequest(req)` before it is sent, which `TestEveryWritingHTTPCallIsRecorded` checks), refuse new work while shutting down, and give the handler a browser-independent context with a 15-minute deadline. Cluster-changing handlers also take the cluster lock (`lockCluster`, 409 `cluster_busy`). `TestEveryNonGetRouteIsGated` fails for a non-GET route without this wrapper. Routes are listed in `pkg/api/routes.go`

#### 2. Kubernetes API Client

All Kubernetes API calls use a **raw HTTP client** (`pkg/cluster/internal/kube/client.go`) instead of the official Kubernetes client library.

**Why**: Avoid dependency bloat, faster builds, simpler debugging.

**Pattern**:

```go
// From pkg/cluster/internal/kube/client.go: Get, Post, Put, Patch, Delete
// and Apply all go through Do.
func (c *Client) Do(method, path, contentType string, body []byte, query url.Values) ([]byte, int, error) {
    req, err := http.NewRequestWithContext(c.ctx, method, c.baseURL+path, reader)
    // ... the bearer token, the response size limit; errors as *K8sError
    // (the API server's answer) or *NetworkError (no answer)
}
```

The REST helpers in `pkg/cluster/internal/kube/rest.go` (`GetInto`, `ListInto`, `GetOptional`, `DeleteIfExists`, `PatchJSON`, ...) cover the common shapes; `kube.IsK8sNotFound(err)` and `kube.IsK8sConflict(err)` test for a 404 or 409.

**Path builders**:

- `rhoai.NamespacedPath(apiGroup, resource, namespace, name)` — for namespaced resources (Pods, Deployments, Subscriptions)
- `rhoai.ClusterPath(apiGroup, resource, name)` — for cluster-scoped resources (ClusterVersions, Nodes)

The RBAC scan resolves these two by name to the request they build, so build paths with them rather than by hand.

**Example**:

```go
path := rhoai.NamespacedPath("operators.coreos.com/v1alpha1", "subscriptions", "ns", "sub-name")
// Returns: /apis/operators.coreos.com/v1alpha1/namespaces/ns/subscriptions/sub-name
```

**TLS Configuration**:

- `KUBE_CA_FILE` if set, else the in-cluster CA (`/var/run/secrets/kubernetes.io/serviceaccount/ca.crt`), else the system trust store
- Verification is skipped only with `DEV_MODE=true` and `DEV_INSECURE_TLS=true`
- Shared `http.Transport` across all clients (CA loaded once at init)

#### 3. Test Patterns

No test waits in real time. Every wait, poll, retry and periodic job takes its time from an injected clock (`kube.Clock()` in `pkg/cluster` and its internal packages, `clk` in `pkg/api`, `clock.Real` in production); tests use `pkg/clock/clocktest`: `pkg/cluster`'s `TestMain` installs a fake clock that advances when the code sleeps, and `pkg/api`'s `resetPackageState` gives each test its own fake clock, which the test advances (`testClock.Advance`) and synchronises with the code under test through its fakes (for example `fakeRecord.beat`, which advances one heartbeat interval and waits with `testClock.WaitForTicksHandled` until the heartbeat handled the tick: made its lease write and decided on it; waiting only for the write is not enough when the code reads the clock after it). Do not add `time.Sleep`, polling loops or "it would have run by now" waits to tests; a `time.After` may only end a test that hangs.

#### 4. Errors and operation results

A function that returns `(*types.OperationResponse, error)` uses the two results for different things, never both:

- **`Success: false`, a `Message` and an `ErrorCode`, `error` nil:** the operation ran, if only its checks, and refused or failed in a way the user reads: invalid input (`validation`), something missing (`prerequisites`), a cluster request that failed, partial work (`partial_failure`). The message says what was and was not changed. The code of a failed cluster request comes from the request's own error (`errorCodeFromK8sErr`: `forbidden`, `network`, `sign_in_ended`, ...), with a fallback such as `prerequisites` only when it gives none. Handlers answer 422 with the response (`writeOperationResult`); streams send it as the final event.
- **A non-nil `error`:** the function could not report an outcome itself: the context ended (cancelled, deadline, `ErrOperationLockLost`) or the caller has to react to that error. Handlers log it and answer it with `writeOperationError`: the status and errorCode the error carries (`operationErrorStatus`: the reads' `HTTPStatusForError`, with the API server's 409 as `conflict` and the user's 403 as `forbidden`), or a generic 500 `internal` for an error of no known class; streams send its text.

Readers that report a state (`types.ResourceState`, assessments, status parts) put a failed read in the state's message instead of returning an error, so one unreadable part does not hide the others. `nilerr` flags these returns, which is why it is not enabled; the convention is checked in review.

### Frontend Patterns

#### 1. PatternFly Component Structure

All components use **PatternFly 6.x** components:

```tsx
// From frontend/src/App.tsx
import {
  Page,
  PageSection,
  Alert,
  Button,
  Spinner,
} from "@patternfly/react-core";
```

**Key patterns**:

- Use PatternFly's layout components (`Page`, `PageSection`, `Grid`, `Flex`)
- Use PatternFly's form components (`Form`, `FormGroup`, `TextInput`, `Select`)
- Use PatternFly's data display (`Table`, `Card`, `Label`, `EmptyState`)
- Import icons from `@patternfly/react-icons`
- No ellipsis (`...` or `…`) at the end of a label or progress text: a button that opens a dialog reads **Delete workbench**, and loading shows a `Spinner` with a plain phrase. PatternFly uses an ellipsis only for omitted or truncated text. ESLint enforces it (`no-restricted-syntax` in `frontend/eslint.config.mjs`); for omitted text, such as `@sha256:...` in a placeholder, disable the rule on that line and say why.

#### 2. State Management

Components use React hooks for state:

```tsx
const [status, setStatus] = useState<StatusResponse | null>(null);
const [loading, setLoading] = useState(true);
const [error, setError] = useState<string | null>(null);

useEffect(() => {
  // Fetch status on mount
  fetchStatus();
}, []);
```

#### 3. API Calls

API calls are centralized in `frontend/src/services/api/`, one module per feature (`client.ts` has the request and `ApiError`, `stream.ts` the operation streams, then `operator.ts`, `components.ts`, `environment.ts`, ...); `index.ts` re-exports them, so components import from `services/api` (`getStatus`, `getUserPermissions`, `trackPageView`, ...). `App.tsx`, for example:

```tsx
import { trackPageView } from "./services/api";
```

## Deploying Changes

### Full Workflow (Build, Push, Deploy)

```bash
make all               # Equivalent to: make build push deploy
```

### Step-by-Step Deployment

#### 1. Build the Image

```bash
make build
```

#### 2. Push to Registry

```bash
make push              # Pushes IMAGE:dev-<GIT_SHA> (and a test TAG of your own) of a clean tree
```

#### 3. Deploy to OpenShift

```bash
make deploy TAG=dev-<GIT_SHA>   # first install; make upgrade afterwards. Requires cluster-admin and 'oc login'
```

Both run `scripts/install.sh` (the script every release attaches as `install.sh`): they resolve `IMAGE:TAG` to a digest, check that the image was built from a commit with this checkout's `deploy/template.yaml`, create the cookie Secret once, apply the template, wait for the rollout, then add the ConsoleLink, remove legacy objects and prune old ReplicaSets. `DRY_RUN=1` only validates. Overrides, rollback and the first-time `ALLOW_TEMPLATE_MISMATCH=1` are described in [RUNBOOK §2](RUNBOOK.md#2-upgrade-roll-back-or-remove-the-updater). To deploy an image you pushed yourself, commit, push it (`make build push`), then run `make upgrade TAG=dev-<GIT_SHA>`.

If you change `deploy/template.yaml` in a way the running code depends on, bump `TEMPLATE_REVISION` there and `api.ExpectedTemplateRevision` together; the UI then tells admins to upgrade. After any change to it, run `make chart` so the Helm chart renders the same template.

Clusters configured from Git deploy the Helm chart with Argo CD instead of `install.sh`: [docs/GITOPS.md](docs/GITOPS.md).

#### 4. Verify Deployment

```bash
./scripts/smoke-test.sh [namespace] [app-name]
```

Read-only. Expected values (route and proxy timeouts, strategy, grace period, template revision) are read from `deploy/template.yaml`. It checks the deployment, pods and `/api/version`, the Route/oauth-proxy settings, the RBAC objects (and that no legacy ClusterRole remains), the Service/NetworkPolicy ports, monitoring (warns when user workload monitoring is off) and the ConsoleLink. It exits 1 on any failure.

### Undeploy

```bash
make undeploy          # install.sh uninstall: the app namespace, its ClusterRole/Binding, Roles in other namespaces, ConsoleLink
```

### Custom Configuration

Create a `.env` file to persist custom configuration:

```bash
make env               # Create .env from .env.example (an existing .env is kept)
```

Edit `.env` with your values:

```bash
IMAGE=quay.io/myorg/rhoai-nightly-updater
TAG=dev
NAMESPACE=my-rhoai-updater
RUNTIME=docker
```

Then `make` commands will use your custom values.

## Environment Variables

Make variables (`IMAGE`, `TAG`, `NAMESPACE`, `APP_NAME`, `RUNTIME`, `PLATFORM`, `OAUTH_PROXY_IMAGE`, `ROLLOUT_TIMEOUT`, `RELEASES_URL`, `DRY_RUN`, `ALLOW_TEMPLATE_MISMATCH`, `ALLOW_MUTABLE_TAG`) can be set on the command line or in `.env` (`make env` creates one). Run `make help` to list them.

Runtime variables of the deployed container (`GITHUB_TOKEN`, `INSTALLER_RELEASES_URL`, `SEAWEEDFS_IMAGE`, `STABLE_SOURCE`, `STABLE_CHANNEL`, `DSC_SAMPLE_REF`, `LOG_LEVEL`, ...) are documented once, in [README: Configuration](README.md#configuration).

Local development only (`dev.sh` sets the first four rows):

| Variable | Purpose |
|---|---|
| `DEV_MODE=true` | No oauth-proxy, no cluster-admin gate; binds `127.0.0.1`; checks Host/Origin |
| `DEV_TOKEN`, `DEV_USER` | Your `oc` token and username |
| `KUBERNETES_SERVICE_HOST`, `KUBERNETES_SERVICE_PORT` | The API server from `oc whoami --show-server` |
| `DEV_S3_ENDPOINT`, `S3_LOCAL_PORT` | The backend's one direct S3 request (creating the storage bucket) cannot resolve the Service's cluster DNS name from a workstation. `dev.sh` keeps `oc port-forward -n seaweedfs svc/seaweedfs-s3` open on `127.0.0.1:$S3_LOCAL_PORT` (default 18333; retried until the Service exists) and sets `DEV_S3_ENDPOINT` to it. Used only with `DEV_MODE`, and only for a loopback `http://host:port`; pipeline servers and the copy Job always use the in-cluster address |
| `KUBE_CA_FILE` | CA bundle for an API server with a private CA. Example: `oc config view --raw --minify -o jsonpath='{..certificate-authority-data}' \| base64 -d > /tmp/ca.pem`. Without it the system trust store is used (ROSA API certificates are publicly trusted) |
| `DEV_INSECURE_TLS=true` | Skips API server certificate verification (only with `DEV_MODE`; logs a warning). Prefer `KUBE_CA_FILE` |
| `BIND_ADDRESS`, `DEV_ALLOW_REMOTE=true` | A non-loopback bind address is refused unless `DEV_ALLOW_REMOTE=true` (anyone who reaches it acts with your token) |
| `DEV_FRONTEND_PORT` | The dev-server port accepted by the Host/Origin check (default 9000) |
| `API_TARGET` | webpack dev-server proxy target (default `http://127.0.0.1:8080`) |
| `PORT`, `STATIC_DIR`, `METRICS_PORT` | Listener port (8080), static directory (`./frontend/dist`), optional probe/metrics port |

## Getting Help

Run `make help` to see all available targets with descriptions:

```bash
make help              # Show all targets and current configuration
```

## Notes

- All `Makefile` variables can be overridden via environment, command line, or `.env` file.
- The `.env` file is not committed (listed in `.gitignore`).
- Use `make env` to create a `.env` file from `.env.example`.
- The container build runs `go vet`, the Go tests, the frontend typecheck, ESLint and tests; any failure stops the build.
