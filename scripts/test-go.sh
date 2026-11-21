#!/bin/sh
# go test -race -count=1 over every package, as one would run
# `go test -race -count=1 ./...`, but with the packages of pkg/cluster/...
# split into several processes: their ~1,200 tests run one after another
# (they change package variables, so they cannot use t.Parallel) and take
# most of the time. Each process runs a share of the top-level tests,
# chosen from each test binary's own -test.list, so every test runs exactly
# once; a test's subtests stay with it. `make test-go` and the CI go job run
# this script.
#
# Arguments: test flags for every package, such as -shuffle=on or
# -run=TestX; each is passed to go test as given and to the test binaries
# as -test.<flag>, one argument each (a value may hold spaces). A value goes
# in the same argument (-run=TestX, not -run TestX). -run and -skip filter
# the tests of every package and every part: a part runs its own share by
# skipping the others' tests, its -skip pattern being theirs plus, as a
# top-level alternative, the caller's.
#
# Environment: GO_TEST_SHARDS, the number of processes for pkg/cluster/...
# (default: the CPUs, at most 4; 1 runs them in one process).
#
# Output: each part's output in full, in a fixed order, then a summary;
# exits non-zero if any part failed.
set -eu

cd "$(dirname "$0")/.."
cpus=$(nproc 2>/dev/null || getconf _NPROCESSORS_ONLN 2>/dev/null || echo 1)
shards=${GO_TEST_SHARDS:-$cpus}
[ "$shards" -le 4 ] || shards=4
[ "$shards" -ge 1 ] || shards=1

# quote ARG: ARG in single quotes, for eval to give back as one word.
quote() {
	printf "'%s'" "$(printf '%s' "$1" | sed "s/'/'\\\\''/g")"
}

# The test binaries' flags, each quoted (eval "set -- $binflags"); the
# caller's -skip pattern apart, as the parts add theirs to it.
binflags=""
skip=""
for f in "$@"; do
	case $f in
	-test.*) b=$f ;;
	-*) b=-test.${f#-} ;;
	*)
		echo "test-go.sh: $f is not a flag" >&2
		exit 2
		;;
	esac
	case $b in
	-test.skip=*) skip=${b#-test.skip=} ;;
	-test.skip | -test.run)
		echo "test-go.sh: $f needs its pattern in the same argument ($f=<regexp>)" >&2
		exit 2
		;;
	*) binflags="$binflags $(quote "$b")" ;;
	esac
done

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT INT TERM

# The packages of pkg/cluster/... with tests: "<index> <import path> <dir>".
go list -f '{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}} {{.Dir}}{{end}}' ./pkg/cluster/... |
	awk 'NF { print NR, $0 }' >"$work/split"
[ -s "$work/split" ] || {
	echo "pkg/cluster/... has no tests" >&2
	exit 1
}

# Their race-instrumented test binaries, built once, and their tests.
while read -r i pkg dir; do
	go test -race -c -o "$work/$i.test" "$pkg"
	(cd "$dir" && "$work/$i.test" -test.list '.*') >"$work/$i.tests"
	[ -s "$work/$i.tests" ] || {
		echo "$pkg lists no tests" >&2
		exit 1
	}
done <"$work/split"

# Deal the tests out in order, one each per shard in turn, across packages.
n=0
while read -r i pkg dir; do
	while IFS= read -r name; do
		printf '%s\n' "$name" >>"$work/shard.$((n % shards)).$i"
		n=$((n + 1))
	done <"$work/$i.tests"
done <"$work/split"

npkgs=$(wc -l <"$work/split" | tr -d ' ')
awk '{ print $2 }' "$work/split" >"$work/split.pkgs"
others=$(go list ./... | grep -vxF -f "$work/split.pkgs" || true)
echo "go test -race: pkg/cluster/... ($n tests in $npkgs package(s)) in $shards process(es) on $cpus CPU(s), with the other packages alongside${1+; test flags: $*}"

# shellcheck disable=SC2086 # $others and the flags are lists.
(go test -race -count=1 "$@" $others >"$work/others.log" 2>&1; echo $? >"$work/others.rc") &
s=0
while [ "$s" -lt "$shards" ]; do
	(
		while read -r i pkg dir; do
			[ -f "$work/shard.$s.$i" ] || continue
			# This part skips the package's tests dealt to the other parts,
			# so the caller's -run (a binary takes one) still chooses among
			# its own. go test splits a -skip pattern at a top-level "|"
			# into alternatives and skips a test any of them matches
			# (testing/match.go), so the caller's -skip is one more.
			pat=""
			if grep -vxF -f "$work/shard.$s.$i" "$work/$i.tests" >"$work/skip.$s.$i"; then
				pat="^($(paste -sd'|' "$work/skip.$s.$i"))\$"
			fi
			[ -z "$skip" ] || pat="${pat:+$pat|}$skip"
			# Linux limits one argument to 128 KiB (MAX_ARG_STRLEN).
			if [ "${#pat}" -gt 120000 ]; then
				echo "test-go.sh: the -skip pattern of $pkg part $((s + 1)) is ${#pat} bytes, too long for one argument" >"$work/shard.$s.$i.log"
				echo "2 0" >"$work/shard.$s.$i.rc"
				continue
			fi
			eval "set -- $binflags"
			[ -z "$pat" ] || set -- "$@" "-test.skip=$pat"
			start=$(date +%s)
			rc=0
			(cd "$dir" && "$work/$i.test" -test.count=1 -test.timeout=10m "$@") >"$work/shard.$s.$i.log" 2>&1 || rc=$?
			echo "$rc $(($(date +%s) - start))" >"$work/shard.$s.$i.rc"
		done <"$work/split"
	) &
	s=$((s + 1))
done
wait

# As go test prints a package: its output only when it failed.
failed=""
cat "$work/others.log"
[ "$(cat "$work/others.rc")" = 0 ] || failed="$failed other-packages"
while read -r i pkg dir; do
	s=0
	while [ "$s" -lt "$shards" ]; do
		if [ -f "$work/shard.$s.$i" ]; then
			count=$(wc -l <"$work/shard.$s.$i" | tr -d ' ')
			read -r rc secs <"$work/shard.$s.$i.rc"
			if [ "$rc" = 0 ]; then
				echo "ok  	$pkg	part $((s + 1)) of $shards, $count tests, ${secs}s"
			else
				cat "$work/shard.$s.$i.log"
				echo "FAIL	$pkg	part $((s + 1)) of $shards, $count tests, ${secs}s (exit $rc)"
				failed="$failed pkg/${pkg#*/pkg/}-part-$((s + 1))"
			fi
		fi
		s=$((s + 1))
	done
done <"$work/split"
if [ -n "$failed" ]; then
	echo "FAILED:$failed"
	exit 1
fi
