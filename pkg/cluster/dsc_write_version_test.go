package cluster

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The DataScienceCluster writes of the Components page and Diagnostics'
// disable-component fix are bound to the DSC their checks were made on:
// the JSON Patch tests its uid and resourceVersion. When the DSC moved
// meanwhile (the operator's status writes move the resourceVersion too),
// it is read again and every check runs again before the next patch.

const dscFixtureUID, dscFixtureRV = "ba54ddbf-ad4a-4257-9ab4-977eb541dc1c", "166624435"

// dscRemoveRay are the two ways to set ray to Removed: the reviewed
// toggle and Diagnostics' fix run the same plan and write.
var dscRemoveRay = map[string]func(t *testing.T, c *Client) *types.OperationResponse{
	"reviewed toggle": func(t *testing.T, c *Client) *types.OperationResponse {
		res, err := reviewedToggle(t, c, types.DSCToggleRequest{Name: "default-dsc", Path: "ray", From: "Managed", To: "Removed"})
		if err != nil {
			t.Fatal(err)
		}
		return res
	},
	"disable-component fix": func(t *testing.T, c *Client) *types.OperationResponse {
		res, err := ApplyFix(c, "disable-component:ray")
		if err != nil {
			t.Fatal(err)
		}
		return res
	},
}

// onRealPatch runs change (under the fake's lock) before the real
// (not dry-run) patches of the DSC: before the first n, or every one with
// n < 0.
func onRealPatch(f *resourceFake, n int, change func(dsc map[string]any)) {
	seen := 0
	f.beforeRequest = func(r *http.Request) {
		if r.Method != http.MethodPatch || r.URL.Path != dscV3Path || r.URL.Query().Get("dryRun") != "" {
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		seen++
		if n >= 0 && seen > n {
			return
		}
		k, _ := parseFakePath(dscV3Path)
		change(f.objects[k])
	}
}

// statusWrite is the operator writing the DSC's status: the
// resourceVersion moves, spec does not.
func statusWrite(f *resourceFake) func(dsc map[string]any) {
	return func(dsc map[string]any) {
		ensureMap(dsc, "status")["observedGeneration"] = 99
		f.bumpRV(ensureMap(dsc, "metadata"))
	}
}

func rayState(f *resourceFake) string {
	s, _ := lookupState(comps(f.get(dscV3Path)), []string{"ray"})
	return s
}

func TestDSCWritesTestTheCheckedUIDAndResourceVersion(t *testing.T) {
	for name, change := range dscRemoveRay {
		t.Run(name, func(t *testing.T) {
			f, c := managerWorld(t, nil)
			if res := change(t, c); !res.Success {
				t.Fatalf("result = %+v", res)
			}
			dry, real := patchesTo(f)
			if len(dry) != 1 || len(real) != 1 {
				t.Fatalf("patches: dry %d, real %d", len(dry), len(real))
			}
			for _, call := range append(dry, real...) {
				ops := decodeOps(t, call.Body)
				if len(ops) < 3 || ops[0]["op"] != "test" || ops[0]["path"] != "/metadata/uid" || ops[0]["value"] != dscFixtureUID ||
					ops[1]["op"] != "test" || ops[1]["path"] != "/metadata/resourceVersion" || ops[1]["value"] != dscFixtureRV {
					t.Fatalf("the patch is not bound to the DSC that was checked: %s", call.Body)
				}
			}
			if rayState(f) != "Removed" {
				t.Fatalf("ray = %q", rayState(f))
			}
		})
	}
}

// The DSC was deleted and created again (same name, same spec) after the
// checks: the change is not applied to a DSC nobody checked.
func TestDSCWriteToARecreatedDSCChangesNothing(t *testing.T) {
	for name, change := range dscRemoveRay {
		t.Run(name, func(t *testing.T) {
			f, c := managerWorld(t, nil)
			done := false
			f.beforeRequest = func(r *http.Request) {
				if r.Method != http.MethodPatch {
					return
				}
				f.mu.Lock()
				defer f.mu.Unlock()
				if done {
					return
				}
				done = true
				k, _ := parseFakePath(dscV3Path)
				meta := ensureMap(f.objects[k], "metadata")
				meta["uid"], meta["resourceVersion"] = "recreated-uid", "200000000"
			}
			res := change(t, c)
			if res.Success || res.ErrorCode != "conflict" || !strings.Contains(res.Message, "Nothing was changed") {
				t.Fatalf("result = %+v", res)
			}
			if _, real := patchesTo(f); len(real) != 0 {
				t.Fatalf("patched the recreated DSC: %d real patches", len(real))
			}
			if rayState(f) != "Managed" {
				t.Fatalf("ray = %q", rayState(f))
			}
		})
	}
}

// The operator wrote the DSC's status between the checks and the patch:
// the patch fails its resourceVersion test, the DSC is read again, every
// check runs again on it, and the second patch carries the new version.
func TestDSCWriteChecksAgainAfterAStatusWrite(t *testing.T) {
	for name, change := range dscRemoveRay {
		t.Run(name, func(t *testing.T) {
			f, c := managerWorld(t, nil)
			onRealPatch(f, 1, statusWrite(f))
			if res := change(t, c); !res.Success {
				t.Fatalf("result = %+v", res)
			}
			_, real := patchesTo(f)
			if len(real) != 2 {
				t.Fatalf("real patches = %d, want 2 (one refused, one after the checks ran again)", len(real))
			}
			if ops := decodeOps(t, real[1].Body); ops[1]["value"] != "166624436" {
				t.Fatalf("the retry is not bound to the DSC read again: %s", real[1].Body)
			}
			// The removal preconditions were read again between the two
			// patches.
			first := slices.Index(f.requests, "PATCH "+dscV3Path)
			between := f.requests[first+1:]
			second := slices.Index(between, "PATCH "+dscV3Path)
			if !slices.Contains(between[:second], "GET "+rhodsOperator) || !slices.Contains(between[:second], "GET "+dscV3List) {
				t.Fatalf("no fresh read of the DSC and the preconditions before the retry: %v", between[:second])
			}
			if rayState(f) != "Removed" {
				t.Fatalf("ray = %q", rayState(f))
			}
		})
	}
}

// A precondition that fails when the checks run again stops the change.
func TestDSCWriteRetryStopsWhenAPreconditionFails(t *testing.T) {
	for name, change := range dscRemoveRay {
		t.Run(name, func(t *testing.T) {
			f, c := managerWorld(t, nil)
			onRealPatch(f, 1, func(dsc map[string]any) {
				statusWrite(f)(dsc)
				k, _ := parseFakePath(rhodsOperator)
				f.objects[k]["status"] = map[string]any{"readyReplicas": 0}
			})
			res := change(t, c)
			if res.Success || res.ErrorCode != "prerequisites" || !strings.Contains(res.Message, "RHOAI operator must be running") || !strings.Contains(res.Message, "Nothing was changed") {
				t.Fatalf("result = %+v", res)
			}
			if _, real := patchesTo(f); len(real) != 1 {
				t.Fatalf("real patches = %d, want only the refused one", len(real))
			}
			if rayState(f) != "Managed" {
				t.Fatalf("ray = %q", rayState(f))
			}
		})
	}
}

// The DSC cannot be read again after a refused patch: nothing was changed,
// and the result says so.
func TestDSCWriteReReadFailureReportsNothingChanged(t *testing.T) {
	for name, change := range dscRemoveRay {
		t.Run(name, func(t *testing.T) {
			f, c := managerWorld(t, nil)
			onRealPatch(f, 1, func(dsc map[string]any) {
				statusWrite(f)(dsc)
				f.fail["GET "+dscV3List] = http.StatusForbidden
			})
			res := change(t, c)
			if res.Success || res.ErrorCode != "forbidden" || !strings.Contains(res.Message, "could not be read again") || !strings.Contains(res.Message, "Nothing was changed") {
				t.Fatalf("result = %+v", res)
			}
			if _, real := patchesTo(f); len(real) != 1 {
				t.Fatalf("real patches = %d", len(real))
			}
			if rayState(f) != "Managed" {
				t.Fatalf("ray = %q", rayState(f))
			}
		})
	}
}

// A 422 while the DSC's uid and resourceVersion did not move is not a
// failed test (a test fails only on a DSC that moved): it is the API
// server's refusal (a validation rule, the operator's webhook), shown as
// it is, without another attempt. Also when the API server's answer is the
// bare message it gives a failed test (apiServerPatchTestFailed).
func TestDSCWriteShowsARejectionOfAnUnchangedDSC(t *testing.T) {
	const invalid = `DataScienceCluster.datasciencecluster.opendatahub.io "default-dsc" is invalid: spec.components.ray: Invalid value: "object": ray cannot be removed while ...`
	for _, tc := range []struct {
		name, key, message string
		dryRun             bool
		wantIn             string
	}{
		{name: "dry run refused", key: "PATCH " + dscV3Path, message: invalid, dryRun: true, wantIn: "The API server refused the change in its dry run: " + invalid},
		{name: "patch refused", key: "PATCH " + dscV3Path, message: invalid, wantIn: "The API server refused the change: " + invalid},
		{name: "bare 422", key: "PATCH " + dscV3Path, message: "the server rejected our request due to an error in our request", wantIn: "the server rejected our request due to an error in our request"},
	} {
		for name, change := range dscRemoveRay {
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				f, c := managerWorld(t, nil)
				if tc.dryRun {
					f.failDryRun[tc.key] = http.StatusUnprocessableEntity
					f.failMessage["DRYRUN "+tc.key] = tc.message
				} else {
					f.fail[tc.key] = http.StatusUnprocessableEntity
					f.failMessage[tc.key] = tc.message
				}
				res := change(t, c)
				if res.Success || res.ErrorCode != "validation" || !strings.Contains(res.Message, tc.wantIn) || !strings.Contains(res.Message, "Nothing was changed") {
					t.Fatalf("result = %+v", res)
				}
				dry, real := patchesTo(f)
				if tc.dryRun && (len(dry) != 1 || len(real) != 0) || !tc.dryRun && (len(dry) != 1 || len(real) != 1) {
					t.Fatalf("retried a rejection: dry %d, real %d", len(dry), len(real))
				}
				if rayState(f) != "Managed" {
					t.Fatalf("ray = %q", rayState(f))
				}
			})
		}
	}
}

