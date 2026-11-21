package cluster

// RBAC coverage: every Kubernetes API call the ServiceAccount makes must be
// granted by deploy/template.yaml, and every grant must be used by the code.
//
// The test parses the source of pkg/cluster and of every package under it,
// finds each call to a Client request method (Get, Post, Put, Patch,
// StrategicPatch, Apply, DryRunApply, Delete, Do, and the REST helpers on
// top of them; calls are told apart by type, so a cache's Get or an
// http.Client's Do is none), resolves the request path
// statically (through plain f(...) and qualified pkg.F(...) calls and
// constants of those packages), and maps it to the RBAC request
// attributes the API server checks (verb, API group, resource, namespace and
// name; https://kubernetes.io/docs/reference/access-authn-authz/authorization/#determine-the-request-verb).
// Error, rollback and restore branches are ordinary calls, so they are
// covered like the happy path. A call whose path cannot be resolved fails the
// test until it is resolved or listed in rbacCallsNotUsingTheSAToken.

import (
	"errors"
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	unknownPart = "\x00"  // a path piece only known at run time
	appNSMarker = "${NS}" // the updater's own namespace (template NAMESPACE)
	anyNS       = "*"     // a namespace chosen at run time
	templateRel = "../../deploy/template.yaml"
	// rbacResolveDepth bounds how many assignments, parameters and returns
	// a path is followed through (a MaaSModelRef path goes through five).
	rbacResolveDepth = 6
)

// Calls made with a token other than the ServiceAccount's. They are checked
// against the user's own RBAC by the API server, not against the template.
var rbacCallsNotUsingTheSAToken = map[string]string{
	"authz.go:CheckUserPermissionWithToken": "SubjectAccessReview sent with the user's token",
	"authz.go:LookupUserWithToken":          "users/~ read with the user's token",
	"authz.go:ReadSessionToken":             "the user's own OAuth access token, read with that token",
	// The hard reset's namespace guard reads with the signed-in
	// cluster-admin's own client (SignedInUser): the namespaces, the
	// owners of any cluster-scoped kind and their API discovery. The
	// ServiceAccount is granted none of those kinds on purpose.
	"dsc_hard_reset_teardown.go:namespaceOwnerBlocker": "hard reset: lists the namespaces with the signed-in user's token",
	"dsc_hard_reset_teardown.go:read":                  "hard reset: reads a namespace's owner, of any cluster-scoped kind, with the signed-in user's token",
	"dsc_hard_reset_teardown.go:path":                  "hard reset: API discovery of the group of a namespace's owner, with the signed-in user's token",
	// Removing a component that serves a CRD conversion webhook lists that
	// CRD's objects (of any API group) with the signed-in cluster-admin's
	// own client, and refuses without one (conversionBlocker). The
	// ServiceAccount is granted none of those kinds on purpose.
	"diagnostics_disable.go:listConversionObjects": "component removal: lists the objects of a CRD whose conversion webhook the component serves, with the signed-in user's token",
}

// Reads made with the signed-in user's own token by the read routes of
// tier user-token and a user's project status (pkg/api route_tiers.go
// withUserRead; pkg/cluster user_reads.go). These functions refuse a
// client that does not carry the user's token (errNotUserClient,
// TestUserReadsRefuseTheServiceAccount), so the API server checks them
// against the user's RBAC, and the template grants the ServiceAccount
// nothing on their account (it gains no permission for the per-user
// views). Shared helpers those reads also call (listDSPAs, modelState,
// getServed...) stay in the ServiceAccount's scan: the scan cannot tell
// their callers' tokens apart, so it over-approximates.
var rbacUserTokenReads = map[string]string{
	"user_reads.go:ListUserDSProjects":              "the user's data science projects, through the projects API",
	"user_reads.go:listProjectLLMInferenceServices": "the LLMInferenceServices of one of the user's projects",
	"user_reads.go:listProjectInferenceServices":    "the InferenceServices of one of the user's projects",
}

// Best-effort reads of paths found by API discovery at run time, whose API
// group and resource cannot be named in the template. A 403 is expected for
// the resources the template does not grant, and is reported as not
// checked; the RHOAI resources the app uses are granted by name and are
// covered by the other call sites.
var rbacBestEffortCalls = map[string]string{
	"diagnostics_conversion.go:getDiscovered": "API versions check: one ?limit=1 list per resource and version of the multi-version *.opendatahub.io groups",
	"diagnostics_webhooks.go:probeCRDObjects": "stale CRD conversions: one ?limit=1 list per RHOAI CRD whose conversion Service is down, to tell whether it holds objects",
}

// Calls only the gitops profile makes (api.StartGitOpsPolicyWatch and
// HandleUpdateStreamGitOps; the watch returns at once in the full profile).
// deploy/template.yaml need not grant them; rbac_gitops_test.go checks them
// against deploy/rbac-gitops.yaml.
var rbacGitOpsOnlyCalls = map[string]string{
	"gitops_policy.go:CheckGitOpsAdmissionPolicy": "reads the gitops profile's admission policy and binding",
}

// gitOpsPolicyNameMarker is the policy name the Template passes in
// GITOPS_ADMISSION_POLICY (${APP_NAME}-${NAMESPACE}-gitops), with APP_NAME
// at its default, as templateGrantsFrom normalises resourceNames.
const gitOpsPolicyNameMarker = "rhoai-nightly-updater-" + appNSMarker + "-gitops"

// API groups of resources whose group is only known at run time (for
// example the MinIO objects whose apiVersion comes from the manifest).
var knownResourceGroups = map[string]string{
	"secrets":                "",
	"services":               "",
	"persistentvolumeclaims": "",
	"deployments":            "apps",
	"routes":                 "route.openshift.io",
	"datascienceclusters":    "datasciencecluster.opendatahub.io",
}

// API groups whose resources the code discovers at run time instead of
// naming them. components.platform.opendatahub.io holds only the RHOAI
// module CRs, and its kinds change with the operator version (3.6 added
// aihubs, aipipelines and mcplifecycleoperators), so the code lists them
// from API discovery. A request for a discovered resource needs a rule with
// resources ["*"] in that group; a rule naming fixed kinds would silently
// miss new ones.
var discoveredResourceGroups = map[string]bool{
	"components.platform.opendatahub.io": true,
	// The databaseservice module CR, and the RHOAI operator's own services
	// (rhods-operator rhoai-3.6 pkg/cluster/gvk/gvk.go), discovered the same
	// way (component_cleanup.go).
	"services.platform.opendatahub.io": true,
	// Singleton operands (<Kind>/cluster) of prerequisite operators, whose
	// kind comes from the operator's CSV (prerequisites.go); the rule is
	// limited to the name "cluster".
	"operator.openshift.io": true,
}

// Request verbs per Client method. Server-side apply creates the object when
// it is missing, and the API server then also checks "create" for that name
// (k8s.io/apiserver/pkg/endpoints/handlers/patch.go: createAuthorizerAttributes
// carries Verb "create" and the object's Name).
var methodVerbs = map[string][]string{
	"post":           {"create"},
	"put":            {"update"},
	"patch":          {"patch"},
	"strategicPatch": {"patch"},
	"apply":          {"patch", "create"},
	"dryRunApply":    {"patch", "create"},
	"delete":         {"delete"},
	// "get" becomes "get" or "list" depending on the path.
}

// requestSourceFiles are the files (by base name) that define the Client
// request methods and accessors (client.go, client_context.go) and the REST
// helpers on top of them (rest.go). Their calls take a path parameter, not
// a path: the scans attribute each request to the helper's call site. Named
// one by one, so any other file, a client_*.go feature file too, is scanned.
var requestSourceFiles = map[string]bool{
	"client.go":         true,
	"client_context.go": true,
	"rest.go":           true,
}

