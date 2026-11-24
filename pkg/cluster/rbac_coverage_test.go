package cluster

// RBAC coverage: every Kubernetes API call the ServiceAccount makes must be
// granted by deploy/template.yaml, and every grant must be used by the code.
//
// The test parses the source of pkg/cluster and of every package under it,
// finds each call to a Client request method (Get, Post, Put, Patch,
// StrategicPatch, Apply, DryRunApply, Delete, Do, and the REST helpers on
// top of them; calls are told apart by type, so a cache's Get or an
// http.Client's Do is none, while a call through an interface or constraint
// *Client implements, or through a method expression, is one; a request
// method used as a function value fails the test), resolves the request
// path statically (through plain f(...) and qualified pkg.F(...) calls and
// constants of those packages, each name resolved in its lexical scope by
// go/types; rbac_scanner_cases_test.go pins these rules on fixtures), and
// maps it to the RBAC request
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
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/serving"
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
	"user_projects.go:ListUserDSProjects":           "the user's data science projects, through the projects API",
	"user_reads.go:listProjectLLMInferenceServices": "the LLMInferenceServices of one of the user's projects",
	"user_reads.go:listProjectInferenceServices":    "the InferenceServices of one of the user's projects",
	"user_reads.go:listConnectionWorkbenches":       "the workbenches of one of the user's projects that use the tool's dashboard Connection",
	"model_registry.go:userReadCR":                  "the Model registry tab's status: this tool's ModelRegistry",
	"model_registry.go:userReadService":             "the Model registry tab's status: the registry's Service (its gateway address)",
	// The Workbench status of an admin (GET /api/resources/workbench) and
	// the Workbench operations read with the admin's own token.
	"workbench_reads.go:readNotebook":         "a project's nightly-workbench Notebook",
	"workbench_reads.go:readStorage":          "a project's nightly-workbench-storage PersistentVolumeClaim",
	"workbench_reads.go:listPods":             "the pods of a project's nightly-workbench",
	"workbench_reads.go:projectLabels":        "a project's namespace, for its labels",
	"workbench_reads.go:listImageStreams":     "the workbench images (ImageStreams) the dashboard offers",
	"workbench_reads.go:listProfiles":         "the hardware profiles of the applications namespace and of a project",
	"workbench_reads.go:listLocalQueues":      "a project's Kueue LocalQueues",
	"workbench_reads.go:listStorageClasses":   "the cluster's StorageClasses",
	"workbench_reads.go:readDashboardConfig":  "the dashboard's OdhDashboardConfig",
	"workbench_reads.go:pipelineConfigExists": "whether a project has Secret ds-pipeline-config",
	"workbench_reads.go:listNotebooks":        "a project's Notebooks, for which use the workbench storage",
	"workbench_reads.go:listProjectPods":      "a project's pods, for which mount the workbench storage",
	"workbench_reads.go:toolConnection":       "a project's dashboard Connection rhoai-nightly-updater-s3",
	// A model's setup in a Kueue project (the dashboard's Kueue rule for
	// its hardware profile).
	"models_hardware.go:listModelLocalQueues": "a project's Kueue LocalQueues, for a model's hardware profile",
	// The Distributed workloads status (GET /api/resources/kueue) and its
	// operations read with the admin's own token.
	"kueue_reads.go:v1beta2Served":       "API discovery of kueue.x-k8s.io (a read that fails is not \"absent\")",
	"kueue_reads.go:readInstance":        "the Kueue CR cluster (kueue.openshift.io)",
	"kueue_reads.go:readFlavor":          "ResourceFlavor nightly-kueue-cpu",
	"kueue_reads.go:readClusterQueue":    "ClusterQueue nightly-kueue",
	"kueue_reads.go:readLocalQueue":      "a project's LocalQueue nightly-kueue",
	"kueue_reads.go:listClusterQueues":   "the ClusterQueues, for what uses the tool's ResourceFlavor and whether kueue is in use",
	"kueue_reads.go:listLocalQueues":     "the LocalQueues of a project or of all, for a default queue and what uses the tool's ClusterQueue",
	"kueue_reads.go:listWorkloads":       "a project's Kueue Workloads, the samples' state and what uses the LocalQueue",
	"kueue_reads.go:readSample":          "a project's RayCluster nightly-raycluster or RayJob nightly-rayjob",
	"kueue_reads.go:readNamespace":       "a project's namespace, for its labels",
	"kueue_reads.go:readDashboardConfig": "the dashboard's OdhDashboardConfig, for disableKueue",
	"kueue_reads.go:readProfile":         "hardware profile nightly-kueue-cpu",
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

// transportMethods are the Client methods that implement the requests
// besides the request methods and REST helpers themselves (requestMethods,
// helperMethods). Like them they take a path parameter, not a path, so the
// scans leave these declarations out and attribute each request to the call
// site of the request method. Every other function is scanned, in client.go,
// client_context.go and rest.go too: one there that sends a path it takes
// is refused, as anywhere else.
var transportMethods = map[string]string{
	"applyWithOptions": "the body of Apply and DryRunApply",
	"doWithHeader":     "the body of Do and ListIntoAt",
}

