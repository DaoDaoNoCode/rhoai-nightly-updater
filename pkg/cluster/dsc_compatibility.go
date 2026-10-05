package cluster

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

func dscSpecSchema(c *Client, apiVersion string) (map[string]interface{}, error) {
	body, _, err := c.get("/apis/apiextensions.k8s.io/v1/customresourcedefinitions/datascienceclusters.datasciencecluster.opendatahub.io")
	if err != nil {
		return nil, err
	}
	var crd struct {
		Spec struct {
			Versions []struct {
				Name   string `json:"name"`
				Served bool   `json:"served"`
				Schema struct {
					OpenAPIV3Schema map[string]interface{} `json:"openAPIV3Schema"`
				} `json:"schema"`
			} `json:"versions"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(body, &crd); err != nil {
		return nil, err
	}
	version := strings.TrimPrefix(apiVersion, "datasciencecluster.opendatahub.io/")
	for _, v := range crd.Spec.Versions {
		if v.Name == version && v.Served {
			properties, _ := v.Schema.OpenAPIV3Schema["properties"].(map[string]interface{})
			if schema, ok := properties["spec"].(map[string]interface{}); ok {
				return schema, nil
			}
		}
	}
	return nil, fmt.Errorf("no served DSC schema found for %s", apiVersion)
}

// Remove unknown keys according to the installed schema. Values (including managementState)
// are deliberately not compared with defaults. Free-form configuration stays intact.
func pruneUnknownDSCFields(value interface{}, schema map[string]interface{}, path string, invalid *[]string) interface{} {
	switch object := value.(type) {
	case map[string]interface{}:
		result := make(map[string]interface{}, len(object))
		properties, _ := schema["properties"].(map[string]interface{})
		additional, hasAdditional := schema["additionalProperties"]
		preserve, _ := schema["x-kubernetes-preserve-unknown-fields"].(bool)
		for key, v := range object {
			childPath := path + "." + key
			childSchema, known := properties[key].(map[string]interface{})
			if !known {
				if extraSchema, ok := additional.(map[string]interface{}); ok {
					childSchema = extraSchema
				} else if preserve || (hasAdditional && additional == true) {
					result[key] = v
					continue
				} else {
					*invalid = append(*invalid, childPath)
					continue
				}
			}
			result[key] = pruneUnknownDSCFields(v, childSchema, childPath, invalid)
		}
		return result
	case []interface{}:
		itemSchema, ok := schema["items"].(map[string]interface{})
		if !ok {
			return value
		}
		result := make([]interface{}, len(object))
		for i, v := range object {
			result[i] = pruneUnknownDSCFields(v, itemSchema, fmt.Sprintf("%s[%d]", path, i), invalid)
		}
		return result
	default:
		return value
	}
}

// checkDSCCompatibilityFor compares a DSC with the installed schema and the
// defaults of the already-read installed operator.
func checkDSCCompatibilityFor(c *Client, dsc map[string]interface{}, op *installedOperator, opErr error) *types.DSCCompatibility {
	result := &types.DSCCompatibility{InvalidFields: []string{}, MissingComponents: []string{}, ExtraComponents: []string{}}
	apiVersion, _ := dsc["apiVersion"].(string)
	schema, err := dscSpecSchema(c, apiVersion)
	if err != nil {
		result.ValidationError = "Unable to check DSC field names: " + err.Error()
	} else {
		pruneUnknownDSCFields(dsc["spec"], schema, "spec", &result.InvalidFields)
		sort.Strings(result.InvalidFields)
	}
	if opErr != nil {
		result.DefaultsError = "Unable to load version-matched defaults: " + opErr.Error()
		return result
	}
	defaults, err := defaultDSCSpecFor(c.ctx, op)
	if err != nil {
		result.DefaultsError = "Unable to load version-matched defaults: " + err.Error()
		return result
	}
	result.OperatorVersion = defaults.Version
	result.Branch = defaults.Branch
	result.SourceURL = defaults.SourceURL
	result.DefaultsSource = defaults.Source
	defaultSpec, _ := defaults.Spec["spec"].(map[string]interface{})
	defaultComponents, _ := defaultSpec["components"].(map[string]interface{})
	currentSpec, _ := dsc["spec"].(map[string]interface{})
	currentComponents, _ := currentSpec["components"].(map[string]interface{})
	for key := range defaultComponents {
		if _, exists := currentComponents[key]; !exists {
			result.MissingComponents = append(result.MissingComponents, key)
		}
	}
	for key := range currentComponents {
		if _, exists := defaultComponents[key]; !exists {
			result.ExtraComponents = append(result.ExtraComponents, key)
		}
	}
	sort.Strings(result.MissingComponents)
	sort.Strings(result.ExtraComponents)
	return result
}

// Merge patches need explicit nulls to delete keys that disappeared from a new spec.
func replacementMergePatch(old, next map[string]interface{}) map[string]interface{} {
	patch := make(map[string]interface{}, len(old)+len(next))
	for key := range old {
		if _, exists := next[key]; !exists {
			patch[key] = nil
		}
	}
	for key, v := range next {
		oldMap, oldIsMap := old[key].(map[string]interface{})
		newMap, newIsMap := v.(map[string]interface{})
		if oldIsMap && newIsMap {
			patch[key] = replacementMergePatch(oldMap, newMap)
		} else {
			patch[key] = v
		}
	}
	return patch
}

func RepairDSC(c *Client, name, mode, expectedOperatorVersion string, expectedExtraComponents []string) (*types.OperationResponse, error) {
	if mode != "remove-invalid" && mode != "remove-extra-components" && mode != "reset-defaults" {
		return nil, fmt.Errorf("invalid DSC repair mode")
	}
	// Read the named resource again so the repair is computed from fresh values and schema.
	path := "/apis/datasciencecluster.opendatahub.io/v2/datascienceclusters/" + name
	body, _, err := c.get(path)
	if IsK8sError(err, 404) {
		path = "/apis/datasciencecluster.opendatahub.io/v1/datascienceclusters/" + name
		body, _, err = c.get(path)
	}
	if err != nil {
		return nil, err
	}
	var dsc map[string]interface{}
	if err := json.Unmarshal(body, &dsc); err != nil {
		return nil, err
	}
	oldSpec, _ := dsc["spec"].(map[string]interface{})
	apiVersion, _ := dsc["apiVersion"].(string)
	schema, err := dscSpecSchema(c, apiVersion)
	if err != nil {
		return nil, fmt.Errorf("cannot repair DSC without its installed schema: %w", err)
	}
	invalid := []string{}
	nextSpec, _ := pruneUnknownDSCFields(oldSpec, schema, "spec", &invalid).(map[string]interface{})
	detail := "Removed invalid DSC fields: " + strings.Join(invalid, ", ")
	if mode == "reset-defaults" || mode == "remove-extra-components" {
		defaults, err := fetchDefaultDSCSpec(c)
		if err != nil {
			return nil, err
		}
		if defaults.Version != expectedOperatorVersion {
			return nil, fmt.Errorf("operator version changed since the defaults preview; refresh and review defaults for %s", defaults.Version)
		}
		if defaults.Spec["apiVersion"] != apiVersion {
			return nil, fmt.Errorf("sample API version does not match the existing DSC; wait for operator migration to complete")
		}
		defaultSpec, _ := defaults.Spec["spec"].(map[string]interface{})
		if mode == "remove-extra-components" {
			defaultComponents, _ := defaultSpec["components"].(map[string]interface{})
			currentComponents, _ := oldSpec["components"].(map[string]interface{})
			nextComponents := make(map[string]interface{}, len(currentComponents))
			removed := []string{}
			for key, value := range currentComponents {
				if _, exists := defaultComponents[key]; exists {
					nextComponents[key] = value
				} else {
					removed = append(removed, key)
				}
			}
			if len(removed) == 0 {
				return &types.OperationResponse{Success: true, Message: "No extra DSC components to remove", Logs: []string{}}, nil
			}
			expected := make(map[string]bool, len(expectedExtraComponents))
			for _, name := range expectedExtraComponents {
				expected[name] = true
			}
			if len(removed) != len(expected) {
				return nil, fmt.Errorf("DSC components or version defaults changed since confirmation; refresh and review the extra components again")
			}
			for _, name := range removed {
				if !expected[name] {
					return nil, fmt.Errorf("DSC components or version defaults changed since confirmation; refresh and review the extra components again")
				}
			}
			sort.Strings(removed)
			nextSpec = make(map[string]interface{}, len(oldSpec))
			for key, value := range oldSpec {
				nextSpec[key] = value
			}
			nextSpec["components"] = nextComponents
			detail = "Removed DSC components absent from the defaults (" + defaults.SourceDescription + "): " + strings.Join(removed, ", ")
		} else {
			nextSpec = defaultSpec
			// A matching branch may be ahead of the installed build. Never apply unknown sample keys.
			unknownDefaults := []string{}
			pruneUnknownDSCFields(nextSpec, schema, "spec", &unknownDefaults)
			if len(unknownDefaults) != 0 {
				return nil, fmt.Errorf("version defaults contain fields unsupported by the installed CRD: %s", strings.Join(unknownDefaults, ", "))
			}
			detail = "Reset DSC spec to defaults from " + defaults.SourceDescription
		}
	} else if len(invalid) == 0 {
		return &types.OperationResponse{Success: true, Message: "No invalid DSC fields to remove", Logs: []string{}}, nil
	}
	meta, _ := dsc["metadata"].(map[string]interface{})
	resourceVersion, _ := meta["resourceVersion"].(string)
	if resourceVersion == "" {
		return nil, fmt.Errorf("DSC has no resourceVersion; refusing an unprotected repair")
	}
	patch, err := json.Marshal(map[string]interface{}{
		"metadata": map[string]interface{}{"resourceVersion": resourceVersion},
		"spec":     replacementMergePatch(oldSpec, nextSpec),
	})
	if err != nil {
		return nil, err
	}
	if _, _, err := c.patch(path, patch); err != nil {
		return nil, err
	}
	RecordActivity(c, types.ActivityEntry{Timestamp: time.Now().UTC().Format(time.RFC3339), User: getUser(c), Action: "repair-dsc", Detail: name + ": " + detail, Success: true})
	return &types.OperationResponse{Success: true, Message: detail, Logs: []string{detail}}, nil
}
