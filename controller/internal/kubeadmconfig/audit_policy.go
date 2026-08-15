// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

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
