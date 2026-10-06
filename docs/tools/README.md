# Documentation images

Every image and GIF under `docs/images` is generated from a mock backend with
made-up data, never from a cluster: the GitHub mirror is public. Regenerate
them all with one command:

```bash
make docs-screenshots                 # all images (about 3 minutes)
make docs-screenshots JOBS="status gif-update"   # only some jobs
```

It needs Node 22+, `playwright-cli`, `ffmpeg` and ImageMagick 7 (`magick`).
It builds the frontend (`SKIP_BUILD=1` reuses `frontend/dist`), starts the
mock, and replaces the images. A full run deletes the PNGs and GIFs first, so
an image that no job produces any more disappears.

## The pieces

| File | What it does |
|---|---|
| `mock/server.mjs` | Serves `frontend/dist` and answers `/api/*` from fixtures. `make docs-mock SCENARIO=<name>` runs it for a look in a browser (port 18181) |
| `mock/fixtures.mjs` | The fixtures, shaped like `pkg/types`, `pkg/api` and `pkg/cluster`. Cluster `example-cluster`, user `dev-user`, digests derived from names |
| `mock/diagnostics/*.json` | Diagnostics answers. `healthy.json` is kept by hand; the others are written by the backend from the fake clusters of the `pkg/cluster` tests: `make docs-fixtures` |
| `capture.js` | The capture jobs (run through `playwright-cli run-code`): full pages, crops, callouts, GIF frames |
| `screenshots.sh` | Starts everything, runs the jobs, optimises the PNGs and builds the GIFs |

Scenarios (`GET /__docs/state` lists them) cover every state the docs show:
the Status verdicts, update and install flows, remote and interrupted
operations, update notices, Components, Dashboard Dev, the S3 storage states
and each Diagnostics problem. In the flow scenarios the update steps and the
S3 setup wait for `POST /__docs/advance` and `POST /__docs/release`, so every
GIF frame shows a known state. The browser clock is fixed at
2026-10-06 14:30 UTC, so relative times ("built 11h ago") do not change.

## Conventions

- **Full pages** (`page-<page>-light.png`, `-dark.png`): 1440 px wide, rendered
  at 2x and scaled down. README uses them in `<picture>` elements with a
  `prefers-color-scheme: dark` source.
- **Crops** (`<page or area>-<what>.png`): 2x, so show them at about half
  their pixel width (`width="760"` in the docs). Light theme only.
- **Callouts**: red numbered badges and boxes drawn into the page before the
  capture. Each annotated image has a numbered legend under it in the docs;
  keep the numbers and the legend in step when you change `capture.js`.
- **GIFs**: 1000 px wide, at most ~10 s, one palette per GIF, no dithering.
  A drawn pointer shows where each click goes.
- **PNGs** are reduced to 256 colours without dithering (text stays sharp).
