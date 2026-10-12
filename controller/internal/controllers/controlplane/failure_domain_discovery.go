// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"cmp"
	"context"
	"fmt"

	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nvke/controller/internal/constants"
	"github.com/dsx-ai-factory/nvke/controller/internal/db"
)

// failureDomains is what control-plane placement needs from outside the database.
type failureDomains struct {
	eligible []string
	// observedByNodeName tracks active CAPI Machine placements for spread counts.
	observedByNodeName map[string]string
}

// readFailureDomains returns a complete snapshot of the control-plane-eligible
// failure domains of a cluster and the domain CAPI already reports for each of
// its active control-plane Machines. A discovery error is returned with a nil
// snapshot and aborts reconciliation.
func (r *reconciler) readFailureDomains(ctx context.Context, cluster db.Cluster) (*failureDomains, error) {
	if !r.cfg.UseControlPlaneControllerForControlPlane() {
		return &failureDomains{}, nil
	}

	if r.k8sClient == nil {
		return nil, fmt.Errorf("kubernetes client is not configured")
	}

	capiCluster := &clusterv1.Cluster{}
	if err := r.k8sClient.Get(ctx, client.ObjectKey{
		Name:      cluster.Name,
		Namespace: constants.NamespaceNVKESystem,
	}, capiCluster); err != nil {
		return nil, fmt.Errorf("get CAPI cluster for failure-domain discovery: %w", err)
	}

	// An initializing cluster has not published its domains yet, which is not the
	// same as a provider that has none.
	if !ptr.Deref(capiCluster.Status.Initialization.InfrastructureProvisioned, false) {
		return nil, fmt.Errorf("capi cluster infrastructure is not yet provisioned")
	}

	discovered := &failureDomains{}
	for _, domain := range capiCluster.Status.FailureDomains {
		if ptr.Deref(domain.ControlPlane, false) {
			discovered.eligible = append(discovered.eligible, domain.Name)
		}
	}
	if len(discovered.eligible) == 0 {
		return discovered, nil
	}

	// Collect active Machine placements to count existing nodes without writing
	// their observed domains back to the database.
	machines := &clusterv1.MachineList{}
	if err := r.k8sClient.List(ctx, machines,
		client.InNamespace(constants.NamespaceNVKESystem),
		client.MatchingLabels{clusterv1.ClusterNameLabel: cluster.Name},
	); err != nil {
		return nil, fmt.Errorf("list CAPI machines for failure-domain discovery: %w", err)
	}

	discovered.observedByNodeName = make(map[string]string, len(machines.Items))
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
			discovered.observedByNodeName[machine.Name] = domain
		}
	}
	return discovered, nil
}
