// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	internalv1 "github.com/dsx-ai-factory/nvke/management-engine/apis/v1"
)

func TestSharedInfraTemplate(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := internalv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		template *corev1.ConfigMap
		want     string
	}{
		{name: "shared template read", template: &corev1.ConfigMap{
			Namespace: "operator-system", Name: "infra",
			Data: map[string]string{"clusterTmpl": "{}", "nodepoolTmpl": "{}", "controlplaneTmpl": "{}"},
		}, want: "get ClusterReleaseChannel"},
		{name: "missing", want: "operator-system/infra"},
		{name: "invalid", template: &corev1.ConfigMap{Namespace: "operator-system", Name: "infra"}, want: "must contain non-empty data.clusterTmpl"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader := fake.NewClientBuilder().WithScheme(scheme)
			if tc.template != nil {
				reader = reader.WithObjects(tc.template)
			}
			shared := reader.Build()
			for _, tenant := range []string{"tenant-a", "tenant-b"} {
				// The tenant client contains a decoy; template reads must use the separate reader.
				tenantClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.ConfigMap{
					Namespace: tenant, Name: "infra",
					Data: map[string]string{"clusterTmpl": "tenant template must not be used"},
				}).Build()
				r := &NVKEClusterReconciler{Client: tenantClient, InfraTemplateReader: shared, InfraTemplateNamespace: "operator-system", InfraTemplateConfigMapName: "infra"}
				cluster := &internalv1.NVKECluster{Namespace: tenant, Name: "cluster"}
				_, err := r.reconcileCluster(context.Background(), cluster)
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("%s: expected %q, got %v", tenant, tc.want, err)
				}
				condition := conditions.Get(cluster, internalv1.ClusterReconciledCondition)
				if condition == nil || condition.Status != metav1.ConditionFalse || condition.Message != err.Error() {
					t.Fatalf("%s: missing failure condition: %v", tenant, condition)
				}
			}
		})
	}
}
