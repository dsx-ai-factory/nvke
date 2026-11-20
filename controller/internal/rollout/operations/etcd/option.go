// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package etcd

import (
	"context"
	"time"

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

// DefragOption configures the defragmentation operation. It is a separate type
// from Option because the two operations share a package but not a struct.
type DefragOption func(*DefragOperation)

func WithDefragDialer(dialer nodeagent.Dialer) DefragOption {
	return func(op *DefragOperation) {
		if dialer == nil {
			op.nodeAgentClientDialer = nil
			return
		}
		op.nodeAgentClientDialer = func(ctx context.Context, node rollout.Node) (nodeAgentDefragClient, func(), error) {
			return dialer(ctx, node)
		}
	}
}

// WithDefragPollInterval overrides how often GetEtcdDefragStatus is polled.
func WithDefragPollInterval(interval time.Duration) DefragOption {
	return func(op *DefragOperation) {
		if interval > 0 {
			op.pollInterval = interval
		}
	}
}

// WithDefragTimeout overrides how long one node may take.
func WithDefragTimeout(timeout time.Duration) DefragOption {
	return func(op *DefragOperation) {
		if timeout > 0 {
			op.timeout = timeout
		}
	}
}
