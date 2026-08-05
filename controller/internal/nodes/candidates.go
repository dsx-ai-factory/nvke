// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package nodes

import (
	"github.com/NVIDIA/nke/controller/internal/db"
)

// PickControlPlaneRunbookCandidates returns the control plane nodes eligible for
// cluster-level operations along with whether the returned list contains healthy
// candidates. Healthy joined nodes (KubernetesReady=True and Deleting!=True)
// are preferred so that an unreachable or torn-down node is never selected when
// a healthy peer exists. If no joined node currently looks healthy, every joined
// node is returned and the second return value is false so the caller can log
// the degraded state.
func PickControlPlaneRunbookCandidates(controlPlaneNodes []db.Node) ([]db.Node, bool) {
	joined := make([]db.Node, 0, len(controlPlaneNodes))
	var healthy []db.Node
	for _, node := range controlPlaneNodes {
		if node.Conditions.Joined.Status != db.ConditionStatusTrue {
			continue
		}
		joined = append(joined, node)
		if node.Conditions.KubernetesReady.Status == db.ConditionStatusTrue &&
			node.Conditions.Deleting.Status != db.ConditionStatusTrue {
			healthy = append(healthy, node)
		}
	}
	if len(healthy) > 0 {
		return healthy, true
	}
	return joined, false
}
