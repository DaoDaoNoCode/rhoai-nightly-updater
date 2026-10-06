#!/usr/bin/env bash
# Installs, upgrades or rolls back the RHOAI Nightly Updater on OpenShift.
# The single implementation behind `make deploy`, `make upgrade`,
# `make rollback` and `make resolve-image`, and the install.sh attached to
# every release.
#
# A release's install.sh embeds that release's deploy/template.yaml and
# installs exactly that release: it needs only `oc` (logged in as
# cluster-admin) and `curl`, no clone. In a clone (this file), the template
# is read from the working tree and the version from the checkout.
#
# Usage: install.sh [ACTION] [OPTIONS]
#   Actions:
#     install          deploy when the app is not installed, else upgrade (default)
#     deploy           first install (re-applies when installed)
#     upgrade          re-apply the template and the image of this version
#     rollback         clone only: an older build with its own commit's
#                      template (--version <release or commit>)
#     resolve-image    print the digest reference that would be applied
#     cleanup-legacy   remove this install's objects from before namespaced names
#     uninstall        remove the app, its RBAC, ConsoleLink and namespace
#   Options (each also has an environment variable):
#     --dry-run              validate with the API server only        DRY_RUN=1
#     --namespace NAME       default rhoai-nightly-updater            NAMESPACE
#     --app-name NAME        default rhoai-nightly-updater            APP_NAME
#     --image-repo REPO      image repository, without a tag          IMAGE
#     --version TAG          vX.Y.Z, main or an 8-character commit    TAG
#     --print-template       print the template this script applies
#     --print-version        print the version this script installs
#   More environment: OAUTH_PROXY_IMAGE (default: from the cluster version),
#   PLATFORM (linux/amd64), ROLLOUT_TIMEOUT (20m), RELEASES_URL,
#   ALLOW_TEMPLATE_MISMATCH=1 (skip the image/template commit check),
#   ALLOW_MUTABLE_TAG=1 (apply the tag when no digest can be resolved).
#
# Why digests and the commit check: an image only works with the template
# of its own commit (ports, probes, environment, RBAC), and a mutable tag
# can move between the check and the pod's pull. Applying the checked digest
# pairs the checked template with exactly that code.
set -euo pipefail

# The release this file installs; scripts/release.sh installer fills this
# block in for release assets. Empty in a clone.
# @@EMBEDDED-RELEASE-BEGIN@@
EMBEDDED_VERSION=""
EMBEDDED_COMMIT=""
EMBEDDED_IMAGE=""
embedded_template() { return 1; }
# @@EMBEDDED-RELEASE-END@@

DEFAULT_IMAGE=quay.io/juntao_wang/rhoai-nightly-updater

err() { printf '%s\n' "$*" >&2; }
die() {
	err "$@"
	exit 1
}
usage() {
	sed -n '2,/^set -euo pipefail/p' "$0" | sed -e '$d' -e 's/^# \{0,1\}//'
	exit "${1:-0}"
}

# --- Arguments ------------------------------------------------------------------

ACTION=""
TAG_EXPLICIT=""
[ -z "${TAG:-}" ] || TAG_EXPLICIT=1
need_value() { [ $# -ge 2 ] && [ -n "$2" ] || die "$1 needs a value (see --help)."; }
while [ $# -gt 0 ]; do
	case "$1" in
	install | deploy | upgrade | rollback | resolve-image | cleanup-legacy | uninstall) ACTION=$1 ;;
	--dry-run) DRY_RUN=1 ;;
	--namespace | --app-name | --image-repo | --version)
		need_value "$@"
		case "$1" in
		--namespace) NAMESPACE=$2 ;;
		--app-name) APP_NAME=$2 ;;
		--image-repo) IMAGE=$2 ;;
		--version) TAG=$2 TAG_EXPLICIT=1 ;;
		esac
		shift
		;;
	--namespace=* | --app-name=* | --image-repo=* | --version=*)
		value=${1#*=}
		[ -n "$value" ] || die "${1%%=*} needs a value (see --help)."
		case "$1" in
		--namespace=*) NAMESPACE=$value ;;
		--app-name=*) APP_NAME=$value ;;
		--image-repo=*) IMAGE=$value ;;
		--version=*) TAG=$value TAG_EXPLICIT=1 ;;
		esac
		;;
	--print-template | --print-version) ACTION=${1#--} ;;
	-h | --help) usage 0 ;;
	*) die "Unknown argument: $1 (see --help)" ;;
	esac
	shift
