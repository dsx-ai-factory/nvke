// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package stackinstall

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"sort"
	"strings"
	"text/template"
	"unicode"

	"github.com/dsx-ai-factory/nvke/cli/internal/artifacts"
	"github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/controllerdeploy"
	"github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/sitespec"
)

// Target is the cluster the stack installs on and its platform, such as
// "linux/amd64" or "linux/arm64".
type Target struct {
	Kind     TargetKind
	Platform string
}

// Inputs is everything Resolve reads. Artifacts is the decoded artifact file.
type Inputs struct {
	Site      *sitespec.SiteSpec
	Stack     StackConfig
	Artifacts *artifacts.Release
	Target    Target
}

// HelmRelease is one release: its name, the artifact output that holds the
// chart, the embedded values files in order, and the values overlay passed
// last (nil when the chart takes no input-derived values).
type HelmRelease struct {
	Name          string
	Chart         artifacts.Output
	ValuesFiles   []string
	ValuesOverlay map[string]any
}

// Manifest is one glue manifest. Path is the embedded path with ".tmpl"
// dropped; Content is rendered.
type Manifest struct {
	Path    string
	Content []byte
}

// NVKEControllerStack is what one target installs: its Helm releases and glue
// manifests. Releases are sorted by name and Manifests by path.
type NVKEControllerStack struct {
	Releases  []HelmRelease
	Manifests []Manifest
}

// placeholderTrustDomain is the trust domain values/spire.yaml carries.
// trustDomainSuffix completes the default <clusterName>.nvke.local.
const (
	placeholderTrustDomain = "example.org"
	trustDomainSuffix      = ".nvke.local"
)

// inputs are the checked values the overlays and templates are built from.
type inputs struct {
	kind        TargetKind
	trustDomain string
	address     string
	clusterName string
	checksum    string
	upstreamCA  bool
	mirror      string
	networks    []string
	cwe         string
	capnico     string
	hasNico     bool
}

// Resolve checks the inputs and builds the stack for in.Target. It reports
// every input problem at once and returns no stack when there is one.
func Resolve(in Inputs) (*NVKEControllerStack, error) {
	v, err := check(in)
	if err != nil {
		return nil, err
	}
	p := &NVKEControllerStack{}
	for _, c := range charts {
		if !c.planned(v.kind, v.hasNico) {
			continue
		}
		rel := HelmRelease{Name: c.release, Chart: *in.Artifacts.Output(c.output)}
		if c.values != "" {
			rel.ValuesFiles = valuesFiles(c.values, v.kind)
			if rel.ValuesOverlay, err = valuesOverlay(c.values, v); err != nil {
				return nil, fmt.Errorf("%s: %w", c.release, err)
			}
		}
		p.Releases = append(p.Releases, rel)
	}
	sort.Slice(p.Releases, func(i, j int) bool { return p.Releases[i].Name < p.Releases[j].Name })
	if p.Manifests, err = renderManifests(v); err != nil {
		return nil, err
	}
	return p, nil
}

