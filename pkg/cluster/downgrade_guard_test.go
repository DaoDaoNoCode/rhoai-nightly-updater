package cluster

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// installedCSVFixture sets up the installed operator the way each getCSV
// lookup path sees it.
type installedCSVFixture struct {
	name, csvName, version string
	// listOnly drops status.installedCSV so getCSV takes the list fallback.
	listOnly bool
}

func (fx installedCSVFixture) apply(f *fakeOLM) {
	f.installed(fx.csvName, nil)
	f.csvs[fx.csvName]["spec"].(map[string]interface{})["version"] = fx.version
	if fx.listOnly {
		status := f.sub["status"].(map[string]interface{})
		delete(status, "installedCSV")
		delete(status, "currentCSV")
	}
}

var downgradeFixtures = []installedCSVFixture{
	// B5 reads the CSV named by the Subscription; spec.version is the
	// authoritative version even when the name does not carry it.
	{name: "subscription installedCSV, version from spec.version", csvName: "rhods-operator.nightly-build", version: "3.7.0"},
	{name: "subscription installedCSV, version from the name", csvName: "rhods-operator.3.7.0", version: ""},
	// Without status.installedCSV the CSV list is read instead.
	{name: "list fallback", csvName: "rhods-operator.3.7.0", version: "3.7.0", listOnly: true},
}

func assertOnlyVerificationWrites(t *testing.T, f *fakeOLM) {
	t.Helper()
	for _, w := range f.writes() {
		if !strings.Contains(w, "-verify-") {
			t.Fatalf("a refused operation changed the cluster: %v", f.writes())
		}
	}
}

// Regression: after B5 switched getCSV to the Subscription's installedCSV,
// the Reinstall guard skipped itself whenever that CSV's name did not parse.
// The guard must find the version on every lookup path.
func TestReinstallDowngradeGuard_EveryCSVLookupPath(t *testing.T) {
	for _, fx := range downgradeFixtures {
		t.Run(fx.name, func(t *testing.T) {
			f := newFakeOLM(t)
			fx.apply(f)
			f.onSubscribe = olmInstalls("rhods-operator.3.6.0")
			result, err := Reinstall(f.client(context.Background()), "nightly", testNightlyImage, "")
			if err != nil || result.Success || result.ErrorCode != errorCodeDowngrade || !strings.Contains(result.Message, "older than the installed") {
				t.Fatalf("3.7.0 -> 3.6.0 was not refused: %+v, %v", result, err)
			}
			assertOnlyVerificationWrites(t, f)
		})
	}
}

// Fail closed: an installed CSV whose version cannot be read must not let a
// reinstall through silently; the user can confirm it explicitly.
func TestReinstallDowngradeGuard_UnknownInstalledVersionNeedsConfirmation(t *testing.T) {
	for _, listOnly := range []bool{false, true} {
		t.Run(fmt.Sprintf("listOnly=%v", listOnly), func(t *testing.T) {
			f := newFakeOLM(t)
			installedCSVFixture{csvName: "rhods-operator.custom-build", listOnly: listOnly}.apply(f)
			f.onSubscribe = olmInstalls("rhods-operator.3.6.0")
			c := f.client(context.Background())
			refused, err := Reinstall(c, "nightly", testNightlyImage, "")
			if err != nil || refused.Success || refused.ErrorCode != errorCodeDowngrade || !strings.Contains(refused.Message, "Cannot tell whether") {
				t.Fatalf("unknown installed version was not refused: %+v, %v", refused, err)
			}
			assertOnlyVerificationWrites(t, f)

			confirmed, err := ReinstallWithOptions(c, "nightly", testNightlyImage, "", OperationOptions{AllowDowngrade: true})
			if err != nil || !confirmed.Success {
				t.Fatalf("confirmed reinstall failed: %+v, %v", confirmed, err)
			}
		})
	}
}

// A same-version or newer target needs no confirmation on either path.
func TestReinstallDowngradeGuard_SameVersionProceeds(t *testing.T) {
	for _, listOnly := range []bool{false, true} {
		f := newFakeOLM(t)
		installedCSVFixture{csvName: "rhods-operator.3.6.0", version: "3.6.0", listOnly: listOnly}.apply(f)
		f.onSubscribe = olmInstalls("rhods-operator.3.6.0")
		result, err := Reinstall(f.client(context.Background()), "nightly", testNightlyImage, "")
		if err != nil || !result.Success {
			t.Fatalf("listOnly=%v: %+v, %v", listOnly, result, err)
		}
	}
}

