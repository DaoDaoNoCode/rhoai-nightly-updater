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
#     uninstall        remove the app, its RBAC, OAuthClient (revoking its
#                      sign-ins), ConsoleLink and namespace
#   Options (each also has an environment variable):
#     --dry-run              validate with the API server only        DRY_RUN=1
#     --namespace NAME       default rhoai-nightly-updater            NAMESPACE
#     --app-name NAME        default rhoai-nightly-updater            APP_NAME
#     --image-repo REPO      image repository, without a tag          IMAGE
#     --version TAG          vX.Y.Z, main or an 8-character commit    TAG
#                            (or a pre-release: vX.Y.Z-rc.N, ...)
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
# The OAuth client users sign in with is OAuthClient $INSTANCE (cluster-
# scoped, so the namespace is in its name); its secret is in this Secret.
CLIENT_SECRET="$APP_NAME-oauth-client"

# A release (vX.Y.Z) or a pre-release (vX.Y.Z-alpha.N, -beta.N, -rc.N; the
# formats scripts/release.sh tags): its tag names its commit.
is_release() { [[ $1 =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-(alpha|beta|rc)\.[1-9][0-9]*)?$ ]]; }

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

# The oauth-proxy tag must match the cluster's OCP minor version, and the registry namespace
# its major version: openshift4 for 4.x, openshift5 for 5.x. The openshift4 path has no 5.x tag,
# so on an OpenShift 5 cluster that image never pulls (ImagePullBackOff, the proxy never starts).
detect_oauth_proxy_image() {
	[ -z "${OAUTH_PROXY_IMAGE:-}" ] || return 0
	local ver major
	ver=$(oc version -o json 2>/dev/null | sed -n 's/.*"openshiftVersion": *"\([0-9][0-9]*\.[0-9][0-9]*\).*/\1/p' | head -1)
	[ -n "$ver" ] || die "Cannot detect the OCP version; set OAUTH_PROXY_IMAGE=registry.redhat.io/openshift<major>/ose-oauth-proxy-rhel9:v<major>.<minor>"
	major=${ver%%.*}
	OAUTH_PROXY_IMAGE=registry.redhat.io/openshift${major}/ose-oauth-proxy-rhel9:v$ver
}

# --- Image ----------------------------------------------------------------------

# The registry's v2 API, anonymously: a bearer token from the registry's
# token service when it asks for one, never the local pull credentials. The
# calls run in command substitutions, so a failing one says why in the file
# $TMP/registry-why (why_text reads it); no token ever goes into it.

