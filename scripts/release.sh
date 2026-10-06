#!/bin/sh
# Release tooling shared by `make release`, the GitLab release pipeline and
# the GitHub release workflow. POSIX sh: it runs under bash, dash and
# busybox ash. scripts/test-release.sh tests it.
#
# Versions are vMAJOR.MINOR.PATCH. MAJOR changes when the deploy contract
# changes (TEMPLATE_REVISION in deploy/template.yaml, RBAC, anything that
# needs the template re-applied), MINOR for features, PATCH for fixes.
#
# Usage:
#   release.sh check VERSION [REF]     the release guard (default REF: HEAD)
#   release.sh notes VERSION [REF]     release notes from CHANGELOG.md (with
#                                      the installer's SHA-256 when INSTALLER
#                                      names it)
#   release.sh installer VERSION [REF] the install.sh release asset on stdout:
#                                      REF's scripts/install.sh with REF's
#                                      deploy/template.yaml embedded
#   release.sh tag VERSION             `make release`: guard, then an annotated
#                                      tag; prints (never runs) the push commands
#   release.sh publish VERSION         CI: push the image tarball as :VERSION,
#                                      then move :vMAJOR and :latest
#   release.sh promote-latest VERSION  CI, manual: move :latest to VERSION
#   release.sh publish-main            CI, main: push the image tarball as the
#                                      commit tag (written once) and move :main
#   release.sh wait-image VERSION      wait (bounded) until IMAGE:VERSION exists,
#                                      built from the commit of tag VERSION
#   release.sh gitlab-release VERSION  CI: create the GitLab Release
#   release.sh should-move NAME NEW CURRENT
#                                      whether tag NAME (latest or vN) moves
#                                      from CURRENT ("-" = no such tag, other
#                                      non-release text = unlabelled) to NEW
#
# Environment for notes: IMAGE, INSTALLER (the generated install.sh) and
# INSTALLER_URL (where it is downloaded from). For installer: IMAGE.
# Environment for publish and promote-latest: IMAGE (repository, no tag),
# IMAGE_TARBALL (publish), CI_COMMIT_SHA, CI_COMMIT_SHORT_SHA, CRANE (the
# crane binary, default crane; it must be logged in to the registry).
# For tag: RELEASE_BRANCH (default main), RELEASE_REMOTE (default origin),
# RELEASE_REMOTES (remotes to print push commands for, default
# "origin github").
set -eu

info() { printf '%s\n' "$*" >&2; }
die() {
	printf 'ERROR: %s\n' "$*" >&2
	exit 1
}

# --- Versions ---------------------------------------------------------------

is_release() {
	printf '%s\n' "$1" | grep -Eq '^v(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})$'
}

