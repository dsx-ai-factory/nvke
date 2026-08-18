// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package auth_test

// Token builders used only by the middleware tests: each returns a minted token
// that is wrong in exactly one way. They live here rather than in
// synthetic_token_test.go so the verification layer compiles without them.

import (
	"testing"
	"time"
)

func spoilNamespace(_ fixture, spec *TokenSpec) {
	spec.Subject = "system:serviceaccount:nke-outsiders:outsider"
	spec.Extra = ServiceAccountClaims("nke-outsiders", "outsider")
}

func mintSpoiled(t *testing.T, f fixture, spoil func(fixture, *TokenSpec)) string {
	t.Helper()
	spec := validKubernetesToken(f, time.Now())
	spoil(f, &spec)
	return MintToken(t, spec)
}

// The tokens the acceptance table is built from.
func admittedToken(t *testing.T, f fixture) string {
	t.Helper()
	return MintToken(t, validKubernetesToken(f, time.Now()))
}

func wrongAudienceToken(t *testing.T, f fixture) string {
	t.Helper()
	return mintSpoiled(t, f, spoilAudience)
}

func unadmittedToken(t *testing.T, f fixture) string {
	t.Helper()
	return mintSpoiled(t, f, spoilNamespace)
}

func expiredToken(t *testing.T, f fixture) string {
	t.Helper()
	return mintSpoiled(t, f, spoilExpiry)
}

func forgedToken(t *testing.T, f fixture) string {
	t.Helper()
	return mintSpoiled(t, f, spoilSignature)
}

func unknownIssuerToken(t *testing.T, f fixture) string {
	t.Helper()
	return mintSpoiled(t, f, spoilIssuer)
}
