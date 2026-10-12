// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package launchlayer

import (
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// UsesFMDSBGPPeerASN is always false: LaunchLayer node pools carry no
// VPC-prefix compatibility signal (that's a NICo-only concept), so kube-vip
// always uses the static default BGP peer ASN — matching CAPI's default.
func (*Client) UsesFMDSBGPPeerASN(_ db.NodePool) (bool, error) {
	return false, nil
}
