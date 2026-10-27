// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package deferredpatch keeps reconciliation writes in the deferred CAPI patch.
package deferredpatch

import (
	"go/ast"
	"go/types"
	"strings"

	"github.com/golangci/plugin-module-register/register"
	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/types/typeutil"
)

const (
	clientPackage = "sigs.k8s.io/controller-runtime/pkg/client"
	patchPackage  = "sigs.k8s.io/cluster-api/util/patch"
	utilPackage   = "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func init() {
	register.Plugin("deferredpatch", func(any) (register.LinterPlugin, error) {
		return plugin{}, nil
	})
}

type plugin struct{}

func (plugin) GetLoadMode() string { return register.LoadModeTypesInfo }

func (plugin) BuildAnalyzers() ([]*analysis.Analyzer, error) {
	return []*analysis.Analyzer{{
		Name: "deferredpatch",
		Doc:  "rejects writes to reconciled CR types outside their deferred CAPI patch",
		Run:  run,
	}}, nil
}

func run(pass *analysis.Pass) (any, error) {
	protected := make(map[types.Type]bool)
	allowed := make(map[*ast.CallExpr]map[types.Type]bool)
	for _, file := range pass.Files {
		if skipFile(pass, file) {
			continue
		}
		for _, declaration := range file.Decls {
			fn, ok := declaration.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || fn.Name.Name != "Reconcile" || fn.Body == nil {
				continue
			}
			reconciled := reconciledTypes(pass, fn)
			for t := range reconciled {
				protected[t] = true
			}
			for call := range deferredCalls(fn) {
				allowed[call] = reconciled
			}
		}
	}
	for _, file := range pass.Files {
		if skipFile(pass, file) {
			continue
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			fn, _ := typeutil.Callee(pass.TypesInfo, call).(*types.Func)
			index := writeObjectIndex(fn)
			args := callArgs(pass.TypesInfo, call)
			if index < 0 || index >= len(args) {
				return true
			}
			t := objectType(pass.TypesInfo.TypeOf(args[index]))
			if !protected[t] || (functionIs(fn, patchPackage, "Patch") && allowed[call][t]) {
				return true
			}
			pass.Reportf(call.Pos(), "%s writes to a reconciled type %s; "+
				"mutate it in memory and persist only with the deferred patch.Helper.Patch in Reconcile",
				fn.Name(), types.TypeString(t, (*types.Package).Name))
			return true
		})
	}
	return nil, nil
}

func skipFile(pass *analysis.Pass, file *ast.File) bool {
	return ast.IsGenerated(file) || strings.HasSuffix(pass.Fset.PositionFor(file.Pos(), false).Filename, "_test.go")
}

func reconciledTypes(pass *analysis.Pass, decl *ast.FuncDecl) map[types.Type]bool {
	reconciled := make(map[types.Type]bool)
	ast.Inspect(decl.Body, func(node ast.Node) bool {
		if _, ok := node.(*ast.FuncLit); ok {
			return false
		}
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) < 3 {
			return true
		}
		fn, _ := typeutil.Callee(pass.TypesInfo, call).(*types.Func)
		if !functionIs(fn, clientPackage, "Get") {
			return true
		}
		args := callArgs(pass.TypesInfo, call)
		if len(args) < 3 {
			return true
		}
		key, ok := ast.Unparen(args[1]).(*ast.SelectorExpr)
		if !ok || key.Sel.Name != "NamespacedName" {
			return true
		}
		named, ok := objectType(pass.TypesInfo.TypeOf(key.X)).(*types.Named)
		if ok && named.Obj().Pkg() != nil && named.Obj().Name() == "Request" &&
			named.Obj().Pkg().Path() == "sigs.k8s.io/controller-runtime/pkg/reconcile" {
			reconciled[objectType(pass.TypesInfo.TypeOf(args[2]))] = true
		}
		return true
	})
	return reconciled
}

func callArgs(info *types.Info, call *ast.CallExpr) []ast.Expr {
	if selector, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr); ok {
		if selection := info.Selections[selector]; selection != nil && selection.Kind() == types.MethodExpr {
			return call.Args[1:]
		}
	}
	return call.Args
}

func objectType(t types.Type) types.Type {
	if t == nil {
		return nil
	}
	t = types.Unalias(t)
	if pointer, ok := t.(*types.Pointer); ok {
		return types.Unalias(pointer.Elem())
	}
	return t
}

func functionIs(fn *types.Func, path, name string) bool {
	return fn != nil && fn.Pkg() != nil && fn.Pkg().Path() == path && fn.Name() == name
}

func deferredCalls(decl *ast.FuncDecl) map[*ast.CallExpr]bool {
	allowed := make(map[*ast.CallExpr]bool)
	ast.Inspect(decl.Body, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.FuncLit:
			return false
		case *ast.DeferStmt:
			if closure, ok := ast.Unparen(node.Call.Fun).(*ast.FuncLit); ok {
				ast.Inspect(closure.Body, func(child ast.Node) bool {
					switch child := child.(type) {
					case *ast.FuncLit, *ast.GoStmt:
						return false
					case *ast.CallExpr:
						allowed[child] = true
					}
					return true
				})
			} else {
				allowed[node.Call] = true
			}
			return false
		}
		return true
	})
	return allowed
}

func writeObjectIndex(fn *types.Func) int {
	if fn == nil || fn.Pkg() == nil {
		return -1
	}
	switch fn.Pkg().Path() {
	case clientPackage:
		switch fn.Name() {
		case "Create", "Update", "Patch", "Delete", "DeleteAllOf", "Apply":
			return 1
		}
	case patchPackage:
		if fn.Name() == "Patch" {
			return 1
		}
	case utilPackage:
		if fn.Name() == "CreateOrUpdate" || fn.Name() == "CreateOrPatch" {
			return 2
		}
	}
	return -1
}