// Update has no downgrade confirmation, so it blocks both a downgrade and
// an unreadable version, on every lookup path, without changing anything.
func TestUpdateDowngradeGuard_EveryCSVLookupPath(t *testing.T) {
	cases := append([]installedCSVFixture{}, downgradeFixtures...)
	cases = append(cases,
		installedCSVFixture{name: "unknown version by name", csvName: "rhods-operator.custom-build"},
		installedCSVFixture{name: "unknown version in list", csvName: "rhods-operator.custom-build", listOnly: true},
	)
	for _, fx := range cases {
		t.Run(fx.name, func(t *testing.T) {
			f := newFakeOLM(t)
			fx.apply(f)
			f.onSubscribe = olmInstalls("rhods-operator.3.6.0")
			result, err := UpdateStream(f.client(context.Background()), testNightlyImage, func(UpdateStepEvent) {})
			want := "Downgrade detected"
			if fx.version == "" && !strings.Contains(fx.csvName, "3.7.0") {
				want = "Cannot rule out a downgrade"
			}
			if err != nil || result.Success || result.ErrorCode != "validation" || !strings.Contains(result.Message, want) {
				t.Fatalf("want %q, got %+v, %v", want, result, err)
			}
			assertOnlyVerificationWrites(t, f)
		})
	}
}

// The dry run reports the same verdict as the real update.
func TestUpdateDryRunDowngradeGuard_EveryCSVLookupPath(t *testing.T) {
	old := lookupTargetBundle
	t.Cleanup(func() { lookupTargetBundle = old })
	lookupTargetBundle = func(*Client, string) (string, error) { return "rhods-operator.3.6.0", nil }
	for _, fx := range append(downgradeFixtures, installedCSVFixture{name: "unknown", csvName: "rhods-operator.custom-build"}) {
		t.Run(fx.name, func(t *testing.T) {
			f := newFakeOLM(t)
			fx.apply(f)
			result, err := Update(f.client(context.Background()), testNightlyImage, true)
			if err != nil || result.Success || result.ErrorCode != "validation" {
				t.Fatalf("dry run did not block: %+v, %v", result, err)
			}
			if len(f.writes()) != 0 {
				t.Fatalf("dry run wrote: %v", f.writes())
			}
		})
	}
}

func TestCompareWithInstalled(t *testing.T) {
	for _, tc := range []struct {
		installed types.CSVInfo
		target    string
		want      versionVerdict
	}{
		{types.CSVInfo{Phase: "Not Found"}, "rhods-operator.3.6.0", verdictNotInstalled},
		{types.CSVInfo{Name: "rhods-operator.3.6.0"}, "rhods-operator.3.5.2", verdictOlder},
		{types.CSVInfo{Name: "rhods-operator.3.6.0"}, "rhods-operator.v3.6.0", verdictNotOlder},
		{types.CSVInfo{Name: "rhods-operator.x", Version: "3.6.0"}, "rhods-operator.3.6.1", verdictNotOlder},
		{types.CSVInfo{Name: "rhods-operator.x", Version: "3.6.0"}, "rhods-operator.3.5.0", verdictOlder},
		// spec.version wins over a name that disagrees.
		{types.CSVInfo{Name: "rhods-operator.3.5.0", Version: "3.6.0"}, "rhods-operator.3.5.2", verdictOlder},
		{types.CSVInfo{Name: "rhods-operator.x"}, "rhods-operator.3.6.0", verdictUnknown},
		{types.CSVInfo{Name: "rhods-operator.3.6.0"}, "", verdictUnknown},
		{types.CSVInfo{Name: "rhods-operator.3.6.0"}, "something-else", verdictUnknown},
	} {
		if got, reason := compareWithInstalled(tc.installed, tc.target); got != tc.want || (got == verdictUnknown) != (reason != "") {
			t.Errorf("%+v -> %q: got %v (%q), want %v", tc.installed, tc.target, got, reason, tc.want)
		}
	}
}
