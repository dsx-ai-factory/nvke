// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import "testing"

func TestUpgradeHops(t *testing.T) {
	for _, tc := range []struct {
		env  string
		want int
	}{
		{"", len(upgradeKubernetesVersionChain)},
		{"1", 1},
		{"2", len(upgradeKubernetesVersionChain)},
		{"9", len(upgradeKubernetesVersionChain)},
		{"0", len(upgradeKubernetesVersionChain)},
		{"x", len(upgradeKubernetesVersionChain)},
	} {
		t.Setenv("NKE_E2E_UPGRADE_HOPS", tc.env)
		if got := upgradeHops(); len(got) != tc.want || got[0] != upgradeKubernetesVersionChain[0] {
			t.Errorf("NKE_E2E_UPGRADE_HOPS=%q: got %v, want the first %d of %v", tc.env, got, tc.want, upgradeKubernetesVersionChain)
		}
	}
}
