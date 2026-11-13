// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server_test

import (
	"context"
	"encoding/json"
	"os"
	"regexp"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	"github.com/dsx-ai-factory/nvke/controller/api"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// inlinePatchRe captures the single-quoted --patch document of a walkthrough's
// `patch cluster` command.
var inlinePatchRe = regexp.MustCompile(`patch cluster \S+ --patch '([^']*)'`)

// TestDocsPatchClusterExampleIsAccepted runs each walkthrough's inline
// `nvkectl patch cluster` example against a cluster freshly created from the
// example spec that same walkthrough uses, and expects the controller to accept it.
// Each example must also match the first patch in the upgrade guide, so the docs
// show one command for the same change.
func TestDocsPatchClusterExampleIsAccepted(t *testing.T) {
	upgradeBytes, err := os.ReadFile("../../../docs/workload-clusters/upgrade.md")
	require.NoError(t, err)
	upgradeMatch := inlinePatchRe.FindSubmatch(upgradeBytes)
	require.NotNil(t, upgradeMatch, "upgrade guide: no `patch cluster <name> --patch '...'` example found")

	tests := []struct {
		name string
		doc  string
		spec string
	}{
		{
			name: "first-cluster walkthrough on nico mock example",
			doc:  "../../../docs/first-cluster.md",
			spec: "../../../cli/examples/nico-mock-cluster.yaml",
		},
		{
			name: "cli user-flow walkthrough on capd example",
			doc:  "../../../cli/docs/user-flow.md",
			spec: "../../../cli/examples/capd-cluster.yaml",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			docBytes, err := os.ReadFile(tt.doc)
			require.NoError(t, err)
			m := inlinePatchRe.FindSubmatch(docBytes)
			require.NotNil(t, m, "%s: no `patch cluster <name> --patch '...'` example found", tt.doc)
			patchDoc := m[1]
			require.Equal(t, string(upgradeMatch[1]), string(patchDoc), "%s: patch example differs from the upgrade guide", tt.doc)

			specBytes, err := os.ReadFile(tt.spec)
			require.NoError(t, err)
			var spec api.CreateClusterRequest
			require.NoError(t, yaml.Unmarshal(specBytes, &spec))
			require.NotNil(t, spec.Infra, "%s: fixture below assumes a CAPI (infra) example", tt.spec)
			require.NotNil(t, spec.ControlPlaneConfig, "%s: fixture below assumes a controlPlaneConfig", tt.spec)
			require.NotNil(t, spec.ControlPlaneConfig.Infra, "%s: fixture below assumes controlPlaneConfig.infra", tt.spec)

			// nvkectl rejects unknown fields, so a misspelled docs patch must fail here too.
			var body api.UpdateClusterRequest
			require.NoError(t, yaml.UnmarshalStrict(patchDoc, &body), "patch document %q", patchDoc)

			// Mirror the row CreateCluster writes from the example's controlPlaneConfig,
			// so the PATCH diffs against the real stored spec, not an empty one.
			machineTemplate, err := json.Marshal(spec.ControlPlaneConfig.Infra.MachineTemplate)
			require.NoError(t, err)
			cpSpec := db.ControlPlaneConfigSpec{
				SystemReservedMemory: spec.ControlPlaneConfig.SystemReservedMemory,
				KubernetesVersion:    spec.ControlPlaneKubernetesVersion,
			}
			cpSpec.SetCapi(db.CAPIControlPlaneProvider{
				InfraMachineTemplatePreferences: []json.RawMessage{machineTemplate},
				PreKubeadmCommands:              spec.ControlPlaneConfig.Infra.PreKubeadmCommands,
			})
			configID := uuid.New()
			clusterID := uuid.New()
			cluster := db.Cluster{
				ID:                clusterID,
				Name:              spec.Name,
				KubernetesVersion: spec.ControlPlaneKubernetesVersion,
				ControlPlaneVIP:   spec.ControlPlaneVIP,
				Conditions: db.ClusterConditions{
					Provisioned: db.Condition{Status: db.ConditionStatusTrue},
					Deleting:    db.Condition{Status: db.ConditionStatusFalse},
				},
				ControlPlaneConfig: db.ControlPlaneConfig{
					ConfigurationID:        configID,
					DesiredCount:           spec.ControlPlaneConfig.DesiredCount,
					ControlPlaneConfigSpec: cpSpec,
				},
			}
			cluster.SetCAPIProvider(db.CAPIClusterProvider{})

			mockDB := &mockDB{}
			mockDB.MockTx = &mockTx{}
			mockDB.MockTx.On("GetClusterForUpdate", mock.Anything, clusterID).Return(cluster, nil)
			mockDB.MockTx.On("GetControlPlaneConfiguration", mock.Anything, configID).
				Return(db.ControlPlaneConfiguration{ID: configID, ClusterID: clusterID, Config: cpSpec}, nil).Maybe()
			// Only an accepted patch reaches the write; a rejected one must fail on the
			// response below, not on an unmet expectation.
			mockDB.MockTx.On("CreateControlPlaneConfiguration", mock.Anything, mock.Anything).
				Return(db.ControlPlaneConfiguration{ID: uuid.New()}, nil).Maybe()
			mockDB.MockTx.On("UpdateCluster", mock.Anything, mock.Anything).Return(cluster, nil).Maybe()
			mockDB.MockTx.On("ListNodePools", mock.Anything, mock.Anything).Return([]db.NodePool{}, nil).Maybe()
			mockDB.On("RunInTx", mock.Anything, mock.Anything).Return(nil)

			svc := newTestClusterService(discardLogger(), mockDB)
			resp, err := svc.UpdateCluster(context.Background(), api.UpdateClusterRequestObject{
				ClusterID: clusterID,
				Body:      &body,
			})
			require.NoError(t, err)
			_, ok := resp.(api.UpdateCluster200JSONResponse)
			require.True(t, ok, "%s patch %q on a cluster created from %s (k8s %s) was not accepted: %T %+v",
				tt.doc, patchDoc, tt.spec, spec.ControlPlaneKubernetesVersion, resp, resp)
		})
	}
}
