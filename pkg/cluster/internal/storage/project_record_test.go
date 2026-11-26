package storage

import (
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
)

// A pipeline server recorded as set up on its project's own storage never
// counts as one of the shared storage's, even when a project editor
// pointed its host at the shared storage: Secure S3 storage, Migrate and
// the keys' status (keyServers, PipelineRecord.Owns) leave it out, so the
// shared storage's key is never written into that project.
func TestProjectRecordedServerGetsNoSharedKey(t *testing.T) {
	meta := kube.ObjectMeta{Namespace: "proj", Name: DSPAName, UID: "uid-dspa", Labels: map[string]string{kube.ManagedByLabelKey: kube.ManagedByLabelValue}}
	moved := DSPA{Meta: meta, Host: S3Host(), Scheme: "http"}
	ep := S3Endpoints{Address: Address{Service: s3Service, Namespace: S3Namespace}}
	project := PipelineRecord{"proj/" + DSPAName: {UID: "uid-dspa", Storage: RecordedOnProjectStorage}}
	if project.Owns(meta) || !project.OwnsOnProjectStorage(meta) || !project.OwnsAny(meta) {
		t.Fatal("a project-storage record is trusted as the shared storage's")
	}
	if recorded, kept := keyServers(ep, []DSPA{moved}, project); len(recorded) != 0 || len(kept) != 1 {
		t.Fatalf("recorded %v kept %v", recorded, kept)
	}
	shared := PipelineRecord{"proj/" + DSPAName: {UID: "uid-dspa"}}
	if recorded, _ := keyServers(ep, []DSPA{moved}, shared); len(recorded) != 1 || shared.OwnsOnProjectStorage(meta) {
		t.Fatalf("a shared-storage record is not trusted: %v", recorded)
	}
}
