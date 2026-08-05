//go:build gofail

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package framework

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/testsuite"
)

const (
	TemporalCLIPathEnv        = "TEMPORAL_CLI_PATH"
	TemporalCLIDownloadDirEnv = "TEMPORAL_CLI_DOWNLOAD_DIR"
)

// StartTemporalDevServer starts a local Temporal server for gofail e2e tests.
func StartTemporalDevServer(t testing.TB, ctx context.Context) *testsuite.DevServer {
	t.Helper()

	server, err := testsuite.StartDevServer(ctx, TemporalDevServerOptions())
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, server.Stop())
	})
	return server
}

// TemporalDevServerOptions configures the SDK dev server with explicit CLI inputs.
func TemporalDevServerOptions() testsuite.DevServerOptions {
	opts := testsuite.DevServerOptions{
		ClientOptions: &client.Options{Namespace: "default"},
		LogLevel:      "error",
	}
	if path := os.Getenv(TemporalCLIPathEnv); path != "" {
		opts.ExistingPath = path
		return opts
	}
	if dir := os.Getenv(TemporalCLIDownloadDirEnv); dir != "" {
		opts.CachedDownload.DestDir = dir
	}
	return opts
}

// TaskQueueName derives a Temporal-safe task queue name from a test name.
func TaskQueueName(prefix, testName string) string {
	return prefix + "-" + strings.NewReplacer("/", "-", " ", "-").Replace(testName)
}
