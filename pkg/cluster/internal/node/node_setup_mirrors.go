package node

import (
	"fmt"
	"slices"
	"strings"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// The image mirror objects that the nodes' /etc/containers/registries.conf
// is rendered from, read the way the Machine Config Operator reads them
// (openshift/runtime-utils @7bdb5b9c pkg/registries/registries.go
// EditRegistriesConfig, :235-350):
//   - ImageDigestMirrorSet (config.openshift.io/v1) and
//     ImageContentSourcePolicy (operator.openshift.io/v1alpha1) are merged
//     into one set of digest-only mirrors per source
//     (mergedDigestMirrorSets, :165-183);
//   - ImageTagMirrorSet (config.openshift.io/v1) gives tag-only mirrors
//     into the same registries.conf entry of its source (:285-289);
//   - an entry whose mirrors only repeat its source is ignored
//     (mirrorsContainsARealMirror, :39-47; addMirrorSet, :62-64);
//   - the mirrors of a source are copied to a more specific scope inside it
//     only while that scope's entry has no mirror of its own (:331-345),
//     and the nodes use only the most specific match (openshift/api
//     @41d93514 config/v1/types_image_digest_mirror_set.go:45-49: "only
//     the objects which define the most specific namespace match will be
//     used").

// The kinds of mirror objects.
const (
	KindIDMS       = "ImageDigestMirrorSet"
	KindICSP       = "ImageContentSourcePolicy"
	mirrorKindITMS = "ImageTagMirrorSet"
)

// MirrorRule is one source entry of an ImageDigestMirrorSet,
// ImageContentSourcePolicy or ImageTagMirrorSet.
type MirrorRule struct {
	Kind, Name string
	Source     string
	Mirrors    []string
	// Policy is the mirrorSourcePolicy of an IDMS or ITMS entry; "" is the
	// default, AllowContactingSource (an ICSP has none).
	Policy string
}

// digest: the rule's mirrors serve pulls by digest (IDMS, ICSP); an ITMS's
// serve pulls by tag.
func (r MirrorRule) digest() bool { return r.Kind != mirrorKindITMS }

func (r MirrorRule) ref() string { return r.Kind + " " + r.Name }

// hasRealMirror: at least one mirror that is not the source itself
// (runtime-utils registries.go mirrorsContainsARealMirror); a rule without
// one is ignored when registries.conf is rendered.
func (r MirrorRule) hasRealMirror() bool {
	for _, m := range r.Mirrors {
		if m != r.Source {
			return true
		}
	}
	return false
}

// redirectsToQuay: the rule serves pulls of IDMSSource by digest from
// mirrorTarget, which is what the image mirror of this setup is for.
func (r MirrorRule) redirectsToQuay() bool {
	return r.digest() && r.Source == rhoai.IDMSSource && slices.Contains(r.Mirrors, mirrorTarget)
}

// ScopeIsNestedInsideScope is runtime-utils registries.go
// ScopeIsNestedInsideScope (:13-36): subScope is superScope, a namespace or
// repository inside it, or matches its wildcard host ("*.example.com").
func ScopeIsNestedInsideScope(subScope, superScope string) bool {
	if superScope == subScope {
		return true
	}
	if len(subScope) > len(superScope) && strings.HasPrefix(subScope, superScope) && subScope[len(superScope)] == '/' {
		return true
	}
	if strings.HasPrefix(superScope, "*.") {
		if strings.Contains(subScope, ":") {
			host := strings.Split(subScope, ":")[0]
			return strings.HasSuffix(host, superScope[1:]) && !strings.Contains(host, "/")
		}
		return strings.HasSuffix(strings.Split(subScope, "/")[0], superScope[1:])
	}
	return false
}

// pathFor is where mirror m of rule r has the images of IDMSSource: the
// part of IDMSSource below r's source appended to m, as the MCO copies a
// mirror into a nested scope (runtime-utils registries.go
// mirrorsAdjustedForNestedScope, :186-210). For a wildcard source the part
// below the host is appended, as the API documents
// (types_image_digest_mirror_set.go:117-119: source *.redhat.io, mirror
// mirror.local/redhat, image registry.redhat.io/product/repo:
// mirror.local/redhat/product/repo).
func (r MirrorRule) pathFor(m string) string {
	if strings.HasPrefix(r.Source, "*.") {
		host, _, _ := strings.Cut(rhoai.IDMSSource, "/")
		return m + rhoai.IDMSSource[len(host):]
	}
	return m + rhoai.IDMSSource[len(r.Source):]
}

// idmsRules are the rules of the ImageDigestMirrorSets listed.
func idmsRules(items []idmsItem) []MirrorRule {
	var out []MirrorRule
	for _, m := range items {
		for _, e := range m.Spec.ImageDigestMirrors {
			out = append(out, MirrorRule{Kind: KindIDMS, Name: m.Metadata.Name, Source: e.Source, Mirrors: e.Mirrors, Policy: e.MirrorSourcePolicy})
		}
	}
	return out
}

// listICSPRules lists the ImageContentSourcePolicies' rules
// (spec.repositoryDigestMirrors). A 404 for the collection means the API
// is not served, so no such object exists; any other failure is an error.
func listICSPRules(c *kube.Client) ([]MirrorRule, error) {
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				RepositoryDigestMirrors []struct {
					Source  string   `json:"source"`
					Mirrors []string `json:"mirrors"`
				} `json:"repositoryDigestMirrors"`
			} `json:"spec"`
		} `json:"items"`
	}
	found, err := c.GetIntoOptional(rhoai.ClusterPath("operator.openshift.io/v1alpha1", "imagecontentsourcepolicies", ""), &list)
	if err != nil {
		if kube.IsDecodeError(err) {
			return nil, fmt.Errorf("parse ImageContentSourcePolicies: %w", err)
		}
		return nil, err
	}
	if !found {
		return nil, nil
	}
	var out []MirrorRule
	for _, p := range list.Items {
		for _, e := range p.Spec.RepositoryDigestMirrors {
			out = append(out, MirrorRule{Kind: KindICSP, Name: p.Metadata.Name, Source: e.Source, Mirrors: e.Mirrors})
		}
	}
	return out, nil
}

