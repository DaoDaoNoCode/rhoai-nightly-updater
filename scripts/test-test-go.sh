#!/bin/sh
# shellcheck disable=SC2015,SC2086 # pass never fails; $SH may be "busybox sh"
# Tests for scripts/test-go.sh, on a small module of its own: two packages
# under pkg/cluster/ and one other, whose tests record their names when
# they run. Needs go (with -race); no network.
#   sh scripts/test-test-go.sh
set -eu

HERE=$(cd "$(dirname "$0")" && pwd)
# The shell that runs test-go.sh: SH=dash, SH="busybox sh"...
SH=${SH:-sh}
command -v go >/dev/null 2>&1 || {
	echo "SKIP: go is not installed; test-go.sh was not tested" >&2
	exit 0
}
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
FAILED=0
PASSED=0
fail() {
	printf 'FAIL: %s\n' "$*" >&2
	FAILED=$((FAILED + 1))
}
pass() { PASSED=$((PASSED + 1)); }
expect_eq() {
	if [ "$2" = "$3" ]; then pass; else fail "$1: want '$2', got '$3'"; fi
}
# expect_in NAME TEXT OUTPUT
expect_in() {
	if printf '%s' "$3" | grep -qF -- "$2"; then pass; else fail "$1: expected '$2' in: $3"; fi
}

# The module's own go version and toolchain: no download, no workspace.
unset GOFLAGS GO_TEST_SHARDS
export GOTOOLCHAIN=local GOWORK=off

REPO="$WORK/repo"
mkdir -p "$REPO/scripts" "$REPO/pkg/cluster/a" "$REPO/pkg/cluster/b" "$REPO/pkg/other"
cp "$HERE/test-go.sh" "$REPO/scripts/test-go.sh"
printf 'module example.com/fake\n\ngo 1.22\n' >"$REPO/go.mod"

# package_with_tests DIR NAME SUBTESTS TEST...: a package whose tests
# append their full names (subtests too) to $RAN, and fail when named in
# $FAIL_TEST (names separated by spaces). With $MEET set, the tests named
# in it meet: each writes its name into the directory $MEET_DIR and waits
# up to $MEET_SECONDS for the others', so they pass only when they run at
# the same time. With SUBTESTS=yes the first test has the subtests "sub"
# and "other".
package_with_tests() {
	dir=$1 name=$2 first=$3
	shift 3
	{
		printf 'package %s\n\nimport (\n\t"os"\n\t"path/filepath"\n\t"strconv"\n\t"strings"\n\t"testing"\n\t"time"\n)\n\n' "$name"
		printf 'func record(t *testing.T) {\n'
		printf '\tf, err := os.OpenFile(os.Getenv("RAN"), os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)\n'
		printf '\tif err != nil {\n\t\tt.Fatal(err)\n\t}\n'
		printf '\tdefer f.Close()\n'
		printf '\tif _, err := f.WriteString(t.Name() + "\\n"); err != nil {\n\t\tt.Fatal(err)\n\t}\n'
		printf '\tif strings.Contains(" "+os.Getenv("FAIL_TEST")+" ", " "+t.Name()+" ") {\n\t\tt.Fatal("failing as asked")\n\t}\n'
		printf '\tif meet := strings.Fields(os.Getenv("MEET")); strings.Contains(" "+os.Getenv("MEET")+" ", " "+t.Name()+" ") {\n'
		printf '\t\tdir := os.Getenv("MEET_DIR")\n'
		printf '\t\tif err := os.WriteFile(filepath.Join(dir, t.Name()), nil, 0o644); err != nil {\n\t\t\tt.Fatal(err)\n\t\t}\n'
		printf '\t\tsecs, _ := strconv.Atoi(os.Getenv("MEET_SECONDS"))\n'
		printf '\t\tdeadline := time.Now().Add(time.Duration(secs) * time.Second)\n'
		printf '\t\tfor _, other := range meet {\n'
		printf '\t\t\tfor {\n'
		printf '\t\t\t\tif _, err := os.Stat(filepath.Join(dir, other)); err == nil {\n\t\t\t\t\tbreak\n\t\t\t\t}\n'
		printf '\t\t\t\tif time.Now().After(deadline) {\n\t\t\t\t\tt.Fatalf("%%s did not run at the same time", other)\n\t\t\t\t}\n'
		printf '\t\t\t\ttime.Sleep(10 * time.Millisecond)\n'
		printf '\t\t\t}\n\t\t}\n\t}\n}\n'
		for t in "$@"; do
			if [ "$first" = yes ]; then
				printf '\nfunc %s(t *testing.T) {\n\trecord(t)\n' "$t"
				printf '\tt.Run("sub", func(t *testing.T) { record(t) })\n'
				printf '\tt.Run("other", func(t *testing.T) { record(t) })\n}\n'
				first=0
			else
				printf '\nfunc %s(t *testing.T) { record(t) }\n' "$t"
			fi
		done
	} >"$REPO/$dir/${name}_test.go"
}
package_with_tests pkg/cluster/a a yes TestA1 TestA2 TestA3 TestA4
package_with_tests pkg/cluster/b b no TestB1 TestB2 TestB3
package_with_tests pkg/other other no TestOther1
# A package without tests, named as a package with tests (pkg/cluster/a):
# it is compiled, but gives no test binary to collide with a.test.
mkdir -p "$REPO/pkg/lib/a"
printf 'package a\n\nfunc Lib() int { return 1 }\n' >"$REPO/pkg/lib/a/a.go"
gofmt -l "$REPO" | grep . && fail "the generated tests are not gofmt-clean" || pass