// isTransport reports whether fn is a Client method that implements the
// requests: one of requestMethods, helperMethods or transportMethods.
func (p *sourcePkg) isTransport(fn *ast.FuncDecl) bool {
	name := fn.Name.Name
	_, request := requestMethods[name]
	_, helper := helperMethods[name]
	_, transport := transportMethods[name]
	if fn.Recv == nil || (!request && !helper && !transport) {
		return false
	}
	obj, ok := p.defs[fn.Name].(*types.Func)
	return ok && isClientType(obj.Signature().Recv().Type())
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
	"ListIntoAt":      "Get",
	"DeleteIfExists":  "Delete",
	"PatchJSON":       "Patch",
}

// requestMethod is the request a call of the method name sends, as a
// requestMethods value, when name is a request method or REST helper.
func requestMethod(name string) (string, bool) {
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

// httpMethod is the request method of e when it names one of the net/http
// constants of rawMethods.
func (p *sourcePkg) httpMethod(e ast.Expr) (string, bool) {
	sel, ok := ast.Unparen(e).(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	c, ok := p.uses[sel.Sel].(*types.Const)
	if !ok || c.Pkg() == nil || c.Pkg().Path() != "net/http" {
		return "", false
	}
	m, ok := rawMethods[c.Name()]
	return m, ok
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
// type) by its package's import path and its name; a method by
// "Type.Method".
type symID struct{ pkg, name string }

// sourcePkg is the source of pkg/cluster and every package under it. Files
// are keyed by base name, unique across the tree (loadClusterSource checks
// it), so call sites ("file:Func") and function positions ("file:line")
// keep their keys when a file moves to another package.
type sourcePkg struct {
	fset  *token.FileSet
	files map[string]*ast.File
	// pkgOf is the import path of each file's package.
	pkgOf  map[string]string
	consts map[symID]string
	lits   map[symID]*ast.CompositeLit
	funcs  map[symID]*ast.FuncDecl
	fileOf map[*ast.FuncDecl]string
	// methods are the method declarations, by type ("Type.Method").
	methods map[symID]*ast.FuncDecl
	// uses and defs are what each identifier refers to (go/types): names
	// are resolved in their lexical scope, never by spelling.
	uses map[*ast.Ident]types.Object
	defs map[*ast.Ident]types.Object
	// selections are the type checker's selectors (methods and fields).
	selections map[*ast.SelectorExpr]*types.Selection
	// clientCalls are the selectors that name a Client method (by type).
	clientCalls map[*ast.SelectorExpr]clientCall
	// outsideRefs counts the references (calls and function values) to the
	// functions of pkg/cluster from the packages the scan does not read
	// (pkg/api, main).
	outsideRefs map[symID]int
	// only, when set, limits the scan to these functions (funcPos keys):
	// their API calls, and the call sites a parameter is resolved from.
	only map[string]bool

	// Memos of the resolver, valid for this scan (only is fixed): the
	// resolver re-walked the whole package for every parameter and call
	// result it met, which made the RBAC tests time out under -race.
	identMemo  map[identKey][]string
	resultMemo map[resultKey][]string
	callers    map[symID][]callSite // by callee; built on first use
	// unlisted are the functions used where the scan lists no call site
	// (unlistedCallers); built on first use.
	unlisted map[symID]bool
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
// qualified by its package (pkg.F(...)), with or without type arguments,
// in a scanned function.
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

// funcIdent is the identifier that names the function a call expression
// calls (f, pkg.F, a method's name), without parentheses and type
// arguments; nil for any other expression.
func funcIdent(fun ast.Expr) *ast.Ident {
	fun = ast.Unparen(fun)
	switch x := fun.(type) {
	case *ast.IndexExpr:
		fun = ast.Unparen(x.X)
	case *ast.IndexListExpr:
		fun = ast.Unparen(x.X)
	}
	switch x := fun.(type) {
	case *ast.Ident:
		return x
	case *ast.SelectorExpr:
		return x.Sel
	}
	return nil
}

// funcRef is the package function id names (not a method, nor a local
// closure or variable of the same name), by the type checker.
func (p *sourcePkg) funcRef(id *ast.Ident) (symID, bool) {
	return funcRefIn(p.uses, id)
}

// pkgLevel is the package-level constant, variable or function id refers
// to, in the scope where it appears.
func (p *sourcePkg) pkgLevel(id *ast.Ident) (symID, bool) {
	obj := p.uses[id]
	if obj == nil || obj.Pkg() == nil || obj.Parent() != obj.Pkg().Scope() {
		return symID{}, false
	}
	return symID{obj.Pkg().Path(), obj.Name()}, true
}

// isPackageName reports whether e names an imported package.
func (p *sourcePkg) isPackageName(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	if !ok {
		return false
	}
	_, ok = p.uses[id].(*types.PkgName)
	return ok
}

// methodOf is the method sel names, as {its type's package, "Type.Method"}.
func (p *sourcePkg) methodOf(sel *ast.SelectorExpr) (symID, bool) {
	s, ok := p.selections[sel]
	if !ok || s.Kind() == types.FieldVal {
		return symID{}, false
	}
	return methodID(s.Obj().(*types.Func))
}

// methodID names a method by its receiver's named type and package.
func methodID(fn *types.Func) (symID, bool) {
	recv := fn.Signature().Recv()
	if recv == nil {
		return symID{}, false
	}
	t := recv.Type()
	if ptr, ok := t.(*types.Pointer); ok {
		t = ptr.Elem()
	}
	named, ok := types.Unalias(t).(*types.Named)
	if !ok || named.Obj().Pkg() == nil {
		return symID{}, false
	}
	return symID{named.Obj().Pkg().Path(), named.Obj().Name() + "." + fn.Name()}, true
}

// callee is the package function a call names: f(...), pkg.F(...), with or
// without type arguments.
func (p *sourcePkg) callee(call *ast.CallExpr) (symID, bool) {
	return p.funcRef(funcIdent(call.Fun))
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
			for ref, call := range funcCalls(caller.Body, p.funcRef) {
				p.callers[ref] = append(p.callers[ref], callSite{caller, call})
			}
		}
	}
	return p.callers[id]
}

// funcCalls yields each call in node of a function that ref resolves (with
// or without type arguments, in parentheses or not).
func funcCalls(node ast.Node, ref func(*ast.Ident) (symID, bool)) func(yield func(symID, *ast.CallExpr) bool) {
	return func(yield func(symID, *ast.CallExpr) bool) {
		ok := true
		ast.Inspect(node, func(n ast.Node) bool {
			if !ok {
				return false
			}
			if call, isCall := n.(*ast.CallExpr); isCall {
				if id, isFunc := ref(funcIdent(call.Fun)); isFunc {
					ok = yield(id, call)
				}
			}
			return true
		})
	}
}

// paramCallSitesOf is callSitesOf for resolving a parameter of id; unknown
// reports that the parameter can also take a value only known at run time,
// from callers that cannot be listed (unlistedCallers). Such a caller widens
// the parameter even when id has callers the scan does list: their values
// are kept, never replaced by the unknown, and never kept alone.
//
// A call from a facade declaration (facadeForward) stands for the facade's
// own calls, so it is replaced by the facade's call sites in the scan, and
// the facade's unlisted callers are id's: pkg/api's calls and uses of the
// facade as a function value (TeardownPipelineServer: called by pkg/api
// with a request's project and stored by cleanup.go as a function value). A
// facade that nothing calls directly keeps an unknown alternative too, as
// an ordinary adapter without call sites would.
func (p *sourcePkg) paramCallSitesOf(id symID) (sites []callSite, unknown bool) {
	unknown = p.unlistedCallers(id)
	for _, cs := range p.callSitesOf(id) {
		if id.pkg == clusterPkg || !p.facadeForward(cs.caller, cs.call) {
			sites = append(sites, cs)
			continue
		}
		w := symID{clusterPkg, cs.caller.Name.Name}
		calls := p.callSitesOf(w)
		sites = append(sites, calls...)
		if len(calls) == 0 || p.unlistedCallers(w) {
			unknown = true
		}
	}
	return sites, unknown
}

// unlistedCallers reports whether the package function id is used where
// the scan lists no call site, so that it is called with arguments the scan
// cannot see: by pkg/api or main (outsideRefs, calls and function values),
// as a function value in a scanned function (stored, returned, passed
// along, with or without type arguments), or in a package-level
// declaration (a function value in a table, or a call in an initializer).
// Uses in the functions the scan leaves out (only) count as little as
// their calls do.
func (p *sourcePkg) unlistedCallers(id symID) bool {
	if p.unlisted == nil {
		p.unlisted = map[symID]bool{}
		for _, name := range slices.Sorted(maps.Keys(p.files)) {
			for _, decl := range p.files[name].Decls {
				fn, isFunc := decl.(*ast.FuncDecl)
				if !isFunc {
					for ref := range funcRefs(decl, p.funcRef, false) {
						p.unlisted[ref] = true
					}
					continue
				}
				if fn.Body == nil || !p.scans(fn) {
					continue
				}
				for ref := range funcRefs(fn.Body, p.funcRef, true) {
					p.unlisted[ref] = true
				}
			}
		}
	}
	return p.unlisted[id] || p.outsideRefs[id] > 0
}

// funcRefs yields each reference in node to a function that ref resolves:
// with valuesOnly, only the uses as a function value, not the function of a
// call (funcIdent: in parentheses or with type arguments).
func funcRefs(node ast.Node, ref func(*ast.Ident) (symID, bool), valuesOnly bool) func(yield func(symID) bool) {
	return func(yield func(symID) bool) {
		called := map[*ast.Ident]bool{}
		if valuesOnly {
			ast.Inspect(node, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					if id := funcIdent(call.Fun); id != nil {
						called[id] = true
					}
				}
				return true
			})
		}
		ok := true
		ast.Inspect(node, func(n ast.Node) bool {
			if !ok {
				return false
			}
			if id, isIdent := n.(*ast.Ident); isIdent && !called[id] {
				if sym, isFunc := ref(id); isFunc {
					ok = yield(sym)
				}
			}
			return true
		})
	}
}

