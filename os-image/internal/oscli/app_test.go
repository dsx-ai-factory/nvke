// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package oscli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v2"

	"github.com/dsx-ai-factory/nvke/os-image/internal/oscli"
)

func TestCommandLine(t *testing.T) {
	tests := []struct {
		name               string
		args               []string
		wantExit           int
		wantStdoutContains []string
		wantStderrContains []string
		wantStdoutEmpty    bool
		wantStderrEmpty    bool
		checkJSON          bool
	}{
		{name: "version prints text", args: []string{"version"}, wantExit: 0, wantStdoutContains: []string{"v1.2.3"}, wantStderrEmpty: true},
		{name: "short output flag prints json envelope", args: []string{"-o", "json", "version"}, wantExit: 0, wantStderrEmpty: true, checkJSON: true},
		{name: "long output flag prints json envelope", args: []string{"--output", "json", "version"}, wantExit: 0, wantStderrEmpty: true, checkJSON: true},
		{name: "root help lists commands", args: []string{"--help"}, wantExit: 0, wantStdoutContains: []string{"version", "manifest", "verify"}},
		{name: "command help", args: []string{"version", "--help"}, wantExit: 0, wantStdoutContains: []string{"version"}},
		{name: "manifest is unsupported", args: []string{"manifest"}, wantExit: 10, wantStdoutEmpty: true, wantStderrContains: []string{"NVKE-OS-E-UNSUPPORTED"}},
		{name: "verify is unsupported", args: []string{"verify"}, wantExit: 10, wantStdoutEmpty: true, wantStderrContains: []string{"NVKE-OS-E-UNSUPPORTED"}},
		{name: "unknown output format is usage error", args: []string{"-o", "yaml", "version"}, wantExit: 2, wantStdoutEmpty: true, wantStderrContains: []string{"NVKE-OS-E-USAGE"}},
		{name: "unknown flag is usage error", args: []string{"--no-such-flag", "version"}, wantExit: 2, wantStderrContains: []string{"NVKE-OS-E-USAGE"}},
		{name: "unknown command is usage error", args: []string{"frobnicate"}, wantExit: 2, wantStderrContains: []string{"NVKE-OS-E-USAGE"}},
		{name: "version rejects positional args", args: []string{"version", "typo"}, wantExit: 2, wantStdoutEmpty: true, wantStderrContains: []string{"NVKE-OS-E-USAGE"}},
		{name: "manifest rejects positional args", args: []string{"manifest", "extra"}, wantExit: 2, wantStderrContains: []string{"NVKE-OS-E-USAGE"}},
		{name: "logging flags parse", args: []string{"--log-level", "debug", "--log-format", "json", "version"}, wantExit: 0},
		{name: "invalid log level is usage error", args: []string{"--log-level", "loud", "version"}, wantExit: 2, wantStderrContains: []string{"NVKE-OS-E-USAGE"}},
		{name: "invalid log format is usage error", args: []string{"--log-format", "xml", "version"}, wantExit: 2, wantStdoutEmpty: true, wantStderrContains: []string{"NVKE-OS-E-USAGE"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			app := oscli.NewApp("nvke-os", "test", "v1.2.3", oscli.VersionCommand(), oscli.ManifestCommand(), oscli.VerifyCommand())
			var stdout, stderr bytes.Buffer

			got := oscli.Run(context.Background(), app, append([]string{"nvke-os"}, tc.args...), &stdout, &stderr)

			require.Equal(t, tc.wantExit, got, "stdout=%q stderr=%q", stdout.String(), stderr.String())
			for _, s := range tc.wantStdoutContains {
				require.Contains(t, stdout.String(), s)
			}
			for _, s := range tc.wantStderrContains {
				require.Contains(t, stderr.String(), s)
			}
			if tc.wantStdoutEmpty {
				require.Empty(t, stdout.String())
			}
			if tc.wantStderrEmpty {
				require.Empty(t, stderr.String())
			}
			if tc.checkJSON {
				var env struct {
					APIVersion string `json:"apiVersion"`
					Kind       string `json:"kind"`
					Data       struct {
						Name    string `json:"name"`
						Version string `json:"version"`
					} `json:"data"`
				}
				dec := json.NewDecoder(&stdout)
				dec.DisallowUnknownFields()
				require.NoError(t, dec.Decode(&env))
				require.False(t, dec.More(), "stdout must hold exactly one JSON object")
				require.Equal(t, oscli.APIVersion, env.APIVersion)
				require.Equal(t, "Version", env.Kind)
				require.Equal(t, "nvke-os", env.Data.Name)
				require.Equal(t, "v1.2.3", env.Data.Version)
			}
		})
	}
}

func TestCommandWithOwnBeforeStillRejectsArgs(t *testing.T) {
	ran := false
	command := &cli.Command{
		Name:   "probe",
		Before: func(*cli.Context) error { ran = true; return nil },
		Action: func(*cli.Context) error { return nil },
	}
	app := oscli.NewApp("nvke-os", "test", "v1.2.3", command)
	var stdout, stderr bytes.Buffer

	got := oscli.Run(context.Background(), app, []string{"nvke-os", "probe", "extra"}, &stdout, &stderr)

	require.Equal(t, 2, got, "stderr=%q", stderr.String())
	require.Contains(t, stderr.String(), "NVKE-OS-E-USAGE")
	require.False(t, ran, "the command's Before must not run on rejected arguments")
}

func TestCommandWithSubcommandsRunsSubcommand(t *testing.T) {
	ran := false
	command := &cli.Command{
		Name: "repo",
		Subcommands: []*cli.Command{{
			Name:   "add",
			Action: func(*cli.Context) error { ran = true; return nil },
		}},
	}
	app := oscli.NewApp("nvke-os", "test", "v1.2.3", command)
	var stdout, stderr bytes.Buffer

	got := oscli.Run(context.Background(), app, []string{"nvke-os", "repo", "add"}, &stdout, &stderr)

	require.Equal(t, 0, got, "stderr=%q", stderr.String())
	require.True(t, ran, "the subcommand's Action must run")
}
