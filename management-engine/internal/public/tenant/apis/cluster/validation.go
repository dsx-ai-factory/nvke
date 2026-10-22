// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Tenant request validation enforces network constraints beyond the generated field mappings.
package cluster

import (
	"errors"
	"net/netip"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/dsx-ai-factory/nvke/management-engine/internal/public/shared/validation"
	"github.com/dsx-ai-factory/nvke/management-engine/internal/public/tenant/apis/cluster/mapping"
	clusterv1 "github.com/dsx-ai-factory/nvke/management-engine/public/tenant/gen/cluster/v1"
)

// validateNetworks limits creation to supported, nonoverlapping pod and service networks.
func validateNetworks(spec *clusterv1.CreateClusterSpec) error {
	pods, err := singleIPv4Prefix(spec.GetClusterNetwork().GetPods().GetCidrBlocks())
	if err != nil {
		return validation.FieldViolation("spec.cluster_network.pods.cidr_blocks", "must contain one canonical IPv4 CIDR")
	}
	services, err := singleIPv4Prefix(spec.GetClusterNetwork().GetServices().GetCidrBlocks())
	if err != nil {
		return validation.FieldViolation("spec.cluster_network.services.cidr_blocks", "must contain one canonical IPv4 CIDR")
	}
	if pods.Overlaps(services) {
		return validation.FieldViolation("spec.cluster_network", "pod and service networks must not overlap")
	}
	return nil
}

// singleIPv4Prefix parses one canonical range so network policy uses unambiguous addresses.
func singleIPv4Prefix(values []string) (netip.Prefix, error) {
	if len(values) != 1 {
		return netip.Prefix{}, errors.New("one network range is required")
	}
	prefix, err := netip.ParsePrefix(values[0])
	if err != nil || !prefix.Addr().Is4() || prefix != prefix.Masked() {
		return netip.Prefix{}, errors.New("a canonical IPv4 prefix is required")
	}
	return prefix, nil
}

// invalidSpec exposes reviewed field violations while hiding unexpected mapping diagnostics.
func invalidSpec(err error) error {
	if invalid, ok := errors.AsType[*mapping.FieldError](err); ok {
		return validation.FieldViolation(invalid.Field, invalid.Description)
	}
	return status.Error(codes.InvalidArgument, "cluster spec is invalid")
}
