#!/bin/sh
# shellcheck disable=SC2015,SC2016,SC2086 # pass never fails; quoted $ is literal; $SH may be "busybox sh"
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

# --- Versions: pre-releases (SemVer 2.0.0 items 9 and 11) ----------------------
expect_eq "kind: a release" release "$($SH "$RELEASE" kind v1.2.3)"
for v in v1.2.3-alpha.1 v1.2.3-beta.2 v1.2.3-rc.10 v0.0.0-alpha.1; do
	expect_eq "kind: $v" prerelease "$($SH "$RELEASE" kind "$v")"
done
for v in v1.2.3-alpha.0 v1.2.3-alpha.01 v1.2.3-alpha v1.2.3-preview.1 v1.2.3-RC.1 v1.2.3-rc.1.2 v1.2.3-rc.1+build v1.2.3- v1.2.3-rc-1; do
	expect_fail "kind: $v is refused" "A pre-release version looks like v1.2.3-alpha.1, v1.2.3-beta.1 or v1.2.3-rc.1" $SH "$RELEASE" kind "$v"
done
for v in 1.2.3 v1.2 v01.2.3 v1.2.3.4 latest; do
	expect_fail "kind: $v is refused" "A release version looks like v1.2.3" $SH "$RELEASE" kind "$v"
