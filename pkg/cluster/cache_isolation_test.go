package cluster

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/components"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/kube"
	"github.com/juntwang/rhoai-nightly-updater/pkg/cluster/internal/rhoai"
)

// resetClusterCaches empties the package's caches, among them those keyed
// by an API server URL. In production one URL is one cluster, so sharing
// them is right. In tests every fake API server is https://127.0.0.1:<port>
// and the OS reuses ports, so a later test's fake cluster could read what
// an earlier one cached: a stale 15-minute catalog index made
// TestDSCCheck_OperandMessage_OperatorNotInstalled fail on a CI runner. The
// shared fake constructors call it when a fake cluster starts and ends. It
// empties every registered cache (cache.go): each LRU registers itself, and
// each hand-rolled cache registers its purge function.
func resetClusterCaches() {
	kube.PurgeAllCaches()
}

// isolateClusterCaches gives a test's fake cluster empty URL-keyed caches.
func isolateClusterCaches(t *testing.T) {
	t.Helper()
	resetClusterCaches()
	t.Cleanup(resetClusterCaches)
}

// urlKeyedState is where pkg/cluster and the packages under it key state by
// the API server URL (c.baseURL or c.BaseURL()): per file (base name, unique
// across pkg/cluster/**), how many lines use it and the package-level caches
// those lines read or write. Each cache must be registered (cache.go): made
// with newLRU, or emptied by a function passed to registerCachePurge.
type urlKeyedState struct {
	lines  int
	caches []string
}

// Every place the tree keys state by the API server URL, per file. A new
// one needs its cache registered; then update this list.
func TestResetClusterCachesCoversEveryURLKeyedCache(t *testing.T) {
	want := map[string]urlKeyedState{
		"api_versions.go":                 {1, []string{"APIVersionsCache"}},
		"argocd.go":                       {1, []string{"ApplicationsCache"}},
		"client_testing.go":               {1, nil}, // TestOptions copies the field for a test; no state
		"components.go":                   {1, []string{"replicaSetOwnerCache"}},
		"control_plane.go":                {2, []string{"ControlPlaneCache"}},
		"dashboard_operator_discovery.go": {2, []string{"TargetCache"}},
		"diagnostics_dsc.go":              {1, []string{"staleSeen"}},
		"maas.go":                         {1, []string{"APIVersionsCache"}}, // a read
		"models_call.go":                  {1, []string{"callChecks"}},
		"models_image.go":                 {2, []string{"CapacityCache", "PlanCache"}},
		"models_project_facts.go":         {1, []string{"capacitySnapshots"}},
		"prerequisites.go":                {2, []string{"packageIndexCache"}},
		"rhoai_namespaces.go":             {1, []string{"NamespacesCache"}},
		"status.go":                       {3, []string{"stableTargetCache", "consoleURLCache"}},
		"dashboard_url_cache.go":          {2, []string{"URLCache"}},
	}
	dirs := clusterSourceDirs(t)
	got := map[string]int{}
	for _, paths := range dirs {
		for _, path := range paths {
			f := filepath.Base(path)
			if clientOwnFiles[f] {
				continue
			}
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			for line := range strings.SplitSeq(string(b), "\n") {
				if (strings.Contains(line, "c.baseURL") || strings.Contains(line, ".BaseURL()")) && !strings.Contains(line, "return user, c.BaseURL()") {
					got[f]++
				}
			}
		}
	}
	for f, n := range got {
		if want[f].lines != n {
			t.Errorf("%s keys state by the API server URL %d times, the list says %d: register any new cache (cache.go), then update this list", f, n, want[f].lines)
		}
	}
	for f, w := range want {
		if got[f] == 0 {
			t.Errorf("%s no longer keys state by the API server URL (the list says %d): update this list", f, w.lines)
		}
	}
	registered := registeredCaches(t)
	for f, w := range want {
		for _, name := range w.caches {
			how, ok := registered[name]
			switch {
			case !ok:
				t.Errorf("%s: cache %s is not registered: make it with newLRU, or pass a function that empties it to registerCachePurge next to its declaration", f, name)
			case how == "":
				t.Errorf("%s: cache %s is declared more than once under pkg/cluster; name the one meant", f, name)
			}
		}
	}
}

// registeredCaches maps the name of each package-level variable under
// pkg/cluster/** that is a registered cache to how it registers: "newLRU"
// when its initializer calls newLRU, "purge" when a function literal passed
// to registerCachePurge in a package-level declaration names it. A name
// declared in two packages maps to "". The source does not change while the
// tests run, so it is read once.
func registeredCaches(t *testing.T) map[string]string {
	t.Helper()
	registeredCachesOnce.Do(func() { registeredCachesVal = readRegisteredCaches(t) })
	if registeredCachesVal == nil {
		t.Fatal("the cache registry could not be read")
	}
	return registeredCachesVal
}

var (
	registeredCachesOnce sync.Once
	registeredCachesVal  map[string]string
)

func readRegisteredCaches(t *testing.T) map[string]string {
	t.Helper()
	ps := clusterSource(t)
	byDir := map[string][]*ast.File{}
	for name, f := range ps.files {
		dir := filepath.Dir(ps.paths[name])
		byDir[dir] = append(byDir[dir], f)
	}
	return registeredCachesIn(byDir)
}

