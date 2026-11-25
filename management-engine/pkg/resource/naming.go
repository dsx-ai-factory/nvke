// SPDX-License-Identifier: Apache-2.0

// This file maps declared HTTP fields between snake_case and canonical JSON names.

package resource

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"github.com/gobuffalo/flect"
)

// Naming selects one spelling for fields, query parameters and error codes on an HTTP surface.
type Naming uint8

const (
	// SnakeCase is the default HTTP naming format.
	SnakeCase Naming = iota
	// CamelCase retains the canonical JSON field spelling.
	CamelCase
)

// Validate rejects unsupported naming formats before a surface is mounted.
func (n Naming) Validate() error {
	if n != SnakeCase && n != CamelCase {
		return fmt.Errorf("unsupported HTTP naming format %d", n)
	}
	return nil
}

// FieldName formats a canonical declared field, query parameter or error code.
// Dictionary keys, enum values and opaque tokens must not pass through this function.
func (n Naming) FieldName(canonical string) string {
	if n == CamelCase {
		return canonical
	}
	return flect.Underscore(canonical)
}

type jsonField struct {
	canonical string
	wire      string
	shape     *jsonShape
}

type jsonShape struct {
	kind      reflect.Kind
	canonical map[string]jsonField
	wire      map[string]jsonField
	element   *jsonShape
}

func newJSONShape(t reflect.Type, naming Naming) (*jsonShape, error) {
	if err := naming.Validate(); err != nil {
		return nil, err
	}
	if t == nil {
		return nil, fmt.Errorf("a declared public JSON type is required")
	}
	return buildJSONShape(t, naming, map[reflect.Type]*jsonShape{})
}

// buildJSONShape treats custom encodings, including timestamps, as opaque JSON values.
func buildJSONShape(t reflect.Type, naming Naming, seen map[reflect.Type]*jsonShape) (*jsonShape, error) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if shape, ok := seen[t]; ok {
		return shape, nil
	}
	shape := &jsonShape{}
	seen[t] = shape
	marshaler := reflect.TypeFor[json.Marshaler]()
	if t.Implements(marshaler) {
		return shape, nil
	}
	// Pointer-only encoders can produce different JSON for items and addressable list elements.
	if reflect.PointerTo(t).Implements(marshaler) {
		return nil, fmt.Errorf("public JSON type %s has a pointer-only MarshalJSON method", t)
	}
	shape.kind = t.Kind()
	switch t.Kind() {
	case reflect.Struct:
		shape.canonical, shape.wire = map[string]jsonField{}, map[string]jsonField{}
		if err := shape.addFields(t, naming, seen); err != nil {
			return nil, err
		}
	case reflect.Array, reflect.Slice, reflect.Map:
		if t.Kind() == reflect.Map && t.Key().Kind() != reflect.String {
			return nil, fmt.Errorf("public JSON dictionary %s must have string keys", t)
		}
		var err error
		shape.element, err = buildJSONShape(t.Elem(), naming, seen)
		if err != nil {
			return nil, err
		}
	case reflect.Interface, reflect.Func, reflect.Chan, reflect.UnsafePointer, reflect.Complex64, reflect.Complex128:
		return nil, fmt.Errorf("public JSON type %s has no declared field shape", t)
	}
	return shape, nil
}

func (s *jsonShape) addFields(t reflect.Type, naming Naming, seen map[reflect.Type]*jsonShape) error {
	for field := range t.Fields() {
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "-" || !field.IsExported() && !field.Anonymous {
			continue
		}
		child, err := buildJSONShape(field.Type, naming, seen)
		if err != nil {
			return err
		}
		if field.Anonymous && name == "" && child.kind == reflect.Struct {
			for _, inline := range child.canonical {
				if err := s.addField(inline); err != nil {
					return err
				}
			}
			continue
		}
		if !field.IsExported() {
			continue
		}
		if name == "" {
			name = field.Name
		}
		if err := s.addField(jsonField{canonical: name, wire: naming.FieldName(name), shape: child}); err != nil {
			return err
		}
	}
	return nil
}

func (s *jsonShape) addField(field jsonField) error {
	if previous, exists := s.wire[field.wire]; exists {
		return fmt.Errorf("public JSON fields %q and %q collide as %q", previous.canonical, field.canonical, field.wire)
	}
	s.canonical[field.canonical], s.wire[field.wire] = field, field
	return nil
}

// rename changes declared fields in a freshly decoded tree in place.
// Nulls and wrong JSON types reach the typed decoder unchanged.
func (s *jsonShape) rename(value any, inbound bool) error {
	switch value := value.(type) {
	case map[string]any:
		if s.kind != reflect.Struct && s.kind != reflect.Map {
			return nil
		}
		// Insert changed keys after iteration so range cannot visit the same field twice.
		type renamedField struct {
			name  string
			value any
		}
		var renamed []renamedField
		for key, entry := range value {
			child, name := s.element, key
			if s.kind == reflect.Struct {
				field, ok := s.canonical[key]
				if inbound {
					field, ok = s.wire[key]
				}
				if !ok {
					return fmt.Errorf("unknown public JSON field %q", key)
				}
				child, name = field.shape, field.wire
				if inbound {
					name = field.canonical
				}
			}
			if err := child.rename(entry, inbound); err != nil {
				return err
			}
			if name == key {
				continue
			}
			if renamed == nil {
				renamed = make([]renamedField, 0, len(value))
			}
			delete(value, key)
			renamed = append(renamed, renamedField{name: name, value: entry})
		}
		for _, field := range renamed {
			value[field.name] = field.value
		}
	case []any:
		if s.kind != reflect.Array && s.kind != reflect.Slice {
			return nil
		}
		for _, entry := range value {
			if err := s.element.rename(entry, inbound); err != nil {
				return err
			}
		}
	}
	return nil
}

// translate renames declared fields while keeping numeric literals unchanged until typed validation.
func (s *jsonShape) translate(data []byte, inbound bool) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if err := s.rename(value, inbound); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

// path retains dictionary keys and array indices while walking declared fields.
func (s *jsonShape) path(parts []string) ([]string, bool) {
	if len(parts) == 0 {
		return nil, true
	}
	if s == nil {
		return nil, false
	}
	name, child := parts[0], s.element
	switch s.kind {
	case reflect.Struct:
		field, ok := s.canonical[name]
		if !ok {
			return nil, false
		}
		name, child = field.wire, field.shape
	case reflect.Array, reflect.Slice:
		if index, err := strconv.Atoi(name); err != nil || index < 0 {
			return nil, false
		}
	case reflect.Map:
	default:
		return nil, false
	}
	remaining, ok := child.path(parts[1:])
	return append([]string{name}, remaining...), ok
}

func (s *jsonShape) pointer(canonical string) string {
	parts := strings.Split(strings.TrimPrefix(canonical, "/"), "/")
	for index := range parts {
		parts[index] = strings.NewReplacer("~1", "/", "~0", "~").Replace(parts[index])
	}
	translated, ok := s.path(parts)
	if !ok {
		return ""
	}
	for index := range translated {
		translated[index] = strings.NewReplacer("~", "~0", "/", "~1").Replace(translated[index])
	}
	return "/" + strings.Join(translated, "/")
}
