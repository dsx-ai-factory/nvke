// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package runbook

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
)

// Assert that one random runbook is embedded.
// The actual runbook chosen is not important.
func TestScript(t *testing.T) {
	script, err := Script("reboot")

	require.NoError(t, err)
	require.Contains(t, script, "Rebooting the system now")
}

func TestNames(t *testing.T) {
	names := Names()

	require.NotEmpty(t, names)
	require.Contains(t, names, "reboot")
	require.True(t, sort.StringsAreSorted(names))
}
