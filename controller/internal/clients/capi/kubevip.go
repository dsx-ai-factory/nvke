// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	"github.com/dsx-ai-factory/nke/controller/internal/db"
)

// UsesFMDSBGPPeerASN is always false: CAPI node pools carry no VPC-prefix
// compatibility signal, so kube-vip always uses the static default BGP peer
// ASN unless the site-wide FMDS endpoint is configured (handled outside
// this client, since it applies to every provider).
func (*Client) UsesFMDSBGPPeerASN(_ db.NodePool) (bool, error) {
	return false, nil
}
