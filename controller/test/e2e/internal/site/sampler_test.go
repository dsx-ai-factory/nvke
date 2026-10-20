// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package site

import (
	"strings"
	"testing"
)

func TestStartSamplerInstallsAUnit(t *testing.T) {
	s := newFakeSession()
	if err := StartSampler(t.Context(), s); err != nil {
		t.Fatalf("StartSampler: %v", err)
	}
	script, ok := s.puts[samplerScriptPath]
	if !ok || script.mode != 0o755 || string(script.data) != siteSamplerScript {
		t.Errorf("the sampler was not staged executable at %s", samplerScriptPath)
	}
	unit, ok := s.puts[samplerUnitPath]
	if !ok || unit.mode != 0o644 {
		t.Fatalf("the unit was not staged at %s", samplerUnitPath)
	}
	for _, want := range []string{
		"ExecStart=/bin/sh /var/lib/nke-e2e/site-sampler.sh /var/lib/nke-e2e/site-timeline",
		"Restart=on-failure",
		"WantedBy=multi-user.target",
	} {
		if !strings.Contains(string(unit.data), want) {
			t.Errorf("the unit lacks %q:\n%s", want, unit.data)
		}
	}
	if strings.Contains(string(unit.data), "Restart=always") {
		t.Error("Restart=always would restart the sampler after its own clean exit")
	}
	if run := s.cmd(t, "systemctl"); !strings.Contains(run, "systemctl daemon-reload && systemctl enable --now nke-e2e-site-sampler.service") {
		t.Errorf("the unit was not enabled and started: %s", run)
	}
}

// Stopping goes by the unit's name. A `pkill` pattern once killed the SSH
// session carrying it, and a unit stop cannot do that.
func TestStopSamplerNeverPkills(t *testing.T) {
	s := newFakeSession()
	if err := StopSampler(t.Context(), s); err != nil {
		t.Fatalf("StopSampler: %v", err)
	}
	run := s.cmd(t, "systemctl")
	for _, want := range []string{"systemctl disable --now nke-e2e-site-sampler.service", "rm -f /etc/systemd/system/nke-e2e-site-sampler.service", "systemctl daemon-reload"} {
		if !strings.Contains(run, want) {
			t.Errorf("the stop lacks %q: %s", want, run)
		}
	}
	for _, forbidden := range []string{"pkill", "killall", " kill "} {
		if strings.Contains(run, forbidden) {
			t.Errorf("the stop uses %q: %s", forbidden, run)
		}
	}
}

// The sampler reads and appends to its own log, nothing else.
func TestSamplerScriptOnlyReads(t *testing.T) {
	for _, line := range scriptLines(siteSamplerScript) {
		if strings.Contains(line, "kubectl") {
			for _, verb := range []string{"delete", "apply", "patch", "edit", "scale", "create", "rollout", "exec"} {
				if strings.Contains(line, " "+verb+" ") {
					t.Errorf("the sampler runs kubectl %s:\n%s", verb, line)
				}
			}
			if !strings.Contains(line, "--request-timeout") {
				t.Errorf("a kubectl call without --request-timeout:\n%s", line)
			}
		}
		if strings.Contains(line, ">") && !strings.Contains(line, "$LOG") && !strings.Contains(line, "2>&1") && !strings.Contains(line, ">/dev/null") {
			t.Errorf("the sampler writes somewhere other than its log:\n%s", line)
		}
	}
}
