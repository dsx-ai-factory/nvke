// SPDX-FileCopyrightText: Copyright (c) 2025 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

// Package sshexec provides SSH command execution on VMs.
package sshexec

import (
	"context"
	"fmt"
	"io"
	"net"
	"time"

	"golang.org/x/crypto/ssh"
)

// Config holds SSH credentials for connecting to VMs.
type Config struct {
	User     string
	Password string
}

// Exec runs a command on the remote host and returns its combined output.
func Exec(ctx context.Context, host string, cfg Config, cmd string) ([]byte, error) {
	client, err := dial(ctx, host, cfg)
	if err != nil {
		return nil, err
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("ssh session: %w", err)
	}
	defer session.Close()

	return session.CombinedOutput(cmd)
}

// ExecStream runs a command on the remote host and streams stdout to w,
// calling flush after each write. It blocks until the command exits or
// ctx is cancelled.
func ExecStream(ctx context.Context, host string, cfg Config, cmd string, w io.Writer, flush func()) error {
	client, err := dial(ctx, host, cfg)
	if err != nil {
		return err
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("ssh session: %w", err)
	}
	defer session.Close()

	stdout, err := session.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}

	if err := session.Start(cmd); err != nil {
		return fmt.Errorf("start command: %w", err)
	}

	// Stream output until EOF or context cancellation.
	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, readErr := stdout.Read(buf)
			if n > 0 {
				w.Write(buf[:n])
				flush()
			}
			if readErr != nil {
				done <- readErr
				return
			}
		}
	}()

	select {
	case <-ctx.Done():
		session.Close()
		return ctx.Err()
	case err := <-done:
		if err == io.EOF {
			return session.Wait()
		}
		return err
	}
}

func dial(ctx context.Context, host string, cfg Config) (*ssh.Client, error) {
	sshCfg := &ssh.ClientConfig{
		User: cfg.User,
		Auth: []ssh.AuthMethod{
			ssh.Password(cfg.Password),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // forge-vms dev tool
		Timeout:         10 * time.Second,
	}

	addr := net.JoinHostPort(host, "22")
	client, err := ssh.Dial("tcp", addr, sshCfg)
	if err != nil {
		return nil, fmt.Errorf("ssh dial %s: %w", addr, err)
	}
	return client, nil
}
