package cluster

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Where a model's download lands. KServe rhoai-3.6 (red-hat-data-services/
// kserve 78c5a9e9) mounts the storage-initializer's download for an
// LLMInferenceService at /mnt/models on the volume
// StorageInitializerConfig.ModelVolumeSource names
// (pkg/controller/v1alpha2/llmisvc/workload_storage.go
// attachStorageInitializer L432-436), and an emptyDir when that is nil
// (pkg/utils/storage.go AddModelMount L127-141; pkg/types/config.go
// L53-58). The shipped inferenceservice-config has "modelVolumeSource":
// null (config/configmap/inferenceservice.yaml L763) and the ODH overlay
// does not set it (config/overlays/odh/patches/
// inferenceservice-config-patch.yaml), so by default the model lives on the
// node's ephemeral storage, which the scheduler only counts when the pod
// requests it. The tool reads the live ConfigMap (data key
// storageInitializer) instead of assuming: another volume means no
// request.

// modelVolumeIsEmptyDir says whether KServe puts a downloaded model on an
// emptyDir: true unless inferenceservice-config's storageInitializer names
// a modelVolumeSource (unreadable counts as KServe's default, emptyDir: a
// request only reserves room).
func modelVolumeIsEmptyDir(c *Client) bool {
	ns, err := appsNamespace(c)
	if err != nil {
		return true
	}
	body, _, err := c.Get(namespacedPath("v1", "configmaps", ns, kserveConfigMap))
	if err != nil {
		return true
	}
	var cm struct {
		Data map[string]string `json:"data"`
	}
	var si struct {
		ModelVolumeSource json.RawMessage `json:"modelVolumeSource"`
	}
	if json.Unmarshal(body, &cm) != nil || json.Unmarshal([]byte(cm.Data["storageInitializer"]), &si) != nil {
		return true
	}
	v := strings.TrimSpace(string(si.ModelVolumeSource))
	return v == "" || v == "null"
}

// quotaWarning says that a ResourceQuota of the project leaves less than
// the model needs (hard minus used), or "". The model's pod is refused
// then, not left pending, until the quota is raised or other workloads
// stop. Unreadable quotas are no warning (unknown is not a failure).
func quotaWarning(c *Client, project string, entry catalogModel, ephemeral bool) string {
	var items []struct {
		Metadata objectMeta `json:"metadata"`
		Status   struct {
			Hard map[string]string `json:"hard"`
			Used map[string]string `json:"used"`
		} `json:"status"`
	}
	if c.ListInto(namespacedPath("v1", "resourcequotas", project, ""), &items) != nil {
		return ""
	}
	need := map[string]string{
		"requests.cpu": entry.CPU, "cpu": entry.CPU, "limits.cpu": entry.CPU,
		"requests.memory": entry.MemoryRequest, "memory": entry.MemoryRequest, "limits.memory": entry.Memory,
		"pods": "1",
	}
	if ephemeral && entry.ephemeralStorage != "" {
		need["requests.ephemeral-storage"] = entry.ephemeralStorage
		need["ephemeral-storage"] = entry.ephemeralStorage
	}
	var short []string
	sort.Slice(items, func(i, j int) bool { return items[i].Metadata.Name < items[j].Metadata.Name })
	for _, q := range items {
		keys := make([]string, 0, len(q.Status.Hard))
		for k := range q.Status.Hard {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			want, ok := need[k]
			if !ok {
				continue
			}
			hard, ok1 := parseQuantity(q.Status.Hard[k])
			used, _ := parseQuantity(q.Status.Used[k])
			n, ok2 := parseQuantity(want)
			if ok1 && ok2 && hard-used < n {
				short = append(short, fmt.Sprintf("ResourceQuota %s: %s has %s left of %s, the model needs %s", q.Metadata.Name, k, quantityWords(k, hard-used), q.Status.Hard[k], want))
			}
		}
	}
	if len(short) == 0 {
		return ""
	}
	return fmt.Sprintf("Project %s has no room in its quota for the model (%s). KServe cannot create the model's pod until the quota is raised or other workloads in the project stop; the setup still runs.", project, strings.Join(short, "; "))
}

// quantityWords prints a quantity left: memory and storage in Gi, else a
// plain number.
func quantityWords(key string, v float64) string {
	if v < 0 {
		v = 0
	}
	if strings.Contains(key, "memory") || strings.Contains(key, "storage") {
		return fmt.Sprintf("%gGi", float64(int(v/(1<<30)*10))/10)
	}
	return fmt.Sprintf("%g", v)
}
