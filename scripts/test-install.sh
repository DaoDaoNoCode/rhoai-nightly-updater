#!/usr/bin/env bash
# shellcheck disable=SC2015 # "check && pass || fail": pass never fails
# Tests for scripts/install.sh and the release asset that
# `scripts/release.sh installer` builds from it. Offline: fake oc and curl
# commands record what the installer would do.
#   bash scripts/test-install.sh
set -euo pipefail

# Start from a clean environment: install.sh and release.sh read these
# variables, and CI defines some of them (GitLab sets IMAGE globally), which
# would override the values each test passes.
unset ACTION ALLOW_MUTABLE_TAG ALLOW_TEMPLATE_MISMATCH APP_NAME DRY_RUN IMAGE \
	NAMESPACE OAUTH_PROXY_IMAGE PLATFORM RELEASES_URL ROLLOUT_TIMEOUT TAG

HERE=$(cd "$(dirname "$0")" && pwd)
SRC=$(cd "$HERE/.." && pwd)
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
FAILED=0
PASSED=0
fail() {
	printf 'FAIL: %s\n' "$*" >&2
	FAILED=$((FAILED + 1))
}
pass() { PASSED=$((PASSED + 1)); }
expect_ok() {
	local name=$1 out
	shift
	if out=$("$@" 2>&1); then pass; else fail "$name: expected success, got: $out"; fi
}
expect_fail() {
	local name=$1 text=$2 out
	shift 2
	if out=$("$@" 2>&1); then
		fail "$name: expected failure, got success: $out"
	elif grep -qF -- "$text" <<<"$out"; then
		pass
	else
		fail "$name: expected '$text' in: $out"
	fi
}
expect_eq() { if [ "$2" = "$3" ]; then pass; else fail "$1: want '$2', got '$3'"; fi; }

export GIT_AUTHOR_NAME=test GIT_AUTHOR_EMAIL=test@example.com
export GIT_COMMITTER_NAME=test GIT_COMMITTER_EMAIL=test@example.com
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1

# --- A clone with this installer and template, tagged v9.8.7 ----------------
REPO="$WORK/repo"
git init -q -b main "$REPO"
mkdir -p "$REPO/scripts" "$REPO/deploy"
cp "$SRC/scripts/install.sh" "$SRC/scripts/release.sh" "$REPO/scripts/"
cp "$SRC/deploy/template.yaml" "$REPO/deploy/template.yaml"
cp "$SRC/Makefile" "$REPO/Makefile"
git -C "$REPO" add -A
git -C "$REPO" commit -qm release
git -C "$REPO" tag -a v9.8.7 -m v9.8.7
COMMIT=$(git -C "$REPO" rev-parse HEAD)

# --- The release asset ----------------------------------------------------------
ASSET="$WORK/install.sh"
(cd "$REPO" && IMAGE=quay.io/example/app sh scripts/release.sh installer v9.8.7 v9.8.7) >"$ASSET"
expect_ok "asset parses" bash -n "$ASSET"
if bash "$ASSET" --print-template | cmp -s - "$SRC/deploy/template.yaml"; then pass; else fail "asset: the embedded template differs from deploy/template.yaml"; fi
expect_eq "asset version" v9.8.7 "$(bash "$ASSET" --print-version)"
grep -qx "EMBEDDED_COMMIT='$COMMIT'" "$ASSET" && pass || fail "asset: commit not embedded"
grep -qx "EMBEDDED_IMAGE='quay.io/example/app'" "$ASSET" && pass || fail "asset: image not embedded"
expect_fail "asset installs only its version" "installs v9.8.7 only" bash "$ASSET" --version v1.0.0 --print-version
expect_fail "installer needs IMAGE" "IMAGE must be set" sh -c "cd '$REPO' && IMAGE= sh scripts/release.sh installer v9.8.7"
expect_eq "clone version" v9.8.7 "$(bash "$REPO/scripts/install.sh" --print-version)"

# --- Fake oc and curl ---------------------------------------------------------
BIN="$WORK/bin"
LOG="$WORK/oc.log"
mkdir -p "$BIN"
cat >"$BIN/oc" <<'FAKE'
#!/usr/bin/env bash
# Fake oc. FAKE_* variables choose the cluster's answers; every call is logged.
printf 'oc %s\n' "$*" >>"$FAKE_LOG"
case "$1 $2" in
"whoami ") echo admin ;;
"whoami --show-server") echo https://api.example.com:6443 ;;
"auth can-i") echo "${FAKE_ADMIN:-yes}"; [ "${FAKE_ADMIN:-yes}" = yes ] ;;
"version -o") echo "{\"openshiftVersion\": \"${FAKE_OCP_VERSION:-4.19.3}\"}" ;;
"image info")
	[ -n "${FAKE_REVISION:-}" ] || exit 1
	# FAKE_NO_OC_DIGEST: oc reads the labels but no digest.
	d="$(printf 'b%.0s' $(seq 64))"
	[ -z "${FAKE_NO_OC_DIGEST:-}" ] || d=none
	printf '{\n  "digest": "sha256:%s",\n  "config": {"config": {"Labels": {\n    "org.opencontainers.image.revision": "%s"\n  }}}\n}\n' \
		"$d" "$FAKE_REVISION" ;;
"get clusterrolebinding") [ -z "${FAKE_LEGACY:-}" ] || echo "$FAKE_LEGACY" ;;
"get route") echo app.example.com ;;
"get consolelink") [ -z "${FAKE_LEGACY:-}" ] || echo https://app.example.com ;;
"get deployment")
	case "$*" in
	*kubernetes*revision*) echo 3 ;;
	*metadata.uid*) echo uid-1 ;;
	*COOKIE_SECRET*) printf '%s' "${FAKE_COOKIE:-}" ;;
	*) [ -n "${FAKE_INSTALLED:-}" ] ;;
	esac ;;
