package cluster

// Fixtures of the RBAC scanner (rbac_coverage_test.go), independent of the
// application's golden: small packages type-checked from source in memory,
// each with the forms of calls the scan must find or must refuse.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const (
	fixKubePkg  = clusterPkg + "/internal/fixkube"
	fixInnerPkg = clusterPkg + "/internal/fixinner"
	fixAppPkg   = clusterPkg + "/internal/fixapp"
	fixOtherPkg = clusterPkg + "/internal/fixother"
	fixFmtPkg   = clusterPkg + "/internal/fixfmt"
)

// fixturePkg is one package of a scanner fixture: its import path and its
// files (base name -> source). An outside package (pkg/api, main) is
// type-checked but not scanned.
type fixturePkg struct {
	path    string
	files   map[string]string
	outside bool
}

// loadFixture type-checks pkgs (each after the packages it imports) from
// source and indexes the packages that are not outside as the scan indexes
// the tree under pkg/cluster.
func loadFixture(t *testing.T, pkgs ...fixturePkg) *sourcePkg {
	t.Helper()
	fset := token.NewFileSet()
	files := map[string]*ast.File{}
	pkgOf := map[string]string{}
	scanned := map[string]*ast.File{}
	order := make([]pkgFiles, 0, len(pkgs))
	for _, fp := range pkgs {
		pf := pkgFiles{path: fp.path}
		for _, name := range slices.Sorted(maps.Keys(fp.files)) {
			if _, dup := files[name]; dup {
				t.Fatalf("fixture file %s appears twice", name)
			}
			f, err := parser.ParseFile(fset, name, fp.files[name], 0)
			if err != nil {
				t.Fatal(err)
			}
			files[name] = f
			pkgOf[name] = fp.path
			pf.names = append(pf.names, name)
			if !fp.outside {
				scanned[name] = f
			}
		}
		order = append(order, pf)
	}
	checked := map[string]*types.Package{}
	ts, err := typeCheckSource(fset, files, order, sourceImporter{checked, exportImporter(t, fset)}, checked)
	if err != nil {
		t.Fatal(err)
	}
	var outside []*ast.File
	for name, f := range files {
		if _, ok := scanned[name]; !ok {
			outside = append(outside, f)
		}
	}
	counts := map[symID]int{}
	countRefs(outside, ts.uses, counts)
	return newSourcePkg(fset, scanned, pkgOf, ts, counts)
}

// fixKube is a Client with the request methods and REST helpers of
// internal/kube, in files named as there; extra adds declarations to a file.
func fixKube(extra map[string]string) fixturePkg {
	files := map[string]string{
		"client.go": `package fixkube

import "net/http"

type Client struct{ book *Client }

func (c *Client) Do(method, path, contentType string, body []byte) ([]byte, int, error) { return nil, 0, nil }

func (c *Client) Get(path string) ([]byte, int, error) { return c.Do(http.MethodGet, path, "", nil) }

func (c *Client) Post(path string, data []byte) ([]byte, int, error) { return c.Do(http.MethodPost, path, "", data) }

func (c *Client) Put(path string, data []byte) ([]byte, int, error) { return c.Do(http.MethodPut, path, "", data) }

func (c *Client) Patch(path string, data []byte) ([]byte, int, error) { return c.Do(http.MethodPatch, path, "", data) }

func (c *Client) StrategicPatch(path string, data []byte) ([]byte, int, error) {
	return c.Do(http.MethodPatch, path, "", data)
}

func (c *Client) Apply(path string, resource any) ([]byte, int, error) { return c.applyWithOptions(path, resource, false) }

func (c *Client) DryRunApply(path string, resource any) ([]byte, int, error) {
	return c.applyWithOptions(path, resource, true)
}

func (c *Client) applyWithOptions(path string, _ any, _ bool) ([]byte, int, error) {
	return c.Do(http.MethodPatch, path, "", nil)
}

func (c *Client) Delete(path string) (int, error) {
	_, status, err := c.Do(http.MethodDelete, path, "", nil)
	return status, err
}

func (c *Client) Bookkeeping() *Client { return c.book }
`,
		"client_context.go": `package fixkube

func (c *Client) Username() string { return "" }
`,
		"rest.go": `package fixkube

func (c *Client) GetOptional(path string) ([]byte, bool, error) {
	body, _, err := c.Get(path)
	return body, true, err
}

func (c *Client) GetInto(path string, _ any) error {
	_, _, err := c.Get(path)
	return err
}

func (c *Client) GetIntoOptional(path string, out any) (bool, error) { return true, c.GetInto(path, out) }

func (c *Client) ListInto(path string, items any) error { return c.GetInto(path, items) }

func (c *Client) PatchJSON(path string, _ any) error {
	_, _, err := c.Patch(path, nil)
	return err
}

func (c *Client) DeleteIfExists(path string) (bool, error) {
	_, err := c.Delete(path)
	return true, err
}
`,
	}
	for name, src := range extra {
		files[name] += "\n" + src
	}
	return fixturePkg{path: fixKubePkg, files: files}
}

