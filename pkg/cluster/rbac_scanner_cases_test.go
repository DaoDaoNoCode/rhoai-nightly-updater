package cluster

import (
	"slices"
	"testing"
)

// H1: a request through an interface or a generic constraint that *Client
// implements, or through a method expression (the receiver is the first
// argument), is a Client request; a method of the same name on an interface
// Client does not implement, or on another type, is none.
func TestRBACScanFindsInterfaceAndMethodExpressionCalls(t *testing.T) {
	src := `package fixapp

import (
	"net/http"

	kube "` + fixKubePkg + `"
)

type getter interface {
	Get(path string) ([]byte, int, error)
}

type booker interface {
	Bookkeeping() *kube.Client
	Delete(path string) (int, error)
}

func viaInterface(g getter) { _, _, _ = g.Get("/api/v1/namespaces/h1/secrets/via-interface") }

func viaMethodExpression(c *kube.Client) {
	_, _, _ = (*kube.Client).Get(c, "/api/v1/namespaces/h1/secrets/via-method-expression")
}

func viaInterfaceMethodExpression(c *kube.Client) {
	_, _, _ = getter.Get(c, "/api/v1/namespaces/h1/secrets/via-interface-method-expression")
}

func viaConstraint[T getter](c T) { _, _, _ = c.Get("/api/v1/namespaces/h1/secrets/via-constraint") }

func viaMethodExpressionDo(c *kube.Client) {
	_, _, _ = (*kube.Client).Do(c, http.MethodPost, "/api/v1/namespaces/h1/configmaps", "", nil)
}

func viaEmbedded(r struct{ *kube.Client }) { _, _ = r.Delete("/api/v1/namespaces/h1/configmaps/via-embedded") }

func viaInterfaceNamingClient(b booker) { _, _ = b.Delete("/api/v1/namespaces/h1/configmaps/via-booker") }

func viaBookkeepingExpression(c *kube.Client) {
	_, _ = (*kube.Client).Delete(c.Bookkeeping(), "/api/v1/namespaces/h1/configmaps/via-bookkeeping")
}

type lookup interface {
	Get(key string, n int) error
}

func notAClient(l lookup) { _ = l.Get("/api/v1/namespaces/h1/secrets/not-a-client", 1) }

type memo struct{}

func (memo) Get(key string) ([]byte, int, error) { return nil, 0, nil }

func notAClientType(m memo) { _, _, _ = m.Get("/api/v1/namespaces/h1/secrets/memo-key") }
`
	p := loadFixture(t, fixKube(nil), fixturePkg{path: fixAppPkg, files: map[string]string{"h1.go": src}})
	keys, problems := fixtureScan(t, p)
	checkKeys(t, keys, []string{
		"get /secrets ns=h1 name=via-interface",
		"get /secrets ns=h1 name=via-method-expression",
		"get /secrets ns=h1 name=via-interface-method-expression",
		"get /secrets ns=h1 name=via-constraint",
		"create /configmaps ns=h1 name=",
		"delete /configmaps ns=h1 name=via-embedded",
		"delete /configmaps ns=h1 name=via-booker",
		"delete /configmaps ns=h1 name=via-bookkeeping",
	}, []string{
		"get /secrets ns=h1 name=not-a-client",
		"get /secrets ns=h1 name=memo-key",
	})
	checkProblems(t, problems, "h1.go", src)
	books, _, _ := p.scan(nil, true)
	if len(books) != 1 || books[0].Name != "via-bookkeeping" {
		t.Errorf("bookkeeping requests %v, want the one on c.Bookkeeping() passed as the method expression's receiver", books)
	}
}

