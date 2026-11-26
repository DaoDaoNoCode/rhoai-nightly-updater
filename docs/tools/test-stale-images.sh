#!/bin/sh
# Tests docs/tools/stale-images.sh (make docs-stale) in a throwaway git
# repository: the receipt's capture commit is the cutoff, a source change
# between capture and receipt counts, a byte-identical recapture clears it,
# a branch merged after the capture counts, test files never count, and the
# exit status for a final release, a pre-release and an invalid version.
# POSIX sh and git only. Run: make docs-stale-test
set -eu

HERE=$(cd "$(dirname "$0")" && pwd)
WORK=$(mktemp -d "${TMPDIR:-/tmp}/stale-images-test.XXXXXX")
trap 'rm -rf "$WORK"' EXIT
export GIT_AUTHOR_NAME=test GIT_AUTHOR_EMAIL=test@example.com
export GIT_COMMITTER_NAME=test GIT_COMMITTER_EMAIL=test@example.com
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1

failures=0
fail() { echo "FAIL: $*"; failures=$((failures + 1)); }

REPO=$WORK/repo
git init -q -b main "$REPO"
cd "$REPO"
mkdir -p docs/tools docs/images frontend/src
cp "$HERE/stale-images.sh" docs/tools/stale-images.sh
cat >docs/tools/images.manifest <<'EOF'
# image  sources
*        frontend/shared.txt
a.png    frontend/src/a.tsx
b.png    frontend/src/b.tsx
EOF
echo shared >frontend/shared.txt
echo a >frontend/src/a.tsx
echo b >frontend/src/b.tsx
echo png-a >docs/images/a.png
echo png-b >docs/images/b.png

commit() { git add -A && git commit -q -m "$1"; }
# receipt COMMIT: both images captured at COMMIT.
receipt() {
	printf '# receipts\na.png %s\nb.png %s\n' "$1" "$1" >docs/tools/images.checked
}
# expect STATUS OUTPUT-PATTERN DESCRIPTION [ARGS...]: runs the check.
expect() {
	want=$1 pattern=$2 what=$3
	shift 3
	status=0
	out=$(sh docs/tools/stale-images.sh "$@" 2>&1) || status=$?
	[ "$status" = "$want" ] || fail "$what: exit $status, want $want: $out"
	printf '%s\n' "$out" | grep -q -- "$pattern" || fail "$what: output lacks '$pattern': $out"
}

commit "sources and images"
A=$(git rev-parse HEAD)
expect 1 "a.png: no capture receipt" "no receipt at all"

receipt "$A"
commit "receipts at A"
expect 0 "All 2 docs images" "receipt at the capture commit"
expect 0 "All 2 docs images" "fresh, final release" --release v1.0.0

receipt 0123456789abcdef0123456789abcdef01234567
expect 1 "no capture receipt" "receipt naming an unknown commit"
git checkout -q -b elsewhere
echo elsewhere >frontend/other.txt
commit "a commit off main"
X=$(git rev-parse HEAD)
git checkout -q main
receipt "$X"
expect 1 "no capture receipt" "receipt naming a commit that is not an ancestor of HEAD"
git checkout -q -- docs/tools/images.checked

# Captured at A; a UI change lands (B); images and receipt are committed
# after it (C). The receipt still says A, so B counts.
echo a2 >frontend/src/a.tsx
commit "B: change a"
echo png-a2 >docs/images/a.png
receipt "$A"
commit "C: images and receipts captured at A"
expect 1 "a.png: 1 commit" "a source change between capture and receipt"
out=$(sh docs/tools/stale-images.sh 2>&1 || true)
printf '%s\n' "$out" | grep -q "b.png" && fail "b.png flagged by a change to a.tsx only: $out"
expect 1 "is a final release" "stale, final release" --release v1.0.0
expect 0 "is a pre-release" "stale, pre-release" --release v1.0.0-alpha.1
expect 2 "is not a release" "invalid version" --release 1.0

# Captured again at HEAD, every image byte for byte the same: only the
# receipts change.
receipt "$(git rev-parse HEAD)"
commit "receipts at C"
expect 0 "All 2 docs images" "byte-identical recapture"

# Test files never count.
echo test >frontend/src/a.test.tsx
commit "a test"
expect 0 "All 2 docs images" "a test-only change"

# A branch started before the capture and merged after it counts.
git checkout -q -b feature "$A"
echo b2 >frontend/src/b.tsx
commit "change b on a branch"
git checkout -q main
receipt "$(git rev-parse HEAD)"
commit "receipts before the merge"
git merge -q --no-edit feature
expect 1 "b.png: 1 commit" "a change merged after the capture"
out=$(sh docs/tools/stale-images.sh 2>&1 || true)
printf '%s\n' "$out" | grep -q "a.png" && fail "a.png flagged by the merged change to b.tsx: $out"

# A shared source makes every image stale.
receipt "$(git rev-parse HEAD)"
commit "receipts after the merge"
echo shared2 >frontend/shared.txt
commit "change the shared source"
expect 1 "2 of 2 docs images" "a shared source change"

if [ "$failures" -gt 0 ]; then
	echo "test-stale-images.sh: $failures failure(s)"
	exit 1
fi
echo "test-stale-images.sh: all cases passed"
