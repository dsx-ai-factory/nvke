// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package apiauthz_test

import (
	"log/slog"
	"regexp"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/api"
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

// Middleware derives the action by lowercasing the first letter of the
// generated operation name. That equals the operationId only when the
// operationId starts lowercase and holds only letters and digits.
func TestOperationIDsSurviveTheGoName(t *testing.T) {
	t.Parallel()

	spec, err := openapi3.NewLoader().LoadFromData([]byte(api.OpenapiYAML))
	require.NoError(t, err)
	valid := regexp.MustCompile(`^[a-z][A-Za-z0-9]*$`)
	for path, item := range spec.Paths.Map() {
		for method, op := range item.Operations() {
			require.Regexp(t, valid, op.OperationID, "%s %s", method, path)
		}
	}
}
