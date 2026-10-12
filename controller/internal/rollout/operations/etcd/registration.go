// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package etcd

import (
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nvke/controller/internal/rollout"
	"github.com/dsx-ai-factory/nvke/controller/internal/rollout/nodeagent"
)

func RegisterDefaults(reg *rollout.Registry, dialer nodeagent.Dialer, k8sClient client.Client, namespace string) {
	keyReader := NewKubernetesEncryptionKeyReader(k8sClient, namespace)
	reg.RegisterOperation(rollout.Compile(NewInitializeOperation(
		WithDialer(dialer),
		WithEncryptionKeyReader(keyReader),
		WithObjectsToEncrypt(DefaultObjectsToEncrypt()),
	)))
	reg.RegisterOperation(rollout.Compile(NewPromoteProvidersOperation(WithDialer(dialer))))
	reg.RegisterOperation(rollout.Compile(NewRemovePlainTextSecondaryOperation(WithDialer(dialer))))
	reg.RegisterOperation(rollout.Compile(NewAddSecondaryKeyOperation(
		WithDialer(dialer),
		WithEncryptionKeyReader(keyReader),
	)))
	reg.RegisterOperation(rollout.Compile(NewPromotePrimaryKeyOperation(
		WithDialer(dialer),
		WithEncryptionKeyReader(keyReader),
	)))
	reg.RegisterOperation(rollout.Compile(NewRemoveOldKeysOperation(
		WithDialer(dialer),
		WithEncryptionKeyReader(keyReader),
	)))
}
