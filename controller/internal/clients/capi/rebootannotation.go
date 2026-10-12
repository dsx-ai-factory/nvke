// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
)

// HardRebootAnnotationKey returns NVKE's own reboot-annotation key.
// CAPD (the only real consumer of this generic/opaque path) has no
// controller watching it, so this is unchanged behavior from before this
// capability existed, not a working hard-reboot path.
func (*Client) HardRebootAnnotationKey() string {
	return constants.MachineAnnotationKeyReboot
}
