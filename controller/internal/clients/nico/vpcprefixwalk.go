// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nico

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	nicov1 "github.com/dsx-ai-factory/cluster-api-provider-nico/api/v1alpha1"

	"github.com/dsx-ai-factory/nvke/controller/internal/clients/shared"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
	"github.com/dsx-ai-factory/nvke/controller/internal/keys"
)

// The controller only learns that a VPC prefix is full when a NICo instance
// create on it fails: it has no NICo API client, and NICo's own capacity
// preflight reads a usage view that can lag the site's IP allocator. These
// helpers keep the walk through a node pool's VPC prefix candidates short and
// make the node's pending message say what is happening, instead of copying
// NICo's raw "IP address exhausted" error for every candidate it passes.
//
// A node that has not landed yet walks the node pool's current candidate
// list, not its node set's snapshot: attaching a prefix to the pool is how an
// operator relieves full prefixes, and a pool update leaves nodes that are
// already provisioning in their old node set.

// vpcPrefixIndex returns the position of vpcPrefixID in vpcPrefixIDs, or -1.
func vpcPrefixIndex(vpcPrefixIDs []uuid.UUID, vpcPrefixID string) int {
	for i, candidate := range vpcPrefixIDs {
		if candidate.String() == vpcPrefixID {
			return i
		}
	}
	return -1
}

