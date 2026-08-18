// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package runbook

import (
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
)

//go:embed scripts/*.sh
var scriptsFS embed.FS

// Script returns the embedded shell-script contents for the named runbook.
func Script(name string) (string, error) {
	b, err := scriptsFS.ReadFile(path.Join("scripts", name+".sh"))
	if err != nil {
		return "", fmt.Errorf("runbook %q not found: %w", name, err)
	}
	return string(b), nil
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
