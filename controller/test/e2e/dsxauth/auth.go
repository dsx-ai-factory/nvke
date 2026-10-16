// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package dsxauth exposes the controller E2E suite's SSA exchange to sibling
// test modules without exposing its internal infrastructure types.
package dsxauth

import (
	"context"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/infra/dsx"
)

// MintToken exchanges the named datahall's SSA credentials for a NICo token.
// It reads SSA_ISSUER and DSX_<DATAHALL>_SSA_CLIENT_ID / _SECRET using the
// existing DSX implementation. Each call performs an exchange; callers must
// obtain a fresh token when needed and must not log it.
func MintToken(ctx context.Context, datahall string) (string, error) {
	return dsx.MintToken(ctx, dsx.Site{Key: datahall})
}
