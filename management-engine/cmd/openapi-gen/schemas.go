// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3gen"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	v1 "github.com/dsx-ai-factory/nvke/management-engine/apis/v1"
	"github.com/dsx-ai-factory/nvke/management-engine/pkg/projection"
	"github.com/dsx-ai-factory/nvke/management-engine/pkg/resource"
)

// schemaBuilder reuses controller-gen validation and reflects only public envelopes and derived views.
type schemaBuilder struct {
	sourceSchemas  map[reflect.Type]*openapi3.Schema
	catalogSources map[reflect.Type]reflect.Type
}

func newSchemaBuilder(root string, registrations []resource.Registration) (*schemaBuilder, error) {
	files, err := filepath.Glob(filepath.Join(root, "config/crd/bases/*.yaml"))
	if err != nil {
		return nil, err
	}
	crds := map[string]apiextensionsv1.CustomResourceDefinition{}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		var crd apiextensionsv1.CustomResourceDefinition
		if err := yaml.Unmarshal(data, &crd); err != nil {
			return nil, fmt.Errorf("read %s: %w", file, err)
		}
		crds[crd.Spec.Group+"/"+crd.Spec.Names.Kind] = crd
	}
	builder := &schemaBuilder{
		sourceSchemas: map[reflect.Type]*openapi3.Schema{}, catalogSources: v1.CatalogSchemaSources(),
	}
	for _, registration := range registrations {
		description := registration.Describe()
		if len(description.Routes) == 0 {
			continue
		}
		crd := crds[description.Info.GVK.Group+"/"+description.Info.GVK.Kind]
		var source *openapi3.Schema
		for _, version := range crd.Spec.Versions {
			if version.Name == description.Info.GVK.Version && version.Schema != nil {
				source, err = projection.JSON[*openapi3.Schema](version.Schema.OpenAPIV3Schema)
				if err != nil {
					return nil, err
				}
			}
		}
		if source == nil {
			return nil, fmt.Errorf("no CRD schema for %s, run make generate", description.Info.GVK)
		}
		// Kubernetes supplies metadata separately. Only spec/status schemas describe our reusable Go types.
		for _, name := range []string{"Spec", "Status"} {
			if field, ok := description.SourceType.FieldByName(name); ok {
				if err := builder.index(field.Type, source.Properties[strings.ToLower(name)]); err != nil {
					return nil, err
				}
			}
		}
	}
	return builder, nil
}

// index associates named source types with their existing validation, including joined catalog sources.
func (builder *schemaBuilder) index(goType reflect.Type, sourceRef *openapi3.SchemaRef) error {
	if sourceRef == nil || sourceRef.Value == nil {
		return fmt.Errorf("missing CRD schema for %s, run make generate", goType)
	}
	if goType.Kind() == reflect.Pointer {
		goType = goType.Elem()
	}
	schema := sourceRef.Value
	if goType == reflect.TypeFor[v1.ResourceName]() {
		// Resource names share syntax, while field-specific annotations stay on their original properties.
		builder.sourceSchemas[goType] = &openapi3.Schema{
			Type: schema.Type, MinLength: schema.MinLength, MaxLength: schema.MaxLength, Pattern: schema.Pattern,
		}
	} else if goType.Name() != "" &&
		slices.Contains([]reflect.Kind{reflect.Struct, reflect.Map, reflect.Slice, reflect.Array}, goType.Kind()) {
		builder.sourceSchemas[goType] = schema
	}
	if goType.Kind() == reflect.Struct {
		if goType == reflect.TypeFor[metav1.Time]() || goType == reflect.TypeFor[metav1.Duration]() {
			return nil
		}
		schema.WithoutAdditionalProperties()
		for field := range goType.Fields() {
			name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
			if name == "" || name == "-" || !field.IsExported() {
				continue
			}
			if err := builder.index(field.Type, schema.Properties[name]); err != nil {
				return fmt.Errorf("%s.%s: %w", goType, field.Name, err)
			}
		}
	}
	if goType.Kind() == reflect.Slice || goType.Kind() == reflect.Array {
		return builder.index(goType.Elem(), schema.Items)
	}
	if goType.Kind() == reflect.Map {
		return builder.index(goType.Elem(), schema.AdditionalProperties.Schema)
	}
	return nil
}

func (builder *schemaBuilder) schema(goType reflect.Type) (*openapi3.SchemaRef, error) {
	return openapi3gen.NewSchemaRefForValue(reflect.New(goType).Elem().Interface(), nil,
		openapi3gen.SchemaCustomizer(builder.customize))
}

func (builder *schemaBuilder) customize(
	_ string,
	goType reflect.Type,
	fieldTag reflect.StructTag,
	schema *openapi3.Schema,
) error {
	if err := builder.applyTypeSchema(goType, schema); err != nil {
		return err
	}
	// A hand-written public field has no CRD schema, and reflection cannot read its doc comment.
	if description := fieldTag.Get("description"); description != "" {
		schema.Description = description
		// The generator hands a slice field's tag to its element type as well.
		if schema.Items != nil && schema.Items.Value != nil && schema.Items.Value.Description == description {
			schema.Items.Value.Description = ""
		}
	}
	return nil
}

