// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// NodeMachineIDLabelValue is always "": a CAPI node has no provider machine
// ID the controller tracks, so it carries no machine-ID label.
func (*Client) NodeMachineIDLabelValue(db.Node) (string, error) {
	return "", nil
}
