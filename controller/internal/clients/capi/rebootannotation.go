// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	"github.com/dsx-ai-factory/nvke/controller/internal/keys"
)

// HardRebootAnnotationKeys returns NVKE's reboot key under the current domain,
// as for NICo: a CAPI template may render a NicoMachine, whose provider reads
// only the key its --reboot-annotation flag names.
func (*Client) HardRebootAnnotationKeys() []string {
	return []string{keys.MachineReboot.String()}
}