"get rs")
	# The current ReplicaSet, an old one of this Deployment, one of another owner.
	printf '%s\n' "rs-old 2 uid-1 0 0" "rs-cur 3 uid-1 1 1" "rs-foreign 1 uid-2 0 0" ;;
"get namespace" | "get project")
	# The namespace's UID (the OAuthClient's owner) once it exists.
	case "$*" in
	*metadata.uid*) printf '%s' "${FAKE_NS_UID-ns-uid-1}" ;;
	*) [ -n "${FAKE_NS:-}" ] ;;
	esac ;;
"get secret")
	# The OAuth client secret's value, as oc prints a Secret's data (base64).
	if [[ " $* " == *"{.data.client-secret}"* ]]; then
		printf '%s' "${FAKE_CLIENT_SECRET-c2VjcmV0LWZyb20tdGhlLWNsdXN0ZXI=}"
		exit 0
	fi
	[ -n "${FAKE_SECRET:-}" ] ;;
"create secret")
	for a in "$@"; do case "$a" in
		--from-env-file=*) cat "${a#--from-env-file=}" >"$FAKE_LOG.secret" ;;
		--from-file=client-secret=*) cat "${a#--from-file=client-secret=}" >"$FAKE_LOG.client-secret" ;;
	esac; done ;;
"get oauthclient")
	# FAKE_CLIENT: absent (default), ours, theirs (another install's
	# labels), unlabelled-ours (no labels, this Route's callback),
	# unlabelled-theirs, error; printed as install.sh's jsonpath asks.
	case "${FAKE_CLIENT:-absent}" in
	ours) printf '%s|%s|https://app.example.com/oauth/callback\n' "${FAKE_CLIENT_APP:-rhoai-nightly-updater}" "${FAKE_CLIENT_NS:-scratch-ns}" ;;
	theirs) printf 'rhoai-nightly|updater-scratch-ns|https://other.example.com/oauth/callback\n' ;;
	unlabelled-ours) printf '||https://app.example.com/oauth/callback\n' ;;
	unlabelled-theirs) printf '||https://old.example.com/oauth/callback\n' ;;
	error) echo "Error from server (Forbidden): oauthclients is forbidden" >&2; exit 1 ;;
	*) echo "Error from server (NotFound): oauthclients.oauth.openshift.io \"$3\" not found" >&2; exit 1 ;;
	esac ;;
"delete oauthclient")
	[ -z "${FAKE_CLIENT_DELETE_FAIL:-}" ] || { echo "error: oauthclients is forbidden" >&2; exit 1; } ;;
"apply --server-side")
	cat >>"$FAKE_LOG.oauthclient"
	[ -z "${FAKE_SSA_FAIL:-}" ] || { echo "error: oauthclients is forbidden" >&2; exit 1; } ;;
"delete oauthaccesstokens")
	[ -z "${FAKE_REVOKE_FAIL:-}" ] || { echo "error: the server doesn't have a resource type" >&2; exit 1; } ;;
"apply -f")
	[ "$3" != - ] || cat >"$FAKE_LOG.stdin" ;;
"process -f")
	if [[ " $* " == *" --parameters "* ]]; then echo "NAME DESCRIPTION GENERATOR VALUE"; echo "IMAGE"; exit 0; fi
	[ -z "${FAKE_PROCESS_FAIL:-}" ] || { echo "error: cannot process the template" >&2; exit 1; }
	# -o jsonpath: "kind namespace" of each object, rendered from the template.
	if [[ " $* " == *" jsonpath="* ]]; then
		pf=
		for a in "$@"; do case "$a" in --param-file=*) pf=${a#--param-file=} ;; esac; done
		"$(dirname "$0")/template-objects" "$3" "$pf" | awk -F'\t' '{print $1, $2}'
		exit 0
	fi
	for a in "$@"; do case "$a" in --param-file=*) cat "${a#--param-file=}" >>"$FAKE_LOG.params" ;; esac; done
	echo '{"kind":"List","items":[]}' ;;
"apply --dry-run=server")
	printf '%b' "${FAKE_APPLY_OUT:-}"; exit "${FAKE_APPLY_RC:-0}" ;;
*) exit 0 ;;
esac
FAKE
cat >"$BIN/curl" <<'FAKE'
#!/usr/bin/env bash
# Fake curl: answers a manifest HEAD with FAKE_DIGEST, or 404. With
# FAKE_REALM, an anonymous HEAD gets a Bearer challenge for that realm.
printf 'curl %s\n' "$*" >>"$FAKE_LOG"
if [[ " $* " == *" -G "* ]]; then
	echo '{"token": "anon"}'
elif [ -n "${FAKE_REALM:-}" ] && [[ " $* " != *"Authorization: Bearer"* ]]; then
	printf 'HTTP/1.1 401 Unauthorized\r\nWWW-Authenticate: Bearer realm="%s",service="quay.io"\r\n\r\n' "$FAKE_REALM"
elif [ -n "${FAKE_DIGEST:-}" ]; then
	printf 'HTTP/1.1 200 OK\r\nDocker-Content-Digest: %s\r\n\r\n' "$FAKE_DIGEST"
else
	printf 'HTTP/1.1 404 Not Found\r\n\r\n'
