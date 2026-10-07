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
	nested := dscNestsComponents(apiVersion)
	result.ResetRemovals = schemaComponents(schema, removedComponents(currentSpec, defaultSpec, nested))
	candidates := append([]string{}, result.ResetRemovals...)
	for _, name := range schemaComponents(schema, removedComponents(currentSpec, withoutComponents(currentSpec, result.ExtraComponents), nested)) {
		if !containsString(candidates, name) {
			candidates = append(candidates, name)
		}
	}
	sort.Strings(candidates)
	guard := newRemovalGuard(c)
	for _, name := range candidates {
		if reasons := guard.blockers(name); len(reasons) > 0 {
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

// dscNestsComponents reports whether DSC components of apiVersion can
// have parts with their own managementState. DataScienceCluster v3 (RHOAI
// 3.6, rhods-operator config/rhoai/samples/datasciencecluster_v3_*) groups
// parts under a component: dashboard {standard, maasPortal} and data
// {featureStore} have no state of their own; kserve {nim, wva}, aigateway
// {modelsAsAService, batchGateway} and workbenches {workbenchesV2} have
// both. v2 and v1 are compared per component, as before.
func dscNestsComponents(apiVersion string) bool {
	version, ok := dscAPIVersion(apiVersion)
	return ok && compareAPIVersions(version, "v2") < 0
}

// managementPaths returns the managementState of every management path of
// spec.components: "kserve" for a component's own state, "kserve.nim" for
// a part (only when nested). A component with neither is listed with ""
// (enabled, as the operator defaults it).
func managementPaths(components map[string]interface{}, nested bool) map[string]string {
	out := map[string]string{}
	for name, v := range components {
		m, _ := v.(map[string]interface{})
		state, hasState := m["managementState"].(string)
		parts := 0
		if nested {
			for part, pv := range m {
				pm, ok := pv.(map[string]interface{})
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

// removedComponents returns the management paths that are enabled in old
// and Removed (or absent) in next: what a repair from old to next would
// remove. A path without a state counts as enabled. Without nesting the
// paths are the component names.
func removedComponents(oldSpec, nextSpec map[string]interface{}, nested bool) []string {
	oldComponents, _ := oldSpec["components"].(map[string]interface{})
	nextComponents, _ := nextSpec["components"].(map[string]interface{})
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

// guardModule maps a management path to the module the removal
// preconditions (removalChecker.blockers) are about. The preconditions are
// per module CR: the refused components, the CR's finalizers, the module
// operator that removes them, and the conversion webhook Services the
// module serves. A part is configured on its component's module CR
// (dashboard.standard and dashboard.maasPortal on the Dashboard CR,
// kserve.nim and kserve.wva on the Kserve CR), so removing any part is
// checked as removing that module, which may refuse a partial removal
// but never lets one through unchecked. The lower-cased component name is
// the module kind (aiHub is AIHub); v2 names map to themselves.
func guardModule(path string) string {
	if m, ok := dscPartModules[path]; ok {
		return m
	}
	component, _, _ := strings.Cut(path, ".")
	return strings.ToLower(component)
}

// removalGuard checks removal preconditions per management path, each
// module once.
type removalGuard struct {
	rc     *removalChecker
	byName map[string][]string
}

func newRemovalGuard(c *Client) *removalGuard {
	return &removalGuard{rc: newRemovalChecker(c), byName: map[string][]string{}}
}

// blockers returns why the management path must not be removed now.
func (g *removalGuard) blockers(path string) []string {
	module := guardModule(path)
	reasons, ok := g.byName[module]
	if !ok {
		reasons = g.rc.blockers(module)
		g.byName[module] = reasons
	}
	return reasons
}

// schemaComponents keeps the paths whose component the installed DSC
// schema defines under spec.components: a key the schema does not define
// was never seen by the operator, so dropping it removes nothing. Without
// a schema every path is kept.
func schemaComponents(specSchema map[string]interface{}, names []string) []string {
	properties, _ := specSchema["properties"].(map[string]interface{})
	components, _ := properties["components"].(map[string]interface{})
	known, _ := components["properties"].(map[string]interface{})
	if known == nil {
		return names
	}
	var out []string
	for _, n := range names {
		component, _, _ := strings.Cut(n, ".")
		if _, ok := known[component]; ok {
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

// errDSCVersionChanged: the DSC is no longer read at the API version the
// user reviewed. Component names differ between versions, so the reviewed
// changes would not be the ones applied.
func errDSCVersionChanged(previewed, now string) error {
	if previewed == "" {
		return fmt.Errorf("this page did not say which DataScienceCluster API version it previewed (it was loaded before an update of this app); refresh and review the changes again. Nothing was changed")
	}
	return fmt.Errorf("the DataScienceCluster's API version changed since the preview (previewed %s, now %s); refresh and review the changes again. Nothing was changed",
		strings.TrimPrefix(previewed, dscGroup+"/"), nonEmpty(now, "not readable"))
}

// errResetRemovalsChanged: a reset would now remove other management paths
// than the preview listed.
func errResetRemovalsChanged(previewed []string, now []string) error {
	if previewed == nil {
		return fmt.Errorf("this page did not say which components the reset would switch to Removed (it was loaded before an update of this app); refresh and review the changes again. Nothing was changed")
	}
	return fmt.Errorf("the components a reset would switch to Removed changed since the preview (previewed: %s; now: %s); refresh and review the changes again. Nothing was changed",
		nonEmpty(strings.Join(previewed, ", "), "none"), nonEmpty(strings.Join(now, ", "), "none"))
}

// RepairDSC repairs the named DSC in one of the modes the Components page
// previews. expectedAPIVersion is the apiVersion the preview was computed
// at ("datasciencecluster.opendatahub.io/v2"): the repair runs only when the
// DSC is still read at exactly that version, so a conversion that starts
// working after the preview (an operator update) cannot apply defaults of
// another version than the ones reviewed. A reset also carries the
// management paths the preview said it would switch to Removed
// (expectedResetRemovals, never nil from a current page) and runs only
// when it would remove exactly those.
func RepairDSC(c *Client, name, mode, expectedOperatorVersion, expectedAPIVersion string, expectedExtraComponents, expectedResetRemovals []string) (*types.OperationResponse, error) {
	if mode != "remove-invalid" && mode != "remove-extra-components" && mode != "reset-defaults" {
		return nil, fmt.Errorf("invalid DSC repair mode")
	}
	if _, ok := dscAPIVersion(expectedAPIVersion); !ok {
		return nil, errDSCVersionChanged("", "")
	}
	if mode == "reset-defaults" && expectedResetRemovals == nil {
		return nil, errResetRemovalsChanged(nil, nil)
	}
	// Read the named resource again so the repair is computed from fresh
	// values and schema, at the version the Components page reads it at.
	read, err := getServed(c, dscGroup, "/apis/datasciencecluster.opendatahub.io/%s/datascienceclusters/"+name, dscFallbackVersions)
	if err != nil && !isConversionWebhookError(err) {
		return nil, err
	}
	if current := dscGroup + "/" + read.Version; err != nil || current != expectedAPIVersion {
		return nil, errDSCVersionChanged(expectedAPIVersion, read.Version)
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
			// The same computation as the preview's ResetRemovals.
			now := schemaComponents(schema, removedComponents(oldSpec, nextSpec, dscNestsComponents(apiVersion)))
			previewed := append([]string{}, expectedResetRemovals...)
			sort.Strings(previewed)
			if strings.Join(previewed, "\n") != strings.Join(now, "\n") {
				return nil, errResetRemovalsChanged(previewed, now)
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
	if removals := schemaComponents(schema, removedComponents(oldSpec, nextSpec, dscNestsComponents(apiVersion))); len(removals) > 0 {
		guard := newRemovalGuard(c)
		var blocked, safe, reasons []string
		for _, name := range removals {
			if why := guard.blockers(name); len(why) > 0 {
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
