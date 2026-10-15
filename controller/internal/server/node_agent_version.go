// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/blang/semver/v4"
)

var (
	errAgentUpgradeInProgress = errors.New("control-plane agent upgrade is in progress; retry this maintenance update after it completes")
	agentReleasePattern       = regexp.MustCompile(`^v?(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$`)
)

func canonicalNodeAgentVersion(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if !agentReleasePattern.MatchString(value) {
		return "", fmt.Errorf("controlPlaneConfig.nodeAgentConfig.version must be an exact release SemVer")
	}
	version, err := semver.Parse(strings.TrimPrefix(value, "v"))
	if err != nil {
		return "", fmt.Errorf("invalid node-agent version: %w", err)
	}
	return "v" + version.String(), nil
}
