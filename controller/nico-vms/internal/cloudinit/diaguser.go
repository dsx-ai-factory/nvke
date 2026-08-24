// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloudinit

import (
	"strings"

	"go.yaml.in/yaml/v4"
)

// InjectDiagUser merges a diagnostic SSH user into cloud-init user-data.
// If the user-data is not a #cloud-config document, it is returned unchanged.
// The injected user has passwordless sudo for diagnostic command execution.
func InjectDiagUser(userData, user, password string) string {
	if !strings.HasPrefix(strings.TrimSpace(userData), "#cloud-config") {
		return userData
	}

	var cfg map[string]interface{}
	body := strings.TrimPrefix(strings.TrimSpace(userData), "#cloud-config")
	if err := yaml.Unmarshal([]byte(body), &cfg); err != nil {
		return userData // unparseable, return unchanged
	}
	if cfg == nil {
		cfg = make(map[string]interface{})
	}

	diagUser := map[string]interface{}{
		"name":              user,
		"plain_text_passwd": password,
		"lock_passwd":       false,
		"sudo":              "ALL=(ALL) NOPASSWD:ALL",
		"shell":             "/bin/bash",
	}

	// Merge into existing users list or create one.
	if existing, ok := cfg["users"].([]interface{}); ok {
		cfg["users"] = append(existing, diagUser)
	} else {
		// "default" preserves the image's default user (e.g. dgxc-admin).
		cfg["users"] = []interface{}{"default", diagUser}
	}

	// Ensure password auth is enabled for SSH.
	cfg["ssh_pwauth"] = true

	out, err := yaml.Marshal(cfg)
	if err != nil {
		return userData
	}
	return "#cloud-config\n" + string(out)
}
