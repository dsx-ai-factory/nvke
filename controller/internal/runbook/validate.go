// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package runbook

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"go.yaml.in/yaml/v4"
)

type configProfile struct {
	Hooks configProfileHooks `yaml:"hooks"`
}

type configProfileHooks struct {
	BeforeJoinNode   []profileHook `yaml:"beforeJoinNode"`
	AfterJoinNode    []profileHook `yaml:"afterJoinNode"`
	BeforeRemoveNode []profileHook `yaml:"beforeRemoveNode"`
	AfterRemoveNode  []profileHook `yaml:"afterRemoveNode"`
}

type profileHook struct {
	Runbook *profileRunbook `yaml:"runbook"`
}

type profileRunbook struct {
	Name string `yaml:"name"`
}

// ValidateProfileReferences verifies that every config-profile lifecycle hook
// resolves to an external script. Embedded controller runbooks do not satisfy
// this contract because profile hooks are owned by the profiles bundle.
func ValidateProfileReferences(profilesDir, runbooksDir string) error {
	configProfilesDir := filepath.Join(profilesDir, "configProfiles")
	configProfilesRoot, err := os.OpenRoot(configProfilesDir)
	if err != nil {
		return fmt.Errorf("open config profiles directory %q: %w", configProfilesDir, err)
	}
	defer func() { _ = configProfilesRoot.Close() }()

	err = fs.WalkDir(configProfilesRoot.FS(), ".", func(profilePath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(profilePath) != ".yaml" {
			return nil
		}

		profileBytes, err := configProfilesRoot.ReadFile(profilePath)
		if err != nil {
			return fmt.Errorf("read config profile %q: %w", profilePath, err)
		}

		var profile configProfile
		if err := yaml.Unmarshal(profileBytes, &profile); err != nil {
			return fmt.Errorf("parse config profile %q: %w", profilePath, err)
		}

		for _, name := range profile.hookRunbookNames() {
			if _, err := externalScript(runbooksDir, name); err != nil {
				return fmt.Errorf("config profile %q references invalid runbook %q: %w", profilePath, name, err)
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("validate config-profile runbooks under %q: %w", configProfilesDir, err)
	}
	return nil
}

func (p configProfile) hookRunbookNames() []string {
	hooks := make([]profileHook, 0,
		len(p.Hooks.BeforeJoinNode)+len(p.Hooks.AfterJoinNode)+
			len(p.Hooks.BeforeRemoveNode)+len(p.Hooks.AfterRemoveNode))
	hooks = append(hooks, p.Hooks.BeforeJoinNode...)
	hooks = append(hooks, p.Hooks.AfterJoinNode...)
	hooks = append(hooks, p.Hooks.BeforeRemoveNode...)
	hooks = append(hooks, p.Hooks.AfterRemoveNode...)

	names := make([]string, 0, len(hooks))
	for _, hook := range hooks {
		if hook.Runbook != nil {
			names = append(names, hook.Runbook.Name)
		}
	}
	return names
}
