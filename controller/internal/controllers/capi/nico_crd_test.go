// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capi

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	nicov1 "github.com/dsx-ai-factory/cluster-api-provider-nico/api/v1alpha1"
)

// erroringRESTMapper returns err from RESTMapping, standing in for a discovery
// failure that is not a missing kind.
type erroringRESTMapper struct {
	meta.RESTMapper
	err error
}

func (m erroringRESTMapper) RESTMapping(schema.GroupKind, ...string) (*meta.RESTMapping, error) {
	return nil, m.err
}

func TestNicoMachineCRDInstalled(t *testing.T) {
	installed := meta.NewDefaultRESTMapper([]schema.GroupVersion{nicov1.GroupVersion})
	installed.Add(nicov1.GroupVersion.WithKind("NicoMachine"), meta.RESTScopeNamespace)

	discoveryErr := errors.New("connection refused")

	tests := []struct {
		name      string
		mapper    meta.RESTMapper
		want      bool
		wantErrIs error
	}{
		{
			name:   "CRD served by the cluster",
			mapper: installed,
			want:   true,
		},
		{
			name:   "no CAPNICo provider installed",
			mapper: meta.NewDefaultRESTMapper(nil),
			want:   false,
		},
		{
			name:      "discovery itself failed",
			mapper:    erroringRESTMapper{err: discoveryErr},
			want:      false,
			wantErrIs: discoveryErr,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NicoMachineCRDInstalled(tt.mapper)
			if tt.wantErrIs != nil {
				require.ErrorIs(t, err, tt.wantErrIs)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tt.want, got)
		})
	}
}

func TestMachineCRDInstalled(t *testing.T) {
	installed := meta.NewDefaultRESTMapper([]schema.GroupVersion{clusterv1.GroupVersion})
	installed.Add(clusterv1.GroupVersion.WithKind("Machine"), meta.RESTScopeNamespace)

	discoveryErr := errors.New("connection refused")

	tests := []struct {
		name      string
		mapper    meta.RESTMapper
		want      bool
		wantErrIs error
	}{
		{
			name:   "CRD served by the cluster",
			mapper: installed,
			want:   true,
		},
		{
			name:   "no Cluster API installed",
			mapper: meta.NewDefaultRESTMapper(nil),
			want:   false,
		},
		{
			name:   "CAPNICo installed but Cluster API is not",
			mapper: nicoOnlyMapper(),
			want:   false,
		},
		{
			name:      "discovery itself failed",
			mapper:    erroringRESTMapper{err: discoveryErr},
			want:      false,
			wantErrIs: discoveryErr,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := MachineCRDInstalled(tt.mapper)
			if tt.wantErrIs != nil {
				require.ErrorIs(t, err, tt.wantErrIs)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tt.want, got)
		})
	}
}

// nicoOnlyMapper serves NicoMachine and nothing else, so a lookup that ignored
// its group and kind would wrongly report Machine as installed.
func nicoOnlyMapper() meta.RESTMapper {
	m := meta.NewDefaultRESTMapper([]schema.GroupVersion{nicov1.GroupVersion})
	m.Add(nicov1.GroupVersion.WithKind("NicoMachine"), meta.RESTScopeNamespace)
	return m
}
