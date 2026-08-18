// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package runbook

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// envKeyPattern matches a POSIX-compatible environment variable identifier.
// Restricting keys to this pattern prevents shell injection via crafted keys
// like "FOO\nexport BAR=evil" or "FOO=$(malicious)" — without it, an
// attacker-controlled key would be inserted raw into an `export KEY=VALUE`
// statement that is later sourced as root.
var envKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// InjectEnvironment takes a map of environment variables and a script content string,
// and injects export statements for each environment variable. If a shebang line is present,
// the exports are added after it, otherwise they are added at the beginning of the script.
// It returns an error if any environment variable key is not a valid POSIX shell
// identifier (matching [A-Za-z_][A-Za-z0-9_]*).
func InjectEnvironment(env map[string]string, script string) (string, error) {
	// Split script into lines for processing
	lines := strings.Split(script, "\n")

	// Create sorted list of environment variables
	keys := make([]string, 0, len(env))
	for key := range env {
		if !envKeyPattern.MatchString(key) {
			return "", fmt.Errorf("invalid environment variable key %q: must match %s", key, envKeyPattern.String())
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)

	// Create export statements in sorted order. This is done to ensure that the order of
	// environment variables is deterministic and consistent across runs (for easier testing).
	exports := make([]string, 0, len(keys))
	for _, key := range keys {
		exports = append(exports, fmt.Sprintf("export %s=%s", key, shellQuote(env[key])))
	}

	// If there are no environment variables to inject, return original script
	if len(exports) == 0 {
		return script, nil
	}

	// Determine where to insert exports based on presence of shebang
	var result []string
	if len(lines) > 0 && strings.HasPrefix(lines[0], "#!") {
		// Insert after shebang
		result = []string{
			lines[0], // shebang line
			"",       // empty line for readability
		}
		result = append(result, exports...)
		result = append(result, "") // empty line for readability
		if len(lines) > 1 {
			result = append(result, lines[1:]...)
		}
	} else {
		// Insert at beginning
		result = append(result, exports...) // Add exports
		result = append(result, "")         // Add trailing empty line
		result = append(result, lines...)   // Add original script
	}

	return strings.Join(result, "\n"), nil
}

// shellQuote returns s wrapped so it is safe to embed as a single token
// in a POSIX shell command. It wraps s in single quotes and replaces any
// embedded single quote with the four-character close-escape-reopen
// sequence, so the result is inert to shell metacharacters, command
// substitution, and word splitting.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