done
ACTION=${ACTION:-install}

NAMESPACE=${NAMESPACE:-rhoai-nightly-updater}
APP_NAME=${APP_NAME:-rhoai-nightly-updater}
PLATFORM=${PLATFORM:-linux/amd64}
ROLLOUT_TIMEOUT=${ROLLOUT_TIMEOUT:-20m}
case "${DRY_RUN:-}" in "" | 0 | false | no) DRY="" ;; *) DRY=1 ;; esac
DRY_FLAG=${DRY:+--dry-run=server}
INSTANCE="$APP_NAME-$NAMESPACE"
PROXY_SECRET="$APP_NAME-proxy"

is_release() { [[ $1 =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; }

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
umask 077

if [ -n "$EMBEDDED_VERSION" ]; then
	MODE=release
	IMAGE=${IMAGE:-$EMBEDDED_IMAGE}
	if [ -n "${TAG:-}" ] && [ "$TAG" != "$EMBEDDED_VERSION" ]; then
		die "This installer installs $EMBEDDED_VERSION only. For $TAG, download the install.sh of that release."
	fi
	TAG=$EMBEDDED_VERSION
	TEMPLATE="$TMP/template.yaml"
	embedded_template >"$TEMPLATE"
else
	MODE=checkout
	ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
	cd "$ROOT"
	IMAGE=${IMAGE:-$DEFAULT_IMAGE}
	TEMPLATE="$ROOT/deploy/template.yaml"
	if [ -z "${TAG:-}" ] && [ "$ACTION" != rollback ]; then
		TAG=$(git describe --tags --exact-match --match 'v[0-9]*' HEAD 2>/dev/null || true)
		is_release "$TAG" || TAG=""
	fi
fi
TAG=${TAG:-}
case "$IMAGE" in *@* | */*:*) die "IMAGE must be a repository without a tag or digest, not $IMAGE." ;; esac

case "$ACTION" in
print-template)
	cat "$TEMPLATE"
	exit 0
	;;
print-version)
	printf '%s\n' "${TAG:-none}"
	exit 0
	;;
esac

# --- Cluster ------------------------------------------------------------------

preflight() {
	command -v oc >/dev/null 2>&1 || die "oc is not installed: https://docs.openshift.com/container-platform/latest/cli_reference/openshift_cli/getting-started-cli.html"
	command -v curl >/dev/null 2>&1 || die "curl is not installed."
	local user
	user=$(oc whoami 2>/dev/null) || die "Not logged in to a cluster; run 'oc login' first."
	[ "$(oc auth can-i '*' '*' --all-namespaces 2>/dev/null || true)" = yes ] ||
		die "$user is not cluster-admin; installing needs the cluster-admin role (it creates ClusterRoles and a ConsoleLink)."
	err "Cluster $(oc whoami --show-server 2>/dev/null || echo '?'), user $user, namespace $NAMESPACE, app $APP_NAME."
}

# The oauth-proxy tag must match the cluster's OCP minor version.
detect_oauth_proxy_image() {
	[ -z "${OAUTH_PROXY_IMAGE:-}" ] || return 0
	local minor
	minor=$(oc version -o json 2>/dev/null | sed -n 's/.*"openshiftVersion": *"\([0-9]*\.[0-9]*\).*/\1/p' | head -1)
	[ -n "$minor" ] || die "Cannot detect the OCP version; set OAUTH_PROXY_IMAGE=registry.redhat.io/openshift4/ose-oauth-proxy-rhel9:v4.<minor>"
	OAUTH_PROXY_IMAGE=registry.redhat.io/openshift4/ose-oauth-proxy-rhel9:v$minor
}

# --- Image ----------------------------------------------------------------------

# registry_digest REPO TAG: the manifest (list) digest from the registry's v2
# API, anonymously (a bearer token from the registry's token service when it
# asks for one). Prints nothing when the registry does not answer.
registry_digest() {
	local repo=$1 tag=$2 host path scheme=https url headers status www realm service token
	host=${repo%%/*}
	path=${repo#*/}
	case "$host" in
	*.* | *:* | localhost) ;;
	*) host=docker.io path=$repo ;;
	esac
	if [ "$host" = docker.io ]; then
		host=registry-1.docker.io
		[[ $path == */* ]] || path=library/$path
	fi
	case "$host" in localhost | localhost:* | 127.0.0.1:*) scheme=http ;; esac
	url="$scheme://$host/v2/$path/manifests/$tag"
	local accept="application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json,application/vnd.oci.image.manifest.v1+json,application/vnd.docker.distribution.manifest.v2+json"
	headers=$(curl -sS -I --max-time 20 -H "Accept: $accept" "$url" 2>/dev/null | tr -d '\r') || return 0
	status=$(printf '%s\n' "$headers" | sed -n '1s/^HTTP[^ ]* \([0-9]*\).*/\1/p')
	if [ "$status" = 401 ]; then
		www=$(printf '%s\n' "$headers" | grep -i '^www-authenticate: *bearer' | head -1 || true)
		realm=$(printf '%s\n' "$www" | sed -n 's/.*realm="\([^"]*\)".*/\1/p')
		service=$(printf '%s\n' "$www" | sed -n 's/.*service="\([^"]*\)".*/\1/p')
		[ -n "$realm" ] || return 0
		token=$(curl -sS --max-time 20 -G "$realm" --data-urlencode "service=$service" \
			--data-urlencode "scope=repository:$path:pull" 2>/dev/null |
			sed -nE 's/.*"(token|access_token)" *: *"([^"]*)".*/\2/p' | head -1) || return 0
		[ -n "$token" ] || return 0
		headers=$(curl -sS -I --max-time 20 -H "Accept: $accept" -H "Authorization: Bearer $token" "$url" 2>/dev/null | tr -d '\r') || return 0
		status=$(printf '%s\n' "$headers" | sed -n '1s/^HTTP[^ ]* \([0-9]*\).*/\1/p')
	fi
	[ "$status" = 200 ] || return 0
	printf '%s\n' "$headers" | sed -n 's/^[Dd]ocker-[Cc]ontent-[Dd]igest: *\(sha256:[0-9a-f]\{64\}\).*/\1/p' | head -1
}

# image_revision REF: the org.opencontainers.image.revision label (a full
# commit) of the image, read with oc and the local pull credentials.
image_revision() {
	oc image info "$1" --filter-by-os="$PLATFORM" -o json 2>/dev/null |
		sed -n 's/^ *"org.opencontainers.image.revision": *"\([0-9a-f]\{40\}\)".*/\1/p' | head -1 || true
}

# resolve_image: sets REF to IMAGE@sha256:... for IMAGE:TAG and checks that
# the image was built from the commit of the template that will be applied.
require_tag() {
	[ -z "$TAG" ] || return 0
	if [ "$MODE" = checkout ]; then
		die "HEAD is not on a release tag. Check out a release (git checkout vX.Y.Z), or pass TAG=main / TAG=<commit> to test a build."
	fi
	die "No version to install."
}

resolve_image() {
	require_tag
	if [ "$TAG" = latest ]; then
		err "WARNING: :latest is the newest release of one major version only and moves without notice; prefer a release (git checkout vX.Y.Z)."
	fi

	# 1. The digest: the registry's v2 API, else oc with the local pull credentials.
	local digest info
	digest=$(registry_digest "$IMAGE" "$TAG")
	if [ -z "$digest" ]; then
		info=$(oc image info "$IMAGE:$TAG" --filter-by-os="$PLATFORM" -o json 2>/dev/null || true)
		digest=$(printf '%s\n' "$info" | sed -n 's/^  "listDigest": *"\(sha256:[0-9a-f]\{64\}\)".*/\1/p' | head -1)
		[ -n "$digest" ] || digest=$(printf '%s\n' "$info" | sed -n 's/^  "digest": *"\(sha256:[0-9a-f]\{64\}\)".*/\1/p' | head -1)
	fi
	if [ -n "$digest" ]; then
		REF="$IMAGE@$digest"
		err "Resolved $IMAGE:$TAG to $REF"
	elif [ "${ALLOW_MUTABLE_TAG:-}" = 1 ]; then
		REF="$IMAGE:$TAG"
		err "WARNING: ALLOW_MUTABLE_TAG=1: cannot resolve $IMAGE:$TAG to a digest; deploying the tag, which may move before the pod pulls it."
	else
		err "ERROR: cannot resolve $IMAGE:$TAG to a digest (no such tag, or the registry is unreachable)."
		err "  Image tags are releases (vX.Y.Z, vN), main, latest or an 8-character commit. Set ALLOW_MUTABLE_TAG=1 to deploy the tag itself."
		exit 1
	fi

	if [ "${ALLOW_TEMPLATE_MISMATCH:-}" = 1 ]; then
		err "WARNING: ALLOW_TEMPLATE_MISMATCH=1: not checking that $REF matches the template."
		return 0
	fi

	# 2. The commit the image was built from: its revision label (set by the
	#    Containerfile).
	local label
	label=$(image_revision "$REF")
	if [ "$MODE" = release ]; then
		if [ "$label" != "$EMBEDDED_COMMIT" ]; then
			err "ERROR: $REF reports revision ${label:-<none>}, not $EMBEDDED_COMMIT, the commit of $EMBEDDED_VERSION whose template this installer holds."
			err "  Set ALLOW_TEMPLATE_MISMATCH=1 to apply this template anyway."
			exit 1
		fi
		err "OK: $REF was built from $label, the commit of $EMBEDDED_VERSION."
		return 0
	fi

	# A label naming a commit that is not in this repository (builds older
	# than the label inherit the base image's) is unknown. A tag that is a
	# commit or a release names its commit too; main, latest and vN move.
	if [ -n "$label" ] && ! git cat-file -e "$label^{commit}" 2>/dev/null; then
		label=""
	fi
	local tagrev=""
	if [[ $TAG =~ ^[0-9a-f]{7,40}$ ]] || is_release "$TAG"; then
		tagrev=$(git rev-parse --verify --quiet "$TAG^{commit}" || true)
	fi
	if [ -n "$label" ] && [ -n "$tagrev" ] && [ "$label" != "$tagrev" ]; then
		err "ERROR: $IMAGE:$TAG reports revision $label, not commit $tagrev that its tag names."
		err "  Set ALLOW_TEMPLATE_MISMATCH=1 to apply this checkout's template anyway."
		exit 1
	fi
	local rev=${label:-$tagrev}
	if [ -z "$rev" ]; then
		err "ERROR: cannot tell which commit $REF was built from (no revision label of this repository; run 'git fetch origin' if it is newer than this clone)."
		err "  Set ALLOW_TEMPLATE_MISMATCH=1 to apply this checkout's template anyway."
		exit 1
	fi

	# 3. The template: the working tree (uncommitted edits included) against
	#    the image's commit, also when that commit is HEAD.
	if ! git diff --quiet "$rev" -- deploy/template.yaml; then
		if [ "$rev" = "$(git rev-parse HEAD)" ]; then
			err "ERROR: deploy/template.yaml has uncommitted changes, so it is not the template $REF was built with."
		else
			err "ERROR: deploy/template.yaml in this checkout differs from the template of commit $rev, which $REF was built from."
			err "  Check out the release you install (git checkout vX.Y.Z), or for an older build use its own template:  make rollback TAG=<release or commit>"
		fi
		err "  Set ALLOW_TEMPLATE_MISMATCH=1 to apply this checkout's template anyway."
		exit 1
	fi
	err "OK: $REF was built from $rev, whose template matches this checkout."
}

# --- Apply ----------------------------------------------------------------------

# The oauth-proxy cookie secret lives in a Secret that the template only
# references, so re-applying keeps everyone's session. An existing install
# keeps the value it had in its Deployment env (COOKIE_SECRET).
ensure_proxy_secret() {
	if oc get secret "$PROXY_SECRET" -n "$NAMESPACE" >/dev/null 2>&1; then
		err "Secret $PROXY_SECRET exists; keeping it."
		return 0
	fi
	local secret
	secret=$(oc get deployment "$APP_NAME" -n "$NAMESPACE" -o jsonpath='{.spec.template.spec.containers[?(@.name=="oauth-proxy")].env[?(@.name=="COOKIE_SECRET")].value}' 2>/dev/null || true)
	if [ -z "$secret" ]; then
		secret=$(head -c 64 /dev/urandom | base64 | tr -dc 'a-zA-Z0-9' | head -c 32)
	fi
	[ ${#secret} -eq 32 ] || die "Could not produce a 32-character cookie secret"
	printf 'session_secret=%s' "$secret" >"$TMP/secret.env"
	oc create secret generic "$PROXY_SECRET" -n "$NAMESPACE" --from-env-file="$TMP/secret.env" >/dev/null
	rm -f "$TMP/secret.env"
	err "Created Secret $PROXY_SECRET. Old ReplicaSets, which still hold the cookie secret in their pod template, are removed after the rollout."
}

# apply_template TEMPLATE IMAGE_REF: oc process | oc apply. The template is
# processed locally (--local), so it does not depend on the current project,
# which may not exist. Parameters go
# through a private file. A template that still takes a COOKIE_SECRET
# parameter (older releases) gets the value of the current Secret.
apply_template() {
	local template=$1 image_ref=$2 secret
	printf '%s\n' "IMAGE=$image_ref" "NAMESPACE=$NAMESPACE" "APP_NAME=$APP_NAME" \
		"OAUTH_PROXY_IMAGE=$OAUTH_PROXY_IMAGE" "IMAGE_REPOSITORY=$IMAGE" >"$TMP/params"
	[ -z "${RELEASES_URL:-}" ] || printf 'RELEASES_URL=%s\n' "$RELEASES_URL" >>"$TMP/params"
	if oc process -f "$template" --parameters --local | awk 'NR > 1 {print $1}' | grep -qx COOKIE_SECRET; then
		secret=$(oc get secret "$PROXY_SECRET" -n "$NAMESPACE" -o jsonpath='{.data.session_secret}' 2>/dev/null | base64 -d 2>/dev/null || true)
		[ -n "$secret" ] || secret=$(oc get deployment "$APP_NAME" -n "$NAMESPACE" -o jsonpath='{.spec.template.spec.containers[?(@.name=="oauth-proxy")].env[?(@.name=="COOKIE_SECRET")].value}' 2>/dev/null || true)
		[ ${#secret} -eq 32 ] || die "Could not read the current 32-character cookie secret (Secret $PROXY_SECRET)."
		printf 'COOKIE_SECRET=%s\n' "$secret" >>"$TMP/params"
	fi
	oc process -f "$template" --param-file="$TMP/params" --ignore-unknown-parameters --local >"$TMP/objects.json"
	rm -f "$TMP/params"
	if [ -z "$DRY" ]; then
		oc apply -f "$TMP/objects.json"
		return
	fi
	# Server-side dry run. Before the first install the namespace does not
	# exist, so the server cannot validate the objects in it; every other
	# object still is.
	local out rc=0 errors missing
	out=$(oc apply --dry-run=server -f "$TMP/objects.json" 2>&1) || rc=$?
	if [ "$rc" -ne 0 ] && ! oc get namespace "$NAMESPACE" >/dev/null 2>&1; then
		errors=$(printf '%s\n' "$out" | grep -c '^Error' || true)
		missing=$(printf '%s\n' "$out" | grep '^Error' | grep -c "namespaces \"$NAMESPACE\" not found" || true)
		if [ "$errors" -gt 0 ] && [ "$errors" = "$missing" ]; then
			printf '%s\n' "$out" | grep -v '^Error' || true
			err "Note: namespace $NAMESPACE does not exist yet (deploy creates it), so the server could not validate the $missing objects in it; all other objects passed."
			return 0
		fi
	fi
	printf '%s\n' "$out"
	return "$rc"
}

# The ConsoleLink needs the Route host, which the router assigns.
consolelink() {
	local host
	host=$(oc get route "$APP_NAME" -n "$NAMESPACE" -o jsonpath='{.spec.host}')
	[ -n "$host" ] || die "Route $APP_NAME has no host yet"
	err "App URL: https://$host"
	printf '%s\n' \
		'apiVersion: console.openshift.io/v1' \
		'kind: ConsoleLink' \
		'metadata:' \
		"  name: $INSTANCE" \
		'  labels:' \
		"    app.kubernetes.io/instance: $INSTANCE" \
		'spec:' \
		'  applicationMenu:' \
		'    section: Red Hat Applications' \
		'    imageURL: data:image/svg+xml;base64,PHN2ZyB4bWxucz0iaHR0cDovL3d3dy53My5vcmcvMjAwMC9zdmciIHZpZXdCb3g9IjAgMCAyNCAyNCIgZmlsbD0iI0VFMDAwMCI+PHBhdGggZD0iTTEyIDJMMyA3djEwbDkgNSA5LTVWN2wtOS01em0wIDIuMThMMTggNy4yN3Y3LjQ2TDEyIDE5LjgyIDYgMTQuNzNWNy4yN0wxMiA0LjE4eiIvPjwvc3ZnPg==' \
		"  href: https://$host" \
		'  location: ApplicationMenu' \
		'  text: RHOAI Nightly Updater' | oc apply -f -
}

# Installs from before the namespace-suffixed names used a ClusterRole,
# ClusterRoleBinding and ConsoleLink named APP_NAME. Remove them only when
# they belong to this install (bound to this namespace's ServiceAccount or
# linking to this Route), after the new RBAC is in place.
cleanup_legacy() {
	local subject host href
	subject=$(oc get clusterrolebinding "$APP_NAME" -o jsonpath='{.subjects[0].namespace}/{.subjects[0].name}' 2>/dev/null || true)
	# A dry run only validates the deletes with the server.
	# shellcheck disable=SC2086 # DRY_FLAG is one word or nothing
	if [ "$subject" = "$NAMESPACE/$APP_NAME" ]; then
		oc delete clusterrolebinding "$APP_NAME" $DRY_FLAG
		oc delete clusterrole "$APP_NAME" --ignore-not-found $DRY_FLAG
	fi
	host=$(oc get route "$APP_NAME" -n "$NAMESPACE" -o jsonpath='{.spec.host}' 2>/dev/null || true)
	href=$(oc get consolelink "$APP_NAME" -o jsonpath='{.spec.href}' 2>/dev/null || true)
	# shellcheck disable=SC2086
	if [ -n "$href" ] && { [ "$href" = "https://$host" ] || [ "$href" = "https://placeholder.apps.example.com" ]; }; then
		oc delete consolelink "$APP_NAME" $DRY_FLAG
	fi
}

# After a successful rollout, delete the Deployment's old ReplicaSets
# (selected by its label and owner, scaled to 0, other revision). Their pod
# templates keep whatever the old pods had, for example the plaintext
# COOKIE_SECRET env of installs from before the proxy Secret, which would
# otherwise stay readable. Rolling back uses `make rollback`, not the
# ReplicaSet history.
prune_replicasets() {
	local current uid old
	current=$(oc get deployment "$APP_NAME" -n "$NAMESPACE" -o jsonpath='{.metadata.annotations.deployment\.kubernetes\.io/revision}')
	uid=$(oc get deployment "$APP_NAME" -n "$NAMESPACE" -o jsonpath='{.metadata.uid}')
	[ -n "$current" ] && [ -n "$uid" ] || die "Cannot read the revision of deployment/$APP_NAME; old ReplicaSets were kept."
	old=$(oc get rs -n "$NAMESPACE" -l "app=$APP_NAME" -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.metadata.annotations.deployment\.kubernetes\.io/revision}{" "}{.metadata.ownerReferences[0].uid}{" "}{.spec.replicas}{" "}{.status.replicas}{"\n"}{end}' |
		awk -v cur="$current" -v uid="$uid" '$2 != cur && $3 == uid && $4 == "0" && ($5 == "0" || $5 == "") {print $1}')
	if [ -z "$old" ]; then
		err "No old ReplicaSets to remove."
		return 0
	fi
	err "Removing old ReplicaSets (their pod templates may hold old secrets): $(printf '%s\n' "$old" | tr '\n' ' ')"
	# shellcheck disable=SC2086 # one name per word
	oc delete rs -n "$NAMESPACE" $old
}

wait_rollout() {
	oc rollout status "deployment/$APP_NAME" -n "$NAMESPACE" --timeout="$ROLLOUT_TIMEOUT"
}

# --- Actions --------------------------------------------------------------------

installed() {
	oc get deployment "$APP_NAME" -n "$NAMESPACE" >/dev/null 2>&1
}

# deploy_or_upgrade deploy|upgrade
deploy_or_upgrade() {
	local action=$1
	require_tag
	preflight
	detect_oauth_proxy_image
	if [ "$action" = upgrade ] && ! installed; then
		die "No deployment $APP_NAME in $NAMESPACE; install it first (make deploy, or install.sh deploy)."
	fi
	resolve_image
	if [ -z "$DRY" ]; then
		if [ "$action" = deploy ]; then
			oc get project "$NAMESPACE" >/dev/null 2>&1 || oc new-project "$NAMESPACE" >/dev/null
		else
			err "Note: a running cluster operation delays the restart until it finishes (up to ~17 minutes)."
		fi
		ensure_proxy_secret
	fi
	err "$([ -n "$DRY" ] && echo Validating || echo Applying) $REF with the template of ${TAG}..."
	apply_template "$TEMPLATE" "$REF"
	if [ -n "$DRY" ]; then
		err "Dry run only; nothing was changed."
		return 0
	fi
	wait_rollout
	consolelink
	cleanup_legacy
	prune_replicasets
	if [ "$action" = deploy ]; then
		err "Deployed $REF."
	elif [ "$MODE" = checkout ]; then
		err "Upgraded to $REF. Verify with ./scripts/smoke-test.sh $NAMESPACE $APP_NAME"
	else
		err "Upgraded to $REF."
	fi
}

# Rolls back (or pins) to the build of an older commit with that commit's
# own deploy/template.yaml, so ports, probes and RBAC match its code. The
# cookie secret is kept, so sessions survive. Legacy cluster objects that an
# old template creates are left alone here (the old build needs them); the
# next upgrade removes them again.
rollback() {
	[ "$MODE" = checkout ] ||
		die "Rollback needs a clone. Without one, run the install.sh of the release you want: bash install.sh upgrade"
	case "$TAG" in latest | "") die "Usage: make rollback TAG=<release or commit> (for example TAG=v1.0.0, an image tag such as 4503bb7d, or any commit in this clone)" ;; esac
	[ -n "$TAG_EXPLICIT" ] || die "Usage: make rollback TAG=<release or commit>"
	preflight
	detect_oauth_proxy_image
	installed || die "No deployment $APP_NAME in $NAMESPACE; run 'make deploy' first."
	[ -n "$DRY" ] || ensure_proxy_secret
	local rev short8 short7 img_tag="" t label
	rev=$(git rev-parse --verify --quiet "$TAG^{commit}") || die "Commit $TAG is not in this clone; run 'git fetch origin --tags' and retry."
	git cat-file -e "$rev:deploy/template.yaml" 2>/dev/null || die "Commit $rev has no deploy/template.yaml."
	short8=${rev:0:8}
	short7=${rev:0:7}
	for t in "$TAG" "$short8" "$short7"; do
		if oc image info "$IMAGE:$t" --filter-by-os="$PLATFORM" >/dev/null 2>&1; then
			img_tag=$t
			break
		fi
	done
	[ -n "$img_tag" ] || die "No image $IMAGE for commit $rev (tried the tags $TAG, $short8 and $short7)."
	label=$(image_revision "$IMAGE:$img_tag")
	if [ -n "$label" ] && [ "$label" != "$rev" ] && git cat-file -e "$label^{commit}" 2>/dev/null; then
		if [ "${ALLOW_TEMPLATE_MISMATCH:-}" = 1 ]; then
			err "WARNING: ALLOW_TEMPLATE_MISMATCH=1: $IMAGE:$img_tag reports revision $label, not $rev; applying the template of $rev anyway."
		else
			err "ERROR: $IMAGE:$img_tag reports revision $label, not $rev, so the template of $rev may not fit it. Nothing was changed."
			die "  Use TAG=$label, or set ALLOW_TEMPLATE_MISMATCH=1 to apply the template of $rev anyway."
		fi
	fi
	git show "$rev:deploy/template.yaml" >"$TMP/rollback-template.yaml"
	err "$([ -n "$DRY" ] && echo Validating || echo Applying) $IMAGE:$img_tag with the template of commit $rev..."
	apply_template "$TMP/rollback-template.yaml" "$IMAGE:$img_tag"
	if [ -n "$DRY" ]; then
		err "Dry run only; nothing was changed."
		return 0
	fi
	err "Note: a running cluster operation delays the restart until it finishes (up to ~17 minutes)."
	wait_rollout
	err "Rolled back. Roll forward again with: make upgrade"
}

# Removes the app, its cluster-wide RBAC, its Roles in other namespaces, the
# ConsoleLink and the namespace. RHOAI and everything the app created on the
# cluster (catalog, pull secret, IDMS, test resources) stay.
uninstall() {
	preflight
	local ns
	# shellcheck disable=SC2086 # DRY_FLAG is one word or nothing
	oc delete consolelink,clusterrolebinding,clusterrole -l "app.kubernetes.io/instance=$INSTANCE" --ignore-not-found $DRY_FLAG
	for ns in kube-system openshift-marketplace openshift-ingress; do
		# shellcheck disable=SC2086
		oc delete rolebinding,role -n "$ns" -l "app.kubernetes.io/instance=$INSTANCE" --ignore-not-found $DRY_FLAG
	done
	cleanup_legacy
	# shellcheck disable=SC2086
	oc delete project "$NAMESPACE" --ignore-not-found $DRY_FLAG
	if [ -n "$DRY" ]; then err "Dry run only; nothing was changed."; else err "Removed $APP_NAME from $NAMESPACE."; fi
}

case "$ACTION" in
install)
	require_tag
	if command -v oc >/dev/null 2>&1 && installed; then deploy_or_upgrade upgrade; else deploy_or_upgrade deploy; fi
	;;
deploy | upgrade) deploy_or_upgrade "$ACTION" ;;
rollback) rollback ;;
resolve-image)
	resolve_image
	printf '%s\n' "$REF"
	;;
cleanup-legacy) cleanup_legacy ;;
uninstall) uninstall ;;
esac
