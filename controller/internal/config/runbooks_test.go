// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEffectiveRunbooksDir(t *testing.T) {
	t.Run("derives directory from profiles", func(t *testing.T) {
		cfg := Config{ProfilesDir: filepath.Join("custom", "profiles")}

		require.Equal(t, filepath.Join("custom", "profiles", "runbooks"), cfg.EffectiveRunbooksDir())
	})

	t.Run("uses explicit directory", func(t *testing.T) {
		cfg := Config{
			ProfilesDir: filepath.Join("custom", "profiles"),
			RunbooksDir: filepath.Join("other", "runbooks"),
		}

		require.Equal(t, filepath.Join("other", "runbooks"), cfg.EffectiveRunbooksDir())
	})
}
