// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

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
}

// Load reads the config file at path and applies defaults. A missing file is
// not an error; in that case all defaults apply.
func Load(path string) (Config, error) {
	var cfg Config
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if uerr := yaml.Unmarshal(data, &cfg); uerr != nil {
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
}

func setDefault(field *string, def string) {
	if *field == "" {
		*field = def
	}
}
