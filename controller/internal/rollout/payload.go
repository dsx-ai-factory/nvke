// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package rollout

// ApplyToNodePayload is an opaque payload built once before the per-node
// loop (by an ApplyToNodePayloadBuilder) and delivered byte-identical to
// every ApplyToNode and Rollback call. Nil when the operation has no
// ApplyToNodePayloadBuilder.
type ApplyToNodePayload []byte
