package cluster

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// Add and Remove of the MCP server and skill samples. Neither needs a
// "create" right: creating a version auto-creates a missing parent
// (MCP: store/tracking/mcp_server_registry/sqlalchemy_mixin.py:272-330;
// skills: server/skill_registry/registration.py:101-145), and the plugin
// checks a version POST as "update" on the parent's name
// (mlflow-kubernetes-plugins v2.1.1 rules_base.py:1351-1354;
// rules_skill_registry.py:30-33, "version registration uses parent update
// even if MLflow creates the parent"). So every MCP and skill call is
// limited to the sample's name.

const (
	mcpSampleTitle       = "DeepWiki (sample)"
	mcpSampleDescription = "Sample MCP server added by rhoai-nightly-updater: DeepWiki's public, no-authentication server for questions about public GitHub repositories."
)

// mcpSampleTools are the tools DeepWiki's server lists (tools/list,
// checked 2026-11-17), so the registry shows them without a discovery.
var mcpSampleTools = []map[string]any{
	{"name": "read_wiki_structure", "description": "Get a list of documentation topics for a GitHub repository.",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"repoName": map[string]string{"type": "string"}}, "required": []string{"repoName"}}},
	{"name": "read_wiki_contents", "description": "View documentation about a GitHub repository.",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"repoName": map[string]string{"type": "string"}}, "required": []string{"repoName"}}},
	{"name": "ask_wiki_question", "description": "Ask a question about a GitHub repository.",
		"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"repoName": map[string]string{"type": "string"}, "question": map[string]string{"type": "string"}}, "required": []string{"repoName", "question"}}},
}

var errNoMCPRegistry = errors.New("this MLflow has no MCP Server Registry")

// addMCP registers the sample server. A new version starts as a draft, gets
// the marker tag, and only then becomes active: a server whose tag could
// not be set is deleted again (a draft can be deleted; an active version
// blocks deletion, sqlalchemy_mixin.py:249-267), so nothing unmarked stays
// behind. The playground lists a registry server only when it is active
// and has an access endpoint (odh-dashboard mcp_helpers.go:139-147).
func addMCP(t *mlflowTarget, log func(string)) (string, error) {
	var srv mcpServer
	found, supported, err := t.registryEntry(mcpServerPath(""), &srv)
	switch {
	case err != nil:
		return "", err
	case !supported:
		return "", errNoMCPRegistry
	case found && srv.Tags[sampleMarkerKey] != SampleMCPServer:
		return "", &sampleRefusal{foreignMessage("MCP server '" + mcpSampleName + "'")}
	}

	if !found {
		created, err := t.createMCPVersion("draft")
		if isAlreadyExists(err) {
			return "", &sampleRefusal{"MCP server '" + mcpSampleName + "' was created by someone else in the meantime, so it is left unchanged."}
		}
		if err != nil {
			return "", err
		}
		// Someone may have created the server between the read above and
		// this version: then the version went into their server, and the tag
		// below would claim it. Only a server holding just this version, made
		// by the same caller, is the tool's.
		if err := t.confirmNewParent("MCP server '"+mcpSampleName+"'", mcpServerPath(""), mcpServerPath("/versions"), "mcp_server_versions", mcpSampleVersion, created.CreatedBy); err != nil {
			if delErr := t.api.sendJSON("DELETE", mcpServerPath("/versions/"+url.PathEscape(mcpSampleVersion)), nil, nil, nil); delErr != nil {
				return "", fmt.Errorf("%w; the version this tool added could not be deleted again (%w): delete version %s of MCP server %s in MLflow", err, delErr, mcpSampleVersion, mcpSampleName)
			}
			return "", err
		}
		if err := t.api.sendJSON("POST", mcpServerPath("/tags"), nil, map[string]string{"key": sampleMarkerKey, "value": SampleMCPServer}, nil); err != nil {
			if delErr := t.api.sendJSON("DELETE", mcpServerPath(""), nil, nil, nil); delErr != nil {
				return "", fmt.Errorf("%w; the new draft server could not be deleted again (%w): delete MCP server %s in MLflow", err, delErr, mcpSampleName)
			}
			return "", err
		}
		log("Registered MCP server '" + mcpSampleName + "' (version " + mcpSampleVersion + ")")
		if err := t.api.sendJSON("PATCH", mcpServerPath(""), nil, map[string]string{"display_name": mcpSampleTitle, "description": mcpSampleDescription}, nil); err != nil {
			return "", err
		}
	}

	var ver struct {
		Status string `json:"status"`
	}
	err = t.api.sendJSON("GET", mcpServerPath("/versions/"+mcpSampleVersion), nil, nil, &ver)
	switch {
	case isNotFound(err):
		if _, err := t.createMCPVersion("active"); err != nil {
			return "", err
		}
	case err != nil:
		return "", err
	case ver.Status != "active":
		// draft -> active and deprecated -> active are allowed transitions
		// (entities/mcp_server.py:26-30).
		if err := t.api.sendJSON("PATCH", mcpServerPath("/versions/"+mcpSampleVersion), nil, map[string]string{"status": "active"}, nil); err != nil {
			return "", err
		}
	}
	if !found || len(srv.AccessEndpoints) == 0 {
		if err := t.api.sendJSON("POST", mcpServerPath("/endpoints"), nil, map[string]string{
			"server_version": mcpSampleVersion, "url": mcpSampleURL, "transport_type": mcpSampleTransport,
		}, nil); err != nil {
			return "", err
		}
		log("Added the endpoint " + mcpSampleURL)
	}
	return "added (active, endpoint " + mcpSampleURL + ")", nil
}

