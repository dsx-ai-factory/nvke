// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package controllers holds the reconcilers of the provisioned Cluster API
// infrastructure provider.
//
// The package imports nothing from the cloud-workflow-engine. Everything it
// needs comes from the Kubernetes API, from Cluster API, and from the sibling
// api/v1alpha1 package, so the provider can move to its own module or
// repository without a code change.
package controllers

import (
	"fmt"
	"net/netip"
	"regexp"
	"sort"

	"k8s.io/apimachinery/pkg/util/validation"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	infrav1 "github.com/dsx-ai-factory/nke/cluster-api-provider-provisioned/api/v1alpha1"
)

var tpmEKPubHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// hostAllocations maps a canonical host IP address to the name of the machine
// that holds it.
type hostAllocations map[string]string

// allocationsFromMachines reads the allocation of every machine of a cluster.
//
// The provider derives this set on every reconcile instead of keeping it in
// status. The provider ID of a machine is the only record of an allocation, so
// the set is always as current as the API server, it survives a restart of the
// provider, and it needs no lease.
//
// A machine with no provider ID holds no host. A machine whose provider ID
// belongs to another provider, or to another cluster, is skipped: it cannot
// refer to a host of this inventory.
func allocationsFromMachines(clusterName string, machines []infrav1.ProvisionedMachine) hostAllocations {
	allocations := make(hostAllocations, len(machines))
	for i := range machines {
		machine := &machines[i]
		if machine.Spec.ProviderID == "" {
			continue
		}
		providerCluster, ipAddress, err := infrav1.ParseProviderID(machine.Spec.ProviderID)
		if err != nil || providerCluster != clusterName {
			continue
		}
		allocations[ipAddress] = machine.Name
	}
	return allocations
}

// selectHosts returns every host of the inventory that the machine could use,
// in inventory order.
//
// It returns all of them rather than the first because a host can still be lost
// at the next step, to a lock held by another cluster. Keeping the order means
// the result does not change between two reconciles of the same state. A host
// is usable when no machine of this cluster holds it and its labels contain
// every entry of the selector of the machine.
func selectHosts(
	hosts []infrav1.ProvisionedHost,
	allocated hostAllocations,
	selector map[string]string,
) []infrav1.ProvisionedHost {
	usable := make([]infrav1.ProvisionedHost, 0, len(hosts))
	for _, host := range hosts {
		if _, used := allocated[host.CanonicalIPAddress()]; used {
			continue
		}
		if !labelsMatch(host.Labels, selector) {
			continue
		}
		usable = append(usable, host)
	}
	return usable
}

// labelsMatch reports whether the labels of a host contain every entry of the
// selector. An empty selector matches every host.
func labelsMatch(hostLabels, selector map[string]string) bool {
	for key, want := range selector {
		if hostLabels[key] != want {
			return false
		}
	}
	return true
}

// validateInventory checks the rules that the provider cannot recover from at
// allocation time.
//
// A repeated hostname would make the inventory ambiguous to an operator. A
// repeated canonical IP address would let two machines believe they hold
// different hosts while they share one, so both are rejected.
func validateInventory(hosts []infrav1.ProvisionedHost) error {
	seenHostnames := make(map[string]struct{}, len(hosts))
	seenIPAddresses := make(map[string]string, len(hosts))

	for _, host := range hosts {
		if host.Hostname == "" {
			return fmt.Errorf("every host must have a hostname")
		}
		if problems := validation.IsDNS1123Subdomain(host.Hostname); len(problems) != 0 {
			return fmt.Errorf("host %q has an invalid hostname: %s", host.Hostname, problems[0])
		}
		if _, exists := seenHostnames[host.Hostname]; exists {
			return fmt.Errorf("hostname %q is used more than once", host.Hostname)
		}
		seenHostnames[host.Hostname] = struct{}{}

		address, err := netip.ParseAddr(host.IPAddress)
		if err != nil {
			return fmt.Errorf("host %q has invalid IP address %q: %w", host.Hostname, host.IPAddress, err)
		}
		canonicalIP := address.String()
		if owner, exists := seenIPAddresses[canonicalIP]; exists && owner != host.Hostname {
			return fmt.Errorf(
				"IP address %q is used by host %q and by host %q",
				canonicalIP, owner, host.Hostname,
			)
		}
		seenIPAddresses[canonicalIP] = host.Hostname

		if host.TPMEKPubHash != "" && !tpmEKPubHashPattern.MatchString(host.TPMEKPubHash) {
			return fmt.Errorf("host %q has invalid TPM EK public key hash %q", host.Hostname, host.TPMEKPubHash)
		}
	}

	return nil
}

// availableHostCount counts the hosts that no machine holds.
func availableHostCount(hosts []infrav1.ProvisionedHost, allocated hostAllocations) int32 {
	var available int32
	for _, host := range hosts {
		if _, used := allocated[host.CanonicalIPAddress()]; !used {
			available++
		}
	}
	return available
}

// sortedAllocations turns the allocation set into the stable list that the
// cluster status reports. The order comes from the host name, so the status
// does not change when the machines are listed in another order.
func sortedAllocations(
	hosts []infrav1.ProvisionedHost,
	allocated hostAllocations,
) []infrav1.HostAllocation {
	if len(allocated) == 0 {
		return nil
	}

	list := make([]infrav1.HostAllocation, 0, len(allocated))
	for _, host := range hosts {
		if machineName, found := allocated[host.CanonicalIPAddress()]; found {
			list = append(list, infrav1.HostAllocation{HostName: host.Hostname, MachineName: machineName})
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].HostName < list[j].HostName })
	return list
}

// machineAddresses returns the hostname and IP address that Cluster API copies
// to Machine status. CWE uses InternalIP to contact the node agent.
func machineAddresses(host infrav1.ProvisionedHost) []clusterv1.MachineAddress {
	return []clusterv1.MachineAddress{
		{Type: clusterv1.MachineHostName, Address: host.Hostname},
		{Type: clusterv1.MachineInternalIP, Address: host.CanonicalIPAddress()},
	}
}
