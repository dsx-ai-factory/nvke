// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// SiteConfig holds connection details for one site controller.
type SiteConfig struct {
	URL string `yaml:"url"`
	// Kubeconfig is the path to the management cluster kubeconfig.
	Kubeconfig string `yaml:"kubeconfig"`
	// ProxyClientCertFile is the path to a TLS client certificate presented to
	// the proxy on every controller API request.
	ProxyClientCertFile string `yaml:"proxy-client-cert,omitempty"`
	// ProxyClientKeyFile is the path to the private key matching ProxyClientCertFile.
	ProxyClientKeyFile string `yaml:"proxy-client-key,omitempty"`
	// OTLPEndpoint is the host:port of the OTLP/gRPC endpoint that receives
	// the spans and log records of commands that use this site. Headers are
	// never stored: they come from OTEL_EXPORTER_OTLP_HEADERS.
	OTLPEndpoint string `yaml:"otlp-endpoint,omitempty"`
	// OTLPInsecure sends to OTLPEndpoint without TLS.
	OTLPInsecure bool `yaml:"otlp-insecure,omitempty"`
}

// Config is the top-level nvkectl config file.
type Config struct {
	Sites       map[string]SiteConfig `yaml:"sites"`
	CurrentSite string                `yaml:"current-site"`
}

// Dir returns the directory where nvkectl stores its config.
// Respects $XDG_CONFIG_HOME; defaults to ~/.config/nvkectl.
func Dir() (string, error) {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "nvkectl"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "nvkectl"), nil
}

func configPath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.yaml"), nil
}

// legacyConfigPath is where nkectl, the binary's previous name, kept the same
// file: the sibling directory nkectl/ next to Dir(). Load reads it when the
// current location has no file yet and writes what it read to the current
// location, so the handover happens once.
func legacyConfigPath() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(dir), "nkectl", "config.yaml"), nil
}

// Load reads the config file. Returns an empty Config if the file does not exist.
func Load() (*Config, error) {
	path, err := configPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	migrated := false
	if os.IsNotExist(err) {
		legacy, lerr := legacyConfigPath()
		if lerr != nil {
			return nil, lerr
		}
		data, err = os.ReadFile(legacy)
		if os.IsNotExist(err) {
			return &Config{Sites: map[string]SiteConfig{}}, nil
		}
		migrated = err == nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}
	if cfg.Sites == nil {
		cfg.Sites = map[string]SiteConfig{}
	}
	if migrated {
		// Best effort: the config was read; a location that cannot be written
		// is reported by the next Save, not by a read.
		_ = cfg.Save()
	}
	return &cfg, nil
}

// Save writes the config to disk.
func (c *Config) Save() error {
	dir, err := Dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating config dir: %w", err)
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), data, 0o600); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	return nil
}

// CurrentSiteConfig returns the SiteConfig for the active site.
func (c *Config) CurrentSiteConfig() (SiteConfig, error) {
	if c.CurrentSite == "" {
		return SiteConfig{}, fmt.Errorf("no site configured: run 'nvkectl config set-site' first")
	}
	site, ok := c.Sites[c.CurrentSite]
	if !ok {
		return SiteConfig{}, fmt.Errorf("site %q not found in config", c.CurrentSite)
	}
	return site, nil
}