// listITMSRules lists the ImageTagMirrorSets' rules (spec.imageTagMirrors),
// with the same 404 handling as listICSPRules.
func listITMSRules(c *kube.Client) ([]MirrorRule, error) {
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				ImageTagMirrors []struct {
					Source             string   `json:"source"`
					Mirrors            []string `json:"mirrors"`
					MirrorSourcePolicy string   `json:"mirrorSourcePolicy"`
				} `json:"imageTagMirrors"`
			} `json:"spec"`
		} `json:"items"`
	}
	found, err := c.GetIntoOptional(rhoai.ClusterPath("config.openshift.io/v1", "imagetagmirrorsets", ""), &list)
	if err != nil {
		if kube.IsDecodeError(err) {
			return nil, fmt.Errorf("parse ImageTagMirrorSets: %w", err)
		}
		return nil, err
	}
	if !found {
		return nil, nil
	}
	var out []MirrorRule
	for _, s := range list.Items {
		for _, e := range s.Spec.ImageTagMirrors {
			out = append(out, MirrorRule{Kind: mirrorKindITMS, Name: s.Metadata.Name, Source: e.Source, Mirrors: e.Mirrors, Policy: e.MirrorSourcePolicy})
		}
	}
	return out, nil
}

// listMirrorRules lists the rules of every mirror object: the
// ImageDigestMirrorSets (also returned as read, for the removal's
// preconditions), the ImageContentSourcePolicies and the
// ImageTagMirrorSets. what names the list that failed.
func listMirrorRules(c *kube.Client) (items []idmsItem, rules []MirrorRule, what string, err error) {
	if items, err = listIDMS(c); err != nil {
		return nil, nil, "ImageDigestMirrorSets", err
	}
	rules = idmsRules(items)
	icsp, err := listICSPRules(c)
	if err != nil {
		return nil, nil, "ImageContentSourcePolicies", err
	}
	itms, err := listITMSRules(c)
	if err != nil {
		return nil, nil, "ImageTagMirrorSets", err
	}
	return items, append(append(rules, icsp...), itms...), "", nil
}

// parentScopeRules are the rules whose mirrors the nodes use for
// IDMSSource now and would stop using once a rule for exactly IDMSSource
// exists: rules with a real mirror for a source that IDMSSource is nested
// in (a parent namespace, the registry, a wildcard host), while no rule of
// any kind with a real mirror has exactly IDMSSource as its source (with
// one, the nodes already use only that entry; registries.go:331-345).
func parentScopeRules(rules []MirrorRule) []MirrorRule {
	var parents []MirrorRule
	for _, r := range rules {
		if !r.hasRealMirror() {
			continue
		}
		if r.Source == rhoai.IDMSSource {
			return nil
		}
		if ScopeIsNestedInsideScope(rhoai.IDMSSource, r.Source) {
			parents = append(parents, r)
		}
	}
	return parents
}