// isRequestSourceFile reports whether a file (by base name) is one of
// requestSourceFiles.
func isRequestSourceFile(base string) bool {
	return requestSourceFiles[base]
}

// requestMethods maps the Client request methods (client.go) to the names
// the scans derive verbs from (methodVerbs, toRequests): "get" becomes get
// or list depending on the path, "do" takes its http.Method argument.
var requestMethods = map[string]string{
	"Get":            "get",
	"Post":           "post",
	"Put":            "put",
	"Patch":          "patch",
	"StrategicPatch": "strategicPatch",
	"Apply":          "apply",
	"DryRunApply":    "dryRunApply",
	"Delete":         "delete",
	"Do":             "do",
}

// helperMethods maps each REST helper (rest.go) to the Client request
// method it sends for its path argument (the first, as for the request
// methods).
var helperMethods = map[string]string{
	"GetOptional":     "Get",
	"GetInto":         "Get",
	"GetIntoOptional": "Get",
	"ListInto":        "Get",
	"DeleteIfExists":  "Delete",
	"PatchJSON":       "Patch",
}

// requestMethod is the request a call sends when sel names a Client
// request method or REST helper (by type: clientMethodCalls), as a
// requestMethods value; false for any other call.
func (p *sourcePkg) requestMethod(sel *ast.SelectorExpr) (string, bool) {
	name, ok := p.clientCalls[sel]
	if !ok {
		return "", false
	}
	if m, ok := helperMethods[name]; ok {
		name = m
	}
	m, ok := requestMethods[name]
	return m, ok
}

// rawMethods maps the http.Method constants passed to Client.Do to the
// Client method with the same RBAC verbs.
var rawMethods = map[string]string{
	"MethodGet":    "get",
	"MethodPost":   "post",
	"MethodPut":    "put",
	"MethodPatch":  "patch",
	"MethodDelete": "delete",
}

func selectorName(e ast.Expr) string {
	if sel, ok := e.(*ast.SelectorExpr); ok {
		return sel.Sel.Name
	}
	return ""
}

type rbacRequest struct {
	Verb, Group, Resource, Namespace, Name string
	Site                                   string
	// NamedReview: an access review another server sends with the
	// object's name for every verb (a DSPA's kube-rbac-proxy). RBAC then
	// matches resourceNames for create too (k8s pkg/apis/rbac/v1/
	// evaluation_helpers.go ResourceNameMatches); a Kubernetes collection
	// POST carries no name, so create never matches a named rule there.
	NamedReview bool
	// Func is the function that makes the call, as "file:line" of its
	// declaration (funcPos).
	Func string
}

func (r rbacRequest) key() string {
	return fmt.Sprintf("%s %s/%s ns=%s name=%s", r.Verb, r.Group, r.Resource, r.Namespace, r.Name)
}

// symID names a package-level declaration (function, constant, variable or
// type) of one of the packages under pkg/cluster: its package's import path
// and its name.
type symID struct{ pkg, name string }

// sourcePkg is the source of pkg/cluster and every package under it. Files
// are keyed by base name, unique across the tree (loadClusterSource checks
// it), so call sites ("file:Func") and function positions ("file:line")
// keep their keys when a file moves to another package.
type sourcePkg struct {
	fset  *token.FileSet
	files map[string]*ast.File
	// pkgOf is the import path of each file's package; imports maps each
	// file's local import names to the import paths of the packages under
	// pkg/cluster it imports.
	pkgOf   map[string]string
	imports map[string]map[string]string
	consts  map[symID]string
	lits    map[symID]*ast.CompositeLit
	funcs   map[symID]*ast.FuncDecl
	fileOf  map[*ast.FuncDecl]string
	// clientCalls maps each selector that calls a Client method to the
	// method's name (clientMethodCalls).
	clientCalls map[*ast.SelectorExpr]string
	// only, when set, limits the scan to these functions (funcPos keys):
	// their API calls, and the call sites a parameter is resolved from.
	only map[string]bool

	// Memos of the resolver, valid for this scan (only is fixed): the
	// resolver re-walked the whole package for every parameter and call
	// result it met, which made the RBAC tests time out under -race.
	identMemo  map[identKey][]string
	resultMemo map[resultKey][]string
	callers    map[symID][]callSite // by callee; built on first use
	locals     map[*ast.FuncDecl]map[string]bool
}

// identKey and resultKey keep the depth, so a memo answers exactly what the
// depth-bounded walk would.
type identKey struct {
	fn    *ast.FuncDecl
	name  string
	depth int
}

type resultKey struct {
	callee symID
	i      int
	depth  int
}

// callSite is a call of a package function, by plain name (f(...)) or
// qualified by an imported package under pkg/cluster (pkg.F(...)), in a
// scanned function.
type callSite struct {
	caller *ast.FuncDecl
	call   *ast.CallExpr
}

// pkgOfFunc is the import path of fn's package.
func (p *sourcePkg) pkgOfFunc(fn *ast.FuncDecl) string {
	if fn == nil {
		return clusterPkg
	}
	return p.pkgOf[p.fileOf[fn]]
}

// localNames is every name fn declares, which could shadow an import: its
// receiver, parameters and results (closures' too), := and range := names,
// and var and const declarations in its body.
func (p *sourcePkg) localNames(fn *ast.FuncDecl) map[string]bool {
	if l, ok := p.locals[fn]; ok {
		return l
	}
	l := map[string]bool{}
	fields := func(fl *ast.FieldList) {
		if fl == nil {
			return
		}
		for _, f := range fl.List {
			for _, n := range f.Names {
				l[n.Name] = true
			}
		}
	}
	define := func(exprs ...ast.Expr) {
		for _, e := range exprs {
			if id, ok := e.(*ast.Ident); ok {
				l[id.Name] = true
			}
		}
	}
	fields(fn.Recv)
	ast.Inspect(fn, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.FuncType:
			fields(s.Params)
			fields(s.Results)
		case *ast.AssignStmt:
			if s.Tok == token.DEFINE {
				define(s.Lhs...)
			}
		case *ast.RangeStmt:
			if s.Tok == token.DEFINE {
				define(s.Key, s.Value)
			}
		case *ast.ValueSpec:
			for _, n := range s.Names {
				l[n.Name] = true
			}
		}
		return true
	})
	p.locals[fn] = l
	return l
}

// qualified resolves pkg.Name in fn, where pkg is the file's name for an
// imported package under pkg/cluster (and not a local of fn), to that
// package's declaration Name.
func (p *sourcePkg) qualified(fn *ast.FuncDecl, x *ast.SelectorExpr) (symID, bool) {
	base, ok := x.X.(*ast.Ident)
	if !ok || fn == nil {
		return symID{}, false
	}
	imp, ok := p.imports[p.fileOf[fn]][base.Name]
	if !ok || p.localNames(fn)[base.Name] {
		return symID{}, false
	}
	return symID{imp, x.Sel.Name}, true
}

// callee is the package function a call in fn names: f(...) in fn's own
// package, or pkg.F(...) through the file's imports.
func (p *sourcePkg) callee(fn *ast.FuncDecl, call *ast.CallExpr) (symID, bool) {
	switch f := call.Fun.(type) {
	case *ast.Ident:
		return symID{p.pkgOfFunc(fn), f.Name}, true
	case *ast.SelectorExpr:
		return p.qualified(fn, f)
	}
	return symID{}, false
}

// callSitesOf returns the calls of the package function id in the scanned
// functions, indexing every function body once.
func (p *sourcePkg) callSitesOf(id symID) []callSite {
	if p.callers == nil {
		p.callers = map[symID][]callSite{}
		for caller := range p.fileOf {
			if caller.Body == nil || !p.scans(caller) {
				continue
			}
			ast.Inspect(caller.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					if id, ok := p.callee(caller, call); ok {
						p.callers[id] = append(p.callers[id], callSite{caller, call})
					}
				}
				return true
			})
		}
	}
	return p.callers[id]
}

