// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cache

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/NVIDIA/nke/controller/internal/config"
)

type recordingOperation struct {
	id     string
	record func(id string)
}

func (op *recordingOperation) GetNextExecutionTime() time.Time {
	return time.Now()
}

func (op *recordingOperation) Execute(_ context.Context) {
	op.record(op.id)
}

func TestRefreshSiteRunsTenantDiscoveryThenMatchingSiteOps(t *testing.T) {
	const siteID = "550e8400-e29b-41d4-a716-446655440001"

	var executionOrder []string
	record := func(id string) {
		executionOrder = append(executionOrder, id)
	}

	ce := NewForgeCacheEngine(config.Config{}, nil, slog.New(slog.DiscardHandler))

	ce.AddOperation("sync-tenant-sites-tenant-a", &recordingOperation{
		id:     "sync-tenant-sites-tenant-a",
		record: record,
	})
	ce.AddOperation(fmt.Sprintf("sync-site-instances-org-%s", siteID), &recordingOperation{
		id:     "site-op-target",
		record: record,
	})
	ce.AddOperation("sync-site-instances-org-other-site", &recordingOperation{
		id:     "site-op-other",
		record: record,
	})
	ce.AddOperation("unrelated-op", &recordingOperation{
		id:     "unrelated",
		record: record,
	})

	ce.RefreshSite(context.Background(), siteID)

	require.Equal(t, []string{
		"sync-tenant-sites-tenant-a",
		"site-op-target",
	}, executionOrder)
}
