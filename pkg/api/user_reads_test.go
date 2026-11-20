package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The projects list is read with the user's own client, for every role.
func TestProjectsAreReadWithTheUsersToken(t *testing.T) {
	setupDevMode(t)
	setAdmin(t)
	orig := listUserDSProjects
	t.Cleanup(func() { listUserDSProjects = orig })
	for _, who := range []string{"user:admin", "user:carol"} {
		var asUser bool
		listUserDSProjects = func(u *cluster.Client) ([]string, error) {
			asUser = u.ActsAsUser()
			return []string{"mine"}, nil
		}
		w := httptest.NewRecorder()
		HandleDSProjects(w, getAs("/api/resources/projects", who))
		if w.Code != 200 || !asUser || strings.TrimSpace(w.Body.String()) != `{"projects":["mine"]}` {
			t.Errorf("%s: %d %s (as user %v)", who, w.Code, w.Body.String(), asUser)
		}
	}
	// A refusal to the user's token is forbidden_for_user.
	listUserDSProjects = func(*cluster.Client) ([]string, error) {
		return nil, fmt.Errorf("list: %w", &cluster.K8sError{Status: 403, User: true})
	}
	w := httptest.NewRecorder()
	HandleDSProjects(w, getAs("/api/resources/projects", "user:carol"))
	if w.Code != 403 || decodeError(t, w)["errorCode"] != cluster.ErrorCodeForbiddenForUser || strings.Contains(w.Body.String(), "make upgrade") {
		t.Errorf("refused: %d %s", w.Code, w.Body.String())
	}
}

func TestMLflowSamplesStatusHandlerReadsAsTheUser(t *testing.T) {
	setupDevMode(t)
	orig := getMLflowSamplesStatus
	t.Cleanup(func() { getMLflowSamplesStatus = orig })
	var saUser, uUser bool
	getMLflowSamplesStatus = func(sa, u *cluster.Client, project string) (*types.MLflowSamplesStatus, error) {
		saUser, uUser = sa.ActsAsUser(), u.ActsAsUser()
		if project == "other" {
			return nil, fmt.Errorf("%w: '%s' is not one of your data science projects", cluster.ErrNotDSProject, project)
		}
		return &types.MLflowSamplesStatus{Project: project}, nil
	}
	w := httptest.NewRecorder()
	HandleMLflowSamplesStatus(w, getAs("/api/resources/samples/mlflow?project=proj", "user:carol"))
	if w.Code != 200 || saUser || !uUser {
		t.Fatalf("%d %s (sa as user %v, u as user %v)", w.Code, w.Body.String(), saUser, uUser)
	}
	w = httptest.NewRecorder()
	HandleMLflowSamplesStatus(w, getAs("/api/resources/samples/mlflow?project=other", "user:carol"))
	if w.Code != 403 || decodeError(t, w)["errorCode"] != cluster.ErrorCodeForbiddenForUser {
		t.Fatalf("not the user's project: %d %s", w.Code, w.Body.String())
	}
}

// The Environment status per role: an admin gets the cluster-wide status
// (not tested here: it reads the cluster), a user must name a project and
// gets that project, read with their token.
func TestResourcesStatusForAUser(t *testing.T) {
	setupDevMode(t)
	setAdmin(t)
	orig := getProjectStatus
	t.Cleanup(func() { getProjectStatus = orig })
	calls := 0
	var uUser bool
	getProjectStatus = func(sa, u *cluster.Client, project string) (*types.ProjectStatus, error) {
		calls++
		uUser = u.ActsAsUser() && !sa.ActsAsUser()
		if project == "other" {
			return nil, fmt.Errorf("%w: '%s' is not one of your data science projects", cluster.ErrNotDSProject, project)
		}
		return &types.ProjectStatus{Project: project}, nil
	}
	for _, tc := range []struct {
		query string
		code  int
		ec    string
	}{
		{"", http.StatusForbidden, adminOnlyCode},
		{"?project=Bad_Name", http.StatusBadRequest, "validation"},
		{"?project=other", http.StatusForbidden, cluster.ErrorCodeForbiddenForUser},
	} {
		w := httptest.NewRecorder()
		HandleResourcesStatus(w, getAs("/api/resources/status"+tc.query, "user:carol"))
		if w.Code != tc.code || decodeError(t, w)["errorCode"] != tc.ec {
			t.Errorf("%q: %d %s", tc.query, w.Code, w.Body.String())
		}
	}
	if calls != 1 {
		t.Errorf("project status read %d times", calls)
	}
	w := httptest.NewRecorder()
	HandleResourcesStatus(w, getAs("/api/resources/status?project=proj", "user:carol"))
	if w.Code != 200 || !uUser || !strings.Contains(w.Body.String(), `"project":"proj"`) {
		t.Fatalf("own project: %d %s (as user %v)", w.Code, w.Body.String(), uUser)
	}
}
