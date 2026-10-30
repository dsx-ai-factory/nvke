// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package nodecontract selects, per node, the old or the new form of each
// contract the controller shares with the node agent.
//
// The controller and the agents on a site may run different versions, so the
// form is chosen from the node's reported agent_version rather than from the
// controller release. A node that has never reported a version carries the
// empty string and always gets the old form.
package nodecontract

import (
	"strconv"
	"strings"

	"github.com/blang/semver/v4"

	"github.com/dsx-ai-factory/nvke/auth/authn"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
)

// NewFormMinAgentVersion is the first agent release that speaks the new form of
// every contract in Contracts. Agents older than this speak only the old form.
const (
	NewFormMinAgentVersion = "0.3.0"
)

// Contracts is the set of node contracts the controller writes or addresses
// for one node.
type Contracts struct {
	// URN is the URI SAN the controller's client certificate presents to the agent.
	URN string
	// UnitName is the agent's systemd unit.
	UnitName string
	// EnvFilePath is the agent's environment file, written at bootstrap.
	EnvFilePath string
	// CNIPackagePrefix prefixes the CNI image and chart Debian package names.
	CNIPackagePrefix string
	// ControllerCABundlePath is where the controller client CA bundle lands on the node.
	ControllerCABundlePath string
	// ControllerAudience is the JWT audience the agent requests in TLS/JWT mode,
	// passed as --controller-audience. It is empty for the old form: that flag
	// arrived with the new form in #1565, and an agent older than that rejects
	// an unknown flag and fails to start. An empty value means pass no flag and
	// let the agent keep its own default, which is the legacy audience.
	ControllerAudience string
}

func oldForm() Contracts {
	return Contracts{
		URN:                    "urn:nvidia:nke:controller",
		UnitName:               "nke-agent.service",
		EnvFilePath:            config.NodeAgentConfigPath,
		CNIPackagePrefix:       "nke-cni-",
		ControllerCABundlePath: config.NodeAgentControllerCAPath,
		ControllerAudience:     "",
	}
}

func newForm() Contracts {
	return Contracts{
		URN:                    "urn:nvidia:nvke:controller",
		UnitName:               "nvke-agent.service",
		EnvFilePath:            "/etc/default/nvke-agent",
		CNIPackagePrefix:       "nvke-cni-",
		ControllerCABundlePath: "/etc/nvke/controller-ca.crt",
		ControllerAudience:     authn.NodeAgentAudience,
	}
}

func newFormMin() semver.Version {
	return semver.MustParse(NewFormMinAgentVersion)
}

// ForAgentVersion returns the contracts for a node reporting agentVersion.
// An empty, unparseable or older version gets the old form; only a version
// that parses and reaches NewFormMinAgentVersion gets the new one.
func ForAgentVersion(agentVersion string) Contracts {
	if IsNewForm(agentVersion) {
		return newForm()
	}
	return oldForm()
}

// IsNewForm reports whether agentVersion selects the new form of the contracts.
// Pre-release and build metadata are ignored, so a release candidate of the
// cutover version counts as the cutover version.
func IsNewForm(agentVersion string) bool {
	version, err := semver.ParseTolerant(normalize(agentVersion))
	if err != nil {
		return false
	}
	version.Pre = nil
	version.Build = nil
	return version.GTE(newFormMin())
}

// normalize reduces `git describe` style versions (v0.3.0-3-gabc1234,
// v0.3.0-dirty) to the release they were built from.
func normalize(version string) string {
	version = strings.TrimPrefix(strings.TrimSpace(version), "v")
	parts := strings.Split(version, "-")
	if len(parts) == 2 && parts[1] == "dirty" {
		return parts[0]
	}
	if len(parts) >= 3 && strings.HasPrefix(parts[2], "g") {
		if _, err := strconv.Atoi(parts[1]); err == nil {
			return parts[0]
		}
	}
	return version
}
