// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kubeadmconfig

import (
	_ "embed"
)

//go:embed audit_policy.yaml
var apiServerAuditPolicyContent string

func apiServerAuditPolicyFile() File {
	return File{
		Path:    defaultAuditPolicyFile,
		Content: []byte(apiServerAuditPolicyContent),
		Mode:    0o644,
		Owner:   rootRoot,
	}
}
