package cluster

import (
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

const (
	legacyNSPath          = "/api/v1/namespaces/minio"
	legacyS3DeployPath    = "/apis/apps/v1/namespaces/minio/deployments/seaweedfs"
	legacyMinIODeployPath = "/apis/apps/v1/namespaces/minio/deployments/minio"
	legacyS3PVCPath       = "/api/v1/namespaces/minio/persistentvolumeclaims/seaweedfs-pvc"
	legacyMinIOPVCPath    = "/api/v1/namespaces/minio/persistentvolumeclaims/minio-pvc"
	legacySecretPath      = "/api/v1/namespaces/minio/secrets/minio-secret"
	legacySvcPath         = "/api/v1/namespaces/minio/services/minio-service"
	legacyUIRoutePath     = "/apis/route.openshift.io/v1/namespaces/minio/routes/minio-ui"
	legacyAPIRoutePath    = "/apis/route.openshift.io/v1/namespaces/minio/routes/minio-api"
	legacyS3NPPath        = "/apis/networking.k8s.io/v1/namespaces/minio/networkpolicies/seaweedfs-ingress"
	legacyMinIONPPath     = "/apis/networking.k8s.io/v1/namespaces/minio/networkpolicies/minio-ingress"
)

// putLegacySeaweedFS puts what 2.1 to 2.3 left in namespace minio (seen
// live): SeaweedFS behind the MinIO-era names, its PVC, and MinIO's kept
// PVC. Most objects carry the tool label; minio-pvc was created by an
// earlier version's server-side apply and carries no label.
func putLegacySeaweedFS(f *resourceFake) {
	f.putJSON(legacyNSPath, `{"metadata":{"labels":`+toolLabelJSON+`,"creationTimestamp":"`+created+`"},"status":{"phase":"Active"}}`)
	f.putJSON(legacyS3DeployPath, `{"metadata":{"labels":`+toolLabelJSON+`,"generation":1},"spec":{"template":{"spec":{"containers":[{"name":"seaweedfs","image":"ghcr.io/chrislusf/seaweedfs@sha256:4e61d15f"}]}}},"status":{"observedGeneration":1,"replicas":1,"readyReplicas":1,"updatedReplicas":1}}`)
	f.putJSON(legacyS3NPPath, `{"metadata":{"labels":`+toolLabelJSON+`}}`)
	f.putJSON(legacySecretPath, `{"metadata":{"labels":`+toolLabelJSON+`},"data":{"minio_root_user":"`+exampleAuth("minio-abc")+`","minio_root_password":"`+exampleAuth("old-pass")+`"}}`)
	f.putJSON(legacySvcPath, `{"metadata":{"labels":`+toolLabelJSON+`},"spec":{"selector":{"app":"seaweedfs"},"clusterIP":"172.30.10.20","ports":[{"name":"api","port":9000,"targetPort":8333},{"name":"ui","port":9090,"targetPort":23646}]}}`)
	f.putJSON(legacyUIRoutePath, `{"metadata":{"labels":`+toolLabelJSON+`},"spec":{"host":"minio-ui-minio.apps.example.com","to":{"kind":"Service","name":"minio-service"},"port":{"targetPort":"ui"}}}`)
	f.putJSON(legacyS3PVCPath, `{"metadata":{"labels":`+toolLabelJSON+`},"status":{"capacity":{"storage":"20Gi"}}}`)
	f.putJSON(legacyMinIOPVCPath, `{"metadata":{"creationTimestamp":"`+created+`","managedFields":`+managedFieldsJSON(envkit.ToolFieldManager, "Apply")+`},"spec":{"resources":{"requests":{"storage":"10Gi"}}}}`)
}

// putLegacyMinIO puts the MinIO of a release before 2.1: no labels, every
// object created by server-side apply, and a minio-api Route.
func putLegacyMinIO(f *resourceFake) {
	byApply := `{"creationTimestamp":"` + created + `","managedFields":` + managedFieldsJSON(envkit.ToolFieldManager, "Apply")
	f.putJSON(legacyNSPath, `{"metadata":{"creationTimestamp":"`+created+`","managedFields":`+managedFieldsJSON(envkit.LegacyPostManager, "Update")+`},"status":{"phase":"Active"}}`)
	f.putJSON(legacyMinIODeployPath, `{"metadata":`+byApply+`,"generation":1},"spec":{"template":{"spec":{"containers":[{"name":"minio","image":"quay.io/minio/minio:latest"}]}}},"status":{"observedGeneration":1,"replicas":1,"readyReplicas":1,"updatedReplicas":1}}`)
	f.putJSON(legacySecretPath, `{"metadata":`+byApply+`},"data":{"minio_root_user":"`+exampleAuth("minio")+`","minio_root_password":"`+exampleAuth("minio123")+`"}}`)
	f.putJSON(legacySvcPath, `{"metadata":`+byApply+`},"spec":{"selector":{"app":"minio"},"clusterIP":"172.30.10.20","ports":[{"name":"api","port":9000,"targetPort":9000}]}}`)
	f.putJSON(legacyAPIRoutePath, `{"metadata":`+byApply+`},"spec":{"host":"minio-api-minio.apps.example.com"}}`)
	f.putJSON(legacyMinIOPVCPath, `{"metadata":`+byApply+`},"status":{"capacity":{"storage":"10Gi"}}}`)
}

func TestReadLegacyStorage_NoneWithoutToolObjects(t *testing.T) {
	t.Run("no namespace", func(t *testing.T) {
		_, c := newResourceFake(t)
		if l, err := readLegacyStorage(c); l != nil || err != nil {
			t.Fatalf("legacy = %+v, %v", l, err)
		}
	})
	t.Run("namespace with someone else's objects", func(t *testing.T) {
		f, c := newResourceFake(t)
		f.putJSON(legacyNSPath, `{"metadata":{},"status":{"phase":"Active"}}`)
		f.putJSON(legacyMinIODeployPath, foreignJSON(""))
		f.putJSON(legacySecretPath, foreignJSON(`,"data":{"k":"dXNlcg=="}`))
		if l, err := readLegacyStorage(c); l != nil || err != nil {
			t.Fatalf("legacy = %+v, %v", l, err)
		}
	})
}

func TestReadLegacyStorage_SeaweedFSOfEarlierVersions(t *testing.T) {
	f, c := newResourceFake(t)
	putLegacySeaweedFS(f)
	l, err := readLegacyStorage(c)
	if err != nil || l == nil {
		t.Fatalf("legacy = %+v, %v", l, err)
	}
	var objects []string
	for _, o := range l.objects {
		objects = append(objects, o.kind+" "+o.name)
	}
	if got := strings.Join(objects, ", "); got != "Deployment seaweedfs, NetworkPolicy seaweedfs-ingress, Route minio-ui, Service minio-service, Secret minio-secret, PersistentVolumeClaim seaweedfs-pvc, PersistentVolumeClaim minio-pvc" {
		t.Errorf("objects = %s", got)
	}
	if l.server != "SeaweedFS" || !l.ready {
		t.Errorf("server = %q ready=%v", l.server, l.ready)
	}
	// MinIO's volume is not served by SeaweedFS: it cannot be copied.
	if got := strings.Join(l.uncopiable(), ","); got != "minio-pvc" {
		t.Errorf("uncopiable = %q", got)
	}
	if !l.address.usedBy("team", "minio-service.minio.svc:9000") || !l.address.usedBy("team", "172.30.10.20") || !l.address.usedBy("team", "minio-ui-minio.apps.example.com") {
		t.Errorf("address = %+v", l.address)
	}
}

func TestReadLegacyStorage_MinIOOfEarlierVersions(t *testing.T) {
	f, c := newResourceFake(t)
	putLegacyMinIO(f)
	l, err := readLegacyStorage(c)
	if err != nil || l == nil || l.server != "MinIO" || !l.ready {
		t.Fatalf("legacy = %+v, %v", l, err)
	}
	// MinIO serves its own volume: a copy through its Service reads it.
	if got := l.uncopiable(); len(got) != 0 {
		t.Errorf("uncopiable = %v", got)
	}
	if !l.address.usedBy("team", "https://minio-api-minio.apps.example.com") {
		t.Errorf("the minio-api Route host must count: %+v", l.address)
	}
}

// Detection fails closed: an object that cannot be read could be the tool's.
func TestReadLegacyStorage_ReadErrorIsReturned(t *testing.T) {
	f, c := newResourceFake(t)
	putLegacySeaweedFS(f)
	f.fail["GET "+legacySecretPath] = 500
	if l, err := readLegacyStorage(c); err == nil {
		t.Fatalf("legacy = %+v, want the read error", l)
	}
}

func TestGetResourcesStatus_ReportsEarlierStorage(t *testing.T) {
	f, c := newResourceFake(t)
	putLegacySeaweedFS(f)
	putDSProject(f, "juntao-test")
	putDSPA(f, "juntao-test", "dspa", `{}`, envkit.LegacyPostManager, "Update", legacyS3Host(), legacyDSPASecretName, "", "")
	st, err := GetResourcesStatus(c)
	if err != nil {
		t.Fatal(err)
	}
	// MigrationPending says it; SetupBlockedReason stays about ownership.
	if st.MinIO.Deployed || !st.MinIO.MigrationPending || st.MinIO.SetupBlockedReason != "" {
		t.Errorf("storage = %+v", st.MinIO)
	}
	l := st.LegacyStorage
	if l == nil || l.Namespace != "minio" || l.Server != "SeaweedFS" || !l.Ready || l.Endpoint != "minio-service.minio.svc:9000" {
		t.Fatalf("legacy = %+v", l)
	}
	if strings.Join(l.UsedBy, ",") != "juntao-test/dspa" || strings.Join(l.UncopiedPVCs, ",") != "minio-pvc" || len(l.Objects) != 7 {
		t.Errorf("legacy = %+v", l)
	}
	if len(l.DataPVCs) != 2 || l.DataPVCs[0].Name != "seaweedfs-pvc" || l.DataPVCs[0].Size != "20Gi" || l.DataPVCs[1].Size != "10Gi" {
		t.Errorf("data PVCs = %+v", l.DataPVCs)
	}
	// The pipeline server on it blocks teardown.
	if !strings.Contains(st.MinIO.TeardownBlockedReason, "juntao-test/dspa") {
		t.Errorf("teardown blocked = %q", st.MinIO.TeardownBlockedReason)
	}
	// It is still listed as the tool's pipeline server.
	if len(st.PipelineServers) != 1 || st.PipelineServers[0].Name != "dspa" {
		t.Errorf("pipeline servers = %+v", st.PipelineServers)
	}
}

func TestGetResourcesStatus_EarlierStorageUnreadableIsAWarning(t *testing.T) {
	f, c := newResourceFake(t)
	putReadyManagedS3(f)
	f.putJSON(legacyNSPath, `{"metadata":{"labels":`+toolLabelJSON+`},"status":{"phase":"Active"}}`)
	f.fail["GET "+legacySecretPath] = 403
	st, err := GetResourcesStatus(c)
	if err != nil {
		t.Fatal(err)
	}
	if st.LegacyStorage != nil || !st.MinIO.Ready || !strings.Contains(st.MinIO.Warning, "Cannot check namespace minio") {
		t.Errorf("storage = %+v, legacy = %+v", st.MinIO, st.LegacyStorage)
	}
}

// Teardown removes what an earlier version left in namespace minio too,
// with the same ownership rules, and keeps both namespaces.
func TestTeardownS3Storage_RemovesEarlierStorage(t *testing.T) {
	fastS3Timings(t)
	f, c := newResourceFake(t)
	putDeployedS3(f)
	putLegacySeaweedFS(f)
	f.putJSON(legacyAPIRoutePath, foreignJSON(`,"spec":{"host":"someone-else.apps.example.com"}`))
	resp, _ := TeardownS3Storage(c, types.S3TeardownRequest{})
	if !resp.Success {
		t.Fatalf("teardown = %+v", resp)
	}
	for _, path := range []string{legacyS3DeployPath, legacyS3NPPath, legacySecretPath, legacySvcPath, legacyUIRoutePath, legacyS3PVCPath, legacyMinIOPVCPath, s3DeployPath, s3PVCPath} {
		if f.has(path) {
			t.Errorf("%s was not deleted", path)
		}
	}
	assertForeignUntouched(t, f, legacyAPIRoutePath, "spec", "someone-else")
	if !f.has(legacyNSPath) || !f.has(s3NSPath) || !strings.Contains(resp.Message, "Namespaces seaweedfs and minio were kept") {
		t.Errorf("teardown = %+v", resp)
	}
}

// A pipeline server on the earlier storage blocks teardown, in any address
// form, and nothing is deleted.
func TestTeardownS3Storage_BlockedByPipelineServersOnEarlierStorage(t *testing.T) {
	for _, host := range []string{legacyS3Host(), "http://minio-service.minio.svc.cluster.local:9000", "172.30.10.20:9000", "https://minio-ui-minio.apps.example.com"} {
		t.Run(host, func(t *testing.T) {
			fastS3Timings(t)
			f, c := newResourceFake(t)
			putLegacySeaweedFS(f)
			putDSPA(f, "team", "dspa", `{}`, "Mozilla", "Update", host, "s", "", "")
			resp, _ := TeardownS3Storage(c, types.S3TeardownRequest{})
			if resp.Success || resp.ErrorCode != "prerequisites" || !strings.Contains(resp.Message, "team/dspa") {
				t.Fatalf("teardown = %+v", resp)
			}
			if m := f.mutations(); len(m) != 0 {
				t.Errorf("a blocked teardown wrote %v", m)
			}
		})
	}
}

func TestTeardownS3Storage_EarlierStorageAddressUnreadableFailsClosed(t *testing.T) {
	fastS3Timings(t)
	f, c := newResourceFake(t)
	putLegacySeaweedFS(f)
	putDSPA(f, "team", "dspa", `{}`, "Mozilla", "Update", "s3.amazonaws.com", "s", "", "")
	f.fail["GET "+legacyAPIRoutePath] = 500
	resp, _ := TeardownS3Storage(c, types.S3TeardownRequest{})
	if resp.Success || !strings.Contains(resp.Message, "nothing was deleted") {
		t.Fatalf("teardown = %+v", resp)
	}
	if m := f.mutations(); len(m) != 0 {
		t.Errorf("teardown wrote %v", m)
	}
}
