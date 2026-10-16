// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package activities

import (
	"testing"

	"github.com/stretchr/testify/require"

	nkeproto "github.com/dsx-ai-factory/nke/agent/pkg/agent/proto"

	"github.com/dsx-ai-factory/nke/controller/internal/db"
)

func TestDPUHostCNIRequestAndBootstrapLabel(t *testing.T) {
	req, err := installCNIRequest(db.Cluster{CniPlugin: db.CNIOvnKubernetesDPUHost}, "")
	require.NoError(t, err)
	require.Equal(t, nkeproto.CniVariant_CNI_VARIANT_OVN, req.GetCniVariant())
	require.Equal(t, "1.5.18", req.GetVersion())
	require.Equal(t, "true", req.GetCniConfig()["nke.ovnDpuHost"])
	labels := nodeLabelsForCNI([]string{"lab=managed"}, db.CNIOvnKubernetesDPUHost)
	require.Contains(t, labels, "k8s.ovn.org/dpu-host")
	require.Equal(t, "managed", labels["lab"])
	require.NotContains(t, nodeLabelsForCNI(nil, db.CNICalico), "k8s.ovn.org/dpu-host")
}
