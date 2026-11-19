package cluster

import (
	"encoding/json"
	"strings"
	"testing"
)

const dscPath = "/apis/datasciencecluster.opendatahub.io/v2/datascienceclusters/default-dsc"

// putDSC stores DataScienceCluster default-dsc with these spec.components.
func putDSC(f *resourceFake, components string) {
	f.putJSON(dscPath, `{"spec":{"components":`+components+`}}`)
}

// putManagedDSC stores a DataScienceCluster that turns on both features.
func putManagedDSC(f *resourceFake) {
	putDSC(f, `{"aipipelines":{"managementState":"Managed"},"mlflowoperator":{"managementState":"Managed"}}`)
}

func TestComponentBlock(t *testing.T) {
	dsc := func(obj string) dscRead {
		var m map[string]any
		if err := json.Unmarshal([]byte(obj), &m); err != nil {
			t.Fatal(err)
		}
		return dscRead{Object: m, State: DSCStatePresent, Version: "v2"}
	}
	cases := []struct {
		name string
		read dscRead
		fc   featureComponent
		// want is a part of the reason, or "" for no block.
		want, state, component string
	}{
		{"managed", dsc(`{"metadata":{"name":"default-dsc"},"spec":{"components":{"aipipelines":{"managementState":"Managed"}}}}`), pipelinesComponent, "", "", ""},
		{"removed", dsc(`{"metadata":{"name":"default-dsc"},"spec":{"components":{"aipipelines":{"managementState":"Removed"}}}}`), pipelinesComponent,
			"component aipipelines is Removed in DataScienceCluster default-dsc", "Removed", "aipipelines"},
		// DataScienceCluster v1 names it datasciencepipelines.
		{"v1 key", dsc(`{"metadata":{"name":"dsc"},"spec":{"components":{"datasciencepipelines":{"managementState":"Removed"}}}}`), pipelinesComponent,
			"component datasciencepipelines is Removed", "Removed", "datasciencepipelines"},
		{"v1 key managed", dsc(`{"spec":{"components":{"datasciencepipelines":{"managementState":"Managed"}}}}`), pipelinesComponent, "", "", ""},
		// v3 groups some components; the operator reports their state in status.
		{"state in status", dsc(`{"metadata":{"name":"d"},"spec":{"components":{"mlflowoperator":{}}},"status":{"components":{"mlflowoperator":{"managementState":"Removed"}}}}`), mlflowComponent,
			"component mlflowoperator is Removed", "Removed", "mlflowoperator"},
		{"no state", dsc(`{"metadata":{"name":"d"},"spec":{"components":{"mlflowoperator":{}}}}`), mlflowComponent, "is not set", "", "mlflowoperator"},
		// A DataScienceCluster that does not name the component cannot tell.
		{"not named", dsc(`{"spec":{"components":{"dashboard":{"managementState":"Managed"}}}}`), mlflowComponent, "", "", ""},
		{"no DSC", dscRead{State: DSCStateNoDSC, Version: "v2"}, pipelinesComponent, "this cluster has no DataScienceCluster", "", "aipipelines"},
		{"no DSC API", dscRead{State: DSCStateNoCRD}, mlflowComponent, "RHOAI is not installed", "", "mlflowoperator"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := componentBlock(tc.read, tc.fc)
			if tc.want == "" {
				if got != nil {
					t.Fatalf("want no block, got %+v", got)
				}
				return
			}
			if got == nil || !strings.Contains(got.Reason, tc.want) || got.State != tc.state || got.Component != tc.component {
				t.Fatalf("want %q (state %q, component %q), got %+v", tc.want, tc.state, tc.component, got)
			}
			if !strings.Contains(got.Reason, "cluster-admin") {
				t.Errorf("the reason must say who can fix it: %q", got.Reason)
			}
		})
	}
}

