package cluster

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

// Prerequisite operators named in DataScienceCluster and module conditions.
//
// RHOAI modules check for operators they depend on and report a missing one
// in a condition message (live on RHOAI 3.6):
//
//	TrainerReady=False (Error): dependency not met: JobSet Operator is not installed. Please install ...
//	KserveLLMInferenceServiceDependencies=False (PreConditionFailed): cert-manager operator not installed
//	KserveLLMInferenceServiceWideEPDependencies=False (PreConditionFailed): LeaderWorkerSet not installed; cert-manager operator (Wide EP) not installed
//
// The names are free text, so they are resolved against the cluster's own
// catalogs (PackageManifests: package name, CSV display name and CSV name),
// after normalising case, punctuation and words such as "operator". The
// tool never installs them: diagnostics prints the exact commands, built
// from the catalog's metadata, for an administrator to run.
//
// "Installed" follows what the modules check: trainer-operator
// (internal/controller/dependencies.go) needs the operator's CRD, an OLM
// OperatorCondition named after its CSV (odh-platform-utilities
// pkg/cluster/olm OperatorExists) and the operand CR JobSetOperator/cluster.
// OLM creates the OperatorCondition with the CSV, so a Succeeded CSV stands
// for both; the operand is read when it is a cluster singleton in
// operator.openshift.io, the group the Red Hat operators use for theirs.

// prerequisiteGrace is how long a prerequisite must have been installed
// before a module that still reports it missing is said to be in back-off.
var prerequisiteGrace = 2 * time.Minute

var (
	// missingSegmentSplit splits a condition message into clauses.
	missingSegmentSplit = regexp.MustCompile(`;|\.\s+|\n`)
	notInstalledPhrase  = regexp.MustCompile(`(?i)\s*\b(?:is|are)?\s*not\s+installed\b`)
	parenthetical       = regexp.MustCompile(`\([^)]*\)`)
	nameListSplit       = regexp.MustCompile(`,\s*|\s+and\s+`)
)

// parseMissingDependencies returns the operator names a condition message
// reports as not installed, in message order and without duplicates.
func parseMissingDependencies(msg string) []string {
	var out []string
	for _, seg := range missingSegmentSplit.Split(msg, -1) {
		loc := notInstalledPhrase.FindStringIndex(seg)
		if loc == nil {
			continue
		}
		subject := seg[:loc[0]]
		if i := strings.LastIndex(subject, ":"); i >= 0 {
			subject = subject[i+1:]
		}
		subject = parenthetical.ReplaceAllString(subject, " ")
		for _, name := range nameListSplit.Split(subject, -1) {
			name = strings.Join(strings.Fields(name), " ")
			name = strings.TrimPrefix(strings.TrimPrefix(name, "the "), "The ")
			if name == "" || len(name) > 80 || len(strings.Fields(name)) > 8 {
				continue
			}
			if !containsString(out, name) {
				out = append(out, name)
			}
		}
	}
	return out
}

// Modules also name a missing operand directly (live, after
// JobSetOperator/cluster was deleted):
//
//	dependency not met: JobSetOperator CR with name 'cluster' not found. Please create the JobSetOperator CR to enable the JobSet controller.
var (
	operandNotFoundPhrase = regexp.MustCompile(`\b([A-Z][A-Za-z0-9]*) (?:CR|custom resource) (?:with name |named )?['"]([a-z0-9][a-z0-9.-]*)['"] (?:not found|does not exist)`)
	operandCreatePhrase   = regexp.MustCompile(`(?i:please create) (?:the |an? )?([A-Z][A-Za-z0-9]*) (?:CR|custom resource)\b`)
)

// operandMention is a missing operand named in a condition message; Name
// is "" when the message does not name it.
type operandMention struct{ Kind, Name string }

// parseMissingOperands returns the operands a message reports missing.
func parseMissingOperands(msg string) []operandMention {
	var out []operandMention
	add := func(kind, name string) {
		for i := range out {
			if out[i].Kind == kind {
				if out[i].Name == "" {
					out[i].Name = name
				}
				return
			}
		}
		out = append(out, operandMention{Kind: kind, Name: name})
	}
	for _, m := range operandNotFoundPhrase.FindAllStringSubmatch(msg, -1) {
		add(m[1], m[2])
	}
	for _, m := range operandCreatePhrase.FindAllStringSubmatch(msg, -1) {
		add(m[1], "")
	}
	return out
}

// operatorNameStopWords are dropped when names are compared: they appear in
// some spellings of an operator's name and not in others.
var operatorNameStopWords = map[string]bool{
	"operator": true, "operators": true, "openshift": true, "for": true, "the": true, "redhat": true, "rh": true,
}

// operatorNameAliases maps normalised short names that operators use in
// messages to the normalised catalog name. Discovery still decides: an alias
// only changes which catalog entry is looked for.
var operatorNameAliases = map[string]string{
	"lws": "leaderworkerset",
}

// normalizeOperatorName reduces an operator name to lower-case letters and
// digits without stop words: "JobSet Operator", "Job Set Operator" and
// "job-set" all become "jobset"; "cert-manager Operator for Red Hat
// OpenShift" and "openshift-cert-manager-operator" become "certmanager".
func normalizeOperatorName(s string) string {
	s = strings.ToLower(parenthetical.ReplaceAllString(s, " "))
	s = strings.ReplaceAll(s, "red hat", " ")
	words := strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	var kept []string
	for _, w := range words {
		if !operatorNameStopWords[w] {
			kept = append(kept, w)
		}
	}
	n := strings.Join(kept, "")
	if a, ok := operatorNameAliases[n]; ok {
		return a
	}
	return n
}

