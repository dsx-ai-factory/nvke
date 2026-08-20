// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package controllers holds the reconcilers of the preprovisioned Cluster API
// infrastructure provider.
//
// The package imports nothing from the cloud-workflow-engine. Everything it
// needs comes from the Kubernetes API, from Cluster API, and from the sibling
// api/v1alpha1 package, so the provider can move to its own module or
// repository without a code change.
package controllers

import (
	"fmt"
	"sort"

	infrav1 "github.com/NVIDIA/nke/controller/preprovisioned/api/v1alpha1"
)

// hostAllocations maps a host name to the name of the machine that holds it.
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
func allocationsFromMachines(clusterName string, machines []infrav1.PreprovisionedMachine) hostAllocations {
	allocations := make(hostAllocations, len(machines))
	for i := range machines {
		machine := &machines[i]
		if machine.Spec.ProviderID == "" {
			continue
		}
		providerCluster, hostName, err := infrav1.ParseProviderID(machine.Spec.ProviderID)
		if err != nil || providerCluster != clusterName {
			continue
		}
		allocations[hostName] = machine.Name
	}
	return allocations
}

// selectHosts returns every host of the inventory that the machine could use,
// in inventory order.
//
// It returns all of them rather than the first because a host can still be lost
// at the next step, to a lock held by another cluster. Keeping the order means
// the result does not change between two reconciles of the same state. A host
// is usable when no machine of this cluster holds it, its role accepts the
// machine, and its labels contain every entry of the selector of the machine.
func selectHosts(
	hosts []infrav1.PreprovisionedHost,
	allocated hostAllocations,
	controlPlane bool,
	selector map[string]string,
) []infrav1.PreprovisionedHost {
	usable := make([]infrav1.PreprovisionedHost, 0, len(hosts))
	for _, host := range hosts {
		if _, used := allocated[host.Name]; used {
			continue
		}
		if !roleAccepts(host.Role, controlPlane) {
			continue
		}
		if !labelsMatch(host.Labels, selector) {
			continue
		}
		usable = append(usable, host)
	}
	return usable
}

// roleAccepts reports whether a host with this role accepts a machine. An empty
// role accepts every machine.
func roleAccepts(role infrav1.HostRole, controlPlane bool) bool {
	switch role {
	case "":
		return true
	case infrav1.HostRoleControlPlane:
		return controlPlane
	case infrav1.HostRoleWorker:
		return !controlPlane
	default:
		// An unknown role never matches. The enum marker on the type keeps this
		// out of the API server, so this only guards an object that was written
		// before the marker existed.
		return false
	}
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
// A repeated host name would let two machines believe they hold different
// hosts while they share one, and kubeadm would then fail on the second. A
// repeated address has the same effect, and the name check alone does not catch
// it, so both are checked here.
func validateInventory(hosts []infrav1.PreprovisionedHost) error {
	seenNames := make(map[string]struct{}, len(hosts))
	seenAddresses := make(map[string]string, len(hosts))

	for _, host := range hosts {
		if host.Name == "" {
			return fmt.Errorf("every host must have a name")
		}
		if _, exists := seenNames[host.Name]; exists {
			return fmt.Errorf("host name %q is used more than once", host.Name)
		}
		seenNames[host.Name] = struct{}{}

		if len(host.Addresses) == 0 {
			return fmt.Errorf("host %q has no address", host.Name)
		}
		for _, address := range host.Addresses {
			if address.Address == "" {
				return fmt.Errorf("host %q has an empty address", host.Name)
			}
			if owner, exists := seenAddresses[address.Address]; exists && owner != host.Name {
				return fmt.Errorf(
					"address %q is used by host %q and by host %q",
					address.Address, owner, host.Name,
				)
			}
			seenAddresses[address.Address] = host.Name
		}
	}

	return nil
}

// availableHostCount counts the hosts that no machine holds.
func availableHostCount(hosts []infrav1.PreprovisionedHost, allocated hostAllocations) int32 {
	var available int32
	for _, host := range hosts {
		if _, used := allocated[host.Name]; !used {
			available++
		}
	}
	return available
}

// sortedAllocations turns the allocation set into the stable list that the
// cluster status reports. The order comes from the host name, so the status
// does not change when the machines are listed in another order.
func sortedAllocations(allocated hostAllocations) []infrav1.HostAllocation {
	if len(allocated) == 0 {
		return nil
	}

	list := make([]infrav1.HostAllocation, 0, len(allocated))
	for hostName, machineName := range allocated {
		list = append(list, infrav1.HostAllocation{HostName: hostName, MachineName: machineName})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].HostName < list[j].HostName })
	return list
}
