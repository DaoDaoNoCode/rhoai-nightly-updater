package cluster

import (
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// clusterLayers is the order of the packages under pkg/cluster/internal,
// bottom first, as doc.go and CONTRIBUTING.md ("Backend layout") list it:
// a package imports only packages of a lower layer, and none imports
// pkg/cluster. No package imports one of its own layer.
var clusterLayers = [][]string{
	{"kube", "report"},
	{"rhoai", "authz"},
	{"record", "argo", "registry"},
	{"catalog", "components"},
	{"dashdev", "envkit", "node"},
	{"mlflow", "storage", "operator"},
	{"serving", "gitops", "workbench"},
	{"modelregistry", "kueue"},
	{"dsc"},
	{"features", "diagnostics"},
	{"observability"},
	{"recipe"},
}

// layerViolations checks the imports of the internal packages (package path
// under internal/ -> import paths of its non-test files) against
// clusterLayers.
func layerViolations(imports map[string][]string) []string {
	layer := map[string]int{}
	for i, names := range clusterLayers {
		for _, name := range names {
			layer[name] = i
		}
	}
	internal := clusterPkg + "/internal/"
	var bad []string
	pkgs := make([]string, 0, len(imports))
	for pkg := range imports {
		pkgs = append(pkgs, pkg)
	}
	slices.Sort(pkgs)
	for _, pkg := range pkgs {
		from, known := layer[pkg]
		if !known {
			bad = append(bad, fmt.Sprintf("internal/%s is in no layer: add it to clusterLayers (layers_test.go), doc.go and CONTRIBUTING.md (Backend layout)", pkg))
		}
		paths := slices.Clone(imports[pkg])
		slices.Sort(paths)
		for _, path := range slices.Compact(paths) {
			switch {
			case path == clusterPkg:
				bad = append(bad, fmt.Sprintf("internal/%s imports pkg/cluster, the facade over it", pkg))
			case strings.HasPrefix(path, internal):
				to := strings.TrimPrefix(path, internal)
				l, ok := layer[to]
				switch {
				case !ok:
					bad = append(bad, fmt.Sprintf("internal/%s imports internal/%s, which is in no layer", pkg, to))
				case !known:
					// Reported above; its layer decides the rest.
				case l == from:
					bad = append(bad, fmt.Sprintf("internal/%s imports internal/%s of its own layer (%s): import only lower layers", pkg, to, strings.Join(clusterLayers[l], ", ")))
				case l > from:
					bad = append(bad, fmt.Sprintf("internal/%s (layer %d) imports internal/%s of the higher layer %d: import only lower layers", pkg, from+1, to, l+1))
				}
			}
		}
	}
	return bad
}

// The internal packages import only lower layers (clusterLayers), and the
// layers are the ones doc.go and CONTRIBUTING.md list. Only non-test files
// count: an external test package may import any package.
func TestInternalPackagesImportOnlyLowerLayers(t *testing.T) {
	t.Run("the check refuses", func(t *testing.T) {
		internal := clusterPkg + "/internal/"
		for _, tc := range []struct {
			name    string
			imports map[string][]string
			want    int
		}{
			{"a higher layer", map[string][]string{"kube": {internal + "rhoai"}}, 1},
			{"its own layer", map[string][]string{"record": {internal + "registry"}}, 1},
			{"the facade", map[string][]string{"dsc": {clusterPkg}}, 1},
			{"a new package", map[string][]string{"newpkg": {internal + "kube"}}, 1},
			{"a new package importing the facade", map[string][]string{"newpkg": {clusterPkg}}, 2},
			{"an unknown one", map[string][]string{"dsc": {internal + "newpkg"}}, 1},
			{"one bad of many", map[string][]string{"features": {"fmt", internal + "dsc", internal + "diagnostics"}}, 1},
			{"lower layers and other packages", map[string][]string{"dsc": {"fmt", internal + "serving", internal + "kube", clusterPkg + "x"}}, 0},
		} {
			if got := layerViolations(tc.imports); len(got) != tc.want {
				t.Errorf("%s: %d violations, want %d: %q", tc.name, len(got), tc.want, got)
			}
		}
	})

	imports := map[string][]string{}
	fset := token.NewFileSet()
	for dir, paths := range clusterSourceDirs(t) {
		pkg, ok := strings.CutPrefix(dir, "internal/")
		if !ok {
			continue
		}
		imports[pkg] = []string{}
		for _, path := range paths {
			f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, spec := range f.Imports {
				p, err := strconv.Unquote(spec.Path.Value)
				if err != nil {
					t.Fatal(err)
				}
				imports[pkg] = append(imports[pkg], p)
			}
		}
	}
	for _, v := range layerViolations(imports) {
		t.Error(v)
	}
	for _, names := range clusterLayers {
		for _, name := range names {
			if _, ok := imports[name]; !ok {
				t.Errorf("clusterLayers lists %s, which is no package under internal/: update it, doc.go and CONTRIBUTING.md", name)
			}
		}
	}

	if got := docLayers(t); !slices.EqualFunc(got, clusterLayers, slices.Equal) {
		t.Errorf("doc.go lists the layers\n  %v\nclusterLayers has\n  %v", got, clusterLayers)
	}
	if got := contributingLayers(t); !slices.EqualFunc(got, clusterLayers, slices.Equal) {
		t.Errorf("CONTRIBUTING.md (Backend layout) lists the layers\n  %v\nclusterLayers has\n  %v", got, clusterLayers)
	}
}

// docLayers reads the layer table of doc.go: a line's first column (before
// two spaces) names a layer's packages; a first column ending in a comma
// continues on the next line.
func docLayers(t *testing.T) [][]string {
	t.Helper()
	b, err := os.ReadFile("doc.go")
	if err != nil {
		t.Fatal(err)
	}
	_, table, ok := strings.Cut(string(b), "// The internal packages, each importing only packages listed above it:\n//\n")
	if ok {
		table, _, ok = strings.Cut(table, "//\n")
	}
	if !ok {
		t.Fatal("doc.go: no layer table (\"The internal packages, each importing only packages listed above it:\")")
	}
	var layers [][]string
	more := false
	for line := range strings.SplitSeq(strings.TrimSuffix(table, "\n"), "\n") {
		first, _, _ := strings.Cut(strings.TrimPrefix(line, "//\t"), "  ")
		first = strings.TrimSpace(first)
		if first == "" {
			continue
		}
		var names []string
		for name := range strings.SplitSeq(strings.TrimSuffix(first, ","), ",") {
			names = append(names, strings.TrimSpace(name))
		}
		if more {
			layers[len(layers)-1] = append(layers[len(layers)-1], names...)
		} else {
			layers = append(layers, names)
		}
		more = strings.HasSuffix(first, ",")
	}
	return layers
}

// contributingLayers reads the Layers item of CONTRIBUTING.md's Backend
// layout: after "From the bottom: ", layers separated by "; ", each naming
// its packages in backquotes.
func contributingLayers(t *testing.T) [][]string {
	t.Helper()
	b, err := os.ReadFile("../../CONTRIBUTING.md")
	if err != nil {
		t.Fatal(err)
	}
	_, list, ok := strings.Cut(string(b), "From the bottom: ")
	if ok {
		list, _, ok = strings.Cut(list, ". ")
	}
	if !ok {
		t.Fatal(`CONTRIBUTING.md: no layer list ("From the bottom: ...")`)
	}
	name := regexp.MustCompile("`([a-z]+)`")
	var layers [][]string
	for part := range strings.SplitSeq(list, "; ") {
		var names []string
		for _, m := range name.FindAllStringSubmatch(part, -1) {
			names = append(names, m[1])
		}
		layers = append(layers, names)
	}
	return layers
}
