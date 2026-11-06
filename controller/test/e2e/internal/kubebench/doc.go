// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package kubebench scans a cluster with kube-bench and judges the result
// against NVKE's exception register.
//
// A scan runs one kube-bench pod per node, with the benchmark that ships in the
// pinned kube-bench image. The reports are evaluated against exceptions.yaml:
// a FAIL or WARN that no entry lists is a violation, and an entry for a check
// kube-bench did not report is stale. See README.md.
package kubebench
