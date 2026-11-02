// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import "testing"

func TestEnvFallbackOrder(t *testing.T) {
	tests := []struct {
		name        string
		get         func() string
		newEnv      string
		previousEnv string
		oldEnv      string
		def         string
	}{
		{"namespace", Namespace, EnvNamespace, PreviousEnvNamespace, LegacyEnvNamespace, ServiceNamespace},
		{"service", Service, EnvService, PreviousEnvService, LegacyEnvService, ServiceName},
		{"deployment", Deployment, EnvService, PreviousEnvService, LegacyEnvService, DeploymentName},
		{"service account", ServiceAccount, EnvService, PreviousEnvService, LegacyEnvService, ServiceName},
		{"port", Port, EnvPort, PreviousEnvPort, LegacyEnvPort, ServicePort},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(tt.newEnv, "")
			t.Setenv(tt.previousEnv, "")
			t.Setenv(tt.oldEnv, "")
			if got := tt.get(); got != tt.def {
				t.Errorf("default = %q, want %q", got, tt.def)
			}
			t.Setenv(tt.oldEnv, "legacy")
			if got := tt.get(); got != "legacy" {
				t.Errorf("legacy only = %q, want legacy", got)
			}
			t.Setenv(tt.previousEnv, "nke")
			if got := tt.get(); got != "nke" {
				t.Errorf("NKE_CONTROLLER and legacy set = %q, want nke", got)
			}
			t.Setenv(tt.newEnv, "current")
			if got := tt.get(); got != "current" {
				t.Errorf("both set = %q, want current", got)
			}
		})
	}
}

func TestDefaults(t *testing.T) {
	for _, v := range []string{ServiceNamespace, ServiceName, DeploymentName} {
		if v != "nvke-controller" {
			t.Errorf("default %q, want nvke-controller", v)
		}
	}
}
