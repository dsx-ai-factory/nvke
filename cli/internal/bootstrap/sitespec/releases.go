// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sitespec

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// releaseDoc is the part of a MachineImage or ClusterRelease manifest the
// pre-flight reads: what the object is, its name, and the image a ClusterRelease
// boots.
type releaseDoc struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		MachineImageRef struct {
			Name string `yaml:"name"`
		} `yaml:"machineImageRef"`
	} `yaml:"spec"`
}

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
		dec := yaml.NewDecoder(bytes.NewReader(data))
		for {
			var doc releaseDoc
			if err := dec.Decode(&doc); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				v.addf("%s %q is not valid YAML: %v", pathBootstrapReleases, file, err)
				return
			}
			switch {
			case doc.Kind == "ClusterRelease" && doc.Metadata.Name == profile:
				foundRelease, releaseImage = true, doc.Spec.MachineImageRef.Name
			case doc.Kind == "MachineImage":
				images[doc.Metadata.Name] = true
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
