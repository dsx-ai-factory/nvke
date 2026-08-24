// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package runbook

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

//go:embed scripts/*.sh
var scriptsFS embed.FS

const (
	runbookNamePattern = `^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`
)

// Script returns the shell-script contents for the named runbook. Profile-owned
// scripts in runbooksDir take precedence over controller-owned embedded scripts.
func Script(runbooksDir, name string) (string, error) {
	if err := validateName(name); err != nil {
		return "", err
	}

	if runbooksDir != "" {
		script, err := externalScript(runbooksDir, name)
		if err == nil {
			return script, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}

	b, err := scriptsFS.ReadFile(path.Join("scripts", name+".sh"))
	if err != nil {
		return "", fmt.Errorf("runbook %q not found in %q or embedded scripts: %w", name, runbooksDir, err)
	}
	return string(b), nil
}

func externalScript(runbooksDir, name string) (string, error) {
	if err := validateName(name); err != nil {
		return "", err
	}

	runbookPath := filepath.Join(runbooksDir, name+".sh")
	b, err := os.ReadFile(runbookPath)
	if err != nil {
		return "", fmt.Errorf("failed to read runbook %q from %q: %w", name, runbooksDir, err)
	}
	return string(b), nil
}

func validateName(name string) error {
	valid, err := regexp.MatchString(runbookNamePattern, name)
	if err != nil {
		return fmt.Errorf("compile runbook name pattern: %w", err)
	}
	if !valid {
		return fmt.Errorf("invalid runbook name %q", name)
	}
	return nil
}

// Names lists all embedded runbook names (filename without the .sh suffix).
func Names() []string {
	entries, _ := fs.ReadDir(scriptsFS, "scripts")
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, strings.TrimSuffix(e.Name(), ".sh"))
	}
	sort.Strings(out)
	return out
}