// applyTypeSchema combines CRD validation with fields declared by public types.
func (builder *schemaBuilder) applyTypeSchema(goType reflect.Type, schema *openapi3.Schema) error {
	if goType.Kind() == reflect.Pointer {
		goType = goType.Elem()
	}
	if goType == reflect.TypeFor[metav1.Time]() {
		*schema = *openapi3.NewDateTimeSchema()
		return nil
	}
	if goType == reflect.TypeFor[metav1.Duration]() {
		*schema = *openapi3.NewStringSchema()
		return nil
	}
	if source := builder.sourceSchemas[goType]; source != nil {
		clonedSchema, err := projection.JSON[*openapi3.Schema](source)
		if err != nil {
			return err
		}
		*schema = *clonedSchema
		return nil
	}
	if goType.Kind() == reflect.Struct {
		schema.Required = requiredFields(goType)
		schema.WithoutAdditionalProperties()
		// Reflection flattens an inlined projection into its parent without visiting it as a type,
		// so its scalar fields would otherwise lose the constraints and descriptions of their source.
		for field := range goType.Fields() {
			name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
			if _, projected := builder.catalogSources[field.Type]; !projected || !field.Anonymous || name != "" {
				continue
			}
			inlined, err := builder.schema(field.Type)
			if err != nil {
				return fmt.Errorf("%s.%s: %w", goType, field.Name, err)
			}
			maps.Copy(schema.Properties, inlined.Value.Properties)
		}
	}
	if original, ok := builder.catalogSources[goType]; ok {
		projected, err := projectSchema(builder.sourceSchemas[original], schema)
		if err != nil {
			return fmt.Errorf("catalog schema %s: %w", goType, err)
		}
		*schema = *projected
	}
	return nil
}

// requiredFields describes emitted fields in public envelopes and derived catalog responses.
// Writable specs instead retain controller-gen's required list, which accounts for +optional and defaults.
func requiredFields(goType reflect.Type) []string {
	var required []string
	for field := range goType.Fields() {
		name, options, _ := strings.Cut(field.Tag.Get("json"), ",")
		switch {
		case field.Anonymous && name == "" && field.Type.Kind() == reflect.Struct:
			required = append(required, requiredFields(field.Type)...)
		case name != "" && name != "-" && !strings.Contains(options, "omitempty") && !strings.Contains(options, "omitzero"):
			required = append(required, name)
		}
	}
	slices.Sort(required)
	return required
}

// describeConstraints documents public rules after projection and keeps portable list validation.
func describeConstraints(path string, ref *openapi3.SchemaRef) error {
	schema := ref.Value
	if len(schema.Extensions) == 0 {
		return nil
	}
	constraints, err := projection.JSON[struct {
		Validations []apiextensionsv1.ValidationRule `json:"x-kubernetes-validations"`
		ListType    string                           `json:"x-kubernetes-list-type"`
		ListKeys    []string                         `json:"x-kubernetes-list-map-keys"`
	}](schema.Extensions)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	var messages []string
	for _, validation := range constraints.Validations {
		message := strings.TrimSpace(validation.Message)
		if message == "" {
			return fmt.Errorf("%s: CEL rule %q needs a message for public documentation", path, validation.Rule)
		}
		messages = append(messages, message)
	}
	delete(schema.Extensions, "x-kubernetes-validations")
	if constraints.ListType == "set" && schema.Items != nil {
		itemType := schema.Items.Value.Type
		if itemType.Is("string") || itemType.Is("integer") || itemType.Is("number") || itemType.Is("boolean") {
			schema.UniqueItems = true
		}
	}
	if len(constraints.ListKeys) != 0 {
		messages = append(messages, "Entries must have unique combinations of these keys: "+
			strings.Join(constraints.ListKeys, ", ")+".")
	}
	if len(messages) != 0 {
		schema.Description = strings.TrimSpace(schema.Description + "\n\n- " + strings.Join(messages, "\n- "))
	}
	return nil
}

// allowOptionalNulls accounts for Kubernetes pruning null object fields before defaulting and validation.
// Required fields and array items retain their declared nullability.
func allowOptionalNulls(schema *openapi3.Schema) {
	for name, field := range schema.Properties {
		if !slices.Contains(schema.Required, name) || field.Value.Default != nil {
			field.Value.Nullable = true
		}
		allowOptionalNulls(field.Value)
	}
	if schema.Items != nil {
		allowOptionalNulls(schema.Items.Value)
	}
	if schema.AdditionalProperties.Schema != nil {
		schema.AdditionalProperties.Schema.Value.Nullable = true
		allowOptionalNulls(schema.AdditionalProperties.Schema.Value)
	}
}

// projectSchema keeps selected field constraints, while dropping Kubernetes rules that may reference omitted fields.
func projectSchema(source, publicShape *openapi3.Schema) (*openapi3.Schema, error) {
	if source == nil {
		return nil, fmt.Errorf("source schema was not indexed")
	}
	result := *source
	result.Extensions = nil
	result.Default = nil
	result.Required = publicShape.Required
	if publicShape.Properties != nil {
		result.Properties = openapi3.Schemas{}
		result.WithoutAdditionalProperties()
		for name, field := range publicShape.Properties {
			original := source.Properties[name]
			if original == nil {
				return nil, fmt.Errorf("no source schema for field %s", name)
			}
			child, err := projectSchema(original.Value, field.Value)
			if err != nil {
				return nil, err
			}
			result.Properties[name] = &openapi3.SchemaRef{Value: child}
		}
	}
	if publicShape.Items != nil {
		if source.Items == nil {
			return nil, fmt.Errorf("source has no array item schema")
		}
		items, err := projectSchema(source.Items.Value, publicShape.Items.Value)
		if err != nil {
			return nil, err
		}
		result.Items = &openapi3.SchemaRef{Value: items}
	}
	if publicShape.AdditionalProperties.Schema != nil {
		if source.AdditionalProperties.Schema == nil {
			return nil, fmt.Errorf("source has no map value schema")
		}
		value, err := projectSchema(source.AdditionalProperties.Schema.Value, publicShape.AdditionalProperties.Schema.Value)
		if err != nil {
			return nil, err
		}
		result.AdditionalProperties.Schema = &openapi3.SchemaRef{Value: value}
	}
	return &result, nil
}
