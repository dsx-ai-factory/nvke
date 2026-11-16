// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// releasepackage prepares a Cluster Service chart from one build's image receipts.
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"sigs.k8s.io/yaml"
)

const manager = "manager"

var components = []string{manager, "tenant-api", "operator-api"}
var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
var revisionPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
var versionPattern = regexp.MustCompile(
	`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-rc\.(0|[1-9][0-9]*)|-sha\.g[0-9a-f]{40})?$`,
)

// Receipt identifies the multi-architecture image produced by this workflow run.
type Receipt struct {
	Revision      string   `json:"revision"`
	Version       string   `json:"version"`
	Image         string   `json:"image"`
	Digest        string   `json:"digest"`
	Architectures []string `json:"architectures"`
}

type options struct{ chart, receipts, repository, version, revision, output, pullSecret string }

func main() {
	var o options
	compare := flag.String("compare", "", "Compare this chart archive with -other, ignoring archive timestamps")
	other := flag.String("other", "", "Previously published chart archive")
	flag.StringVar(&o.chart, "chart", "chart", "Generated chart source")
	flag.StringVar(&o.receipts, "receipts", "", "Directory containing the three image receipts")
	flag.StringVar(&o.repository, "repository", "", "Destination registry/org, without a scheme")
	flag.StringVar(&o.version, "version", "", "Image version shared by this build")
	flag.StringVar(&o.revision, "revision", "", "Full source commit")
	flag.StringVar(&o.output, "output", "", "New staging directory")
	flag.StringVar(&o.pullSecret, "pull-secret", "", "Existing destination image-pull Secret name")
	flag.Parse()
	var err error
	if *compare != "" {
		err = comparePackages(*compare, *other)
	} else {
		err = prepare(o)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func readReceipts(o options) (map[string]Receipt, error) {
	if !versionPattern.MatchString(o.version) || !revisionPattern.MatchString(o.revision) {
		return nil, errors.New("a full source revision and vX.Y.Z, vX.Y.Z-rc.N or vX.Y.Z-sha.g<commit> version are required")
	}
	if strings.Contains(o.version, "-sha.g") && !strings.HasSuffix(o.version, o.revision) {
		return nil, errors.New("development version does not match the source revision")
	}
	if o.repository == "" || strings.ContainsAny(o.repository, " \n\r\t@:") || strings.HasSuffix(o.repository, "/") {
		return nil, errors.New("repository must be a registry/org path without a scheme, tag or trailing slash")
	}
	images := make(map[string]Receipt, len(components))
	for _, component := range components {
		data, err := os.ReadFile(filepath.Join(o.receipts, component+".json"))
		if err != nil {
			return nil, fmt.Errorf("%s receipt: %w", component, err)
		}
		var receipt Receipt
		if err := json.Unmarshal(data, &receipt); err != nil {
			return nil, fmt.Errorf("%s receipt: %w", component, err)
		}
		arches := slices.Clone(receipt.Architectures)
		slices.Sort(arches)
		if receipt.Revision != o.revision || receipt.Version != o.version ||
			receipt.Image != o.repository+"/cluster-service-"+component ||
			!digestPattern.MatchString(receipt.Digest) || !slices.Equal(arches, []string{"amd64", "arm64"}) {
			return nil, fmt.Errorf("%s receipt has mismatched source, version, destination, digest or architectures", component)
		}
		images[component] = receipt
	}
	return images, nil
}

func prepare(o options) error {
	images, err := readReceipts(o)
	if err != nil {
		return err
	}
	if o.output == "" {
		return errors.New("output staging directory is required")
	}
	// Refuse reuse: stale files or receipts from another run must not enter a package.
	if err := os.Mkdir(o.output, 0o755); err != nil {
		return fmt.Errorf("create staging directory: %w", err)
	}
	if err := os.CopyFS(o.output, os.DirFS(o.chart)); err != nil {
		return err
	}
	metadata, err := readYAML(filepath.Join(o.output, "Chart.yaml"))
	if err != nil {
		return err
	}
	if metadata["name"] != "cluster-service" {
		return errors.New("expected the cluster-service chart")
	}
	metadata["version"], metadata["appVersion"] = strings.TrimPrefix(o.version, "v"), o.version
	annotations, _ := metadata["annotations"].(map[string]any)
	if annotations == nil {
		annotations = map[string]any{}
	}
	annotations["nvke.nvidia.com/source-revision"] = o.revision
	metadata["annotations"] = annotations
	if err := writeYAML(filepath.Join(o.output, "Chart.yaml"), metadata); err != nil {
		return err
	}
	values, err := readYAML(filepath.Join(o.output, "values.yaml"))
	if err != nil {
		return err
	}
	for _, pair := range [][2]string{
		{manager, manager}, {"tenantAPI", "tenant-api"}, {"operatorAPI", "operator-api"},
	} {
		workload, ok := values[pair[0]].(map[string]any)
		if !ok {
			return fmt.Errorf("chart lacks %s values; API packaging must be present", pair[0])
		}
		image, ok := workload["image"].(map[string]any)
		if !ok {
			return fmt.Errorf("chart lacks %s.image", pair[0])
		}
		receipt := images[pair[1]]
		image["repository"], image["tag"] = receipt.Image+"@"+receipt.Digest, ""
		if o.pullSecret != "" {
			workload["imagePullSecrets"] = []any{map[string]any{"name": o.pullSecret}}
		}
	}
	if err := writeYAML(filepath.Join(o.output, "values.yaml"), values); err != nil {
		return err
	}
	// This record travels inside the chart, pairing state schemas with the three images.
	record := struct {
		Revision string             `json:"revision"`
		Version  string             `json:"version"`
		Images   map[string]Receipt `json:"images"`
	}{o.revision, o.version, images}
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(o.output, "release.json"), append(data, '\n'), 0o644)
}

func readYAML(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var value map[string]any
	if err := yaml.UnmarshalStrict(data, &value); err != nil {
		return nil, err
	}
	return value, nil
}

func writeYAML(path string, value map[string]any) error {
	data, err := yaml.Marshal(value)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// Compare contents rather than gzip timestamps so a completed destination is
// reusable after another publication job fails. A changed file fails closed.
func comparePackages(candidate, published string) error {
	left, err := archiveFiles(candidate)
	if err != nil {
		return err
	}
	right, err := archiveFiles(published)
	if err != nil {
		return err
	}
	if len(left) != len(right) {
		return errors.New("published chart has different files")
	}
	for name, data := range left {
		other, exists := right[name]
		if !exists || !bytes.Equal(data, other) {
			return fmt.Errorf("published chart differs at %s", name)
		}
	}
	return nil
}

func archiveFiles(path string) (map[string][]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	reader, err := gzip.NewReader(file)
	if err != nil {
		return nil, err
	}
	defer func() { _ = reader.Close() }()
	archive := tar.NewReader(reader)
	files := map[string][]byte{}
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			return files, nil
		}
		if err != nil {
			return nil, err
		}
		if header.Typeflag == tar.TypeDir {
			continue
		}
		if header.Typeflag != tar.TypeReg || header.Size > 16<<20 {
			return nil, errors.New("unexpected chart archive entry")
		}
		if _, exists := files[header.Name]; exists {
			return nil, errors.New("duplicate chart archive entry")
		}
		data, err := io.ReadAll(archive)
		if err != nil {
			return nil, err
		}
		files[header.Name] = data
	}
}
