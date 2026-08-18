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

	"github.com/NVIDIA/nke/controller/internal/db"
	"github.com/NVIDIA/nke/controller/internal/providers/forge"
	"github.com/NVIDIA/nke/controller/internal/testutil"
)

func TestDeleteForgeInstance(t *testing.T) {
	type parameters struct {
		mutateNode           func(*db.Node)
		wantResultInstanceID string
		wantDeletedOrgID     string
		wantDeletedID        string
		wantErrContains      string
	}

	tests := map[string]parameters{
		"uses nico node provider": {
			wantResultInstanceID: "payload-instance",
			wantDeletedOrgID:     "org-1",
			wantDeletedID:        "payload-instance",
		},
		"missing nico node provider does not fallback": {
			mutateNode: func(node *db.Node) {
				node.Nico = nil
			},
			wantErrContains: db.ErrMissingProviderPayload.Error(),
		},
	}

	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			clusterID := uuid.MustParse("10000000-0000-0000-0000-000000000001")
			nodeID := uuid.MustParse("10000000-0000-0000-0000-000000000002")
			nodePoolID := uuid.MustParse("10000000-0000-0000-0000-000000000003")
			nodeSetID := uuid.MustParse("10000000-0000-0000-0000-000000000004")
			siteID := uuid.MustParse("10000000-0000-0000-0000-000000000005")
			vpcID := uuid.MustParse("10000000-0000-0000-0000-000000000006")
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
			forgeProvider := &deleteForgeInstanceForgeProvider{}
			activities := &Activities{db: mockDB, forgeProvider: forgeProvider}

			var result DeleteForgeInstanceResult
			err := executeDeleteForgeInstanceActivity(t, activities.deleteForgeInstance, DeleteForgeInstanceInput{NodeID: node.ID}, &result)

			if params.wantErrContains != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), params.wantErrContains)
				assert.Empty(t, forgeProvider.deleteInstanceID)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, params.wantResultInstanceID, result.InstanceID)
			assert.Equal(t, params.wantDeletedOrgID, forgeProvider.deleteOrgID)
			assert.Equal(t, params.wantDeletedID, forgeProvider.deleteInstanceID)
		})
	}
}

func executeDeleteForgeInstanceActivity(t *testing.T, activityFn, input, result any) error {
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

type deleteForgeInstanceForgeProvider struct {
	forgeProvider
	deleteOrgID      string
	deleteInstanceID string
}

func (p *deleteForgeInstanceForgeProvider) DeleteInstance(_ context.Context, orgID, instanceID string, _ *forge.MachineHealthIssue) error {
	p.deleteOrgID = orgID
	p.deleteInstanceID = instanceID
	return nil
}
