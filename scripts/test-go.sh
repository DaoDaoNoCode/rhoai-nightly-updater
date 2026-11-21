#!/bin/sh
# go test -race -count=1 over every package, as one would run
# `go test -race -count=1 ./...`, with the test binaries run side by side
# and a package with many tests split into parts: the ~1,200 tests of
# pkg/cluster run one after another (they change package variables, so they
# cannot use t.Parallel) and would take most of the time. A part runs a
# share of the package's top-level tests, chosen from the test binary's own
# -test.list, so every test runs exactly once; a test's subtests stay with
# it. `make test-go` and the CI go job run this script.
#
# The race test binaries of every package with tests are built at once
# (go test -c, which builds in parallel), then run as jobs, at most
# GO_TEST_JOBS at a time, the longest first: each part of a split package,
# and each other package whole. A package is split only when it has at
# least GO_TEST_PART_TESTS tests: a part then holds about that many, and
# there are at most GO_TEST_SHARDS parts.
#
# Arguments: test flags for every package, such as -shuffle=on or
# -run=TestX; each is passed to the test binaries as -test.<flag>, one
# argument each (a value may hold spaces). A value goes in the same
# argument (-run=TestX, not -run TestX). -run and -skip filter the tests of
# every package and every part: a part runs its own share by skipping the
# others' tests, its -skip pattern being theirs plus, as a top-level
# alternative, the caller's.
#
# Environment (defaults: the CPUs, nproc):
#   GO_TEST_JOBS        test processes at a time (default: the CPUs)
#   GO_TEST_SHARDS      most parts of one package (default: the CPUs; 1
#                       runs every package in one process)
#   GO_TEST_PART_TESTS  tests per part of a split package (default 200)
#
# Output: each job's line in a fixed order (package, then part), the output
# of each failed one in full, then a summary; exits non-zero if any failed.
set -eu

cd "$(dirname "$0")/.."
cpus=$(nproc 2>/dev/null || getconf _NPROCESSORS_ONLN 2>/dev/null || echo 1)
jobs=${GO_TEST_JOBS:-$cpus}
[ "$jobs" -ge 1 ] || jobs=1
shards=${GO_TEST_SHARDS:-$cpus}
[ "$shards" -ge 1 ] || shards=1
per=${GO_TEST_PART_TESTS:-200}
[ "$per" -ge 1 ] || per=1

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
module=$(go list -m)

# The packages with tests: "<index> <import path> <dir>".
go list -f '{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}} {{.Dir}}{{end}}' ./... |
	awk 'NF { print NR, $0 }' >"$work/pkgs"
[ -s "$work/pkgs" ] || {
	echo "test-go.sh: no package has tests" >&2
	exit 1
}
# go test -c -o DIR/ names each binary after the last element of its
# import path; two such packages would overwrite each other's.
dup=$(awk '{ n = split($2, p, "/"); print p[n] }' "$work/pkgs" | sort | uniq -d)
[ -z "$dup" ] || {
	echo "test-go.sh: several packages with tests end in: $dup (their test binaries would share a name)" >&2
	exit 1
}

# Their race-instrumented test binaries, built at once.
mkdir "$work/bin"
# shellcheck disable=SC2046 # the packages are a list
go test -race -c -o "$work/bin/" $(awk '{ print $2 }' "$work/pkgs")

# Each package's binary, and how many parts it runs in: its tests (counted
# from the source; the parts are dealt from -test.list) per part, at most
# $shards.
while read -r i pkg dir; do
	bin="$work/bin/${pkg##*/}.test"
	[ -x "$bin" ] || {
		echo "test-go.sh: no test binary for $pkg" >&2
		exit 1
	}
	est=$(cat "$dir"/*_test.go | grep -cE '^func (Test|Example|Fuzz)[^a-z]' || true)
	parts=$(((est + per - 1) / per))
	[ "$parts" -le "$shards" ] || parts=$shards
	[ "$parts" -ge 1 ] || parts=1
	echo "$i $parts $est" >"$work/plan.$i"
	printf '%s\n' "$pkg" >"$work/pkg.$i"
	printf '%s\n' "$dir" >"$work/dir.$i"
done <"$work/pkgs"

# A split package's tests, from its binary, dealt out in order, one each
# per part in turn. The binaries list at the same time.
while read -r i pkg dir; do
	read -r _ parts _ <"$work/plan.$i"
	[ "$parts" -gt 1 ] || continue
	(cd "$dir" && "$work/bin/${pkg##*/}.test" -test.list '.*' >"$work/$i.tests" 2>&1; echo $? >"$work/$i.listrc") </dev/null &
done <"$work/pkgs"
wait
while read -r i pkg dir; do
	read -r _ parts _ <"$work/plan.$i"
	[ "$parts" -gt 1 ] || continue
	if [ "$(cat "$work/$i.listrc")" != 0 ] || [ ! -s "$work/$i.tests" ]; then
		cat "$work/$i.tests" >&2
		echo "test-go.sh: $pkg lists no tests" >&2
		exit 1
	fi
	awk -v d="$work/part.$i." -v k="$parts" '{ print > (d (NR - 1) % k) }' "$work/$i.tests"
