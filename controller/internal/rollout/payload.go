// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package rollout

// ApplyToNodePayload is an opaque payload built once before the per-node
// loop (by an ApplyToNodePayloadBuilder) and delivered byte-identical to
// every ApplyToNode and Rollback call. Nil when the operation has no
// ApplyToNodePayloadBuilder.
type ApplyToNodePayload []byte
