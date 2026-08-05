// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: LicenseRef-NvidiaProprietary
//
// NVIDIA CORPORATION, its affiliates and licensors retain all intellectual
// property and proprietary rights in and to this material, related
// documentation and any modifications thereto. Any use, reproduction,
// disclosure or distribution of this material and related documentation
// without an express license agreement from NVIDIA CORPORATION or
// its affiliates is strictly prohibited.

package spiffe

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/spiffe/go-spiffe/v2/spiffegrpc/grpccredentials"
	"github.com/spiffe/go-spiffe/v2/spiffeid"
	"github.com/spiffe/go-spiffe/v2/spiffetls/tlsconfig"
	"github.com/spiffe/go-spiffe/v2/workloadapi"
	agentv1 "github.com/spiffe/spire-api-sdk/proto/spire/api/server/agent/v1"
	typesv1 "github.com/spiffe/spire-api-sdk/proto/spire/api/types"
	"google.golang.org/grpc"

	"github.com/NVIDIA/nke/controller/internal/config"
)

func CreateJoinToken(ctx context.Context, cfg config.Config, agentAlternateID string) (string, error) {
	agentAlternateSPIFFEID, err := spiffeIDProtoFromString(agentAlternateID)
	if err != nil {
		return "", fmt.Errorf("failed to parse agent alternate SPIFFE ID: %w", err)
	}

	workloadClient, err := workloadapi.New(ctx, workloadapi.WithAddr(cfg.SpiffeEndpointSocket))
	if err != nil {
		return "", fmt.Errorf("failed to create SPIFFE workload API client: %w", err)
	}
	defer workloadClient.Close()

	x509Source, err := workloadapi.NewX509Source(ctx, workloadapi.WithClient(workloadClient))
	if err != nil {
		return "", fmt.Errorf("failed to create SPIFFE X509 source: %w", err)
	}
	defer x509Source.Close()

	bundleSource, err := workloadapi.NewBundleSource(ctx, workloadapi.WithClient(workloadClient))
	if err != nil {
		return "", fmt.Errorf("failed to create SPIFFE bundle source: %w", err)
	}
	defer bundleSource.Close()

	svid, err := x509Source.GetX509SVID()
	if err != nil {
		return "", fmt.Errorf("failed to get CWE workload X509-SVID: %w", err)
	}

	conn, err := grpc.NewClient(
		cfg.SpireServerAPIAddress,
		grpc.WithTransportCredentials(
			grpccredentials.MTLSClientCredentials(
				x509Source,
				bundleSource,
				tlsconfig.AuthorizeMemberOf(svid.ID.TrustDomain()),
			),
		),
	)
	if err != nil {
		return "", fmt.Errorf("failed to connect to SPIRE server API address %q: %w", cfg.SpireServerAPIAddress, err)
	}
	defer conn.Close()

	ttlSecondsDuration := cfg.SpireJoinTokenTTL / time.Second
	if ttlSecondsDuration > math.MaxInt32 {
		return "", fmt.Errorf("SPIRE join token TTL must be less than or equal to %d seconds", math.MaxInt32)
	}
	ttlSeconds := int32(ttlSecondsDuration) //nolint:gosec // ttlSecondsDuration is validated to be within the int32 range above.
	if ttlSeconds <= 0 {
		return "", fmt.Errorf("SPIRE join token TTL must be at least 1 second")
	}

	token, err := agentv1.NewAgentClient(conn).CreateJoinToken(ctx, &agentv1.CreateJoinTokenRequest{
		Ttl:     ttlSeconds,
		AgentId: agentAlternateSPIFFEID,
	})
	if err != nil {
		return "", fmt.Errorf("failed to create SPIRE join token: %w", err)
	}
	if token.GetValue() == "" {
		return "", fmt.Errorf("SPIRE server returned an empty join token")
	}

	return token.GetValue(), nil
}

func spiffeIDProtoFromString(id string) (*typesv1.SPIFFEID, error) {
	parsedID, err := spiffeid.FromString(id)
	if err != nil {
		return nil, err
	}

	return &typesv1.SPIFFEID{
		TrustDomain: parsedID.TrustDomain().Name(),
		Path:        parsedID.Path(),
	}, nil
}
