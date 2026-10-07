// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package util

import (
	"fmt"
	"io"
	"os"
)

// ReadFileOrStdin returns the contents of the file at path, or everything read
// from stdin when path is "-". what names the input in a read error, as in
// "reading spec".
func ReadFileOrStdin(stdin io.Reader, path, what string) ([]byte, error) {
	r := stdin
	if path != "-" {
		f, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("opening %q: %w", path, err)
		}
		defer f.Close()
		r = f
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", what, err)
	}
	return data, nil
}
