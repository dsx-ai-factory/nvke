// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package stackartifacts

import (
	"encoding/json"
	"errors"
	"fmt"
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
