// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package catalogtemplate renders the AICR catalog template (aicr-catalog/) into a catalog directory.
// The template carries NVKE's default chart locations and versions; a release description overrides the ones
// it names. The result is a data tree `aicr recipe --data` and `aicr bundle --data` take as is.
package catalogtemplate

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/nvkrrelease"
)

const (
	// DescriptorFile names the template's own description beside registry.yaml.
	DescriptorFile = "catalog.yaml"
	// APIVersion is the descriptor's schema.
	APIVersion = "nvke.dev/aicr-catalog/v1"
	// UnpublishedHost marks a chart location NVKE has no default home for: it must come from a release.
	UnpublishedHost = "nvke.invalid"

	chartKind = "helm-oci"
)

// Descriptor is catalog.yaml: which release outputs may replace what.
type Descriptor struct {
	APIVersion string      `yaml:"apiVersion"`
	Version    string      `yaml:"version"`
	Charts     []ChartRule `yaml:"charts"`
	Images     []ImageRule `yaml:"images"`
}

// ChartRule replaces a component's chart location and version with a release output's.
type ChartRule struct {
	Component string `yaml:"component"`
	// Overlay, when set, names the overlay whose componentRef takes the chart; otherwise registry.yaml does.
	Overlay string `yaml:"overlay"`
	Output  string `yaml:"output"`
}

// ImageRule replaces an image reference in one manifest with a release output's repository and digest.
type ImageRule struct {
	File   string `yaml:"file"`
	Ref    string `yaml:"ref"`
	Output string `yaml:"output"`
}

// Options says what to render.
type Options struct {
	// TemplateDir is the template (aicr-catalog/).
	TemplateDir string
	// OutDir receives the catalog. It must not exist or must be empty.
	OutDir string
	// Target is the overlay the catalog is for (nvke-seed, nvke-site). Only its components must be resolvable.
	Target string
	// Release is optional. Without one the template's defaults stand.
	Release *nvkrrelease.Release
	// Arch selects the architecture of an image output (linux/amd64, linux/arm64).
	Arch string
}

// Result lists what the release changed, one line each, in rule order.
type Result struct {
	Replaced []string
}

// LoadDescriptor reads and validates the template's catalog.yaml.
func LoadDescriptor(templateDir string) (*Descriptor, error) {
	data, err := os.ReadFile(filepath.Join(templateDir, DescriptorFile))
	if err != nil {
		return nil, fmt.Errorf("reading the catalog template: %w", err)
	}
	var d Descriptor
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", DescriptorFile, err)
	}
	if d.APIVersion != APIVersion {
		return nil, fmt.Errorf("%s: apiVersion %q is not %q", DescriptorFile, d.APIVersion, APIVersion)
	}
	for i, c := range d.Charts {
		if c.Component == "" || c.Output == "" {
			return nil, fmt.Errorf("%s: charts[%d] needs component and output", DescriptorFile, i)
		}
	}
	for i, im := range d.Images {
		if im.File == "" || im.Ref == "" || im.Output == "" {
			return nil, fmt.Errorf("%s: images[%d] needs file, ref and output", DescriptorFile, i)
		}
	}
	return &d, nil
}

// Render copies the template to OutDir, applies the release's outputs, and fails when a component the target
// deploys still has an unpublished chart location.
func Render(opts Options) (Result, error) {
	var res Result
	desc, err := LoadDescriptor(opts.TemplateDir)
	if err != nil {
		return res, err
	}
	if opts.Target == "" {
		return res, errors.New("a target overlay is required")
	}
	if err := prepareOut(opts.OutDir); err != nil {
		return res, err
	}
	if err := copyTree(opts.TemplateDir, opts.OutDir); err != nil {
		return res, err
	}
	if opts.Release != nil {
		if res.Replaced, err = applyRelease(opts, desc); err != nil {
			return res, err
		}
	}
	if err := checkPublished(opts.OutDir, opts.Target); err != nil {
		return res, err
	}
	return res, nil
}

