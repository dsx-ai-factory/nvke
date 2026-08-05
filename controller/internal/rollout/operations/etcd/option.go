// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package etcd

import (
	"context"

	"github.com/NVIDIA/nke/controller/internal/rollout"
	"github.com/NVIDIA/nke/controller/internal/rollout/nodeagent"
)

type Option func(*EncryptionOperation)

func WithDialer(dialer nodeagent.Dialer) Option {
	return func(op *EncryptionOperation) {
		if dialer == nil {
			op.nodeAgentClientDialer = nil
			return
		}
		op.nodeAgentClientDialer = func(ctx context.Context, node rollout.Node) (nodeAgentEncryptionClient, func(), error) {
			return dialer(ctx, node)
		}
	}
}

func WithEncryptionKeyReader(reader EncryptionKeyReader) Option {
	return func(op *EncryptionOperation) {
		op.encryptionKeyReader = reader
	}
}

func WithObjectsToEncrypt(objects []string) Option {
	return func(op *EncryptionOperation) {
		op.objectsToEncrypt = append([]string(nil), objects...)
	}
}
