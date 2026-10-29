// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bootstraptest

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

func readyDeployment(ns, name string) *appsv1.Deployment {
	one := int32(1)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Generation: 2},
		Spec:       appsv1.DeploymentSpec{Replicas: &one},
		Status:     appsv1.DeploymentStatus{ObservedGeneration: 2, Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1},
	}
}

func pod(ns, name string, phase corev1.PodPhase, ready bool) *corev1.Pod {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Status: corev1.PodStatus{
			Phase:      phase,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: status}},
		},
	}
}

func defaultStorageClass() *storagev1.StorageClass {
	return &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{
		Name: "local-path", Annotations: map[string]string{defaultStorageClassAnnotation: "true"},
	}}
}

func TestVerifySiteStack(t *testing.T) {
	unavailable := readyDeployment("postgres", "postgres")
	unavailable.Status.AvailableReplicas = 0
	stale := readyDeployment("postgres", "postgres")
	stale.Status.ObservedGeneration = 1

	tests := []struct {
		name    string
		objects []runtime.Object
		want    []string
	}{
		{
			name: "every workload rolled out and every pod Ready",
			objects: []runtime.Object{
				readyDeployment("postgres", "postgres"), pod("postgres", "postgres-0", corev1.PodRunning, true),
				pod("postgres", "schema-job", corev1.PodSucceeded, false), defaultStorageClass(),
			},
		},
		{
			name:    "a deployment without its replica available",
			objects: []runtime.Object{unavailable, defaultStorageClass()},
			want:    []string{"deployment postgres/postgres is not rolled out"},
		},
		{
			name:    "a deployment the controller has not observed yet",
			objects: []runtime.Object{stale, defaultStorageClass()},
			want:    []string{"deployment postgres/postgres is not rolled out"},
		},
		{
			name: "a running pod that is not Ready",
			objects: []runtime.Object{
				readyDeployment("postgres", "postgres"), pod("postgres", "postgres-0", corev1.PodRunning, false), defaultStorageClass(),
			},
			want: []string{"pod postgres/postgres-0 is not Ready"},
		},
		{
			name:    "an empty namespace",
			objects: []runtime.Object{defaultStorageClass()},
			want:    []string{"postgres has no workloads"},
		},
		{
			name:    "no default StorageClass",
			objects: []runtime.Object{readyDeployment("postgres", "postgres")},
			want:    []string{"no default StorageClass"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := VerifySiteStack(context.Background(), fake.NewClientset(tt.objects...), []string{"postgres"})
			if len(tt.want) == 0 {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			for _, want := range tt.want {
				assert.Contains(t, err.Error(), want)
			}
		})
	}
}

func TestSiteStackNamespacesFromEnv(t *testing.T) {
	t.Setenv(EnvSiteStackNamespaces, "")
	assert.Nil(t, SiteStackNamespacesFromEnv(false))

	t.Setenv(EnvSiteStackNamespaces, "postgres, spire-server,,capnico-system")
	assert.Equal(t, []string{"postgres", "spire-server", "capnico-system"}, SiteStackNamespacesFromEnv(false))
	assert.Equal(t, []string{"postgres", "capnico-system"}, SiteStackNamespacesFromEnv(true), "no SPIRE without mTLS")
}
