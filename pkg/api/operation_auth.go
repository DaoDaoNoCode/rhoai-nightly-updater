package api

import (
	"context"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
	"os"
)

// All mutations use the same app operator permission, evaluated for each
// request. The read-only UI and mutation endpoints share this check.
var mutationPermission = func(ctx context.Context, token string) (bool, error) {
	// Explicit local development bypass; never bypass with a mounted SA token.
	if os.Getenv("DEV_MODE") == "true" && os.Getenv("DEV_TOKEN") != "" {
		if _, err := os.Stat("/var/run/secrets/kubernetes.io/serviceaccount/token"); os.IsNotExist(err) {
			return true, nil
		}
	}
	return cluster.CheckUserPermissionWithToken(ctx, token, "update", "subscriptions", "operators.coreos.com", cluster.SubNS)
}

func sendOperationResult(s *SSEWriter, result *types.OperationResponse, err error) {
	step := UpdateStep{Step: "operation_complete", Status: "failed", Message: "Operation failed"}
	if result != nil {
		step.Message = result.Message
		step.ErrorCode = result.ErrorCode
		if result.Success && err == nil {
			step.Status = "success"
		}
	}
	if err != nil {
		step.Message = err.Error()
	}
	_ = s.SendStep(step)
}
