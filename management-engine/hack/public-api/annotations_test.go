// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Annotation tests preserve public constraints without exposing private admission details.

package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

// TestPublicAnnotationsRespectOperationAndPresence checks generated public constraints.
// Input restrictions and optional fields must remain accurate for each operation.
func TestPublicAnnotationsRespectOperationAndPresence(t *testing.T) {
	crd, policy := fixture(t)
	model, err := compile(crd, policy)
	require.NoError(t, err)
	outputs, err := render(model)
	require.NoError(t, err)
	contract := string(outputs[contractDir+"/spec.proto"])
	message := func(name string) string {
		t.Helper()
		_, body, found := strings.Cut(contract, "message "+name+" {\n")
		require.True(t, found, "missing generated message %s", name)
		body, _, _ = strings.Cut(body, "\nmessage ")
		return body
	}
	create, replace := message("CreateClusterSpec"), message("ReplaceClusterSpec")
	require.Contains(t, create, "\"cluster_network\"")
	require.Contains(t, create, "\"control_plane_class\"")
	require.NotContains(t, replace, "cluster_network")
	require.Contains(t, replace, "optional string control_plane_class")
	require.Contains(t, replace, "min_length: 1")
	require.Contains(t, replace, "max_length: 253")
	require.Contains(t, replace, "pattern:")
	channel := message("CreateReleaseChannel")
	require.Contains(t, channel, "required: [\"name\"]")
	require.Contains(t, channel, "optional string version")
	require.Contains(t, channel, "max_length: 128")
	for _, kind := range []string{"Pods", "Services"} {
		createNetwork := message("CreateClusterNetwork" + kind)
		readNetwork := message("ReadClusterNetwork" + kind)
		require.Contains(t, createNetwork, "max_items: 1")
		require.Contains(t, createNetwork, "canonical IPv4")
		require.Contains(t, createNetwork, "must not overlap")
		require.NotContains(t, createNetwork, "IPv6")
		require.Contains(t, readNetwork, "max_items: 2")
		require.Contains(t, readNetwork, "max_length: 49")
		require.Contains(t, readNetwork, "IPv6")
	}
	for _, private := range []string{"service_domain", "cluster.local", "public_access_enabled", "isCIDR(", "oldSelf"} {
		require.NotContains(t, contract, private)
		require.NotContains(t, string(outputs[generatedDir+"/FIELDS.md"]), private)
	}
}

// TestAdmissionRulesStayPrivateButCompatibilityChecked changes private validation rules.
// The compatibility check must detect them without publishing their expressions or diagnostics.
func TestAdmissionRulesStayPrivateButCompatibilityChecked(t *testing.T) {
	crdData, policyData := fixture(t)
	before, err := compile(crdData, policyData)
	require.NoError(t, err)
	lock, err := verifyLock(before, nil, true)
	require.NoError(t, err)
	var crd apiextensionsv1.CustomResourceDefinition
	require.NoError(t, yaml.UnmarshalStrict(crdData, &crd))
	spec := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	class := spec.Properties["controlPlaneClass"]
	class.XValidations = append(class.XValidations, apiextensionsv1.ValidationRule{
		Rule: "self != 'PRIVATE-ADMISSION-VALUE'", Message: "PRIVATE-ADMISSION-DIAGNOSTIC",
	})
	spec.Properties["controlPlaneClass"] = class
	crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"] = spec
	changed, err := yaml.Marshal(crd)
	require.NoError(t, err)
	after, err := compile(changed, policyData)
	require.NoError(t, err)
	want, err := render(before)
	require.NoError(t, err)
	got, err := render(after)
	require.NoError(t, err)
	require.Equal(t, want, got, "private admission expressions and diagnostics must not enter public artifacts")
	_, err = verifyLock(after, lock, false)
	require.ErrorContains(t, err, "changed entry controlPlaneClass")
}

// TestPublicProseIsEditableWithoutChangingCompatibility revises reviewed descriptions.
// Documentation and static admission diagnostics must not require a new lock.
func TestPublicProseIsEditableWithoutChangingCompatibility(t *testing.T) {
	crdData, policyData := fixture(t)
	before, err := compile(crdData, policyData)
	require.NoError(t, err)
	lock, err := verifyLock(before, nil, true)
	require.NoError(t, err)
	var exposure policy
	require.NoError(t, json.Unmarshal(policyData, &exposure))
	policyField(t, &exposure, "clusterNetwork").Description = "Reviewed shared description."
	policyField(t, &exposure, "clusterNetwork.pods.cidrBlocks").Input.Description = "Reviewed input description."
	changed, err := json.Marshal(exposure)
	require.NoError(t, err)
	after, err := compile(crdData, changed)
	require.NoError(t, err)
	_, err = verifyLock(after, lock, false)
	require.NoError(t, err)

	var crd apiextensionsv1.CustomResourceDefinition
	require.NoError(t, yaml.UnmarshalStrict(crdData, &crd))
	var reword func(*apiextensionsv1.JSONSchemaProps)
	reword = func(schema *apiextensionsv1.JSONSchemaProps) {
		for i := range schema.XValidations {
			schema.XValidations[i].Message = "Revised admission diagnostic."
		}
		for name, child := range schema.Properties {
			reword(&child)
			schema.Properties[name] = child
		}
		if schema.Items != nil && schema.Items.Schema != nil {
			reword(schema.Items.Schema)
		}
	}
	reword(crd.Spec.Versions[0].Schema.OpenAPIV3Schema)
	changedCRD, err := yaml.Marshal(crd)
	require.NoError(t, err)
	after, err = compile(changedCRD, changed)
	require.NoError(t, err)
	_, err = verifyLock(after, lock, false)
	require.NoError(t, err, "static admission wording must stay outside the lock at every schema level")
	for _, test := range []struct {
		name   string
		mutate func(*apiextensionsv1.ValidationRule)
	}{
		{"rule", func(rule *apiextensionsv1.ValidationRule) { rule.Rule = "self.controlPlaneClass.startsWith('x')" }},
		{"message expression", func(rule *apiextensionsv1.ValidationRule) {
			rule.MessageExpression = "'dynamic diagnostic'"
		}},
		{"reason", func(rule *apiextensionsv1.ValidationRule) { rule.Reason = new(apiextensionsv1.FieldValueForbidden) }},
	} {
		t.Run(test.name+" remains reviewable", func(t *testing.T) {
			candidate := crd.DeepCopy()
			spec := candidate.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
			test.mutate(&spec.XValidations[0])
			candidate.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"] = spec
			data, err := yaml.Marshal(candidate)
			require.NoError(t, err)
			model, err := compile(data, policyData)
			require.NoError(t, err)
			_, err = verifyLock(model, lock, false)
			require.ErrorContains(t, err, "changed entry $spec")
		})
	}
}
