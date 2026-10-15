// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Compilation combines explicit exposure with stored schema rules to keep private fields outside public models.

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

// policy declares reviewed public exposure and service-owned create values.
type policy struct {
	// Version identifies the policy format so incompatible inputs can be rejected.
	Version string `json:"version"`
	// Fields lists reviewed paths so private schema growth cannot expand public exposure.
	Fields []field `json:"fields"`
	// Defaults supplies create values for fields omitted from public create inputs.
	Defaults []defaultValue `json:"defaults"`
}

// defaultValue supplies internal configuration without requiring a public create field.
type defaultValue struct {
	// Path identifies the stored spec property that receives the service-owned value.
	Path string `json:"path"`
	// Value is explicit because the compiler does not evaluate CRD defaults.
	Value json.RawMessage `json:"value"`
}

// field ties one stored property to its reviewed public identity and operations.
type field struct {
	// Path identifies an exact property beneath spec so exposure never relies on wildcard selection.
	Path string `json:"path"`
	// Tag preserves protobuf wire identity independently of field ordering.
	Tag int `json:"tag"`
	// Access selects read and write operations. It does not grant caller permissions.
	Access []string `json:"access"`
	// Description replaces internal wording when the public meaning needs clarification.
	Description string `json:"description,omitempty"`
	// Input narrows writable values without restricting what read responses can represent.
	Input *inputPolicy `json:"input,omitempty"`
}

// inputPolicy refines writable field requirements while retaining the stored read contract.
type inputPolicy struct {
	// MaxItems caps public input collections. Omission retains the stored schema's bound.
	MaxItems *int64 `json:"max_items,omitempty"`
	// Description explains write-specific restrictions without changing read documentation.
	Description string `json:"description,omitempty"`
}

// record captures compatibility-sensitive decisions so schema or policy drift requires review.
type record struct {
	// field keeps wire identity and operation limits in the same compatibility comparison.
	field
	// Name records the public field identity or admission context being compared.
	Name string `json:"name"`
	// Required preserves presence obligations so omission rules cannot change silently.
	Required bool `json:"required"`
	// Schema retains stored constraints so admission changes trigger compatibility review.
	Schema apiextensionsv1.JSONSchemaProps `json:"schema"`
	// Default preserves service-owned create values that affect the stored configuration.
	Default json.RawMessage `json:"default,omitempty"`
}

// node combines a compatibility record with the nesting and prose needed to render public models.
type node struct {
	// record separates compatibility data from editable presentation details.
	record
	// children limits nested rendering to explicitly reviewed descendants.
	children []*node
	// description carries public wording without making prose part of wire compatibility.
	description string
	// inputDescription explains write restrictions separately from stored read capabilities.
	inputDescription string
}

// model shares one validated exposure interpretation across generation and compatibility checks.
type model struct {
	// root retains spec-wide admission rules even when individual public fields are unchanged.
	root *node
	// nodes keeps deterministic traversal separate from recursive nesting.
	nodes []*node
	// resourceAdmission tracks whole-resource rules that can invalidate public requests.
	resourceAdmission apiextensionsv1.JSONSchemaProps
	// defaults lets generated creates satisfy private requirements without exposing new tenant inputs.
	defaults []defaultValue
	// createAdmission tracks private defaults and ancestor rules that constrain creation.
	createAdmission map[string]record
}

const (
	// Schema kinds limit generation to types with defined presence and validation behavior.
	schemaObject = "object"
	schemaString = "string"
	schemaArray  = "array"
	// Operation modes separate response, create and replacement fields without granting caller permissions.
	modeRead    = "read"
	modeCreate  = "create"
	modeReplace = "replace"
)

// pathPattern confines policy paths to explicit schema properties so wildcards cannot expand exposure.
var pathPattern = regexp.MustCompile(`^[a-z][a-zA-Z0-9]*(\.[a-z][a-zA-Z0-9]*)*$`)

