// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInclusiveCatalogProjection(t *testing.T) {
	const fixture = "package v1\n" +
		"type Identifier string\ntype Handle Identifier\n" +
		"type Lifecycle struct {\n// +nvke:catalog:include\nDeprecated bool `json:\"deprecated\"`\n" +
		"// +nvke:catalog:include\nSupportedUntil *metav1.Time `json:\"supportedUntil,omitempty\"`\n" +
		"Hidden bool `json:\"hidden\"`\nFutureSecret string `json:\"futureSecret\"` }\n" +
		"type Entries []Lifecycle\n" +
		"// +nvke:catalog:view\ntype Source struct {\n" +
		"// +nvke:catalog:include\nName Handle `json:\"name,omitempty\"`\n" +
		"// +nvke:catalog:include\nLifecycle *Lifecycle `json:\"lifecycle,omitempty\"`\n" +
		"// +nvke:catalog:include\nGroups map[string]Entries `json:\"groups,omitempty\"`\n" +
		"PrivateLifecycle Lifecycle `json:\"privateLifecycle\"`\n" +
		"PrivateSlice []Lifecycle `json:\"privateSlice\"`\n" +
		"PrivateMap map[string]Lifecycle `json:\"privateMap\"`\nFutureSecret string `json:\"futureSecret\"` }\n"
	root := t.TempDir()
	writeMarkerFixture(t, root, "apis/v1/types.go", fixture)
	m, err := readModel(root)
	require.NoError(t, err)
	got := string(emitCatalog(m))
	require.Contains(t, got, "type CatalogSource struct")
	for _, absent := range []string{
		"Hidden", "FutureSecret", "PrivateLifecycle", "PrivateSlice", "PrivateMap",
	} {
		require.NotContains(t, got, absent)
	}
	require.Contains(t, got, "Name Handle `json:\"name,omitempty\"`")
	require.Contains(t, got, "Lifecycle *CatalogLifecycle `json:\"lifecycle,omitempty\"`")
	require.Contains(t, got, "Groups map[string]CatalogEntries")
	require.Contains(t, got, "type CatalogEntries []CatalogLifecycle")
	require.Contains(t, got, "SupportedUntil *metav1.Time")
	require.Equal(t, 1, strings.Count(got, "type CatalogLifecycle "))
	// Declaration order cannot affect the stable generated result.
	reordered := strings.ReplaceAll(fixture, "type Handle Identifier\n", "") + "type Handle Identifier\n"
	writeMarkerFixture(t, root, "apis/v1/types.go", reordered)
	again, err := readModel(root)
	require.NoError(t, err)
	require.Equal(t, emitCatalog(m), emitCatalog(again))
}

func TestInclusiveCatalogRejectsUnsafeSelections(t *testing.T) {
	const rootMarker = "// +nvke:catalog:view\ntype Source struct {\n"
	const include = "// +nvke:catalog:include\n"
	for _, tt := range []struct{ name, source, want string }{
		{"empty child", "type Child struct { Secret string };\n" + rootMarker + include +
			"Child Child `json:\"child\"` }", "no included fields"},
		{"external struct", rootMarker + include + "Metadata metav1.ObjectMeta `json:\"metadata\"` }",
			"unsupported catalog field type"},
		{"interface", rootMarker + include + "Value any `json:\"value\"` }", "unsupported catalog field type"},
		{"hidden cannot opt in", rootMarker + include + "Hidden bool `json:\"hidden\"` }", "forbidden JSON name"},
		{"bad marker", rootMarker + "// +nvke:catalog:include=true\nValue string `json:\"value\"` }",
			"invalid or duplicate"},
		{"recursive", rootMarker + include + "Child *Source `json:\"child\"` }", "recursive catalog type"},
		{"generated name collision", "type CatalogChild struct{}\ntype Child struct {\n" + include +
			"Leaf string `json:\"leaf\"` }\n" + rootMarker + include +
			"Child Child `json:\"child\"` }", "conflicts with source"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			writeMarkerFixture(t, root, "apis/v1/types.go", "package v1\n"+tt.source)
			_, err := readModel(root)
			require.ErrorContains(t, err, tt.want)
		})
	}
}
