// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package etcd

import (
	"context"

	"github.com/dsx-ai-factory/nvke/controller/internal/rollout"
	"github.com/dsx-ai-factory/nvke/controller/internal/rollout/nodeagent"
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
