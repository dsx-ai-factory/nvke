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

func TestClient_CreateMachine_RejectsUnsupportedAttestationBeforeApplying(t *testing.T) {
	t.Parallel()

	for _, method := range []db.SpireAttestationMethod{db.SpireAttestationMethodJoinToken, db.SpireAttestationMethodTPM} {
		t.Run(string(method), func(t *testing.T) {
			t.Parallel()

			deps := testDeps(config.Config{SpiffeEnabled: true}, "example.org", "bundle")
			deps.K8sClient = fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
				Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
					t.Fatal("CreateMachine applied an object for a node it must reject")
					return nil
				},
			}).Build()
			node := db.Node{SpiffeEnabled: true, SpireAttestationMethod: method}

			err := (&Client{}).CreateMachine(t.Context(), deps, node, db.NodeSet{}, db.NodePool{}, db.Cluster{})

			require.ErrorIs(t, err, shared.ErrUnsupportedMachineConfig)
			require.ErrorContains(t, err, "SPIFFE-enabled LaunchLayer nodes require dsxImds SPIRE attestation")
		})
	}
}
