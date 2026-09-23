// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package otelx

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nke/controller/internal/config"
)

func TestInitDisabledIsNoop(t *testing.T) {
	t.Parallel()

	shutdown, err := Init(context.Background(), config.TracingConfig{Enabled: false}, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	require.NoError(t, shutdown(context.Background()))
}

func TestInitEnabledWithoutEndpointIsNoop(t *testing.T) {
	t.Parallel()

	shutdown, err := Init(context.Background(), config.TracingConfig{Enabled: true, Endpoint: ""}, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	require.NoError(t, shutdown(context.Background()))
}

func TestStripOTLPScheme(t *testing.T) {
	t.Parallel()

	require.Equal(t, "localhost:4317", stripOTLPScheme("http://localhost:4317"))
	require.Equal(t, "localhost:4317", stripOTLPScheme("https://localhost:4317"))
	require.Equal(t, "localhost:4317", stripOTLPScheme("localhost:4317"))
}

func TestHTTPClientWrapsTransport(t *testing.T) {
	t.Parallel()

	client := HTTPClient(nil)
	require.NotNil(t, client)
	require.NotNil(t, client.Transport)
}
