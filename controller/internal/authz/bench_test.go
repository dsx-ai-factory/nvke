// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// What one decision costs, against the real bundle under deploy/authz. It runs
// in the request path, so the number belongs beside the code rather than in the
// design's cost table alone.
func BenchmarkLocalAuthorize(b *testing.B) {
	local, err := NewLocal(bundleDir(b))
	require.NoError(b, err)

	query := Query{
		Principal:    subjectReader,
		Issuer:       "kube-apiserver",
		Action:       "getCluster",
		ResourceType: "NKE::Cluster",
		ResourceID:   someClusterID,
		Method:       http.MethodGet,
	}
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := local.Authorize(ctx, query); err != nil {
			b.Fatal(err)
		}
	}
}

// A refusal for comparison. Both cases evaluate the whole policy set — Cedar
// does not stop at the first match, since a later forbid can override — so the
// two are the same order, and neither depends on which answer comes back.
func BenchmarkLocalAuthorizeDenied(b *testing.B) {
	local, err := NewLocal(bundleDir(b))
	require.NoError(b, err)

	query := Query{
		Principal:    subjectStranger,
		Issuer:       "kube-apiserver",
		Action:       "deleteCluster",
		ResourceType: "NKE::Cluster",
		ResourceID:   someClusterID,
		Method:       http.MethodDelete,
	}
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := local.Authorize(ctx, query); err != nil {
			b.Fatal(err)
		}
	}
}
