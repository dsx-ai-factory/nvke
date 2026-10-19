// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package envalias

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGetPrefersTheNewNameAndFallsBackToTheOld(t *testing.T) {
	t.Setenv("NVKECTL_TEST_SETTING", "")
	t.Setenv("NKECTL_TEST_SETTING", "old")
	assert.Equal(t, "old", Get("NVKECTL_TEST_SETTING"), "the previous name is read when the new one is unset")

	t.Setenv("NVKECTL_TEST_SETTING", "new")
	assert.Equal(t, "new", Get("NVKECTL_TEST_SETTING"), "the new name wins when both are set")

	t.Setenv("OTHER_SETTING", "x")
	assert.Equal(t, "x", Get("OTHER_SETTING"), "a name without the prefix is read as is")
	assert.Equal(t, "", Old("OTHER_SETTING"))
}

func TestGetReadsAnNVKEPrefixedSettingOnlyUnderItsOwnName(t *testing.T) {
	t.Setenv("NVKECTL_NVKE_TEST_SETTING", "")
	t.Setenv("NKECTL_NVKE_TEST_SETTING", "never a name")
	assert.Equal(t, "", Old("NVKECTL_NVKE_TEST_SETTING"))
	assert.Equal(t, "", Get("NVKECTL_NVKE_TEST_SETTING"))
	assert.Empty(t, Deprecated("NVKECTL_NVKE_TEST_SETTING"))
}

func TestDeprecatedReportsOnlySettingsSetUnderTheOldName(t *testing.T) {
	t.Setenv("NVKECTL_A", "")
	t.Setenv("NKECTL_A", "set")
	t.Setenv("NVKECTL_B", "new")
	t.Setenv("NKECTL_B", "old")
	t.Setenv("NVKECTL_C", "")
	t.Setenv("NKECTL_C", "")

	got := Deprecated("NVKECTL_C", "NVKECTL_B", "NVKECTL_A", "UNPREFIXED")
	assert.Equal(t, []Renamed{{Old: "NKECTL_A", New: "NVKECTL_A"}}, got)
	assert.Equal(t, "NKECTL_A is deprecated and will be removed in a future release; set NVKECTL_A instead", got[0].Warning())
}
