// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nvkrrelease

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const helmChartMediaType = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"

// ChartLayer returns the chart archive layer of an OCI image manifest: the
// layer with the Helm chart content media type, else the first layer. The
// file name is the layer's title annotation when that is a plain file name,
// else fallback.
func ChartLayer(manifest []byte, fallback string) (layer ocispec.Descriptor, file string, err error) {
	var doc ocispec.Manifest
	if err := json.Unmarshal(manifest, &doc); err != nil {
		return layer, "", fmt.Errorf("cannot read the OCI manifest: %w", err)
	}
	if len(doc.Layers) == 0 {
		return layer, "", errors.New("manifest has no layers")
	}
	layer = doc.Layers[0]
	for _, candidate := range doc.Layers {
		if candidate.MediaType == helmChartMediaType {
			layer = candidate
			break
		}
	}
	if layer.Digest == "" {
		return layer, "", errors.New("the chart layer has no digest")
	}
	// The title becomes a file name under the chart cache, so anything that
	// is not a plain file name falls back: the archive is verified by digest
	// either way.
	file = layer.Annotations[ocispec.AnnotationTitle]
	if file == "" || file == "." || file == ".." || strings.ContainsAny(file, "/\\") {
		file = fallback
	}
	return layer, file, nil
}

// FindLayout returns the OCI layout under dir whose index lists a manifest
// with digest. dir is a completed NVKR Run (.nvkr/runs/<attempt>), which holds
// one layout per artifact under packages/, or a directory of layouts: dir
// itself, its direct subdirectories and those of dir/packages are searched.
// Layouts are found by digest, never by name. Exactly one must match.
func FindLayout(dir, digest string) (string, error) {
	candidates := []string{dir}
	for _, parent := range []string{dir, filepath.Join(dir, "packages")} {
		entries, err := os.ReadDir(parent)
		if err != nil {
			if parent == dir {
				return "", err
			}
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() {
				candidates = append(candidates, filepath.Join(parent, entry.Name()))
			}
		}
	}
	var found []string
	for _, candidate := range candidates {
		data, err := os.ReadFile(filepath.Join(candidate, "index.json"))
		if err != nil {
			continue
		}
		var index ocispec.Index
		if err := json.Unmarshal(data, &index); err != nil {
			return "", fmt.Errorf("%s/index.json: %w", candidate, err)
		}
		for _, manifest := range index.Manifests {
			if manifest.Digest.String() == digest {
				found = append(found, candidate)
				break
			}
		}
	}
	switch len(found) {
	case 0:
		return "", fmt.Errorf("no OCI layout under %s lists a manifest with digest %s", dir, digest)
	case 1:
		return found[0], nil
	default:
		return "", fmt.Errorf("several OCI layouts under %s list digest %s: %s", dir, digest, strings.Join(found, ", "))
	}
}