ALL='TestA1
TestA1/other
TestA1/sub
TestA2
TestA3
TestA4
TestB1
TestB2
TestB3
TestOther1'

# run NAME [FLAG...]: runs test-go.sh with each pkg/cluster package in 3
# parts (a part per test at most: GO_TEST_PART_TESTS=1); $out is its
# output, $rc its exit status, $ran the tests that ran (sorted, duplicates
# kept). The caller may set GO_TEST_SHARDS and GO_TEST_JOBS.
run() {
	name=$1
	shift
	: >"$WORK/ran"
	rc=0
	out=$(RAN="$WORK/ran" GO_TEST_SHARDS=${GO_TEST_SHARDS:-3} GO_TEST_PART_TESTS=1 $SH "$REPO/scripts/test-go.sh" "$@" 2>&1) || rc=$?
	ran=$(sort "$WORK/ran")
}

# Every test runs exactly once, its subtests with it.
run all
expect_eq "all: exit status" 0 "$rc"
expect_eq "all: every test once" "$ALL" "$ran"
expect_in "all: the jobs" "3 package(s) with tests, as 7 process(es)" "$out"
expect_in "all: the split packages" "split: example.com/fake/pkg/cluster/a (4 tests, 3 parts) example.com/fake/pkg/cluster/b (3 tests, 3 parts)" "$out"
expect_in "all: a part's line" "example.com/fake/pkg/cluster/b	part 3 of 3, 1 tests" "$out"

# A caller's -run filters every part of every package, not only the part
# whose own selection it would replace. A file whose name the pattern
# matches as a glob must not change the pattern either.
: >"$REPO/pkg/cluster/a/-test.run=TestA4|TestB2|TestOther1"
run "caller -run" '-run=TestA[14]|TestB2|TestOther1'
expect_eq "caller -run: exit status" 0 "$rc"
expect_eq "caller -run: only the matching tests, once" 'TestA1
TestA1/other
TestA1/sub
TestA4
TestB2
TestOther1' "$ran"
rm "$REPO/pkg/cluster/a/-test.run=TestA4|TestB2|TestOther1"

# Its subtest level still applies.
run "caller -run subtest" '-run=TestA1/sub'
expect_eq "caller -run subtest: exit status" 0 "$rc"
expect_eq "caller -run subtest: the test and that subtest" 'TestA1
TestA1/sub' "$ran"

# A flag value with spaces stays one argument; a caller's -skip adds to
# the parts' own.
run "caller -skip" '-skip=TestB1|no such test|TestA1/other'
expect_eq "caller -skip: exit status" 0 "$rc"
expect_eq "caller -skip: all but the skipped" 'TestA1
TestA1/sub
TestA2
TestA3
TestA4
TestB2
TestB3
TestOther1' "$ran"

