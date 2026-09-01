// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/NVIDIA/nke/cli/internal/clients"
	api "github.com/NVIDIA/nke/controller/api"
)

// registerTemporaryNilSite keeps compatibility with CWE versions that still
// require a site row before they schedule CAPI-backed nodes. Delete this file
// and its call after the site-gating removal is available.
func registerTemporaryNilSite(ctx context.Context, client *clients.CWEClient, logger *slog.Logger) error {
	logger.Info("registering the temporary site for the CAPI cluster", "site", uuid.Nil)
	if _, err := client.RegisterSite(ctx, api.RegisterSiteJSONRequestBody{
		ID:        uuid.Nil,
		VIPRanges: []string{},
	}); err != nil {
		return fmt.Errorf("registering the temporary site: %w", err)
	}
	return nil
}
