// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"testing"

	"github.com/stretchr/testify/require"

	nvkeproto "github.com/dsx-ai-factory/nvke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

func TestDPUHostCNIRequestAndBootstrapLabel(t *testing.T) {
	req, err := installCNIRequest(db.Cluster{CniPlugin: db.CNIOvnKubernetesDPUHost}, "")
	require.NoError(t, err)
	require.Equal(t, nvkeproto.CniVariant_CNI_VARIANT_OVN, req.GetCniVariant())
	require.Equal(t, "1.4.0", req.GetVersion())
	require.Equal(t, "true", req.GetCniConfig()["nvke.ovnDpuHost"])
	labels := nodeLabelsForCNI(map[string]string{"nvke-nais-dpu-sim": "managed"}, db.CNIOvnKubernetesDPUHost)
	require.Contains(t, labels, "k8s.ovn.org/dpu-host")
	require.Contains(t, labels, "nvke-nais-dpu-sim=managed")
	require.NotContains(t, nodeLabelsForCNI(nil, db.CNICalico), "k8s.ovn.org/dpu-host")
}
