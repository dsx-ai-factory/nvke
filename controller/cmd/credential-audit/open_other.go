// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !unix

package main

import (
	"errors"
	"os"
)

// openRegularFile refuses inputs that are not regular files. Opening never
// blocks on these platforms, so the opened descriptor's type is checked directly.
func openRegularFile(path string) (*os.File, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("could not open public certificate input")
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, errors.New("public certificate input must be a regular file")
	}
	return file, nil
}