// fixtureScan scans every function of p: the distinct request keys and the
// problems.
func fixtureScan(t *testing.T, p *sourcePkg) (map[string]bool, []string) {
	t.Helper()
	reqs, _, problems := p.scan(nil, false)
	keys := map[string]bool{}
	for _, r := range reqs {
		keys[r.key()] = true
	}
	return keys, problems
}

// checkKeys fails for each key of want the scan did not derive and each key
// of absent it did.
func checkKeys(t *testing.T, keys map[string]bool, want, absent []string) {
	t.Helper()
	for _, k := range want {
		if !keys[k] {
			t.Errorf("the scan does not derive %q", k)
		}
	}
	for _, k := range absent {
		if keys[k] {
			t.Errorf("the scan derives %q, which is no Client request", k)
		}
	}
	if t.Failed() {
		got := slices.Sorted(maps.Keys(keys))
		t.Logf("derived:\n%s", strings.Join(got, "\n"))
	}
}

// checkProblems fails unless problems are exactly one per marker, each
// naming the file and line of the marker's first line in src.
func checkProblems(t *testing.T, problems []string, file, src string, markers ...string) {
	t.Helper()
	var want []string
	for _, m := range markers {
		want = append(want, file+":"+lineOf(t, src, m))
	}
	sort.Strings(want)
	var got []string
	for _, p := range problems {
		pos, _, _ := strings.Cut(p, " ")
		got = append(got, pos)
	}
	sort.Strings(got)
	if !slices.Equal(got, want) {
		t.Errorf("problems at %q, want at %q:\n%s", got, want, strings.Join(problems, "\n"))
	}
}

// lineOf is the line (from 1) of the first line of src that contains marker.
func lineOf(t *testing.T, src, marker string) string {
	t.Helper()
	for i, l := range strings.Split(src, "\n") {
		if strings.Contains(l, marker) {
			return strconv.Itoa(i + 1)
		}
	}
	t.Fatalf("no line contains %q", marker)
	return ""
}

// paramValues resolves parameter param of the function fn of package pkg
// from its call sites, sorted.
func paramValues(t *testing.T, p *sourcePkg, pkg, fn, param string) []string {
	t.Helper()
	decl := p.funcs[symID{pkg, fn}]
	if decl == nil {
		t.Fatalf("no function %s.%s", pkg, fn)
	}
	for _, f := range decl.Type.Params.List {
		for _, n := range f.Names {
			if n.Name == param {
				got := slices.Clone(resolver{p, decl, 1}.resolveIdent(n))
				slices.Sort(got)
				return got
			}
		}
	}
	t.Fatalf("%s.%s has no parameter %s", pkg, fn, param)
	return nil
}
