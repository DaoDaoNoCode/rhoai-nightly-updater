#!/usr/bin/env bash
# Tests for scripts/install.sh and the release asset that
# `scripts/release.sh installer` builds from it. Offline: fake oc and curl
# commands record what the installer would do.
#   bash scripts/test-install.sh
set -euo pipefail

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
"version -o") echo '{"openshiftVersion": "4.19.3"}' ;;
"image info")
	[ -n "${FAKE_REVISION:-}" ] || exit 1
	printf '{\n  "digest": "sha256:%s",\n  "config": {"config": {"Labels": {\n    "org.opencontainers.image.revision": "%s"\n  }}}\n}\n' \
		"$(printf 'b%.0s' $(seq 64))" "$FAKE_REVISION" ;;
"get clusterrolebinding") [ -z "${FAKE_LEGACY:-}" ] || echo "$FAKE_LEGACY" ;;
"get route") echo app.example.com ;;
"get consolelink") [ -z "${FAKE_LEGACY:-}" ] || echo https://app.example.com ;;
"get deployment") [ -n "${FAKE_INSTALLED:-}" ] ;;
"get namespace" | "get project") [ -n "${FAKE_NS:-}" ] ;;
"get secret") [ -n "${FAKE_NS:-}" ] ;;
"process -f")
	if [[ " $* " == *" --parameters "* ]]; then echo "NAME DESCRIPTION GENERATOR VALUE"; echo "IMAGE"; exit 0; fi
	for a in "$@"; do case "$a" in --param-file=*) cat "${a#--param-file=}" >>"$FAKE_LOG.params" ;; esac; done
	echo '{"kind":"List","items":[]}' ;;
"apply --dry-run=server")
	printf '%b' "${FAKE_APPLY_OUT:-}"; exit "${FAKE_APPLY_RC:-0}" ;;
*) exit 0 ;;
esac
FAKE
cat >"$BIN/curl" <<'FAKE'
#!/usr/bin/env bash
# Fake curl: answers a manifest HEAD with FAKE_DIGEST, or 404.
printf 'curl %s\n' "$*" >>"$FAKE_LOG"
if [ -n "${FAKE_DIGEST:-}" ]; then
	printf 'HTTP/1.1 200 OK\r\nDocker-Content-Digest: %s\r\n\r\n' "$FAKE_DIGEST"
else
	printf 'HTTP/1.1 404 Not Found\r\n\r\n'
fi
FAKE
chmod +x "$BIN/oc" "$BIN/curl"
DIGEST="sha256:$(printf 'a%.0s' $(seq 64))"

# run ARGS...: the given command with the fakes, a fresh log, and the
# defaults of a cluster without the app.
run() {
	rm -f "$LOG" "$LOG.params"
	env PATH="$BIN:$PATH" FAKE_LOG="$LOG" FAKE_DIGEST="${FAKE_DIGEST-$DIGEST}" FAKE_REVISION="${FAKE_REVISION-$COMMIT}" "$@"
}
mutations() { grep -E '^oc (create|new-project|delete|rollout|apply -f)' "$LOG" || true; }

# Fresh install, dry run: the namespace does not exist, so the server
# rejects only the objects in it.
FAKE_APPLY_OUT='configmap/x created (server dry run)\nError from server (NotFound): error when creating "x": namespaces "scratch-ns" not found\n' FAKE_APPLY_RC=1 \
	expect_ok "asset: fresh install dry run" run bash "$ASSET" --dry-run --namespace scratch-ns
expect_eq "dry run changes nothing" "" "$(mutations)"
grep -qx "IMAGE=quay.io/example/app@$DIGEST" "$LOG.params" && pass || fail "dry run: image not pinned by digest: $(cat "$LOG.params")"
grep -qx "OAUTH_PROXY_IMAGE=registry.redhat.io/openshift4/ose-oauth-proxy-rhel9:v4.19" "$LOG.params" && pass || fail "dry run: oauth-proxy image not from the cluster version"
grep -qx "IMAGE_REPOSITORY=quay.io/example/app" "$LOG.params" && pass || fail "dry run: IMAGE_REPOSITORY missing"
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
expect_eq "rollback dry run changes nothing" "" "$(mutations)"
expect_ok "asset: uninstall dry run" run bash "$ASSET" uninstall --dry-run --namespace scratch-ns
deletes=$(grep -c '^oc delete' "$LOG" || true)
expect_eq "uninstall: every delete is a dry run" "$deletes" "$(grep '^oc delete' "$LOG" | grep -c -- '--dry-run=server' || true)"
grep -q '^oc delete project scratch-ns' "$LOG" && pass || fail "uninstall: namespace not removed"
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

printf '%d passed, %d failed\n' "$PASSED" "$FAILED"
[ "$FAILED" -eq 0 ]
