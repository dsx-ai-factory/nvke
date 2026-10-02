// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package container

import (
	"io"
	"log/slog"
	"strings"
	"testing"
)

func TestUnit_New(t *testing.T) {
	base := Config{
		OrgID: "org", TenantID: "tenant", SiteID: "site", VPCID: "vpc",
		SubnetID: "subnet", SSHKeyGroupID: "ssh-group", InstanceTypeID: "instance-type",
	}
	type parameters struct {
		method     string
		wantMethod string
		wantError  string
	}
	tests := map[string]parameters{
		"defaults to TPM":        {wantMethod: "tpm"},
		"trims TPM whitespace":   {method: " tpm ", wantMethod: "tpm"},
		"accepts join token":     {method: "joinToken", wantMethod: "joinToken"},
		"accepts DSX IMDS":       {method: "dsxImds", wantMethod: "dsxImds"},
		"rejects unknown method": {method: "unknown", wantError: "unsupported SPIRE attestation method"},
		"rejects incorrect case": {method: "TPM", wantError: "unsupported SPIRE attestation method"},
	}
	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := base
			cfg.SpireAttestationMethod = params.method
			service, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if params.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), params.wantError) {
					t.Fatalf("New() error = %v, want containing %q", err, params.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			if service.cfg.SpireAttestationMethod != params.wantMethod {
				t.Errorf("SPIRE attestation method = %q, want %q", service.cfg.SpireAttestationMethod, params.wantMethod)
			}
		})
	}
}
