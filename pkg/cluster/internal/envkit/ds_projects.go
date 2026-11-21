package envkit

import (
	"fmt"
	"slices"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
)

// DSProjects returns namespaces with the opendatahub.io/dashboard label.
func DSProjects(c *kube.Client) ([]string, error) {
	path := "/api/v1/namespaces?labelSelector=opendatahub.io/dashboard=true"
	var items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Status struct {
			Phase string `json:"phase"`
		} `json:"status"`
	}
	if err := c.ListInto(path, &items); err != nil {
		if kube.IsDecodeError(err) {
			return nil, fmt.Errorf("parse namespace list: %w", err)
		}
		return nil, fmt.Errorf("list DS projects: %w", err)
	}

	var projects []string
	for _, ns := range items {
		if ns.Status.Phase != "Terminating" {
			projects = append(projects, ns.Metadata.Name)
		}
	}
	return projects, nil
}

// IsDSProject reports whether project is a Data Science project (the
// projects the Environment page lists).
func IsDSProject(c *kube.Client, project string) (bool, error) {
	projects, err := DSProjects(c)
	if err != nil {
		return false, err
	}
	if slices.Contains(projects, project) {
		return true, nil
	}
	return false, nil
}
