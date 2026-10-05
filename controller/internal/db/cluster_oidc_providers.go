// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"

	"github.com/google/uuid"

	"github.com/dsx-ai-factory/nke/controller/internal/controllerconfig"
)

// ClusterOIDCProvider is one pinned provider. The database owns this shape
// rather than storing controllerconfig.OIDCProvider, so a stored snapshot
// stays readable when the configuration file's schema changes.
type ClusterOIDCProvider struct {
	Name           string            `json:"name"`
	IssuerURL      string            `json:"issuerURL"`
	Audiences      []string          `json:"audiences"`
	UsernameClaim  string            `json:"usernameClaim"`
	UsernamePrefix string            `json:"usernamePrefix"`
	GroupsClaim    string            `json:"groupsClaim,omitempty"`
	GroupsPrefix   string            `json:"groupsPrefix,omitempty"`
	RequiredClaims map[string]string `json:"requiredClaims,omitempty"`
	CABundle       string            `json:"caBundle"`
}

// ClusterOIDCProvidersFromConfig converts the fleet-wide providers from
// nke-controller-config into the stored shape. The result is empty rather than
// nil when there are none, and shares no slice or map with providers.
func ClusterOIDCProvidersFromConfig(providers []controllerconfig.OIDCProvider) []ClusterOIDCProvider {
	out := make([]ClusterOIDCProvider, 0, len(providers))
	for _, provider := range providers {
		out = append(out, ClusterOIDCProvider{
			Name:           provider.Name,
			IssuerURL:      provider.IssuerURL,
			Audiences:      append([]string(nil), provider.Audiences...),
			UsernameClaim:  provider.UsernameClaim,
			UsernamePrefix: provider.UsernamePrefix,
			GroupsClaim:    provider.GroupsClaim,
			GroupsPrefix:   provider.GroupsPrefix,
			RequiredClaims: maps.Clone(provider.RequiredClaims),
			CABundle:       provider.CABundle,
		})
	}
	return out
}

func (q *queriesImpl) PinClusterOIDCProviders(ctx context.Context, clusterID uuid.UUID, providers []ClusterOIDCProvider) ([]ClusterOIDCProvider, error) {
	// Empty, not nil: nil is stored as NULL, which means not pinned.
	if providers == nil {
		providers = []ClusterOIDCProvider{}
	}
	_, err := q.db.NewUpdate().
		Model(&Cluster{ID: clusterID, OIDCProviders: providers}).
		Column("oidc_providers").
		WherePK().
		Where("oidc_providers IS NULL").
		Exec(ctx)
	if err != nil {
		return nil, fmt.Errorf("pin OIDC providers for cluster %s: %w", clusterID, err)
	}

	var cluster Cluster
	err = q.db.NewSelect().Model(&cluster).Column("oidc_providers").Where("id = ?", clusterID).Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("read OIDC providers for cluster %s: %w", clusterID, err)
	}
	return cluster.OIDCProviders, nil
}
