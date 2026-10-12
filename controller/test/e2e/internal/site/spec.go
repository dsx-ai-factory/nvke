// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package site turns a provisioned machine into a site controller: it
// bootstraps vanilla Kubernetes on the machine with nvkectl, deploys the NVKE
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
	"regexp"
	"strings"
)

const (
	// workDir holds everything staged onto the node and everything the
	// bootstrap writes. Root-owned, since the bootstrap runs as root.
	workDir = "/var/lib/nke-e2e"
)

// siteName is what nvkectl accepts as a cluster name: an RFC 1123 label.
var siteName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// Spec is the site to bootstrap: the software under qualification and the
// shape of the cluster nvkectl is asked to build.
//
// It describes the site and nothing about what happens afterwards. The stack
// the cluster ends up running is a Deployer's, chosen by the caller, which is
// why nothing here names one.
type Spec struct {
	// Name names the cluster, and through it the SPIRE trust domain.
	Name string

	// ImageArchives are files on the runner, as docker save writes them,
	// imported into the node's containerd. The node's containerd is the
	// cluster's, so an imported image resolves with IfNotPresent and needs no
	// pull credential.
	//
	// Paths, where ControllerImage and CAPIProvisionedImage below are
	// references. More archives are staged than references are passed: the
	// profiles image is the deploy's, and nvkectl never names it.
	ImageArchives []string

	KubernetesVersion string
	CNI               string

	// DisableMTLS bootstraps without SPIFFE. It must match how the node
	// agent was started, or the bootstrap fails as a TLS handshake error.
	DisableMTLS bool

	// ControllerImage and CAPIProvisionedImage are the references the staged
	// archives carry, so the bootstrap's kind cluster resolves them from
	// containerd rather than from a registry.
	//
	// References rather than the environment variables nvkectl reads them
	// from: which variable carries which is the bootstrap's business, and a
	// caller that spelled them would be writing nvkectl's interface from the
	// outside.
	ControllerImage      string
	CAPIProvisionedImage string
}

func (s Spec) validate() error {
	var problems []string
	if !siteName.MatchString(s.Name) {
		problems = append(problems, fmt.Sprintf("site name %q is not an RFC 1123 label", s.Name))
	}
	for _, image := range s.ImageArchives {
		if _, err := os.Stat(image); err != nil {
			problems = append(problems, fmt.Sprintf("image archive: %v", err))
		}
	}
	if s.KubernetesVersion == "" || strings.HasPrefix(s.KubernetesVersion, "v") {
		problems = append(problems, fmt.Sprintf("kubernetes version %q must be a plain semantic version", s.KubernetesVersion))
	}
	if s.CNI == "" {
		problems = append(problems, "no CNI")
	}
	if len(problems) > 0 {
		return errors.New("site: " + strings.Join(problems, "; "))
	}
	return nil
}
