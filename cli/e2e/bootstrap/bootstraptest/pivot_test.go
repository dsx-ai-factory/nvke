// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bootstraptest

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	fakediscovery "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
)

var capiClusterGVR = schema.GroupVersionResource{Group: capiGroup, Version: "v1beta2", Resource: "clusters"}

// pivotSide builds one side of the pivot: its controller Deployment at replicas,
// and the Cluster API Cluster "site" when holdsCluster.
func pivotSide(replicas int32, holdsCluster bool) PivotCluster {
	kube := fake.NewClientset(&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: controllerNamespace, Name: controllerName},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
	})
	kube.Discovery().(*fakediscovery.FakeDiscovery).Resources = []*metav1.APIResourceList{
		{GroupVersion: capiClusterGVR.GroupVersion().String(), APIResources: []metav1.APIResource{{Name: "clusters", Namespaced: true, Kind: "Cluster"}}},
	}
	var objects []runtime.Object
	if holdsCluster {
		cluster := &unstructured.Unstructured{}
		cluster.SetAPIVersion(capiClusterGVR.GroupVersion().String())
		cluster.SetKind("Cluster")
		cluster.SetNamespace(managementNamespace)
		cluster.SetName("site")
		objects = append(objects, cluster)
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{capiClusterGVR: "ClusterList"}, objects...)
	return PivotCluster{Kube: kube, Dynamic: dyn}
}

func TestVerifyPivot(t *testing.T) {
	tests := []struct {
		name       string
		seed, site PivotCluster
		want       []string
	}{
		{name: "the site holds the cluster and the seed controller is stopped", seed: pivotSide(0, false), site: pivotSide(1, true)},
		{
			name: "the Cluster API objects were not moved", seed: pivotSide(0, true), site: pivotSide(1, false),
			want: []string{"the site does not hold", "the seed still holds"},
		},
		{
			name: "the seed controller still runs", seed: pivotSide(1, false), site: pivotSide(1, true),
			want: []string{"the seed controller nvke-controller/nvke-controller is not scaled to zero"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := VerifyPivot(context.Background(), tt.seed, tt.site, "site")
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
