package cluster

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

const mlflowPath = "/apis/" + mlflowAPIGroup + "/mlflows/" + mlflowCRName

func putMLflow(f *resourceFake, labels, manager, op, extraMeta, spec, status string) {
	f.putJSON(mlflowPath, `{"metadata":{"labels":`+labels+`,"creationTimestamp":"`+created+`","managedFields":`+managedFieldsJSON(manager, op)+extraMeta+`},
		"spec":`+spec+`,"status":`+status+`}`)
}

func mlflowImageOf(f *resourceFake) (string, bool) {
	spec, _ := f.get(mlflowPath)["spec"].(map[string]interface{})
	img, _ := spec["image"].(map[string]interface{})
	v, ok := img["image"].(string)
	return v, ok
}

func mlflowAnnotations(f *resourceFake) map[string]interface{} {
	a, _ := f.get(mlflowPath)["metadata"].(map[string]interface{})["annotations"].(map[string]interface{})
	return a
}

func stubPRResolver(t *testing.T, fn func(pr int) (string, bool, error)) {
	t.Helper()
	orig := resolveMLflowPRImage
	resolveMLflowPRImage = func(_ context.Context, pr int) (string, bool, error) { return fn(pr) }
	t.Cleanup(func() { resolveMLflowPRImage = orig })
}

const notAvailable = `{"conditions":[{"type":"Available","status":"False","reason":"DeploymentNotReady","message":"MLflow deployment is not ready"}]}`

func TestGetMLflowStatus_BrowserCreatedCRIsNotManaged(t *testing.T) {
	f, c := newResourceFake(t)
	putMLflow(f, `{}`, "Mozilla", "Update", "", `{"image":{"image":"quay.io/opendatahub/mlflow:odh-stable"}}`,
		`{"conditions":[{"type":"Available","status":"True"}]}`)
	f.putJSON("/api/v1/namespaces/redhat-ods-applications/persistentvolumeclaims/mlflow-pvc", `{"metadata":{}}`)
	st := getMLflowStatus(c)
	if !st.Deployed || !st.Ready || st.ManagedByTool || st.TeardownBlockedReason == "" || len(st.DataPVCs) != 0 || st.PROverride {
		t.Errorf("state = %+v", st)
	}
}

func TestGetMLflowStatus_PROverrideIsReportedWhileBroken(t *testing.T) {
	f, c := newResourceFake(t)
	putMLflow(f, toolLabelJSON, toolFieldManager, "Update", `,"annotations":{"`+mlflowOriginalImageAnnotation+`":""}`,
		`{"image":{"image":"quay.io/opendatahub/mlflow:odh-pr-42@sha256:`+strings.Repeat("a", 64)+`"}}`, notAvailable)
	f.putJSON("/api/v1/namespaces/redhat-ods-applications/pods/mlflow-1", `{"metadata":{"labels":{"app":"mlflow"}},"spec":{"containers":[{"image":"x"}]},
		"status":{"containerStatuses":[{"state":{"waiting":{"reason":"CrashLoopBackOff","message":"back-off restarting"}},"lastState":{"terminated":{"reason":"Error","exitCode":1}}}]}}`)
	st := getMLflowStatus(c)
	if st.Ready || !st.ManagedByTool || !st.PROverride || st.PRNumber != 42 || st.RevertImage != "" {
		t.Errorf("state = %+v", st)
	}
	if st.WaitingReason != "CrashLoopBackOff" || !st.TerminalError || !strings.Contains(st.Message, "exit: Error, code 1") {
		t.Errorf("pod reason not surfaced: %+v", st)
	}
}

func TestGetMLflowStatus_LegacyApplyIsManagedAndOwnedPVCListed(t *testing.T) {
	f, c := newResourceFake(t)
	putMLflow(f, `{}`, toolFieldManager, "Apply", `,"uid":"cr-uid"`, `{}`, `{}`)
	f.putJSON("/api/v1/namespaces/redhat-ods-applications/persistentvolumeclaims/mlflow-pvc", `{"metadata":{"ownerReferences":[{"kind":"MLflow","name":"mlflow","uid":"cr-uid"}]}}`)
	f.putJSON("/api/v1/namespaces/redhat-ods-applications/persistentvolumeclaims/other", `{"metadata":{}}`)
	st := getMLflowStatus(c)
	if !st.ManagedByTool || len(st.DataPVCs) != 1 || st.DataPVCs[0] != "mlflow-pvc" || st.Message != "Provisioning" {
		t.Errorf("state = %+v", st)
	}
}

