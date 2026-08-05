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

	"github.com/spiffe/go-spiffe/v2/workloadapi"

	"github.com/NVIDIA/nke/controller/internal/config"
)

// GetTrustDomainAndBundle returns the trust domain
// and trust bundle (Marshaled to PEM format) from the SPIFFE workload API.
func GetTrustDomainAndBundle(ctx context.Context, cfg config.Config) (string, string, error) {
	client, err := workloadapi.New(ctx, workloadapi.WithAddr(cfg.SpiffeEndpointSocket))
	if err != nil {
		return "", "", fmt.Errorf("failed to create SPIFFE workload API client: %w", err)
	}
	defer client.Close()

	x509Context, err := client.FetchX509Context(ctx)
	if err != nil {
		return "", "", fmt.Errorf("failed to fetch SPIFFE x509 context: %w", err)
	}

	if len(x509Context.SVIDs) == 0 {
		return "", "", fmt.Errorf("no SPIFFE SVIDs found")
	}

	if len(x509Context.SVIDs) > 1 {
		return "", "", fmt.Errorf("multiple SPIFFE SVIDs found")
	}

	svid := x509Context.SVIDs[0]
	trustDomain := svid.ID.TrustDomain()

	// Get the trust bundle for this trust domain
	bundle, ok := x509Context.Bundles.Get(trustDomain)
	if !ok {
		return "", "", fmt.Errorf("no trust bundle found for trust domain %s", trustDomain.Name())
	}

	// Marshal the trust bundle to PEM format
	trustBundlePEM, err := bundle.Marshal()
	if err != nil {
		return "", "", fmt.Errorf("failed to marshal trust bundle: %w", err)
	}

	return trustDomain.Name(), string(trustBundlePEM), nil
}
