package features

import (
	"net/http"
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

// mustBeTest panics unless the program is a test binary.
func mustBeTest(what string) {
	if !testing.Testing() {
		panic(what + " is only for tests")
	}
}
