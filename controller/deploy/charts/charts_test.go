// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package charts

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestNames_MatchesTheChartsOnDisk closes the gap between the embed directive
// and the directory: a chart added beside charts.go without being named in
// //go:embed is invisible to every caller and nothing else would say so.
func TestNames_MatchesTheChartsOnDisk(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var onDisk []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(entry.Name(), "Chart.yaml")); err != nil {
			continue
		}
		onDisk = append(onDisk, entry.Name())
	}
	sort.Strings(onDisk)
	if got := Names(); !reflect.DeepEqual(got, onDisk) {
		t.Errorf("embedded charts %v but the directory holds %v; name every chart in the //go:embed directive in charts.go", got, onDisk)
	}
}

// TestHas_OnlyChartRoots: a path into a chart is not a chart, and neither is
// the tree root. FS and Extract would otherwise hand back a directory with no
// Chart.yaml for helm to reject.
func TestHas_OnlyChartRoots(t *testing.T) {
	for _, name := range []string{"", ".", "controller/templates", "./controller", "controller/"} {
		if Has(name) {
			t.Errorf("Has(%q) = true", name)
		}
		if _, err := FS(name); err == nil {
			t.Errorf("FS(%q) returned a chart", name)
		}
	}
	if !Has("controller") {
		t.Error(`Has("controller") = false`)
	}
}

// TestExtract_LeavesNothingBehindOnFailure: a write that fails partway must
// not leave a subset of the chart, which the emptiness guard would then refuse
// on every retry without saying the tree is incomplete.
func TestExtract_LeavesNothingBehindOnFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	dir := filepath.Join(t.TempDir(), "chart")
	if err := os.Mkdir(dir, 0o500); err != nil { // readable, not writable
		t.Fatal(err)
	}
	if err := Extract("controller", dir); err == nil {
		t.Fatal("Extract into an unwritable directory succeeded")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("a failed Extract left %d entries behind", len(entries))
	}

	// The retry, once the cause is gone, must work rather than report the
	// directory as occupied.
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Extract("controller", dir); err != nil {
		t.Fatalf("retry after the failure: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "Chart.yaml")); err != nil {
		t.Errorf("retry did not write the chart: %v", err)
	}
}

// TestExtract_RejectsANonDirectory keeps the guard's message about the path it
// was given rather than a mkdir error from halfway through the walk.
func TestExtract_RejectsANonDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := Extract("controller", path)
	if err == nil || !strings.Contains(err.Error(), "is not a directory") {
		t.Errorf("Extract into a regular file = %v", err)
	}
}

// TestExtract_SecuresTheDirectoryItWasGiven: MkdirAll leaves an existing
// directory's mode alone, so the 0700 the doc promises has to be applied.
func TestExtract_SecuresTheDirectoryItWasGiven(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "chart")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Extract("postgresql", dir); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("extracted directory mode = %o, want 700", got)
	}
}

// chartName matches the name field of a Chart.yaml.
var chartName = regexp.MustCompile(`(?m)^name:[ \t]+\S+`)

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
		// That it declares a name, not which one: the directory is the key
		// this package indexes by, and the Helm chart name is independent of
		// it. controller/ holds the chart named nvke-controller, after the OCI
		// repository it publishes to.
		if !chartName.Match(meta) {
			t.Errorf("chart %s: Chart.yaml declares no name:\n%s", name, meta)
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