// csvBaseName is a CSV name without its version: "jobset-operator.v1.0.1"
// becomes "jobset-operator".
func csvBaseName(csv string) string {
	base, _, _ := strings.Cut(csv, ".")
	return base
}

// --- Catalog ---

// almExample is one object of a CSV's alm-examples annotation.
type almExample struct {
	APIVersion string
	Kind       string
	Name       string
	Namespace  string
	Spec       json.RawMessage
}

func (e almExample) group() string {
	g, _, _ := strings.Cut(e.APIVersion, "/")
	if !strings.Contains(e.APIVersion, "/") {
		return ""
	}
	return g
}

// objName is the operand's name ("cluster" when the example has none).
func (e almExample) objName() string { return nonEmpty(e.Name, "cluster") }

// ref is "<Kind>/<name>".
func (e almExample) ref() string { return e.Kind + "/" + e.objName() }

// resourceArg is the kind as oc accepts it, qualified by its group.
func (e almExample) resourceArg() string {
	if g := e.group(); g != "" {
		return strings.ToLower(e.Kind) + "." + g
	}
	return strings.ToLower(e.Kind)
}

// catalogPackage is the part of a PackageManifest diagnostics uses: the
// default channel's head CSV.
type catalogPackage struct {
	Name             string
	Catalog          string
	CatalogNamespace string
	Provider         string
	DefaultChannel   string
	CurrentCSV       string
	DisplayName      string // the head CSV's display name
	InstallModes     []string
	SuggestedNS      string
	ClusterMonitor   bool // operatorframework.io/cluster-monitoring=true
	Examples         []almExample
	OwnedKinds       []string // kinds of the head CSV's owned CRDs
}

// rank orders the catalogs a package may come from: the Red Hat catalog,
// other Red Hat-provided packages, certified, then anything else
// (community), which Red Hat does not support.
func (p catalogPackage) rank() int {
	switch {
	case p.Catalog == "redhat-operators":
		return 0
	case strings.Contains(strings.ToLower(p.Provider), "red hat"):
		return 1
	case p.Catalog == "certified-operators":
		return 2
	default:
		return 3
	}
}

func (p catalogPackage) supports(mode string) bool { return containsString(p.InstallModes, mode) }

func (p catalogPackage) describe() string {
	return fmt.Sprintf("package %s in catalog %s (provider %s), default channel %s, head %s",
		p.Name, p.Catalog, nonEmpty(p.Provider, "unknown"), p.DefaultChannel, nonEmpty(p.CurrentCSV, "unknown"))
}

// matchKeys are the normalised names the package is known by.
func (p catalogPackage) matchKeys() []string {
	var keys []string
	for _, s := range []string{p.Name, p.DisplayName, csvBaseName(p.CurrentCSV)} {
		if k := normalizeOperatorName(s); k != "" && !containsString(keys, k) {
			keys = append(keys, k)
		}
	}
	return keys
}

// singletonFor returns the package's singleton operand for the dependency
// (see singletonFrom).
func (p catalogPackage) singletonFor(key string) *almExample {
	return singletonFrom(p.Examples, append([]string{key}, p.matchKeys()...))
}

// singletonFrom returns the cluster-scoped singleton operand ("cluster",
// no namespace) of a CSV's alm-examples: the only one, or the one whose
// kind (without "Operator") matches one of the operator's normalised
// names. nil when there is none or it is ambiguous.
func singletonFrom(examples []almExample, names []string) *almExample {
	var singletons []almExample
	for _, e := range examples {
		if e.Name == "cluster" && e.Namespace == "" && e.Kind != "" {
			singletons = append(singletons, e)
		}
	}
	if len(singletons) == 1 {
		return &singletons[0]
	}
	for i, e := range singletons {
		if containsString(names, normalizeOperatorName(e.Kind)) {
			return &singletons[i]
		}
	}
	return nil
}

type packageIndex struct {
	at       time.Time
	packages []catalogPackage
}

var (
	packageIndexMu    sync.Mutex
	packageIndexCache = map[string]packageIndex{} // by API server URL
	// packageIndexTTL bounds how stale the catalog view may be. Listing
	// every PackageManifest is large (about 20 MB on a cluster with the
	// default catalogs), so it is read only when a condition names a
	// dependency, and then at most this often.
	packageIndexTTL = 15 * time.Minute
)

// loadPackageIndex lists the PackageManifests of the global catalogs.
func loadPackageIndex(c *Client) ([]catalogPackage, error) {
	packageIndexMu.Lock()
	defer packageIndexMu.Unlock()
	if idx, ok := packageIndexCache[c.baseURL]; ok && time.Since(idx.at) < packageIndexTTL {
		return idx.packages, nil
	}
	body, _, err := c.get(namespacedPath("packages.operators.coreos.com/v1", "packagemanifests", CatalogNS, ""))
	if err != nil {
		return nil, fmt.Errorf("list package manifests: %w", err)
	}
	pkgs, err := parsePackageManifests(body)
	if err != nil {
		return nil, err
	}
	packageIndexCache[c.baseURL] = packageIndex{at: time.Now(), packages: pkgs}
	return pkgs, nil
}

