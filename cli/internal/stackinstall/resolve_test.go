// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package stackinstall

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/cli/internal/artifacts"
	"github.com/dsx-ai-factory/nvke/cli/internal/bootstrap/sitespec"
)

const (
	testTrustDomain = "site-a.nvke.example"
	testVIP         = "10.0.0.10"
	testSeedAddress = "192.0.2.10"
)

func testArtifacts(t *testing.T, withNico bool) *artifacts.Release {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "artifacts.json"))
	require.NoError(t, err)
	defer f.Close()
	rel, err := artifacts.Decode(f)
	require.NoError(t, err)
	if !withNico {
		rel.Outputs = slices.DeleteFunc(rel.Outputs, func(o artifacts.Output) bool {
			return o.Name == "nico" || o.Name == "nico-prereqs"
		})
	}
	return rel
}

func testInputs(t *testing.T, kind TargetKind, platform string, withNico bool) Inputs {
	t.Helper()
	return Inputs{
		Site: &sitespec.SiteSpec{
			ClusterName: "site-a",
			Infra:       sitespec.Infra{Provider: sitespec.ProviderNico},
			Cluster:     sitespec.Cluster{ControlPlaneVIP: testVIP},
			Spire:       sitespec.Spire{TrustDomain: testTrustDomain, ExternalAddress: testSeedAddress},
		},
		Stack:     StackConfig{ClientNetworks: []string{"192.0.2.0/24", "198.51.100.7/32"}},
		Artifacts: testArtifacts(t, withNico),
		Target:    Target{Kind: kind, Platform: platform},
	}
}

func mustResolve(t *testing.T, in Inputs) *NVKEControllerStack {
	t.Helper()
	p, err := Resolve(in)
	require.NoError(t, err)
	return p
}

func release(t *testing.T, p *NVKEControllerStack, name string) HelmRelease {
	t.Helper()
	for _, rel := range p.Releases {
		if rel.Name == name {
			return rel
		}
	}
	require.Failf(t, "release not in the stack", "%s", name)
	return HelmRelease{}
}

func releaseNames(p *NVKEControllerStack) []string {
	var names []string
	for _, rel := range p.Releases {
		names = append(names, rel.Name)
	}
	return names
}

func manifestPaths(p *NVKEControllerStack) []string {
	var paths []string
	for _, m := range p.Manifests {
		paths = append(paths, m.Path)
	}
	return paths
}

func manifestContent(t *testing.T, p *NVKEControllerStack, path string) string {
	t.Helper()
	for _, m := range p.Manifests {
		if m.Path == path {
			return string(m.Content)
		}
	}
	require.Failf(t, "manifest not in the stack", "%s", path)
	return ""
}

