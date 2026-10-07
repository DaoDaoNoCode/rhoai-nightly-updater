package cluster

import (
	"encoding/json"
	"errors"
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
// defaults of the already-read installed operator, in the API version the
// DSC was read at.
func checkDSCCompatibilityFor(c *Client, read dscRead, op *installedOperator, opErr error) *types.DSCCompatibility {
	dsc := read.Object
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
	defaults, err := defaultDSCSpecFor(c.ctx, op, servedVersions(c, dscGroup, dscFallbackVersions), read.Version)
	if err != nil {
		result.DefaultsError = defaultsErrorMessage(read, err)
		return result
	}
	if defaults.Spec["apiVersion"] != apiVersion {
		// defaultDSCSpecFor only returns the version asked for; never compare across versions.
		result.DefaultsError = fmt.Sprintf("Unable to load version-matched defaults: the defaults are %v, the DataScienceCluster is %s", defaults.Spec["apiVersion"], apiVersion)
		return result
	}
	result.DefaultsAPIVersion = apiVersion
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

	// The repair preview: which components a repair would remove, and which
	// of those must not be removed now. Only evaluated when a repair would
	// remove something, so a DSC that matches its defaults costs nothing.
	result.ResetRemovals = schemaComponents(schema, removedComponents(currentSpec, defaultSpec))
	candidates := append([]string{}, result.ResetRemovals...)
	for _, name := range schemaComponents(schema, removedComponents(currentSpec, withoutComponents(currentSpec, result.ExtraComponents))) {
		if !containsString(candidates, name) {
			candidates = append(candidates, name)
		}
	}
	sort.Strings(candidates)
	rc := newRemovalChecker(c)
	for _, name := range candidates {
		if reasons := rc.blockers(name); len(reasons) > 0 {
			result.RemovalBlocks = append(result.RemovalBlocks, types.DSCRemovalBlock{Component: name, Reasons: reasons})
		}
	}
	return result
}

// defaultsErrorMessage explains why the DSC has no defaults to compare
// with. A missing version is spelled out with the reason the DSC was read
// at that version, instead of a sample URL that returned 404.
func defaultsErrorMessage(read dscRead, err error) string {
	var missing *missingDefaultsError
	if !errors.As(err, &missing) {
		return "Unable to load version-matched defaults: " + err.Error()
	}
	var b strings.Builder
	fallback := read.versionFallback()
	if fallback != nil {
		fmt.Fprintf(&b, "This DataScienceCluster can only be read as %s: reading it as %s fails because the operator's conversion webhook cannot convert it. ", fallback.Used, fallback.Version)
	}
	msg := missing.Error()
	b.WriteString(strings.ToUpper(msg[:1]) + msg[1:] + ". ")
	b.WriteString("Component names differ between API versions, so the DataScienceCluster is not compared with defaults of another version.")
	if fallback != nil {
		b.WriteString(" Diagnostics explains the conversion failure and how to fix it.")
	}
	return b.String()
}

// componentState returns spec.components[name].managementState.
func componentState(components map[string]interface{}, name string) (string, bool) {
	v, ok := components[name]
	if !ok {
		return "", false
	}
	m, _ := v.(map[string]interface{})
	state, _ := m["managementState"].(string)
	return state, true
}

// removedComponents returns the components that are not Removed in old and
// are Removed or absent in next: the ones a repair from old to next would
// remove. A component without a state counts as enabled.
func removedComponents(oldSpec, nextSpec map[string]interface{}) []string {
	oldComponents, _ := oldSpec["components"].(map[string]interface{})
	nextComponents, _ := nextSpec["components"].(map[string]interface{})
	var out []string
	for name := range oldComponents {
		if state, _ := componentState(oldComponents, name); state == "Removed" {
			continue
		}
		if state, present := componentState(nextComponents, name); !present || state == "Removed" {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// schemaComponents keeps the names the installed DSC schema defines under
// spec.components: a key the schema does not define was never seen by the
// operator, so dropping it removes nothing. Without a schema every name is
// kept.
func schemaComponents(specSchema map[string]interface{}, names []string) []string {
	properties, _ := specSchema["properties"].(map[string]interface{})
	components, _ := properties["components"].(map[string]interface{})
	known, _ := components["properties"].(map[string]interface{})
	if known == nil {
		return names
	}
	var out []string
	for _, n := range names {
		if _, ok := known[n]; ok {
			out = append(out, n)
		}
	}
	return out
}

// withoutComponents returns spec with the named components dropped.
func withoutComponents(spec map[string]interface{}, names []string) map[string]interface{} {
	components, _ := spec["components"].(map[string]interface{})
	kept := make(map[string]interface{}, len(components))
	for k, v := range components {
		if !containsString(names, k) {
			kept[k] = v
		}
	}
	return map[string]interface{}{"components": kept}
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
	// Read the named resource again so the repair is computed from fresh
	// values and schema, at the version the Components page compared.
	read, err := getServed(c, dscGroup, "/apis/datasciencecluster.opendatahub.io/%s/datascienceclusters/"+name, dscFallbackVersions)
	if err != nil {
		return nil, err
	}
	path := fmt.Sprintf("/apis/datasciencecluster.opendatahub.io/%s/datascienceclusters/", read.Version) + name
	var dsc map[string]interface{}
	if err := json.Unmarshal(read.Body, &dsc); err != nil {
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
		defaults, err := fetchDefaultDSCSpec(c, read.Version)
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
	// Components the repair switches to Removed (or drops) get the same
	// preconditions as disable-component; with any of them unmet the whole
	// repair is refused: a partial reset would be neither the defaults nor
	// the current DSC.
	if removals := schemaComponents(schema, removedComponents(oldSpec, nextSpec)); len(removals) > 0 {
		rc := newRemovalChecker(c)
		var blocked, safe, reasons []string
		for _, name := range removals {
			if why := rc.blockers(name); len(why) > 0 {
				blocked = append(blocked, name)
				reasons = append(reasons, fmt.Sprintf("%s: %s", name, strings.Join(why, "; ")))
			} else {
				safe = append(safe, name)
			}
		}
		if len(blocked) > 0 {
			msg := fmt.Sprintf("Not repairing the DSC: it would set %s to Removed, which could hang in deletion now. Nothing was changed.", strings.Join(blocked, ", "))
			if len(safe) > 0 {
				msg += fmt.Sprintf(" (%s could be removed safely.)", strings.Join(safe, ", "))
			}
			return &types.OperationResponse{Success: false, Message: msg + " " + strings.Join(reasons, " "), Logs: reasons, ErrorCode: "prerequisites"}, nil
		}
		detail += ". Components set to Removed: " + strings.Join(removals, ", ")
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
