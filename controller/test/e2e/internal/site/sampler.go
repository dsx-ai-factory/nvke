// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package site

import (
	"context"
	_ "embed"
	"fmt"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/infra"
)

// The sampler is a systemd unit rather than a background loop: it has to
// outlive the SSH session that started it, survive the reboot the bootstrap
// may do, and stop on a known name rather than on a `pkill` pattern.
const (
	samplerScriptPath = workDir + "/site-sampler.sh"
	samplerUnitName   = "nke-e2e-site-sampler.service"
	samplerUnitPath   = "/etc/systemd/system/" + samplerUnitName
)

//go:embed site-sampler.sh
var siteSamplerScript string

// samplerUnit is the unit file. Restart=on-failure and not always: the script
// exits 0 on its own after six hours, so an abandoned node stops sampling.
func samplerUnit() string {
	return "[Unit]\n" +
		"Description=NVKE e2e site sampler (timeline for the failure bundle)\n" +
		"After=network-online.target\n" +
		"\n" +
		"[Service]\n" +
		"Type=simple\n" +
		"ExecStart=/bin/sh " + samplerScriptPath + " " + timelineDir + "\n" +
		"Restart=on-failure\n" +
		"RestartSec=5\n" +
		"\n" +
		"[Install]\n" +
		"WantedBy=multi-user.target\n"
}

// StartSampler stages the sampler and starts it as an enabled unit. It is
// safe to call on a node where it already runs: the unit is rewritten and
// `enable --now` leaves a running unit alone.
func StartSampler(ctx context.Context, s infra.Session) error {
	if err := s.Put(ctx, samplerScriptPath, []byte(siteSamplerScript), 0o755); err != nil {
		return fmt.Errorf("stage the site sampler: %w", err)
	}
	if err := s.Put(ctx, samplerUnitPath, []byte(samplerUnit()), 0o644); err != nil {
		return fmt.Errorf("stage the site sampler unit: %w", err)
	}
	out, err := s.Run(ctx, asRoot("systemctl daemon-reload && systemctl enable --now "+samplerUnitName))
	if err != nil {
		return fmt.Errorf("start the site sampler: %w\n%s", err, tail(out))
	}
	return nil
}

// StopSampler stops and removes the unit. Every step is tolerated: a node
// that never had the sampler, or lost it to a reboot, reports nothing here.
// The timeline it wrote stays in timelineDir for the collector to copy.
func StopSampler(ctx context.Context, s infra.Session) error {
	cmd := "systemctl disable --now " + samplerUnitName + " >/dev/null 2>&1 || true; " +
		"rm -f " + shellQuote(samplerUnitPath) + "; systemctl daemon-reload || true"
	out, err := s.Run(ctx, asRoot(cmd))
	if err != nil {
		return fmt.Errorf("stop the site sampler: %w\n%s", err, tail(out))
	}
	return nil
}
