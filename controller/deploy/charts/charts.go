// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package charts embeds the Helm charts this repository owns, so a binary
// built from a commit installs the charts of that same commit without a
// checkout at hand. Each chart directory beside this file is one chart, named
// by the directory, and each is named in the embed directive below;
// TestNames_MatchesTheChartsOnDisk keeps the two in step.
package charts

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The chart directories, so a caller names one by constant rather than by
// literal. These are directory names: the nvke-controller chart lives in
// controller/, because a chart is named for the OCI repository it publishes
// to rather than for where it is kept.
const (
	NVKEController = "controller"
	Postgresql     = "postgresql"
)

// all: so that templates/_helpers.tpl is included; plain go:embed skips files
// whose names start with "_" or ".", and a chart without its helpers renders
// nothing useful.
//
//go:embed all:controller all:postgresql
var files embed.FS

// Names returns every embedded chart, sorted. These are directory names, which
// is what FS and Extract take; a chart's Helm name is declared in its
// Chart.yaml and need not match.
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

// Has reports whether name is an embedded chart. Only a chart's own name
// counts: a path into one is not a chart, and neither is the tree root.
func Has(name string) bool {
	if name == "" || name == "." || strings.ContainsAny(name, `/\`) {
		return false
	}
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
//
// It is all or nothing. A write that fails partway leaves dir empty rather
// than holding a subset of the chart, which the emptiness check above would
// otherwise refuse on every later attempt without ever saying it is
// incomplete.
func Extract(name, dir string) error {
	chart, err := FS(name)
	if err != nil {
		return err
	}
	switch info, statErr := os.Stat(dir); {
	case statErr == nil && !info.IsDir():
		return fmt.Errorf("charts: %s is not a directory", dir)
	case statErr == nil:
		entries, readErr := os.ReadDir(dir)
		if readErr != nil {
			return fmt.Errorf("charts: reading %s: %w", dir, readErr)
		}
		if len(entries) > 0 {
			return fmt.Errorf("charts: %s is not empty", dir)
		}
	case !errors.Is(statErr, fs.ErrNotExist):
		return fmt.Errorf("charts: %s: %w", dir, statErr)
	}

	if err := writeChart(chart, dir); err != nil {
		clearDir(dir)
		return err
	}
	// After the walk, not before: MkdirAll leaves an existing directory's mode
	// alone, so this is what makes the 0700 above true of dir itself.
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("charts: securing %s: %w", dir, err)
	}
	return nil
}

// writeChart copies every file of chart into dir.
func writeChart(chart fs.FS, dir string) error {
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

// clearDir empties dir, leaving the directory itself as it was found so a
// caller that created it keeps it.
func clearDir(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		_ = os.RemoveAll(filepath.Join(dir, entry.Name()))
	}
}
