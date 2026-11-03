// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package spiffe

import (
	"fmt"

	"github.com/google/uuid"
)

// NodeAgentID returns the SPIFFE ID a node's agent runs as: the DSX exchange
// path when the node uses TLS-JWT, and the legacy host path otherwise.
func NodeAgentID(trustDomain, subjectTenant, subjectService string, nodeID uuid.UUID, tlsJWT bool) string {
	if tlsJWT {
		return fmt.Sprintf("spiffe://%s/tenant/%s/service/%s/bm/%s", trustDomain, subjectTenant, subjectService, nodeID.String())
	}
	return fmt.Sprintf("spiffe://%s/host/%s", trustDomain, nodeID.String())
}