func TestResolveRefuses(t *testing.T) {
	cases := []struct {
		name   string
		kind   TargetKind
		mutate func(*Inputs)
		want   string
	}{
		{"trust domain placeholder", Site, func(in *Inputs) { in.Site.Spire.TrustDomain = "example.org" }, "placeholder example.org"},
		{"platform without TPM checksum", Site, func(in *Inputs) { in.Target.Platform = "linux/s390x" }, "no SPIRE TPM attestor checksum"},
		{"missing cwe", Site, func(in *Inputs) { dropOutput(in, "cwe") }, "no cwe output"},
		{"cwe by tag", Site, func(in *Inputs) { setReference(in, "cwe", "registry.example.invalid/nvke/cwe:1.0") }, "cwe reference"},
		{"capnico by tag", Site, func(in *Inputs) {
			setReference(in, "capnico-controller", "registry.example.invalid/nvke/capnico-controller:1.0")
		}, "is not <repository>@sha256:<64 hex digits>"},
		{"capnico reference with a comment", Site, func(in *Inputs) {
			setReference(in, "capnico-controller", "registry.example.invalid/x #@sha256:"+strings.Repeat("b", 64))
		}, "capnico-controller reference"},
		{"capnico digest too short", Site, func(in *Inputs) {
			setReference(in, "capnico-controller", "registry.example.invalid/x@sha256:"+strings.Repeat("b", 63))
		}, "capnico-controller reference"},
		{"seed with an IPv6 address", Seed, func(in *Inputs) { in.Site.Spire.ExternalAddress = "2001:db8::10" }, "must be an IPv4 address or a DNS name"},
		{"missing chart output", Site, func(in *Inputs) { dropOutput(in, "temporal") }, "no temporal output for the temporal release"},
		{"missing nktp output on the site", Site, func(in *Inputs) { dropOutput(in, "nktp") }, "no nktp output"},
		{"empty client networks", Site, func(in *Inputs) { in.Stack.ClientNetworks = nil }, "clientNetworks is empty"},
		{"client network not a CIDR", Site, func(in *Inputs) { in.Stack.ClientNetworks = []string{"192.0.2.1"} }, "clientNetworks[0] \"192.0.2.1\" is not a CIDR"},
		{"site without VIP", Site, func(in *Inputs) { in.Site.Cluster.ControlPlaneVIP = "" }, "cluster.controlPlaneVIP"},
		{"seed without external address", Seed, func(in *Inputs) { in.Site.Spire.ExternalAddress = "" }, "spire.externalAddress is required on the seed"},
		{"provider not nico", Site, func(in *Inputs) { in.Site.Infra.Provider = sitespec.ProviderProvisioned }, "the stack install needs \"nico\""},
		{"unknown target kind", "tenant", func(*Inputs) {}, "target kind \"tenant\""},
		{"cluster name with a quote", Site, func(in *Inputs) { in.Site.ClusterName = `site"a` }, "clusterName holds a quote"},
		{"cluster name unset", Site, func(in *Inputs) { in.Site.ClusterName = "" }, "clusterName is required"},
		{"mirror is a URL", Site, func(in *Inputs) { in.Stack.Mirror = "https://mirror.example.internal" }, "is not a host[:port][/prefix]"},
		{"mirror with a control character", Site, func(in *Inputs) { in.Stack.Mirror = "mirror\n" }, "is not a host[:port][/prefix]"},
		{"mirror with a trailing slash", Site, func(in *Inputs) { in.Stack.Mirror = "mirror.example.internal/" }, "is not a host[:port][/prefix]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := testInputs(t, tc.kind, "linux/amd64", true)
			tc.mutate(&in)
			p, err := Resolve(in)
			require.Error(t, err)
			assert.Nil(t, p)
			assert.Contains(t, err.Error(), tc.want)
			var joined interface{ Unwrap() []error }
			require.True(t, errors.As(err, &joined), "input problems are joined")
			assert.Len(t, joined.Unwrap(), 1, "exactly one problem: %v", err)
		})
	}
}

func TestResolveRefusesMissingInputs(t *testing.T) {
	for name, clear := range map[string]func(*Inputs){
		"site":      func(in *Inputs) { in.Site = nil },
		"artifacts": func(in *Inputs) { in.Artifacts = nil },
	} {
		in := testInputs(t, Site, "linux/amd64", true)
		clear(&in)
		_, err := Resolve(in)
		require.ErrorContains(t, err, "site spec and the artifact file are both required", name)
	}
}

func dropOutput(in *Inputs, name string) {
	in.Artifacts.Outputs = slices.DeleteFunc(in.Artifacts.Outputs, func(o artifacts.Output) bool { return o.Name == name })
}

func setReference(in *Inputs, name, ref string) {
	in.Artifacts.Output(name).Reference = ref
}

func TestSiteHasNoNico(t *testing.T) {
	p := mustResolve(t, testInputs(t, Site, "linux/amd64", true))
	assert.NotContains(t, releaseNames(p), "nico")
	assert.NotContains(t, releaseNames(p), "nico-prereqs")
	for _, path := range manifestPaths(p) {
		assert.NotContains(t, path, "manifests/nico/")
	}
}

func TestSeedWithoutNico(t *testing.T) {
	p := mustResolve(t, testInputs(t, Seed, "linux/amd64", false))
	assert.NotContains(t, releaseNames(p), "nico")
	assert.NotContains(t, releaseNames(p), "nico-prereqs")
	for _, path := range manifestPaths(p) {
		assert.NotContains(t, path, "manifests/nico/")
	}
}

