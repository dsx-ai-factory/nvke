// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package site turns a provisioned machine into a site controller: it
// bootstraps vanilla Kubernetes on the machine with nkectl, deploys the NKE
// stack onto it, and hands back a kubeconfig the runner can use.
//
// Everything that touches the cluster before the last step runs on the
// machine, over the session the provider returned. The runner has no route to
// the cluster's network; the session is the only way in.
package site

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	// workDir holds everything staged onto the node and everything the
	// bootstrap writes. Root-owned, since the bootstrap runs as root.
	workDir = "/var/lib/nke-e2e"

	// assetsToken in a BootstrapEnv value is replaced with where the assets
	// were unpacked on the node, so a value can name a chart in the staged
	// checkout without the caller knowing the path.
	assetsToken = "{assets}"
)

// siteName is what nkectl accepts as a cluster name: an RFC 1123 label.
var siteName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// Candidate is what a site controller is built from: the software under
// qualification and where the machine gets it.
type Candidate struct {
	// SiteName names the cluster, and through it the SPIRE trust domain.
	SiteName string

	// Nkectl is a linux nkectl binary on the runner, for the node's
	// architecture. It is staged rather than downloaded: every release is a
	// pre-release, which the latest-release URL skips.
	Nkectl string

	// Assets is a checkout on the runner. AssetPaths, relative to it, are
	// staged to the node with their layout intact, which the deploy path
	// depends on: a release reaches its chart by relative path.
	Assets     string
	AssetPaths []string

	// Images are image archives on the runner, as docker save writes them,
	// imported into the node's containerd. The node's containerd is the
	// cluster's, so an imported image resolves with IfNotPresent and needs no
	// pull credential.
	Images []string

	KubernetesVersion string
	CNI               string

	// DisableMTLS bootstraps without SPIFFE. It must match how the node
	// agent was started, or the bootstrap fails as a TLS handshake error.
	DisableMTLS bool

	// BootstrapEnv is passed to nkectl bootstrap site. Values may contain
	// {assets}.
	BootstrapEnv map[string]string

	// ControllerVersion is what CWE must report once deployed. Empty skips
	// the check, for a developer run with no version under qualification.
	ControllerVersion string

	// Deploy installs the NKE stack onto the bootstrapped cluster.
	Deploy Deployer
}

func (c Candidate) validate() error {
	var problems []string
	if !siteName.MatchString(c.SiteName) {
		problems = append(problems, fmt.Sprintf("site name %q is not an RFC 1123 label", c.SiteName))
	}
	for name, path := range map[string]string{"nkectl": c.Nkectl, "assets": c.Assets} {
		if path == "" {
			problems = append(problems, "no "+name)
			continue
		}
		if _, err := os.Stat(path); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", name, err))
		}
	}
	for _, rel := range c.AssetPaths {
		if filepath.IsAbs(rel) || strings.HasPrefix(filepath.Clean(rel), "..") {
			problems = append(problems, fmt.Sprintf("asset path %q is not inside the checkout", rel))
		}
	}
	for _, image := range c.Images {
		if _, err := os.Stat(image); err != nil {
			problems = append(problems, fmt.Sprintf("image archive: %v", err))
		}
	}
	if c.KubernetesVersion == "" || strings.HasPrefix(c.KubernetesVersion, "v") {
		problems = append(problems, fmt.Sprintf("kubernetes version %q must be a plain semantic version", c.KubernetesVersion))
	}
	if c.CNI == "" {
		problems = append(problems, "no CNI")
	}
	if c.Deploy == nil {
		problems = append(problems, "no deployer")
	}
	if len(problems) > 0 {
		return errors.New("site candidate: " + strings.Join(problems, "; "))
	}
	return nil
}
