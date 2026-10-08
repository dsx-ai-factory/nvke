// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"errors"
	"os"
	"syscall"
)

// openRegularFile opens path without blocking, so a FIFO without a writer cannot
// hang the command, and checks the type of the opened descriptor rather than of
// the path, which could be replaced between a check and the open.
func openRegularFile(path string) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New("could not open public certificate input")
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, errors.New("public certificate input must be a regular file")
	}
	// Reads are ordinary blocking reads from here on.
	conn, err := file.SyscallConn()
	if err == nil {
		controlErr := conn.Control(func(fd uintptr) { err = syscall.SetNonblock(int(fd), false) })
		err = errors.Join(controlErr, err)
	}
	if err != nil {
		_ = file.Close()
		return nil, errors.New("could not open public certificate input")
	}
	return file, nil
}
