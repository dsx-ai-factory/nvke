// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package activities

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"

	forgesdk "github.com/NVIDIA/ncx-infra-controller-rest/sdk/standard"

	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/testutil"
)

func TestWaitForForgeInstanceReadyUsesNicoNodeProvider(t *testing.T) {
	clusterID := uuid.MustParse("20000000-0000-0000-0000-000000000001")
	nodeID := uuid.MustParse("20000000-0000-0000-0000-000000000002")
	siteID := uuid.MustParse("20000000-0000-0000-0000-000000000003")
	vpcID := uuid.MustParse("20000000-0000-0000-0000-000000000004")
	node := db.Node{
		ID:              nodeID,
		ClusterID:       clusterID,
		ControlPlane:    true,
		Name:            "worker-0",
		ForgeInstanceID: "legacy-instance",
		Nico: &db.NicoNodeProvider{
			ForgeInstanceID: "payload-instance",
		},
	}
	cluster := db.Cluster{
		ID:           clusterID,
		ProviderType: db.ProviderTypeNico,
		Nico: &db.NicoClusterProvider{
			OrgID:  "org-1",
			SiteID: siteID,
			VpcID:  vpcID,
		},
	}
	mockDB := &testutil.MockDB{}
	mockDB.On("GetNode", mock.Anything, node.ID).Return(node, nil)
	mockDB.On("GetCluster", mock.Anything, cluster.ID).Return(cluster, nil)
	forgeProvider := &waitForForgeInstanceReadyForgeProvider{
		instance: &forgesdk.Instance{
			Id: forgesdk.PtrString("payload-instance"),
			Interfaces: []forgesdk.Interface{{
				IpAddresses: []string{"10.0.0.10"},
			}},
		},
	}
	activities := &Activities{db: mockDB, forgeProvider: forgeProvider}

	var result *forgesdk.Instance
	err := executeWaitForForgeInstanceReadyActivity(t, activities.waitForForgeInstanceReady, WaitForForgeInstanceReadyInput{NodeID: node.ID}, &result)

	require.NoError(t, err)
	assert.Equal(t, "payload-instance", forgeProvider.getInstanceID)
	assert.Equal(t, "payload-instance", forgeProvider.applyLabelsInstanceID)
	mockDB.AssertExpectations(t)
}

func executeWaitForForgeInstanceReadyActivity(t *testing.T, activityFn, input, result any) error {
	t.Helper()

	testSuite := &testsuite.WorkflowTestSuite{}
	env := testSuite.NewTestActivityEnvironment()
	env.RegisterActivity(activityFn)
	encoded, err := env.ExecuteActivity(activityFn, input)
	if err != nil {
		return err
	}
	return encoded.Get(result)
}

type waitForForgeInstanceReadyForgeProvider struct {
	forgeProvider
	getInstanceID         string
	applyLabelsInstanceID string
	instance              *forgesdk.Instance
}

func (p *waitForForgeInstanceReadyForgeProvider) ApplyInstanceLabels(_ context.Context, _, instanceID string, _ map[string]string) (*forgesdk.Instance, error) {
	p.applyLabelsInstanceID = instanceID
	return p.instance, nil
}

func (p *waitForForgeInstanceReadyForgeProvider) GetInstanceByID(_ context.Context, _, _, instanceID string) (*forgesdk.Instance, error) {
	p.getInstanceID = instanceID
	return p.instance, nil
}
