// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

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

func TestForgeRebootNodeUsesNicoNodeProvider(t *testing.T) {
	type parameters struct {
		mutateNode      func(*db.Node)
		wantInstanceID  string
		wantErrContains string
	}

	tests := map[string]parameters{
		"uses nico node provider": {
			wantInstanceID: "payload-instance",
		},
		"empty nico forge instance ID does not call forge": {
			mutateNode: func(node *db.Node) {
				node.Nico = &db.NicoNodeProvider{}
			},
			wantErrContains: "has no Forge instance ID",
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			clusterID := uuid.MustParse("30000000-0000-0000-0000-000000000001")
			nodeID := uuid.MustParse("30000000-0000-0000-0000-000000000002")
			nodePoolID := uuid.MustParse("30000000-0000-0000-0000-000000000003")
			nodeSetID := uuid.MustParse("30000000-0000-0000-0000-000000000004")
			siteID := uuid.MustParse("30000000-0000-0000-0000-000000000005")
			vpcID := uuid.MustParse("30000000-0000-0000-0000-000000000006")
			node := db.Node{
				ID:              nodeID,
				ClusterID:       clusterID,
				NodePoolID:      nodePoolID,
				NodeSetID:       nodeSetID,
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
			if params.mutateNode != nil {
				params.mutateNode(&node)
			}
			mockDB := &testutil.MockDB{}
			mockDB.On("GetNode", mock.Anything, node.ID).Return(node, nil)
			mockDB.On("GetCluster", mock.Anything, cluster.ID).Return(cluster, nil)
			forgeProvider := &forgeRebootNodeForgeProvider{instance: &forgesdk.Instance{Name: forgesdk.PtrString("node-1")}}
			activities := &Activities{db: mockDB, forgeProvider: forgeProvider}

			var result ForgeRebootNodeResult
			err := executeForgeRebootNodeActivity(t, activities.forgeRebootNode, ForgeRebootNodeInput{NodeID: node.ID}, &result)

			if params.wantErrContains != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), params.wantErrContains)
				assert.False(t, forgeProvider.rebootCalled)
				return
			}
			require.NoError(t, err)
			assert.True(t, forgeProvider.rebootCalled)
			assert.Equal(t, params.wantInstanceID, forgeProvider.rebootInstanceID)
		})
	}
}

func executeForgeRebootNodeActivity(t *testing.T, activityFn, input, result any) error {
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

type forgeRebootNodeForgeProvider struct {
	forgeProvider
	rebootCalled     bool
	rebootInstanceID string
	instance         *forgesdk.Instance
}

func (p *forgeRebootNodeForgeProvider) TriggerInstanceReboot(_ context.Context, _, instanceID string) (*forgesdk.Instance, error) {
	p.rebootCalled = true
	p.rebootInstanceID = instanceID
	return p.instance, nil
}