// compile combines the stored schema with explicit exposure policy to build a bounded public model.
func compile(crdData, policyData []byte) (*model, error) {
	var exposure policy
	decoder := json.NewDecoder(bytes.NewReader(policyData))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&exposure); err != nil {
		return nil, fmt.Errorf("decode exposure policy: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("exposure policy must contain exactly one JSON value")
	}
	if exposure.Version != "v1" || len(exposure.Fields) == 0 {
		return nil, fmt.Errorf("expected a nonempty v1 exposure policy")
	}
	spec, resource, err := storedSchemas(crdData)
	if err != nil {
		return nil, err
	}
	root := &node{Schema: constraintsOnly(spec)}
	result := &model{root: root, resourceAdmission: constraintsOnly(resource)}
	indexed := map[string]*node{"": root}
	slices.SortFunc(exposure.Fields, func(a, b field) int { return strings.Compare(a.Path, b.Path) })
	for _, selected := range exposure.Fields {
		entry, err := compileField(selected, spec, indexed)
		if err != nil {
			return nil, err
		}
		indexed[entry.Path] = entry
		result.nodes = append(result.nodes, entry)
	}
	for _, entry := range indexed {
		slices.SortFunc(entry.children, func(a, b *node) int { return a.Tag - b.Tag })
		if entry.Schema.Type != schemaObject || entry == root {
			continue
		}
		if len(entry.children) == 0 {
			return nil, fmt.Errorf("exposed object %s has no reviewed child fields", entry.Path)
		}
		// Every writable object must reach a populated leaf through required children.
		// Child objects are checked independently; private defaults do not establish public presence.
		for _, access := range entry.Access {
			if access != modeRead && !slices.ContainsFunc(entry.children, func(child *node) bool {
				return child.Required && slices.Contains(child.Access, access)
			}) {
				return nil, fmt.Errorf("writable object %s requires a required %s child to preserve presence", entry.Path, access)
			}
		}
	}
	if err := compileDefaults(result, exposure.Defaults, spec, indexed); err != nil {
		return nil, err
	}
	return result, nil
}

// storedSchemas selects the expected served CRD version so generation cannot target an unrelated resource.
func storedSchemas(data []byte) (*apiextensionsv1.JSONSchemaProps, *apiextensionsv1.JSONSchemaProps, error) {
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.UnmarshalStrict(data, &crd); err != nil {
		return nil, nil, fmt.Errorf("decode internal CRD: %w", err)
	}
	if crd.Spec.Group != "internal.nvke.nvidia.com" || crd.Spec.Names.Kind != "NVKECluster" ||
		crd.Spec.Scope != apiextensionsv1.NamespaceScoped {
		return nil, nil, fmt.Errorf("expected namespaced internal.nvke.nvidia.com NVKECluster CRD")
	}
	for _, version := range crd.Spec.Versions {
		if version.Name == "v1" && version.Served && version.Schema != nil && version.Schema.OpenAPIV3Schema != nil {
			spec, found := version.Schema.OpenAPIV3Schema.Properties["spec"]
			if found && spec.Type == schemaObject {
				return &spec, version.Schema.OpenAPIV3Schema, nil
			}
		}
	}
	return nil, nil, fmt.Errorf("internal v1 spec schema unavailable")
}

