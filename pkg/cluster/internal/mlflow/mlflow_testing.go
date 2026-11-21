package mlflow

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// Test hooks for MLflow and its samples. Each sets what a test needs and
// returns the function that puts the previous value back; each panics
// outside a test binary.

// SetResolvePRImageForTest makes resolve the lookup of an MLflow PR's
// image.
func SetResolvePRImageForTest(resolve func(ctx context.Context, pr int) (string, bool, error)) (restore func()) {
	mustBeTest("SetResolvePRImageForTest")
	prev := resolveMLflowPRImage
	resolveMLflowPRImage = resolve
	return func() { resolveMLflowPRImage = prev }
}

// SetDeleteTimingForTest sets the MLflow deletion wait and its poll.
func SetDeleteTimingForTest(timeout, poll time.Duration) (restore func()) {
	mustBeTest("SetDeleteTimingForTest")
	prevTimeout, prevPoll := DeleteTimeout, DeletePoll
	DeleteTimeout, DeletePoll = timeout, poll
	return func() { DeleteTimeout, DeletePoll = prevTimeout, prevPoll }
}

// SetSamplesHTTPClientForTest makes client send the samples' requests.
func SetSamplesHTTPClientForTest(client *http.Client) (restore func()) {
	mustBeTest("SetSamplesHTTPClientForTest")
	prev := SamplesHTTPClient
	SamplesHTTPClient = client
	return func() { SamplesHTTPClient = prev }
}

// mustBeTest panics unless the program is a test binary.
func mustBeTest(what string) {
	if !testing.Testing() {
		panic(what + " is only for tests")
	}
}
