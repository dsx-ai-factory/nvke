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
	"os"
	"path/filepath"

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

	if check {
		var stale []string
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
		fmt.Fprintf(os.Stdout, "composition up to date: %s\n", outDir)
		return nil
	}

	for path, content := range files {
		if err := os.WriteFile(path, content, 0o600); err != nil {
			return err
		}
		fmt.Fprintf(os.Stdout, "wrote %s\n", path)
	}
	return nil
}
