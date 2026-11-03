// SPDX-License-Identifier: Apache-2.0

// public-gen generates public API types and registrations from Go types in apis/v1,
// CRDs in config/crd/bases, and +nvke:public markers on the selected source types.
// Run from the module root. The -check flag reports generated-file drift without writing files.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
)

const (
	publicOutput   = "apis/v1/zz_generated.public.go"
	registryOutput = "pkg/publicapi/zz_generated.registrations.go"
)

// main selects generation or drift checking from the module working directory.
func main() {
	check := flag.Bool("check", false, "check generated files without writing them")
	flag.Parse()
	if err := generate(".", *check); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// generate validates all input before checking or replacing either generated file.
func generate(root string, check bool) error {
	m, err := readModel(root)
	if err != nil {
		return err
	}
	outputs := map[string][]byte{}
	for _, output := range []struct {
		path string
		emit func(model) []byte
	}{
		{publicOutput, emitPublic}, {registryOutput, emitRegistrations},
	} {
		outputs[output.path], err = format.Source(output.emit(m))
		if err != nil {
			return fmt.Errorf("format %s: %w", output.path, err)
		}
		if output.path == publicOutput {
			m.SourceImport, err = checkTypes(root, outputs[publicOutput], m.Resources)
			if err != nil {
				return err
			}
		}
	}
	for _, path := range []string{publicOutput, registryOutput} {
		filename := filepath.Join(root, path)
		if check {
			existing, err := os.ReadFile(filename)
			if err != nil || !bytes.Equal(existing, outputs[path]) {
				return fmt.Errorf("%s is stale; run make generate-public", path)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filename, outputs[path], 0o644); err != nil {
			return err
		}
	}
	return nil
}
