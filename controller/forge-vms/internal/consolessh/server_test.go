// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package consolessh

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestServerStreamsConsoleLog(t *testing.T) {
	// Create a fake instance directory with a console log.
	instancesDir := t.TempDir()
	instanceID := "test-instance-001"
	instanceDir := filepath.Join(instancesDir, instanceID)
	if err := os.MkdirAll(instanceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	wantContent := "Hello from the VM console!\nBoot complete.\n"
	if err := os.WriteFile(filepath.Join(instanceDir, "console.log"), []byte(wantContent), 0o644); err != nil {
		t.Fatal(err)
	}

	// Start the SSH server.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := New(instancesDir, slog.Default())
	port, err := srv.Start(ctx, ":0")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Connect as SSH client with the instance ID as username.
	cfg := &ssh.ClientConfig{
		User:            instanceID,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec
	}
	client, err := ssh.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port), cfg)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer session.Close()

	output, err := session.CombinedOutput("dump")
	if err != nil {
		t.Fatalf("CombinedOutput: %v", err)
	}

	if string(output) != wantContent {
		t.Errorf("got %q, want %q", string(output), wantContent)
	}
}

func TestServerMissingInstance(t *testing.T) {
	instancesDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := New(instancesDir, slog.Default())
	port, err := srv.Start(ctx, ":0")
	if err != nil {
		t.Fatal(err)
	}

	cfg := &ssh.ClientConfig{
		User:            "nonexistent-instance",
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec
	}
	client, err := ssh.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	output, err := session.CombinedOutput("dump")
	if err != nil {
		t.Fatalf("CombinedOutput: %v", err)
	}

	if got := string(output); got == "" {
		t.Error("expected error message for missing instance, got empty output")
	}
}