fi
FAKE
# template-objects TEMPLATE [PARAM_FILE]: "kind<TAB>namespace<TAB>instance
# label<TAB>name" of each object of an OpenShift Template, its ${PARAM}s replaced by the param
# file's values, else the template's defaults: what `oc process --local`
# renders, for the fake oc (checked against the real oc below, when there is one).
cat >"$BIN/template-objects" <<'FAKE'
#!/usr/bin/env bash
awk -v pf="${2:-/dev/null}" '
function rep(s, from, to,   i) { while ((i = index(s, from)) > 0) s = substr(s, 1, i - 1) to substr(s, i + length(from)); return s }
function subst(s,   k) { for (k in set) s = rep(s, "${" k "}", set[k]); for (k in def) s = rep(s, "${" k "}", def[k]); return s }
FILENAME == pf { i = index($0, "="); if (i > 1) set[substr($0, 1, i - 1)] = substr($0, i + 1); next }
/^[a-z]/ { section = $1; next }
section == "parameters:" && /^  - name: / { name = $3; next }
section == "parameters:" && /^    value: / { if (name != "") def[name] = $2; next }
section != "objects:" { next }
/^  - / { n++; meta = 0; labels = 0; sub(/^  - /, "    ") }
!n { next }
/^    kind: / { kind[n] = $2; next }
/^    metadata:/ { meta = 1; next }
/^    [^ ]/ { meta = 0 }
meta && /^      namespace: / { ns[n] = $2 }
meta && /^      name: / { nm[n] = $2 }
meta && /^      labels:/ { labels = 1; next }
meta && /^      [^ ]/ { labels = 0 }
labels && /^        app\.kubernetes\.io\/instance: / { inst[n] = $2 }
END { OFS = "\t"; for (i = 1; i <= n; i++) print kind[i], subst(ns[i]), subst(inst[i]), subst(nm[i]) }
' "${2:-/dev/null}" "$1"
FAKE
chmod +x "$BIN/oc" "$BIN/curl" "$BIN/template-objects"
DIGEST="sha256:$(printf 'a%.0s' $(seq 64))"

# run ARGS...: the given command with the fakes, a fresh log, and the
# defaults of a cluster without the app.
run() {
	rm -f "$LOG" "$LOG.params" "$LOG.oauthclient" "$LOG.client-secret"
	env PATH="$BIN:$PATH" FAKE_LOG="$LOG" FAKE_DIGEST="${FAKE_DIGEST-$DIGEST}" FAKE_REVISION="${FAKE_REVISION-$COMMIT}" FAKE_OCP_VERSION="${FAKE_OCP_VERSION-4.19.3}" "$@"
}
mutations() { grep -E '^oc (create|new-project|delete|rollout|apply -f|apply --server-side)' "$LOG" | grep -v -- '--dry-run=server' || true; }

# Fresh install, dry run: the namespace does not exist, so the server
# rejects only the objects in it.
FAKE_APPLY_OUT='configmap/x created (server dry run)\nError from server (NotFound): error when creating "x": namespaces "scratch-ns" not found\n' FAKE_APPLY_RC=1 \
	expect_ok "asset: fresh install dry run" run bash "$ASSET" --dry-run --namespace scratch-ns
expect_eq "dry run changes nothing" "" "$(mutations)"
grep -qx "IMAGE=quay.io/example/app@$DIGEST" "$LOG.params" && pass || fail "dry run: image not pinned by digest: $(cat "$LOG.params")"
grep -qx "OAUTH_PROXY_IMAGE=registry.redhat.io/openshift4/ose-oauth-proxy-rhel9:v4.19" "$LOG.params" && pass || fail "dry run: oauth-proxy image not from the cluster version"
grep -qx "IMAGE_REPOSITORY=quay.io/example/app" "$LOG.params" && pass || fail "dry run: IMAGE_REPOSITORY missing"

# An OpenShift 5 cluster must pull the proxy from the openshift5 namespace, not openshift4:
# the openshift4 repository has no 5.x tag, so there it would never pull (ImagePullBackOff).
FAKE_OCP_VERSION=5.0.1 expect_ok "asset: dry run on a 5.x cluster" run bash "$ASSET" --dry-run --namespace scratch-ns
grep -qx "OAUTH_PROXY_IMAGE=registry.redhat.io/openshift5/ose-oauth-proxy-rhel9:v5.0" "$LOG.params" && pass || fail "dry run: oauth-proxy image not openshift5:v5.0 on a 5.x cluster ($(cat "$LOG.params" 2>/dev/null))"
grep -qx "NAMESPACE=scratch-ns" "$LOG.params" && pass || fail "dry run: namespace flag ignored"
grep -q "manifests/v9.8.7" "$LOG" && pass || fail "the asset did not resolve its own version"

FAKE_APPLY_OUT='Error from server (Forbidden): clusterroles is forbidden\n' FAKE_APPLY_RC=1 \
	expect_fail "a real dry-run error fails" "forbidden" run bash "$ASSET" --dry-run --namespace scratch-ns
FAKE_REVISION=1111111111111111111111111111111111111111 \
	expect_fail "asset: image from another commit" "not $COMMIT, the commit of v9.8.7" run bash "$ASSET" --dry-run
FAKE_REVISION=1111111111111111111111111111111111111111 ALLOW_TEMPLATE_MISMATCH=1 \
	expect_ok "asset: ALLOW_TEMPLATE_MISMATCH" run bash "$ASSET" --dry-run
FAKE_ADMIN=no expect_fail "not cluster-admin" "is not cluster-admin" run bash "$ASSET" --dry-run
FAKE_DIGEST="" FAKE_REVISION="" expect_fail "no such tag" "cannot resolve quay.io/example/app:v9.8.7" run bash "$ASSET" --dry-run
expect_fail "upgrade needs an install" "No deployment rhoai-nightly-updater" run bash "$ASSET" upgrade --dry-run
FAKE_INSTALLED=1 FAKE_NS=1 expect_ok "asset: upgrade dry run" run bash "$ASSET" --dry-run
expect_eq "upgrade dry run changes nothing" "" "$(mutations)"