// frozen returns s with its capacity cut to its length, so a caller that
// appends to a memoized result gets a copy instead of writing into it.
func frozen(s []string) []string { return s[:len(s):len(s)] }

// funcPos identifies a function declaration of the package: "file:line".
func (p *sourcePkg) funcPos(fn *ast.FuncDecl) string {
	return fmt.Sprintf("%s:%d", p.fileOf[fn], p.fset.Position(fn.Pos()).Line)
}

// scans reports whether the scan covers fn.
func (p *sourcePkg) scans(fn *ast.FuncDecl) bool {
	return p.only == nil || p.only[p.funcPos(fn)]
}

// clusterSourceDirs are pkg/cluster and every directory under it that the
// go command builds (not testdata, nor names starting with "." or "_"),
// relative to pkg/cluster, with their non-test Go files.
func clusterSourceDirs(t *testing.T) map[string][]string {
	t.Helper()
	dirs := map[string][]string{}
	err := filepath.WalkDir(".", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if path != "." && (name == "testdata" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") && !strings.HasPrefix(name, ".") && !strings.HasPrefix(name, "_") {
			dir := filepath.Dir(path)
			dirs[dir] = append(dirs[dir], path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return dirs
}

// clusterImportPath is the import path of a directory relative to pkg/cluster.
func clusterImportPath(dir string) string {
	if dir == "." {
		return clusterPkg
	}
	return clusterPkg + "/" + filepath.ToSlash(dir)
}

// parsedSource is the non-test source of pkg/cluster and of every package
// under it, files keyed by base name.
type parsedSource struct {
	fset  *token.FileSet
	files map[string]*ast.File
	paths map[string]string // relative to pkg/cluster
}

// clusterSource parses the tree once per test binary: the source does not
// change while the tests run, and the scans only read the syntax trees.
// The scans key files by base name, so two files with the same base name
// anywhere in the tree fail it.
func clusterSource(t *testing.T) *parsedSource {
	t.Helper()
	clusterSourceOnce.Do(func() {
		ps := &parsedSource{fset: token.NewFileSet(), files: map[string]*ast.File{}, paths: map[string]string{}}
		var all []string
		for _, paths := range clusterSourceDirs(t) {
			all = append(all, paths...)
		}
		sort.Strings(all)
		for _, path := range all {
			name := filepath.Base(path)
			if other, dup := ps.paths[name]; dup {
				clusterSourceErr = fmt.Errorf("pkg/cluster/%s and pkg/cluster/%s share the base name %s; the RBAC scans key call sites and functions by base name (rbacCallsNotUsingTheSAToken, funcPos), so rename one", filepath.ToSlash(other), filepath.ToSlash(path), name)
				return
			}
			f, err := parser.ParseFile(ps.fset, path, nil, 0)
			if err != nil {
				clusterSourceErr = err
				return
			}
			ps.files[name] = f
			ps.paths[name] = path
		}
		clusterSourceVal = ps
	})
	if clusterSourceErr != nil {
		t.Fatal(clusterSourceErr)
	}
	if clusterSourceVal == nil {
		t.Fatal("the pkg/cluster source could not be read")
	}
	return clusterSourceVal
}

var (
	clusterSourceOnce sync.Once
	clusterSourceVal  *parsedSource
	clusterSourceErr  error
)

// clientMethodCalls maps each selector in the parsed tree (clusterSource)
// that calls a method of Client to the method's name. The selectors are
// told apart by type, not by name: a cache's Get, a header's Get or an
// http.Client's Do is no API request. Each package under pkg/cluster is
// type-checked from the same syntax trees once per test binary, its
// imports from their export data (exportData). It fails when a name in
// requestMethods or helperMethods is not a Client method, so a renamed
// request method cannot drop out of the scans.
func clientMethodCalls(t *testing.T) map[*ast.SelectorExpr]string {
	t.Helper()
	clientCallsOnce.Do(func() {
		ps := clusterSource(t)
		for name := range requestSourceFiles {
			if _, ok := ps.files[name]; !ok {
				clientCallsErr = fmt.Errorf("requestSourceFiles names %s, which no package under pkg/cluster has: update the list", name)
				return
			}
		}
		exports := exportData(t)
		byDir := map[string][]string{}
		for name := range ps.files {
			dir := filepath.Dir(ps.paths[name])
			byDir[dir] = append(byDir[dir], name)
		}
		dirs := make([]string, 0, len(byDir))
		for dir := range byDir {
			dirs = append(dirs, dir)
		}
		sort.Strings(dirs)
		imp := importer.ForCompiler(ps.fset, "gc", func(path string) (io.ReadCloser, error) {
			file, ok := exports[path]
			if !ok {
				return nil, fmt.Errorf("no export data for %s", path)
			}
			return os.Open(file)
		})
		calls := map[*ast.SelectorExpr]string{}
		var client *types.Named
		for _, dir := range dirs {
			names := byDir[dir]
			sort.Strings(names)
			files := make([]*ast.File, 0, len(names))
			for _, name := range names {
				files = append(files, ps.files[name])
			}
			info := &types.Info{Selections: map[*ast.SelectorExpr]*types.Selection{}}
			pkg, err := (&types.Config{Importer: imp}).Check(clusterImportPath(dir), ps.fset, files, info)
			if err != nil {
				clientCallsErr = fmt.Errorf("type-check %s: %w", clusterImportPath(dir), err)
				return
			}
			if tn, ok := pkg.Scope().Lookup("Client").(*types.TypeName); ok {
				client, _ = tn.Type().(*types.Named)
			}
			for sel, s := range info.Selections {
				if s.Kind() != types.MethodVal {
					continue
				}
				if isClientType(s.Obj().(*types.Func).Signature().Recv().Type()) {
					calls[sel] = s.Obj().Name()
				}
			}
		}
		if client == nil {
			clientCallsErr = errors.New("no package under pkg/cluster declares Client")
			return
		}
		for _, m := range []map[string]string{requestMethods, helperMethods} {
			for name := range m {
				if fn, _, _ := types.LookupFieldOrMethod(client, true, client.Obj().Pkg(), name); fn == nil {
					clientCallsErr = fmt.Errorf("the RBAC scans know a Client request method %s that Client does not have: update requestMethods/helperMethods", name)
					return
				}
			}
		}
		clientCallsVal = calls
	})
	if clientCallsErr != nil {
		t.Fatal(clientCallsErr)
	}
	if clientCallsVal == nil {
		t.Fatal("the Client calls could not be found")
	}
	return clientCallsVal
}

// isClientType reports whether t is Client or *Client of a package under
// pkg/cluster.
func isClientType(t types.Type) bool {
	if ptr, ok := t.(*types.Pointer); ok {
		t = ptr.Elem()
	}
	named, ok := t.(*types.Named)
	if !ok || named.Obj().Name() != "Client" || named.Obj().Pkg() == nil {
		return false
	}
	path := named.Obj().Pkg().Path()
	return path == clusterPkg || strings.HasPrefix(path, clusterPkg+"/")
}

var (
	clientCallsOnce sync.Once
	clientCallsVal  map[*ast.SelectorExpr]string
	clientCallsErr  error
)

// loadClusterSource indexes the source of pkg/cluster and of every package
// under it for one scan.
func loadClusterSource(t *testing.T) *sourcePkg {
	t.Helper()
	ps := clusterSource(t)
	p := &sourcePkg{fset: ps.fset, files: map[string]*ast.File{}, pkgOf: map[string]string{}, imports: map[string]map[string]string{},
		consts: map[symID]string{}, lits: map[symID]*ast.CompositeLit{}, funcs: map[symID]*ast.FuncDecl{}, fileOf: map[*ast.FuncDecl]string{},
		identMemo: map[identKey][]string{}, resultMemo: map[resultKey][]string{}, locals: map[*ast.FuncDecl]map[string]bool{},
		clientCalls: clientMethodCalls(t)}
	pkgNames := map[string]string{}
	names := make([]string, 0, len(ps.files))
	for name, f := range ps.files {
		names = append(names, name)
		pkgNames[clusterImportPath(filepath.Dir(ps.paths[name]))] = f.Name.Name
	}
	sort.Strings(names)
	for _, name := range names {
		f := ps.files[name]
		pkg := clusterImportPath(filepath.Dir(ps.paths[name]))
		p.files[name] = f
		p.pkgOf[name] = pkg
		imports := map[string]string{}
		for _, spec := range f.Imports {
			imp, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			local, ok := pkgNames[imp]
			if !ok {
				continue // not a package under pkg/cluster
			}
			if spec.Name != nil {
				local = spec.Name.Name
			}
			imports[local] = imp
		}
		p.imports[name] = imports
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for i, n := range vs.Names {
						if i < len(vs.Values) {
							if cl, ok := vs.Values[i].(*ast.CompositeLit); ok {
								p.lits[symID{pkg, n.Name}] = cl
							}
							if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
								if s, err := strconv.Unquote(lit.Value); err == nil {
									p.consts[symID{pkg, n.Name}] = s
								}
							}
						}
					}
				}
			case *ast.FuncDecl:
				if d.Recv == nil {
					p.funcs[symID{pkg, d.Name.Name}] = d
				}
				p.fileOf[d] = name
			}
		}
	}
	return p
}