// H2: an import is shadowed only inside the scope of the declaration that
// shadows it (go/types, not names): a closure parameter named like an
// imported package hides nothing outside the closure, and a local closure
// named like a package function is not that function.
func TestRBACScanImportsFollowLexicalScope(t *testing.T) {
	inner := `package fixinner

import kube "` + fixKubePkg + `"

func Fetch(c *kube.Client, ns string) { _, _, _ = c.Get("/api/v1/namespaces/" + ns + "/secrets/h2") }
`
	src := `package fixapp

import (
	"` + fixInnerPkg + `"
	kube "` + fixKubePkg + `"
)

func callsThenShadowsInAClosure(c *kube.Client) {
	fixinner.Fetch(c, "before-the-closure")
	_ = func(fixinner string) string { return fixinner }
}

func callsPlainly(c *kube.Client) { fixinner.Fetch(c, "plain") }

func shadowsForReal(c *kube.Client) {
	fixinner := struct{ Fetch func(*kube.Client, string) }{}
	fixinner.Fetch(c, "a-field-not-the-package")
}

func fetchHere(c *kube.Client, ns string) { _, _, _ = c.Get("/api/v1/namespaces/" + ns + "/configmaps/h2") }

func callsHere(c *kube.Client) { fetchHere(c, "package-function") }

func shadowsHere(c *kube.Client) {
	fetchHere := func(*kube.Client, string) {}
	fetchHere(c, "local-closure")
}
`
	p := loadFixture(t, fixKube(nil), fixturePkg{path: fixInnerPkg, files: map[string]string{"inner.go": inner}},
		fixturePkg{path: fixAppPkg, files: map[string]string{"h2.go": src}})
	keys, problems := fixtureScan(t, p)
	checkKeys(t, keys, []string{
		"get /secrets ns=before-the-closure name=h2",
		"get /secrets ns=plain name=h2",
		"get /configmaps ns=package-function name=h2",
	}, []string{
		"get /secrets ns=a-field-not-the-package name=h2",
		"get /configmaps ns=local-closure name=h2",
	})
	checkProblems(t, problems, "h2.go", src)
}