func parsePackageManifests(body []byte) ([]catalogPackage, error) {
	var list struct {
		Items []struct {
			Metadata struct {
				Name   string            `json:"name"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
			Status struct {
				CatalogSource          string `json:"catalogSource"`
				CatalogSourceNamespace string `json:"catalogSourceNamespace"`
				Provider               struct {
					Name string `json:"name"`
				} `json:"provider"`
				DefaultChannel string `json:"defaultChannel"`
				Channels       []struct {
					Name           string `json:"name"`
					CurrentCSV     string `json:"currentCSV"`
					CurrentCSVDesc struct {
						DisplayName  string `json:"displayName"`
						InstallModes []struct {
							Type      string `json:"type"`
							Supported bool   `json:"supported"`
						} `json:"installModes"`
						Annotations               map[string]string `json:"annotations"`
						CustomResourceDefinitions struct {
							Owned []ownedCRD `json:"owned"`
						} `json:"customresourcedefinitions"`
					} `json:"currentCSVDesc"`
				} `json:"channels"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("parse package manifests: %w", err)
	}
	out := make([]catalogPackage, 0, len(list.Items))
	for _, it := range list.Items {
		p := catalogPackage{
			Name: it.Metadata.Name, Catalog: it.Status.CatalogSource, CatalogNamespace: it.Status.CatalogSourceNamespace,
			Provider: it.Status.Provider.Name, DefaultChannel: it.Status.DefaultChannel,
		}
		if p.Catalog == "" {
			p.Catalog = it.Metadata.Labels["catalog"]
		}
		if p.CatalogNamespace == "" {
			p.CatalogNamespace = nonEmpty(it.Metadata.Labels["catalog-namespace"], CatalogNS)
		}
		if p.Provider == "" {
			p.Provider = it.Metadata.Labels["provider"]
		}
		for _, ch := range it.Status.Channels {
			if ch.Name != p.DefaultChannel {
				continue
			}
			d := ch.CurrentCSVDesc
			p.CurrentCSV, p.DisplayName = ch.CurrentCSV, d.DisplayName
			for _, m := range d.InstallModes {
				if m.Supported {
					p.InstallModes = append(p.InstallModes, m.Type)
				}
			}
			p.SuggestedNS = d.Annotations["operatorframework.io/suggested-namespace"]
			p.ClusterMonitor = d.Annotations["operatorframework.io/cluster-monitoring"] == "true"
			p.Examples = parseALMExamples(d.Annotations["alm-examples"])
			for _, o := range d.CustomResourceDefinitions.Owned {
				p.OwnedKinds = append(p.OwnedKinds, o.Kind)
			}
		}
		out = append(out, p)
	}
	return out, nil
}

func parseALMExamples(raw string) []almExample {
	var objs []struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Metadata   struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"metadata"`
		Spec json.RawMessage `json:"spec"`
	}
	if raw == "" || json.Unmarshal([]byte(raw), &objs) != nil {
		return nil
	}
	out := make([]almExample, 0, len(objs))
	for _, o := range objs {
		out = append(out, almExample{APIVersion: o.APIVersion, Kind: o.Kind, Name: o.Metadata.Name, Namespace: o.Metadata.Namespace, Spec: o.Spec})
	}
	return out
}

// matchPackages returns the packages known by the normalised key, best
// first (catalog rank, then name).
func matchPackages(pkgs []catalogPackage, key string) []catalogPackage {
	var out []catalogPackage
	for _, p := range pkgs {
		if containsString(p.matchKeys(), key) {
			out = append(out, p)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].rank() != out[j].rank() {
			return out[i].rank() < out[j].rank()
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// --- Installed operators ---

// clusterCSV is a CSV anywhere on the cluster (OLM's copies excluded).
type clusterCSV struct {
	Name, Namespace, DisplayName, Phase string
	Labels                              map[string]string
	Changed                             time.Time // status.lastTransitionTime
	Examples                            []almExample
	Owned                               []ownedCRD
}

// ownedCRD is one entry of spec.customresourcedefinitions.owned.
type ownedCRD struct {
	Name    string `json:"name"` // <plural>.<group>
	Kind    string `json:"kind"`
	Version string `json:"version"`
}

// matchNames are the normalised names the installed operator is known by.
func (csv clusterCSV) matchNames() []string {
	var out []string
	for _, s := range append([]string{csv.DisplayName, csvBaseName(csv.Name)}, csv.packages()...) {
		if n := normalizeOperatorName(s); n != "" && !containsString(out, n) {
			out = append(out, n)
		}
	}
	return out
}

func listClusterCSVs(c *Client) ([]clusterCSV, error) {
	// OLM copies the CSV of an AllNamespaces operator into every namespace
	// and labels the copies olm.copiedFrom.
	body, _, err := c.do(http.MethodGet, clusterPath("operators.coreos.com/v1alpha1", "clusterserviceversions", ""), "", nil,
		url.Values{"labelSelector": {"!olm.copiedFrom"}})
	if err != nil {
		return nil, fmt.Errorf("list CSVs: %w", err)
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name        string            `json:"name"`
				Namespace   string            `json:"namespace"`
				Labels      map[string]string `json:"labels"`
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
			Spec struct {
				DisplayName               string `json:"displayName"`
				CustomResourceDefinitions struct {
					Owned []ownedCRD `json:"owned"`
				} `json:"customresourcedefinitions"`
			} `json:"spec"`
			Status struct {
				Phase              string `json:"phase"`
				LastTransitionTime string `json:"lastTransitionTime"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("parse CSVs: %w", err)
	}
	out := make([]clusterCSV, 0, len(list.Items))
	for _, it := range list.Items {
		csv := clusterCSV{Name: it.Metadata.Name, Namespace: it.Metadata.Namespace, DisplayName: it.Spec.DisplayName, Phase: it.Status.Phase, Labels: it.Metadata.Labels}
		csv.Changed, _ = parseK8sTime(it.Status.LastTransitionTime)
		csv.Examples = parseALMExamples(it.Metadata.Annotations["alm-examples"])
		csv.Owned = it.Spec.CustomResourceDefinitions.Owned
		out = append(out, csv)
	}
	return out, nil
}

// packages returns the package names OLM labelled the CSV with
// (operators.coreos.com/<package>.<namespace>).
func (csv clusterCSV) packages() []string {
	var out []string
	for k := range csv.Labels {
		rest, ok := strings.CutPrefix(k, "operators.coreos.com/")
		if !ok {
			continue
		}
		if pkg, ok := strings.CutSuffix(rest, "."+csv.Namespace); ok && pkg != "" {
			out = append(out, pkg)
		}
	}
	return out
}

// findInstalledCSV returns the CSV of the package (or, without a catalog
// match, of an operator known by the key), preferring a Succeeded one.
func findInstalledCSV(csvs []clusterCSV, pkg *catalogPackage, key string) *clusterCSV {
	var best *clusterCSV
	for i, csv := range csvs {
		match := false
		if pkg != nil {
			match = containsString(csv.packages(), pkg.Name) || (pkg.CurrentCSV != "" && csvBaseName(csv.Name) == csvBaseName(pkg.CurrentCSV))
		}
		if !match {
			match = containsString(csv.matchNames(), key)
		}
		if match && (best == nil || (best.Phase != "Succeeded" && csv.Phase == "Succeeded")) {
			best = &csvs[i]
		}
	}
	return best
}

// --- Operands ---

// operandAPIGroup is the only group whose singleton operands the tool may
// read (RBAC: get, resourceNames ["cluster"]). The Red Hat JobSet,
// LeaderWorkerSet and cert-manager operators keep their operand there.
const operandAPIGroup = "operator.openshift.io"

type operandState string

const (
	operandNone       operandState = ""           // the package has no singleton operand
	operandPresent    operandState = "present"    // <Kind>/cluster exists
	operandMissing    operandState = "missing"    // it does not
	operandUnverified operandState = "unverified" // not readable here
)

var errOperandGroup = errors.New("the tool only reads singleton operands in " + operandAPIGroup)

// readSingletonOperand reports whether the cluster-scoped operand
// <Kind>/cluster exists. Other names and groups are not readable (RBAC).
func readSingletonOperand(c *Client, e almExample) (operandState, error) {
	group, version, ok := strings.Cut(e.APIVersion, "/")
	if !ok || group != operandAPIGroup || !dns1123Label.MatchString(version) {
		return operandUnverified, errOperandGroup
	}
	if e.objName() != "cluster" || e.Namespace != "" {
		return operandUnverified, fmt.Errorf("the tool only reads cluster-scoped operands named cluster, not %s", e.ref())
	}
	resource, found, err := discoverPlural(c, version, e.Kind)
	if err != nil {
		return operandUnverified, err
	}
	if !found {
		return operandMissing, nil // its CRD is not served
	}
	_, _, err = c.get("/apis/" + operandAPIGroup + "/" + version + "/" + resource + "/cluster")
	switch {
	case err == nil:
		return operandPresent, nil
	case IsK8sError(err, http.StatusNotFound):
		return operandMissing, nil
	default:
		return operandUnverified, err
	}
}

// discoverPlural finds the resource of a kind in operandAPIGroup from API
// discovery (readable by every authenticated user).
func discoverPlural(c *Client, version, kind string) (string, bool, error) {
	body, _, err := c.get("/apis/" + operandAPIGroup + "/" + version)
	if IsK8sError(err, http.StatusNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("discover %s/%s: %w", operandAPIGroup, version, err)
	}
	var list struct {
		Resources []struct {
			Name string `json:"name"`
			Kind string `json:"kind"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return "", false, fmt.Errorf("parse %s/%s discovery: %w", operandAPIGroup, version, err)
	}
	for _, r := range list.Resources {
		if r.Kind == kind && !strings.Contains(r.Name, "/") && dns1123Label.MatchString(r.Name) {
			return r.Name, true, nil
		}
	}
	return "", false, nil
}

// --- Dependency state ---

// dependency is one prerequisite operator named in conditions, resolved
// against the catalogs and the installed CSVs.
type dependency struct {
	Mention string // as the first message names it
	// OperandKind/OperandName: the message names a missing operand
	// rather than an operator; its operator is the CSV that owns the kind.
	OperandKind, OperandName string
	Key                      string // normalised
	Package                  *catalogPackage
	Others                   []catalogPackage // other catalog matches, best first
	CatalogErr               error
	CSV                      *clusterCSV
	CSVErr                   error
	Operand                  *almExample
	OperandSt                operandState
	OperandErr               error
	// Reporters are the conditions that name it.
	Reporters []*classifiedCondition
	// NeededBy are other findings that need it (a Certificate that is
	// never issued without cert-manager); NeededImpact says what that
	// breaks. Either makes it block, like a blocking condition.
	NeededBy     []string
	NeededImpact string
}

// installed: a Succeeded CSV.
func (d *dependency) installed() bool { return d.CSV != nil && d.CSV.Phase == "Succeeded" }

// satisfied: installed, and its singleton operand (if any) was seen. An
// operand that could not be read (RBAC, discovery, another API group) is
// not satisfied: nothing proves the module waits on a retry rather than on
// the operand, so no restart is offered on that basis.
func (d *dependency) satisfied() bool {
	return d.installed() && (d.OperandSt == operandNone || d.OperandSt == operandPresent)
}

// operandProblem: installed, but the operand is missing or unverified.
func (d *dependency) operandProblem() bool {
	return d.installed() && (d.OperandSt == operandMissing || d.OperandSt == operandUnverified)
}

func (d *dependency) displayName() string {
	switch {
	case d.OperandKind != "" && d.CSV == nil && d.Package == nil:
		return "the operator that provides " + d.OperandKind
	case d.Package != nil && d.Package.DisplayName != "":
		return d.Package.DisplayName
	case d.CSV != nil && d.CSV.DisplayName != "":
		return d.CSV.DisplayName
	}
	return d.Mention
}

// slug identifies the dependency in problem IDs.
func (d *dependency) slug() string {
	if d.CSV != nil {
		if pkgs := d.CSV.packages(); len(pkgs) > 0 {
			sort.Strings(pkgs)
			return pkgs[0]
		}
	}
	if d.Package != nil {
		return d.Package.Name
	}
	return d.Key
}

// blocking reports whether any condition that names it blocks Ready.
func (d *dependency) blocking() bool {
	for _, r := range d.Reporters {
		if r.Blocking {
			return true
		}
	}
	return false
}

// problemID is the ID of the problem that tells how to fix the dependency,
// "" when it is satisfied (a module back-off problem covers that).
func (d *dependency) problemID() string {
	switch {
	case d.satisfied():
		return ""
	case d.operandProblem():
		return "prerequisite-operand-" + d.slug()
	case d.CSV != nil:
		return "prerequisite-not-ready-" + d.slug()
	}
	return "prerequisite-missing-" + d.slug()
}

// resolveDependencies fills in catalog, CSV and operand state. Lookups are
// made only when there is something to resolve.
func resolveDependencies(c *Client, deps []*dependency) {
	if len(deps) == 0 {
		return
	}
	var pkgs []catalogPackage
	var csvs []clusterCSV
	var pkgErr, csvErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); pkgs, pkgErr = loadPackageIndex(c) }()
	go func() { defer wg.Done(); csvs, csvErr = listClusterCSVs(c) }()
	wg.Wait()
	for _, d := range deps {
		d.CatalogErr, d.CSVErr = pkgErr, csvErr
		if d.OperandKind != "" {
			resolveOperandMention(c, d, pkgs, csvs)
			continue
		}
		if pkgErr == nil {
			if matches := matchPackages(pkgs, d.Key); len(matches) > 0 {
				d.Package = &matches[0]
				d.Others = matches[1:]
			}
		}
		if csvErr == nil {
			d.CSV = findInstalledCSV(csvs, d.Package, d.Key)
		}
		// The operand comes from what is installed: an installed CSV
		// (perhaps of another package than the preferred catalog entry)
		// ships its own examples.
		switch {
		case d.CSV != nil:
			d.Operand = singletonFrom(d.CSV.Examples, append([]string{d.Key}, d.CSV.matchNames()...))
		case d.Package != nil:
			d.Operand = d.Package.singletonFor(d.Key)
		}
		if d.Operand != nil && d.installed() {
			d.OperandSt, d.OperandErr = readSingletonOperand(c, *d.Operand)
		}
	}
}

// resolveOperandMention finds the operator of a missing operand: the
// installed CSV that owns its kind (its own alm-examples give the object),
// else the catalog package whose head CSV owns it.
func resolveOperandMention(c *Client, d *dependency, pkgs []catalogPackage, csvs []clusterCSV) {
	var owned *ownedCRD
	for i := range csvs {
		for j, o := range csvs[i].Owned {
			if o.Kind == d.OperandKind && (d.CSV == nil || (d.CSV.Phase != "Succeeded" && csvs[i].Phase == "Succeeded")) {
				d.CSV, owned = &csvs[i], &csvs[i].Owned[j]
			}
		}
	}
	examples := []almExample(nil)
	if d.CSV != nil {
		examples = d.CSV.Examples
	} else {
		for i, p := range pkgs {
			if containsString(p.OwnedKinds, d.OperandKind) && (d.Package == nil || p.rank() < d.Package.rank()) {
				d.Package = &pkgs[i]
			}
		}
		if d.Package != nil {
			examples = d.Package.Examples
		}
	}
	var pick *almExample
	for i, e := range examples {
		if e.Kind != d.OperandKind {
			continue
		}
		if pick == nil || (d.OperandName != "" && e.Name == d.OperandName) {
			pick = &examples[i]
		}
	}
	switch {
	case pick != nil:
		op := *pick
		if d.OperandName != "" {
			op.Name = d.OperandName
		}
		d.Operand = &op
	case owned != nil:
		// No example: the kind's group and version from the owned CRD.
		if _, group, ok := strings.Cut(owned.Name, "."); ok && owned.Version != "" {
			d.Operand = &almExample{APIVersion: group + "/" + owned.Version, Kind: d.OperandKind, Name: d.OperandName}
		}
	}
	if d.Operand == nil && d.installed() {
		// Nothing to build the object from: guidance only.
		d.Operand = &almExample{Kind: d.OperandKind, Name: d.OperandName}
	}
	if d.Operand != nil && d.installed() {
		d.OperandSt, d.OperandErr = readSingletonOperand(c, *d.Operand)
	}
}

// --- Problems ---

// prerequisiteProblem tells how to install, finish or configure one
// dependency. existingOG names the OperatorGroup already in the target
// namespace, if any.
func prerequisiteProblem(c *Client, d *dependency) Problem {
	var conds []string
	var fixes []string
	for _, r := range d.Reporters {
		conds = append(conds, r.label())
	}
	sort.Strings(conds)
	conds = append(conds, d.NeededBy...)
	severity, impact := "info", "It only gates an optional feature: the reporting conditions have severity Info, so the DataScienceCluster can be Ready without it."
	switch {
	case d.blocking():
		severity, impact = "warning", "It keeps the DataScienceCluster from being Ready."
	case len(d.NeededBy) > 0:
		severity, impact = "warning", d.NeededImpact
	}
	evidence := append([]string{}, conds...)
	if d.Package != nil {
		evidence = append(evidence, "Catalog: "+d.Package.describe())
		if d.Package.rank() == 3 {
			evidence = append(evidence, fmt.Sprintf("Only a community package matches %q; Red Hat does not support it. Check whether your subscription includes a supported one before using it.", d.Mention))
		}
		for _, o := range d.Others {
			note := "also matches"
			if o.rank() == 3 {
				note = "also matches (community, not supported by Red Hat; use the one above)"
			}
			evidence = append(evidence, fmt.Sprintf("Catalog: package %s in %s %s", o.Name, o.Catalog, note))
		}
	}
	if d.CatalogErr != nil {
		evidence = append(evidence, fmt.Sprintf("Could not read the catalogs: %v%s", d.CatalogErr, templateHint(d.CatalogErr)))
	}
	if d.CSVErr != nil {
		evidence = append(evidence, fmt.Sprintf("Could not list installed operators: %v%s", d.CSVErr, templateHint(d.CSVErr)))
	}
	p := Problem{
		ID:        d.problemID(),
		Severity:  severity,
		mergeable: true,
	}
	name := d.displayName()

	switch {
	case d.operandProblem():
		csv := d.CSV
		evidence = append(evidence, fmt.Sprintf("Installed: CSV %s/%s is Succeeded", csv.Namespace, csv.Name))
		if d.OperandSt == operandMissing {
			evidence = append(evidence, fmt.Sprintf("%s (%s) does not exist", d.Operand.ref(), d.Operand.APIVersion))
			p.Title = fmt.Sprintf("%s is installed, but its %s does not exist", name, d.Operand.ref())
		} else {
			evidence = append(evidence, fmt.Sprintf("Whether %s (%s) exists could not be checked: %v%s", d.Operand.ref(), d.Operand.APIVersion, d.OperandErr, templateHint(d.OperandErr)))
			p.Title = fmt.Sprintf("%s is installed; check that its %s exists", name, d.Operand.ref())
			fixes = append(fixes, fmt.Sprintf("The tool could not read %s, so it cannot tell a missing operand from a module operator that has not retried, and offers no restart.", d.Operand.ref()))
		}
		p.Description = fmt.Sprintf("Modules check for the operator and for its operand: %s does nothing until %s exists, and the module keeps reporting it as not installed. %s", name, d.Operand.ref(), impact)
		p.AffectedObjects = []string{d.Operand.Kind + " " + d.Operand.objName()}
		cmd, err := operandCommand(*d.Operand)
		if err != nil {
			fixes = append(fixes, fmt.Sprintf("Create %s as the operator's documentation describes (%v).", d.Operand.ref(), err))
		} else {
			fixes = append(fixes, fmt.Sprintf("Create %s from the example the operator ships in its CSV (command below; it creates the object only if it is still missing). Then, if the module still reports it after a few minutes, restart the module's operator (Diagnostics offers it once it can see the operand).", d.Operand.ref()))
			p.TechnicalCmd = cmd
		}
	case d.CSV != nil && !d.installed():
		csv := d.CSV
		evidence = append(evidence, fmt.Sprintf("CSV %s/%s is in phase %s", csv.Namespace, csv.Name, nonEmpty(csv.Phase, "unknown")))
		p.Title = fmt.Sprintf("%s is installed but not ready (CSV phase %s)", name, nonEmpty(csv.Phase, "unknown"))
		p.Description = fmt.Sprintf("OLM has an install of %s, but its CSV is not Succeeded, so modules treat it as missing. %s", name, impact)
		p.AffectedObjects = []string{fmt.Sprintf("ClusterServiceVersion %s/%s", csv.Namespace, csv.Name)}
		fixes = append(fixes, "Read the CSV's status message (command below). Installing usually finishes within minutes; a Failed CSV needs its cause fixed, after which OLM retries.")
		p.TechnicalCmd = shellCommand("oc", "get", "csv", csv.Name, "-n", csv.Namespace, "-o", "jsonpath={.status.phase}: {.status.reason}: {.status.message}")
	case d.OperandKind != "" && d.Package == nil && d.CSV == nil:
		p.Title = fmt.Sprintf("No installed operator provides %s, which a module needs", d.OperandKind)
		p.Description = fmt.Sprintf("A module reports %s missing. No installed CSV owns the %s kind, and no package in this cluster's catalogs does. %s", d.Mention, d.OperandKind, impact)
		fixes = append(fixes, fmt.Sprintf("Install the operator that provides %s (the module's message names its purpose), then create %s.", d.OperandKind, d.Mention))
		p.TechnicalCmd = "oc get crd -o custom-columns=NAME:.metadata.name,KIND:.spec.names.kind | grep -w " + shellQuote(d.OperandKind)
	case d.Package == nil:
		p.Title = fmt.Sprintf("Prerequisite operator %q is not installed", d.Mention)
		p.Description = fmt.Sprintf("%q is not installed, and no package in this cluster's catalogs matches that name. %s", d.Mention, impact)
		fixes = append(fixes, fmt.Sprintf("Find the operator in the OpenShift console (Ecosystem or Operators > OperatorHub, search %q) and install it; prefer a Red Hat provided package. If the catalogs of this cluster are restricted (disconnected install), mirror the operator first.", d.Mention))
		p.TechnicalCmd = shellCommand("oc", "get", "packagemanifests", "-n", CatalogNS, "-o", "custom-columns=NAME:.metadata.name,CATALOG:.status.catalogSource,DISPLAY:.status.channels[0].currentCSVDesc.displayName") +
			" | grep -i " + shellQuote(firstWord(d.Mention))
	default:
		pkg := d.Package
		p.Title = fmt.Sprintf("Prerequisite operator %s is not installed", name)
		p.Description = fmt.Sprintf("%q is not installed. It is package %s in the %s catalog. %s", d.Mention, pkg.Name, pkg.Catalog, impact)
		plan, err := planInstall(c, *pkg, d.Operand)
		if err != nil {
			fixes = append(fixes, fmt.Sprintf("Install %s (package %s, channel %s, catalog %s) from the OpenShift console: %v", name, pkg.Name, pkg.DefaultChannel, pkg.Catalog, err))
			break
		}
		evidence = append(evidence, plan.evidence...)
		fixes = append(fixes, fmt.Sprintf("Install %s with the commands below (as a cluster administrator; the tool does not install operators). %s", name, strings.Join(plan.notes, " ")))
		p.TechnicalCmd = plan.script
	}
	p.Evidence = evidence
	p.Fix = strings.Join(fixes, " ")
	return p
}

func firstWord(s string) string {
	for _, w := range strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-' }) {
		if !operatorNameStopWords[strings.ToLower(w)] {
			return w
		}
	}
	return s
}

// prerequisiteInstall is the copyable install of one package.
type prerequisiteInstall struct {
	script   string
	notes    []string
	evidence []string
}

// planInstall builds the Namespace, OperatorGroup and Subscription for a
// package from its catalog metadata, the wait commands, and the singleton
// operand when the package has one.
func planInstall(c *Client, p catalogPackage, operand *almExample) (prerequisiteInstall, error) {
	for _, s := range []string{p.Name, p.Catalog, p.CatalogNamespace, p.DefaultChannel} {
		if s == "" || !dns1123Subdomain.MatchString(s) {
			return prerequisiteInstall{}, fmt.Errorf("the catalog entry has an unexpected name, channel or source (%q)", s)
		}
	}
	var plan prerequisiteInstall
	var objects []string
	ns := ""
	switch {
	case p.supports("OwnNamespace"):
		ns = p.SuggestedNS
		if ns == "" {
			ns = p.Name
			if !strings.HasPrefix(ns, "openshift-") {
				ns = "openshift-" + ns
			}
		}
		if !dns1123Label.MatchString(ns) {
			return prerequisiteInstall{}, fmt.Errorf("the suggested namespace %q is not a valid namespace name", ns)
		}
		nsObj := "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: " + ns
		if p.ClusterMonitor {
			nsObj += "\n  labels:\n    openshift.io/cluster-monitoring: \"true\""
		}
		objects = append(objects, nsObj)
		where := "its suggested namespace"
		if p.SuggestedNS == "" {
			where = "a namespace named after the package (the CSV suggests none)"
		}
		plan.notes = append(plan.notes, fmt.Sprintf("It goes into %s %s, watching only that namespace (install mode OwnNamespace, which the CSV supports).", where, ns))
		ogs, err := listOperatorGroups(c, ns)
		switch {
		case err != nil:
			plan.notes = append(plan.notes, fmt.Sprintf("Could not check for an existing OperatorGroup in %s (%v); if one exists, leave the OperatorGroup out: a namespace may have only one, and it must target a mode the CSV supports.", ns, err))
			fallthrough
		case len(ogs) == 0:
			objects = append(objects, "apiVersion: operators.coreos.com/v1\nkind: OperatorGroup\nmetadata:\n  name: "+ns+"\n  namespace: "+ns+"\nspec:\n  targetNamespaces:\n  - "+ns)
		default:
			note, err := reuseOperatorGroup(p, ns, ogs)
			if err != nil {
				return prerequisiteInstall{}, err
			}
			plan.notes = append(plan.notes, note)
		}
	case p.supports("AllNamespaces"):
		ns = "openshift-operators"
		plan.notes = append(plan.notes, "The CSV does not support OwnNamespace, so it is installed for all namespaces into openshift-operators.")
		ogs, err := listOperatorGroups(c, ns)
		switch {
		case err != nil:
			plan.notes = append(plan.notes, fmt.Sprintf("Could not read the OperatorGroup of openshift-operators (%v); OpenShift creates global-operators there, which watches all namespaces.", err))
		case len(ogs) == 0:
			objects = append(objects, "apiVersion: operators.coreos.com/v1\nkind: OperatorGroup\nmetadata:\n  name: global-operators\n  namespace: openshift-operators\nspec: {}")
			plan.notes = append(plan.notes, "openshift-operators has no OperatorGroup (OpenShift normally creates global-operators), so one that watches all namespaces is created.")
		default:
			note, err := reuseOperatorGroup(p, ns, ogs)
			if err != nil {
				return prerequisiteInstall{}, err
			}
			plan.notes = append(plan.notes, note)
		}
	default:
		return prerequisiteInstall{}, fmt.Errorf("the CSV supports only the install modes %s, which need a target namespace chosen by you", nonEmpty(strings.Join(p.InstallModes, ", "), "(none)"))
	}
	objects = append(objects, fmt.Sprintf("apiVersion: operators.coreos.com/v1alpha1\nkind: Subscription\nmetadata:\n  name: %s\n  namespace: %s\nspec:\n  channel: %s\n  name: %s\n  source: %s\n  sourceNamespace: %s\n  installPlanApproval: Automatic",
		p.Name, ns, p.DefaultChannel, p.Name, p.Catalog, p.CatalogNamespace))
	lines := []string{
		"oc apply -f - <<'EOF'\n" + strings.Join(objects, "\n---\n") + "\nEOF",
		shellCommand("oc", "wait", "subscription/"+p.Name, "-n", ns, "--for=jsonpath={.status.state}=AtLatestKnown", "--timeout=10m"),
		"oc wait csv/\"$(oc get subscription/" + p.Name + " -n " + ns + " -o jsonpath='{.status.installedCSV}')\" -n " + ns + " --for=jsonpath='{.status.phase}'=Succeeded --timeout=10m",
	}
	plan.notes = append(plan.notes, fmt.Sprintf("The Subscription follows the default channel %s of %s with automatic updates; the two oc wait lines return once the operator is installed.", p.DefaultChannel, p.Catalog))
	plan.evidence = append(plan.evidence, fmt.Sprintf("Install modes supported by %s: %s; suggested namespace: %s", nonEmpty(p.CurrentCSV, p.Name), strings.Join(p.InstallModes, ", "), nonEmpty(p.SuggestedNS, "none")))
	if operand != nil {
		cmd, err := operandCommand(*operand)
		if err == nil {
			lines = append(lines, cmd)
			plan.notes = append(plan.notes, fmt.Sprintf("The last command creates %s from the CSV's example, which the operator needs before it does anything; some operators create it themselves, so it is created only if it is still missing.", operand.ref()))
		}
	}
	plan.script = strings.Join(lines, "\n")
	return plan, nil
}

// operandCommand creates <Kind>/cluster from the CSV example unless it
// exists. The quoted here-document keeps the shell from expanding it, and
// the JSON is re-encoded so no line can end the here-document early.
func operandCommand(e almExample) (string, error) {
	if !kindPattern.MatchString(e.Kind) || !groupVersion.MatchString(e.APIVersion) || !dns1123Subdomain.MatchString(e.objName()) || e.Namespace != "" {
		return "", fmt.Errorf("the example has an unexpected apiVersion, kind, name or a namespace")
	}
	obj := map[string]interface{}{"apiVersion": e.APIVersion, "kind": e.Kind, "metadata": map[string]string{"name": e.objName()}}
	if len(e.Spec) > 0 && string(e.Spec) != "null" {
		obj["spec"] = e.Spec
	}
	data, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		return "", err
	}
	return shellCommand("oc", "get", e.resourceArg()+"/"+e.objName()) + " >/dev/null 2>&1 || oc create -f - <<'EOF'\n" + string(data) + "\nEOF", nil
}

// operatorGroup is the part of an OperatorGroup that decides the install
// mode of the operators in its namespace.
type operatorGroup struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		TargetNamespaces []string                   `json:"targetNamespaces"`
		Selector         map[string]json.RawMessage `json:"selector"`
	} `json:"spec"`
}

// installMode is the OLM install mode the OperatorGroup gives operators in
// namespace ns ("" when a label selector picks the namespaces, which the
// tool cannot evaluate).
func (og operatorGroup) installMode(ns string) string {
	t := og.Spec.TargetNamespaces
	switch {
	case len(og.Spec.Selector) > 0:
		return ""
	case len(t) == 0:
		return "AllNamespaces"
	case len(t) == 1 && t[0] == ns:
		return "OwnNamespace"
	case len(t) == 1:
		return "SingleNamespace"
	}
	return "MultiNamespace"
}

// listOperatorGroups returns the OperatorGroups of a namespace (none when
// it does not exist).
func listOperatorGroups(c *Client, namespace string) ([]operatorGroup, error) {
	body, _, err := c.get(namespacedPath("operators.coreos.com/v1", "operatorgroups", namespace, ""))
	if IsK8sError(err, http.StatusNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []operatorGroup `json:"items"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("parse OperatorGroups in %s: %w", namespace, err)
	}
	return list.Items, nil
}

// reuseOperatorGroup checks that the namespace's existing OperatorGroup
// gives an install mode the CSV supports (OLM fails the CSV with
// UnsupportedOperatorGroup otherwise, and with TooManyOperatorGroups when
// a namespace has several).
func reuseOperatorGroup(p catalogPackage, ns string, ogs []operatorGroup) (string, error) {
	if len(ogs) > 1 {
		var names []string
		for _, og := range ogs {
			names = append(names, og.Metadata.Name)
		}
		sort.Strings(names)
		return "", fmt.Errorf("namespace %s has %d OperatorGroups (%s); OLM installs nothing there until only one is left (TooManyOperatorGroups), so the tool gives no commands for it", ns, len(ogs), strings.Join(names, ", "))
	}
	og := ogs[0]
	mode := og.installMode(ns)
	switch {
	case mode == "":
		return "", fmt.Errorf("namespace %s has OperatorGroup %s, which picks its target namespaces with a label selector; check that it gives an install mode the CSV supports (%s) and install from the console", ns, og.Metadata.Name, strings.Join(p.InstallModes, ", "))
	case !p.supports(mode):
		return "", fmt.Errorf("namespace %s has OperatorGroup %s with install mode %s (targetNamespaces %s), which %s does not support (%s); OLM would fail the install. Use another namespace or change that OperatorGroup",
			ns, og.Metadata.Name, mode, nonEmpty(strings.Join(og.Spec.TargetNamespaces, ", "), "all"), nonEmpty(p.CurrentCSV, p.Name), strings.Join(p.InstallModes, ", "))
	}
	return fmt.Sprintf("%s already has OperatorGroup %s (install mode %s, which the CSV supports), so none is created: a namespace may have only one.", ns, og.Metadata.Name, mode), nil
}