# A failing test fails the run and names its part; the other parts still run.
FAIL_TEST=TestB2 run "failing pkg/cluster test"
expect_eq "failing pkg/cluster test: exit status" 1 "$rc"
expect_in "failing pkg/cluster test: the failure" "--- FAIL: TestB2" "$out"
expect_in "failing pkg/cluster test: the summary" "FAILED: pkg/cluster/b-part-" "$out"
expect_eq "failing pkg/cluster test: every test ran once" "$ALL" "$ran"

FAIL_TEST=TestOther1 run "failing other test"
expect_eq "failing other test: exit status" 1 "$rc"
expect_in "failing other test: the failure" "--- FAIL: TestOther1" "$out"
expect_in "failing other test: the summary" "FAILED: pkg/other" "$out"

# Failures in two packages at once: both reported, every test still ran.
FAIL_TEST="TestA3 TestOther1" run "failing tests in two packages"
expect_eq "two failures: exit status" 1 "$rc"
expect_in "two failures: the first" "--- FAIL: TestA3" "$out"
expect_in "two failures: the second" "--- FAIL: TestOther1" "$out"
expect_in "two failures: pkg/other named" "pkg/other" "$(printf '%s' "$out" | grep '^FAILED:')"
expect_in "two failures: pkg/cluster/a named" "pkg/cluster/a-part-" "$(printf '%s' "$out" | grep '^FAILED:')"
expect_eq "two failures: every test ran once" "$ALL" "$ran"

# A variable set for a function call stays set after it (POSIX sh).
unset FAIL_TEST

# Test binaries run at the same time: TestA1 (pkg/cluster/a) and TestB1
# (pkg/cluster/b) wait for each other. Two at a time, they meet; one at a
# time (GO_TEST_JOBS=1) the first waits in vain and fails, which proves
# the check can fail.
mkdir "$WORK/meet1" "$WORK/meet2"
MEET="TestA1 TestB1" MEET_DIR="$WORK/meet1" MEET_SECONDS=30 GO_TEST_SHARDS=1 GO_TEST_JOBS=2 run "two at a time"
expect_eq "two at a time: exit status" 0 "$rc"
[ "$rc" = 0 ] || printf '%s\n' "$out" >&2
expect_eq "two at a time: every test once" "$ALL" "$ran"
MEET="TestA1 TestB1" MEET_DIR="$WORK/meet2" MEET_SECONDS=1 GO_TEST_SHARDS=1 GO_TEST_JOBS=1 run "one at a time"
expect_eq "one at a time: exit status" 1 "$rc"
expect_in "one at a time: they did not meet" "did not run at the same time" "$out"

unset MEET MEET_DIR MEET_SECONDS GO_TEST_SHARDS GO_TEST_JOBS

# Two packages whose test binaries would share a name are refused.
mkdir -p "$REPO/pkg/dup/a"
package_with_tests pkg/dup/a a no TestDup
run "same binary name"
expect_eq "same binary name: exit status" 1 "$rc"
expect_in "same binary name: says so" "several packages with tests end in: a" "$out"
rm -r "$REPO/pkg/dup"

# A package without tests that does not compile fails the run, as it
# fails go test ./..., though nothing imports it; no test runs.
mkdir -p "$REPO/pkg/broken"
printf 'package broken\n\nfunc F() int { return "not an int" }\n' >"$REPO/pkg/broken/broken.go"
run "broken package without tests"
expect_eq "broken package without tests: exit status" 1 "$rc"
expect_in "broken package without tests: names it" "example.com/fake/pkg/broken" "$out"
expect_in "broken package without tests: the compiler's error" "broken.go:3" "$out"
expect_eq "broken package without tests: no test ran" "" "$ran"
rm -r "$REPO/pkg/broken"

# Not flags, and patterns in a separate argument, are refused.
run "not a flag" -run TestA1
expect_eq "separate pattern: exit status" 2 "$rc"
expect_in "separate pattern: says how" "-run needs its pattern in the same argument" "$out"
run "not a flag" TestA1
expect_eq "not a flag: exit status" 2 "$rc"
expect_in "not a flag: says so" "TestA1 is not a flag" "$out"

echo "test-test-go.sh: $PASSED passed, $FAILED failed ($SH)"
[ "$FAILED" = 0 ]