// registryVersion is the part of a created MCP server or skill version the
// tool reads (MCPServerVersionResponse, SkillVersionResponse): its version
// (a string for MCP, a number for skills) and who created it.
type registryVersion struct {
	Version   json.RawMessage `json:"version"`
	CreatedBy *string         `json:"created_by"`
}

func (v registryVersion) id() string { return strings.Trim(string(v.Version), `"`) }

// confirmNewParent checks, after the tool added a version to a parent it did
// not find, that the parent is the one that version created: it holds only
// that version (the list route: mcpservers / skills get on the name,
// rules_base.py:1355-1358, rules_skill_registry.py:80-82), and its
// created_by is the version's. Otherwise someone created the parent in
// between, and it is theirs.
func (t *mlflowTarget) confirmNewParent(what, parentPath, versionsPath, listKey, version string, creator *string) error {
	var parent struct {
		CreatedBy *string `json:"created_by"`
	}
	if err := t.api.sendJSON("GET", parentPath, nil, nil, &parent); err != nil {
		return err
	}
	var list map[string][]registryVersion
	if err := t.api.sendJSON("GET", versionsPath, nil, nil, &list); err != nil {
		return err
	}
	versions := list[listKey]
	foreign := len(versions) != 1 || versions[0].id() != version
	if creator != nil && parent.CreatedBy != nil && *creator != *parent.CreatedBy {
		foreign = true
	}
	if foreign {
		return &sampleRefusal{"Someone else created " + what + " while this tool was adding it, so it is left as they made it; only the version this tool added is deleted again."}
	}
	return nil
}

func (t *mlflowTarget) createMCPVersion(status string) (registryVersion, error) {
	var out registryVersion
	err := t.api.sendJSON("POST", mcpServerPath("/versions"), nil, map[string]any{
		"server_json": map[string]any{
			"name": mcpSampleName, "version": mcpSampleVersion, "title": mcpSampleTitle, "description": mcpSampleDescription,
			"remotes":    []map[string]string{{"type": mcpSampleTransport, "url": mcpSampleURL}},
			"websiteUrl": "https://docs.devin.ai/work-with-devin/deepwiki-mcp",
		},
		"status": status,
		"tools":  mcpSampleTools,
	}, &out)
	return out, err
}

