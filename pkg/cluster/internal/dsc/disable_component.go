package dsc

import (
	"fmt"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/argo"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/components"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// DisableComponent sets a component to Removed (Diagnostics and the
// Components page's "disable-component:<name>" fix). It is the Components
// page's toggle (SetDSCManagementState): the component must be a
// managementState the installed CRD defines, the removal preconditions run
// first, and the change is a tested JSON Patch of that one field after a
// server-side dry run.
func DisableComponent(c *kube.Client, compName string) (*types.OperationResponse, error) {
	if compName == "" {
		return &types.OperationResponse{Success: false, Message: "Component name is required", ErrorCode: "validation"}, nil
	}
	v, err := ReadView(c)
	if err != nil {
		return &types.OperationResponse{Success: false, Message: fmt.Sprintf("Cannot find the DataScienceCluster: %v", err), ErrorCode: kube.ErrorCodeFromK8sErr(err)}, nil
	}
	switch {
	case v.state == rhoai.DSCStateNoCRD:
		return &types.OperationResponse{Success: false, Message: "Cannot find the DataScienceCluster: the DataScienceCluster API is not installed", ErrorCode: "prerequisites"}, nil
	case v.state != rhoai.DSCStatePresent:
		return &types.OperationResponse{Success: false, Message: "Cannot find the DataScienceCluster: no DataScienceCluster exists", ErrorCode: "prerequisites"}, nil
	case v.argoApp != "":
		return &types.OperationResponse{Success: false, Message: argo.GitOpsRefusal("The DataScienceCluster", v.argoApp, "removing "+compName) + fmt.Sprintf(" Set %s to Removed in Git.", compName), ErrorCode: kube.ErrorCodeGitOpsManaged}, nil
	}
	if reason, refused := RefusedComponents[compName]; refused {
		return &types.OperationResponse{Success: false, Message: fmt.Sprintf("The tool does not remove %s. %s", compName, reason), ErrorCode: "validation"}, nil
	}
	if t, ok := v.byKey[compName]; v.crdErr == nil && (!ok || !t.Toggle) {
		return &types.OperationResponse{Success: false, ErrorCode: "validation",
			Message: fmt.Sprintf("Component %q is not a component the installed DataScienceCluster CRD defines. Use the OpenShift Console to edit the DSC directly.", compName)}, nil
	}
	state, present := components.LookupState(v.components(), strings.Split(compName, "."))
	if !present {
		return components.NothingToDo(fmt.Sprintf("the DataScienceCluster has no %s component. Nothing was changed.", compName), nil), nil
	}
	if state == "Removed" {
		return components.NothingToDo(fmt.Sprintf("%s is already Removed. Nothing was changed.", compName), nil), nil
	}
	res := v.setManagementState(c, types.DSCToggleRequest{Name: v.name, Path: compName, From: state, To: "Removed"}, "disable-component", false)
	if res.Success {
		res.Message = fmt.Sprintf("Set %s to Removed (was %s). The operator will clean up the component.", compName, kube.NonEmpty(state, "unset"))
	}
	return res, nil
}
