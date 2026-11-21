package cluster

import (
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
	facade := `package cluster

import (
	"` + innerPkg + `"
	kube "` + fixKubePkg + `"
)

type Client = kube.Client

func Teardown(c *Client, project string) error { return inner.Teardown(c, project) }

func Pinned(c *Client) error { return inner.Teardown(c, "pinned") }

func Wrapped(c *Client, ns string) error { return inner.Other(c, ns) }

func Recreate(c *Client) error { return Wrapped(c, "from-facade") }
`
	inner := `package inner

import kube "` + fixKubePkg + `"

func Teardown(c *kube.Client, project string) error { return nil }

func replace(c *kube.Client) error { return Teardown(c, "from-inner") }

func Other(c *kube.Client, ns string) error { return nil }
`
	api := `package api

import "` + clusterPkg + `"

func teardown(c *cluster.Client, project string) error { return cluster.Teardown(c, project) }

func pinned(c *cluster.Client) error { return cluster.Pinned(c) }
`
	p := loadFixture(t, fixKube(nil), fixturePkg{path: innerPkg, files: map[string]string{"inner.go": inner}},
		fixturePkg{path: clusterPkg, files: map[string]string{"facade.go": facade}},
		fixturePkg{path: apiPkg, files: map[string]string{"api.go": api}, outside: true})
	if got, want := paramValues(t, p, innerPkg, "Teardown", "project"), []string{"from-inner", "pinned"}; !slices.Equal(got, want) {
		t.Errorf("inner.Teardown's project = %q, want %q (the forwarding facade.Teardown adds no caller)", got, want)
	}
	if got, want := paramValues(t, p, innerPkg, "Other", "ns"), []string{"from-facade"}; !slices.Equal(got, want) {
		t.Errorf("inner.Other's ns = %q, want %q (facade.Wrapped's callers)", got, want)
	}
}
