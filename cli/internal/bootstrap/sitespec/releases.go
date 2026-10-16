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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"

	enginev1 "github.com/dsx-ai-factory/nvke/management-engine/apis/v1"
)

// releaseAPIVersion is the apiVersion of the MachineImage and ClusterRelease
// CRDs the controller chart installs.
var releaseAPIVersion = enginev1.SchemeGroupVersion.String()

// releaseFiles lists the manifest files infra.nico.releases names: the file
// itself, or the .yaml, .yml and .json files directly inside the directory, as
// `kubectl apply -f <dir>` reads it: the extension is matched exactly, so
// release.YAML is skipped there and here.
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
		switch filepath.Ext(e.Name()) {
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
// release names and the MachineImage that release boots. The controller
// resolves both from its own API when it creates the first node, so a missing
// one otherwise surfaces only after the bootstrap cluster is built.
func (v *validator) checkBootstrapReleases(inv *SiteSpec) {
	if !inv.IsNico() || inv.Infra.Nico == nil || inv.Infra.Nico.Releases == "" {
		return
	}
	releases := inv.Infra.Nico.Releases
	profile := inv.Infra.Nico.ControlPlane.Release
	path := inv.ResolvePath(releases)
	files, err := releaseFiles(path)
	if err != nil {
		v.addf("%s %q cannot be read: %v", pathNicoReleases, releases, err)
		return
	}

	releaseImage, images := "", map[string]bool{}
	foundRelease := false
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			v.addf("%s %q cannot be read: %v", pathNicoReleases, file, err)
			return
		}
		dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
		for {
			var raw json.RawMessage
			if err := dec.Decode(&raw); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				v.addf("%s %q is not valid YAML: %v", pathNicoReleases, file, err)
				return
			}
			// An empty document, such as one holding only a comment, is skipped
			// here as kubectl skips it.
			if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
				continue
			}
			var meta metav1.TypeMeta
			if err := json.Unmarshal(raw, &meta); err != nil {
				v.addf("%s %q is not a Kubernetes manifest: %v", pathNicoReleases, file, err)
				return
			}
			// A manifest without the CRD's group and version is not one kubectl
			// can apply, whatever its kind says.
			if (meta.Kind == "ClusterRelease" || meta.Kind == "MachineImage") && meta.APIVersion != releaseAPIVersion {
				v.addf("%s %q holds a %s with apiVersion %q, want %q", pathNicoReleases, file, meta.Kind, meta.APIVersion, releaseAPIVersion)
				return
			}
			// kubectl apply refuses any other document without these, after the
			// bootstrap cluster is built: a request document for nvkectl create,
			// say, left beside the manifests.
			if meta.APIVersion == "" || meta.Kind == "" {
				v.addf("%s %q holds a document without apiVersion and kind, which kubectl cannot apply; "+
					"write each with nvkectl create --dry-run=client -o yaml", pathNicoReleases, file)
				return
			}
			switch meta.Kind {
			case "ClusterRelease":
				var release enginev1.ClusterRelease
				if err := json.Unmarshal(raw, &release); err != nil {
					v.addf("%s %q holds an invalid ClusterRelease: %v", pathNicoReleases, file, err)
					return
				}
				if release.Name == profile {
					foundRelease, releaseImage = true, string(release.Spec.MachineImageRef.Name)
				}
			case "MachineImage":
				var image enginev1.MachineImage
				if err := json.Unmarshal(raw, &image); err != nil {
					v.addf("%s %q holds an invalid MachineImage: %v", pathNicoReleases, file, err)
					return
				}
				images[image.Name] = true
			}
		}
	}
	switch {
	case !foundRelease:
		v.addf("%s %q holds no ClusterRelease named %q, the infra.nico.controlPlane.release; the controller cannot create a node without it",
			pathNicoReleases, releases, profile)
	case !images[releaseImage]:
		v.addf("%s %q holds ClusterRelease %q but not the MachineImage %q it names", pathNicoReleases, releases, profile, releaseImage)
	}
}