// removeMCP deprecates the active version, then deletes the server (a hard
// delete, refused while a version is active: sqlalchemy_mixin.py:249-267,
// 520-568). active -> deprecated is an allowed transition.
func removeMCP(t *mlflowTarget, log func(string)) (string, error) {
	var srv mcpServer
	found, supported, err := t.registryEntry(mcpServerPath(""), &srv)
	switch {
	case err != nil:
		return "", err
	case !supported || !found:
		return "nothing to remove", nil
	case srv.Tags[sampleMarkerKey] != SampleMCPServer:
		return "", &sampleRefusal{foreignMessage("MCP server '" + mcpSampleName + "'")}
	}
	var ver struct {
		Status string `json:"status"`
	}
	err = t.api.sendJSON("GET", mcpServerPath("/versions/"+mcpSampleVersion), nil, nil, &ver)
	if err != nil && !isNotFound(err) {
		return "", err
	}
	if err == nil && ver.Status == "active" {
		if err := t.api.sendJSON("PATCH", mcpServerPath("/versions/"+mcpSampleVersion), nil, map[string]string{"status": "deprecated"}, nil); err != nil {
			return "", err
		}
		log("Deprecated version " + mcpSampleVersion)
	}
	if err := t.api.sendJSON("DELETE", mcpServerPath(""), nil, nil, nil); err != nil && !isNotFound(err) {
		return "", err
	}
	log("Deleted MCP server '" + mcpSampleName + "'")
	return "removed", nil
}

var errNoSkillRegistry = errors.New("this MLflow has no Skill Registry")

// addSkill registers the sample skill from its pinned Git source (metadata
// only; the server does not fetch it) and marks it. As for MCP, a skill
// whose tag could not be set is deleted again.
func addSkill(t *mlflowTarget, log func(string)) (string, error) {
	var sk skill
	found, supported, err := t.registryEntry(skillPath(""), &sk)
	switch {
	case err != nil:
		return "", err
	case !supported:
		return "", errNoSkillRegistry
	case found && sk.Tags[sampleMarkerKey] != SampleSkill:
		return "", &sampleRefusal{foreignMessage("Skill '" + skillSampleName + "'")}
	case found && sk.LatestVersion != nil:
		return "already present", nil
	}
	var created registryVersion
	if err := t.api.sendJSON("POST", skillPath("/versions"), nil, map[string]string{
		"name": skillSampleName, "source": skillSampleSource, "source_type": "git",
		"ref": skillSampleRef, "subpath": skillSampleSubpath, "status": "active",
	}, &created); err != nil {
		return "", err
	}
	if !found {
		// As for MCP: a skill someone created since the read is theirs.
		if err := t.confirmNewParent("skill '"+skillSampleName+"'", skillPath(""), skillPath("/versions"), "skill_versions", created.id(), created.CreatedBy); err != nil {
			if delErr := t.api.sendJSON("DELETE", skillPath("/versions/"+url.PathEscape(created.id())), nil, nil, nil); delErr != nil {
				return "", fmt.Errorf("%w; the version this tool added could not be deleted again (%w): delete version %s of skill %s in MLflow", err, delErr, created.id(), skillSampleName)
			}
			return "", err
		}
		if err := t.api.sendJSON("POST", skillPath("/tags"), nil, map[string]string{"key": sampleMarkerKey, "value": SampleSkill}, nil); err != nil {
			if delErr := t.api.sendJSON("DELETE", skillPath(""), nil, nil, nil); delErr != nil {
				return "", fmt.Errorf("%w; the new skill could not be deleted again (%w): delete skill %s in MLflow", err, delErr, skillSampleName)
			}
			return "", err
		}
	}
	log("Registered skill '" + skillSampleName + "' from " + skillSampleSource + " (" + skillSampleSubpath + ")")
	return "added", nil
}

// removeSkill deletes the skill: a hard delete with its versions and owned
// artifacts (server/skill_registry/deletion.py:13-37).
func removeSkill(t *mlflowTarget, log func(string)) (string, error) {
	var sk skill
	found, supported, err := t.registryEntry(skillPath(""), &sk)
	switch {
	case err != nil:
		return "", err
	case !supported || !found:
		return "nothing to remove", nil
	case sk.Tags[sampleMarkerKey] != SampleSkill:
		return "", &sampleRefusal{foreignMessage("Skill '" + skillSampleName + "'")}
	}
	if err := t.api.sendJSON("DELETE", skillPath(""), nil, nil, nil); err != nil && !isNotFound(err) {
		return "", err
	}
	log("Deleted skill '" + skillSampleName + "'")
	return "removed", nil
}
