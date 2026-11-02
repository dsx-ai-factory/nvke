// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package darwinqemu

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFirmwareUsesSelectedQEMU(t *testing.T) {
	dir := t.TempDir()
	data := filepath.Join(dir, "QEMU data")
	require.NoError(t, os.Mkdir(data, 0o700))
	for _, name := range []string{"code.fd", "vars.fd"} {
		require.NoError(t, os.WriteFile(filepath.Join(data, name), []byte(name), 0o444))
	}
	qemu := filepath.Join(dir, "qemu")
	require.NoError(t, os.WriteFile(qemu, []byte("#!/bin/sh\nprintf '%s\\n' '"+data+"'\n"), 0o700))
	t.Setenv("NICO_VMS_EFI_CODE", "")
	t.Setenv("NICO_VMS_EFI_VARS_TEMPLATE", "")
	code, vars, err := firmwarePair(context.Background(), qemu, "code.fd", "vars.fd")
	require.NoError(t, err)
	require.Equal(t, filepath.Join(data, "code.fd"), code)
	dst := filepath.Join(dir, "private-vars.fd")
	require.NoError(t, copyVariables(vars, dst))
	info, err := os.Stat(dst)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	require.NoError(t, os.WriteFile(dst, []byte("guest state"), 0o600))
	require.NoError(t, copyVariables(vars, dst))
	content, err := os.ReadFile(dst)
	require.NoError(t, err)
	require.Equal(t, "guest state", string(content))
}

func TestFirmwareOverrideMustBeComplete(t *testing.T) {
	t.Setenv("NICO_VMS_EFI_CODE", "/missing/code")
	t.Setenv("NICO_VMS_EFI_VARS_TEMPLATE", "")
	_, _, err := firmwarePair(context.Background(), "not-invoked", "code", "vars")
	require.ErrorContains(t, err, "set both")
}
