#!/bin/sh
# Tests for scripts/release.sh. Needs git and a POSIX shell; no network and
# no registry: a fake crane keeps the registry in a directory.
#   sh scripts/test-release.sh
set -eu

HERE=$(cd "$(dirname "$0")" && pwd)
RELEASE="$HERE/release.sh"
# The shell that runs release.sh: SH=dash, SH="busybox sh"...
SH=${SH:-sh}
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
FAILED=0
PASSED=0

fail() {
	printf 'FAIL: %s\n' "$*" >&2
	FAILED=$((FAILED + 1))
}
pass() { PASSED=$((PASSED + 1)); }

# expect_ok NAME CMD...: CMD succeeds.
expect_ok() {
	name=$1
	shift
	if out=$("$@" 2>&1); then pass; else fail "$name: expected success, got: $out"; fi
}

# expect_fail NAME TEXT CMD...: CMD fails and prints TEXT.
expect_fail() {
	name=$1 text=$2
	shift 2
	if out=$("$@" 2>&1); then
		fail "$name: expected failure, got success: $out"
	elif printf '%s' "$out" | grep -qF -- "$text"; then
		pass
	else
		fail "$name: expected '$text' in: $out"
	fi
}

# expect_eq NAME WANT GOT
expect_eq() {
	if [ "$2" = "$3" ]; then pass; else fail "$1: want '$2', got '$3'"; fi
}

export GIT_AUTHOR_NAME=test GIT_AUTHOR_EMAIL=test@example.com
export GIT_COMMITTER_NAME=test GIT_COMMITTER_EMAIL=test@example.com
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1

template() { # REVISION
	printf '%s\n' 'kind: Template' 'objects:' '  - env:' '      - name: TEMPLATE_REVISION' "        value: \"$1\""
}

changelog() { # lines after the title
	printf '%s\n' '# Changelog' '' '## [Unreleased]' '' "$@"
}

commit_all() {
	git add -A
	git commit -qm "$1"
}

# --- A repository with v1.0.0 (template revision 2) ---------------------------
REPO="$WORK/repo"
git init -q -b main "$REPO"
cd "$REPO"
mkdir deploy
template 2 >deploy/template.yaml
changelog '## [1.0.0] - 2026-10-06' '' 'First release.' >CHANGELOG.md
commit_all v1
git tag -a v1.0.0 -m v1.0.0

# --- check ---------------------------------------------------------------------
expect_ok "check v1.0.0 at its tag" $SH "$RELEASE" check v1.0.0 v1.0.0
expect_fail "malformed version" "looks like v1.2.3" $SH "$RELEASE" check 1.0.0
expect_fail "leading zero" "looks like v1.2.3" $SH "$RELEASE" check v1.02.0
expect_fail "changelog behind" "top release section of CHANGELOG.md is [1.0.0], not [1.1.0]" $SH "$RELEASE" check v1.1.0

# Template revision 4, CHANGELOG still "unreleased".
template 4 >deploy/template.yaml
changelog '## [2.0.0] - unreleased' '' '- SeaweedFS.' '' '## [1.0.0] - 2026-10-06' '' 'First release.' >CHANGELOG.md
commit_all v2-wip
expect_fail "unreleased date" "is dated 'unreleased'" $SH "$RELEASE" check v2.0.0

changelog '## [2.0.0] - 2026-10-07' '' '- SeaweedFS.' '' '## [1.0.0] - 2026-10-06' '' 'First release.' >CHANGELOG.md
commit_all v2
expect_ok "major bump with a template change" $SH "$RELEASE" check v2.0.0
# The tag itself exists in a tag pipeline; it is not its own previous release.
git tag v2.0.0
expect_ok "check in a tag pipeline" $SH "$RELEASE" check v2.0.0 v2.0.0
git tag -d v2.0.0 >/dev/null

changelog '## [1.1.0] - 2026-10-07' '' '- Feature.' >CHANGELOG.md
commit_all v1.1-template-change
expect_fail "minor with a template change" "MAJOR version must be higher than v1.0.0's: release v2.0.0" $SH "$RELEASE" check v1.1.0

template 2 >deploy/template.yaml
commit_all v1.1-same-template
expect_ok "minor without a template change" $SH "$RELEASE" check v1.1.0

