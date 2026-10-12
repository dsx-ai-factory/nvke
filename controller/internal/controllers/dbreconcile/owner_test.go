// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package dbreconcile_test

import (
	"testing"

	"github.com/go-logr/logr"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	crlog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/dsx-ai-factory/nvke/controller/internal/controllers/dbreconcile"
)

func TestOwnerLabel(t *testing.T) {
	tests := map[string]struct {
		kind dbreconcile.Kind
		want string
	}{
		"cluster":   {kind: dbreconcile.KindCluster, want: "owner.nke.nvidia.com/cluster"},
		"node pool": {kind: dbreconcile.KindNodePool, want: "owner.nke.nvidia.com/node_pool"},
		"node set":  {kind: dbreconcile.KindNodeSet, want: "owner.nke.nvidia.com/node_set"},
		"node":      {kind: dbreconcile.KindNode, want: "owner.nke.nvidia.com/node"},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.want, dbreconcile.OwnerLabel(tt.kind))
		})
	}
}

func TestSetOwner(t *testing.T) {
	ownerID := uuid.New()
	tests := map[string]struct {
		labels map[string]string
		want   map[string]string
	}{
		"initializes labels": {
			want: map[string]string{dbreconcile.OwnerLabel(dbreconcile.KindCluster): ownerID.String()},
		},
		"preserves existing labels": {
			labels: map[string]string{"existing": "label"},
			want: map[string]string{
				"existing": "label",
				dbreconcile.OwnerLabel(dbreconcile.KindCluster): ownerID.String(),
			},
		},
		"replaces existing owner": {
			labels: map[string]string{dbreconcile.OwnerLabel(dbreconcile.KindCluster): uuid.NewString()},
			want:   map[string]string{dbreconcile.OwnerLabel(dbreconcile.KindCluster): ownerID.String()},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Labels: tt.labels}}
			dbreconcile.SetOwner(secret, dbreconcile.KindCluster, ownerID)
			assert.Equal(t, tt.want, secret.Labels)
		})
	}
}

func TestSelectorForKind(t *testing.T) {
	selector, err := metav1.LabelSelectorAsSelector(dbreconcile.SelectorForKind(dbreconcile.KindCluster))
	require.NoError(t, err)
	tests := map[string]struct {
		labels map[string]string
		want   bool
	}{
		"owner label exists": {
			labels: map[string]string{dbreconcile.OwnerLabel(dbreconcile.KindCluster): uuid.NewString()},
			want:   true,
		},
		"owner label is missing": {},
		"different owner kind": {
			labels: map[string]string{dbreconcile.OwnerLabel(dbreconcile.KindNode): uuid.NewString()},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.want, selector.Matches(labels.Set(tt.labels)))
		})
	}
}

func TestRequestForOwner(t *testing.T) {
	ownerID := uuid.New()
	tests := map[string]struct {
		value string
		want  []dbreconcile.Key
	}{
		"valid owner": {
			value: ownerID.String(),
			want:  []dbreconcile.Key{dbreconcile.ClusterKey(ownerID)},
		},
		"missing owner": {},
		"malformed owner": {
			value: "not-a-uuid",
		},
	}
	ctx := crlog.IntoContext(t.Context(), logr.Discard())
	mapper := dbreconcile.RequestForOwner(dbreconcile.KindCluster)

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			secret := new(corev1.Secret)
			if tt.value != "" {
				secret.Labels = map[string]string{dbreconcile.OwnerLabel(dbreconcile.KindCluster): tt.value}
			}
			assert.Equal(t, tt.want, mapper(ctx, secret))
		})
	}
}

func TestKindOwns(t *testing.T) {
	tests := map[string]struct {
		kind      dbreconcile.Kind
		namespace string
	}{
		"cluster Secret": {kind: dbreconcile.KindCluster, namespace: "nke-system"},
		"node Secret":    {kind: dbreconcile.KindNode, namespace: "nodes"},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			ownerID := uuid.New()
			watch := tt.kind.Owns(&corev1.Secret{}, tt.namespace)
			secret := new(corev1.Secret)
			dbreconcile.SetOwner(secret, tt.kind, ownerID)

			assert.IsType(t, &corev1.Secret{}, watch.Object)
			assert.Equal(t, tt.namespace, watch.Namespace)
			assert.Equal(t, []dbreconcile.Key{{Kind: tt.kind, ID: ownerID}}, watch.Map(t.Context(), secret))
		})
	}
}
