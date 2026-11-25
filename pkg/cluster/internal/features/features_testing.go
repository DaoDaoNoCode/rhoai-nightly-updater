package features

import (
	"net/http"
	"slices"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
)

// Test hooks for the feature setups and the playground. Each panics outside
// a test binary.

// SetGenAIForTest makes the playground's gen-ai calls go to baseURL's
// address with client; restore puts both back.
func SetGenAIForTest(baseURL func(*kube.Client) string, client *http.Client) (restore func()) {
	mustBeTest("SetGenAIForTest")
	prevBase, prevClient := genAIBaseURL, genAIHTTPClient
	genAIBaseURL, genAIHTTPClient = baseURL, client
	return func() { genAIBaseURL, genAIHTTPClient = prevBase, prevClient }
}

// SetGenAIHTTPClientForTest makes client send the gen-ai calls.
func SetGenAIHTTPClientForTest(client *http.Client) (restore func()) {
	mustBeTest("SetGenAIHTTPClientForTest")
	prev := genAIHTTPClient
	genAIHTTPClient = client
	return func() { genAIHTTPClient = prev }
}

// GenAIHTTPClientForTest is the client that sends the gen-ai calls.
func GenAIHTTPClientForTest() *http.Client {
	mustBeTest("GenAIHTTPClientForTest")
	return genAIHTTPClient
}

// ComponentForTest is a DataScienceCluster component a feature setup turns
// on: the feature, the key it is written with and the state it is turned
// on with.
type ComponentForTest struct {
	Feature, Key, On string
}

// ComponentsForTest lists every component the feature setups turn on, by
// each key it may be written with (the v1 key included).
func ComponentsForTest() []ComponentForTest {
	mustBeTest("ComponentsForTest")
	var out []ComponentForTest
	for _, fs := range featureSetups {
		for _, fc := range fs.components {
			keys := fc.Keys
			if fc.V1Key != "" && !slices.Contains(keys, fc.V1Key) {
				keys = append(slices.Clone(keys), fc.V1Key)
			}
			for _, key := range keys {
				out = append(out, ComponentForTest{Feature: fs.id, Key: key, On: fc.OnState()})
			}
		}
	}
	return out
}

// mustBeTest panics unless the program is a test binary.
func mustBeTest(what string) {
	if !testing.Testing() {
		panic(what + " is only for tests")
	}
}
