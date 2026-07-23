package cluster

import (
	"os"
	"testing"
	"time"
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

	// Skip real Quay credential verification in tests.
	// Individual tests override this to test rejection behavior.
	verifyQuayCredentials = func(string) error { return nil }

	os.Exit(m.Run())
}