func TestSeedWithNico(t *testing.T) {
	p := mustResolve(t, testInputs(t, Seed, "linux/arm64", true))
	assert.Contains(t, releaseNames(p), "nico")
	assert.Contains(t, releaseNames(p), "nico-prereqs")
	assert.Contains(t, manifestPaths(p), "manifests/nico/namespace.yaml")
	assert.Contains(t, manifestPaths(p), "manifests/nico/cluster-issuer.yaml")
}

func TestNKTPSiteOnly(t *testing.T) {
	for _, withNico := range []bool{false, true} {
		seed := mustResolve(t, testInputs(t, Seed, "linux/amd64", withNico))
		assert.NotContains(t, releaseNames(seed), "nktp")
		assert.NotContains(t, manifestPaths(seed), "manifests/nktp-cluster.yaml")
		site := mustResolve(t, testInputs(t, Site, "linux/amd64", withNico))
		assert.Contains(t, releaseNames(site), "nktp")
		assert.Contains(t, manifestPaths(site), "manifests/nktp-cluster.yaml")
	}
}

func TestReleaseCharts(t *testing.T) {
	want := map[string]string{
		"nvke-controller": "controller-chart",
		"nktp":            "nktp",
		"postgres":        "postgresql",
		"spire-crds":      "spire-crds",
		"spire":           "spire",
		"temporal":        "temporal",
		"nico":            "nico",
		"nico-prereqs":    "postgresql",
	}
	got := map[string]string{}
	for _, in := range []Inputs{testInputs(t, Seed, "linux/amd64", true), testInputs(t, Site, "linux/amd64", true)} {
		for _, rel := range mustResolve(t, in).Releases {
			got[rel.Name] = rel.Chart.Name
			assert.Equal(t, in.Artifacts.Output(rel.Chart.Name).Reference, rel.Chart.Reference)
		}
	}
	assert.Equal(t, want, got)
}

func TestSeedTakesKindLayer(t *testing.T) {
	seed := mustResolve(t, testInputs(t, Seed, "linux/amd64", true))
	assert.Equal(t, []string{"values/postgresql.yaml", "values/kind/postgresql.yaml"}, release(t, seed, "postgres").ValuesFiles)
	assert.Equal(t, []string{"values/nico-prereqs.yaml", "values/kind/nico-prereqs.yaml"}, release(t, seed, "nico-prereqs").ValuesFiles)
	assert.Equal(t, []string{"values/temporal.yaml", "values/kind/temporal.yaml"}, release(t, seed, "temporal").ValuesFiles)
	assert.Equal(t, []string{"values/nico.yaml"}, release(t, seed, "nico").ValuesFiles)
	assert.Empty(t, release(t, seed, "spire-crds").ValuesFiles)

	site := mustResolve(t, testInputs(t, Site, "linux/amd64", true))
	for _, rel := range site.Releases {
		for _, file := range rel.ValuesFiles {
			assert.NotContains(t, file, "/kind/", rel.Name)
		}
	}
}

// monitorSwitches returns the path of every enabled monitor or tracing switch.
func monitorSwitches(values map[string]any, prefix string) []string {
	var on []string
	for k, v := range values {
		m, ok := v.(map[string]any)
		if !ok {
			continue
		}
		path := prefix + k
		if (k == "serviceMonitor" || k == "podMonitor" || k == "tracing") && m["enabled"] == true {
			on = append(on, path)
		}
		on = append(on, monitorSwitches(m, path+".")...)
	}
	sort.Strings(on)
	return on
}

func TestSeedTurnsMonitorsAndTracingOff(t *testing.T) {
	seed := mustResolve(t, testInputs(t, Seed, "linux/amd64", true))
	for _, rel := range seed.Releases {
		merged, err := mergedValues(rel)
		require.NoError(t, err)
		assert.Empty(t, monitorSwitches(merged, ""), rel.Name)
	}

	site := mustResolve(t, testInputs(t, Site, "linux/amd64", true))
	want := map[string][]string{
		"nvke-controller": {"serviceMonitor", "tracing"},
		"spire":           {"spire-agent.telemetry.prometheus.podMonitor", "spire-server.telemetry.prometheus.podMonitor"},
		"temporal":        {"server.metrics.serviceMonitor"},
	}
	for name, paths := range want {
		merged, err := mergedValues(release(t, site, name))
		require.NoError(t, err)
		assert.Equal(t, paths, monitorSwitches(merged, ""), name)
	}
}

