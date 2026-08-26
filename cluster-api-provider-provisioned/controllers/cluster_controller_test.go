// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	ctrl "sigs.k8s.io/controller-runtime"
)

func TestClusterReconcilerReturnsSuccess(t *testing.T) {
	reconciler := &ClusterReconciler{}
	result, err := reconciler.Reconcile(context.Background(), ctrl.Request{})
	require.NoError(t, err)
	require.Equal(t, ctrl.Result{}, result)
}
