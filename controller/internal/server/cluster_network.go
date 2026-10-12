// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"fmt"
	"net/netip"
	"strings"

	"github.com/dsx-ai-factory/nvke/controller/api"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

const (
	defaultClusterPodCIDR     = "192.168.0.0/16"
	defaultClusterServiceCIDR = "10.96.0.0/12"
	minClusterCIDRPrefixBits  = 12
	maxClusterCIDRPrefixBits  = 24
	maxPodCIDRPrefixBits      = 23
	podCIDRNodePrefixBits     = 24
	minimumPodCIDRHeadroom    = 1
)

type resolvedClusterNetwork struct {
	podCIDR         string
	podCIDRPrefixes []netip.Prefix
	serviceCIDR     netip.Prefix
}

func (n resolvedClusterNetwork) validateCNI(cni db.CniPlugin) error {
	usesDefaults := n.podCIDR == defaultClusterPodCIDR &&
		n.serviceCIDR.String() == defaultClusterServiceCIDR
	if cni != db.CNIOvnKubernetes && !usesDefaults {
		return fmt.Errorf("custom pod or service CIDRs require CNI plugin %q", db.CNIOvnKubernetes)
	}
	return nil
}

func validatePodCIDRCapacity(podCIDRs []netip.Prefix, desiredNodes, configuredSurge int) error {
	headroom := max(minimumPodCIDRHeadroom, configuredSurge)
	requiredNodeBlocks := desiredNodes + headroom
	availableNodeBlocks := 0
	podCIDRParts := make([]string, 0, len(podCIDRs))
	for _, podCIDR := range podCIDRs {
		availableNodeBlocks += 1 << (podCIDRNodePrefixBits - podCIDR.Bits())
		podCIDRParts = append(podCIDRParts, podCIDR.String())
	}
	if availableNodeBlocks < requiredNodeBlocks {
		return fmt.Errorf(
			"network.podCIDR %q provides %d /%d node blocks, but %d are required for %d desired nodes plus %d surge/headroom; choose a larger pod CIDR",
			strings.Join(podCIDRParts, ","),
			availableNodeBlocks,
			podCIDRNodePrefixBits,
			requiredNodeBlocks,
			desiredNodes,
			headroom,
		)
	}
	return nil
}

func podCIDRCapacityRequirement(nodePools []db.NodePool, clusterMaxSurge int) (int, int) {
	desiredNodes := 0
	configuredSurge := clusterMaxSurge
	for _, nodePool := range nodePools {
		desiredNodes += nodePool.DesiredCount
		configuredSurge += nodePool.MaxSurge
	}
	return desiredNodes, configuredSurge
}

func requiredPodCIDRNodeBlocks(nodePools []db.NodePool, clusterMaxSurge int) int {
	desiredNodes, configuredSurge := podCIDRCapacityRequirement(nodePools, clusterMaxSurge)
	return desiredNodes + max(minimumPodCIDRHeadroom, configuredSurge)
}

func validateStoredPodCIDRCapacity(cluster db.Cluster, nodePools []db.NodePool) error {
	network, err := resolveStoredClusterNetwork(cluster)
	if err != nil {
		return fmt.Errorf("invalid stored cluster network: %w", err)
	}
	desiredNodes, configuredSurge := podCIDRCapacityRequirement(nodePools, cluster.ControlPlaneConfig.MaxSurge)
	return validatePodCIDRCapacity(network.podCIDRPrefixes, desiredNodes, configuredSurge)
}

func resolveClusterNetwork(config *api.ClusterNetworkCreateConfig) (resolvedClusterNetwork, error) {
	podCIDR := defaultClusterPodCIDR
	serviceCIDR := defaultClusterServiceCIDR
	if config != nil {
		if config.PodCIDR != "" {
			podCIDR = config.PodCIDR
		}
		if config.ServiceCIDR != "" {
			serviceCIDR = config.ServiceCIDR
		}
	}

	podCIDRPrefixes, canonicalPodCIDR, err := parseClusterPodCIDRs("network.podCIDR", podCIDR)
	if err != nil {
		return resolvedClusterNetwork{}, err
	}
	servicePrefix, err := parseClusterCIDR("network.serviceCIDR", serviceCIDR, maxClusterCIDRPrefixBits)
	if err != nil {
		return resolvedClusterNetwork{}, err
	}
	for _, podPrefix := range podCIDRPrefixes {
		if podPrefix.Overlaps(servicePrefix) {
			return resolvedClusterNetwork{}, fmt.Errorf(
				"network.podCIDR %q and network.serviceCIDR %q must not overlap",
				podPrefix,
				servicePrefix,
			)
		}
	}

	return resolvedClusterNetwork{
		podCIDR:         canonicalPodCIDR,
		podCIDRPrefixes: podCIDRPrefixes,
		serviceCIDR:     servicePrefix,
	}, nil
}

