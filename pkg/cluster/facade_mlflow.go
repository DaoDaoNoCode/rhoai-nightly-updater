package cluster

import (
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/mlflow"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// MLflow and its samples (internal/mlflow), under the names pkg/api and
// this package use.

// The samples' ids (types.MLflowSample.ID).
const (
	SampleGenAIDemo = mlflow.SampleGenAIDemo
	SampleTraining  = mlflow.SampleTraining
	SampleMCPServer = mlflow.SampleMCPServer
)

// ErrNotDSProject is mlflow.ErrNotDSProject: the same value, so errors.Is
// matches either name.
var ErrNotDSProject = mlflow.ErrNotDSProject

// SetupMLflow is mlflow.Setup.
func SetupMLflow(c *Client) (*types.OperationResponse, error) {
	return mlflow.Setup(c)
}

// TeardownMLflow is mlflow.Teardown.
func TeardownMLflow(c *Client) (*types.OperationResponse, error) {
	return mlflow.Teardown(c)
}

// DeployMLflowPR is mlflow.DeployPR.
func DeployMLflowPR(c *Client, prNumber int) (*types.OperationResponse, error) {
	return mlflow.DeployPR(c, prNumber)
}

// RevertMLflowImage is mlflow.RevertImage.
func RevertMLflowImage(c *Client) (*types.OperationResponse, error) {
	return mlflow.RevertImage(c)
}

// GetMLflowSamplesStatus is mlflow.SamplesStatus.
func GetMLflowSamplesStatus(sa, u *Client, project string) (*types.MLflowSamplesStatus, error) {
	return mlflow.SamplesStatus(sa, u, project)
}

// ValidateMLflowSamplesRequest is mlflow.ValidateSamplesRequest.
func ValidateMLflowSamplesRequest(req types.MLflowSamplesRequest) string {
	return mlflow.ValidateSamplesRequest(req)
}

// RunMLflowSamples is mlflow.RunSamples.
func RunMLflowSamples(c *Client, req types.MLflowSamplesRequest, onStep func(UpdateStepEvent)) (*types.OperationResponse, error) {
	return mlflow.RunSamples(c, req, onStep)
}

// PipelineSampleSpec is mlflow.PipelineSampleSpec.
func PipelineSampleSpec() types.PipelineSampleSpec {
	return mlflow.PipelineSampleSpec()
}
