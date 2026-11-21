package storage

import "testing"

func TestLegacyRemovalPhases_CoverEveryLegacyObject(t *testing.T) {
	seen := map[legacyObjectName]int{}
	for _, phase := range [][]legacyObjectName{legacyRemovalPhases.beforeServer, legacyRemovalPhases.server, legacyRemovalPhases.afterServer} {
		for _, o := range phase {
			seen[o]++
		}
	}
	for _, o := range legacyStorageObjects {
		if seen[legacyObjectName{o.kind, o.name}] != 1 {
			t.Errorf("%s %s is deleted %d times by Remove old storage", o.kind, o.name, seen[legacyObjectName{o.kind, o.name}])
		}
		delete(seen, legacyObjectName{o.kind, o.name})
	}
	if len(seen) > 0 {
		t.Errorf("Remove old storage deletes objects teardown does not know: %v", seen)
	}
}
