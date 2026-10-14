// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloudinit

import (
	"crypto/sha256"
	"fmt"
	"strings"
)

// Exact blob integrity for the local plain profile's shell command, not a
// structured identity or artifact receipt. The source is the additive profile
// patch at NVKR historical commit 71b356f5899efb759571f0ea51781ead3f7c466a.
// Only this complete command is adapted; changed variants fail before output.
const nativeLocalBootcmdBlobSHA256 = "8770a832057f672f8318d064aaa69c19ab8be0a943831995dbe377370bd04900"

const legacyLocalInterfaceGuard = `case "$candidate" in eth0|enp1s0) ;; *) exit 1 ;; esac`

func projectNativeLocalBootcmd(cfg map[string]interface{}) error {
	current, exists := cfg["bootcmd"]
	if !exists {
		return nil
	}
	commands, ok := current.([]interface{})
	if !ok {
		return fmt.Errorf("native cloud-config bootcmd must be a sequence")
	}
	adapted := 0
	for i, command := range commands {
		body, ok := command.(string)
		if !ok {
			continue // Other caller commands remain owned by cloud-init.
		}
		if !strings.Contains(body, legacyLocalInterfaceGuard) && !strings.Contains(body, "/etc/default/grub.d/zz-nkd-local-network.cfg") {
			continue
		}
		if fmt.Sprintf("%x", sha256.Sum256([]byte(body))) != nativeLocalBootcmdBlobSHA256 || adapted != 0 {
			return fmt.Errorf("native local interface adapter requires the exact single supported boot command")
		}
		commands[i] = strings.Replace(body, legacyLocalInterfaceGuard,
			`case "$candidate" in eth0|enp1s0|enp0s1) ;; *) exit 1 ;; esac`, 1)
		adapted++
	}
	cfg["bootcmd"] = commands
	return nil
}
