// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package kubebench scans a cluster with kube-bench and judges the result
// against NVKE's exception register.
//
// A scan runs one kube-bench pod per node. kube-bench's benchmark files come
// from a pinned upstream commit, fetched when the scan runs and checked by
// SHA-256, because no kube-bench release ships the benchmark yet; they are
// never checked into this repository. The reports are evaluated against
// exceptions.yaml: a FAIL or WARN that no entry covers is a violation, and an
// entry that no longer matches anything is stale. See README.md.
package kubebench
