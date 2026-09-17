// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"fmt"

	"k8s.io/utils/ptr"

	"github.com/dsx-ai-factory/nke/controller/internal/capi"
	"github.com/dsx-ai-factory/nke/controller/internal/db"
)

// prepareFailureDomainAssignments persists placement decisions for control-plane nodes using
// control plane configuration from the database.
func (r *reconciler) prepareFailureDomainAssignments(ctx context.Context, controlPlane *controlPlane) error {
	if !r.cfg.UseControlPlaneControllerForControlPlane() {
		return nil
	}

	// Zero-out struct fields to avoid using stale data
	r.domainAssignments = r.domainAssignments[:0]
	r.unknownDomains = r.unknownDomains[:0]
	r.retryFailureDomains = false
	pending := []*db.Node{}

	for i := range controlPlane.nodes {
		node := &controlPlane.nodes[i]
		if node.IsMarkedForDeletion() {
			continue
		}

		// Node database record does not have a failure domain set and the nil
		// discoveryErr value indicates it is safe to assign one.
		if !node.HasFailureDomainDecision() && r.failureDomainSnapshot.discoveryErr == nil {
			if observed := r.failureDomainSnapshot.observedByNodeName[node.Name]; observed != "" {
				node.FailureDomain = ptr.To(observed)
			} else if len(r.failureDomainSnapshot.eligible) == 0 {
				// This provider does not support control-plane failure domains.
				node.FailureDomain = ptr.To("")
			}
			if node.HasFailureDomainDecision() {
				if _, err := r.db.UpdateNode(ctx, node); err != nil {
					return fmt.Errorf("persist failure domain for node %s: %w", node.ID, err)
				}
			}
		}

		switch {
		case node.HasFailureDomainDecision():
			if failureDomain := node.GetFailureDomain(); failureDomain != "" {
				r.domainAssignments = append(r.domainAssignments, failureDomain)
			}
		case node.HasInfrastructureProvisioningStarted():
			r.unknownDomains = append(r.unknownDomains, node.Name)
		default:
			// Node failure domain decision not made and infra provisioning has not started,
			// so it's safe to assign the failure domain to the node.
			pending = append(pending, node)
		}
	}

	if r.failureDomainSnapshot.discoveryErr != nil || len(r.unknownDomains) > 0 {
		// Defer new assignments until discovery succeeds and every node whose
		// infrastructure has started has an observed placement. A later snapshot
		// can resolve unknownDomains from the CAPI Machine's spec or status.
		r.retryFailureDomains = len(pending) > 0 || len(r.unknownDomains) > 0
		return nil
	}
	for _, node := range pending {
		failureDomain, err := r.selectFailureDomain()
		if err != nil {
			return err
		}
		node.FailureDomain = failureDomain
		if _, err := r.db.UpdateNode(ctx, node); err != nil {
			return fmt.Errorf("persist failure domain for pending node %s: %w", node.ID, err)
		}
		if *failureDomain != "" {
			r.domainAssignments = append(r.domainAssignments, *failureDomain)
		}
	}
	return nil
}

// selectFailureDomain chooses the eligible failure domain using the chosen
// FailureDomainSelectionAlgorithm implementation
func (r *reconciler) selectFailureDomain() (*string, error) {
	if r.failureDomainSnapshot.discoveryErr != nil {
		return nil, r.failureDomainSnapshot.discoveryErr
	}
	if len(r.unknownDomains) > 0 {
		return nil, fmt.Errorf("waiting for observed failure domains for existing control-plane nodes: %v", r.unknownDomains)
	}

	domain, err := capi.LeastUsedFailureDomainSelection(r.failureDomainSnapshot.eligible, r.domainAssignments)
	if err != nil {
		return nil, fmt.Errorf("select control-plane failure domain: %w", err)
	}
	return ptr.To(domain), nil
}
