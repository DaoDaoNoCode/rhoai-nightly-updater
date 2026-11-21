package dsc

import (
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/argo"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/components"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// CreateDefault creates a DataScienceCluster with default component configuration.
// If a DSC already exists, it returns success without modification.
//
// The DSC is created with a plain create, never an apply: on Managed RHOAI the
// operator creates default-dsc itself at startup (bootstrap.RunLeaderElectionInit,
// platform ManagedRhoai only), and an apply racing it would take over and
// overwrite its fields. A create that loses the race gets 409 AlreadyExists,
// and the operator's validating webhook rejects a second DSC under another
// name ("Only one instance of DataScienceCluster object is allowed").
func CreateDefault(c *kube.Client) (*types.OperationResponse, error) {
	logs := []string{}

	// Step 1: Check if a DSC already exists
	logs = append(logs, "Checking for existing DataScienceCluster...")
	read, err := rhoai.ReadFirstDSC(c)
	if err != nil {
		return &types.OperationResponse{
			Success:   false,
			Message:   fmt.Sprintf("Failed to check for existing DSC: %v", err),
			Logs:      logs,
			ErrorCode: kube.ErrorCodeFromK8sErr(err),
		}, nil
	}
	if g, err := argo.ReadOperatorGitOps(c); err != nil {
		return &types.OperationResponse{Success: false, Message: "Cannot check whether Argo CD manages the operator: " + err.Error(), Logs: logs, ErrorCode: kube.ErrorCodeFromK8sErr(err)}, nil
	} else if app := kube.NonEmpty(g.SubscriptionApp, g.CatalogApp); app != "" {
		// Argo CD only: an Argo CD application that installs the operator
		// (the odh-gitops chart) renders the DSC too. An ACM policy that
		// enforces the catalog says nothing about the DSC.
		return &types.OperationResponse{Success: false, ErrorCode: kube.ErrorCodeGitOpsManaged, Logs: logs,
			Message: fmt.Sprintf("Argo CD application %q manages the RHOAI operator here and creates the DataScienceCluster from Git; one made by hand would be adopted and overwritten on its next sync. Define the DataScienceCluster in Git instead. Nothing was changed.", app)}, nil
	}
	switch read.State {
	case rhoai.DSCStateNoCRD:
		return &types.OperationResponse{
			Success:   false,
			Message:   "DataScienceCluster CRD not found. Install the RHOAI operator first.",
			Logs:      logs,
			ErrorCode: "prerequisites",
		}, nil
	case rhoai.DSCStatePresent:
		logs = append(logs, "DataScienceCluster already exists")
		return &types.OperationResponse{
			Success: true,
			Message: "DataScienceCluster already exists",
			Logs:    logs,
		}, nil
	}

	// Step 2: Use the sample shipped with the installed operator version.
	logs = append(logs, "Creating default DataScienceCluster...")
	defaults, fetchErr := components.FetchDefaultDSCSpec(c, "")
	if fetchErr != nil {
		return &types.OperationResponse{Success: false, Message: "Failed to fetch version-matched DSC defaults: " + fetchErr.Error(), Logs: logs, ErrorCode: kube.NonEmpty(kube.ErrorCodeFromK8sErr(fetchErr), "prerequisites")}, nil
	}
	logs = append(logs, fmt.Sprintf("DSC spec source: %s (%s)", defaults.SourceDescription, defaults.Version))

	apiVersion, _ := defaults.Spec["apiVersion"].(string)
	body, err := json.Marshal(defaults.Spec)
	if err != nil {
		return &types.OperationResponse{Success: false, Message: fmt.Sprintf("Failed to encode DSC: %v", err), Logs: logs, ErrorCode: "internal"}, nil
	}
	_, _, createErr := c.Post("/apis/"+apiVersion+"/datascienceclusters?fieldManager=rhoai-nightly-updater", body)
	if kube.IsK8sConflict(createErr) {
		logs = append(logs, "DataScienceCluster already exists (created concurrently)")
		return &types.OperationResponse{Success: true, Message: "DataScienceCluster already exists", Logs: logs}, nil
	}
	if createErr != nil {
		return &types.OperationResponse{
			Success:   false,
			Message:   fmt.Sprintf("Failed to create DataScienceCluster: %v", createErr),
			Logs:      logs,
			ErrorCode: kube.ErrorCodeFromK8sErr(createErr),
		}, nil
	}

	logs = append(logs, "OK: DataScienceCluster created")

	// Step 3: Record activity
	record.RecordActivity(c, types.ActivityEntry{
		Timestamp: record.NowRFC3339(),
		User:      kube.GetUser(c),
		Action:    "create-dsc",
		Detail:    "default-dsc with default components",
		Success:   true,
	})

	slog.Info("dsc created", "user", kube.GetUser(c))

	return &types.OperationResponse{
		Success: true,
		Message: "DataScienceCluster created with default components",
		Logs:    logs,
	}, nil
}
