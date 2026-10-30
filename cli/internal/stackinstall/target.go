// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package stackinstall

// TargetKind is the cluster an install targets.
type TargetKind string

const (
	Seed TargetKind = "seed" // the ephemeral kind cluster
	Site TargetKind = "site" // the permanent cluster
)
