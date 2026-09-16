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

// failureDomainSnapshot carries the results of one discovery pass together
// from discovery into placement. Keeping eligible domains, observed machine
// assignments, and any discovery error in one value ensures placement uses
// related inputs while allowing unrelated reconciliation to continue.
type failureDomainSnapshot struct {
	eligible           []string
	observedByNodeName map[string]string
	discoveryErr       error
}

// readFailureDomains returns eligible control-plane failure domains and the
// observed domain for each active control-plane machine. It retains discovery
// failures in the snapshot so callers can preserve partial results and defer
// only work that requires a complete placement view.
func (c *Controller) readFailureDomains(ctx context.Context, clusterID uuid.UUID) failureDomainSnapshot {
	if !c.cfg.UseControlPlaneControllerForControlPlane() {
		return failureDomainSnapshot{}
	}

	cluster, err := c.db.GetCluster(ctx, clusterID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return failureDomainSnapshot{}
		}
		return failureDomainSnapshot{discoveryErr: fmt.Errorf("get cluster for failure-domain discovery: %w", err)}
	}
	if cluster.Conditions.Deleting.Status == db.ConditionStatusTrue {
		return failureDomainSnapshot{}
	}
	if c.k8sClient == nil {
		return failureDomainSnapshot{discoveryErr: fmt.Errorf("kubernetes client is not configured")}
	}

	// Query cluster to get its available failure domains
	capiCluster := &clusterv1.Cluster{}
	if err := c.k8sClient.Get(ctx, client.ObjectKey{
		Name:      cluster.Name,
		Namespace: constants.NamespaceNKESystem,
	}, capiCluster); err != nil {
		return failureDomainSnapshot{discoveryErr: fmt.Errorf("get CAPI cluster for failure-domain discovery: %w", err)}
	}

	// Short-circuit when cluster is still initializing to distinguish between
	// a cluster with no available domains and a cluster still initializing
	// and hasn't yet set its available domains
	if !ptr.Deref(capiCluster.Status.Initialization.InfrastructureProvisioned, false) {
		return failureDomainSnapshot{discoveryErr: fmt.Errorf("capi cluster infrastructure is not yet provisioned")}
	}

	result := failureDomainSnapshot{}
	for _, domain := range capiCluster.Status.FailureDomains {
		if ptr.Deref(domain.ControlPlane, false) {
			result.eligible = append(result.eligible, domain.Name)
		}
	}
	if len(result.eligible) == 0 {
		return result
	}

	machines := &clusterv1.MachineList{}
	if err := c.k8sClient.List(ctx, machines,
		client.InNamespace(constants.NamespaceNKESystem),
		client.MatchingLabels{clusterv1.ClusterNameLabel: cluster.Name},
	); err != nil {
		result.discoveryErr = fmt.Errorf("list CAPI machines for failure-domain discovery: %w", err)
		return result
	}

	result.observedByNodeName = make(map[string]string, len(machines.Items))
	for i := range machines.Items {
		machine := &machines.Items[i]
		if _, controlPlane := machine.Labels[clusterv1.MachineControlPlaneLabel]; !controlPlane {
			continue
		}
		if !machine.DeletionTimestamp.IsZero() {
			continue
		}
		// Get the machine's observed failure domain. If observed domain is not set,
		// use the domain in the machine's spec.
		if domain := cmp.Or(machine.Status.FailureDomain, machine.Spec.FailureDomain); domain != "" {
			result.observedByNodeName[machine.Name] = domain
		}
	}
	return result
}
