// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package stackinstall

import (
	"bytes"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"text/template"
)

// templateContext is what manifests/*.yaml.tmpl render against.
type templateContext struct {
	ClusterName    string
	ClientNetworks []string
	CAPNICoImage   string
}

// manifests returns the target's glue manifests, templates rendered, sorted
// by path.
func manifests(v inputs) ([]Manifest, error) {
	ctx := templateContext{ClusterName: v.clusterName, ClientNetworks: v.networks, CAPNICoImage: v.capnico}
	var out []Manifest
	err := fs.WalkDir(stackFS, "manifests", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !manifestPlanned(path, v.kind, v.hasNico) {
			return err
		}
		data, err := fs.ReadFile(stackFS, path)
		if err != nil {
			return err
		}
		if !strings.HasSuffix(path, ".tmpl") {
			out = append(out, Manifest{Path: path, Content: data})
			return nil
		}
		tmpl, err := template.New(path).Option("missingkey=error").Parse(string(data))
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		var b bytes.Buffer
		if err := tmpl.Execute(&b, ctx); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		out = append(out, Manifest{Path: strings.TrimSuffix(path, ".tmpl"), Content: b.Bytes()})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, err
}
