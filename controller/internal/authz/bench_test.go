// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"context"
	"net/http"
	"testing"
)

// What one decision costs in-process, against the real bundle under
// deploy/authz. The comparable sidecar figure is a network round trip and is
// measured out-of-band; see the design's cost table.
func BenchmarkLocalAuthorize(b *testing.B) {
	dir := "../../deploy/authz"
	local, err := NewLocal(dir)
	if err != nil {
		b.Skipf("bundle sources not loadable from %s: %v", dir, err)
	}

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
	dir := "../../deploy/authz"
	local, err := NewLocal(dir)
	if err != nil {
		b.Skipf("bundle sources not loadable from %s: %v", dir, err)
	}

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
