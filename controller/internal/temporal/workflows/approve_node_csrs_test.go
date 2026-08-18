// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

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
