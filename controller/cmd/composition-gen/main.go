// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// composition-gen turns the recipe under controller/deploy/composition into the
// nke-stack umbrella chart.
//
//	make composition        regenerate generated/nke-stack
//	make composition-check  fail if the committed chart is out of date
//
// -check exists so CI can prove the committed chart is what the recipe produces.
// Without it the two drift the moment someone edits one and not the other, which
// is the failure the recipe is meant to remove rather than introduce.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/dsx-ai-factory/nke/controller/internal/composition"
)

func main() {
	var (
		recipeDir = flag.String("recipe", "deploy/composition", "directory holding registry.yaml and the composition")
		name      = flag.String("composition", "nke-stack.yaml", "composition file within -recipe")
		outDir    = flag.String("out", "../generated/nke-stack", "directory to write Chart.yaml and values.yaml into")
		check     = flag.Bool("check", false, "compare against the committed output instead of writing")
	)
	flag.Parse()

	if err := run(*recipeDir, *name, *outDir, *check); err != nil {
		fmt.Fprintln(os.Stderr, "composition-gen:", err)
		os.Exit(1)
	}
}

func run(recipeDir, name, outDir string, check bool) error {
	recipe, err := composition.Load(recipeDir, name)
	if err != nil {
		return err
	}

	chart, err := recipe.EmitChart()
	if err != nil {
		return err
	}
	values, err := recipe.EmitValues()
	if err != nil {
		return err
	}

	files := map[string][]byte{
		filepath.Join(outDir, "Chart.yaml"):  chart,
		filepath.Join(outDir, "values.yaml"): values,
	}

	// The SBOM trees sit beside the chart rather than inside it: they reference
	// the chart, they are not part of it. Emitting them here puts them under the
	// same -check gate, so a stale deploy path fails the merge the same way a
	// stale Chart.yaml does.
	sboms, err := recipe.EmitSBOMs()
	if err != nil {
		return err
	}
	roots := map[string]bool{}
	for path, content := range sboms {
		files[filepath.Join(filepath.Dir(outDir), path)] = content
		// The first segment is the set: everything under it is this generator's
		// output, so it owns removals as well as writes.
		root, _, _ := strings.Cut(path, string(filepath.Separator))
		roots[filepath.Join(filepath.Dir(outDir), root)] = true
	}

	// A file the recipe no longer produces -- a target that stopped selecting a
	// base-config entry, say -- would otherwise sit in the tree unnoticed,
	// because comparing only what is emitted cannot see it.
	stale, err := unexpectedFiles(roots, files)
	if err != nil {
		return err
	}

	if check {
		for path, want := range files {
			got, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			if !bytes.Equal(got, want) {
				stale = append(stale, path)
			}
		}
		if len(stale) > 0 {
			return fmt.Errorf("out of date, run `make composition`: %v", stale)
		}
		fmt.Fprintf(os.Stdout, "artifacts up to date: %v\n", sortedKeys(roots))
		fmt.Fprintf(os.Stdout, "composition up to date: %s\n", outDir)
		return nil
	}

	for _, path := range stale {
		if err := os.Remove(path); err != nil {
			return err
		}
		fmt.Fprintf(os.Stdout, "removed %s\n", path)
	}

	for path, content := range files {
		// A deploy path is a directory per item, so the tree does not exist yet
		// on a first run.
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return err
		}
		if err := os.WriteFile(path, content, 0o600); err != nil {
			return err
		}
		fmt.Fprintf(os.Stdout, "wrote %s\n", path)
	}
	return nil
}

// unexpectedFiles lists files under the generated roots that the recipe no
// longer produces. In check mode they are drift; in write mode they are removed.
func unexpectedFiles(roots map[string]bool, expected map[string][]byte) ([]string, error) {
	var found []string
	for root := range roots {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			switch {
			case os.IsNotExist(err):
				// Nothing generated yet, which is not drift.
				return nil
			case err != nil:
				return err
			case entry.IsDir():
				return nil
			}
			if _, want := expected[path]; !want {
				found = append(found, path)
			}
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}
	sort.Strings(found)
	return found, nil
}

// sortedKeys returns a map's keys in a stable order, for messages.
func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
