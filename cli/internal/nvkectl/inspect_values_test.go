// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nvkectl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInspectValues: the top-level keys of a values file are read by a YAML
// parser, so every spelling of the same mapping gives the same keys.
func TestInspectValues(t *testing.T) {
	for _, tc := range []struct {
		name, content string
		want          []string
	}{
		{"block mapping", "appDb:\n  url: x\nimage:\n  tag: v1\n", []string{"appDb", "image"}},
		{"a nested key of the same name is not a top-level key", "secrets:\n  appDb: x\n", []string{"secrets"}},
		{"a flow mapping after a comment", "# Database override\n{appDb: {url: external-pg.example.test:6432}}\n", []string{"appDb"}},
		{"a flow mapping on several lines", "{\n  appDb: {url: x},\n  image: {tag: v1}\n}\n", []string{"appDb", "image"}},
		{"a top-level mapping indented as a whole", "  appDb:\n    url: x\n  image:\n    tag: v1\n", []string{"appDb", "image"}},
		{"a document start and comments", "---\n# c\nappDb: {}\n", []string{"appDb"}},
		{"JSON", "{\"appDb\": {\"url\": \"x\"}, \"image\": {}}\n", []string{"appDb", "image"}},
		{"quoted keys", "\"appDb\":\n  a: 1\n'image': {}\n", []string{"appDb", "image"}},
		{"a null value is still set", "appDb:\n", []string{"appDb"}},
		{"a repeated key is listed each time", "appDb: {url: a}\nimage: {}\nappDb: {url: b}\n", []string{"appDb", "image", "appDb"}},
		{"a merge key brings its keys in", "base: &base\n  appDb: {url: a}\n<<: *base\nimage: {}\n", []string{"base", "appDb", "image"}},
		{"a merge of several mappings", "x: &x {appDb: 1}\ny: &y {image: 2}\n<<: [*x, *y]\n", []string{"x", "y", "appDb", "image"}},
		{"only comments", "# nothing here\n", nil},
		{"an empty file", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "values.yaml")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			stdout, _, err := runGenerate(t, "inspect", "values", path)
			if err != nil {
				t.Fatal(err)
			}
			got := strings.Fields(stdout)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("keys %v, want %v", got, tc.want)
			}
		})
	}
}

// TestInspectValues_Refusals: a file Helm would not read as one mapping is an
// error naming the file, with nothing on standard output.
func TestInspectValues_Refusals(t *testing.T) {
	for _, tc := range []struct{ name, content, want string }{
		{"a list", "- appDb\n- image\n", "the top level is not a mapping"},
		{"a scalar", "appDb\n", "the top level is not a mapping"},
		{"two documents", "appDb: {}\n---\nimage: {}\n", "more than one document"},
		{"a syntax error", "appDb: [unclosed\n", "values file"},
		{"a merge of a list", "<<: [1]\n", "a merge key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "values.yaml")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			stdout, _, err := runGenerate(t, "inspect", "values", path)
			if err == nil || stdout != "" || !strings.Contains(err.Error(), "values file "+path) || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("stdout %q, error %v", stdout, err)
			}
		})
	}
	if _, _, err := runGenerate(t, "inspect", "values", "/nonexistent/values.yaml"); err == nil || !strings.Contains(err.Error(), "nonexistent") {
		t.Errorf("missing file: %v", err)
	}
}
