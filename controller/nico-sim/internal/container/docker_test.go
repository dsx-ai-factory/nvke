// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package container

import (
	"strings"
	"testing"
)

func TestUnit_InstanceStatusForContainer(t *testing.T) {
	t.Parallel()

	type parameters struct {
		running          bool
		tPMEKCertificate string
		labels           map[string]string
	}
	tests := map[string]struct {
		parameters parameters
		want       string
	}{
		"stopped":                           {parameters: parameters{labels: map[string]string{labelSpireAttestationMethod: "dsxImds"}}, want: "Error"},
		"TPM awaiting EK certificate":       {parameters: parameters{running: true, labels: map[string]string{labelSpireAttestationMethod: "tpm"}}, want: "Pending"},
		"TPM with EK certificate":           {parameters: parameters{running: true, tPMEKCertificate: "certificate", labels: map[string]string{labelSpireAttestationMethod: "tpm"}}, want: "Ready"},
		"join token without EK certificate": {parameters: parameters{running: true, labels: map[string]string{labelSpireAttestationMethod: "joinToken"}}, want: "Ready"},
		"DSX IMDS without EK certificate":   {parameters: parameters{running: true, labels: map[string]string{labelSpireAttestationMethod: "dsxImds"}}, want: "Ready"},
		"unlabeled container uses TPM":      {parameters: parameters{running: true}, want: "Pending"},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := instanceStatusForContainer(tt.parameters.running, tt.parameters.tPMEKCertificate, tt.parameters.labels)
			if string(got) != tt.want {
				t.Errorf("instanceStatusForContainer() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestUnit_DecodeCloudConfigContent(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		parameters cloudConfigWriteFile
		want       string
		wantErr    string
	}{
		"plain content": {
			parameters: cloudConfigWriteFile{Content: "plain text"},
			want:       "plain text",
		},
		"base64 alias": {
			parameters: cloudConfigWriteFile{Encoding: "b64", Content: "cGVtIGNlcnQ="},
			want:       "pem cert",
		},
		"base64 encoding": {
			parameters: cloudConfigWriteFile{Encoding: "base64", Content: "cGVtIGNlcnQ="},
			want:       "pem cert",
		},
		"invalid base64": {
			parameters: cloudConfigWriteFile{Encoding: "b64", Content: "%%%"},
			wantErr:    "decode base64 content",
		},
		"unsupported encoding": {
			parameters: cloudConfigWriteFile{Encoding: "gzip", Content: "content"},
			wantErr:    `unsupported cloud-config encoding "gzip"`,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := decodeCloudConfigContent(tt.parameters)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("decodeCloudConfigContent() error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("decodeCloudConfigContent() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("decodeCloudConfigContent() = %q, want %q", got, tt.want)
			}
		})
	}
}
