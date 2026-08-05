// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

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
