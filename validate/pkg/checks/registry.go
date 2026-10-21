// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package checks

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Registry is the parsed, validated checks.yaml: the versioned set of checks a
// release is judged by. The version travels into every report so a verdict
// names the check set that produced it.
type Registry struct {
	Version string     `yaml:"version"`
	Checks  []CheckDef `yaml:"checks"`
}

// LoadRegistry reads and validates a checks.yaml.
func LoadRegistry(path string) (*Registry, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading check registry: %w", err)
	}
	reg, err := ParseRegistry(raw)
	if err != nil {
		return nil, fmt.Errorf("check registry %s: %w", path, err)
	}
	return reg, nil
}

// ParseRegistry validates a checks.yaml document from one byte snapshot, so
// callers hash and parse the same bytes. Unknown fields are rejected so a
// selector typo cannot silently widen a check's scope.
func ParseRegistry(raw []byte) (*Registry, error) {
	var reg Registry
	dec := yaml.NewDecoder(newBytesReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&reg); err != nil {
		return nil, fmt.Errorf("parsing: %w", err)
	}
	if reg.Version == "" {
		return nil, fmt.Errorf("version is required")
	}
	if len(reg.Checks) == 0 {
		return nil, fmt.Errorf("no checks defined")
	}
	seen := make(map[string]bool, len(reg.Checks))
	for _, c := range reg.Checks {
		if err := c.validate(); err != nil {
			return nil, err
		}
		if seen[c.ID] {
			return nil, fmt.Errorf("duplicate check id %s", c.ID)
		}
		seen[c.ID] = true
	}
	return &reg, nil
}
