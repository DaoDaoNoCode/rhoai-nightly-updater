package components

import (
	"fmt"
	"sort"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
)

func DSCSpecSchema(c *kube.Client, apiVersion string) (map[string]any, error) {
	var crd struct {
		Spec struct {
			Versions []struct {
				Name   string `json:"name"`
				Served bool   `json:"served"`
				Schema struct {
					OpenAPIV3Schema map[string]any `json:"openAPIV3Schema"`
				} `json:"schema"`
			} `json:"versions"`
		} `json:"spec"`
	}
	if err := c.GetInto("/apis/apiextensions.k8s.io/v1/customresourcedefinitions/datascienceclusters.datasciencecluster.opendatahub.io", &crd); err != nil {
		return nil, err
	}
	version := strings.TrimPrefix(apiVersion, "datasciencecluster.opendatahub.io/")
	for _, v := range crd.Spec.Versions {
		if v.Name == version && v.Served {
			properties, _ := v.Schema.OpenAPIV3Schema["properties"].(map[string]any)
			if schema, ok := properties["spec"].(map[string]any); ok {
				return schema, nil
			}
		}
	}
	return nil, fmt.Errorf("no served DSC schema found for %s", apiVersion)
}

// Remove unknown keys according to the installed schema. Values (including managementState)
// are deliberately not compared with defaults. Free-form configuration stays intact.
func PruneUnknownDSCFields(value any, schema map[string]any, path string, invalid *[]string) any {
	switch object := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(object))
		properties, _ := schema["properties"].(map[string]any)
		additional, hasAdditional := schema["additionalProperties"]
		preserve, _ := schema["x-kubernetes-preserve-unknown-fields"].(bool)
		for key, v := range object {
			childPath := path + "." + key
			childSchema, known := properties[key].(map[string]any)
			if !known {
				if extraSchema, ok := additional.(map[string]any); ok {
					childSchema = extraSchema
				} else if preserve || (hasAdditional && additional == true) {
					result[key] = v
					continue
				} else {
					*invalid = append(*invalid, childPath)
					continue
				}
			}
			result[key] = PruneUnknownDSCFields(v, childSchema, childPath, invalid)
		}
		return result
	case []any:
		itemSchema, ok := schema["items"].(map[string]any)
		if !ok {
			return value
		}
		result := make([]any, len(object))
		for i, v := range object {
			result[i] = PruneUnknownDSCFields(v, itemSchema, fmt.Sprintf("%s[%d]", path, i), invalid)
		}
		return result
	default:
		return value
	}
}

// DSCNestsComponents reports whether DSC components of apiVersion can
// have parts with their own managementState. DataScienceCluster v3 (RHOAI
// 3.6, rhods-operator config/rhoai/samples/datasciencecluster_v3_*) groups
// parts under a component: dashboard {standard, maasPortal} and data
// {featureStore} have no state of their own; kserve {nim, wva}, aigateway
// {modelsAsAService, batchGateway} and workbenches {workbenchesV2} have
// both. v2 and v1 are compared per component, as before.
func DSCNestsComponents(apiVersion string) bool {
	version, ok := DSCAPIVersion(apiVersion)
	return ok && rhoai.CompareAPIVersions(version, "v2") < 0
}

// managementPaths returns the managementState of every management path of
// spec.components: "kserve" for a component's own state, "kserve.nim" for
// a part (only when nested). A component with neither is listed with ""
// (enabled, as the operator defaults it).
func managementPaths(components map[string]any, nested bool) map[string]string {
	out := map[string]string{}
	for name, v := range components {
		m, _ := v.(map[string]any)
		state, hasState := m["managementState"].(string)
		parts := 0
		if nested {
			for part, pv := range m {
				pm, ok := pv.(map[string]any)
				if !ok {
					continue
				}
				if ps, ok := pm["managementState"].(string); ok {
					out[name+"."+part] = ps
					parts++
				}
			}
		}
		if hasState || parts == 0 {
			out[name] = state
		}
	}
	return out
}