func TestAdminIDMatchesClusterSPIFFEID(t *testing.T) {
	p := mustResolve(t, testInputs(t, Site, "linux/amd64", true))
	spire := release(t, p, "spire").ValuesOverlay
	adminIDs := dig(spire, "spire-server", "adminIDs").([]string)
	require.Len(t, adminIDs, 1)

	var controller struct {
		ServiceAccount struct {
			Name string `yaml:"name"`
		} `yaml:"serviceAccount"`
		Spiffe struct {
			ServicePathSegment string `yaml:"servicePathSegment"`
		} `yaml:"spiffe"`
	}
	require.NoError(t, readValues("values/controller.yaml", &controller))
	var spireValues struct {
		Server struct {
			ControllerManager struct {
				Identities struct {
					ClusterSPIFFEIDs map[string]struct {
						SpiffeIDTemplate string `yaml:"spiffeIDTemplate"`
					} `yaml:"clusterSPIFFEIDs"`
				} `yaml:"identities"`
			} `yaml:"controllerManager"`
		} `yaml:"spire-server"`
	}
	require.NoError(t, readValues("values/spire.yaml", &spireValues))
	account, segment := controller.ServiceAccount.Name, controller.Spiffe.ServicePathSegment
	tmpl := spireValues.Server.ControllerManager.Identities.ClusterSPIFFEIDs["nvke-service"].SpiffeIDTemplate
	require.NotEmpty(t, tmpl)

	fromTemplate := strings.NewReplacer("{{.TrustDomain}}", testTrustDomain, "{{.PodSpec.ServiceAccountName}}", account).Replace(tmpl)
	assert.Equal(t, fromTemplate, adminIDs[0])
	assert.Equal(t, "spiffe://"+testTrustDomain+"/"+segment+"/"+account, adminIDs[0])
	assert.Equal(t, "spiffe://"+testTrustDomain+"/nvke-service/nvke-controller", adminIDs[0])
}

func TestTPMChecksumAMD64IsChartDefault(t *testing.T) {
	// spire chart 0.30.1, spire-server nodeAttestor.tpmDirect.checksum (plugin v1.9.0).
	const chartDefault = "46d0caad8c25a027dd11c93e18b58a8bc6fbd9f1fe2e36fa2a0dd440986de4dc"
	p := mustResolve(t, testInputs(t, Site, "linux/amd64", true))
	assert.Equal(t, chartDefault, dig(release(t, p, "spire").ValuesOverlay, "spire-server", "nodeAttestor", "tpmDirect", "checksum"))
}

func TestImageValues(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	cases := []struct {
		name string
		ref  string
		want map[string]any
	}{
		{"digest", "registry.example.invalid/nvke/cwe@" + digest, map[string]any{"repository": "registry.example.invalid/nvke/cwe", "digest": digest, "tag": ""}},
		{"tag and digest", "registry.example.invalid/nvke/cwe:1.0@" + digest, map[string]any{"repository": "registry.example.invalid/nvke/cwe", "digest": digest, "tag": ""}},
		{"registry with a port", "mirror.example.internal:5000/cwe@" + digest, map[string]any{"repository": "mirror.example.internal:5000/cwe", "digest": digest, "tag": ""}},
		{"tag only", "registry.example.invalid/nvke/cwe:1.0", nil},
		{"short digest", "registry.example.invalid/nvke/cwe@sha256:" + strings.Repeat("a", 63), nil},
		{"space in repository", "registry.example.invalid/x #@" + digest, nil},
		{"neither", "registry.example.invalid/nvke/cwe", nil},
		{"trailing slash", "registry.example.invalid/nvke/cwe/@" + digest, nil},
		{"double dot", "registry.example.invalid/nvke/c..we@" + digest, nil},
		{"port without path", "localhost:5000@" + digest, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, imageValues(tc.ref))
		})
	}
}

