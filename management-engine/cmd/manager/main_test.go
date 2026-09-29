// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCommandFlags(t *testing.T) {
	// Keep these cases before startup; SetupSignalHandler is process-global.
	t.Setenv("KUBECONFIG", "/missing/config")
	// Help and version must work without a usable kubeconfig.
	require.ErrorIs(t, run([]string{"--help"}), flag.ErrHelp)
	require.NoError(t, run([]string{"--version"}))
	err := run([]string{"--log-level=invalid"})
	require.ErrorIs(t, err, errFlagParse)
	require.ErrorContains(t, err, "invalid")
	require.ErrorContains(t, run([]string{"unexpected"}), "positional")
	require.ErrorContains(t, run([]string{"--kubeconfig=/missing/config"}), "load management kubeconfig")
}
