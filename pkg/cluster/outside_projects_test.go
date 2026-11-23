package cluster

import (
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/storage"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// N3: removing the dashboard label drops a project from the Data Science set
// and out of the picker, while pipeline servers and models this tool placed
// in that namespace stay on the cluster and continue to depend on storage
// and the gateway. Resources status labels those objects as outside Data
// Science projects so they remain visible for deletion.
func TestResourcesStatus_TheToolsObjectsOutsideDataScienceProjects(t *testing.T) {
	mc := newModelsCluster(t)
	if res, _ := runModels(t, mc.c, types.ModelsRequest{Action: "setup", Gateway: true, Project: "proj"}); !res.Success {
		t.Fatalf("setup: %+v", res)
	}
	putDSPA(mc.f, "proj", storage.DSPAName, toolLabelJSON, envkit.ToolFieldManager, "Apply", storage.S3Host(), storage.DSPASecretName, "", "")
	// A pipeline server another party owns is not something this tool should enumerate.
	putDSPA(mc.f, "theirs", "dspa", `{}`, "Mozilla", "Update", "s3.amazonaws.com", "x", "", "")
	mc.f.putJSON("/api/v1/namespaces/proj", `{"metadata":{"labels":{}},"status":{"phase":"Active"}}`)
	st, err := GetResourcesStatus(mc.c)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.PipelineServers) != 0 || len(st.Models.Items) != 0 {
		t.Fatalf("listed in the projects: %+v, %+v", st.PipelineServers, st.Models.Items)
	}
	if len(st.OutsidePipelineServers) != 1 || st.OutsidePipelineServers[0].Namespace != "proj" || !st.OutsidePipelineServers[0].ManagedByTool {
		t.Fatalf("outside pipeline servers = %+v", st.OutsidePipelineServers)
	}
	if len(st.Models.Outside) != 1 || st.Models.Outside[0].Namespace != "proj" || st.Models.Outside[0].Name != tModel || !st.Models.Outside[0].ManagedByTool {
		t.Fatalf("outside models = %+v", st.Models.Outside)
	}
}
