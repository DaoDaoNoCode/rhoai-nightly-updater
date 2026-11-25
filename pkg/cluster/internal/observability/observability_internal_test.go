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
		{"a stack Secret made after", kube.ObjectMeta{Name: "x", CreationTimestamp: after, Annotations: cert}, true, stackSecret},
		{"made before the record, though not listed", kube.ObjectMeta{Name: "x", CreationTimestamp: "2026-11-25T04:00:00Z", Annotations: cert}, false, stackSecret},
		{"listed before", kube.ObjectMeta{Name: "before", CreationTimestamp: after, Annotations: cert}, false, stackSecret},
		{"owned", kube.ObjectMeta{Name: "x", CreationTimestamp: after, Annotations: cert, OwnerReferences: []struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
			UID  string `json:"uid"`
		}{{Kind: "Certificate"}}}, false, stackSecret},
		{"no stack evidence", kube.ObjectMeta{Name: "x", CreationTimestamp: after}, false, stackSecret},
		{"the web CA", kube.ObjectMeta{Name: webCASecret, CreationTimestamp: after}, true, stackSecret},
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