func TestEveryStackFileIsPlanned(t *testing.T) {
	planned := map[string]bool{}
	for _, in := range []Inputs{testInputs(t, Seed, "linux/arm64", true), testInputs(t, Site, "linux/amd64", true)} {
		p := mustResolve(t, in)
		for _, rel := range p.Releases {
			for _, file := range rel.ValuesFiles {
				planned[file] = true
			}
		}
		for _, m := range p.Manifests {
			assert.NotContains(t, string(m.Content), "{{", m.Path)
			if _, err := fs.Stat(stackFS, m.Path+".tmpl"); err == nil {
				planned[m.Path+".tmpl"] = true
			} else {
				planned[m.Path] = true
			}
		}
	}
	for _, root := range []string{"values", "manifests"} {
		require.NoError(t, fs.WalkDir(stackFS, root, func(path string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				assert.True(t, planned[path], "%s is in no stack", path)
			}
			return err
		}))
	}
}

func TestCAPNICoImageRendered(t *testing.T) {
	in := testInputs(t, Site, "linux/amd64", true)
	content := manifestContent(t, mustResolve(t, in), "manifests/capnico.yaml")
	assert.Equal(t, 1, strings.Count(content, "image: "+in.Artifacts.Output("capnico-controller").Reference+"\n"))
	assert.NotContains(t, content, "{{")
}

func TestDefaultTrustDomain(t *testing.T) {
	for _, kind := range []TargetKind{Seed, Site} {
		in := testInputs(t, kind, "linux/amd64", true)
		in.Site.Spire.TrustDomain = ""
		spire := release(t, mustResolve(t, in), "spire").ValuesOverlay
		assert.Equal(t, in.Site.ClusterName+".nvke.local", dig(spire, "global", "spire", "trustDomain"), kind)
	}
}

func TestIdentityMatchesSeedAndSite(t *testing.T) {
	inputs := func(kind TargetKind) Inputs {
		in := testInputs(t, kind, "linux/arm64", true)
		in.Site.Spire.UpstreamCA = true
		in.Stack.Mirror = "mirror.example.internal:5000/nvke"
		return in
	}
	seed, site := mustResolve(t, inputs(Seed)), mustResolve(t, inputs(Site))

	for _, rel := range site.Releases {
		if rel.Name == "nktp" {
			continue
		}
		assert.Equal(t, rel.Chart, release(t, seed, rel.Name).Chart, rel.Name)
	}
	same := map[string][][]string{
		"nvke-controller": {{"image"}, {"spiffe", "trustDomain"}},
		"spire": {
			{"global", "spire", "trustDomain"},
			{"global", "spire", "caSubject"},
			{"global", "spire", "image"},
			{"spire-server", "adminIDs"},
			{"spire-server", "upstreamAuthority"},
			{"spire-server", "nodeAttestor"},
		},
		"postgres": {{"global"}},
		"temporal": {{"server"}, {"admintools"}, {"web"}},
	}
	for name, paths := range same {
		for _, path := range paths {
			want := dig(release(t, site, name).ValuesOverlay, path...)
			require.NotNil(t, want, "%s %v", name, path)
			assert.Equal(t, want, dig(release(t, seed, name).ValuesOverlay, path...), "%s %v", name, path)
		}
	}
	differ := map[string][][]string{
		"nvke-controller": {{"spiffe", "spire", "serverAddress"}},
		"spire":           {{"global", "spire", "clusterName"}, {"spiffe-oidc-discovery-provider", "config", "jwksUri"}},
	}
	for name, paths := range differ {
		for _, path := range paths {
			assert.NotEqual(t, dig(release(t, site, name).ValuesOverlay, path...), dig(release(t, seed, name).ValuesOverlay, path...), "%s %v", name, path)
		}
	}
	assert.Equal(t, sitespec.DefaultBootstrapClusterName, dig(release(t, seed, "spire").ValuesOverlay, "global", "spire", "clusterName"))
}

// leaves returns the dotted path of every non-map value, lists included.
func leaves(values map[string]any, prefix string) []string {
	var out []string
	for k, v := range values {
		if m, ok := v.(map[string]any); ok {
			out = append(out, leaves(m, prefix+k+".")...)
			continue
		}
		out = append(out, prefix+k)
	}
	sort.Strings(out)
	return out
}

