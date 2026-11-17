// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package resolve

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// DefaultToolVersions is the file that owns every tool pin in the repository.
const DefaultToolVersions = "make/tool-versions.mk"

// MakeVar reads a pin out of a make include.
//
// The pin is read where it lives and never copied, so there is one owner for
// the value and a plan cannot disagree with the build that uses it.
type MakeVar struct {
	// Dir is the repository root the path is relative to.
	Dir string

	// Path is the make include, relative to Dir.
	Path string

	// Var is the variable to read.
	Var string
}

// Resolve returns the variable's value.
//
// Only a literal is read. A value built from other variables or shell output
// is not a pin, and returning its unexpanded text would put a make expression
// in a plan.
//
// With InputOverrideKey the file is not read and the named value is returned. A
// tool pin has no list of valid values to check it against.
func (m MakeVar) Resolve(inputs map[string]string) (map[string]string, error) {
	if v := inputs[InputOverrideKey]; v != "" {
		return map[string]string{ResultVersionKey: v}, nil
	}

	path := filepath.Join(m.Dir, m.Path)
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", m.Path, err)
	}
	defer f.Close()

	assign := regexp.MustCompile(`^` + regexp.QuoteMeta(m.Var) + `\s*[?:]?=\s*(.*)$`)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		match := assign.FindStringSubmatch(scanner.Text())
		if match == nil {
			continue
		}
		value, _, _ := strings.Cut(match[1], "#")
		value = strings.TrimSpace(value)
		if value == "" {
			return nil, fmt.Errorf("%s in %s is empty", m.Var, m.Path)
		}
		if strings.ContainsAny(value, "$`") {
			return nil, fmt.Errorf("%s in %s is %q, which is computed rather than a pin", m.Var, m.Path, value)
		}
		return map[string]string{ResultVersionKey: value}, nil
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading %s: %w", m.Path, err)
	}
	return nil, fmt.Errorf("%s does not set %s", m.Path, m.Var)
}

// Validate accepts no declared input, because the pin is read from the file.
func (MakeVar) Validate(inputs map[string]string) error { return takesOnly(inputs) }
