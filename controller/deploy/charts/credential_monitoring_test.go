// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package charts

import (
	"os/exec"
	"strings"
	"testing"
)

func TestCredentialMonitoringRuleInstallationAndConfig(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm required for render checks")
	}
	for _, test := range []struct {
		name     string
		args     []string
		wantRule bool
	}{
		{"default optional rules absent", nil, false},
		{"enabled rules selected", []string{"--set", "credentialMonitoring.alerts.enabled=true", "--set", "credentialMonitoring.alerts.labels.release=prometheus"}, true},
		{"umbrella monitoring kill switch", []string{"--set", "credentialMonitoring.alerts.enabled=true", "--set", "global.monitoring.enabled=false"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			args := append([]string{"template", "test", "controller"}, test.args...)
			output, err := exec.CommandContext(t.Context(), helm, args...).CombinedOutput()
			if err != nil {
				t.Fatalf("helm template: %v\n%s", err, output)
			}
			rendered := string(output)
			if strings.Contains(rendered, "kind: PrometheusRule") != test.wantRule {
				t.Fatalf("unexpected rule presence for %s", test.name)
			}
			if test.wantRule && (!strings.Contains(rendered, "release: prometheus") || !strings.Contains(rendered, "alert: NVKECertificateExpiring")) {
				t.Fatal("rule selector or expressions missing")
			}
			// The setting reaches the controller through nvke-controller-config, not
			// through an environment variable the controller no longer reads.
			if !strings.Contains(rendered, "warningDays: 45") || strings.Contains(rendered, "name: CERTIFICATE_WARNING_DAYS") {
				t.Fatal("monitoring config not wired through nvke-controller-config")
			}
		})
	}
}