done <"$work/pkgs"

# The jobs, longest first: "<tests> <index> <part> <parts>" (part 0 of 1:
# the whole package).
while read -r i pkg dir; do
	read -r _ parts est <"$work/plan.$i"
	p=0
	while [ "$p" -lt "$parts" ]; do
		echo "$((est / parts)) $i $p $parts"
		p=$((p + 1))
	done
done <"$work/pkgs" | sort -k1,1nr -k2,2n -k3,3n >"$work/jobs"

npkgs=$(wc -l <"$work/pkgs" | tr -d ' ')
njobs=$(wc -l <"$work/jobs" | tr -d ' ')
split=$(awk '$4 > 1 && $3 == 0 { print $2 }' "$work/jobs" | while read -r i; do
	read -r _ parts est <"$work/plan.$i"
	printf ' %s (%s tests, %s parts)' "$(awk -v i="$i" '$1 == i { print $2 }' "$work/pkgs")" "$est" "$parts"
done)
echo "go test -race: $npkgs package(s) with tests, as $njobs process(es), $jobs at a time, on $cpus CPU(s); split:${split:- none}${1+; test flags: $*}"

# run_job I PART PARTS: runs a package, or a part of it, in the package's
# directory (as go test does); "<exit status> <seconds>" in job.I.PART.rc.
run_job() {
	i=$1 p=$2 k=$3
	pkg=$(cat "$work/pkg.$i")
	dir=$(cat "$work/dir.$i")
	log="$work/job.$i.$p.log"
	pat=""
	if [ "$k" -gt 1 ]; then
		# This part skips the package's tests dealt to the other parts,
		# so the caller's -run (a binary takes one) still chooses among
		# its own. go test splits a -skip pattern at a top-level "|"
		# into alternatives and skips a test any of them matches
		# (testing/match.go), so the caller's -skip is one more.
		if grep -vxF -f "$work/part.$i.$p" "$work/$i.tests" >"$work/skip.$i.$p"; then
			pat="^($(paste -sd'|' "$work/skip.$i.$p"))\$"
		fi
	fi
	[ -z "$skip" ] || pat="${pat:+$pat|}$skip"
	# Linux limits one argument to 128 KiB (MAX_ARG_STRLEN).
	if [ "${#pat}" -gt 120000 ]; then
		echo "test-go.sh: the -skip pattern of $pkg part $((p + 1)) is ${#pat} bytes, too long for one argument" >"$log"
		echo "2 0" >"$work/job.$i.$p.rc"
		return
	fi
	eval "set -- $binflags"
	[ -z "$pat" ] || set -- "$@" "-test.skip=$pat"
	start=$(date +%s)
	rc=0
	(cd "$dir" && "$work/bin/${pkg##*/}.test" -test.paniconexit0 -test.count=1 -test.timeout=10m "$@") >"$log" 2>&1 || rc=$?
	echo "$rc $(($(date +%s) - start))" >"$work/job.$i.$p.rc"
}

# At most $jobs at a time: a job takes a token from the pipe before it
# starts and puts it back when it ends.
mkfifo "$work/slots"
exec 3<>"$work/slots"
t=0
while [ "$t" -lt "$jobs" ]; do
	echo >&3
	t=$((t + 1))
done
while read -r _ i p k; do
	read -r _ <&3
	(
		# The token goes back whatever happens to the job; a job that
		# wrote no status failed.
		rc=0
		run_job "$i" "$p" "$k" || rc=$?
		[ -f "$work/job.$i.$p.rc" ] || echo "${rc:-1} 0" >"$work/job.$i.$p.rc"
		echo >&3
	) </dev/null &
done <"$work/jobs"
wait
exec 3>&-

# In package order: a line per job, the output of a failed one in full.
failed=""
while read -r i pkg dir; do
	read -r _ parts _ <"$work/plan.$i"
	rel=${pkg#"$module"}
	rel=${rel#/}
	p=0
	while [ "$p" -lt "$parts" ]; do
		read -r rc secs <"$work/job.$i.$p.rc"
		what="${secs}s"
		name=${rel:-.}
		if [ "$parts" -gt 1 ]; then
			what="part $((p + 1)) of $parts, $(wc -l <"$work/part.$i.$p" | tr -d ' ') tests, ${secs}s"
			name="$name-part-$((p + 1))"
		fi
		if [ "$rc" = 0 ]; then
			echo "ok  	$pkg	$what"
		else
			cat "$work/job.$i.$p.log"
			echo "FAIL	$pkg	$what (exit $rc)"
			failed="$failed $name"
		fi
		p=$((p + 1))
	done
done <"$work/pkgs"
if [ -n "$failed" ]; then
	echo "FAILED:$failed"
	exit 1
fi
