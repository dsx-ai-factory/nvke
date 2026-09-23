// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubevip

import (
	"github.com/dsx-ai-factory/nke/controller/internal/rollout"
	"github.com/dsx-ai-factory/nke/controller/internal/rollout/nodeagent"
)

func RegisterDefaults(reg *rollout.Registry, dialer nodeagent.Dialer) {
	reg.RegisterNodeGuard(GuardNameKubeVipDrainRestore, func(_ map[string]string) (rollout.NodeGuard, error) {
		return NewGuard(dialer), nil
	})
}