// H3: the path helpers the scan knows (rhoai.NamespacedPath, the activity
// namespace, rhoai.ComponentKind.ListPath, fmt.Sprintf, the http.Method
// constants of Do) are told by package and name (ListPath by its type too);
// a same-named function of another package is resolved from its own body,
// and a method or constant that is not the known one is refused.
func TestRBACScanSpecialHelpersByIdentity(t *testing.T) {
	record := `package record

func GetActivityNamespace() string { return "not-read" }
`
	rhoaiSrc := `package rhoai

func NamespacedPath(apiGroup, resource, namespace, name string) string { return "not-read" }

type ComponentKind struct{ Group, Version, Resource string }

func (k ComponentKind) ListPath() string { return "not-read" }
`
	other := `package fixother

func GetActivityNamespace() string { return "other-ns" }

func NamespacedPath(apiGroup, resource, namespace, name string) string {
	return "/api/v1/namespaces/" + namespace + "/configmaps/" + name
}

type Kind struct{}

func (Kind) ListPath() string { return "/api/v1/pods" }

const MethodGet = "DELETE"
`
	fmtSrc := `package fixfmt

func Sprintf(format string, args ...any) string { return "/api/v1/namespaces/fixfmt/secrets/h3-fmt" }
`
	src := `package fixapp

import (
	"net/http"

	"` + fixOtherPkg + `"
	kube "` + fixKubePkg + `"
	"` + recordPkg + `"
	"` + rhoaiPkg + `"
)

func helpers(c *kube.Client, k rhoai.ComponentKind, o fixother.Kind) {
	_, _, _ = c.Get("/api/v1/namespaces/" + record.GetActivityNamespace() + "/secrets/h3-record")
	_, _, _ = c.Get("/api/v1/namespaces/" + fixother.GetActivityNamespace() + "/secrets/h3-other")
	_, _, _ = c.Get(rhoai.NamespacedPath("v1", "secrets", "h3", "h3-rhoai"))
	_, _, _ = c.Get(fixother.NamespacedPath("v1", "secrets", "h3", "h3-other-path"))
	_, _, _ = c.Get(k.ListPath())
	_, _, _ = c.Get(o.ListPath()) // other-list-path
	_, _, _ = c.Do(http.MethodGet, "/api/v1/namespaces/h3/secrets/h3-http", "", nil)
	_, _, _ = c.Do(fixother.MethodGet, "/api/v1/namespaces/h3/secrets/h3-other-method", "", nil) // other-method
}
`
	fmtUse := `package fixapp

import (
	fmt "` + fixFmtPkg + `"
	kube "` + fixKubePkg + `"
)

func sprintf(c *kube.Client) {
	_, _, _ = c.Get(fmt.Sprintf("/api/v1/namespaces/%s/secrets/h3-sprintf", "sprintf-ns"))
}
`
	p := loadFixture(t, fixKube(nil),
		fixturePkg{path: recordPkg, files: map[string]string{"activity.go": record}},
		fixturePkg{path: rhoaiPkg, files: map[string]string{"platform.go": rhoaiSrc}},
		fixturePkg{path: fixOtherPkg, files: map[string]string{"other.go": other}},
		fixturePkg{path: fixFmtPkg, files: map[string]string{"fmt.go": fmtSrc}},
		fixturePkg{path: fixAppPkg, files: map[string]string{"h3.go": src, "h3fmt.go": fmtUse}})
	keys, problems := fixtureScan(t, p)
	checkKeys(t, keys, []string{
		"get /secrets ns=" + appNSMarker + " name=h3-record",
		"get /secrets ns=other-ns name=h3-other",
		"get /secrets ns=h3 name=h3-rhoai",
		"get /configmaps ns=h3 name=h3-other-path",
		"list components.platform.opendatahub.io/* ns= name=",
		"get /secrets ns=h3 name=h3-http",
		"get /secrets ns=fixfmt name=h3-fmt",
	}, []string{
		"get /secrets ns=" + appNSMarker + " name=h3-other",
		"get /secrets ns=h3 name=h3-other-path",
		"get /secrets ns=h3 name=h3-other-method",
		"get /secrets ns=sprintf-ns name=h3-sprintf",
	})
	checkProblems(t, problems, "h3.go", src, "other-list-path", "other-method")
}

