package cluster

// RBAC of the gitops profile: deploy/rbac-gitops.yaml must grant exactly the
// ServiceAccount requests that the gitops profile can make, as
// rbac_coverage_test.go checks deploy/template.yaml against every request
// the package makes.
//
// What the profile can make is found from the code: a reference graph over
// the type-checked sources of main.go, pkg/api and this package. A function
// reaches every function, method and package-level variable its body
// (closures included) names, called or not, and a variable reaches what its
// initializer and every assignment to it name; a function value passed
// along is reached where it is named. The roots are main.main and the init
// functions, with api.Register replaced by the gitops profile's handlers
// (api.gitOpsRoutes): Register adds no other handler in that profile, and
// refusedInGitOpsProfile answers without a call. The ServiceAccount
// requests of the reached functions of this package are then derived as
// for the full profile, with each parameter that makes up a path resolved
// from reached call sites only.
//
// Interface method calls (the packages declare no interface of their own)
// are not followed into implementations; TestEveryServiceAccountCallIsReachable
// fails if that, or anything else, hides a function that calls the API from
// the full profile's roots.

import (
	"bufio"
	"bytes"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	modulePath    = "github.com/juntwang/rhoai-nightly-updater"
	clusterPkg    = modulePath + "/pkg/cluster"
	apiPkg        = modulePath + "/pkg/api"
	rbacGitOpsRel = "../../deploy/rbac-gitops.yaml"
)

// typedPkg is one package type-checked from its source.
type typedPkg struct {
	pkg   *types.Package
	info  *types.Info
	fset  *token.FileSet
	files map[string]*ast.File // by base name
}

var (
	exportOnce  sync.Once
	exportFiles map[string]string
	exportErr   error
)

// exportData maps each import path the three packages depend on to its
// compiled export data, from `go list -export` (the go command builds them
// into its cache), so the packages type-check without any other module.
func exportData(t *testing.T) map[string]string {
	t.Helper()
	exportOnce.Do(func() {
		cmd := exec.Command("go", "list", "-export", "-deps", "-f", "{{.ImportPath}}\t{{.Export}}", modulePath)
		cmd.Dir = "../.."
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			exportErr = fmt.Errorf("go list -export: %v: %s", err, stderr.String())
			return
		}
		exportFiles = map[string]string{}
		sc := bufio.NewScanner(bytes.NewReader(out))
		for sc.Scan() {
			path, file, _ := strings.Cut(sc.Text(), "\t")
			if file != "" {
				exportFiles[path] = file
			}
		}
	})
	if exportErr != nil {
		t.Fatal(exportErr)
	}
	return exportFiles
}

