// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tenant

import (
	"context"
	"errors"
	"path/filepath"
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
		require.Equal(t, "nke-tenant-controller-manager/"+clusterID.String(), config.UserAgent)
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

// Start parses the Secret with kube.NewRESTConfigFromKubeconfig, so it rejects
// an exec plugin other than the SPIRE command before the probe would run it.
func TestManagerFactoryDoesNotRunExecPluginFromSecret(t *testing.T) {
	t.Parallel()

	marker := filepath.Join(t.TempDir(), "ran")
	kubeconfig := []byte(`apiVersion: v1
kind: Config
clusters:
- name: tenant
  cluster:
    server: https://127.0.0.1:1
contexts:
- name: tenant
  context:
    cluster: tenant
    user: tenant
current-context: tenant
users:
- name: tenant
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1
      command: /bin/sh
      args: ["-c", "touch ` + marker + `"]
      interactiveMode: Never
`)

	_, err := NewManagerFactory(nil).Start(context.Background(), Target{
		ClusterID:   uuid.New(),
		ClusterName: "tenant",
		Kubeconfig:  kubeconfig,
	})
	require.Error(t, err)
	require.NoFileExists(t, marker)
}

var testKubeconfig = []byte(`apiVersion: v1
kind: Config
clusters:
- name: tenant
  cluster:
    server: https://127.0.0.1:6443
    certificate-authority-data: dGVzdC1jYQ==
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