# --- The clone path (make deploy / upgrade / rollback call this) ---------------
CLONE="$REPO/scripts/install.sh"
FAKE_INSTALLED=1 FAKE_NS=1 expect_ok "clone: upgrade dry run on the release tag" run bash "$CLONE" upgrade --dry-run
grep -qx "IMAGE=quay.io/juntao_wang/rhoai-nightly-updater@$DIGEST" "$LOG.params" && pass || fail "clone: default image repository"
FAKE_INSTALLED=1 FAKE_NS=1 IMAGE=quay.io/fork/app expect_ok "clone: IMAGE from the environment" run bash "$CLONE" upgrade --dry-run
grep -qx "IMAGE_REPOSITORY=quay.io/fork/app" "$LOG.params" && pass || fail "clone: IMAGE env ignored"
expect_fail "clone: IMAGE with a tag" "without a tag" run env IMAGE=quay.io/fork/app:latest bash "$CLONE" upgrade --dry-run

# The Makefile wraps the same script; TAG defaults to the release tag.
if command -v make >/dev/null 2>&1; then
	grep -q 'TAG        = v9.8.7' <<<"$(make -s -C "$REPO" help)" && pass || fail "make: TAG does not default to the release tag"
	FAKE_INSTALLED=1 FAKE_NS=1 expect_ok "make upgrade DRY_RUN=1" run make -s -C "$REPO" upgrade DRY_RUN=1
	grep -qx "IMAGE=quay.io/juntao_wang/rhoai-nightly-updater@$DIGEST" "$LOG.params" && pass || fail "make upgrade: not the release digest"
	FAKE_INSTALLED=1 expect_fail "make rollback needs an explicit TAG" "Usage: make rollback" run make -s -C "$REPO" rollback DRY_RUN=1
	FAKE_INSTALLED=1 FAKE_NS=1 expect_ok "make rollback TAG=v9.8.7" run make -s -C "$REPO" rollback TAG=v9.8.7 DRY_RUN=1
fi

echo "# edit" >>"$REPO/deploy/template.yaml"
FAKE_INSTALLED=1 expect_fail "clone: uncommitted template" "uncommitted changes" run bash "$CLONE" upgrade --dry-run
git -C "$REPO" checkout -q deploy/template.yaml

git -C "$REPO" commit -q --allow-empty -m after-release
FAKE_INSTALLED=1 expect_fail "clone: HEAD not on a release" "git checkout vX.Y.Z), or pass TAG=main / TAG=<commit>" run bash "$CLONE" upgrade --dry-run
FAKE_INSTALLED=1 FAKE_NS=1 TAG=main expect_ok "clone: TAG=main uses the image's label" run bash "$CLONE" upgrade --dry-run
FAKE_INSTALLED=1 FAKE_NS=1 TAG=latest expect_ok "clone: TAG=latest still works" run bash "$CLONE" upgrade --dry-run
FAKE_INSTALLED=1 FAKE_NS=1 TAG=latest expect_fail "clone: TAG=latest warns" "WARNING: :latest" run sh -c "bash '$CLONE' upgrade --dry-run 2>&1; exit 1"
FAKE_INSTALLED=1 FAKE_REVISION="$(git -C "$REPO" rev-parse HEAD)" TAG=v9.8.7 \
	expect_fail "clone: release tag image from another commit" "not commit $COMMIT that its tag names" run bash "$CLONE" upgrade --dry-run
FAKE_INSTALLED=1 FAKE_REVISION="" FAKE_NS=1 TAG=main \
	expect_fail "clone: no revision label on a moving tag" "cannot tell which commit" run bash "$CLONE" upgrade --dry-run
FAKE_INSTALLED=1 expect_fail "clone: rollback needs a TAG" "Usage: make rollback TAG=" run bash "$CLONE" rollback --dry-run
FAKE_INSTALLED=1 FAKE_NS=1 TAG=v9.8.7 expect_ok "clone: rollback dry run" run bash "$CLONE" rollback --dry-run
grep -qx "IMAGE=quay.io/juntao_wang/rhoai-nightly-updater@$DIGEST" "$LOG.params" && pass || fail "rollback: not applied by digest: $(grep '^IMAGE=' "$LOG.params")"
FAKE_INSTALLED=1 FAKE_NS=1 TAG=v9.8.7 FAKE_DIGEST="" FAKE_NO_OC_DIGEST=1 \
	expect_fail "rollback: no digest, fail closed" "cannot resolve quay.io/juntao_wang/rhoai-nightly-updater:v9.8.7" run bash "$CLONE" rollback --dry-run
[ ! -s "$LOG.params" ] && pass || fail "rollback without a digest applied something"
FAKE_INSTALLED=1 FAKE_NS=1 TAG=v9.8.7 FAKE_DIGEST="" FAKE_NO_OC_DIGEST=1 ALLOW_MUTABLE_TAG=1 \
	expect_ok "rollback: ALLOW_MUTABLE_TAG applies the tag" run bash "$CLONE" rollback --dry-run
grep -qx "IMAGE=quay.io/juntao_wang/rhoai-nightly-updater:v9.8.7" "$LOG.params" && pass || fail "rollback: ALLOW_MUTABLE_TAG"
expect_eq "rollback dry run changes nothing" "" "$(mutations)"
FAKE_INSTALLED=1 FAKE_NS=1 TAG=v9.8.7 FAKE_CLIENT=theirs \
	expect_fail "rollback to a template with the OAuthClient: another install's client" "is not this install's" run bash "$CLONE" rollback
