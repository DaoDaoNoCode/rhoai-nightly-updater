package cluster

// RBAC coverage: every Kubernetes API call the ServiceAccount makes must be
// granted by deploy/template.yaml, and every grant must be used by the code.
//
// The test parses this package's source, finds each call to a Client request
// method (get, post, put, patch, strategicPatch, apply, dryRunApply, delete),
// resolves the request path statically, and maps it to the RBAC request
// attributes the API server checks (verb, API group, resource, namespace and
// name; https://kubernetes.io/docs/reference/access-authn-authz/authorization/#determine-the-request-verb).
// Error, rollback and restore branches are ordinary calls, so they are
// covered like the happy path. A call whose path cannot be resolved fails the
// test until it is resolved or listed in rbacCallsNotUsingTheSAToken.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	unknownPart = "\x00"  // a path piece only known at run time
	appNSMarker = "${NS}" // the updater's own namespace (template NAMESPACE)
	anyNS       = "*"     // a namespace chosen at run time
	templateRel = "../../deploy/template.yaml"
)

// Calls made with a token other than the ServiceAccount's. They are checked
// against the user's own RBAC by the API server, not against the template.
var rbacCallsNotUsingTheSAToken = map[string]string{
	"authz.go:CheckUserPermissionWithToken": "SubjectAccessReview sent with the user's token",
	"authz.go:LookupUserWithToken":          "users/~ read with the user's token",
}

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

// rawMethods maps the http.Method constants passed to Client.do to the
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
}

func (r rbacRequest) key() string {
	return fmt.Sprintf("%s %s/%s ns=%s name=%s", r.Verb, r.Group, r.Resource, r.Namespace, r.Name)
}

type sourcePkg struct {
	fset   *token.FileSet
	files  map[string]*ast.File
	consts map[string]string
	lits   map[string]*ast.CompositeLit
	funcs  map[string]*ast.FuncDecl
	fileOf map[*ast.FuncDecl]string
}

func loadClusterSource(t *testing.T) *sourcePkg {
	t.Helper()
	p := &sourcePkg{fset: token.NewFileSet(), files: map[string]*ast.File{}, consts: map[string]string{}, lits: map[string]*ast.CompositeLit{}, funcs: map[string]*ast.FuncDecl{}, fileOf: map[*ast.FuncDecl]string{}}
	matches, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range matches {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(p.fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		p.files[name] = f
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
								p.lits[n.Name] = cl
							}
							if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
								if s, err := strconv.Unquote(lit.Value); err == nil {
									p.consts[n.Name] = s
								}
							}
						}
					}
				}
			case *ast.FuncDecl:
				if d.Recv == nil {
					p.funcs[d.Name.Name] = d
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
	switch fun := x.Fun.(type) {
	case *ast.Ident:
		switch fun.Name {
		case "namespacedPath", "clusterPath":
			var args [][]string
			for _, a := range x.Args {
				args = append(args, r.resolve(a))
			}
			var out []string
			for _, combo := range combos(args) {
				if fun.Name == "namespacedPath" {
					out = append(out, namespacedPath(combo[0], combo[1], combo[2], combo[3]))
				} else {
					out = append(out, clusterPath(combo[0], combo[1], combo[2]))
				}
			}
			return out
		case "getActivityNamespace":
			return []string{appNSMarker}
		}
		if _, ok := r.p.funcs[fun.Name]; ok {
			return r.resolveCallResult(x, 0)
		}
	case *ast.SelectorExpr:
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
	return []string{unknownPart}
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
	if v, ok := r.p.consts[id.Name]; ok && !r.isLocal(id.Name) {
		return []string{v}
	}
	if r.depth > 4 || r.fn == nil {
		return []string{unknownPart}
	}
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
	if idx := paramIndex(r.fn, id.Name); idx >= 0 {
		for caller := range r.p.fileOf {
			if caller.Body == nil {
				continue
			}
			ast.Inspect(caller.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if f, ok := call.Fun.(*ast.Ident); ok && f.Name == r.fn.Name.Name && r.fn.Recv == nil && idx < len(call.Args) {
					out = append(out, resolver{r.p, caller, r.depth + 1}.resolve(call.Args[idx])...)
				}
				return true
			})
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
	id, ok := call.Fun.(*ast.Ident)
	if !ok || r.depth > 4 {
		return []string{unknownPart}
	}
	fn, ok := r.p.funcs[id.Name]
	if !ok || fn.Body == nil {
		return []string{unknownPart}
	}
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
		if id, isIdent := x.(*ast.Ident); isIdent {
			lit = r.findCompositeLit(id.Name)
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
		found = r.p.lits[name]
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
		return []string{unknownPart}
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
	ns, resource, name := "", "", ""
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

// serviceAccountRequests scans the package for Client calls.
func serviceAccountRequests(t *testing.T) ([]rbacRequest, map[string]bool) {
	t.Helper()
	p := loadClusterSource(t)
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
			if !ok || fn.Body == nil || file == "client.go" {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || len(call.Args) == 0 {
					return true
				}
				method := sel.Sel.Name
				pathArg := call.Args[0]
				if method == "do" {
					// c.do(http.MethodX, path, ...): the raw request method.
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
				site := fmt.Sprintf("%s:%s", file, fn.Name.Name)
				if _, excluded := rbacCallsNotUsingTheSAToken[site]; excluded {
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
var builtInGrants = []grant{{
	rule:   rbacRule{APIGroups: []string{"", "user.openshift.io"}, Resources: []string{"users"}, ResourceNames: []string{"~"}, Verbs: []string{"get"}},
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
	data, err := os.ReadFile(templateRel)
	if err != nil {
		t.Fatal(err)
	}
	var tmpl struct {
		Objects []templateObject `yaml:"objects"`
	}
	if err := yaml.Unmarshal(data, &tmpl); err != nil {
		t.Fatal(err)
	}
	norm := func(s string) string { return strings.ReplaceAll(s, "${NAMESPACE}", appNSMarker) }
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
		if r.Verb == "create" || r.Name == "" || r.Name == anyNS {
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
	reqs, seenExcluded := serviceAccountRequests(t)
	for site := range rbacCallsNotUsingTheSAToken {
		if !seenExcluded[site] {
			t.Errorf("rbacCallsNotUsingTheSAToken lists %s, but that function no longer makes API calls; remove the entry", site)
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
	reqs, _ := serviceAccountRequests(t)
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