// typeCheck parses the non-test Go files of dir (or the one file) and
// type-checks them as package path.
func typeCheck(t *testing.T, path, dir string, only ...string) *typedPkg {
	t.Helper()
	exports := exportData(t)
	fset := token.NewFileSet()
	names := only
	if len(names) == 0 {
		matches, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range matches {
			if !strings.HasSuffix(m, "_test.go") {
				names = append(names, filepath.Base(m))
			}
		}
	}
	tp := &typedPkg{fset: fset, files: map[string]*ast.File{}}
	var files []*ast.File
	for _, name := range names {
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		tp.files[name] = f
		files = append(files, f)
	}
	imp := importer.ForCompiler(fset, "gc", func(p string) (io.ReadCloser, error) {
		file, ok := exports[p]
		if !ok {
			return nil, fmt.Errorf("no export data for %s", p)
		}
		return os.Open(file)
	})
	tp.info = &types.Info{
		Defs:       map[*ast.Ident]types.Object{},
		Uses:       map[*ast.Ident]types.Object{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
	}
	conf := types.Config{Importer: imp}
	pkg, err := conf.Check(path, fset, files, tp.info)
	if err != nil {
		t.Fatalf("type-check %s: %v", path, err)
	}
	tp.pkg = pkg
	return tp
}

// funcKey names a function or method the same way in every package that
// refers to it: "<pkg>.<name>", or "<pkg>.<type>.<method>" (pointer and
// type arguments dropped).
func funcKey(f *types.Func) string {
	f = f.Origin()
	if f.Pkg() == nil {
		return "builtin." + f.Name()
	}
	sig, _ := f.Type().(*types.Signature)
	if sig != nil && sig.Recv() != nil {
		rt := sig.Recv().Type()
		if p, ok := rt.(*types.Pointer); ok {
			rt = p.Elem()
		}
		if n, ok := types.Unalias(rt).(*types.Named); ok {
			return f.Pkg().Path() + "." + n.Origin().Obj().Name() + "." + f.Name()
		}
		return f.Pkg().Path() + ".<interface>." + f.Name()
	}
	return f.Pkg().Path() + "." + f.Name()
}

// varKey names a package-level variable.
func varKey(v *types.Var) string {
	return "var " + v.Pkg().Path() + "." + v.Name()
}

func packageLevelVar(obj types.Object) (*types.Var, bool) {
	v, ok := obj.(*types.Var)
	if !ok || v.Pkg() == nil || v.IsField() || v.Pkg().Scope().Lookup(v.Name()) != v {
		return nil, false
	}
	return v, true
}

// refGraph: node -> the nodes it names.
type refGraph map[string]map[string]bool

func (g refGraph) add(from, to string) {
	if g[from] == nil {
		g[from] = map[string]bool{}
	}
	g[from][to] = true
}

// addRefs adds an edge from "from" to every function, method and
// package-level variable that node names.
func (g refGraph) addRefs(tp *typedPkg, from string, node ast.Node) {
	ast.Inspect(node, func(n ast.Node) bool {
		var obj types.Object
		switch x := n.(type) {
		case *ast.SelectorExpr:
			if sel := tp.info.Selections[x]; sel != nil {
				obj = sel.Obj()
			}
		case *ast.Ident:
			obj = tp.info.Uses[x]
		}
		switch o := obj.(type) {
		case *types.Func:
			g.add(from, funcKey(o))
		case *types.Var:
			if v, ok := packageLevelVar(o); ok {
				g.add(from, varKey(v))
			}
		}
		return true
	})
}

// addPackage adds the package's functions, methods and package-level
// variables to the graph.
func (g refGraph) addPackage(tp *typedPkg) {
	for _, f := range tp.files {
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				fn, _ := tp.info.Defs[d.Name].(*types.Func)
				if fn == nil || d.Body == nil {
					continue
				}
				from := funcKey(fn)
				g.add(from, from)
				g.addRefs(tp, from, d.Body)
				// An assignment to a package-level variable (a seam such as
				// runUpdateStream) makes the variable reach the value.
				ast.Inspect(d.Body, func(n ast.Node) bool {
					as, ok := n.(*ast.AssignStmt)
					if !ok || as.Tok != token.ASSIGN {
						return true
					}
					for i, lhs := range as.Lhs {
						id, ok := lhs.(*ast.Ident)
						if !ok {
							continue
						}
						if v, ok := packageLevelVar(tp.info.Uses[id]); ok {
							rhs := as.Rhs[0]
							if len(as.Rhs) == len(as.Lhs) {
								rhs = as.Rhs[i]
							}
							g.addRefs(tp, varKey(v), rhs)
						}
					}
					return true
				})
			case *ast.GenDecl:
				if d.Tok != token.VAR {
					continue
				}
				for _, spec := range d.Specs {
					vs := spec.(*ast.ValueSpec)
					for i, name := range vs.Names {
						v, ok := tp.info.Defs[name].(*types.Var)
						if !ok {
							continue
						}
						from := varKey(v)
						g.add(from, from)
						switch {
						case len(vs.Values) == len(vs.Names):
							g.addRefs(tp, from, vs.Values[i])
						case len(vs.Values) == 1:
							g.addRefs(tp, from, vs.Values[0])
						}
					}
				}
			}
		}
	}
}

