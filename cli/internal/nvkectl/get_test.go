// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nvkectl

import (
	"os"
	"path/filepath"
	"testing"
)

const validKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: mgmt
  cluster:
    server: https://127.0.0.1:6443
    insecure-skip-tls-verify: true
contexts:
- name: mgmt
  context:
    cluster: mgmt
    user: mgmt
current-context: mgmt
users:
- name: mgmt
  user:
    token: fake-token
`

func TestNewMgmtClientBuildsFromValidKubeconfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(validKubeconfig), 0o600); err != nil {
		t.Fatalf("writing test kubeconfig: %v", err)
	}

	fetcher, err := newMgmtClient(path)
	if err != nil {
		t.Fatalf("newMgmtClient: %v", err)
	}
	if fetcher == nil {
		t.Fatal("newMgmtClient returned a nil KubeconfigFetcher")
	}
}
