// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package otelx

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/NVIDIA/nke/controller/internal/config"
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