// reach returns every node reachable from roots, never entering pruned.
func (g refGraph) reach(roots []string, pruned map[string]bool) map[string]bool {
	seen := map[string]bool{}
	stack := append([]string(nil), roots...)
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[n] || pruned[n] {
			continue
		}
		seen[n] = true
		for next := range g[n] {
			stack = append(stack, next)
		}
	}
	return seen
}

// profileGraph is the reference graph of the binary, and what it needs.
type profileGraph struct {
	graph   refGraph
	cluster *typedPkg
	// funcPositions maps this package's function keys to their funcPos
	// ("file:line"), the key serviceAccountRequestsIn uses.
	funcPositions map[string]string
}

var (
	profileGraphOnce sync.Once
	profileGraphVal  *profileGraph
)

func loadProfileGraph(t *testing.T) *profileGraph {
	t.Helper()
	profileGraphOnce.Do(func() {
		clusterTP := typeCheck(t, clusterPkg, ".")
		apiTP := typeCheck(t, apiPkg, "../api")
		mainTP := typeCheck(t, "main", "../..", "main.go")
		g := refGraph{}
		g.addPackage(clusterTP)
		g.addPackage(apiTP)
		g.addPackage(mainTP)
		positions := map[string]string{}
		for name, f := range clusterTP.files {
			for _, decl := range f.Decls {
				if d, ok := decl.(*ast.FuncDecl); ok {
					if fn, _ := clusterTP.info.Defs[d.Name].(*types.Func); fn != nil {
						positions[funcKey(fn)] = fmt.Sprintf("%s:%d", name, clusterTP.fset.Position(d.Pos()).Line)
					}
				}
			}
		}
		profileGraphVal = &profileGraph{graph: g, cluster: clusterTP, funcPositions: positions}
	})
	if profileGraphVal == nil {
		t.Fatal("the reference graph could not be built")
	}
	return profileGraphVal
}

func (pg *profileGraph) roots() []string {
	roots := []string{"main.main"}
	for _, p := range []string{"main", apiPkg, clusterPkg} {
		roots = append(roots, p+".init")
	}
	return roots
}

// gitOpsReach is what the gitops profile reaches: api.Register is replaced
// by the handlers of api.gitOpsRoutes.
func (pg *profileGraph) gitOpsReach() map[string]bool {
	return pg.graph.reach(append(pg.roots(), "var "+apiPkg+".gitOpsRoutes"), map[string]bool{apiPkg + ".Register": true})
}

// fullReach is what the full profile reaches.
func (pg *profileGraph) fullReach() map[string]bool {
	return pg.graph.reach(pg.roots(), nil)
}

// clusterFuncs turns a reach into the funcPos set of this package.
func (pg *profileGraph) clusterFuncs(reach map[string]bool) map[string]bool {
	out := map[string]bool{}
	for key, pos := range pg.funcPositions {
		if reach[key] {
			out[pos] = true
		}
	}
	return out
}

// The graph must find every function that calls the API with the
// ServiceAccount from the full profile's roots; one it misses would also
// be missing from the gitops profile's reach without anyone noticing.
func TestEveryServiceAccountCallIsReachable(t *testing.T) {
	pg := loadProfileGraph(t)
	reached := pg.clusterFuncs(pg.fullReach())
	reqs, _ := serviceAccountRequests(t)
	var missing []string
	for _, r := range reqs {
		if !reached[r.Func] {
			missing = append(missing, r.Func+" ("+r.Site+")")
		}
	}
	if len(missing) > 0 {
		t.Fatalf("the reference graph does not reach these functions with API calls from main.main; follow how they are called (or delete dead code):\n%s",
			strings.Join(dedupe(missing), "\n"))
	}
	// The gitops roots are a strict part of the full ones.
	if g, f := len(pg.gitOpsReach()), len(pg.fullReach()); g == 0 || g >= f {
		t.Fatalf("gitops profile reaches %d nodes, the full profile %d", g, f)
	}
}

