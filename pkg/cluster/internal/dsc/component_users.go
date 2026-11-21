package dsc

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/mlflow"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/serving"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/storage"
)

// ComponentUsers names what on the cluster still uses a component (objects
// being deleted do not count). A read that fails is returned: the caller
// keeps the component on (fail closed).
func ComponentUsers(c *kube.Client, key string) ([]string, error) {
	var out []string
	live := func(meta kube.ObjectMeta, kind string) {
		if meta.DeletionTimestamp == "" {
			out = append(out, fmt.Sprintf("%s %s/%s", kind, meta.Namespace, meta.Name))
		}
	}
	switch key {
	case "aipipelines", "datasciencepipelines":
		dspas, err := storage.ListDSPAs(c, "")
		if errors.Is(err, storage.ErrDSPACRDMissing) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		for _, d := range dspas {
			live(d.Meta, "pipeline server")
		}
	case "mlflowoperator":
		cr, err := mlflow.ReadCR(c)
		if kube.IsK8sNotFound(err) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read the MLflow CR: %w", err)
		}
		if cr.Metadata.DeletionTimestamp == "" {
			out = append(out, "MLflow "+cr.Metadata.Name)
		}
	case "kserve":
		models, err := serving.ListLLMInferenceServices(c)
		if err != nil && !errors.Is(err, serving.ErrLLMISvcAPIMissing) {
			return nil, err
		}
		for _, m := range models {
			live(m.Metadata, "LLMInferenceService")
		}
		body, found, err := c.GetOptional(rhoai.ClusterPath("serving.kserve.io/v1beta1", "inferenceservices", ""))
		if err != nil {
			return nil, fmt.Errorf("list InferenceServices: %w", err)
		}
		if found {
			var list struct {
				Items []struct {
					Metadata kube.ObjectMeta `json:"metadata"`
				} `json:"items"`
			}
			if err := json.Unmarshal(body, &list); err != nil {
				return nil, fmt.Errorf("parse InferenceServices: %w", err)
			}
			for _, it := range list.Items {
				live(it.Metadata, "InferenceService")
			}
		}
	case "ogx", "llamastackoperator", "dashboard":
		// The playground's servers (the dashboard serves their gen-ai API).
		read, err := rhoai.ListServed(c, "ogx.io", "/apis/ogx.io/%s/ogxservers", nil, []string{"v1beta1"})
		if err != nil {
			return nil, fmt.Errorf("list OGXServers: %w", err)
		}
		if read.Body != nil {
			var list struct {
				Items []struct {
					Metadata kube.ObjectMeta `json:"metadata"`
				} `json:"items"`
			}
			if err := json.Unmarshal(read.Body, &list); err != nil {
				return nil, fmt.Errorf("parse OGXServers: %w", err)
			}
			for _, it := range list.Items {
				live(it.Metadata, "playground server")
			}
		}
	default:
		return nil, fmt.Errorf("component %s is not one this tool turns on", key)
	}
	sort.Strings(out)
	return out, nil
}
