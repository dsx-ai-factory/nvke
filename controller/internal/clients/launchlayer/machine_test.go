// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package launchlayer

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/dsx-ai-factory/nvke/controller/internal/clients/shared"
	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

func TestClient_CreateMachine_RejectsBeforeApplying(t *testing.T) {
	t.Parallel()

	var withoutKubernetesVersion db.NodeSet
	withoutKubernetesVersion.SetLaunchLayer(db.LaunchLayerNodePoolProvider{InstanceType: "dgx200"})
	var vmPool db.NodePool
	vmPool.SetLaunchLayer(db.LaunchLayerNodePoolProvider{InstanceType: "vm200", VMProfile: "ubuntu"})
	tests := map[string]struct {
		node        db.Node
		nodeSet     db.NodeSet
		nodePool    db.NodePool
		wantErrIs   error
		wantErrText string
	}{
		"vmProfile pool": {
			node:        db.Node{SpiffeEnabled: true, SpireAttestationMethod: db.SpireAttestationMethodDSXIMDS},
			nodePool:    vmPool,
			wantErrIs:   shared.ErrUnsupportedMachineConfig,
			wantErrText: "SPIFFE-enabled LaunchLayer nodes without external mTLS require hostOSProfile",
		},
		"a Machine that cannot be built": {
			nodeSet:     withoutKubernetesVersion,
			wantErrText: "kubernetes version is empty",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			deps := testDeps(config.Config{SpiffeEnabled: tt.node.SpiffeEnabled}, "example.org", "bundle")
			deps.K8sClient = fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
				Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
					t.Fatal("CreateMachine applied an object for a node it must reject")
					return nil
				},
			}).Build()

			err := (&Client{}).CreateMachine(t.Context(), deps, tt.node, tt.nodeSet, tt.nodePool, db.Cluster{})

			if tt.wantErrIs != nil {
				require.ErrorIs(t, err, tt.wantErrIs)
			}
			require.ErrorContains(t, err, tt.wantErrText)
		})
	}
}
