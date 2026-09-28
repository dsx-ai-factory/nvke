// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package config holds the minimal, file-based, defaults-first configuration
// for the OIDC-SPIRE E2E suite. A missing config file is not an error: every
// field has a default so the suite runs zero-config against a local cluster.
package config

import (
	"fmt"
	"os"

	"sigs.k8s.io/yaml"
)

// Config is the E2E suite configuration.
type Config struct {
	Kube struct {
		Kubeconfig string `json:"kubeconfig"` // "" -> default loading (KUBECONFIG/~/.kube/config)
		Context    string `json:"context"`    // "" -> current-context
	} `json:"kube"`
	Cluster struct {
		CAPDNodeImage string `json:"capdNodeImage"` // default "cwe-capd-kindest-node:local"
	} `json:"cluster"`
	Lifecycle struct {
		SkipDelete        bool   `json:"skipDelete"`
		PreserveOnFailure bool   `json:"preserveOnFailure"`
		ReuseCluster      string `json:"reuseCluster"` // non-empty -> skip create, verify this cluster
	} `json:"lifecycle"`
	// Site configures the lane that builds its own site controller. It holds
	// identifiers and paths only: the Forge API key and the registry token
	// are read from the environment and never written to a file.
	Site Site `json:"site"`
}

// Site is what a site controller is built from.
type Site struct {
	// Name is the site's cluster name and its node's hostname. Empty
	// generates one per run.
	Name string `json:"name"`

	// Repo is the clone the deploy path is exported from and that relative
	// paths below resolve against. Empty is the clone the suite runs from.
	Repo string `json:"repo"`
	// DeployPathRef is the commit the deploy path is exported from. Empty is
	// the commit under test.
	DeployPathRef string `json:"deployPathRef"`

	// Nkectl is a linux nkectl for the node. Empty is what
	// `gmake -C cli build-cross` writes for the node's architecture.
	Nkectl string `json:"nkectl"`
	// Images are image archives imported into the node's containerd.
	Images []string `json:"images"`
	// BootstrapEnv is passed to nkectl bootstrap site; {assets} expands to
	// where the deploy path was unpacked on the node.
	BootstrapEnv map[string]string `json:"bootstrapEnv"`

	CNI         string `json:"cni"`         // default "ovn-kubernetes"
	DisableMTLS bool   `json:"disableMTLS"` // node agent and bootstrap without SPIFFE

	// ControllerRepository is the CWE image repository; the tag is the
	// controller version under qualification.
	ControllerRepository string `json:"controllerRepository"`
	// ProfilesImage is repository:tag. The controller chart requires one.
	ProfilesImage string `json:"profilesImage"`
	// RegistryServer is where private images are pulled from, with the
	// credential in GHCR_USERNAME and GHCR_TOKEN.
	RegistryServer string `json:"registryServer"`

	// JumpHost is the node the Forge site's machines are reached through. The
	// Teleport proxy is not configured here: tsh takes it from
	// $TELEPORT_PROXY or the logged-in profile.
	JumpHost string `json:"jumpHost"`
	// TeleportLogin is the jump host login; empty is tsh's default.
	TeleportLogin string `json:"teleportLogin"`
	// TeleportIdentityFile is a tsh identity file, as a CI bot has instead of
	// a login session. Empty leaves it to tsh: $TELEPORT_IDENTITY_FILE, else
	// the runner's tsh profile.
	TeleportIdentityFile string `json:"teleportIdentityFile"`

	// JumpHostUser is the jump host login for the plain-SSH route, taken
	// when JUMP_HOST_PASSWORD is set. Empty falls back to TeleportLogin, so a
	// site that uses the same account for both names it once.
	JumpHostUser string `json:"jumpHostUser"`
	// JumpHostKeyFingerprint pins the jump host's host key on the plain-SSH
	// route. Empty accepts whatever it presents and warns on every dial;
	// see teleport.SSHJumpHost for how to read one.
	JumpHostKeyFingerprint string `json:"jumpHostKeyFingerprint"`
}

// Load reads the config file at path and applies defaults. A missing file is
// not an error; in that case all defaults apply.
func Load(path string) (Config, error) {
	var cfg Config
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		// Strict, as the environment file is parsed: an unknown key is
		// otherwise dropped in silence and the run proceeds on the default,
		// which surfaces half an hour into bare metal as behavior nobody
		// asked for, over a file that reads correctly.
		//
		// Not case-sensitive, though. This decodes through encoding/json,
		// whose field matching is not, so jumphost still binds to jumpHost.
		// That is a match rather than a miss, so nothing is lost.
		if uerr := yaml.UnmarshalStrict(data, &cfg); uerr != nil {
			return Config{}, fmt.Errorf("parsing config %s: %w", path, uerr)
		}
	case os.IsNotExist(err):
		// Missing file -> defaults only.
	default:
		return Config{}, fmt.Errorf("reading config %s: %w", path, err)
	}

	cfg.applyDefaults()
	return cfg, nil
}

func (c *Config) applyDefaults() {
	setDefault(&c.Cluster.CAPDNodeImage, "cwe-capd-kindest-node:local")
	setDefault(&c.Site.CNI, "ovn-kubernetes")
	setDefault(&c.Site.ControllerRepository, "ghcr.io/dsx-ai-factory/nke/nke-controller")
	setDefault(&c.Site.RegistryServer, "ghcr.io")
}

func setDefault(field *string, def string) {
	if *field == "" {
		*field = def
	}
}
