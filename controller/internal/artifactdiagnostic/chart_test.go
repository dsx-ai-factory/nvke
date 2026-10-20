// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package artifactdiagnostic

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

func chartFixture(t *testing.T, entries []*tar.Header) string {
	t.Helper()
	var raw bytes.Buffer
	gz := gzip.NewWriter(&raw)
	tr := tar.NewWriter(gz)
	for _, h := range entries {
		if err := tr.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tr.Write(bytes.Repeat([]byte("x"), int(h.Size))); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "supplied.tgz")
	if err := os.WriteFile(path, raw.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPackageAdmission(t *testing.T) {
	metadata := &tar.Header{Name: "chart/Chart.yaml", Mode: 0o600, Size: 1, Typeflag: tar.TypeReg}
	for _, tc := range []struct {
		name    string
		entries []*tar.Header
		fail    bool
	}{
		{"package", []*tar.Header{metadata}, false},
		{"missing metadata", nil, true},
		{"traversal", []*tar.Header{metadata, {Name: "chart/../../escape", Typeflag: tar.TypeReg}}, true},
		{"symlink", []*tar.Header{metadata, {Name: "chart/link", Typeflag: tar.TypeSymlink, Linkname: "/tmp"}}, true},
		{"duplicate", []*tar.Header{metadata, metadata}, true},
		{"second root", []*tar.Header{metadata, {Name: "other/file", Typeflag: tar.TypeReg}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := chartFixture(t, tc.entries)
			_, err := extractChart(t.Context(), file, "")
			if (err != nil) != tc.fail {
				t.Fatalf("preparation-free admission: %v", err)
			}
			if tc.fail {
				return
			}
			dir := t.TempDir()
			path, err := extractChart(t.Context(), file, dir)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(path, "Chart.yaml")); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCanceledPackageAndWrongIntegrity(t *testing.T) {
	file := chartFixture(t, []*tar.Header{{Name: "chart/Chart.yaml", Size: 1, Mode: 0o600, Typeflag: tar.TypeReg}})
	if err := checkHash(t.Context(), file, "sha256:"+string(bytes.Repeat([]byte("0"), 64))); err == nil {
		t.Fatal("changed bytes accepted")
	}
}
