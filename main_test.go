package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
)

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
