package serving

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/envkit"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/types"
)

// This tool's MaaSSubscription and MaaSAuthPolicy: who may use its
// published models (the audience) and each model's token limit.
//
// The schema (models-as-a-service rhoai-3.6 and main,
// maas-controller/api/maas/v1alpha1/maassubscription_types.go):
// spec.owner {groups: [{name}], users: [...]}, the users and groups
// "entitled to select this subscription"; spec.modelRefs[] each with
// exactly one of tokenRateLimits [{limit, window}] or unlimited: true
// (XValidation "tokenRateLimits is required unless unlimited is true" and
// "tokenRateLimits must not be set when unlimited is true"); limit 1 to
// 1,000,000,000, window ^[1-9]\d{0,3}(s|m|h)$ ("Days (d) are not
// supported"). RHOAI 3.4 (release-3.4) has no unlimited and requires
// tokenRateLimits, so its API server refuses an unlimited reference in the
// dry run: nothing is written there.
//
// The docs (configuration-and-management/quota-and-access-configuration.md)
// give the presets: the free sample (100 tokens / 1m), the premium sample
// (100000 / 24h) and the unlimited example ("Token usage is still
// metered"), with the tip not to fake unlimited with huge limits (every
// limit is copied into the gateway's WasmPlugin, capped at 1.5 MiB). A user
// needs both access (MaaSAuthPolicy subjects) and quota (MaaSSubscription
// owner), so both name the same audience.
//
// One subscription for every model this tool publishes, not one per
// audience: an API key is bound to one subscription when it is minted, a
// caller in two of them can hang on Connectivity Link 1.4.x (RHCLWarning),
// and two of the tool's own, both at priority 0 (maas_priority.go), would
// tie for every caller in both. Limits are per model; the audience is the
// subscription's.

const (
	maasMaxRates     = 5
	maasMaxTokens    = 1000000000
	maasMaxAudience  = 20
	maasMaxNameBytes = 253
)

// maasWindowPattern is the CRD's pattern for a window.
var maasWindowPattern = regexp.MustCompile(`^[1-9]\d{0,3}(s|m|h)$`)

// MaaSRate is one tokenRateLimits entry.
type MaaSRate struct {
	Limit  int64
	Window string
}

// MaaSLimit is a model's limit: unlimited, or its rates.
type MaaSLimit struct {
	Unlimited bool
	Rates     []MaaSRate
}

// maasLimitPresets are the docs' samples.
var maasLimitPresets = map[string]MaaSLimit{
	"free":      {Rates: []MaaSRate{{100, "1m"}}},
	"premium":   {Rates: []MaaSRate{{100000, "24h"}}},
	"unlimited": {Unlimited: true},
}

// maasFreeSample is the limit of a model published without one (and of
// every model an earlier build published).
func maasFreeSample() MaaSLimit { return maasLimitPresets["free"] }

// ParseMaaSLimit checks a requested limit: none is the free sample (what
// Publish did before limits could be chosen). It returns why it is
// invalid, or "".
func ParseMaaSLimit(l *types.MaaSLimit) (MaaSLimit, string) {
	if l == nil {
		return maasFreeSample(), ""
	}
	if p, ok := maasLimitPresets[l.Preset]; ok {
		if l.Unlimited || len(l.Rates) > 0 {
			return MaaSLimit{}, fmt.Sprintf("the %s preset takes no rates of its own", l.Preset)
		}
		return p, ""
	}
	if l.Preset != "custom" {
		return MaaSLimit{}, "the limit preset must be free, premium, unlimited or custom"
	}
	if l.Unlimited {
		return MaaSLimit{}, "a custom limit has rates; choose the Unlimited preset for no token budget"
	}
	if len(l.Rates) == 0 {
		return MaaSLimit{}, "a custom limit needs at least one rate"
	}
	if len(l.Rates) > maasMaxRates {
		return MaaSLimit{}, fmt.Sprintf("a custom limit has at most %d rates", maasMaxRates)
	}
	out := MaaSLimit{}
	seen := map[string]bool{}
	for _, r := range l.Rates {
		if r.Limit < 1 || r.Limit > maasMaxTokens {
			return MaaSLimit{}, "a rate's token limit must be a whole number from 1 to 1,000,000,000 (the MaaSSubscription API's range; for no limit choose Unlimited)"
		}
		if !maasWindowPattern.MatchString(r.Window) {
			return MaaSLimit{}, "a rate's window must be 1 to 9999 seconds, minutes or hours, written like 30s, 1m or 24h (the MaaSSubscription API has no days)"
		}
		if seen[r.Window] {
			return MaaSLimit{}, fmt.Sprintf("the window %s is given twice", r.Window)
		}
		seen[r.Window] = true
		out.Rates = append(out.Rates, MaaSRate{r.Limit, r.Window})
	}
	return out, ""
}