// compileField adds one reviewed path after checking its shape, presence and wire identity.
// Requiring an explicit parent keeps nested schema growth private.
func compileField(selected field, spec *apiextensionsv1.JSONSchemaProps, indexed map[string]*node) (*node, error) {
	if !pathPattern.MatchString(selected.Path) || selected.Tag < 1 || selected.Tag > 536870911 ||
		(selected.Tag >= 19000 && selected.Tag <= 19999) {
		return nil, fmt.Errorf("invalid field path or protobuf tag: %s", selected.Path)
	}
	if _, exists := indexed[selected.Path]; exists {
		return nil, fmt.Errorf("duplicate exposure path: %s", selected.Path)
	}
	parts := strings.Split(selected.Path, ".")
	parent := indexed[strings.Join(parts[:len(parts)-1], ".")]
	if parent == nil {
		return nil, fmt.Errorf("exposed field %s requires an explicit parent policy", selected.Path)
	}
	schema, required, err := resolveSchema(spec, parts)
	if err != nil {
		return nil, err
	}
	if err := supportedSchema(schema); err != nil {
		return nil, fmt.Errorf("unsupported exposed field %s: %w", selected.Path, err)
	}
	if err := validateAccess(selected, parent); err != nil {
		return nil, err
	}
	if err := validateInputPolicy(selected, schema); err != nil {
		return nil, err
	}
	// Repeated protobuf fields cannot preserve absent versus empty input lists.
	if schema.Type == schemaArray && !required &&
		(slices.Contains(selected.Access, modeCreate) || slices.Contains(selected.Access, modeReplace)) {
		return nil, fmt.Errorf("optional writable array %s requires explicit presence semantics", selected.Path)
	}
	// An empty repeated value is indistinguishable from omission in protobuf and
	// is omitted by default ProtoJSON output, including read-only projections.
	if schema.Type == schemaArray && required && (schema.MinItems == nil || *schema.MinItems < 1) {
		return nil, fmt.Errorf("required array %s needs positive minItems to preserve presence", selected.Path)
	}
	name := snake(parts[len(parts)-1])
	for _, sibling := range parent.children {
		if sibling.Tag == selected.Tag || sibling.Name == name {
			return nil, fmt.Errorf("duplicate sibling tag or public name: %s", selected.Path)
		}
	}
	entry := &node{
		field: selected, Name: name, Required: required, Schema: constraintsOnly(schema),
		description: schema.Description,
	}
	if selected.Description != "" {
		entry.description = selected.Description
	}
	// The contract lock tracks accepted values independently of editable prose.
	entry.Description = ""
	if selected.Input != nil {
		entry.inputDescription = selected.Input.Description
		entry.Input = nil
		if selected.Input.MaxItems != nil {
			entry.Input = &inputPolicy{MaxItems: selected.Input.MaxItems}
		}
	}
	parent.children = append(parent.children, entry)
	return entry, nil
}

// validateInputPolicy checks write overrides so public input can narrow but never weaken stored bounds.
func validateInputPolicy(selected field, schema *apiextensionsv1.JSONSchemaProps) error {
	input := selected.Input
	if input == nil {
		return nil
	}
	if !slices.Contains(selected.Access, modeCreate) && !slices.Contains(selected.Access, modeReplace) {
		return fmt.Errorf("input policy on %s requires a writable operation", selected.Path)
	}
	if input.MaxItems == nil && strings.TrimSpace(input.Description) == "" {
		return fmt.Errorf("input policy on %s is empty", selected.Path)
	}
	if input.MaxItems != nil {
		if schema.Type != schemaArray {
			return fmt.Errorf("input max_items on %s requires an array", selected.Path)
		}
		if *input.MaxItems <= 0 ||
			(schema.MinItems != nil && *input.MaxItems < *schema.MinItems) ||
			(schema.MaxItems != nil && *input.MaxItems > *schema.MaxItems) {
			return fmt.Errorf("input max_items on %s must narrow the stored bounds to a positive maximum", selected.Path)
		}
	}
	return nil
}

// publicSchema selects operation-specific limits so documentation and generated validation stay aligned.
func (n *node) publicSchema(mode string) apiextensionsv1.JSONSchemaProps {
	schema := n.Schema
	if mode != modeRead && n.Input != nil && n.Input.MaxItems != nil {
		schema.MaxItems = n.Input.MaxItems
	}
	return schema
}

// publicDescription selects reviewed input prose so write restrictions do not describe read responses.
func (n *node) publicDescription(mode string) string {
	if mode != modeRead && n.inputDescription != "" {
		return n.inputDescription
	}
	return n.description
}

// constraintsOnly removes child properties and prose while retaining admission rules for compatibility checks.
func constraintsOnly(schema *apiextensionsv1.JSONSchemaProps) apiextensionsv1.JSONSchemaProps {
	constraints := *schema.DeepCopy()
	constraints.Description = ""
	// Static error wording does not change which requests admission accepts.
	for i := range constraints.XValidations {
		constraints.XValidations[i].Message = ""
	}
	if constraints.Items != nil && constraints.Items.Schema != nil {
		item := constraintsOnly(constraints.Items.Schema)
		constraints.Items.Schema = &item
	}
	constraints.Properties = nil
	// A newly required private sibling can invalidate public creates even though it stays masked.
	slices.Sort(constraints.Required)
	return constraints
}

