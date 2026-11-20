// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package oscli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v2"

	oserr "github.com/dsx-ai-factory/nvke/os-image/internal/errors"
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
		{name: "version format json prints flat result", args: []string{"version", "--format", "json"}, wantExit: 0, wantStderrEmpty: true, checkJSON: true},
		{name: "version format text prints text", args: []string{"version", "--format", "text"}, wantExit: 0, wantStdoutContains: []string{"nvke-os version v1.2.3"}, wantStderrEmpty: true},
		{name: "root version flag prints text", args: []string{"--version"}, wantExit: 0, wantStdoutContains: []string{"v1.2.3"}, wantStderrEmpty: true},
		{name: "root help lists commands", args: []string{"--help"}, wantExit: 0, wantStdoutContains: []string{"version", "manifest", "verify"}},
		{name: "command help", args: []string{"version", "--help"}, wantExit: 0, wantStdoutContains: []string{"version", "--format"}},
		{name: "manifest is unsupported", args: []string{"manifest"}, wantExit: 10, wantStdoutEmpty: true, wantStderrContains: []string{"NVKE-OS-E-UNSUPPORTED"}},
		{name: "verify is unsupported", args: []string{"verify"}, wantExit: 10, wantStdoutEmpty: true, wantStderrContains: []string{"NVKE-OS-E-UNSUPPORTED"}},
		{name: "unknown format is usage error", args: []string{"version", "--format", "yaml"}, wantExit: 2, wantStdoutEmpty: true, wantStderrContains: []string{"NVKE-OS-E-USAGE"}},
		{name: "global output flag is gone", args: []string{"-o", "json", "version"}, wantExit: 2, wantStdoutEmpty: true, wantStderrContains: []string{"NVKE-OS-E-USAGE"}},
		{name: "global log level flag is gone", args: []string{"--log-level", "debug", "version"}, wantExit: 2, wantStdoutEmpty: true, wantStderrContains: []string{"NVKE-OS-E-USAGE"}},
		{name: "unknown flag is usage error", args: []string{"--no-such-flag", "version"}, wantExit: 2, wantStderrContains: []string{"NVKE-OS-E-USAGE"}},
		{name: "unknown command is usage error", args: []string{"frobnicate"}, wantExit: 2, wantStderrContains: []string{"NVKE-OS-E-USAGE"}},
		{name: "version rejects positional args", args: []string{"version", "typo"}, wantExit: 2, wantStdoutEmpty: true, wantStderrContains: []string{"NVKE-OS-E-USAGE"}},
		{name: "manifest rejects positional args", args: []string{"manifest", "extra"}, wantExit: 2, wantStderrContains: []string{"NVKE-OS-E-USAGE"}},
		{name: "help for a known command", args: []string{"--help", "version"}, wantExit: 0, wantStdoutContains: []string{"--format"}},
		{name: "help for an unknown command is usage error", args: []string{"--help", "frobnicate"}, wantExit: 2, wantStderrContains: []string{"NVKE-OS-E-USAGE"}},
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
				var result struct {
					SchemaVersion string `json:"schemaVersion"`
					Name          string `json:"name"`
					Version       string `json:"version"`
				}
				dec := json.NewDecoder(&stdout)
				dec.DisallowUnknownFields()
				require.NoError(t, dec.Decode(&result))
				require.False(t, dec.More(), "stdout must hold exactly one JSON object")
				require.Equal(t, "os.nvke.dev/version-result/v1", result.SchemaVersion)
				require.Equal(t, "nvke-os", result.Name)
				require.Equal(t, "v1.2.3", result.Version)
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

// A verb with subcommands keeps the usage contract one level down: an unknown
// subcommand, a bad flag or a stray argument is a coded usage error, never
// urfave's own exit or a silently ignored argument.
func TestSubcommandUsageContract(t *testing.T) {
	tests := []struct {
		name               string
		args               []string
		wantExit           int
		wantRan            bool
		wantStdoutEmpty    bool
		wantStderrContains string
	}{
		{name: "subcommand runs", args: []string{"repo", "add"}, wantExit: 0, wantRan: true},
		{name: "parent alone shows help", args: []string{"repo"}, wantExit: 0},
		{name: "unknown subcommand is usage error", args: []string{"repo", "frobnicate"}, wantExit: 2, wantStdoutEmpty: true, wantStderrContains: "NVKE-OS-E-USAGE"},
		{name: "unknown subcommand flag is usage error", args: []string{"repo", "add", "--bogus"}, wantExit: 2, wantStdoutEmpty: true, wantStderrContains: "NVKE-OS-E-USAGE"},
		{name: "stray subcommand argument is usage error", args: []string{"repo", "add", "extra"}, wantExit: 2, wantStdoutEmpty: true, wantStderrContains: "NVKE-OS-E-USAGE"},
		{name: "help subcommand for a known subcommand", args: []string{"repo", "help", "add"}, wantExit: 0},
		{name: "help subcommand for an unknown subcommand is usage error", args: []string{"repo", "help", "frobnicate"}, wantExit: 2, wantStderrContains: "NVKE-OS-E-USAGE"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
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

			got := oscli.Run(context.Background(), app, append([]string{"nvke-os"}, tc.args...), &stdout, &stderr)

			require.Equal(t, tc.wantExit, got, "stdout=%q stderr=%q", stdout.String(), stderr.String())
			require.Equal(t, tc.wantRan, ran)
			if tc.wantStdoutEmpty {
				require.Empty(t, stdout.String())
			}
			require.Contains(t, stderr.String(), tc.wantStderrContains)
		})
	}
}

// A cancelled or timed-out run reports the retryable timeout status, as NVKR's
// ADR-03 classifies it, unless the verb already returned a coded error.
func TestCancelledRunExitsTimeout(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	tests := []struct {
		name       string
		ctx        context.Context
		err        error
		wantExit   int
		wantStderr string
	}{
		{name: "cancelled context and uncoded error", ctx: cancelled, err: errors.New("interrupted"), wantExit: 31},
		{name: "deadline error from the verb", ctx: context.Background(), err: fmt.Errorf("probe: %w", context.DeadlineExceeded), wantExit: 31},
		{name: "cancelled context keeps a coded error", ctx: cancelled, err: oserr.New(oserr.CodeUnsupported, "x"), wantExit: 10},
		{name: "live context and uncoded error is internal", ctx: context.Background(), err: errors.New("boom"), wantExit: 1, wantStderr: "NVKE-OS-E-INTERNAL"},
		{name: "cancelled context and success is success", ctx: cancelled, err: nil, wantExit: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			command := &cli.Command{Name: "probe", Action: func(*cli.Context) error { return tc.err }}
			app := oscli.NewApp("nvke-os", "test", "v1.2.3", command)
			var stdout, stderr bytes.Buffer

			got := oscli.Run(tc.ctx, app, []string{"nvke-os", "probe"}, &stdout, &stderr)

			require.Equal(t, tc.wantExit, got, "stderr=%q", stderr.String())
			require.Contains(t, stderr.String(), tc.wantStderr)
		})
	}
}