changelog '## [3.0.0] - 2026-10-07' '' '- Breaking.' >CHANGELOG.md
commit_all v3-same-template
expect_ok "major without a template change" $SH "$RELEASE" check v3.0.0

changelog '## [3.0.0] - 2026-10-07' >CHANGELOG.md
commit_all empty-section
expect_fail "empty section" "section [3.0.0] is empty" $SH "$RELEASE" check v3.0.0

# The previous release is the highest LOWER tag: a v1 patch after v2.
git checkout -q v1.0.0
git checkout -q -b release-1
changelog '## [1.0.1] - 2026-10-08' '' '- Fix.' '' '## [1.0.0] - 2026-10-06' >CHANGELOG.md
commit_all v1.0.1
git tag v2.0.0 main
expect_ok "patch of an older major" $SH "$RELEASE" check v1.0.1
git tag -d v2.0.0 >/dev/null
git checkout -q main

# No earlier tag at all.
git init -q -b main "$WORK/first"
(
	cd "$WORK/first"
	mkdir deploy
	template 1 >deploy/template.yaml
	changelog '## [0.1.0] - 2026-01-01' '' 'Hello.' >CHANGELOG.md
	commit_all first
)
expect_ok "first release" sh -c "cd '$WORK/first' && $SH '$RELEASE' check v0.1.0"

# --- notes -----------------------------------------------------------------------
notes=$(IMAGE=quay.io/example/app $SH "$RELEASE" notes v1.0.0 v1.0.0)
expect_eq "notes body" "First release." "$(printf '%s\n' "$notes" | head -1)"
printf '%s\n' "$notes" | grep -qF 'Image: `quay.io/example/app:v1.0.0`' && pass || fail "notes: image line missing: $notes"
printf '%s\n' "$notes" | grep -qF '(template revision 2)' && pass || fail "notes: template revision missing: $notes"
printf 'echo installer\n' >"$WORK/install.sh"
notes=$(INSTALLER="$WORK/install.sh" INSTALLER_URL=https://example.com/v1.0.0/install.sh $SH "$RELEASE" notes v1.0.0 v1.0.0)
sum=$( (sha256sum "$WORK/install.sh" 2>/dev/null || shasum -a 256 "$WORK/install.sh") | cut -d' ' -f1)
printf '%s\n' "$notes" | grep -qF "install.sh SHA-256: \`$sum\`" && pass || fail "notes: installer checksum missing: $notes"
printf '%s\n' "$notes" | grep -qxF 'curl -fsSLO https://example.com/v1.0.0/install.sh' && pass || fail "notes: download command missing"
printf '%s\n' "$notes" | grep -q 'curl.*| *bash' && fail "notes: piping curl into bash" || pass
expect_fail "notes for a missing section" "no section [9.9.9]" $SH "$RELEASE" notes v9.9.9 v1.0.0

# --- should-move -------------------------------------------------------------
sm() { $SH "$RELEASE" should-move "$@" | cut -d: -f1; }
expect_eq "latest missing" move "$(sm latest v2.0.0 -)"
expect_eq "latest unlabelled counts as v1: v1 patch" move "$(sm latest v1.0.1 2cdcb42d)"
expect_eq "latest unlabelled counts as v1: v2" keep "$(sm latest v2.0.0 2cdcb42d)"
expect_eq "latest same major, newer" move "$(sm latest v1.2.0 v1.1.0)"
expect_eq "latest same major, older" keep "$(sm latest v1.0.1 v1.1.0)"
expect_eq "latest same version" keep "$(sm latest v1.1.0 v1.1.0)"
expect_eq "latest other major" keep "$(sm latest v2.0.0 v1.4.0)"
expect_eq "latest on v2, v1 patch" keep "$(sm latest v1.0.2 v2.0.0)"
expect_eq "latest on v2, v2 minor" move "$(sm latest v2.1.0 v2.0.0)"
expect_eq "vN missing" move "$(sm v2 v2.0.0 -)"
expect_eq "vN unlabelled" move "$(sm v1 v1.0.1 2cdcb42d)"
expect_eq "vN newer" move "$(sm v1 v1.10.0 v1.9.3)"
expect_eq "vN older backport" keep "$(sm v1 v1.0.2 v1.1.0)"
expect_eq "numeric, not lexical" move "$(sm v1 v1.10.0 v1.9.0)"

# --- publish / promote-latest with a fake crane ---------------------------------
REG="$WORK/registry"
mkdir -p "$REG/tags" "$REG/blobs"
CRANE="$WORK/crane"
cat >"$CRANE" <<'FAKE'
#!/bin/sh
# Fake crane: tags/<tag> holds a digest, blobs/<digest> the config JSON.
set -eu
reg=${FAKE_REGISTRY:?}
echo "$*" >>"$reg/calls"
resolve() {
	case "$1" in
	*@*) d=${1#*@} ;;
	*) t=${1##*:}; [ -f "$reg/tags/$t" ] || { echo "MANIFEST_UNKNOWN: $t" >&2; exit 1; }; d=$(cat "$reg/tags/$t") ;;
	esac
	printf '%s\n' "$d"
}
case "$1" in
ls)
	[ -n "${FAKE_LS_ERROR:-}" ] && { echo "$FAKE_LS_ERROR" >&2; exit 1; }
	[ -e "$reg/exists" ] || { echo "NAME_UNKNOWN: repository not found" >&2; exit 1; }
	ls "$reg/tags" ;;