// facadeForward reports whether fn is a facade declaration that only
// forwards to call: an exported, non-generic function of pkg/cluster (not a
// method) whose whole body is `return call`, or the call statement when it
// returns nothing, call passing fn's parameters unchanged and in order (and
// not spread with ...).
func (p *sourcePkg) facadeForward(fn *ast.FuncDecl, call *ast.CallExpr) bool {
	if fn.Recv != nil || fn.Type.TypeParams != nil || !fn.Name.IsExported() || p.pkgOfFunc(fn) != clusterPkg ||
		len(fn.Body.List) != 1 || call.Ellipsis.IsValid() {
		return false
	}
	switch st := fn.Body.List[0].(type) {
	case *ast.ReturnStmt:
		if len(st.Results) != 1 || st.Results[0] != call {
			return false
		}
	case *ast.ExprStmt:
		if st.X != call || fn.Type.Results != nil {
			return false
		}
	default:
		return false
	}
	var params []string
	for _, f := range fn.Type.Params.List {
		if len(f.Names) == 0 {
			return false
		}
		for _, n := range f.Names {
			params = append(params, n.Name)
		}
	}
	if len(call.Args) != len(params) {
		return false
	}
	for i, a := range call.Args {
		if id, ok := a.(*ast.Ident); !ok || id.Name != params[i] {
			return false
		}
	}
	return true
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

// clusterTypes type-checks the parsed tree (clusterSource) once per test
// binary: each package under pkg/cluster from the same syntax trees, its
// imports from their export data (exportData). It fails when a name in
// requestMethods, helperMethods or transportMethods is not a Client method,
// so a renamed request method cannot drop out of the scans.
func clusterTypes(t *testing.T) *typedSource {
	t.Helper()
	clusterTypesOnce.Do(func() {
		ps := clusterSource(t)
		byDir := map[string][]string{}
		for name := range ps.files {
			dir := filepath.Dir(ps.paths[name])
			byDir[dir] = append(byDir[dir], name)
		}
		pkgs := make([]pkgFiles, 0, len(byDir))
		for dir, names := range byDir {
			pkgs = append(pkgs, pkgFiles{path: clusterImportPath(dir), names: names})
		}
		ts, err := typeCheckSource(ps.fset, ps.files, pkgs, exportImporter(t, ps.fset), nil)
		if err != nil {
			clusterTypesErr = err
			return
		}
		if err := ts.checkClientMethods(); err != nil {
			clusterTypesErr = err
			return
		}
		clusterTypesVal = ts
	})
	if clusterTypesErr != nil {
		t.Fatal(clusterTypesErr)
	}
	if clusterTypesVal == nil {
		t.Fatal("the pkg/cluster source could not be type-checked")
	}
	return clusterTypesVal
}

var (
	clusterTypesOnce sync.Once
	clusterTypesVal  *typedSource
	clusterTypesErr  error
)

// exportImporter imports packages from their compiled export data
// (exportData).
func exportImporter(t *testing.T, fset *token.FileSet) types.Importer {
	t.Helper()
	exports := exportData(t)
	return importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		file, ok := exports[path]
		if !ok {
			return nil, fmt.Errorf("no export data for %s", path)
		}
		return os.Open(file)
	})
}

