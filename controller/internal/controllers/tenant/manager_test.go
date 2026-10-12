// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tenant

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
)

func TestManagerFactoryAppliesConservativeClientLimitsBeforeProbe(t *testing.T) {
	t.Parallel()

	clusterID := uuid.New()
	stopProbe := errors.New("stop after inspecting REST config")
	factory := NewManagerFactory(nil)
	factory.verify = func(_ context.Context, config *rest.Config, timeout time.Duration) error {
		require.InDelta(t, tenantManagerQPS, config.QPS, 0)
		require.Equal(t, tenantManagerBurst, config.Burst)
		require.Equal(t, "nvke-tenant-controller-manager/"+clusterID.String(), config.UserAgent)
		require.Equal(t, tenantManagerProbeTimeout, timeout)
		return stopProbe
	}

	_, err := factory.Start(context.Background(), Target{
		ClusterID:           clusterID,
		ClusterName:         "tenant-a",
		KubeconfigSecretRef: types.NamespacedName{Namespace: "nke-system", Name: "tenant-a-kubeconfig"},
		KubeconfigSecretUID: types.UID("uid"),
		Kubeconfig:          testKubeconfig,
	})
	require.ErrorIs(t, err, stopProbe)
}

var testKubeconfig = []byte(`apiVersion: v1
kind: Config
clusters:
- name: tenant
  cluster:
    server: https://127.0.0.1:6443
contexts:
- name: tenant
  context:
    cluster: tenant
    user: tenant
current-context: tenant
users:
- name: tenant
  user: {}
`)
