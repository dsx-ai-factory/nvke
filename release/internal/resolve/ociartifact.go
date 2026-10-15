// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package resolve

import (
	"fmt"
	"strings"
)

// OCIArtifact names where a version is published in a registry.
//
// Most artifacts are one tag, "<repository>:<version>". Some are pushed as one
// tag per platform, "<repository>:<version>-<os>-<arch>", with no index above
// them, so there is no bare tag to pull with --platform. nvkectl and the agent
// are both published that way, and Arch is what says so.
type OCIArtifact struct {
	// Base is the registry and namespace the artifact is published under,
	// expanded from the source its registry entry names.
	Base string

	// Repository is the artifact's name under Base.
	Repository string

	// OS is the operating system in the tag, for an artifact tagged per
	// platform. Empty when Arch is empty.
	OS string

	// Arch is the architecture in the tag. Empty for an artifact that is one
	// tag for every platform.
	Arch string
}

// DefaultOS is the operating system assumed for a per-platform artifact whose
// registry entry names none, which is every artifact a run stages today.
const DefaultOS = "linux"

// Resolve returns the artifact's URL for the version it is given.
func (a OCIArtifact) Resolve(inputs map[string]string) (map[string]string, error) {
	// Overriding the location would leave the version the plan records and the
	// artifact it names free to disagree. Overriding the version moves both.
	if inputs[InputOverrideKey] != "" {
		return nil, fmt.Errorf("%s takes no override, override the version instead", a.Repository)
	}

	version := inputs[InputVersionKey]
	if version == "" {
		return nil, fmt.Errorf("no %s given, so there is no %s artifact to name", InputVersionKey, a.Repository)
	}

	tag := version
	if a.Arch != "" {
		tag += "-" + a.OS + "-" + a.Arch
	}
	return map[string]string{
		ResultURLKey: fmt.Sprintf("%s/%s:%s", strings.TrimSuffix(a.Base, "/"), a.Repository, tag),
	}, nil
}

// Validate accepts no declared input, because the location is built from the
// registry entry and the version alone.
func (OCIArtifact) Validate(inputs map[string]string) error { return takesOnly(inputs) }
