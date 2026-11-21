package cluster

import (
	"os"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/clock/clocktest"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/operator"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/registry"
)

func TestMain(m *testing.M) {
	// Override timing constants with fast values for tests.
	// Production uses 5-120 second waits; tests use 10-100ms.
	operator.UseTestTimings()

	// Every wait runs on a fake clock that moves when the code sleeps, so
	// no test waits in real time (a cache's entries age on their own fake
	// clock: fakeCacheClock).
	kube.SetClockForTest(clocktest.NewAutoFake())

	// Skip real Quay credential verification in tests.
	// Individual tests override this to test rejection behavior.
	registry.SetVerifyQuayCredentialsForTest(func(string) error { return nil })

	os.Exit(m.Run())
}
