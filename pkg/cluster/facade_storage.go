package cluster

import (
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/storage"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The S3 storage, its migration and the pipeline servers
// (internal/storage), under the names pkg/api and this package use.

// MigrateOptions is storage.MigrateOptions.
type MigrateOptions = storage.MigrateOptions

// S3Login is storage.S3Login.
type S3Login = storage.S3Login

// S3AdminLoginUnavailable is storage.S3AdminLoginUnavailable.
type S3AdminLoginUnavailable = storage.S3AdminLoginUnavailable

// MigrateS3Storage is storage.Migrate.
func MigrateS3Storage(c *Client, opts MigrateOptions, onStep func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
	return storage.Migrate(c, opts, onStep)
}

// SetupS3Storage is storage.SetupS3.
func SetupS3Storage(c *Client) (*types.OperationResponse, error) {
	return storage.SetupS3(c)
}

// SecureS3Storage is storage.SecureS3.
func SecureS3Storage(c *Client, removeLeftover []string, onStep func(types.UpdateStepEvent)) (*types.OperationResponse, error) {
	return storage.SecureS3(c, removeLeftover, onStep)
}

// RemoveLeftoverS3Keys is storage.RemoveLeftoverKeys.
func RemoveLeftoverS3Keys(c *Client, users []string) (*types.OperationResponse, error) {
	return storage.RemoveLeftoverKeys(c, users)
}

// TeardownS3Storage is storage.TeardownS3.
func TeardownS3Storage(c *Client, opts types.S3TeardownRequest) (*types.OperationResponse, error) {
	return storage.TeardownS3(c, opts)
}

// S3AdminLogin is storage.S3AdminLogin.
func S3AdminLogin(c *Client) (S3Login, error) {
	return storage.S3AdminLogin(c)
}

// SetupPipelineServer is storage.SetupPipelineServer.
func SetupPipelineServer(c *Client, project string) (*types.OperationResponse, error) {
	return storage.SetupPipelineServer(c, project)
}

// RecreatePipelineServer is storage.RecreatePipelineServer.
func RecreatePipelineServer(c *Client, project, confirmedUID string, confirmedWorkbenches map[string]string) (*types.OperationResponse, error) {
	return storage.RecreatePipelineServer(c, project, confirmedUID, confirmedWorkbenches)
}

// TeardownPipelineServer is storage.TeardownPipelineServer.
func TeardownPipelineServer(c *Client, project, confirmedUID string, confirmedWorkbenches map[string]string) (*types.OperationResponse, error) {
	return storage.TeardownPipelineServer(c, project, confirmedUID, confirmedWorkbenches)
}
