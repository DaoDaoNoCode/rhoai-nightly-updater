#!/usr/bin/env bash
# Regenerates every image and GIF under docs/images from the docs mock
# backend (docs/tools/mock), never from a cluster.
#
#   ./docs/tools/screenshots.sh            all jobs
#   ./docs/tools/screenshots.sh status gif-update   only these jobs
#
# Needs node (22+), playwright-cli, ffmpeg and ImageMagick 7 (magick).
# SKIP_BUILD=1 reuses frontend/dist; PORT picks the mock's port (18181).
set -euo pipefail
shopt -s nullglob

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
IMAGES="$ROOT/docs/images"
PORT=${PORT:-18181}
SESSION=docs-screenshots
ALL_JOBS=(pages status banners components builds dashdev resources diagnostics
  gif-update gif-install gif-migrate gif-repair gif-diagnostics gif-pr-search)
if [ $# -gt 0 ]; then JOBS=("$@"); else JOBS=("${ALL_JOBS[@]}"); fi

for tool in node playwright-cli ffmpeg magick curl; do
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
# scaled back to 1440 px.
cat >"$WORK/cli.config.json" <<'JSON'
{"browser":{"contextOptions":{"viewport":{"width":1440,"height":900},"deviceScaleFactor":2,
"timezoneId":"UTC","locale":"en-US","colorScheme":"light",
"permissions":["clipboard-read","clipboard-write"]}}}
JSON
# playwright-cli only reads and writes files under its working directory.
cp "$ROOT/docs/tools/capture.js" "$WORK/capture.js"
cd "$WORK"
playwright-cli -s="$SESSION" open --config="$WORK/cli.config.json" "http://127.0.0.1:$PORT/__docs/state" >/dev/null

RAW="$WORK/raw"
mkdir -p "$RAW/full" "$RAW/crop" "$RAW/frames"
for job in "${JOBS[@]}"; do
  echo "Capturing $job..."
  curl -fsS -X POST "http://127.0.0.1:$PORT/__docs/job?name=$job&out=$RAW" >/dev/null
  out=$(playwright-cli -s="$SESSION" run-code --filename="$WORK/capture.js" 2>&1)
  if ! grep -q "\"$job: done\"" <<<"$out"; then
    echo "$out" | head -30 >&2
    echo "ERROR: job $job failed" >&2
    exit 1
  fi
done

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

# GIFs: each frame file is NNN-<ms>.png; one palette for the whole GIF and
# no dithering, so the UI does not flicker.
for dir in "$RAW"/frames/*/; do
  [ -d "$dir" ] || continue
  name=$(basename "$dir")
  list="$WORK/$name.txt"
  : >"$list"
  last=
  for f in "$dir"*.png; do
    ms=${f##*-}; ms=${ms%.png}
    printf "file '%s'\nduration %s\n" "$f" "$(awk "BEGIN{print $ms/1000}")" >>"$list"
    last=$f
  done
  [ -n "$last" ] || continue
  printf "file '%s'\n" "$last" >>"$list"
  width=${GIF_WIDTH:-860}
  ffmpeg -v error -y -f concat -safe 0 -i "$list" \
    -vf "scale=$width:-1:flags=lanczos,palettegen=max_colors=256:stats_mode=full" "$WORK/$name-palette.png"
  ffmpeg -v error -y -f concat -safe 0 -i "$list" -i "$WORK/$name-palette.png" \
    -lavfi "scale=$width:-1:flags=lanczos[v];[v][1:v]paletteuse=dither=none:diff_mode=rectangle" \
    -loop 0 "$IMAGES/$name.gif"
done

# Chromium's text rendering and the colour reduction move a few pixels from
# run to run. An image that differs from the committed one by less than
# 0.05% of its pixels (each by more than 3%) keeps its committed bytes, so a
# run without a UI change leaves git clean.
while IFS= read -r f; do
  git -C "$ROOT" show "HEAD:$f" >"$WORK/old" 2>/dev/null || continue
  new="$ROOT/$f"
  if [[ $f == *.gif ]]; then
    [ "$(magick identify "$WORK/old" | wc -l)" = "$(magick identify "$new" | wc -l)" ] || continue
    magick "$WORK/old" -coalesce -append "$WORK/old.png"
    magick "$new" -coalesce -append "$WORK/new.png"
  else
    cp "$WORK/old" "$WORK/old.png"; cp "$new" "$WORK/new.png"
  fi
  [ "$(magick identify -format '%wx%h' "$WORK/old.png")" = "$(magick identify -format '%wx%h' "$WORK/new.png")" ] || continue
  diff=$(magick compare -fuzz 3% -metric AE "$WORK/old.png" "$WORK/new.png" null: 2>&1 | cut -d' ' -f1 || true)
  pixels=$(magick identify -format '%[fx:w*h]' "$WORK/old.png")
  if awk "BEGIN{exit !(${diff:-1e9} < $pixels * 0.0005)}"; then cp "$WORK/old" "$new"; fi
done < <(git -C "$ROOT" status --porcelain -- docs/images | awk '$1 == "M" {print $2}')

# A contact sheet of the images that changed (GIFs: every frame), to look at
# before committing: a UI change can move content without breaking a locator.
REVIEW="$ROOT/tmp/docs-review"
rm -rf "$REVIEW" && mkdir -p "$REVIEW"
changed=()
while IFS= read -r f; do changed+=("$ROOT/$f"); done < <(git -C "$ROOT" status --porcelain -- docs/images | awk '$1 != "D" {print $2}')
if [ ${#changed[@]} -gt 0 ]; then
  for f in "${changed[@]}"; do
    case $f in
      *.gif) magick "$f" -coalesce -resize 600x "$REVIEW/$(basename "$f" .gif)-%02d.png" ;;
      *) magick "$f" -resize 600x\> "$REVIEW/$(basename "$f")" ;;
    esac
  done
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
