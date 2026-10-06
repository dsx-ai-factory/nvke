// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nico

import (
	"github.com/dsx-ai-factory/nke/controller/internal/db"
)

// UsesFMDSBGPPeerASN reports whether nodePool has VPC prefixes configured —
// a compatibility signal for existing NICo deployments that predate
// site-wide FMDS configuration — meaning kube-vip should source its BGP
// peer ASN from FMDS instead of using the static default.
func (*Client) UsesFMDSBGPPeerASN(nodePool db.NodePool) (bool, error) {
	provider, err := nodePool.NicoProvider()
	if err != nil {
		return false, err
	}
	return len(provider.VpcPrefixIDs) > 0, nil
}
