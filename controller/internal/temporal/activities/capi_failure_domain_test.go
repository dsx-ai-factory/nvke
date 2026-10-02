// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/dsx-ai-factory/nke/controller/internal/config"
)

func TestApplyCAPIMachinePreservesExistingFailureDomain(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, clusterv1.AddToScheme(scheme))
	existing := &clusterv1.Machine{
		TypeMeta: metav1.TypeMeta{
			APIVersion: clusterv1.GroupVersion.String(),
			Kind:       "Machine",
		},
		ObjectMeta: metav1.ObjectMeta{Name: "cp-0", Namespace: "nke-system"},
		Spec: clusterv1.MachineSpec{
			ClusterName:   "cluster-a",
			FailureDomain: "fd-a",
		},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()
	activity := &Activities{
		cfg:       config.Config{FieldManagerName: "cloud-workflow-engine"},
		k8sClient: k8sClient,
	}
	desired := existing.DeepCopy()
	desired.Spec.FailureDomain = "fd-b"

	require.NoError(t, activity.applyCAPIMachine(context.Background(), desired))

	actual := &clusterv1.Machine{}
	require.NoError(t, k8sClient.Get(context.Background(), client.ObjectKeyFromObject(existing), actual))
	assert.Equal(t, "fd-a", actual.Spec.FailureDomain)
}
