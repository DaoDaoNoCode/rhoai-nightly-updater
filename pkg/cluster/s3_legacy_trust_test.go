package cluster

import (
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/storage"
)

// Any user may `oc new-project minio` and put objects there that look like
// the tool's (its label, its field manager). Namespace minio is the tool's
// legacy storage only when the namespace itself shows the tool made it: the
// tool label (2.x set it, and labelled earlier versions' namespaces), or an
// earliest version's plain POST without a ProjectRequest's requester.
func TestReadLegacyStorage_IgnoresANamespaceTheToolDidNotMake(t *testing.T) {
	planted := map[string]string{
		"project request": `{"metadata":{"annotations":{"openshift.io/requester":"mallory"},"managedFields":` + managedFieldsJSON("openshift-apiserver", "Update") + `},"status":{"phase":"Active"}}`,
		"project request with the earliest manager": `{"metadata":{"annotations":{"openshift.io/requester":"mallory"},"managedFields":` + managedFieldsJSON(envkit.LegacyPostManager, "Update") + `},"status":{"phase":"Active"}}`,
		"no marker at all":                          `{"metadata":{},"status":{"phase":"Active"}}`,
	}
	for name, ns := range planted {
		t.Run(name, func(t *testing.T) {
			fastMigrateTimings(t)
			f, c := newResourceFake(t)
			putLegacySeaweedFS(f) // tool-looking objects, with the attacker's keys
			f.putJSON(legacyNSPath, ns)
			if l, err := storage.ReadLegacy(c); l != nil || err != nil {
				t.Fatalf("adopted: %+v, %v", l, err)
			}
			st, err := GetResourcesStatus(c)
			if err != nil {
				t.Fatal(err)
			}
			if st.LegacyStorage != nil || !st.ForeignLegacyNamespace || st.MinIO.MigrationPending {
				t.Fatalf("status: legacy %+v foreign %v", st.LegacyStorage, st.ForeignLegacyNamespace)
			}
			// Migrate storage has nothing to do and writes nothing.
			ok, msg, _, _ := runMigrate(t, c, MigrateOptions{})
			if !ok || !strings.Contains(msg, "Nothing to do") || len(f.mutations()) != 0 {
				t.Fatalf("migrate: %v %s %v", ok, msg, f.mutations())
			}
		})
	}
}

// Genuine installs keep working: 2.x's labelled namespace, and the
// earliest MinIO's (created by a plain POST, no label, no requester).
func TestReadLegacyStorage_TrustsTheToolsOwnNamespace(t *testing.T) {
	for name, put := range map[string]func(*resourceFake){"2.x": putLegacySeaweedFS, "earliest MinIO": putLegacyMinIO} {
		f, c := newResourceFake(t)
		put(f)
		if l, err := storage.ReadLegacy(c); l == nil || err != nil {
			t.Errorf("%s: not detected: %v", name, err)
		}
		st, _ := GetResourcesStatus(c)
		if st == nil || st.ForeignLegacyNamespace {
			t.Errorf("%s: foreign", name)
		}
	}
	// A labelled namespace is trusted even with a requester: only a
	// cluster-admin (or the tool) can set the label.
	f, c := newResourceFake(t)
	putLegacySeaweedFS(f)
	f.putJSON(legacyNSPath, `{"metadata":{"labels":`+toolLabelJSON+`,"annotations":{"openshift.io/requester":"admin"}},"status":{"phase":"Active"}}`)
	if l, err := storage.ReadLegacy(c); l == nil || err != nil {
		t.Errorf("labelled: not detected: %v", err)
	}
}