func TestSetupMLflow(t *testing.T) {
	t.Run("creates a labelled CR without pinning an image", func(t *testing.T) {
		f, c := newResourceFake(t)
		if resp, _ := SetupMLflow(c); !resp.Success {
			t.Fatalf("got %+v", resp)
		}
		cr := f.get(mlflowPath)
		if cr["metadata"].(map[string]interface{})["labels"].(map[string]interface{})[managedByLabelKey] != managedByLabelValue {
			t.Error("CR lacks the ownership label")
		}
		if _, set := mlflowImageOf(f); set {
			t.Error("setup must leave spec.image unset so the operator's RHOAI default applies")
		}
		if hasMutation(f, "PATCH "+mlflowPath) {
			t.Error("setup must create, not force-apply over an existing CR")
		}
	})
	t.Run("CRD missing", func(t *testing.T) {
		f, c := newResourceFake(t)
		f.fail["POST /apis/"+mlflowAPIGroup+"/mlflows"] = 404
		if resp, _ := SetupMLflow(c); resp.Success || resp.ErrorCode != "prerequisites" {
			t.Fatalf("got %+v", resp)
		}
	})
	t.Run("created concurrently", func(t *testing.T) {
		f, c := newResourceFake(t)
		f.fail["POST /apis/"+mlflowAPIGroup+"/mlflows"] = 409
		if resp, _ := SetupMLflow(c); resp.Success || resp.ErrorCode != "conflict" {
			t.Fatalf("got %+v", resp)
		}
	})
}

func TestTeardownMLflow(t *testing.T) {
	fast := func(t *testing.T) {
		oldT, oldP := MLflowDeleteTimeout, MLflowDeletePoll
		MLflowDeleteTimeout, MLflowDeletePoll = 50*time.Millisecond, 10*time.Millisecond
		t.Cleanup(func() { MLflowDeleteTimeout, MLflowDeletePoll = oldT, oldP })
	}
	t.Run("browser-created CR is never deleted", func(t *testing.T) {
		fast(t)
		f, c := newResourceFake(t)
		putMLflow(f, `{}`, "Mozilla", "Update", "", `{}`, `{}`)
		resp, _ := TeardownMLflow(c)
		if resp.Success || resp.ErrorCode != "not_managed" || hasMutation(f, "DELETE") || !f.has(mlflowPath) {
			t.Fatalf("got %+v, mutations %v", resp, f.mutations())
		}
	})
	t.Run("tool CR is deleted and data loss is stated", func(t *testing.T) {
		fast(t)
		f, c := newResourceFake(t)
		putMLflow(f, toolLabelJSON, toolFieldManager, "Update", `,"uid":"cr-uid"`, `{}`, `{}`)
		f.putJSON("/api/v1/namespaces/redhat-ods-applications/persistentvolumeclaims/mlflow-pvc", `{"metadata":{"ownerReferences":[{"uid":"cr-uid"}]}}`)
		resp, _ := TeardownMLflow(c)
		if !resp.Success || f.has(mlflowPath) || !strings.Contains(resp.Message, "mlflow-pvc") {
			t.Fatalf("got %+v", resp)
		}
	})
	t.Run("stuck finalizer is reported", func(t *testing.T) {
		fast(t)
		f, c := newResourceFake(t)
		putMLflow(f, toolLabelJSON, toolFieldManager, "Update", `,"finalizers":["example.com/hold"]`, `{}`, `{}`)
		resp, _ := TeardownMLflow(c)
		if resp.Success || resp.ErrorCode != "in_progress" || !strings.Contains(resp.Message, "example.com/hold") {
			t.Fatalf("got %+v", resp)
		}
		// Re-running reports the same truth without deleting again.
		resp, _ = TeardownMLflow(c)
		if resp.Success || resp.ErrorCode != "in_progress" || !strings.Contains(resp.Message, "example.com/hold") {
			t.Fatalf("re-run got %+v", resp)
		}
	})
	t.Run("already gone", func(t *testing.T) {
		fast(t)
		_, c := newResourceFake(t)
		if resp, _ := TeardownMLflow(c); !resp.Success {
			t.Fatalf("got %+v", resp)
		}
	})
}

