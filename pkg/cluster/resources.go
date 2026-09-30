package cluster

import (
	"encoding/json"
	"fmt"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// GetResourcesStatus checks the state of test infrastructure resources.
// userVisibleProjects filters pipeline server results to only projects the user can access.
func GetResourcesStatus(c *Client, userVisibleProjects []string) (*types.ResourcesStatus, error) {
	status := &types.ResourcesStatus{}
	status.MinIO = getMinIOStatus(c)
	status.MLflow = getMLflowStatus(c)

	// Get status for DSPA projects that the user can see
	dspaProjects, err := findDSPAProjects(c)
	if err != nil {
		return nil, err
	}
	visible := map[string]bool{}
	for _, p := range userVisibleProjects {
		visible[p] = true
	}
	for _, p := range dspaProjects {
		if visible[p] {
			status.PipelineServers = append(status.PipelineServers, getPipelineServerStatus(c, p))
		}
	}

	return status, nil
}

// GetDSProjects returns namespaces with the opendatahub.io/dashboard label.
func GetDSProjects(c *Client) ([]string, error) {
	path := "/api/v1/namespaces?labelSelector=opendatahub.io/dashboard=true"
	body, _, err := c.get(path)
	if err != nil {
		return nil, fmt.Errorf("list DS projects: %w", err)
	}

	var nsList struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Status struct{ Phase string `json:"phase"` } `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &nsList); err != nil {
		return nil, fmt.Errorf("parse namespace list: %w", err)
	}

	var projects []string
	for _, ns := range nsList.Items {
		if ns.Status.Phase != "Terminating" {
			projects = append(projects, ns.Metadata.Name)
		}
	}
	return projects, nil
}
