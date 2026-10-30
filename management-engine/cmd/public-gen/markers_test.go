// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const widgetMarkers = `// +nvke:public:name=PublicWidget
// +nvke:public:path=/widgets
// +nvke:public:metadata=name
`

func TestPublicMarkerValidation(t *testing.T) {
	for _, tt := range []struct{ name, markers, wantError string }{
		{
			"missing name", strings.ReplaceAll(widgetMarkers, "// +nvke:public:name=PublicWidget\n", ""),
			`missing public marker "name"`,
		},
		{
			"missing path", strings.ReplaceAll(widgetMarkers, "// +nvke:public:path=/widgets\n", ""),
			`missing public marker "path"`,
		},
		{
			"missing metadata", strings.ReplaceAll(widgetMarkers, "// +nvke:public:metadata=name\n", ""),
			`missing public marker "metadata"`,
		},
		{"unknown", widgetMarkers + "// +nvke:public:verbs=get\n", `unknown public marker "verbs"`},
		{"duplicate", widgetMarkers + "// +nvke:public:path=/other\n", `duplicate public marker "path"`},
		{"missing equals", strings.ReplaceAll(widgetMarkers, "path=/widgets", "path"), "invalid public marker"},
		{"empty", strings.ReplaceAll(widgetMarkers, "path=/widgets", "path="), "invalid public marker"},
		{"malformed prefix", widgetMarkers + "// +nvke:public\n", "invalid public marker"},
		{
			"private name", strings.ReplaceAll(widgetMarkers, "PublicWidget", "privateWidget"),
			"invalid or duplicate public name/path",
		},
		{
			"source name collision", strings.ReplaceAll(widgetMarkers, "PublicWidget", "Widget"),
			"invalid or duplicate public name/path",
		},
		{
			"relative path", strings.ReplaceAll(widgetMarkers, "=/widgets", "=widgets"),
			"invalid or duplicate public name/path",
		},
		{
			"unclean path", strings.ReplaceAll(widgetMarkers, "=/widgets", "=/widgets/../other"),
			"invalid or duplicate public name/path",
		},
		{
			"private metadata", strings.ReplaceAll(widgetMarkers, "metadata=name", "metadata=namespace"),
			"unsupported or duplicate metadata",
		},
		{
			"duplicate metadata", strings.ReplaceAll(widgetMarkers, "metadata=name", "metadata=name,name"),
			"unsupported or duplicate metadata",
		},
		{
			"empty metadata entry", strings.ReplaceAll(widgetMarkers, "metadata=name", "metadata=name,"),
			"unsupported or duplicate metadata",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			writeMarkerFixture(t, root, "apis/v1/widget.go", "package v1\ntype Spec struct{}\n"+
				tt.markers+"type Widget struct { Spec Spec `json:\"spec\"` }\n")
			_, err := readModel(root)
			require.ErrorContains(t, err, tt.wantError)
		})
	}
}

