#!/usr/bin/env bash
# Resolves IMAGE:TAG to an immutable digest reference (IMAGE@sha256:...) and
# checks that the image was built from a commit whose deploy/template.yaml
# matches this checkout's working tree. Prints the reference to deploy on
# stdout; everything else goes to stderr. Used by `make deploy` and
# `make upgrade`.
#
# Why: an image only works with the template of its own commit (ports,
# probes, environment, RBAC), and a mutable tag such as :latest can move
# between the check and the pod's pull. Deploying the checked digest pairs
# the checked template with exactly that code.
#
# Environment:
#   IMAGE, TAG, PLATFORM (default linux/amd64)
#   ALLOW_TEMPLATE_MISMATCH=1  skip the revision and template checks
#   ALLOW_MUTABLE_TAG=1        deploy IMAGE:TAG when the digest cannot be resolved
set -euo pipefail

: "${IMAGE:?IMAGE is not set}"
: "${TAG:?TAG is not set}"
PLATFORM=${PLATFORM:-linux/amd64}
err() { printf '%s\n' "$*" >&2; }

# 1. The digest. Quay's tag API answers anonymously for public repositories
#    (https://docs.quay.io/api/swagger/, GET /api/v1/repository/{repo}/tag/);
#    manifest_digest is the manifest-list digest for multi-arch tags.
digest=""
case "$IMAGE" in
quay.io/*/*)
	json=$(curl -sf --max-time 20 "https://quay.io/api/v1/repository/${IMAGE#quay.io/}/tag/?specificTag=${TAG}&onlyActiveTags=true" || true)
	digest=$(printf '%s' "$json" | sed -n 's/.*"manifest_digest": *"\(sha256:[0-9a-f]\{64\}\)".*/\1/p' | head -1)
	;;
esac
if [ -z "$digest" ]; then
	# Other registries, or a repository Quay does not show anonymously: ask
	# the registry with the local pull credentials.
	info=$(oc image info "$IMAGE:$TAG" --filter-by-os="$PLATFORM" -o json 2>/dev/null || true)
	digest=$(printf '%s\n' "$info" | sed -n 's/^  "listDigest": *"\(sha256:[0-9a-f]\{64\}\)".*/\1/p' | head -1)
	[ -n "$digest" ] || digest=$(printf '%s\n' "$info" | sed -n 's/^  "digest": *"\(sha256:[0-9a-f]\{64\}\)".*/\1/p' | head -1)
fi
if [ -n "$digest" ]; then
	ref="$IMAGE@$digest"
	err "Resolved $IMAGE:$TAG to $ref"
elif [ "${ALLOW_MUTABLE_TAG:-}" = "1" ]; then
	ref="$IMAGE:$TAG"
	err "WARNING: ALLOW_MUTABLE_TAG=1: cannot resolve $IMAGE:$TAG to a digest; deploying the tag, which may move before the pod pulls it."
else
	err "ERROR: cannot resolve $IMAGE:$TAG to a digest (no such tag, or the registry is unreachable)."
	err "  Image tags are latest or an 8-character commit. Set ALLOW_MUTABLE_TAG=1 to deploy the tag itself."
	exit 1
fi

if [ "${ALLOW_TEMPLATE_MISMATCH:-}" = "1" ]; then
	err "WARNING: ALLOW_TEMPLATE_MISMATCH=1: not checking that $ref matches deploy/template.yaml."
	printf '%s\n' "$ref"
	exit 0
fi

# 2. The commit the image was built from: its org.opencontainers.image.revision
#    label (set by the Containerfile). A label naming a commit that is not in
#    this repository (builds older than the label inherit the base image's) is
#    unknown. An image tag that is a commit of this repository names it too.
label=$(oc image info "$ref" --filter-by-os="$PLATFORM" -o json 2>/dev/null |
	sed -n 's/^ *"org.opencontainers.image.revision": *"\([0-9a-f]\{40\}\)".*/\1/p' | head -1 || true)
if [ -n "$label" ] && ! git cat-file -e "$label^{commit}" 2>/dev/null; then
	label=""
fi
tagrev=""
if [ "$TAG" != "latest" ]; then
	tagrev=$(git rev-parse --verify --quiet "$TAG^{commit}" || true)
fi
if [ -n "$label" ] && [ -n "$tagrev" ] && [ "$label" != "$tagrev" ]; then
	err "ERROR: $IMAGE:$TAG reports revision $label, not commit $tagrev that its tag names."
	err "  Set ALLOW_TEMPLATE_MISMATCH=1 to apply this checkout's template anyway."
	exit 1
fi
rev=${label:-$tagrev}
if [ -z "$rev" ]; then
	err "ERROR: cannot tell which commit $ref was built from (no revision label of this repository; run 'git fetch origin' if it is newer than this clone)."
	err "  Set ALLOW_TEMPLATE_MISMATCH=1 to apply this checkout's template anyway."
	exit 1
fi

# 3. The template: the working tree (uncommitted edits included) against the
#    image's commit, also when that commit is HEAD.
if ! git diff --quiet "$rev" -- deploy/template.yaml; then
	if [ "$rev" = "$(git rev-parse HEAD)" ]; then
		err "ERROR: deploy/template.yaml has uncommitted changes, so it is not the template $ref was built with."
	else
		err "ERROR: deploy/template.yaml in this checkout differs from the template of commit $rev, which $ref was built from."
		err "  To run an older build, use its own template:  make rollback TAG=<commit>"
		err "  For :latest, update this checkout (git pull) or wait until CI has published it."
	fi
	err "  Set ALLOW_TEMPLATE_MISMATCH=1 to apply this checkout's template anyway."
	exit 1
fi
err "OK: $ref was built from $rev, whose template matches this checkout."
printf '%s\n' "$ref"
