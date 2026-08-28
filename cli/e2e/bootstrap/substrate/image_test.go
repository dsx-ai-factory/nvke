// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package substrate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStageAgentConfigAlwaysRebootstraps(t *testing.T) {
	dir := t.TempDir()
	err := stageAgentConfig(Config{
		SpireServerAddress: "192.0.2.1",
		SpireServerPort:    "30081",
		SpireTrustDomain:   "example.org",
	}, dir)
	if err != nil {
		t.Fatalf("stageAgentConfig() error = %v", err)
	}

	body, err := os.ReadFile(filepath.Join(dir, "agent.conf"))
	if err != nil {
		t.Fatalf("reading rendered agent config: %v", err)
	}
	if !strings.Contains(string(body), `rebootstrap_mode = "always"`) {
		t.Errorf("rendered agent config does not enable reattestation:\n%s", body)
	}
}
