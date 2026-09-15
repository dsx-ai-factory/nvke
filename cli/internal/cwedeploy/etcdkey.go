// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cwedeploy

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/NVIDIA/nke/cli/internal/etcdkey"
)

// EtcdEncryptionKey reads the etcd encryption key CWE holds for clusterID and
// returns the Secret's data, decoded.
//
// The command's output is not placed in any error, unlike the other kubectl
// reads in this package: for this Secret the output is the key.
func (e *Engine) EtcdEncryptionKey(
	ctx context.Context,
	kubeconfigPath string,
	clusterID uuid.UUID,
) (map[string][]byte, error) {
	name := etcdkey.SecretName(clusterID)
	out, err := e.Runner.Run(ctx, "kubectl", "--kubeconfig", kubeconfigPath,
		"-n", cweNamespace, "get", "secret", name, "-o", "json")
	if err != nil {
		return nil, fmt.Errorf("cwedeploy: reading %s/%s: %w", cweNamespace, name, err)
	}

	var secret struct {
		Data map[string][]byte `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &secret); err != nil {
		return nil, fmt.Errorf("cwedeploy: %s/%s is not a Secret: %w", cweNamespace, name, err)
	}
	if len(secret.Data[etcdkey.FieldActive]) == 0 {
		return nil, fmt.Errorf("cwedeploy: %s/%s has no %q key", cweNamespace, name, etcdkey.FieldActive)
	}
	return secret.Data, nil
}