major() {
	v=${1#v}
	printf '%s\n' "${v%%.*}"
}

# version_gt A B: A is a higher release than B (both vX.Y.Z).
version_gt() {
	a=${1#v}
	b=${2#v}
	a1=${a%%.*} a=${a#*.}
	b1=${b%%.*} b=${b#*.}
	a2=${a%%.*} a3=${a#*.}
	b2=${b%%.*} b3=${b#*.}
	if [ "$a1" -ne "$b1" ]; then [ "$a1" -gt "$b1" ]; return; fi
	if [ "$a2" -ne "$b2" ]; then [ "$a2" -gt "$b2" ]; return; fi
	[ "$a3" -gt "$b3" ]
}

# previous_release VERSION: the highest vX.Y.Z tag lower than VERSION.
previous_release() {
	best=""
	for t in $(git tag -l 'v*'); do
		is_release "$t" || continue
		version_gt "$1" "$t" || continue
		if [ -z "$best" ] || version_gt "$t" "$best"; then best=$t; fi
	done
	printf '%s\n' "$best"
}

# --- Repository contents ------------------------------------------------------

# template_revision REF: TEMPLATE_REVISION of deploy/template.yaml at REF;
# "none" when the template has no such variable, empty when there is no
# template.
template_revision() {
	git cat-file -e "$1:deploy/template.yaml" 2>/dev/null || return 0
	r=$(git show "$1:deploy/template.yaml" |
		sed -n '/name: TEMPLATE_REVISION/{n;s/^ *value: *"\{0,1\}\([^"]*\)"\{0,1\}.*/\1/p;}' | head -1)
	printf '%s\n' "${r:-none}"
}

# changelog_top REF: "VERSION DATE" of the first "## [X.Y.Z] - DATE" heading
# of CHANGELOG.md at REF, skipping "## [Unreleased]".
changelog_top() {
	git cat-file -e "$1:CHANGELOG.md" 2>/dev/null || return 0
	git show "$1:CHANGELOG.md" | awk '
		/^## \[/ {
			h = $0
			sub(/^## \[/, "", h)
			ver = h
			sub(/\].*/, "", ver)
			if (tolower(ver) == "unreleased") next
			d = h
			sub(/^[^]]*\]/, "", d)
			sub(/^[ \t]*-[ \t]*/, "", d)
			sub(/[ \t\r]+$/, "", d)
			print ver, d
			exit
		}'
}

# changelog_section REF X.Y.Z: the body of that version's section, without
# leading and trailing blank lines.
changelog_section() {
	git cat-file -e "$1:CHANGELOG.md" 2>/dev/null || return 0
	git show "$1:CHANGELOG.md" | tr -d '\r' | awk -v ver="$2" '
		/^## \[/ {
			if (found) exit
			h = $0
			sub(/^## \[/, "", h)
			sub(/\].*/, "", h)
			if (h == ver) found = 1
			next
		}
		found { lines[++n] = $0 }
		END {
			first = 1
			while (first <= n && lines[first] ~ /^[ \t]*$/) first++
			last = n
			while (last >= first && lines[last] ~ /^[ \t]*$/) last--
			for (i = first; i <= last; i++) print lines[i]
		}'
}

# --- check --------------------------------------------------------------------

cmd_check() {
	version=$1
	ref=${2:-HEAD}
	is_release "$version" || die "A release version looks like v1.2.3, not '$version'."
	git rev-parse --verify --quiet "$ref^{commit}" >/dev/null || die "$ref is not a commit of this repository."
	plain=${version#v}

	top=$(changelog_top "$ref")
	[ -n "$top" ] || die "CHANGELOG.md at $ref has no release section ('## [$plain] - YYYY-MM-DD')."
	top_version=${top%% *}
	top_date=${top#* }
	[ "$top_version" = "$plain" ] ||
		die "The top release section of CHANGELOG.md is [$top_version], not [$plain]. Add '## [$plain] - YYYY-MM-DD' above it."
	printf '%s\n' "$top_date" | grep -Eq '^[0-9]{4}-[0-9]{2}-[0-9]{2}$' ||
		die "CHANGELOG.md section [$plain] is dated '$top_date'. Replace it with the release date (YYYY-MM-DD) in a commit on main before tagging."
	[ -n "$(changelog_section "$ref" "$plain")" ] || die "CHANGELOG.md section [$plain] is empty."

	rev=$(template_revision "$ref")
	[ -n "$rev" ] || die "$ref has no deploy/template.yaml."
	prev=$(previous_release "$version")
	if [ -z "$prev" ]; then
		info "OK: CHANGELOG.md [$plain] - $top_date; no earlier release tag, so no template check (template revision $rev)."
		return 0
	fi
	prev_rev=$(template_revision "$prev")
	[ -n "$prev_rev" ] || prev_rev=none
	if [ "$rev" != "$prev_rev" ] && [ "$(major "$version")" -le "$(major "$prev")" ]; then
		die "deploy/template.yaml changed TEMPLATE_REVISION from $prev_rev ($prev) to $rev, which needs a full redeploy, so the MAJOR version must be higher than $prev's: release v$(($(major "$prev") + 1)).0.0, not $version."
	fi
	info "OK: CHANGELOG.md [$plain] - $top_date; template revision $rev (previous release $prev: $prev_rev)."
}

# --- notes ------------------------------------------------------------------

cmd_notes() {
	version=$1
	ref=${2:-HEAD}
	is_release "$version" || die "A release version looks like v1.2.3, not '$version'."
	body=$(changelog_section "$ref" "${version#v}")
	[ -n "$body" ] || die "CHANGELOG.md at $ref has no section [${version#v}]."
	rev=$(template_revision "$ref")
	printf '%s\n\n---\n\n' "$body"
	if [ -n "${INSTALLER:-}" ]; then
		[ -f "$INSTALLER" ] || die "No installer at $INSTALLER."
		sum=$(sha256 "$INSTALLER")
		printf '**Install or upgrade without a clone** (needs `oc`, logged in as cluster-admin, and `curl`):\n\n'
		printf '```sh\ncurl -fsSLO %s\n' "${INSTALLER_URL:-<the install.sh asset of this release>}"
		printf 'echo "%s  install.sh" | sha256sum -c -   # macOS: shasum -a 256 -c\n' "$sum"
		printf 'less install.sh   # read it first\nbash install.sh --dry-run\nbash install.sh\n```\n\n'
		printf 'install.sh SHA-256: `%s`\n\n' "$sum"
	fi
	printf 'From a clone: `git checkout %s && make deploy` (first install) or `make upgrade`.\n' "$version"
	printf 'Deployment template: `deploy/template.yaml` of this tag (template revision %s).\n' "$rev"
	if [ -n "${IMAGE:-}" ]; then
		printf 'Image: `%s:%s`\n' "$IMAGE" "$version"
	fi
}

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d' ' -f1
	else
		shasum -a 256 "$1" | cut -d' ' -f1
	fi
}

# --- installer ------------------------------------------------------------------

EMBED_EOF=RHOAI_NIGHTLY_UPDATER_TEMPLATE_EOF

# cmd_installer VERSION [REF]: REF's scripts/install.sh with REF's
# deploy/template.yaml, commit, VERSION and IMAGE embedded, on stdout.
cmd_installer() {
	version=$1
	ref=${2:-HEAD}
	is_release "$version" || die "A release version looks like v1.2.3, not '$version'."
	commit=$(git rev-parse --verify --quiet "$ref^{commit}") || die "$ref is not a commit of this repository."
	image=${IMAGE:-}
	printf '%s\n' "$image" | grep -Eq '^[a-z0-9][a-z0-9._:/-]*$' || die "IMAGE must be set to the image repository (got '$image')."
	git cat-file -e "$commit:scripts/install.sh" 2>/dev/null || die "$ref has no scripts/install.sh."
	git cat-file -e "$commit:deploy/template.yaml" 2>/dev/null || die "$ref has no deploy/template.yaml."
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	git show "$commit:scripts/install.sh" >"$tmp/install.sh"
	git show "$commit:deploy/template.yaml" >"$tmp/template.yaml"
	[ "$(tail -c 1 "$tmp/template.yaml" | od -An -c | tr -d ' ')" = '\n' ] || die "deploy/template.yaml must end with a newline."
	! grep -qx "$EMBED_EOF" "$tmp/template.yaml" || die "deploy/template.yaml contains the line $EMBED_EOF."
	begin=$(grep -n '^# @@EMBEDDED-RELEASE-BEGIN@@$' "$tmp/install.sh" | cut -d: -f1)
	end=$(grep -n '^# @@EMBEDDED-RELEASE-END@@$' "$tmp/install.sh" | cut -d: -f1)
	[ -n "$begin" ] && [ -n "$end" ] && [ "$begin" -lt "$end" ] || die "scripts/install.sh has no embedded-release block."
	head -n "$begin" "$tmp/install.sh"
	printf "EMBEDDED_VERSION='%s'\n" "$version"
	printf "EMBEDDED_COMMIT='%s'\n" "$commit"
	printf "EMBEDDED_IMAGE='%s'\n" "$image"
	printf 'embedded_template() {\n'
	printf "\tcat <<'%s'\n" "$EMBED_EOF"
	cat "$tmp/template.yaml"
	printf '%s\n}\n' "$EMBED_EOF"
	tail -n "+$end" "$tmp/install.sh"
}

# --- tag (make release) -------------------------------------------------------

cmd_tag() {
	version=$1
	branch=${RELEASE_BRANCH:-main}
	remote=${RELEASE_REMOTE:-origin}
	is_release "$version" || die "Usage: make release VERSION=vX.Y.Z (got '$version')."
	[ -z "$(git status --porcelain)" ] || die "The working tree has changes; commit or stash them first."
	current=$(git symbolic-ref --quiet --short HEAD || true)
	[ "$current" = "$branch" ] || die "Releases are tagged on $branch; this checkout is on ${current:-a detached HEAD}."
	git fetch --quiet --tags "$remote" "$branch" || die "Cannot fetch $branch from $remote."
	[ "$(git rev-parse HEAD)" = "$(git rev-parse "$remote/$branch")" ] ||
		die "$branch is not the same commit as $remote/$branch; pull (or push) first."
	! git rev-parse --verify --quiet "refs/tags/$version" >/dev/null || die "Tag $version already exists."
	[ -z "$(git ls-remote --tags "$remote" "refs/tags/$version")" ] || die "Tag $version already exists on $remote."
	cmd_check "$version" HEAD
	git tag -a "$version" -m "Release $version"
	info "Created the annotated tag $version on $(git rev-parse --short=8 HEAD). Nothing was pushed."
	info "Push it to start the release pipeline (GitLab) and the GitHub release:"
	for r in ${RELEASE_REMOTES-origin github}; do
		if git remote get-url "$r" >/dev/null 2>&1; then
			printf '  git push %s %s\n' "$r" "$version"
		else
			printf '  git push %s %s   # no remote named %s in this clone\n' "$r" "$version" "$r"
		fi
	done
	info "To undo before pushing: git tag -d $version"
}

# --- Registry (crane) ---------------------------------------------------------

crane_cmd() {
	${CRANE:-crane} "$@"
}

# list_tags: every tag of IMAGE, one per line. A repository that does not
# exist yet has none; any other error stops the release.
list_tags() {
	if out=$(crane_cmd ls "$IMAGE" 2>&1); then
		printf '%s\n' "$out"
		return 0
	fi
	case "$out" in
	*NAME_UNKNOWN*) return 0 ;;
	esac
	die "Cannot list the tags of $IMAGE: $out"
}

has_tag() {
	printf '%s\n' "$TAGS" | grep -Fqx -- "$1"
}

# image_label REF KEY: a label of the image's config, empty when missing.
image_label() {
	config=$(crane_cmd config "$1") || die "Cannot read the configuration of $1."
	printf '%s\n' "$config" | sed -n "s/.*\"$2\": *\"\([^\"]*\)\".*/\1/p" | head -1
}

version_label() {
	image_label "$1" 'org\.opencontainers\.image\.version'
}

# decide NAME NEW CURRENT: prints "move: <why>" or "keep: <why>".
#   NAME     latest or vN
#   NEW      the version being released
#   CURRENT  the version label of the image NAME points at; "-" when the tag
#            does not exist. Images from before releases carry a commit, not
#            a version: for :latest they count as v1, and any release of the
#            same major line is newer.
decide() {
	name=$1 new=$2 current=$3
	if [ "$current" = "-" ]; then
		echo "move: :$name does not exist yet"
		return
	fi
	if [ "$name" = latest ]; then
		if is_release "$current"; then cur_major=$(major "$current"); else cur_major=1; fi
		if [ "$(major "$new")" != "$cur_major" ]; then
			echo "keep: :latest is on major version $cur_major; moving it to v$(major "$new") is the manual promote-latest job"
			return
		fi
	fi
	if ! is_release "$current"; then
		echo "move: :$name is a build from before releases (${current:-no version label})"
	elif version_gt "$new" "$current"; then
		echo "move: $new is newer than $current"
	else
		echo "keep: :$name is already $current, not older than $new"
	fi
}

cmd_should_move() {
	is_release "$2" || die "NEW must be a release version, not '$2'."
	decide "$1" "$2" "$3"
}

require_publish_env() {
	: "${IMAGE:?IMAGE (the repository, without a tag) is not set}"
	case "$IMAGE" in *@* | */*:*) die "IMAGE must be a repository without a tag or digest, not $IMAGE." ;; esac
}

# move_tag NAME VERSION REF: point NAME at REF when decide says so.
move_tag() {
	name=$1 version=$2 ref=$3
	if has_tag "$name"; then current=$(version_label "$IMAGE:$name"); else current=-; fi
	verdict=$(decide "$name" "$version" "$current")
	case "$verdict" in
	move:*)
		crane_cmd tag "$ref" "$name" >/dev/null
		info ":$name -> $version (${verdict#move: })"
		;;
	*) info ":$name unchanged (${verdict#keep: })" ;;
	esac
}

cmd_publish() {
	version=$1
	is_release "$version" || die "A release version looks like v1.2.3, not '$version'."
	require_publish_env
	: "${CI_COMMIT_SHA:?CI_COMMIT_SHA is not set}"
	TAGS=$(list_tags)
	if has_tag "$version"; then
		built_from=$(image_label "$IMAGE:$version" 'org\.opencontainers\.image\.revision')
		[ "$built_from" = "$CI_COMMIT_SHA" ] ||
			die "$IMAGE:$version already exists (built from ${built_from:-an unknown commit}); release tags are immutable. Release a new version instead."
		info "$IMAGE:$version was already published from $CI_COMMIT_SHA (a retried job); it is kept as it is."
	else
		: "${IMAGE_TARBALL:?IMAGE_TARBALL is not set}"
		[ -f "$IMAGE_TARBALL" ] || die "No image tarball at $IMAGE_TARBALL."
		crane_cmd push "$IMAGE_TARBALL" "$IMAGE:$version" >/dev/null
		info "Pushed $IMAGE:$version."
	fi
	digest=$(crane_cmd digest "$IMAGE:$version") || die "Cannot read the digest of $IMAGE:$version."
	ref="$IMAGE@$digest"
	[ -z "${CI_COMMIT_SHORT_SHA:-}" ] || commit_tag "$ref" "$digest"
	move_tag "v$(major "$version")" "$version" "$ref"
	move_tag latest "$version" "$ref"
	info "Published $ref as :$version."
}

# commit_tag REF DIGEST: the 8-character commit tag is written once, by
# whichever pipeline (main or release) publishes first. An existing tag is
# kept when it is the same image or another build of the same commit; one
# from another commit stops the job.
commit_tag() {
	ref=$1 digest=$2 short=$CI_COMMIT_SHORT_SHA
	if ! has_tag "$short"; then
		crane_cmd tag "$ref" "$short" >/dev/null
		info ":$short -> $digest"
		return 0
	fi
	existing=$(crane_cmd digest "$IMAGE:$short") || die "Cannot read the digest of $IMAGE:$short."
	if [ "$existing" = "$digest" ]; then
		info ":$short unchanged (already this image)"
		return 0
	fi
	built_from=$(image_label "$IMAGE:$short" 'org\.opencontainers\.image\.revision')
	[ "$built_from" = "$CI_COMMIT_SHA" ] ||
		die "$IMAGE:$short exists from ${built_from:-an unknown commit}, not $CI_COMMIT_SHA; commit tags are written once."
	info ":$short unchanged (an earlier build of this commit; commit tags are written once)"
}

# The tip of the default branch, from the Git smart HTTP ref advertisement
# (MAIN_TIP overrides it in tests).
default_branch_tip() {
	if [ -n "${MAIN_TIP:-}" ]; then
		printf '%s\n' "$MAIN_TIP"
		return 0
	fi
	curl -sSf --max-time 30 -u "gitlab-ci-token:$CI_JOB_TOKEN" \
		"$CI_SERVER_URL/$CI_PROJECT_PATH.git/info/refs?service=git-upload-pack" |
		sed -n "s|^[0-9a-f]\{4\}\([0-9a-f]\{40\}\) refs/heads/$CI_DEFAULT_BRANCH\$|\1|p"
}

# cmd_publish_main: a main pipeline publishes its image tarball as the
# write-once commit tag, and moves :main only while the commit is still the
# tip of the default branch, so a late or retried pipeline of an older
# commit cannot move :main back. Never :latest.
cmd_publish_main() {
	require_publish_env
	: "${CI_COMMIT_SHA:?}" "${CI_COMMIT_SHORT_SHA:?}" "${IMAGE_TARBALL:?IMAGE_TARBALL is not set}"
	[ -f "$IMAGE_TARBALL" ] || die "No image tarball at $IMAGE_TARBALL."
	TAGS=$(list_tags)
	short=$CI_COMMIT_SHORT_SHA
	if has_tag "$short"; then
		new=$(crane_cmd digest --tarball "$IMAGE_TARBALL") || die "Cannot read the digest of $IMAGE_TARBALL."
		commit_tag "$IMAGE@$new" "$new"
	else
		crane_cmd push "$IMAGE_TARBALL" "$IMAGE:$short" >/dev/null
		info "Pushed $IMAGE:$short."
	fi
	digest=$(crane_cmd digest "$IMAGE:$short") || die "Cannot read the digest of $IMAGE:$short."
	tip=$(default_branch_tip) || true
	[ -n "$tip" ] || die "Cannot read the tip of ${CI_DEFAULT_BRANCH:-the default branch}; :main was not moved (a retry checks again)."
	if [ "$tip" = "$CI_COMMIT_SHA" ]; then
		crane_cmd tag "$IMAGE@$digest" main >/dev/null
		info ":main -> $short ($digest)"
	else
		info "$CI_COMMIT_SHA is no longer the tip ($tip); :main unchanged."
	fi
}

# cmd_wait_image VERSION: waits until IMAGE:VERSION exists and was built from
# the commit of tag VERSION (its revision label), reading anonymously.
# Gives up after WAIT_SECONDS (default 1800), checking every WAIT_INTERVAL
# (default 30) seconds. The GitHub release uses it, so it never publishes
# an installer for an image GitLab has not published.
cmd_wait_image() {
	version=$1
	is_release "$version" || die "A release version looks like v1.2.3, not '$version'."
	require_publish_env
	commit=$(git rev-parse --verify --quiet "$version^{commit}") || die "Tag $version is not in this repository."
	deadline=$(($(date +%s) + ${WAIT_SECONDS:-1800}))
	while :; do
		if config=$(crane_cmd config "$IMAGE:$version" 2>/dev/null); then
			built_from=$(printf '%s\n' "$config" | sed -n 's/.*"org\.opencontainers\.image\.revision": *"\([^"]*\)".*/\1/p' | head -1)
			if [ "$built_from" = "$commit" ]; then
				info "$IMAGE:$version is published, built from $commit."
				return 0
			fi
			[ -z "$built_from" ] || die "$IMAGE:$version was built from $built_from, not $commit."
		fi
		[ "$(date +%s)" -lt "$deadline" ] || die "$IMAGE:$version did not appear within ${WAIT_SECONDS:-1800} seconds."
		info "Waiting for $IMAGE:$version..."
		sleep "${WAIT_INTERVAL:-30}"
	done
}

cmd_promote_latest() {
	version=$1
	is_release "$version" || die "A release version looks like v1.2.3, not '$version'."
	require_publish_env
	TAGS=$(list_tags)
	has_tag "$version" || die "$IMAGE:$version does not exist; the release job publishes it first."
	if has_tag latest; then previous=$(version_label "$IMAGE:latest"); else previous=""; fi
	digest=$(crane_cmd digest "$IMAGE:$version") || die "Cannot read the digest of $IMAGE:$version."
	crane_cmd tag "$IMAGE@$digest" latest >/dev/null
	info "Moved $IMAGE:latest from ${previous:-nothing} to $version ($digest)."
	info "Deployments that pull :latest get $version on their next restart; they need the template of $version (make upgrade from a $version checkout)."
}

# --- GitLab Release -------------------------------------------------------------

# json_string TEXT: TEXT as a JSON string.
json_string() {
	printf '%s\n' "$1" | tr -d '\r' |
		sed -e 's/\\/\\\\/g' -e 's/"/\\"/g' -e 's/	/\\t/g' |
		awk 'BEGIN { printf "%s", "\"" } NR > 1 { printf "%s", "\\n" } { printf "%s", $0 } END { printf "%s", "\"" }'
}

cmd_gitlab_release() {
	version=$1
	is_release "$version" || die "A release version looks like v1.2.3, not '$version'."
	: "${CI_API_V4_URL:?}" "${CI_PROJECT_ID:?}" "${CI_JOB_TOKEN:?}" "${CI_PROJECT_URL:?}"
	api="$CI_API_V4_URL/projects/$CI_PROJECT_ID/releases"
	code=$(curl -sS -o /dev/null -w '%{http_code}' --header "JOB-TOKEN: $CI_JOB_TOKEN" "$api/$version") || code=000
	case "$code" in
	200)
		info "The GitLab Release $version exists already; leaving it."
		return 0
		;;
	404) ;;
	*) die "Cannot check for the GitLab Release $version (HTTP $code)." ;;
	esac
	: "${INSTALLER:?INSTALLER (the generated install.sh) is not set}"
	: "${INSTALLER_JOB_ID:?INSTALLER_JOB_ID (the job that built install.sh) is not set}"
	# The link names the job that built this install.sh (its artifact is kept
	# forever), not "the latest job of this ref", which a retry would change;
	# the checksum in the notes is of this same file. The Release also serves
	# it under a permanent URL: <project>/-/releases/<tag>/downloads/install.sh
	artifact_url="$CI_PROJECT_URL/-/jobs/$INSTALLER_JOB_ID/artifacts/raw/$INSTALLER"
	INSTALLER_URL=${INSTALLER_URL:-$CI_PROJECT_URL/-/releases/$version/downloads/install.sh}
	notes=$(cmd_notes "$version" HEAD)
	rev=$(template_revision HEAD)
	tmp=$(mktemp)
	trap 'rm -f "$tmp"' EXIT
	printf '{"tag_name":%s,"name":%s,"description":%s,"assets":{"links":[{"name":%s,"url":%s,"direct_asset_path":"/install.sh","link_type":"package"},{"name":%s,"url":%s,"link_type":"other"}]}}\n' \
		"$(json_string "$version")" "$(json_string "$version")" "$(json_string "$notes")" \
		"$(json_string "install.sh (SHA-256 $(sha256 "$INSTALLER"))")" "$(json_string "$artifact_url")" \
		"$(json_string "deploy/template.yaml (template revision $rev)")" \
		"$(json_string "$CI_PROJECT_URL/-/raw/$version/deploy/template.yaml")" >"$tmp"
	curl -sS --fail-with-body -o /dev/null --header "JOB-TOKEN: $CI_JOB_TOKEN" \
		--header "Content-Type: application/json" --data "@$tmp" "$api" ||
		die "Creating the GitLab Release $version failed."
	info "Created the GitLab Release $version."
}

# --- main -----------------------------------------------------------------------

usage() {
	sed -n '2,/^set -eu/p' "$0" | sed -e '$d' -e 's/^# \{0,1\}//' >&2
	exit 2
}

[ $# -ge 1 ] || usage
cmd=$1
shift
case "$cmd" in
check) [ $# -ge 1 ] || usage; cmd_check "$@" ;;
notes) [ $# -ge 1 ] || usage; cmd_notes "$@" ;;
installer) [ $# -ge 1 ] || usage; cmd_installer "$@" ;;
tag) [ $# -eq 1 ] || usage; cmd_tag "$1" ;;
publish) [ $# -eq 1 ] || usage; cmd_publish "$1" ;;
promote-latest) [ $# -eq 1 ] || usage; cmd_promote_latest "$1" ;;
publish-main) [ $# -eq 0 ] || usage; cmd_publish_main ;;
wait-image) [ $# -eq 1 ] || usage; cmd_wait_image "$1" ;;
gitlab-release) [ $# -eq 1 ] || usage; cmd_gitlab_release "$1" ;;
should-move) [ $# -eq 3 ] || usage; cmd_should_move "$@" ;;
*) usage ;;
esac
