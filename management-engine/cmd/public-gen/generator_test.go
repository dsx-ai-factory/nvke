// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGenerate(t *testing.T) {
	const source = "package v1\ntype WidgetSpec struct { Enabled bool `json:\"enabled,omitempty\"` }\n" +
		"type Widget struct { Spec WidgetSpec `json:\"spec\"` }\n"
	for _, tt := range []struct {
		name, change, metadata, validation, module, extraConfig, wantError string
		stale, deepcopy                                                    bool
	}{
		{name: "missing output and default validation"},
		{name: "changed spec with stale public output", change: "rename", stale: true},
		{
			name: "stale deepcopy needs normal generation first", change: "rename", stale: true, deepcopy: true,
			wantError: "undefined: WidgetSpec",
		},
		{
			name: "handwritten validation references generated type", stale: true,
			validation: "func (w PublicWidget) Validate() error { _ = w.Spec.Enabled; return nil }",
		},
		{
			name: "internal validation has wrong result", validation: "func (Widget) Validate() string { return \"bad\" }",
			wantError: "wrong type for method Validate",
		},
		{
			name: "public validation has wrong result", validation: "func (PublicWidget) Validate() string { return \"bad\" }",
			wantError: "wrong type for method Validate",
		},
		{
			name:       "pointer validation cannot satisfy value contract",
			validation: "func (*Widget) Validate() error { return nil }",
			wantError:  "Validate must have a value receiver",
		},
		{name: "another provider module", module: "example.test/widget-provider"},
		{name: "genuine source error", change: "invalid", wantError: "undefined: Missing"},
		{
			name: "stale deepcopy cannot hide handwritten errors", change: "rename-invalid", stale: true, deepcopy: true,
			wantError: "undefined: Missing",
		},
		{
			name: "stale deepcopy cannot hide unresolved selected spec", change: "invalid-spec", deepcopy: true,
			wantError: "undefined: Missing",
		},
		{name: "selected type must resolve", change: "missing", wantError: "source Widget must be a declared struct"},
		{name: "private metadata rejected", metadata: "namespace", wantError: "unsupported or duplicate metadata"},
		{
			name: "unknown configuration key rejected", extraConfig: "  unsupported: true\n",
			wantError: `unknown field "unsupported"`,
		},
		{
			name: "duplicate configuration key rejected", extraConfig: "  source: Widget\n",
			wantError: `key "source" already set`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			write := func(path, body string) {
				t.Helper()
				filename := filepath.Join(root, path)
				require.NoError(t, os.MkdirAll(filepath.Dir(filename), 0o755))
				require.NoError(t, os.WriteFile(filename, []byte(body), 0o644))
			}
			module := "github.com/dsx-ai-factory/nvke/management-engine"
			write("go.mod", "module "+module+"\n\ngo 1.27.3\n")
			helper, err := os.ReadFile("../../pkg/projection/json.go")
			require.NoError(t, err)
			write("pkg/projection/json.go", string(helper))
			if tt.module != "" {
				write("library/go.mod", "module "+module+"\n\ngo 1.27.3\n")
				write("library/pkg/projection/json.go", string(helper))
				write("go.mod", "module "+tt.module+"\n\ngo 1.27.3\nrequire "+module+" v0.0.0\nreplace "+module+" => ./library\n")
				module = tt.module
			}
			metadata := tt.metadata
			if metadata == "" {
				metadata = "name"
			}
			write("config/public-resources.yaml", "- source: Widget\n  public: PublicWidget\n  path: /widgets\n"+
				"  metadata: ["+metadata+"]\n"+tt.extraConfig)
			write("config/crd/bases/widgets.yaml", `spec:
  group: example.test
  names: {kind: Widget, plural: widgets}
  scope: Namespaced
  versions:
  - {name: v1, served: true, storage: true}
`)
			write("apis/v1/widget.go", source)
			if tt.deepcopy {
				data, err := os.ReadFile("testdata/zz_generated.deepcopy.go")
				require.NoError(t, err)
				write("apis/v1/zz_generated.deepcopy.go", string(data))
			}
			if tt.stale {
				require.NoError(t, generate(root, false))
				if strings.HasPrefix(tt.change, "rename") {
					write("apis/v1/widget.go", strings.ReplaceAll(source, "WidgetSpec", "ChangedSpec"))
				} else {
					write(publicOutput, "package v1\nvar broken =\n")
				}
			}
			if tt.validation != "" {
				write("apis/v1/validation.go", "package v1\n"+tt.validation+"\n")
			}
			switch tt.change {
			case "invalid":
				write("apis/v1/widget.go", source+"var broken Missing\n")
			case "rename-invalid":
				write("apis/v1/widget.go", strings.ReplaceAll(source, "WidgetSpec", "ChangedSpec")+"var broken Missing\n")
			case "invalid-spec":
				write("apis/v1/widget.go", strings.Replace(source, "Spec WidgetSpec", "Spec Missing", 1))
			case "missing":
				write("apis/v1/widget.go", strings.ReplaceAll(source, "type Widget struct", "type Renamed struct"))
			}
			previous := map[string][]byte{}
			for _, path := range []string{publicOutput, registryOutput} {
				previous[path], _ = os.ReadFile(filepath.Join(root, path))
			}
			err = generate(root, false)
			if tt.wantError != "" {
				require.ErrorContains(t, err, tt.wantError)
				for path, before := range previous {
					after, _ := os.ReadFile(filepath.Join(root, path))
					require.Equal(t, before, after, "invalid input must not change %s", path)
				}
				return
			}
			require.NoError(t, err)
			require.NoError(t, generate(root, true))
			output, err := os.ReadFile(filepath.Join(root, publicOutput))
			require.NoError(t, err)
			require.Contains(t, string(output), "+kubebuilder:object:generate=false")
			require.NotContains(t, string(output), "UID")
			require.Equal(t, tt.validation == "", strings.Contains(string(output), "func (PublicWidget) Validate() error"))
			if tt.change == "rename" {
				require.Contains(t, string(output), "ChangedSpec")
			}
			registry, err := os.ReadFile(filepath.Join(root, registryOutput))
			require.NoError(t, err)
			require.Contains(t, string(registry), `v1 "`+module+`/apis/v1"`)
			require.Contains(t, string(registry), `Group: "example.test", Version: "v1", Kind: "Widget"`)
			require.Contains(t, string(registry), `Scope: resource.Scope("Namespaced")`)
			write(publicOutput, string(output)+"\n// drift\n")
			require.ErrorContains(t, generate(root, true), "is stale")
		})
	}
}
