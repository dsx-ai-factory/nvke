// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authn_test

// Setup is the whole three-state decision in one place, so these are the tests
// that say which state a given configuration produces. Before it existed the
// sequence lived in cmd/worker and could only be exercised by starting the
// binary.

import (
	"bytes"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/auth/authn"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// The default every deployment gets. Nil and no error, so the caller keeps the
// middleware out of the chain.
func TestSetupDisabledWhenNoConfigPath(t *testing.T) {
	t.Parallel()

	authenticator, err := authn.Setup(t.Context(), authn.Options{}, discardLogger())

	require.NoError(t, err)
	assert.Nil(t, authenticator, "an empty ConfigPath is disabled, not enforcing")
}

// A set-but-unreadable path is an error, never "disabled". Read as disabled, it
// would produce a server that authenticates nobody.
func TestSetupUnreadableConfigPathIsAnError(t *testing.T) {
	t.Parallel()

	missing := filepath.Join(t.TempDir(), "not-mounted.yaml")

	authenticator, err := authn.Setup(t.Context(), authn.Options{ConfigPath: missing}, discardLogger())

	require.Error(t, err, "a set-but-unreadable path must fail, never disable")
	assert.Nil(t, authenticator)
	assert.Contains(t, err.Error(), missing, "the error should name the path that was tried")
}

// A malformed file is the same class of failure as a missing one.
func TestSetupInvalidConfigIsAnError(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, configHeader+"audiences: []\nissuers: []\n")

	authenticator, err := authn.Setup(t.Context(), authn.Options{ConfigPath: path}, discardLogger())

	require.Error(t, err)
	assert.Nil(t, authenticator)
}

// Permissive reaches the end of the sequence without any issuer's keys, which is
// what lets it run where enforcing would refuse to start.
func TestSetupPermissiveBuildsWithoutReachableKeys(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, exampleConfig)

	authenticator, err := authn.Setup(t.Context(), authn.Options{ConfigPath: path, Permissive: true}, discardLogger())

	require.NoError(t, err, "permissive must not require reachable issuer keys")
	assert.NotNil(t, authenticator)
}

// Enforcing refuses to start without reachable keys. exampleConfig's issuers do
// not resolve from a test process, which is the condition under test.
func TestSetupEnforcingRefusesWithoutReachableKeys(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, exampleConfig)

	authenticator, err := authn.Setup(t.Context(), authn.Options{ConfigPath: path}, discardLogger())

	require.Error(t, err, "enforcing must not start without every issuer's keys")
	assert.Nil(t, authenticator)
}

// apiAuth in nvke-controller-config arrives as a Config rather than a path. From
// there it takes the same road, so permissive builds without reachable keys.
func TestSetupFromControllerConfig(t *testing.T) {
	t.Parallel()

	authenticator, err := authn.Setup(t.Context(), authn.Options{Permissive: true, Config: exampleStructConfig()}, discardLogger())

	require.NoError(t, err)
	assert.NotNil(t, authenticator)
}

// A Config that Load would refuse is refused here too, never served.
func TestSetupInvalidControllerConfigIsAnError(t *testing.T) {
	t.Parallel()

	cfg := exampleStructConfig()
	cfg.Issuers[0].Admit = nil

	authenticator, err := authn.Setup(t.Context(), authn.Options{Permissive: true, Config: cfg}, discardLogger())

	require.ErrorContains(t, err, "admit is required")
	assert.Nil(t, authenticator)
}

// Two sources could disagree about who is admitted, so neither wins.
func TestSetupRefusesBothSources(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, exampleConfig)

	authenticator, err := authn.Setup(t.Context(), authn.Options{ConfigPath: path, Permissive: true, Config: exampleStructConfig()}, discardLogger())

	require.ErrorContains(t, err, "configured twice")
	assert.Nil(t, authenticator)
}

// The startup line names where the configuration was read from, which is how an
// operator tells the chart-rendered section from an external ConfigMap.
func TestSetupLogsItsSource(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		fromFile bool // the file at ConfigPath rather than a Config
		want     string
	}{
		{name: "apiAuth in nvke-controller-config", want: `"source":"nvke-controller-config"`},
		{name: "the file at AUTH_CONFIG_PATH", fromFile: true, want: `"source":"AUTH_CONFIG_PATH"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			opts := authn.Options{Permissive: true, Config: exampleStructConfig()}
			if tt.fromFile {
				opts = authn.Options{Permissive: true, ConfigPath: writeConfig(t, exampleConfig)}
			}

			var buf bytes.Buffer
			authenticator, err := authn.Setup(t.Context(), opts, slog.New(slog.NewJSONHandler(&buf, nil)))

			require.NoError(t, err)
			require.NotNil(t, authenticator)
			assert.Contains(t, buf.String(), `"msg":"API authentication is enabled"`)
			assert.Contains(t, buf.String(), tt.want)
		})
	}
}