// resolver turns a path expression into the set of strings it can take.
type resolver struct {
	p     *sourcePkg
	fn    *ast.FuncDecl
	depth int
}

func cartesian(parts [][]string) []string {
	out := []string{""}
	for _, options := range parts {
		var next []string
		for _, prefix := range out {
			for _, o := range options {
				next = append(next, prefix+o)
			}
		}
		if len(next) > 256 {
			next = next[:256]
		}
		out = next
	}
	return out
}

func (r resolver) resolve(e ast.Expr) []string {
	switch x := e.(type) {
	case *ast.ParenExpr:
		return r.resolve(x.X)
	case *ast.BasicLit:
		if x.Kind == token.STRING {
			if s, err := strconv.Unquote(x.Value); err == nil {
				return []string{s}
			}
		}
		return []string{unknownPart}
	case *ast.BinaryExpr:
		if x.Op == token.ADD {
			return cartesian([][]string{r.resolve(x.X), r.resolve(x.Y)})
		}
	case *ast.Ident:
		return r.resolveIdent(x)
	case *ast.SelectorExpr:
		return r.resolveSelector(x)
	case *ast.CallExpr:
		return r.resolveCall(x)
	}
	return []string{unknownPart}
}

func (r resolver) resolveCall(x *ast.CallExpr) []string {
	if fun, ok := x.Fun.(*ast.SelectorExpr); ok {
		if fun.Sel.Name == "listPath" && len(x.Args) == 0 {
			// componentKind.listPath: a discovered module kind.
			return moduleKindPaths()
		}
		if pkg, ok := fun.X.(*ast.Ident); ok && pkg.Name == "fmt" && fun.Sel.Name == "Sprintf" && len(x.Args) > 0 {
			formats := r.resolve(x.Args[0])
			var args [][]string
			for _, a := range x.Args[1:] {
				args = append(args, r.resolve(a))
			}
			var out []string
			verb := regexp.MustCompile(`%[sdvq]`)
			for _, format := range formats {
				for _, combo := range combos(args) {
					i := 0
					out = append(out, verb.ReplaceAllStringFunc(format, func(string) string {
						if i < len(combo) {
							i++
							return combo[i-1]
						}
						return unknownPart
					}))
				}
			}
			return out
		}
	}
	// A package function, by plain name or qualified by its package.
	callee, ok := r.p.callee(r.fn, x)
	if !ok {
		return []string{unknownPart}
	}
	switch callee.name {
	case "namespacedPath", "clusterPath":
		var args [][]string
		for _, a := range x.Args {
			args = append(args, r.resolve(a))
		}
		var out []string
		for _, combo := range combos(args) {
			if callee.name == "namespacedPath" {
				out = append(out, namespacedPath(combo[0], combo[1], combo[2], combo[3]))
			} else {
				out = append(out, clusterPath(combo[0], combo[1], combo[2]))
			}
		}
		return out
	case "getActivityNamespace":
		return []string{appNSMarker}
	case "maasAPI":
		// maas.opendatahub.io at its served version
		return []string{maasGroup + "/" + unknownPart}
	case "gitOpsAdmissionPolicyName":
		return []string{gitOpsPolicyNameMarker}
	case "componentListPath":
		// A discovered kind of one of the module API groups.
		return moduleKindPaths()
	}
	if _, ok := r.p.funcs[callee]; ok {
		return r.resolveCallResult(x, 0)
	}
	return []string{unknownPart}
}

// moduleKindPaths are the collections of a discovered kind in each module
// API group (component_cleanup.go), at a discovered version.
func moduleKindPaths() []string {
	var out []string
	for _, g := range moduleAPIGroups {
		out = append(out, "/apis/"+g+"/"+unknownPart+"/"+unknownPart)
	}
	return out
}

func combos(args [][]string) [][]string {
	out := [][]string{{}}
	for _, options := range args {
		var next [][]string
		for _, prefix := range out {
			for _, o := range options {
				c := append(append([]string{}, prefix...), o)
				next = append(next, c)
			}
		}
		out = next
	}
	return out
}

func (r resolver) resolveIdent(id *ast.Ident) []string {
	if v, ok := r.p.consts[symID{r.p.pkgOfFunc(r.fn), id.Name}]; ok && !r.isLocal(id.Name) {
		return []string{v}
	}
	if r.depth > rbacResolveDepth || r.fn == nil {
		return []string{unknownPart}
	}
	key := identKey{r.fn, id.Name, r.depth}
	if v, ok := r.p.identMemo[key]; ok {
		return v
	}
	v := frozen(r.resolveIdentUncached(id))
	r.p.identMemo[key] = v
	return v
}

