// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package launchlayer

// hardRebootAnnotationKey is CAPLL's own reboot-request annotation key. It's
// hardcoded in CAPLL's controller (controllers/instance_lifecycle.go) — no
// configurable override exists, unlike CAPNico's --reboot-annotation flag —
// so NVKE must write this exact key for LaunchLayer-backed nodes rather than
// the nke.nvidia.com/reboot key CAPNico is configured to watch.
const hardRebootAnnotationKey = "infrastructure.cluster.x-k8s.io/reboot-requested"

// HardRebootAnnotationKey returns CAPLL's fixed reboot-request annotation key.
func (*Client) HardRebootAnnotationKey() string {
	return hardRebootAnnotationKey
}
