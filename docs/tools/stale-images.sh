#!/bin/sh
# Lists the docs images whose sources have commits after the image was
# captured: make docs-stale. Git history only, no browser. The sources of
# each image are in docs/tools/images.manifest; the capture is the commit
# docs/tools/images.checked records for the image (screenshots.sh writes it
# only for a build of that commit with clean inputs). An image without such
# a receipt, or whose receipt names a commit that is not an ancestor of
# HEAD, counts as stale. Commits are counted by ancestry (HEAD --not the
# capture commit), not by date, so a branch merged after the capture counts.
#
#   stale-images.sh                    exit 1 when an image is stale
#   stale-images.sh --release VERSION  release-check: a final release
#                                      (vX.Y.Z) stops when an image is
#                                      stale; a pre-release only warns
#
# POSIX sh and git only: it runs in the release guard's Go image.
set -euf

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
cd "$ROOT"
MANIFEST=docs/tools/images.manifest
CHECKED=docs/tools/images.checked

version=
kind=
case ${1:-} in
'') ;;
--release)
	version=${2:-}
	if printf '%s\n' "$version" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+$'; then
		kind=release
	elif printf '%s\n' "$version" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+-(alpha|beta|rc)\.[0-9]+$'; then
		kind=prerelease
	else
		echo "ERROR: '$version' is not a release (vX.Y.Z) or a pre-release (vX.Y.Z-alpha.N, -beta.N, -rc.N)." >&2
		exit 2
	fi
	;;
*) echo "Usage: $0 [--release VERSION]" >&2; exit 2 ;;
esac

if [ "$(git rev-parse --is-shallow-repository)" = true ]; then
	echo "ERROR: this clone is shallow; the staleness check needs the full history (GitLab CI: GIT_DEPTH: \"0\")." >&2
	exit 2
fi

common=$(awk '!/^#/ && $1 == "*" { for (i = 2; i <= NF; i++) print $i }' "$MANIFEST")
images=$(awk '!/^#/ && NF && $1 != "*" { print $1 }' "$MANIFEST" | sort -u)
problems=0
stale=0
report=
for img in $images; do
	if [ ! -f "docs/images/$img" ]; then
		echo "ERROR: $MANIFEST names docs/images/$img, which does not exist." >&2
		problems=1
		continue
	fi
	sources=$(awk -v img="$img" '!/^#/ && $1 == img { for (i = 2; i <= NF; i++) print $i }' "$MANIFEST")
	captured=
	[ ! -f "$CHECKED" ] || captured=$(awk -v img="$img" '!/^#/ && $1 == img { print $2; exit }' "$CHECKED")
	commit=
	[ -z "$captured" ] || commit=$(git rev-parse --verify --quiet "$captured^{commit}" || true)
	if [ -z "$commit" ] || ! git merge-base --is-ancestor "$commit" HEAD; then
		stale=$((stale + 1))
		report="$report  $img: no capture receipt for a commit of this history in $CHECKED${captured:+ ($captured)}
"
		continue
	fi
	# shellcheck disable=SC2086 # word splitting: one pathspec per word (set -f: no globbing)
	commits=$(git log --format='%h %ad %s' --date=short HEAD --not "$commit" -- \
		$common $sources ':(exclude,glob)**/*.test.*' ':(exclude)frontend/src/test')
	[ -n "$commits" ] || continue
	stale=$((stale + 1))
	n=$(printf '%s\n' "$commits" | wc -l | tr -d ' ')
	report="$report$(printf '  %s: %s commit(s) since it was captured, newest:\n    %s' "$img" "$n" "$(printf '%s\n' "$commits" | head -n 1)")
"
done
[ "$problems" -eq 0 ] || exit 2

total=$(printf '%s\n' "$images" | grep -c .)
if [ "$stale" -eq 0 ]; then
	echo "All $total docs images were captured after the last change to their sources."
	exit 0
fi
echo "$stale of $total docs images may be stale (their sources changed after the capture):"
printf '%s' "$report"
echo "Regenerate them in the release pass (make docs-screenshots), look at each one, and commit them"
echo "with docs/tools/images.checked; a source change that did not change an image then counts as checked."

case $kind in
release)
	echo "ERROR: $version is a final release: regenerate the stale docs images before tagging it." >&2
	exit 1
	;;
prerelease)
	echo "WARNING: $version is a pre-release, so stale docs images do not stop it; its final release needs them current."
	exit 0
	;;
esac
exit 1