func TestPublicResourceDiscovery(t *testing.T) {
	root := t.TempDir()
	first := "package v1\ntype FirstSpec struct{}\n// Existing documentation.\n" +
		"// +kubebuilder:object:root=true\n// +nvke:public:name=Alpha\n// +nvke:public:path=/first\n" +
		"// +nvke:public:metadata=name, uid\ntype First struct { Spec FirstSpec `json:\"spec\"` }\n"
	second := "package v1\ntype (\nSecondSpec struct{}\n// +nvke:public:name=Zulu\n" +
		"// +nvke:public:path=/second\n// +nvke:public:metadata=name\n" +
		"Second struct { Spec SecondSpec `json:\"spec\"` }\nUnselected struct { Private string }\n)\n"
	writeMarkerFixture(t, root, "apis/v1/z_first.go", first)
	writeMarkerFixture(t, root, "apis/v1/a_second.go", second)
	for _, source := range []string{"First", "Second"} {
		writeMarkerCRD(t, root, source)
	}
	before, err := readModel(root)
	require.NoError(t, err)
	require.Len(t, before.Resources, 2, "unmarked types must not opt in")
	require.Equal(t, "First", before.Resources[0].Source)
	require.Equal(t, "Second", before.Resources[1].Source)
	require.Equal(t, []string{"name", "uid"}, before.Resources[0].Metadata)

	// Changing file order must not reorder generated declarations or registrations.
	require.NoError(t, os.Remove(filepath.Join(root, "apis/v1/z_first.go")))
	require.NoError(t, os.Remove(filepath.Join(root, "apis/v1/a_second.go")))
	writeMarkerFixture(t, root, "apis/v1/a_first.go", first)
	writeMarkerFixture(t, root, "apis/v1/z_second.go", second)
	after, err := readModel(root)
	require.NoError(t, err)
	require.Equal(t, emitPublic(before), emitPublic(after))
	require.Equal(t, emitRegistrations(before), emitRegistrations(after))

	// An annotated type and its CRD are sufficient: there is no separate catalog to edit.
	writeMarkerFixture(t, root, "apis/v1/new.go", "package v1\n"+widgetMarkers+
		"type Widget struct { Spec FirstSpec `json:\"spec\"` }\n")
	writeMarkerCRD(t, root, "Widget")
	after, err = readModel(root)
	require.NoError(t, err)
	require.Len(t, after.Resources, 3)
	require.Equal(t, "Widget", after.Resources[1].Source)
	require.Contains(t, string(emitPublic(after)), "type PublicWidget struct")
	require.Contains(t, string(emitRegistrations(after)), `]("/widgets", resource.ResourceInfo{`)
}

func TestPublicMarkerAssociation(t *testing.T) {
	for _, tt := range []struct{ name, source, wantError string }{
		{
			"group-level ambiguity", widgetMarkers + "type (Widget struct{}; Other struct{})",
			"must attach to individual types",
		},
		{
			"duplicate across declaration and spec", widgetMarkers + "type (\n// +nvke:public:name=Other\nWidget struct{}\n)",
			`duplicate public marker "name"`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			writeMarkerFixture(t, root, "apis/v1/widget.go", "package v1\n"+tt.source)
			_, _, _, err := readSource(root)
			require.ErrorContains(t, err, tt.wantError)
		})
	}
}

func TestPublicResourceCollisions(t *testing.T) {
	for _, field := range []string{"name", "path"} {
		t.Run(field, func(t *testing.T) {
			root := t.TempDir()
			other := strings.ReplaceAll(widgetMarkers, "PublicWidget", "Other")
			if field == "name" {
				other = strings.ReplaceAll(widgetMarkers, "/widgets", "/other")
			}
			writeMarkerFixture(t, root, "apis/v1/widget.go", "package v1\ntype Spec struct{}\n"+
				widgetMarkers+"type Widget struct { Spec Spec `json:\"spec\"` }\n"+
				other+"type Another struct { Spec Spec `json:\"spec\"` }\n")
			writeMarkerCRD(t, root, "Widget")
			writeMarkerCRD(t, root, "Another")
			_, err := readModel(root)
			require.ErrorContains(t, err, "invalid or duplicate public name/path")
		})
	}
}

func writeMarkerFixture(t *testing.T, root, path, body string) {
	t.Helper()
	filename := filepath.Join(root, path)
	require.NoError(t, os.MkdirAll(filepath.Dir(filename), 0o755))
	require.NoError(t, os.WriteFile(filename, []byte(body), 0o644))
}

func writeMarkerCRD(t *testing.T, root, source string) {
	t.Helper()
	writeMarkerFixture(t, root, "config/crd/bases/"+source+".yaml", fmt.Sprintf(`spec:
  group: example.test
  names: {kind: %s, plural: %ss}
  scope: Namespaced
  versions:
  - {name: v1, served: true, storage: true}
`, source, strings.ToLower(source)))
}
