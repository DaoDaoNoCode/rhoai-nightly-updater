package dsc

import (
	"encoding/json"
	"fmt"
	"maps"
	"sort"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/argo"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/components"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/record"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// Merge patches need explicit nulls to delete keys that disappeared from a new spec.
func replacementMergePatch(old, next map[string]any) map[string]any {
	patch := make(map[string]any, len(old)+len(next))
	for key := range old {
		if _, exists := next[key]; !exists {
			patch[key] = nil
		}
	}
	for key, v := range next {
		oldMap, oldIsMap := old[key].(map[string]any)
		newMap, newIsMap := v.(map[string]any)
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
		strings.TrimPrefix(previewed, rhoai.DSCGroup+"/"), kube.NonEmpty(now, "not readable"))
}

// errResetRemovalsChanged: a reset would now remove other management paths
// than the preview listed.
func errResetRemovalsChanged(previewed []string, now []string) error {
	if previewed == nil {
		return fmt.Errorf("this page did not say which components the reset would switch to Removed (it was loaded before an update of this app); refresh and review the changes again. Nothing was changed")
	}
	return fmt.Errorf("the components a reset would switch to Removed changed since the preview (previewed: %s; now: %s); refresh and review the changes again. Nothing was changed",
		kube.NonEmpty(strings.Join(previewed, ", "), "none"), kube.NonEmpty(strings.Join(now, ", "), "none"))
}

// Repair repairs the named DSC in one of the modes the Components page
// previews. expectedAPIVersion is the apiVersion the preview was computed
// at ("datasciencecluster.opendatahub.io/v2"): the repair runs only when the
// DSC is still read at exactly that version, so a conversion that starts
// working after the preview (an operator update) cannot apply defaults of
// another version than the ones reviewed. A reset also carries the
// management paths the preview said it would switch to Removed
// (expectedResetRemovals, never nil from a current page) and runs only
// when it would remove exactly those.
func Repair(c *kube.Client, name, mode, expectedOperatorVersion, expectedAPIVersion string, expectedExtraComponents, expectedResetRemovals []string) (*types.OperationResponse, error) {
	if mode != "remove-invalid" && mode != "remove-extra-components" && mode != "reset-defaults" {
		return nil, fmt.Errorf("invalid DSC repair mode")
	}
	if _, ok := components.DSCAPIVersion(expectedAPIVersion); !ok {
		return nil, errDSCVersionChanged("", "")
	}
	if mode == "reset-defaults" && expectedResetRemovals == nil {
		return nil, errResetRemovalsChanged(nil, nil)
	}
	// Read the named resource again so the repair is computed from fresh
	// values and schema, at the version the Components page reads it at.
	read, err := rhoai.GetServed(c, rhoai.DSCGroup, "/apis/datasciencecluster.opendatahub.io/%s/datascienceclusters/"+name, rhoai.DSCFallbackVersions)
	if err != nil && !rhoai.IsConversionWebhookError(err) {
		return nil, err
	}
	if current := rhoai.DSCGroup + "/" + read.Version; err != nil || current != expectedAPIVersion {
		return nil, errDSCVersionChanged(expectedAPIVersion, read.Version)
	}
	path := fmt.Sprintf("/apis/datasciencecluster.opendatahub.io/%s/datascienceclusters/", read.Version) + name
	var dsc map[string]any
	if err := json.Unmarshal(read.Body, &dsc); err != nil {
		return nil, err
	}
	if argoCD := argo.TrackingOf(c, dsc, rhoai.DSCGroup, "DataScienceCluster"); argoCD.ManagedBy() != "" {
		return &types.OperationResponse{Success: false, Message: argo.GitOpsRefusal("DataScienceCluster "+name, argoCD.ManagedBy(), "this change") + argoCD.UnconfirmedNote(), ErrorCode: kube.ErrorCodeGitOpsManaged}, nil
	}
	oldSpec, _ := dsc["spec"].(map[string]any)
	apiVersion, _ := dsc["apiVersion"].(string)
	schema, err := components.DSCSpecSchema(c, apiVersion)
	if err != nil {
		return nil, fmt.Errorf("cannot repair DSC without its installed schema: %w", err)
	}
	invalid := []string{}
	nextSpec, _ := components.PruneUnknownDSCFields(oldSpec, schema, "spec", &invalid).(map[string]any)
	detail := "Removed invalid DSC fields: " + strings.Join(invalid, ", ")
	if mode == "reset-defaults" || mode == "remove-extra-components" {
		defaults, err := components.FetchDefaultDSCSpec(c, read.Version)
		if err != nil {
			return nil, err
		}
		if defaults.Version != expectedOperatorVersion {
			return nil, fmt.Errorf("operator version changed since the defaults preview; refresh and review defaults for %s", defaults.Version)
		}
		if defaults.Spec["apiVersion"] != apiVersion {
			return nil, fmt.Errorf("sample API version does not match the existing DSC; wait for operator migration to complete")
		}
		defaultSpec, _ := defaults.Spec["spec"].(map[string]any)
		if mode == "remove-extra-components" {
			defaultComponents, _ := defaultSpec["components"].(map[string]any)
			currentComponents, _ := oldSpec["components"].(map[string]any)
			nextComponents := make(map[string]any, len(currentComponents))
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
			nextSpec = make(map[string]any, len(oldSpec))
			maps.Copy(nextSpec, oldSpec)
			nextSpec["components"] = nextComponents
			detail = "Removed DSC components absent from the defaults (" + defaults.SourceDescription + "): " + strings.Join(removed, ", ")
		} else {
			nextSpec = defaultSpec
			// A matching branch may be ahead of the installed build. Never apply unknown sample keys.
			unknownDefaults := []string{}
			components.PruneUnknownDSCFields(nextSpec, schema, "spec", &unknownDefaults)
			if len(unknownDefaults) != 0 {
				return nil, fmt.Errorf("version defaults contain fields unsupported by the installed CRD: %s", strings.Join(unknownDefaults, ", "))
			}
			// The same computation as the preview's ResetRemovals.
			now := components.SchemaComponents(schema, components.RemovedComponents(oldSpec, nextSpec, components.DSCNestsComponents(apiVersion)))
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
	meta, _ := dsc["metadata"].(map[string]any)
	resourceVersion, _ := meta["resourceVersion"].(string)
	if resourceVersion == "" {
		return nil, fmt.Errorf("DSC has no resourceVersion; refusing an unprotected repair")
	}
	// Components the repair switches to Removed (or drops) get the same
	// preconditions as disable-component; with any of them unmet the whole
	// repair is refused: a partial reset would be neither the defaults nor
	// the current DSC.
	if removals := components.SchemaComponents(schema, components.RemovedComponents(oldSpec, nextSpec, components.DSCNestsComponents(apiVersion))); len(removals) > 0 {
		guard := newRemovalGuard(c)
		var blocked, safe, reasons []string
		for _, name := range removals {
			why := guard.blockers(name)
			// MaaS in use (dsc_maas_guard.go), checked here at the repair
			// and not in the preview: the preview is part of the gitops
			// profile, whose ServiceAccount does not list MaaS objects.
			if maas := guard.rc.maasBlocker(name); maas != "" {
				why = append(append([]string{}, why...), maas)
			}
			if len(why) > 0 {
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
	if err := c.PatchJSON(path, map[string]any{
		"metadata": map[string]any{"resourceVersion": resourceVersion},
		"spec":     replacementMergePatch(oldSpec, nextSpec),
	}); err != nil {
		return nil, err
	}
	record.RecordActivity(c, types.ActivityEntry{Timestamp: kube.NowRFC3339(), User: kube.GetUser(c), Action: "repair-dsc", Detail: name + ": " + detail, Success: true})
	return &types.OperationResponse{Success: true, Message: detail, Logs: []string{detail}}, nil
}