func (r resolver) resolveIdentUncached(id *ast.Ident) []string {
	var out []string
	ast.Inspect(r.fn.Body, func(n ast.Node) bool {
		switch s := n.(type) {
		case *ast.AssignStmt:
			if call, ok := s.Rhs[0].(*ast.CallExpr); ok && len(s.Rhs) == 1 && len(s.Lhs) > 1 {
				for i, lhs := range s.Lhs {
					if l, ok := lhs.(*ast.Ident); ok && l.Name == id.Name {
						out = append(out, resolver{r.p, r.fn, r.depth + 1}.resolveCallResult(call, i)...)
					}
				}
			}
			if len(s.Lhs) == len(s.Rhs) {
				for i, lhs := range s.Lhs {
					if l, ok := lhs.(*ast.Ident); ok && l.Name == id.Name {
						out = append(out, resolver{r.p, r.fn, r.depth + 1}.resolve(s.Rhs[i])...)
					}
				}
			}
		case *ast.ValueSpec:
			for i, n := range s.Names {
				if n.Name == id.Name && i < len(s.Values) {
					out = append(out, resolver{r.p, r.fn, r.depth + 1}.resolve(s.Values[i])...)
				}
			}
		case *ast.RangeStmt:
			if v, ok := s.Value.(*ast.Ident); ok && v.Name == id.Name {
				out = append(out, r.rangeElements(s.X, "")...)
			}
		}
		return true
	})
	if len(out) > 0 {
		return out
	}
	// A function parameter: resolve the argument at every call site.
	if idx := paramIndex(r.fn, id.Name); idx >= 0 && r.fn.Recv == nil {
		for _, cs := range r.p.callSitesOf(symID{r.p.pkgOfFunc(r.fn), r.fn.Name.Name}) {
			if idx < len(cs.call.Args) {
				out = append(out, resolver{r.p, cs.caller, r.depth + 1}.resolve(cs.call.Args[idx])...)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return []string{unknownPart}
}

// resolveCallResult resolves result i of a call to a package function from
// that function's return statements (ignoring empty-string error returns).
func (r resolver) resolveCallResult(call *ast.CallExpr, i int) []string {
	id, ok := r.p.callee(r.fn, call)
	if !ok || r.depth > rbacResolveDepth {
		return []string{unknownPart}
	}
	fn, ok := r.p.funcs[id]
	if !ok || fn.Body == nil {
		return []string{unknownPart}
	}
	key := resultKey{id, i, r.depth}
	if v, ok := r.p.resultMemo[key]; ok {
		return v
	}
	v := frozen(r.resolveResultOf(fn, i))
	r.p.resultMemo[key] = v
	return v
}

// resolveResultOf resolves result i of fn from its return statements.
func (r resolver) resolveResultOf(fn *ast.FuncDecl, i int) []string {
	var out []string
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if _, isLit := n.(*ast.FuncLit); isLit {
			return false
		}
		if ret, ok := n.(*ast.ReturnStmt); ok && i < len(ret.Results) {
			for _, v := range (resolver{r.p, fn, r.depth + 1}).resolve(ret.Results[i]) {
				if v != "" {
					out = append(out, v)
				}
			}
		}
		return true
	})
	if len(out) == 0 {
		return []string{unknownPart}
	}
	return out
}

func (r resolver) isLocal(name string) bool {
	if r.fn == nil {
		return false
	}
	local := paramIndex(r.fn, name) >= 0
	ast.Inspect(r.fn.Body, func(n ast.Node) bool {
		if s, ok := n.(*ast.AssignStmt); ok && s.Tok == token.DEFINE {
			for _, lhs := range s.Lhs {
				if l, ok := lhs.(*ast.Ident); ok && l.Name == name {
					local = true
				}
			}
		}
		return true
	})
	return local
}

func paramIndex(fn *ast.FuncDecl, name string) int {
	i := 0
	for _, field := range fn.Type.Params.List {
		for _, n := range field.Names {
			if n.Name == name {
				return i
			}
			i++
		}
		if len(field.Names) == 0 {
			i++
		}
	}
	return -1
}

// rangeElements returns the values of a slice literal (or of a field of its
// struct elements) that a range statement iterates over.
func (r resolver) rangeElements(x ast.Expr, field string) []string {
	lit, ok := x.(*ast.CompositeLit)
	if !ok {
		switch v := x.(type) {
		case *ast.Ident:
			lit = r.findCompositeLit(v.Name)
		case *ast.SelectorExpr:
			if id, ok := r.p.qualified(r.fn, v); ok {
				lit = r.p.lits[id]
			}
		}
		if lit == nil {
			return []string{unknownPart}
		}
	}
	fieldIdx := -1
	if field != "" {
		arr, ok := lit.Type.(*ast.ArrayType)
		if !ok {
			return []string{unknownPart}
		}
		st, ok := arr.Elt.(*ast.StructType)
		if !ok {
			return []string{unknownPart}
		}
		i := 0
		for _, f := range st.Fields.List {
			for _, n := range f.Names {
				if n.Name == field {
					fieldIdx = i
				}
				i++
			}
		}
	}
	var out []string
	for _, elt := range lit.Elts {
		if field == "" {
			out = append(out, r.resolve(elt)...)
			continue
		}
		el, ok := elt.(*ast.CompositeLit)
		if !ok {
			out = append(out, unknownPart)
			continue
		}
		for i, v := range el.Elts {
			if kv, ok := v.(*ast.KeyValueExpr); ok {
				if k, ok := kv.Key.(*ast.Ident); ok && k.Name == field {
					out = append(out, r.resolve(kv.Value)...)
				}
			} else if i == fieldIdx {
				out = append(out, r.resolve(v)...)
			}
		}
	}
	if len(out) == 0 {
		return []string{unknownPart}
	}
	return out
}

func (r resolver) findCompositeLit(name string) *ast.CompositeLit {
	var found *ast.CompositeLit
	ast.Inspect(r.fn.Body, func(n ast.Node) bool {
		if s, ok := n.(*ast.AssignStmt); ok && len(s.Lhs) == len(s.Rhs) {
			for i, lhs := range s.Lhs {
				if l, ok := lhs.(*ast.Ident); ok && l.Name == name {
					if cl, ok := s.Rhs[i].(*ast.CompositeLit); ok {
						found = cl
					}
				}
			}
		}
		return true
	})
	if found == nil && !r.isLocal(name) {
		found = r.p.lits[symID{r.p.pkgOfFunc(r.fn), name}]
	}
	return found
}

func (r resolver) resolveSelector(x *ast.SelectorExpr) []string {
	base, ok := x.X.(*ast.Ident)
	if !ok || r.fn == nil {
		return []string{unknownPart}
	}
	var out []string
	ast.Inspect(r.fn.Body, func(n ast.Node) bool {
		if s, ok := n.(*ast.RangeStmt); ok {
			if v, ok := s.Value.(*ast.Ident); ok && v.Name == base.Name {
				out = append(out, r.rangeElements(s.X, x.Sel.Name)...)
			}
		}
		return true
	})
	if len(out) == 0 {
		// pkg.Name: a constant of an imported package under pkg/cluster.
		if id, ok := r.p.qualified(r.fn, x); ok {
			if v, ok := r.p.consts[id]; ok {
				return []string{v}
			}
			return []string{unknownPart}
		}
	}
	if len(out) == 0 {
		out = r.resolveStepField(base.Name, x.Sel.Name)
	}
	if len(out) == 0 {
		return []string{unknownPart}
	}
	return out
}

// resolveStepField resolves base.field where base is a parameter of type *T,
// the state a run's steps share (maasPublish, maasUnpublish, ...): one step
// sets the field (base.field = v) and a later one reads it. It resolves v
// in every scanned function that takes a *T and assigns the field. T is
// keyed by package, so the steps may live in other packages under
// pkg/cluster.
func (r resolver) resolveStepField(base, field string) []string {
	typ, ok := r.p.pointerParamType(r.fn, base)
	if !ok || r.depth > rbacResolveDepth {
		return nil
	}
	var out []string
	for fn := range r.p.fileOf {
		if fn.Body == nil || !r.p.scans(fn) {
			continue
		}
		params := r.p.pointerParamsOf(fn, typ)
		if len(params) == 0 {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			s, ok := n.(*ast.AssignStmt)
			if !ok || len(s.Lhs) != len(s.Rhs) {
				return true
			}
			for i, lhs := range s.Lhs {
				sel, ok := lhs.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != field {
					continue
				}
				if id, ok := sel.X.(*ast.Ident); ok && params[id.Name] {
					out = append(out, resolver{r.p, fn, r.depth + 1}.resolve(s.Rhs[i])...)
				}
			}
			return true
		})
	}
	return out
}

// pointerTarget is T, keyed by package, when typ is *T or *pkg.T (pkg an
// imported package under pkg/cluster) in fn.
func (p *sourcePkg) pointerTarget(fn *ast.FuncDecl, typ ast.Expr) (symID, bool) {
	star, ok := typ.(*ast.StarExpr)
	if !ok {
		return symID{}, false
	}
	switch x := star.X.(type) {
	case *ast.Ident:
		return symID{p.pkgOfFunc(fn), x.Name}, true
	case *ast.SelectorExpr:
		if base, ok := x.X.(*ast.Ident); ok {
			if imp, ok := p.imports[p.fileOf[fn]][base.Name]; ok {
				return symID{imp, x.Sel.Name}, true
			}
		}
	}
	return symID{}, false
}