func prepareOut(dir string) error {
	entries, err := os.ReadDir(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return os.MkdirAll(dir, 0o755)
	case err != nil:
		return err
	case len(entries) > 0:
		return fmt.Errorf("catalog output directory %s is not empty", dir)
	}
	return nil
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s is not a regular file", p)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}

func applyRelease(opts Options, desc *Descriptor) ([]string, error) {
	var replaced []string
	for _, rule := range desc.Charts {
		art, ok := opts.Release.Artifact(rule.Output)
		if !ok {
			continue
		}
		if art.Kind != chartKind {
			return nil, fmt.Errorf("release output %q is a %s, but component %s needs a %s", rule.Output, art.Kind, rule.Component, chartKind)
		}
		if art.ChartVersion == "" || art.Reference == "" {
			return nil, fmt.Errorf("release output %q has no chart reference or version", rule.Output)
		}
		repo := art.Repository()
		source := "oci://" + path.Dir(repo)
		chart := path.Base(repo)
		where := "registry.yaml"
		if rule.Overlay != "" {
			where = "overlays/" + rule.Overlay + ".yaml"
		}
		if err := setChart(opts.OutDir, rule, source, chart, art.ChartVersion); err != nil {
			return nil, fmt.Errorf("%s in %s: %w", rule.Component, where, err)
		}
		replaced = append(replaced, fmt.Sprintf("%s (%s): %s %s from release output %s", rule.Component, where, repo, art.ChartVersion, rule.Output))
	}
	for _, rule := range desc.Images {
		art, err := opts.Release.Select(rule.Output, opts.Arch)
		if err != nil {
			if errors.Is(err, nvkrrelease.ErrNotPublished) {
				continue
			}
			return nil, fmt.Errorf("release output %q: %w", rule.Output, err)
		}
		p := filepath.Join(opts.OutDir, filepath.FromSlash(rule.File))
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		if !strings.Contains(string(data), rule.Ref) {
			return nil, fmt.Errorf("%s does not contain the image %s the template says the release replaces", rule.File, rule.Ref)
		}
		pinned := art.Repository() + "@" + art.Digest
		if err := os.WriteFile(p, []byte(strings.ReplaceAll(string(data), rule.Ref, pinned)), 0o644); err != nil {
			return nil, err
		}
		replaced = append(replaced, fmt.Sprintf("%s: image %s -> %s", rule.File, rule.Ref, pinned))
	}
	return replaced, nil
}

// setChart rewrites one chart location in registry.yaml or an overlay, keeping the file's comments.
func setChart(dir string, rule ChartRule, source, chart, version string) error {
	if rule.Overlay == "" {
		return editYAML(filepath.Join(dir, "registry.yaml"), func(root *yaml.Node) error {
			item := findNamed(child(root, "components"), rule.Component)
			if item == nil {
				return fmt.Errorf("no component %q", rule.Component)
			}
			helm := child(item, "helm")
			if helm == nil {
				return fmt.Errorf("component %q has no helm section", rule.Component)
			}
			setScalar(helm, "defaultRepository", source)
			setScalar(helm, "defaultChart", chart)
			setScalar(helm, "defaultVersion", version)
			return nil
		})
	}
	return editYAML(filepath.Join(dir, "overlays", rule.Overlay+".yaml"), func(root *yaml.Node) error {
		item := findNamed(child(child(root, "spec"), "componentRefs"), rule.Component)
		if item == nil {
			return fmt.Errorf("no componentRef %q", rule.Component)
		}
		setScalar(item, "source", source)
		setScalar(item, "chart", chart)
		setScalar(item, "version", version)
		return nil
	})
}

func readYAML(file string) (*yaml.Node, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", file, err)
	}
	if len(doc.Content) == 0 {
		return nil, fmt.Errorf("%s is empty", file)
	}
	return &doc, nil
}

// viewYAML reads a file and hands its root to view; the file is not written back.
func viewYAML(file string, view func(root *yaml.Node) error) error {
	doc, err := readYAML(file)
	if err != nil {
		return err
	}
	return view(doc.Content[0])
}

func editYAML(file string, edit func(root *yaml.Node) error) error {
	doc, err := readYAML(file)
	if err != nil {
		return err
	}
	if err := edit(doc.Content[0]); err != nil {
		return err
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	return os.WriteFile(file, out, 0o644)
}

func child(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

func findNamed(seq *yaml.Node, name string) *yaml.Node {
	if seq == nil || seq.Kind != yaml.SequenceNode {
		return nil
	}
	for _, item := range seq.Content {
		if n := child(item, "name"); n != nil && n.Value == name {
			return item
		}
	}
	return nil
}

func setScalar(m *yaml.Node, key, value string) {
	if v := child(m, key); v != nil {
		v.Kind, v.Tag, v.Value, v.Style = yaml.ScalarNode, "!!str", value, yaml.DoubleQuotedStyle
		return
	}
	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value, Style: yaml.DoubleQuotedStyle})
}

// checkPublished fails when a component the target deploys still has a chart location NVKE has no default for.
func checkPublished(dir, target string) error {
	reg := map[string]string{}
	if err := viewYAML(filepath.Join(dir, "registry.yaml"), func(root *yaml.Node) error {
		for _, item := range child(root, "components").Content {
			helm := child(item, "helm")
			if name, repo := child(item, "name"), child(helm, "defaultRepository"); name != nil && repo != nil {
				reg[name.Value] = repo.Value
			}
		}
		return nil
	}); err != nil {
		return err
	}
	chain, err := overlayChain(dir, target)
	if err != nil {
		return err
	}
	effective := map[string]string{}
	var order []string
	for _, ov := range chain {
		for name, src := range ov {
			if _, seen := effective[name]; !seen {
				order = append(order, name)
			}
			if src != "" || effective[name] == "" {
				effective[name] = src
			}
		}
	}
	var missing []string
	for _, name := range order {
		src := effective[name]
		if src == "" {
			src = reg[name]
		}
		if strings.Contains(src, UnpublishedHost) {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%s has no published chart for %s: take them from a release (--release) that carries their outputs", target, strings.Join(missing, ", "))
	}
	return nil
}

// overlayChain returns each overlay from the root of target's base chain down to target as component -> source.
func overlayChain(dir, target string) ([]map[string]string, error) {
	var chain []map[string]string
	seen := map[string]bool{}
	for name := target; name != "" && name != "base"; {
		if seen[name] {
			return nil, fmt.Errorf("overlay %s extends itself", name)
		}
		seen[name] = true
		refs := map[string]string{}
		var next string
		if err := viewYAML(filepath.Join(dir, "overlays", name+".yaml"), func(root *yaml.Node) error {
			spec := child(root, "spec")
			if b := child(spec, "base"); b != nil {
				next = b.Value
			}
			for _, item := range child(spec, "componentRefs").Content {
				n, s := child(item, "name"), child(item, "source")
				if n == nil {
					continue
				}
				refs[n.Value] = ""
				if s != nil {
					refs[n.Value] = s.Value
				}
			}
			return nil
		}); err != nil {
			return nil, fmt.Errorf("overlay %s: %w", name, err)
		}
		chain = append([]map[string]string{refs}, chain...)
		name = next
	}
	return chain, nil
}