# registry_endpoint REPO: sets REG_HOST, REG_PATH and REG_ORIGIN (scheme and
# host) of REPO's registry, and clears REG_TOKEN and the last failure.
registry_endpoint() {
	REG_HOST=${1%%/*} REG_PATH=${1#*/} REG_TOKEN=""
	case "$REG_HOST" in
	*.* | *:* | localhost) ;;
	*) REG_HOST=docker.io REG_PATH=$1 ;;
	esac
	if [ "$REG_HOST" = docker.io ]; then
		REG_HOST=registry-1.docker.io
		[[ $REG_PATH == */* ]] || REG_PATH=library/$REG_PATH
	fi
	REG_ORIGIN=https://$REG_HOST
	case "$REG_HOST" in localhost | localhost:* | 127.0.0.1:*) REG_ORIGIN=http://$REG_HOST ;; esac
	REG_ACCEPT="application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json,application/vnd.oci.image.manifest.v1+json,application/vnd.docker.distribution.manifest.v2+json"
	rm -f "$TMP/registry-why"
}
registry_why() { printf '%s\n' "$*" >"$TMP/registry-why"; }
# one_line FILE: the text of FILE (an error message) on one line.
one_line() {
	local text
	text=$(tr '\r\n' '  ' <"$1" | sed 's/  */ /g; s/^ //; s/ $//')
	printf '%s\n' "${text:-no message}"
}
why_text() { if [ -s "$TMP/registry-why" ]; then one_line "$TMP/registry-why"; else echo "no answer"; fi; }

# registry_token HEADERS: sets REG_TOKEN to a pull token for REG_PATH from the
# token service that the Bearer challenge in a 401 answer's HEADERS names.
registry_token() {
	local www realm realm_host service out
	www=$(printf '%s\n' "$1" | grep -i '^www-authenticate: *bearer' | head -1 || true)
	realm=$(printf '%s\n' "$www" | sed -n 's/.*realm="\([^"]*\)".*/\1/p')
	service=$(printf '%s\n' "$www" | sed -n 's/.*service="\([^"]*\)".*/\1/p')
	if [ -z "$realm" ]; then
		registry_why "$REG_HOST asks for credentials (HTTP 401) without naming a token service"
		return 1
	fi
	# The token service must be https on the registry host (or Docker Hub's
	# known token host), so a registry answer cannot send the request
	# elsewhere.
	realm_host=${realm#https://}
	realm_host=${realm_host%%[/?]*}
	if [[ $realm != https://* ]] ||
		{ [ "$realm_host" != "$REG_HOST" ] && ! { [ "$REG_HOST" = registry-1.docker.io ] && [ "$realm_host" = auth.docker.io ]; }; }; then
		err "WARNING: ignoring the token service $realm of $REG_HOST (not https on the registry host)."
		registry_why "$REG_HOST names the token service $realm, which is not https on the registry host"
		return 1
	fi
	if ! out=$(curl -sS --max-time 20 --max-filesize 1048576 -G "$realm" --data-urlencode "service=$service" \
		--data-urlencode "scope=repository:$REG_PATH:pull" 2>"$TMP/curl-err"); then
		registry_why "token service $realm: $(one_line "$TMP/curl-err")"
		return 1
	fi
	REG_TOKEN=$(printf '%s\n' "$out" | sed -nE 's/.*"(token|access_token)" *: *"([^"]*)".*/\2/p' | head -1)
	if [ -z "$REG_TOKEN" ]; then
		registry_why "the token service $realm gave no token for $REG_PATH"
		return 1
	fi
}

# registry_head URL: the headers of a HEAD request for URL (with REG_TOKEN
# when set), carriage returns removed.
registry_head() {
	local -a auth=()
	[ -z "$REG_TOKEN" ] || auth=(-H "Authorization: Bearer $REG_TOKEN")
	if ! curl -sS -I --max-time 20 -H "Accept: $REG_ACCEPT" ${auth[@]+"${auth[@]}"} "$1" 2>"$TMP/curl-err" >"$TMP/registry-head"; then
		registry_why "HEAD $1: $(one_line "$TMP/curl-err")"
		return 1
	fi
	tr -d '\r' <"$TMP/registry-head"
}

# registry_digest REPO TAG: the manifest (list) digest of REPO:TAG. Prints
# nothing when the registry does not give one.
registry_digest() {
	local url headers status digest
	registry_endpoint "$1"
	url="$REG_ORIGIN/v2/$REG_PATH/manifests/$2"
	headers=$(registry_head "$url") || return 0
	status=$(printf '%s\n' "$headers" | sed -n '1s/^HTTP[^ ]* \([0-9]*\).*/\1/p')
	if [ "$status" = 401 ]; then
		registry_token "$headers" || return 0
		headers=$(registry_head "$url") || return 0
		status=$(printf '%s\n' "$headers" | sed -n '1s/^HTTP[^ ]* \([0-9]*\).*/\1/p')
	fi
	if [ "$status" != 200 ]; then
		registry_why "HTTP ${status:-?} from $url"
		return 0
	fi
	digest=$(printf '%s\n' "$headers" | sed -n 's/^[Dd]ocker-[Cc]ontent-[Dd]igest: *\(sha256:[0-9a-f]\{64\}\).*/\1/p' | head -1)
	[ -n "$digest" ] || registry_why "no Docker-Content-Digest header from $url"
	printf '%s\n' "$digest"
}

# registry_fetch URL OUT: GETs URL into the file OUT; sets REG_STATUS to the
# HTTP status. A 401 from the registry gets a token, once. Redirects are
# followed here, not by curl -L: the token goes to the registry's own origin
# only, never to the storage host a blob redirects to (Quay sends blobs to
# its CDN), which curl -L with an -H header does on older curl versions.
# A redirect is followed only to https (registry_redirect_ok).
registry_fetch() {
	local url=$1 out=$2 hops=0 w rest origin next
	local -a auth
	while :; do
		rest=${url#*://}
		origin=${url%%://*}://${rest%%[/?#]*}
		auth=()
		[ -z "$REG_TOKEN" ] || [ "$origin" != "$REG_ORIGIN" ] || auth=(-H "Authorization: Bearer $REG_TOKEN")
		if ! w=$(curl -sS --proto =http,https --max-time 30 --max-filesize 4194304 -o "$out" -D "$TMP/registry-headers" \
			-w '%{http_code} %{redirect_url}' -H "Accept: $REG_ACCEPT" ${auth[@]+"${auth[@]}"} "$url" 2>"$TMP/curl-err"); then
			# Without the query: a storage URL's is a signature.
			registry_why "GET ${url%%\?*}: $(one_line "$TMP/curl-err")"
			return 1
		fi
		REG_STATUS=${w%% *}
		case "$REG_STATUS" in
		301 | 302 | 303 | 307 | 308)
			hops=$((hops + 1))
			if [ "$hops" -gt 5 ] || [ -z "${w#* }" ]; then
				registry_why "GET ${url%%\?*}: HTTP $REG_STATUS, too many redirects or no Location"
				return 1
			fi
			next=${w#* }
			if ! registry_redirect_ok "$next"; then
				# Without the query: a storage URL's is a signature.
				registry_why "GET ${url%%\?*}: HTTP $REG_STATUS redirects to ${next%%\?*}, which is not https (only a registry on this machine may redirect to http, on this machine)"
				return 1
			fi
			url=$next
			;;
		401)
			[ -z "$REG_TOKEN" ] && [ "$origin" = "$REG_ORIGIN" ] || return 0
			registry_token "$(tr -d '\r' <"$TMP/registry-headers")" || return 1
			;;
		*) return 0 ;;
		esac
	done
}

# registry_redirect_ok URL: whether registry_fetch follows a redirect to URL:
# https, or http when the registry itself is on this machine (registry_endpoint
# gives REG_ORIGIN http:// only for localhost and 127.0.0.1) and URL is on
# this machine too. A host with user information (http://localhost@x/) is
# another host.
registry_redirect_ok() {
	local host
	case "$1" in
	https://*) return 0 ;;
	http://*)
		[[ $REG_ORIGIN == http://* ]] || return 1
		host=${1#http://}
		host=${host%%[/?#]*}
		[[ $host != *@* ]] || return 1
		case "$host" in localhost | localhost:* | 127.0.0.1 | 127.0.0.1:*) return 0 ;; esac
		;;
	esac
	return 1
}

# json_flat: the JSON on stdin as "path<TAB>value" lines, one per scalar
# value, the path's keys and array indexes joined by "/"
# (config/Labels/version). With jq when there is one, else awk.
json_flat() { if command -v jq >/dev/null 2>&1; then json_flat_jq; else json_flat_awk; fi; }
json_flat_jq() {
	jq -r 'paths(type != "object" and type != "array") as $p | [($p | map(tostring) | join("/")), (getpath($p) | tostring)] | @tsv'
}
# The same without jq, for compact and indented JSON alike; strings keep
# their escapes but \" and \/ (as jq's @tsv prints them, for the values read
# here: digests, platforms, commits).
json_flat_awk() {
	awk '
	function emit(v,   p, i) {
		p = key[1]
		for (i = 2; i <= d; i++) p = p "/" key[i]
		print p "\t" v
	}
	{
		s = $0
		while (s != "") {
			if (match(s, /^[ \t\r]+/)) { s = substr(s, RLENGTH + 1); continue }
			c = substr(s, 1, 1)
			if (c == "\"") {
				if (!match(s, /^"([^"\\]|\\.)*"/)) exit 1
				t = substr(s, 2, RLENGTH - 2)
				s = substr(s, RLENGTH + 1)
				gsub(/\\"/, "\"", t)
				gsub(/\\\//, "/", t)
				if (type[d] == "o" && want[d]) { key[d] = t; want[d] = 0 } else emit(t)
				continue
			}
			if (c == "{" || c == "[") { d++; type[d] = (c == "{") ? "o" : "a"; want[d] = 1; key[d] = 0 }
			else if (c == "}" || c == "]") d--
			else if (c == ",") { if (type[d] == "a") key[d]++; else want[d] = 1 }
			else if (c != ":") {
				if (!match(s, /^[-+.0-9A-Za-z]+/)) exit 1
				emit(substr(s, 1, RLENGTH))
				s = substr(s, RLENGTH + 1)
				continue
			}
			s = substr(s, 2)
		}
	}'
}

# sha256_file FILE: the hex SHA-256 of FILE.
sha256_file() {
	local sum
	if command -v sha256sum >/dev/null 2>&1; then
		sum=$(sha256sum <"$1")
	elif command -v shasum >/dev/null 2>&1; then
		sum=$(shasum -a 256 <"$1")
	elif command -v openssl >/dev/null 2>&1; then
		sum=$(openssl dgst -sha256 -r <"$1")
	else
		return 1
	fi
	printf '%s\n' "${sum%% *}"
}

# registry_config_label REF LABEL: the value of the label LABEL of the image
# REF (REPO@DIGEST or REPO:TAG; of a manifest list, its $PLATFORM image),
# from the image's config blob; nothing when the image has no such label.
# Fails, saying why, when the registry does not serve the image. Every
# manifest read by digest must be the bytes that digest names: REF's own
# (REPO@sha256:...; another algorithm is not checked, so not read) and a
# manifest list's entry for $PLATFORM, as the config blob is checked below.
registry_config_label() {
	local ref=$1 label=$2 repo reference url flat config os arch variant sum n expect=""
	if [[ $ref == *@* ]]; then
		repo=${ref%@*} reference=${ref#*@}
		if ! [[ $reference =~ ^sha256:[0-9a-f]{64}$ ]]; then
			registry_why "$ref is not pinned by a sha256 digest, which this installer checks"
			return 1
		fi
		expect=$reference
	elif [[ ${ref##*/} == *:* ]]; then
		repo=${ref%:*} reference=${ref##*:}
	else
		repo=$ref reference=latest
	fi
	registry_endpoint "$repo"
	os=${PLATFORM%%/*} arch=${PLATFORM#*/} variant=""
	if [[ $arch == */* ]]; then variant=${arch#*/} arch=${arch%%/*}; fi
	for n in 1 2; do
		url="$REG_ORIGIN/v2/$REG_PATH/manifests/$reference"
		registry_fetch "$url" "$TMP/manifest.json" || return 1
		if [ "$REG_STATUS" != 200 ]; then
			registry_why "HTTP $REG_STATUS from $url"
			return 1
		fi
		if [ -n "$expect" ]; then
			if ! sum=$(sha256_file "$TMP/manifest.json"); then
				registry_why "no sha256sum, shasum or openssl to check the manifest of $url"
				return 1
			fi
			if [ "sha256:$sum" != "$expect" ]; then
				registry_why "the manifest from $url does not match its digest"
				return 1
			fi
		fi
		flat=$(json_flat <"$TMP/manifest.json" || true)
		config=$(printf '%s\n' "$flat" | awk -F'\t' '$1 == "config/digest" {print $2; exit}')
		[ -z "$config" ] || break
		# A manifest list (image index): its entry for $PLATFORM, as
		# oc image info --filter-by-os picks it.
		reference=$(printf '%s\n' "$flat" | awk -F'\t' -v os="$os" -v arch="$arch" -v variant="$variant" '
			$1 ~ /^manifests\/[0-9]+\// {
				split($1, p, "/")
				i = p[2]
				if (!(i in seen)) { seen[i] = 1; order[++n] = i }
				v[i, substr($1, length(p[1]) + length(i) + 3)] = $2
			}
			END {
				for (j = 1; j <= n; j++) {
					i = order[j]
					if (v[i, "platform/os"] == os && v[i, "platform/architecture"] == arch &&
						(variant == "" || v[i, "platform/variant"] == variant)) { print v[i, "digest"]; exit }
				}
			}')
		if [ "$n" = 2 ] || ! [[ $reference =~ ^sha256:[0-9a-f]{64}$ ]]; then
			registry_why "$url is neither an image manifest nor a manifest list with a $PLATFORM image"
			return 1
		fi
		# The list (checked above when REF is a digest; a tag cannot be)
		# names its image by digest: the image manifest must match it.
		expect=$reference
	done
	if ! [[ $config =~ ^sha256:[0-9a-f]{64}$ ]]; then
		registry_why "$url names no valid config digest"
		return 1
	fi
	url="$REG_ORIGIN/v2/$REG_PATH/blobs/$config"
	registry_fetch "$url" "$TMP/config.json" || return 1
	if [ "$REG_STATUS" != 200 ]; then
		registry_why "HTTP $REG_STATUS from $url"
		return 1
	fi
	# It may come from another host (the registry's blob storage): it must
	# be the content its digest names.
	if ! sum=$(sha256_file "$TMP/config.json"); then
		registry_why "no sha256sum, shasum or openssl to check the config blob of $url"
		return 1
	fi
	if [ "sha256:$sum" != "$config" ]; then
		registry_why "the config blob from $url does not match its digest"
		return 1
	fi
	json_flat <"$TMP/config.json" | awk -F'\t' -v k="config/Labels/$label" '$1 == k {print $2; exit}'
}

# image_revision REF: sets REVISION to the org.opencontainers.image.revision
# label (a full commit) of the image REF, empty when it has none: from the
# registry's v2 API, else oc with the local pull credentials. When neither
# can read the image, REVISION_ERROR says why, one line for each.
image_revision() {
	local label info why
	REVISION="" REVISION_ERROR=""
	if ! label=$(registry_config_label "$1" org.opencontainers.image.revision); then
		why="the registry API: $(why_text)"
		if ! info=$(oc image info "$1" --filter-by-os="$PLATFORM" -o json 2>"$TMP/oc-err"); then
			REVISION_ERROR="$why"$'\n'"oc image info: $(one_line "$TMP/oc-err")"
			return 0
		fi
		label=$(printf '%s\n' "$info" | json_flat |
			awk -F'\t' '$1 ~ /(^|\/)config\/Labels\/org\.opencontainers\.image\.revision$/ {print $2; exit}')
	fi
	if [[ $label =~ ^[0-9a-f]{40}$ ]]; then REVISION=$label; fi
}

# revision_help TRUST: why the revision label could not be read, and what to
# do; TRUST says which revision the image must have.
revision_help() {
	printf '%s\n' "$REVISION_ERROR" | sed 's/^/  Tried /' >&2
	err "  Check that this machine can reach the registry and read the image, for example:"
	err "    oc image info $REF --filter-by-os=$PLATFORM | grep revision="
	err "  Only if you trust the image ($1), set ALLOW_TEMPLATE_MISMATCH=1 to apply this template anyway."
}

# resolve_image: sets REF to IMAGE@sha256:... for IMAGE:TAG and checks that
# the image was built from the commit of the template that will be applied.
# pin_image TAG: sets REF to IMAGE@sha256:... for IMAGE:TAG, from the
# registry's v2 API, else oc with the local pull credentials. Without a
# digest it stops, unless ALLOW_MUTABLE_TAG=1 (REF is then the tag).
pin_image() {
	local tag=$1 digest info why=""
	digest=$(registry_digest "$IMAGE" "$tag")
	if [ -z "$digest" ]; then
		why="the registry API: $(why_text)"
		if info=$(oc image info "$IMAGE:$tag" --filter-by-os="$PLATFORM" -o json 2>"$TMP/oc-err"); then
			digest=$(printf '%s\n' "$info" | json_flat | awk -F'\t' '$1 == "listDigest" {l = $2} $1 == "digest" {d = $2} END {print (l != "" ? l : d)}')
			[[ $digest =~ ^sha256:[0-9a-f]{64}$ ]] || digest=""
			[ -n "$digest" ] || why+=$'\n'"oc image info: no digest in its answer"
		else
			why+=$'\n'"oc image info: $(one_line "$TMP/oc-err")"
		fi
	fi
	if [ -n "$digest" ]; then
		REF="$IMAGE@$digest"
		err "Resolved $IMAGE:$tag to $REF"
	elif [ "${ALLOW_MUTABLE_TAG:-}" = 1 ]; then
		REF="$IMAGE:$tag"
		err "WARNING: ALLOW_MUTABLE_TAG=1: cannot resolve $IMAGE:$tag to a digest; deploying the tag, which may move before the pod pulls it."
	else
		err "ERROR: cannot resolve $IMAGE:$tag to a digest (no such tag, or the registry is unreachable). Nothing was changed."
		printf '%s\n' "$why" | sed 's/^/  Tried /' >&2
		err "  Image tags are releases (vX.Y.Z, vN), pre-releases (vX.Y.Z-rc.N, ...), main, latest or an 8-character commit. Set ALLOW_MUTABLE_TAG=1 to deploy the tag itself."
		exit 1
	fi
}

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

	# 1. The digest.
	pin_image "$TAG"

	if [ "${ALLOW_TEMPLATE_MISMATCH:-}" = 1 ]; then
		err "WARNING: ALLOW_TEMPLATE_MISMATCH=1: not checking that $REF matches the template."
		return 0
	fi

	# 2. The commit the image was built from: its revision label (set by the
	#    Containerfile).
	local label
	image_revision "$REF"
	label=$REVISION
	if [ "$MODE" = release ]; then
		if [ -n "$REVISION_ERROR" ]; then
			err "ERROR: cannot read the revision label of $REF, so cannot check that it was built from $EMBEDDED_COMMIT, the commit of $EMBEDDED_VERSION whose template this installer holds. Nothing was changed."
			revision_help "its revision must be $EMBEDDED_COMMIT"
			exit 1
		fi
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
	if [ -z "$rev" ] && [ -n "$REVISION_ERROR" ]; then
		err "ERROR: cannot read the revision label of $REF, so cannot tell which commit it was built from. Nothing was changed."
		revision_help "its revision must be a commit whose deploy/template.yaml is this checkout's"
		exit 1
	elif [ -z "$rev" ]; then
		err "ERROR: cannot tell which commit $REF was built from (no revision label of this repository; run 'git fetch origin' if it is newer than this clone)."
		err "  Set ALLOW_TEMPLATE_MISMATCH=1 to apply this checkout's template anyway."
		exit 1
	elif [ -n "$REVISION_ERROR" ]; then
		err "WARNING: cannot read the revision label of $REF; trusting that tag $TAG names commit $rev."
		printf '%s\n' "$REVISION_ERROR" | sed 's/^/  Tried /' >&2
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

# uses_oauth_client TEMPLATE: whether the template signs users in through
# OAuthClient $INSTANCE (4.0.0 and later) rather than through the app's
# ServiceAccount as the OAuth client (earlier releases, which a rollback
# may apply).
uses_oauth_client() {
	# shellcheck disable=SC2016 # the template's own ${PARAM} placeholders
	grep -qF -- '--client-id=${APP_NAME}-${NAMESPACE}' "$1"
}

# The OAuth client secret, Secret $CLIENT_SECRET (key client-secret): 32
# random alphanumerics without a trailing newline (oauth-proxy reads the
# file raw). Created once and kept, so re-applies keep the OAuthClient and
# everyone's session; delete the Secret and re-run to rotate it.
ensure_client_secret() {
	if oc get secret "$CLIENT_SECRET" -n "$NAMESPACE" >/dev/null 2>&1; then
		err "Secret $CLIENT_SECRET exists; keeping it."
		return 0
	fi
	local secret
	secret=$(head -c 64 /dev/urandom | base64 | tr -dc 'a-zA-Z0-9' | head -c 32)
	[ ${#secret} -eq 32 ] || die "Could not produce a 32-character OAuth client secret"
	printf '%s' "$secret" >"$TMP/client-secret"
	unset secret
	oc create secret generic "$CLIENT_SECRET" -n "$NAMESPACE" --from-file=client-secret="$TMP/client-secret" >/dev/null
	rm -f "$TMP/client-secret"
	err "Created Secret $CLIENT_SECRET."
}

# oauth_client_manifest HOST SECRET: OAuthClient $INSTANCE, as RHOAI's
# operator makes its own (data-science): grantMethod auto (no "Authorize
# Access" page), one redirect URI on the Route's host (the OAuth server
# matches scheme and host exactly), and the client secret. Its labels name
# the install's app and namespace apart (oauth_client_owner reads them): the
# name alone is the same for app a-b in namespace c and app a in namespace
# b-c.
#
# The OAuth server also answers the implicit flow (response_type=token)
# for any client, so with grantMethod auto whoever later serves the
# redirect URI's host (a project admin's Route with that host, once the
# namespace is gone) could collect a signed-in user's token from a crafted
# link; the client secret does not stop that. Hence:
#   - scopeRestrictions: exactly the scope oauth-proxy asks for, so the
#     client's id cannot get any other scope;
#   - uninstall and rollback delete it and revoke its sign-ins
#     (remove_oauth_client). Nothing else removes it: deleting the
#     namespace by hand leaves the client (live-checked: the garbage
#     collector did not delete a client whose ownerReference named the
#     deleted namespace, so it has none), and `install.sh uninstall`
#     removes it even after the namespace is gone.
# The chart's hook applies the same object (scripts/test-chart.sh compares
# them).
oauth_client_manifest() {
	local host=$1 secret=$2
	printf '%s\n' \
		'apiVersion: oauth.openshift.io/v1' \
		'kind: OAuthClient' \
		'metadata:' \
		"  name: $INSTANCE" \
		'  labels:' \
		"    app.kubernetes.io/instance: $INSTANCE" \
		"    app.kubernetes.io/name: \"$APP_NAME\"" \
		"    rhoai-nightly-updater/namespace: \"$NAMESPACE\"" \
		'grantMethod: auto' \
		'redirectURIs:' \
		"  - https://$host/oauth/callback" \
		'scopeRestrictions:' \
		'  - literals:' \
		'      - user:full'
	printf 'secret: %s\n' "$secret"
}

# oauth_client_owner HOST: sets CLIENT_OWNER to whose OAuthClient $INSTANCE
# is: "absent"; "ours" when its labels name this install's app and
# namespace, or, without them (a client applied before the labels), when
# its only redirect URI is this install's Route (HOST); else "foreign",
# with CLIENT_FOREIGN saying why. Its name cannot tell: app a-b in
# namespace c and app a in namespace b-c both use a-b-c, and applying ours
# with --force-conflicts would take the other install's client over (its
# users would sign in to this app). Stops on any other read error: nothing
# may be applied or deleted without knowing.
oauth_client_owner() {
	local host=$1 out rc=0 app ns uris
	CLIENT_OWNER="" CLIENT_FOREIGN=""
	out=$(oc get oauthclient "$INSTANCE" -o jsonpath='{.metadata.labels.app\.kubernetes\.io/name}{"|"}{.metadata.labels.rhoai-nightly-updater/namespace}{"|"}{.redirectURIs[*]}{"\n"}' 2>&1) || rc=$?
	if [ "$rc" -ne 0 ]; then
		case "$out" in
		*NotFound* | *"not found"*)
			CLIENT_OWNER=absent
			return 0
			;;
		esac
		die "Cannot read OAuthClient $INSTANCE, so this install cannot tell whether it is its own: $out"
	fi
	IFS='|' read -r app ns uris <<<"$out" || true
	if [ -n "$app" ] || [ -n "$ns" ]; then
		if [ "$app" = "$APP_NAME" ] && [ "$ns" = "$NAMESPACE" ]; then
			CLIENT_OWNER=ours
		else
			CLIENT_OWNER=foreign CLIENT_FOREIGN="it belongs to app ${app:-?} in namespace ${ns:-?}"
		fi
	elif [ -n "$host" ] && [ "$uris" = "https://$host/oauth/callback" ]; then
		CLIENT_OWNER=ours
	else
		CLIENT_OWNER=foreign CLIENT_FOREIGN="it has no labels of an install and redirects to ${uris:-nothing}, not to this install's Route${host:+ (https://$host/oauth/callback)}"
	fi
}

# refuse_foreign_client HOST: stops, before anything is changed, when
# OAuthClient $INSTANCE is another install's (oauth_client_owner).
refuse_foreign_client() {
	oauth_client_owner "$1"
	[ "$CLIENT_OWNER" = foreign ] || return 0
	err "ERROR: OAuthClient $INSTANCE exists and is not this install's: $CLIENT_FOREIGN."
	err "  Its name, <APP_NAME>-<NAMESPACE>, is the same for app a-b in namespace c and app a in namespace b-c. Use another --app-name or --namespace."
	err "  If it is left from an install that is gone, delete it and the sign-ins it issued, then re-run:"
	err "    oc delete oauthclient $INSTANCE"
	err "    oc delete oauthaccesstokens --field-selector clientName=$INSTANCE"
	die "Nothing was changed."
}

route_host() {
	oc get route "$APP_NAME" -n "$NAMESPACE" -o jsonpath='{.spec.host}' 2>/dev/null || true
}

# The OAuthClient needs the Route host, which the API server assigns when
# the Route is created; the router does not have to admit it first. The
# secret goes to oc on stdin, never on a command line. Server-side apply, so
# no last-applied annotation repeats the secret. A dry run validates the
# object with the API server (a placeholder secret, and a placeholder host
# when the Route does not exist yet).
oauth_client() {
	local host secret owners
	host=$(route_host)
	# Checked again right before the apply (also before anything changed).
	oauth_client_owner "$host"
	[ "$CLIENT_OWNER" != foreign ] ||
		die "OAuthClient $INSTANCE is not this install's ($CLIENT_FOREIGN), so it was not changed; users cannot sign in to this install until it is. See: oc get oauthclient $INSTANCE --show-labels"
	if [ -n "$DRY" ]; then
		oauth_client_manifest "${host:-$APP_NAME-$NAMESPACE.apps.example.com}" dry-run-placeholder |
			oc apply --server-side --field-manager=rhoai-nightly-updater-install --force-conflicts --dry-run=server -f - >/dev/null
		err "Validated OAuthClient $INSTANCE."
		return 0
	fi
	[ -n "$host" ] || die "Route $APP_NAME has no host yet; users cannot sign in until OAuthClient $INSTANCE exists. Re-run this command."
	secret=$(oc get secret "$CLIENT_SECRET" -n "$NAMESPACE" -o jsonpath='{.data.client-secret}' | base64 -d)
	[ -n "$secret" ] || die "Secret $CLIENT_SECRET has no client-secret; delete it and re-run this command."
	oauth_client_manifest "$host" "$secret" |
		oc apply --server-side --field-manager=rhoai-nightly-updater-install --force-conflicts -f - >/dev/null
	unset secret
	# 4.0.0 test builds gave the client an ownerReference to the namespace,
	# which the garbage collector did not act on (live-checked: the client
	# outlived its deleted namespace). The apply above drops it only when
	# this field manager set it, not when the chart's hook did, so a client
	# that still has one loses it here.
	owners=$(oc get oauthclient "$INSTANCE" -o jsonpath='{.metadata.ownerReferences[*].uid}') ||
		die "Cannot read OAuthClient $INSTANCE after applying it. Re-run this command."
	if [ -n "$owners" ]; then
		oc patch oauthclient "$INSTANCE" --type=merge -p '{"metadata":{"ownerReferences":null}}' >/dev/null ||
			die "Cannot remove the ownerReference of OAuthClient $INSTANCE. Re-run this command."
		err "Removed the ownerReference a 4.0.0 test build put on OAuthClient $INSTANCE."
	fi
	err "OAuthClient $INSTANCE signs users in at https://$host (redirect https://$host/oauth/callback)."
}

# write_params IMAGE_REF: the template parameters this script sets, in
# $TMP/params. OAUTH_PROXY_IMAGE is unset only when uninstall renders the
# template (it needs no image), and then keeps the template's default.
write_params() {
	printf '%s\n' "IMAGE=$1" "NAMESPACE=$NAMESPACE" "APP_NAME=$APP_NAME" "IMAGE_REPOSITORY=$IMAGE" >"$TMP/params"
	[ -z "${OAUTH_PROXY_IMAGE:-}" ] || printf 'OAUTH_PROXY_IMAGE=%s\n' "$OAUTH_PROXY_IMAGE" >>"$TMP/params"
	[ -z "${RELEASES_URL:-}" ] || printf 'RELEASES_URL=%s\n' "$RELEASES_URL" >>"$TMP/params"
}

# apply_template TEMPLATE IMAGE_REF: oc process | oc apply. The template is
# processed locally (--local), so it does not depend on the current project,
# which may not exist. Parameters go
# through a private file. A template that still takes a COOKIE_SECRET
# parameter (older releases) gets the value of the current Secret.
apply_template() {
	local template=$1 image_ref=$2 secret
	write_params "$image_ref"
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
	refuse_foreign_client "$(route_host)"
	if [ -z "$DRY" ]; then
		if [ "$action" = deploy ]; then
			oc get project "$NAMESPACE" >/dev/null 2>&1 || oc new-project "$NAMESPACE" >/dev/null
		else
			err "Note: a running cluster operation delays the restart until it finishes (up to ~17 minutes)."
		fi
		ensure_proxy_secret
		ensure_client_secret
	fi
	err "$([ -n "$DRY" ] && echo Validating || echo Applying) $REF with the template of ${TAG}..."
	apply_template "$TEMPLATE" "$REF"
	# Before the rollout, so the new pod's sign-in works at once. An upgrade
	# from a release whose ServiceAccount was the OAuth client gets the
	# OAuthClient here; the Deployment rolls because its proxy args changed.
	oauth_client
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
	local rev short8 short7 img_tag="" t label tried=""
	rev=$(git rev-parse --verify --quiet "$TAG^{commit}") || die "Commit $TAG is not in this clone; run 'git fetch origin --tags' and retry."
	git cat-file -e "$rev:deploy/template.yaml" 2>/dev/null || die "Commit $rev has no deploy/template.yaml."
	short8=${rev:0:8}
	short7=${rev:0:7}
	for t in "$TAG" "$short8" "$short7"; do
		if [ -n "$(registry_digest "$IMAGE" "$t")" ]; then
			img_tag=$t
			break
		fi
		tried+="Tried $t: the registry API: $(why_text)"$'\n'
		if oc image info "$IMAGE:$t" --filter-by-os="$PLATFORM" >/dev/null 2>"$TMP/oc-err"; then
			img_tag=$t
			break
		fi
		tried+="Tried $t: oc image info: $(one_line "$TMP/oc-err")"$'\n'
	done
	if [ -z "$img_tag" ]; then
		err "No image $IMAGE for commit $rev (tried the tags $TAG, $short8 and $short7)."
		printf '%s' "$tried" | sed 's/^/  /' >&2
		exit 1
	fi
	# By digest, like deploy and upgrade: the checked image is the one applied.
	pin_image "$img_tag"
	image_revision "$REF"
	label=$REVISION
	if [ -n "$REVISION_ERROR" ]; then
		err "WARNING: cannot read the revision label of $REF; trusting that tag $img_tag names commit $rev."
		printf '%s\n' "$REVISION_ERROR" | sed 's/^/  Tried /' >&2
	fi
	if [ -n "$label" ] && [ "$label" != "$rev" ] && git cat-file -e "$label^{commit}" 2>/dev/null; then
		if [ "${ALLOW_TEMPLATE_MISMATCH:-}" = 1 ]; then
			err "WARNING: ALLOW_TEMPLATE_MISMATCH=1: $IMAGE:$img_tag reports revision $label, not $rev; applying the template of $rev anyway."
		else
			err "ERROR: $IMAGE:$img_tag reports revision $label, not $rev, so the template of $rev may not fit it. Nothing was changed."
			die "  Use TAG=$label, or set ALLOW_TEMPLATE_MISMATCH=1 to apply the template of $rev anyway."
		fi
	fi
	git show "$rev:deploy/template.yaml" >"$TMP/rollback-template.yaml"
	# A template that signs users in through OAuthClient $INSTANCE needs its
	# secret and the client; an older one signs in through the ServiceAccount,
	# so once it runs this install's client is deleted and its sign-ins
	# revoked (an unused client with grantMethod auto is only a risk). The
	# client Secret stays for a roll forward.
	local client=""
	! uses_oauth_client "$TMP/rollback-template.yaml" || client=1
	[ -z "$client" ] || refuse_foreign_client "$(route_host)"
	[ -n "$DRY" ] || ensure_proxy_secret
	[ -n "$DRY" ] || [ -z "$client" ] || ensure_client_secret
	err "$([ -n "$DRY" ] && echo Validating || echo Applying) $REF with the template of commit $rev..."
	apply_template "$TMP/rollback-template.yaml" "$REF"
	[ -z "$client" ] || oauth_client
	if [ -n "$DRY" ]; then
		[ -n "$client" ] || remove_oauth_client || true
		err "Dry run only; nothing was changed."
		return 0
	fi
	err "Note: a running cluster operation delays the restart until it finishes (up to ~17 minutes)."
	wait_rollout
	if [ -z "$client" ]; then
		if ! remove_oauth_client; then
			err "ERROR: rolled back, but OAuthClient $INSTANCE, which the rolled-back build does not use, could not be deleted. Delete it and revoke its sign-ins:"
			err "  oc delete oauthclient $INSTANCE"
			die "  oc delete oauthaccesstokens --field-selector clientName=$INSTANCE"
		fi
		[ -n "$REVOKED" ] || die "ERROR: rolled back and OAuthClient $INSTANCE deleted, but its sign-ins could not be revoked: oc delete oauthaccesstokens --field-selector clientName=$INSTANCE"
	fi
	err "Rolled back. Roll forward again with: make upgrade"
}

# rbac_namespaces: the namespaces other than NAMESPACE where the template
# creates Roles or RoleBindings, read from the template rendered locally
# with this install's parameters, so uninstall misses none the template has.
# Called in a condition, where set -e is off: each failure returns.
rbac_namespaces() {
	write_params "$IMAGE"
	oc process -f "$TEMPLATE" --param-file="$TMP/params" --ignore-unknown-parameters --local \
		-o jsonpath='{range .items[*]}{.kind} {.metadata.namespace}{"\n"}{end}' >"$TMP/kinds" || return 1
	rm -f "$TMP/params"
	awk -v ns="$NAMESPACE" '($1 == "Role" || $1 == "RoleBinding") && $2 != "" && $2 != ns {print $2}' "$TMP/kinds" | sort -u
}

# Removes the app's OAuthClient and the sign-ins it issued, the app, its
# cluster-wide RBAC, its Roles in other namespaces, the ConsoleLink and the
# namespace. RHOAI and everything the app created on the cluster (catalog,
# pull secret, IDMS, test resources) stay.
# remove_oauth_client: deletes OAuthClient $INSTANCE when it is this
# install's (oauth_client_owner), so nobody signs in through it again, then
# revokes the access tokens it issued: deleting a client does not invalidate
# them (the API server does not check that a token's client still exists),
# and each is a user's full token that would stay valid until it expires.
# Another install's client and its sign-ins are left alone; when there is
# no client, the tokens issued under its name are still revoked. Returns 1
# when the client could not be deleted; REVOKED is empty when the
# revocation failed.
remove_oauth_client() {
	REVOKED=1
	oauth_client_owner "$(route_host)"
	case "$CLIENT_OWNER" in
	foreign)
		err "Keeping OAuthClient $INSTANCE and its sign-ins: it is not this install's ($CLIENT_FOREIGN)."
		return 0
		;;
	ours)
		# shellcheck disable=SC2086 # DRY_FLAG is one word or nothing
		oc delete oauthclient "$INSTANCE" --ignore-not-found $DRY_FLAG || return 1
		;;
	esac
	# shellcheck disable=SC2086
	oc delete oauthaccesstokens --field-selector "clientName=$INSTANCE" $DRY_FLAG || REVOKED=""
}

uninstall() {
	preflight
	local ns namespaces
	namespaces=$(rbac_namespaces) || die "Cannot render $TEMPLATE to find the namespaces of its Roles; nothing was deleted."
	# First the OAuthClient. If it stays, so does everything else: while the
	# namespace and its Route exist, nobody else can claim the Route's host
	# and receive sign-ins through the client (a cluster-admin's full token
	# with grantMethod auto).
	if ! remove_oauth_client; then
		err "ERROR: OAuthClient $INSTANCE could not be deleted, so nothing else was removed: the namespace keeps its Route's host, which the client's redirect URI names. Delete it, then run uninstall again:"
		die "  oc delete oauthclient $INSTANCE"
	fi
	# shellcheck disable=SC2086
	oc delete consolelink,clusterrolebinding,clusterrole -l "app.kubernetes.io/instance=$INSTANCE" --ignore-not-found $DRY_FLAG
	for ns in $namespaces; do
		# shellcheck disable=SC2086
		oc delete rolebinding,role -n "$ns" -l "app.kubernetes.io/instance=$INSTANCE" --ignore-not-found $DRY_FLAG
	done
	cleanup_legacy
	# shellcheck disable=SC2086
	oc delete project "$NAMESPACE" --ignore-not-found $DRY_FLAG
	if [ -n "$DRY" ]; then err "Dry run only; nothing was changed."; else err "Removed $APP_NAME from $NAMESPACE."; fi
	if [ -z "$REVOKED" ]; then
		err "ERROR: the sign-ins of OAuthClient $INSTANCE could not be revoked; each stays a valid OpenShift token until it expires. Revoke them with:"
		die "  oc delete oauthaccesstokens --field-selector clientName=$INSTANCE"
	fi
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
