// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestManualEvidenceRejectsFIFOWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "public.pem")
	require.NoError(t, syscall.Mkfifo(path, 0o600))
	var output bytes.Buffer
	err := run(append(manualArgs(), "--certificate", path), &output)
	require.EqualError(t, err, "public certificate input must be a regular file")
	require.Empty(t, output.String())
}

// TestOpenRegularFileChecksTheOpenedDescriptor refuses non-regular inputs by the
// type of the descriptor it opened, and hands back a regular file in blocking mode.
func TestOpenRegularFileChecksTheOpenedDescriptor(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo.pem")
	require.NoError(t, syscall.Mkfifo(fifo, 0o600))
	regular := filepath.Join(dir, "public.pem")
	require.NoError(t, os.WriteFile(regular, []byte("public"), 0o600))
	link := filepath.Join(dir, "link.pem")
	require.NoError(t, os.Symlink(fifo, link))
	for _, test := range []struct {
		name string
		path string
	}{
		{"fifo without a writer", fifo},
		{"symlink to a fifo", link},
		{"directory", dir},
	} {
		t.Run(test.name, func(t *testing.T) {
			file, err := openRegularFile(test.path)
			require.EqualError(t, err, "public certificate input must be a regular file")
			require.Nil(t, file)
		})
	}

	file, err := openRegularFile(regular)
	require.NoError(t, err)
	defer file.Close()
	conn, err := file.SyscallConn()
	require.NoError(t, err)
	var flags uintptr
	var errno syscall.Errno
	require.NoError(t, conn.Control(func(fd uintptr) {
		flags, _, errno = syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_GETFL, 0)
	}))
	require.Zero(t, errno)
	require.Zero(t, flags&syscall.O_NONBLOCK, "the descriptor is still non-blocking")
}