// gitOpsRequests are the ServiceAccount requests the gitops profile can make.
func gitOpsRequests(t *testing.T) []rbacRequest {
	t.Helper()
	pg := loadProfileGraph(t)
	reqs, _ := serviceAccountRequestsIn(t, pg.clusterFuncs(pg.gitOpsReach()))
	if len(reqs) == 0 {
		t.Fatal("the gitops profile reaches no API call")
	}
	return reqs
}

func TestGitOpsRBACCoversEveryReachableServiceAccountCall(t *testing.T) {
	reqs := gitOpsRequests(t)
	grants := templateGrantsFrom(t, rbacGitOpsRel)
	missing := map[string][]string{}
	for _, r := range reqs {
		ok := false
		for _, g := range grants {
			if g.allows(r) {
				ok = true
				break
			}
		}
		if !ok {
			missing[r.key()] = append(missing[r.key()], r.Site)
		}
	}
	if len(missing) > 0 {
		var lines []string
		for k, sites := range missing {
			lines = append(lines, fmt.Sprintf("%s  (%s)", k, strings.Join(dedupe(sites), ", ")))
		}
		sort.Strings(lines)
		t.Errorf("deploy/rbac-gitops.yaml does not grant these requests of the gitops profile:\n%s", strings.Join(lines, "\n"))
	}
}

func TestGitOpsRBACGrantsNothingTheProfileDoesNotReach(t *testing.T) {
	reqs := gitOpsRequests(t)
	for _, g := range templateGrantsFrom(t, rbacGitOpsRel) {
		if g.source == "built-in basic-user" {
			continue
		}
		for _, group := range g.rule.APIGroups {
			for _, res := range g.rule.Resources {
				for _, verb := range g.rule.Verbs {
					used := false
					for _, r := range reqs {
						if r.Verb == verb && r.Group == group && r.Resource == res && g.allows(r) {
							used = true
							break
						}
					}
					if !used {
						t.Errorf("%s grants %s %s/%s (namespace %q), which the gitops profile never requests; remove it", g.source, verb, group, res, g.namespace)
					}
				}
			}
		}
	}
}

// What the gitops profile must never be able to do, whatever the code
// reaches: the audit's findings against the full profile's RBAC.
func TestGitOpsRBACHasNoClusterWideWrites(t *testing.T) {
	writes := map[string]bool{"create": true, "update": true, "patch": true, "delete": true, "deletecollection": true, "*": true, "escalate": true, "bind": true, "impersonate": true}
	for _, g := range templateGrantsFrom(t, rbacGitOpsRel) {
		for _, v := range g.rule.Verbs {
			if g.namespace == "" && writes[v] {
				t.Errorf("%s: %s on %v/%v is cluster-wide", g.source, v, g.rule.APIGroups, g.rule.Resources)
			}
		}
		for _, res := range g.rule.Resources {
			if res == "*" && g.rule.APIGroups[0] != "components.platform.opendatahub.io" && g.rule.APIGroups[0] != "operator.openshift.io" {
				t.Errorf("%s: resources * in %v", g.source, g.rule.APIGroups)
			}
			switch res {
			case "secrets":
				for _, v := range g.rule.Verbs {
					if v != "get" || len(g.rule.ResourceNames) == 0 {
						t.Errorf("%s: %s secrets %v; only get by name is allowed", g.source, v, g.rule.ResourceNames)
					}
				}
			case "validatingwebhookconfigurations", "mutatingwebhookconfigurations", "deployments", "operatorgroups", "namespaces":
				for _, v := range g.rule.Verbs {
					if writes[v] {
						t.Errorf("%s: %s %s", g.source, v, res)
					}
				}
			}
		}
	}
}

