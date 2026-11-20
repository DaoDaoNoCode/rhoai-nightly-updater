#!/bin/sh
# The GitLab image jobs (build-main, release-build, build-branch): build the
# Containerfile with buildah (chroot isolation, vfs storage), for
# linux/amd64, with RUN_CHECKS=false (the jobs `need` the test jobs of their
# pipeline, which ran those checks on this commit).
#
# Usage:
#   ci-build-image.sh tarball VERSION OUT  build, then write the image to OUT as
#                                          a docker-archive tarball tagged
#                                          IMAGE:VERSION (the crane jobs push it)
#   ci-build-image.sh branch VERSION       build, then push the image as
#                                          BRANCH_IMAGE:CI_COMMIT_REF_SLUG when
#                                          BRANCH_IMAGE, QUAY_BRANCH_USER and
#                                          QUAY_BRANCH_TOKEN are set; else
#                                          build only
#
# Environment: CI_PROJECT_DIR, CI_COMMIT_SHA, CI_COMMIT_TIMESTAMP, DOCKER_HUB,
# IMAGE (tarball), CI_COMMIT_REF_SLUG (branch), and the Dependency Proxy's
# CI_DEPENDENCY_PROXY_SERVER/_USER/_PASSWORD when DOCKER_HUB points at it.
#
# Credentials: this script never reads QUAY_USER/QUAY_TOKEN. Its registry
# logins live in a temporary auth file outside the build context, which the
# RUN steps (chroot) do not see: the Dependency Proxy login (job-scoped) for
# the base images, and, in branch mode, the branch robot's login, written
# only after the build has finished.
set -eu

info() { printf '%s\n' "$*" >&2; }
die() {
	printf 'ERROR: %s\n' "$*" >&2
	exit 1
}

bh() { buildah --storage-driver vfs "$@"; }

# login HOST USER: the password comes on stdin.
login() {
	bh login --authfile "$REGISTRY_AUTH_FILE" --username "$2" --password-stdin "$1" >/dev/null
}

# build VERSION: build the Containerfile as the local image $LOCAL.
build() {
	bh build --isolation chroot --platform linux/amd64 --format docker \
		--build-arg "GIT_SHA=$CI_COMMIT_SHA" \
		--build-arg "VERSION=$1" \
		--build-arg "BUILD_DATE=$CI_COMMIT_TIMESTAMP" \
		--build-arg RUN_CHECKS=false \
		--build-arg "DOCKER_HUB=$DOCKER_HUB" \
		--tag "$LOCAL" \
		--file "$CI_PROJECT_DIR/Containerfile" \
		"$CI_PROJECT_DIR"
}

[ $# -ge 2 ] || die "Usage: $0 tarball VERSION OUT | branch VERSION"
mode=$1 version=$2
: "${CI_PROJECT_DIR:?}" "${CI_COMMIT_SHA:?}" "${CI_COMMIT_TIMESTAMP:?}" "${DOCKER_HUB:?}"
LOCAL=localhost/rhoai-nightly-updater:ci

auth_dir=$(mktemp -d)
trap 'rm -rf "$auth_dir"' EXIT
REGISTRY_AUTH_FILE=$auth_dir/auth.json
export REGISTRY_AUTH_FILE
printf '{"auths":{}}\n' >"$REGISTRY_AUTH_FILE"

buildah version

# What the runner gives this job, for the log (its configuration is set by
# the runner's owners and not visible to this project): the effective
# capabilities, and the block devices in /dev, which a privileged container
# sees from its host. Names only; nothing is opened.
info "Job capabilities: $(grep '^CapEff:' /proc/self/status | tr -s ' \t' ' ')"
blk=$(find /dev -maxdepth 1 -type b 2>/dev/null | sed 's|^/dev/||' | sort | tr '\n' ' ')
info "Block devices in /dev: ${blk:-none}"

# The Containerfile's Docker Hub bases come through the Dependency Proxy
# when DOCKER_HUB points at it: log in with the job's proxy credentials.
hub=${DOCKER_HUB%%/*}
if [ -n "${CI_DEPENDENCY_PROXY_SERVER:-}" ] &&
	{ [ "$hub" = "$CI_DEPENDENCY_PROXY_SERVER" ] || [ "$hub" = "${CI_DEPENDENCY_PROXY_SERVER%:443}" ]; }; then
	printf '%s' "$CI_DEPENDENCY_PROXY_PASSWORD" | login "$hub" "$CI_DEPENDENCY_PROXY_USER"
fi

case "$mode" in
tarball)
	[ $# -eq 3 ] || die "Usage: $0 tarball VERSION OUT"
	out=$3
	: "${IMAGE:?IMAGE (the repository, without a tag) is not set}"
	build "$version"
	rm -f "$out"
	bh push "$LOCAL" "docker-archive:$out:$IMAGE:$version"
	info "Wrote $IMAGE:$version to $out."
	;;
branch)
	: "${CI_COMMIT_REF_SLUG:?}"
	dest=""
	if [ -n "${BRANCH_IMAGE:-}" ] && [ -n "${QUAY_BRANCH_USER:-}" ] && [ -n "${QUAY_BRANCH_TOKEN:-}" ]; then
		case "$BRANCH_IMAGE" in
		"${IMAGE:-}" | *@* | */*:*)
			die "BRANCH_IMAGE must be a repository of its own, without a tag or digest, not $BRANCH_IMAGE (IMAGE is ${IMAGE:-unset})." ;;
		esac
		dest=$BRANCH_IMAGE:$CI_COMMIT_REF_SLUG
	else
		info "BRANCH_IMAGE, QUAY_BRANCH_USER or QUAY_BRANCH_TOKEN is not set: the image is built, not pushed (CONTRIBUTING.md, Automatic Image Publishing)."
	fi
	build "$version"
	if [ -n "$dest" ]; then
		printf '%s' "$QUAY_BRANCH_TOKEN" | login "${BRANCH_IMAGE%%/*}" "$QUAY_BRANCH_USER"
		bh push --digestfile "$auth_dir/digest" "$LOCAL" "docker://$dest"
		info "Pushed $dest ($(cat "$auth_dir/digest"))."
	fi
	;;
*) die "Unknown mode '$mode': tarball or branch." ;;
esac
