// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controller

import "testing"

func TestEnvFallbackOrder(t *testing.T) {
	tests := []struct {
		name   string
		get    func() string
		newEnv string
		def    string
	}{
		{"namespace", Namespace, EnvNamespace, ServiceNamespace},
		{"service", Service, EnvService, ServiceName},
		{"deployment", Deployment, EnvService, DeploymentName},
		{"service account", ServiceAccount, EnvService, ServiceName},
		{"port", Port, EnvPort, ServicePort},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(tt.newEnv, "")
			if got := tt.get(); got != tt.def {
				t.Errorf("default = %q, want %q", got, tt.def)
			}
			t.Setenv(tt.newEnv, "current")
			if got := tt.get(); got != "current" {
				t.Errorf("set = %q, want current", got)
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