push)
	d="sha256:$(cksum <"$2" | cut -d' ' -f1)"
	cp "$2" "$reg/blobs/$d"; touch "$reg/exists"
	printf '%s\n' "$d" >"$reg/tags/${3##*:}" ;;
digest) resolve "$2" ;;
config) cat "$reg/blobs/$(resolve "$2")" ;;
tag) resolve "$2" >"$reg/tags/$3" ;;
*) echo "fake crane: unsupported $*" >&2; exit 1 ;;
esac
FAKE
chmod +x "$CRANE"

# seed TAG CONFIG: an image with that config under that tag.
seed() {
	d="sha256:seed-$1"
	printf '%s\n' "$2" >"$REG/blobs/$d"
	printf '%s\n' "$d" >"$REG/tags/$1"
	touch "$REG/exists"
}
config() { # VERSION REVISION
	printf '{"config":{"Labels":{"org.opencontainers.image.revision":"%s","org.opencontainers.image.version":"%s"}}}' "$2" "$1"
}
tag_digest() { cat "$REG/tags/$1" 2>/dev/null || echo none; }
reset_registry() { rm -rf "$REG"; mkdir -p "$REG/tags" "$REG/blobs"; }

SHA2=2222222222222222222222222222222222222222
config v2.0.0 "$SHA2" >"$WORK/image.tar"
publish() {
	env FAKE_REGISTRY="$REG" CRANE="$CRANE" IMAGE=quay.io/example/app IMAGE_TARBALL="$WORK/image.tar" \
		CI_COMMIT_SHA="$SHA2" CI_COMMIT_SHORT_SHA=22222222 $SH "$RELEASE" publish "$@"
}
promote() {
	env FAKE_REGISTRY="$REG" CRANE="$CRANE" IMAGE=quay.io/example/app $SH "$RELEASE" promote-latest "$@"
}

# Today's registry: :latest, :v1 and :v1.0.0 on the unlabelled main build.
reset_registry
seed 2cdcb42d '{"config":{"Labels":{"org.opencontainers.image.revision":"1111111111111111111111111111111111111111","org.opencontainers.image.version":"2cdcb42d"}}}'
cp "$REG/tags/2cdcb42d" "$REG/tags/latest"
cp "$REG/tags/2cdcb42d" "$REG/tags/v1"
cp "$REG/tags/2cdcb42d" "$REG/tags/v1.0.0"
old=$(tag_digest latest)
expect_ok "publish v2.0.0" publish v2.0.0
new=$(tag_digest v2.0.0)
expect_eq "v2.0.0 pushed" "$new" "$(tag_digest v2)"
expect_eq "commit tag" "$new" "$(tag_digest 22222222)"
expect_eq ":latest stays on v1" "$old" "$(tag_digest latest)"
expect_eq ":v1 untouched" "$old" "$(tag_digest v1)"

calls_before=$(wc -l <"$REG/calls")
expect_ok "retried publish of the same commit" publish v2.0.0
grep -q '^push' "$REG/calls" && [ "$(grep -c '^push' "$REG/calls")" = 1 ] && pass || fail "a retried publish pushed again"
[ "$(wc -l <"$REG/calls")" -gt "$calls_before" ] && pass || fail "retry did nothing"

