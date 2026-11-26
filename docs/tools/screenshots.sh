#!/usr/bin/env bash
# Regenerates the docs images under docs/images (the curated set that
# docs/tools/images.manifest lists) from the docs mock backend
# (docs/tools/mock), never from a cluster. Run it in the release pass, not
# on feature branches (AGENTS.md, CONTRIBUTING.md "Documentation images").
#
#   ./docs/tools/screenshots.sh            all docs images
#   ./docs/tools/screenshots.sh status     only these jobs
#
# Needs node (22+), playwright-cli and ImageMagick 7 (magick).
# SKIP_BUILD=1 reuses frontend/dist; PORT picks the mock's port (18181).
# The images use the Glass contrast mode; CONTRAST=default or
# CONTRAST=high-contrast captures them in another mode.
# The themes job (not run by default) captures every page in every color
# scheme and contrast mode into THEMES_DIR (default tmp/themes), for review.
set -euo pipefail
shopt -s nullglob

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
IMAGES="$ROOT/docs/images"
PORT=${PORT:-18181}
SESSION=${SESSION:-docs-screenshots}
CONTRAST=${CONTRAST:-glass}
case $CONTRAST in glass | default | high-contrast) ;; *) echo "ERROR: CONTRAST must be glass, default or high-contrast" >&2; exit 1 ;; esac
ALL_JOBS=(pages status resources)
if [ $# -gt 0 ]; then JOBS=("$@"); else JOBS=("${ALL_JOBS[@]}"); fi

for tool in node playwright-cli magick curl; do
  command -v "$tool" >/dev/null 2>&1 || { echo "ERROR: $tool is not installed" >&2; exit 1; }
done

# The physical path: playwright-cli compares paths without resolving symlinks.
WORK=$(cd "$(mktemp -d "${TMPDIR:-/tmp}/docs-shots.XXXXXX")" && pwd -P)
MOCK_PID=
cleanup() {
  playwright-cli -s="$SESSION" close >/dev/null 2>&1 || true
  [ -n "$MOCK_PID" ] && kill "$MOCK_PID" 2>/dev/null || true
  if [ -n "${KEEP_WORK:-}" ]; then echo "Work directory kept: $WORK"; else rm -rf "$WORK"; fi
}
trap cleanup EXIT

if [ "${SKIP_BUILD:-}" != 1 ]; then
  echo "Building the frontend..."
  [ -d "$ROOT/frontend/node_modules" ] || npm --prefix "$ROOT/frontend" ci --no-audit --no-fund >/dev/null
  npm --prefix "$ROOT/frontend" run build >/dev/null
fi

node "$ROOT/docs/tools/mock/server.mjs" --port "$PORT" >"$WORK/mock.log" 2>&1 &
MOCK_PID=$!
for _ in $(seq 50); do curl -fsS "http://127.0.0.1:$PORT/__docs/state" >/dev/null 2>&1 && break; sleep 0.2; done

# 1440x900 at 2x: crops stay sharp on high-density screens; full pages are
# scaled back to 1440 px. Without the GPU: with it, Chromium draws the Glass
# background image (fixed, under a backdrop blur) slightly differently from
# one frame to the next, so no two runs gave the same bytes.
cat >"$WORK/cli.config.json" <<'JSON'
{"browser":{"launchOptions":{"args":["--disable-gpu"]},
"contextOptions":{"viewport":{"width":1440,"height":900},"deviceScaleFactor":2,
"timezoneId":"UTC","locale":"en-US","colorScheme":"light",
"permissions":["clipboard-read","clipboard-write"]}}}
JSON
# playwright-cli only reads and writes files under its working directory.
cp "$ROOT/docs/tools/capture.js" "$WORK/capture.js"
cd "$WORK"
playwright-cli -s="$SESSION" open --config="$WORK/cli.config.json" "http://127.0.0.1:$PORT/__docs/state" >/dev/null

RAW="$WORK/raw"
mkdir -p "$RAW/full" "$RAW/crop" "$RAW/themes"
for job in "${JOBS[@]}"; do
  echo "Capturing $job..."
  curl -fsS -X POST "http://127.0.0.1:$PORT/__docs/job?name=$job&out=$RAW&contrast=$CONTRAST" >/dev/null
  out=$(playwright-cli -s="$SESSION" run-code --filename="$WORK/capture.js" 2>&1)
  if ! grep -q "\"$job: done\"" <<<"$out"; then
    echo "$out" | head -30 >&2
    echo "ERROR: job $job failed" >&2
    exit 1
  fi
done

# Theme review images (themes job): scaled like full pages, not docs images.
themes=("$RAW"/themes/*.png)
if [ ${#themes[@]} -gt 0 ]; then
  THEMES_DIR=${THEMES_DIR:-$ROOT/tmp/themes}
  mkdir -p "$THEMES_DIR"
  for f in "${themes[@]}"; do
    magick "$f" -filter Lanczos -resize 1440x "$THEMES_DIR/$(basename "$f")"
  done
  echo "Theme review images: $THEMES_DIR"
fi

# A full run replaces every image, so none is left that the docs no longer use.
if [ $# -eq 0 ]; then
  find "$IMAGES" -maxdepth 1 \( -name '*.png' -o -name '*.gif' \) -delete
fi
mkdir -p "$IMAGES"

# PNGs: 256 colours without dithering keeps UI text crisp at a fraction of the size.
optimise() { magick "$1" -strip +dither -colors 256 -define png:compression-level=9 "PNG8:$2"; }
for f in "$RAW"/full/*.png; do
  [ -e "$f" ] || continue
  magick "$f" -filter Lanczos -resize 1440x "$WORK/scaled.png"
  optimise "$WORK/scaled.png" "$IMAGES/$(basename "$f")"
done
for f in "$RAW"/crop/*.png; do
  [ -e "$f" ] || continue
  optimise "$f" "$IMAGES/$(basename "$f")"
done

# On one machine two runs give the same bytes (Chromium without the GPU,
# fixed clock and data). Another machine or Chromium version can still
# shift a few pixels slightly (by less than 10%). An image with no pixel
# that differs from the committed one by more than 10% keeps its committed
# bytes, so a run without a UI change leaves git clean. A real change, even
# one full stop, moves pixels by far more. KEEP_ALL=1 keeps every new image.
while IFS= read -r f; do
  git -C "$ROOT" show "HEAD:$f" >"$WORK/old" 2>/dev/null || continue
  new="$ROOT/$f"
  cp "$WORK/old" "$WORK/old.png"; cp "$new" "$WORK/new.png"
  [ "$(magick identify -format '%wx%h' "$WORK/old.png")" = "$(magick identify -format '%wx%h' "$WORK/new.png")" ] || continue
  diff=$(magick compare -fuzz 10% -metric AE "$WORK/old.png" "$WORK/new.png" null: 2>&1 | cut -d' ' -f1 || true)
  if [ -z "${KEEP_ALL:-}" ] && awk "BEGIN{exit !(${diff:-1e9} < 1)}"; then cp "$WORK/old" "$new"; fi
done < <(git -C "$ROOT" status --porcelain -- docs/images | awk '$1 == "M" {print $2}')

# The commit each image was captured at, for docs/tools/stale-images.sh: a
# run that leaves an image's bytes as they were makes no commit of the
# image, so this line records that it was checked. Commit it with the images.
CHECKED="$ROOT/docs/tools/images.checked"
captured=()
for f in "$RAW"/full/*.png "$RAW"/crop/*.png; do captured+=("$(basename "$f")"); done
if [ ${#captured[@]} -gt 0 ]; then
  head=$(git -C "$ROOT" rev-parse HEAD)
  kept=
  # A full run drops the lines of images it no longer makes.
  if [ $# -gt 0 ] && [ -f "$CHECKED" ]; then
    kept=$(awk 'NR == FNR { skip[$1] = 1; next } !/^#/ && NF && !skip[$1]' <(printf '%s\n' "${captured[@]}") "$CHECKED")
  fi
  {
    echo "# Written by docs/tools/screenshots.sh: each docs image and the commit it was last"
    echo "# captured at (make docs-stale). Commit it with the images; do not edit it by hand."
    { [ -z "$kept" ] || printf '%s\n' "$kept"; for n in "${captured[@]}"; do echo "$n $head"; done; } | sort
  } >"$CHECKED"
fi

# A contact sheet of the images that changed, to look at
# before committing: a UI change can move content without breaking a locator.
REVIEW="$ROOT/tmp/docs-review"
rm -rf "$REVIEW" && mkdir -p "$REVIEW"
changed=()
while IFS= read -r f; do changed+=("$ROOT/$f"); done < <(git -C "$ROOT" status --porcelain -- docs/images | awk '$1 != "D" {print $2}')
if [ ${#changed[@]} -gt 0 ]; then
  for f in "${changed[@]}"; do magick "$f" -resize 600x\> "$REVIEW/$(basename "$f")"; done
  # montage needs a font even without labels; index.txt lists the tiles in order.
  font=
  for candidate in /System/Library/Fonts/Supplemental/Arial.ttf /usr/share/fonts/dejavu/DejaVuSans.ttf /usr/share/fonts/truetype/dejavu/DejaVuSans.ttf; do
    [ -f "$candidate" ] && font=$candidate && break
  done
  (cd "$REVIEW" && printf '%s\n' ./*.png) >"$REVIEW/index.txt"
  [ -n "$font" ] && magick montage -font "$font" "$REVIEW"/*.png -tile 4x -geometry +6+6 -background '#777' "$REVIEW/contact-sheet.png"
  echo "Changed images: ${#changed[@]}; review $REVIEW/contact-sheet.png before committing."
fi

echo "Images in $IMAGES:"
for f in "$IMAGES"/*; do printf '  %-44s %5d KB\n' "$(basename "$f")" $(( $(wc -c <"$f") / 1024 )); done
