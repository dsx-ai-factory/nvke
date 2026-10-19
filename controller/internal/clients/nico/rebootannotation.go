// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nico

import (
	"github.com/dsx-ai-factory/nvke/controller/internal/keys"
)

// HardRebootAnnotationKeys returns NVKE's reboot key under both domains:
// cluster-api-provider-nico reads only the key its --reboot-annotation flag
// names, which a deployment may still set to the legacy domain.
// FIXME: https://github.com/dsx-ai-factory/nvke/issues/1718 -- write
// only the current domain once every deployment reads the new key.
func (*Client) HardRebootAnnotationKeys() []string {
	return keys.MachineReboot.All()
}