expect_eq "rollback, another install's OAuthClient: nothing changed" "" "$(mutations)"
# Rolling back to a template from before the OAuthClient (3.x signs in
# through the ServiceAccount): after the rollout this install's client is
# deleted and its sign-ins revoked; another install's is kept.
# shellcheck disable=SC2016 # the template's own ${PARAM} placeholders
sed '/--client-id=\${APP_NAME}-\${NAMESPACE}$/d' "$SRC/deploy/template.yaml" >"$REPO/deploy/template.yaml"
git -C "$REPO" commit -qam three-x
OLD=$(git -C "$REPO" rev-parse HEAD)
git -C "$REPO" reset -q --hard HEAD~1
FAKE_INSTALLED=1 FAKE_NS=1 FAKE_SECRET=1 FAKE_REVISION=$OLD TAG=$OLD FAKE_CLIENT=ours FAKE_CLIENT_NS=rhoai-nightly-updater expect_ok "rollback to 3.x" run bash "$CLONE" rollback
expect_eq "rollback to 3.x: the client goes after the rollout" "$(printf '%s\n' \
	"oc rollout status deployment/rhoai-nightly-updater -n rhoai-nightly-updater --timeout=20m" \
	"oc delete oauthclient rhoai-nightly-updater-rhoai-nightly-updater --ignore-not-found" \
	"oc delete oauthaccesstokens --field-selector clientName=rhoai-nightly-updater-rhoai-nightly-updater")" \
	"$(grep -E '^oc (rollout|delete|apply --server-side)' "$LOG" || true)"
FAKE_INSTALLED=1 FAKE_NS=1 FAKE_SECRET=1 FAKE_REVISION=$OLD TAG=$OLD FAKE_CLIENT=theirs expect_ok "rollback to 3.x next to another install's client" run bash "$CLONE" rollback
expect_eq "rollback to 3.x: another install's client kept" "" "$(grep '^oc delete oauth' "$LOG" || true)"
FAKE_INSTALLED=1 FAKE_NS=1 FAKE_SECRET=1 FAKE_REVISION=$OLD TAG=$OLD FAKE_CLIENT=ours FAKE_CLIENT_NS=rhoai-nightly-updater FAKE_CLIENT_DELETE_FAIL=1 \
	expect_fail "rollback to 3.x: the client cannot be deleted" "oc delete oauthclient rhoai-nightly-updater-rhoai-nightly-updater" run bash "$CLONE" rollback
FAKE_INSTALLED=1 FAKE_NS=1 FAKE_SECRET=1 FAKE_REVISION=$OLD TAG=$OLD FAKE_CLIENT=ours FAKE_CLIENT_NS=rhoai-nightly-updater expect_ok "rollback to 3.x, dry run" run bash "$CLONE" rollback --dry-run
expect_eq "rollback to 3.x, dry run: changes nothing" "" "$(mutations)"
expect_ok "asset: uninstall dry run" run bash "$ASSET" uninstall --dry-run --namespace scratch-ns
deletes=$(grep -c '^oc delete' "$LOG" || true)
expect_eq "uninstall: every delete is a dry run" "$deletes" "$(grep '^oc delete' "$LOG" | grep -c -- '--dry-run=server' || true)"
grep -q '^oc delete project scratch-ns' "$LOG" && pass || fail "uninstall: namespace not removed"
# The registry's token service must be on the registry host.
FAKE_REALM=https://quay.io/v2/auth expect_ok "token from the registry host" run bash "$ASSET" resolve-image
grep -q -- "-G https://quay.io/v2/auth" "$LOG" && pass || fail "no token request to the registry host"
grep -q "Authorization: Bearer anon" "$LOG" && pass || fail "the token was not used"
FAKE_REALM=https://evil.example.com/token expect_ok "token realm elsewhere: falls back to oc" run bash "$ASSET" resolve-image
grep -q "evil.example.com" <<<"$(grep -- '-G' "$LOG" || true)" && fail "a token was requested from another host" || pass
FAKE_REALM=https://evil.example.com/token expect_fail "token realm elsewhere: warning" "ignoring the token service" \
	run sh -c "bash '$ASSET' resolve-image 2>&1; exit 1"
FAKE_REALM=http://quay.io/v2/auth expect_fail "plain-http token realm: warning" "ignoring the token service" \
	run sh -c "bash '$ASSET' resolve-image 2>&1; exit 1"

# --- The mutating paths (no dry run), against the fake oc ----------------------
calls() { grep -E "^oc ($1)" "$LOG" | sed 's/ --from-env-file=.*//; s/ --from-file=.*//' || true; }

expect_ok "fresh install" run bash "$ASSET" --namespace scratch-ns
SSA='oc apply --server-side --field-manager=rhoai-nightly-updater-install --force-conflicts -f -'
expect_eq "fresh install: the steps in order" "$(printf '%s\n' \
	'oc new-project scratch-ns' \
	'oc create secret generic rhoai-nightly-updater-proxy -n scratch-ns' \
	'oc create secret generic rhoai-nightly-updater-oauth-client -n scratch-ns' \
	'oc apply -f' \
	"$SSA" \
	'oc rollout status deployment/rhoai-nightly-updater -n scratch-ns --timeout=20m' \
	'oc apply -f -' \
	'oc delete rs -n scratch-ns rs-old')" \
	"$(calls 'new-project|create|apply|rollout|delete' | sed 's|^oc apply -f /.*|oc apply -f|')"
