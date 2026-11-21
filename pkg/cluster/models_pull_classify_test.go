package cluster

import (
	"encoding/json"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/serving"
)

// A mirrored pull lists every source; live on ROSA HCP (2026-11-18) a node
// that had joined seconds before got "manifest unknown" from
// registry.redhat.io (the image is only in the mirror) and "unauthorized"
// from quay.io/rhoai (the node had no pull secret yet). That is a pull
// failure to wait on, not an image that is not published.
func TestManifestUnknown_MirrorRefusingCredentialsIsNotMissing(t *testing.T) {
	live := `unable to pull image or OCI artifact: pull image err: initializing source docker://registry.redhat.io/rhoai/odh-kserve-storage-initializer-rhel9@sha256:a214f46cee37ab4cc1b1805435188a4d02ec3696d3a22887db3ae9e9ea461c48: (Mirrors also failed: [quay.io/rhoai/odh-kserve-storage-initializer-rhel9@sha256:a214f46cee37ab4cc1b1805435188a4d02ec3696d3a22887db3ae9e9ea461c48: reading manifest sha256:a214f46cee37ab4cc1b1805435188a4d02ec3696d3a22887db3ae9e9ea461c48 in quay.io/rhoai/odh-kserve-storage-initializer-rhel9: unauthorized: access to the requested resource is not authorized]): registry.redhat.io/rhoai/odh-kserve-storage-initializer-rhel9@sha256:a214f46cee37ab4cc1b1805435188a4d02ec3696d3a22887db3ae9e9ea461c48: reading manifest sha256:a214f46cee37ab4cc1b1805435188a4d02ec3696d3a22887db3ae9e9ea461c48 in registry.redhat.io/rhoai/odh-kserve-storage-initializer-rhel9: manifest unknown`
	for _, c := range []struct {
		name, msg string
		want      bool
	}{
		{"mirror refuses credentials", live, false},
		{"every source lacks it", `initializing source docker://registry.redhat.io/rhaiis/vllm-cpu@sha256:abc: (Mirrors also failed: [quay.io/rhoai/vllm-cpu@sha256:abc: reading manifest sha256:abc in quay.io/rhoai/vllm-cpu: manifest unknown]): registry.redhat.io/rhaiis/vllm-cpu@sha256:abc: reading manifest sha256:abc in registry.redhat.io/rhaiis/vllm-cpu: manifest unknown`, true},
		{"no mirror, missing", `reading manifest sha256:abc in registry.redhat.io/x: manifest unknown`, true},
		{"denied", `manifest unknown; quay.io/x: denied: requested access to the resource is denied`, false},
		{"credentials only", `unauthorized: authentication required`, false},
	} {
		if got := serving.ManifestUnknown(c.msg); got != c.want {
			t.Errorf("%s: manifestUnknown = %v, want %v", c.name, got, c.want)
		}
	}
}

// The setup's pod reading takes the mirror case as a pull failure on its
// node (so the young-node wait applies), not as a missing image.
func TestDescribeModelPod_MirrorRefusalWaitsOnTheNode(t *testing.T) {
	live := `pull image err: initializing source docker://registry.redhat.io/rhoai/x@sha256:a: (Mirrors also failed: [quay.io/rhoai/x@sha256:a: reading manifest sha256:a in quay.io/rhoai/x: unauthorized: access to the requested resource is not authorized]): registry.redhat.io/rhoai/x@sha256:a: reading manifest sha256:a in registry.redhat.io/rhoai/x: manifest unknown`
	raw, _ := json.Marshal(map[string]any{
		"spec": map[string]any{"nodeName": "new-node"},
		"status": map[string]any{"initContainerStatuses": []any{map[string]any{
			"name": "storage-initializer", "state": map[string]any{"waiting": map[string]any{"reason": "ErrImagePull", "message": live}},
		}}},
	})
	var p serving.ModelPod
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	got := serving.DescribeModelPod(p, "")
	if got.ImageMissing || got.Problem != "" {
		t.Fatalf("taken as a missing image or a problem: %+v", got)
	}
	if got.PullFailure == "" || got.Node != "new-node" {
		t.Fatalf("not a pull failure on its node: %+v", got)
	}
}
