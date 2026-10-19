// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nico

import (
	"github.com/dsx-ai-factory/nvke/controller/internal/keys"
)

// HardRebootAnnotationKeys returns NVKE's reboot key under the current domain.
// cluster-api-provider-nico reads only the key its --reboot-annotation flag
// names, so that flag must name this key.
func (*Client) HardRebootAnnotationKeys() []string {
	return []string{keys.MachineReboot.String()}
}
