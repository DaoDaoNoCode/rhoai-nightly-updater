package serving

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
)

// A published model leaves Gen AI studio's AI assets, as the dashboard's
// MaaS checkbox does it: maasDeploymentTransformer.ts applyMaaSEndpointData
// (odh-dashboard rhoai-3.6 a9ea0d86 packages/maas/frontend/src/odh/
// modelServingExtensions/modelDeploymentWizard, L27-40) deletes the
// opendatahub.io/genai-asset label and the opendatahub.io/genai-use-case
// annotation, so that MaaSModelRef provides the subscribed users'
// discovery. Kept, the model was listed twice: once as the project's
// asset and once through MaaS. Publish records what it removed in the MaaS
// record; Unpublish puts back what is still absent then (a value someone
// set meanwhile is theirs and stays). Setup leaves them off while the
// model is published.
const (
	genAIAssetLabel        = "opendatahub.io/genai-asset"
	GenAIUseCaseAnnotation = "opendatahub.io/genai-use-case"
)

// maasGenAI is what a published model had before publishing.
type maasGenAI struct {
	Asset      string `json:"asset,omitempty"`
	HadAsset   bool   `json:"hadAsset,omitempty"`
	UseCase    string `json:"useCase,omitempty"`
	HadUseCase bool   `json:"hadUseCase,omitempty"`
}

func genAIOf(meta kube.ObjectMeta) maasGenAI {
	var g maasGenAI
	g.Asset, g.HadAsset = meta.Labels[genAIAssetLabel]
	g.UseCase, g.HadUseCase = meta.Annotations[GenAIUseCaseAnnotation]
	return g
}

func (r *MaaSRecord) setGenAI(project, model string, g maasGenAI) {
	if r.GenAI == nil {
		r.GenAI = map[string]maasGenAI{}
	}
	r.GenAI[MaaSPub{project, model}.Key()] = g
}

// patchModelGenAI merge-patches the tool's model's genai label and
// annotation (nil removes) with its resourceVersion as a precondition, as
// field manager rhoai-nightly-updater-maas.
func patchModelGenAI(c *kube.Client, project, name, rv string, labels, annotations map[string]any) error {
	path, _ := ToolModelPath(project, name)
	meta := map[string]any{"resourceVersion": rv}
	if len(labels) > 0 {
		meta["labels"] = labels
	}
	if len(annotations) > 0 {
		meta["annotations"] = annotations
	}
	patch, _ := json.Marshal(map[string]any{"metadata": meta})
	_, _, err := c.Do(http.MethodPatch, path, "application/merge-patch+json", patch, url.Values{"fieldManager": {maasModelManager}})
	return err
}

// stripModelGenAI removes the genai label and annotation from the tool's
// model (nothing when it has neither).
func stripModelGenAI(c *kube.Client, project, name string) error {
	model, found, err := ReadToolModel(c, project, name)
	if err != nil || !found {
		return err
	}
	labels, annotations := map[string]any{}, map[string]any{}
	if _, ok := model.Metadata.Labels[genAIAssetLabel]; ok {
		labels[genAIAssetLabel] = nil
	}
	if _, ok := model.Metadata.Annotations[GenAIUseCaseAnnotation]; ok {
		annotations[GenAIUseCaseAnnotation] = nil
	}
	if len(labels)+len(annotations) == 0 {
		return nil
	}
	if err := patchModelGenAI(c, project, name, model.Metadata.ResourceVersion, labels, annotations); err != nil {
		return fmt.Errorf("take %s in %s out of Gen AI studio's AI assets: %w", name, project, err)
	}
	return nil
}

// restoreModelGenAI puts back what publish removed, where it is still
// absent.
func restoreModelGenAI(c *kube.Client, project, name string, g maasGenAI) error {
	model, found, err := ReadToolModel(c, project, name)
	if err != nil || !found {
		return err
	}
	labels, annotations := map[string]any{}, map[string]any{}
	if _, ok := model.Metadata.Labels[genAIAssetLabel]; g.HadAsset && !ok {
		labels[genAIAssetLabel] = g.Asset
	}
	if _, ok := model.Metadata.Annotations[GenAIUseCaseAnnotation]; g.HadUseCase && !ok {
		annotations[GenAIUseCaseAnnotation] = g.UseCase
	}
	if len(labels)+len(annotations) == 0 {
		return nil
	}
	if err := patchModelGenAI(c, project, name, model.Metadata.ResourceVersion, labels, annotations); err != nil {
		return fmt.Errorf("put %s in %s back in Gen AI studio's AI assets: %w", name, project, err)
	}
	return nil
}

// genAIPending: publish took the genai label or annotation off the model
// of p (recorded) and it is still off: Unpublish has not put it back.
func (r MaaSRecord) genAIPending(p MaaSPub, meta kube.ObjectMeta) bool {
	g, ok := r.GenAI[p.Key()]
	if !ok {
		return false
	}
	_, hasAsset := meta.Labels[genAIAssetLabel]
	_, hasUseCase := meta.Annotations[GenAIUseCaseAnnotation]
	return (g.HadAsset && !hasAsset) || (g.HadUseCase && !hasUseCase)
}
