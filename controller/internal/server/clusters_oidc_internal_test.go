// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/dsx-ai-factory/nvke/controller/internal/config"
	"github.com/dsx-ai-factory/nvke/controller/internal/controllerconfig"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

func TestPinFleetOIDCProviders(t *testing.T) {
	t.Parallel()

	corp := controllerconfig.OIDCProvider{
		Name:           "corp",
		IssuerURL:      "https://login.example.test",
		Audiences:      []string{"nke"},
		UsernameClaim:  "email",
		UsernamePrefix: "corp:",
		CABundle:       "corp CA",
	}

	tests := []struct {
		name        string
		fleetOIDC   *controllerconfig.OIDCConfig
		version     string
		want        []db.ClusterOIDCProvider
		wantWarning bool
	}{
		{
			name:    "no nke-controller-config pins nothing",
			version: "1.34.1",
		},
		{
			name:      "providers are pinned",
			fleetOIDC: &controllerconfig.OIDCConfig{Providers: []controllerconfig.OIDCProvider{corp}},
			version:   "1.34.1",
			want:      db.ClusterOIDCProvidersFromConfig([]controllerconfig.OIDCProvider{corp}),
		},
		{
			name:      "no providers pins an empty list",
			fleetOIDC: &controllerconfig.OIDCConfig{},
			version:   "1.34.1",
			want:      []db.ClusterOIDCProvider{},
		},
		{
			name:        "before 1.34 pins an empty list and warns",
			fleetOIDC:   &controllerconfig.OIDCConfig{Providers: []controllerconfig.OIDCProvider{corp}},
			version:     "1.33.4",
			want:        []db.ClusterOIDCProvider{},
			wantWarning: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var logs bytes.Buffer
			s := &clusterService{logger: slog.New(slog.NewTextHandler(&logs, nil)), fleetOIDC: tt.fleetOIDC}
			cluster := db.Cluster{Name: "tenant-1", KubernetesVersion: tt.version}

			s.pinFleetOIDCProviders(&cluster)

			assert.Equal(t, tt.want, cluster.OIDCProviders)
			if tt.wantWarning {
				assert.Contains(t, logs.String(), "level=WARN")
				assert.Contains(t, logs.String(), "kubernetesVersion=1.33.4")
			} else {
				assert.Empty(t, logs.String())
			}
		})
	}
}

func TestWithFleetOIDCReachesTheClusterService(t *testing.T) {
	t.Parallel()
	fleetOIDC := &controllerconfig.OIDCConfig{}

	assert.Same(t, fleetOIDC, NewServerImpl(config.Config{}, slog.New(slog.DiscardHandler), nil, nil, nil, nil, nil, WithFleetOIDC(fleetOIDC)).clusterService.fleetOIDC)
	assert.Nil(t, NewServerImpl(config.Config{}, slog.New(slog.DiscardHandler), nil, nil, nil, nil, nil).clusterService.fleetOIDC)
}
