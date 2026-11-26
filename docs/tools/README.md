# Documentation images

Every image under `docs/images` (the curated set `images.manifest` lists,
about 15) is generated from a mock backend with made-up data, never from a
cluster: the GitHub mirror is public. Branches do not regenerate them; the
release pass does, with one command ([CONTRIBUTING.md](../../CONTRIBUTING.md#documentation-images)):

```bash
make docs-screenshots                    # all docs images (about a minute)
make docs-screenshots JOBS="status"      # only some jobs (pages, status, resources)
make docs-screenshots JOBS="themes"      # review images in tmp/themes, not docs images
make docs-stale                          # images whose sources changed since their capture
```

It needs Node 22+, `playwright-cli` and ImageMagick 7 (`magick`). It builds
the frontend (`SKIP_BUILD=1` reuses `frontend/dist`), starts the mock, and
replaces the images. A full run deletes the PNGs and GIFs first, so an image
that no job produces any more disappears. Each run writes the commit it
captured at into `images.checked` for every image it made; commit that file
with the images.

**Byte-stable.** With an unchanged UI, two runs give the same bytes: the
browser clock and the mock's data are fixed, Chromium runs without the GPU
(with it, the Glass background differed from frame to frame), and each
screenshot is written once two in a row are the same. On another machine
or Chromium version a few pixels can still move slightly; an image with no
pixel that moved by more than 10% keeps its committed bytes (`KEEP_ALL=1`
keeps every new image).

**Unanswered reads fail the run.** The mock records every `GET /api/*` no
route answers (`GET /__docs/unhandled`), and a job stops with their names:
an image would otherwise show an error or every action disabled.

## The pieces

| File | What it does |
|---|---|
| `mock/server.mjs` | Serves `frontend/dist` and answers `/api/*` from fixtures. `make docs-mock SCENARIO=<name>` runs it for a look in a browser (port 18181) |
| `mock/fixtures.mjs` | The fixtures, shaped like `pkg/types`, `pkg/api` and `pkg/cluster`. Cluster `example-cluster`, user `dev-user`, digests derived from names |
| `mock/scenarios.mjs` | The scenarios (the world each starts from) and the reads every page makes. `npm test` in `frontend` type-checks every scenario's reads against the frontend's API types (`src/test/docsMockFixtures.test.ts`) |
| `mock/diagnostics/*.json` | Diagnostics answers. `healthy.json` is kept by hand; the others are written by the backend from the fake clusters of the `pkg/cluster` tests: `make docs-fixtures` |
| `capture.js` | The capture jobs (run through `playwright-cli run-code`): the full pages and crops of the docs, and the `themes` and `*-review` jobs (scratch review images in `THEMES_DIR`) |
| `screenshots.sh` | Starts everything, runs the jobs, optimises the PNGs and writes `images.checked` |
| `images.manifest` | Each docs image and the source paths it shows; `make docs-check` wants a row for every image |
| `stale-images.sh` | `make docs-stale`: images whose sources have commits after the capture (git only); `make release-check` stops a final release on one |

Scenarios (`GET /__docs/state` lists them) cover the states the docs show
and many more to look at by hand (`make docs-mock SCENARIO=<name>`): the
Status verdicts, update and install flows, remote and interrupted
operations, update notices, Components, Dashboard Dev, the S3 storage states,
each Diagnostics problem, and the views of a user who is not a cluster-admin
(`user`, `user-no-projects`, `user-update-running`). In the flow scenarios
the update steps and the S3 setup wait for `POST /__docs/advance` and
`POST /__docs/release`, so each step can be looked at in a known state. `POST /__docs/hold?path=/api/resources/maas`
holds the reads under a path until `POST /__docs/hold` without one, so a
loading state can be captured (the `loading-review` job does, into
`THEMES_DIR`); `make docs-mock DELAY=2000` answers every Environment read two
seconds late, to look at the loading states by hand. The browser clock and the mock's own
times are fixed at 2026-10-06 14:30 UTC, so relative times ("built 11h ago")
do not change.

## Conventions

- **Contrast mode**: every image uses PatternFly's Glass contrast mode
  (translucent surfaces over the theme's background image). `CONTRAST=default` or `CONTRAST=high-contrast` captures them in
  another mode; `capture.js` sets it as the app stores it
  (`src/theme/themeCore.ts` `CONTRAST_MODE_KEY`), and `open()` takes
  `contrast` for a single image.
- **Light theme only**, in plain Markdown image syntax; no
  `<picture>` (GitLab's sanitizer drops it), no dark copies.
- **Full pages** (`page-<page>.png`): 1440 px wide, rendered at 2x and
  scaled down.
- **Crops** (`<page or area>-<what>.png`): an 860 px window at 2x, so show them at about half
  their pixel width (`width="760"` in the docs).
- **No callouts and no GIFs**: they are costly to keep accurate. Describe
  the parts in text that quotes the UI's labels.
- **PNGs** are reduced to 256 colours without dithering (text stays sharp).
