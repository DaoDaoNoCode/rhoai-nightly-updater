package api

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// denyModelPatch makes the preflight deny patch on the LLMInferenceService
// of each model named, and nothing else, as an access review would.
func denyModelPatch(names ...string) {
	reviewProjectActions = func(_ *cluster.Client, ids []string, project, _ string) (map[string]cluster.ActionReview, error) {
		out := map[string]cluster.ActionReview{}
		for _, id := range ids {
			a, _ := cluster.ProjectActionByID(id)
			rv := cluster.ActionReview{}
			for _, r := range a.Requests {
				if r.Verb == "patch" && slices.Contains(names, r.Name) {
					req := cluster.AccessRequest{Verb: r.Verb, Group: r.Group, Resource: r.Resource, Name: r.Name, Namespace: project}
					rv.Missing = append(rv.Missing, cluster.MissingPermission{Request: req, Text: cluster.MissingTextFor(req, r.Source, project), Source: r.Source, Target: r.Target})
				}
			}
			out[id] = rv
		}
		return out, nil
	}
}

// A model's deploy needs the permissions on that model only: one missing
// on another catalog model does not refuse it; one missing on it refuses
// with that model alone and who grants it once; a request without a model
// is the catalog's default, scoped the same way.
func TestModelPreflightIsScopedToTheRequestedModel(t *testing.T) {
	env := newProjectGateEnv(t, roleEditor)
	const path = "/api/resources/models/project/stream"
	def := cluster.ProjectModelTarget(types.ProjectModelRequest{})
	if def == "" || def == "smollm2-135m-instruct" {
		t.Fatalf("the catalog's default is %q", def)
	}

	denyModelPatch("smollm2-135m-instruct")
	w := serveProjectAction(t, path, `{"action":"setup","project":"team-a","model":"smollm2-135m-instruct"}`)
	msg, _ := decodeError(t, w)["error"].(string)
	if w.Code != http.StatusForbidden || !strings.Contains(msg, "patch llminferenceservices.serving.kserve.io smollm2-135m-instruct") || strings.Count(msg, "edit role") != 1 || strings.Contains(msg, def) || env.runs != 0 {
		t.Fatalf("denied on the model: %d %q", w.Code, msg)
	}

	denyModelPatch(def)
	w = serveProjectAction(t, path, `{"action":"setup","project":"team-a"}`)
	msg, _ = decodeError(t, w)["error"].(string)
	if w.Code != http.StatusForbidden || !strings.Contains(msg, def) || env.runs != 0 {
		t.Fatalf("no model named, the default denied: %d %q", w.Code, msg)
	}

	denyModelPatch("qwen3-1-7b", def)
	w = serveProjectAction(t, path, `{"action":"setup","project":"team-a","model":"smollm2-135m-instruct"}`)
	if w.Code != http.StatusOK || env.runs != 1 {
		t.Fatalf("denied on other models only: %d %s", w.Code, w.Body.String())
	}
}
