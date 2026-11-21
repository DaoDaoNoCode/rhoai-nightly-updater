package cluster

import (
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/serving"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Models, the inference gateway and MaaS (internal/serving), under the names
// pkg/api and this package use.

// ValidateModelsRequest is serving.ValidateModelsRequest.
func ValidateModelsRequest(req types.ModelsRequest) string {
	return serving.ValidateModelsRequest(req)
}

// RunModels is serving.RunModels.
func RunModels(c *Client, req types.ModelsRequest, onStep func(UpdateStepEvent)) (*types.OperationResponse, error) {
	return serving.RunModels(c, req, onStep)
}

// ValidModelName is serving.ValidModelName.
func ValidModelName(name string) bool {
	return serving.ValidModelName(name)
}

// ValidExposure is serving.ValidExposure.
func ValidExposure(e string) bool {
	return serving.ValidExposure(e)
}

// GetModelExposures is serving.GetModelExposures.
func GetModelExposures(c *Client, project, model string) (*types.ModelExposures, string, string) {
	return serving.GetModelExposures(c, project, model)
}

// ValidateMaaSRequest is serving.ValidateMaaSRequest.
func ValidateMaaSRequest(req types.MaaSRequest) string {
	return serving.ValidateMaaSRequest(req)
}

// RunMaaS is serving.RunMaaS.
func RunMaaS(c *Client, req types.MaaSRequest, onStep func(UpdateStepEvent)) (*types.OperationResponse, error) {
	return serving.RunMaaS(c, req, onStep)
}

// GetMaaSStatus is serving.GetMaaSStatus.
func GetMaaSStatus(c *Client) (*types.MaaSStatus, error) {
	return serving.GetMaaSStatus(c)
}

// CheckMaaSGroups is serving.CheckMaaSGroups.
func CheckMaaSGroups(c *Client, names []string) ([]types.MaaSGroupProblem, string) {
	return serving.CheckMaaSGroups(c, names)
}

// PruneGoneMaaSPublicationsInBackground is
// serving.PruneGoneMaaSPublicationsInBackground.
func PruneGoneMaaSPublicationsInBackground(c *Client) {
	serving.PruneGoneMaaSPublicationsInBackground(c)
}