grep -Eqx 'session_secret=[a-zA-Z0-9]{32}' "$LOG.secret" && pass || fail "fresh install: no 32-character cookie secret"
grep -q 'href: https://app.example.com' "$LOG.stdin" && pass || fail "fresh install: ConsoleLink without the Route host"
grep -q 'name: rhoai-nightly-updater-scratch-ns' "$LOG.stdin" && pass || fail "fresh install: ConsoleLink name"
grep -qx "IMAGE=quay.io/example/app@$DIGEST" "$LOG.params" && pass || fail "fresh install: not by digest"
# The OAuth client secret: 32 alphanumerics with no trailing newline
# (oauth-proxy reads the file raw), never on a command line.
[[ $(cat "$LOG.client-secret") =~ ^[a-zA-Z0-9]{32}$ ]] && [ "$(wc -c <"$LOG.client-secret" | tr -d ' ')" = 32 ] && pass ||
	fail "fresh install: the OAuth client secret is not 32 alphanumerics without a newline"
# The OAuthClient: the instance's name and label, auto grant, the Route's
# callback, the Secret's value (from the cluster, decoded), on stdin only.
expect_eq "fresh install: the OAuthClient" "$(printf '%s\n' \
	'apiVersion: oauth.openshift.io/v1' \
	'kind: OAuthClient' \
	'metadata:' \
	'  name: rhoai-nightly-updater-scratch-ns' \
	'  labels:' \
	'    app.kubernetes.io/instance: rhoai-nightly-updater-scratch-ns' \
	'    app.kubernetes.io/name: "rhoai-nightly-updater"' \
	'    rhoai-nightly-updater/namespace: "scratch-ns"' \
	'  ownerReferences:' \
	'    - apiVersion: v1' \
	'      kind: Namespace' \
	'      name: "scratch-ns"' \
	'      uid: "ns-uid-1"' \
	'grantMethod: auto' \
	'redirectURIs:' \
	'  - https://app.example.com/oauth/callback' \
	'scopeRestrictions:' \
	'  - literals:' \
	'      - user:full' \
	'secret: secret-from-the-cluster')" "$(cat "$LOG.oauthclient")"
grep -q "secret-from-the-cluster\|c2VjcmV0LWZyb20tdGhlLWNsdXN0ZXI=" "$LOG" && fail "the OAuth client secret is on an oc command line" || pass
grep -q "accessTokenMaxAgeSeconds" "$LOG.oauthclient" && fail "the OAuthClient shortens the sign-in" || pass
# Without the namespace's UID there is no owner: nothing is applied.
FAKE_INSTALLED=1 FAKE_NS=1 FAKE_SECRET=1 FAKE_NS_UID="" expect_fail "upgrade: no namespace UID" "Cannot read the UID of namespace scratch-ns" run bash "$ASSET" --namespace scratch-ns
[ ! -s "$LOG.oauthclient" ] && pass || fail "upgrade without the namespace UID applied the OAuthClient"

# An upgrade from a release whose ServiceAccount was the OAuth client: the
# installed Secrets are kept, the OAuthClient is created, the new template
# rolls the Deployment.
FAKE_INSTALLED=1 FAKE_NS=1 FAKE_SECRET=1 expect_ok "upgrade from 3.x" run bash "$ASSET" --namespace scratch-ns
expect_eq "upgrade from 3.x: no Secret created, the OAuthClient before the rollout" "$(printf '%s\n' \
	'oc apply -f' \
	"$SSA" \
	'oc rollout status deployment/rhoai-nightly-updater -n scratch-ns --timeout=20m' \
	'oc apply -f -')" \
	"$(calls 'create|apply|rollout' | sed 's|^oc apply -f /.*|oc apply -f|')"
grep -q 'secret: secret-from-the-cluster' "$LOG.oauthclient" && pass || fail "upgrade: the OAuthClient does not carry the kept secret"
# A Secret without the key fails the install before anything signs in with it.
FAKE_INSTALLED=1 FAKE_NS=1 FAKE_SECRET=1 FAKE_CLIENT_SECRET="" \
	expect_fail "upgrade: an empty client secret stops" "has no client-secret" run bash "$ASSET" --namespace scratch-ns
FAKE_INSTALLED=1 FAKE_NS=1 FAKE_SECRET=1 FAKE_SSA_FAIL=1 \
	expect_fail "upgrade: the OAuthClient cannot be applied" "forbidden" run bash "$ASSET" --namespace scratch-ns
expect_eq "upgrade: no rollout without the OAuthClient" "" "$(calls rollout)"
# A dry run validates the OAuthClient with a placeholder secret only.
FAKE_INSTALLED=1 FAKE_NS=1 expect_ok "upgrade dry run validates the OAuthClient" run bash "$ASSET" --dry-run --namespace scratch-ns
grep -q -- '--force-conflicts --dry-run=server -f -' "$LOG" && pass || fail "dry run: the OAuthClient is not validated with the server"
grep -q 'secret: dry-run-placeholder' "$LOG.oauthclient" && pass || fail "dry run: the OAuthClient carries a real secret"

# The OAuthClient's name, <APP_NAME>-<NAMESPACE>, is the same for app a-b in
# namespace c and app a in namespace b-c. A client that is not this
# install's (other labels, or without labels a redirect to another host) or
# that cannot be read stops deploy, upgrade and their dry runs before
# anything changes; this install's own (labels, or an unlabelled one on
# this Route) is applied over.
for client in ours unlabelled-ours; do
	FAKE_INSTALLED=1 FAKE_NS=1 FAKE_SECRET=1 FAKE_CLIENT=$client expect_ok "upgrade over our OAuthClient ($client)" run bash "$ASSET" --namespace scratch-ns
	grep -q 'rhoai-nightly-updater/namespace: "scratch-ns"' "$LOG.oauthclient" && pass || fail "upgrade over $client: not applied with the labels"
done
FAKE_INSTALLED=1 FAKE_NS=1 FAKE_SECRET=1 FAKE_CLIENT=theirs \
	expect_fail "upgrade: another install's OAuthClient" "belongs to app rhoai-nightly in namespace updater-scratch-ns" run bash "$ASSET" --namespace scratch-ns
