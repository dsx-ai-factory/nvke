// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sitespec

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	enginev1 "github.com/dsx-ai-factory/nvke/management-engine/apis/v1"
)

// releaseFiles lists the manifest files bootstrapReleases names: the file
// itself, or the .yaml, .yml and .json files directly inside the directory, as
// `kubectl apply -f <dir>` reads it.
func releaseFiles(path string) ([]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return []string{path}, nil
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, e := range entries {
		switch strings.ToLower(filepath.Ext(e.Name())) {
		case ".yaml", ".yml", ".json":
			if !e.IsDir() {
				files = append(files, filepath.Join(path, e.Name()))
			}
		}
	}
	sort.Strings(files)
	return files, nil
}

// checkBootstrapReleases checks that the release manifests the bootstrap
// applies to the Kind cluster hold the ClusterRelease the control plane's
// osProfile names and the MachineImage that release boots. The controller
// resolves both from its own API when it creates the first node, so a missing
// one otherwise surfaces only after the bootstrap cluster is built.
func (v *validator) checkBootstrapReleases(inv *SiteSpec) {
	if !inv.IsNico() || inv.BootstrapReleases == "" || inv.Cluster.Nico == nil || inv.Cluster.Nico.ControlPlane == nil {
		return
	}
	profile := inv.Cluster.Nico.ControlPlane.OSProfile
	path := inv.ResolvePath(inv.BootstrapReleases)
	files, err := releaseFiles(path)
	if err != nil {
		v.addf("%s %q cannot be read: %v", pathBootstrapReleases, inv.BootstrapReleases, err)
		return
	}

	releaseImage, images := "", map[string]bool{}
	foundRelease := false
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			v.addf("%s %q cannot be read: %v", pathBootstrapReleases, file, err)
			return
		}
		dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
		for {
			var raw json.RawMessage
			if err := dec.Decode(&raw); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				v.addf("%s %q is not valid YAML: %v", pathBootstrapReleases, file, err)
				return
			}
			var meta metav1.TypeMeta
			if err := json.Unmarshal(raw, &meta); err != nil {
				v.addf("%s %q is not a Kubernetes manifest: %v", pathBootstrapReleases, file, err)
				return
			}
			switch meta.Kind {
			case "ClusterRelease":
				var release enginev1.ClusterRelease
				if err := json.Unmarshal(raw, &release); err != nil {
					v.addf("%s %q holds an invalid ClusterRelease: %v", pathBootstrapReleases, file, err)
					return
				}
				if release.Name == profile {
					foundRelease, releaseImage = true, string(release.Spec.MachineImageRef.Name)
				}
			case "MachineImage":
				var image enginev1.MachineImage
				if err := json.Unmarshal(raw, &image); err != nil {
					v.addf("%s %q holds an invalid MachineImage: %v", pathBootstrapReleases, file, err)
					return
				}
				images[image.Name] = true
			}
		}
	}
	switch {
	case !foundRelease:
		v.addf("%s %q holds no ClusterRelease named %q, the cluster.nico.controlPlane.osProfile; the controller cannot create a node without it",
			pathBootstrapReleases, inv.BootstrapReleases, profile)
	case !images[releaseImage]:
		v.addf("%s %q holds ClusterRelease %q but not the MachineImage %q it names", pathBootstrapReleases, inv.BootstrapReleases, profile, releaseImage)
	}
}
