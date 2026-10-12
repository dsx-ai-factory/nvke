// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nico

import (
	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
)

// HardRebootAnnotationKey returns NVKE's own reboot-annotation key. CAPNico's
// `--reboot-annotation` flag is deployed configured to this same value, so
// writing it triggers an out-of-band CAPNico power cycle.
func (*Client) HardRebootAnnotationKey() string {
	return constants.MachineAnnotationKeyReboot
}