func TestValuesOverlayDoesNotRestateCommitted(t *testing.T) {
	// Lists Helm would replace whole and the placeholder the overlay must replace.
	restated := []string{
		"global.spire.trustDomain",
		"spire-server.adminIDs",
		"spiffe-oidc-discovery-provider.config.additionalDomains",
		"spiffe-oidc-discovery-provider.tls.certManager.certificate.dnsNames",
	}
	want := map[string][]string{
		"nvke-controller": {"image.digest", "image.repository", "image.tag", "nvkeOIDCAuth.discoveryURL", "spiffe.spire.serverAddress", "spiffe.trustDomain"},
		"spire": {
			"global.spire.caSubject.commonName", "global.spire.clusterName", "global.spire.image.registry", "global.spire.trustDomain",
			"spiffe-oidc-discovery-provider.config.additionalDomains", "spiffe-oidc-discovery-provider.config.jwksUri",
			"spiffe-oidc-discovery-provider.tls.certManager.certificate.ipAddresses",
			"spire-server.adminIDs", "spire-server.nodeAttestor.tpmDirect.checksum",
			"spire-server.upstreamAuthority.disk.enabled", "spire-server.upstreamAuthority.disk.secret.create",
			"spire-server.upstreamAuthority.disk.secret.data.bundle", "spire-server.upstreamAuthority.disk.secret.name",
		},
		"postgres":     {"global.imageRegistry", "global.security.allowInsecureImages"},
		"nico-prereqs": {"global.imageRegistry", "global.security.allowInsecureImages"},
		"temporal":     {"admintools.image.repository", "server.image.repository", "web.image.repository"},
	}
	for _, kind := range []TargetKind{Seed, Site} {
		in := testInputs(t, kind, "linux/arm64", true)
		in.Site.Spire.UpstreamCA = true
		in.Stack.Mirror = "mirror.example.internal:5000/nvke"
		for _, rel := range mustResolve(t, in).Releases {
			paths := leaves(rel.ValuesOverlay, "")
			if rel.ValuesOverlay == nil {
				assert.NotContains(t, want, rel.Name, "%s %s has no overlay", kind, rel.Name)
				continue
			}
			assert.Equal(t, want[rel.Name], paths, "%s %s", kind, rel.Name)
			base, err := mergedValues(HelmRelease{ValuesFiles: rel.ValuesFiles})
			require.NoError(t, err)
			for _, path := range paths {
				if slices.Contains(restated, path) {
					continue
				}
				assert.Nil(t, dig(base, strings.Split(path, ".")...), "%s %s restates %s", kind, rel.Name, path)
			}
		}
	}
}

func TestSeedDNSAddress(t *testing.T) {
	in := testInputs(t, Seed, "linux/amd64", false)
	in.Site.Spire.ExternalAddress = "seed.example.internal"
	spire := release(t, mustResolve(t, in), "spire").ValuesOverlay
	cert := dig(spire, "spiffe-oidc-discovery-provider", "tls", "certManager", "certificate").(map[string]any)
	assert.NotContains(t, cert, "ipAddresses")
	assert.Equal(t, []string{"nvke.site-controller.internal", "localhost", "seed.example.internal"}, cert["dnsNames"])
	assert.Equal(t, []string{"nvke.site-controller.internal", "localhost", "127.0.0.1", "seed.example.internal"},
		dig(spire, "spiffe-oidc-discovery-provider", "config", "additionalDomains"))
}

// mergedValues layers a release's values files and overlay as Helm does:
// maps merge key by key, anything else replaces.
func mergedValues(rel HelmRelease) (map[string]any, error) {
	out := map[string]any{}
	for _, file := range rel.ValuesFiles {
		var values map[string]any
		if err := readValues(file, &values); err != nil {
			return nil, err
		}
		out = mergeValues(out, values)
	}
	return mergeValues(out, rel.ValuesOverlay), nil
}

func mergeValues(dst, src map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range dst {
		out[k] = v
	}
	for k, v := range src {
		srcMap, ok := v.(map[string]any)
		dstMap, had := out[k].(map[string]any)
		if ok && had {
			out[k] = mergeValues(dstMap, srcMap)
		} else {
			out[k] = v
		}
	}
	return out
}

func dig(values map[string]any, keys ...string) any {
	var v any = values
	for _, key := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[key]
	}
	return v
}
