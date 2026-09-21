// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/dsx-ai-factory/nke/controller/internal/constants"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
)

const (
	failureDomainRetryInterval = 30 * time.Second
)

// failureDomains is what control-plane placement needs from outside the database.
type failureDomains struct {
	eligible []string
	// observedByNodeName carries the placement CAPI already made, for node rows
	// that never recorded their own. Empty once no such row remains, at which
	// point the Machine list below can go.
	observedByNodeName map[string]string
}

// readFailureDomains returns the control-plane-eligible failure domains of a
// cluster and the domain CAPI already reports for each of its active
// control-plane Machines.
//
// Call it outside the reconcile transaction so the cluster row lock is not held
// across the Kubernetes reads.
func (c *Controller) readFailureDomains(ctx context.Context, clusterID uuid.UUID) (failureDomains, error) {
	if !c.cfg.UseControlPlaneControllerForControlPlane() {
		return failureDomains{}, nil
	}

	cluster, err := c.db.GetCluster(ctx, clusterID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return failureDomains{}, nil
		}
		return failureDomains{}, fmt.Errorf("get cluster for failure-domain discovery: %w", err)
	}
	if cluster.Conditions.Deleting.Status == db.ConditionStatusTrue {
		return failureDomains{}, nil
	}
	if c.k8sClient == nil {
		return failureDomains{}, fmt.Errorf("kubernetes client is not configured")
	}

	capiCluster := &clusterv1.Cluster{}
	if err := c.k8sClient.Get(ctx, client.ObjectKey{
		Name:      cluster.Name,
		Namespace: constants.NamespaceNKESystem,
	}, capiCluster); err != nil {
		return failureDomains{}, fmt.Errorf("get CAPI cluster for failure-domain discovery: %w", err)
	}

	// An initializing cluster has not published its domains yet, which is not the
	// same as a provider that has none.
	if !ptr.Deref(capiCluster.Status.Initialization.InfrastructureProvisioned, false) {
		return failureDomains{}, fmt.Errorf("capi cluster infrastructure is not yet provisioned")
	}

	discovered := failureDomains{}
	for _, domain := range capiCluster.Status.FailureDomains {
		if ptr.Deref(domain.ControlPlane, false) {
			discovered.eligible = append(discovered.eligible, domain.Name)
		}
	}
	if len(discovered.eligible) == 0 {
		return discovered, nil
	}

	machines := &clusterv1.MachineList{}
	if err := c.k8sClient.List(ctx, machines,
		client.InNamespace(constants.NamespaceNKESystem),
		client.MatchingLabels{clusterv1.ClusterNameLabel: cluster.Name},
	); err != nil {
		// Eligible domains are already known so we can return
		// and use those even if the observation failed.
		return discovered, fmt.Errorf("list CAPI machines for failure-domain discovery: %w", err)
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
		if domain := cmp.Or(machine.Status.FailureDomain, machine.Spec.FailureDomain); domain != "" {
			discovered.observedByNodeName[machine.Name] = domain
		}
	}
	return discovered, nil
}
