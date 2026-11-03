// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package bootstrap

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kubeadmv1 "sigs.k8s.io/cluster-api/api/bootstrap/kubeadm/v1beta2"
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

func TestKubeadmConfigCRDInstalled(t *testing.T) {
	installed := meta.NewDefaultRESTMapper([]schema.GroupVersion{kubeadmv1.GroupVersion})
	installed.Add(kubeadmv1.GroupVersion.WithKind("KubeadmConfig"), meta.RESTScopeNamespace)

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
			name:   "no Kubeadm bootstrap provider installed",
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
			got, err := KubeadmConfigCRDInstalled(tt.mapper)
			if tt.wantErrIs != nil {
				require.ErrorIs(t, err, tt.wantErrIs)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tt.want, got)
		})
	}
}