expect_eq "another install's OAuthClient: nothing changed" "" "$(mutations)"
for client in unlabelled-theirs error; do
	for dry in "" --dry-run; do
		FAKE_INSTALLED=1 FAKE_NS=1 FAKE_SECRET=1 FAKE_CLIENT=$client \
			expect_fail "upgrade $dry: OAuthClient $client" "OAuthClient rhoai-nightly-updater-scratch-ns" run bash "$ASSET" --namespace scratch-ns $dry
		expect_eq "upgrade $dry, OAuthClient $client: nothing changed" "" "$(mutations)"
	done
done
FAKE_CLIENT=unlabelled-theirs expect_fail "fresh install: a leftover OAuthClient" "oc delete oauthaccesstokens --field-selector clientName=rhoai-nightly-updater-scratch-ns" \
	run bash "$ASSET" --namespace scratch-ns

LEGACY_COOKIE=abcdefghijklmnopqrstuvwxyz012345
FAKE_INSTALLED=1 FAKE_NS=1 FAKE_COOKIE=$LEGACY_COOKIE FAKE_LEGACY=scratch-ns/rhoai-nightly-updater \
	expect_ok "upgrade of a legacy install" run bash "$ASSET" --namespace scratch-ns
expect_eq "legacy upgrade: the cookie secret is carried over" "session_secret=$LEGACY_COOKIE" "$(cat "$LOG.secret")"
expect_eq "legacy upgrade: no new project" "" "$(calls new-project)"
expect_eq "legacy upgrade: legacy objects and the old ReplicaSet removed" "$(printf '%s\n' \
	'oc delete clusterrolebinding rhoai-nightly-updater' \
	'oc delete clusterrole rhoai-nightly-updater --ignore-not-found' \
	'oc delete consolelink rhoai-nightly-updater' \
	'oc delete rs -n scratch-ns rs-old')" "$(calls delete)"

rm -f "$LOG.secret"
FAKE_INSTALLED=1 FAKE_NS=1 FAKE_SECRET=1 expect_ok "upgrade with the Secret in place" run bash "$ASSET" --namespace scratch-ns
expect_eq "upgrade: the Secret is kept" "" "$(calls create)"
[ ! -e "$LOG.secret" ] && pass || fail "upgrade: a Secret was written"

# Legacy objects of this install: a dry run must not delete them.
FAKE_LEGACY=scratch-ns/rhoai-nightly-updater expect_ok "cleanup-legacy dry run" run bash "$ASSET" cleanup-legacy --dry-run --namespace scratch-ns
expect_eq "cleanup-legacy dry run: three deletes, all server dry runs" "3 3" \
	"$(grep -c '^oc delete' "$LOG") $(grep '^oc delete' "$LOG" | grep -c -- '--dry-run=server')"
FAKE_LEGACY=scratch-ns/rhoai-nightly-updater expect_ok "uninstall dry run with legacy objects" run bash "$ASSET" uninstall --dry-run --namespace scratch-ns
expect_eq "uninstall dry run: no real delete" "" "$(grep '^oc delete' "$LOG" | grep -v -- '--dry-run=server' || true)"
FAKE_LEGACY=scratch-ns/rhoai-nightly-updater expect_ok "cleanup-legacy" run bash "$ASSET" cleanup-legacy --namespace scratch-ns
expect_eq "cleanup-legacy deletes for real" "0" "$(grep '^oc delete' "$LOG" | grep -c -- '--dry-run' || true)"
FAKE_LEGACY=other-ns/rhoai-nightly-updater expect_ok "cleanup-legacy of another install" run bash "$ASSET" cleanup-legacy --namespace scratch-ns
expect_eq "another install's binding is kept" "" "$(grep '^oc delete clusterrolebinding' "$LOG" || true)"
expect_fail "asset: no rollback" "Rollback needs a clone" run bash "$ASSET" rollback --version v9.8.7

# --- Uninstall removes every Role and RoleBinding the template creates ---------
# Template revision 5 added a Role in openshift-ingress-operator that a fixed
# list of namespaces missed. The namespaces now come from the template.
INST=rhoai-nightly-updater-scratch-ns
printf '%s\n' NAMESPACE=scratch-ns IMAGE=x >"$WORK/render.params"
objects=$("$BIN/template-objects" "$SRC/deploy/template.yaml" "$WORK/render.params")
# The fake renders the template as oc does (only where a real oc is installed).
if REAL_OC=$(command -v oc); then
	expect_eq "the fake oc renders the template like oc process --local" \
		"$(KUBECONFIG=/dev/null "$REAL_OC" process -f "$SRC/deploy/template.yaml" --param-file="$WORK/render.params" --local \
			-o jsonpath='{range .items[*]}{.kind} {.metadata.namespace}{"\n"}{end}' | sort)" \
		"$(awk -F'\t' '{print $1, $2}' <<<"$objects" | sort)"
fi
rbac=$(awk -F'\t' '($1 == "Role" || $1 == "RoleBinding") && $2 != "scratch-ns"' <<<"$objects")
rbac_ns=$(cut -f2 <<<"$rbac" | sort -u)
grep -q "^Role	openshift-ingress-operator	" <<<"$rbac" && pass || fail "the template has no Role in openshift-ingress-operator: $rbac"
# Uninstall deletes them by the instance label, kind by kind it knows: any
# other kind outside the app's namespace would be left behind.
expect_eq "every Role and RoleBinding outside the app's namespace carries the instance label" "" \
	"$(awk -F'\t' -v i="$INST" '$3 != i' <<<"$rbac")"