// pkgFiles is one package to type-check: its import path and its files (by
// base name).
type pkgFiles struct {
	path  string
	names []string
}

// typedSource is what the scans take from the type checker: what every
// identifier and selector of the checked files refers to, and the selectors
// that name a Client method.
type typedSource struct {
	uses        map[*ast.Ident]types.Object
	defs        map[*ast.Ident]types.Object
	selections  map[*ast.SelectorExpr]*types.Selection
	clientCalls map[*ast.SelectorExpr]clientCall
	// clients are the Client types the packages declare.
	clients []*types.Named
}

// clientCall is a selector that names a Client method: a method of Client
// (also one promoted from an embedded Client), or a method of an interface
// or type constraint that *Client implements, which may be Client's at run
// time.
type clientCall struct {
	name string
	// argOffset is where the method's own arguments start in a call: 1 for
	// a method expression ((*kube.Client).Get(c, path)), whose first
	// argument is the receiver; else 0.
	argOffset int
	// called: the selector is the function of a call expression. Any other
	// use is a function value, whose calls the scans cannot follow.
	called bool
}

// typeCheckSource type-checks pkgs (in this order) from files. Each
// package's imports come from imp; when checked is not nil, every package
// checked is added to it (an importer that reads it imports them from this
// source: the scanner fixtures).
func typeCheckSource(fset *token.FileSet, files map[string]*ast.File, pkgs []pkgFiles, imp types.Importer, checked map[string]*types.Package) (*typedSource, error) {
	ts := &typedSource{uses: map[*ast.Ident]types.Object{}, defs: map[*ast.Ident]types.Object{},
		selections: map[*ast.SelectorExpr]*types.Selection{}, clientCalls: map[*ast.SelectorExpr]clientCall{}}
	if checked == nil {
		pkgs = slices.Clone(pkgs)
		slices.SortFunc(pkgs, func(a, b pkgFiles) int { return strings.Compare(a.path, b.path) })
	}
	called := map[*ast.SelectorExpr]bool{}
	for _, pf := range pkgs {
		names := slices.Sorted(slices.Values(pf.names))
		fl := make([]*ast.File, 0, len(names))
		for _, name := range names {
			f := files[name]
			fl = append(fl, f)
			ast.Inspect(f, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					if sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr); ok {
						called[sel] = true
					}
				}
				return true
			})
		}
		info := &types.Info{Uses: ts.uses, Defs: ts.defs, Selections: ts.selections}
		pkg, err := (&types.Config{Importer: imp}).Check(pf.path, fset, fl, info)
		if err != nil {
			return nil, fmt.Errorf("type-check %s: %w", pf.path, err)
		}
		if checked != nil {
			checked[pf.path] = pkg
		}
		if tn, ok := pkg.Scope().Lookup("Client").(*types.TypeName); ok && !tn.IsAlias() {
			if named, ok := tn.Type().(*types.Named); ok && isClientType(named) {
				ts.clients = append(ts.clients, named)
			}
		}
	}
	for sel, s := range ts.selections {
		if s.Kind() == types.FieldVal {
			continue
		}
		fn := s.Obj().(*types.Func)
		if recv := fn.Signature().Recv().Type(); !isClientType(recv) {
			iface, ok := types.Unalias(recv).Underlying().(*types.Interface)
			if !ok || !ts.clientMayImplement(iface) {
				continue
			}
		}
		cc := clientCall{name: fn.Name(), called: called[sel]}
		if s.Kind() == types.MethodExpr {
			cc.argOffset = 1
		}
		ts.clientCalls[sel] = cc
	}
	return ts, nil
}

