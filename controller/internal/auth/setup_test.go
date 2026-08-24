// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package auth_test

// Setup is the whole three-state decision in one place, so these are the tests
// that say which state a given configuration produces. Before it existed the
// sequence lived in cmd/worker and could only be exercised by starting the
// binary.

import (
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NVIDIA/nke/controller/internal/auth"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// The default every deployment gets. Nil and no error, so the caller keeps the
// middleware out of the chain.
func TestSetupDisabledWhenNoConfigPath(t *testing.T) {
	t.Parallel()

	authenticator, err := auth.Setup(t.Context(), auth.Options{}, discardLogger())

	require.NoError(t, err)
	assert.Nil(t, authenticator, "an empty ConfigPath is disabled, not enforcing")
}

// A set-but-unreadable path is an error, never "disabled". Read as disabled, it
// would produce a server that authenticates nobody.
func TestSetupUnreadableConfigPathIsAnError(t *testing.T) {
	t.Parallel()

	missing := filepath.Join(t.TempDir(), "not-mounted.yaml")

	authenticator, err := auth.Setup(t.Context(), auth.Options{ConfigPath: missing}, discardLogger())

	require.Error(t, err, "a set-but-unreadable path must fail, never disable")
	assert.Nil(t, authenticator)
	assert.Contains(t, err.Error(), missing, "the error should name the path that was tried")
}

// A malformed file is the same class of failure as a missing one.
func TestSetupInvalidConfigIsAnError(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, configHeader+"audiences: []\nissuers: []\n")

	authenticator, err := auth.Setup(t.Context(), auth.Options{ConfigPath: path}, discardLogger())

	require.Error(t, err)
	assert.Nil(t, authenticator)
}

// Permissive reaches the end of the sequence without any issuer's keys, which is
// what lets it run where enforcing would refuse to start.
func TestSetupPermissiveBuildsWithoutReachableKeys(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, exampleConfig)

	authenticator, err := auth.Setup(t.Context(), auth.Options{ConfigPath: path, Permissive: true}, discardLogger())

	require.NoError(t, err, "permissive must not require reachable issuer keys")
	assert.NotNil(t, authenticator)
}

// Enforcing refuses to start without reachable keys. exampleConfig's issuers do
// not resolve from a test process, which is the condition under test.
func TestSetupEnforcingRefusesWithoutReachableKeys(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, exampleConfig)

	authenticator, err := auth.Setup(t.Context(), auth.Options{ConfigPath: path}, discardLogger())

	require.Error(t, err, "enforcing must not start without every issuer's keys")
	assert.Nil(t, authenticator)
}