// A real refusal (a webhook or validation 422) while the operator keeps
// writing the status: the moved resourceVersion makes it look stale, so it
// is retried within the bound, and the result still quotes the API server's
// refusal instead of hiding it behind "it changed".
func TestDSCWriteKeepsTheRefusalWhenTheStatusMovesMeanwhile(t *testing.T) {
	const invalid = `admission webhook "vdatasciencecluster.opendatahub.io" denied the request: ray cannot be removed while RayClusters exist`
	for name, change := range dscRemoveRay {
		t.Run(name, func(t *testing.T) {
			f, c := managerWorld(t, nil)
			f.fail["PATCH "+dscV3Path] = http.StatusUnprocessableEntity
			f.failMessage["PATCH "+dscV3Path] = invalid
			onRealPatch(f, -1, statusWrite(f))
			res := change(t, c)
			if res.Success || res.ErrorCode != "conflict" || !strings.Contains(res.Message, invalid) || strings.Count(res.Message, "Nothing was changed") != 1 {
				t.Fatalf("result = %+v", res)
			}
			if _, real := patchesTo(f); len(real) != dscWriteAttempts {
				t.Fatalf("real patches = %d, want %d", len(real), dscWriteAttempts)
			}
			if rayState(f) != "Managed" {
				t.Fatalf("ray = %q", rayState(f))
			}
		})
	}
}

