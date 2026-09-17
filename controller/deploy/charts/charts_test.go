// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package charts

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestNames(t *testing.T) {
	if got, want := Names(), []string{"controller", "postgresql"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Names() = %v, want %v", got, want)
	}
	if Has("nope") {
		t.Error("Has(nope) = true")
	}
}

// TestEveryChartIsComplete: the files a chart cannot render without are
// present, including the underscore-prefixed helpers that plain go:embed
// would have skipped.
func TestEveryChartIsComplete(t *testing.T) {
	for _, name := range Names() {
		chart, err := FS(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"Chart.yaml", "values.yaml", "templates/_helpers.tpl"} {
			if _, err := fs.Stat(chart, want); err != nil {
				t.Errorf("chart %s is missing %s: %v", name, want, err)
			}
		}
		meta, err := fs.ReadFile(chart, "Chart.yaml")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(meta), "name: "+name) {
			t.Errorf("chart %s: Chart.yaml does not name it:\n%s", name, meta)
		}
	}
}

// TestExtract_MatchesTheSourceTree: what is written to disk is what is in the
// repository, file for file.
func TestExtract_MatchesTheSourceTree(t *testing.T) {
	for _, name := range Names() {
		dir := filepath.Join(t.TempDir(), name)
		if err := Extract(name, dir); err != nil {
			t.Fatal(err)
		}
		src := os.DirFS(name)
		var missing []string
		if err := fs.WalkDir(src, ".", func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			want, _ := fs.ReadFile(src, path)
			got, readErr := os.ReadFile(filepath.Join(dir, filepath.FromSlash(path)))
			if readErr != nil || !bytes.Equal(got, want) {
				missing = append(missing, path)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if len(missing) > 0 {
			t.Errorf("chart %s: extracted tree differs from source at %v", name, missing)
		}
		if err := Extract(name, dir); err == nil {
			t.Error("Extract into a non-empty directory must fail")
		}
	}
}