// gitOpsTemplate is deploy/rbac-gitops.yaml, decoded loosely.
type gitOpsTemplate struct {
	Parameters []templateParameter `yaml:"parameters"`
	Objects    []map[string]interface{}
}

func readGitOpsTemplate(t *testing.T) gitOpsTemplate {
	t.Helper()
	data, err := os.ReadFile(rbacGitOpsRel)
	if err != nil {
		t.Fatal(err)
	}
	var tmpl gitOpsTemplate
	if err := yaml.Unmarshal(data, &tmpl); err != nil {
		t.Fatal(err)
	}
	return tmpl
}

// The chart renders deploy/rbac-gitops.yaml with deploy/template.yaml's
// parameters: each of its parameters must be one of those, with the same
// default, except OPERATOR_NAMESPACE, which is the code's.
func TestGitOpsTemplateParameters(t *testing.T) {
	data, err := os.ReadFile(templateRel)
	if err != nil {
		t.Fatal(err)
	}
	var main struct {
		Parameters []templateParameter `yaml:"parameters"`
	}
	if err := yaml.Unmarshal(data, &main); err != nil {
		t.Fatal(err)
	}
	defaults := map[string]string{}
	for _, p := range main.Parameters {
		defaults[p.Name] = p.Value
	}
	tmpl := readGitOpsTemplate(t)
	seen := map[string]bool{}
	for _, p := range tmpl.Parameters {
		seen[p.Name] = true
		if p.Name == "OPERATOR_NAMESPACE" {
			if p.Value != SubNS {
				t.Errorf("OPERATOR_NAMESPACE defaults to %q, but the code uses %q", p.Value, SubNS)
			}
			if _, clash := defaults[p.Name]; clash {
				t.Error("deploy/template.yaml has OPERATOR_NAMESPACE too; keep one definition")
			}
			continue
		}
		want, ok := defaults[p.Name]
		switch {
		case !ok:
			t.Errorf("parameter %s is not a parameter of deploy/template.yaml", p.Name)
		case want != p.Value:
			t.Errorf("parameter %s defaults to %q here and to %q in deploy/template.yaml", p.Name, p.Value, want)
		}
	}
	for _, name := range []string{"NAMESPACE", "APP_NAME", "PULL_SECRET_NAMESPACE", "MARKETPLACE_NAMESPACE", "OPERATOR_NAMESPACE"} {
		if !seen[name] {
			t.Errorf("parameter %s is missing", name)
		}
	}
	if defaults["MARKETPLACE_NAMESPACE"] != CatalogNS {
		t.Errorf("MARKETPLACE_NAMESPACE defaults to %q, but the code uses %q", defaults["MARKETPLACE_NAMESPACE"], CatalogNS)
	}
	// `oc process` drops literal namespaces (TestTemplateNamespacesAreParameters).
	for _, o := range tmpl.Objects {
		meta, _ := o["metadata"].(map[string]interface{})
		if ns, _ := meta["namespace"].(string); ns != "" && !strings.HasPrefix(ns, "${") {
			t.Errorf("%v %v has the literal namespace %q", o["kind"], meta["name"], ns)
		}
	}
}

