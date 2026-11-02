// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// providerUnionFieldSuffix marks the pointer fields of ControlPlaneConfigSpec
// that form the provider union: their element type name ends with it.
const providerUnionFieldSuffix = "ControlPlaneProvider"

// controlPlaneProviderUnionFields returns the ControlPlaneConfigSpec fields that
// are pointers to a *ControlPlaneProvider struct -- the provider union members.
func controlPlaneProviderUnionFields() []reflect.StructField {
	specType := reflect.TypeOf(ControlPlaneConfigSpec{})
	var fields []reflect.StructField
	for i := range specType.NumField() {
		field := specType.Field(i)
		if field.Type.Kind() != reflect.Pointer {
			continue
		}
		if elem := field.Type.Elem(); elem.Kind() == reflect.Struct && strings.HasSuffix(elem.Name(), providerUnionFieldSuffix) {
			fields = append(fields, field)
		}
	}
	return fields
}

// TestActiveProviderCoversEveryProvider guards the provider union: every
// *ControlPlaneProvider field must be returned by activeProvider when it is the
// one set. A new provider field added without a matching case in activeProvider
// is caught here rather than silently degrading ScaleDownRanker (and any future
// capability accessor) to its nil-provider fallback. The reflection walk means
// adding the field is enough to extend the guard -- no edit to this test.
func TestActiveProviderCoversEveryProvider(t *testing.T) {
	fields := controlPlaneProviderUnionFields()
	require.NotEmpty(t, fields, "expected at least one *ControlPlaneProvider field in the union")

	for _, field := range fields {
		t.Run(field.Name, func(t *testing.T) {
			var spec ControlPlaneConfigSpec
			// Set only this provider, to a non-nil zero value.
			reflect.ValueOf(&spec).Elem().FieldByIndex(field.Index).Set(reflect.New(field.Type.Elem()))

			active := spec.activeProvider()
			require.NotNil(t, active, "activeProvider returned nil for %s: add a case in activeProvider", field.Name)
			assert.Equal(t, field.Type.Elem(), reflect.TypeOf(active),
				"activeProvider must return the dereferenced provider value (not a pointer) for %s, "+
					"so value-receiver capability methods assert cleanly", field.Name)
		})
	}
}
