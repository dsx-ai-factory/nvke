// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

func TestNVSentinelValues(t *testing.T) {
	t.Log("Testing: the embedded values cordon on a hot GPU and need no StorageClass")
	var values struct {
		GPUHealthMonitor struct {
			DCGMFieldsMonitoring struct {
				GPUTempLimitStoreOnly *bool `json:"gpuTempLimitStoreOnly"`
			} `json:"dcgmFieldsMonitoring"`
		} `json:"gpu-health-monitor"`
		MongoDBStore struct {
			PSMDB struct {
				Replsets struct {
					RS0 struct {
						VolumeSpec map[string]any `json:"volumeSpec"`
					} `json:"rs0"`
				} `json:"replsets"`
			} `json:"psmdb-db"`
		} `json:"mongodb-store"`
	}
	require.NoError(t, yaml.Unmarshal(nvsentinelValues, &values))
	require.Equal(t, new(bool), values.GPUHealthMonitor.DCGMFieldsMonitoring.GPUTempLimitStoreOnly)
	require.Equal(t, map[string]any{"emptyDir": map[string]any{}, "pvc": nil}, values.MongoDBStore.PSMDB.Replsets.RS0.VolumeSpec)
}