func TestGetResourcesStatus_ReportsFeatureBlocks(t *testing.T) {
	t.Run("components removed", func(t *testing.T) {
		f, c := newResourceFake(t)
		putDSC(f, `{"aipipelines":{"managementState":"Removed"},"mlflowoperator":{"managementState":"Removed"}}`)
		s, err := GetResourcesStatus(c)
		if err != nil {
			t.Fatal(err)
		}
		if s.PipelinesBlocked == nil || s.PipelinesBlocked.State != "Removed" || s.MLflowBlocked == nil || s.MLflowBlocked.Component != "mlflowoperator" {
			t.Fatalf("got pipelines %+v, mlflow %+v", s.PipelinesBlocked, s.MLflowBlocked)
		}
	})
	t.Run("components managed", func(t *testing.T) {
		f, c := newResourceFake(t)
		putManagedDSC(f)
		s, err := GetResourcesStatus(c)
		if err != nil {
			t.Fatal(err)
		}
		if s.PipelinesBlocked != nil || s.MLflowBlocked != nil {
			t.Fatalf("got pipelines %+v, mlflow %+v", s.PipelinesBlocked, s.MLflowBlocked)
		}
	})
	t.Run("pipelines managed but its API not there yet", func(t *testing.T) {
		f, c := newResourceFake(t)
		putManagedDSC(f)
		f.fail["GET /apis/"+dspaAPIGroup+"/datasciencepipelinesapplications"] = 404
		s, err := GetResourcesStatus(c)
		if err != nil {
			t.Fatal(err)
		}
		if s.PipelinesBlocked == nil || !strings.Contains(s.PipelinesBlocked.Reason, "no DataSciencePipelinesApplication API") {
			t.Fatalf("got %+v", s.PipelinesBlocked)
		}
	})
	t.Run("an unreadable DataScienceCluster blocks nothing", func(t *testing.T) {
		f, c := newResourceFake(t)
		f.fail["GET /apis/datasciencecluster.opendatahub.io/v2/datascienceclusters"] = 500
		s, err := GetResourcesStatus(c)
		if err != nil {
			t.Fatal(err)
		}
		if s.PipelinesBlocked != nil || s.MLflowBlocked != nil {
			t.Fatalf("got pipelines %+v, mlflow %+v", s.PipelinesBlocked, s.MLflowBlocked)
		}
	})
}

// Setup refuses before it changes anything when the component is off: the
// CRD stays when a component is Removed, so the CR would never start.
func TestSetups_RefuseWhenTheComponentIsOff(t *testing.T) {
	t.Run("pipeline server", func(t *testing.T) {
		f, c := newResourceFake(t)
		putReadyManagedS3(f)
		putDSC(f, `{"aipipelines":{"managementState":"Removed"}}`)
		resp, _ := SetupPipelineServer(c, "p1")
		if resp.Success || resp.ErrorCode != "prerequisites" || !strings.Contains(resp.Message, "Nothing was changed") {
			t.Fatalf("got %+v", resp)
		}
		if hasMutation(f, "POST /api/v1/namespaces") || f.get(dspaPath("p1", dspaName)) != nil {
			t.Error("nothing may be created")
		}
	})
	t.Run("MLflow", func(t *testing.T) {
		f, c := newResourceFake(t)
		putDSC(f, `{"mlflowoperator":{"managementState":"Removed"}}`)
		resp, _ := SetupMLflow(c)
		if resp.Success || resp.ErrorCode != "prerequisites" {
			t.Fatalf("got %+v", resp)
		}
		if f.get(mlflowPath) != nil {
			t.Error("the MLflow CR may not be created")
		}
	})
}

// A ready pipeline server reports the address the KFP SDK uses: DSPO's
// status.components.apiServer.externalUrl, https only.
func TestGetResourcesStatus_PipelineServerAPIAddress(t *testing.T) {
	f, c := newResourceFake(t)
	putReadyManagedS3(f)
	putDSProject(f, "p1")
	putDSProject(f, "p2")
	ready := func(url string) string {
		return `{"conditions":[{"type":"Ready","status":"True"}],"components":{"apiServer":{"externalUrl":"` + url + `"}}}`
	}
	putDSPA(f, "p1", dspaName, toolLabelJSON, toolFieldManager, "Apply", s3Host(), dspaSecretName, "", ready("https://ds-pipeline-nightly-dspa-p1.apps.example.com"))
	putDSPA(f, "p2", dspaName, toolLabelJSON, toolFieldManager, "Apply", s3Host(), dspaSecretName, "", ready("javascript:alert(1)"))
	s, err := GetResourcesStatus(c)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, ps := range s.PipelineServers {
		got[ps.Namespace] = ps.APIRoute
	}
	if got["p1"] != "https://ds-pipeline-nightly-dspa-p1.apps.example.com" || got["p2"] != "" {
		t.Fatalf("got %v", got)
	}
}