// pointerParamType is T when fn's parameter name has type *T.
func (p *sourcePkg) pointerParamType(fn *ast.FuncDecl, name string) (symID, bool) {
	if fn == nil {
		return symID{}, false
	}
	for _, f := range fn.Type.Params.List {
		for _, n := range f.Names {
			if n.Name != name {
				continue
			}
			return p.pointerTarget(fn, f.Type)
		}
	}
	return symID{}, false
}

// pointerParamsOf names fn's parameters of type *typ.
func (p *sourcePkg) pointerParamsOf(fn *ast.FuncDecl, typ symID) map[string]bool {
	var out map[string]bool
	for _, f := range fn.Type.Params.List {
		if id, ok := p.pointerTarget(fn, f.Type); ok && id == typ {
			for _, n := range f.Names {
				if out == nil {
					out = map[string]bool{}
				}
				out[n.Name] = true
			}
		}
	}
	return out
}

// toRequests maps one resolved request path to RBAC request attributes.
func toRequests(method, path, site string) ([]rbacRequest, error) {
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	if path == "/version" {
		return nil, nil // non-resource URL readable by every authenticated user
	}
	segs := strings.Split(strings.TrimPrefix(path, "/"), "/")
	// API discovery documents (/apis/<group> and /apis/<group>/<version>)
	// are non-resource URLs that the system:discovery ClusterRole grants to
	// every authenticated user.
	if method == "get" && segs[0] == "apis" && (len(segs) == 2 || len(segs) == 3) && !strings.Contains(segs[1], unknownPart) {
		return nil, nil
	}
	var group string
	switch {
	case len(segs) >= 2 && segs[0] == "api" && segs[1] == "v1":
		segs = segs[2:]
	case len(segs) >= 3 && segs[0] == "apis" && strings.Contains(segs[1], unknownPart):
		group = segs[1] // "group/version" only known at run time
		segs = segs[2:]
	case len(segs) >= 3 && segs[0] == "apis":
		group = segs[1]
		segs = segs[3:]
	default:
		return nil, fmt.Errorf("unrecognised API path %q", path)
	}
	var ns, resource, name string
	switch {
	case len(segs) >= 3 && segs[0] == "namespaces":
		ns, resource = segs[1], segs[2]
		if len(segs) >= 4 {
			name = segs[3]
		}
		if len(segs) >= 5 {
			resource += "/" + segs[4]
		}
	case len(segs) >= 1:
		resource = segs[0]
		if len(segs) >= 2 {
			name = segs[1]
		}
		if len(segs) >= 3 {
			resource += "/" + segs[2]
		}
	default:
		return nil, fmt.Errorf("no resource in %q", path)
	}
	if strings.Contains(resource, unknownPart) && discoveredResourceGroups[group] {
		resource = "*"
	}
	if resource == "" || strings.Contains(resource, unknownPart) {
		return nil, fmt.Errorf("resource of %q is only known at run time", path)
	}
	if strings.Contains(group, unknownPart) {
		g, ok := knownResourceGroups[resource]
		if !ok {
			return nil, fmt.Errorf("API group of %q is only known at run time", path)
		}
		group = g
	}
	if strings.Contains(ns, unknownPart) {
		ns = anyNS
	}
	if strings.Contains(name, unknownPart) {
		name = anyNS
	}
	var verbs []string
	if method == "get" {
		if name == "" {
			verbs = []string{"list"}
		} else {
			verbs = []string{"get"}
		}
	} else {
		verbs = methodVerbs[method]
	}
	var out []rbacRequest
	for _, v := range verbs {
		n := name
		if method == "post" {
			n = "" // a POST to the collection carries no name, so resourceNames cannot match it
		}
		out = append(out, rbacRequest{Verb: v, Group: group, Resource: resource, Namespace: ns, Name: n, Site: site})
	}
	return out, nil
}

// manifestNames collects, per resource, the metadata.name of every object
// manifest (a map literal with "kind" and "metadata") built in a function.
func manifestNames(r resolver) map[string][]string {
	out := map[string][]string{}
	strKey := func(kv *ast.KeyValueExpr) string {
		if lit, ok := kv.Key.(*ast.BasicLit); ok {
			s, _ := strconv.Unquote(lit.Value)
			return s
		}
		return ""
	}
	ast.Inspect(r.fn.Body, func(n ast.Node) bool {
		cl, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		var kinds, names []string
		for _, e := range cl.Elts {
			kv, ok := e.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			switch strKey(kv) {
			case "kind":
				kinds = r.resolve(kv.Value)
			case "metadata":
				if meta, ok := kv.Value.(*ast.CompositeLit); ok {
					for _, me := range meta.Elts {
						if mkv, ok := me.(*ast.KeyValueExpr); ok && strKey(mkv) == "name" {
							names = r.resolve(mkv.Value)
						}
					}
				}
			}
		}
		for _, k := range kinds {
			for _, name := range names {
				if !strings.Contains(k, unknownPart) && !strings.Contains(name, unknownPart) {
					res := strings.ToLower(k) + "s"
					out[res] = append(out[res], name)
				}
			}
		}
		return true
	})
	return out
}

// fullScan is the scan of every function, done once per test binary: the
// source does not change while the tests run. Each caller gets copies.
var fullScan struct {
	done     bool
	reqs     []rbacRequest
	excluded map[string]bool
}

// serviceAccountRequests scans the package for Client calls.
func serviceAccountRequests(t *testing.T) ([]rbacRequest, map[string]bool) {
	t.Helper()
	if !fullScan.done {
		fullScan.reqs, fullScan.excluded = serviceAccountRequestsIn(t, nil)
		fullScan.done = true
	}
	excluded := make(map[string]bool, len(fullScan.excluded))
	maps.Copy(excluded, fullScan.excluded)
	return append([]rbacRequest(nil), fullScan.reqs...), excluded
}

// serviceAccountRequestsIn scans the functions in only (funcPos keys; nil
// for every function) for Client calls. A parameter that makes up a path is
// resolved from the call sites in those functions only.
func serviceAccountRequestsIn(t *testing.T, only map[string]bool) ([]rbacRequest, map[string]bool) {
	t.Helper()
	return scanRequests(t, only, false)
}

// bookkeepingRequests are the calls on c.Bookkeeping() anywhere in the
// package: the updater's own records, always written with the
// ServiceAccount's token, whatever path reaches them.
func bookkeepingRequests(t *testing.T) []rbacRequest {
	t.Helper()
	reqs, _ := scanRequests(t, nil, true)
	return reqs
}