// policyConflictRules are the rules for exactly IDMSSource whose
// mirrorSourcePolicy is not the default. The Machine Config Operator
// refuses the whole registries configuration when two IDMS, ITMS or ICSP
// entries for one source set different policies ("conflicting
// mirrorSourcePolicy is set for the same source"; machine-config-operator
// @4dabc4b5 pkg/controller/container-runtime-config/helpers.go
// validateRegistriesConfScopes, :961-975, before EditRegistriesConfig,
// :590-596), and the setup's IDMS has the default (an ICSP counts as the
// default too: convertICSPToIDMS sets none). Creating it next to one of
// these would stop every mirror change from reaching the nodes.
func policyConflictRules(rules []MirrorRule) []MirrorRule {
	var out []MirrorRule
	for _, r := range rules {
		if r.Source == rhoai.IDMSSource && r.Policy != "" && r.Policy != "AllowContactingSource" {
			out = append(out, r)
		}
	}
	return out
}

// policyConflictRefusal explains the refusal of policyConflictRules.
func policyConflictRefusal(conflicts []MirrorRule) string {
	names := make([]string, len(conflicts))
	for i, r := range conflicts {
		names[i] = r.ref()
	}
	return fmt.Sprintf("%s sets mirrorSourcePolicy %s for %s. ImageDigestMirrorSet %s would leave it at the default, AllowContactingSource, and the Machine Config Operator refuses the cluster's whole mirror configuration while two objects set different policies for one source, so no mirror change would reach the nodes. So it is not created. Nothing was changed. "+
		"To use %s, the cluster's administrator adds it to the mirrors of an ImageDigestMirrorSet entry for %s that has the same mirrorSourcePolicy (the entry there, if it is an ImageDigestMirrorSet's).", strings.Join(names, ", "), conflicts[0].Policy, rhoai.IDMSSource, MirrorName, mirrorTarget, rhoai.IDMSSource)
}

// ParentScopeRefusal explains why the mirror is not created over parents:
// which object, what it maps, what creating the mirror would change, and
// what to do instead.
func ParentScopeRefusal(parents []MirrorRule) string {
	var b strings.Builder
	digest, tag := false, false
	for _, r := range parents {
		pulls := "by digest"
		if r.digest() {
			digest = true
		} else {
			pulls, tag = "by tag", true
		}
		fmt.Fprintf(&b, "%s maps %s to %s, so pulls of %s images %s go to %s now. ", r.ref(), r.Source, strings.Join(r.Mirrors, ", "), rhoai.IDMSSource, pulls, strings.Join(r.mirrorPaths(), ", "))
	}
	fmt.Fprintf(&b, "The nodes use only the mirrors of the most specific source that matches an image, and the Machine Config Operator copies a mirror to a namespace inside its source only while that namespace has no mirror of its own. "+
		"Creating ImageDigestMirrorSet %s, a mirror for exactly %s, would take those pulls away from that mirror:", MirrorName, rhoai.IDMSSource)
	if digest {
		fmt.Fprintf(&b, " pulls by digest would go to %s, then to registry.redhat.io itself", mirrorTarget)
	}
	if digest && tag {
		b.WriteString(";")
	}
	if tag {
		b.WriteString(" pulls by tag would go to registry.redhat.io itself")
	}
	b.WriteString(". So it is not created. Nothing was changed. To keep that mirror in use, the cluster's administrator adds an entry for " + rhoai.IDMSSource + " to that object, ")
	hints := make([]string, len(parents))
	for i, r := range parents {
		hints[i] = manualEntryHint(r)
	}
	b.WriteString(strings.Join(hints, "; and "))
	b.WriteString(". Or whoever runs that mirror makes it carry the nightly images at that path.")
	return b.String()
}