// Preset names the docs' sample the limit is, else "custom".
func (l MaaSLimit) Preset() string {
	for _, name := range []string{"free", "premium", "unlimited"} {
		if l.equal(maasLimitPresets[name]) {
			return name
		}
	}
	return "custom"
}

func (l MaaSLimit) equal(o MaaSLimit) bool {
	if l.Unlimited != o.Unlimited || len(l.Rates) != len(o.Rates) {
		return false
	}
	for i := range l.Rates {
		if l.Rates[i] != o.Rates[i] {
			return false
		}
	}
	return true
}

// api is the limit as GET /api/resources/maas reports it.
func (l MaaSLimit) api() *types.MaaSLimit {
	out := &types.MaaSLimit{Preset: l.Preset(), Unlimited: l.Unlimited}
	for _, r := range l.Rates {
		out.Rates = append(out.Rates, types.MaaSTokenRate{Limit: r.Limit, Window: r.Window})
	}
	return out
}

// AddTo sets the limit on a modelRefs entry: exactly one of unlimited and
// tokenRateLimits.
func (l MaaSLimit) AddTo(ref map[string]any) {
	if l.Unlimited {
		ref["unlimited"] = true
		return
	}
	var rates []any
	for _, r := range l.Rates {
		rates = append(rates, map[string]any{"limit": r.Limit, "window": r.Window})
	}
	ref["tokenRateLimits"] = rates
}

// MaaSLimitFromModelRef reads the limit of a modelRefs entry as the
// cluster has it.
func MaaSLimitFromModelRef(ref map[string]any) (MaaSLimit, bool) {
	if u, _ := ref["unlimited"].(bool); u {
		return MaaSLimit{Unlimited: true}, true
	}
	raw, _ := ref["tokenRateLimits"].([]any)
	var out MaaSLimit
	for _, item := range raw {
		m, _ := item.(map[string]any)
		w, _ := m["window"].(string)
		var n int64
		switch v := m["limit"].(type) {
		case float64:
			n = int64(v)
		case int64:
			n = v
		case int:
			n = int64(v)
		case json.Number:
			n, _ = v.Int64()
		}
		if w == "" || n < 1 {
			return MaaSLimit{}, false
		}
		out.Rates = append(out.Rates, MaaSRate{n, w})
	}
	return out, len(out.Rates) > 0
}

// Text is the limit in words: "100 tokens a minute", "unlimited (usage
// still metered)".
func (l MaaSLimit) Text() string {
	if l.Unlimited {
		return "unlimited (usage still metered)"
	}
	var parts []string
	for _, r := range l.Rates {
		parts = append(parts, fmt.Sprintf("%s %s %s", thousands(r.Limit), kube.Verb(int(min(r.Limit, 2)), "token", "tokens"), windowText(r.Window)))
	}
	return joinWords(parts)
}

// windowText is a window in words: "a minute", "every 30 seconds", "a day".
func windowText(w string) string {
	if w == "24h" {
		return "a day"
	}
	n, _ := strconv.Atoi(w[:len(w)-1])
	unit := map[byte]string{'s': "second", 'm': "minute", 'h': "hour"}[w[len(w)-1]]
	if n == 1 {
		if unit == "hour" {
			return "an hour"
		}
		return "a " + unit
	}
	return fmt.Sprintf("every %d %ss", n, unit)
}

