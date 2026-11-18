package cluster

import (
	"fmt"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// CheckNodePull checks that the cluster's nodes can pull image, a nightly
// catalog image, with their own pull secret: the same check as Update's
// catalog verification (preflightReinstallCatalog), without its channel
// checks. It creates a temporary verification CatalogSource for image
// (labelled, removed at the end, and by the next operation if this one is
// killed), waits until OLM's registry pod serves it (READY: the image was
// pulled) or until that pod fails to pull it (pullFailureWatch), and deletes
// it. Nothing else changes. It is the only way to tell from inside the
// updater, which has no access to openshift-config/pull-secret, that the
// nodes of a self-managed cluster have the quay.io/rhoai credentials.
func CheckNodePull(c *Client, image string) (*types.OperationResponse, error) {
	logs := []string{"Image: " + image}
	name, path, remove, err := createVerificationCatalog(c, image)
	if err != nil {
		return &types.OperationResponse{
			Success:   false,
			Message:   fmt.Sprintf("The check could not start: %v. Nothing was changed.", err),
			Logs:      logs,
			ErrorCode: errorCodeFromK8sErr(err),
		}, nil
	}
	defer remove()
	logs = append(logs, "Created the temporary CatalogSource "+CatalogNS+"/"+name)

	deadline := clk.Now().Add(CatalogReadyTimeout)
	pull := pullFailureWatch{advice: nodePullAdvice}
	last := ""
	for clk.Now().Before(deadline) {
		state, err := verificationCatalogState(c, path)
		switch {
		case err != nil:
			last = err.Error()
		case state == "READY":
			logs = append(logs, "OK: the catalog pod pulled the image and serves it")
			return &types.OperationResponse{
				Success: true,
				Message: "A temporary catalog pod pulled " + image + " with the nodes' pull secret, and was removed again. " +
					"A node whose machine config pool is still updating may not have the credentials yet: oc get mcp shows UPDATED True for every pool when they all do.",
				Logs: logs,
			}, nil
		default:
			last = "catalog state is " + state
			if pullErr := pull.check(c, name, image); pullErr != nil {
				// "The catalog image ... cannot be pulled (...). <advice>."
				text := pullErr.Error()
				msg := strings.ToUpper(text[:1]) + text[1:] + "."
				return &types.OperationResponse{Success: false, Message: msg, Logs: append(logs, msg), ErrorCode: "catalog_image_pull"}, nil
			}
		}
		if err := clk.Sleep(c.ctx, CatalogPollInterval); err != nil {
			return &types.OperationResponse{Success: false, Message: "The check was stopped", Logs: logs}, err
		}
	}
	msg := fmt.Sprintf("The check found no answer within %s (%s): the image may still be downloading. Try again in a minute.", CatalogReadyTimeout, last)
	return &types.OperationResponse{Success: false, Message: msg, Logs: append(logs, msg), ErrorCode: "timeout"}, nil
}
