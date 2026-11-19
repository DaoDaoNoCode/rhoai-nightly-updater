package cluster

import (
	"encoding/json"
	"net/url"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The MCP server and agent skill samples: entries in MLflow's MCP Server
// Registry (/api/3.0/mlflow/mcp-servers, mlflow server/mcp_server_api.py)
// and Skill Registry (/api/3.0/mlflow/skills, server/skill_registry_api.py),
// both per workspace. The dashboard shows them at AI hub > MCP servers >
// Registry and AI hub > Skills; the Gen AI playground lists a registry
// server only when it is active and has an access endpoint (odh-dashboard
// gen-ai BFF api/mcp_helpers.go:139-147).
const (
	// mcpSampleName is "<reverse-DNS namespace>/<slug>" (entities/
	// mcp_server.py:41-71). The namespace is DeepWiki's domain, whose
	// server the entry points at; the slug marks it as this tool's sample.
	mcpSampleName    = "com.deepwiki/rhoai-nightly-sample"
	mcpSampleVersion = "1.0.0"
	// mcpSampleURL is DeepWiki's MCP server: "a free, remote,
	// no-authentication-required service that provides access to public
	// repositories", streamable HTTP at /mcp (docs.devin.ai/work-with-devin/
	// deepwiki-mcp, read 2026-11-17). Checked 2026-11-17: initialize and
	// tools/list answer without a key (read_wiki_structure,
	// read_wiki_contents, ask_wiki_question). The playground needs egress
	// to reach it; the registry entry itself needs none.
	mcpSampleURL       = "https://mcp.deepwiki.com/mcp"
	mcpSampleTransport = "streamable-http"

	// skillSampleName follows the skill name rules (lowercase letters,
	// digits and single hyphens; utils/validation.py:758-774).
	skillSampleName = "nightly-sample-mcp-builder"
	// The skill points at Anthropic's public mcp-builder skill (Apache-2.0,
	// skills/mcp-builder/LICENSE.txt), pinned to a commit. Registration is
	// metadata only: the server records the Git source and never fetches
	// it (server/skill_registry/registration.py:101-145, 265-320).
	skillSampleSource  = "https://github.com/anthropics/skills"
	skillSampleRef     = "e4bb5d960d87126ff3b3a8af1a104b7db67560ba"
	skillSampleSubpath = "skills/mcp-builder"
)

// mcpServerPath is the registry path of mcpSampleName. Each part is escaped
// on its own; the "/" between them stays a path separator, which the
// {name:path} routes expect (mcp_server_api.py).
func mcpServerPath(suffix string) string {
	ns, slug, _ := strings.Cut(mcpSampleName, "/")
	return "/api/3.0/mlflow/mcp-servers/" + url.PathEscape(ns) + "/" + url.PathEscape(slug) + suffix
}

func skillPath(suffix string) string {
	return "/api/3.0/mlflow/skills/" + url.PathEscape(skillSampleName) + suffix
}

// mcpServer is MCPServerResponse (mcp_server_api.py:327-341).
type mcpServer struct {
	Name            string            `json:"name"`
	Status          string            `json:"status"`
	AccessEndpoints []json.RawMessage `json:"access_endpoints"`
	LatestVersion   string            `json:"latest_version"`
	Tags            map[string]string `json:"tags"`
}

// registryEntry reads a registry object at path into out; found is false on
// a 404 for the object, and supported is false when the server has no such
// API (isEndpointMissing).
func (t *mlflowTarget) registryEntry(path string, out any) (found, supported bool, err error) {
	err = t.api.sendJSON("GET", path, nil, nil, out)
	switch {
	case err == nil:
		return true, true, nil
	case isEndpointMissing(err):
		return false, false, nil
	case isNotFound(err):
		return false, true, nil
	}
	return false, true, err
}

func mcpStatus(t *mlflowTarget) types.SampleStatus {
	s := types.SampleStatus{}
	var srv mcpServer
	found, supported, err := t.registryEntry(mcpServerPath(""), &srv)
	switch {
	case err != nil:
		return statusFromError(s, err)
	case !supported:
		s.State, s.Actions = "unsupported", []string{}
		s.Message = "This MLflow has no MCP Server Registry."
		return s
	case !found:
		s.State, s.Actions = "absent", []string{"add"}
		return s
	case srv.Tags[sampleMarkerKey] != SampleMCPServer:
		s.State, s.Actions = "foreign", []string{}
		s.Message = foreignMessage("MCP server '" + mcpSampleName + "'")
		return s
	}
	if srv.Status != "active" {
		s.Missing = append(s.Missing, "active version")
	}
	if len(srv.AccessEndpoints) == 0 {
		s.Missing = append(s.Missing, "endpoint")
	}
	if len(s.Missing) > 0 {
		s.State, s.Actions = "partial", []string{"add", "remove"}
		return s
	}
	s.State, s.Actions = "present", []string{"remove"}
	s.Message = "Points at " + mcpSampleURL + "; the playground needs internet access to call it."
	return s
}

// skill is SkillResponse (skill_registry_api.py:141-155).
type skill struct {
	Name          string            `json:"name"`
	LatestVersion *int              `json:"latest_version"`
	Tags          map[string]string `json:"tags"`
}

// skillStatus also detects the API: a 404 ENDPOINT_NOT_FOUND (or an unknown
// route) means this MLflow has no Skill Registry (RHOAI 3.5 has none).
func skillStatus(t *mlflowTarget) types.SampleStatus {
	s := types.SampleStatus{}
	var sk skill
	found, supported, err := t.registryEntry(skillPath(""), &sk)
	switch {
	case err != nil:
		return statusFromError(s, err)
	case !supported:
		s.State, s.Actions = "unsupported", []string{}
		s.Message = "This MLflow has no Skill Registry."
	case !found:
		s.State, s.Actions = "absent", []string{"add"}
	case sk.Tags[sampleMarkerKey] != SampleSkill:
		s.State, s.Actions = "foreign", []string{}
		s.Message = foreignMessage("Skill '" + skillSampleName + "'")
	case sk.LatestVersion == nil:
		s.State, s.Actions, s.Missing = "partial", []string{"add", "remove"}, []string{"version"}
	default:
		s.State, s.Actions = "present", []string{"remove"}
		s.Message = "Registers " + skillSampleSource + " (" + skillSampleSubpath + " at " + skillSampleRef[:12] + "); the dashboard's Skills page needs the skillRegistry feature flag."
	}
	return s
}
