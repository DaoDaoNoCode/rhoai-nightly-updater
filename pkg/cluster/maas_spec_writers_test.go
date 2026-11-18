package cluster

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// setModelManagedFields gives the tool's model in proj these
// metadata.managedFields entries, as the API server records them.
func setModelManagedFields(mc *modelsCluster, entries ...map[string]interface{}) {
	mc.f.mu.Lock()
	defer mc.f.mu.Unlock()
	k, _ := parseFakePath(testModelPath)
	meta := ensureMap(mc.f.objects[k], "metadata")
	list := make([]interface{}, 0, len(entries))
	for _, e := range entries {
		list = append(list, e)
	}
	meta["managedFields"] = list
}

func managedEntry(manager, subresource string, fields map[string]interface{}) map[string]interface{} {
	e := map[string]interface{}{"manager": manager, "operation": "Update", "apiVersion": "serving.kserve.io/v1alpha2", "fieldsType": "FieldsV1", "fieldsV1": fields}
	if subresource != "" {
		e["subresource"] = subresource
	}
	return e
}

// A project editor changes the tool's model (here its image) before a
// cluster-admin publishes it: publish refuses before any write, so MaaS
// never offers every signed-in user a model that is not the catalog's.
func TestMaaSPublish_RefusesAModelWhoseSpecSomeoneElseWrote(t *testing.T) {
	for _, field := range []string{"f:template", "f:model", "f:baseRefs"} {
		t.Run(field, func(t *testing.T) {
			mc := modelReadyForMaaS(t)
			setModelManagedFields(mc,
				managedEntry(toolFieldManager, "", map[string]interface{}{"f:spec": map[string]interface{}{"f:model": map[string]interface{}{}}}),
				managedEntry("kubectl-edit", "", map[string]interface{}{"f:spec": map[string]interface{}{field: map[string]interface{}{}}}),
			)
			mc.f.requests = nil
			res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "publish", Project: "proj"})
			if res.Success || res.ErrorCode != "conflict" || !strings.Contains(res.Message, "kubectl-edit") || !strings.Contains(res.Message, "set it up again") || !strings.Contains(res.Message, "Nothing was changed") {
				t.Fatalf("publish = %+v", res)
			}
			if muts := mc.f.mutations(); len(muts) != 0 {
				t.Fatalf("wrote %v", muts)
			}
		})
	}
}

// What others may write without stopping a publication: metadata (the
// dashboard's annotations, for example), the status subresource, and the
// refs this tool's MaaS manager writes.
func TestMaaSPublish_AllowsMetadataStatusAndItsOwnRefsByOthers(t *testing.T) {
	mc := modelReadyForMaaS(t)
	setModelManagedFields(mc,
		managedEntry(toolFieldManager, "", map[string]interface{}{"f:spec": map[string]interface{}{"f:template": map[string]interface{}{}}}),
		managedEntry(maasModelManager, "", map[string]interface{}{"f:spec": map[string]interface{}{"f:router": map[string]interface{}{}}}),
		managedEntry("odh-dashboard", "", map[string]interface{}{"f:metadata": map[string]interface{}{"f:annotations": map[string]interface{}{}}}),
		managedEntry("manager", "status", map[string]interface{}{"f:status": map[string]interface{}{}}),
	)
	if res, _ := runMaaSOp(t, mc.c, types.MaaSRequest{Action: "publish", Project: "proj"}); !res.Success {
		t.Fatalf("publish: %+v", res)
	}
}

func TestForeignSpecWriters(t *testing.T) {
	m := objectMeta{ManagedFields: []managedFieldsEntry{
		{Manager: toolFieldManager, FieldsV1: map[string]json.RawMessage{"f:spec": []byte(`{"f:model":{}}`)}},
		{Manager: "b-editor", FieldsV1: map[string]json.RawMessage{"f:spec": []byte(`{"f:template":{}}`)}},
		{Manager: "a-editor", FieldsV1: map[string]json.RawMessage{"f:spec": []byte(`{".":{}}`)}},
		{Manager: "b-editor", FieldsV1: map[string]json.RawMessage{"f:spec": []byte(`{"f:model":{}}`)}},
		{Manager: "dashboard", FieldsV1: map[string]json.RawMessage{"f:metadata": []byte(`{}`)}},
		{Manager: "controller", Subresource: "status", FieldsV1: map[string]json.RawMessage{"f:spec": []byte(`{"f:x":{}}`)}},
	}}
	if got := strings.Join(foreignSpecWriters(m), ","); got != "a-editor,b-editor" {
		t.Fatalf("writers = %s", got)
	}
}
