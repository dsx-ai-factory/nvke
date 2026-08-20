// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"fmt"
	"strings"
)

const (
	// ProviderIDScheme is the scheme of every provider ID that this provider
	// writes.
	ProviderIDScheme = "preprovisioned"

	providerIDPrefix = ProviderIDScheme + "://"
)

// FormatProviderID builds the provider ID that binds a machine to a host.
//
// The provider ID is the record of the allocation, so it must identify the host
// without any other lookup.
func FormatProviderID(clusterName, hostName string) string {
	return providerIDPrefix + clusterName + "/" + hostName
}

// ParseProviderID returns the cluster name and the host name of a provider ID
// that this provider wrote. It returns an error for any other provider ID, so a
// caller never reads a host name out of, for example, a nico:// value.
func ParseProviderID(providerID string) (string, string, error) {
	rest, found := strings.CutPrefix(providerID, providerIDPrefix)
	if !found {
		return "", "", fmt.Errorf("provider ID %q does not start with %q", providerID, providerIDPrefix)
	}

	clusterName, hostName, found := strings.Cut(rest, "/")
	if !found || clusterName == "" || hostName == "" {
		return "", "", fmt.Errorf("provider ID %q must have the form %s<cluster>/<host>", providerID, providerIDPrefix)
	}

	return clusterName, hostName, nil
}