// mirrorRemoveImpactFor words what the nodes do when the setup's IDMS is
// deleted and remaining stays, and how to undo it. others are the
// remaining objects that redirect IDMSSource to mirrorTarget.
func mirrorRemoveImpactFor(remaining []MirrorRule, others []string) (impact, undo string) {
	undo = "Create the image mirror again (one click, no drain)."
	if len(others) > 0 {
		return fmt.Sprintf(mirrorRemoveKeptImpact, strings.Join(others, ", ")), undo
	}
	// The remaining mirrors for exactly the source serve it; without any,
	// those of a parent scope do again (registries.go:331-345).
	var same []string
	for _, r := range remaining {
		if r.Source == rhoai.IDMSSource && r.hasRealMirror() && r.digest() {
			same = append(same, fmt.Sprintf("%s maps it to %s", r.ref(), strings.Join(r.Mirrors, ", ")))
		}
	}
	if len(same) > 0 {
		return mirrorRemoveDrain + " After that, pulls of registry.redhat.io/rhoai images by digest go to the mirrors that stay for it (" + strings.Join(same, "; ") + "), then to registry.redhat.io itself: unless those mirrors carry the nightly builds, installing or updating a nightly fails until the mirror is back. Images already on a node stay.", undo
	}
	parents := parentScopeRules(remaining)
	if len(parents) == 0 {
		return mirrorRemoveImpact, undo
	}
	var back []string
	for _, r := range parents {
		pulls := "by digest"
		if !r.digest() {
			pulls = "by tag"
		}
		back = append(back, fmt.Sprintf("%s maps %s, so pulls %s go to %s", r.ref(), r.Source, pulls, strings.Join(r.mirrorPaths(), ", ")))
	}
	return mirrorRemoveDrain + " After that, registry.redhat.io/rhoai images are pulled as before the mirror existed (" + strings.Join(back, "; ") + "): unless that mirror carries the nightly builds, installing or updating a nightly fails until the mirror is back. Images already on a node stay.",
		"Creating the image mirror again here is refused while " + parents[0].ref() + " maps " + parents[0].Source + " (it would take those pulls away from that mirror, which the review explains); the cluster's administrator can add the entry to that object instead."
}

// mirrorPaths are r's real mirrors as the paths that serve IDMSSource.
func (r MirrorRule) mirrorPaths() []string {
	var out []string
	for _, m := range r.Mirrors {
		if m != r.Source {
			out = append(out, r.pathFor(m))
		}
	}
	return out
}

// manualEntryHint words the entry the administrator would add to r's
// object, in that object's own field: its mirror's path for IDMSSource
// first, so the nodes keep using it, and for a digest object
// mirrorTarget after it, which also sets up the image mirror (this item
// then counts it as done). An ImageTagMirrorSet's entry keeps its tag
// mirror; the image mirror can then be created here.
func manualEntryHint(r MirrorRule) string {
	field := map[string]string{KindIDMS: "spec.imageDigestMirrors", KindICSP: "spec.repositoryDigestMirrors", mirrorKindITMS: "spec.imageTagMirrors"}[r.Kind]
	mirrors := r.mirrorPaths()
	after := "then Create the image mirror works here"
	if r.digest() {
		mirrors = append(mirrors, mirrorTarget)
		after = "that also sets up the image mirror"
	}
	return fmt.Sprintf("in %s of %s: source %s, mirrors %s (%s)", field, r.ref(), rhoai.IDMSSource, strings.Join(mirrors, ", "), after)
}

func ImageMirror(c *kube.Client) (types.ImageMirrorInfo, error) {
	path := rhoai.ClusterPath("config.openshift.io/v1", "imagedigestmirrorsets", "")
	var items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Spec struct {
			ImageDigestMirrors []struct {
				Source string `json:"source"`
			} `json:"imageDigestMirrors"`
		} `json:"spec"`
	}
	if err := c.ListInto(path, &items); err != nil {
		if kube.IsDecodeError(err) {
			return types.ImageMirrorInfo{Exists: false}, fmt.Errorf("unmarshal: %w", err)
		}
		return types.ImageMirrorInfo{Exists: false}, fmt.Errorf("request failed: %w", err)
	}
	for _, item := range items {
		for _, mirror := range item.Spec.ImageDigestMirrors {
			if mirror.Source == rhoai.IDMSSource {
				return types.ImageMirrorInfo{Exists: true, Kind: KindIDMS, Name: item.Metadata.Name, Source: mirror.Source}, nil
			}
		}
	}
	// An ImageContentSourcePolicy mirrors the source just as well: the
	// Machine Config Operator merges its mirrors with the IDMSs' (runtime-utils
	// @7bdb5b9c pkg/registries/registries.go mergedDigestMirrorSets,
	// :165-183).
	icsp, err := listICSPRules(c)
	if err != nil {
		return types.ImageMirrorInfo{Exists: false}, fmt.Errorf("list ImageContentSourcePolicies: %w", err)
	}
	for _, r := range icsp {
		if r.Source == rhoai.IDMSSource {
			return types.ImageMirrorInfo{Exists: true, Kind: KindICSP, Name: r.Name, Source: r.Source}, nil
		}
	}
	return types.ImageMirrorInfo{Exists: false}, nil
}
