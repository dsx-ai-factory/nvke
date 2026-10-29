// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nvkectl

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/dsx-ai-factory/nvke/cli/internal/config"
)

func TestSetSiteRequiresKubeconfig(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	root := NewRootCmd("test", "test", "test", "test")
	root.SetArgs([]string{"config", "set-site", "--url", "http://localhost:8080"})
	if err := root.Execute(); err == nil {
		t.Fatal("expected error when --kubeconfig is omitted, got nil")
	}
}

func TestSetSitePersistsConnectionDetailsAndCurrentSite(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	root := NewRootCmd("test", "test", "test", "test")
	root.SetArgs([]string{
		"config", "set-site",
		"--url", "http://localhost:8080",
		"--kubeconfig", "/home/op/.kube/dev-mgmt.yaml",
		"--proxy-client-cert", "/home/op/.config/nvkectl/client.crt",
		"--proxy-client-key", "/home/op/.config/nvkectl/client.key",
		"--name", "dev",
	})
	if err := root.Execute(); err != nil {
		t.Fatalf("set-site: %v", err)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.CurrentSite != "dev" {
		t.Errorf("CurrentSite = %q, want %q", cfg.CurrentSite, "dev")
	}
	site, ok := cfg.Sites["dev"]
	if !ok {
		t.Fatal("site \"dev\" not saved")
	}
	if site.Kubeconfig != "/home/op/.kube/dev-mgmt.yaml" {
		t.Errorf("Kubeconfig = %q, want %q", site.Kubeconfig, "/home/op/.kube/dev-mgmt.yaml")
	}
	if site.ProxyClientCertFile != "/home/op/.config/nvkectl/client.crt" {
		t.Errorf("ProxyClientCertFile = %q, want proxy client certificate path", site.ProxyClientCertFile)
	}
	if site.ProxyClientKeyFile != "/home/op/.config/nvkectl/client.key" {
		t.Errorf("ProxyClientKeyFile = %q, want proxy client key path", site.ProxyClientKeyFile)
	}
}

func TestSetSiteRequiresClientCertificateAndKeyTogether(t *testing.T) {
	tests := map[string][]string{
		"certificate only": {"--proxy-client-cert", "/client.crt"},
		"key only":         {"--proxy-client-key", "/client.key"},
	}
	for name, flags := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			args := []string{
				"config", "set-site",
				"--url", "https://site.example.com",
				"--kubeconfig", "/management.kubeconfig",
			}
			args = append(args, flags...)

			root := NewRootCmd("test", "test", "test", "test")
			root.SetArgs(args)
			if err := root.Execute(); err == nil {
				t.Fatal("expected an error for an incomplete client certificate pair")
			}
		})
	}
}