// scanRequests scans the functions in only (nil: all) for Client calls,
// or only the calls on c.Bookkeeping() with onlyBookkeeping.
func scanRequests(t *testing.T, only map[string]bool, onlyBookkeeping bool) ([]rbacRequest, map[string]bool) {
	t.Helper()
	p := loadClusterSource(t)
	p.only = only
	var reqs []rbacRequest
	seenExcluded := map[string]bool{}
	var problems []string
	names := make([]string, 0, len(p.files))
	for name := range p.files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, file := range names {
		for _, decl := range p.files[file].Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || isRequestSourceFile(file) || !p.scans(fn) {
				continue
			}
			fnPos := p.funcPos(fn)
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || len(call.Args) == 0 {
					return true
				}
				method, ok := p.requestMethod(sel)
				if !ok {
					return true
				}
				pathArg := call.Args[0]
				if method == "do" {
					// c.Do(http.MethodX, path, ...): the raw request method.
					m, ok := rawMethods[selectorName(call.Args[0])]
					if !ok || len(call.Args) < 2 {
						problems = append(problems, fmt.Sprintf("%s:%d do: request method is not an http.Method constant", file, p.fset.Position(call.Pos()).Line))
						return true
					}
					method, pathArg = m, call.Args[1]
				}
				if _, known := methodVerbs[method]; !known && method != "get" {
					return true
				}
				if inner, ok := sel.X.(*ast.CallExpr); onlyBookkeeping && (!ok || selectorName(inner.Fun) != "Bookkeeping") {
					return true
				}
				site := fmt.Sprintf("%s:%s", file, fn.Name.Name)
				if _, excluded := rbacCallsNotUsingTheSAToken[site]; excluded {
					seenExcluded[site] = true
					return true
				}
				if _, bestEffort := rbacBestEffortCalls[site]; bestEffort {
					seenExcluded[site] = true
					return true
				}
				if _, userRead := rbacUserTokenReads[site]; userRead {
					seenExcluded[site] = true
					return true
				}
				if _, gitOpsOnly := rbacGitOpsOnlyCalls[site]; gitOpsOnly && only == nil {
					seenExcluded[site] = true
					return true
				}
				pos := p.fset.Position(call.Pos())
				manifests := manifestNames(resolver{p: p, fn: fn})
				for _, path := range (resolver{p: p, fn: fn}).resolve(pathArg) {
					rs, err := toRequests(method, path, fmt.Sprintf("%s:%d", file, pos.Line))
					if err != nil {
						problems = append(problems, fmt.Sprintf("%s:%d %s: %v", file, pos.Line, method, err))
						continue
					}
					for _, r := range rs {
						r.Func = fnPos
						// A name taken from an object manifest built in the
						// same function is one of that manifest's names.
						if names := manifests[r.Resource]; r.Name == anyNS && len(names) > 0 {
							for _, n := range names {
								named := r
								named.Name = n
								reqs = append(reqs, named)
							}
							continue
						}
						reqs = append(reqs, r)
					}
				}
				return true
			})
		}
	}
	if len(problems) > 0 {
		t.Fatalf("cannot derive the RBAC needed by these API calls; resolve the path statically or extend the test:\n%s", strings.Join(problems, "\n"))
	}
	return reqs, seenExcluded
}

// --- template side ---

type rbacRule struct {
	APIGroups     []string `yaml:"apiGroups"`
	Resources     []string `yaml:"resources"`
	Verbs         []string `yaml:"verbs"`
	ResourceNames []string `yaml:"resourceNames"`
}

