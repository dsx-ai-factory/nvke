//go:build kubeadm_bringup

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nicovms

import (
	"context"
	"errors"
	"fmt"
)

// NodeSpec is one element of a batch CreateNodes call.
type NodeSpec struct {
	Name             string // VM hostname (also passed to nico-vms as the instance name)
	UserData         string // rendered cloud-init body (from internal/cloudinit.Render)
	PhoneHomeEnabled bool   // typically true so "ready" == cloud-init done
}

// Node is the resolved per-VM result of a CreateNodes call.
type Node struct {
	Name       string
	InstanceID string
	IP         string
}

// CreateNodes creates every spec in turn, ONE AT A TIME. Returns the resolved
// Nodes in the SAME ORDER as specs so the caller can rely on positional
// indexing (specs[0] → nodes[0] → vm-1 etc.).
//
// On failure it stops, best-effort deletes whichever nodes did create, and
// returns the error. Callers must NOT rely on that partial cleanup — they still
// need to invoke proc.DeleteNode for any IDs they got back from a SUCCESSFUL
// CreateNodes return.
//
// SEQUENTIAL, deliberately. This used to fan out with an errgroup, on the
// stated grounds that "nico-vms's libvirt backend handles parallel provisioning
// fine". It does not, under qemu:///session. Two `virsh start` calls 125µs apart
// both try to create the same per-user swtpm run directory and one loses:
//
//	error: Failed to start domain 'nico-vm-<id>'
//	error: failed to create directory '$HOME/.cache/libvirt/qemu/run/swtpm': File exists
//
// That is a race in the libvirt path, not in this test, and it is timing
// dependent — two CI runs passed before a third hit it. Serialising here buys a
// stable lane while the underlying bug is fixed where it belongs. The cost is
// wall clock: VM boots no longer overlap. With 2 VMs under emulation that is a
// few minutes, which the lane's budget absorbs easily.
func (p *Process) CreateNodes(ctx context.Context, specs []NodeSpec) ([]Node, error) {
	if len(specs) == 0 {
		return nil, nil
	}

	results := make([]Node, len(specs))
	// Track instance IDs so a later failure can clean up earlier successes.
	var created []string
	var failure error

	for i, spec := range specs {
		id, ip, err := p.CreateNode(ctx, spec.Name, spec.UserData, spec.PhoneHomeEnabled)
		// CreateNode may return a non-empty id even on error (the instance was
		// created but never reached ready) — record it either way so cleanup
		// can target it.
		if id != "" {
			created = append(created, id)
		}
		if err != nil {
			failure = fmt.Errorf("create %s: %w", spec.Name, err)
			break
		}
		results[i] = Node{Name: spec.Name, InstanceID: id, IP: ip}
	}

	if err := failure; err != nil {
		// Best-effort cleanup of any siblings that did succeed. Use a fresh
		// context so cleanup runs even if the caller's ctx is cancelled.
		var cleanupErrs []error
		for _, id := range created {
			if derr := p.DeleteNode(context.Background(), id); derr != nil {
				cleanupErrs = append(cleanupErrs, derr)
			}
		}
		if len(cleanupErrs) > 0 {
			return nil, fmt.Errorf("CreateNodes failed: %w; cleanup errors: %w",
				err, errors.Join(cleanupErrs...))
		}
		return nil, fmt.Errorf("CreateNodes failed: %w", err)
	}
	return results, nil
}