func TestUseSiteSwitchesCurrentSite(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	cfg := &config.Config{
		Sites: map[string]config.SiteConfig{
			"dev":  {URL: "http://localhost:8080", Kubeconfig: "/kube/dev.yaml"},
			"prod": {URL: "https://prod-controller.example.com", Kubeconfig: "/kube/prod.yaml"},
		},
		CurrentSite: "dev",
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	root := NewRootCmd("test", "test", "test", "test")
	root.SetArgs([]string{"config", "use-site", "prod"})
	if err := root.Execute(); err != nil {
		t.Fatalf("use-site: %v", err)
	}

	loaded, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.CurrentSite != "prod" {
		t.Errorf("CurrentSite = %q, want %q", loaded.CurrentSite, "prod")
	}
}

func TestUseSiteUnknownNameErrors(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	cfg := &config.Config{
		Sites:       map[string]config.SiteConfig{"dev": {URL: "http://localhost:8080", Kubeconfig: "/kube/dev.yaml"}},
		CurrentSite: "dev",
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	root := NewRootCmd("test", "test", "test", "test")
	root.SetArgs([]string{"config", "use-site", "staging"})
	if err := root.Execute(); err == nil {
		t.Fatal("expected error for unknown site name, got nil")
	}

	loaded, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.CurrentSite != "dev" {
		t.Errorf("CurrentSite changed to %q despite error, want unchanged %q", loaded.CurrentSite, "dev")
	}
}

func TestResolveSiteConfigPrefersSiteFlagOverride(t *testing.T) {
	cfg := &config.Config{
		Sites: map[string]config.SiteConfig{
			"dev":  {URL: "http://localhost:8080", Kubeconfig: "/kube/dev.yaml"},
			"prod": {URL: "https://prod-controller.example.com", Kubeconfig: "/kube/prod.yaml"},
		},
		CurrentSite: "dev",
	}

	site = "prod"
	t.Cleanup(func() { site = "" })

	resolved, err := resolveSiteConfig(cfg)
	if err != nil {
		t.Fatalf("resolveSiteConfig: %v", err)
	}
	if resolved.URL != "https://prod-controller.example.com" {
		t.Errorf("URL = %q, want prod URL", resolved.URL)
	}
}

func TestResolveSiteConfigFallsBackToCurrentSite(t *testing.T) {
	cfg := &config.Config{
		Sites:       map[string]config.SiteConfig{"dev": {URL: "http://localhost:8080", Kubeconfig: "/kube/dev.yaml"}},
		CurrentSite: "dev",
	}

	site = ""

	resolved, err := resolveSiteConfig(cfg)
	if err != nil {
		t.Fatalf("resolveSiteConfig: %v", err)
	}
	if resolved.URL != "http://localhost:8080" {
		t.Errorf("URL = %q, want dev URL", resolved.URL)
	}
}

func TestResolveSiteConfigUnknownSiteFlagErrors(t *testing.T) {
	cfg := &config.Config{
		Sites:       map[string]config.SiteConfig{"dev": {URL: "http://localhost:8080", Kubeconfig: "/kube/dev.yaml"}},
		CurrentSite: "dev",
	}

	site = "staging"
	t.Cleanup(func() { site = "" })

	if _, err := resolveSiteConfig(cfg); err == nil {
		t.Fatal("expected error for unknown --site override, got nil")
	}
}

func TestResolveSiteConfigErrorsWhenNoSiteConfigured(t *testing.T) {
	cfg := &config.Config{Sites: map[string]config.SiteConfig{}}

	site = ""

	if _, err := resolveSiteConfig(cfg); err == nil {
		t.Fatal("expected error when no site is configured, got nil")
	}
}

func TestResolveSiteConfigErrorsWhenCurrentSiteIsDangling(t *testing.T) {
	// CurrentSite names a site no longer present in Sites -- e.g. a hand-edited
	// config.yaml, or a site removed without clearing current-site.
	cfg := &config.Config{
		Sites:       map[string]config.SiteConfig{"prod": {URL: "https://prod-controller.example.com"}},
		CurrentSite: "dev",
	}

	site = ""

	if _, err := resolveSiteConfig(cfg); err == nil {
		t.Fatal("expected error when CurrentSite is not in Sites, got nil")
	}
}

// runConfig runs nvkectl with args against the config in XDG_CONFIG_HOME and
// returns what it printed and its error.
func runConfig(t *testing.T, args ...string) (string, error) {
	t.Helper()
	for _, k := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_INSECURE"} {
		t.Setenv(k, "")
	}
	prevLogger := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prevLogger) })
	root := NewRootCmd("test", "test", "test", "test")
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func mustSetSite(t *testing.T, args ...string) {
	t.Helper()
	if _, err := runConfig(t, append([]string{"config", "set-site"}, args...)...); err != nil {
		t.Fatalf("set-site %v: %v", args, err)
	}
}

func loadSavedSite(t *testing.T, name string) config.SiteConfig {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	site, ok := cfg.Sites[name]
	if !ok {
		t.Fatalf("site %q not saved", name)
	}
	return site
}

const (
	testURL        = "http://dev:8080"
	testKubeconfig = "/kube/dev.yaml"
)

// setSiteDev runs set-site for the site "dev" with the required flags plus extra.
func setSiteDev(t *testing.T, extra ...string) (string, error) {
	t.Helper()
	args := append([]string{"config", "set-site", "--name", "dev", "--url", testURL, "--kubeconfig", testKubeconfig}, extra...)
	return runConfig(t, args...)
}

func TestSetSiteStoresTheOTLPEndpoint(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	mustSetSite(t, "--name", "dev", "--url", testURL, "--kubeconfig", testKubeconfig, "--otlp-endpoint", "c:4317", "--otlp-insecure")
	want := config.SiteConfig{URL: testURL, Kubeconfig: testKubeconfig, OTLPEndpoint: "c:4317", OTLPInsecure: true}
	if got := loadSavedSite(t, "dev"); got != want {
		t.Errorf("site = %+v, want %+v", got, want)
	}
}

func TestSetSiteStoresEndpointsNormalisedWithTheirSecurity(t *testing.T) {
	tests := map[string]struct {
		flags        []string
		wantEndpoint string
		wantInsecure bool
	}{
		"bare host":                      {[]string{"--otlp-endpoint", "b:4317"}, "b:4317", false},
		"bare host with --otlp-insecure": {[]string{"--otlp-endpoint", "b:4317", "--otlp-insecure"}, "b:4317", true},
		"http url":                       {[]string{"--otlp-endpoint", "http://b:4317"}, "b:4317", true},
		"https url":                      {[]string{"--otlp-endpoint", "https://b:4317"}, "b:4317", false},
		"http url ignores =false":        {[]string{"--otlp-endpoint", "http://b:4317", "--otlp-insecure=false"}, "b:4317", true},
		"loopback is allowed":            {[]string{"--otlp-endpoint", "127.0.0.1:4317"}, "127.0.0.1:4317", false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			if _, err := setSiteDev(t, tt.flags...); err != nil {
				t.Fatalf("set-site: %v", err)
			}
			site := loadSavedSite(t, "dev")
			if site.OTLPEndpoint != tt.wantEndpoint || site.OTLPInsecure != tt.wantInsecure {
				t.Errorf("site = %+v, want endpoint %q insecure %v", site, tt.wantEndpoint, tt.wantInsecure)
			}
		})
	}
}

