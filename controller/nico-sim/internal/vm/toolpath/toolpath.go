// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
// Package toolpath locates optional host VM tools without requiring a package manager.
package toolpath

import (
	"os"
	"os/exec"
	"path/filepath"
)

// Find locates an optional host tool: an env var override takes precedence
// over a PATH lookup. An override is returned as-is; exec reports a bad one
// with its path. Returns "" if nothing resolves.
func Find(name, env string) string {
	if override := os.Getenv(env); override != "" {
		return override
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return ""
}

// VMNet finds an explicitly configured or installed vmnet executable.
func VMNet(name, env string) string {
	if p := Find(name, env); p != "" {
		return p
	}
	for _, prefix := range []string{"/opt/vmnet-helper/bin", "/opt/homebrew/opt/vmnet-helper/libexec", "/usr/local/opt/vmnet-helper/libexec"} {
		p := filepath.Join(prefix, name)
		if isExecutableRegularFile(p) {
			return p
		}
	}
	return ""
}

func isExecutableRegularFile(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
}