func resolveStoredClusterNetwork(cluster db.Cluster) (resolvedClusterNetwork, error) {
	if cluster.PodCIDR == "" || cluster.ServiceCIDR == "" {
		return resolvedClusterNetwork{}, fmt.Errorf("stored pod and service CIDRs must both be set")
	}
	podCIDRPrefixes, canonicalPodCIDR, err := parseClusterPodCIDRs("cluster pod CIDR", cluster.PodCIDR)
	if err != nil {
		return resolvedClusterNetwork{}, err
	}
	serviceCIDR, err := parseClusterCIDR("cluster service CIDR", cluster.ServiceCIDR, maxClusterCIDRPrefixBits)
	if err != nil {
		return resolvedClusterNetwork{}, err
	}
	for _, podPrefix := range podCIDRPrefixes {
		if podPrefix.Overlaps(serviceCIDR) {
			return resolvedClusterNetwork{}, fmt.Errorf(
				"stored pod CIDR %q and service CIDR %q overlap",
				podPrefix,
				serviceCIDR,
			)
		}
	}
	return resolvedClusterNetwork{podCIDR: canonicalPodCIDR, podCIDRPrefixes: podCIDRPrefixes, serviceCIDR: serviceCIDR}, nil
}

func parseClusterPodCIDRs(field, value string) ([]netip.Prefix, string, error) {
	parts := strings.Split(value, ",")
	prefixes := make([]netip.Prefix, 0, len(parts))
	canonicalParts := make([]string, 0, len(parts))

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, "", fmt.Errorf("%s %q contains an empty CIDR entry", field, value)
		}

		prefix, err := parseClusterCIDR(field, part, maxPodCIDRPrefixBits)
		if err != nil {
			return nil, "", err
		}
		for _, existing := range prefixes {
			if existing.Overlaps(prefix) {
				return nil, "", fmt.Errorf("%s %q overlaps %q", field, prefix, existing)
			}
		}

		prefixes = append(prefixes, prefix)
		canonicalParts = append(canonicalParts, prefix.String())
	}

	return prefixes, strings.Join(canonicalParts, ","), nil
}

func parseClusterCIDR(field, value string, maxPrefixBits int) (netip.Prefix, error) {
	prefix, err := netip.ParsePrefix(value)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("%s %q is not a valid CIDR: %w", field, value, err)
	}
	if !prefix.Addr().Is4() {
		return netip.Prefix{}, fmt.Errorf("%s %q must be an IPv4 CIDR", field, value)
	}

	prefix = prefix.Masked()
	if prefix.Bits() < minClusterCIDRPrefixBits || prefix.Bits() > maxPrefixBits {
		return netip.Prefix{}, fmt.Errorf(
			"%s %q must have a prefix length between /%d and /%d",
			field,
			value,
			minClusterCIDRPrefixBits,
			maxPrefixBits,
		)
	}
	if !isContainedInPrivateAddressSpace(prefix) {
		return netip.Prefix{}, fmt.Errorf("%s %q must be contained in RFC 1918 private address space", field, value)
	}
	return prefix, nil
}

// isContainedInPrivateAddressSpace reports whether the entire prefix is contained in RFC 1918 private address space.
func isContainedInPrivateAddressSpace(prefix netip.Prefix) bool {
	privateAddressSpaces := [...]netip.Prefix{
		netip.MustParsePrefix("10.0.0.0/8"),
		netip.MustParsePrefix("172.16.0.0/12"),
		netip.MustParsePrefix("192.168.0.0/16"),
	}
	for _, privateAddressSpace := range privateAddressSpaces {
		if prefix.Bits() >= privateAddressSpace.Bits() && privateAddressSpace.Contains(prefix.Addr()) {
			return true
		}
	}
	return false
}

func (n resolvedClusterNetwork) validateControlPlaneVIP(vip string) error {
	addr, err := netip.ParseAddr(vip)
	if err != nil {
		return fmt.Errorf("invalid controlPlaneVIP %q: %w", vip, err)
	}
	if !addr.Is4() {
		return fmt.Errorf("invalid controlPlaneVIP %q: must be an IPv4 address", vip)
	}
	for _, podPrefix := range n.podCIDRPrefixes {
		if podPrefix.Contains(addr) {
			return newBadRequestErrorf(
				"controlPlaneVIP %q overlaps %s %q",
				vip,
				"network.podCIDR",
				podPrefix,
			)
		}
	}
	if n.serviceCIDR.Contains(addr) {
		return newBadRequestErrorf(
			"controlPlaneVIP %q overlaps %s %q",
			vip,
			"network.serviceCIDR",
			n.serviceCIDR,
		)
	}
	return nil
}
