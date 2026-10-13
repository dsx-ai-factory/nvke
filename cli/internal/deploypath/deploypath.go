// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package deploypath reads a deploy path: a directory holding nvke-deploy.yaml
// and one directory per item, as documented in
// docs/site-bootstrap/deploy-repository.md. It parses the item list and an
// item's release.yaml, and writes release.yaml back in the same grammar the
// shell-side reader (deploy/lib/nvke-deploy-items.py) accepts: one `key: value`
// per line under chart: and release:, `- key=path` under chart.setFile:,
// `- file` under values:, and one flow mapping per line under requires:.
package deploypath

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Item types a deploy path lists.
const (
	TypeTemplate = "template"
	TypeManifest = "manifest"
	TypeHelm     = "helm"
	TypeArgoCD   = "argocd"
)

// tokenPattern is an item name: letters, digits, `-`, `_` and inner dots.
// `.` and `..` are not names, and the shell reader accepts the same set.
var tokenPattern = regexp.MustCompile(`^[\w-]+(\.[\w-]+)*$`)

// Item is one entry of nvke-deploy.yaml.
type Item struct {
	Name string `yaml:"name"`
	Type string `yaml:"type"`
}

type itemList struct {
	Items []Item `yaml:"items"`
}

// Items reads nvke-deploy.yaml under dir and returns its items in order.
func Items(dir string) ([]Item, error) {
	path := filepath.Join(dir, "nvke-deploy.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var list itemList
	if err := decoder.Decode(&list); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(list.Items) == 0 {
		return nil, fmt.Errorf("%s: no items", path)
	}
	for _, item := range list.Items {
		if !tokenPattern.MatchString(item.Name) {
			return nil, fmt.Errorf("%s: item %q: names are tokens", path, item.Name)
		}
		switch item.Type {
		case TypeTemplate, TypeManifest, TypeHelm, TypeArgoCD:
		default:
			return nil, fmt.Errorf("%s: item %q has unknown type %q", path, item.Name, item.Type)
		}
	}
	return list.Items, nil
}

// MarshalItems writes nvke-deploy.yaml's item list in the grammar the
// shell-side reader accepts, with a header naming the render.
func MarshalItems(items []Item, header string) []byte {
	var b strings.Builder
	if header != "" {
		for _, line := range strings.Split(strings.TrimRight(header, "\n"), "\n") {
			fmt.Fprintf(&b, "# %s\n", line)
		}
	}
	b.WriteString("items:\n")
	for _, item := range items {
		fmt.Fprintf(&b, "  - {name: %s, type: %s}\n", item.Name, item.Type)
	}
	return []byte(b.String())
}

// Chart is the chart: section of a helm item's release.yaml.
type Chart struct {
	Name       string `yaml:"name"`
	Version    string `yaml:"version"`
	Repository string `yaml:"repository,omitempty"`
	Path       string `yaml:"path,omitempty"`
	// Artifact, ImageArtifact and ProfilesArtifact bind the item to the
	// artifacts of an NVKR release: the chart (default: the item name), the
	// image the chart runs, and its profiles content image.
	Artifact         string `yaml:"artifact,omitempty"`
	ImageArtifact    string `yaml:"imageArtifact,omitempty"`
	ProfilesArtifact string `yaml:"profilesArtifact,omitempty"`
	// SetFile lists <helm key>=<absolute path> entries the executor passes
	// as --set-file, so a file the operator holds is read at install time
	// and never copied into the deploy path.
	SetFile []string `yaml:"setFile,omitempty"`
}

// HelmRelease is the release: section.
type HelmRelease struct {
	Name      string `yaml:"name"`
	Namespace string `yaml:"namespace"`
}

// Require is one requires: entry: an object that must exist and reach a
// condition before the item installs.
type Require struct {
	Kind      string `yaml:"kind"`
	Name      string `yaml:"name"`
	Namespace string `yaml:"namespace"`
	Condition string `yaml:"condition"`
}

// Release is an item's release.yaml. A helm item fills Chart, HelmRelease,
// Values and Requires; a manifest item fills Image and Artifact.
type Release struct {
	Chart    *Chart       `yaml:"chart,omitempty"`
	Release  *HelmRelease `yaml:"release,omitempty"`
	Values   []string     `yaml:"values,omitempty"`
	Requires []Require    `yaml:"requires,omitempty"`
	// Image is the image a manifest item's committed YAML runs, as
	// repository:tag; Artifact is the NVKR artifact that publishes it.
	Image    string `yaml:"image,omitempty"`
	Artifact string `yaml:"artifact,omitempty"`
}

// ReadRelease parses release.yaml under an item directory.
func ReadRelease(itemDir string) (*Release, error) {
	path := filepath.Join(itemDir, "release.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var r Release
	if err := decoder.Decode(&r); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := r.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &r, nil
}

func (r *Release) validate() error {
	if r.Chart == nil {
		if r.Image != "" {
			last := r.Image[strings.LastIndex(r.Image, "/")+1:]
			name, tag, ok := strings.Cut(last, ":")
			if !ok || name == "" || tag == "" || strings.Contains(r.Image, "@") {
				return fmt.Errorf("image must be <repository>:<tag>, with no digest: %s", r.Image)
			}
		}
		return nil
	}
	if r.Chart.Name == "" || r.Chart.Version == "" {
		return errors.New("chart.name and chart.version are required")
	}
	if r.Chart.Repository != "" && r.Chart.Path != "" {
		return errors.New("chart has both repository and path; name one source")
	}
	if r.Release == nil || r.Release.Name == "" || r.Release.Namespace == "" {
		return errors.New("release.name and release.namespace are required")
	}
	for _, entry := range r.Chart.SetFile {
		key, path, ok := strings.Cut(entry, "=")
		if !ok || key == "" || !filepath.IsAbs(path) {
			return fmt.Errorf("chart.setFile entry must be <helm key>=<absolute path>: %s", entry)
		}
	}
	return nil
}

// Marshal writes release.yaml in the grammar the shell-side reader accepts.
func (r *Release) Marshal() []byte {
	var b strings.Builder
	if r.Chart != nil {
		b.WriteString("chart:\n")
		for _, kv := range [][2]string{
			{"name", r.Chart.Name},
			{"version", quote(r.Chart.Version)},
			{"repository", r.Chart.Repository},
			{"path", r.Chart.Path},
			{"artifact", r.Chart.Artifact},
			{"imageArtifact", r.Chart.ImageArtifact},
			{"profilesArtifact", r.Chart.ProfilesArtifact},
		} {
			if kv[1] != "" {
				fmt.Fprintf(&b, "  %s: %s\n", kv[0], kv[1])
			}
		}
		if len(r.Chart.SetFile) > 0 {
			b.WriteString("  setFile:\n")
			for _, entry := range r.Chart.SetFile {
				fmt.Fprintf(&b, "    - %s\n", entry)
			}
		}
	}
	if r.Release != nil {
		fmt.Fprintf(&b, "release:\n  name: %s\n  namespace: %s\n", r.Release.Name, r.Release.Namespace)
	}
	if len(r.Values) > 0 {
		b.WriteString("values:\n")
		for _, v := range r.Values {
			fmt.Fprintf(&b, "  - %s\n", v)
		}
	}
	if len(r.Requires) > 0 {
		b.WriteString("requires:\n")
		for _, q := range r.Requires {
			fmt.Fprintf(&b, "  - {kind: %s, name: %s, namespace: %s, condition: %s}\n", q.Kind, q.Name, q.Namespace, q.Condition)
		}
	}
	if r.Image != "" {
		fmt.Fprintf(&b, "image: %s\n", r.Image)
	}
	if r.Artifact != "" && r.Chart == nil {
		fmt.Fprintf(&b, "artifact: %s\n", r.Artifact)
	}
	return []byte(b.String())
}

// quote keeps a version a string, as the committed files do.
func quote(value string) string {
	return `"` + value + `"`
}
