// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package launchlayer

const (
	// hardRebootAnnotationKey is CAPLL's own reboot-request annotation key. It's
	// hardcoded in CAPLL's controller (controllers/instance_lifecycle.go) — no
	// configurable override exists, unlike CAPNico's --reboot-annotation flag —
	// so NVKE must write this exact key for LaunchLayer-backed nodes rather than
	// the reboot key CAPNico is configured to watch.
	hardRebootAnnotationKey = "infrastructure.cluster.x-k8s.io/reboot-requested"
)

// HardRebootAnnotationKeys returns CAPLL's fixed reboot-request annotation key.
func (*Client) HardRebootAnnotationKeys() []string {
	return []string{hardRebootAnnotationKey}
}
