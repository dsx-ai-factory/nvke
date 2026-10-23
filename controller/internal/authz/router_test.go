// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/api"
)

// The one direction the other parity tests cannot cover.
//
// actions_test.go compares the generated table against api/openapi.yaml, and the
// generator is built from api/openapi.yaml — so both sides of that comparison
// come from the same reading of the same document, and a route the *router*
// registers and neither reading produced is invisible to it. That is not
// hypothetical: while the generator carried a hand-written list of five methods,
// a spec declaring `head:` or `options:` got a route registered by
// HandlerWithOptions, no row in the table, and a 403 on every call to it with
// all of CI green.
//
// So this reads what the router registered. api/server.gen.go is generated, so
// its registrations are a list of literals, and parsing them is the closest a
// test can get to asking http.ServeMux what is on it — which it has no API for.
//
// Its scope is exactly that one file: registeredPatterns only walks the
// HandleFunc calls api/server.gen.go itself contains. A route registered some
// other way — directly on the mux in internal/server/server.go, say — would not
// appear here and this test would not catch it. That is a coverage gap in this
// test, not a live security hole: an unmatched route is denied by Resolve
// (ErrNoRoute), not silently allowed, so the failure mode for a route this test
// missed is fail-closed. Worth a future reader knowing, rather than assuming
// these three tests are a complete guarantee that every registered route is
// authorized.
func registeredPatterns(t *testing.T) map[string]string {
	t.Helper()

	path := filepath.Join("..", "..", "api", "server.gen.go")
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	require.NoErrorf(t, err, "parsing %s", path)

	patterns := map[string]string{}
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "HandleFunc" || len(call.Args) != 2 {
			return true
		}
		pattern, ok := patternLiteral(call.Args[0])
		if !ok {
			// A registration built some other way is a gap this test cannot
			// see, so it fails rather than skipping quietly.
			t.Errorf("%s registers a route with a pattern this test cannot read: %#v", path, call.Args[0])
			return true
		}
		patterns[pattern] = handlerName(call.Args[1])
		return true
	})
	return patterns
}

// patternLiteral evaluates the concatenation HandlerWithOptions builds a pattern
// from: "GET " + options.BaseURL + "/clusters". BaseURL is the only non-literal
// term, and internal/server passes api.BasePath for it.
func patternLiteral(expr ast.Expr) (string, bool) {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return "", false
		}
		unquoted, err := strconv.Unquote(e.Value)
		return unquoted, err == nil
	case *ast.SelectorExpr:
		if e.Sel.Name != "BaseURL" {
			return "", false
		}
		return api.BasePath, true
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return "", false
		}
		left, ok := patternLiteral(e.X)
		if !ok {
			return "", false
		}
		right, ok := patternLiteral(e.Y)
		if !ok {
			return "", false
		}
		return left + right, true
	default:
		return "", false
	}
}

func handlerName(expr ast.Expr) string {
	if selector, ok := expr.(*ast.SelectorExpr); ok {
		return selector.Sel.Name
	}
	return "?"
}

func TestEveryRegisteredRouteIsInTheTable(t *testing.T) {
	registered := registeredPatterns(t)
	require.NotEmpty(t, registered, "no routes were read out of api/server.gen.go; the check would pass vacuously")

	for pattern, handler := range registered {
		_, ok := operations[pattern]
		assert.Truef(t, ok,
			"the router registers %q (%s) and actions.gen.go has no row for it, so every call to it is denied "+
				"as an unknown operation; run 'go generate ./internal/authz/'", pattern, handler)
	}
}

func TestEveryTableEntryIsRegistered(t *testing.T) {
	registered := registeredPatterns(t)

	for pattern := range operations {
		_, ok := registered[pattern]
		assert.Truef(t, ok,
			"actions.gen.go names %q and the router registers no such route; run 'go generate ./internal/authz/'",
			pattern)
	}
}

// The table and the router must agree on the base path too. They spell it once
// each — api.BasePath — and this is what would notice if one of them stopped.
func TestEveryTableEntryCarriesTheBasePath(t *testing.T) {
	require.NotEmpty(t, operations)
	for pattern := range operations {
		_, path, ok := strings.Cut(pattern, " ")
		require.Truef(t, ok, "%q is not a method-and-path pattern", pattern)
		assert.Truef(t, strings.HasPrefix(path, api.BasePath+"/"),
			"%q does not start at %q", pattern, api.BasePath)
	}
}
