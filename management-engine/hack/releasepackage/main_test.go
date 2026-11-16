// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T) options {
	t.Helper()
	dir := t.TempDir()
	o := options{
		chart: "../../chart", receipts: dir, repository: "nvcr.io/example/team", version: "v0.3.0-rc.1",
		revision: strings.Repeat("a", 40), output: filepath.Join(dir, "staged"), pullSecret: "pull-credentials",
	}
	for _, component := range components {
		writeReceipt(t, o, component, Receipt{
			o.revision, o.version, o.repository + "/cluster-service-" + component,
			"sha256:" + strings.Repeat("b", 64), []string{"arm64", "amd64"},
		})
	}
	return o
}

func writeReceipt(t *testing.T, o options, component string, receipt Receipt) {
	t.Helper()
	data, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(o.receipts, component+".json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPreparePairsChartAndAllImages(t *testing.T) {
	o := fixture(t)
	if err := prepare(o); err != nil {
		t.Fatal(err)
	}
	meta, err := readYAML(filepath.Join(o.output, "Chart.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if meta["name"] != "cluster-service" || meta["version"] != "0.3.0-rc.1" || meta["appVersion"] != o.version {
		t.Fatalf("unexpected chart metadata: %v", meta)
	}
	values, err := readYAML(filepath.Join(o.output, "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"manager", "tenantAPI", "operatorAPI"} {
		workload := values[name].(map[string]any)
		image := workload["image"].(map[string]any)
		if !strings.Contains(image["repository"].(string), "@sha256:") || image["tag"] != "" {
			t.Fatalf("image is not digest-pinned: %v", image)
		}
		if name != "manager" && workload["enabled"] != false {
			t.Fatal("packaging must not enable APIs")
		}
	}
	if _, err := os.Stat(filepath.Join(o.output, "release.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(o.output, "files/crds/service.nvke.nvidia.com_nvkeclusters.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := prepare(o); err == nil {
		t.Fatal("reused staging directory was accepted")
	}
}

func TestRejectInconsistentOrMissingReceipts(t *testing.T) {
	for _, field := range []string{
		"revision", "version", "image", "digest", "architectures", "missing", "development-version",
	} {
		t.Run(field, func(t *testing.T) {
			o := fixture(t)
			r := Receipt{
				o.revision, o.version, o.repository + "/cluster-service-manager",
				"sha256:" + strings.Repeat("b", 64), []string{"amd64", "arm64"},
			}
			switch field {
			case "revision":
				r.Revision = strings.Repeat("c", 40)
			case "version":
				r.Version = "v0.2.0"
			case "image":
				r.Image = "nvcr.io/another/cluster-service-manager"
			case "digest":
				r.Digest = "latest"
			case "architectures":
				r.Architectures = []string{"amd64"}
			case "missing":
				if err := os.Remove(filepath.Join(o.receipts, "operator-api.json")); err != nil {
					t.Fatal(err)
				}
			case "development-version":
				o.version = "v0.0.0-sha.g" + strings.Repeat("c", 40)
			}
			writeReceipt(t, o, "manager", r)
			if err := prepare(o); err == nil {
				t.Fatal("inconsistent inputs were accepted")
			}
			if _, err := os.Stat(o.output); !os.IsNotExist(err) {
				t.Fatal("invalid inputs created a staging directory")
			}
		})
	}
}

func TestPreparedChartRendersDigestPinnedWorkloads(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("Helm render is exercised by the chart-check job with pinned Helm")
	}
	o := fixture(t)
	if err := prepare(o); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(helm, "template", "cluster-service", o.output,
		"--namespace", "cluster-service-system", "--set", "metrics.enabled=false",
		"--set", "manager.nvkeController.endpoint=https://backend.example.invalid",
		"--set", "manager.infrastructureTemplateConfigMap=templates",
		"--set", "tenantAPI.enabled=true", "--set", "tenantAPI.namespace=tenant-example", "--set", "operatorAPI.enabled=true")
	rendered, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("render: %v\n%s", err, rendered)
	}
	if count := strings.Count(string(rendered), "kind: CustomResourceDefinition"); count != 6 {
		t.Fatalf("got %d state CRDs", count)
	}
	for _, component := range components {
		if !strings.Contains(string(rendered), o.repository+"/cluster-service-"+component+"@sha256:") {
			t.Fatalf("missing digest-pinned %s", component)
		}
	}
}

func TestComparePackagesAllowsOnlyIdenticalContents(t *testing.T) {
	makeArchive := func(name, content string, stamp int64) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), name)
		file, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		compressed := gzip.NewWriter(file)
		writer := tar.NewWriter(compressed)
		header := &tar.Header{
			Name: "cluster-service/values.yaml", Size: int64(len(content)), Mode: 0o644, ModTime: time.Unix(stamp, 0),
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
		for _, err := range []error{writer.Close(), compressed.Close(), file.Close()} {
			if err != nil {
				t.Fatal(err)
			}
		}
		return path
	}
	candidate := makeArchive("candidate.tgz", "images: one", 1)
	same := makeArchive("same.tgz", "images: one", 2)
	changed := makeArchive("changed.tgz", "images: two", 1)
	if err := comparePackages(candidate, same); err != nil {
		t.Fatal(err)
	}
	if err := comparePackages(candidate, changed); err == nil {
		t.Fatal("changed published package accepted")
	}
}
