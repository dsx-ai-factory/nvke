// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"go/ast"
	"go/types"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

type catalogDeclaration struct{ Name, Body string }

// catalogBuilder replaces composite source types with projections so unmarked fields cannot pass through.
type catalogBuilder struct {
	source   map[string]ast.Expr
	output   map[string]string
	active   map[string]bool
	usesTime bool
}

// readCatalogTypes builds projections from marked roots with explicit field selection at every struct.
// Sorting declarations prevents map iteration order from changing generated files.
func readCatalogTypes(source map[string]ast.Expr, roots []string) ([]catalogDeclaration, bool, error) {
	b := catalogBuilder{source: source, output: map[string]string{}, active: map[string]bool{}}
	for _, root := range roots {
		if _, ok := source[root].(*ast.StructType); !ok {
			return nil, false, fmt.Errorf("catalog source %s must be a declared struct", root)
		}
		if _, err := b.project(ast.NewIdent(root)); err != nil {
			return nil, false, err
		}
	}
	result := make([]catalogDeclaration, 0, len(b.output))
	for name, body := range b.output {
		result = append(result, catalogDeclaration{name, body})
	}
	slices.SortFunc(result, func(a, b catalogDeclaration) int { return strings.Compare(a.Name, b.Name) })
	return result, b.usesTime, nil
}

// project preserves supported containers while replacing local composite types with catalog projections.
func (b *catalogBuilder) project(expr ast.Expr) (string, error) {
	switch value := expr.(type) {
	case *ast.Ident:
		if b.scalar(value) {
			return value.Name, nil
		}
		if b.source[value.Name] != nil {
			return b.named(value.Name)
		}
	case *ast.StarExpr:
		inner, err := b.project(value.X)
		return "*" + inner, err
	case *ast.ArrayType:
		if value.Len != nil {
			break
		}
		inner, err := b.project(value.Elt)
		return "[]" + inner, err
	case *ast.MapType:
		key, ok := value.Key.(*ast.Ident)
		if !ok || key.Name != "string" {
			break
		}
		inner, err := b.project(value.Value)
		return "map[string]" + inner, err
	case *ast.SelectorExpr:
		// Time has a scalar JSON encoding. Never pass arbitrary external structs through.
		pkg, ok := value.X.(*ast.Ident)
		if ok && pkg.Name == "metav1" && value.Sel.Name == "Time" {
			b.usesTime = true
			return "metav1.Time", nil
		}
	case *ast.StructType:
		return b.structure(value)
	}
	return "", fmt.Errorf("unsupported catalog field type %T; select scalar leaves or local structs", expr)
}

// scalar recognizes local types backed by primitives so their declared names can be reused without filtering.
func (b *catalogBuilder) scalar(expr ast.Expr) bool {
	visited := map[string]bool{}
	for {
		name, ok := expr.(*ast.Ident)
		if !ok || visited[name.Name] {
			return false
		}
		visited[name.Name] = true
		if underlying := b.source[name.Name]; underlying != nil {
			expr = underlying
			continue
		}
		builtin := types.Universe.Lookup(name.Name)
		if builtin == nil {
			return false
		}
		basic, ok := builtin.Type().(*types.Basic)
		return ok && basic.Info()&(types.IsBoolean|types.IsInteger|types.IsFloat|types.IsString) != 0
	}
}

// named reuses completed projections and rejects cycles or conflicts with source declarations.
func (b *catalogBuilder) named(sourceName string) (string, error) {
	name := "Catalog" + sourceName
	if b.active[sourceName] {
		return "", fmt.Errorf("recursive catalog type %s is unsupported", sourceName)
	}
	if b.output[name] != "" {
		return name, nil
	}
	if b.source[name] != nil {
		return "", fmt.Errorf("catalog type %s conflicts with source", name)
	}
	b.active[sourceName] = true
	body, err := b.project(b.source[sourceName])
	delete(b.active, sourceName)
	if err != nil {
		return "", fmt.Errorf("%s: %w", sourceName, err)
	}
	b.output[name] = body
	return name, nil
}

// structure includes only marked fields and preserves their JSON tags to keep source and view names aligned.
// Nested structs must select their own fields.
func (b *catalogBuilder) structure(value *ast.StructType) (string, error) {
	var fields []string
	for _, field := range value.Fields.List {
		include, err := readCatalogMarker("include", field.Doc, field.Comment)
		if err != nil {
			return "", err
		}
		if !include {
			continue
		}
		if len(field.Names) != 1 || !ast.IsExported(field.Names[0].Name) || field.Tag == nil {
			return "", fmt.Errorf("included field requires one exported name and an explicit JSON tag")
		}
		tag, err := strconv.Unquote(field.Tag.Value)
		if err != nil {
			return "", err
		}
		jsonName, _, _ := strings.Cut(reflect.StructTag(tag).Get("json"), ",")
		if jsonName == "" || jsonName == "-" || jsonName == "hidden" {
			return "", fmt.Errorf("catalog field %s has forbidden JSON name %q", field.Names[0].Name, jsonName)
		}
		fieldType, err := b.project(field.Type)
		if err != nil {
			return "", fmt.Errorf("%s: %w", field.Names[0].Name, err)
		}
		fields = append(fields, fmt.Sprintf("%s %s %s", field.Names[0].Name, fieldType, field.Tag.Value))
	}
	if len(fields) == 0 {
		return "", fmt.Errorf("included struct has no included fields")
	}
	return "struct {\n" + strings.Join(fields, "\n") + "\n}", nil
}

// readCatalogMarker rejects unsupported catalog directives instead of treating them as absent.
func readCatalogMarker(kind string, groups ...*ast.CommentGroup) (bool, error) {
	marked := false
	for _, group := range groups {
		for line := range strings.SplitSeq(group.Text(), "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "+nvke:catalog") {
				continue
			}
			if line != "+nvke:catalog:"+kind || marked {
				return false, fmt.Errorf("invalid or duplicate catalog marker %q; use +nvke:catalog:%s", line, kind)
			}
			marked = true
		}
	}
	return marked, nil
}