func TestSetSiteOTLPInsecureNeedsAnEndpoint(t *testing.T) {
	for name, flags := range map[string][]string{
		"no endpoint":    {"--otlp-insecure"},
		"empty endpoint": {"--otlp-endpoint", "", "--otlp-insecure"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			_, err := setSiteDev(t, flags...)
			if err == nil || !strings.Contains(err.Error(), "--otlp-endpoint") {
				t.Fatalf("err = %v, want an error naming --otlp-endpoint", err)
			}
		})
	}
}

func TestSetSiteRejectsInsecureWithAnHTTPSEndpoint(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	_, err := setSiteDev(t, "--otlp-endpoint", "https://c:4317", "--otlp-insecure")
	if err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("err = %v, want an error naming the https scheme", err)
	}
}

// The flags of set-site name an endpoint to store: they do not say where set-site
// itself exports, so a loopback endpoint is stored and set-site sends nothing to it.
func TestSetSiteFlagsDoNotChooseItsOwnExport(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	if _, err := setSiteDev(t, "--otlp-endpoint", "localhost:4317"); err != nil {
		t.Fatalf("set-site: %v", err)
	}
	if site := loadSavedSite(t, "dev"); site.OTLPEndpoint != "localhost:4317" {
		t.Errorf("site = %+v, want the loopback endpoint stored", site)
	}

	cmd := newConfigSetSiteCmd()
	if err := cmd.ParseFlags([]string{"--otlp-endpoint", "c.example:4317"}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _, err := resolveOTLP(cmd); ok || err != nil {
		t.Errorf("resolveOTLP(set-site) = ok %v, err %v; want no endpoint", ok, err)
	}
}

func TestSetSiteEmptyEndpointMeansNone(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if _, err := setSiteDev(t, "--otlp-endpoint", ""); err != nil {
		t.Fatalf("set-site: %v", err)
	}
	if site := loadSavedSite(t, "dev"); site.OTLPEndpoint != "" || site.OTLPInsecure {
		t.Errorf("site = %+v, want no endpoint", site)
	}
}

func TestSetSiteRejectsABadEndpoint(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if _, err := setSiteDev(t, "--otlp-endpoint", "::1"); err == nil {
		t.Fatal("expected an error for a bare IPv6 endpoint")
	}
}

func TestSetSiteReplacesTheWholeEntry(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	mustSetSite(t, "--name", "dev", "--url", testURL, "--kubeconfig", testKubeconfig,
		"--proxy-client-cert", "/c.crt", "--proxy-client-key", "/c.key",
		"--otlp-endpoint", "c:4317", "--otlp-insecure")
	if _, err := setSiteDev(t); err != nil {
		t.Fatalf("set-site: %v", err)
	}
	want := config.SiteConfig{URL: testURL, Kubeconfig: testKubeconfig}
	if got := loadSavedSite(t, "dev"); got != want {
		t.Errorf("site = %+v, want %+v", got, want)
	}
}

func TestSetSiteSetsCurrentSite(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	mustSetSite(t, "--name", "other", "--url", "http://o:8080", "--kubeconfig", "/o")
	if _, err := setSiteDev(t, "--otlp-endpoint", "z:4317"); err != nil {
		t.Fatalf("set-site: %v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.CurrentSite != "dev" {
		t.Errorf("CurrentSite = %q, want dev", cfg.CurrentSite)
	}
}

func TestConfigViewShowsTheEndpointAndNoHeaders(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if _, err := setSiteDev(t, "--otlp-endpoint", "c:4317", "--otlp-insecure"); err != nil {
		t.Fatalf("set-site: %v", err)
	}
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "authorization=Bearer s3cret")
	out, err := runConfig(t, "config", "view")
	if err != nil {
		t.Fatalf("view: %v", err)
	}
	if !strings.Contains(out, "otlp-endpoint") || !strings.Contains(out, "c:4317") {
		t.Errorf("view output lacks the endpoint:\n%s", out)
	}
	if strings.Contains(out, "s3cret") {
		t.Errorf("view output leaks a header value:\n%s", out)
	}
}