// thousands writes n with comma separators.
func thousands(n int64) string {
	s := strconv.FormatInt(n, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// joinWords joins "a", "a and b", "a, b and c".
func joinWords(parts []string) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}

// --- the audience ------------------------------------------------------------------

// MaaSAudience is who may use the published models: every signed-in user
// (system:authenticated, the docs' sample), or the groups and users named.
type MaaSAudience struct {
	Everyone bool
	Groups   []string
	Users    []string
}

// ParseMaaSAudience checks a requested audience: none is everyone (what
// Publish did before). Names are trimmed and given once.
func ParseMaaSAudience(a *types.MaaSAudience) (MaaSAudience, string) {
	if a == nil {
		return MaaSAudience{Everyone: true}, ""
	}
	groups, users := cleanNames(a.Groups), cleanNames(a.Users)
	if a.Everyone {
		if len(groups) > 0 || len(users) > 0 {
			return MaaSAudience{}, "Everyone signed in names no groups or users"
		}
		return MaaSAudience{Everyone: true}, ""
	}
	if len(groups) == 0 && len(users) == 0 {
		return MaaSAudience{}, "name at least one group or user, or choose Everyone signed in"
	}
	if len(groups) > maasMaxAudience || len(users) > maasMaxAudience {
		return MaaSAudience{}, fmt.Sprintf("name at most %d groups and %d users (the MaaS docs advise groups for people: Subscription Cardinality)", maasMaxAudience, maasMaxAudience)
	}
	// MaaS matches both exactly, as the identity provider sends them
	// (maas-controller api/maas/v1alpha1: GroupReference.Name "the opaque
	// group name used for exact-match authorization"; owner groups "are
	// matched exactly and are not Kubernetes object names"; no CRD
	// pattern), so an OIDC group like /admins or oidc:ml is valid. Only
	// what cannot be a name is refused; a group with no OpenShift Group is
	// a warning (MaaSGroupProblems).
	for _, g := range groups {
		if g == maasSubjectGroup {
			return MaaSAudience{}, "system:authenticated is every signed-in user: choose Everyone signed in"
		}
		if why := namePrintable(g); why != "" {
			return MaaSAudience{}, fmt.Sprintf("group %q %s", g, why)
		}
	}
	for _, u := range users {
		if why := namePrintable(u); why != "" {
			return MaaSAudience{}, fmt.Sprintf("user %q %s", u, why)
		}
	}
	return MaaSAudience{Groups: groups, Users: users}, ""
}

func cleanNames(in []string) []string {
	var out []string
	for _, n := range in {
		n = strings.TrimSpace(n)
		if n != "" && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}

// namePrintable: no control characters (a pasted tab or newline).
func namePrintable(n string) string {
	for _, r := range n {
		if unicode.IsControl(r) {
			return "has a control character"
		}
	}
	if len(n) > maasMaxNameBytes {
		return fmt.Sprintf("is longer than %d characters", maasMaxNameBytes)
	}
	return ""
}

// openShiftGroupName: n can be the name of an OpenShift Group object
// (openshift/apiserver-library-go pkg/apivalidation/uservalidation.go
// ValidateGroupName: a path segment, so not "." or ".." and no "/" or "%";
// no ":"; not "~"). A group whose name cannot be has no Group object, so
// only the identity provider can put anyone in it.
func openShiftGroupName(n string) bool {
	return n != "." && n != ".." && n != "~" && !strings.ContainsAny(n, "/%:")
}

// Owner is spec.owner of the subscription, and spec.subjects of the access
// policy (same shape).
func (a MaaSAudience) Owner() map[string]any {
	groups := []any{}
	users := []any{}
	if a.Everyone {
		groups = append(groups, map[string]any{"name": maasSubjectGroup})
	}
	for _, g := range a.Groups {
		groups = append(groups, map[string]any{"name": g})
	}
	for _, u := range a.Users {
		users = append(users, u)
	}
	out := map[string]any{"groups": groups, "users": users}
	if len(groups) == 0 {
		delete(out, "groups")
	}
	return out
}

// MaaSAudienceFromOwner reads spec.owner (or spec.subjects) as the
// cluster has it; system:authenticated alone is everyone.
func MaaSAudienceFromOwner(owner map[string]any) MaaSAudience {
	var a MaaSAudience
	groups, _ := owner["groups"].([]any)
	for _, g := range groups {
		m, _ := g.(map[string]any)
		if name, _ := m["name"].(string); name != "" {
			a.Groups = append(a.Groups, name)
		}
	}
	users, _ := owner["users"].([]any)
	for _, u := range users {
		if name, _ := u.(string); name != "" {
			a.Users = append(a.Users, name)
		}
	}
	if len(a.Groups) == 1 && a.Groups[0] == maasSubjectGroup && len(a.Users) == 0 {
		return MaaSAudience{Everyone: true}
	}
	return a
}

// equal: the same audience, in any order (MaaS matches names, not
// positions).
func (a MaaSAudience) Equal(o MaaSAudience) bool {
	sorted := func(in []string) string {
		s := append([]string(nil), in...)
		sort.Strings(s)
		return strings.Join(s, "\n")
	}
	return a.Everyone == o.Everyone && sorted(a.Groups) == sorted(o.Groups) && sorted(a.Users) == sorted(o.Users)
}

func (a MaaSAudience) api() types.MaaSAudience {
	return types.MaaSAudience{Everyone: a.Everyone, Groups: a.Groups, Users: a.Users}
}

// Text is the audience in words: "every signed-in user", "groups a, b and
// user x"; more than three names are counted.
func (a MaaSAudience) Text() string {
	if a.Everyone {
		return "every signed-in user"
	}
	shown := 0
	name := func(singular, plural string, names []string) string {
		if len(names) == 0 {
			return ""
		}
		left := 3 - shown
		if left <= 0 {
			return ""
		}
		take := names
		if len(take) > left {
			take = take[:left]
		}
		shown += len(take)
		return kube.Verb(len(names), singular, plural) + " " + strings.Join(take, ", ")
	}
	var parts []string
	if g := name("group", "groups", a.Groups); g != "" {
		parts = append(parts, g)
	}
	if u := name("user", "users", a.Users); u != "" {
		parts = append(parts, u)
	}
	if more := len(a.Groups) + len(a.Users) - shown; more > 0 {
		parts = append(parts, fmt.Sprintf("%d more", more))
	}
	return joinWords(parts)
}

// MaaSGroupProblems checks that each group exists as an OpenShift Group.
// A missing one is a warning, not a refusal: with an external identity
// provider the groups come from the sign-in token and have no Group
// object. A name that cannot be an OpenShift Group's is not looked up: it
// has none, and none can be created. One that cannot be read (no
// permission) is said so.
func MaaSGroupProblems(c *kube.Client, groups []string) []types.MaaSGroupProblem {
	var out []types.MaaSGroupProblem
	missing := func(g string) types.MaaSGroupProblem {
		return types.MaaSGroupProblem{Name: g, Missing: true, Message: g + " is not a group on this cluster. Nobody is in it unless your identity provider adds it at sign-in."}
	}
	for _, g := range groups {
		if !openShiftGroupName(g) {
			out = append(out, missing(g))
			continue
		}
		_, found, err := kube.ReadObjectMeta(c, rhoai.ClusterPath("user.openshift.io/v1", "groups", url.PathEscape(g)))
		switch {
		case err != nil:
			out = append(out, types.MaaSGroupProblem{Name: g, Message: fmt.Sprintf("Cannot check whether group %s exists (%v). Check it with oc get group %s.", g, err, shellQuoteArg(g))})
		case !found:
			p := missing(g)
			p.Commands = fmt.Sprintf("oc adm groups new %s\noc adm groups add-users %s <user>", shellQuoteArg(g), shellQuoteArg(g))
			out = append(out, p)
		}
	}
	return out
}

// CheckMaaSGroups is MaaSGroupProblems for names typed in a dialog (GET
// /api/resources/maas/groups): it refuses what Publish would refuse.
func CheckMaaSGroups(c *kube.Client, names []string) ([]types.MaaSGroupProblem, string) {
	aud, problem := ParseMaaSAudience(&types.MaaSAudience{Groups: names})
	if problem != "" {
		return nil, problem
	}
	out := MaaSGroupProblems(c, aud.Groups)
	if out == nil {
		out = []types.MaaSGroupProblem{}
	}
	return out, ""
}

var shellSafeArg = regexp.MustCompile(`^[A-Za-z0-9._@-]+$`)

// shellQuoteArg quotes a name for a shell command when it needs it.
func shellQuoteArg(s string) string {
	if shellSafeArg.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// --- the tool's subscription as read --------------------------------------------------

// maasToolSub is this tool's MaaSSubscription as the cluster has it: its
// audience, the limit of each model it lists ("<project>/<model>"), its
// spec.priority (unset reads as 0, the CRD's default) and its metadata.
type maasToolSub struct {
	found, owned bool
	audience     MaaSAudience
	limits       map[string]MaaSLimit
	priority     int64
	meta         kube.ObjectMeta
}

func readMaaSToolSubscription(c *kube.Client) (maasToolSub, error) {
	var sub struct {
		Metadata kube.ObjectMeta `json:"metadata"`
		Spec     struct {
			Owner     map[string]any   `json:"owner"`
			ModelRefs []map[string]any `json:"modelRefs"`
			Priority  *int64           `json:"priority"`
		} `json:"spec"`
	}
	exists, err := c.GetIntoOptional(maasAccessPath(c, "MaaSSubscription"), &sub)
	if err != nil {
		if kube.IsDecodeError(err) {
			return maasToolSub{}, fmt.Errorf("parse MaaSSubscription %s: %w", MaaSSubscriptionName, err)
		}
		return maasToolSub{}, fmt.Errorf("read MaaSSubscription %s/%s: %w", MaaSTenantNamespace, MaaSSubscriptionName, err)
	}
	if !exists {
		return maasToolSub{audience: MaaSAudience{Everyone: true}, limits: map[string]MaaSLimit{}}, nil
	}
	out := maasToolSub{found: true, owned: ToolOwned(sub.Metadata), audience: MaaSAudienceFromOwner(sub.Spec.Owner), limits: map[string]MaaSLimit{}, meta: sub.Metadata}
	if sub.Spec.Priority != nil {
		out.priority = *sub.Spec.Priority
	}
	for _, ref := range sub.Spec.ModelRefs {
		ns, _ := ref["namespace"].(string)
		name, _ := ref["name"].(string)
		if l, ok := MaaSLimitFromModelRef(ref); ok && ns != "" && name != "" {
			out.limits[MaaSPub{ns, name}.Key()] = l
		}
	}
	return out, nil
}

// limitOf is a publication's limit in the subscription; one it does not
// list yet (an earlier build's, whose entry was the docs' sample) is the
// free sample.
func (s maasToolSub) limitOf(p MaaSPub) MaaSLimit {
	if l, ok := s.limits[p.Key()]; ok {
		return l
	}
	return maasFreeSample()
}

// MaaSSubscriptionDescription is the subscription's openshift.io/description,
// which the dashboard's API-key dialog shows next to its name: the one
// limit every model has, or each model's, and who it is for.
func MaaSSubscriptionDescription(pubs []MaaSPub, limits map[string]MaaSLimit, aud MaaSAudience) string {
	byText := map[string]bool{}
	names := map[string]int{}
	for _, p := range pubs {
		byText[limitFor(limits, p).Text()] = true
		names[p.Model]++
	}
	if len(byText) <= 1 {
		for text := range byText {
			return text + " for " + aud.Text()
		}
		return "For " + aud.Text()
	}
	var parts []string
	for _, p := range pubs {
		entry, _ := CatalogEntry(p.Model)
		label := kube.FirstNonEmpty(entry.DisplayName, p.Model)
		if names[p.Model] > 1 {
			label += " in " + p.Project
		}
		parts = append(parts, label+": "+limitFor(limits, p).Text())
	}
	return strings.Join(parts, "; ") + "; for " + aud.Text()
}

func limitFor(limits map[string]MaaSLimit, p MaaSPub) MaaSLimit {
	if l, ok := limits[p.Key()]; ok {
		return l
	}
	return maasFreeSample()
}

// maasAccessObjects are this tool's MaaSSubscription and MaaSAuthPolicy for
// the published models: each model with its limit, both for the audience,
// the subscription always at priority 0 (maasSubscriptionPriority), written
// out so that a write also lowers a priority an earlier version set.
func maasAccessObjects(c *kube.Client, pubs []MaaSPub, limits map[string]MaaSLimit, aud MaaSAudience) (map[string]any, map[string]any) {
	sort.Slice(pubs, func(i, j int) bool { return pubs[i].Key() < pubs[j].Key() })
	var subRefs, authRefs []any
	for _, p := range pubs {
		ref := map[string]any{"name": p.Model, "namespace": p.Project}
		limitFor(limits, p).AddTo(ref)
		subRefs = append(subRefs, ref)
		authRefs = append(authRefs, map[string]any{"name": p.Model, "namespace": p.Project})
	}
	who := "Every signed-in user may use this tool's published models"
	if !aud.Everyone {
		who = "Only " + aud.Text() + " may use this tool's published models"
	}
	sub := map[string]any{
		"apiVersion": MaaSAPI(c),
		"kind":       "MaaSSubscription",
		"metadata": map[string]any{"name": "rhoai-nightly-updater-subscription", "namespace": MaaSTenantNamespace, "labels": envkit.ToolLabels(),
			"annotations": map[string]any{"openshift.io/display-name": "rhoai-nightly-updater models", "openshift.io/description": MaaSSubscriptionDescription(pubs, limits, aud)}},
		"spec": map[string]any{"owner": aud.Owner(), "modelRefs": subRefs, "priority": maasSubscriptionPriority},
	}
	auth := map[string]any{
		"apiVersion": MaaSAPI(c),
		"kind":       "MaaSAuthPolicy",
		"metadata": map[string]any{"name": "rhoai-nightly-updater-access", "namespace": MaaSTenantNamespace, "labels": envkit.ToolLabels(),
			"annotations": map[string]any{"openshift.io/display-name": "rhoai-nightly-updater models", "openshift.io/description": who}},
		"spec": map[string]any{"modelRefs": authRefs, "subjects": aud.Owner()},
	}
	return sub, auth
}

// UnlimitedRefusal turns the API server's refusal of an unlimited model
// reference (a MaaSSubscription API without unlimited requires
// tokenRateLimits) into words.
func UnlimitedRefusal(err error, l MaaSLimit) string {
	if l.Unlimited && err != nil && strings.Contains(err.Error(), "tokenRateLimits") {
		return "This cluster's MaaS has no unlimited option (its MaaSSubscription API requires a token limit). Choose a limit instead."
	}
	return ""
}

// MaaSNoUnlimited: the cluster's MaaSSubscription API, in the version this
// tool writes, has no modelRefs[].unlimited (read from its CRD's schema,
// with the existing list of CRDs, by name). False when that cannot be
// told: Publish's dry run refuses then (UnlimitedRefusal).
func MaaSNoUnlimited(c *kube.Client) bool {
	body, _, err := c.Do(http.MethodGet, "/apis/apiextensions.k8s.io/v1/customresourcedefinitions", "", nil, url.Values{"fieldSelector": {"metadata.name=maassubscriptions." + MaaSGroup}})
	if err != nil {
		return false
	}
	type props map[string]struct {
		Properties props `json:"properties"`
		Items      *struct {
			Properties props `json:"properties"`
		} `json:"items"`
	}
	var list struct {
		Items []struct {
			Spec struct {
				Versions []struct {
					Name   string `json:"name"`
					Schema struct {
						OpenAPIV3Schema struct {
							Properties props `json:"properties"`
						} `json:"openAPIV3Schema"`
					} `json:"schema"`
				} `json:"versions"`
			} `json:"spec"`
		} `json:"items"`
	}
	if json.Unmarshal(body, &list) != nil || len(list.Items) != 1 {
		return false
	}
	version := strings.TrimPrefix(MaaSAPI(c), MaaSGroup+"/")
	for _, v := range list.Items[0].Spec.Versions {
		if v.Name != version {
			continue
		}
		refs := v.Schema.OpenAPIV3Schema.Properties["spec"].Properties["modelRefs"].Items
		if refs == nil || len(refs.Properties) == 0 {
			return false
		}
		_, ok := refs.Properties["unlimited"]
		return !ok
	}
	return false
}
