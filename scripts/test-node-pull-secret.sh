#!/usr/bin/env bash
# shellcheck disable=SC2015 # "check && pass || fail": pass never fails
# Runs the command the setup card shows for a self-managed cluster
# (pkg/cluster/node_pull_secret.sh: add the quay.io/rhoai entry to
# openshift-config/pull-secret) against a fake oc, with the real jq, in bash
# and, when installed, zsh. Offline.
#   bash scripts/test-node-pull-secret.sh
# Needs jq 1.6 or later (as the command does); without it the test is
# skipped, unless REQUIRE_JQ=1.
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
SRC=$(cd "$HERE/.." && pwd)
CMD_FILE="$SRC/pkg/cluster/node_pull_secret.sh"

if ! command -v jq >/dev/null 2>&1 || ! jq -n '"eA==" | @base64d' >/dev/null 2>&1; then
	if [ "${REQUIRE_JQ:-}" = 1 ]; then
		echo "FAIL: jq 1.6 or later is required" >&2
		exit 1
	fi
	echo "SKIP: test-node-pull-secret.sh needs jq 1.6 or later"
	exit 0
fi

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
FAILED=0
PASSED=0
fail() {
	printf 'FAIL: %s\n' "$*" >&2
	FAILED=$((FAILED + 1))
}
pass() { PASSED=$((PASSED + 1)); }
expect_eq() { if [ "$2" = "$3" ]; then pass; else fail "$1: want '$2', got '$3'"; fi; }

# --- Fake oc and a jq wrapper that records its arguments ---------------------
# oc get prints $WORK/secret.json (exit 1 when it is absent); oc replace
# stores its input in $WORK/replaced.json, refusing an empty input as oc
# does ("no objects passed to replace") and a stale resourceVersion with a
# conflict.
BIN="$WORK/bin"
mkdir -p "$BIN"
cat >"$BIN/oc" <<'EOF'
#!/usr/bin/env bash
echo "$*" >>"$WORK/oc.log"
case "$1" in
get)
	[ -f "$WORK/secret.json" ] || { echo 'Error from server (NotFound): secrets "pull-secret" not found' >&2; exit 1; }
	cat "$WORK/secret.json"
	;;
replace)
	input=$(cat)
	if [ -z "$input" ]; then echo "error: no objects passed to replace" >&2; exit 1; fi
	rv=$(jq -r .metadata.resourceVersion <<<"$input")
	if [ "$rv" != "$(cat "$WORK/live-rv")" ]; then
		echo 'Error from server (Conflict): Operation cannot be fulfilled on secrets "pull-secret": the object has been modified' >&2
		exit 1
	fi
	printf '%s' "$input" >"$WORK/replaced.json"
	echo 'secret/pull-secret replaced'
	;;
*) exit 2 ;;
esac
EOF
REAL_JQ=$(command -v jq)
cat >"$BIN/jq" <<EOF
#!/usr/bin/env bash
printf '%s\n' "\$*" >>"\$WORK/jq-args.log"
exec "$REAL_JQ" "\$@"
EOF
chmod +x "$BIN/oc" "$BIN/jq"
export WORK

b64() { printf '%s' "$1" | base64 | tr -d '\n'; }
TOKEN=$(b64 'rhoai+robot:s3cr3t-token')
OTHERS='{"auths":{"cloud.openshift.com":{"auth":"Y2xvdWQ6eA==","email":"a@example.com"},"registry.redhat.io":{"auth":"cmVkaGF0Ong="},"quay.io/rhoai":{"auth":"b2xkOnRva2Vu"}}}'

# secret writes the live Secret: resourceVersion 42, .dockerconfigjson $1.
secret() {
	printf '{"apiVersion":"v1","kind":"Secret","type":"kubernetes.io/dockerconfigjson","metadata":{"name":"pull-secret","namespace":"openshift-config","resourceVersion":"42"},"data":{".dockerconfigjson":"%s"}}' "$1" >"$WORK/secret.json"
	echo 42 >"$WORK/live-rv"
}

# in_shell SHELL SCRIPT runs SCRIPT in SHELL with the fakes first on PATH;
# zsh with -f: no startup files, which could put another jq or oc first.
in_shell() {
	if [ "$1" = zsh ]; then
		PATH="$BIN:$PATH" zsh -f -c "$2"
	else
		PATH="$BIN:$PATH" "$1" -c "$2"
	fi
}

# run SHELL INPUT: runs the command file in SHELL with INPUT on stdin.
run() {
	rm -f "$WORK/replaced.json" "$WORK/oc.log" "$WORK/jq-args.log"
	printf '%s' "$2" | in_shell "$1" "$(cat "$CMD_FILE")" >"$WORK/out" 2>&1 || true
}

