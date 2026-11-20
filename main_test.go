package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
)

func TestIsLoopbackHost(t *testing.T) {
	for host, want := range map[string]bool{
		"127.0.0.1": true, "127.0.0.2": true, "localhost": true, "LOCALHOST": true, "::1": true, "[::1]": true,
		"": false, "0.0.0.0": false, "::": false, "192.168.1.10": false, "example.com": false,
	} {
		if got := isLoopbackHost(host); got != want {
			t.Errorf("isLoopbackHost(%q) = %v", host, got)
		}
	}
}

// DEV_MODE listens on 127.0.0.1 by default and refuses a non-loopback
// BIND_ADDRESS unless DEV_ALLOW_REMOTE=true; outside DEV_MODE the address
// is BIND_ADDRESS as given (the template sets 127.0.0.1).
func TestListenAddress(t *testing.T) {
	for _, tc := range []struct {
		dev             bool
		bind, allow     string
		want            string
		refused, warned bool
	}{
		{dev: true, want: "127.0.0.1"},
		{dev: true, bind: "localhost", want: "localhost"},
		{dev: true, bind: "::1", want: "::1"},
		{dev: true, bind: "0.0.0.0", want: "0.0.0.0", refused: true},
		{dev: true, bind: "192.168.1.10", allow: "yes", want: "192.168.1.10", refused: true},
		{dev: true, bind: "0.0.0.0", allow: "true", want: "0.0.0.0", warned: true},
		{dev: false, want: ""},
		{dev: false, bind: "0.0.0.0", want: "0.0.0.0"},
		{dev: false, bind: "127.0.0.1", want: "127.0.0.1"},
	} {
		env := map[string]string{"BIND_ADDRESS": tc.bind, "DEV_ALLOW_REMOTE": tc.allow}
		got, warning, err := listenAddress(tc.dev, func(k string) string { return env[k] })
		if got != tc.want || (err != nil) != tc.refused || (warning != "") != tc.warned {
			t.Errorf("%+v: %q, warning %q, err %v", tc, got, warning, err)
		}
		if err != nil && !strings.Contains(err.Error(), "refusing to serve DEV_MODE on a non-loopback address") {
			t.Errorf("%+v: error %v", tc, err)
		}
	}
}

// TestMainRegistersNoUngatedRoutes: API endpoints are registered with
// api.Register, whose table the api package tests against the mutation
// gate. main.go itself may only add GET routes and the read-only catch-alls,
// so a handler added here cannot bypass that test.
func TestMainRegistersNoUngatedRoutes(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	catchAlls := map[string]bool{"/api/": true, "/": true}
	usesRegister := false
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "api" && sel.Sel.Name == "Register" {
			usesRegister = true
		}
		if sel.Sel.Name != "HandleFunc" && sel.Sel.Name != "Handle" || len(call.Args) == 0 {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok {
			t.Errorf("route pattern is not a literal: %#v", call.Args[0])
			return true
		}
		pattern, _ := strconv.Unquote(lit.Value)
		if !strings.HasPrefix(pattern, "GET ") && !catchAlls[pattern] {
			t.Errorf("main.go registers %q; add non-GET API routes to api.routes instead", pattern)
		}
		return true
	})
	if !usesRegister {
		t.Fatal("main.go does not call api.Register")
	}
}
