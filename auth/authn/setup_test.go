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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/auth/authn"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// mustParseConfig is parseConfig for a configuration the test needs to be valid.
func mustParseConfig(t *testing.T, contents string) *authn.Config {
	t.Helper()
	cfg, err := parseConfig(contents)
	require.NoError(t, err)
	return cfg
}

// The default every deployment gets. Nil and no error, so the caller keeps the
// middleware out of the chain.
func TestSetupDisabledWhenNoConfig(t *testing.T) {
	t.Parallel()

	authenticator, err := authn.Setup(t.Context(), authn.Options{}, discardLogger())

	require.NoError(t, err)
	assert.Nil(t, authenticator, "a nil Config is disabled, not enforcing")
}

// An invalid Config is an error, never "disabled". Read as disabled, it would
// produce a server that authenticates nobody.
func TestSetupInvalidConfigIsAnError(t *testing.T) {
	t.Parallel()

	cfg := &authn.Config{Audiences: []string{}, Issuers: []authn.Issuer{}}

	authenticator, err := authn.Setup(t.Context(), authn.Options{Config: cfg}, discardLogger())

	require.Error(t, err)
	assert.Nil(t, authenticator)
}

// Permissive reaches the end of the sequence without any issuer's keys, which is
// what lets it run where enforcing would refuse to start.
func TestSetupPermissiveBuildsWithoutReachableKeys(t *testing.T) {
	t.Parallel()

	cfg := mustParseConfig(t, exampleConfig)

	authenticator, err := authn.Setup(t.Context(), authn.Options{Config: cfg, Permissive: true}, discardLogger())

	require.NoError(t, err, "permissive must not require reachable issuer keys")
	assert.NotNil(t, authenticator)
}

// Enforcing refuses to start without reachable keys. exampleConfig's issuers do
// not resolve from a test process, which is the condition under test.
func TestSetupEnforcingRefusesWithoutReachableKeys(t *testing.T) {
	t.Parallel()

	cfg := mustParseConfig(t, exampleConfig)

	authenticator, err := authn.Setup(t.Context(), authn.Options{Config: cfg}, discardLogger())

	require.Error(t, err, "enforcing must not start without every issuer's keys")
	assert.Nil(t, authenticator)
}

// A Config built in Go, as controllerconfig hands over apiAuth, takes the same
// road, so permissive builds without reachable keys.
func TestSetupFromControllerConfig(t *testing.T) {
	t.Parallel()

	authenticator, err := authn.Setup(t.Context(), authn.Options{Permissive: true, Config: exampleStructConfig()}, discardLogger())

	require.NoError(t, err)
	assert.NotNil(t, authenticator)
}

// Setup runs ValidateStartup itself, so a Config that skipped it is refused
// here too, never served.
func TestSetupInvalidControllerConfigIsAnError(t *testing.T) {
	t.Parallel()

	cfg := exampleStructConfig()
	cfg.Issuers[0].Admit = nil

	authenticator, err := authn.Setup(t.Context(), authn.Options{Permissive: true, Config: cfg}, discardLogger())

	require.ErrorContains(t, err, "admit is required")
	assert.Nil(t, authenticator)
}

// The startup line says authentication is on, in which mode, and for which
// issuers, so an operator can confirm what the controller is enforcing.
func TestSetupLogsEnabled(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	authenticator, err := authn.Setup(t.Context(), authn.Options{Permissive: true, Config: exampleStructConfig()},
		slog.New(slog.NewJSONHandler(&buf, nil)))

	require.NoError(t, err)
	require.NotNil(t, authenticator)
	assert.Contains(t, buf.String(), `"msg":"API authentication is enabled"`)
	assert.Contains(t, buf.String(), `"mode":"permissive"`)
	assert.Contains(t, buf.String(), `"issuers":["k8s"]`)
}
