// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package workflows

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/NVIDIA/nke/controller/internal/db"
)

func TestBuildKnownNodes(t *testing.T) {
	got := buildKnownNodes([]db.Node{
		{Name: "node-b", IP: "10.0.0.2"},
		{Name: "node-a", IP: "10.0.0.1"},
		{Name: "", IP: "10.0.0.3"},
		{Name: "node-c"},
	})
	assert.Equal(t, "node-a=10.0.0.1;node-b=10.0.0.2", got)
}