expect_eq "outside the app's namespace the template creates only Roles, RoleBindings and cluster-scoped RBAC" "" \
	"$(awk -F'\t' '$2 != "scratch-ns" && !($1 ~ /^(Role|RoleBinding)$/ && $2 != "") && !($1 ~ /^Cluster(Role|RoleBinding)$/ && $2 == "")' <<<"$objects")"
FAKE_CLIENT=ours expect_ok "uninstall" run bash "$ASSET" uninstall --namespace scratch-ns
for ns in $rbac_ns; do
	grep -qx "oc delete rolebinding,role -n $ns -l app.kubernetes.io/instance=$INST --ignore-not-found" "$LOG" && pass ||
		fail "uninstall: the template's Roles in $ns are not deleted"
done
expect_eq "uninstall: no Role deletes in the app's namespace (deleted with it)" "" "$(grep '^oc delete rolebinding,role -n scratch-ns ' "$LOG" || true)"
expect_eq "uninstall: the deletes in order" "$(
	echo "oc delete oauthclient $INST --ignore-not-found"
	echo "oc delete oauthaccesstokens --field-selector clientName=$INST"
	echo "oc delete consolelink,clusterrolebinding,clusterrole -l app.kubernetes.io/instance=$INST --ignore-not-found"
	for ns in $rbac_ns; do
		echo "oc delete rolebinding,role -n $ns -l app.kubernetes.io/instance=$INST --ignore-not-found"
	done
	echo "oc delete project scratch-ns --ignore-not-found"
)" "$(calls delete)"
# Tokens that cannot be revoked: the rest is still removed, and the command
# fails with the revocation to run by hand.
FAKE_CLIENT=ours FAKE_REVOKE_FAIL=1 expect_fail "uninstall: the sign-ins cannot be revoked" "oc delete oauthaccesstokens --field-selector clientName=$INST" \
	run bash "$ASSET" uninstall --namespace scratch-ns
grep -q '^oc delete project scratch-ns' "$LOG" && pass || fail "uninstall: a failed revocation stopped the removal"
# An OAuthClient that cannot be deleted keeps everything else, the namespace
# above all: its Route holds the host the client redirects to, which nobody
# else can claim while it exists. An unreadable one deletes nothing.
FAKE_CLIENT=ours FAKE_CLIENT_DELETE_FAIL=1 expect_fail "uninstall: the OAuthClient cannot be deleted" "nothing else was removed" \
	run bash "$ASSET" uninstall --namespace scratch-ns
expect_eq "uninstall, OAuthClient not deleted: no other delete" "oc delete oauthclient $INST --ignore-not-found" "$(calls delete)"
FAKE_CLIENT=error expect_fail "uninstall: the OAuthClient cannot be read" "Cannot read OAuthClient $INST" run bash "$ASSET" uninstall --namespace scratch-ns
expect_eq "uninstall, OAuthClient unreadable: no delete" "" "$(calls delete)"
# Another install's client (same name) and its sign-ins stay; the rest goes.
FAKE_CLIENT=theirs expect_ok "uninstall next to another install's OAuthClient" run bash "$ASSET" uninstall --namespace scratch-ns
expect_eq "uninstall: another install's OAuthClient and sign-ins kept" "" "$(grep -E '^oc delete (oauthclient|oauthaccesstokens)' "$LOG" || true)"
grep -q '^oc delete project scratch-ns' "$LOG" && pass || fail "uninstall next to another install's client: the namespace stayed"
# No client (removed before): its name's leftover sign-ins are revoked.
expect_ok "uninstall without an OAuthClient" run bash "$ASSET" uninstall --namespace scratch-ns
expect_eq "uninstall without an OAuthClient: tokens revoked, no client delete" "oc delete oauthaccesstokens --field-selector clientName=$INST" \
	"$(grep -E '^oc delete (oauthclient|oauthaccesstokens)' "$LOG")"
# A namespace the template gains later is covered without touching install.sh.
# shellcheck disable=SC2016 # the template's own ${PARAM} placeholders
printf '%s\n' '  - apiVersion: rbac.authorization.k8s.io/v1' '    kind: Role' '    metadata:' \
	'      name: ${APP_NAME}-${NAMESPACE}-later' '      namespace: ${NAMESPACE}-later' '      labels:' \
	'        app.kubernetes.io/instance: ${APP_NAME}-${NAMESPACE}' '    rules: []' >>"$REPO/deploy/template.yaml"
expect_ok "clone: uninstall with a Role in a new namespace" run bash "$CLONE" uninstall --namespace scratch-ns
grep -qx "oc delete rolebinding,role -n scratch-ns-later -l app.kubernetes.io/instance=$INST --ignore-not-found" "$LOG" && pass ||
	fail "uninstall: a Role in a namespace the template gained is not deleted"
git -C "$REPO" checkout -q deploy/template.yaml
# Without the namespaces it would leave Roles behind: nothing is deleted.
# UPGRADING.md's command for 2.3.x, which leaves them: the template's names
# with its default parameters.
name=$("$BIN/template-objects" "$SRC/deploy/template.yaml" | awk -F'\t' '$1 == "Role" && $2 == "openshift-ingress-operator" {print $4}')
grep -qF "oc delete role,rolebinding $name -n openshift-ingress-operator" "$SRC/docs/UPGRADING.md" && pass ||
	fail "docs/UPGRADING.md has no 'oc delete role,rolebinding $name -n openshift-ingress-operator'"
FAKE_PROCESS_FAIL=1 expect_fail "uninstall: the template cannot be rendered" "nothing was deleted" run bash "$ASSET" uninstall --namespace scratch-ns
expect_eq "uninstall: no delete when the template cannot be rendered" "" "$(calls delete)"

printf '%d passed, %d failed\n' "$PASSED" "$FAILED"
[ "$FAILED" -eq 0 ]
