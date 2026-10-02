// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	crdvalidation "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/validation"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	schemacel "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	structuraldefaulting "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/defaulting"
	apivalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"sigs.k8s.io/yaml"
)

// crdSchema admits candidate custom resources the way the API server does on
// create: structural defaulting, OpenAPI v3 schema and CEL rules. It does not
// simulate admission webhooks or the status subresource.
type crdSchema struct {
	structural *structuralschema.Structural
	openAPI    apivalidation.SchemaValidator
	cel        *schemacel.Validator
}

// schemaSet holds one crdSchema per served kind, keyed by kind.
type schemaSet map[string]crdSchema

// loadSchemas reads every CRD manifest in dir. The generated manifests are validated against
// the same CRDs the chart installs, so a manifest the API server would refuse
// fails at generation time instead of at Argo CD sync time.
func loadSchemas(dir string) (schemaSet, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, fmt.Errorf("listing CRDs in %s: %w", dir, err)
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no CRD manifests found in %s", dir)
	}
	set := schemaSet{}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading CRD %s: %w", path, err)
		}
		var crd apiextensionsv1.CustomResourceDefinition
		if err := yaml.UnmarshalStrict(data, &crd); err != nil {
			return nil, fmt.Errorf("parsing CRD %s: %w", path, err)
		}
		schema, err := newCRDSchema(crd)
		if err != nil {
			return nil, fmt.Errorf("CRD %s: %w", path, err)
		}
		set[crd.Spec.Names.Kind] = schema
	}
	return set, nil
}

func newCRDSchema(crd apiextensionsv1.CustomResourceDefinition) (crdSchema, error) {
	apiextensionsv1.SetDefaults_CustomResourceDefinition(&crd)
	var internal apiextensions.CustomResourceDefinition
	if err := apiextensionsv1.Convert_v1_CustomResourceDefinition_To_apiextensions_CustomResourceDefinition(&crd, &internal, nil); err != nil {
		return crdSchema{}, fmt.Errorf("converting CRD: %w", err)
	}
	if errs := crdvalidation.ValidateCustomResourceDefinition(context.Background(), &internal); len(errs) != 0 {
		return crdSchema{}, fmt.Errorf("invalid CRD: %w", errs.ToAggregate())
	}
	if len(internal.Spec.Versions) != 1 {
		return crdSchema{}, fmt.Errorf("expected one version, found %d", len(internal.Spec.Versions))
	}
	validation := internal.Spec.Versions[0].Schema
	if validation == nil {
		// Conversion lifts a schema shared by all versions to spec.validation.
		validation = internal.Spec.Validation
	}
	if validation == nil || validation.OpenAPIV3Schema == nil {
		return crdSchema{}, errors.New("CRD has no validation schema")
	}
	structural, err := structuralschema.NewStructural(validation.OpenAPIV3Schema)
	if err != nil {
		return crdSchema{}, fmt.Errorf("building structural schema: %w", err)
	}
	openAPI, _, err := apivalidation.NewSchemaValidator(validation.OpenAPIV3Schema)
	if err != nil {
		return crdSchema{}, fmt.Errorf("building schema validator: %w", err)
	}
	return crdSchema{
		structural: structural,
		openAPI:    openAPI,
		// Nil when the schema carries no CEL rules, which Validate allows.
		cel: schemacel.NewValidator(structural, true, celconfig.PerCallLimit),
	}, nil
}

// validateDocuments admits every generated manifest against its CRD and returns
// all failures, sorted by file name.
func validateDocuments(schemas schemaSet, docs map[string][]byte) error {
	names := make([]string, 0, len(docs))
	for name := range docs {
		names = append(names, name)
	}
	sort.Strings(names)

	var failures []error
	for _, name := range names {
		if err := validateDocument(schemas, docs[name]); err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", name, err))
		}
	}
	return errors.Join(failures...)
}

func validateDocument(schemas schemaSet, body []byte) error {
	var obj map[string]any
	if err := yaml.Unmarshal(body, &obj); err != nil {
		return fmt.Errorf("parsing manifest: %w", err)
	}
	kind, _ := obj["kind"].(string)
	schema, ok := schemas[kind]
	if !ok {
		return fmt.Errorf("no CRD for kind %q", kind)
	}
	structuraldefaulting.Default(obj, schema.structural)
	errs := apivalidation.ValidateCustomResource(nil, obj, schema.openAPI)
	if schema.cel != nil {
		celErrs, _ := schema.cel.Validate(context.Background(), nil, schema.structural, obj, nil, celconfig.RuntimeCELCostBudget)
		errs = append(errs, celErrs...)
	}
	if len(errs) != 0 {
		return fmt.Errorf("rejected by the %s CRD: %w", kind, errs.ToAggregate())
	}
	return nil
}
