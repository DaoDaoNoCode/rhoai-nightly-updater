package dsc

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/components"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// CheckCompatibilityFor compares a DSC with the installed schema and the
// defaults of the already-read installed operator, in the API version the
// DSC was read at.
func CheckCompatibilityFor(c *kube.Client, read rhoai.DSCRead, op *components.InstalledOperator, opErr error, fetchSample components.DSCSampleFetcher) *types.DSCCompatibility {
	dsc := read.Object
	result := &types.DSCCompatibility{InvalidFields: []string{}, MissingComponents: []string{}, ExtraComponents: []string{}}
	apiVersion, _ := dsc["apiVersion"].(string)
	schema, err := components.DSCSpecSchema(c, apiVersion)
	if err != nil {
		result.ValidationError = "Unable to check DSC field names: " + err.Error()
	} else {
		components.PruneUnknownDSCFields(dsc["spec"], schema, "spec", &result.InvalidFields)
		sort.Strings(result.InvalidFields)
	}
	if opErr != nil {
		result.DefaultsError = "Unable to load version-matched defaults: " + opErr.Error()
		return result
	}
	defaults, err := components.DefaultDSCSpecFrom(c.Context(), op, rhoai.ServedVersions(c, rhoai.DSCGroup, rhoai.DSCFallbackVersions), read.Version, fetchSample)
	if err != nil {
		result.DefaultsError = defaultsErrorMessage(read, err)
		return result
	}
	if defaults.Spec["apiVersion"] != apiVersion {
		// components.DefaultDSCSpecFor only returns the version asked for; never compare across versions.
		result.DefaultsError = fmt.Sprintf("Unable to load version-matched defaults: the defaults are %v, the DataScienceCluster is %s", defaults.Spec["apiVersion"], apiVersion)
		return result
	}
	result.DefaultsAPIVersion = apiVersion
	result.OperatorVersion = defaults.Version
	result.Branch = defaults.Branch
	result.SourceURL = defaults.SourceURL
	result.DefaultsSource = defaults.Source
	defaultSpec, _ := defaults.Spec["spec"].(map[string]any)
	defaultComponents, _ := defaultSpec["components"].(map[string]any)
	currentSpec, _ := dsc["spec"].(map[string]any)
	currentComponents, _ := currentSpec["components"].(map[string]any)
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
	nested := components.DSCNestsComponents(apiVersion)
	result.ResetRemovals = components.SchemaComponents(schema, components.RemovedComponents(currentSpec, defaultSpec, nested))
	candidates := append([]string{}, result.ResetRemovals...)
	for _, name := range components.SchemaComponents(schema, components.RemovedComponents(currentSpec, withoutComponents(currentSpec, result.ExtraComponents), nested)) {
		if !slices.Contains(candidates, name) {
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
func defaultsErrorMessage(read rhoai.DSCRead, err error) string {
	var missing *components.MissingDefaultsError
	if !errors.As(err, &missing) {
		return "Unable to load version-matched defaults: " + err.Error()
	}
	var b strings.Builder
	fallback := read.VersionFallback()
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

// removalGuard checks removal preconditions per management path, each
// module once.
type removalGuard struct {
	rc     *RemovalChecker
	byName map[string][]string
}

func newRemovalGuard(c *kube.Client) *removalGuard {
	return &removalGuard{rc: NewRemovalChecker(c), byName: map[string][]string{}}
}

// blockers returns why the management path must not be removed now.
func (g *removalGuard) blockers(path string) []string {
	module := components.GuardModule(path)
	reasons, ok := g.byName[module]
	if !ok {
		reasons = g.rc.blockers(module)
		g.byName[module] = reasons
	}
	return reasons
}

// withoutComponents returns spec with the named components dropped.
func withoutComponents(spec map[string]any, names []string) map[string]any {
	components, _ := spec["components"].(map[string]any)
	kept := make(map[string]any, len(components))
	for k, v := range components {
		if !slices.Contains(names, k) {
			kept[k] = v
		}
	}
	return map[string]any{"components": kept}
}