// resolveSchema finds an explicit stored path and its requiredness so exposure uses the CRD's rules.
func resolveSchema(
	spec *apiextensionsv1.JSONSchemaProps,
	parts []string,
) (*apiextensionsv1.JSONSchemaProps, bool, error) {
	current := spec
	required := false
	for _, part := range parts {
		required = slices.Contains(current.Required, part)
		child, exists := current.Properties[part]
		if !exists {
			return nil, false, fmt.Errorf("exposure path missing from internal schema: %s", strings.Join(parts, "."))
		}
		current = &child
	}
	return current, required, nil
}

// validateAccess checks and orders permissions so children cannot exceed their parent's exposure.
func validateAccess(selected field, parent *node) error {
	if len(selected.Access) == 0 {
		return fmt.Errorf("missing field access: %s", selected.Path)
	}
	seen := map[string]bool{}
	for _, access := range selected.Access {
		if (access != modeCreate && access != modeRead && access != modeReplace) || seen[access] {
			return fmt.Errorf("unsupported or duplicate access on %s", selected.Path)
		}
		if parent.Path != "" && !slices.Contains(parent.Access, access) {
			return fmt.Errorf("parent does not expose %s for %s", access, selected.Path)
		}
		seen[access] = true
	}
	slices.Sort(selected.Access)
	return nil
}

// supportedSchema rejects shapes the generator cannot faithfully map, validate and document.
func supportedSchema(schema *apiextensionsv1.JSONSchemaProps) error {
	if schema.Nullable || schema.XIntOrString || schema.XPreserveUnknownFields != nil ||
		schema.AdditionalProperties != nil || len(schema.AllOf)+len(schema.AnyOf)+len(schema.OneOf) > 0 ||
		schema.Not != nil || schema.Ref != nil || schema.Default != nil || len(schema.Enum) != 0 || schema.Format != "" {
		return fmt.Errorf("unreviewed nullable, union, map, reference, default, enum or format")
	}
	switch schema.Type {
	case schemaObject:
		return nil
	case schemaString:
		if err := supportedBounds(schema.MinLength, schema.MaxLength); err != nil {
			return err
		}
		if _, err := regexp.Compile(schema.Pattern); err != nil {
			return fmt.Errorf("invalid string pattern: %w", err)
		}
		return nil
	case schemaArray:
		if schema.XListType == nil || *schema.XListType != "atomic" || schema.Items == nil ||
			schema.Items.Schema == nil || schema.Items.Schema.Type != schemaString {
			return fmt.Errorf("only atomic string collections have a defined public contract")
		}
		if schema.UniqueItems {
			return fmt.Errorf("uniqueItems requires explicit public validation semantics")
		}
		if err := supportedBounds(schema.MinItems, schema.MaxItems); err != nil {
			return err
		}
		return supportedSchema(schema.Items.Schema)
	default:
		return fmt.Errorf("unsupported scalar type %q", schema.Type)
	}
}

// supportedBounds rejects invalid or unrepresentable limits so generated metadata preserves their meaning.
func supportedBounds(minimum, maximum *int64) error {
	// OpenAPI annotation maxima use proto3 scalars: zero would disappear from the
	// generated document and incorrectly advertise an unbounded field.
	if (minimum != nil && *minimum < 0) ||
		(maximum != nil && *maximum <= 0) ||
		(minimum != nil && maximum != nil && *minimum > *maximum) {
		return fmt.Errorf("unsupported negative, empty-only or inconsistent bounds")
	}
	return nil
}

// snake converts ASCII field names to snake_case with acronym boundaries to keep public naming consistent.
func snake(value string) string {
	var out strings.Builder
	for i, r := range value {
		if r >= 'A' && r <= 'Z' {
			// Keep acronym runs together, but separate their final capital from a following word.
			if i > 0 && (value[i-1] >= 'a' && value[i-1] <= 'z' ||
				value[i-1] >= '0' && value[i-1] <= '9' ||
				i+1 < len(value) && value[i+1] >= 'a' && value[i+1] <= 'z') {
				out.WriteByte('_')
			}
			r += 'a' - 'A'
		}
		out.WriteRune(r)
	}
	return out.String()
}

// title joins path words into exported identifiers so generated fields and message names use one convention.
func title(value string) string {
	parts := strings.FieldsFunc(value, func(r rune) bool { return r == '.' || r == '_' })
	for i, part := range parts {
		parts[i] = strings.ToUpper(part[:1]) + part[1:]
	}
	return strings.Join(parts, "")
}
