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
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/NVIDIA/nke/controller/internal/rollout"
	"github.com/NVIDIA/nke/controller/internal/rollout/nodeagent"
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