// registeredCachesIn is registeredCaches of these packages (files by
// package directory). A name declared at package level in more than one
// package maps to "", however each registers (or does not): a URL-keyed
// cache is listed by name, which must name one variable.
func registeredCachesIn(byDir map[string][]*ast.File) map[string]string {
	calls := func(call *ast.CallExpr, name string) bool {
		fun := call.Fun
		switch f := fun.(type) {
		case *ast.IndexListExpr: // newLRU[K, V](...)
			fun = f.X
		case *ast.IndexExpr:
			fun = f.X
		}
		switch f := fun.(type) {
		case *ast.Ident:
			return f.Name == name
		case *ast.SelectorExpr:
			return f.Sel.Name == name
		}
		return false
	}
	out := map[string]string{}
	// declaredIn: the package directories declaring each package-level name.
	declaredIn := map[string]map[string]bool{}
	for dir, files := range byDir {
		declared := map[string]bool{} // this package's package-level vars
		purged := map[string]bool{}   // names in its registerCachePurge functions
		for _, f := range files {
			// Package-level var declarations only: a cache registers next
			// to its declaration.
			for _, decl := range f.Decls {
				gd, ok := decl.(*ast.GenDecl)
				if !ok || gd.Tok != token.VAR {
					continue
				}
				for _, spec := range gd.Specs {
					vs := spec.(*ast.ValueSpec)
					for i, n := range vs.Names {
						if n.Name == "_" {
							continue
						}
						declared[n.Name] = true
						if declaredIn[n.Name] == nil {
							declaredIn[n.Name] = map[string]bool{}
						}
						declaredIn[n.Name][dir] = true
						if i < len(vs.Values) {
							if call, ok := vs.Values[i].(*ast.CallExpr); ok && (calls(call, "newLRU") || calls(call, "NewLRU")) {
								out[n.Name] = "newLRU"
							}
						}
					}
					for _, v := range vs.Values {
						call, ok := v.(*ast.CallExpr)
						if !ok || len(call.Args) != 1 || !calls(call, "registerCachePurge") && !calls(call, "RegisterCachePurge") {
							continue
						}
						if lit, ok := call.Args[0].(*ast.FuncLit); ok {
							ast.Inspect(lit.Body, func(n ast.Node) bool {
								if id, ok := n.(*ast.Ident); ok {
									purged[id.Name] = true
								}
								return true
							})
						}
					}
				}
			}
		}
		for name := range purged {
			if declared[name] && out[name] == "" {
				out[name] = "purge"
			}
		}
	}
	for name, dirs := range declaredIn {
		if len(dirs) > 1 {
			out[name] = ""
		}
	}
	return out
}

// The registry check itself: an LRU registers by being made, a hand-rolled
// cache only through registerCachePurge.
func TestRegisteredCachesSeesEveryKind(t *testing.T) {
	reg := registeredCaches(t)
	for name, want := range map[string]string{
		"APIVersionsCache":  "newLRU",
		"packageIndexCache": "purge",
		"staleSeen":         "purge",
		"callChecks":        "purge",
	} {
		if reg[name] != want {
			t.Errorf("registeredCaches()[%s] = %q, want %q", name, reg[name], want)
		}
	}
	if _, ok := reg["packageIndexTTL"]; ok {
		t.Error("a variable declared next to a registered cache is not a cache")
	}
}

// A cache name declared in two packages names no one cache, however each
// registers; one declaration keeps its kind.
func TestRegisteredCachesRejectsDuplicateNames(t *testing.T) {
	fset := token.NewFileSet()
	parse := func(src string) *ast.File {
		f, err := parser.ParseFile(fset, "fixture.go", src, 0)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	lru := func(pkg, name string) *ast.File {
		return parse("package " + pkg + "\nvar " + name + " = newLRU[string, int](8)\n")
	}
	purged := func(pkg, name string) *ast.File {
		return parse("package " + pkg + "\nvar " + name + " = map[string]int{}\nvar _ = registerCachePurge(func() { clear(" + name + ") })\n")
	}
	plain := func(pkg, name string) *ast.File {
		return parse("package " + pkg + "\nvar " + name + " = map[string]int{}\n")
	}
	reg := registeredCachesIn(map[string][]*ast.File{
		"a": {lru("a", "sameLRU"), purged("a", "samePurge"), lru("a", "mixed"), lru("a", "onceLRU"), purged("a", "oncePurge"), plain("a", "halfRegistered")},
		"b": {lru("b", "sameLRU"), purged("b", "samePurge"), purged("b", "mixed"), lru("b", "halfRegistered")},
	})
	for name, want := range map[string]string{
		"sameLRU":        "",
		"samePurge":      "",
		"mixed":          "",
		"halfRegistered": "",
		"onceLRU":        "newLRU",
		"oncePurge":      "purge",
	} {
		if got, ok := reg[name]; !ok || got != want {
			t.Errorf("registeredCachesIn()[%s] = %q (present %v), want %q", name, got, ok, want)
		}
	}
}

// A fake cluster starts with nothing an earlier fake cluster cached, even
// under the same URL (a reused port).
func TestFakeClusterStartsWithEmptyURLKeyedCaches(t *testing.T) {
	const earlier = "https://127.0.0.1:1"
	components.SeedPackageIndexForTest(earlier)
	rhoai.APIVersionsCache.Add(earlier+"|kuadrant.io", []string{"v1"}, 0)
	newFakeAPI(t)
	n := components.PackageIndexLenForTest()
	if _, ok := rhoai.APIVersionsCache.Get(earlier + "|kuadrant.io"); ok || n != 0 {
		t.Fatalf("a new fake cluster still sees an earlier one's caches: package index %d entries, api versions cached %v", n, ok)
	}
}

// clientOwnFiles are the files of internal/kube that declare Client, its
// accessors and its REST helpers: they read the API server URL to send the
// requests, not as a cache key.
var clientOwnFiles = map[string]bool{
	"client.go":         true,
	"client_context.go": true,
	"rest.go":           true,
}
