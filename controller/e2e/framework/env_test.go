// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package framework

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGetenvWithLegacy(t *testing.T) {
	const key, previousKey, cweKey = "NVKE_CONTROLLER_GOFAIL_E2E_TEST_NEW", "NKE_CONTROLLER_GOFAIL_E2E_TEST_OLD", "CWE_GOFAIL_E2E_TEST_OLD"
	tests := []struct {
		name string
		env  map[string]string // names left out are unset
		want string
	}{
		{name: "new name only", env: map[string]string{key: "new"}, want: "new"},
		{name: "NKE_CONTROLLER name only", env: map[string]string{previousKey: "nke"}, want: "nke"},
		{name: "CWE name only", env: map[string]string{cweKey: "cwe"}, want: "cwe"},
		{name: "new name wins over both deprecated names", env: map[string]string{key: "new", previousKey: "nke", cweKey: "cwe"}, want: "new"},
		{name: "NKE_CONTROLLER name wins over CWE name", env: map[string]string{previousKey: "nke", cweKey: "cwe"}, want: "nke"},
		{name: "empty new name does not hide deprecated names", env: map[string]string{key: "", previousKey: "nke", cweKey: "cwe"}, want: "nke"},
		{name: "empty new and NKE_CONTROLLER names do not hide CWE name", env: map[string]string{key: "", previousKey: "", cweKey: "cwe"}, want: "cwe"},
		{name: "no names", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, name := range []string{key, previousKey, cweKey} {
				t.Setenv(name, "") // restores the caller's value afterwards
				require.NoError(t, os.Unsetenv(name))
			}
			for name, value := range tt.env {
				t.Setenv(name, value)
			}

			require.Equal(t, tt.want, getenvWithLegacy(key, previousKey, cweKey))
		})
	}
}
