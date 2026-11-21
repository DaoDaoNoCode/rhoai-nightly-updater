package cluster

import (
	"os"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/clock/clocktest"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/registry"
)

func TestMain(m *testing.M) {
	// Override timing constants with fast values for tests.
	// Production uses 5-120 second waits; tests use 10-100ms.
	CatalogReadyTimeout = 100 * time.Millisecond
	CatalogPollInterval = 10 * time.Millisecond
	InstallPlanPollTimeout = 100 * time.Millisecond
	InstallPlanPollInterval = 10 * time.Millisecond
	PropagationWait = 10 * time.Millisecond
	RefreshCleanupWait = 10 * time.Millisecond
	SubRetryBackoffs = []time.Duration{10 * time.Millisecond, 10 * time.Millisecond}
	ChannelRetryDelay = 10 * time.Millisecond
	PackageManifestPropagationWait = 10 * time.Millisecond
	OperatorInstallTimeout = 400 * time.Millisecond
	ResolutionFailedGrace = 60 * time.Millisecond
	BundlePullFailureGrace = 60 * time.Millisecond
	CSVFailedGrace = 60 * time.Millisecond
	CSVSucceededSettle = 30 * time.Millisecond
	CSVDeletionTimeout = 50 * time.Millisecond
	CatalogImagePullGrace = 30 * time.Millisecond

	// Every wait runs on a fake clock that moves when the code sleeps, so
	// no test waits in real time (a cache's entries age on their own fake
	// clock: fakeCacheClock).
	kube.SetClockForTest(clocktest.NewAutoFake())

	// Skip real Quay credential verification in tests.
	// Individual tests override this to test rejection behavior.
	registry.SetVerifyQuayCredentialsForTest(func(string) error { return nil })

	os.Exit(m.Run())
}