// clientMayImplement reports whether *Client of one of the packages may
// implement iface: it has every method of iface, by name, with as many
// parameters and results (and as variadic). Shapes, not types, are
// compared, so an interface declared in a package checked from export data
// matches the Client checked from source; it over-approximates and never
// misses an interface Client implements. Type terms of a constraint are
// ignored, which over-approximates too.
func (ts *typedSource) clientMayImplement(iface *types.Interface) bool {
	if iface.NumMethods() == 0 {
		return false
	}
	shape := func(sig *types.Signature) [3]int {
		v := 0
		if sig.Variadic() {
			v = 1
		}
		return [3]int{sig.Params().Len(), sig.Results().Len(), v}
	}
	for _, client := range ts.clients {
		all := true
		for i := range iface.NumMethods() {
			m := iface.Method(i)
			obj, _, _ := types.LookupFieldOrMethod(types.NewPointer(client), false, m.Pkg(), m.Name())
			cm, ok := obj.(*types.Func)
			if !ok || shape(cm.Signature()) != shape(m.Signature()) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

// checkClientMethods fails when no package declares Client, or when a name
// in requestMethods, helperMethods or transportMethods is not a method of
// Client.
func (ts *typedSource) checkClientMethods() error {
	if len(ts.clients) == 0 {
		return errors.New("no package under pkg/cluster declares Client")
	}
	for _, client := range ts.clients {
		for _, m := range []map[string]string{requestMethods, helperMethods, transportMethods} {
			for name := range m {
				if fn, _, _ := types.LookupFieldOrMethod(client, true, client.Obj().Pkg(), name); fn == nil {
					return fmt.Errorf("the RBAC scans know a Client request method %s that Client does not have: update requestMethods/helperMethods/transportMethods", name)
				}
			}
		}
	}
	return nil
}

// sourceImporter imports the packages in pkgs (type-checked from source)
// and every other package through fallback.
type sourceImporter struct {
	pkgs     map[string]*types.Package
	fallback types.Importer
}

func (i sourceImporter) Import(path string) (*types.Package, error) {
	if p, ok := i.pkgs[path]; ok {
		return p, nil
	}
	return i.fallback.Import(path)
}

// isClientType reports whether t is Client or *Client of a package under
// pkg/cluster.
func isClientType(t types.Type) bool {
	if ptr, ok := t.(*types.Pointer); ok {
		t = ptr.Elem()
	}
	named, ok := types.Unalias(t).(*types.Named)
	if !ok || named.Obj().Name() != "Client" || named.Obj().Pkg() == nil {
		return false
	}
	path := named.Obj().Pkg().Path()
	return path == clusterPkg || strings.HasPrefix(path, clusterPkg+"/")
}

// funcRefIn is the package function id names, by uses.
func funcRefIn(uses map[*ast.Ident]types.Object, id *ast.Ident) (symID, bool) {
	if id == nil {
		return symID{}, false
	}
	fn, ok := uses[id].(*types.Func)
	if !ok || fn.Pkg() == nil || fn.Signature().Recv() != nil {
		return symID{}, false
	}
	return symID{fn.Pkg().Path(), fn.Name()}, true
}

// countRefs adds the references in files to package functions (calls and
// function values) to counts.
func countRefs(files []*ast.File, uses map[*ast.Ident]types.Object, counts map[symID]int) {
	ref := func(id *ast.Ident) (symID, bool) { return funcRefIn(uses, id) }
	for _, f := range files {
		for id := range funcRefs(f, ref, false) {
			counts[id]++
		}
	}
}

// outsideRefs counts the references of pkg/api and main.go (calls and
// function values) to the functions of pkg/cluster, once per test binary.
func outsideRefs(t *testing.T) map[symID]int {
	t.Helper()
	outsideOnce.Do(func() {
		counts := map[symID]int{}
		for _, tp := range []*typedPkg{typeCheck(t, apiPkg, "../api"), typeCheck(t, "main", "../..", "main.go")} {
			files := make([]*ast.File, 0, len(tp.files))
			for _, name := range slices.Sorted(maps.Keys(tp.files)) {
				files = append(files, tp.files[name])
			}
			countRefs(files, tp.info.Uses, counts)
		}
		outsideVal = counts
	})
	if outsideVal == nil {
		t.Fatal("the references of pkg/api and main.go could not be counted")
	}
	return outsideVal
}

var (
	outsideOnce sync.Once
	outsideVal  map[symID]int
)

// pathHelpers are the functions (and the method, "Type.Method") whose
// results the resolver computes instead of reading their bodies, told by
// package and name: a function of the same name elsewhere is resolved from
// its own body.
var (
	namespacedPathFunc    = symID{rhoaiPkg, "NamespacedPath"}
	clusterPathFunc       = symID{rhoaiPkg, "ClusterPath"}
	activityNamespaceFunc = symID{recordPkg, "GetActivityNamespace"}
	maasAPIFunc           = symID{servingPkg, "MaaSAPI"}
	gitOpsPolicyNameFunc  = symID{gitopsPkg, "gitOpsAdmissionPolicyName"}
	componentListPathFunc = symID{rhoaiPkg, "ComponentListPath"}
	componentKindListPath = symID{rhoaiPkg, "ComponentKind.ListPath"}
	sprintfFunc           = symID{"fmt", "Sprintf"}
)

// checkPathHelpers fails when a path helper the resolver knows is no
// longer declared where it says: renamed or moved, it would be resolved
// from its body, or not at all.
func (p *sourcePkg) checkPathHelpers() error {
	for _, id := range []symID{namespacedPathFunc, clusterPathFunc, activityNamespaceFunc, maasAPIFunc, gitOpsPolicyNameFunc, componentListPathFunc} {
		if p.funcs[id] == nil {
			return fmt.Errorf("the RBAC resolver computes %s.%s, which no package under pkg/cluster declares: update the path helpers", id.pkg, id.name)
		}
	}
	if p.methods[componentKindListPath] == nil {
		return fmt.Errorf("the RBAC resolver computes %s.%s, which no package under pkg/cluster declares: update the path helpers", componentKindListPath.pkg, componentKindListPath.name)
	}
	return nil
}

// loadClusterSource indexes the source of pkg/cluster and of every package
// under it for one scan.
func loadClusterSource(t *testing.T) *sourcePkg {
	t.Helper()
	ps := clusterSource(t)
	pkgOf := make(map[string]string, len(ps.files))
	for name := range ps.files {
		pkgOf[name] = clusterImportPath(filepath.Dir(ps.paths[name]))
	}
	p := newSourcePkg(ps.fset, ps.files, pkgOf, clusterTypes(t), outsideRefs(t))
	if err := p.checkPathHelpers(); err != nil {
		t.Fatal(err)
	}
	return p
}

// newSourcePkg indexes files (by base name; pkgOf is each one's package
// import path) for one scan; outsideRefs counts the references to their
// functions from packages the scan does not read.
func newSourcePkg(fset *token.FileSet, files map[string]*ast.File, pkgOf map[string]string, ts *typedSource, outsideRefs map[symID]int) *sourcePkg {
	p := &sourcePkg{fset: fset, files: map[string]*ast.File{}, pkgOf: map[string]string{},
		consts: map[symID]string{}, lits: map[symID]*ast.CompositeLit{}, funcs: map[symID]*ast.FuncDecl{}, fileOf: map[*ast.FuncDecl]string{},
		methods: map[symID]*ast.FuncDecl{}, uses: ts.uses, defs: ts.defs, selections: ts.selections, clientCalls: ts.clientCalls,
		outsideRefs: outsideRefs,
		identMemo:   map[identKey][]string{}, resultMemo: map[resultKey][]string{}}
	for _, name := range slices.Sorted(maps.Keys(files)) {
		f := files[name]
		pkg := pkgOf[name]
		p.files[name] = f
		p.pkgOf[name] = pkg
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
				} else if fn, ok := p.defs[d.Name].(*types.Func); ok {
					if id, ok := methodID(fn); ok {
						p.methods[id] = d
					}
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
	if fun, ok := ast.Unparen(x.Fun).(*ast.SelectorExpr); ok {
		if m, ok := r.p.methodOf(fun); ok && m == componentKindListPath && len(x.Args) == 0 {
			// rhoai.ComponentKind.ListPath: a discovered module kind.
			return moduleKindPaths()
		}
	}
	// A package function, by plain name or qualified by its package.
	callee, ok := r.p.callee(x)
	if !ok {
		return []string{unknownPart}
	}
	switch callee {
	case sprintfFunc:
		if len(x.Args) == 0 {
			return []string{unknownPart}
		}
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
	case namespacedPathFunc, clusterPathFunc:
		var args [][]string
		for _, a := range x.Args {
			args = append(args, r.resolve(a))
		}
		var out []string
		for _, combo := range combos(args) {
			if callee == namespacedPathFunc {
				out = append(out, rhoai.NamespacedPath(combo[0], combo[1], combo[2], combo[3]))
			} else {
				out = append(out, rhoai.ClusterPath(combo[0], combo[1], combo[2]))
			}
		}
		return out
	case activityNamespaceFunc:
		return []string{appNSMarker}
	case maasAPIFunc:
		// maas.opendatahub.io at its served version
		return []string{serving.MaaSGroup + "/" + unknownPart}
	case gitOpsPolicyNameFunc:
		return []string{gitOpsPolicyNameMarker}
	case componentListPathFunc:
		// A discovered kind of one of the module API groups.
		return moduleKindPaths()
	}
	if _, ok := r.p.funcs[callee]; ok {
		return r.resolveCallResult(x, 0)
	}
	return []string{unknownPart}
}

// moduleKindPaths are the collections of a discovered kind in each module
// API group (internal/rhoai platform.go), at a discovered version.
func moduleKindPaths() []string {
	var out []string
	for _, g := range rhoai.ModuleAPIGroups {
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
	// A package constant, as the identifier refers to it where it is.
	if pid, ok := r.p.pkgLevel(id); ok {
		if v, ok := r.p.consts[pid]; ok {
			return []string{v}
		}
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
	// A function parameter: resolve the argument at every call site, and
	// keep a value known only at run time when the function has callers
	// that cannot be listed.
	if idx := paramIndex(r.fn, id.Name); idx >= 0 && r.fn.Recv == nil {
		sites, unknown := r.p.paramCallSitesOf(symID{r.p.pkgOfFunc(r.fn), r.fn.Name.Name})
		for _, cs := range sites {
			if idx < len(cs.call.Args) {
				out = append(out, resolver{r.p, cs.caller, r.depth + 1}.resolve(cs.call.Args[idx])...)
			}
		}
		if unknown {
			out = append(out, unknownPart)
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
	id, ok := r.p.callee(call)
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
			lit = r.findCompositeLit(v)
		case *ast.SelectorExpr:
			if id, ok := r.p.pkgLevel(v.Sel); ok && r.p.isPackageName(v.X) {
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

func (r resolver) findCompositeLit(ident *ast.Ident) *ast.CompositeLit {
	name := ident.Name
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
	if found == nil {
		if id, ok := r.p.pkgLevel(ident); ok {
			found = r.p.lits[id]
		}
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
		if r.p.isPackageName(x.X) {
			if id, ok := r.p.pkgLevel(x.Sel); ok {
				if v, ok := r.p.consts[id]; ok {
					return []string{v}
				}
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

// pointerTarget is T, keyed by package, when typ is *T or *pkg.T (by the
// type checker; T of any package).
func (p *sourcePkg) pointerTarget(typ ast.Expr) (symID, bool) {
	star, ok := typ.(*ast.StarExpr)
	if !ok {
		return symID{}, false
	}
	var name *ast.Ident
	switch x := star.X.(type) {
	case *ast.Ident:
		name = x
	case *ast.SelectorExpr:
		name = x.Sel
	default:
		return symID{}, false
	}
	tn, ok := p.uses[name].(*types.TypeName)
	if !ok || tn.Pkg() == nil {
		return symID{}, false
	}
	return symID{tn.Pkg().Path(), tn.Name()}, true
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
			return p.pointerTarget(f.Type)
		}
	}
	return symID{}, false
}

// pointerParamsOf names fn's parameters of type *typ.
func (p *sourcePkg) pointerParamsOf(fn *ast.FuncDecl, typ symID) map[string]bool {
	var out map[string]bool
	for _, f := range fn.Type.Params.List {
		if id, ok := p.pointerTarget(f.Type); ok && id == typ {
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
	reqs, seenExcluded, problems := loadClusterSource(t).scan(only, onlyBookkeeping)
	if len(problems) > 0 {
		t.Fatalf("cannot derive the RBAC needed by these API calls; resolve the path statically or extend the test:\n%s", strings.Join(problems, "\n"))
	}
	return reqs, seenExcluded
}

// scan scans the functions in only (nil: all) for Client calls, or only the
// calls on c.Bookkeeping() with onlyBookkeeping. problems are the calls
// whose request it cannot derive.
func (p *sourcePkg) scan(only map[string]bool, onlyBookkeeping bool) ([]rbacRequest, map[string]bool, []string) {
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
			if !ok {
				// A Client request method stored by a package-level variable.
				_, refused := p.requestCalls(file, decl)
				problems = append(problems, refused...)
				continue
			}
			if fn.Body == nil || p.isTransport(fn) || !p.scans(fn) {
				continue
			}
			fnPos := p.funcPos(fn)
			calls, refused := p.requestCalls(file, fn.Body)
			problems = append(problems, refused...)
			for _, rc := range calls {
				if onlyBookkeeping && !p.isBookkeeping(rc.client) {
					continue
				}
				site := fmt.Sprintf("%s:%s", file, fn.Name.Name)
				if _, excluded := rbacCallsNotUsingTheSAToken[site]; excluded {
					seenExcluded[site] = true
					continue
				}
				if _, bestEffort := rbacBestEffortCalls[site]; bestEffort {
					seenExcluded[site] = true
					continue
				}
				if _, userRead := rbacUserTokenReads[site]; userRead {
					seenExcluded[site] = true
					continue
				}
				if _, gitOpsOnly := rbacGitOpsOnlyCalls[site]; gitOpsOnly && only == nil {
					seenExcluded[site] = true
					continue
				}
				pos := p.fset.Position(rc.call.Pos())
				manifests := manifestNames(resolver{p: p, fn: fn})
				for _, path := range (resolver{p: p, fn: fn}).resolve(rc.path) {
					rs, err := toRequests(rc.method, path, fmt.Sprintf("%s:%d", file, pos.Line))
					if err != nil {
						problems = append(problems, fmt.Sprintf("%s:%d %s: %v", file, pos.Line, rc.method, err))
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
			}
		}
	}
	return reqs, seenExcluded, problems
}

// requestCall is a call of a Client request method or REST helper.
type requestCall struct {
	call *ast.CallExpr
	// method is the request it sends (a requestMethods value; Do's is its
	// http.Method's), path its path argument.
	method string
	path   ast.Expr
	// client is the expression of the client it is sent with: the
	// selector's receiver, or a method expression's first argument.
	client ast.Expr
}

// requestCalls lists the calls of Client request methods and REST helpers
// in node, by type (clientCalls): on a Client, an embedded Client, an
// interface or constraint *Client implements, or through a method
// expression. refused are the ones whose request cannot be derived: a Do
// whose method is not an http.Method constant, and a request method used as
// a function value (stored, passed, assigned), whose calls cannot be
// followed.
func (p *sourcePkg) requestCalls(file string, node ast.Node) (calls []requestCall, refused []string) {
	ast.Inspect(node, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			if cc, ok := p.clientCalls[x]; ok && !cc.called {
				if _, request := requestMethod(cc.name); request {
					refused = append(refused, fmt.Sprintf("%s:%d %s: a Client request method used as a function value; the scan cannot follow its calls, so call it directly", file, p.fset.Position(x.Pos()).Line, cc.name))
				}
			}
		case *ast.CallExpr:
			sel, ok := ast.Unparen(x.Fun).(*ast.SelectorExpr)
			if !ok {
				return true
			}
			cc, ok := p.clientCalls[sel]
			if !ok || len(x.Args) <= cc.argOffset {
				return true
			}
			method, ok := requestMethod(cc.name)
			if !ok {
				return true
			}
			args := x.Args[cc.argOffset:]
			rc := requestCall{call: x, method: method, path: args[0], client: sel.X}
			if cc.argOffset > 0 {
				rc.client = x.Args[0]
			}
			if method == "do" {
				// c.Do(http.MethodX, path, ...): the raw request method.
				m, ok := p.httpMethod(args[0])
				if !ok || len(args) < 2 {
					refused = append(refused, fmt.Sprintf("%s:%d do: request method is not an http.Method constant", file, p.fset.Position(x.Pos()).Line))
					return true
				}
				rc.method, rc.path = m, args[1]
			}
			if _, known := methodVerbs[rc.method]; !known && rc.method != "get" {
				return true
			}
			calls = append(calls, rc)
		}
		return true
	})
	return calls, refused
}

// isBookkeeping reports whether client is a call of Client.Bookkeeping().
func (p *sourcePkg) isBookkeeping(client ast.Expr) bool {
	call, ok := ast.Unparen(client).(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
	if !ok {
		return false
	}
	cc, ok := p.clientCalls[sel]
	return ok && cc.name == "Bookkeeping"
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
