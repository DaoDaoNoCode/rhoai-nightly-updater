package observability

import (
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
)

// Each guard of what Clean up may delete, failing on its own.
func TestLeftoverGuards(t *testing.T) {
	at := "2026-11-25T05:00:00Z"
	after := "2026-11-25T05:01:00Z"
	cert := map[string]string{certificateNameAnnotation: "c"}
	for _, tc := range []struct {
		name  string
		m     kube.ObjectMeta
		want  bool
		stack func(kube.ObjectMeta) bool
	}{
		{"a stack Secret made after", kube.ObjectMeta{Name: "x", CreationTimestamp: after, Annotations: cert}, true, anyStack},
		{"made before the record, though not listed", kube.ObjectMeta{Name: "x", CreationTimestamp: "2026-11-25T04:00:00Z", Annotations: cert}, false, anyStack},
		{"listed before", kube.ObjectMeta{Name: "before", CreationTimestamp: after, Annotations: cert}, false, anyStack},
		{"owned", kube.ObjectMeta{Name: "x", CreationTimestamp: after, Annotations: cert, OwnerReferences: []struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
			UID  string `json:"uid"`
		}{{Kind: "Certificate"}}}, false, anyStack},
		{"a Loki PVC", kube.ObjectMeta{Name: "wal-data-science-usage-logs-lokistack-ingester-0", CreationTimestamp: after}, true, stackPVC},
		{"another PVC", kube.ObjectMeta{Name: "data-mydb-0", CreationTimestamp: after}, false, stackPVC},
	} {
		got := leftover(tc.m, []string{"before"}, at) && tc.stack(tc.m)
		if got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
	_ = time.Now
}

func anyStack(kube.ObjectMeta) bool { return true }
