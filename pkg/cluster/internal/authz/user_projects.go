package authz

import (
	"errors"
	"fmt"
	"net/url"
	"slices"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
)

// ErrNotUserClient: a read that must be made with the user's own token got
// another client.
var ErrNotUserClient = errors.New("this read must be made with the signed-in user's own token")

// dsProjectSelector selects RHOAI's data science projects (the label the
// dashboard sets), for namespaces and projects alike.
const dsProjectSelector = "opendatahub.io/dashboard=true"

// ListUserDSProjects lists the data science projects the signed-in user
// may see, with their own token: the OpenShift projects API
// (project.openshift.io/v1 projects) with the label selector of
// envkit.DSProjects. The API lists the namespaces whose "get namespaces" the
// user's RBAC allows, applies the label selector, and keeps those the
// token's scopes allow (openshift/openshift-apiserver release-4.17
// 6ea968b: pkg/project/apiserver/registry/project/proxy/proxy.go:91-102,
// pkg/project/auth/cache.go:478-518, the review of pkg/project/auth/
// reviewer.go:49-56). Listing projects is granted to every authenticated
// user by OpenShift's basic-user ClusterRole (openshift-apiserver
// pkg/bootstrappolicy/policy.go:461, bound to system:authenticated at
// :766). A cluster-admin sees every project.
func ListUserDSProjects(u *kube.Client) ([]string, error) {
	if !u.ActsAsUser() {
		return nil, ErrNotUserClient
	}
	var items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Status struct {
			Phase string `json:"phase"`
		} `json:"status"`
	}
	if err := u.ListInto("/apis/project.openshift.io/v1/projects?labelSelector="+url.QueryEscape(dsProjectSelector), &items); err != nil {
		if kube.IsDecodeError(err) {
			return nil, fmt.Errorf("parse the project list: %w", err)
		}
		return nil, fmt.Errorf("list your data science projects: %w", err)
	}
	projects := []string{}
	for _, p := range items {
		if p.Status.Phase != "Terminating" {
			projects = append(projects, p.Metadata.Name)
		}
	}
	slices.Sort(projects)
	return projects, nil
}

// IsUserDSProject reports whether project is one of the signed-in user's
// data science projects (ListUserDSProjects).
func IsUserDSProject(u *kube.Client, project string) (bool, error) {
	projects, err := ListUserDSProjects(u)
	if err != nil {
		return false, err
	}
	return slices.Contains(projects, project), nil
}
