// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package framework

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGetenvWithLegacy(t *testing.T) {
	const key, legacyKey = "NKE_CONTROLLER_GOFAIL_E2E_TEST_NEW", "CWE_GOFAIL_E2E_TEST_OLD"
	tests := []struct {
		name string
		env  map[string]string // names left out are unset
		want string
	}{
		{name: "new name only", env: map[string]string{key: "new"}, want: "new"},
		{name: "deprecated name only", env: map[string]string{legacyKey: "old"}, want: "old"},
		{name: "new name wins over deprecated name", env: map[string]string{key: "new", legacyKey: "old"}, want: "new"},
		{name: "empty new name does not hide deprecated name", env: map[string]string{key: "", legacyKey: "old"}, want: "old"},
		{name: "neither name", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, name := range []string{key, legacyKey} {
				t.Setenv(name, "") // restores the caller's value afterwards
				require.NoError(t, os.Unsetenv(name))
			}
			for name, value := range tt.env {
				t.Setenv(name, value)
			}

			require.Equal(t, tt.want, getenvWithLegacy(key, legacyKey))
		})
	}
}
