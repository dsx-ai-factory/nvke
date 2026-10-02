// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunRequiresPaths(t *testing.T) {
	err := run(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "required")
}

func TestRunHelp(t *testing.T) {
	err := run([]string{"-h"})
	require.ErrorIs(t, err, flag.ErrHelp)
}

func TestRunRejectsPositionalArgs(t *testing.T) {
	err := run([]string{"--profiles", "p", "--output", "o", "extra"})
	require.Error(t, err)
	assert.NotErrorIs(t, err, flag.ErrHelp)
}