// uniqueVpcPrefixIDs drops repeats, keeping first-seen order. Node pool
// create stores the list as given, and a repeated candidate would make the
// walk step from a full prefix onto itself.
func uniqueVpcPrefixIDs(vpcPrefixIDs []uuid.UUID) []uuid.UUID {
	seen := make(map[uuid.UUID]struct{}, len(vpcPrefixIDs))
	unique := make([]uuid.UUID, 0, len(vpcPrefixIDs))
	for _, id := range vpcPrefixIDs {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	return unique
}

func joinVpcPrefixIDs(vpcPrefixIDs []uuid.UUID) string {
	return strings.Join(uuidStrings(vpcPrefixIDs), ",")
}

// vpcPrefixWalk is where a NicoMachine stands in its walk through the node
// pool's VPC prefix candidates. candidates is never empty.
type vpcPrefixWalk struct {
	candidates []uuid.UUID
	// current is the position of the NicoMachine's prefix, or -1 when the
	// pool no longer lists it.
	current int
	start   int
	allFull bool
	// restarted is set when the walk was recorded against a different
	// candidate list (or before the list was recorded): a new walk begins at
	// the candidate after the current one.
	restarted bool
}

func readVpcPrefixWalk(nicoMachine *nicov1.NicoMachine, vpcPrefixIDs []uuid.UUID) vpcPrefixWalk {
	walk := vpcPrefixWalk{
		candidates: vpcPrefixIDs,
		current:    vpcPrefixIndex(vpcPrefixIDs, nicoMachine.Spec.Interfaces[0].VPCPrefixID),
	}
	annotations := nicoMachine.GetAnnotations()
	if keys.VPCPrefixWalkCandidates.Value(annotations) != joinVpcPrefixIDs(vpcPrefixIDs) {
		walk.restarted = true
		walk.start = walk.next()
		return walk
	}
	if i := vpcPrefixIndex(vpcPrefixIDs, keys.VPCPrefixWalkStart.Value(annotations)); i != -1 {
		walk.start = i
	}
	_, walk.allFull = keys.VPCPrefixesAllFull.Get(annotations)
	return walk
}

// next is the candidate the walk moves to after the current one fails.
func (w vpcPrefixWalk) next() int {
	if w.current == -1 {
		return 0
	}
	return (w.current + 1) % len(w.candidates)
}

// position is the 1-based place of candidate i in the walk.
func (w vpcPrefixWalk) position(i int) int {
	count := len(w.candidates)
	return (i-w.start+count)%count + 1
}

// startVpcPrefix picks the VPC prefix a node's NicoMachine is applied with,
// and the walk annotations to apply with it. A NicoMachine that already
// exists keeps its current prefix and walk record, so a retried create does
// not undo a rotation. A new one starts on the prefix of the node pool's most
// recently provisioned NicoMachine, which had free addresses then; with
// none, it starts on the first candidate. Nodes created in the same burst
// still share a start.
func startVpcPrefix(ctx context.Context, k8sClient client.Client, nicoMachine *nicov1.NicoMachine, nodePoolID uuid.UUID, vpcPrefixIDs []uuid.UUID) (string, map[string]string, error) {
	vpcPrefixIDs = uniqueVpcPrefixIDs(vpcPrefixIDs)
	existing := &nicov1.NicoMachine{}
	err := k8sClient.Get(ctx, client.ObjectKeyFromObject(nicoMachine), existing)
	switch {
	case err == nil:
		if len(existing.Spec.Interfaces) > 0 && existing.Spec.Interfaces[0].VPCPrefixID != "" {
			annotations := map[string]string{}
			for _, key := range []keys.Key{keys.VPCPrefixWalkCandidates, keys.VPCPrefixWalkStart} {
				if value, ok := key.Get(existing.GetAnnotations()); ok {
					annotations[key.String()] = value
				}
			}
			return existing.Spec.Interfaces[0].VPCPrefixID, annotations, nil
		}
	case !apierrors.IsNotFound(err):
		return "", nil, fmt.Errorf("failed to get NicoMachine %s/%s: %w", nicoMachine.Namespace, nicoMachine.Name, err)
	}

	siblings := &nicov1.NicoMachineList{}
	if err := k8sClient.List(ctx, siblings,
		client.InNamespace(nicoMachine.Namespace),
		client.MatchingLabels{clusterv1.ClusterNameLabel: nicoMachine.Labels[clusterv1.ClusterNameLabel]},
	); err != nil {
		return "", nil, fmt.Errorf("failed to list NicoMachines in %s: %w", nicoMachine.Namespace, err)
	}

	start := vpcPrefixIDs[0].String()
	var latest time.Time
	for i := range siblings.Items {
		sibling := &siblings.Items[i]
		if keys.NodePoolID.Value(sibling.Spec.Labels) != nodePoolID.String() || len(sibling.Spec.Interfaces) == 0 {
			continue
		}
		siblingVpcPrefixID := sibling.Spec.Interfaces[0].VPCPrefixID
		if vpcPrefixIndex(vpcPrefixIDs, siblingVpcPrefixID) == -1 {
			continue
		}
		provisioned := nicoMachineProvisionedCondition(sibling.Status.Conditions)
		if provisioned == nil || provisioned.Status != metav1.ConditionTrue {
			continue
		}
		if provisioned.LastTransitionTime.After(latest) {
			latest = provisioned.LastTransitionTime.Time
			start = siblingVpcPrefixID
		}
	}
	return start, map[string]string{
		keys.VPCPrefixWalkCandidates.String(): joinVpcPrefixIDs(vpcPrefixIDs),
		keys.VPCPrefixWalkStart.String():      start,
	}, nil
}

// DescribeStuckMachine returns the node pending message for a NicoMachine
// that is walking its VPC prefix candidates because NICo reported the current
// one full. It returns "" for every other state, so any other failure (an
// instance type with no allocations, a NICo API error) reaches the node
// unchanged.
func (*Client) DescribeStuckMachine(ctx context.Context, deps shared.MachineAdvanceDeps, nodePool db.NodePool, machine *clusterv1.Machine) (string, error) {
	nicoNodePoolProvider, err := nodePool.NicoProvider()
	if err != nil {
		return "", err
	}
	if len(nicoNodePoolProvider.VpcPrefixIDs) == 0 {
		return "", nil
	}
	nicoMachine := &nicov1.NicoMachine{}
	if err := deps.K8sClient.Get(ctx, client.ObjectKey{
		Namespace: machine.Namespace,
		Name:      machine.Spec.InfrastructureRef.Name,
	}, nicoMachine); err != nil {
		return "", fmt.Errorf("failed to get NicoMachine %s/%s: %w", machine.Namespace, machine.Spec.InfrastructureRef.Name, err)
	}
	return describeVpcPrefixWalk(nicoMachine, uniqueVpcPrefixIDs(nicoNodePoolProvider.VpcPrefixIDs)), nil
}

func describeVpcPrefixWalk(nicoMachine *nicov1.NicoMachine, vpcPrefixIDs []uuid.UUID) string {
	const full = "VPC prefix %s has no free IP addresses; trying the next configured VPC prefix (%d of %d)."
	if len(nicoMachine.Spec.Interfaces) == 0 || nicoMachine.Spec.Interfaces[0].VPCPrefixID == "" {
		return ""
	}
	provisioned := nicoMachineProvisionedCondition(nicoMachine.Status.Conditions)
	if provisioned == nil ||
		provisioned.Status != metav1.ConditionFalse ||
		provisioned.Reason != nicov1.InstanceCreateFailedReason ||
		!isNicoNetworkExhaustionMessage(provisioned.Message) {
		return ""
	}

	walk := readVpcPrefixWalk(nicoMachine, vpcPrefixIDs)
	count := len(vpcPrefixIDs)
	// A condition from an older generation is the failure on the previous
	// candidate: the controller has rotated and CAPNICo has not retried yet.
	failedOnCurrent := provisioned.ObservedGeneration == 0 || provisioned.ObservedGeneration >= nicoMachine.Generation

	switch {
	case walk.current == -1:
		return fmt.Sprintf("VPC prefix %s is no longer configured on the node pool; trying configured VPC prefix %s (1 of %d).",
			nicoMachine.Spec.Interfaces[0].VPCPrefixID, vpcPrefixIDs[0], count)
	case walk.allFull || (failedOnCurrent && !walk.restarted && walk.position(walk.current) == count):
		return allVpcPrefixesFullMessage(vpcPrefixIDs)
	case failedOnCurrent:
		return fmt.Sprintf(full, vpcPrefixIDs[walk.current], walk.position(walk.next()), count)
	case walk.restarted:
		// The pool's list changed after the last rotation. The count
		// starts again once this prefix fails or lands.
		return fmt.Sprintf("Trying configured VPC prefix %s; the previous one has no free IP addresses.",
			vpcPrefixIDs[walk.current])
	case walk.position(walk.current) > 1:
		// Past the walk's first step, the previous candidate is the one that
		// failed: keep its message until CAPNICo retries on this one.
		return fmt.Sprintf(full, vpcPrefixIDs[(walk.current-1+count)%count], walk.position(walk.current), count)
	default:
		return fmt.Sprintf("Trying configured VPC prefix %s (%d of %d); the previous one has no free IP addresses.",
			vpcPrefixIDs[walk.current], walk.position(walk.current), count)
	}
}

func allVpcPrefixesFullMessage(vpcPrefixIDs []uuid.UUID) string {
	const action = "Attach a VPC prefix with free IP addresses to the node pool; the controller keeps retrying."
	if len(vpcPrefixIDs) == 1 {
		return fmt.Sprintf("The configured VPC prefix %s has no free IP addresses. %s", vpcPrefixIDs[0], action)
	}
	return fmt.Sprintf("All %d configured VPC prefixes have no free IP addresses (%s). %s",
		len(vpcPrefixIDs), strings.Join(uuidStrings(vpcPrefixIDs), ", "), action)
}
