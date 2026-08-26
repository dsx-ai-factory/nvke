// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"fmt"
	"net/netip"
	"strings"
)

const (
	// ProviderIDScheme is the scheme of every provider ID that this provider
	// writes.
	ProviderIDScheme = "provisioned"

	providerIDPrefix = ProviderIDScheme + "://"
)

// FormatProviderID builds the provider ID that binds a machine to a host.
//
// The provider ID is the record of the allocation, so it must identify the host
// without any other lookup.
func FormatProviderID(clusterName, ipAddress string) string {
	if address, err := netip.ParseAddr(ipAddress); err == nil {
		ipAddress = address.String()
	}
	return providerIDPrefix + clusterName + "/" + ipAddress
}

// ParseProviderID returns the cluster name and canonical IP address of a
// provider ID that this provider wrote. It returns an error for any other
// provider ID, so a caller never reads an identity out of a nico:// value.
func ParseProviderID(providerID string) (string, string, error) {
	rest, found := strings.CutPrefix(providerID, providerIDPrefix)
	if !found {
		return "", "", fmt.Errorf("provider ID %q does not start with %q", providerID, providerIDPrefix)
	}

	clusterName, ipAddress, found := strings.Cut(rest, "/")
	if !found || clusterName == "" || ipAddress == "" {
		return "", "", fmt.Errorf("provider ID %q must have the form %s<cluster>/<ip-address>", providerID, providerIDPrefix)
	}

	address, err := netip.ParseAddr(ipAddress)
	if err != nil {
		return "", "", fmt.Errorf("provider ID %q has invalid IP address %q: %w", providerID, ipAddress, err)
	}

	return clusterName, address.String(), nil
}

// CanonicalIPAddress returns the normalized IP address used in provider IDs
// and allocation keys. Invalid inventory data returns an empty string; the
// cluster reconciler reports the validation error before machines can use it.
func (h ProvisionedHost) CanonicalIPAddress() string {
	address, err := netip.ParseAddr(h.IPAddress)
	if err != nil {
		return ""
	}
	return address.String()
}
