// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nvke

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/dsx-ai-factory/nvke/controller/test/e2e/internal/controller"
)

func TestControllerSetting(t *testing.T) {
	t.Parallel()
	data := map[string]string{controllerConfigKey: "permissiveMode: true\noidc:\n  managementOIDCAuth:\n    enabled: false\n"}

	tests := []struct {
		name    string
		data    map[string]string
		key     string
		want    any
		wantErr string
	}{
		{name: "top-level key", data: data, key: permissiveModeKey, want: true},
		{name: "nested key", data: data, key: oidcEnabledKey, want: false},
		{name: "missing key", data: data, key: "oidc.providers", wantErr: "has no oidc.providers"},
		{name: "missing parent", data: data, key: "teleport.enabled", wantErr: "has no teleport.enabled"},
		{name: "no config.yaml", data: map[string]string{}, key: permissiveModeKey, wantErr: "has no config.yaml"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ControllerSetting(tt.data, tt.key)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// Where Reloader runs, it updates the Deployment after the ConfigMap edit, so the
// restart's own update can conflict. The restart retries it on a fresh read.
func TestRestartControllerRetriesAConflict(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	replicas := int32(1)
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: controller.Namespace(), Name: controller.Deployment()},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
		Status:     appsv1.DeploymentStatus{Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1},
	}

	updates := 0
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(deploy).WithInterceptorFuncs(interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			updates++
			if updates == 1 {
				return apierrors.NewConflict(schema.GroupResource{Group: "apps", Resource: "deployments"}, obj.GetName(), nil)
			}
			return c.Update(ctx, obj, opts...)
		},
	}).Build()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	require.NoError(t, restartController(ctx, c))
	assert.Equal(t, 2, updates)

	var got appsv1.Deployment
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(deploy), &got))
	assert.NotEmpty(t, got.Spec.Template.Annotations[restartedAtAnnotation])
}

// A restart can fail after the ConfigMap write, and the caller must still put the
// old value back, so the write is reported whenever it happened.
func TestSetControllerSettingReportsTheWrite(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	settings := func() *corev1.ConfigMap {
		return &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: controller.Namespace(), Name: ControllerConfigMap},
			Data:       map[string]string{controllerConfigKey: "permissiveMode: true\n"},
		}
	}
	replicas := int32(1)
	deployment := func() *appsv1.Deployment {
		return &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Namespace: controller.Namespace(), Name: controller.Deployment()},
			Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
			Status:     appsv1.DeploymentStatus{Replicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1},
		}
	}

	tests := []struct {
		name         string
		objects      []client.Object
		wantPrevious any
		wantWritten  bool
		wantErr      string
		wantStored   string
	}{
		{name: "no ConfigMap, nothing written", wantErr: "getting ConfigMap"},
		{
			name:         "restart fails after the write",
			objects:      []client.Object{settings()},
			wantPrevious: true,
			wantWritten:  true,
			wantErr:      "getting NVKE deployment",
			wantStored:   "permissiveMode: false\n",
		},
		{
			name:         "written and restarted",
			objects:      []client.Object{settings(), deployment()},
			wantPrevious: true,
			wantWritten:  true,
			wantStored:   "permissiveMode: false\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tt.objects...).Build()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			previous, written, err := setControllerSetting(ctx, c, permissiveModeKey, false)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tt.wantPrevious, previous)
			assert.Equal(t, tt.wantWritten, written)
			if tt.wantStored != "" {
				var cm corev1.ConfigMap
				require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: controller.Namespace(), Name: ControllerConfigMap}, &cm))
				assert.Equal(t, tt.wantStored, cm.Data[controllerConfigKey])
			}
		})
	}
}