done
expect_fail "kind: two lines are not a version" "looks like" $SH "$RELEASE" kind "$(printf 'v1.2.3-rc.1\nx')"
expect_fail "kind: two lines are not a release" "looks like" $SH "$RELEASE" kind "$(printf 'v1.2.3\nv1.2.4')"
# Precedence: every version is lower than every later one in this list.
sed -n '/^CORE_RE=/,/^# --- Repository contents/p' "$RELEASE" >"$WORK/versions-only.sh"
ORDER="v1.0.0-alpha.1 v1.0.0-alpha.2 v1.0.0-alpha.10 v1.0.0-beta.1 v1.0.0-beta.2 v1.0.0-beta.11 v1.0.0-rc.1 v1.0.0 v1.0.1-alpha.1 v1.0.1 v1.1.0-rc.1 v1.1.0 v1.10.0 v2.0.0-alpha.1 v2.0.0 v10.0.0-rc.1"
order_errors=$(sh -c '. "$1"; shift; i=0
	for a in "$@"; do
		i=$((i + 1)) j=0
		for b in "$@"; do
			j=$((j + 1))
			if version_gt "$b" "$a"; then gt=1; else gt=0; fi
			if [ "$j" -gt "$i" ]; then want=1; else want=0; fi
			[ "$gt" = "$want" ] || echo "version_gt $b $a: got $gt"
		done
	done' sh "$WORK/versions-only.sh" $ORDER)
expect_eq "version_gt: SemVer precedence" "" "$order_errors"

# --- check: pre-releases ----------------------------------------------------------
# A repository with v1.0.0 (template 2); 1.1.0 and 2.0.0 are iterated with
# pre-releases before their releases.
git init -q -b main "$WORK/pre"
inpre() { sh -c "cd '$WORK/pre' && $SH '$RELEASE' $*"; }
(
	cd "$WORK/pre"
	mkdir deploy
	template 2 >deploy/template.yaml
	changelog '## [1.0.0] - 2026-10-06' '' 'First release.' >CHANGELOG.md
	commit_all v1
	git tag -a v1.0.0 -m v1.0.0
	changelog '## [1.1.0] - unreleased' '' '- A feature.' '' '## [1.0.0] - 2026-10-06' '' 'First release.' >CHANGELOG.md
	commit_all v1.1-wip
)
expect_ok "pre-release: unreleased section" inpre check v1.1.0-alpha.1
expect_ok "pre-release: beta" inpre check v1.1.0-beta.3
expect_ok "pre-release: rc" inpre check v1.1.0-rc.1
expect_fail "pre-release: the release still needs the date" "is dated 'unreleased'" inpre check v1.1.0
expect_fail "pre-release: another version" "is [1.1.0], not [1.2.0]. A pre-release of 1.2.0 needs '## [1.2.0] - unreleased' at the top" inpre check v1.2.0-alpha.1
expect_fail "pre-release: malformed" "A pre-release version looks like" inpre check v1.1.0-alpha.0
out=$(inpre check v1.1.0-alpha.1 2>&1)
printf '%s\n' "$out" | grep -qF 'previous release v1.0.0: 2' && pass || fail "pre-release: not compared with v1.0.0: $out"
(cd "$WORK/pre" && changelog '## [1.1.0] - 2026-10-08' '' '- A feature.' >CHANGELOG.md && commit_all v1.1-dated)
expect_fail "pre-release: a dated section" "is dated '2026-10-08', but v1.1.0-rc.1 is a pre-release" inpre check v1.1.0-rc.1
expect_ok "pre-release: then the release" inpre check v1.1.0
# A template change needs a new MAJOR for a pre-release as for its release.
(cd "$WORK/pre" && template 3 >deploy/template.yaml && changelog '## [1.1.0] - unreleased' '' '- A feature.' >CHANGELOG.md && commit_all v1.1-template)
expect_fail "pre-release: a template change in a minor" "MAJOR version must be higher than v1.0.0's: pre-release v2.0.0 (for example v2.0.0-alpha.1), not v1.1.0-alpha.1" inpre check v1.1.0-alpha.1
(cd "$WORK/pre" && changelog '## [2.0.0] - unreleased' '' '- New template.' >CHANGELOG.md && commit_all v2-alpha1)
expect_ok "pre-release: a new major with a template change" inpre check v2.0.0-alpha.1
(cd "$WORK/pre" && git tag -a v2.0.0-alpha.1 -m x)
expect_ok "pre-release: in its own tag pipeline" inpre check v2.0.0-alpha.1 v2.0.0-alpha.1
# The next pre-release changes the template again: pre-releases are not
# releases, so the guard still compares with v1.0.0 only.
(cd "$WORK/pre" && template 4 >deploy/template.yaml && commit_all v2-alpha2)
out=$(inpre check v2.0.0-alpha.2 2>&1) && pass || fail "pre-release: template changed between pre-releases: $out"
printf '%s\n' "$out" | grep -qF 'previous release v1.0.0: 2' && pass || fail "pre-release: compared with a pre-release: $out"
(cd "$WORK/pre" && git tag -a v2.0.0-alpha.2 -m x && changelog '## [2.0.0] - 2026-10-09' '' '- New template.' >CHANGELOG.md && commit_all v2)
out=$(inpre check v2.0.0 2>&1) && pass || fail "release after pre-releases: $out"
printf '%s\n' "$out" | grep -qF 'previous release v1.0.0: 2' && pass || fail "release after pre-releases: not compared with v1.0.0: $out"
(cd "$WORK/pre" && git tag -a v2.0.0 -m x)
# The release exists: no more pre-releases of it, nor of anything below it.
(cd "$WORK/pre" && changelog '## [2.0.0] - unreleased' '' '- New template.' >CHANGELOG.md && commit_all v2-again)
expect_fail "pre-release of a released version" "release v2.0.0 is not lower than v2.0.0" inpre check v2.0.0-rc.1
(cd "$WORK/pre" && changelog '## [1.5.0] - unreleased' '' '- Old line.' >CHANGELOG.md && commit_all v1.5)
expect_fail "pre-release below the last release" "Pre-release a version above v2.0.0" inpre check v1.5.0-alpha.1
(cd "$WORK/pre" && changelog '## [2.1.0] - unreleased' '' '- Next.' >CHANGELOG.md && commit_all v2.1)
expect_ok "pre-release of the next minor" inpre check v2.1.0-alpha.1
(cd "$WORK/pre" && template 5 >deploy/template.yaml && commit_all v2.1-template)
expect_fail "pre-release of a minor, template changed since the last release" "from 4 (v2.0.0, the last release) to 5" inpre check v2.1.0-alpha.1

# --- check: the Helm chart (deploy/chart) ----------------------------------------
# chart VERSION APPVERSION: a chart with a copy of the template.
chart() {
	mkdir -p deploy/chart/files
	printf '%s\n' 'apiVersion: v2' 'name: app' "version: $1" "appVersion: \"$2\"" >deploy/chart/Chart.yaml
	cp deploy/template.yaml deploy/chart/files/template.yaml
}
git init -q -b main "$WORK/chart"
(
	cd "$WORK/chart"
	mkdir deploy
	template 1 >deploy/template.yaml
	changelog '## [0.2.0] - 2026-01-02' '' 'Chart.' >CHANGELOG.md
	chart 0.2.0 v0.2.0
	commit_all with-chart
)
incharts() { sh -c "cd '$WORK/chart' && $SH '$RELEASE' $*"; }
expect_ok "chart: version and copy match" incharts check v0.2.0
(cd "$WORK/chart" && chart 0.1.0 v0.1.0 && commit_all old-chart-version)
expect_fail "chart: version of another release" "has version '0.1.0' and appVersion 'v0.1.0'; set them to 0.2.0 and v0.2.0" incharts check v0.2.0
(cd "$WORK/chart" && chart 0.2.0 0.2.0 && commit_all appversion-without-v)
expect_fail "chart: appVersion without the v" "appVersion '0.2.0'" incharts check v0.2.0
(cd "$WORK/chart" && chart 0.2.0 v0.2.0 && template 2 >deploy/template.yaml && commit_all template-not-copied)
expect_fail "chart: template not copied" "Run 'make chart'" incharts check v0.2.0
(cd "$WORK/chart" && rm deploy/chart/files/template.yaml && commit_all copy-missing)
expect_fail "chart: copy missing" "is not a copy of deploy/template.yaml" incharts check v0.2.0
(cd "$WORK/chart" && chart 0.2.0 v0.2.0 && commit_all fixed)
expect_ok "chart: fixed" incharts check v0.2.0
# The gitops profile's RBAC is copied into the chart too, once it exists.
(cd "$WORK/chart" && printf 'kind: Template\n' >deploy/rbac-gitops.yaml && commit_all rbac-not-copied)
expect_fail "chart: rbac-gitops.yaml not copied" "deploy/chart/files/rbac-gitops.yaml at" incharts check v0.2.0
(cd "$WORK/chart" && cp deploy/rbac-gitops.yaml deploy/chart/files/rbac-gitops.yaml && commit_all rbac-copied)
expect_ok "chart: rbac-gitops.yaml copied" incharts check v0.2.0
notes=$(incharts notes v0.2.0)
printf '%s\n' "$notes" | grep -qF 'the Helm chart `deploy/chart` of this tag' && pass || fail "notes: no chart line: $notes"
# A pre-release's chart names the pre-release itself, not its release nor
# another pre-release: the chart refuses images of other versions by tag.
(cd "$WORK/chart" && changelog '## [0.3.0] - unreleased' '' 'Next.' >CHANGELOG.md && chart 0.3.0-rc.1 v0.3.0-rc.1 && commit_all chart-rc1)
expect_ok "chart: a pre-release" incharts check v0.3.0-rc.1
expect_fail "chart: another pre-release" "has version '0.3.0-rc.1' and appVersion 'v0.3.0-rc.1'; set them to 0.3.0-rc.2 and v0.3.0-rc.2" incharts check v0.3.0-rc.2
(cd "$WORK/chart" && chart 0.3.0 v0.3.0 && commit_all chart-final-version)
expect_fail "chart: the release's version for a pre-release" "set them to 0.3.0-rc.1 and v0.3.0-rc.1" incharts check v0.3.0-rc.1

# --- notes -----------------------------------------------------------------------
notes=$(IMAGE=quay.io/example/app $SH "$RELEASE" notes v1.0.0 v1.0.0)
expect_eq "notes: install lines first" 'From a clone: `git checkout v1.0.0 && make deploy` (first install) or `make upgrade`.' \
	"$(printf '%s\n' "$notes" | head -1)"
expect_eq "notes: CHANGELOG section last" "First release." "$(printf '%s\n' "$notes" | tail -1)"
printf '%s\n' "$notes" | grep -qF 'Image: `quay.io/example/app:v1.0.0`' && pass || fail "notes: image line missing: $notes"
printf '%s\n' "$notes" | grep -qF '(template revision 2)' && pass || fail "notes: template revision missing: $notes"
printf 'echo installer\n' >"$WORK/install.sh"
notes=$(INSTALLER="$WORK/install.sh" INSTALLER_URL=https://example.com/v1.0.0/install.sh $SH "$RELEASE" notes v1.0.0 v1.0.0)
sum=$( (sha256sum "$WORK/install.sh" 2>/dev/null || shasum -a 256 "$WORK/install.sh") | cut -d' ' -f1)
printf '%s\n' "$notes" | grep -qF "install.sh SHA-256: \`$sum\`" && pass || fail "notes: installer checksum missing: $notes"
printf '%s\n' "$notes" | grep -qxF 'curl -fsSLO https://example.com/v1.0.0/install.sh' && pass || fail "notes: download command missing"
printf '%s\n' "$notes" | grep -q 'curl.*| *bash' && fail "notes: piping curl into bash" || pass
# The copy-paste block opens the notes; the CHANGELOG section (with its
# ### headings) follows the --- separator, however long it is.
(
	git init -q -b main "$WORK/notes"
	cd "$WORK/notes"
	mkdir deploy
	template 3 >deploy/template.yaml
	changelog '## [1.1.0] - 2026-11-18' '' '### Added' '' '- A feature.' '' '### Fixed' '' '- A fix.' >CHANGELOG.md
	commit_all v1.1.0
)
notes=$(cd "$WORK/notes" && INSTALLER="$WORK/install.sh" INSTALLER_URL=https://example.com/v1.1.0/install.sh \
	IMAGE=quay.io/example/app $SH "$RELEASE" notes v1.1.0 HEAD)
expect_eq "notes: the install block opens them" \
	'**Install or upgrade without a clone** (needs `oc`, logged in as cluster-admin, and `curl`):' \
	"$(printf '%s\n' "$notes" | head -1)"
line_of() { printf '%s\n' "$notes" | grep -nxF -- "$1" | head -1 | cut -d: -f1; }
first_heading=$(printf '%s\n' "$notes" | grep -n '^### ' | head -1 | cut -d: -f1)
for l in 'curl -fsSLO https://example.com/v1.1.0/install.sh' "install.sh SHA-256: \`$(
	(sha256sum "$WORK/install.sh" 2>/dev/null || shasum -a 256 "$WORK/install.sh") | cut -d' ' -f1
)\`" 'Image: `quay.io/example/app:v1.1.0`' '---'; do
	n=$(line_of "$l")
	[ -n "$n" ] && [ -n "$first_heading" ] && [ "$n" -lt "$first_heading" ] && pass ||
		fail "notes: '$l' (line ${n:-none}) is not before the first ### heading (line ${first_heading:-none}): $notes"
done
expect_eq "notes: the section ends them" "- A fix." "$(printf '%s\n' "$notes" | tail -1)"
expect_fail "notes for a missing section" "no section [9.9.9]" $SH "$RELEASE" notes v9.9.9 v1.0.0
# A pre-release: a line saying so, then the usual notes with its release's
# section and its own image and tag.
notes=$(cd "$WORK/pre" && INSTALLER="$WORK/install.sh" INSTALLER_URL=https://example.com/v2.0.0-alpha.2/install.sh \
	IMAGE=quay.io/example/app $SH "$RELEASE" notes v2.0.0-alpha.2 v2.0.0-alpha.2)
expect_eq "notes: a pre-release says so first" \
	'**Pre-release of 2.0.0, not for general use.** It is for testing 2.0.0 before its release; the notes below are those of 2.0.0 so far. `:latest` and `:v2` do not move to it, and the app'"'"'s update check never offers it.' \
	"$(printf '%s\n' "$notes" | head -1)"
printf '%s\n' "$notes" | grep -qF 'Image: `quay.io/example/app:v2.0.0-alpha.2`' && pass || fail "notes: pre-release image line: $notes"
printf '%s\n' "$notes" | grep -qF 'git checkout v2.0.0-alpha.2 && make deploy' && pass || fail "notes: pre-release checkout line: $notes"
printf '%s\n' "$notes" | grep -qxF 'curl -fsSLO https://example.com/v2.0.0-alpha.2/install.sh' && pass || fail "notes: pre-release download: $notes"
expect_eq "notes: the release's section ends them" "- New template." "$(printf '%s\n' "$notes" | tail -1)"
notes=$(cd "$WORK/pre" && $SH "$RELEASE" notes v2.0.0 v2.0.0)
printf '%s\n' "$notes" | grep -qF 'Pre-release' && fail "notes: a release reads as a pre-release: $notes" || pass

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
# A pre-release never moves :vN or :latest, not even a missing one.
for name in latest v2; do
	expect_eq "$name, pre-release, tag missing" keep "$(sm $name v2.1.0-rc.1 -)"
	expect_eq "$name, pre-release, older current" keep "$(sm $name v2.1.0-rc.1 v2.0.0)"
	expect_eq "$name, pre-release, unlabelled current" keep "$(sm $name v2.1.0-rc.1 2cdcb42d)"
done
expect_eq "a release above its pre-release" move "$(sm v2 v2.1.0 v2.1.0-rc.1)"
expect_eq "a pre-release label is older than its release" move "$(sm latest v2.1.0 v2.1.0-rc.1)"

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
digest) if [ "$2" = --tarball ]; then echo "sha256:$(cksum <"$3" | cut -d' ' -f1)"; else resolve "$2"; fi ;;
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

# A pre-release publishes its own tag (written once) and nothing else moves.
config v2.2.0-rc.1 "$SHA2" >"$WORK/image.tar"
printf 'rc\n' >>"$WORK/image.tar"
latest_before=$(tag_digest latest) v2_before=$(tag_digest v2)
expect_ok "publish a pre-release" publish v2.2.0-rc.1
expect_eq "the pre-release tag is this image" "sha256:$(cksum <"$WORK/image.tar" | cut -d' ' -f1)" "$(tag_digest v2.2.0-rc.1)"
expect_eq ":latest not moved by a pre-release" "$latest_before" "$(tag_digest latest)"
expect_eq ":v2 not moved by a pre-release" "$v2_before" "$(tag_digest v2)"
expect_eq "commit tag still immutable" "$new" "$(tag_digest 22222222)"
expect_ok "retried publish of a pre-release" publish v2.2.0-rc.1
expect_eq "a pre-release is pushed once" 1 "$(grep -c '^push .*:v2.2.0-rc.1$' "$REG/calls")"
seed v2.2.0-rc.2 "$(config v2.2.0-rc.2 3333333333333333333333333333333333333333)"
expect_fail "a pre-release tag from another commit" "already exists (built from 3333333333333333333333333333333333333333)" publish v2.2.0-rc.2
calls_before=$(wc -l <"$REG/calls")
expect_fail "promote-latest refuses a pre-release" "v2.2.0-rc.1 is a pre-release; :latest moves to releases (vX.Y.Z) only" promote v2.2.0-rc.1
expect_eq ":latest not promoted to a pre-release" "$latest_before" "$(tag_digest latest)"
expect_eq "promote-latest of a pre-release calls no crane" "$calls_before" "$(wc -l <"$REG/calls")"
expect_fail "publish refuses a malformed pre-release" "A pre-release version looks like" publish v2.2.0-rc.01
# Into an empty repository: still no :latest and no :vN.
reset_registry
config v1.0.0-alpha.1 "$SHA2" >"$WORK/image.tar"
expect_ok "publish a pre-release to an empty repository" publish v1.0.0-alpha.1
[ -f "$REG/tags/v1.0.0-alpha.1" ] && [ ! -e "$REG/tags/latest" ] && [ ! -e "$REG/tags/v1" ] && pass ||
	fail "a pre-release in an empty repository: tags $(ls "$REG/tags" | tr '\n' ' ')"

# Tags are compared literally: v2x0x1 is not v2.0.1 (a regex dot would match it).
reset_registry
seed v2x0x1 "$(config v2x0x1 3333333333333333333333333333333333333333)"
config v2.0.1 "$SHA2" >"$WORK/image.tar"
expect_ok "a near-matching tag is another tag" publish v2.0.1
[ -f "$REG/tags/v2.0.1" ] && pass || fail "v2.0.1 was not pushed next to v2x0x1"

# A new repository: everything is published.
reset_registry
config v1.0.0 "$SHA2" >"$WORK/image.tar"
expect_ok "publish to an empty repository" publish v1.0.0
expect_eq "latest created" "$(tag_digest v1.0.0)" "$(tag_digest latest)"

# --- publish-main: write-once commit tag, :main only from the tip ---------------
SHA3=3333333333333333333333333333333333333333
mainpub() { # SHA SHORT TIP
	env FAKE_REGISTRY="$REG" CRANE="$CRANE" IMAGE=quay.io/example/app IMAGE_TARBALL="$WORK/image.tar" \
		CI_COMMIT_SHA="$1" CI_COMMIT_SHORT_SHA="$2" CI_DEFAULT_BRANCH=main MAIN_TIP="$3" $SH "$RELEASE" publish-main
}
tarball_digest() { echo "sha256:$(cksum <"$WORK/image.tar" | cut -d' ' -f1)"; }
commit_tag_pushes() { grep -c "^push .*:$1\$" "$REG/calls" || true; }
reset_registry
config 22222222 "$SHA2" >"$WORK/image.tar"
expect_ok "publish-main" mainpub "$SHA2" 22222222 "$SHA2"
first=$(tarball_digest)
expect_eq "the commit tag is this pipeline's image" "$first" "$(tag_digest 22222222)"
expect_eq ":main is this pipeline's image" "$first" "$(tag_digest main)"
expect_ok "publish-main retried with the same image" mainpub "$SHA2" 22222222 "$SHA2"
expect_eq "the commit tag is pushed once" 1 "$(commit_tag_pushes 22222222)"
expect_eq ":main still this pipeline's image" "$first" "$(tag_digest main)"

# Another image of the same commit under the commit tag (a rebuilt
# build-main, or a workstation push before `make push` used dev- tags) is
# not adopted: the job stops and :main stays where it was.
printf 'rebuilt\n' >>"$WORK/image.tar"
expect_fail "publish-main with a rebuild of the same commit" "adopts no image it did not build" mainpub "$SHA2" 22222222 "$SHA2"
expect_eq "the commit tag is written once" "$first" "$(tag_digest 22222222)"
expect_eq ":main not moved by a refused job" "$first" "$(tag_digest main)"
SHA5=5555555555555555555555555555555555555555
seed 55555555 "$(config 55555555 "$SHA5")"
config 55555555 "$SHA5" >"$WORK/image.tar"
expect_fail "a workstation push under the commit tag" "adopts no image it did not build" mainpub "$SHA5" 55555555 "$SHA5"
expect_eq ":main not moved to the workstation image" "$first" "$(tag_digest main)"
expect_eq "the workstation image is left for a person to check" "sha256:seed-55555555" "$(tag_digest 55555555)"
# A workstation image labelled with a release version is not a release
# image unless :vX.Y.Z is that same image.
seed v7.0.0 "$(config v7.0.0 "$SHA5")"
seed 55555555 "$(config v7.0.0 "$SHA5")"
expect_fail "a commit tag that claims a release" "adopts no image it did not build" mainpub "$SHA5" 55555555 "$SHA5"
expect_eq ":main still not moved" "$first" "$(tag_digest main)"

config 33333333 "$SHA3" >"$WORK/image.tar"
expect_ok "publish-main of a commit that is no longer the tip" mainpub "$SHA3" 33333333 "$SHA2"
expect_eq ":main not moved back" "$first" "$(tag_digest main)"
[ -f "$REG/tags/33333333" ] && pass || fail "the commit tag of a non-tip commit was not published"
seed 44444444 "$(config 44444444 "$SHA3")"
expect_fail "a commit tag from another commit" "commit tags are written once" mainpub "$SHA2" 44444444 "$SHA2"

# The tag pipeline of the same commit published first: the commit tag is
# its release image (the same digest as :v6.0.0) and is kept; :main is
# still main's own build.
SHA6=6666666666666666666666666666666666666666
config v6.0.0 "$SHA6" >"$WORK/image.tar"
expect_ok "release before main" env FAKE_REGISTRY="$REG" CRANE="$CRANE" IMAGE=quay.io/example/app IMAGE_TARBALL="$WORK/image.tar" \
	CI_COMMIT_SHA="$SHA6" CI_COMMIT_SHORT_SHA=66666666 $SH "$RELEASE" publish v6.0.0
release6=$(tag_digest v6.0.0)
expect_eq "the release wrote the commit tag" "$release6" "$(tag_digest 66666666)"
config 66666666 "$SHA6" >"$WORK/image.tar"
expect_ok "main after the release of its commit" mainpub "$SHA6" 66666666 "$SHA6"
expect_eq "the commit tag stays the release image" "$release6" "$(tag_digest 66666666)"
expect_eq ":main is main's own build, not the release image" "$(tarball_digest)" "$(tag_digest main)"
# The same with a pre-release tagged on main.
SHA7=7777777777777777777777777777777777777777
config v7.0.0-beta.1 "$SHA7" >"$WORK/image.tar"
expect_ok "pre-release before main" env FAKE_REGISTRY="$REG" CRANE="$CRANE" IMAGE=quay.io/example/app IMAGE_TARBALL="$WORK/image.tar" \
	CI_COMMIT_SHA="$SHA7" CI_COMMIT_SHORT_SHA=77777777 $SH "$RELEASE" publish v7.0.0-beta.1
beta7=$(tag_digest v7.0.0-beta.1)
config 77777777 "$SHA7" >"$WORK/image.tar"
expect_ok "main after the pre-release of its commit" mainpub "$SHA7" 77777777 "$SHA7"
expect_eq "the commit tag stays the pre-release image" "$beta7" "$(tag_digest 77777777)"

# Back to commit 22222222's image for the release-after-main check below.
reset_registry
config 22222222 "$SHA2" >"$WORK/image.tar"
expect_ok "publish-main again" mainpub "$SHA2" 22222222 "$SHA2"
first=$(tag_digest 22222222)
expect_fail "unreadable tip" "Cannot read the tip" env FAKE_REGISTRY="$REG" CRANE="$CRANE" IMAGE=quay.io/example/app \
	IMAGE_TARBALL="$WORK/image.tar" CI_COMMIT_SHA="$SHA3" CI_COMMIT_SHORT_SHA=33333333 CI_DEFAULT_BRANCH=main \
	CI_SERVER_URL=http://127.0.0.1:9 CI_PROJECT_PATH=x CI_JOB_TOKEN=x $SH "$RELEASE" publish-main
# A release after main: the commit tag stays main's build.
config v5.0.0 "$SHA2" >"$WORK/image.tar"
expect_ok "release after main" publish v5.0.0
expect_eq "release keeps main's commit tag" "$first" "$(tag_digest 22222222)"

# Registry errors stop the release.
expect_fail "registry unreachable" "Cannot list the tags" env FAKE_LS_ERROR="dial tcp: i/o timeout" FAKE_REGISTRY="$REG" CRANE="$CRANE" \
	IMAGE=quay.io/example/app IMAGE_TARBALL="$WORK/image.tar" CI_COMMIT_SHA="$SHA2" $SH "$RELEASE" publish v1.0.1
expect_fail "IMAGE with a tag" "without a tag" env FAKE_REGISTRY="$REG" CRANE="$CRANE" IMAGE=quay.io/example/app:latest \
	CI_COMMIT_SHA="$SHA2" $SH "$RELEASE" publish v1.0.1

# --- make build / make push: local builds never take a CI tag -------------------
# The Makefile, in a repository of its own, with a fake container runtime.
if command -v make >/dev/null 2>&1; then
	MK="$WORK/make"
	git init -q -b main "$MK"
	cp "$HERE/../Makefile" "$MK/Makefile"
	printf '#!/bin/sh\necho "$*" >>"%s/runtime-calls"\n' "$WORK" >"$WORK/runtime"
	chmod +x "$WORK/runtime"
	(cd "$MK" && echo x >file && commit_all mk)
	MKSHA=$(cd "$MK" && git rev-parse HEAD | cut -c1-8)
	mk() { (cd "$MK" && make --no-print-directory RUNTIME="$WORK/runtime" IMAGE=quay.io/example/app "$@") }
	rm -f "$WORK/runtime-calls"
	expect_ok "make push: clean tree" mk push
	expect_eq "make push: pushes dev-<commit>" "push quay.io/example/app:dev-$MKSHA" "$(cat "$WORK/runtime-calls")"
	rm -f "$WORK/runtime-calls"
	expect_ok "make push: a commit-like TAG" mk push TAG=1234abcd
	expect_eq "make push: a commit-like TAG is not pushed" "push quay.io/example/app:dev-$MKSHA" "$(cat "$WORK/runtime-calls")"
	rm -f "$WORK/runtime-calls"
	expect_ok "make push: a TAG of your own" mk push TAG=my-test
	expect_eq "make push: a TAG of your own is pushed too" "push quay.io/example/app:dev-$MKSHA
push quay.io/example/app:my-test" "$(cat "$WORK/runtime-calls")"
	build=$(mk -n build)
	printf '%s\n' "$build" | grep -qF -- "-t quay.io/example/app:dev-$MKSHA " && pass || fail "make build: not tagged dev-<commit>: $build"
	echo y >>"$MK/file"
	rm -f "$WORK/runtime-calls"
	expect_fail "make push: dirty tree" "uncommitted changes" mk push
	[ ! -e "$WORK/runtime-calls" ] && pass || fail "make push pushed from a dirty tree: $(cat "$WORK/runtime-calls")"
	build=$(mk -n build)
	printf '%s\n' "$build" | grep -qF -- "-t quay.io/example/app:dev-$MKSHA-dirty " && pass || fail "make build: a dirty build is not tagged -dirty: $build"
	(cd "$MK" && git checkout -q file && echo new >untracked)
	expect_fail "make push: untracked files" "uncommitted changes" mk push
	rm "$MK/untracked"
	rm -f "$WORK/runtime-calls"
	expect_ok "make push: a pre-release TAG" mk push TAG=v1.2.3-rc.1
	expect_eq "make push: a pre-release TAG is not pushed" "push quay.io/example/app:dev-$MKSHA" "$(cat "$WORK/runtime-calls")"
	# On a pre-release tag, TAG and the build's version default to it.
	(cd "$MK" && git tag -a v1.2.3-rc.1 -m x)
	build=$(mk -n build)
	printf '%s\n' "$build" | grep -qF -- "--build-arg VERSION=v1.2.3-rc.1 " && pass || fail "make build: the pre-release tag is not the version: $build"
	mk help | grep -F 'TAG        = v1.2.3-rc.1' >/dev/null && pass || fail "make help: TAG is not the pre-release tag: $(mk help | grep 'TAG ')"
	(cd "$MK" && git tag -d v1.2.3-rc.1 >/dev/null && git tag -a v1.2.3-preview.1 -m x)
	mk help | grep -F 'TAG        = (none' >/dev/null && pass || fail "make help: a tag that is not a release is TAG: $(mk help | grep 'TAG ')"
	(cd "$MK" && git tag -d v1.2.3-preview.1 >/dev/null)
else
	echo "SKIP: make is not installed; the make build/push checks did not run" >&2
fi

# --- wait-image (GitHub release) ------------------------------------------------
waitimg() { env FAKE_REGISTRY="$REG" CRANE="$CRANE" IMAGE=quay.io/example/app WAIT_SECONDS=1 WAIT_INTERVAL=1 $SH "$RELEASE" wait-image "$@"; }
V1COMMIT=$(git rev-parse 'v1.0.0^{commit}')
reset_registry
expect_fail "wait-image: never published" "did not appear within 1 seconds" waitimg v1.0.0
seed v1.0.0 "$(config v1.0.0 "$SHA3")"
expect_fail "wait-image: built from another commit" "was built from $SHA3" waitimg v1.0.0
seed v1.0.0 "$(config v1.0.0 "$V1COMMIT")"
expect_ok "wait-image: published from the tag's commit" waitimg v1.0.0
expect_fail "wait-image: unknown tag" "not in this repository" waitimg v7.7.7
git tag -a v1.1.0-rc.1 -m x 'v1.0.0^{commit}'
seed v1.1.0-rc.1 "$(config v1.1.0-rc.1 "$V1COMMIT")"
expect_ok "wait-image: a pre-release" waitimg v1.1.0-rc.1
git tag -d v1.1.0-rc.1 >/dev/null

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

# Pre-releases: on main or an integ/* branch; releases on main only.
git checkout -q -b integ/2.1.0
changelog '## [2.1.0] - unreleased' '' '- Next.' '' '## [2.0.0] - 2026-10-07' '' '- SeaweedFS.' >CHANGELOG.md
commit_all integ-2.1
expect_fail "tag: pre-release, branch not on the remote" "Cannot fetch integ/2.1.0 from origin" $SH "$RELEASE" tag v2.1.0-alpha.1
git push -q origin integ/2.1.0
expect_fail "tag: a release on integ/*" "Releases are tagged on main; this checkout is on integ/2.1.0" $SH "$RELEASE" tag v2.1.0
expect_fail "tag: a malformed pre-release" "Usage: make release VERSION=vX.Y.Z, or VERSION=vX.Y.Z-alpha.N" $SH "$RELEASE" tag v2.1.0-dev.1
out=$($SH "$RELEASE" tag v2.1.0-alpha.1 2>&1) && pass || fail "tag v2.1.0-alpha.1 on integ/2.1.0: $out"
expect_eq "pre-release: annotated tag" "tag Pre-release v2.1.0-alpha.1" "$(git cat-file -t v2.1.0-alpha.1) $(git tag -l --format='%(contents:subject)' v2.1.0-alpha.1)"
printf '%s\n' "$out" | grep -qF 'git push origin v2.1.0-alpha.1' && pass || fail "tag: pre-release push command: $out"
[ -z "$(git ls-remote --tags origin refs/tags/v2.1.0-alpha.1)" ] && pass || fail "tag: the pre-release was pushed"
git commit -q --allow-empty -m integ-ahead
expect_fail "tag: pre-release, integ/* not pushed" "integ/2.1.0 is not the same commit as origin/integ/2.1.0" $SH "$RELEASE" tag v2.1.0-alpha.2
git checkout -q -b topic-2
expect_fail "tag: pre-release on another branch" "Pre-releases are tagged on main or an integ/* branch; this checkout is on topic-2" $SH "$RELEASE" tag v2.1.0-alpha.2
git checkout -q main
git merge -q --ff-only integ/2.1.0
git push -q origin main
expect_ok "tag: pre-release on main" $SH "$RELEASE" tag v2.1.0-beta.1
expect_fail "tag: pre-release pushed already" "Tag v2.1.0-alpha.1 already exists" sh -c "git push -q origin v2.1.0-alpha.1 && git tag -d v2.1.0-alpha.1 >/dev/null && $SH '$RELEASE' tag v2.1.0-alpha.1"

# --- gitlab-release: the payload, with a fake curl ------------------------------
mkdir -p "$WORK/curlbin"
cat >"$WORK/curlbin/curl" <<'FAKE'
#!/bin/sh
# Fake curl: no Release exists yet (404); a POST body is saved.
for a in "$@"; do
	case "$a" in
	%{http_code}) printf 404; exit 0 ;;
	@*) cp "${a#@}" "$FAKE_RELEASE_BODY" ;;
	esac
done
FAKE
chmod +x "$WORK/curlbin/curl"
printf 'echo installer\n' >"$WORK/install.sh"
expect_ok "gitlab-release" env PATH="$WORK/curlbin:$PATH" FAKE_RELEASE_BODY="$WORK/release.json" \
	CI_API_V4_URL=https://gitlab.example.com/api/v4 CI_PROJECT_ID=7 CI_JOB_TOKEN=x CI_PROJECT_URL=https://gitlab.example.com/g/app \
	INSTALLER="install.sh" INSTALLER_JOB_ID=12345 sh -c "cd '$REPO' && cp '$WORK/install.sh' install.sh && $SH '$RELEASE' gitlab-release v2.0.0; rc=\$?; rm -f install.sh; exit \$rc"
grep -qF '"url":"https://gitlab.example.com/g/app/-/jobs/12345/artifacts/raw/install.sh"' "$WORK/release.json" && pass || fail "gitlab-release: the installer link is not pinned to the job: $(cat "$WORK/release.json")"
grep -qF "$sum" "$WORK/release.json" && pass || fail "gitlab-release: the installer checksum is missing"
grep -qF 'releases/v2.0.0/downloads/install.sh' "$WORK/release.json" && pass || fail "gitlab-release: the permanent download URL is missing from the notes"
# The canonical project is not public: its notes download from the GitHub mirror.
expect_ok "gitlab-release (canonical project)" env PATH="$WORK/curlbin:$PATH" FAKE_RELEASE_BODY="$WORK/release.json" \
	CI_API_V4_URL=https://gitlab.example.com/api/v4 CI_PROJECT_ID=7 CI_JOB_TOKEN=x CI_PROJECT_URL=https://gitlab.com/redhat/ai/rhoai-dashboard-team/rhoai-nightly-updater \
	INSTALLER="install.sh" INSTALLER_JOB_ID=12345 sh -c "cd '$REPO' && cp '$WORK/install.sh' install.sh && $SH '$RELEASE' gitlab-release v2.0.0; rc=\$?; rm -f install.sh; exit \$rc"
grep -qF 'curl -fsSLO https://github.com/DaoDaoNoCode/rhoai-nightly-updater/releases/download/v2.0.0/install.sh' "$WORK/release.json" && pass || fail "gitlab-release: the canonical project's notes do not download from the mirror: $(cat "$WORK/release.json")"
# A pre-release: GitLab Releases have no pre-release flag, so the name and
# the first line of the notes say it.
expect_ok "gitlab-release (pre-release)" env PATH="$WORK/curlbin:$PATH" FAKE_RELEASE_BODY="$WORK/release.json" \
	CI_API_V4_URL=https://gitlab.example.com/api/v4 CI_PROJECT_ID=7 CI_JOB_TOKEN=x CI_PROJECT_URL=https://gitlab.example.com/g/app \
	INSTALLER="install.sh" INSTALLER_JOB_ID=12345 sh -c "cd '$REPO' && cp '$WORK/install.sh' install.sh && $SH '$RELEASE' gitlab-release v2.1.0-beta.1; rc=\$?; rm -f install.sh; exit \$rc"
grep -qF '"tag_name":"v2.1.0-beta.1","name":"v2.1.0-beta.1 (pre-release)","description":"**Pre-release of 2.1.0, not for general use.**' "$WORK/release.json" && pass ||
	fail "gitlab-release: the pre-release is not named as one: $(cat "$WORK/release.json")"
grep -qF 'releases/v2.1.0-beta.1/downloads/install.sh' "$WORK/release.json" && pass || fail "gitlab-release: pre-release download URL: $(cat "$WORK/release.json")"
grep -qF '"name":"v2.0.0 (pre-release)"' "$WORK/release.json" && fail "gitlab-release: wrong release" || pass
expect_fail "gitlab-release needs the installer job" "INSTALLER_JOB_ID" env PATH="$WORK/curlbin:$PATH" FAKE_RELEASE_BODY="$WORK/release.json" \
	CI_API_V4_URL=https://gitlab.example.com/api/v4 CI_PROJECT_ID=7 CI_JOB_TOKEN=x CI_PROJECT_URL=https://gitlab.example.com/g/app \
	INSTALLER="$WORK/install.sh" $SH "$RELEASE" gitlab-release v2.0.0

# --- json_string (GitLab Release body) ---------------------------------------------
sed -n '/^json_string()/,/^}/p' "$RELEASE" >"$WORK/json-only.sh"
json=$(sh -c '. "$1"; json_string "$2"' sh "$WORK/json-only.sh" "$(printf 'a "b"\\c\n\td')")
expect_eq "json_string" '"a \"b\"\\c\n\td"' "$json"

printf '%d passed, %d failed\n' "$PASSED" "$FAILED"
[ "$FAILED" -eq 0 ]
