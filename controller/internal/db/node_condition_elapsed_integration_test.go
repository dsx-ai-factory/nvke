// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

func TestListNodeConditionElapsed(t *testing.T) {
	ctx := context.Background()
	bunDB := newMigratedPostgres(ctx, t)
	f := &cpMigrationFixture{t: t, ctx: ctx, bunDB: bunDB, dbi: db.NewDB(bunDB)}

	cluster := f.insertCluster("elapsed", false)
	workerPool, workerSet := f.insertPool(cluster.ID, false, false)

	hourAgo := time.Now().Add(-time.Hour)
	cond := func(status bool) db.Condition {
		return db.Condition{Status: db.ConditionStatusFromBool(status), LastTransitionTime: hourAgo}
	}
	// provisionedHealthy is a joined node with nothing wrong; cases override from here.
	provisionedHealthy := func() db.NodeConditions {
		return db.NodeConditions{
			Provisioned:     cond(true),
			Deleting:        cond(false),
			NodeAgentReady:  cond(true),
			KubernetesReady: cond(true),
			Rebooting:       cond(false),
		}
	}

	tests := []struct {
		name         string
		controlPlane bool
		conditions   func() db.NodeConditions
		want         []string
	}{
		{
			name: "provisioned node with unready agent reports nodeAgentReady",
			conditions: func() db.NodeConditions {
				c := provisionedHealthy()
				c.NodeAgentReady = cond(false)
				return c
			},
			want: []string{"nodeAgentReady"},
		},
		{
			name: "provisioned node with unknown agent status reports nodeAgentReady",
			conditions: func() db.NodeConditions {
				c := provisionedHealthy()
				c.NodeAgentReady = db.Condition{Status: db.ConditionStatusUnknown, LastTransitionTime: hourAgo}
				return c
			},
			want: []string{"nodeAgentReady"},
		},
		{
			name: "provisioned node with unready kubelet reports kubernetesReady",
			conditions: func() db.NodeConditions {
				c := provisionedHealthy()
				c.KubernetesReady = cond(false)
				return c
			},
			want: []string{"kubernetesReady"},
		},
		{
			name: "provisioned node with agent and kubelet unready reports both",
			conditions: func() db.NodeConditions {
				c := provisionedHealthy()
				c.NodeAgentReady = cond(false)
				c.KubernetesReady = cond(false)
				return c
			},
			want: []string{"nodeAgentReady", "kubernetesReady"},
		},
		{
			name: "pending node is not reported",
			conditions: func() db.NodeConditions {
				return db.NodeConditions{
					Pending:         cond(true),
					Provisioned:     cond(false),
					NodeAgentReady:  cond(false),
					KubernetesReady: cond(false),
				}
			},
		},
		{
			name: "provisioning node is not reported",
			conditions: func() db.NodeConditions {
				return db.NodeConditions{
					Provisioning:    cond(true),
					Provisioned:     cond(false),
					NodeAgentReady:  cond(false),
					KubernetesReady: cond(false),
				}
			},
		},
		{
			name: "deleting node with unready agent is not reported",
			conditions: func() db.NodeConditions {
				c := provisionedHealthy()
				c.Deleting = cond(true)
				c.NodeAgentReady = cond(false)
				return c
			},
		},
		{
			name: "rebooting node reports rebooting",
			conditions: func() db.NodeConditions {
				c := provisionedHealthy()
				c.Rebooting = cond(true)
				return c
			},
			want: []string{"rebooting"},
		},
		{
			name: "deleting node that is rebooting reports rebooting",
			conditions: func() db.NodeConditions {
				c := provisionedHealthy()
				c.Deleting = cond(true)
				c.Rebooting = cond(true)
				return c
			},
			want: []string{"rebooting"},
		},
		{
			name:       "recovered healthy node is not reported",
			conditions: provisionedHealthy,
		},
		{
			name:         "control-plane node without pool reports nodeAgentReady",
			controlPlane: true,
			conditions: func() db.NodeConditions {
				c := provisionedHealthy()
				c.NodeAgentReady = cond(false)
				return c
			},
			want: []string{"nodeAgentReady"},
		},
	}

	nodeNames := make([]string, len(tests))
	for i, tc := range tests {
		node := db.Node{
			ID:                     uuid.New(),
			ClusterID:              cluster.ID,
			ControlPlane:           tc.controlPlane,
			Name:                   "elapsed-" + uuid.NewString()[:8],
			IP:                     "10.0.0.1",
			SpireAttestationMethod: db.SpireAttestationMethodTPM,
			Conditions:             tc.conditions(),
		}
		if !tc.controlPlane {
			node.NodePoolID = workerPool.ID
			node.NodeSetID = workerSet.ID
		}
		_, err := f.dbi.CreateNode(ctx, &node)
		require.NoError(t, err)
		nodeNames[i] = node.Name
	}

	rows, err := f.dbi.ListNodeConditionElapsed(ctx)
	require.NoError(t, err)

	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			wantType := "worker"
			if tc.controlPlane {
				wantType = "cp"
			}
			var got []string
			for _, r := range rows {
				if r.NodeName != nodeNames[i] {
					continue
				}
				got = append(got, r.Condition)
				assert.Equal(t, cluster.Name, r.ClusterName)
				assert.Equal(t, wantType, r.NodeType)
				assert.InDelta(t, time.Hour.Seconds(), r.ElapsedSecs, 60)
			}
			assert.ElementsMatch(t, tc.want, got)
		})
	}
}