# auths prints the .auths of the replaced Secret's .dockerconfigjson.
auths() { jq -c '.data[".dockerconfigjson"] | @base64d | fromjson | .auths' "$WORK/replaced.json"; }

SHELLS="bash"
command -v zsh >/dev/null 2>&1 && SHELLS="bash zsh"
for sh in $SHELLS; do
	# Adds the entry, keeps every other login and the rest of the object.
	secret "$(b64 "$OTHERS")"
	run "$sh" "$TOKEN
"
	if [ -f "$WORK/replaced.json" ]; then
		pass
		expect_eq "$sh: quay.io/rhoai entry" "{\"auth\":\"$TOKEN\"}" "$(auths | jq -c '."quay.io/rhoai"')"
		expect_eq "$sh: other logins kept" "$(jq -c '.auths | del(."quay.io/rhoai")' <<<"$OTHERS")" "$(auths | jq -c 'del(."quay.io/rhoai")')"
		expect_eq "$sh: resourceVersion kept" 42 "$(jq -r .metadata.resourceVersion "$WORK/replaced.json")"
		expect_eq "$sh: object otherwise unchanged" "$(jq -c 'del(.data)' "$WORK/secret.json")" "$(jq -c 'del(.data)' "$WORK/replaced.json")"
	else
		fail "$sh: nothing was replaced: $(cat "$WORK/out")"
	fi
	grep -qF "$TOKEN" "$WORK/jq-args.log" && fail "$sh: the token was an argument of jq (ps shows it)" || pass
	grep -qF "$TOKEN" "$WORK/out" && fail "$sh: the token was printed" || pass
	grep -qF 'quay.io/rhoai token' "$WORK/out" && pass || fail "$sh: no prompt: $(cat "$WORK/out")"

	# A Secret without the entry gets it.
	secret "$(b64 '{"auths":{"registry.redhat.io":{"auth":"cmVkaGF0Ong="}}}')"
	run "$sh" "$TOKEN
"
	expect_eq "$sh: entry added" "{\"quay.io/rhoai\":{\"auth\":\"$TOKEN\"},\"registry.redhat.io\":{\"auth\":\"cmVkaGF0Ong=\"}}" "$(auths 2>/dev/null | jq -cS . || true)"

	# No token (an empty line, or end of input): oc is not run at all.
	secret "$(b64 "$OTHERS")"
	run "$sh" "
"
	[ ! -e "$WORK/oc.log" ] && pass || fail "$sh: oc ran without a token: $(cat "$WORK/oc.log")"
	run "$sh" ""
	[ ! -e "$WORK/oc.log" ] && pass || fail "$sh: oc ran at end of input: $(cat "$WORK/oc.log")"

	# A missing Secret: jq gets nothing, oc replace gets no object.
	rm -f "$WORK/secret.json"
	run "$sh" "$TOKEN
"
	[ ! -e "$WORK/replaced.json" ] && pass || fail "$sh: replaced without a Secret"
	grep -qF 'no objects passed to replace' "$WORK/out" && pass || fail "$sh: missing Secret: $(cat "$WORK/out")"

	# A garbled .dockerconfigjson: jq fails, nothing is replaced.
	secret "bm90IGpzb24="
	run "$sh" "$TOKEN
"
	[ ! -e "$WORK/replaced.json" ] && pass || fail "$sh: replaced a garbled Secret"

	# The Secret changed between get and replace: the conflict stops it.
	secret "$(b64 "$OTHERS")"
	echo 43 >"$WORK/live-rv"
	run "$sh" "$TOKEN
"
	[ ! -e "$WORK/replaced.json" ] && pass || fail "$sh: replaced a Secret that changed meanwhile"
	grep -qF 'Conflict' "$WORK/out" && pass || fail "$sh: no conflict: $(cat "$WORK/out")"

	# The token is not left in the shell.
	secret "$(b64 "$OTHERS")"
	rm -f "$WORK/replaced.json"
	# shellcheck disable=SC2016 # expanded by the shell under test
	printf '%s\n' "$TOKEN" | in_shell "$sh" "$(cat "$CMD_FILE")"'
echo "left=[${QUAY_AUTH-}]"' >"$WORK/out" 2>&1 || true
	grep -qF 'left=[]' "$WORK/out" && pass || fail "$sh: QUAY_AUTH left set: $(cat "$WORK/out")"
done

echo "test-node-pull-secret.sh: $PASSED passed, $FAILED failed ($SHELLS; $(jq --version))"
[ "$FAILED" -eq 0 ]
