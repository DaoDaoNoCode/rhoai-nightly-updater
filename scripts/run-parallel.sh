#!/bin/sh
# run-parallel.sh CMD...: runs every CMD (a shell command line) at the same
# time, then prints each one's output in the order given, under a header,
# and a summary: how long each took, every "SKIP:" line it printed (a test
# that did not run in full says so there) and every failure. Exits 1 if
# any CMD failed. `make test-scripts` uses it: the script tests are
# independent (each works in its own mktemp directory) and take their time
# starting processes, not on a CPU.
set -eu

[ "$#" -gt 0 ] || {
	echo "Usage: $0 CMD..." >&2
	exit 2
}
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT INT TERM

i=0
for cmd in "$@"; do
	(
		start=$(date +%s)
		rc=0
		sh -c "$cmd" >"$work/$i.log" 2>&1 </dev/null || rc=$?
		echo "$rc $(($(date +%s) - start))" >"$work/$i.rc"
	) &
	i=$((i + 1))
done
wait

summary=""
failed=0
i=0
for cmd in "$@"; do
	read -r rc secs <"$work/$i.rc"
	echo "=== $cmd"
	cat "$work/$i.log"
	if [ "$rc" = 0 ]; then
		summary="$summary
ok    ${secs}s  $cmd"
	else
		summary="$summary
FAIL  ${secs}s  $cmd (exit $rc)"
		failed=1
	fi
	skips=$(grep '^SKIP:' "$work/$i.log" || true)
	if [ -n "$skips" ]; then
		summary="$summary
$(printf '%s\n' "$skips" | sed 's/^/      /')"
	fi
	i=$((i + 1))
done
echo "=== summary$summary"
exit "$failed"
