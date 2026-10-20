// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package artifactdiagnostic

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestSuppliedChartContracts exercises the real upstream assertions without provisioning.
// It is a contract integration test, not native platform acceptance of Run.
func TestSuppliedChartContracts(t *testing.T) {
	if os.Getenv("NVKE_TEST_CHART_ARTIFACT") != "1" {
		t.Skip("opt-in real Helm/yq/PyYAML contract integration")
	}
	t.Chdir("../..")
	packages := t.TempDir()
	cmd := exec.CommandContext(t.Context(), "helm", "package", "deploy/charts/controller", "--destination", packages, "--version", "1.2.3", "--app-version", "1.2.3")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("prepare supplied test package: %v\n%s", err, output)
	}
	// Resolve the chart's actual name rather than asserting a source naming convention.
	files, err := filepath.Glob(filepath.Join(packages, "*.tgz"))
	if err != nil || len(files) != 1 {
		t.Fatalf("package selection: %v %v", files, err)
	}
	packagePath := files[0]
	raw, err := os.ReadFile(packagePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, broken := range []bool{false, true} {
		t.Run(map[bool]string{false: "supplied package", true: "contract breaking package"}[broken], func(t *testing.T) {
			subject := packagePath
			if broken {
				zipped, err := gzip.NewReader(bytes.NewReader(raw))
				if err != nil {
					t.Fatal(err)
				}
				defer zipped.Close()
				var changed bytes.Buffer
				out := gzip.NewWriter(&changed)
				writer := tar.NewWriter(out)
				reader := tar.NewReader(zipped)
				for {
					header, err := reader.Next()
					if err == io.EOF {
						break
					}
					if err != nil {
						t.Fatal(err)
					}
					if filepath.Base(header.Name) == "configmap-controller-config.yaml" {
						continue
					}
					if err := writer.WriteHeader(header); err != nil {
						t.Fatal(err)
					}
					if _, err := io.CopyN(writer, reader, header.Size); err != nil {
						t.Fatal(err)
					}
				}
				if err := writer.Close(); err != nil {
					t.Fatal(err)
				}
				if err := out.Close(); err != nil {
					t.Fatal(err)
				}
				subject = filepath.Join(t.TempDir(), "broken.tgz")
				if err := os.WriteFile(subject, changed.Bytes(), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			output := t.TempDir()
			log, err := os.Create(filepath.Join(output, "execution.log"))
			if err != nil {
				t.Fatal(err)
			}
			defer log.Close()
			r := runner{options: Options{Subject: subject, Output: output}, log: log}
			err = r.chart(t.Context())
			if (err != nil) != broken {
				diagnostics, _ := os.ReadFile(log.Name())
				t.Fatalf("contracts: %v\n%s", err, diagnostics)
			}
			if r.report.Cleanup != complete {
				t.Fatal("private extraction not cleaned")
			}
			unchanged, err := os.ReadFile(packagePath)
			if err != nil || !bytes.Equal(raw, unchanged) {
				t.Fatal("supplied package changed")
			}
		})
	}
}