// H4: a facade's pure forward (returning or not) stands for the facade's
// own calls. A facade that nothing calls directly (used only as a function
// value, in the scanned source or outside it, or not at all) keeps an
// unknown alternative, as an ordinary adapter without call sites does;
// pkg/api's direct calls add nothing, as before the split when pkg/api
// called the function itself. A function that is not an exported pure
// forward of the facade (reordered arguments, a constant, a variadic
// forward, a method, an unexported adapter) is an ordinary caller.
func TestRBACScanFacadeKeepsCallersItCannotList(t *testing.T) {
	inner := `package fixinner

import kube "` + fixKubePkg + `"

func ByValue(c *kube.Client, ns string) error        { return nil }
func ByOutsideValue(c *kube.Client, ns string) error { return nil }
func Uncalled(c *kube.Client, ns string) error       { return nil }
func ByOutsideCall(c *kube.Client, ns string) error  { return nil }
func Pruned(c *kube.Client, ns string)               {}
func Swapped(c *kube.Client, ns string) error        { return nil }
func Constant(c *kube.Client, ns string) error       { return nil }
func Variadic(c *kube.Client, ns ...string) error    { return nil }
func Method(c *kube.Client, ns string) error         { return nil }
func Adapter(c *kube.Client, ns string) error        { return nil }

func fixed(c *kube.Client) {
	_ = ByValue(c, "fixed")
	_ = ByOutsideValue(c, "fixed")
	_ = Uncalled(c, "fixed")
	_ = ByOutsideCall(c, "fixed")
	Pruned(c, "fixed")
	_ = Swapped(c, "fixed")
	_ = Constant(c, "fixed")
	_ = Variadic(c, "fixed")
	_ = Method(c, "fixed")
	_ = Adapter(c, "fixed")
}
`
	facade := `package cluster

import (
	"` + fixInnerPkg + `"
	kube "` + fixKubePkg + `"
)

type Client = kube.Client

func ByValue(c *Client, ns string) error { return fixinner.ByValue(c, ns) }

var byValue = []func(*Client, string) error{ByValue}

func ByOutsideValue(c *Client, ns string) error { return fixinner.ByOutsideValue(c, ns) }

func Uncalled(c *Client, ns string) error { return fixinner.Uncalled(c, ns) }

func ByOutsideCall(c *Client, ns string) error { return fixinner.ByOutsideCall(c, ns) }

func Pruned(c *Client, ns string) { fixinner.Pruned(c, ns) }

func callsPruned(c *Client) { Pruned(c, "via-pruned") }

func Swapped(ns string, c *Client) error { return fixinner.Swapped(c, ns) }

func callsSwapped(c *Client) { _ = Swapped("via-swapped", c) }

func Constant(c *Client) error { return fixinner.Constant(c, "pinned") }

func Variadic(c *Client, ns ...string) error { return fixinner.Variadic(c, ns...) }

func callsVariadic(c *Client) { _ = Variadic(c, "via-variadic") }

type handler struct{}

func (handler) Method(c *Client, ns string) error { return fixinner.Method(c, ns) }

func callsMethod(c *Client) { _ = handler{}.Method(c, "via-method") }

func adapter(c *Client, ns string) error { return fixinner.Adapter(c, ns) }

var adapters = []func(*Client, string) error{adapter}
`
	api := `package api

import "` + clusterPkg + `"

var handlers = map[string]func(*cluster.Client, string) error{"v": cluster.ByOutsideValue}

func handle(c *cluster.Client, ns string) error {
	cluster.Pruned(c, ns)
	return cluster.ByOutsideCall(c, ns)
}
`
	p := loadFixture(t, fixKube(nil), fixturePkg{path: fixInnerPkg, files: map[string]string{"inner.go": inner}},
		fixturePkg{path: clusterPkg, files: map[string]string{"facade.go": facade}},
		fixturePkg{path: apiPkg, files: map[string]string{"api.go": api}, outside: true})
	for _, tc := range []struct {
		fn   string
		want []string
	}{
		{"ByValue", []string{unknownPart, "fixed"}},
		{"ByOutsideValue", []string{unknownPart, "fixed"}},
		{"Uncalled", []string{unknownPart, "fixed"}},
		{"ByOutsideCall", []string{"fixed"}},
		{"Pruned", []string{"fixed", "via-pruned"}},
		{"Swapped", []string{"fixed", "via-swapped"}},
		{"Constant", []string{"fixed", "pinned"}},
		{"Variadic", []string{"fixed", "via-variadic"}},
		{"Method", []string{unknownPart, "fixed"}},
		{"Adapter", []string{unknownPart, "fixed"}},
	} {
		if got := paramValues(t, p, fixInnerPkg, tc.fn, "ns"); !slices.Equal(got, tc.want) {
			t.Errorf("fixinner.%s's ns = %q, want %q", tc.fn, got, tc.want)
		}
	}
}

