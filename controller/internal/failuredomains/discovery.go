// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package failuredomains discovers a cluster's control-plane failure domains and
// spreads control-plane nodes across them.
package failuredomains

import (
	"cmp"
	"context"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// Snapshot is what control-plane placement needs from outside the database.
type Snapshot struct {
	Eligible []string
	// ObservedByNodeName tracks active CAPI Machine placements for spread counts.
	ObservedByNodeName map[string]string
}

// ErrInfrastructureNotProvisioned means the CAPI cluster does not exist yet or has
// not published its failure domains.
var ErrInfrastructureNotProvisioned = errors.New("capi cluster infrastructure is not yet provisioned")

// Discover returns a complete snapshot of the control-plane-eligible failure
// domains of the named CAPI cluster and the domain CAPI already reports for each
// of its active control-plane Machines. A discovery error is returned with a nil
// snapshot; it wraps ErrInfrastructureNotProvisioned while the cluster is initializing.
func Discover(ctx context.Context, k8sClient client.Client, namespace, clusterName string) (*Snapshot, error) {
	capiCluster := &clusterv1.Cluster{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: clusterName, Namespace: namespace}, capiCluster); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("get CAPI cluster for failure-domain discovery: %w: %w", ErrInfrastructureNotProvisioned, err)
		}
		return nil, fmt.Errorf("get CAPI cluster for failure-domain discovery: %w", err)
	}

	// An initializing cluster has not published its domains yet, which is not the
	// same as a provider that has none.
	if !ptr.Deref(capiCluster.Status.Initialization.InfrastructureProvisioned, false) {
		return nil, ErrInfrastructureNotProvisioned
	}

	discovered := &Snapshot{}
	for _, domain := range capiCluster.Status.FailureDomains {
		if ptr.Deref(domain.ControlPlane, false) {
			discovered.Eligible = append(discovered.Eligible, domain.Name)
		}
	}
	if len(discovered.Eligible) == 0 {
		return discovered, nil
	}

	// Collect active Machine placements to count existing nodes without writing
	// their observed domains back to the database.
	machines := &clusterv1.MachineList{}
	if err := k8sClient.List(ctx, machines,
		client.InNamespace(namespace),
		client.MatchingLabels{clusterv1.ClusterNameLabel: clusterName},
	); err != nil {
		return nil, fmt.Errorf("list CAPI machines for failure-domain discovery: %w", err)
	}

	discovered.ObservedByNodeName = make(map[string]string, len(machines.Items))
	for i := range machines.Items {
		machine := &machines.Items[i]
		if _, controlPlane := machine.Labels[clusterv1.MachineControlPlaneLabel]; !controlPlane {
			continue
		}
		if !machine.DeletionTimestamp.IsZero() {
			continue
		}
		// Status is the observed placement, but it is optional and can lag spec
		// while the infrastructure provider is reconciling an older Machine.
		if domain := cmp.Or(machine.Status.FailureDomain, machine.Spec.FailureDomain); domain != "" {
			discovered.ObservedByNodeName[machine.Name] = domain
		}
	}
	return discovered, nil
}

// DomainOf returns node's placement: the Machine's observed or requested domain,
// else the persisted decision. It returns "" when neither is known. A nil
// snapshot reports the persisted decision.
func (s *Snapshot) DomainOf(node db.Node) string {
	if s == nil {
		return node.FailureDomain
	}
	return cmp.Or(s.ObservedByNodeName[node.Name], node.FailureDomain)
}
