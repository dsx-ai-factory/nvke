// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	nicov1 "github.com/dsx-ai-factory/cluster-api-provider-nico/api/v1alpha1"

	"github.com/dsx-ai-factory/nvke/controller/internal/clients/shared"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

func TestTryAdvanceStuckMachineRotatesOnlyNicoMachines(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		ref      clusterv1.ContractVersionedObjectReference
		wantCall bool
	}{
		{name: "NicoMachine", ref: clusterv1.ContractVersionedObjectReference{APIGroup: nicov1.GroupVersion.Group, Kind: "NicoMachine", Name: "cp-0"}, wantCall: true},
		{name: "DockerMachine", ref: clusterv1.ContractVersionedObjectReference{APIGroup: "infrastructure.cluster.x-k8s.io", Kind: "DockerMachine", Name: "cp-0"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			scheme := runtime.NewScheme()
			require.NoError(t, nicov1.AddToScheme(scheme))
			called := false
			deps := shared.MachineAdvanceDeps{
				K8sClient: fake.NewClientBuilder().WithScheme(scheme).Build(),
				RotateFailureDomain: func(context.Context, db.Node, *clusterv1.Machine, bool) (bool, error) {
					called = true
					return false, nil
				},
			}
			machine := &clusterv1.Machine{Spec: clusterv1.MachineSpec{FailureDomain: "fd-a", InfrastructureRef: tt.ref}}

			_, err := (&Client{}).TryAdvanceStuckMachine(context.Background(), deps, db.Node{}, db.NodeSet{}, db.NodePool{}, machine)
			require.NoError(t, err)
			assert.Equal(t, tt.wantCall, called)
		})
	}
}