// A DSC that changes before every patch: bounded, then a conflict.
func TestDSCWriteGivesUpWhenTheDSCKeepsChanging(t *testing.T) {
	for name, change := range dscRemoveRay {
		t.Run(name, func(t *testing.T) {
			f, c := managerWorld(t, nil)
			onRealPatch(f, -1, statusWrite(f))
			res := change(t, c)
			if res.Success || res.ErrorCode != "conflict" || !strings.Contains(res.Message, "changed each time") || !strings.Contains(res.Message, "Nothing was changed") {
				t.Fatalf("result = %+v", res)
			}
			if _, real := patchesTo(f); len(real) != dscWriteAttempts {
				t.Fatalf("real patches = %d, want %d", len(real), dscWriteAttempts)
			}
			if rayState(f) != "Managed" {
				t.Fatalf("ray = %q", rayState(f))
			}
		})
	}
}

// Without a uid or resourceVersion the write cannot be bound to the DSC
// that was checked: refused before anything is sent (fail closed).
func TestDSCWriteRefusesADSCWithoutAVersion(t *testing.T) {
	for _, field := range []string{"uid", "resourceVersion"} {
		for name, change := range dscRemoveRay {
			t.Run(field+"/"+name, func(t *testing.T) {
				f, c := managerWorld(t, nil)
				k, _ := parseFakePath(dscV3Path)
				delete(ensureMap(f.objects[k], "metadata"), field)
				res := change(t, c)
				if res.Success || !strings.Contains(res.Message, "Nothing was changed") {
					t.Fatalf("result = %+v", res)
				}
				if dry, real := patchesTo(f); len(dry)+len(real) != 0 {
					t.Fatal("patched a DSC the write cannot be bound to")
				}
			})
		}
	}
}

// Fix and soft reset share the write: a status write in between does not
// fail a reviewed plan, which is checked again (token included).
func TestDSCPlannedWritesRetryAfterAStatusWrite(t *testing.T) {
	f, c := managerWorld(t, func(dsc map[string]any) {
		setPath(t, dsc, "spec.components.databaseservice", nil)
	})
	plan, err := PlanDSCChange(c, "fix", "", "", false, false)
	if err != nil || plan.Token == "" {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
	onRealPatch(f, 1, statusWrite(f))
	res, err := FixDSC(c, types.DSCPlanRequest{Name: "default-dsc", UID: plan.UID, Token: plan.Token})
	if err != nil || !res.Success {
		t.Fatalf("fix = %+v, %v", res, err)
	}
	if _, real := patchesTo(f); len(real) != 2 {
		t.Fatalf("real patches = %d", len(real))
	}
	if s, _ := lookupState(comps(f.get(dscV3Path)), []string{"databaseservice"}); s != "Removed" {
		t.Fatalf("databaseservice = %q", s)
	}
}
