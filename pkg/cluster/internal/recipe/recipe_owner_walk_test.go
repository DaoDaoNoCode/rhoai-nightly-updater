package recipe

import (
	"fmt"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
)

// meta is an object with the given owner references, each "<kind>/<uid>"
// (the name is the UID).
func meta(uid string, refs ...string) kube.ObjectMeta {
	m := kube.ObjectMeta{UID: uid}
	for _, r := range refs {
		kind, ref, _ := strings.Cut(r, "/")
		m.OwnerReferences = append(m.OwnerReferences, struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
			UID  string `json:"uid"`
		}{Kind: kind, Name: ref, UID: ref})
	}
	return m
}

// A crafted fan-out (every pod names every ReplicaSet, every ReplicaSet
// every Deployment) costs one evaluation per owner and depth, not one per
// path: the walk keeps the answer for an owner at a depth.
func TestOwnerWalk_FanOutIsEvaluatedOncePerOwnerAndDepth(t *testing.T) {
	const n = 200
	links := map[string]ownerLink{}
	var deps, rss []string
	for i := range n {
		d := fmt.Sprintf("dep-%d", i)
		links[d] = ownerLink{kind: "Deployment", meta: meta(d, "OGXServer/ogx")}
		deps = append(deps, "Deployment/"+d)
	}
	for i := range n {
		r := fmt.Sprintf("rs-%d", i)
		links[r] = ownerLink{kind: "ReplicaSet", meta: meta(r, deps...)}
		rss = append(rss, "ReplicaSet/"+r)
	}
	w := newOwnerWalk(map[string]bool{ownerKey("OGXServer", "ogx"): true}, links)
	for i := range n {
		if !w.owned(meta(fmt.Sprintf("pod-%d", i), rss...)) {
			t.Fatalf("pod %d is not owned", i)
		}
	}
	if len(w.memo) != 2*n {
		t.Fatalf("evaluated %d owners at a depth, want %d", len(w.memo), 2*n)
	}
}