func check(in Inputs) (inputs, error) {
	if in.Site == nil || in.Artifacts == nil {
		return inputs{}, errors.New("the site spec and the artifact file are both required")
	}
	spec, stack := in.Site, in.Stack
	v := inputs{
		kind:        in.Target.Kind,
		trustDomain: spec.Spire.TrustDomain,
		upstreamCA:  spec.Spire.UpstreamCA,
		mirror:      stack.Mirror,
		hasNico:     in.Artifacts.Output("nico") != nil,
	}
	var problems []error
	add := func(format string, a ...any) { problems = append(problems, fmt.Errorf(format, a...)) }

	if !spec.IsNico() {
		add("site spec: infra.provider is %q; the stack install needs %q", spec.Infra.Provider, sitespec.ProviderNico)
	}
	switch {
	case v.trustDomain == placeholderTrustDomain:
		add("site spec: spire.trustDomain is the placeholder %s", placeholderTrustDomain)
	case v.trustDomain == "" && spec.ClusterName != "":
		// The same on the seed and the site, and different per site.
		v.trustDomain = spec.ClusterName + trustDomainSuffix
	}

	switch v.kind {
	case Site:
		addr, err := spec.ControllerEndpoint()
		if err != nil {
			add("site spec: %w", err)
		}
		v.address, v.clusterName = addr, spec.ClusterName
		if v.clusterName == "" {
			add("site spec: clusterName is required")
		} else if !templateSafe(v.clusterName) {
			add("site spec: clusterName %s", unsafeChar)
		}
	case Seed:
		v.address, v.clusterName = spec.Spire.ExternalAddress, sitespec.DefaultBootstrapClusterName
		if v.address == "" {
			add("site spec: spire.externalAddress is required on the seed; nodes reach the seed's SPIRE server there")
		} else if ip, err := netip.ParseAddr(v.address); err == nil && !ip.Is4() {
			add("site spec: spire.externalAddress %q must be an IPv4 address or a DNS name", v.address)
		}
	default:
		add("target kind %q is neither %q nor %q", v.kind, Seed, Site)
	}

	v.checksum = controllerdeploy.SpireTPMDirectChecksum(strings.TrimPrefix(in.Target.Platform, "linux/"))
	if v.checksum == "" {
		add("target platform %q has no SPIRE TPM attestor checksum", in.Target.Platform)
	}

	image := func(name string) string {
		out := in.Artifacts.Output(name)
		switch {
		case out == nil:
			add("artifact file: no %s output", name)
			return ""
		case imageValues(out.Reference) == nil:
			add("artifact file: %s reference %q is not <repository>@sha256:<64 hex digits>", name, out.Reference)
		}
		return out.Reference
	}
	v.cwe, v.capnico = image("cwe"), image("capnico-controller")

	seen := map[string]bool{}
	for _, c := range charts {
		if !c.planned(v.kind, v.hasNico) || seen[c.output] {
			continue
		}
		seen[c.output] = true
		if in.Artifacts.Output(c.output) == nil {
			add("artifact file: no %s output for the %s release", c.output, c.release)
		}
	}

	if len(stack.ClientNetworks) == 0 {
		add("stack config: clientNetworks is empty; nothing outside the cluster could reach the NVKE API")
	}
	for i, cidr := range stack.ClientNetworks {
		prefix, err := netip.ParsePrefix(cidr)
		if err != nil {
			add("stack config: clientNetworks[%d] %q is not a CIDR", i, cidr)
			continue
		}
		v.networks = append(v.networks, prefix.String())
	}
	if v.mirror != "" && !mirrorPrefix.MatchString(v.mirror) {
		add("stack config: mirror %q is not a host[:port][/prefix]", v.mirror)
	}
	return v, errors.Join(problems...)
}

const unsafeChar = "holds a quote, a backslash or a control character, which the templates cannot carry"

// templateSafe reports whether value can go into a template, which does no
// escaping.
func templateSafe(value string) bool {
	return !strings.ContainsAny(value, "\"\\") && !strings.ContainsFunc(value, unicode.IsControl)
}

// valuesFiles is values/<name>.yaml, then values/kind/<name>.yaml on the seed
// when it exists.
func valuesFiles(name string, kind TargetKind) []string {
	files := []string{"values/" + name + ".yaml"}
	if kind == Seed {
		kindFile := "values/kind/" + name + ".yaml"
		if _, err := fs.Stat(stackFS, kindFile); err == nil {
			files = append(files, kindFile)
		}
	}
	return files
}

// manifestInputs is what manifests/*.yaml.tmpl render against.
type manifestInputs struct {
	ClusterName    string
	ClientNetworks []string
	CAPNICoImage   string
}

// renderManifests returns the target's glue manifests, templates rendered,
// sorted by path.
func renderManifests(v inputs) ([]Manifest, error) {
	in := manifestInputs{ClusterName: v.clusterName, ClientNetworks: v.networks, CAPNICoImage: v.capnico}
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
		if err := tmpl.Execute(&b, in); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		out = append(out, Manifest{Path: strings.TrimSuffix(path, ".tmpl"), Content: b.Bytes()})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, err
}
