// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package filesystem writes files the CLI hands to an operator.
package filesystem

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const privateFileMode = 0o600

// WritePrivateFile writes data to path at mode 0600. It creates a temp file in
// path's directory, then renames it over path, so an existing 0644 file is not
// left world-readable and a symlink at path is replaced rather than followed.
func WritePrivateFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".nkectl-*")
	if err != nil {
		return fmt.Errorf("creating temp file in %q: %w", dir, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if err := tmp.Chmod(privateFileMode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("setting permissions on %q: %w", tmpPath, err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing %q: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing %q: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("writing %q: %w", path, err)
	}
	return nil
}

// WriteKubeconfig writes a kubeconfig to w, or to path when path is set.
// A path is written at mode 0600, and w then receives the path that was written.
func WriteKubeconfig(w io.Writer, path string, data []byte) error {
	if path == "" {
		_, err := w.Write(data)
		return err
	}
	if err := WritePrivateFile(path, data); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "kubeconfig written to %s\n", path)
	return err
}
