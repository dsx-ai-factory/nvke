// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"

	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"
	"go.yaml.in/yaml/v4"

	"github.com/dsx-ai-factory/nke/controller/internal/nodes"
)

type ConfigProfile struct {
	Hooks             ConfigProfileHookSpec `yaml:"hooks"`
	PreJoinNodeReboot bool                  `yaml:"preJoinNodeReboot"`
	Labels            []string              `yaml:"labels"`
}

type ConfigProfileHookSpec struct {
	BeforeJoinNode   []*HookSpec `yaml:"beforeJoinNode"`
	AfterJoinNode    []*HookSpec `yaml:"afterJoinNode"`
	BeforeRemoveNode []*HookSpec `yaml:"beforeRemoveNode"`
	AfterRemoveNode  []*HookSpec `yaml:"afterRemoveNode"`
}

type HookSpec struct {
	Runbook *RunbookHookSpec `yaml:"runbook"`
}

type RunbookHookSpec struct {
	Name string            `yaml:"name"`
	Env  map[string]string `yaml:"env"`
}

type GetNodeConfigProfileInput struct {
	NodeID uuid.UUID
}

func (a *Activities) GetNodeConfigProfile(ctx context.Context, input GetNodeConfigProfileInput) (ConfigProfile, error) {
	node, err := a.db.GetNode(ctx, input.NodeID)
	if err != nil {
		return ConfigProfile{}, fmt.Errorf("failed to get node: %w", err)
	}

	nodeSet, _, err := nodes.GetNodeContext(ctx, a.db, node)
	if err != nil {
		return ConfigProfile{}, fmt.Errorf("failed to get node context: %w", err)
	}
	configProfileName := ""
	if nicoNodeSetProvider, err := nodeSet.NicoProvider(); err == nil {
		configProfileName = nicoNodeSetProvider.ConfigProfile
	}

	configProfile, missing, err := getConfigProfile(a.cfg.ProfilesDir, configProfileName)
	if err != nil {
		return ConfigProfile{}, fmt.Errorf("failed to get config profile: %w", err)
	}

	if missing {
		activity.GetLogger(ctx).Warn("config profile file not found in active or deprecated dirs, treating as empty",
			"configProfile", configProfileName)
	}

	return configProfile, nil
}

// getConfigProfile returns the parsed ConfigProfile, a boolean indicating
// whether the file was absent from both the active and deprecated dirs, and any
// read/parse error.
//
// Search order:
//  1. configProfiles/<name>.yaml  (active)
//  2. configProfiles/deprecated/<name>.yaml  (deprecated but hooks preserved)
//
// If found in the deprecated dir, the profile is returned normally so that
// teardown hooks still execute. Only when the file is absent from both
// locations is the profile treated as fully retired (missing=true, empty result).
func getConfigProfile(profilesDir, configProfileName string) (ConfigProfile, bool, error) {
	if configProfileName == "" {
		return ConfigProfile{}, false, nil
	}

	activePath := path.Join(profilesDir, "configProfiles", configProfileName+".yaml")
	deprecatedPath := path.Join(profilesDir, "configProfiles", "deprecated", configProfileName+".yaml")

	for _, profilePath := range []string{activePath, deprecatedPath} {
		configProfileBytes, err := os.ReadFile(profilePath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return ConfigProfile{}, false, fmt.Errorf("failed to read config profile %q: %w", configProfileName, err)
		}

		configProfile := ConfigProfile{}
		if err := yaml.Unmarshal(configProfileBytes, &configProfile); err != nil {
			return ConfigProfile{}, false, fmt.Errorf("failed to unmarshal config profile %q: %w", configProfileName, err)
		}
		return configProfile, false, nil
	}

	// Not found in active or deprecated dirs -- fully retired.
	return ConfigProfile{}, true, nil
}