type templateObject struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
	Rules   []rbacRule `yaml:"rules"`
	RoleRef struct {
		Kind string `yaml:"kind"`
		Name string `yaml:"name"`
	} `yaml:"roleRef"`
	Subjects []struct {
		Kind      string `yaml:"kind"`
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"subjects"`
}

// builtInGrants are permissions every authenticated identity has through
// OpenShift's default bindings. The basic-user ClusterRole (bound to
// system:authenticated by the basic-users ClusterRoleBinding) allows "get"
// on users named "~" (checked on the live cluster with
// `oc auth can-i get users/~ --as=system:serviceaccount:...`).
//
// Kubernetes's system:basic-user ClusterRole, bound to system:authenticated,
// allows create on selfsubjectaccessreviews
// (plugin/pkg/auth/authorizer/rbac/bootstrappolicy/policy.go, basicUserRules
// and the system:basic-user ClusterRoleBinding): the gitops profile's
// Secret-access check (checkServiceAccountSecretAccess) uses it.
var builtInGrants = []grant{{
	rule:   rbacRule{APIGroups: []string{"", "user.openshift.io"}, Resources: []string{"users"}, ResourceNames: []string{"~"}, Verbs: []string{"get"}},
	source: "built-in basic-user",
}, {
	rule:   rbacRule{APIGroups: []string{"authorization.k8s.io"}, Resources: []string{"selfsubjectaccessreviews"}, Verbs: []string{"create"}},
	source: "built-in basic-user",
}}

// grant is a rule together with where it applies ("" = cluster-wide).
type grant struct {
	rule      rbacRule
	namespace string
	source    string
}

func templateGrants(t *testing.T) []grant {
	t.Helper()
	return templateGrantsFrom(t, templateRel)
}

// templateGrantsFrom reads the rules an OpenShift Template binds to the
// updater's ServiceAccount (deploy/template.yaml, deploy/rbac-gitops.yaml).
func templateGrantsFrom(t *testing.T, path string) []grant {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var tmpl struct {
		Parameters []templateParameter `yaml:"parameters"`
		Objects    []templateObject    `yaml:"objects"`
	}
	if err := yaml.Unmarshal(data, &tmpl); err != nil {
		t.Fatal(err)
	}
	// Other namespace parameters (the pull-secret and marketplace
	// namespaces) resolve to their default values.
	norm := func(s string) string {
		s = strings.ReplaceAll(s, "${NAMESPACE}", appNSMarker)
		for _, p := range tmpl.Parameters {
			s = strings.ReplaceAll(s, "${"+p.Name+"}", p.Value)
		}
		return s
	}
	roles := map[string]templateObject{}
	for _, o := range tmpl.Objects {
		if o.Kind == "ClusterRole" || o.Kind == "Role" {
			roles[o.Kind+"/"+norm(o.Metadata.Namespace)+"/"+o.Metadata.Name] = o
		}
	}
	var grants []grant
	for _, b := range tmpl.Objects {
		if b.Kind != "ClusterRoleBinding" && b.Kind != "RoleBinding" {
			continue
		}
		boundToSA := false
		for _, s := range b.Subjects {
			if s.Kind == "ServiceAccount" && s.Name == "${APP_NAME}" && s.Namespace == "${NAMESPACE}" {
				boundToSA = true
			}
		}
		if !boundToSA {
			continue
		}
		bindingNS := norm(b.Metadata.Namespace)
		roleKey := b.RoleRef.Kind + "/" + bindingNS + "/" + b.RoleRef.Name
		if b.RoleRef.Kind == "ClusterRole" {
			roleKey = "ClusterRole//" + b.RoleRef.Name
		}
		role, ok := roles[roleKey]
		if !ok {
			t.Fatalf("%s %s refers to %s, which the template does not define", b.Kind, b.Metadata.Name, roleKey)
		}
		scope := ""
		if b.Kind == "RoleBinding" {
			scope = bindingNS
		}
		for _, rule := range role.Rules {
			names := make([]string, len(rule.ResourceNames))
			for i, n := range rule.ResourceNames {
				names[i] = norm(n)
			}
			rule.ResourceNames = names
			grants = append(grants, grant{rule: rule, namespace: scope, source: fmt.Sprintf("%s %s via %s %s", role.Kind, role.Metadata.Name, b.Kind, b.Metadata.Name)})
		}
	}
	if len(grants) == 0 {
		t.Fatal("no RBAC rules bound to the ServiceAccount found in the template")
	}
	return append(grants, builtInGrants...)
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v || x == "*" {
			return true
		}
	}
	return false
}

// allows mirrors the RBAC authorizer's rule matching
// (k8s.io/kubernetes/pkg/apis/rbac/v1/evaluation_helpers.go).
func (g grant) allows(r rbacRequest) bool {
	if g.namespace != "" && g.namespace != r.Namespace {
		return false
	}
	if !contains(g.rule.Verbs, r.Verb) || !contains(g.rule.APIGroups, r.Group) || !contains(g.rule.Resources, r.Resource) {
		return false
	}
	if len(g.rule.ResourceNames) > 0 {
		// Conservatively, as documented: "You cannot restrict create or
		// deletecollection requests by their resource name."
		if (r.Verb == "create" && !r.NamedReview) || r.Name == "" || r.Name == anyNS {
			return false
		}
		found := false
		for _, n := range g.rule.ResourceNames {
			if n == r.Name {
				found = true
			}
		}
		return found
	}
	return true
}

func TestTemplateRBACCoversEveryServiceAccountCall(t *testing.T) {
	_, seenExcluded := serviceAccountRequests(t)
	reqs := templateSARequests(t)
	for site := range rbacCallsNotUsingTheSAToken {
		if !seenExcluded[site] {
			t.Errorf("rbacCallsNotUsingTheSAToken lists %s, but that function no longer makes API calls; remove the entry", site)
		}
	}
	for site := range rbacBestEffortCalls {
		if !seenExcluded[site] {
			t.Errorf("rbacBestEffortCalls lists %s, but that function no longer makes API calls; remove the entry", site)
		}
	}
	for site := range rbacUserTokenReads {
		if !seenExcluded[site] {
			t.Errorf("rbacUserTokenReads lists %s, but that function no longer makes API calls; remove the entry", site)
		}
	}
	for site := range rbacGitOpsOnlyCalls {
		if !seenExcluded[site] {
			t.Errorf("rbacGitOpsOnlyCalls lists %s, but that function no longer makes API calls; remove the entry", site)
		}
	}
	grants := templateGrants(t)
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
		t.Errorf("deploy/template.yaml does not grant these ServiceAccount requests:\n%s", strings.Join(lines, "\n"))
	}
}

func TestTemplateRBACGrantsNothingTheCodeDoesNotUse(t *testing.T) {
	reqs := templateSARequests(t)
	for _, g := range templateGrants(t) {
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
						t.Errorf("%s grants %s %s/%s (namespace %q) but no code path uses it; remove it to keep least privilege", g.source, verb, group, res, g.namespace)
					}
					// Each name of a named rule, too: a name no code path
					// requests widens the grant for nothing.
					for _, name := range g.rule.ResourceNames {
						named := false
						for _, r := range reqs {
							named = named || (r.Verb == verb && r.Group == group && r.Resource == res && r.Name == name && g.allows(r))
						}
						if used && !named {
							t.Errorf("%s grants %s %s/%s on %q (namespace %q) but no code path requests that name; remove it to keep least privilege", g.source, verb, group, res, name, g.namespace)
						}
					}
				}
			}
		}
	}
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// TestRBACRequestDerivation pins the path-to-attributes mapping.
func TestRBACRequestDerivation(t *testing.T) {
	cases := []struct {
		method, path string
		want         []string
	}{
		{"get", "/api/v1/namespaces/minio", []string{"get /namespaces ns= name=minio"}},
		{"post", "/api/v1/namespaces", []string{"create /namespaces ns= name="}},
		{"get", "/api/v1/namespaces/kube-system/secrets/additional-pull-secret", []string{"get /secrets ns=kube-system name=additional-pull-secret"}},
		{"get", "/api/v1/namespaces/x/pods?labelSelector=a%3Db", []string{"list /pods ns=x name="}},
		{"apply", "/apis/operators.coreos.com/v1alpha1/namespaces/redhat-ods-operator/subscriptions/rhods-operator", []string{
			"patch operators.coreos.com/subscriptions ns=redhat-ods-operator name=rhods-operator",
			"create operators.coreos.com/subscriptions ns=redhat-ods-operator name=rhods-operator",
		}},
		{"delete", "/apis/admissionregistration.k8s.io/v1/validatingwebhookconfigurations/" + unknownPart, []string{"delete admissionregistration.k8s.io/validatingwebhookconfigurations ns= name=*"}},
		{"get", "/apis/components.platform.opendatahub.io/v1alpha1/dashboards", []string{"list components.platform.opendatahub.io/dashboards ns= name="}},
		{"post", "/api/v1/namespaces/" + unknownPart + "/secrets", []string{"create /secrets ns=* name="}},
		{"get", "/version", nil},
		{"get", "/apis/components.platform.opendatahub.io", nil},
		{"get", "/apis/components.platform.opendatahub.io/" + unknownPart, nil},
		{"get", "/apis/components.platform.opendatahub.io/" + unknownPart + "/" + unknownPart, []string{"list components.platform.opendatahub.io/* ns= name="}},
		{"patch", "/apis/components.platform.opendatahub.io/" + unknownPart + "/" + unknownPart + "/" + unknownPart, []string{"patch components.platform.opendatahub.io/* ns= name=*"}},
	}
	for _, tc := range cases {
		got, err := toRequests(tc.method, tc.path, "t")
		if err != nil {
			t.Fatalf("%s %s: %v", tc.method, tc.path, err)
		}
		var keys []string
		for _, r := range got {
			keys = append(keys, r.key())
		}
		if strings.Join(keys, "|") != strings.Join(tc.want, "|") {
			t.Errorf("%s %s = %v, want %v", tc.method, tc.path, keys, tc.want)
		}
	}
	if _, err := toRequests("get", "/apis/x/v1/"+unknownPart+"/"+unknownPart, "t"); err == nil {
		t.Error("an unknown resource must fail the derivation")
	}
	if _, err := toRequests("delete", "/apis/x/v1", "t"); err == nil {
		t.Error("only GET of a discovery document is exempt")
	}
	named := grant{rule: rbacRule{APIGroups: []string{"components.platform.opendatahub.io"}, Resources: []string{"kserves"}, Verbs: []string{"list"}}}
	if named.allows(rbacRequest{Verb: "list", Group: "components.platform.opendatahub.io", Resource: "*"}) {
		t.Error("a discovered resource needs a wildcard rule, not a list of kinds")
	}
}

// TestRBACGrantMatching pins the scope rules used by the coverage check.
func TestRBACGrantMatching(t *testing.T) {
	nsRule := grant{rule: rbacRule{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"get"}, ResourceNames: []string{"a"}}, namespace: "kube-system"}
	if !nsRule.allows(rbacRequest{Verb: "get", Resource: "secrets", Namespace: "kube-system", Name: "a"}) {
		t.Error("named get in the Role's namespace must be allowed")
	}
	for _, r := range []rbacRequest{
		{Verb: "get", Resource: "secrets", Namespace: "other", Name: "a"},
		{Verb: "get", Resource: "secrets", Namespace: "kube-system", Name: "b"},
		{Verb: "get", Resource: "secrets", Namespace: "kube-system", Name: anyNS},
		{Verb: "list", Resource: "secrets", Namespace: "kube-system"},
		{Verb: "get", Resource: "secrets", Namespace: anyNS, Name: "a"},
	} {
		if nsRule.allows(r) {
			t.Errorf("%s must not be allowed by a namespaced, name-restricted rule", r.key())
		}
	}
	cluster := grant{rule: rbacRule{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"create"}}}
	if !cluster.allows(rbacRequest{Verb: "create", Resource: "secrets", Namespace: anyNS}) {
		t.Error("a cluster-wide rule covers any namespace")
	}
}

type templateParameter struct {
	Name  string `yaml:"name"`
	Value string `yaml:"value"`
}

// `oc process` drops a hard-coded metadata.namespace from template objects
// and keeps only parameterised ones, so a literal namespace would create the
// object in whatever project is current (seen live: the kube-system and
// openshift-marketplace Roles landed in "default").
func TestTemplateNamespacesAreParameters(t *testing.T) {
	data, err := os.ReadFile(templateRel)
	if err != nil {
		t.Fatal(err)
	}
	var tmpl struct {
		Objects []struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name      string `yaml:"name"`
				Namespace string `yaml:"namespace"`
			} `yaml:"metadata"`
		} `yaml:"objects"`
	}
	if err := yaml.Unmarshal(data, &tmpl); err != nil {
		t.Fatal(err)
	}
	for _, o := range tmpl.Objects {
		if ns := o.Metadata.Namespace; ns != "" && !strings.HasPrefix(ns, "${") {
			t.Errorf("%s %s has the literal namespace %q; use a template parameter, or oc process drops it", o.Kind, o.Metadata.Name, ns)
		}
	}
}
