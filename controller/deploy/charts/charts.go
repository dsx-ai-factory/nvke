// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package charts embeds the Helm charts this repository owns, so a binary
// built from a commit installs the charts of that same commit without a
// checkout at hand. Each chart directory beside this file is one chart, named
// by the directory.
package charts

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// all: so that templates/_helpers.tpl is included; plain go:embed skips files
// whose names start with "_" or ".", and a chart without its helpers renders
// nothing useful.
//
//go:embed all:controller all:postgresql
var files embed.FS

// Names returns every embedded chart, sorted.
func Names() []string {
	entries, err := files.ReadDir(".")
	if err != nil {
		panic("charts: embedded tree does not list: " + err.Error())
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

// Has reports whether name is an embedded chart.
func Has(name string) bool {
	info, err := fs.Stat(files, name)
	return err == nil && info.IsDir()
}

// FS returns the embedded chart directory for name, or an error naming the
// charts that exist.
func FS(name string) (fs.FS, error) {
	if !Has(name) {
		return nil, fmt.Errorf("charts: no embedded chart %q; embedded charts are %v", name, Names())
	}
	return fs.Sub(files, name)
}

// Extract writes the embedded chart name into dir, which must not exist or
// must be empty, so the result is exactly the chart and nothing else. The
// directory can then be handed to helm as a chart path. Files are 0600 and
// directories 0700: only the process that extracted the chart reads it.
func Extract(name, dir string) error {
	chart, err := FS(name)
	if err != nil {
		return err
	}
	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		return fmt.Errorf("charts: %s is not empty", dir)
	}
	return fs.WalkDir(chart, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		target := filepath.Join(dir, filepath.FromSlash(path))
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		content, err := fs.ReadFile(chart, path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, content, 0o600)
	})
}
