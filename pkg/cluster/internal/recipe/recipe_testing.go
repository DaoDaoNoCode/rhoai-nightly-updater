package recipe

import (
	"fmt"
	"testing"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
)

// ProjectLeftovers is projectLeftovers of recipe project name as the first
// check of a clean-up sees it (nothing carried), for tests. The project's
// UID comes from its namespace; a namespace that is not there gives none,
// so a Kueue record that names the project's UID does not match. It
// panics outside a test binary.
func ProjectLeftovers(c *kube.Client, name string) ([]string, error) {
	if !testing.Testing() {
		panic("ProjectLeftovers is only for tests")
	}
	meta, found, err := kube.ReadObjectMeta(c, namespacePath(name))
	if err != nil {
		return nil, fmt.Errorf("read namespace %s: %w", name, err)
	}
	uid := ""
	if found {
		uid = meta.UID
	}
	left, _, err := projectLeftovers(c, name, uid, nil)
	return left, err
}

// SetDSCTimingForTest sets the DataScienceCluster step's create retry, its
// delay, and the wait for conditions with its poll, and returns the
// function that puts the previous values back. It panics outside a test
// binary.
func SetDSCTimingForTest(retry, retryDelay, wait, poll time.Duration) (restore func()) {
	if !testing.Testing() {
		panic("SetDSCTimingForTest is only for tests")
	}
	pr, pd, pw, pp := dscCreateRetry, dscCreateRetryDelay, dscConditionsWait, dscConditionsPoll
	dscCreateRetry, dscCreateRetryDelay, dscConditionsWait, dscConditionsPoll = retry, retryDelay, wait, poll
	return func() { dscCreateRetry, dscCreateRetryDelay, dscConditionsWait, dscConditionsPoll = pr, pd, pw, pp }
}