// M1: a Client request method used as a function value (a method value,
// a method expression not called at once, a method passed along) cannot be
// followed, so the scan refuses it; a generic function's call with explicit
// type arguments is a call site like any other.
func TestRBACScanRefusesFuncValuesAndFollowsInstantiations(t *testing.T) {
	inner := `package fixinner

import kube "` + fixKubePkg + `"

func Fetch[T any](c *kube.Client, ns string, _ T) {
	_, _, _ = c.Get("/api/v1/namespaces/" + ns + "/secrets/m1-qualified")
}
`
	src := `package fixapp

import (
	"` + fixInnerPkg + `"
	kube "` + fixKubePkg + `"
)

type getter interface {
	Get(path string) ([]byte, int, error)
}

func methodValue(c *kube.Client) {
	get := c.Get // method-value
	_, _, _ = get("/api/v1/namespaces/m1/secrets/method-value")
}

var deleteRef = (*kube.Client).Delete // package-level

func passed(c *kube.Client) { run(c.GetInto) } // passed-along

func run(f func(string, any) error) { _ = f("/api/v1/namespaces/m1/secrets/passed", nil) }

func interfaceValue(g getter) {
	f := g.Get // interface-value
	_, _, _ = f("/api/v1/namespaces/m1/secrets/interface-value")
}

func calledInParens(c *kube.Client) { _, _, _ = (c.Get)("/api/v1/namespaces/m1/secrets/in-parens") }

func Fetch[T any](c *kube.Client, ns string, _ T) {
	_, _, _ = c.Get("/api/v1/namespaces/" + ns + "/secrets/m1-generic")
}

func Fetch2[T, U any](c *kube.Client, ns string, _ T, _ U) {
	_, _, _ = c.Get("/api/v1/namespaces/" + ns + "/configmaps/m1-generic")
}

func callers(c *kube.Client) {
	Fetch(c, "implicit", 1)
	Fetch[string](c, "explicit", "x")
	Fetch2(c, "implicit", 1, "")
	Fetch2[int, string](c, "explicit-list", 1, "")
	fixinner.Fetch(c, "implicit", 1)
	fixinner.Fetch[int](c, "explicit-qualified", 1)
}
`
	p := loadFixture(t, fixKube(nil), fixturePkg{path: fixInnerPkg, files: map[string]string{"inner.go": inner}},
		fixturePkg{path: fixAppPkg, files: map[string]string{"m1.go": src}})
	keys, problems := fixtureScan(t, p)
	checkKeys(t, keys, []string{
		"get /secrets ns=m1 name=in-parens",
		"get /secrets ns=implicit name=m1-generic",
		"get /secrets ns=explicit name=m1-generic",
		"get /configmaps ns=implicit name=m1-generic",
		"get /configmaps ns=explicit-list name=m1-generic",
		"get /secrets ns=implicit name=m1-qualified",
		"get /secrets ns=explicit-qualified name=m1-qualified",
	}, nil)
	checkProblems(t, problems, "m1.go", src, "method-value", "package-level", "passed-along", "interface-value")
}

// M2: only the declarations that implement the requests (the request
// methods, the REST helpers, applyWithOptions) are left out of the scan,
// not the files they are in: a request any other function of client.go,
// client_context.go or rest.go makes is scanned, and a Client method that
// sends a path it takes, without being one of those, is refused.
func TestRBACScanExcludesTransportDeclarationsOnly(t *testing.T) {
	kube := fixKube(map[string]string{
		"client.go": `func (c *Client) ReadVersionish() { _, _, _ = c.Get("/api/v1/namespaces/m2/configmaps/in-client-go") }

func (c *Client) Head(path string) ([]byte, int, error) { return c.Do("HEAD", path, "", nil) } // unlisted-transport
`,
		"client_context.go": `func (c *Client) ReadOwn() { _, _, _ = c.Get("/api/v1/namespaces/m2/secrets/in-client-context") }
`,
		"rest.go": `func ReadDefaults(c *Client) error { return c.GetInto("/api/v1/namespaces/m2/configmaps/in-rest-go", nil) }
`,
	})
	keys, problems := fixtureScan(t, loadFixture(t, kube))
	checkKeys(t, keys, []string{
		"get /configmaps ns=m2 name=in-client-go",
		"get /secrets ns=m2 name=in-client-context",
		"get /configmaps ns=m2 name=in-rest-go",
	}, nil)
	checkProblems(t, problems, "client.go", kube.files["client.go"], "unlisted-transport")
}
