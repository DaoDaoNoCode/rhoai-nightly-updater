#!/bin/sh
# go test -race -count=1 over every package, as one would run
# `go test -race -count=1 ./...`, but with pkg/cluster split into several
# processes: its ~1,200 tests run one after another (they change package
# variables, so they cannot use t.Parallel) and take most of the time.
# Each process runs a share of the top-level tests, chosen from the test
# binary's own -test.list, so every test runs exactly once; a test's
# subtests stay with it. `make test-go` and the CI go job run this script.
#
# Environment: GO_TEST_SHARDS, the number of pkg/cluster processes (default:
# the CPUs, at most 4; 1 runs the package in one process).
#
# Output: each part's output in full, in a fixed order, then a summary;
# exits non-zero if any part failed.
set -eu

cd "$(dirname "$0")/.."
cpus=$(nproc 2>/dev/null || getconf _NPROCESSORS_ONLN 2>/dev/null || echo 1)
shards=${GO_TEST_SHARDS:-$cpus}
[ "$shards" -le 4 ] || shards=4
[ "$shards" -ge 1 ] || shards=1

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT INT TERM

# The cluster package's race-instrumented test binary, built once.
go test -race -c -o "$work/cluster.test" ./pkg/cluster
(cd pkg/cluster && "$work/cluster.test" -test.list '.*') >"$work/tests"
total=$(wc -l <"$work/tests" | tr -d ' ')
[ "$total" -gt 0 ] || {
	echo "pkg/cluster lists no tests" >&2
	exit 1
}

# Deal the tests out in order, one each per shard in turn.
i=0
while IFS= read -r name; do
	printf '%s\n' "$name" >>"$work/shard.$((i % shards))"
	i=$((i + 1))
done <"$work/tests"

others=$(go list ./... | grep -v '/pkg/cluster$')
echo "go test -race: pkg/cluster ($total tests) in $shards process(es) on $cpus CPU(s), with the other packages alongside"

# shellcheck disable=SC2086 # $others is a list of packages.
(go test -race -count=1 $others >"$work/others.log" 2>&1; echo $? >"$work/others.rc") &
s=0
while [ "$s" -lt "$shards" ]; do
	run="^($(paste -sd'|' "$work/shard.$s"))\$"
	(
		cd pkg/cluster
		start=$(date +%s)
		rc=0
		"$work/cluster.test" -test.count=1 -test.timeout=10m -test.run "$run" >"$work/shard.$s.log" 2>&1 || rc=$?
		echo "$rc $(($(date +%s) - start))" >"$work/shard.$s.rc"
	) &
	s=$((s + 1))
done
wait

# As go test prints a package: its output only when it failed.
failed=""
cat "$work/others.log"
[ "$(cat "$work/others.rc")" = 0 ] || failed="$failed other-packages"
pkg=$(go list ./pkg/cluster)
s=0
while [ "$s" -lt "$shards" ]; do
	n=$(wc -l <"$work/shard.$s" | tr -d ' ')
	read -r rc secs <"$work/shard.$s.rc"
	if [ "$rc" = 0 ]; then
		echo "ok  	$pkg	part $((s + 1)) of $shards, $n tests, ${secs}s"
	else
		cat "$work/shard.$s.log"
		echo "FAIL	$pkg	part $((s + 1)) of $shards, $n tests, ${secs}s (exit $rc)"
		failed="$failed pkg/cluster-part-$((s + 1))"
	fi
	s=$((s + 1))
done
if [ -n "$failed" ]; then
	echo "FAILED:$failed"
	exit 1
fi
