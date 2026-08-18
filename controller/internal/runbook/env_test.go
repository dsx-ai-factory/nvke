// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package runbook

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInjectEnvironment(t *testing.T) {
	tests := []struct {
		name     string
		env      map[string]string
		script   string
		expected string
	}{
		{
			name: "basic injection with shebang",
			env: map[string]string{
				"FOO": "bar",
			},
			script: `#!/usr/bin/env bash
echo "Hello"`,
			expected: `#!/usr/bin/env bash

export FOO='bar'

echo "Hello"`,
		},
		{
			name: "multiple variables with shebang",
			env: map[string]string{
				"FOO": "bar",
				"BAZ": "qux",
			},
			script: `#!/bin/bash
echo "Hello"`,
			expected: `#!/bin/bash

export BAZ='qux'
export FOO='bar'

echo "Hello"`,
		},
		{
			name: "script without shebang",
			env: map[string]string{
				"FOO": "bar",
				"BAZ": "qux",
			},
			script: `echo "Hello"`,
			expected: `export BAZ='qux'
export FOO='bar'

echo "Hello"`,
		},
		{
			name: "empty environment",
			env:  map[string]string{},
			script: `#!/bin/bash
echo "Hello"`,
			expected: `#!/bin/bash
echo "Hello"`,
		},
		{
			name: "empty script",
			env: map[string]string{
				"FOO": "bar",
			},
			script: "",
			expected: `export FOO='bar'

`,
		},
		{
			name: "values with spaces and special characters",
			env: map[string]string{
				"PATH":     "/usr/bin:/usr/local/bin",
				"GREETING": "Hello World",
			},
			script: `#!/bin/bash
echo "Test"`,
			expected: `#!/bin/bash

export GREETING='Hello World'
export PATH='/usr/bin:/usr/local/bin'

echo "Test"`,
		},
		{
			name: "values with spaces and special characters without shebang",
			env: map[string]string{
				"PATH":     "/usr/bin:/usr/local/bin",
				"GREETING": "Hello World",
			},
			script: `echo "Test"`,
			expected: `export GREETING='Hello World'
export PATH='/usr/bin:/usr/local/bin'

echo "Test"`,
		},
		{
			name: "value with command substitution is single-quoted (not evaluated)",
			env: map[string]string{
				"INJECT": "$(id)",
			},
			script: `#!/bin/bash
echo "Hi"`,
			expected: `#!/bin/bash

export INJECT='$(id)'

echo "Hi"`,
		},
		{
			name: "value with embedded single quote is escaped",
			env: map[string]string{
				"QUOTED": "it's fine",
			},
			script: `#!/bin/bash
echo "Hi"`,
			expected: `#!/bin/bash

export QUOTED='it'\''s fine'

echo "Hi"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := InjectEnvironment(tt.env, tt.script)
			require.NoError(t, err)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestInjectEnvironment_RejectsInvalidKeys(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
	}{
		{"newline injection", map[string]string{"FOO\nexport BAR": "evil"}},
		{"command substitution", map[string]string{"FOO=$(id)": "x"}},
		{"semicolon", map[string]string{"FOO;BAR": "x"}},
		{"space", map[string]string{"FOO BAR": "x"}},
		{"leading digit", map[string]string{"1FOO": "x"}},
		{"dash", map[string]string{"FOO-BAR": "x"}},
		{"empty key", map[string]string{"": "x"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := InjectEnvironment(tt.env, "#!/bin/bash\necho hi")
			assert.ErrorContains(t, err, "invalid environment variable key")
		})
	}
}

func TestShellQuote(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", "''"},
		{"plain", "hello", "'hello'"},
		{"spaces", "hello world", "'hello world'"},
		{"double-quote", `hello "world"`, `'hello "world"'`},
		{"command-substitution", "$(rm -rf /)", "'$(rm -rf /)'"},
		{"backtick", "`whoami`", "'`whoami`'"},
		{"semicolon-chain", "foo; rm -rf /", "'foo; rm -rf /'"},
		{"pipe", "foo | tee /etc/passwd", "'foo | tee /etc/passwd'"},
		{"dollar-var", "$HOME", "'$HOME'"},
		{"glob", "*.txt", "'*.txt'"},
		{"single-quote", "it's", `'it'\''s'`},
		{"only-single-quote", "'", `''\'''`},
		{"adjacent-single-quotes", "''", `''\'''\'''`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shellQuote(tc.in)
			if got != tc.want {
				t.Errorf("shellQuote(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
