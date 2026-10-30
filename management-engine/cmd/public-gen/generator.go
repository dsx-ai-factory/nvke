// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"golang.org/x/tools/go/packages"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

type resourceModel struct {
	Source   string
	Public   string
	Path     string
	Metadata []string

	Fields                                                            []metadataField
	Spec, Status, SpecTag, StatusTag, Group, Version, Resource, Scope string
	NeedsSourceValidation, NeedsPublicValidation, HasStatus           bool
}

type metadataField struct{ Name, Type, Tag string }

type model struct {
	Resources         []resourceModel
	SourceImport      string
	UsesTime, UsesUID bool
}

const (
	specField         = "Spec"
	publicNameKey     = "name"
	publicPathKey     = "path"
	publicMetadataKey = "metadata"
)

// readModel limits projection to approved roots and metadata. Nested spec/status types are reused.
func readModel(root string) (model, error) {
	var result model
	declarations, validations, resources, err := readSource(root)
	if err != nil {
		return result, err
	}
	result.Resources = resources
	crds, err := readCRDs(root)
	if err != nil {
		return result, err
	}
	seen := map[string]bool{}
	allowed := map[string]metadataField{
		publicNameKey:       {"Name", "string", "name,omitempty"},
		"creationTimestamp": {"CreationTimestamp", "metav1.Time", "creationTimestamp,omitempty,omitzero"},
		"deletionTimestamp": {"DeletionTimestamp", "*metav1.Time", "deletionTimestamp,omitempty"},
		"uid":               {"UID", "types.UID", "uid,omitempty"},
	}
	for i := range result.Resources {
		r := &result.Resources[i]
		if !token.IsIdentifier(r.Public) || !ast.IsExported(r.Public) || seen[r.Public] || seen[r.Path] ||
			!strings.HasPrefix(r.Path, "/") || path.Clean(r.Path) != r.Path ||
			r.Path == "/" || declarations[r.Public] != nil {
			return result, fmt.Errorf("invalid or duplicate public name/path: %q %q", r.Public, r.Path)
		}
		seen[r.Public], seen[r.Path] = true, true
		structure, ok := declarations[r.Source].(*ast.StructType)
		if !ok {
			return result, fmt.Errorf("source %s must be a declared struct", r.Source)
		}
		if err := r.readFields(structure); err != nil {
			return result, err
		}
		for _, key := range r.Metadata {
			field, ok := allowed[key]
			if !ok || seen[r.Public+"."+key] {
				return result, fmt.Errorf("%s: unsupported or duplicate metadata %q", r.Public, key)
			}
			seen[r.Public+"."+key] = true
			r.Fields = append(r.Fields, field)
			result.UsesTime = result.UsesTime || strings.Contains(field.Type, "metav1.")
			result.UsesUID = result.UsesUID || key == "uid"
		}
		r.NeedsSourceValidation, r.NeedsPublicValidation = !validations[r.Source], !validations[r.Public]
		crd, ok := crds[r.Source]
		if !ok {
			return result, fmt.Errorf("no CRD for source %s", r.Source)
		}
		r.Group, r.Resource, r.Scope = crd.Spec.Group, crd.Spec.Names.Plural, string(crd.Spec.Scope)
		for _, version := range crd.Spec.Versions {
			if version.Storage {
				r.Version = version.Name
				r.HasStatus = version.Subresources != nil && version.Subresources.Status != nil
			}
		}
		if r.Version == "" || (r.HasStatus && r.Status == "") {
			return result, fmt.Errorf("%s: storage version/status does not match source", r.Source)
		}
	}
	return result, nil
}

// readFields reuses named spec/status types and their existing JSON tags without recursive copying.
func (r *resourceModel) readFields(structure *ast.StructType) error {
	for _, field := range structure.Fields.List {
		if len(field.Names) != 1 || (field.Names[0].Name != specField && field.Names[0].Name != "Status") {
			continue
		}
		name, ok := field.Type.(*ast.Ident)
		if !ok || field.Tag == nil {
			return fmt.Errorf("%s.%s must use a named local type and JSON tag", r.Source, field.Names[0].Name)
		}
		if field.Names[0].Name == specField {
			r.Spec, r.SpecTag = name.Name, field.Tag.Value
		} else {
			r.Status, r.StatusTag = name.Name, field.Tag.Value
		}
	}
	if r.Spec == "" {
		return fmt.Errorf("source %s has no Spec field", r.Source)
	}
	return nil
}

// readSource ignores previous output so removed or renamed input types can regenerate.
func readSource(root string) (map[string]ast.Expr, map[string]bool, []resourceModel, error) {
	declarations, validations := map[string]ast.Expr{}, map[string]bool{}
	var resources []resourceModel
	files, err := filepath.Glob(filepath.Join(root, "apis/v1/*.go"))
	if err != nil {
		return nil, nil, nil, err
	}
	for _, filename := range files {
		if filepath.Base(filename) == filepath.Base(publicOutput) || strings.HasSuffix(filename, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), filename, nil, parser.ParseComments)
		if err != nil {
			return nil, nil, nil, err
		}
		for _, declaration := range file.Decls {
			switch declaration := declaration.(type) {
			case *ast.GenDecl:
				if declaration.Tok != token.TYPE {
					continue
				}
				for _, spec := range declaration.Specs {
					if spec, ok := spec.(*ast.TypeSpec); ok {
						declarations[spec.Name.Name] = spec.Type
						if len(declaration.Specs) > 1 {
							for line := range strings.SplitSeq(declaration.Doc.Text(), "\n") {
								if strings.HasPrefix(strings.TrimSpace(line), "+nvke:public") {
									return nil, nil, nil, fmt.Errorf(
										"%s: public markers on grouped declarations must attach to individual types", filename)
								}
							}
						}
						r, marked, err := readPublicMarkers(spec.Name.Name, declaration.Doc, spec.Doc)
						if err != nil {
							return nil, nil, nil, fmt.Errorf("%s: %w", filename, err)
						}
						if marked {
							resources = append(resources, r)
						}
					}
				}
			case *ast.FuncDecl:
				if declaration.Name.Name == "Validate" && declaration.Recv != nil {
					name, ok := declaration.Recv.List[0].Type.(*ast.Ident)
					if !ok {
						return nil, nil, nil, fmt.Errorf("%s: Validate must have a value receiver", filename)
					}
					validations[name.Name] = true
				}
			}
		}
	}
	// Public names give stable output regardless of source filenames or declaration order.
	slices.SortFunc(resources, func(a, b resourceModel) int { return strings.Compare(a.Public, b.Public) })
	return declarations, validations, resources, nil
}

