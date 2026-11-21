package cluster

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"testing"
)

// A parameter of a function in a package under pkg/cluster is resolved at
// its call sites as before the split: a facade wrapper that only forwards
// its parameters adds no caller of its own (pkg/api's calls of it are not
// in the scanned source), a wrapper's own callers are followed, and a facade
// function that passes anything else is a caller like any other.
func TestRBACParamThroughFacadeWrapper(t *testing.T) {
	innerPkg := clusterPkg + "/internal/inner"
	sources := map[string]struct{ pkg, src string }{
		"facade.go": {clusterPkg, `package cluster

import "` + innerPkg + `"

func Teardown(c *Client, project string) error { return inner.Teardown(c, project) }

func Pinned(c *Client) error { return inner.Teardown(c, "pinned") }

func Wrapped(c *Client, ns string) error { return inner.Other(c, ns) }

func Recreate(c *Client) error { return Wrapped(c, "from-facade") }
`},
		"inner.go": {innerPkg, `package inner

func Teardown(c *Client, project string) error { return nil }

func replace(c *Client) error { return Teardown(c, "from-inner") }

func Other(c *Client, ns string) error { return nil }
`},
	}
	p := &sourcePkg{fset: token.NewFileSet(), files: map[string]*ast.File{}, pkgOf: map[string]string{},
		imports: map[string]map[string]string{}, consts: map[symID]string{}, lits: map[symID]*ast.CompositeLit{},
		funcs: map[symID]*ast.FuncDecl{}, fileOf: map[*ast.FuncDecl]string{}, identMemo: map[identKey][]string{},
		resultMemo: map[resultKey][]string{}, locals: map[*ast.FuncDecl]map[string]bool{}}
	for name, s := range sources {
		f, err := parser.ParseFile(p.fset, name, s.src, 0)
		if err != nil {
			t.Fatal(err)
		}
		p.files[name], p.pkgOf[name] = f, s.pkg
		p.imports[name] = map[string]string{"inner": innerPkg}
		for _, d := range f.Decls {
			if fn, ok := d.(*ast.FuncDecl); ok {
				p.funcs[symID{s.pkg, fn.Name.Name}] = fn
				p.fileOf[fn] = name
			}
		}
	}
	param := func(pkg, fn, name string) []string {
		got := slices.Clone(resolver{p, p.funcs[symID{pkg, fn}], 1}.resolveIdent(ast.NewIdent(name)))
		slices.Sort(got)
		return got
	}
	if got, want := param(innerPkg, "Teardown", "project"), []string{"from-inner", "pinned"}; !slices.Equal(got, want) {
		t.Errorf("inner.Teardown's project = %q, want %q (the forwarding facade.Teardown adds no caller)", got, want)
	}
	if got, want := param(innerPkg, "Other", "ns"), []string{"from-facade"}; !slices.Equal(got, want) {
		t.Errorf("inner.Other's ns = %q, want %q (facade.Wrapped's callers)", got, want)
	}
}
