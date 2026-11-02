// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package engine

import (
	"context"
	"fmt"
	"regexp"
)

// dockerHubRegistries are the registry hosts a docker.io image reference
// resolves through. containerd looks each one up under its own certs.d
// directory, so both need a hosts.toml for the mirror to take effect.
var dockerHubRegistries = []string{"docker.io", "registry-1.docker.io"}

// registryMirrorPattern is a registry host with an optional port. The value
// is written into a TOML string and a shell command, so nothing else is
// accepted.
var registryMirrorPattern = regexp.MustCompile(`^[a-zA-Z0-9.-]+(:[0-9]{1,5})?$`)

// validateRegistryMirror returns an error unless mirror is empty or a
// registry host with an optional port.
func validateRegistryMirror(mirror string) error {
	if mirror == "" || registryMirrorPattern.MatchString(mirror) {
		return nil
	}
	return fmt.Errorf("bootstrapengine: Config.DockerHubMirror %q must be a registry host with an optional port", mirror)
}

// configureDockerHubMirror writes a containerd hosts.toml for each Docker Hub
// registry host on the cluster's control-plane node, so the node's docker.io
// pulls go to cfg.DockerHubMirror. kind's node image enables containerd's
// config_path, so the files apply to the next pull with no restart.
//
// The mirror is both the pull host and the server, the same shape
// controller/hack/ci/backend-e2e.sh gives the management cluster: a mirror
// that does not answer fails the pull rather than falling back to Docker Hub,
// which is the registry the mirror exists to keep the run away from.
func (e *Engine) configureDockerHubMirror(ctx context.Context, cfg Config) error {
	node := cfg.ClusterName + "-control-plane"
	e.log.Info("routing the bootstrap cluster's docker.io pulls through a mirror",
		"bootstrap_kind_cluster", cfg.ClusterName, "mirror", cfg.DockerHubMirror)

	hostsTOML := fmt.Sprintf("server = \"https://%[1]s\"\n\n[host.\"https://%[1]s\"]\n  capabilities = [\"pull\", \"resolve\"]\n", cfg.DockerHubMirror)
	for _, registry := range dockerHubRegistries {
		dir := "/etc/containerd/certs.d/" + registry
		// hostsTOML holds no single quote, so the literal is safe.
		script := fmt.Sprintf("mkdir -p %s && printf '%%s' '%s' > %s/hosts.toml", dir, hostsTOML, dir)
		out, err := e.Runner.Run(ctx, nil, RuntimeCLI, "exec", node, "sh", "-c", script)
		if err != nil {
			return fmt.Errorf("configuring the docker.io mirror %s on %s: %w\n%s", cfg.DockerHubMirror, node, err, out)
		}
	}
	return nil
}
