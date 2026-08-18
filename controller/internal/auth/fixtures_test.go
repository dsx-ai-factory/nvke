// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package auth_test

// Fixtures shared by the admission tests and everything built on top of them.
// Separate from synthetic_token_test.go so the admission layer can be reviewed
// and compiled without the token minting that only verification needs.

const (
	k8sName   = "local-kubernetes"
	k8sIssuer = "https://kubernetes.default.svc.cluster.local"

	spireName   = "spire"
	spireIssuer = "https://spire.example.com"
)

// ServiceAccountClaims returns the nested "kubernetes.io" claim object in the
// shape the apiserver emits it.
func ServiceAccountClaims(namespace, name string) map[string]any {
	return map[string]any{
		"kubernetes.io": map[string]any{
			"namespace": namespace,
			"serviceaccount": map[string]any{
				"name": name,
				"uid":  "11111111-2222-3333-4444-555555555555",
			},
			"pod": map[string]any{
				"name": name + "-0",
				"uid":  "66666666-7777-8888-9999-000000000000",
			},
		},
	}
}
