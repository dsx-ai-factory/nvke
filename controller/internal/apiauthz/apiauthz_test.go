// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package apiauthz_test

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/apiauthz"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllerconfig"
)

// apiAuthz off builds no middleware. On, a bundle that can't load fails
// startup.
func TestSetup(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.DiscardHandler)
	m, err := apiauthz.Setup(t.Context(), apiauthz.Options{}, logger)
	require.NoError(t, err)
	require.Nil(t, m)

	var cfg controllerconfig.APIAuthzConfig
	cfg.Enabled = true
	cfg.Authorizer.LocalBundle.Dir = t.TempDir()
	_, err = apiauthz.Setup(t.Context(), apiauthz.Options{Config: cfg, Issuers: []string{"idp"}}, logger)
	require.ErrorContains(t, err, "load the authorization bundle")
}