// readPublicMarkers makes a complete set of type-local markers the explicit opt-in.
func readPublicMarkers(source string, groups ...*ast.CommentGroup) (resourceModel, bool, error) {
	r := resourceModel{Source: source}
	values := map[string]string{}
	for _, group := range groups {
		for line := range strings.SplitSeq(group.Text(), "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "+nvke:public") {
				continue
			}
			marker, ok := strings.CutPrefix(line, "+nvke:public:")
			key, value, hasValue := strings.Cut(marker, "=")
			value = strings.TrimSpace(value)
			if !ok || !hasValue || value == "" {
				return r, false, fmt.Errorf("%s: invalid public marker %q", source, line)
			}
			if key != publicNameKey && key != publicPathKey && key != publicMetadataKey {
				return r, false, fmt.Errorf("%s: unknown public marker %q", source, key)
			}
			if _, exists := values[key]; exists {
				return r, false, fmt.Errorf("%s: duplicate public marker %q", source, key)
			}
			values[key] = value
		}
	}
	if len(values) == 0 {
		return r, false, nil
	}
	for _, key := range []string{publicNameKey, publicPathKey, publicMetadataKey} {
		if _, exists := values[key]; !exists {
			return r, false, fmt.Errorf("%s: missing public marker %q", source, key)
		}
	}
	r.Public, r.Path = values[publicNameKey], values[publicPathKey]
	for field := range strings.SplitSeq(values[publicMetadataKey], ",") {
		r.Metadata = append(r.Metadata, strings.TrimSpace(field))
	}
	return r, true, nil
}

// readCRDs derives storage identity and scope from existing generated CRDs.
func readCRDs(root string) (map[string]apiextensionsv1.CustomResourceDefinition, error) {
	result := map[string]apiextensionsv1.CustomResourceDefinition{}
	files, err := filepath.Glob(filepath.Join(root, "config/crd/bases/*.yaml"))
	if err != nil {
		return nil, err
	}
	for _, filename := range files {
		data, err := os.ReadFile(filename)
		if err != nil {
			return nil, err
		}
		var crd apiextensionsv1.CustomResourceDefinition
		if err := yaml.Unmarshal(data, &crd); err != nil {
			return nil, fmt.Errorf("read %s: %w", filename, err)
		}
		result[crd.Spec.Names.Kind] = crd
	}
	return result, nil
}

// checkTypes validates the candidate alongside handwritten methods before replacing output.
func checkTypes(root string, candidate []byte, resources []resourceModel) (string, error) {
	filename, err := filepath.Abs(filepath.Join(root, publicOutput))
	if err != nil {
		return "", err
	}
	for _, resource := range resources {
		for _, pair := range [][2]string{{resource.Public, resource.Source}, {resource.Source, resource.Public}} {
			candidate = fmt.Appendf(candidate,
				"\nvar _ interface { Validate() error; Convert() (%s, error) } = %s{}\n", pair[1], pair[0])
		}
	}
	loaded, err := packages.Load(&packages.Config{
		Dir: root, Mode: packages.NeedName | packages.NeedTypes | packages.NeedImports | packages.NeedDeps,
		Overlay: map[string][]byte{filename: candidate},
	}, "./apis/v1")
	if err != nil {
		return "", err
	}
	if len(loaded) != 1 {
		return "", fmt.Errorf("expected one source package, found %d", len(loaded))
	}
	if len(loaded[0].Errors) != 0 {
		return "", fmt.Errorf("source package: %v", loaded[0].Errors)
	}
	for _, resource := range resources {
		object := loaded[0].Types.Scope().Lookup(resource.Source)
		if object == nil {
			return "", fmt.Errorf("source %s did not resolve", resource.Source)
		}
		named, ok := object.Type().(*types.Named)
		if !ok {
			return "", fmt.Errorf("source %s is not a named type", resource.Source)
		}
		structure, ok := named.Underlying().(*types.Struct)
		if !ok {
			return "", fmt.Errorf("source %s is not a struct", resource.Source)
		}
		for i := range structure.NumFields() {
			field := structure.Field(i)
			if field.Name() == specField || field.Name() == "Status" {
				if _, ok := field.Type().(*types.Named); !ok {
					return "", fmt.Errorf("%s.%s is not a named type", resource.Source, field.Name())
				}
				name, _, _ := strings.Cut(reflect.StructTag(structure.Tag(i)).Get("json"), ",")
				if name != strings.ToLower(field.Name()) {
					return "", fmt.Errorf("%s.%s must keep its JSON field name", resource.Source, field.Name())
				}
			}
		}
	}
	return loaded[0].PkgPath, nil
}