func TestDeployAndRevertMLflowPR(t *testing.T) {
	digestA := "quay.io/opendatahub/mlflow:odh-pr-7@sha256:" + strings.Repeat("a", 64)
	digestB := "quay.io/opendatahub/mlflow:odh-pr-7@sha256:" + strings.Repeat("b", 64)
	current := digestA
	stubPRResolver(t, func(pr int) (string, bool, error) {
		if pr == 404 {
			return "", false, nil
		}
		if pr == 500 {
			return "", false, errors.New("Quay returned HTTP 500")
		}
		return current, true, nil
	})

	t.Run("user image is restored on revert", func(t *testing.T) {
		f, c := newResourceFake(t)
		putMLflow(f, `{}`, "Mozilla", "Update", "", `{"image":{"image":"registry.example.com/custom-mlflow:1"}}`, notAvailable)
		current = digestA
		if resp, _ := DeployMLflowPR(c, 7); !resp.Success {
			t.Fatalf("deploy: %+v", resp)
		}
		if img, _ := mlflowImageOf(f); img != digestA {
			t.Fatalf("image = %q, want the digest-pinned PR build", img)
		}
		if mlflowAnnotations(f)[mlflowOriginalImageAnnotation] != "registry.example.com/custom-mlflow:1" {
			t.Fatalf("original not saved: %v", mlflowAnnotations(f))
		}
		// The PR tag moved: a redeploy must change the image so it rolls out.
		current = digestB
		if resp, _ := DeployMLflowPR(c, 7); !resp.Success {
			t.Fatalf("redeploy: %+v", resp)
		}
		if img, _ := mlflowImageOf(f); img != digestB {
			t.Fatalf("redeploy did not pick up the new build: %q", img)
		}
		if mlflowAnnotations(f)[mlflowOriginalImageAnnotation] != "registry.example.com/custom-mlflow:1" {
			t.Fatal("the saved original must survive redeploys")
		}
		// Status reports the override even though MLflow is not ready.
		if st := getMLflowStatus(c); !st.PROverride || st.RevertImage != "registry.example.com/custom-mlflow:1" || st.PRNumber != 7 {
			t.Errorf("status = %+v", st)
		}
		if resp, _ := RevertMLflowImage(c); !resp.Success {
			t.Fatalf("revert: %+v", resp)
		}
		if img, _ := mlflowImageOf(f); img != "registry.example.com/custom-mlflow:1" {
			t.Errorf("revert restored %q", img)
		}
		if _, ok := mlflowAnnotations(f)[mlflowOriginalImageAnnotation]; ok {
			t.Error("revert must clear the saved original")
		}
	})

	t.Run("unset image reverts to the operator default", func(t *testing.T) {
		f, c := newResourceFake(t)
		putMLflow(f, toolLabelJSON, toolFieldManager, "Update", "", `{}`, `{}`)
		current = digestA
		if resp, _ := DeployMLflowPR(c, 7); !resp.Success {
			t.Fatalf("deploy: %+v", resp)
		}
		if resp, _ := RevertMLflowImage(c); !resp.Success || !strings.Contains(resp.Message, "operator default") {
			t.Fatalf("revert: %+v", resp)
		}
		if _, set := mlflowImageOf(f); set {
			t.Error("spec.image.image must be removed so the operator default applies")
		}
	})

	t.Run("PR image from an older version reverts to the operator default", func(t *testing.T) {
		f, c := newResourceFake(t)
		putMLflow(f, `{}`, toolFieldManager, "Apply", "", `{"image":{"image":"quay.io/opendatahub/mlflow:odh-pr-3"}}`, `{}`)
		if st := getMLflowStatus(c); !st.PROverride || st.PRNumber != 3 {
			t.Fatalf("legacy PR image not detected: %+v", st)
		}
		if resp, _ := RevertMLflowImage(c); !resp.Success {
			t.Fatalf("revert: %+v", resp)
		}
		if _, set := mlflowImageOf(f); set {
			t.Error("legacy revert must remove spec.image.image")
		}
	})

	t.Run("same build is a no-op", func(t *testing.T) {
		f, c := newResourceFake(t)
		current = digestA
		putMLflow(f, toolLabelJSON, toolFieldManager, "Update", `,"annotations":{"`+mlflowOriginalImageAnnotation+`":""}`, `{"image":{"image":"`+digestA+`"}}`, `{}`)
		if resp, _ := DeployMLflowPR(c, 7); !resp.Success || hasMutation(f, "PATCH") {
			t.Fatalf("got %+v %v", resp, f.mutations())
		}
	})

	errCases := []struct {
		name  string
		pr    int
		setup func(f *resourceFake)
		code  string
	}{
		{"not deployed", 7, func(*resourceFake) {}, "prerequisites"},
		{"image not found", 404, func(f *resourceFake) { putMLflow(f, toolLabelJSON, toolFieldManager, "Update", "", `{}`, `{}`) }, "validation"},
		{"quay error", 500, func(f *resourceFake) { putMLflow(f, toolLabelJSON, toolFieldManager, "Update", "", `{}`, `{}`) }, "network"},
		{"concurrent change", 7, func(f *resourceFake) {
			putMLflow(f, toolLabelJSON, toolFieldManager, "Update", "", `{}`, `{}`)
			f.onGet = func(k fakeKey, obj map[string]interface{}) {
				if k.plural == "mlflows" {
					obj["metadata"].(map[string]interface{})["resourceVersion"] = "stale"
				}
			}
		}, "conflict"},
	}
	for _, tc := range errCases {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newResourceFake(t)
			current = digestA
			tc.setup(f)
			resp, _ := DeployMLflowPR(c, tc.pr)
			if resp.Success || resp.ErrorCode != tc.code {
				t.Fatalf("got %+v", resp)
			}
			if img, _ := mlflowImageOf(f); img == digestA {
				t.Error("a failed deploy must not change the image")
			}
		})
	}

	t.Run("revert without override", func(t *testing.T) {
		f, c := newResourceFake(t)
		putMLflow(f, toolLabelJSON, toolFieldManager, "Update", "", `{}`, `{}`)
		if resp, _ := RevertMLflowImage(c); resp.Success || resp.ErrorCode != "validation" || hasMutation(f, "PATCH") {
			t.Fatalf("got %+v", resp)
		}
	})
}
