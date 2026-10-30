// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package container

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
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

func TestUnit_CloudConfigScript(t *testing.T) {
	type parameters struct {
		commands []string
		status   string
		output   string
		wantErr  bool
	}
	tests := map[string]parameters{
		"commands run in order":              {commands: []string{"printf first", "printf second"}, status: "done", output: "firstsecond"},
		"failure stops later commands":       {commands: []string{"echo failed >&2; exit 42", "echo should-not-run"}, status: "error", output: "failed\n", wantErr: true},
		"quotes and newlines survive":        {commands: []string{"printf '%s' \"a 'quoted' value\"\nprintf '%s' '$(not-executed)'"}, status: "done", output: "a 'quoted' value$(not-executed)"},
		"command environments stay separate": {commands: []string{"export NVKE_TEST_VALUE=first", "printf '%s' \"${NVKE_TEST_VALUE-unset}\""}, status: "done", output: "unset"},
		"empty cloud config":                 {commands: []string{"", "  "}, status: "done"},
	}
	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			status := filepath.Join(dir, "status")
			script := filepath.Join(dir, "runcmd.sh")
			require.NoError(t, os.WriteFile(status, []byte("running\n"), 0o644))
			require.NoError(t, os.WriteFile(script, []byte(cloudConfigScript(params.commands)), 0o644))
			err := exec.Command("sh", script, status).Run()
			if params.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			got, err := os.ReadFile(status)
			require.NoError(t, err)
			require.Equal(t, params.status+"\n", string(got))
			output, err := os.ReadFile(filepath.Join(dir, "output.log"))
			require.NoError(t, err)
			require.Equal(t, params.output, string(output))
		})
	}
}

func TestUnit_CloudConfigScript_BlockingGate(t *testing.T) {
	dir := t.TempDir()
	status := filepath.Join(dir, "status")
	started := filepath.Join(dir, "started")
	release := filepath.Join(dir, "release")
	script := filepath.Join(dir, "runcmd.sh")
	command := "touch '" + started + "'; while [ ! -f '" + release + "' ]; do sleep 0.01; done"
	require.NoError(t, os.WriteFile(status, []byte("running\n"), 0o644))
	require.NoError(t, os.WriteFile(script, []byte(cloudConfigScript([]string{command})), 0o644))
	cmd := exec.Command("sh", script, status)
	require.NoError(t, cmd.Start())
	completed := make(chan error, 1)
	go func() { defer close(completed); completed <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = os.WriteFile(release, nil, 0o644)
		select {
		case <-completed:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
		}
	})
	require.Eventually(t, func() bool { _, err := os.Stat(started); return err == nil }, 2*time.Second, 10*time.Millisecond)
	got, err := os.ReadFile(status)
	require.NoError(t, err)
	require.Equal(t, "running\n", string(got))
	require.NoError(t, os.WriteFile(release, nil, 0o644))
	select {
	case err := <-completed:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("cloud-init did not finish after gate release")
	}
	got, err = os.ReadFile(status)
	require.NoError(t, err)
	require.Equal(t, "done\n", string(got))
}