seed v3.0.0 "$(config v3.0.0 3333333333333333333333333333333333333333)"
expect_fail "immutable tag from another commit" "already exists (built from 3333333333333333333333333333333333333333)" publish v3.0.0

# Promote across majors, then a v2 minor follows :latest.
expect_ok "promote-latest" promote v2.0.0
expect_eq ":latest promoted" "$new" "$(tag_digest latest)"
config v2.1.0 "$SHA2" >"$WORK/image.tar"
printf 'x\n' >>"$WORK/image.tar"
expect_ok "publish v2.1.0" publish v2.1.0
expect_eq ":latest follows v2.1.0" "$(tag_digest v2.1.0)" "$(tag_digest latest)"
expect_eq ":v2 follows v2.1.0" "$(tag_digest v2.1.0)" "$(tag_digest v2)"
expect_eq "commit tag is immutable" "$new" "$(tag_digest 22222222)"
expect_fail "promote a missing version" "does not exist" promote v9.0.0

# A new repository: everything is published.
reset_registry
config v1.0.0 "$SHA2" >"$WORK/image.tar"
expect_ok "publish to an empty repository" publish v1.0.0
expect_eq "latest created" "$(tag_digest v1.0.0)" "$(tag_digest latest)"

# Registry errors stop the release.
expect_fail "registry unreachable" "Cannot list the tags" env FAKE_LS_ERROR="dial tcp: i/o timeout" FAKE_REGISTRY="$REG" CRANE="$CRANE" \
	IMAGE=quay.io/example/app IMAGE_TARBALL="$WORK/image.tar" CI_COMMIT_SHA="$SHA2" $SH "$RELEASE" publish v1.0.1
expect_fail "IMAGE with a tag" "without a tag" env FAKE_REGISTRY="$REG" CRANE="$CRANE" IMAGE=quay.io/example/app:latest \
	CI_COMMIT_SHA="$SHA2" $SH "$RELEASE" publish v1.0.1

# --- tag (make release) -----------------------------------------------------------
git init -q --bare "$WORK/origin.git"
git remote add origin "$WORK/origin.git"
git push -q origin main
git push -q origin v1.0.0
changelog '## [2.0.0] - 2026-10-07' '' '- SeaweedFS.' >CHANGELOG.md
template 4 >deploy/template.yaml
commit_all release-2
expect_fail "tag: branch not pushed" "not the same commit as origin/main" $SH "$RELEASE" tag v2.0.0
git push -q origin main
echo dirty >>CHANGELOG.md
expect_fail "tag: dirty tree" "working tree has changes" $SH "$RELEASE" tag v2.0.0
git checkout -q CHANGELOG.md
expect_fail "tag: version not in the CHANGELOG" "is [2.0.0], not [1.1.0]" $SH "$RELEASE" tag v1.1.0
git checkout -q -b topic
expect_fail "tag: not on main" "this checkout is on topic" $SH "$RELEASE" tag v2.0.0
git checkout -q main
out=$($SH "$RELEASE" tag v2.0.0 2>&1) && pass || fail "tag v2.0.0: $out"
expect_eq "annotated tag" tag "$(git cat-file -t v2.0.0)"
printf '%s\n' "$out" | grep -qF 'git push origin v2.0.0' && pass || fail "tag: no push command: $out"
printf '%s\n' "$out" | grep -qF 'git push github v2.0.0   # no remote named github' && pass || fail "tag: github hint missing: $out"
[ -z "$(git ls-remote --tags origin refs/tags/v2.0.0)" ] && pass || fail "tag: the tag was pushed"
expect_fail "tag: exists" "already exists" $SH "$RELEASE" tag v2.0.0

# --- json_string (GitLab Release body) ---------------------------------------------
sed -n '/^json_string()/,/^}/p' "$RELEASE" >"$WORK/json-only.sh"
json=$(sh -c '. "$1"; json_string "$2"' sh "$WORK/json-only.sh" "$(printf 'a "b"\\c\n\td')")
expect_eq "json_string" '"a \"b\"\\c\n\td"' "$json"

printf '%d passed, %d failed\n' "$PASSED" "$FAILED"
[ "$FAILED" -eq 0 ]