// The admission policy names what the code writes: the Subscription and
// package, the catalog, the operator's CSV prefix, the ServiceAccount the
// binding is for; it never reads labels or annotations, which a
// ValidatingAdmissionPolicy cannot see.
func TestGitOpsAdmissionPolicyMatchesTheCode(t *testing.T) {
	tmpl := readGitOpsTemplate(t)
	var policy, binding map[string]interface{}
	for _, o := range tmpl.Objects {
		switch o["kind"] {
		case "ValidatingAdmissionPolicy":
			policy = o
		case "ValidatingAdmissionPolicyBinding":
			binding = o
		}
		if strings.HasPrefix(fmt.Sprint(o["apiVersion"]), "admissionregistration.k8s.io/") && o["apiVersion"] != "admissionregistration.k8s.io/v1" {
			t.Errorf("%v uses %v; use the GA admissionregistration.k8s.io/v1", o["kind"], o["apiVersion"])
		}
	}
	if policy == nil || binding == nil {
		t.Fatal("deploy/rbac-gitops.yaml has no ValidatingAdmissionPolicy and binding")
	}
	meta := policy["metadata"].(map[string]interface{})
	if meta["name"] != "${APP_NAME}-${NAMESPACE}-gitops" || binding["metadata"].(map[string]interface{})["name"] != meta["name"] {
		t.Errorf("the policy and binding must be named ${APP_NAME}-${NAMESPACE}-gitops, the name the Template passes in %s", GitOpsAdmissionPolicyEnv)
	}
	spec := policy["spec"].(map[string]interface{})
	if spec["failurePolicy"] != "Fail" {
		t.Errorf("failurePolicy = %v, want Fail", spec["failurePolicy"])
	}
	bspec := binding["spec"].(map[string]interface{})
	if bspec["policyName"] != meta["name"] || fmt.Sprint(bspec["validationActions"]) != "[Deny]" || bspec["matchResources"] != nil {
		t.Errorf("binding spec = %v", bspec)
	}
	conds, _ := spec["matchConditions"].([]interface{})
	if len(conds) != 1 || !strings.Contains(fmt.Sprint(conds[0]), "request.userInfo.username == 'system:serviceaccount:${NAMESPACE}:${APP_NAME}'") {
		t.Errorf("matchConditions = %v, want the updater's ServiceAccount only", conds)
	}
	rules := fmt.Sprint(spec["matchConstraints"])
	for _, res := range []string{"subscriptions", "catalogsources", "operatorgroups", "installplans", "clusterserviceversions"} {
		if !strings.Contains(rules, res) {
			t.Errorf("the policy does not match %s", res)
		}
	}
	var exprs []string
	for _, v := range spec["validations"].([]interface{}) {
		exprs = append(exprs, v.(map[string]interface{})["expression"].(string))
	}
	all := strings.Join(exprs, "\n") + fmt.Sprint(spec["variables"])
	for _, want := range []string{
		"request.name == '" + SubName + "'",
		"object.spec.name == '" + SubName + "'",
		"object.spec.source == '" + CatalogName + "'",
		"object.spec.sourceNamespace == '${MARKETPLACE_NAMESPACE}'",
		"request.namespace == '${OPERATOR_NAMESPACE}'",
		"'${SUBSCRIPTION_ENV_ALLOWLIST}'.split(',')",
		"startsWith('" + SubName + ".')",
		"request.resource.resource != 'operatorgroups'",
		"request.resource.resource != 'catalogsources'",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("the policy does not check %s", want)
		}
	}
	if strings.Contains(all, "metadata.labels") || strings.Contains(all, "metadata.annotations") {
		t.Error("the policy reads labels or annotations, which a ValidatingAdmissionPolicy cannot see")
	}
}

// The gitops profile never writes a CatalogSource or an OperatorGroup and
// never creates an InstallPlan: OLM rewrites a CatalogSource's image from
// its olm.catalogImageTemplate annotation, which the admission policy
// cannot see, so no catalog write may be reachable at all.
func TestGitOpsProfileReachesNoCatalogWrite(t *testing.T) {
	for _, r := range gitOpsRequests(t) {
		write := r.Verb != "get" && r.Verb != "list"
		switch {
		case write && (r.Resource == "catalogsources" || r.Resource == "operatorgroups" || r.Resource == "namespaces"):
			t.Errorf("the gitops profile reaches %s (%s)", r.key(), r.Site)
		case r.Resource == "installplans" && r.Verb == "create":
			t.Errorf("the gitops profile reaches %s (%s)", r.key(), r.Site)
		}
	}
}
