// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nico_test

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	nicov1 "github.com/dsx-ai-factory/cluster-api-provider-nico/api/v1alpha1"

	"github.com/dsx-ai-factory/nke/controller/internal/clients/nico"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
)

func TestClient_GetClusterScaffoldManifest(t *testing.T) {
	t.Run("builds NicoCluster manifest from provider payload", func(t *testing.T) {
		siteID := uuid.New()
		cluster := db.Cluster{
			ID:           uuid.New(),
			ProviderType: db.ProviderTypeNico,
			Nico:         &db.NicoClusterProvider{OrgID: "org-1", SiteID: siteID, VpcID: uuid.New()},
		}

		got, err := nico.New().GetClusterScaffoldManifest(cluster)
		require.NoError(t, err)

		var obj map[string]any
		require.NoError(t, json.Unmarshal(got, &obj))
		u := &unstructured.Unstructured{Object: obj}
		assert.Equal(t, nicov1.GroupVersion.String(), u.GetAPIVersion())
		assert.Equal(t, "NicoCluster", u.GetKind())
		gotSiteID, found, err := unstructured.NestedString(u.Object, "spec", "siteID")
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, siteID.String(), gotSiteID)
		gotFailureDomainLabelKey, found, err := unstructured.NestedString(u.Object, "spec", "failureDomainLabelKey")
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, "failure_domain", gotFailureDomainLabelKey)
	})

	t.Run("missing provider payload errors", func(t *testing.T) {
		_, err := nico.New().GetClusterScaffoldManifest(db.Cluster{
			ID:           uuid.New(),
			ProviderType: db.ProviderTypeNico,
		})
		require.Error(t, err)
		require.ErrorIs(t, err, db.ErrMissingProviderPayload)
	})
}