// pathRemoved reports whether a management path is Removed in paths: its
// own state is Removed, it is absent, or its component is Removed (a
// component switched to Removed removes its parts).
func pathRemoved(paths map[string]string, path string) bool {
	state, present := paths[path]
	if !present || state == "Removed" {
		return true
	}
	if component, _, isPart := strings.Cut(path, "."); isPart {
		if s, ok := paths[component]; ok && s == "Removed" {
			return true
		}
	}
	return false
}

// RemovedComponents returns the management paths that are enabled in old
// and Removed (or absent) in next: what a repair from old to next would
// remove. A path without a state counts as enabled. Without nesting the
// paths are the component names.
func RemovedComponents(oldSpec, nextSpec map[string]any, nested bool) []string {
	oldComponents, _ := oldSpec["components"].(map[string]any)
	nextComponents, _ := nextSpec["components"].(map[string]any)
	oldPaths := managementPaths(oldComponents, nested)
	nextPaths := managementPaths(nextComponents, nested)
	var out []string
	for path := range oldPaths {
		if pathRemoved(oldPaths, path) {
			continue
		}
		if pathRemoved(nextPaths, path) {
			out = append(out, path)
		}
	}
	sort.Strings(out)
	return out
}

// dscPartModules maps the DataScienceCluster v3 parts whose component is
// not a module of its own to the module that serves them
// (components.platform.opendatahub.io kind FeastOperator).
var dscPartModules = map[string]string{
	"data.featureStore": "feastoperator",
}

// GuardModule maps a management path to the module the removal
// preconditions (removalChecker.blockers) are about. The preconditions are
// per module CR: the refused components, the CR's finalizers, the module
// operator that removes them, and the conversion webhook Services the
// module serves. A part is configured on its component's module CR
// (dashboard.standard and dashboard.maasPortal on the Dashboard CR,
// kserve.nim and kserve.wva on the Kserve CR), so removing any part is
// checked as removing that module, which may refuse a partial removal
// but never lets one through unchecked. The lower-cased component name is
// the module kind (aiHub is AIHub); v2 names map to themselves.
func GuardModule(path string) string {
	if m, ok := dscPartModules[path]; ok {
		return m
	}
	component, _, _ := strings.Cut(path, ".")
	return strings.ToLower(component)
}

// SchemaComponents keeps the management paths the installed DSC schema
// defines under spec.components, segment by segment: a component (and a
// part of it, components.properties[component].properties[part]) the
// schema does not define was never seen by the operator, so dropping it
// removes nothing. Free-form levels (x-kubernetes-preserve-unknown-fields or
// additionalProperties) keep what they hold: the API server stored it, so the
// operator may act on it. Without a schema every path is kept.
func SchemaComponents(specSchema map[string]any, names []string) []string {
	properties, _ := specSchema["properties"].(map[string]any)
	components, _ := properties["components"].(map[string]any)
	if components == nil {
		return names
	}
	known, _ := components["properties"].(map[string]any)
	var out []string
	for _, n := range names {
		component, part, isPart := strings.Cut(n, ".")
		if !schemaDefines(components, component) {
			continue
		}
		// A component accepted only by a free-form components schema has no
		// schema of its own, so its parts are kept too.
		if componentSchema, ok := known[component].(map[string]any); ok && isPart && !schemaDefines(componentSchema, part) {
			continue
		}
		out = append(out, n)
	}
	return out
}

// schemaDefines reports whether an object schema accepts the key: it names
// it, or it is free-form.
func schemaDefines(schema map[string]any, key string) bool {
	if props, _ := schema["properties"].(map[string]any); props != nil {
		if _, ok := props[key]; ok {
			return true
		}
	}
	if preserve, _ := schema["x-kubernetes-preserve-unknown-fields"].(bool); preserve {
		return true
	}
	switch additional := schema["additionalProperties"].(type) {
	case bool:
		return additional
	case map[string]any:
		return true
	}
	return false
}
